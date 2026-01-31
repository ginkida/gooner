package context

import (
	"google.golang.org/genai"
)

// SummaryStrategy defines how messages should be summarized and what to keep.
type SummaryStrategy struct {
	// Keep system prompts (always true by default)
	KeepSystemPrompts bool
	// Keep tool calls in the middle section
	KeepToolCalls bool
	// Keep file references and mentions
	KeepFileReferences bool
	// Number of recent messages to always keep
	RecentMessageCount int
	// Number of initial messages to keep (system prompt + setup)
	InitialMessageCount int
	// Target ratio for summarization (0.5 = aim for 50% reduction)
	TargetRatio float64
	// Whether to use message importance scoring
	UseImportanceScoring bool
	// Minimum messages required before summarizing
	MinMessagesForSummary int
	// Maximum messages to keep in history (hard limit)
	MaxHistorySize int
}

// DefaultSummaryStrategy returns the default summarization strategy.
func DefaultSummaryStrategy() SummaryStrategy {
	return SummaryStrategy{
		KeepSystemPrompts:     true,
		KeepToolCalls:         true,
		KeepFileReferences:    true,
		RecentMessageCount:    10,
		InitialMessageCount:   2,
		TargetRatio:           0.5,
		UseImportanceScoring:  true,
		MinMessagesForSummary: 12,
		MaxHistorySize:        50,
	}
}

// CompactStrategy returns a more aggressive strategy for very long contexts.
func CompactStrategy() SummaryStrategy {
	return SummaryStrategy{
		KeepSystemPrompts:     true,
		KeepToolCalls:         false,
		KeepFileReferences:    true,
		RecentMessageCount:    6,
		InitialMessageCount:   2,
		TargetRatio:           0.3,
		UseImportanceScoring:  true,
		MinMessagesForSummary: 8,
		MaxHistorySize:        30,
	}
}

// VerboseStrategy returns a strategy that keeps more context.
func VerboseStrategy() SummaryStrategy {
	return SummaryStrategy{
		KeepSystemPrompts:     true,
		KeepToolCalls:         true,
		KeepFileReferences:    true,
		RecentMessageCount:    15,
		InitialMessageCount:   3,
		TargetRatio:           0.7,
		UseImportanceScoring:  true,
		MinMessagesForSummary: 20,
		MaxHistorySize:        100,
	}
}

// SummaryPlan represents the plan for summarizing messages.
type SummaryPlan struct {
	// Messages to keep from the start
	KeepStart []*genai.Content
	// Messages to summarize in the middle
	ToSummarize []*genai.Content
	// Messages to keep from the end
	KeepEnd []*genai.Content
	// Estimated token savings
	EstimatedSavings int
	// Reason for the plan
	Reason string
}

// CreateSummaryPlan creates a plan for summarizing messages based on strategy.
func CreateSummaryPlan(
	messages []*genai.Content,
	strategy SummaryStrategy,
	scorer *MessageScorer,
) *SummaryPlan {
	if len(messages) < strategy.MinMessagesForSummary {
		return &SummaryPlan{
			KeepStart:        messages,
			ToSummarize:      []*genai.Content{},
			KeepEnd:          []*genai.Content{},
			EstimatedSavings: 0,
			Reason:           "Not enough messages to summarize",
		}
	}

	// Hard limit check
	if len(messages) > strategy.MaxHistorySize {
		// Need aggressive summarization
		strategy.RecentMessageCount = strategy.MaxHistorySize / 3
		if strategy.RecentMessageCount < 5 {
			strategy.RecentMessageCount = 5
		}
	}

	// Calculate split points
	keepStart := strategy.InitialMessageCount
	keepEnd := strategy.RecentMessageCount

	// Ensure we don't overlap
	if keepStart+keepEnd >= len(messages) {
		keepEnd = len(messages) - keepStart - 1
		if keepEnd < 0 {
			keepEnd = 0
		}
	}

	// Build the plan
	plan := &SummaryPlan{
		KeepStart:   messages[:keepStart],
		ToSummarize: messages[keepStart : len(messages)-keepEnd],
		KeepEnd:     messages[len(messages)-keepEnd:],
		Reason:      "Standard summarization plan",
	}

	// Apply importance scoring if enabled
	if strategy.UseImportanceScoring && scorer != nil {
		plan = refinePlanWithScoring(plan, messages, strategy, scorer)
	}

	// Estimate savings (rough estimate)
	plan.EstimatedSavings = estimateTokenSavings(plan.ToSummarize, strategy.TargetRatio)

	return plan
}

// refinePlanWithScoring adjusts the summary plan based on message importance.
func refinePlanWithScoring(
	plan *SummaryPlan,
	messages []*genai.Content,
	strategy SummaryStrategy,
	scorer *MessageScorer,
) *SummaryPlan {
	// Score the middle section
	scores := scorer.ScoreMessages(plan.ToSummarize)

	// Identify critical messages in the middle that should be kept
	var keepFromMiddle []*genai.Content
	var newToSummarize []*genai.Content

	for i, msg := range plan.ToSummarize {
		score := scores[i]
		// Always keep critical or high priority messages
		if score.Priority == PriorityCritical || score.Priority == PriorityHigh {
			if strategy.KeepToolCalls || len(score.ToolsUsed) == 0 {
				keepFromMiddle = append(keepFromMiddle, msg)
				continue
			}
		}
		// Summarize the rest
		newToSummarize = append(newToSummarize, msg)
	}

	// Rebuild plan with adjustments
	refinedPlan := &SummaryPlan{
		KeepStart:   append(plan.KeepStart, keepFromMiddle...),
		ToSummarize: newToSummarize,
		KeepEnd:     plan.KeepEnd,
		Reason:      "Plan refined with importance scoring",
	}

	return refinedPlan
}

// estimateTokenSavings provides a rough estimate of token savings.
func estimateTokenSavings(messages []*genai.Content, ratio float64) int {
	if len(messages) == 0 {
		return 0
	}

	// Estimate current tokens
	currentTokens := EstimateContentsTokens(messages)

	// Estimate summary tokens (assume 30% of original after summarization)
	estimatedSummaryTokens := int(float64(currentTokens) * 0.3)

	// Savings = current - summary - overhead
	savings := currentTokens - estimatedSummaryTokens - 200 // 200 tokens for summary overhead
	if savings < 0 {
		savings = 0
	}

	return savings
}

// ApplySummaryPlan executes a summary plan by creating a summary message.
func ApplySummaryPlan(
	plan *SummaryPlan,
	summary *genai.Content,
) []*genai.Content {
	// If nothing to summarize, return original
	if len(plan.ToSummarize) == 0 {
		return append(plan.KeepStart, plan.KeepEnd...)
	}

	// Build new history: start + summary + end
	newHistory := make([]*genai.Content, 0,
		len(plan.KeepStart)+1+len(plan.KeepEnd))

	newHistory = append(newHistory, plan.KeepStart...)
	newHistory = append(newHistory, summary)
	newHistory = append(newHistory, plan.KeepEnd...)

	return newHistory
}
