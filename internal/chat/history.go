package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gokin/internal/fileutil"
	"gokin/internal/logging"
)

// HistoryEntry represents a saved history entry.
type HistoryEntry struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// HistoryFile represents a saved session history.
type HistoryFile struct {
	SessionID string         `json:"session_id"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time"`
	Entries   []HistoryEntry `json:"entries"`
}

// HistoryManager manages session history persistence.
type HistoryManager struct {
	dataDir string
}

// NewHistoryManager creates a new history manager.
func NewHistoryManager() (*HistoryManager, error) {
	// Get data directory
	dataDir, err := getDataDir()
	if err != nil {
		return nil, err
	}

	// Create directory if it doesn't exist (0700: only owner can access session data)
	if err := ensurePrivateDir(dataDir); err != nil {
		return nil, err
	}

	return &HistoryManager{
		dataDir: dataDir,
	}, nil
}

// Save saves a session history to disk.
// Save persists a degraded view of the session — only role+text per
// entry, no tool calls / responses. This file format predates the
// fuller SessionState used by SaveFull/LoadFull and is kept for
// backward-compatible reads of older session files.
//
// Deprecated: use SaveFull. Save loses tool interactions, so any
// session resumed from a Save'd file will look like the model never
// called any tools. Last in-tree user (the shutdown fallback in
// app/signals.go saveSessionHistory) was switched to SaveFull —
// kept around only for backward-compatible reads of older session
// files written by the legacy code path.
func (m *HistoryManager) Save(session *Session) error {
	if session == nil {
		return fmt.Errorf("cannot save a nil session")
	}
	file, err := snapshotLegacyHistory(session)
	if err != nil {
		return err
	}
	if err := ValidateSessionID(file.SessionID); err != nil {
		return err
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(data)) > maxLegacyHistoryFileBytes {
		return fmt.Errorf("legacy session history is too large: %d bytes (maximum %d)", len(data), maxLegacyHistoryFileBytes)
	}
	if err := validateBoundedJSON(data); err != nil {
		return fmt.Errorf("legacy session history cannot be safely persisted: %w", err)
	}

	// Write file (0600: only owner can read/write session data).
	// AtomicWrite (tmp + Sync + rename) so a SIGKILL mid-write doesn't
	// leave a half-flushed JSON that fails to load on next start.
	if err := ensurePrivateDir(m.dataDir); err != nil {
		return err
	}
	filename, err := sessionJSONPath(m.dataDir, file.SessionID)
	if err != nil {
		return err
	}
	if err := preparePrivateWriteTarget(filename); err != nil {
		return err
	}
	return fileutil.AtomicWrite(filename, data, 0600)
}

// snapshotLegacyHistory captures identity and degraded role/text history under
// one Session lock. It avoids both the old lock-free ID/StartTime reads and the
// unnecessary recursive branch/tool-state copy that GetState would perform.
func snapshotLegacyHistory(session *Session) (HistoryFile, error) {
	session.mu.RLock()
	defer session.mu.RUnlock()

	now := time.Now()
	file := HistoryFile{
		SessionID: session.ID,
		StartTime: session.StartTime,
		EndTime:   now,
		Entries:   make([]HistoryEntry, 0, len(session.History)),
	}
	if len(session.History) > maxJSONContainerEntries {
		return HistoryFile{}, fmt.Errorf("legacy history has more than %d entries", maxJSONContainerEntries)
	}
	remaining := maxLegacyHistoryFileBytes
	for _, content := range session.History {
		if content == nil {
			continue
		}
		var text strings.Builder
		for _, part := range content.Parts {
			if part != nil && part.Text != "" {
				encoded := jsonStringEncodedLen(part.Text)
				if encoded > remaining {
					return HistoryFile{}, fmt.Errorf("legacy session history is too large (maximum %d bytes)", maxLegacyHistoryFileBytes)
				}
				remaining -= encoded
				text.WriteString(part.Text)
			}
		}
		file.Entries = append(file.Entries, HistoryEntry{
			Role:      string(content.Role),
			Content:   text.String(),
			Timestamp: now,
		})
	}
	return file, nil
}

