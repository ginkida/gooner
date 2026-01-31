package router

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"gokin/internal/client"
	"gokin/internal/logging"
)

// TaskType represents the type of task
type TaskType string

const (
	TaskTypeQuestion    TaskType = "question"    // Simple question requiring direct answer
	TaskTypeSingleTool  TaskType = "single_tool" // Can be done with one tool call
	TaskTypeMultiTool   TaskType = "multi_tool"  // Requires multiple tools sequentially
	TaskTypeExploration TaskType = "exploration" // Code exploration/research
	TaskTypeRefactoring TaskType = "refactoring" // Code changes/analysis
	TaskTypeComplex     TaskType = "complex"     // Complex multi-step task
	TaskTypeBackground  TaskType = "background"  // Long-running background task
)

// TaskComplexity represents the complexity level of a task
type TaskComplexity struct {
	Score     int // 1-10 scale
	Type      TaskType
	Strategy  ExecutionStrategy
	Reasoning string
}

// ExecutionStrategy determines how to execute the task
type ExecutionStrategy string

const (
	StrategyDirect     ExecutionStrategy = "direct"      // Direct AI response
	StrategySingleTool ExecutionStrategy = "single_tool" // One tool call
	StrategyExecutor   ExecutionStrategy = "executor"    // Standard function calling loop
	StrategySubAgent   ExecutionStrategy = "sub_agent"   // Spawn a sub-agent
)

// TaskAnalyzer analyzes task complexity and determines optimal execution strategy
type TaskAnalyzer struct {
	// Configuration
	decomposeThreshold int // Complexity score >= this triggers decomposition
	parallelThreshold  int // Complexity score >= this triggers parallel execution

	// Patterns for task type detection
	questionPatterns    []*regexp.Regexp
	explorationPatterns []*regexp.Regexp
	refactoringPatterns []*regexp.Regexp
	backgroundPatterns  []*regexp.Regexp
	multiToolPatterns   []*regexp.Regexp

	// LLM-based decomposition (Phase 2)
	llmClient     client.Client
	llmConfig     *LLMDecomposerConfig
}

// NewTaskAnalyzer creates a new task analyzer
func NewTaskAnalyzer(decomposeThreshold, parallelThreshold int) *TaskAnalyzer {
	return &TaskAnalyzer{
		decomposeThreshold:  decomposeThreshold,
		parallelThreshold:   parallelThreshold,
		questionPatterns:    compilePatterns(questionRegexPatterns),
		explorationPatterns: compilePatterns(explorationRegexPatterns),
		refactoringPatterns: compilePatterns(refactoringRegexPatterns),
		backgroundPatterns:  compilePatterns(backgroundRegexPatterns),
		multiToolPatterns:   compilePatterns(multiToolRegexPatterns),
		llmConfig:           DefaultLLMDecomposerConfig(),
	}
}

// SetLLMClient sets the LLM client for advanced decomposition.
func (ta *TaskAnalyzer) SetLLMClient(c client.Client) {
	ta.llmClient = c
}

// SetLLMConfig sets the LLM decomposer configuration.
func (ta *TaskAnalyzer) SetLLMConfig(config *LLMDecomposerConfig) {
	ta.llmConfig = config
}

// Analyze determines the complexity and optimal execution strategy for a task
func (ta *TaskAnalyzer) Analyze(message string) *TaskComplexity {
	message = strings.TrimSpace(message)
	if message == "" {
		return &TaskComplexity{
			Score:     0,
			Type:      TaskTypeQuestion,
			Strategy:  StrategyDirect,
			Reasoning: "Empty message",
		}
	}

	// Calculate base complexity score
	score := ta.calculateScore(message)

	// Determine task type
	taskType := ta.determineTaskType(message, score)

	// Determine strategy based on type and score
	strategy := ta.determineStrategy(taskType, score)

	reasoning := ta.generateReasoning(taskType, score, strategy)

	return &TaskComplexity{
		Score:     score,
		Type:      taskType,
		Strategy:  strategy,
		Reasoning: reasoning,
	}
}

