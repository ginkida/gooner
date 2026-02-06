package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store manages persistent memory storage.
type Store struct {
	configDir   string
	projectPath string
	projectHash string
	maxEntries  int

	entries       map[string]*Entry // ID -> Entry (Project & Session)
	globalEntries map[string]*Entry // ID -> Global Entry
	byKey         map[string]string // Key -> ID (All types)

	// Debounced write support
	dirty     bool         // Whether there are unsaved changes
	saveTimer *time.Timer  // Timer for debounced save
	saveMu    sync.Mutex   // Protects saveTimer

	mu sync.RWMutex
}

// NewStore creates a new memory store.
func NewStore(configDir, projectPath string, maxEntries int) (*Store, error) {
	// Create memory directory
	memDir := filepath.Join(configDir, "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create memory directory: %w", err)
	}

	// Generate project hash
	projectHash := hashPath(projectPath)

	store := &Store{
		configDir:   configDir,
		projectPath: projectPath,
		projectHash: projectHash,
		maxEntries:  maxEntries,
		entries:     make(map[string]*Entry),
		globalEntries: make(map[string]*Entry),
		byKey:       make(map[string]string),
	}

	// Load existing entries
	if err := store.load(); err != nil {
		// Non-fatal - start fresh if load fails
		store.entries = make(map[string]*Entry)
		store.byKey = make(map[string]string)
	}

	return store, nil
}

// Auto-tagging regex patterns.
var (
	reFilePath     = regexp.MustCompile(`(?:^|\s)(/[a-zA-Z0-9_.\-/]+)`)
	reFuncName     = regexp.MustCompile(`(?:func|function)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	rePackageName  = regexp.MustCompile(`package\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// extractContentTags extracts key concepts from content and returns them as tags.
func extractContentTags(content string) []string {
	seen := make(map[string]bool)
	var tags []string

	addTag := func(tag string) {
		if tag != "" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}

	// Extract file paths
	for _, match := range reFilePath.FindAllStringSubmatch(content, -1) {
		addTag(match[1])
	}

	// Extract function names
	for _, match := range reFuncName.FindAllStringSubmatch(content, -1) {
		addTag(match[1])
	}

	// Extract package names
	for _, match := range rePackageName.FindAllStringSubmatch(content, -1) {
		addTag(match[1])
	}

	return tags
}

// autoTag merges extracted tags into the entry, deduplicating with existing tags.
func autoTag(entry *Entry) {
	extracted := extractContentTags(entry.Content)
	if len(extracted) == 0 {
		return
	}

	seen := make(map[string]bool)
	for _, t := range entry.Tags {
		seen[t] = true
	}
	for _, t := range extracted {
		if !seen[t] {
			entry.Tags = append(entry.Tags, t)
			seen[t] = true
		}
	}
}

// Add adds a new entry to the store.
func (s *Store) Add(entry *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Auto-tag: extract key concepts from content
	autoTag(entry)

	// If key exists, remove old ID from both stores and index
	if entry.Key != "" {
		if oldID, ok := s.byKey[entry.Key]; ok {
			delete(s.entries, oldID)
			delete(s.globalEntries, oldID)
		}
		s.byKey[entry.Key] = entry.ID
	}

	// Determine storage
	if entry.Type == MemoryGlobal {
		s.globalEntries[entry.ID] = entry
	} else {
		// Set project if not already set (for project/session)
		if entry.Project == "" {
			entry.Project = s.projectHash
		}
		s.entries[entry.ID] = entry
	}

	// Enforce max entries limit (total across project and global)
	if s.maxEntries > 0 && (len(s.entries)+len(s.globalEntries)) > s.maxEntries {
		s.pruneOldest()
	}

	// Mark dirty and schedule debounced save
	s.dirty = true
	s.scheduleSave()
	return nil
}

// Get retrieves an entry by key.
func (s *Store) Get(key string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, ok := s.byKey[key]
	if !ok {
		return nil, false
	}

	entry, ok := s.entries[id]
	return entry, ok
}

// GetByID retrieves an entry by ID.
func (s *Store) GetByID(id string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.entries[id]
	return entry, ok
}

// Edit updates the content of an existing entry by ID, re-runs auto-tagging,
// and marks the store dirty.
func (s *Store) Edit(id string, newContent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok {
		entry, ok = s.globalEntries[id]
	}
	if !ok {
		return fmt.Errorf("entry not found: %s", id)
	}

	entry.Content = newContent
	// Reset tags and re-run auto-tagging
	entry.Tags = nil
	autoTag(entry)

	s.dirty = true
	s.scheduleSave()
	return nil
}

// scoredEntry holds an entry and its relevance score for search ranking.
type scoredEntry struct {
	entry *Entry
	score int
}

// scoreEntry calculates a relevance score for an entry against the query.
// Exact key match = 10, tag match = 5 per tag, content substring = 1.
func scoreEntry(entry *Entry, query SearchQuery) int {
	score := 0
	queryLower := strings.ToLower(query.Query)

	// Exact key match
	if query.Query != "" && strings.EqualFold(entry.Key, query.Query) {
		score += 10
	}

	// Tag matches
	if query.Query != "" {
		for _, tag := range entry.Tags {
			if strings.EqualFold(tag, query.Query) {
				score += 5
			}
		}
	}

	// Content substring match
	if query.Query != "" && strings.Contains(strings.ToLower(entry.Content), queryLower) {
		score += 1
	}

	// If no query text provided, give a base score so entries aren't filtered out
	if query.Query == "" {
		score = 1
	}

	return score
}

// Search finds entries matching the query.
// Results are scored by relevance: exact key match = 10, tag match = 5,
// content substring = 1. Results are sorted by score descending, then by
// timestamp descending as a tiebreaker.
func (s *Store) Search(query SearchQuery) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Set current project for filtering
	query.Project = s.projectHash

	var scored []scoredEntry

	// Search project and session entries
	for _, entry := range s.entries {
		if !entry.Matches(query) {
			continue
		}
		sc := scoreEntry(entry, query)
		if sc > 0 {
			scored = append(scored, scoredEntry{entry: entry, score: sc})
		}
	}

	// Search global entries (unless ProjectOnly is specified)
	if !query.ProjectOnly {
		for _, entry := range s.globalEntries {
			if !entry.Matches(query) {
				continue
			}
			sc := scoreEntry(entry, query)
			if sc > 0 {
				scored = append(scored, scoredEntry{entry: entry, score: sc})
			}
		}
	}

	// Sort by score descending, then by timestamp descending
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].entry.Timestamp.After(scored[j].entry.Timestamp)
	})

	// Extract entries from scored results
	results := make([]*Entry, len(scored))
	for i, se := range scored {
		results[i] = se.entry
	}

	// Apply limit
	if query.Limit > 0 && len(results) > query.Limit {
		results = results[:query.Limit]
	}

	return results
}

