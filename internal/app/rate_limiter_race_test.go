package app

import (
	"sync"
	"testing"
	"time"

	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/ratelimit"
)

// TestRateLimiterReaders_NoRaceWithApplyConfigSwap covers the field sitting
// right beside a.config in applyConfig, which the earlier fixes for that field
// walked past. applyConfig creates and assigns a.rateLimiter under a.mu, while
// handleRateLimitMetadata is the agent runner's rate-limit callback — a
// goroutine that is not serialized with the app's — and sendContextHealthUpdate
// is reached from it. Both read the field lock-free.
func TestRateLimiterReaders_NoRaceWithApplyConfigSwap(t *testing.T) {
	a := &App{config: config.DefaultConfig()}
	l1 := ratelimit.NewLimiter(ratelimit.Config{Enabled: true, RequestsPerMinute: 60, TokensPerMinute: 1000, BurstSize: 5})
	l2 := ratelimit.NewLimiter(ratelimit.Config{Enabled: true, RequestsPerMinute: 30, TokensPerMinute: 500, BurstSize: 3})

	meta := &client.RateLimitMetadata{
		RequestsLimit: 60, RequestsRemaining: 30, RequestsReset: time.Second,
		TokensLimit: 1000, TokensRemaining: 500, TokensReset: time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// Reader #1: the runner's rate-limit callback.
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			a.handleRateLimitMetadata(meta)
		}
	}()
	// Reader #2: the health update it fans out to.
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			a.sendContextHealthUpdate()
		}
	}()

	// Writer: applyConfig assigning the limiter under a.mu.
	for i := 0; i < 1000; i++ {
		a.mu.Lock()
		if i%2 == 0 {
			a.rateLimiter = l1
		} else {
			a.rateLimiter = l2
		}
		a.mu.Unlock()
	}
	wg.Wait()
}
