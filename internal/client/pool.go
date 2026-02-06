package client

import (
	"fmt"
	"sync"
	"time"

	"gokin/internal/logging"
)

const (
	// DefaultMaxPoolSize is the default maximum number of clients in the pool.
	DefaultMaxPoolSize = 5

	// DefaultIdleTimeout is the duration after which idle clients are removed.
	DefaultIdleTimeout = 5 * time.Minute
)

// ClientPool manages a pool of reusable Client instances keyed by "provider:model".
type ClientPool struct {
	mu       sync.RWMutex
	clients  map[string]Client
	maxSize  int
	lastUsed map[string]time.Time
	closed   bool
}

// NewClientPool creates a new ClientPool with the given maximum size.
// If maxSize <= 0, DefaultMaxPoolSize is used.
func NewClientPool(maxSize int) *ClientPool {
	if maxSize <= 0 {
		maxSize = DefaultMaxPoolSize
	}
	return &ClientPool{
		clients:  make(map[string]Client),
		maxSize:  maxSize,
		lastUsed: make(map[string]time.Time),
	}
}

// poolKey generates a pool key from provider and model.
func poolKey(provider, model string) string {
	return fmt.Sprintf("%s:%s", provider, model)
}

// Get retrieves a client from the pool for the given provider and model.
// Returns the client and true if found, or nil and false if not pooled.
func (p *ClientPool) Get(provider, model string) (Client, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, false
	}

	key := poolKey(provider, model)
	c, ok := p.clients[key]
	if ok {
		p.lastUsed[key] = time.Now()
		logging.Debug("client retrieved from pool",
			"provider", provider,
			"model", model)
	}
	return c, ok
}

// Put stores a client in the pool. If the pool is full, the oldest idle client
// is evicted to make room.
func (p *ClientPool) Put(provider, model string, client Client) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	key := poolKey(provider, model)

	// If client already exists for this key, close the old one
	if existing, ok := p.clients[key]; ok {
		existing.Close()
	}

	// If pool is full, evict the oldest idle client
	if len(p.clients) >= p.maxSize {
		p.evictOldest()
	}

	p.clients[key] = client
	p.lastUsed[key] = time.Now()

	logging.Debug("client stored in pool",
		"provider", provider,
		"model", model,
		"pool_size", len(p.clients))
}

// evictOldest removes the least recently used client from the pool.
// Must be called with p.mu held.
func (p *ClientPool) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, t := range p.lastUsed {
		if oldestKey == "" || t.Before(oldestTime) {
			oldestKey = key
			oldestTime = t
		}
	}

	if oldestKey != "" {
		if c, ok := p.clients[oldestKey]; ok {
			c.Close()
		}
		delete(p.clients, oldestKey)
		delete(p.lastUsed, oldestKey)

		logging.Debug("evicted oldest client from pool",
			"key", oldestKey)
	}
}

// Cleanup removes clients that have been idle for longer than DefaultIdleTimeout.
func (p *ClientPool) Cleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	now := time.Now()
	for key, t := range p.lastUsed {
		if now.Sub(t) > DefaultIdleTimeout {
			if c, ok := p.clients[key]; ok {
				c.Close()
			}
			delete(p.clients, key)
			delete(p.lastUsed, key)

			logging.Debug("cleaned up idle client from pool",
				"key", key)
		}
	}
}

// Size returns the current number of clients in the pool.
func (p *ClientPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients)
}

// Close closes all pooled clients and marks the pool as closed.
func (p *ClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true
	for key, c := range p.clients {
		c.Close()
		delete(p.clients, key)
		delete(p.lastUsed, key)
	}

	logging.Debug("client pool closed")
}
