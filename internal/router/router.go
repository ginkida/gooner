package router

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gooner/internal/agent"
	"gooner/internal/client"
	"gooner/internal/logging"
	"gooner/internal/tools"

	"google.golang.org/genai"
)

// Router determines the optimal execution strategy for incoming tasks
// and routes them to the appropriate handler (direct, executor, or sub-agent).
type Router struct {
	analyzer    *TaskAnalyzer
	executor    *tools.Executor
	agentRunner AgentRunner
	client      client.Client
	workDir     string

	// Configuration
	enabled            bool
	decomposeThreshold int
	parallelThreshold  int
}

// AgentRunner interface for spawning agents (implemented by agent.Runner)
type AgentRunner interface {
	Spawn(ctx context.Context, agentType string, prompt string, maxTurns int, model string) (string, error)
	SpawnAsync(ctx context.Context, agentType string, prompt string, maxTurns int, model string) string
	GetResult(agentID string) (*agent.AgentResult, bool)
}

// RouterConfig holds configuration for the router
type RouterConfig struct {
	Enabled            bool
	DecomposeThreshold int // Default: 4
	ParallelThreshold  int // Default: 7
}

// NewRouter creates a new task router
func NewRouter(cfg *RouterConfig, executor *tools.Executor, agentRunner AgentRunner, client client.Client, workDir string) *Router {
	if cfg == nil {
		cfg = &RouterConfig{
			Enabled:            true,
			DecomposeThreshold: 4,
			ParallelThreshold:  7,
		}
	}

	return &Router{
		analyzer:           NewTaskAnalyzer(cfg.DecomposeThreshold, cfg.ParallelThreshold),
		executor:           executor,
		agentRunner:        agentRunner,
		client:             client,
		workDir:            workDir,
		enabled:            cfg.Enabled,
		decomposeThreshold: cfg.DecomposeThreshold,
		parallelThreshold:  cfg.ParallelThreshold,
	}
}

// Route determines the best execution strategy and returns a routing decision
func (r *Router) Route(message string) *RoutingDecision {
	analysis := r.analyzer.Analyze(message)

	logging.Debug("task routed",
		"message", message,
		"complexity", analysis.Score,
		"type", analysis.Type,
		"strategy", analysis.Strategy,
		"reasoning", analysis.Reasoning)

	decision := &RoutingDecision{
		Analysis:    analysis,
		Message:     message,
		ShouldRoute: true,
	}

	// Check for automatic decomposition for high-complexity tasks
	if analysis.Score >= r.decomposeThreshold {
		decomposition := r.analyzer.Decompose(message)
		if len(decomposition.Subtasks) > 1 {
			decision.Handler = HandlerCoordinated
			decision.Decomposition = decomposition
			decision.Reasoning = fmt.Sprintf("Auto-decomposition: %d subtasks (%s)",
				len(decomposition.Subtasks), decomposition.Reasoning)

			logging.Info("task decomposed",
				"message", message,
				"subtasks", len(decomposition.Subtasks),
				"can_parallel", decomposition.CanParallel)

			return decision
		}
	}

	// Determine which handler to use
	switch analysis.Strategy {
	case StrategyDirect:
		decision.Handler = HandlerDirect
		decision.Reasoning = "Direct AI response without tools"

	case StrategySingleTool:
		decision.Handler = HandlerExecutor
		decision.Reasoning = "Expecting a single tool call"

	case StrategyExecutor:
		decision.Handler = HandlerExecutor
		decision.Reasoning = "Standard execution via function calling loop"

	case StrategySubAgent:
		decision.Handler = HandlerSubAgent
		decision.SubAgentType = r.selectSubAgentType(analysis.Type)
		decision.Background = analysis.Type == TaskTypeBackground
		decision.Reasoning = fmt.Sprintf("Using sub-agent type '%s'", decision.SubAgentType)

	default:
		decision.Handler = HandlerExecutor
		decision.Reasoning = "Standard strategy"
	}

	return decision
}

// Execute routes the task to the appropriate handler and returns the result
func (r *Router) Execute(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	decision := r.Route(message)

	switch decision.Handler {
	case HandlerDirect:
		// Direct AI response without tools
		return r.executeDirect(ctx, history, message)

	case HandlerExecutor:
		// Standard function calling loop
		return r.executor.Execute(ctx, history, message)

	case HandlerSubAgent:
		// Spawn a sub-agent
		return r.executeViaSubAgent(ctx, message, decision.SubAgentType, decision.Background)

	case HandlerCoordinated:
		// Execute via coordinator with decomposed subtasks
		return r.executeCoordinated(ctx, decision.Decomposition)

	default:
		return r.executor.Execute(ctx, history, message)
	}
}

