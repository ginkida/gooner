package chat

import (
	"sync"
	"time"

	"google.golang.org/genai"
)

const (
	// MaxMessages is the maximum number of messages to keep in history.
	MaxMessages = 50
)

// ChangeEvent represents a session history change event.
type ChangeEvent struct {
	OldCount int
	NewCount int
	Version  int64
}

// ChangeHandler is called when session history changes.
type ChangeHandler func(ChangeEvent)

// Session represents a chat session.
type Session struct {
	ID          string
	StartTime   time.Time
	WorkDir     string
	History     []*genai.Content
	tokenCounts []int // tokens per message
	totalTokens int   // cached total
	version     int64 // version for optimistic concurrency control
	onChange    ChangeHandler
	mu          sync.RWMutex
}

// NewSession creates a new chat session.
func NewSession() *Session {
	return &Session{
		ID:        generateSessionID(),
		StartTime: time.Now(),
		History:   make([]*genai.Content, 0),
	}
}

// SetChangeHandler sets the callback for history changes.
func (s *Session) SetChangeHandler(handler ChangeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = handler
}

// notifyChange notifies the handler of history changes.
func (s *Session) notifyChange(oldCount int) {
	if s.onChange == nil {
		return
	}

	// Capture event data BEFORE unlocking to avoid race conditions
	event := ChangeEvent{
		OldCount: oldCount,
		NewCount: len(s.History),
		Version:  s.version,
	}

	// Release lock before calling handler to prevent deadlock
	s.mu.Unlock()

	// Protect against panics in the handler
	defer func() {
		if r := recover(); r != nil {
			// Log panic but don't crash
			// In production, you might want to log this
		}
	}()

	// Call handler outside lock (prevent deadlock if handler tries to access session)
	s.onChange(event)
}

// AddUserMessage adds a user message to the history.
func (s *Session) AddUserMessage(message string) {
	s.mu.Lock()

	oldCount := len(s.History)
	s.History = append(s.History, genai.NewContentFromText(message, genai.RoleUser))
	s.version++

	// Auto-trim if history exceeds max
	s.trimHistoryLocked()

	s.notifyChange(oldCount)
}

// AddModelMessage adds a model message to the history.
func (s *Session) AddModelMessage(message string) {
	s.mu.Lock()

	oldCount := len(s.History)
	s.History = append(s.History, genai.NewContentFromText(message, genai.RoleModel))
	s.version++

	// Auto-trim if history exceeds max
	s.trimHistoryLocked()

	s.notifyChange(oldCount)
}

// AddContent adds raw content to the history.
func (s *Session) AddContent(content *genai.Content) {
	s.mu.Lock()

	oldCount := len(s.History)
	s.History = append(s.History, content)
	s.version++

	// Auto-trim if history exceeds max
	s.trimHistoryLocked()

	s.notifyChange(oldCount)
}

// SetHistory replaces the entire history and applies sliding window.
func (s *Session) SetHistory(history []*genai.Content) {
	s.mu.Lock()

	oldCount := len(s.History)

	// Apply sliding window if history exceeds max
	if len(history) > MaxMessages {
		// Keep first 2 messages (system prompt + initial response) and last N messages
		systemMessages := 2
		if systemMessages > len(history) {
			systemMessages = 0
		}
		remaining := MaxMessages - systemMessages
		if remaining < 0 {
			remaining = 0
		}
		startIdx := len(history) - remaining
		if startIdx < systemMessages {
			startIdx = systemMessages
		}
		history = append(history[:systemMessages], history[startIdx:]...)
	}

	s.History = history
	s.version++
	s.notifyChange(oldCount)
}

// SetHistoryIfVersion atomically sets history only if the version matches.
// Returns true if the update was applied, false if version mismatch.
func (s *Session) SetHistoryIfVersion(history []*genai.Content, expectedVersion int64) bool {
	s.mu.Lock()

	if s.version != expectedVersion {
		s.mu.Unlock()
		return false
	}

	oldCount := len(s.History)

	// Apply sliding window if history exceeds max
	if len(history) > MaxMessages {
		systemMessages := 2
		if systemMessages > len(history) {
			systemMessages = 0
		}
		remaining := MaxMessages - systemMessages
		if remaining < 0 {
			remaining = 0
		}
		startIdx := len(history) - remaining
		if startIdx < systemMessages {
			startIdx = systemMessages
		}
		history = append(history[:systemMessages], history[startIdx:]...)
	}

	s.History = history
	s.version++
	s.notifyChange(oldCount)
	return true
}

// GetVersion returns the current version of the session history.
func (s *Session) GetVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// GetHistoryWithVersion returns a copy of the history along with its version.
func (s *Session) GetHistoryWithVersion() ([]*genai.Content, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	history := make([]*genai.Content, len(s.History))
	copy(history, s.History)
	return history, s.version
}

