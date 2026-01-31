package context

import (
	"strings"

	"google.golang.org/genai"
)

// MessagePriority represents the importance level of a message.
type MessagePriority int

const (
	PriorityLow      MessagePriority = 0 // Verbose logs, trivial reads
	PriorityNormal   MessagePriority = 1 // Normal messages
	PriorityHigh     MessagePriority = 2 // File edits, important decisions
	PriorityCritical MessagePriority = 3 // System prompts, errors
)

// MessageScore represents the importance score and metadata for a message.
type MessageScore struct {
	Priority    MessagePriority
	Score       float64 // 0.0 - 1.0
	Reason      string  // Explanation of the score
	IsSystem    bool
	HasFileEdit bool
	HasError    bool
	ToolsUsed   []string
	References  []string // File paths, function names
}

// MessageScorer evaluates message importance for context retention decisions.
type MessageScorer struct {
	// Tools that should be considered high priority
	criticalTools map[string]bool
	// Tools that should be considered low priority
	verboseTools map[string]bool
}

// NewMessageScorer creates a new message scorer with default configuration.
func NewMessageScorer() *MessageScorer {
	return &MessageScorer{
		criticalTools: map[string]bool{
			"write": true,
			"edit":  true,
			"bash":  true,
		},
		verboseTools: map[string]bool{
			"read":        true,
			"list_dir":    true,
			"tree":        true,
			"glob":        true,
			"git_log":     true,
			"env":         true,
			"task_output": true,
		},
	}
}

// ScoreMessage evaluates a single message and returns its importance score.
func (s *MessageScorer) ScoreMessage(msg *genai.Content) MessageScore {
	score := MessageScore{
		Priority:   PriorityNormal,
		Score:      0.5,
		Reason:     "normal message",
		ToolsUsed:  make([]string, 0),
		References: make([]string, 0),
	}

	// Check role
	if msg.Role == genai.RoleUser {
		// User messages are generally important
		score.Score += 0.1
	}

	// Analyze parts
	for _, part := range msg.Parts {
		s.scoreTextPart(&score, part.Text)
		s.scoreFunctionCall(&score, part.FunctionCall)
		s.scoreFunctionResponse(&score, part.FunctionResponse)
	}

	// Determine final priority based on score
	if score.IsSystem || score.HasError {
		score.Priority = PriorityCritical
		score.Score = 1.0
	} else if score.HasFileEdit {
		score.Priority = PriorityHigh
		score.Score = 0.8
	} else if score.Score < 0.3 {
		score.Priority = PriorityLow
	} else if score.Score > 0.7 {
		score.Priority = PriorityHigh
	}

	return score
}

// scoreTextPart analyzes text content for importance indicators.
func (s *MessageScorer) scoreTextPart(score *MessageScore, text string) {
	if text == "" {
		return
	}

	lower := strings.ToLower(text)

	// Check for system indicators
	if strings.Contains(lower, "system prompt") ||
		strings.Contains(lower, "instructions") ||
		strings.Contains(lower, "context preservation") {
		score.IsSystem = true
		score.Reason = "system instructions"
		score.Score += 0.4
	}

	// Check for decision keywords
	decisionKeywords := []string{
		"decided to", "will implement", "going to", "plan to",
		"summary", "conclusion", "resolved", "fixed",
	}
	for _, keyword := range decisionKeywords {
		if strings.Contains(lower, keyword) {
			score.Score += 0.1
		}
	}

	// Check for error indicators
	errorKeywords := []string{
		"error", "failed", "exception", "bug", "issue",
		"problem", "warning", "not found",
	}
	for _, keyword := range errorKeywords {
		if strings.Contains(lower, keyword) {
			score.HasError = true
			score.Score += 0.2
		}
	}

	// Check for file references (simple heuristic)
	if strings.Contains(text, ".go") ||
		strings.Contains(text, ".md") ||
		strings.Contains(text, ".yaml") ||
		strings.Contains(text, ".json") {
		// Extract potential file paths
		words := strings.Fields(text)
		for _, word := range words {
			if strings.Contains(word, "/") &&
				(strings.HasSuffix(word, ".go") ||
					strings.HasSuffix(word, ".md") ||
					strings.HasSuffix(word, ".yaml") ||
					strings.HasSuffix(word, ".json")) {
				score.References = append(score.References, strings.Trim(word, "`'\""))
			}
		}
	}
}

