package chat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

const (
	// MaxMessages is the maximum number of messages to keep in history.
	MaxMessages = 100
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
	ID                string
	StartTime         time.Time
	WorkDir           string
	History           []*genai.Content
	Branches          map[string]*Session // named branches (forks)
	Checkpoints       map[string]int      // named checkpoints (name -> history index)
	SystemInstruction string              // System prompt, passed via API parameter (not in history)
	tokenCounts       []int               // tokens per message
	totalTokens       int                 // cached total
	version           int64               // version for optimistic concurrency control
	onChange          ChangeHandler
	scratchpad        string
	mu                sync.RWMutex
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

	// Sync tokenCounts with History to avoid desynchronization
	if len(s.tokenCounts) > 0 {
		// Apply the same trimming logic to tokenCounts
		if len(s.tokenCounts) > MaxMessages {
			tcSystemMessages := systemMessages
			if tcSystemMessages > len(s.tokenCounts) {
				tcSystemMessages = 0
			}
			tcStartIdx := len(s.tokenCounts) - remaining
			if tcStartIdx < tcSystemMessages {
				tcStartIdx = tcSystemMessages
			}
			if tcStartIdx < len(s.tokenCounts) {
				s.tokenCounts = append(s.tokenCounts[:tcSystemMessages], s.tokenCounts[tcStartIdx:]...)
			}
		}

		// Recalculate totalTokens from remaining tokenCounts
		s.totalTokens = 0
		for _, count := range s.tokenCounts {
			s.totalTokens += count
		}
	}
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
		ID:                s.ID,
		StartTime:         s.StartTime,
		LastActive:        time.Now(),
		WorkDir:           s.WorkDir,
		History:           history,
		TokenCounts:       make([]int, len(s.tokenCounts)),
		TotalTokens:       s.totalTokens,
		Version:           s.version,
		Scratchpad:        s.scratchpad,
		SystemInstruction: s.SystemInstruction,
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
	s.scratchpad = state.Scratchpad
	s.SystemInstruction = state.SystemInstruction

	return nil
}

// GetScratchpad returns the current scratchpad content.
func (s *Session) GetScratchpad() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scratchpad
}

// SetScratchpad sets the scratchpad content.
func (s *Session) SetScratchpad(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scratchpad = content
}

// --- Session Branching (Forking) ---

// Fork creates a branch by copying the current history into a new Session.
// The branch is stored in the Branches map under the given name.
func (s *Session) Fork(name string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Branches == nil {
		s.Branches = make(map[string]*Session)
	}

	// Copy current history into a new session
	historyCopy := make([]*genai.Content, len(s.History))
	copy(historyCopy, s.History)

	tokenCountsCopy := make([]int, len(s.tokenCounts))
	copy(tokenCountsCopy, s.tokenCounts)

	branch := &Session{
		ID:          generateSessionID() + "-" + name,
		StartTime:   time.Now(),
		WorkDir:     s.WorkDir,
		History:     historyCopy,
		tokenCounts: tokenCountsCopy,
		totalTokens: s.totalTokens,
		scratchpad:  s.scratchpad,
	}

	s.Branches[name] = branch
	return branch
}

// GetBranch retrieves a branch by name.
func (s *Session) GetBranch(name string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Branches == nil {
		return nil, false
	}
	branch, ok := s.Branches[name]
	return branch, ok
}

// ListBranches returns all branch names.
func (s *Session) ListBranches() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Branches == nil {
		return nil
	}
	names := make([]string, 0, len(s.Branches))
	for name := range s.Branches {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// --- Named Checkpoints ---

// SaveCheckpoint saves the current history length as a named checkpoint.
func (s *Session) SaveCheckpoint(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Checkpoints == nil {
		s.Checkpoints = make(map[string]int)
	}
	s.Checkpoints[name] = len(s.History)
}

// RestoreCheckpoint truncates the history to the saved checkpoint index.
// Returns true if the checkpoint was found and restored, false otherwise.
func (s *Session) RestoreCheckpoint(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Checkpoints == nil {
		return false
	}
	idx, ok := s.Checkpoints[name]
	if !ok {
		return false
	}

	// Truncate history to checkpoint index
	if idx < len(s.History) {
		s.History = s.History[:idx]
	}

	// Truncate token counts to match
	if idx < len(s.tokenCounts) {
		s.tokenCounts = s.tokenCounts[:idx]
		// Recalculate total tokens
		s.totalTokens = 0
		for _, count := range s.tokenCounts {
			s.totalTokens += count
		}
	}

	// Remove any checkpoints that referenced indices beyond the new length
	for cpName, cpIdx := range s.Checkpoints {
		if cpIdx > idx {
			delete(s.Checkpoints, cpName)
		}
	}

	s.version++
	return true
}

// ListCheckpoints returns checkpoint names sorted by their history index.
func (s *Session) ListCheckpoints() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Checkpoints == nil {
		return nil
	}

	type cpEntry struct {
		name string
		idx  int
	}
	entries := make([]cpEntry, 0, len(s.Checkpoints))
	for name, idx := range s.Checkpoints {
		entries = append(entries, cpEntry{name: name, idx: idx})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].idx < entries[j].idx
	})

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

