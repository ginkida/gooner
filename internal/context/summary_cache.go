package context

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"google.golang.org/genai"
)

// CachedSummary represents a cached summary with metadata.
type CachedSummary struct {
	Summary      *genai.Content
	TokenCount   int
	CreatedAt    time.Time
	LastUsedAt   time.Time
	MessageHash  string // Hash of the messages that were summarized
	MessageRange struct {
		Start int
		End   int
	}
}

// SummaryCache caches generated summaries to avoid regeneration.
type SummaryCache struct {
	mu       sync.RWMutex
	cache    map[string]*CachedSummary // messageHash -> summary
	lruList  []*string                 // Order of recent use (front = most recent)
	maxCache int
	ttl      time.Duration // Time-to-live for cache entries
}

// NewSummaryCache creates a new summary cache.
func NewSummaryCache(maxEntries int, ttl time.Duration) *SummaryCache {
	return &SummaryCache{
		cache:    make(map[string]*CachedSummary),
		lruList:  make([]*string, 0, maxEntries),
		maxCache: maxEntries,
		ttl:      ttl,
	}
}

// Get retrieves a cached summary if available and not expired.
func (c *SummaryCache) Get(messageHash string) (*CachedSummary, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.cache[messageHash]
	if !ok {
		return nil, false
	}

	// Check TTL
	if c.ttl > 0 && time.Since(entry.CreatedAt) > c.ttl {
		return nil, false
	}

	// Update last used time
	entry.LastUsedAt = time.Now()

	return entry, true
}

// Put stores a summary in the cache.
func (c *SummaryCache) Put(messageHash string, summary *genai.Content, tokenCount int, startIdx, endIdx int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if already exists
	if _, ok := c.cache[messageHash]; ok {
		return
	}

	// Evict oldest if at capacity
	if len(c.cache) >= c.maxCache {
		c.evictOldest()
	}

	now := time.Now()
	entry := &CachedSummary{
		Summary:     summary,
		TokenCount:  tokenCount,
		CreatedAt:   now,
		LastUsedAt:  now,
		MessageHash: messageHash,
	}
	entry.MessageRange.Start = startIdx
	entry.MessageRange.End = endIdx

	c.cache[messageHash] = entry
	c.lruList = append([]*string{&messageHash}, c.lruList...)
}

// Invalidate removes a cache entry.
func (c *SummaryCache) Invalidate(messageHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.cache, messageHash)

	// Remove from LRU list
	for i, key := range c.lruList {
		if key != nil && *key == messageHash {
			c.lruList = append(c.lruList[:i], c.lruList[i+1:]...)
			break
		}
	}
}

// Clear removes all entries from the cache.
func (c *SummaryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache = make(map[string]*CachedSummary)
	c.lruList = make([]*string, 0, c.maxCache)
}

// evictOldest removes the least recently used entry.
func (c *SummaryCache) evictOldest() {
	if len(c.lruList) == 0 {
		return
	}

	// Remove last (oldest) entry
	oldest := c.lruList[len(c.lruList)-1]
	if oldest != nil {
		delete(c.cache, *oldest)
	}
	c.lruList = c.lruList[:len(c.lruList)-1]
}

// Size returns the current cache size.
func (c *SummaryCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// GetStats returns cache statistics.
type CacheStats struct {
	Size    int
	HitRate float64
	TTL     time.Duration
	MaxSize int
}

// GetStats returns cache statistics.
func (c *SummaryCache) GetStats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		Size:    len(c.cache),
		TTL:     c.ttl,
		MaxSize: c.maxCache,
	}
}

// HashMessages creates a hash of messages for cache key.
func HashMessages(messages []*genai.Content) string {
	h := sha256.New()
	for _, msg := range messages {
		h.Write([]byte(msg.Role))
		for _, part := range msg.Parts {
			if part.Text != "" {
				// For text, hash first 500 chars to avoid huge computations
				text := part.Text
				if len(text) > 500 {
					text = text[:500]
				}
				h.Write([]byte(text))
			}
			if part.FunctionCall != nil {
				h.Write([]byte(part.FunctionCall.Name))
				// Hash args keys and presence
				for k := range part.FunctionCall.Args {
					h.Write([]byte(k))
				}
			}
			if part.FunctionResponse != nil {
				h.Write([]byte(part.FunctionResponse.Name))
				// For responses, just hash presence and first 200 chars
				if content, ok := part.FunctionResponse.Response["content"].(string); ok {
					if len(content) > 200 {
						content = content[:200]
					}
					h.Write([]byte(content))
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