// executeDirect gets a direct AI response without tool usage
func (r *Router) executeDirect(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	// Add user message to history
	userContent := genai.NewContentFromText(message, genai.RoleUser)
	history = append(history, userContent)

	// For direct responses, we can use a simplified approach:
	// Temporarily disable tools to force direct response
	// Or use a system prompt that discourages tool usage

	// For now, fall back to executor (it will handle this correctly)
	return r.executor.Execute(ctx, history, message)
}

// executeViaSubAgent spawns a sub-agent to handle the task
func (r *Router) executeViaSubAgent(ctx context.Context, message string, agentType string, background bool) ([]*genai.Content, string, error) {
	if r.agentRunner == nil {
		return nil, "", fmt.Errorf("sub-agent requested but agent runner is not configured")
	}

	logging.Info("spawning sub-agent",
		"type", agentType,
		"background", background,
		"message", message)

	var agentID string
	var err error

	if background {
		// Spawn in background, return immediately
		agentID = r.agentRunner.SpawnAsync(ctx, agentType, message, 30, "")
		backgroundMsg := fmt.Sprintf("Background agent %s started for the task. ID: %s\nUse /task_output %s to check status.", agentType, agentID, agentID)
		return nil, backgroundMsg, nil
	}

	// Spawn and wait for completion
	agentID, err = r.agentRunner.Spawn(ctx, agentType, message, 30, "")
	if err != nil {
		return nil, "", fmt.Errorf("failed to spawn sub-agent: %w", err)
	}

	// Get result
	result, ok := r.agentRunner.GetResult(agentID)
	if !ok {
		return nil, "", fmt.Errorf("sub-agent %s did not return a result", agentID)
	}

	if result.Error != "" {
		return nil, "", fmt.Errorf("sub-agent failed: %s", result.Error)
	}

	// Format output
	var response string
	if result.Output != "" {
		response = result.Output
	} else {
		response = fmt.Sprintf("Agent %s completed the task", agentType)
	}

	// Return minimal history (just the result)
	history := []*genai.Content{
		genai.NewContentFromText(response, genai.RoleModel),
	}

	return history, response, nil
}

// selectSubAgentType chooses the appropriate sub-agent type based on task type
func (r *Router) selectSubAgentType(taskType TaskType) string {
	switch taskType {
	case TaskTypeExploration:
		return "explore"
	case TaskTypeBackground:
		return "bash"
	case TaskTypeRefactoring:
		return "general" // Refactoring needs write access
	case TaskTypeComplex:
		return "general"
	case TaskTypeMultiTool:
		return "general"
	default:
		return "general"
	}
}

// SubtaskResult holds the result of a subtask execution.
type SubtaskResult struct {
	ID      string
	AgentID string
	Output  string
	Error   string
	Success bool
}

// maxParallelSubtasks is the maximum number of subtasks to execute in parallel.
const maxParallelSubtasks = 5

