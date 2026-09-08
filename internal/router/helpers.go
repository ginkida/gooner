package router

import (
	"fmt"
)

// String returns string representation
func (s ExecutionStrategy) String() string {
	return string(s)
}

// String returns string representation
func (t TaskType) String() string {
	return string(t)
}

// GetDescription returns a human-readable description
func (s ExecutionStrategy) GetDescription() string {
	switch s {
	case StrategyDirect:
		return "Direct response (core tools, fast model)"
	case StrategySingleTool:
		return "Single tool call"
	case StrategyExecutor:
		return "Standard execution"
	case StrategySubAgent:
		return "Specialized agent"
	default:
		return "Unknown strategy"
	}
}

// GetDescription returns a human-readable description
func (t TaskType) GetDescription() string {
	switch t {
	case TaskTypeQuestion:
		return "Simple question"
	case TaskTypeSingleTool:
		return "Single tool"
	case TaskTypeMultiTool:
		return "Multiple tools"
	case TaskTypeExploration:
		return "Code exploration"
	case TaskTypeRefactoring:
		return "Refactoring"
	case TaskTypeComplex:
		return "Complex task"
	case TaskTypeBackground:
		return "Background task"
	default:
		return "Unknown type"
	}
}

// FormatReasoning formats the reasoning with emojis for better readability
func (tc *TaskComplexity) FormatReasoning() string {
	emoji := tc.getEmoji()
	return fmt.Sprintf("%s %s [Strategy: %s]", emoji, tc.Reasoning, tc.Strategy)
}

// getEmoji returns an emoji based on task type
func (tc *TaskComplexity) getEmoji() string {
	switch tc.Type {
	case TaskTypeQuestion:
		return "❓"
	case TaskTypeSingleTool:
		return "🔧"
	case TaskTypeMultiTool:
		return "🔧🔧"
	case TaskTypeExploration:
		return "🔍"
	case TaskTypeRefactoring:
		return "♻️"
	case TaskTypeComplex:
		return "🚀"
	case TaskTypeBackground:
		return "⏳"
	default:
		return "📝"
	}
}

// IsValid checks if the strategy is valid
func (s ExecutionStrategy) IsValid() bool {
	switch s {
	case StrategyDirect, StrategySingleTool, StrategyExecutor, StrategySubAgent:
		return true
	default:
		return false
	}
}

// RequiresTools was removed: it answered false for StrategyDirect, and that
// was not true of any released build — executeDirect runs the same executor as
// every other strategy, with ToolSetCore (read/write/edit/bash/glob/grep) plus
// memory. Nothing read the predicate, so the falsehood cost nothing directly;
// it cost plenty indirectly, by supplying the premise for decisions made
// elsewhere. A wrong statement no one reads is worse than an absent one.
// RequiresMultipleAgents checks if the strategy requires multiple agents
func (s ExecutionStrategy) RequiresMultipleAgents() bool {
	return false // No coordinator support
}