// calculateScore computes the complexity score (1-10)
func (ta *TaskAnalyzer) calculateScore(message string) int {
	score := 1 // Base score

	// Length factor
	wordCount := countWords(message)
	switch {
	case wordCount > 100:
		score += 3
	case wordCount > 50:
		score += 2
	case wordCount > 20:
		score += 1
	}

	// Keyword indicators
	complexityKeywords := map[int][]string{
		3: {"анализируй", "проанализируй", "исследуй", "найди все", "объясни как работает", "how does", "analyze", "investigate"},
		2: {"создай", "добавь", "измени", "рефактори", "оптимизируй", "create", "implement", "refactor", "optimize"},
		1: {"покажи", "какой", "где", "what", "where", "show", "list", "find"},
	}

	lowerMessage := strings.ToLower(message)
	for points, keywords := range complexityKeywords {
		for _, keyword := range keywords {
			if strings.Contains(lowerMessage, keyword) {
				score += points
				break
			}
		}
	}

	// Multiple verbs/phrases indicate multi-step
	if hasMultipleInstructions(message) {
		score += 2
	}

	// Technical complexity indicators
	if strings.Contains(lowerMessage, "git") || strings.Contains(lowerMessage, "diff") ||
		strings.Contains(lowerMessage, "merge") || strings.Contains(lowerMessage, "branch") {
		score += 1
	}

	// Cap at 10
	if score > 10 {
		score = 10
	}

	return score
}

// determineTaskType identifies the type of task
func (ta *TaskAnalyzer) determineTaskType(message string, score int) TaskType {
	lowerMessage := strings.ToLower(message)

	// Check patterns in priority order

	// Background tasks (long-running)
	if ta.matchesAny(lowerMessage, ta.backgroundPatterns) {
		return TaskTypeBackground
	}

	// Refactoring tasks
	if ta.matchesAny(lowerMessage, ta.refactoringPatterns) {
		return TaskTypeRefactoring
	}

	// Exploration tasks
	if ta.matchesAny(lowerMessage, ta.explorationPatterns) {
		return TaskTypeExploration
	}

	// Multi-tool tasks
	if ta.matchesAny(lowerMessage, ta.multiToolPatterns) {
		return TaskTypeMultiTool
	}

	// Simple questions
	if ta.matchesAny(lowerMessage, ta.questionPatterns) || score <= 2 {
		return TaskTypeQuestion
	}

	// Default based on score
	if score >= 7 {
		return TaskTypeComplex
	}

	return TaskTypeSingleTool
}

// determineStrategy selects the optimal execution strategy
func (ta *TaskAnalyzer) determineStrategy(taskType TaskType, score int) ExecutionStrategy {
	switch taskType {
	case TaskTypeQuestion:
		if score <= 2 {
			return StrategyDirect // Simple questions - direct response
		}
		return StrategyExecutor // May need tools

	case TaskTypeSingleTool:
		return StrategyExecutor // Standard function calling

	case TaskTypeMultiTool:
		if score <= 4 {
			return StrategyExecutor // Sequential tools
		}
		return StrategySubAgent // Complex multi-step - use sub-agent

	case TaskTypeExploration:
		if score <= 5 {
			return StrategySubAgent // Single explore agent
		}
		return StrategySubAgent // Complex exploration - coordinator

	case TaskTypeRefactoring:
		if score <= 5 {
			return StrategyExecutor // Simple refactoring
		}
		return StrategySubAgent // Complex refactoring needs planning

	case TaskTypeBackground:
		return StrategySubAgent // Always use sub-agent with background flag

	case TaskTypeComplex:
		if score >= ta.parallelThreshold {
			return StrategySubAgent // High complexity - parallel agents
		}
		if score >= ta.decomposeThreshold {
			return StrategySubAgent // Medium complexity - sub-agent
		}
		return StrategyExecutor // Low complexity - standard executor
	}

	return StrategyExecutor
}