// --- Sensitive Data Redaction ---

// sensitivePatterns are compiled regex patterns for detecting sensitive data.
var sensitivePatterns = []*regexp.Regexp{
	// API keys: sk-... (OpenAI style)
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	// API keys: AIza... (Google style)
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{20,}`),
	// Generic long hex/base64 tokens (40+ chars)
	regexp.MustCompile(`(?i)(?:password|passwd|token|secret|api_key|apikey|api-key|access_key|auth)\s*[=:]\s*["']?([^\s"']{8,})["']?`),
	// Bearer tokens
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9_\-.]+`),
	// AWS access keys
	regexp.MustCompile(`AKIA[A-Z0-9]{16}`),
	// Generic key=value patterns
	regexp.MustCompile(`(?i)(?:key|token|password)\s*=\s*\S{8,}`),
}

// redactSensitiveData scans text and replaces sensitive patterns with [REDACTED].
func redactSensitiveData(text string) string {
	result := text
	for _, pattern := range sensitivePatterns {
		result = pattern.ReplaceAllString(result, "[REDACTED]")
	}
	return result
}

// --- Export ---

// ExportMarkdown exports the conversation as markdown.
// Each message is formatted with a ## User or ## Assistant header.
// Tool calls are formatted as code blocks.
func (s *Session) ExportMarkdown() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Session %s\n\n", s.ID))
	sb.WriteString(fmt.Sprintf("**Started:** %s\n\n", s.StartTime.Format("2006-01-02 15:04:05")))
	if s.WorkDir != "" {
		sb.WriteString(fmt.Sprintf("**Working Directory:** %s\n\n", s.WorkDir))
	}
	sb.WriteString("---\n\n")

	for _, content := range s.History {
		role := "Assistant"
		if content.Role == string(genai.RoleUser) {
			role = "User"
		}

		sb.WriteString(fmt.Sprintf("## %s\n\n", role))

		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				sb.WriteString(fmt.Sprintf("**Tool Call:** `%s`\n\n", part.FunctionCall.Name))
				if part.FunctionCall.Args != nil {
					argsJSON, err := json.MarshalIndent(part.FunctionCall.Args, "", "  ")
					if err == nil {
						redacted := redactSensitiveData(string(argsJSON))
						sb.WriteString("```json\n")
						sb.WriteString(redacted)
						sb.WriteString("\n```\n\n")
					}
				}
			} else if part.FunctionResponse != nil {
				sb.WriteString(fmt.Sprintf("**Tool Response:** `%s`\n\n", part.FunctionResponse.Name))
				if part.FunctionResponse.Response != nil {
					respJSON, err := json.MarshalIndent(part.FunctionResponse.Response, "", "  ")
					if err == nil {
						redacted := redactSensitiveData(string(respJSON))
						sb.WriteString("```json\n")
						sb.WriteString(redacted)
						sb.WriteString("\n```\n\n")
					}
				}
			} else if part.Text != "" {
				redacted := redactSensitiveData(part.Text)
				sb.WriteString(redacted)
				sb.WriteString("\n\n")
			}
		}
	}

	return sb.String()
}

// ExportJSON exports the session as JSON with history, metadata, and timestamps.
// Sensitive data is redacted before export.
func (s *Session) ExportJSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Serialize history
	history := make([]SerializedContent, len(s.History))
	for i, content := range s.History {
		history[i] = SerializeContent(content)
	}

	// Redact sensitive data in serialized history
	for i := range history {
		for j := range history[i].Parts {
			history[i].Parts[j].Text = redactSensitiveData(history[i].Parts[j].Text)
			if history[i].Parts[j].FunctionCall != nil {
				redactMapValues(history[i].Parts[j].FunctionCall.Args)
			}
			if history[i].Parts[j].FunctionResp != nil {
				redactMapValues(history[i].Parts[j].FunctionResp.Response)
			}
		}
	}

	export := struct {
		ID          string              `json:"id"`
		StartTime   time.Time           `json:"start_time"`
		ExportedAt  time.Time           `json:"exported_at"`
		WorkDir     string              `json:"work_dir,omitempty"`
		History     []SerializedContent `json:"history"`
		TotalTokens int                 `json:"total_tokens"`
		Version     int64               `json:"version"`
		Scratchpad  string              `json:"scratchpad,omitempty"`
	}{
		ID:          s.ID,
		StartTime:   s.StartTime,
		ExportedAt:  time.Now(),
		WorkDir:     s.WorkDir,
		History:     history,
		TotalTokens: s.totalTokens,
		Version:     s.version,
		Scratchpad:  redactSensitiveData(s.scratchpad),
	}

	return json.MarshalIndent(export, "", "  ")
}

// redactMapValues recursively redacts sensitive data in map string values.
func redactMapValues(m map[string]any) {
	if m == nil {
		return
	}
	for k, v := range m {
		switch val := v.(type) {
		case string:
			m[k] = redactSensitiveData(val)
		case map[string]any:
			redactMapValues(val)
		}
	}
}