// Remove removes an entry by ID or key.
func (s *Store) Remove(idOrKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Try as ID first
	if entry, ok := s.entries[idOrKey]; ok {
		delete(s.entries, idOrKey)
		if entry.Key != "" {
			delete(s.byKey, entry.Key)
		}
		_ = s.save()
		return true
	}
	if entry, ok := s.globalEntries[idOrKey]; ok {
		delete(s.globalEntries, idOrKey)
		if entry.Key != "" {
			delete(s.byKey, entry.Key)
		}
		_ = s.save()
		return true
	}

	// Try as key
	if id, ok := s.byKey[idOrKey]; ok {
		return s.Remove(id)
	}

	return false
}

// List returns entries for the current project (optionally project-only).
func (s *Store) List(projectOnly bool) []*Entry {
	return s.Search(SearchQuery{
		ProjectOnly: projectOnly,
		Project:     s.projectHash,
	})
}

// ListAll returns all entries (project + global) sorted by timestamp, newest first.
func (s *Store) ListAll() []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([]*Entry, 0, len(s.entries)+len(s.globalEntries))
	for _, entry := range s.entries {
		results = append(results, entry)
	}
	for _, entry := range s.globalEntries {
		results = append(results, entry)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})

	return results
}

// Export serializes all entries (project + global) to JSON.
func (s *Store) Export() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := make([]*Entry, 0, len(s.entries)+len(s.globalEntries))
	for _, entry := range s.entries {
		all = append(all, entry)
	}
	for _, entry := range s.globalEntries {
		all = append(all, entry)
	}

	return json.MarshalIndent(all, "", "  ")
}

// Import deserializes entries from JSON and merges them into the store.
// Duplicate entries (by ID) are skipped.
func (s *Store) Import(data []byte) error {
	var entries []*Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("failed to unmarshal import data: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		// Skip duplicates by ID
		if _, ok := s.entries[entry.ID]; ok {
			continue
		}
		if _, ok := s.globalEntries[entry.ID]; ok {
			continue
		}

		if entry.Type == MemoryGlobal {
			s.globalEntries[entry.ID] = entry
		} else {
			s.entries[entry.ID] = entry
		}

		if entry.Key != "" {
			s.byKey[entry.Key] = entry.ID
		}
	}

	s.dirty = true
	s.scheduleSave()
	return nil
}