// generateReasoning creates a human-readable explanation for the decision
func (ta *TaskAnalyzer) generateReasoning(taskType TaskType, score int, strategy ExecutionStrategy) string {
	var reasoning string

	switch taskType {
	case TaskTypeQuestion:
		reasoning = "Simple question requiring direct answer"
	case TaskTypeSingleTool:
		reasoning = "Task can be completed with a single tool"
	case TaskTypeMultiTool:
		reasoning = "Requires multiple tools sequentially"
	case TaskTypeExploration:
		reasoning = "Code exploration requires analysis"
	case TaskTypeRefactoring:
		reasoning = "Code refactoring task"
	case TaskTypeBackground:
		reasoning = "Long-running task, better run in background"
	case TaskTypeComplex:
		reasoning = "Complex multi-step task"
	}

	reasoning += fmt.Sprintf(" (complexity: %d/10, strategy: %s)", score, strategy)

	return reasoning
}

// matchesAny checks if the message matches any of the given patterns
func (ta *TaskAnalyzer) matchesAny(message string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// Helper functions

func countWords(s string) int {
	count := 0
	inWord := false

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if !inWord {
				count++
				inWord = true
			}
		} else {
			inWord = false
		}
	}

	return count
}

func hasMultipleInstructions(message string) bool {
	// Count sentence delimiters
	delimiters := []string{". ", "! ", "? ", "\n", "; "}
	count := 0
	for _, delim := range delimiters {
		count += strings.Count(message, delim)
	}
	return count >= 2
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	var compiled []*regexp.Regexp
	for _, pattern := range patterns {
		regex, err := regexp.Compile("(?i)" + pattern) // Case-insensitive
		if err != nil {
			continue
		}
		compiled = append(compiled, regex)
	}
	return compiled
}

// Pattern definitions

var questionRegexPatterns = []string{
	`\?`,
	`^what\s`,
	`^how\s+(do|does|much|many|can)\b`,
	`^where\s`,
	`^why\s`,
	`^which\s`,
	`^who\s`,
	`^кто\b`,
	`^что\b`,
	`^где\b`,
	`^как\b`,
	`^почему\b`,
	`^зачем\b`,
	`explain`,
	`объясни`,
	`покажи\b`,
	`show\s+me`,
	`what'?s\s`,
	`whats\s`,
}

var explorationRegexPatterns = []string{
	`explore`,
	`исследуй\b`,
	`проанализируй\s+(код|проект|файл)`,
	`анализ\s+(кода|проекта|файла)`,
	`найди\s+(все\s+)?(файлы|использования|где\s+используется)`,
	`find\s+(all\s+)?(files|usages|where\s+used)`,
	`show\s+me\s+(the\s+)?(structure|architecture)`,
	`покажи\s+(структуру|архитектуру)`,
	`understand\s+(the\s+)?code`,
	`пойми\s+код`,
	`analyze\s+codebase`,
	`обзор\s+кода`,
	`code\s+overview`,
	`what\s+does\s+this\s+code\s+do`,
	`что\s+делает\s+этот\s+код`,
}

var refactoringRegexPatterns = []string{
	`refactor`,
	`рефактор`,
	`optimize`,
	`оптимизируй`,
	`rewrite`,
	`перепиши`,
	`clean\s+up`,
	`почисти`,
	`improve\s+(the\s+)?code`,
	`улучшись?\s+код`,
	`fix\s+(style|formatting)`,
	`исправь\s+(стиль|форматирование)`,
	`reorganize`,
	`реорганизуй`,
	`extract`,
	`extract\s+(function|method|class)`,
	`выделить\s+(функцию|метод|класс)`,
	`rename`,
	`переименуй`,
}

var backgroundRegexPatterns = []string{
	`run\s+in\s+background`,
	`background`,
	`асинхронно`,
	`запусти\s+в\s+фоне`,
	`long\s+running`,
	`долгая\s+задача`,
	`test\s+(everything|all)`,
	`протестируй\s+(все|весь)`,
	`benchmark`,
	`бенчмарк`,
	`compile\s+(everything|all)`,
	`скомпилируй\s+(все|весь)`,
}

