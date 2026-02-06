package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GrepResult represents cached grep search results.
type GrepResult struct {
	Matches   []GrepMatch
	FileCount int
	CachedAt  time.Time
}

// GrepMatch represents a single grep match.
type GrepMatch struct {
	FilePath string
	LineNum  int
	Line     string
}

// GlobResult represents cached glob search results.
type GlobResult struct {
	Files    []string
	CachedAt time.Time
}

// SearchCache provides caching for grep and glob operations.
type SearchCache struct {
	grepCache   *LRUCache[string, GrepResult]
	globCache   *LRUCache[string, GlobResult]
	fileToKeys  map[string]map[string]bool // file path -> set of cache keys
	keyToFiles  map[string]map[string]bool // cache key -> set of file paths
	indexMu     sync.RWMutex
	enabled     bool
	cleanupDone chan struct{}
}

// NewSearchCache creates a new search cache with the given capacity and TTL.
func NewSearchCache(capacity int, ttl time.Duration) *SearchCache {
	sc := &SearchCache{
		grepCache:   NewLRUCache[string, GrepResult](capacity, ttl),
		globCache:   NewLRUCache[string, GlobResult](capacity, ttl),
		fileToKeys:  make(map[string]map[string]bool),
		keyToFiles:  make(map[string]map[string]bool),
		enabled:     true,
		cleanupDone: make(chan struct{}),
	}
	go sc.backgroundCleanup(ttl)
	return sc
}

// backgroundCleanup periodically removes expired entries.
func (c *SearchCache) backgroundCleanup(ttl time.Duration) {
	ticker := time.NewTicker(ttl / 2)
	defer ticker.Stop()

	for {
		select {
		case <-c.cleanupDone:
			return
		case <-ticker.C:
			c.Cleanup()
		}
	}
}

// StopCleanup stops the background cleanup goroutine.
func (c *SearchCache) StopCleanup() {
	select {
	case <-c.cleanupDone:
		// Already closed
	default:
		close(c.cleanupDone)
	}
}

// trackFiles associates a cache key with the files it depends on.
func (c *SearchCache) trackFiles(key string, files []string) {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()

	// Track key -> files
	if c.keyToFiles[key] == nil {
		c.keyToFiles[key] = make(map[string]bool)
	}
	for _, f := range files {
		c.keyToFiles[key][f] = true

		// Track file -> keys (reverse index)
		if c.fileToKeys[f] == nil {
			c.fileToKeys[f] = make(map[string]bool)
		}
		c.fileToKeys[f][key] = true
	}
}

// removeKeyFromIndex removes a cache key from the reverse index.
func (c *SearchCache) removeKeyFromIndex(key string) {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()

	if files, ok := c.keyToFiles[key]; ok {
		for f := range files {
			if keys, ok := c.fileToKeys[f]; ok {
				delete(keys, key)
				if len(keys) == 0 {
					delete(c.fileToKeys, f)
				}
			}
		}
		delete(c.keyToFiles, key)
	}
}

// SetEnabled enables or disables the cache.
func (c *SearchCache) SetEnabled(enabled bool) {
	c.enabled = enabled
}

// IsEnabled returns whether the cache is enabled.
func (c *SearchCache) IsEnabled() bool {
	return c.enabled
}

