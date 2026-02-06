package client

import (
	"math/rand"
	"time"
)

// RetryConfig holds retry configuration used across all client implementations.
type RetryConfig struct {
	MaxRetries int           // Maximum number of retry attempts
	RetryDelay time.Duration // Initial delay between retries
	MaxDelay   time.Duration // Maximum backoff delay (cap)
}

// DefaultRetryConfig returns sensible retry defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		RetryDelay: 1 * time.Second,
		MaxDelay:   30 * time.Second,
	}
}

// CalculateBackoff calculates exponential backoff with jitter.
// This prevents thundering herd problem when many clients retry simultaneously.
func CalculateBackoff(baseDelay time.Duration, attempt int, maxDelay time.Duration) time.Duration {
	// Exponential backoff: baseDelay * 2^attempt
	delay := baseDelay * time.Duration(1<<uint(attempt))
	if delay > maxDelay {
		delay = maxDelay
	}

	// Add jitter: random value between 0 and 25% of delay
	jitter := time.Duration(rand.Int63n(int64(delay / 4)))
	return delay + jitter
}
