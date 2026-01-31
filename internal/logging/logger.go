package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	logger  *slog.Logger
	logFile *os.File
	mu      sync.RWMutex
)

func init() {
	// Default: discard logs to avoid TUI interference
	// Use EnableFileLogging() to enable logging to a file
	logger = slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// EnableFileLogging enables logging to a file in the config directory.
// This should be called before the TUI starts.
func EnableFileLogging(configDir string, level Level) error {
	mu.Lock()
	defer mu.Unlock()

	logPath := filepath.Join(configDir, "gokin.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	// Close previous log file if any
	if logFile != nil {
		logFile.Close()
	}
	logFile = f

	var slogLevel slog.Level
	switch strings.ToLower(string(level)) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn", "warning":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelWarn
	}

	logger = slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level: slogLevel,
	}))

	return nil
}

// DisableLogging disables all logging output.
func DisableLogging() {
	mu.Lock()
	defer mu.Unlock()

	if logFile != nil {
		logFile.Close()
		logFile = nil
	}

	logger = slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

// Close closes the log file if open.
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

// Level represents a logging level.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Configure configures the global logger with the given level and writer.
func Configure(level Level, w io.Writer) {
	mu.Lock()
	defer mu.Unlock()

	var slogLevel slog.Level
	switch strings.ToLower(string(level)) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn", "warning":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	if w == nil {
		w = os.Stderr
	}

	logger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slogLevel,
	}))
}

// SetLevel sets the logging level.
func SetLevel(level Level) {
	Configure(level, nil)
}

// Debug logs a debug message.
func Debug(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Debug(msg, args...)
}

// Info logs an info message.
func Info(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Info(msg, args...)
}

// Warn logs a warning message.
func Warn(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Warn(msg, args...)
}

// Error logs an error message.
func Error(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Error(msg, args...)
}

// With returns a new logger with the given attributes.
func With(args ...any) *slog.Logger {
	mu.RLock()
	l := logger
	mu.RUnlock()
	return l.With(args...)
}

// Logger returns the underlying slog.Logger.
func Logger() *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return logger
}

// ParseLevel parses a level string to Level.
func ParseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}