// Load loads a session history from disk.
func (m *HistoryManager) Load(sessionID string) (*HistoryFile, error) {
	if err := ensurePrivateDir(m.dataDir); err != nil {
		return nil, err
	}
	filename, err := sessionJSONPath(m.dataDir, sessionID)
	if err != nil {
		return nil, err
	}

	data, err := readPrivateRegularFile(filename, maxLegacyHistoryFileBytes)
	if err != nil {
		return nil, err
	}
	if err := validateBoundedJSON(data); err != nil {
		return nil, fmt.Errorf("invalid legacy session history: %w", err)
	}

	var file HistoryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.SessionID == "" || file.SessionID != sessionID {
		return nil, fmt.Errorf("session identity mismatch: requested %q, file contains %q", sessionID, file.SessionID)
	}

	return &file, nil
}

// List lists all saved sessions.
func (m *HistoryManager) List() ([]string, error) {
	names, err := scanJSONFiles(m.dataDir)
	if err != nil {
		return nil, err
	}

	sessions := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(m.dataDir, name)
		f, _, openErr := openVerifiedRegular(path)
		if openErr != nil {
			continue
		}
		if closeErr := f.Close(); closeErr != nil {
			continue
		}
		sessions = append(sessions, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(sessions)
	return sessions, nil
}

// Delete deletes a session history.
func (m *HistoryManager) Delete(sessionID string) error {
	if err := ensurePrivateDir(m.dataDir); err != nil {
		return err
	}
	filename, err := sessionJSONPath(m.dataDir, sessionID)
	if err != nil {
		return err
	}
	return removePrivateRegular(filename)
}

// getDataDir returns the data directory for history storage.
func getDataDir() (string, error) {
	// Check XDG_DATA_HOME first
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "gokin", "history"), nil
	}

	// Fall back to ~/.local/share
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, ".local", "share", "gokin", "history"), nil
}

// getSessionsDir returns the data directory for full session storage.
func getSessionsDir() (string, error) {
	// Check XDG_DATA_HOME first
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "gokin", "sessions"), nil
	}

	// Fall back to ~/.local/share
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, ".local", "share", "gokin", "sessions"), nil
}

// SaveFull saves a complete session state including all content.
// Uses atomic write (tmp + rename) to prevent corruption on crash.
func (m *HistoryManager) SaveFull(session *Session) error {
	if session == nil {
		return fmt.Errorf("cannot save a nil session")
	}
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return err
	}

	// Create directory if it doesn't exist (0700: only owner can access session data)
	if err := ensurePrivateDir(sessionsDir); err != nil {
		return err
	}

	state := session.GetState()
	if err := ValidateSessionID(state.ID); err != nil {
		return err
	}
	if err := validateJSONPayloadForSave(state, maxSessionFileBytes); err != nil {
		return fmt.Errorf("session state cannot be safely persisted: %w", err)
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(data)) > maxSessionFileBytes {
		return fmt.Errorf("session state is too large: %d bytes (maximum %d)", len(data), maxSessionFileBytes)
	}
	if err := validateBoundedJSON(data); err != nil {
		return fmt.Errorf("session state cannot be safely persisted: %w", err)
	}

	// Atomic write via fileutil.AtomicWrite (tmp + Sync + rename).
	// Pre-fix this used WriteFile + Rename without Sync, so a power loss
	// between WriteFile and Rename could leave the rename pointing at an
	// unflushed empty file.
	//
	// Use the ID from the locked GetState() snapshot, NOT a lock-free
	// session.ID re-read: the field is mutated concurrently (SetID via /save),
	// so re-reading it here both races that write and can diverge from the ID
	// serialized inside `state` — persisting the state under the wrong filename.
	filename, err := sessionJSONPath(sessionsDir, state.ID)
	if err != nil {
		return err
	}
	if err := preparePrivateWriteTarget(filename); err != nil {
		return err
	}
	return fileutil.AtomicWrite(filename, data, 0600)
}

// LoadFull loads a complete session state.
func (m *HistoryManager) LoadFull(sessionID string) (*SessionState, error) {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(sessionsDir); err != nil {
		return nil, err
	}

	filename, err := sessionJSONPath(sessionsDir, sessionID)
	if err != nil {
		return nil, err
	}

	data, err := readPrivateRegularFile(filename, maxSessionFileBytes)
	if err != nil {
		if errors.Is(err, errSessionFileTooLarge) {
			return nil, fmt.Errorf("%w: %w", errSessionStateCorrupt, err)
		}
		return nil, err
	}
	if err := validateBoundedJSON(data); err != nil {
		return nil, fmt.Errorf("%w: invalid session state: %w", errSessionStateCorrupt, err)
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: %w", errSessionStateCorrupt, err)
	}
	// The filename is the persistence identity. Never trust an embedded ID
	// that names another file: ListSessions/LoadLast use this boundary to keep
	// one project's metadata from redirecting resume to a different session.
	if ValidateSessionID(state.ID) != nil || state.ID != sessionID {
		return nil, fmt.Errorf("%w: requested %q, file contains %q", errSessionIdentityMismatch, sessionID, state.ID)
	}

	return &state, nil
}

