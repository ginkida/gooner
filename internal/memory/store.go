package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// Add adds a new entry to the store.
func (s *Store) Add(entry *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

	return s.save()
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

// Search finds entries matching the query.
func (s *Store) Search(query SearchQuery) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Set current project for filtering
	query.Project = s.projectHash

	var results []*Entry

	// Search project and session entries
	for _, entry := range s.entries {
		if !entry.Matches(query) {
			continue
		}
		if s.matchesQuery(entry, query) {
			results = append(results, entry)
		}
	}

	// Search global entries (unless ProjectOnly is specified)
	if !query.ProjectOnly {
		for _, entry := range s.globalEntries {
			// Global entries don't have a project, so Match will ignore project check
			if s.matchesQuery(entry, query) {
				results = append(results, entry)
			}
		}
	}

	// Sort by timestamp (newest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})

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

// matchesQuery helper for search logic
func (s *Store) matchesQuery(entry *Entry, query SearchQuery) bool {
	if query.Query != "" {
		queryLower := strings.ToLower(query.Query)
		contentLower := strings.ToLower(entry.Content)
		keyLower := strings.ToLower(entry.Key)

		if !strings.Contains(contentLower, queryLower) &&
			!strings.Contains(keyLower, queryLower) {
			return false
		}
	}
	return true
}

// List returns all entries for the current project.
func (s *Store) List(projectOnly bool) []*Entry {
	return s.Search(SearchQuery{
		ProjectOnly: projectOnly,
		Project:     s.projectHash,
	})
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
