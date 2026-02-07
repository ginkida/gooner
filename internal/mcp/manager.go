package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gokin/internal/logging"
	"gokin/internal/tools"
)

// Manager manages multiple MCP server connections.
type Manager struct {
	servers map[string]*ServerConfig // Server configurations
	clients map[string]*Client       // Active client connections
	tools   []tools.Tool             // All registered tools from all servers
	health  map[string]*ServerHealth
	mu      sync.RWMutex

	// Auto-healing
	healMu        sync.Mutex // protects healingCancel and healingDone (separate from mu to avoid deadlock)
	healingCancel context.CancelFunc
	healingDone   chan struct{}
}

// ServerHealth tracks health status of an MCP server.
type ServerHealth struct {
	Healthy            bool
	LastCheck          time.Time
	ConsecutiveFails   int
	ReconnectAttempts  int
	MaxReconnects      int
	LastReconnectError string
}

// NewManager creates a new MCP manager.
func NewManager(servers []*ServerConfig) *Manager {
	m := &Manager{
		servers: make(map[string]*ServerConfig),
		clients: make(map[string]*Client),
		tools:   make([]tools.Tool, 0),
		health:  make(map[string]*ServerHealth),
	}

	for _, cfg := range servers {
		m.servers[cfg.Name] = cfg
	}

	return m
}

// defaultServerTimeout is the per-server connection timeout.
const defaultServerTimeout = 15 * time.Second

// serverResult holds the result of a single server connection attempt.
type serverResult struct {
	name  string
	client *Client
	tools  []tools.Tool
	err    error
}

// ConnectAll connects to all servers configured for auto-connect.
// Connections are made in parallel with per-server timeouts.
func (m *Manager) ConnectAll(ctx context.Context) error {
	// Collect auto-connect servers under read lock
	m.mu.RLock()
	var toConnect []*ServerConfig
	for name, cfg := range m.servers {
		if !cfg.AutoConnect {
			logging.Debug("MCP server skipped (auto_connect=false)", "name", name)
			continue
		}
		toConnect = append(toConnect, cfg)
	}
	m.mu.RUnlock()

	if len(toConnect) == 0 {
		return nil
	}

	// Connect to all servers in parallel
	results := make(chan serverResult, len(toConnect))
	var wg sync.WaitGroup

	for _, cfg := range toConnect {
		wg.Add(1)
		go func(cfg *ServerConfig) {
			defer wg.Done()

			serverCtx, cancel := context.WithTimeout(ctx, defaultServerTimeout)
			defer cancel()

			res := serverResult{name: cfg.Name}

			// Create client (no lock needed — pure network I/O)
			client, err := NewClient(cfg)
			if err != nil {
				res.err = fmt.Errorf("failed to create client: %w", err)
				results <- res
				return
			}

			// Initialize connection
			if err := client.Initialize(serverCtx); err != nil {
				client.Close()
				res.err = fmt.Errorf("initialization failed: %w", err)
				results <- res
				return
			}

			// List tools
			mcpTools, err := client.ListTools(serverCtx)
			if err != nil {
				client.Close()
				res.err = fmt.Errorf("failed to list tools: %w", err)
				results <- res
				return
			}

			// Create tool wrappers
			for _, t := range mcpTools {
				tool := NewMCPTool(client, cfg.Name, cfg.ToolPrefix, t)
				res.tools = append(res.tools, tool)
			}

			res.client = client
			results <- res
		}(cfg)
	}

	// Close results channel when all goroutines complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results under write lock
	var errs []error
	m.mu.Lock()
	for res := range results {
		if res.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", res.name, res.err))
			continue
		}

		m.clients[res.name] = res.client
		m.tools = append(m.tools, res.tools...)

		logging.Info("MCP server connected",
			"name", res.name,
			"tools", len(res.tools))
	}
	m.mu.Unlock()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// connectServer connects to a single server and registers its tools.
// Must be called with m.mu held.
func (m *Manager) connectServer(ctx context.Context, cfg *ServerConfig) error {
	// Create client
	client, err := NewClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Initialize connection
	if err := client.Initialize(ctx); err != nil {
		client.Close()
		return fmt.Errorf("initialization failed: %w", err)
	}

	// List tools
	mcpTools, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return fmt.Errorf("failed to list tools: %w", err)
	}

	// Store client
	m.clients[cfg.Name] = client

	// Create tool wrappers
	for _, t := range mcpTools {
		tool := NewMCPTool(client, cfg.Name, cfg.ToolPrefix, t)
		m.tools = append(m.tools, tool)
	}

	logging.Info("MCP server connected",
		"name", cfg.Name,
		"tools", len(mcpTools))

	return nil
}