// sessionMeta mirrors ONLY the fields ListSessions needs, so a directory scan
// doesn't deserialize every message of every session into the full
// SerializedContent tree. History is decoded as raw array elements — we only
// need its length (MessageCount), not its contents. This is the difference
// between O(total messages across all sessions) struct allocation and a shallow
// per-file parse; it kept startup auto-resume from scaling with conversation
// size (the low-sev half of the v0.100.13 reliability audit).
type sessionMeta struct {
	ID         string            `json:"id"`
	StartTime  time.Time         `json:"start_time"`
	LastActive time.Time         `json:"last_active"`
	WorkDir    string            `json:"work_dir"`
	Summary    string            `json:"summary"`
	History    []json.RawMessage `json:"history"`
}

func loadSessionMeta(path string) (SessionInfo, error) {
	data, err := readPrivateRegularFile(path, maxSessionFileBytes)
	if err != nil {
		return SessionInfo{}, err
	}
	if err := validateBoundedJSON(data); err != nil {
		return SessionInfo{}, err
	}
	var meta sessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return SessionInfo{}, err
	}
	return SessionInfo{
		ID:           meta.ID,
		StartTime:    meta.StartTime,
		LastActive:   meta.LastActive,
		Summary:      meta.Summary,
		MessageCount: len(meta.History),
		WorkDir:      meta.WorkDir,
	}, nil
}

// ListSessions returns information about all saved sessions.
func (m *HistoryManager) ListSessions() ([]SessionInfo, error) {
	sessions, problems, err := m.ListSessionsWithProblems()
	// A session that cannot be read is a conversation the user still has and
	// cannot reach. Callers on this path discard that fact, so record it here:
	// without a line naming the file, an unreadable session is indistinguishable
	// from never having saved one, and auto-resume simply starts fresh.
	for _, p := range problems {
		logging.Warn("unreadable session file skipped", "error", p)
	}
	return sessions, err
}

// ListSessionsWithProblems returns the readable sessions plus one error per
// file that could not be read, following the same two-return shape the loop
// store uses. The distinction matters at exactly one moment: when the readable
// list comes back empty. "No saved sessions" is then a claim about the user's
// history, and it is false if a file was skipped — so a caller that reports
// emptiness to a person must ask for the problems and say which files it could
// not read.
func (m *HistoryManager) ListSessionsWithProblems() ([]SessionInfo, []error, error) {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return nil, nil, err
	}

	// Create directory if it doesn't exist (0700: only owner can access session data)
	names, err := scanJSONFiles(sessionsDir)
	if err != nil {
		return nil, nil, err
	}

	var sessions []SessionInfo
	var problems []error
	for _, name := range names {
		info, err := loadSessionMeta(filepath.Join(sessionsDir, name))
		if err != nil {
			problems = append(problems, fmt.Errorf("session %s: %w", name, err))
			continue
		}

		// Bind shallow metadata to the file it came from. Previously a corrupt
		// `redirect.json` with `{"id":"other-session"}` was returned as
		// other-session; LoadLast would then open other-session.json, bypassing
		// the work-directory filter applied to the redirect's metadata.
		filenameID := strings.TrimSuffix(name, ".json")
		if ValidateSessionID(info.ID) != nil || info.ID != filenameID {
			problems = append(problems, fmt.Errorf("session %s: file names %q but contains id %q",
				name, filenameID, info.ID))
			continue
		}
		sessions = append(sessions, info)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastActive.Equal(sessions[j].LastActive) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})

	return sessions, problems, nil
}

// DeleteSession deletes a saved session.
func (m *HistoryManager) DeleteSession(sessionID string) error {
	sessionsDir, err := getSessionsDir()
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(sessionsDir); err != nil {
		return err
	}

	filename, err := sessionJSONPath(sessionsDir, sessionID)
	if err != nil {
		return err
	}
	return removePrivateRegular(filename)
}
