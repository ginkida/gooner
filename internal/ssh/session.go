package ssh

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gooner/internal/logging"
)

// SessionInfo represents information about an active SSH session.
type SessionInfo struct {
	Key       string    // "user@host:port"
	Host      string
	Port      int
	User      string
	Connected bool
	LastUse   time.Time
	IdleTime  time.Duration
}

// SessionManager manages persistent SSH sessions with connection pooling.
type SessionManager struct {
	sessions map[string]*SSHClient // key: "user@host:port"
	mu       sync.RWMutex
	maxIdle  time.Duration // Auto-close idle sessions
	stopCh   chan struct{} // Stop cleanup goroutine
}

// NewSessionManager creates a session manager.
func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*SSHClient),
		maxIdle:  15 * time.Minute, // Default 15 minute idle timeout
		stopCh:   make(chan struct{}),
	}

	// Start cleanup goroutine
	go sm.cleanupLoop()

	return sm
}

// SetMaxIdle sets the maximum idle time before a session is closed.
func (m *SessionManager) SetMaxIdle(d time.Duration) {
	m.mu.Lock()
	m.maxIdle = d
	m.mu.Unlock()
}

// GetOrCreate returns existing session or creates new one.
func (m *SessionManager) GetOrCreate(ctx context.Context, config *SSHConfig) (*SSHClient, error) {
	// Build session key
	key := fmt.Sprintf("%s@%s:%d", config.User, config.Host, config.Port)

	// Check for existing session
	m.mu.RLock()
	client, exists := m.sessions[key]
	m.mu.RUnlock()

	if exists {
		// Verify connection is still alive
		if client.IsConnected() {
			logging.Debug("reusing existing SSH session", "key", key)
			return client, nil
		}
		// Connection dead, remove and create new
		logging.Debug("existing SSH session dead, reconnecting", "key", key)
		m.Close(key)
	}

	// Create new session
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if client, exists = m.sessions[key]; exists && client.IsConnected() {
		return client, nil
	}

	// Create new client
	client = NewSSHClient(config)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}

	m.sessions[key] = client
	logging.Info("created new SSH session", "key", key)
	return client, nil
}

// Get returns an existing session if it exists and is connected.
func (m *SessionManager) Get(key string) (*SSHClient, bool) {
	m.mu.RLock()
	client, exists := m.sessions[key]
	m.mu.RUnlock()

	if !exists {
		return nil, false
	}

	if !client.IsConnected() {
		return nil, false
	}

	return client, true
}

// Close closes a specific session.
func (m *SessionManager) Close(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, exists := m.sessions[key]
	if !exists {
		return fmt.Errorf("session not found: %s", key)
	}

	err := client.Close()
	delete(m.sessions, key)
	logging.Info("closed SSH session", "key", key)
	return err
}

// CloseAll closes all sessions.
func (m *SessionManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, client := range m.sessions {
		if err := client.Close(); err != nil {
			logging.Warn("error closing SSH session", "key", key, "error", err)
		}
	}
	m.sessions = make(map[string]*SSHClient)
	logging.Info("closed all SSH sessions")
}

// CleanupIdle closes sessions idle longer than maxIdle.
func (m *SessionManager) CleanupIdle() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleaned := 0
	now := time.Now()

	for key, client := range m.sessions {
		idleTime := now.Sub(client.LastUse())
		if idleTime > m.maxIdle {
			logging.Info("closing idle SSH session", "key", key, "idle", idleTime)
			client.Close()
			delete(m.sessions, key)
			cleaned++
		}
	}

	return cleaned
}

// cleanupLoop periodically cleans up idle sessions.
func (m *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cleaned := m.CleanupIdle()
			if cleaned > 0 {
				logging.Debug("cleaned up idle SSH sessions", "count", cleaned)
			}
		case <-m.stopCh:
			return
		}
	}
}

// Stop stops the cleanup goroutine and closes all sessions.
func (m *SessionManager) Stop() {
	close(m.stopCh)
	m.CloseAll()
}

// List returns all active sessions.
func (m *SessionManager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	infos := make([]SessionInfo, 0, len(m.sessions))

	for key, client := range m.sessions {
		lastUse := client.LastUse()
		infos = append(infos, SessionInfo{
			Key:       key,
			Host:      client.config.Host,
			Port:      client.config.Port,
			User:      client.config.User,
			Connected: client.IsConnected(),
			LastUse:   lastUse,
			IdleTime:  now.Sub(lastUse),
		})
	}

	return infos
}

// Count returns the number of active sessions.
func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}
