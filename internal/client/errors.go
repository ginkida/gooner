package client

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// APIError represents an API error with HTTP status code.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Message)
}

// IsRetryableAPIError returns true if the API error has a retryable status code.
func IsRetryableAPIError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 429, 502, 503, 504:
			return true
		}
	}
	return false
}

// IsRetryableError checks if an error is retryable using proper type checks.
// Uses errors.Is/errors.As for typed errors, with string fallback only for untyped errors.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Typed checks first
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}

	// Network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// API errors with retryable status codes
	if IsRetryableAPIError(err) {
		return true
	}

	// String fallback only for untyped errors from third-party libraries
	msg := err.Error()
	untyped := []string{
		"rate limit",
		"eof",
		"tls handshake",
		"no such host",
	}
	for _, pattern := range untyped {
		if containsLower(msg, pattern) {
			return true
		}
	}

	return false
}

// containsLower checks if s contains substr (case-insensitive).
func containsLower(s, substr string) bool {
	return len(s) >= len(substr) && containsFold(s, substr)
}

func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if equalFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