// GetHistory returns a copy of the history.
func (s *Session) GetHistory() []*genai.Content {
	s.mu.RLock()
	defer s.mu.RUnlock()

	history := make([]*genai.Content, len(s.History))
	copy(history, s.History)
	return history
}

// Clear clears the session history.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.History = make([]*genai.Content, 0)
}

// MessageCount returns the number of messages in the session.
func (s *Session) MessageCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.History)
}

// trimHistoryLocked trims history to max messages.
// Caller MUST hold s.mu.Lock() before calling.
func (s *Session) trimHistoryLocked() {
	if len(s.History) <= MaxMessages {
		return
	}

	// Keep first 2 messages (system prompt + initial response) and last N messages
	systemMessages := 2
	if systemMessages > len(s.History) {
		systemMessages = 0
	}
	remaining := MaxMessages - systemMessages
	if remaining < 0 {
		remaining = 0
	}
	startIdx := len(s.History) - remaining
	if startIdx < systemMessages {
		startIdx = systemMessages
	}
	s.History = append(s.History[:systemMessages], s.History[startIdx:]...)
}

// TrimHistory manually triggers history trimming to max messages.
func (s *Session) TrimHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trimHistoryLocked()
}

// generateSessionID generates a unique session ID.
func generateSessionID() string {
	return time.Now().Format("20060102-150405")
}

// AddContentWithTokens adds content with its token count.
func (s *Session) AddContentWithTokens(content *genai.Content, tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldCount := len(s.History)
	s.History = append(s.History, content)
	s.tokenCounts = append(s.tokenCounts, tokens)
	s.totalTokens += tokens
	s.version++
	s.notifyChange(oldCount)
}

// GetTokenCount returns the cached total token count.
func (s *Session) GetTokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalTokens
}

// SetTotalTokens sets the total token count (from external counting).
func (s *Session) SetTotalTokens(tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalTokens = tokens
}

// ReplaceWithSummary replaces messages up to index with a summary.
func (s *Session) ReplaceWithSummary(upToIndex int, summary *genai.Content, summaryTokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if upToIndex > len(s.History) {
		upToIndex = len(s.History)
	}

	// Keep messages after upToIndex
	remaining := s.History[upToIndex:]

	// Safely handle tokenCounts which may be shorter than History
	var remainingTokens []int
	if upToIndex <= len(s.tokenCounts) {
		remainingTokens = s.tokenCounts[upToIndex:]
	}

	// Build new history with summary
	s.History = make([]*genai.Content, 0, 1+len(remaining))
	s.History = append(s.History, summary)
	s.History = append(s.History, remaining...)

	// Rebuild token counts
	s.tokenCounts = make([]int, 0, 1+len(remainingTokens))
	s.tokenCounts = append(s.tokenCounts, summaryTokens)
	s.tokenCounts = append(s.tokenCounts, remainingTokens...)

	// Recalculate total
	s.totalTokens = summaryTokens
	for _, t := range remainingTokens {
		s.totalTokens += t
	}
}

// GetTokenCounts returns token counts per message.
func (s *Session) GetTokenCounts() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make([]int, len(s.tokenCounts))
	copy(counts, s.tokenCounts)
	return counts
}

// SetWorkDir sets the working directory for this session.
func (s *Session) SetWorkDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WorkDir = dir
}

// GetState returns the current state of the session for serialization.
func (s *Session) GetState() *SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	history := make([]SerializedContent, len(s.History))
	for i, content := range s.History {
		history[i] = SerializeContent(content)
	}

	state := &SessionState{
		ID:          s.ID,
		StartTime:   s.StartTime,
		LastActive:  time.Now(),
		WorkDir:     s.WorkDir,
		History:     history,
		TokenCounts: make([]int, len(s.tokenCounts)),
		TotalTokens: s.totalTokens,
		Version:     s.version,
	}
	copy(state.TokenCounts, s.tokenCounts)

	// Generate summary
	state.Summary = state.GenerateSummary()

	return state
}

// RestoreFromState restores the session from a saved state.
func (s *Session) RestoreFromState(state *SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	history := make([]*genai.Content, len(state.History))
	for i, sc := range state.History {
		content, err := DeserializeContent(sc)
		if err != nil {
			return err
		}
		history[i] = content
	}

	s.ID = state.ID
	s.StartTime = state.StartTime
	s.WorkDir = state.WorkDir
	s.History = history
	s.tokenCounts = make([]int, len(state.TokenCounts))
	copy(s.tokenCounts, state.TokenCounts)
	s.totalTokens = state.TotalTokens
	s.version = state.Version

	return nil
}