var multiToolRegexPatterns = []string{
	`create\s+(new\s+)?(feature|function|class|file)`,
	`создай\s+(новую\s+)?(функцию|класс|файл|фичу)`,
	`implement`,
	`реализуй`,
	`add\s+(new\s+)?(feature|functionality)`,
	`добавь\s+(новую\s+)?(функцию|фичу|возможность)`,
	`build\s+(application|system|module)`,
	`построй\b`,
	`update\s+(multiple|several|all)\s+files`,
	`обнови\s+(несколько|все)\s+файлов`,
	`change\s+(\w+\s+)?and\s+(\w+\s+)?`,
	`измени\s+.*\s+и\s+`,
	`first\s+.*\s+then\s+`,
	`сначала\s+.*\s+потом\s+`,
}

// Subtask represents a decomposed part of a complex task.
type Subtask struct {
	ID           string
	Prompt       string
	AgentType    string
	Priority     int
	Dependencies []string
}

// DecompositionResult contains the result of task decomposition.
type DecompositionResult struct {
	Original     string
	Subtasks     []Subtask
	CanParallel  bool
	TotalSteps   int
	Reasoning    string
}

// Decompose breaks a complex task into subtasks.
func (ta *TaskAnalyzer) Decompose(message string) *DecompositionResult {
	return ta.DecomposeWithContext(context.Background(), message)
}

// DecomposeWithContext breaks a complex task into subtasks with context support.
func (ta *TaskAnalyzer) DecomposeWithContext(ctx context.Context, message string) *DecompositionResult {
	result := &DecompositionResult{
		Original:   message,
		Subtasks:   make([]Subtask, 0),
		CanParallel: false,
	}

	message = strings.TrimSpace(message)
	if message == "" {
		return result
	}

	// Try LLM-based decomposition first if available and enabled
	if ta.llmClient != nil && ta.llmConfig != nil && ta.llmConfig.Enabled {
		llmResult, err := ta.decomposeWithLLM(ctx, message)
		if err != nil {
			logging.Debug("LLM decomposition failed, falling back to regex",
				"error", err)
		} else if len(llmResult.Subtasks) > 0 {
			logging.Debug("LLM decomposition successful",
				"subtasks", len(llmResult.Subtasks),
				"can_parallel", llmResult.CanParallel)
			return llmResult
		}

		// Fall back to regex if LLM didn't produce valid results
		if !ta.llmConfig.FallbackToRegex {
			return result
		}
	}

	// Regex-based decomposition (fallback)
	return ta.decomposeWithRegex(message)
}

