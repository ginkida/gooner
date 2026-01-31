package audit

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Entry represents a single audit log entry.
type Entry struct {
	ID        string         `json:"id"`
	Timestamp time.Time      `json:"timestamp"`
	ToolName  string         `json:"tool_name"`
	Args      map[string]any `json:"args"`
	Result    string         `json:"result"` // Truncated result
	Success   bool           `json:"success"`
	Error     string         `json:"error,omitempty"`
	Duration  time.Duration  `json:"duration_ms"`
	SessionID string         `json:"session_id"`
}

// NewEntry creates a new audit entry with a generated ID and timestamp.
func NewEntry(sessionID, toolName string, args map[string]any) *Entry {
	return &Entry{
		ID:        uuid.New().String(),
		Timestamp: time.Now(),
		ToolName:  toolName,
		Args:      args,
		SessionID: sessionID,
	}
}

// Complete fills in the result fields after tool execution.
func (e *Entry) Complete(result string, success bool, err string, duration time.Duration) {
	e.Result = result
	e.Success = success
	e.Error = err
	e.Duration = duration
}

// MarshalJSON implements custom JSON marshaling.
func (e *Entry) MarshalJSON() ([]byte, error) {
	type Alias Entry
	return json.Marshal(&struct {
		*Alias
		DurationMs int64 `json:"duration_ms"`
	}{
		Alias:      (*Alias)(e),
		DurationMs: e.Duration.Milliseconds(),
	})
}

// UnmarshalJSON implements custom JSON unmarshaling.
func (e *Entry) UnmarshalJSON(data []byte) error {
	type Alias Entry
	aux := &struct {
		*Alias
		DurationMs int64 `json:"duration_ms"`
	}{
		Alias: (*Alias)(e),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	e.Duration = time.Duration(aux.DurationMs) * time.Millisecond
	return nil
}

// QueryFilter defines criteria for querying audit entries.
type QueryFilter struct {
	ToolName    string
	SessionID   string
	Success     *bool
	Since       time.Time
	Until       time.Time
	MinDuration time.Duration
	Limit       int
	Offset      int
}

// Matches checks if the entry matches the filter criteria.
func (e *Entry) Matches(filter QueryFilter) bool {
	if filter.ToolName != "" && e.ToolName != filter.ToolName {
		return false
	}
	if filter.SessionID != "" && e.SessionID != filter.SessionID {
		return false
	}
	if filter.Success != nil && e.Success != *filter.Success {
		return false
	}
	if !filter.Since.IsZero() && e.Timestamp.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && e.Timestamp.After(filter.Until) {
		return false
	}
	if filter.MinDuration > 0 && e.Duration < filter.MinDuration {
		return false
	}
	return true
}

// SanitizeArgs creates a copy of args with sensitive values redacted.
func SanitizeArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}

	sanitized := make(map[string]any, len(args))
	sensitiveKeys := map[string]bool{
		"password":    true,
		"secret":      true,
		"token":       true,
		"api_key":     true,
		"apikey":      true,
		"credentials": true,
		"auth":        true,
	}

	for k, v := range args {
		if sensitiveKeys[k] {
			sanitized[k] = "[REDACTED]"
		} else {
			sanitized[k] = v
		}
	}

	return sanitized
}

// TruncateResult truncates a result string to the specified maximum length.
func TruncateResult(result string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 1000
	}
	if len(result) <= maxLen {
		return result
	}
	return result[:maxLen] + "...[truncated]"
}
