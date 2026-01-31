package cache

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileMetadataCache caches file metadata (size, modtime, existence).
type FileMetadataCache struct {
	cache *LRUCache[string, *FileInfo]
	mu    sync.RWMutex
	ttl   time.Duration
}

// FileInfo represents cached file information.
type FileInfo struct {
	Exists   bool
	Size     int64
	ModTime  time.Time
	IsDir    bool
	CachedAt time.Time
}

// NewFileMetadataCache creates a new file metadata cache.
func NewFileMetadataCache(capacity int, ttl time.Duration) *FileMetadataCache {
	return &FileMetadataCache{
		cache: NewLRUCache[string, *FileInfo](capacity, ttl),
		ttl:   ttl,
	}
}

// Get retrieves cached file metadata.
func (c *FileMetadataCache) Get(path string) (*FileInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if entry, ok := c.cache.Get(path); ok {
		if time.Since(entry.CachedAt) < c.ttl {
			return entry, true
		}
	}
	return nil, false
}

// Set caches file metadata.
func (c *FileMetadataCache) Set(path string, info *FileInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	info.CachedAt = time.Now()
	c.cache.Set(path, info)
}

// GetFileMetadata gets file metadata with caching.
func (c *FileMetadataCache) GetFileMetadata(path string) (*FileInfo, error) {
	// Check cache first
	if info, ok := c.Get(path); ok {
		return info, nil
	}

	// Stat the file
	fileInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			info := &FileInfo{Exists: false}
			c.Set(path, info)
			return info, nil
		}
		return nil, err
	}

	info := &FileInfo{
		Exists:  true,
		Size:    fileInfo.Size(),
		ModTime: fileInfo.ModTime(),
		IsDir:   fileInfo.IsDir(),
	}
	c.Set(path, info)
	return info, nil
}

// Invalidate invalidates cache entry for a path.
func (c *FileMetadataCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.Delete(path)
}

// InvalidateByPrefix invalidates all entries with a prefix.
func (c *FileMetadataCache) InvalidateByPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache.Remove(func(key string, value *FileInfo) bool {
		return strings.HasPrefix(key, prefix)
	})
}

// DirectoryTreeCache caches directory tree structures.
type DirectoryTreeCache struct {
	cache *LRUCache[string, *TreeEntry]
	mu    sync.RWMutex
	ttl   time.Duration
}

// TreeEntry represents a cached directory tree entry.
type TreeEntry struct {
	Path     string
	Children []string
	IsDir    bool
	CachedAt time.Time
}

// NewDirectoryTreeCache creates a new directory tree cache.
func NewDirectoryTreeCache(capacity int, ttl time.Duration) *DirectoryTreeCache {
	return &DirectoryTreeCache{
		cache: NewLRUCache[string, *TreeEntry](capacity, ttl),
		ttl:   ttl,
	}
}

// Get retrieves cached tree entry.
func (c *DirectoryTreeCache) Get(path string) (*TreeEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if entry, ok := c.cache.Get(path); ok {
		if time.Since(entry.CachedAt) < c.ttl {
			return entry, true
		}
	}
	return nil, false
}

// Set caches a tree entry.
func (c *DirectoryTreeCache) Set(path string, entry *TreeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry.CachedAt = time.Now()
	c.cache.Set(path, entry)
}

// BuildTree builds and caches a directory tree.
func (c *DirectoryTreeCache) BuildTree(root string, maxDepth int) (*TreeEntry, error) {
	// Check cache first
	if entry, ok := c.Get(root); ok {
		return entry, nil
	}

	// Build tree
	entry, err := c.buildTreeRecursive(root, maxDepth, 0)
	if err != nil {
		return nil, err
	}

	c.Set(root, entry)
	return entry, nil
}

func (c *DirectoryTreeCache) buildTreeRecursive(path string, maxDepth, currentDepth int) (*TreeEntry, error) {
	if currentDepth >= maxDepth {
		return &TreeEntry{Path: path, IsDir: true}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	entry := &TreeEntry{
		Path:     path,
		IsDir:    true,
		Children: make([]string, 0),
	}

	for _, e := range entries {
		name := e.Name()
		fullPath := filepath.Join(path, name)
		entry.Children = append(entry.Children, fullPath)

		if e.IsDir() {
			// Recursively build child trees
			_, err := c.buildTreeRecursive(fullPath, maxDepth, currentDepth+1)
			if err != nil {
				// Continue on error, just log
				continue
			}
		}
	}

	return entry, nil
}

// Invalidate invalidates tree cache for a path.
func (c *DirectoryTreeCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.Delete(path)
}
