package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limiter provides rate limiting for API requests.
type Limiter struct {
	requestBucket *TokenBucket
	tokenBucket   *TokenBucket
	enabled       bool
	mu            sync.RWMutex

	// Statistics
	totalRequests   int64
	blockedRequests int64
	totalTokens     int64
}

// Config holds rate limiter configuration.
type Config struct {
	Enabled           bool
	RequestsPerMinute int
	TokensPerMinute   int64
	BurstSize         int
}

// DefaultConfig returns the default rate limiter configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		RequestsPerMinute: 60,
		TokensPerMinute:   1000000,
		BurstSize:         10,
	}
}

// NewLimiter creates a new rate limiter with the given configuration.
func NewLimiter(cfg Config) *Limiter {
	// Calculate refill rates (tokens per second)
	requestRefillRate := float64(cfg.RequestsPerMinute) / 60.0
	tokenRefillRate := float64(cfg.TokensPerMinute) / 60.0

	// Burst size determines max tokens in bucket
	requestBurst := float64(cfg.BurstSize)
	if requestBurst < 1 {
		requestBurst = 1
	}

	// Token bucket burst is a percentage of per-minute limit
	tokenBurst := float64(cfg.TokensPerMinute) / 10.0 // Allow 10% burst

	return &Limiter{
		requestBucket: NewTokenBucket(requestBurst, requestRefillRate),
		tokenBucket:   NewTokenBucket(tokenBurst, tokenRefillRate),
		enabled:       cfg.Enabled,
	}
}

// Acquire blocks until a request slot is available.
// estimatedTokens is the estimated number of tokens for the request.
func (l *Limiter) Acquire(estimatedTokens int64) error {
	if !l.isEnabled() {
		return nil
	}

	l.mu.Lock()
	l.totalRequests++
	l.mu.Unlock()

	// First, acquire a request slot
	l.requestBucket.Consume(1)

	// Then, acquire token capacity
	if estimatedTokens > 0 {
		l.tokenBucket.Consume(float64(estimatedTokens))
	}

	return nil
}

// TryAcquire attempts to acquire a request slot without blocking.
// Returns true if successful, false if rate limited.
func (l *Limiter) TryAcquire(estimatedTokens int64) bool {
	if !l.isEnabled() {
		return true
	}

	l.mu.Lock()
	l.totalRequests++
	l.mu.Unlock()

	// Try to acquire request slot
	if !l.requestBucket.TryConsume(1) {
		l.mu.Lock()
		l.blockedRequests++
		l.mu.Unlock()
		return false
	}

	// Try to acquire token capacity
	if estimatedTokens > 0 {
		if !l.tokenBucket.TryConsume(float64(estimatedTokens)) {
			// Put back the request slot we took
			l.requestBucket.Return(1)
			l.mu.Lock()
			l.blockedRequests++
			l.mu.Unlock()
			return false
		}
	}

	return true
}

// AcquireWithTimeout attempts to acquire a request slot with a timeout.
// Returns nil on success, error if timeout expired.
func (l *Limiter) AcquireWithTimeout(estimatedTokens int64, timeout time.Duration) error {
	if !l.isEnabled() {
		return nil
	}

	l.mu.Lock()
	l.totalRequests++
	l.mu.Unlock()

	// Try to acquire request slot with timeout
	if !l.requestBucket.ConsumeWithTimeout(1, timeout) {
		l.mu.Lock()
		l.blockedRequests++
		l.mu.Unlock()
		return fmt.Errorf("rate limit exceeded: request limit")
	}

	// Try to acquire token capacity with remaining timeout
	if estimatedTokens > 0 {
		if !l.tokenBucket.ConsumeWithTimeout(float64(estimatedTokens), timeout) {
			l.mu.Lock()
			l.blockedRequests++
			l.mu.Unlock()
			return fmt.Errorf("rate limit exceeded: token limit")
		}
	}

	return nil
}

// AcquireWithContext attempts to acquire a request slot respecting context cancellation.
func (l *Limiter) AcquireWithContext(ctx context.Context, estimatedTokens int64) error {
	if !l.isEnabled() {
		return nil
	}

	// Use a reasonable default timeout if context has no deadline
	timeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	// Create a channel to signal completion
	done := make(chan error, 1)

	go func() {
		err := l.AcquireWithTimeout(estimatedTokens, timeout)
		// Protected send - don't block if context is cancelled
		select {
		case done <- err:
		case <-ctx.Done():
			// Context cancelled, don't block on channel send
			// The goroutine will exit cleanly
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RecordUsage records actual token usage after a request completes.
// This can be used to adjust estimates for future requests.
func (l *Limiter) RecordUsage(actualTokens int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.totalTokens += actualTokens
}

// ReturnTokens returns tokens back to the buckets.
// This should be called when a request fails after tokens were acquired,
// to prevent bucket exhaustion due to failed requests.
func (l *Limiter) ReturnTokens(requestTokens int, estimatedTokens int64) {
	if !l.isEnabled() {
		return
	}
	if requestTokens > 0 {
		l.requestBucket.Return(float64(requestTokens))
	}
	if estimatedTokens > 0 {
		l.tokenBucket.Return(float64(estimatedTokens))
	}
}

// Stats returns rate limiter statistics.
func (l *Limiter) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return Stats{
		Enabled:           l.enabled,
		TotalRequests:     l.totalRequests,
		BlockedRequests:   l.blockedRequests,
		TotalTokens:       l.totalTokens,
		AvailableRequests: l.requestBucket.Available(),
		AvailableTokens:   l.tokenBucket.Available(),
	}
}

// Stats holds rate limiter statistics.
type Stats struct {
	Enabled           bool
	TotalRequests     int64
	BlockedRequests   int64
	TotalTokens       int64
	AvailableRequests float64
	AvailableTokens   float64
}

// SetEnabled enables or disables the rate limiter.
func (l *Limiter) SetEnabled(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = enabled
}

// isEnabled checks if the limiter is enabled (thread-safe).
func (l *Limiter) isEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.enabled
}

// Reset resets all buckets and statistics.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.requestBucket.Reset()
	l.tokenBucket.Reset()
	l.totalRequests = 0
	l.blockedRequests = 0
	l.totalTokens = 0
}

// EstimateTokens estimates the number of tokens for a message.
// This is a rough estimate based on character count.
func EstimateTokens(message string) int64 {
	// Rough estimate: ~4 characters per token
	return int64(len(message) / 4)
}

// EstimateTokensFromContents estimates tokens for multiple content items.
func EstimateTokensFromContents(contents int, avgLength int) int64 {
	// Estimate based on number of contents and average length
	return int64(contents * avgLength / 4)
}