// GetForContext returns a formatted string of memories for injection into prompts.
func (s *Store) GetForContext(projectOnly bool) string {
	entries := s.List(projectOnly)
	if len(entries) == 0 {
		return ""
	}

	// Group by type
	byType := make(map[MemoryType][]*Entry)
	for _, entry := range entries {
		byType[entry.Type] = append(byType[entry.Type], entry)
	}

	var builder strings.Builder
	builder.WriteString("## Memory\n\n")

	types := []struct {
		t     MemoryType
		label string
	}{
		{MemorySession, "Current Session"},
		{MemoryProject, "Project Knowledge"},
		{MemoryGlobal, "User Preferences"},
	}

	for _, tc := range types {
		if items, ok := byType[tc.t]; ok && len(items) > 0 {
			builder.WriteString(fmt.Sprintf("### %s\n", tc.label))
			for _, entry := range items {
				if entry.Key != "" {
					builder.WriteString(fmt.Sprintf("- **%s**: %s\n", entry.Key, entry.Content))
				} else {
					builder.WriteString(fmt.Sprintf("- %s\n", entry.Content))
				}
			}
			builder.WriteString("\n")
		}
	}

	return builder.String()
}

// Count returns the number of entries.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Clear removes all entries.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = make(map[string]*Entry)
	s.byKey = make(map[string]string)
	return s.save()
}

// storagePath returns the path to the memory file for the current project.
func (s *Store) storagePath() string {
	return filepath.Join(s.configDir, "memory", s.projectHash+".json")
}

// globalStoragePath returns the path to the global memory file.
func (s *Store) globalStoragePath() string {
	return filepath.Join(s.configDir, "memory", "global.json")
}

// load loads entries from disk.
func (s *Store) load() error {
	// Load project entries
	if err := s.loadFile(s.storagePath(), s.entries); err != nil {
		return err
	}
	// Load global entries
	if err := s.loadFile(s.globalStoragePath(), s.globalEntries); err != nil {
		return err
	}

	// Update byKey index
	for _, entry := range s.entries {
		if entry.Key != "" {
			s.byKey[entry.Key] = entry.ID
		}
	}
	for _, entry := range s.globalEntries {
		if entry.Key != "" {
			s.byKey[entry.Key] = entry.ID
		}
	}

	return nil
}

func (s *Store) loadFile(path string, target map[string]*Entry) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var entries []*Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	for _, entry := range entries {
		target[entry.ID] = entry
	}
	return nil
}

// save persists entries to disk.
func (s *Store) save() error {
	// Save project entries (filter out session entries)
	projectEntries := make([]*Entry, 0)
	for _, entry := range s.entries {
		if entry.Type == MemoryProject {
			projectEntries = append(projectEntries, entry)
		}
	}
	if err := s.saveFile(s.storagePath(), projectEntries); err != nil {
		return err
	}

	// Save global entries
	globalEntries := make([]*Entry, 0, len(s.globalEntries))
	for _, entry := range s.globalEntries {
		globalEntries = append(globalEntries, entry)
	}
	return s.saveFile(s.globalStoragePath(), globalEntries)
}

func (s *Store) saveFile(path string, entries []*Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// pruneOldest removes the oldest entries to stay within limit.
func (s *Store) pruneOldest() {
	if s.maxEntries <= 0 || len(s.entries) <= s.maxEntries {
		return
	}

	// Get all entries sorted by timestamp
	entries := make([]*Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})

	// Remove oldest entries
	toRemove := len(entries) - s.maxEntries
	for i := 0; i < toRemove; i++ {
		entry := entries[i]
		delete(s.entries, entry.ID)
		if entry.Key != "" {
			delete(s.byKey, entry.Key)
		}
	}
}

// hashPath generates a deterministic hash for a file path.
func hashPath(path string) string {
	hash := sha256.Sum256([]byte(path))
	return hex.EncodeToString(hash[:8])
}

// scheduleSave schedules a debounced save operation.
// Multiple calls within 2 seconds will be coalesced into a single save.
func (s *Store) scheduleSave() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	// Cancel existing timer if any
	if s.saveTimer != nil {
		s.saveTimer.Stop()
	}

	// Schedule new save after 2 seconds
	s.saveTimer = time.AfterFunc(2*time.Second, func() {
		s.mu.RLock()
		dirty := s.dirty
		s.mu.RUnlock()

		if dirty {
			s.mu.Lock()
			_ = s.save()
			s.dirty = false
			s.mu.Unlock()
		}
	})
}

// Flush forces an immediate save of any pending changes.
// Should be called during shutdown to ensure data is persisted.
func (s *Store) Flush() error {
	s.saveMu.Lock()
	if s.saveTimer != nil {
		s.saveTimer.Stop()
		s.saveTimer = nil
	}
	s.saveMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dirty {
		return nil
	}

	err := s.save()
	if err == nil {
		s.dirty = false
	}
	return err
}
