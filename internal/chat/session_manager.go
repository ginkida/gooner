package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gokin/internal/logging"
)

// SessionManager handles automatic session persistence.
type SessionManager struct {
	session        *Session
	historyManager *HistoryManager
	config         SessionManagerConfig
	mu             sync.Mutex
	lastSaveTime   time.Time
	saveTimer      *time.Timer
	stopChan       chan struct{}
}

// SessionManagerConfig configures session persistence behavior.
type SessionManagerConfig struct {
	Enabled         bool          // Enable auto-save/load
	SaveInterval    time.Duration // Periodic save interval (default 2m)
	AutoLoad        bool          // Auto-load last session on startup
	MaxSessionAge   time.Duration // Maximum age for sessions before cleanup (default: 30 days)
	MaxSessionCount int           // Maximum number of sessions to keep (default: 50)
}

// DefaultSessionManagerConfig returns default configuration.
func DefaultSessionManagerConfig() SessionManagerConfig {
	return SessionManagerConfig{
		Enabled:         true,
		SaveInterval:    2 * time.Minute,
		AutoLoad:        true,
		MaxSessionAge:   30 * 24 * time.Hour, // 30 days
		MaxSessionCount: 50,                  // Keep max 50 sessions
	}
}

// NewSessionManager creates a new session manager.
func NewSessionManager(session *Session, config SessionManagerConfig) (*SessionManager, error) {
	historyMgr, err := NewHistoryManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create history manager: %w", err)
	}

	sm := &SessionManager{
		session:        session,
		historyManager: historyMgr,
		config:         config,
		stopChan:       make(chan struct{}),
	}

	return sm, nil
}

// Start initializes the session manager.
func (sm *SessionManager) Start(ctx context.Context) {
	if !sm.config.Enabled {
		return
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Clean up old sessions in background (non-blocking)
	go sm.CleanupOldSessions()

	// Start periodic save timer
	sm.saveTimer = time.AfterFunc(sm.config.SaveInterval, func() {
		sm.periodicSave()
	})

	// Start goroutine to handle stop signal
	go func() {
		<-sm.stopChan
		sm.mu.Lock()
		if sm.saveTimer != nil {
			sm.saveTimer.Stop()
		}
		sm.mu.Unlock()
	}()
}

// Stop gracefully shuts down the session manager.
func (sm *SessionManager) Stop() {
	close(sm.stopChan)

	// Final save on shutdown
	if err := sm.Save(); err != nil {
		logging.Warn("failed to save session on shutdown", "error", err)
	}
}

// Save saves the current session state.
func (sm *SessionManager) Save() error {
	if !sm.config.Enabled {
		return nil
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if err := sm.historyManager.SaveFull(sm.session); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	sm.lastSaveTime = time.Now()
	logging.Debug("session saved", "session_id", sm.session.ID, "messages", sm.session.MessageCount())
	return nil
}

// LoadLast attempts to load the most recent session.
// Returns the loaded session state and info about it, or error if none found.
func (sm *SessionManager) LoadLast() (*SessionState, *SessionInfo, error) {
	if !sm.config.Enabled || !sm.config.AutoLoad {
		return nil, nil, nil
	}

	sessions, err := sm.historyManager.ListSessions()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(sessions) == 0 {
		return nil, nil, nil // No sessions to load
	}

	// Find the most recent session
	var latest *SessionInfo
	for i := range sessions {
		if latest == nil || sessions[i].LastActive.After(latest.LastActive) {
			latest = &sessions[i]
		}
	}

	if latest == nil {
		return nil, nil, nil
	}

	// Load the session state
	state, err := sm.historyManager.LoadFull(latest.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load session %s: %w", latest.ID, err)
	}

	logging.Info("loaded previous session",
		"session_id", latest.ID,
		"messages", latest.MessageCount,
		"last_active", latest.LastActive.Format("2006-01-02 15:04:05"))

	return state, latest, nil
}

// RestoreFromState restores the current session from a saved state.
func (sm *SessionManager) RestoreFromState(state *SessionState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if err := sm.session.RestoreFromState(state); err != nil {
		return fmt.Errorf("failed to restore session: %w", err)
	}

	sm.lastSaveTime = time.Now()
	return nil
}

// SaveAfterMessage saves the session after a message is added.
// This is called automatically after each user message and AI response.
func (sm *SessionManager) SaveAfterMessage() error {
	if !sm.config.Enabled {
		return nil
	}

	// Save immediately after message
	return sm.Save()
}

// periodicSave performs periodic save and resets the timer.
func (sm *SessionManager) periodicSave() {
	if err := sm.Save(); err != nil {
		logging.Warn("periodic session save failed", "error", err)
	}

	// Reset timer for next save
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.saveTimer != nil {
		sm.saveTimer.Reset(sm.config.SaveInterval)
	}
}

// GetLastSaveTime returns when the session was last saved.
func (sm *SessionManager) GetLastSaveTime() time.Time {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.lastSaveTime
}

// ClearCurrentSession clears the current session from disk.
// Useful when starting a fresh conversation.
func (sm *SessionManager) ClearCurrentSession() error {
	if !sm.config.Enabled {
		return nil
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if err := sm.historyManager.DeleteSession(sm.session.ID); err != nil {
		// Ignore error if file doesn't exist
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete session: %w", err)
		}
	}

	return nil
}

// getCurrentSessionPath returns the path to the current session file.
func (sm *SessionManager) getCurrentSessionPath() string {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return ""
	}
	return filepath.Join(sessionsDir, sm.session.ID+".json")
}

// hasRecentSession checks if there's a recent session file.
func (sm *SessionManager) hasRecentSession() bool {
	path := sm.getCurrentSessionPath()
	if path == "" {
		return false
	}

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	// Consider session recent if modified within last hour
	return time.Since(info.ModTime()) < time.Hour
}

// CleanupOldSessions removes old sessions based on age and count limits.
// This runs in the background and does not block the main application.
func (sm *SessionManager) CleanupOldSessions() error {
	if sm.historyManager == nil {
		return nil
	}

	sessions, err := sm.historyManager.ListSessions()
	if err != nil {
		logging.Debug("failed to list sessions for cleanup", "error", err)
		return err
	}

	if len(sessions) == 0 {
		return nil
	}

	// Sort sessions by LastActive (newest first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})

	cutoff := time.Now().Add(-sm.config.MaxSessionAge)
	deletedCount := 0

	// Delete sessions that are too old
	for i, sess := range sessions {
		// Always keep current session
		if sess.ID == sm.session.ID {
			continue
		}

		shouldDelete := false

		// Delete if older than MaxSessionAge
		if sess.LastActive.Before(cutoff) {
			shouldDelete = true
		}

		// Delete if we have more than MaxSessionCount (keeping newest)
		if i >= sm.config.MaxSessionCount {
			shouldDelete = true
		}

		if shouldDelete {
			if err := sm.historyManager.DeleteSession(sess.ID); err != nil {
				logging.Debug("failed to delete old session", "session_id", sess.ID, "error", err)
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 {
		logging.Info("cleaned up old sessions", "deleted", deletedCount)
	}

	return nil
}
