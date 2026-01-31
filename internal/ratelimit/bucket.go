package ratelimit

import (
	"sync"
	"time"
)

// TokenBucket implements a token bucket rate limiter.
type TokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
	mu         sync.Mutex
}

// NewTokenBucket creates a new token bucket with the given parameters.
// maxTokens is the maximum number of tokens the bucket can hold.
// refillRate is the number of tokens added per second.
func NewTokenBucket(maxTokens float64, refillRate float64) *TokenBucket {
	return &TokenBucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// refill adds tokens based on elapsed time since last refill.
func (b *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now
}

// TryConsume attempts to consume the specified number of tokens.
// Returns true if successful, false if not enough tokens available.
func (b *TokenBucket) TryConsume(tokens float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill()

	if b.tokens >= tokens {
		b.tokens -= tokens
		return true
	}
	return false
}

// Consume blocks until the specified number of tokens are available.
func (b *TokenBucket) Consume(tokens float64) {
	for {
		b.mu.Lock()
		b.refill()

		if b.tokens >= tokens {
			b.tokens -= tokens
			b.mu.Unlock()
			return
		}

		// Calculate wait time for required tokens
		deficit := tokens - b.tokens
		waitTime := time.Duration(deficit/b.refillRate*1000) * time.Millisecond
		b.mu.Unlock()

		// Wait a bit before retrying
		if waitTime < 10*time.Millisecond {
			waitTime = 10 * time.Millisecond
		}
		time.Sleep(waitTime)
	}
}

// ConsumeWithTimeout attempts to consume tokens within the given timeout.
// Returns true if successful, false if timeout expired.
func (b *TokenBucket) ConsumeWithTimeout(tokens float64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		b.mu.Lock()
		b.refill()

		if b.tokens >= tokens {
			b.tokens -= tokens
			b.mu.Unlock()
			return true
		}

		// Calculate wait time for required tokens
		deficit := tokens - b.tokens
		waitTime := time.Duration(deficit/b.refillRate*1000) * time.Millisecond
		b.mu.Unlock()

		// Don't wait longer than remaining time
		remaining := time.Until(deadline)
		if waitTime > remaining {
			waitTime = remaining
		}
		if waitTime < 10*time.Millisecond {
			waitTime = 10 * time.Millisecond
		}

		time.Sleep(waitTime)
	}

	return false
}

// Available returns the current number of available tokens.
func (b *TokenBucket) Available() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill()
	return b.tokens
}

// Reset resets the bucket to full capacity.
func (b *TokenBucket) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.tokens = b.maxTokens
	b.lastRefill = time.Now()
}

// Return returns tokens back to the bucket.
// This is useful when a request is cancelled or fails and the tokens should be released.
func (b *TokenBucket) Return(tokens float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.tokens += tokens
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
}
