package agent

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gokin/internal/logging"
)

// SharedEntryType represents the type of shared memory entry.
type SharedEntryType string

const (
	SharedEntryTypeFact           SharedEntryType = "fact"
	SharedEntryTypeInsight        SharedEntryType = "insight"
	SharedEntryTypeFileState      SharedEntryType = "file_state"
	SharedEntryTypeDecision       SharedEntryType = "decision"
	SharedEntryTypeContextSnapshot SharedEntryType = "context_snapshot"

	// MaxSharedEntries is the maximum number of entries to keep in shared memory
	MaxSharedEntries = 500
)

// ContextSnapshot captures key information for plan→execute transitions.
// This preserves critical context that would otherwise be lost during context compaction.
type ContextSnapshot struct {
	// Key files that were read and their important sections
	KeyFiles map[string]string `json:"key_files"`

	// Important discoveries from exploration phase
	Discoveries []string `json:"discoveries"`

	// Error patterns encountered and their solutions
	ErrorPatterns map[string]string `json:"error_patterns"`

	// Tool results that should be preserved (not compacted)
	CriticalResults []CriticalResult `json:"critical_results"`

	// User requirements and constraints
	Requirements []string `json:"requirements"`

	// Architectural decisions made
	Decisions []string `json:"decisions"`

	// Created at timestamp
	CreatedAt time.Time `json:"created_at"`

	// Source agent/phase
	Source string `json:"source"`
}

// CriticalResult represents a tool result that should be preserved.
type CriticalResult struct {
	ToolName string `json:"tool_name"`
	Summary  string `json:"summary"`
	Details  string `json:"details"`
}

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
	closingCh   map[string]bool // Track channels that are being closed
	mu          sync.RWMutex

	// Metrics for monitoring
	droppedMessages atomic.Int64 // Count of messages dropped due to slow subscribers
}

