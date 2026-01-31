package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Logger handles audit logging for tool operations.
type Logger struct {
	configDir    string
	sessionID    string
	maxEntries   int
	maxResultLen int
	entries      []*Entry
	mu           sync.RWMutex
	wg           sync.WaitGroup // Track pending async saves
	enabled      bool
	retention    time.Duration
}

// Config holds audit logger configuration.
type Config struct {
	Enabled       bool
	MaxEntries    int
	MaxResultLen  int
	RetentionDays int
}

// DefaultConfig returns the default audit configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		MaxEntries:    10000,
		MaxResultLen:  1000,
		RetentionDays: 30,
	}
}

// NewLogger creates a new audit logger.
func NewLogger(configDir, sessionID string, cfg Config) (*Logger, error) {
	if !cfg.Enabled {
		return &Logger{enabled: false}, nil
	}

	auditDir := filepath.Join(configDir, "audit")
	// Use 0700 to restrict access to owner only (contains sensitive data)
	if err := os.MkdirAll(auditDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create audit directory: %w", err)
	}

	logger := &Logger{
		configDir:    configDir,
		sessionID:    sessionID,
		maxEntries:   cfg.MaxEntries,
		maxResultLen: cfg.MaxResultLen,
		entries:      make([]*Entry, 0),
		enabled:      true,
		retention:    time.Duration(cfg.RetentionDays) * 24 * time.Hour,
	}

	// Load existing entries for this session
	if err := logger.load(); err != nil {
		// Non-fatal, just start fresh
		logger.entries = make([]*Entry, 0)
	}

	return logger, nil
}

// Log records a new audit entry.
func (l *Logger) Log(entry *Entry) error {
	if !l.enabled || entry == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Sanitize args and truncate result
	entry.Args = SanitizeArgs(entry.Args)
	entry.Result = TruncateResult(entry.Result, l.maxResultLen)

	l.entries = append(l.entries, entry)

	// Trim if over max entries
	if len(l.entries) > l.maxEntries {
		l.entries = l.entries[len(l.entries)-l.maxEntries:]
	}

	// Save asynchronously to avoid blocking, but track with WaitGroup
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.save()
	}()

	return nil
}

// Query retrieves entries matching the filter.
func (l *Logger) Query(filter QueryFilter) []*Entry {
	if !l.enabled {
		return nil
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	var results []*Entry
	skipped := 0

	for _, entry := range l.entries {
		if entry.Matches(filter) {
			if skipped < filter.Offset {
				skipped++
				continue
			}
			results = append(results, entry)
			if filter.Limit > 0 && len(results) >= filter.Limit {
				break
			}
		}
	}

	return results
}

// GetRecent returns the most recent n entries.
func (l *Logger) GetRecent(n int) []*Entry {
	if !l.enabled || n <= 0 {
		return nil
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	if n > len(l.entries) {
		n = len(l.entries)
	}

	// Return entries in reverse order (newest first)
	results := make([]*Entry, n)
	for i := 0; i < n; i++ {
		results[i] = l.entries[len(l.entries)-1-i]
	}

	return results
}

// Export exports entries to the specified format.
func (l *Logger) Export(format string) ([]byte, error) {
	if !l.enabled {
		return nil, nil
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	switch format {
	case "json":
		return json.MarshalIndent(l.entries, "", "  ")
	case "jsonl":
		var result []byte
		for _, entry := range l.entries {
			line, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			result = append(result, line...)
			result = append(result, '\n')
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
}

// Clear removes all entries.
func (l *Logger) Clear() error {
	if !l.enabled {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = make([]*Entry, 0)

	// Remove the session file
	filePath := l.getFilePath()
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove audit file: %w", err)
	}

	return nil
}

// Len returns the number of entries.
func (l *Logger) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

// Flush waits for all pending async saves to complete.
// This should be called before shutdown to ensure no data is lost.
func (l *Logger) Flush() {
	l.wg.Wait()
}

// Close flushes pending saves and performs cleanup.
// This should be called on application shutdown.
func (l *Logger) Close() error {
	l.Flush()
	return nil
}

// Stats returns audit statistics.
func (l *Logger) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()

	stats := Stats{
		TotalEntries:  len(l.entries),
		SessionID:     l.sessionID,
		Enabled:       l.enabled,
		ToolBreakdown: make(map[string]int),
	}

	var successCount, errorCount int
	var totalDuration time.Duration

	for _, entry := range l.entries {
		stats.ToolBreakdown[entry.ToolName]++
		if entry.Success {
			successCount++
		} else {
			errorCount++
		}
		totalDuration += entry.Duration
	}

	stats.SuccessCount = successCount
	stats.ErrorCount = errorCount
	if len(l.entries) > 0 {
		stats.AvgDuration = totalDuration / time.Duration(len(l.entries))
	}

	return stats
}

// Stats holds audit statistics.
type Stats struct {
	TotalEntries  int
	SuccessCount  int
	ErrorCount    int
	AvgDuration   time.Duration
	SessionID     string
	Enabled       bool
	ToolBreakdown map[string]int
}

// getFilePath returns the file path for the current session's audit log.
func (l *Logger) getFilePath() string {
	return filepath.Join(l.configDir, "audit", l.sessionID+".json")
}

// load loads existing entries from disk.
func (l *Logger) load() error {
	filePath := l.getFilePath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	var entries []*Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}

	l.entries = entries
	return nil
}

// save persists entries to disk.
func (l *Logger) save() error {
	l.mu.RLock()
	data, err := json.MarshalIndent(l.entries, "", "  ")
	l.mu.RUnlock()

	if err != nil {
		return err
	}

	filePath := l.getFilePath()
	return os.WriteFile(filePath, data, 0644)
}

// Cleanup removes entries older than retention period.
// Returns the number of entries removed.
func (l *Logger) Cleanup() int {
	if !l.enabled {
		return 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-l.retention)
	removed := 0

	var kept []*Entry
	for _, entry := range l.entries {
		if entry.Timestamp.After(cutoff) {
			kept = append(kept, entry)
		} else {
			removed++
		}
	}

	l.entries = kept

	if removed > 0 {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.save()
		}()
	}

	return removed
}

// CleanupOldFiles removes session files older than the retention period.
func (l *Logger) CleanupOldFiles() (int, error) {
	if !l.enabled {
		return 0, nil
	}

	auditDir := filepath.Join(l.configDir, "audit")
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-l.retention)
	removed := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			filePath := filepath.Join(auditDir, entry.Name())
			if err := os.Remove(filePath); err == nil {
				removed++
			}
		}
	}

	return removed, nil
}

// GetSessions returns a list of all session IDs with audit logs.
func (l *Logger) GetSessions() ([]SessionInfo, error) {
	auditDir := filepath.Join(l.configDir, "audit")
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		return nil, err
	}

	var sessions []SessionInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		sessionID := entry.Name()[:len(entry.Name())-5] // Remove .json
		sessions = append(sessions, SessionInfo{
			ID:        sessionID,
			ModTime:   info.ModTime(),
			Size:      info.Size(),
			IsCurrent: sessionID == l.sessionID,
		})
	}

	// Sort by modification time (newest first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})

	return sessions, nil
}

// SessionInfo holds information about an audit session.
type SessionInfo struct {
	ID        string
	ModTime   time.Time
	Size      int64
	IsCurrent bool
}