// Connect connects to a specific server by name.
func (m *Manager) Connect(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, exists := m.servers[name]
	if !exists {
		return fmt.Errorf("unknown server: %s", name)
	}

	// Check if already connected
	if _, connected := m.clients[name]; connected {
		return nil
	}

	return m.connectServer(ctx, cfg)
}

// Disconnect disconnects from a specific server.
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, exists := m.clients[name]
	if !exists {
		return nil
	}

	// Remove tools from this server
	newTools := make([]tools.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		if mcpTool, ok := t.(*MCPTool); ok {
			if mcpTool.GetServerName() != name {
				newTools = append(newTools, t)
			}
		} else {
			newTools = append(newTools, t)
		}
	}
	m.tools = newTools

	// Close client
	delete(m.clients, name)
	if err := client.Close(); err != nil {
		return fmt.Errorf("failed to close client: %w", err)
	}

	logging.Info("MCP server disconnected", "name", name)
	return nil
}

// GetTools returns all tools from all connected servers.
func (m *Manager) GetTools() []tools.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]tools.Tool, len(m.tools))
	copy(result, m.tools)
	return result
}

// GetClient returns the client for a specific server.
func (m *Manager) GetClient(name string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	client, ok := m.clients[name]
	return client, ok
}

// GetConnectedServers returns the names of all connected servers.
func (m *Manager) GetConnectedServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	return names
}

// GetServerConfig returns the configuration for a server.
func (m *Manager) GetServerConfig(name string) (*ServerConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg, ok := m.servers[name]
	return cfg, ok
}

// GetAllServerConfigs returns all server configurations.
func (m *Manager) GetAllServerConfigs() []*ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	configs := make([]*ServerConfig, 0, len(m.servers))
	for _, cfg := range m.servers {
		configs = append(configs, cfg)
	}
	return configs
}

// AddServer adds a new server configuration.
func (m *Manager) AddServer(cfg *ServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.servers[cfg.Name]; exists {
		return fmt.Errorf("server already exists: %s", cfg.Name)
	}

	m.servers[cfg.Name] = cfg
	return nil
}

// RemoveServer removes a server configuration and disconnects if connected.
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Disconnect if connected
	if client, exists := m.clients[name]; exists {
		client.Close()
		delete(m.clients, name)
	}

	// Remove configuration
	delete(m.servers, name)
	return nil
}

// Shutdown disconnects from all servers and stops background processes.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.StopHealthCheck()

	m.mu.Lock()
	defer m.mu.Unlock()

	var lastErr error
	for name, client := range m.clients {
		if err := client.Close(); err != nil {
			logging.Warn("MCP client close error", "name", name, "error", err)
			lastErr = err
		}
	}

	m.clients = make(map[string]*Client)
	m.tools = nil

	logging.Debug("MCP manager shutdown complete")
	return lastErr
}

// CheckHealth checks the health of all connected servers.
func (m *Manager) CheckHealth(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, client := range m.clients {
		fails := client.ConsecutiveFails()

		h, ok := m.health[name]
		if !ok {
			h = &ServerHealth{Healthy: true}
			m.health[name] = h
		}

		h.ConsecutiveFails = fails
		h.LastCheck = time.Now()

		if fails >= 3 {
			if h.Healthy {
				logging.Warn("MCP server marked unhealthy", "name", name, "fails", fails)
			}
			h.Healthy = false
		} else {
			h.Healthy = true
		}
	}
}

// IsHealthy returns whether a server is healthy.
func (m *Manager) IsHealthy(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	h, ok := m.health[name]
	if !ok {
		return true // Unknown servers are considered healthy
	}
	return h.Healthy
}

// ServerStatus contains status information about an MCP server.
type ServerStatus struct {
	Name        string
	Connected   bool
	Healthy     bool
	ServerInfo  *ServerInfo
	ToolCount   int
	ToolNames   []string
}

// GetServerStatus returns status information for all servers.
func (m *Manager) GetServerStatus() []*ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]*ServerStatus, 0, len(m.servers))

	for name, cfg := range m.servers {
		status := &ServerStatus{
			Name:      name,
			Connected: false,
		}

		if client, ok := m.clients[name]; ok {
			status.Connected = true
			if h, ok := m.health[name]; ok {
				status.Healthy = h.Healthy
			} else {
				status.Healthy = true
			}
			status.ServerInfo = client.GetServerInfo()

			// Count tools from this server
			for _, t := range m.tools {
				if mcpTool, ok := t.(*MCPTool); ok {
					if mcpTool.GetServerName() == name {
						status.ToolCount++
						status.ToolNames = append(status.ToolNames, mcpTool.Name())
					}
				}
			}
		} else {
			status.Healthy = false
		}

		// Use config name if server info not available
		if status.ServerInfo == nil {
			status.ServerInfo = &ServerInfo{Name: cfg.Name}
		}

		statuses = append(statuses, status)
	}

	return statuses
}

