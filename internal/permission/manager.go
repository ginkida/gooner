package permission

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
)

// PromptHandler is a function that prompts the user for permission.
// It receives a request and returns the user's decision.
type PromptHandler func(ctx context.Context, req *Request) (Decision, error)

// Manager handles permission checks and session caching.
type Manager struct {
	rules   *Rules
	enabled bool

	// Session cache for "allow for session" and "deny for session" decisions
	sessionCache map[string]Decision

	// Prompt handler for asking the user
	promptHandler PromptHandler

	// LRU cache limits to prevent unbounded growth
	maxCacheEntries int

	mu sync.RWMutex
}

const (
	// DefaultMaxCacheEntries is the default maximum number of cached permission decisions
	DefaultMaxCacheEntries = 1000
)

// NewManager creates a new permission manager.
func NewManager(rules *Rules, enabled bool) *Manager {
	if rules == nil {
		rules = DefaultRules()
	}

	return &Manager{
		rules:           rules,
		enabled:         enabled,
		sessionCache:    make(map[string]Decision),
		maxCacheEntries: DefaultMaxCacheEntries,
	}
}

// SetPromptHandler sets the function to call when user input is needed.
func (m *Manager) SetPromptHandler(handler PromptHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.promptHandler = handler
}

// cacheKey generates a cache key for a tool invocation.
// For sensitive tools, the key includes a hash of relevant arguments.
func (m *Manager) cacheKey(toolName string, args map[string]any) string {
	switch toolName {
	case "bash":
		// Include command hash to differentiate different bash commands
		if cmd, ok := args["command"].(string); ok {
			hash := sha256.Sum256([]byte(cmd))
			return fmt.Sprintf("%s:%x", toolName, hash[:8])
		}
	case "write", "edit":
		// Include file path to differentiate different file operations
		if path, ok := args["path"].(string); ok {
			return fmt.Sprintf("%s:%s", toolName, path)
		}
		if path, ok := args["file_path"].(string); ok {
			return fmt.Sprintf("%s:%s", toolName, path)
		}
	}
	return toolName
}

// Check checks if a tool is allowed to execute.
// Returns a Response indicating whether execution is allowed.
func (m *Manager) Check(ctx context.Context, toolName string, args map[string]any) (*Response, error) {
	// If permissions are disabled, allow everything
	if !m.enabled {
		return &Response{Allowed: true, Decision: DecisionAllow}, nil
	}

	// Generate cache key that may include args for sensitive tools
	key := m.cacheKey(toolName, args)

	// Check session cache first
	m.mu.RLock()
	if decision, ok := m.sessionCache[key]; ok {
		m.mu.RUnlock()
		switch decision {
		case DecisionAllowSession:
			return &Response{Allowed: true, Decision: decision}, nil
		case DecisionDenySession:
			return &Response{
				Allowed:  false,
				Decision: decision,
				Reason:   "Denied for session",
			}, nil
		}
	} else {
		m.mu.RUnlock()
	}

	// Get policy from rules
	policy := m.rules.GetPolicy(toolName)

	switch policy {
	case LevelAllow:
		return &Response{Allowed: true, Decision: DecisionAllow}, nil

	case LevelDeny:
		return &Response{
			Allowed:  false,
			Decision: DecisionDeny,
			Reason:   "Tool is not permitted by configuration",
		}, nil

	case LevelAsk:
		return m.askUser(ctx, toolName, args)
	}

	// Default to asking
	return m.askUser(ctx, toolName, args)
}

// askUser prompts the user for permission.
func (m *Manager) askUser(ctx context.Context, toolName string, args map[string]any) (*Response, error) {
	m.mu.RLock()
	handler := m.promptHandler
	m.mu.RUnlock()

	if handler == nil {
		// No handler set, default to allow (backwards compatibility)
		return &Response{Allowed: true, Decision: DecisionAllow}, nil
	}

	// Create permission request
	req := NewRequest(toolName, args)

	// Ask the user
	decision, err := handler(ctx, req)
	if err != nil {
		return &Response{
			Allowed:  false,
			Decision: DecisionDeny,
			Reason:   err.Error(),
		}, err
	}

	// Generate cache key for session decisions
	key := m.cacheKey(toolName, args)

	// Handle the decision
	switch decision {
	case DecisionAllow:
		return &Response{Allowed: true, Decision: decision}, nil

	case DecisionAllowSession:
		m.rememberKey(key, decision)
		return &Response{Allowed: true, Decision: decision}, nil

	case DecisionDeny:
		return &Response{
			Allowed:  false,
			Decision: decision,
			Reason:   "Denied by user",
		}, nil

	case DecisionDenySession:
		m.rememberKey(key, decision)
		return &Response{
			Allowed:  false,
			Decision: decision,
			Reason:   "Denied for session",
		}, nil

	default:
		return &Response{
			Allowed:  false,
			Decision: DecisionDeny,
			Reason:   "Unknown decision",
		}, nil
	}
}

// rememberKey stores a session-level decision for a cache key.
// Implements LRU eviction when cache exceeds max entries.
func (m *Manager) rememberKey(key string, decision Decision) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if we need to evict entries
	if len(m.sessionCache) >= m.maxCacheEntries {
		// Simple eviction strategy: remove half of the oldest entries
		// In production, you might want a proper LRU with access timestamps
		evictCount := m.maxCacheEntries / 2
		count := 0
		for k := range m.sessionCache {
			if count >= evictCount {
				break
			}
			delete(m.sessionCache, k)
			count++
		}
	}

	m.sessionCache[key] = decision
}

// Remember stores a session-level decision for a tool.
// Deprecated: Use RememberWithArgs for sensitive tools.
func (m *Manager) Remember(toolName string, decision Decision) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionCache[toolName] = decision
}

// RememberWithArgs stores a session-level decision for a tool with args.
func (m *Manager) RememberWithArgs(toolName string, args map[string]any, decision Decision) {
	key := m.cacheKey(toolName, args)
	m.rememberKey(key, decision)
}

// Forget removes a session-level decision for a tool.
func (m *Manager) Forget(toolName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessionCache, toolName)
}

// ForgetWithArgs removes a session-level decision for a tool with args.
func (m *Manager) ForgetWithArgs(toolName string, args map[string]any) {
	key := m.cacheKey(toolName, args)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessionCache, key)
}

// ClearSession clears all session-level decisions.
func (m *Manager) ClearSession() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionCache = make(map[string]Decision)
}

// IsEnabled returns whether the permission system is enabled.
func (m *Manager) IsEnabled() bool {
	return m.enabled
}

// SetEnabled enables or disables the permission system.
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enabled
}

// GetRules returns the current rules.
func (m *Manager) GetRules() *Rules {
	return m.rules
}

// SetRules sets new rules.
func (m *Manager) SetRules(rules *Rules) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = rules
}
