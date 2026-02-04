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
// configDir should be the base config directory (e.g., ~/.config/gokin).
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

// SaveCheckpoint saves an agent checkpoint to disk.
func (s *AgentStore) SaveCheckpoint(cp *AgentCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create checkpoints subdirectory
	checkpointsDir := filepath.Join(s.dir, "checkpoints")
	if err := os.MkdirAll(checkpointsDir, 0755); err != nil {
		return fmt.Errorf("failed to create checkpoints directory: %w", err)
	}

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint: %w", err)
	}

	filePath := filepath.Join(checkpointsDir, cp.CheckpointID+".json")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write checkpoint: %w", err)
	}

	return nil
}

// LoadCheckpoint loads a checkpoint from disk.
func (s *AgentStore) LoadCheckpoint(checkpointID string) (*AgentCheckpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	filePath := filepath.Join(s.dir, "checkpoints", checkpointID+".json")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("checkpoint not found: %s", checkpointID)
		}
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var cp AgentCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal checkpoint: %w", err)
	}

	return &cp, nil
}

// ListCheckpoints returns all checkpoint IDs for an agent.
func (s *AgentStore) ListCheckpoints(agentID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	checkpointsDir := filepath.Join(s.dir, "checkpoints")
	entries, err := os.ReadDir(checkpointsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read checkpoints directory: %w", err)
	}

	var ids []string
	prefix := agentID + "-"
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			name := entry.Name()[:len(entry.Name())-5]
			// Filter by agent ID prefix
			if len(agentID) == 0 || (len(name) >= len(prefix) && name[:len(prefix)] == prefix) {
				ids = append(ids, name)
			}
		}
	}

	return ids, nil
}

// GetLatestCheckpoint returns the most recent checkpoint for an agent.
func (s *AgentStore) GetLatestCheckpoint(agentID string) (*AgentCheckpoint, error) {
	ids, err := s.ListCheckpoints(agentID)
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return nil, fmt.Errorf("no checkpoints found for agent: %s", agentID)
	}

	// Checkpoints are named with timestamps, so the last one is the latest
	// Sort by name (which includes timestamp)
	latestID := ids[len(ids)-1]
	for _, id := range ids {
		if id > latestID {
			latestID = id
		}
	}

	return s.LoadCheckpoint(latestID)
}

// CleanupCheckpoints removes old checkpoints, keeping only the most recent N.
func (s *AgentStore) CleanupCheckpoints(agentID string, keepCount int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	checkpointsDir := filepath.Join(s.dir, "checkpoints")
	entries, err := os.ReadDir(checkpointsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read checkpoints directory: %w", err)
	}

	prefix := agentID + "-"
	var agentCheckpoints []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			name := entry.Name()[:len(entry.Name())-5]
			if len(name) > len(prefix) && name[:len(prefix)] == prefix {
				agentCheckpoints = append(agentCheckpoints, entry.Name())
			}
		}
	}

	// Sort to find oldest
	if len(agentCheckpoints) <= keepCount {
		return 0, nil
	}

	// Remove oldest checkpoints
	toRemove := agentCheckpoints[:len(agentCheckpoints)-keepCount]
	removed := 0
	for _, name := range toRemove {
		filePath := filepath.Join(checkpointsDir, name)
		if err := os.Remove(filePath); err == nil {
			removed++
		}
	}

	return removed, nil
}
