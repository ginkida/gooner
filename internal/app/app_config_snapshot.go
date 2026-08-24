package app

import (
	"gokin/internal/config"
	"gokin/internal/ratelimit"
)

// snapshotConfig returns the live config pointer, read under a.mu.
//
// ApplyConfig SWAPS a.config under that lock, so any reader on a goroutine
// other than the app's own must synchronize the read of the FIELD — the
// pointed-to config is immutable once published, so holding the lock only
// long enough to copy the pointer is sufficient and cannot deadlock against
// callbacks that re-enter arbitrary code.
//
// Readers on the request/command goroutine are already serialized with
// ApplyConfig and do not need this; the ones that do are callbacks invoked by
// other subsystems — the Bubble Tea status goroutine, a /loop iteration, a
// sub-agent's tool execution.
func (a *App) snapshotConfig() *config.Config {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config
}

// rateLimiterSnapshot returns the live rate limiter, read under a.mu.
//
// applyConfig creates and assigns a.rateLimiter under that lock, while both
// readers — handleRateLimitMetadata and sendContextHealthUpdate — run on the
// agent runner's rate-limit callback goroutine, which is NOT serialized with
// the app's. The limiter itself is internally synchronized, so the lock is held
// only long enough to copy the pointer.
func (a *App) rateLimiterSnapshot() *ratelimit.Limiter {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rateLimiter
}
