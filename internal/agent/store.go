package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AgentStore provides persistent storage for agent states.
type AgentStore struct {
	dir string
	mu  sync.RWMutex
}

// NewAgentStore creates a new agent store.
// configDir should be the base config directory (e.g., ~/.config/gooner).
func NewAgentStore(configDir string) (*AgentStore, error) {
	dir := filepath.Join(configDir, "agents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agents directory: %w", err)
	}

	return &AgentStore{
		dir: dir,
	}, nil
}

// Save saves an agent's state to disk.
func (s *AgentStore) Save(agent *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := agent.GetState()
	return s.saveState(state)
}

// SaveState saves an agent state directly.
func (s *AgentStore) SaveState(state *AgentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveState(state)
}

// saveState is the internal save implementation.
func (s *AgentStore) saveState(state *AgentState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal agent state: %w", err)
	}

	filePath := filepath.Join(s.dir, state.ID+".json")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}

	return nil
}

// Load loads an agent state from disk.
func (s *AgentStore) Load(agentID string) (*AgentState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	filePath := filepath.Join(s.dir, agentID+".json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("agent not found: %s", agentID)
		}
		return nil, fmt.Errorf("failed to read agent state: %w", err)
	}

	var state AgentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal agent state: %w", err)
	}

	return &state, nil
}

// Delete removes an agent state from disk.
func (s *AgentStore) Delete(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filePath := filepath.Join(s.dir, agentID+".json")
	if err := os.Remove(filePath); err != nil {
		if os.IsNotExist(err) {
			return nil // Already deleted
		}
		return fmt.Errorf("failed to delete agent state: %w", err)
	}

	return nil
}

// List returns all stored agent IDs.
func (s *AgentStore) List() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read agents directory: %w", err)
	}

	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			id := entry.Name()[:len(entry.Name())-5] // Remove .json extension
			ids = append(ids, id)
		}
	}

	return ids, nil
}

// Cleanup removes agent states older than the specified duration.
func (s *AgentStore) Cleanup(maxAge time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("failed to read agents directory: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	cleaned := 0

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		filePath := filepath.Join(s.dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filePath); err == nil {
				cleaned++
			}
		}
	}

	return cleaned, nil
}

// Exists checks if an agent state exists.
func (s *AgentStore) Exists(agentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	filePath := filepath.Join(s.dir, agentID+".json")
	_, err := os.Stat(filePath)
	return err == nil
}
