package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// HistoryEntry represents a saved history entry.
type HistoryEntry struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// HistoryFile represents a saved session history.
type HistoryFile struct {
	SessionID string         `json:"session_id"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time"`
	Entries   []HistoryEntry `json:"entries"`
}

// HistoryManager manages session history persistence.
type HistoryManager struct {
	dataDir string
}

// NewHistoryManager creates a new history manager.
func NewHistoryManager() (*HistoryManager, error) {
	// Get data directory
	dataDir, err := getDataDir()
	if err != nil {
		return nil, err
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	return &HistoryManager{
		dataDir: dataDir,
	}, nil
}

// Save saves a session history to disk.
func (m *HistoryManager) Save(session *Session) error {
	history := session.GetHistory()

	file := HistoryFile{
		SessionID: session.ID,
		StartTime: session.StartTime,
		EndTime:   time.Now(),
		Entries:   make([]HistoryEntry, 0),
	}

	for _, content := range history {
		var text string
		for _, part := range content.Parts {
			if part.Text != "" {
				text += part.Text
			}
		}

		file.Entries = append(file.Entries, HistoryEntry{
			Role:      string(content.Role),
			Content:   text,
			Timestamp: time.Now(),
		})
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}

	// Write file
	filename := filepath.Join(m.dataDir, session.ID+".json")
	return os.WriteFile(filename, data, 0644)
}

// Load loads a session history from disk.
func (m *HistoryManager) Load(sessionID string) (*HistoryFile, error) {
	filename := filepath.Join(m.dataDir, sessionID+".json")

	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var file HistoryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	return &file, nil
}

// List lists all saved sessions.
func (m *HistoryManager) List() ([]string, error) {
	entries, err := os.ReadDir(m.dataDir)
	if err != nil {
		return nil, err
	}

	var sessions []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			sessions = append(sessions, entry.Name()[:len(entry.Name())-5])
		}
	}

	return sessions, nil
}

// Delete deletes a session history.
func (m *HistoryManager) Delete(sessionID string) error {
	filename := filepath.Join(m.dataDir, sessionID+".json")
	return os.Remove(filename)
}

// getDataDir returns the data directory for history storage.
func getDataDir() (string, error) {
	// Check XDG_DATA_HOME first
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "gooner", "history"), nil
	}

	// Fall back to ~/.local/share
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, ".local", "share", "gooner", "history"), nil
}

// getSessionsDir returns the data directory for full session storage.
func getSessionsDir() (string, error) {
	// Check XDG_DATA_HOME first
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "gooner", "sessions"), nil
	}

	// Fall back to ~/.local/share
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, ".local", "share", "gooner", "sessions"), nil
}

// SaveFull saves a complete session state including all content.
func (m *HistoryManager) SaveFull(session *Session) error {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return err
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return err
	}

	state := session.GetState()

	// Marshal to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	// Write file
	filename := filepath.Join(sessionsDir, session.ID+".json")
	return os.WriteFile(filename, data, 0644)
}

// LoadFull loads a complete session state.
func (m *HistoryManager) LoadFull(sessionID string) (*SessionState, error) {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return nil, err
	}

	filename := filepath.Join(sessionsDir, sessionID+".json")

	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// ListSessions returns information about all saved sessions.
func (m *HistoryManager) ListSessions() ([]SessionInfo, error) {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return nil, err
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, err
	}

	var sessions []SessionInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		sessionID := entry.Name()[:len(entry.Name())-5]
		state, err := m.LoadFull(sessionID)
		if err != nil {
			continue // Skip invalid files
		}

		sessions = append(sessions, SessionInfo{
			ID:           state.ID,
			StartTime:    state.StartTime,
			LastActive:   state.LastActive,
			Summary:      state.Summary,
			MessageCount: len(state.History),
		})
	}

	return sessions, nil
}

// DeleteSession deletes a saved session.
func (m *HistoryManager) DeleteSession(sessionID string) error {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return err
	}

	filename := filepath.Join(sessionsDir, sessionID+".json")
	return os.Remove(filename)
}