// decomposeWithLLM uses LLM to decompose a task.
func (ta *TaskAnalyzer) decomposeWithLLM(ctx context.Context, message string) (*DecompositionResult, error) {
	result := &DecompositionResult{
		Original: message,
		Subtasks: make([]Subtask, 0),
	}

	// Build prompt
	prompt := LLMDecomposerPrompt + message

	// Send to LLM
	stream, err := ta.llmClient.SendMessage(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("LLM request failed: %w", err)
	}

	resp, err := stream.Collect()
	if err != nil {
		return nil, fmt.Errorf("LLM response collection failed: %w", err)
	}

	// Parse JSON response
	responseText := strings.TrimSpace(resp.Text)

	// Try to extract JSON from the response (it might be wrapped in markdown)
	jsonStart := strings.Index(responseText, "{")
	jsonEnd := strings.LastIndex(responseText, "}")
	if jsonStart == -1 || jsonEnd == -1 || jsonEnd <= jsonStart {
		return nil, fmt.Errorf("no JSON found in LLM response")
	}
	jsonStr := responseText[jsonStart : jsonEnd+1]

	var llmResponse LLMDecompositionResponse
	if err := json.Unmarshal([]byte(jsonStr), &llmResponse); err != nil {
		return nil, fmt.Errorf("failed to parse LLM JSON response: %w", err)
	}

	// Validate and convert subtasks
	if len(llmResponse.Subtasks) == 0 {
		return nil, fmt.Errorf("LLM returned no subtasks")
	}

	if ta.llmConfig.MaxSubtasks > 0 && len(llmResponse.Subtasks) > ta.llmConfig.MaxSubtasks {
		llmResponse.Subtasks = llmResponse.Subtasks[:ta.llmConfig.MaxSubtasks]
	}

	for _, llmSt := range llmResponse.Subtasks {
		subtask := Subtask{
			ID:           llmSt.ID,
			Prompt:       llmSt.Prompt,
			AgentType:    llmSt.AgentType,
			Priority:     llmSt.Priority,
			Dependencies: llmSt.Dependencies,
		}

		// Validate agent type
		validTypes := map[string]bool{"explore": true, "bash": true, "general": true, "plan": true}
		if !validTypes[subtask.AgentType] {
			subtask.AgentType = "general"
		}

		result.Subtasks = append(result.Subtasks, subtask)
	}

	result.CanParallel = llmResponse.CanParallel
	result.TotalSteps = len(result.Subtasks)
	result.Reasoning = llmResponse.Reasoning

	return result, nil
}

// decomposeWithRegex uses regex patterns to decompose a task.
func (ta *TaskAnalyzer) decomposeWithRegex(message string) *DecompositionResult {
	result := &DecompositionResult{
		Original:   message,
		Subtasks:   make([]Subtask, 0),
		CanParallel: false,
	}

	// Pattern 1: "X and Y" pattern (can be parallel)
	if andParts := splitByAndPattern(message); len(andParts) > 1 {
		result.CanParallel = true
		result.Reasoning = "Tasks separated by 'and' can run in parallel"

		for i, part := range andParts {
			subtask := ta.createSubtask(fmt.Sprintf("sub_%d", i+1), part)
			result.Subtasks = append(result.Subtasks, subtask)
		}
		result.TotalSteps = len(result.Subtasks)
		return result
	}

	// Pattern 2: "first X, then Y" pattern (sequential)
	if thenParts := splitByThenPattern(message); len(thenParts) > 1 {
		result.CanParallel = false
		result.Reasoning = "Tasks with 'then'/'after' indicate sequence"

		var prevID string
		for i, part := range thenParts {
			subtask := ta.createSubtask(fmt.Sprintf("sub_%d", i+1), part)
			if prevID != "" {
				subtask.Dependencies = []string{prevID}
			}
			result.Subtasks = append(result.Subtasks, subtask)
			prevID = subtask.ID
		}
		result.TotalSteps = len(result.Subtasks)
		return result
	}

	// Pattern 3: Complex task decomposition based on task type
	analysis := ta.Analyze(message)
	if analysis.Score >= ta.decomposeThreshold {
		result.Subtasks = ta.decomposeByType(message, analysis)
		result.CanParallel = len(result.Subtasks) > 1 && !hasSequentialDependencies(result.Subtasks)
		result.TotalSteps = len(result.Subtasks)
		result.Reasoning = fmt.Sprintf("Complex task (score %d) decomposed into %d subtasks", analysis.Score, len(result.Subtasks))
	}

	return result
}

// createSubtask creates a subtask from a prompt.
func (ta *TaskAnalyzer) createSubtask(id string, prompt string) Subtask {
	analysis := ta.Analyze(prompt)
	agentType := ta.selectAgentType(analysis.Type)

	priority := 5
	if analysis.Score >= 7 {
		priority = 3 // Lower priority for complex subtasks
	}

	return Subtask{
		ID:        id,
		Prompt:    strings.TrimSpace(prompt),
		AgentType: agentType,
		Priority:  priority,
	}
}

