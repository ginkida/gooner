package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	maxHistoryEntries = 100
	historyFileName   = "command_history.json"
)

// HistoryEntry represents a single command usage entry.
type HistoryEntry struct {
	Command   string    `json:"command"`
	Timestamp time.Time `json:"timestamp"`
	Count     int       `json:"count"`
}

// CommandHistory manages the history of used commands.
type CommandHistory struct {
	entries  map[string]*HistoryEntry
	filePath string
	mu       sync.RWMutex
}

// NewCommandHistory creates a new CommandHistory.
func NewCommandHistory() *CommandHistory {
	ch := &CommandHistory{
		entries: make(map[string]*HistoryEntry),
	}

	// Determine file path
	configDir, err := getConfigDir()
	if err == nil {
		ch.filePath = filepath.Join(configDir, historyFileName)
		_ = ch.load()
	}

	return ch
}

// getConfigDir returns the config directory path.
func getConfigDir() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "gooner"), nil
}

// RecordUsage records that a command was used.
func (ch *CommandHistory) RecordUsage(command string) {
	ch.mu.Lock()
	defer ch.mu.Unlock()

	entry, exists := ch.entries[command]
	if exists {
		entry.Timestamp = time.Now()
		entry.Count++
	} else {
		ch.entries[command] = &HistoryEntry{
			Command:   command,
			Timestamp: time.Now(),
			Count:     1,
		}
	}

	// Prune if too many entries
	if len(ch.entries) > maxHistoryEntries {
		ch.pruneOldest()
	}

	// Save asynchronously
	go ch.save()
}

// GetRecentCommands returns the most recently used commands.
func (ch *CommandHistory) GetRecentCommands(limit int) []string {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	// Convert to slice for sorting
	entries := make([]*HistoryEntry, 0, len(ch.entries))
	for _, e := range ch.entries {
		entries = append(entries, e)
	}

	// Sort by timestamp (most recent first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.After(entries[j].Timestamp)
	})

	// Extract command names
	result := make([]string, 0, limit)
	for i, e := range entries {
		if i >= limit {
			break
		}
		result = append(result, e.Command)
	}

	return result
}

// GetUsageCount returns how many times a command has been used.
func (ch *CommandHistory) GetUsageCount(command string) int {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	if entry, exists := ch.entries[command]; exists {
		return entry.Count
	}
	return 0
}

// IsRecent checks if a command is in the recent history.
func (ch *CommandHistory) IsRecent(command string, limit int) bool {
	recent := ch.GetRecentCommands(limit)
	for _, c := range recent {
		if c == command {
			return true
		}
	}
	return false
}

// pruneOldest removes the oldest entries to stay under the limit.
func (ch *CommandHistory) pruneOldest() {
	// Convert to slice for sorting
	entries := make([]*HistoryEntry, 0, len(ch.entries))
	for _, e := range ch.entries {
		entries = append(entries, e)
	}

	// Sort by timestamp (oldest first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})

	// Remove oldest entries
	toRemove := len(entries) - maxHistoryEntries
	for i := 0; i < toRemove && i < len(entries); i++ {
		delete(ch.entries, entries[i].Command)
	}
}

// load loads the history from disk.
func (ch *CommandHistory) load() error {
	if ch.filePath == "" {
		return nil
	}

	data, err := os.ReadFile(ch.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var entries []*HistoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	for _, e := range entries {
		ch.entries[e.Command] = e
	}

	return nil
}

// save saves the history to disk.
func (ch *CommandHistory) save() error {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	if ch.filePath == "" {
		return nil
	}

	// Ensure directory exists
	dir := filepath.Dir(ch.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Convert to slice
	entries := make([]*HistoryEntry, 0, len(ch.entries))
	for _, e := range ch.entries {
		entries = append(entries, e)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ch.filePath, data, 0644)
}