// RefreshTools refreshes the tool list from a specific server.
func (m *Manager) RefreshTools(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, exists := m.clients[name]
	if !exists {
		return fmt.Errorf("server not connected: %s", name)
	}

	cfg, exists := m.servers[name]
	if !exists {
		return fmt.Errorf("server config not found: %s", name)
	}

	// Get updated tool list
	mcpTools, err := client.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("failed to list tools: %w", err)
	}

	// Remove old tools from this server
	newTools := make([]tools.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		if mcpTool, ok := t.(*MCPTool); ok {
			if mcpTool.GetServerName() != name {
				newTools = append(newTools, t)
			}
		} else {
			newTools = append(newTools, t)
		}
	}

	// Add updated tools
	for _, t := range mcpTools {
		tool := NewMCPTool(client, name, cfg.ToolPrefix, t)
		newTools = append(newTools, tool)
	}

	m.tools = newTools

	logging.Debug("MCP tools refreshed", "name", name, "tools", len(mcpTools))
	return nil
}

// StartHealthCheck starts a background goroutine that periodically checks
// server health and attempts to reconnect unhealthy servers.
func (m *Manager) StartHealthCheck(ctx context.Context, interval time.Duration) {
	m.healMu.Lock()
	defer m.healMu.Unlock()

	// Stop existing health check if running
	if m.healingCancel != nil {
		m.healingCancel()
		if m.healingDone != nil {
			<-m.healingDone
		}
	}

	if interval <= 0 {
		interval = 30 * time.Second
	}

	healCtx, cancel := context.WithCancel(ctx)
	m.healingCancel = cancel
	m.healingDone = make(chan struct{})

	go m.healthCheckLoop(healCtx, interval)
}

// StopHealthCheck stops the background health check goroutine.
func (m *Manager) StopHealthCheck() {
	m.healMu.Lock()
	cancel := m.healingCancel
	done := m.healingDone
	m.healingCancel = nil
	m.healingDone = nil
	m.healMu.Unlock()

	if cancel != nil {
		cancel()
		if done != nil {
			<-done
		}
	}
}

const maxReconnectAttempts = 10

// healthCheckLoop runs periodically to check health and reconnect unhealthy servers.
func (m *Manager) healthCheckLoop(ctx context.Context, interval time.Duration) {
	defer close(m.healingDone)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckHealth(ctx)
			m.tryReconnectUnhealthy(ctx)
		}
	}
}

// tryReconnectUnhealthy attempts to reconnect servers marked as unhealthy.
func (m *Manager) tryReconnectUnhealthy(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, h := range m.health {
		if h.Healthy {
			continue
		}

		if h.MaxReconnects == 0 {
			h.MaxReconnects = maxReconnectAttempts
		}

		if h.ReconnectAttempts >= h.MaxReconnects {
			continue // Gave up on this server
		}

		cfg, exists := m.servers[name]
		if !exists {
			continue
		}

		h.ReconnectAttempts++
		logging.Info("MCP auto-reconnect attempt",
			"name", name,
			"attempt", h.ReconnectAttempts,
			"max", h.MaxReconnects)

		// Close old client if exists
		if oldClient, ok := m.clients[name]; ok {
			oldClient.Close()
			delete(m.clients, name)

			// Remove old tools from this server
			newTools := make([]tools.Tool, 0, len(m.tools))
			for _, t := range m.tools {
				if mcpTool, ok := t.(*MCPTool); ok {
					if mcpTool.GetServerName() != name {
						newTools = append(newTools, t)
					}
				} else {
					newTools = append(newTools, t)
				}
			}
			m.tools = newTools
		}

		// Try to reconnect
		if err := m.connectServer(ctx, cfg); err != nil {
			h.LastReconnectError = err.Error()
			logging.Warn("MCP auto-reconnect failed",
				"name", name,
				"attempt", h.ReconnectAttempts,
				"error", err)
			continue
		}

		// Reconnect succeeded
		h.Healthy = true
		h.ConsecutiveFails = 0
		h.ReconnectAttempts = 0
		h.LastReconnectError = ""
		logging.Info("MCP server auto-reconnected successfully", "name", name)
	}
}