// NewSharedMemory creates a new shared memory instance.
func NewSharedMemory() *SharedMemory {
	return &SharedMemory{
		entries:     make(map[string]*SharedEntry),
		byType:      make(map[SharedEntryType][]string),
		subscribers: make(map[string]chan<- *SharedEntry),
		closingCh:   make(map[string]bool),
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

	// Cleanup if we're at capacity
	if len(sm.entries) >= MaxSharedEntries {
		sm.cleanupExpiredLocked()
		// If still at capacity after cleanup, remove oldest entries
		if len(sm.entries) >= MaxSharedEntries {
			sm.removeOldestLocked(MaxSharedEntries / 4)
		}
	}

	// Create or update entry
	entry, exists := sm.entries[key]
	if exists {
		// If type changed, update the byType index
		if entry.Type != entryType {
			sm.removeFromTypeIndexLocked(entry.Type, key)
			sm.byType[entryType] = append(sm.byType[entryType], key)
		}
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

	// Notify subscribers (skip channels that are being closed)
	for subscriberID, ch := range sm.subscribers {
		// Skip channels marked for closing to prevent send-on-closed-channel panic
		if sm.closingCh[subscriberID] {
			continue
		}
		select {
		case ch <- entry:
		default:
			// Non-blocking send, drop if subscriber is slow
			dropped := sm.droppedMessages.Add(1)
			logging.Warn("shared memory: message dropped for slow subscriber",
				"subscriber_id", subscriberID,
				"entry_key", key,
				"total_dropped", dropped)
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
// The subscriber should stop reading from the channel before calling this.
func (sm *SharedMemory) Unsubscribe(agentID string) {
	sm.mu.Lock()
	ch, ok := sm.subscribers[agentID]
	if !ok {
		sm.mu.Unlock()
		return
	}

	// Mark as closing BEFORE removing from subscribers map
	// This prevents Write() from sending to this channel while we close it
	sm.closingCh[agentID] = true
	delete(sm.subscribers, agentID)
	sm.mu.Unlock()

	// Close the channel outside the lock
	// Using recover to handle any potential panic from double-close
	func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Warn("shared memory: recovered from channel close panic",
					"agent_id", agentID, "panic", r)
			}
		}()
		close(ch)
	}()

	// Clean up the closing marker (safe to do without lock since it's only read under lock)
	sm.mu.Lock()
	delete(sm.closingCh, agentID)
	sm.mu.Unlock()
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

// removeFromTypeIndexLocked removes a key from the byType index.
// Must be called with lock held.
func (sm *SharedMemory) removeFromTypeIndexLocked(entryType SharedEntryType, key string) {
	keys := sm.byType[entryType]
	for i, k := range keys {
		if k == key {
			sm.byType[entryType] = append(keys[:i], keys[i+1:]...)
			return
		}
	}
}

// cleanupExpiredLocked removes expired entries. Must be called with lock held.
func (sm *SharedMemory) cleanupExpiredLocked() int {
	var expired []string
	for key, entry := range sm.entries {
		if entry.IsExpired() {
			expired = append(expired, key)
		}
	}

	for _, key := range expired {
		entry := sm.entries[key]
		sm.removeFromTypeIndexLocked(entry.Type, key)
		delete(sm.entries, key)
	}

	return len(expired)
}

// removeOldestLocked removes the oldest N entries. Must be called with lock held.
func (sm *SharedMemory) removeOldestLocked(count int) {
	if count <= 0 || len(sm.entries) == 0 {
		return
	}

	// Find oldest entries by timestamp
	type entryTime struct {
		key string
		ts  time.Time
	}
	var entries []entryTime
	for key, entry := range sm.entries {
		entries = append(entries, entryTime{key: key, ts: entry.Timestamp})
	}

	// Simple selection of oldest (not sorted, just pick oldest)
	removed := 0
	for removed < count && len(entries) > 0 {
		oldestIdx := 0
		for i := 1; i < len(entries); i++ {
			if entries[i].ts.Before(entries[oldestIdx].ts) {
				oldestIdx = i
			}
		}

		key := entries[oldestIdx].key
		if entry, ok := sm.entries[key]; ok {
			sm.removeFromTypeIndexLocked(entry.Type, key)
			delete(sm.entries, key)
		}

		// Remove from local list
		entries = append(entries[:oldestIdx], entries[oldestIdx+1:]...)
		removed++
	}

	if removed > 0 {
		logging.Debug("shared memory: removed oldest entries", "count", removed)
	}
}

// CleanupExpired removes all expired entries.
func (sm *SharedMemory) CleanupExpired() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	return sm.cleanupExpiredLocked()
}

// Stats returns statistics about the shared memory.
func (sm *SharedMemory) Stats() SharedMemoryStats {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stats := SharedMemoryStats{
		TotalEntries:    len(sm.entries),
		Subscribers:     len(sm.subscribers),
		ByType:          make(map[SharedEntryType]int),
		DroppedMessages: sm.droppedMessages.Load(),
	}

	for entryType, keys := range sm.byType {
		stats.ByType[entryType] = len(keys)
	}

	return stats
}

// SharedMemoryStats contains statistics about shared memory usage.
type SharedMemoryStats struct {
	TotalEntries    int
	Subscribers     int
	ByType          map[SharedEntryType]int
	DroppedMessages int64 // Total messages dropped due to slow subscribers
}

// Clear removes all entries from shared memory.
func (sm *SharedMemory) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.entries = make(map[string]*SharedEntry)
	sm.byType = make(map[SharedEntryType][]string)
}

// SaveContextSnapshot saves a context snapshot for plan→execute transition.
func (sm *SharedMemory) SaveContextSnapshot(snapshot *ContextSnapshot, sourceAgent string) {
	if snapshot == nil {
		return
	}
	snapshot.CreatedAt = time.Now()
	snapshot.Source = sourceAgent

	sm.Write("context_snapshot", snapshot, SharedEntryTypeContextSnapshot, sourceAgent)
	logging.Debug("saved context snapshot",
		"source", sourceAgent,
		"key_files", len(snapshot.KeyFiles),
		"discoveries", len(snapshot.Discoveries),
		"requirements", len(snapshot.Requirements))
}

// GetContextSnapshot retrieves the latest context snapshot.
func (sm *SharedMemory) GetContextSnapshot() *ContextSnapshot {
	entry, ok := sm.Read("context_snapshot")
	if !ok {
		return nil
	}

	snapshot, ok := entry.Value.(*ContextSnapshot)
	if !ok {
		return nil
	}

	return snapshot
}

// GetContextSnapshotForPrompt returns a formatted context snapshot for injection into prompts.
func (sm *SharedMemory) GetContextSnapshotForPrompt() string {
	snapshot := sm.GetContextSnapshot()
	if snapshot == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
	sb.WriteString("                    CONTEXT FROM PLANNING PHASE\n")
	sb.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")

	// Key files
	if len(snapshot.KeyFiles) > 0 {
		sb.WriteString("**Key Files Analyzed:**\n")
		for path, summary := range snapshot.KeyFiles {
			sb.WriteString(fmt.Sprintf("- `%s`: %s\n", path, summary))
		}
		sb.WriteString("\n")
	}

	// Discoveries
	if len(snapshot.Discoveries) > 0 {
		sb.WriteString("**Key Discoveries:**\n")
		for _, discovery := range snapshot.Discoveries {
			sb.WriteString(fmt.Sprintf("- %s\n", discovery))
		}
		sb.WriteString("\n")
	}

	// Requirements
	if len(snapshot.Requirements) > 0 {
		sb.WriteString("**Requirements:**\n")
		for _, req := range snapshot.Requirements {
			sb.WriteString(fmt.Sprintf("- %s\n", req))
		}
		sb.WriteString("\n")
	}

	// Decisions
	if len(snapshot.Decisions) > 0 {
		sb.WriteString("**Architectural Decisions:**\n")
		for _, decision := range snapshot.Decisions {
			sb.WriteString(fmt.Sprintf("- %s\n", decision))
		}
		sb.WriteString("\n")
	}

	// Critical results
	if len(snapshot.CriticalResults) > 0 {
		sb.WriteString("**Critical Tool Results:**\n")
		for _, result := range snapshot.CriticalResults {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", result.ToolName, result.Summary))
			if result.Details != "" && len(result.Details) < 500 {
				sb.WriteString(fmt.Sprintf("  Details: %s\n", result.Details))
			}
		}
		sb.WriteString("\n")
	}

	// Error patterns
	if len(snapshot.ErrorPatterns) > 0 {
		sb.WriteString("**Known Error Patterns:**\n")
		for pattern, solution := range snapshot.ErrorPatterns {
			sb.WriteString(fmt.Sprintf("- `%s`: %s\n", pattern, solution))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")

	return sb.String()
}

// NewContextSnapshot creates a new empty context snapshot.
func NewContextSnapshot() *ContextSnapshot {
	return &ContextSnapshot{
		KeyFiles:        make(map[string]string),
		Discoveries:     make([]string, 0),
		ErrorPatterns:   make(map[string]string),
		CriticalResults: make([]CriticalResult, 0),
		Requirements:    make([]string, 0),
		Decisions:       make([]string, 0),
		CreatedAt:       time.Now(),
	}
}

// AddKeyFile adds a key file with its summary to the snapshot.
func (cs *ContextSnapshot) AddKeyFile(path, summary string) {
	cs.KeyFiles[path] = summary
}

// AddDiscovery adds a discovery to the snapshot.
func (cs *ContextSnapshot) AddDiscovery(discovery string) {
	cs.Discoveries = append(cs.Discoveries, discovery)
}

// AddRequirement adds a requirement to the snapshot.
func (cs *ContextSnapshot) AddRequirement(requirement string) {
	cs.Requirements = append(cs.Requirements, requirement)
}

// AddDecision adds an architectural decision to the snapshot.
func (cs *ContextSnapshot) AddDecision(decision string) {
	cs.Decisions = append(cs.Decisions, decision)
}

// AddCriticalResult adds a critical tool result to the snapshot.
func (cs *ContextSnapshot) AddCriticalResult(toolName, summary, details string) {
	cs.CriticalResults = append(cs.CriticalResults, CriticalResult{
		ToolName: toolName,
		Summary:  summary,
		Details:  details,
	})
}

// AddErrorPattern adds an error pattern and its solution.
func (cs *ContextSnapshot) AddErrorPattern(pattern, solution string) {
	cs.ErrorPatterns[pattern] = solution
}
