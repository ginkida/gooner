package semantic

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CacheEntry represents a cached embedding with metadata.
type CacheEntry struct {
	Embedding []float32
	Hash      string    // Content hash for invalidation
	Timestamp time.Time // When the embedding was created
	MetaData  string    // Optional metadata (e.g., project directory)
}

// EmbeddingCache provides persistent caching for embeddings.
type EmbeddingCache struct {
	mu        sync.RWMutex
	entries   map[string]CacheEntry
	filePath  string
	ttl       time.Duration
	dirty     bool
	projectID string // Unique identifier for the project
}

// NewEmbeddingCache creates a new embedding cache for a specific project.
func NewEmbeddingCache(configDir, projectDir string, ttl time.Duration) *EmbeddingCache {
	// Generate project ID from directory path
	projectID := generateProjectID(projectDir)

	// Create semantic_cache directory if it doesn't exist
	cacheDir := filepath.Join(configDir, "semantic_cache")
	cache := &EmbeddingCache{
		entries:   make(map[string]CacheEntry),
		filePath:  filepath.Join(cacheDir, projectID+".gob"),
		ttl:       ttl,
		projectID: projectID,
	}
	_ = cache.Load() // Ignore error on load - start fresh if file doesn't exist
	return cache
}

// generateProjectID creates a unique identifier for a project directory.
func generateProjectID(projectDir string) string {
	// Normalize the path
	projectDir = filepath.Clean(projectDir)

	// Create a short hash of the full path
	hash := sha256.Sum256([]byte(projectDir))
	return hex.EncodeToString(hash[:8]) // 8 characters = 16M possible values
}

// GetProjectID returns the project identifier for this cache.
func (c *EmbeddingCache) GetProjectID() string {
	return c.projectID
}

// GetProjectDir returns the original project directory (stored in metadata).
func (c *EmbeddingCache) GetProjectDir() string {
	// Store project path in a special entry
	c.mu.RLock()
	defer c.mu.RUnlock()
	if entry, ok := c.entries["__meta__:project_dir"]; ok {
		return entry.MetaData
	}
	return ""
}

// SetProjectDir stores the original project directory path.
func (c *EmbeddingCache) SetProjectDir(projectDir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries["__meta__:project_dir"] = CacheEntry{
		Embedding: []float32{}, // Empty embedding for metadata entry
		Hash:      "meta",
		Timestamp: time.Now(),
		MetaData:  projectDir,
	}
	c.dirty = true
}

// Get retrieves an embedding from cache.
// Returns the embedding and true if found and valid, nil and false otherwise.
func (c *EmbeddingCache) Get(key string, contentHash string) ([]float32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	// Check if entry is expired
	if time.Since(entry.Timestamp) > c.ttl {
		return nil, false
	}

	// Check if content has changed
	if entry.Hash != contentHash {
		return nil, false
	}

	return entry.Embedding, true
}

// Set stores an embedding in cache.
func (c *EmbeddingCache) Set(key string, embedding []float32, contentHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = CacheEntry{
		Embedding: embedding,
		Hash:      contentHash,
		Timestamp: time.Now(),
	}
	c.dirty = true
}

// Delete removes an entry from cache.
func (c *EmbeddingCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
	c.dirty = true
}

// InvalidateByPath removes all entries matching a file path.
func (c *EmbeddingCache) InvalidateByPath(filePath string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.entries {
		// Keys are typically "filepath:chunkIndex"
		if len(key) >= len(filePath) && key[:len(filePath)] == filePath {
			delete(c.entries, key)
			c.dirty = true
		}
	}
}

// Save persists the cache to disk.
func (c *EmbeddingCache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.dirty {
		return nil
	}

	// Create directory if needed
	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Write to temp file first
	tmpPath := c.filePath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	encoder := gob.NewEncoder(f)
	if err := encoder.Encode(c.entries); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Atomic rename
	if err := os.Rename(tmpPath, c.filePath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	c.dirty = false
	return nil
}

// Load loads the cache from disk.
func (c *EmbeddingCache) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	f, err := os.Open(c.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Fresh start
		}
		return err
	}
	defer f.Close()

	decoder := gob.NewDecoder(f)
	if err := decoder.Decode(&c.entries); err != nil {
		c.entries = make(map[string]CacheEntry) // Reset on decode error
		return err
	}

	return nil
}

// Cleanup removes expired entries from cache.
func (c *EmbeddingCache) Cleanup() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	now := time.Now()
	for key, entry := range c.entries {
		if now.Sub(entry.Timestamp) > c.ttl {
			delete(c.entries, key)
			count++
			c.dirty = true
		}
	}
	return count
}

// Size returns the number of entries in cache.
func (c *EmbeddingCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Clear removes all entries from the cache.
func (c *EmbeddingCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]CacheEntry)
	c.dirty = true
}

// GetSizeOnDisk returns the size of the cache file in bytes.
func (c *EmbeddingCache) GetSizeOnDisk() int64 {
	info, err := os.Stat(c.filePath)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ContentHash generates a hash of content for cache invalidation.
func ContentHash(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:8]) // Use first 8 bytes for shorter key
}