// selectAgentType chooses the appropriate agent for a task type.
func (ta *TaskAnalyzer) selectAgentType(taskType TaskType) string {
	switch taskType {
	case TaskTypeExploration:
		return "explore"
	case TaskTypeBackground:
		return "bash"
	case TaskTypeRefactoring:
		return "general"
	case TaskTypeComplex:
		return "general"
	default:
		return "general"
	}
}

// decomposeByType creates subtasks based on task type.
func (ta *TaskAnalyzer) decomposeByType(message string, analysis *TaskComplexity) []Subtask {
	subtasks := make([]Subtask, 0)
	lowerMessage := strings.ToLower(message)

	switch analysis.Type {
	case TaskTypeRefactoring:
		// Refactoring: explore -> plan -> execute -> verify
		subtasks = append(subtasks, Subtask{
			ID:        "explore",
			Prompt:    "Explore the codebase to understand the current implementation related to: " + message,
			AgentType: "explore",
			Priority:  10,
		})
		subtasks = append(subtasks, Subtask{
			ID:           "plan",
			Prompt:       "Create a detailed plan for: " + message,
			AgentType:    "plan",
			Priority:     8,
			Dependencies: []string{"explore"},
		})
		subtasks = append(subtasks, Subtask{
			ID:           "execute",
			Prompt:       message,
			AgentType:    "general",
			Priority:     5,
			Dependencies: []string{"plan"},
		})

	case TaskTypeComplex:
		// Complex: explore context, then execute
		if strings.Contains(lowerMessage, "test") || strings.Contains(lowerMessage, "тест") {
			// If involves testing, add bash for test execution
			subtasks = append(subtasks, Subtask{
				ID:        "explore",
				Prompt:    "Explore the codebase to understand what needs to be done for: " + message,
				AgentType: "explore",
				Priority:  10,
			})
			subtasks = append(subtasks, Subtask{
				ID:           "implement",
				Prompt:       message,
				AgentType:    "general",
				Priority:     7,
				Dependencies: []string{"explore"},
			})
			subtasks = append(subtasks, Subtask{
				ID:           "test",
				Prompt:       "Run tests to verify the changes",
				AgentType:    "bash",
				Priority:     5,
				Dependencies: []string{"implement"},
			})
		} else {
			subtasks = append(subtasks, Subtask{
				ID:        "explore",
				Prompt:    "Explore relevant code for: " + message,
				AgentType: "explore",
				Priority:  10,
			})
			subtasks = append(subtasks, Subtask{
				ID:           "execute",
				Prompt:       message,
				AgentType:    "general",
				Priority:     5,
				Dependencies: []string{"explore"},
			})
		}

	case TaskTypeMultiTool:
		// Multi-tool: may have parallel opportunities
		if strings.Contains(lowerMessage, "create") || strings.Contains(lowerMessage, "создай") {
			subtasks = append(subtasks, Subtask{
				ID:        "explore",
				Prompt:    "Find similar patterns and understand the codebase structure for: " + message,
				AgentType: "explore",
				Priority:  10,
			})
			subtasks = append(subtasks, Subtask{
				ID:           "create",
				Prompt:       message,
				AgentType:    "general",
				Priority:     5,
				Dependencies: []string{"explore"},
			})
		} else {
			// Default multi-tool decomposition
			subtasks = append(subtasks, Subtask{
				ID:        "main",
				Prompt:    message,
				AgentType: "general",
				Priority:  5,
			})
		}

	default:
		// Single subtask
		subtasks = append(subtasks, Subtask{
			ID:        "main",
			Prompt:    message,
			AgentType: ta.selectAgentType(analysis.Type),
			Priority:  5,
		})
	}

	return subtasks
}

// splitByAndPattern splits a message by "and" conjunctions.
func splitByAndPattern(message string) []string {
	// Match patterns like "X and Y", "X, and Y", "X и Y"
	patterns := []string{
		`\s+and\s+`,
		`\s+и\s+`,
		`,\s*and\s+`,
		`,\s*и\s+`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile("(?i)" + pattern)
		parts := re.Split(message, -1)
		if len(parts) > 1 {
			// Validate each part is substantial
			validParts := make([]string, 0)
			for _, part := range parts {
				trimmed := strings.TrimSpace(part)
				if len(trimmed) > 5 { // Minimum meaningful length
					validParts = append(validParts, trimmed)
				}
			}
			if len(validParts) > 1 {
				return validParts
			}
		}
	}

	return nil
}

