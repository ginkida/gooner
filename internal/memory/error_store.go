package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrorEntry represents a learned error pattern with solution.
type ErrorEntry struct {
	ID          string    `json:"id"`
	ErrorType   string    `json:"error_type"`
	Pattern     string    `json:"pattern"`      // Substring or regex pattern to match
	Solution    string    `json:"solution"`     // What solved this error
	Tags        []string  `json:"tags"`         // Related categories
	SuccessRate float64   `json:"success_rate"` // How often this solution works
	UseCount    int       `json:"use_count"`    // Times this was applied
	LastUsed    time.Time `json:"last_used"`
	Created     time.Time `json:"created"`
}

// ErrorStore manages persistent storage of learned error patterns.
type ErrorStore struct {
	configDir string
	entries   map[string]*ErrorEntry // ID -> Entry
	byType    map[string][]string    // ErrorType -> []EntryID
	mu        sync.RWMutex
}

// NewErrorStore creates a new error store.
func NewErrorStore(configDir string) (*ErrorStore, error) {
	errDir := filepath.Join(configDir, "memory")
	if err := os.MkdirAll(errDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create error store directory: %w", err)
	}

	store := &ErrorStore{
		configDir: configDir,
		entries:   make(map[string]*ErrorEntry),
		byType:    make(map[string][]string),
	}

	// Load existing entries
	if err := store.load(); err != nil {
		// Non-fatal - start fresh if load fails
		store.entries = make(map[string]*ErrorEntry)
		store.byType = make(map[string][]string)
	}

	return store, nil
}

// storagePath returns the path to the error store file.
func (es *ErrorStore) storagePath() string {
	return filepath.Join(es.configDir, "memory", "errors.json")
}

// load loads entries from disk.
func (es *ErrorStore) load() error {
	data, err := os.ReadFile(es.storagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var entries []*ErrorEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	for _, entry := range entries {
		es.entries[entry.ID] = entry
		es.byType[entry.ErrorType] = append(es.byType[entry.ErrorType], entry.ID)
	}

	return nil
}

// save persists entries to disk.
func (es *ErrorStore) save() error {
	entries := make([]*ErrorEntry, 0, len(es.entries))
	for _, entry := range es.entries {
		entries = append(entries, entry)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(es.storagePath(), data, 0644)
}

// LearnError stores a new error pattern with its solution.
func (es *ErrorStore) LearnError(errorType, pattern, solution string, tags []string) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	// Check if similar pattern already exists
	for _, entry := range es.entries {
		if entry.ErrorType == errorType && entry.Pattern == pattern {
			// Update existing entry
			entry.Solution = solution
			entry.Tags = tags
			entry.LastUsed = time.Now()
			return es.save()
		}
	}

	// Create new entry
	entry := &ErrorEntry{
		ID:          generateID(pattern + solution),
		ErrorType:   errorType,
		Pattern:     pattern,
		Solution:    solution,
		Tags:        tags,
		SuccessRate: 0.5, // Start with neutral success rate
		UseCount:    0,
		Created:     time.Now(),
		LastUsed:    time.Now(),
	}

	es.entries[entry.ID] = entry
	es.byType[errorType] = append(es.byType[errorType], entry.ID)

	return es.save()
}

// GetLearnedErrors finds error entries matching the given error message.
func (es *ErrorStore) GetLearnedErrors(errorMsg string) []*ErrorEntry {
	es.mu.RLock()
	defer es.mu.RUnlock()

	var matches []*ErrorEntry
	lowerError := strings.ToLower(errorMsg)

	for _, entry := range es.entries {
		// Check if pattern matches
		lowerPattern := strings.ToLower(entry.Pattern)
		if strings.Contains(lowerError, lowerPattern) {
			matches = append(matches, entry)
		}
	}

	// Sort by success rate (higher first), then by use count
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].SuccessRate != matches[j].SuccessRate {
			return matches[i].SuccessRate > matches[j].SuccessRate
		}
		return matches[i].UseCount > matches[j].UseCount
	})

	return matches
}

// GetErrorContext returns formatted context for injection into prompts.
func (es *ErrorStore) GetErrorContext(errorMsg string) string {
	matches := es.GetLearnedErrors(errorMsg)
	if len(matches) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## Learned from Previous Errors\n\n")
	sb.WriteString("I've encountered similar errors before. Here's what worked:\n\n")

	// Show top 3 matches
	shown := 0
	for _, entry := range matches {
		if shown >= 3 {
			break
		}
		sb.WriteString(fmt.Sprintf("### %s (%.0f%% success rate)\n", entry.ErrorType, entry.SuccessRate*100))
		sb.WriteString(fmt.Sprintf("**Pattern:** %s\n", entry.Pattern))
		sb.WriteString(fmt.Sprintf("**Solution:** %s\n\n", entry.Solution))
		shown++
	}

	return sb.String()
}

// RecordSuccess records that a learned solution was successful.
func (es *ErrorStore) RecordSuccess(entryID string) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	entry, ok := es.entries[entryID]
	if !ok {
		return fmt.Errorf("entry not found: %s", entryID)
	}

	// Update success rate using exponential moving average
	entry.UseCount++
	alpha := 0.3 // Smoothing factor
	entry.SuccessRate = entry.SuccessRate*(1-alpha) + 1.0*alpha
	entry.LastUsed = time.Now()

	return es.save()
}

// RecordFailure records that a learned solution did not work.
func (es *ErrorStore) RecordFailure(entryID string) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	entry, ok := es.entries[entryID]
	if !ok {
		return fmt.Errorf("entry not found: %s", entryID)
	}

	// Update success rate using exponential moving average
	entry.UseCount++
	alpha := 0.3 // Smoothing factor
	entry.SuccessRate = entry.SuccessRate * (1 - alpha)
	entry.LastUsed = time.Now()

	return es.save()
}

// GetByType returns all error entries of a specific type.
func (es *ErrorStore) GetByType(errorType string) []*ErrorEntry {
	es.mu.RLock()
	defer es.mu.RUnlock()

	ids, ok := es.byType[errorType]
	if !ok {
		return nil
	}

	entries := make([]*ErrorEntry, 0, len(ids))
	for _, id := range ids {
		if entry, ok := es.entries[id]; ok {
			entries = append(entries, entry)
		}
	}

	return entries
}

// Clear removes all entries.
func (es *ErrorStore) Clear() error {
	es.mu.Lock()
	defer es.mu.Unlock()

	es.entries = make(map[string]*ErrorEntry)
	es.byType = make(map[string][]string)

	return es.save()
}

// Count returns the number of learned error patterns.
func (es *ErrorStore) Count() int {
	es.mu.RLock()
	defer es.mu.RUnlock()
	return len(es.entries)
}

// PruneOldEntries removes entries not used in the specified duration.
func (es *ErrorStore) PruneOldEntries(maxAge time.Duration) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var toDelete []string

	for id, entry := range es.entries {
		if entry.LastUsed.Before(cutoff) && entry.SuccessRate < 0.3 {
			toDelete = append(toDelete, id)
		}
	}

	for _, id := range toDelete {
		entry := es.entries[id]
		delete(es.entries, id)

		// Remove from byType index
		ids := es.byType[entry.ErrorType]
		for i, eid := range ids {
			if eid == id {
				es.byType[entry.ErrorType] = append(ids[:i], ids[i+1:]...)
				break
			}
		}
	}

	return es.save()
}
