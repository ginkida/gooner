package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// SharedEntryType represents the type of shared memory entry.
type SharedEntryType string

const (
	SharedEntryTypeFact      SharedEntryType = "fact"
	SharedEntryTypeInsight   SharedEntryType = "insight"
	SharedEntryTypeFileState SharedEntryType = "file_state"
	SharedEntryTypeDecision  SharedEntryType = "decision"
)

// SharedEntry represents an entry in shared memory.
type SharedEntry struct {
	Key       string          `json:"key"`
	Value     any             `json:"value"`
	Type      SharedEntryType `json:"type"`
	Source    string          `json:"source"`    // Agent ID that wrote this
	Timestamp time.Time       `json:"timestamp"` // When this was written
	TTL       time.Duration   `json:"ttl"`       // Time-to-live (0 = never expires)
	Version   int             `json:"version"`   // Incremented on each update
}

// IsExpired returns true if the entry has expired.
func (e *SharedEntry) IsExpired() bool {
	if e.TTL == 0 {
		return false // Never expires
	}
	return time.Since(e.Timestamp) > e.TTL
}

// SharedMemory provides a shared memory space for inter-agent communication.
// Agents can write facts, insights, decisions, and file states that other agents can read.
type SharedMemory struct {
	entries     map[string]*SharedEntry
	byType      map[SharedEntryType][]string // Type -> list of keys
	subscribers map[string]chan<- *SharedEntry
	mu          sync.RWMutex
}

// NewSharedMemory creates a new shared memory instance.
func NewSharedMemory() *SharedMemory {
	return &SharedMemory{
		entries:     make(map[string]*SharedEntry),
		byType:      make(map[SharedEntryType][]string),
		subscribers: make(map[string]chan<- *SharedEntry),
	}
}

// Write writes a value to shared memory.
func (sm *SharedMemory) Write(key string, value any, entryType SharedEntryType, sourceAgent string) {
	sm.WriteWithTTL(key, value, entryType, sourceAgent, 0)
}

// WriteWithTTL writes a value to shared memory with a time-to-live.
func (sm *SharedMemory) WriteWithTTL(key string, value any, entryType SharedEntryType, sourceAgent string, ttl time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Create or update entry
	entry, exists := sm.entries[key]
	if exists {
		entry.Value = value
		entry.Type = entryType
		entry.Source = sourceAgent
		entry.Timestamp = time.Now()
		entry.TTL = ttl
		entry.Version++
	} else {
		entry = &SharedEntry{
			Key:       key,
			Value:     value,
			Type:      entryType,
			Source:    sourceAgent,
			Timestamp: time.Now(),
			TTL:       ttl,
			Version:   1,
		}
		sm.entries[key] = entry

		// Add to type index
		sm.byType[entryType] = append(sm.byType[entryType], key)
	}

	// Notify subscribers
	for _, ch := range sm.subscribers {
		select {
		case ch <- entry:
		default:
			// Non-blocking send, drop if subscriber is slow
		}
	}
}

// Read reads a value from shared memory.
func (sm *SharedMemory) Read(key string) (*SharedEntry, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	entry, ok := sm.entries[key]
	if !ok {
		return nil, false
	}

	// Check if expired
	if entry.IsExpired() {
		return nil, false
	}

	return entry, true
}

// ReadByType returns all entries of a specific type.
func (sm *SharedMemory) ReadByType(entryType SharedEntryType) []*SharedEntry {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	keys, ok := sm.byType[entryType]
	if !ok {
		return nil
	}

	var results []*SharedEntry
	for _, key := range keys {
		if entry, exists := sm.entries[key]; exists && !entry.IsExpired() {
			results = append(results, entry)
		}
	}

	return results
}

// ReadAll returns all non-expired entries.
func (sm *SharedMemory) ReadAll() []*SharedEntry {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var results []*SharedEntry
	for _, entry := range sm.entries {
		if !entry.IsExpired() {
			results = append(results, entry)
		}
	}

	return results
}

// Delete removes an entry from shared memory.
func (sm *SharedMemory) Delete(key string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	entry, ok := sm.entries[key]
	if !ok {
		return false
	}

	// Remove from type index
	keys := sm.byType[entry.Type]
	for i, k := range keys {
		if k == key {
			sm.byType[entry.Type] = append(keys[:i], keys[i+1:]...)
			break
		}
	}

	delete(sm.entries, key)
	return true
}

// Subscribe creates a subscription channel for an agent.
// The channel receives notifications when entries are written.
func (sm *SharedMemory) Subscribe(agentID string) <-chan *SharedEntry {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ch := make(chan *SharedEntry, 100) // Buffered channel
	sm.subscribers[agentID] = ch
	return ch
}

// Unsubscribe removes a subscription.
func (sm *SharedMemory) Unsubscribe(agentID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if ch, ok := sm.subscribers[agentID]; ok {
		close(ch)
		delete(sm.subscribers, agentID)
	}
}

// GetForContext returns a formatted string of relevant entries for injection into prompts.
// This filters entries based on relevance to the requesting agent.
func (sm *SharedMemory) GetForContext(agentID string, maxEntries int) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if len(sm.entries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## Shared Memory Context\n")
	sb.WriteString("The following information has been shared by other agents:\n\n")

	count := 0
	for _, entry := range sm.entries {
		if entry.IsExpired() {
			continue
		}

		// Skip entries from the same agent (they already know this)
		if entry.Source == agentID {
			continue
		}

		if count >= maxEntries {
			sb.WriteString(fmt.Sprintf("... and %d more entries\n", len(sm.entries)-count))
			break
		}

		sb.WriteString(fmt.Sprintf("- **%s** [%s from %s]: %v\n",
			entry.Key, entry.Type, entry.Source, entry.Value))
		count++
	}

	if count == 0 {
		return "" // No relevant entries
	}

	sb.WriteString("\n")
	return sb.String()
}

// CleanupExpired removes all expired entries.
func (sm *SharedMemory) CleanupExpired() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	var expired []string
	for key, entry := range sm.entries {
		if entry.IsExpired() {
			expired = append(expired, key)
		}
	}

	for _, key := range expired {
		entry := sm.entries[key]
		// Remove from type index
		keys := sm.byType[entry.Type]
		for i, k := range keys {
			if k == key {
				sm.byType[entry.Type] = append(keys[:i], keys[i+1:]...)
				break
			}
		}
		delete(sm.entries, key)
	}

	return len(expired)
}

// Stats returns statistics about the shared memory.
func (sm *SharedMemory) Stats() SharedMemoryStats {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stats := SharedMemoryStats{
		TotalEntries: len(sm.entries),
		Subscribers:  len(sm.subscribers),
		ByType:       make(map[SharedEntryType]int),
	}

	for entryType, keys := range sm.byType {
		stats.ByType[entryType] = len(keys)
	}

	return stats
}

// SharedMemoryStats contains statistics about shared memory usage.
type SharedMemoryStats struct {
	TotalEntries int
	Subscribers  int
	ByType       map[SharedEntryType]int
}

// Clear removes all entries from shared memory.
func (sm *SharedMemory) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.entries = make(map[string]*SharedEntry)
	sm.byType = make(map[SharedEntryType][]string)
}
