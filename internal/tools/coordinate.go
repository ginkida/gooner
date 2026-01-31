package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"
)

// CoordinatedTaskDef defines a task for coordination.
type CoordinatedTaskDef struct {
	Prompt       string   `json:"prompt"`
	AgentType    string   `json:"agent_type"`
	Priority     int      `json:"priority"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// CoordinateCallback is called when coordination events occur.
type CoordinateCallback interface {
	OnTaskStart(taskID string, agentType string, prompt string)
	OnTaskComplete(taskID string, success bool, output string)
	OnTaskProgress(taskID string, progress float64, currentStep string) // Phase 2: Progress updates
	OnTaskText(taskID string, text string)                              // Phase 2: Streaming text
	OnAllComplete(results map[string]string)
}

// CoordinateTool manages parallel agent execution with dependencies.
type CoordinateTool struct {
	coordinatorFactory func() any // Returns *agent.Coordinator
	callback           CoordinateCallback
}

// NewCoordinateTool creates a new coordinate tool.
func NewCoordinateTool() *CoordinateTool {
	return &CoordinateTool{}
}

// SetCoordinatorFactory sets the factory function for creating coordinators.
func (t *CoordinateTool) SetCoordinatorFactory(factory func() any) {
	t.coordinatorFactory = factory
}

// SetCallback sets the callback for coordination events.
func (t *CoordinateTool) SetCallback(cb CoordinateCallback) {
	t.callback = cb
}

func (t *CoordinateTool) Name() string {
	return "coordinate"
}

func (t *CoordinateTool) Description() string {
	return `Coordinates multiple agents to work in parallel on related tasks. Use this when you need to:
1. Run multiple independent tasks in parallel (e.g., explore code AND run tests)
2. Run tasks with dependencies (e.g., explore first, THEN refactor)
3. Split a complex task into subtasks with proper orchestration

Each task can specify an agent type (explore, bash, general, plan) and dependencies on other tasks.
Tasks without dependencies run in parallel. Tasks with dependencies wait for their prerequisites.`
}

func (t *CoordinateTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"tasks": {
					Type:        genai.TypeArray,
					Description: "List of tasks to coordinate",
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"id": {
								Type:        genai.TypeString,
								Description: "Unique identifier for this task (used for dependencies)",
							},
							"prompt": {
								Type:        genai.TypeString,
								Description: "The task description/prompt for the agent",
							},
							"agent_type": {
								Type:        genai.TypeString,
								Description: "Type of agent: 'explore', 'bash', 'general', or 'plan'",
								Enum:        []string{"explore", "bash", "general", "plan"},
							},
							"priority": {
								Type:        genai.TypeInteger,
								Description: "Priority (1-10, higher runs first). Default: 5",
							},
							"depends_on": {
								Type:        genai.TypeArray,
								Description: "Task IDs that must complete before this task starts",
								Items: &genai.Schema{
									Type: genai.TypeString,
								},
							},
						},
						Required: []string{"id", "prompt", "agent_type"},
					},
				},
				"max_parallel": {
					Type:        genai.TypeInteger,
					Description: "Maximum number of agents to run in parallel. Default: 3",
				},
				"timeout_minutes": {
					Type:        genai.TypeInteger,
					Description: "Maximum time to wait for all tasks (in minutes). Default: 10",
				},
			},
			Required: []string{"tasks"},
		},
	}
}

func (t *CoordinateTool) Validate(args map[string]any) error {
	tasks, ok := args["tasks"].([]any)
	if !ok || len(tasks) == 0 {
		return NewValidationError("tasks", "must be a non-empty array")
	}

	// Validate each task
	ids := make(map[string]bool)
	for i, taskAny := range tasks {
		task, ok := taskAny.(map[string]any)
		if !ok {
			return NewValidationError("tasks", fmt.Sprintf("task %d must be an object", i))
		}

		id, _ := task["id"].(string)
		if id == "" {
			return NewValidationError("tasks", fmt.Sprintf("task %d must have an id", i))
		}
		if ids[id] {
			return NewValidationError("tasks", fmt.Sprintf("duplicate task id: %s", id))
		}
		ids[id] = true

		prompt, _ := task["prompt"].(string)
		if prompt == "" {
			return NewValidationError("tasks", fmt.Sprintf("task %s must have a prompt", id))
		}

		agentType, _ := task["agent_type"].(string)
		if agentType == "" {
			return NewValidationError("tasks", fmt.Sprintf("task %s must have an agent_type", id))
		}
	}

	// Validate dependencies exist
	for _, taskAny := range tasks {
		task := taskAny.(map[string]any)
		id := task["id"].(string)
		if deps, ok := task["depends_on"].([]any); ok {
			for _, depAny := range deps {
				dep, _ := depAny.(string)
				if !ids[dep] {
					return NewValidationError("tasks", fmt.Sprintf("task %s depends on unknown task: %s", id, dep))
				}
				if dep == id {
					return NewValidationError("tasks", fmt.Sprintf("task %s cannot depend on itself", id))
				}
			}
		}
	}

	return nil
}

func (t *CoordinateTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.coordinatorFactory == nil {
		return NewErrorResult("coordinator not configured"), nil
	}

	// Parse arguments
	tasksAny := args["tasks"].([]any)
	_ = 3 // maxParallel default (used by coordinator)
	if _, ok := args["max_parallel"].(float64); ok {
		// maxParallel configured via coordinator factory
	}
	timeoutMinutes := 10
	if tm, ok := args["timeout_minutes"].(float64); ok {
		timeoutMinutes = int(tm)
	}

	// Create coordinator via factory
	coordAny := t.coordinatorFactory()
	if coordAny == nil {
		return NewErrorResult("failed to create coordinator"), nil
	}

	// We use interface methods to avoid import cycle
	type coordinatorInterface interface {
		AddTask(prompt string, agentType any, priority any, deps []string) string
		Start()
		WaitWithTimeout(timeout time.Duration) (map[string]any, error)
		GetStatus() any
	}

	coord, ok := coordAny.(coordinatorInterface)
	if !ok {
		// Fall back to simplified execution
		return t.executeSimple(ctx, tasksAny)
	}

	// Build task ID mapping (user IDs -> internal IDs)
	taskIDMap := make(map[string]string)

	// Add tasks to coordinator
	for _, taskAny := range tasksAny {
		task := taskAny.(map[string]any)
		userID := task["id"].(string)
		prompt := task["prompt"].(string)
		agentType := task["agent_type"].(string)

		priority := 5
		if p, ok := task["priority"].(float64); ok {
			priority = int(p)
		}

		// Map dependencies to internal IDs
		var deps []string
		if depsAny, ok := task["depends_on"].([]any); ok {
			for _, depAny := range depsAny {
				depUserID := depAny.(string)
				if internalID, exists := taskIDMap[depUserID]; exists {
					deps = append(deps, internalID)
				}
			}
		}

		internalID := coord.AddTask(prompt, agentType, priority, deps)
		taskIDMap[userID] = internalID
	}

	// Start coordination
	coord.Start()

	// Wait for completion
	timeout := time.Duration(timeoutMinutes) * time.Minute
	results, err := coord.WaitWithTimeout(timeout)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("coordination failed: %v", err)), nil
	}

	// Build result summary
	var sb strings.Builder
	sb.WriteString("## Coordination Complete\n\n")

	// Reverse map internal IDs to user IDs
	reverseMap := make(map[string]string)
	for userID, internalID := range taskIDMap {
		reverseMap[internalID] = userID
	}

	succeeded := 0
	failed := 0

	for internalID, resultAny := range results {
		userID := reverseMap[internalID]
		if userID == "" {
			userID = internalID
		}

		sb.WriteString(fmt.Sprintf("### Task: %s\n", userID))

		if resultAny == nil {
			sb.WriteString("Status: No result\n\n")
			failed++
			continue
		}

		// Extract result fields via type assertion or JSON
		resultJSON, _ := json.Marshal(resultAny)
		var result struct {
			Status string `json:"status"`
			Output string `json:"output"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(resultJSON, &result)

		if result.Status == "completed" || result.Error == "" {
			sb.WriteString("Status: **Completed**\n")
			succeeded++
		} else {
			sb.WriteString(fmt.Sprintf("Status: **Failed** - %s\n", result.Error))
			failed++
		}

		if result.Output != "" {
			// Truncate long outputs
			output := result.Output
			if len(output) > 500 {
				output = output[:500] + "...[truncated]"
			}
			sb.WriteString(fmt.Sprintf("Output:\n```\n%s\n```\n", output))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("---\n**Summary:** %d succeeded, %d failed out of %d tasks\n",
		succeeded, failed, len(tasksAny)))

	return NewSuccessResult(sb.String()), nil
}

// executeSimple is a fallback when coordinator interface isn't available.
func (t *CoordinateTool) executeSimple(ctx context.Context, tasksAny []any) (ToolResult, error) {
	var sb strings.Builder
	sb.WriteString("## Task Plan\n\n")
	sb.WriteString("Coordinator not available. Tasks to execute:\n\n")

	for i, taskAny := range tasksAny {
		task := taskAny.(map[string]any)
		sb.WriteString(fmt.Sprintf("%d. **%s** (%s)\n", i+1, task["id"], task["agent_type"]))
		sb.WriteString(fmt.Sprintf("   Prompt: %s\n", task["prompt"]))
		if deps, ok := task["depends_on"].([]any); ok && len(deps) > 0 {
			sb.WriteString(fmt.Sprintf("   Depends on: %v\n", deps))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Use the `task` tool to run these tasks individually.\n")

	return NewSuccessResult(sb.String()), nil
}