// executeCoordinated executes decomposed subtasks via coordinator
func (r *Router) executeCoordinated(ctx context.Context, decomposition *DecompositionResult) ([]*genai.Content, string, error) {
	if r.agentRunner == nil {
		return nil, "", fmt.Errorf("agent runner not configured for coordination")
	}

	logging.Info("executing coordinated task",
		"subtasks", len(decomposition.Subtasks),
		"can_parallel", decomposition.CanParallel)

	var allOutputs strings.Builder
	allOutputs.WriteString(fmt.Sprintf("## Coordinated Execution\n\n"))
	allOutputs.WriteString(fmt.Sprintf("Task decomposed into %d subtasks.\n\n", len(decomposition.Subtasks)))

	if decomposition.CanParallel {
		allOutputs.WriteString("**Mode:** Parallel execution\n\n")
	} else {
		allOutputs.WriteString("**Mode:** Sequential execution\n\n")
	}

	// Track completed tasks and their results
	completed := make(map[string]bool)
	subtaskResults := make(map[string]*SubtaskResult)
	var resultsMu sync.Mutex

	for {
		// Find ready tasks (dependencies met)
		var ready []Subtask
		for _, st := range decomposition.Subtasks {
			if completed[st.ID] {
				continue
			}

			// Check dependencies
			depsOK := true
			for _, dep := range st.Dependencies {
				if !completed[dep] {
					depsOK = false
					break
				}
			}

			if depsOK {
				ready = append(ready, st)
			}
		}

		if len(ready) == 0 {
			break // All done or blocked
		}

		// Execute ready tasks - parallel if allowed, sequential otherwise
		if decomposition.CanParallel && len(ready) > 1 {
			// Parallel execution with semaphore
			var wg sync.WaitGroup
			semaphore := make(chan struct{}, maxParallelSubtasks)

			for _, st := range ready {
				wg.Add(1)
				semaphore <- struct{}{} // Acquire semaphore

				go func(subtask Subtask) {
					defer wg.Done()
					defer func() { <-semaphore }() // Release semaphore

					result := r.executeSubtask(ctx, subtask)

					resultsMu.Lock()
					subtaskResults[subtask.ID] = result
					completed[subtask.ID] = true
					resultsMu.Unlock()
				}(st)
			}

			wg.Wait()
		} else {
			// Sequential execution
			for _, st := range ready {
				result := r.executeSubtask(ctx, st)
				subtaskResults[st.ID] = result
				completed[st.ID] = true
			}
		}
	}

	// Write results in order
	successCount := 0
	failedCount := 0

	for _, st := range decomposition.Subtasks {
		result, ok := subtaskResults[st.ID]
		if !ok {
			continue
		}

		allOutputs.WriteString(fmt.Sprintf("### Subtask: %s (%s)\n", st.ID, st.AgentType))
		allOutputs.WriteString(fmt.Sprintf("Prompt: %s\n\n", st.Prompt))

		if result.Success {
			successCount++
			output := result.Output
			if len(output) > 1000 {
				output = output[:1000] + "...[truncated]"
			}
			allOutputs.WriteString("**Status:** Completed\n\n")
			allOutputs.WriteString(fmt.Sprintf("**Result:**\n%s\n\n", output))
		} else {
			failedCount++
			allOutputs.WriteString(fmt.Sprintf("**Status:** Error - %s\n\n", result.Error))
		}
	}

	// Summary
	allOutputs.WriteString("---\n")
	allOutputs.WriteString(fmt.Sprintf("**Total:** %d succeeded, %d failed out of %d subtasks\n",
		successCount, failedCount, len(decomposition.Subtasks)))

	response := allOutputs.String()
	history := []*genai.Content{
		genai.NewContentFromText(response, genai.RoleModel),
	}

	return history, response, nil
}

// executeSubtask executes a single subtask and returns the result.
func (r *Router) executeSubtask(ctx context.Context, st Subtask) *SubtaskResult {
	result := &SubtaskResult{
		ID: st.ID,
	}

	agentID, err := r.agentRunner.Spawn(ctx, st.AgentType, st.Prompt, 20, "")
	if err != nil {
		result.Error = err.Error()
		result.Success = false
		return result
	}

	result.AgentID = agentID

	agentResult, ok := r.agentRunner.GetResult(agentID)
	if !ok {
		result.Error = "no result returned"
		result.Success = false
		return result
	}

	if agentResult.Error != "" {
		result.Error = agentResult.Error
		result.Success = false
		return result
	}

	result.Output = agentResult.Output
	result.Success = true
	return result
}

// GetAnalysis returns the task analysis without executing
func (r *Router) GetAnalysis(message string) *TaskComplexity {
	return r.analyzer.Analyze(message)
}

// RoutingDecision represents the routing decision for a task
type RoutingDecision struct {
	Analysis        *TaskComplexity
	Message         string
	Handler         HandlerType
	SubAgentType    string
	Background      bool
	ShouldRoute     bool
	Reasoning       string
	Decomposition   *DecompositionResult // For HandlerCoordinated
	LearnedExamples []LearnedExample     // Similar past tasks (Phase 2)
}

// LearnedExample contains information about a learned example for few-shot learning.
type LearnedExample struct {
	ID        string
	TaskType  string
	Prompt    string
	AgentType string
	Score     float64
}

// HandlerType represents the execution handler
type HandlerType string

const (
	HandlerDirect      HandlerType = "direct"
	HandlerExecutor    HandlerType = "executor"
	HandlerSubAgent    HandlerType = "sub_agent"
	HandlerCoordinated HandlerType = "coordinated"
)

// String returns the string representation
func (h HandlerType) String() string {
	return string(h)
}
