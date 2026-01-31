package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
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
	grepCache *LRUCache[string, GrepResult]
	globCache *LRUCache[string, GlobResult]
	enabled   bool
}

// NewSearchCache creates a new search cache with the given capacity and TTL.
func NewSearchCache(capacity int, ttl time.Duration) *SearchCache {
	return &SearchCache{
		grepCache: NewLRUCache[string, GrepResult](capacity, ttl),
		globCache: NewLRUCache[string, GlobResult](capacity, ttl),
		enabled:   true,
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
}

// InvalidateByPath invalidates cache entries that match the given path.
// This should be called when a file is modified.
func (c *SearchCache) InvalidateByPath(path string) {
	if !c.enabled {
		return
	}

	// Note: In the future, we could implement more targeted invalidation
	// by tracking which files are included in each cache entry
	_ = path

	// For glob cache, we can't easily determine which entries are affected,
	// so we clear the entire glob cache when any file changes.
	// This is conservative but safe.
	c.globCache.Clear()

	// For grep cache, we also clear everything since grep results
	// could be affected by any file change.
	c.grepCache.Clear()
}

// InvalidateByDir invalidates all cache entries for files in the given directory.
func (c *SearchCache) InvalidateByDir(dir string) {
	if !c.enabled {
		return
	}

	// Clear both caches for any directory change
	// Note: In the future, we could implement more targeted invalidation
	// by checking if paths start with the given directory
	_ = dir
	c.globCache.Clear()
	c.grepCache.Clear()
}

// Clear clears all cached entries.
func (c *SearchCache) Clear() {
	c.grepCache.Clear()
	c.globCache.Clear()
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
