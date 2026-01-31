package app

import (
	"context"
	"fmt"
	"os"
)

// ErrorHandler provides centralized error handling with logging.
type ErrorHandler struct {
	logFile *os.File
}

// NewErrorHandler creates a new error handler.
func NewErrorHandler() *ErrorHandler {
	return &ErrorHandler{}
}

// Handle handles an error with appropriate logging and user feedback.
// It returns true if the error is fatal and the application should exit.
func (h *ErrorHandler) Handle(ctx context.Context, err error, operation string) bool {
	if err == nil {
		return false
	}

	// Log the error with context
	errMsg := fmt.Sprintf("[%s] %v", operation, err)

	// Check for context cancellation
	if ctx.Err() != nil {
		errMsg += fmt.Sprintf(" (context cancelled: %v)", ctx.Err())
	}

	// Write to log file if available
	if h.logFile != nil {
		h.logFile.WriteString(errMsg + "\n")
	}

	// Could also send to UI for display
	return false // By default, errors are non-fatal
}

// HandleWithRecovery wraps a function with panic recovery.
func (h *ErrorHandler) HandleWithRecovery(operation string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in %s: %v", operation, r)
			h.Handle(context.Background(), err, operation)
		}
	}()

	return fn()
}

// Close closes the error handler and releases resources.
func (h *ErrorHandler) Close() error {
	if h.logFile != nil {
		return h.logFile.Close()
	}
	return nil
}

// SafeExecute wraps a function with error handling and recovery.
func SafeExecute(operation string, fn func() error) error {
	handler := NewErrorHandler()
	return handler.HandleWithRecovery(operation, fn)
}

// LogOptional logs an error that occurred during optional feature initialization.
func LogOptional(feature string, err error) {
	if err != nil {
		// Use a simple fmt for now - could be enhanced with proper logging
		fmt.Fprintf(os.Stderr, "Warning: %s not available: %v\n", feature, err)
	}
}

// ErrorCode represents standardized error codes for the application.
type ErrorCode int

const (
	ErrCodeUnknown ErrorCode = iota
	ErrCodeConfig
	ErrCodeClient
	ErrCodeTool
	ErrCodePermission
	ErrCodeContext
	ErrCodeNetwork
	ErrCodeTimeout
	ErrCodeCancelled
	ErrCodeValidation
	ErrCodeIO
)

// AppError is a typed error with code for better error handling.
type AppError struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error {
	return e.Err
}

// NewAppError creates a new application error with code.
func NewAppError(code ErrorCode, message string, err error) *AppError {
	return &AppError{
		Code:    code,
		Message: message,
		Err:     err,
	}
}

// LogIgnoredError logs an error that is being intentionally ignored.
// Unlike the old IgnoreError, this always logs for debugging.
func LogIgnoredError(operation string, err error) {
	if err != nil {
		// Log to stderr in debug builds, or to file if logging is enabled
		fmt.Fprintf(os.Stderr, "Ignored error in %s: %v\n", operation, err)
	}
}

// GracefulReturn handles an error gracefully without panicking.
// Use this instead of Must() when initialization can fail non-fatally.
// Returns the error for the caller to handle or log appropriately.
func GracefulReturn(err error, msg string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", msg, err)
	}
	return nil
}