// scoreFunctionCall analyzes function calls for importance.
func (s *MessageScorer) scoreFunctionCall(score *MessageScore, fc *genai.FunctionCall) {
	if fc == nil {
		return
	}

	score.ToolsUsed = append(score.ToolsUsed, fc.Name)

	// Check if it's a critical tool (file modifications)
	if s.criticalTools[fc.Name] {
		score.HasFileEdit = true
		score.Score += 0.3
		score.Reason = "file modification operation"

		// Extract file paths from args
		if path, ok := fc.Args["file_path"].(string); ok {
			score.References = append(score.References, path)
		}
		if path, ok := fc.Args["path"].(string); ok {
			score.References = append(score.References, path)
		}
	}

	// Check if it's a verbose tool (information gathering)
	if s.verboseTools[fc.Name] {
		score.Score -= 0.1
		if score.Score < 0.2 {
			score.Score = 0.2
		}
		if score.Reason == "normal message" {
			score.Reason = "information gathering"
		}
	}
}

// scoreFunctionResponse analyzes function responses for importance.
func (s *MessageScorer) scoreFunctionResponse(score *MessageScore, fr *genai.FunctionResponse) {
	if fr == nil {
		return
	}

	// Check for errors in response
	if fr.Response != nil {
		if errMsg, ok := fr.Response["error"].(string); ok && errMsg != "" {
			score.HasError = true
			score.Score += 0.3
		}

		// Check content size - very large responses are less important
		if content, ok := fr.Response["content"].(string); ok {
			if len(content) > 5000 {
				score.Score -= 0.1
			}
		}

		// Check for success indicators
		if success, ok := fr.Response["success"].(bool); ok && success {
			score.Score += 0.05
		}
	}
}

// ScoreMessages scores a batch of messages.
func (s *MessageScorer) ScoreMessages(messages []*genai.Content) []MessageScore {
	scores := make([]MessageScore, len(messages))
	for i, msg := range messages {
		scores[i] = s.ScoreMessage(msg)
	}
	return scores
}

// SelectImportantMessages selects messages to keep based on scores and target count.
// It uses a combination of priority and recency to make decisions.
func (s *MessageScorer) SelectImportantMessages(
	messages []*genai.Content,
	scores []MessageScore,
	keepCount int,
) []*genai.Content {
	if len(messages) <= keepCount {
		return messages
	}

	// Always keep critical priority messages
	selected := make([]*genai.Content, 0, keepCount)
	selectedIndices := make(map[int]bool)

	// First pass: add all critical messages
	for i, score := range scores {
		if score.Priority == PriorityCritical && len(selected) < keepCount {
			selected = append(selected, messages[i])
			selectedIndices[i] = true
		}
	}

	// Second pass: add high priority messages
	for i, score := range scores {
		if score.Priority == PriorityHigh && !selectedIndices[i] && len(selected) < keepCount {
			selected = append(selected, messages[i])
			selectedIndices[i] = true
		}
	}

	// Third pass: fill with most recent messages if space remains
	if len(selected) < keepCount {
		// Start from the end (most recent)
		for i := len(messages) - 1; i >= 0 && len(selected) < keepCount; i-- {
			if !selectedIndices[i] {
				selected = append(selected, messages[i])
				selectedIndices[i] = true
			}
		}
	}

	return selected
}

// CalculateTokenBudget calculates how many tokens should be allocated for messages
// based on their importance scores.
func (s *MessageScorer) CalculateTokenBudget(
	scores []MessageScore,
	totalBudget int,
) []int {
	if len(scores) == 0 {
		return []int{}
	}

	// Calculate total score
	totalScore := 0.0
	for _, score := range scores {
		totalScore += float64(score.Priority) + score.Score
	}

	// Allocate budget proportionally
	budgets := make([]int, len(scores))
	for i, score := range scores {
		weight := (float64(score.Priority) + score.Score) / totalScore
		budgets[i] = int(float64(totalBudget) * weight)
		if budgets[i] < 100 { // Minimum allocation
			budgets[i] = 100
		}
	}

	return budgets
}
