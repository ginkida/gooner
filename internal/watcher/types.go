package watcher

import "time"

// Operation represents the type of file system operation.
type Operation int

const (
	OpCreate Operation = iota
	OpModify
	OpDelete
	OpRename
)

// String returns the string representation of the operation.
func (op Operation) String() string {
	switch op {
	case OpCreate:
		return "create"
	case OpModify:
		return "modify"
	case OpDelete:
		return "delete"
	case OpRename:
		return "rename"
	default:
		return "unknown"
	}
}

// Event represents a file system event.
type Event struct {
	Path      string
	Operation Operation
	Time      time.Time
}

// Config holds file watcher configuration.
type Config struct {
	Enabled    bool
	DebounceMs int
	MaxWatches int
}

// DefaultConfig returns the default watcher configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:    false, // Disabled by default
		DebounceMs: 500,
		MaxWatches: 1000,
	}
}

// FileChangeHandler is a callback for file change events.
type FileChangeHandler func(path string, op Operation)
