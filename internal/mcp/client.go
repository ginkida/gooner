package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gokin/internal/logging"
)

// Client handles JSON-RPC communication with an MCP server.
type Client struct {
	transport   Transport
	serverInfo  *ServerInfo
	tools       []*ToolInfo
	resources   []*Resource

	// Connection state
	initialized bool
	mu          sync.RWMutex

	// Request tracking
	nextID     int64
	pending    map[int64]chan *JSONRPCMessage
	pendingMu  sync.Mutex

	// Configuration
	serverName string
	config     *ServerConfig

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// NewClient creates a new MCP client with the specified transport.
func NewClient(cfg *ServerConfig) (*Client, error) {
	var transport Transport
	var err error

	switch cfg.Transport {
	case "stdio":
		transport, err = NewStdioTransport(cfg.Command, cfg.Args, cfg.Env)
	case "http":
		transport, err = NewHTTPTransport(cfg.URL, cfg.Headers, cfg.Timeout)
	default:
		return nil, fmt.Errorf("unknown transport type: %s", cfg.Transport)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &Client{
		transport:  transport,
		serverName: cfg.Name,
		config:     cfg,
		pending:    make(map[int64]chan *JSONRPCMessage),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}

	// Start message receiver goroutine
	go c.receiveLoop()

	return c, nil
}

// receiveLoop reads messages from the transport and routes them.
func (c *Client) receiveLoop() {
	defer close(c.done)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msg, err := c.transport.Receive()
		if err != nil {
			if c.ctx.Err() != nil {
				// Context cancelled, expected
				return
			}
			logging.Warn("MCP receive error", "error", err)
			return
		}

		c.handleMessage(msg)
	}
}

// handleMessage routes an incoming message to the appropriate handler.
func (c *Client) handleMessage(msg *JSONRPCMessage) {
	if msg.IsResponse() {
		// Route to pending request
		id, ok := msg.ID.(float64) // JSON numbers are float64
		if !ok {
			logging.Warn("MCP response with invalid ID type", "id", msg.ID)
			return
		}

		c.pendingMu.Lock()
		ch, exists := c.pending[int64(id)]
		if exists {
			delete(c.pending, int64(id))
		}
		c.pendingMu.Unlock()

		if exists {
			select {
			case ch <- msg:
			default:
				logging.Warn("MCP response channel full", "id", id)
			}
		} else {
			logging.Warn("MCP response for unknown request", "id", id)
		}
	} else if msg.IsNotification() {
		// Handle notifications
		logging.Debug("MCP notification received", "method", msg.Method)
		// Could dispatch to notification handlers here
	}
}

// request sends a request and waits for a response.
func (c *Client) request(ctx context.Context, method string, params any) (*JSONRPCMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)

	// Create response channel
	respCh := make(chan *JSONRPCMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	// Clean up on return
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	// Build and send request
	msg := &JSONRPCMessage{
		ID:     id,
		Method: method,
		Params: params,
	}

	if err := c.transport.Send(msg); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Wait for response with timeout
	timeout := c.config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("request timeout after %v", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// notify sends a notification (no response expected).
func (c *Client) notify(method string, params any) error {
	msg := &JSONRPCMessage{
		Method: method,
		Params: params,
	}
	return c.transport.Send(msg)
}

// Initialize initializes the connection with the MCP server.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initialized {
		return nil
	}

	// Send initialize request
	params := &InitializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientInfo: &ClientInfo{
			Name:    "gokin",
			Version: "1.0.0",
		},
		Capabilities: map[string]any{}, // We don't advertise any client capabilities yet
	}

	resp, err := c.request(ctx, MethodInitialize, params)
	if err != nil {
		return fmt.Errorf("initialize failed: %w", err)
	}

	// Parse result
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Errorf("failed to marshal initialize result: %w", err)
	}

	var result InitializeResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return fmt.Errorf("failed to parse initialize result: %w", err)
	}

	c.serverInfo = result.ServerInfo

	// Send initialized notification
	if err := c.notify(MethodInitialized, nil); err != nil {
		return fmt.Errorf("failed to send initialized notification: %w", err)
	}

	c.initialized = true

	logging.Info("MCP server initialized",
		"name", c.serverName,
		"server", c.serverInfo.Name,
		"version", c.serverInfo.Version)

	return nil
}

// ListTools retrieves the list of tools from the server.
func (c *Client) ListTools(ctx context.Context) ([]*ToolInfo, error) {
	c.mu.RLock()
	if !c.initialized {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not initialized")
	}
	c.mu.RUnlock()

	resp, err := c.request(ctx, MethodToolsList, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list failed: %w", err)
	}

	// Parse result
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tools result: %w", err)
	}

	var result ListToolsResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to parse tools result: %w", err)
	}

	c.mu.Lock()
	c.tools = result.Tools
	c.mu.Unlock()

	logging.Debug("MCP tools listed",
		"server", c.serverName,
		"count", len(result.Tools))

	return result.Tools, nil
}

// CallTool calls a tool on the MCP server.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	c.mu.RLock()
	if !c.initialized {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not initialized")
	}
	c.mu.RUnlock()

	params := &CallToolParams{
		Name:      name,
		Arguments: args,
	}

	resp, err := c.request(ctx, MethodToolsCall, params)
	if err != nil {
		return nil, fmt.Errorf("tools/call failed: %w", err)
	}

	// Parse result
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal call result: %w", err)
	}

	var result CallToolResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to parse call result: %w", err)
	}

	logging.Debug("MCP tool called",
		"server", c.serverName,
		"tool", name,
		"is_error", result.IsError)

	return &result, nil
}

// ListResources retrieves the list of resources from the server.
func (c *Client) ListResources(ctx context.Context) ([]*Resource, error) {
	c.mu.RLock()
	if !c.initialized {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not initialized")
	}
	c.mu.RUnlock()

	resp, err := c.request(ctx, MethodResourcesList, nil)
	if err != nil {
		return nil, fmt.Errorf("resources/list failed: %w", err)
	}

	// Parse result
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal resources result: %w", err)
	}

	var result ListResourcesResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to parse resources result: %w", err)
	}

	c.mu.Lock()
	c.resources = result.Resources
	c.mu.Unlock()

	logging.Debug("MCP resources listed",
		"server", c.serverName,
		"count", len(result.Resources))

	return result.Resources, nil
}

// GetServerInfo returns information about the connected server.
func (c *Client) GetServerInfo() *ServerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverInfo
}

// GetServerName returns the configured server name.
func (c *Client) GetServerName() string {
	return c.serverName
}

// IsInitialized returns whether the client has been initialized.
func (c *Client) IsInitialized() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.initialized
}

// Close closes the client and releases resources.
func (c *Client) Close() error {
	c.cancel()

	// Wait for receive loop to finish
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		logging.Warn("MCP client receive loop did not stop in time")
	}

	// Close transport
	if err := c.transport.Close(); err != nil {
		return fmt.Errorf("failed to close transport: %w", err)
	}

	logging.Debug("MCP client closed", "server", c.serverName)
	return nil
}

// Ping sends a ping request to verify the connection is alive.
func (c *Client) Ping(ctx context.Context) error {
	c.mu.RLock()
	if !c.initialized {
		c.mu.RUnlock()
		return fmt.Errorf("client not initialized")
	}
	c.mu.RUnlock()

	_, err := c.request(ctx, MethodPing, nil)
	return err
}