// splitByThenPattern splits a message by sequential indicators.
func splitByThenPattern(message string) []string {
	// Match patterns like "first X, then Y", "X, after that Y"
	patterns := []string{
		`(?i)first\s+(.+?)\s*,?\s*then\s+(.+)`,
		`(?i)сначала\s+(.+?)\s*,?\s*потом\s+(.+)`,
		`(?i)(.+?)\s*,?\s*then\s+(.+)`,
		`(?i)(.+?)\s*,?\s*после\s+(.+)`,
		`(?i)(.+?)\s*,?\s*after\s+that\s+(.+)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(message)
		if len(matches) >= 3 {
			parts := make([]string, 0, len(matches)-1)
			for _, m := range matches[1:] {
				trimmed := strings.TrimSpace(m)
				if trimmed != "" {
					parts = append(parts, trimmed)
				}
			}
			if len(parts) > 1 {
				return parts
			}
		}
	}

	return nil
}

// hasSequentialDependencies checks if subtasks have dependencies.
func hasSequentialDependencies(subtasks []Subtask) bool {
	for _, st := range subtasks {
		if len(st.Dependencies) > 0 {
			return true
		}
	}
	return false
}

// ========== LLM-based Decomposition (Phase 2) ==========

// LLMDecomposer provides LLM-based task decomposition.
type LLMDecomposer interface {
	// DecomposeTask breaks down a complex task into subtasks using LLM.
	DecomposeTask(ctx context.Context, message string) (*DecompositionResult, error)
}

// LLMDecomposerConfig holds configuration for the LLM decomposer.
type LLMDecomposerConfig struct {
	Enabled          bool
	Model            string
	MaxSubtasks      int
	FallbackToRegex  bool
}

// DefaultLLMDecomposerConfig returns the default configuration.
func DefaultLLMDecomposerConfig() *LLMDecomposerConfig {
	return &LLMDecomposerConfig{
		Enabled:         true,
		Model:           "",  // Use default model
		MaxSubtasks:     10,
		FallbackToRegex: true,
	}
}

// LLMDecomposerPrompt is the system prompt for task decomposition.
const LLMDecomposerPrompt = `You are a task decomposition assistant. Given a complex task, break it down into smaller, manageable subtasks.

Respond ONLY with a JSON object in this exact format:
{
  "subtasks": [
    {
      "id": "step_1",
      "prompt": "Description of what to do",
      "agent_type": "explore|bash|general|plan",
      "priority": 1-10,
      "dependencies": []
    }
  ],
  "can_parallel": true|false,
  "reasoning": "Brief explanation of the decomposition"
}

Rules:
1. Each subtask should be atomic and independently executable
2. Use appropriate agent types:
   - "explore" for code exploration and analysis
   - "bash" for command execution
   - "general" for code modifications
   - "plan" for creating plans
3. Set dependencies as array of subtask IDs that must complete first
4. Set can_parallel to true if independent subtasks can run simultaneously
5. Priority 1-10 where 10 is highest priority
6. Maximum 10 subtasks

Task to decompose:
`

// LLMDecompositionResponse represents the expected JSON response from LLM.
type LLMDecompositionResponse struct {
	Subtasks    []LLMSubtask `json:"subtasks"`
	CanParallel bool         `json:"can_parallel"`
	Reasoning   string       `json:"reasoning"`
}

// LLMSubtask represents a subtask in the LLM response.
type LLMSubtask struct {
	ID           string   `json:"id"`
	Prompt       string   `json:"prompt"`
	AgentType    string   `json:"agent_type"`
	Priority     int      `json:"priority"`
	Dependencies []string `json:"dependencies"`
}
