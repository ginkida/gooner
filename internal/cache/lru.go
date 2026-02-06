package cache

import (
	"container/list"
	"sync"
	"time"
)

// entry represents a cache entry with value and expiration.
type entry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time
	element   *list.Element
}

// LRUCache is a generic LRU cache with TTL support.
type LRUCache[K comparable, V any] struct {
	capacity  int
	ttl       time.Duration
	entries   map[K]*entry[K, V]
	evictList *list.List
	mu        sync.RWMutex

	// Background cleanup
	cleanupStop chan struct{}
}

// NewLRUCache creates a new LRU cache with the given capacity and TTL.
// A background goroutine periodically removes expired entries.
// Call Close() to stop the background cleanup goroutine.
func NewLRUCache[K comparable, V any](capacity int, ttl time.Duration) *LRUCache[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	c := &LRUCache[K, V]{
		capacity:    capacity,
		ttl:         ttl,
		entries:     make(map[K]*entry[K, V]),
		evictList:   list.New(),
		cleanupStop: make(chan struct{}),
	}

	// Start background cleanup goroutine
	go c.cleanupLoop()

	return c
}

// cleanupLoop periodically removes expired entries.
func (c *LRUCache[K, V]) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.Cleanup()
		case <-c.cleanupStop:
			return
		}
	}
}

// Close stops the background cleanup goroutine and releases resources.
func (c *LRUCache[K, V]) Close() {
	select {
	case <-c.cleanupStop:
		// Already closed
	default:
		close(c.cleanupStop)
	}
}

// Get retrieves a value from the cache.
// Returns the value and true if found and not expired, zero value and false otherwise.
func (c *LRUCache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero V
	e, ok := c.entries[key]
	if !ok {
		return zero, false
	}

	// Check if expired
	if time.Now().After(e.expiresAt) {
		c.removeEntry(e)
		return zero, false
	}

	// Move to front (most recently used)
	c.evictList.MoveToFront(e.element)
	return e.value, true
}

// Set adds or updates a value in the cache.
func (c *LRUCache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if key exists
	if e, ok := c.entries[key]; ok {
		// Update existing entry
		e.value = value
		e.expiresAt = time.Now().Add(c.ttl)
		c.evictList.MoveToFront(e.element)
		return
	}

	// Create new entry
	e := &entry[K, V]{
		key:       key,
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}
	e.element = c.evictList.PushFront(e)
	c.entries[key] = e

	// Evict if over capacity
	for c.evictList.Len() > c.capacity {
		c.evictOldest()
	}
}

// Put is an alias for Set for backward compatibility.
func (c *LRUCache[K, V]) Put(key K, value V) {
	c.Set(key, value)
}

// Size returns the number of entries in the cache (alias for Len).
func (c *LRUCache[K, V]) Size() int {
	return c.Len()
}

// Delete removes a key from the cache.
func (c *LRUCache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[key]; ok {
		c.removeEntry(e)
	}
}

// Remove removes entries matching the predicate function.
func (c *LRUCache[K, V]) Remove(pred func(K, V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, e := range c.entries {
		if pred(key, e.value) {
			c.removeEntry(e)
		}
	}
}

// Clear removes all entries from the cache.
func (c *LRUCache[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[K]*entry[K, V])
	c.evictList = list.New()
}

// Len returns the number of entries in the cache.
func (c *LRUCache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// evictOldest removes the oldest entry from the cache.
func (c *LRUCache[K, V]) evictOldest() {
	elem := c.evictList.Back()
	if elem == nil {
		return
	}
	e := elem.Value.(*entry[K, V])
	c.removeEntry(e)
}

// removeEntry removes an entry from the cache.
func (c *LRUCache[K, V]) removeEntry(e *entry[K, V]) {
	c.evictList.Remove(e.element)
	delete(c.entries, e.key)
}

// Cleanup removes expired entries from the cache.
// Returns the number of entries removed.
func (c *LRUCache[K, V]) Cleanup() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	removed := 0

	for key, e := range c.entries {
		if now.After(e.expiresAt) {
			c.evictList.Remove(e.element)
			delete(c.entries, key)
			removed++
		}
	}

	return removed
}

// Keys returns all non-expired keys in the cache.
func (c *LRUCache[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	keys := make([]K, 0, len(c.entries))

	for key, e := range c.entries {
		if !now.After(e.expiresAt) {
			keys = append(keys, key)
		}
	}

	return keys
}
