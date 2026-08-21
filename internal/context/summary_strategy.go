package context

import (
	"context"
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
		RecentMessageCount:    20,
		InitialMessageCount:   4,
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
		KeepToolCalls:         true, // Keep tool calls — losing them makes agent forget what it did
		KeepFileReferences:    true,
		RecentMessageCount:    8,
		InitialMessageCount:   2,
		TargetRatio:           0.35,
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

// CreateSummaryPlan keeps the no-context signature for callers that have none;
// it cannot cancel the semantic scoring an importance-scoring strategy may run.
// Any caller holding a context — every compaction path does — must use
// CreateSummaryPlanWithContext instead, the same split Router.Route keeps.
func CreateSummaryPlan(
	messages []*genai.Content,
	strategy SummaryStrategy,
	scorer *MessageScorer,
) *SummaryPlan {
	return CreateSummaryPlanWithContext(context.Background(), messages, strategy, scorer)
}

// CreateSummaryPlanWithContext creates a plan for summarizing messages based on
// strategy. The context matters because importance scoring can call the model:
// it carries both the caller's cancellation and its deadline down to that call.
func CreateSummaryPlanWithContext(
	ctx context.Context,
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

	// Adjust boundaries so FunctionCall/FunctionResponse pairs are not split
	startBoundary := AdjustBoundaryForToolPairs(messages, keepStart)
	endBoundary := AdjustBoundaryForToolPairs(messages, len(messages)-keepEnd)

	// Safety: if adjustments caused boundaries to overlap or invert, fall back to originals
	if startBoundary >= endBoundary {
		startBoundary = keepStart
		endBoundary = len(messages) - keepEnd
	}

	// Build the plan
	plan := &SummaryPlan{
		KeepStart:   messages[:startBoundary],
		ToSummarize: messages[startBoundary:endBoundary],
		KeepEnd:     messages[endBoundary:],
		Reason:      "Standard summarization plan",
	}

	// Apply importance scoring if enabled
	if strategy.UseImportanceScoring && scorer != nil {
		plan = refinePlanWithScoring(ctx, plan, messages, strategy, scorer)
	}

	// Estimate savings (rough estimate)
	plan.EstimatedSavings = estimateTokenSavings(plan.ToSummarize, strategy.TargetRatio)

	return plan
}

// refinePlanWithScoring adjusts the summary plan based on message importance.
func refinePlanWithScoring(
	ctx context.Context,
	plan *SummaryPlan,
	messages []*genai.Content,
	strategy SummaryStrategy,
	scorer *MessageScorer,
) *SummaryPlan {
	// Score the middle section. ScoreMessages would score on
	// context.Background(), so a compaction the caller bounded at 60s could sit
	// here for the full model-round cap and keep running after the user pressed
	// Esc; the context form inherits both bounds.
	scores := scorer.ScoreMessagesWithContext(ctx, plan.ToSummarize)

	// Identify critical messages in the middle that should be kept. Tool
	// exchanges are an atomic unit: a high-priority call (for example bash/edit)
	// frequently has an ordinary successful response, while an error response
	// can be critical even when its call is not. Keeping only the scored half
	// creates an orphan after ApplySummaryPlan; strict providers reject that
	// history and last-defense sanitizers have to discard the very operation we
	// intended to preserve.
	keep := make([]bool, len(plan.ToSummarize))
	for i, score := range scores {
		if score.Priority == PriorityCritical || score.Priority == PriorityHigh {
			if strategy.KeepToolCalls || len(score.ToolsUsed) == 0 {
				keep[i] = true
			}
		}
	}
	expandToolPairRetention(plan.ToSummarize, keep)

	var keepFromMiddle []*genai.Content
	var newToSummarize []*genai.Content

	for i, msg := range plan.ToSummarize {
		if keep[i] {
			keepFromMiddle = append(keepFromMiddle, msg)
			continue
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

// expandToolPairRetention closes an initial keep-set over tool-call IDs. A
// message may contain several parallel calls/results, so this deliberately
// propagates transitively through every ID on a newly retained message.
func expandToolPairRetention(messages []*genai.Content, keep []bool) {
	if len(messages) == 0 || len(keep) != len(messages) {
		return
	}

	messageIDs := make([][]string, len(messages))
	byID := make(map[string][]int)
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		seen := make(map[string]bool)
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			id := ""
			if part.FunctionCall != nil {
				id = part.FunctionCall.ID
			} else if part.FunctionResponse != nil {
				id = part.FunctionResponse.ID
			}
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			messageIDs[i] = append(messageIDs[i], id)
			byID[id] = append(byID[id], i)
		}
	}

	queue := make([]int, 0, len(messages))
	queued := make([]bool, len(messages))
	for i, retained := range keep {
		if retained {
			queue = append(queue, i)
			queued[i] = true
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		for _, id := range messageIDs[i] {
			for _, related := range byID[id] {
				if keep[related] {
					continue
				}
				keep[related] = true
				if !queued[related] {
					queue = append(queue, related)
					queued[related] = true
				}
			}
		}
	}
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
