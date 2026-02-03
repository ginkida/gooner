package client

import "time"

// StatusCallback provides notifications about client operation status.
// This allows the UI to show feedback during retry operations, rate limiting,
// stream idle states, and recoverable errors.
type StatusCallback interface {
	// OnRetry is called when the client is retrying a failed request.
	// attempt is the current retry number (1-based), maxAttempts is the total allowed.
	// delay is the time before the retry will be attempted.
	// reason describes why the retry is happening (e.g., "connection reset", "429 rate limit").
	OnRetry(attempt, maxAttempts int, delay time.Duration, reason string)

	// OnRateLimit is called when the client is waiting due to rate limiting.
	// waitTime is the duration the client will wait before retrying.
	OnRateLimit(waitTime time.Duration)

	// OnStreamIdle is called when the streaming response has been idle for a while.
	// elapsed is the time since the last data was received.
	OnStreamIdle(elapsed time.Duration)

	// OnStreamResume is called when the stream resumes after being idle.
	OnStreamResume()

	// OnError is called when an error occurs.
	// recoverable indicates whether the client will attempt to recover from the error.
	OnError(err error, recoverable bool)
}

// DefaultStatusCallback is a no-op implementation of StatusCallback.
// Use this when you don't need status notifications.
type DefaultStatusCallback struct{}

// OnRetry does nothing.
func (d *DefaultStatusCallback) OnRetry(attempt, maxAttempts int, delay time.Duration, reason string) {
}

// OnRateLimit does nothing.
func (d *DefaultStatusCallback) OnRateLimit(waitTime time.Duration) {}

// OnStreamIdle does nothing.
func (d *DefaultStatusCallback) OnStreamIdle(elapsed time.Duration) {}

// OnStreamResume does nothing.
func (d *DefaultStatusCallback) OnStreamResume() {}

// OnError does nothing.
func (d *DefaultStatusCallback) OnError(err error, recoverable bool) {}