// GrepKey generates a cache key for grep operations.
func GrepKey(pattern, path, glob string, caseInsensitive bool, contextLines int) string {
	data := fmt.Sprintf("grep:%s:%s:%s:%v:%d", pattern, path, glob, caseInsensitive, contextLines)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// GlobKey generates a cache key for glob operations.
func GlobKey(pattern, path string) string {
	data := fmt.Sprintf("glob:%s:%s", pattern, path)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// GetGrep retrieves cached grep results.
func (c *SearchCache) GetGrep(key string) (GrepResult, bool) {
	if !c.enabled {
		return GrepResult{}, false
	}
	return c.grepCache.Get(key)
}

// SetGrep stores grep results in the cache.
func (c *SearchCache) SetGrep(key string, result GrepResult) {
	if !c.enabled {
		return
	}
	result.CachedAt = time.Now()
	c.grepCache.Set(key, result)

	// Track files in reverse index
	files := make([]string, 0, len(result.Matches))
	seen := make(map[string]bool)
	for _, m := range result.Matches {
		if !seen[m.FilePath] {
			seen[m.FilePath] = true
			files = append(files, m.FilePath)
		}
	}
	c.trackFiles(key, files)
}

// GetGlob retrieves cached glob results.
func (c *SearchCache) GetGlob(key string) (GlobResult, bool) {
	if !c.enabled {
		return GlobResult{}, false
	}
	return c.globCache.Get(key)
}

// SetGlob stores glob results in the cache.
func (c *SearchCache) SetGlob(key string, result GlobResult) {
	if !c.enabled {
		return
	}
	result.CachedAt = time.Now()
	c.globCache.Set(key, result)

	// Track files in reverse index
	c.trackFiles(key, result.Files)
}

// InvalidateByPath invalidates cache entries that match the given path.
// This should be called when a file is modified.
func (c *SearchCache) InvalidateByPath(path string) {
	if !c.enabled {
		return
	}

	c.indexMu.RLock()
	keys, ok := c.fileToKeys[path]
	if !ok {
		c.indexMu.RUnlock()
		return
	}
	// Copy keys to avoid holding lock during deletion
	keyList := make([]string, 0, len(keys))
	for k := range keys {
		keyList = append(keyList, k)
	}
	c.indexMu.RUnlock()

	// Invalidate each affected cache key
	for _, key := range keyList {
		c.grepCache.Delete(key)
		c.globCache.Delete(key)
		c.removeKeyFromIndex(key)
	}
}

// InvalidateByDir invalidates all cache entries for files in the given directory.
func (c *SearchCache) InvalidateByDir(dir string) {
	if !c.enabled {
		return
	}

	c.indexMu.RLock()
	keysToInvalidate := make(map[string]bool)
	for filePath, keys := range c.fileToKeys {
		if strings.HasPrefix(filePath, dir) {
			for k := range keys {
				keysToInvalidate[k] = true
			}
		}
	}
	c.indexMu.RUnlock()

	for key := range keysToInvalidate {
		c.grepCache.Delete(key)
		c.globCache.Delete(key)
		c.removeKeyFromIndex(key)
	}
}

// Clear clears all cached entries.
func (c *SearchCache) Clear() {
	c.grepCache.Clear()
	c.globCache.Clear()

	c.indexMu.Lock()
	c.fileToKeys = make(map[string]map[string]bool)
	c.keyToFiles = make(map[string]map[string]bool)
	c.indexMu.Unlock()
}

// Cleanup removes expired entries from both caches.
// Returns the total number of entries removed.
func (c *SearchCache) Cleanup() int {
	return c.grepCache.Cleanup() + c.globCache.Cleanup()
}

// Stats returns cache statistics.
func (c *SearchCache) Stats() CacheStats {
	return CacheStats{
		GrepEntries: c.grepCache.Len(),
		GlobEntries: c.globCache.Len(),
		Enabled:     c.enabled,
	}
}

// CacheStats holds cache statistics.
type CacheStats struct {
	GrepEntries int
	GlobEntries int
	Enabled     bool
}

// FormatCachedGrep formats cached grep results for display.
func FormatCachedGrep(result GrepResult, workDir string) string {
	var builder strings.Builder

	for _, match := range result.Matches {
		relPath, err := filepath.Rel(workDir, match.FilePath)
		if err != nil {
			relPath = match.FilePath
		}
		builder.WriteString(fmt.Sprintf("%s:%d: %s\n", relPath, match.LineNum, match.Line))
	}

	return builder.String()
}
