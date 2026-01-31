package tools

import (
	"context"
	"fmt"

	"gooner/internal/tasks"

	"google.golang.org/genai"
)

// TaskStopTool stops running background tasks (both shell and agent tasks).
type TaskStopTool struct {
	manager *tasks.Manager
	runner  AgentRunner
}

// NewTaskStopTool creates a new task stop tool.
func NewTaskStopTool() *TaskStopTool {
	return &TaskStopTool{}
}

// SetManager sets the task manager for shell tasks.
func (t *TaskStopTool) SetManager(manager *tasks.Manager) {
	t.manager = manager
}

// SetRunner sets the agent runner for agent tasks.
func (t *TaskStopTool) SetRunner(runner AgentRunner) {
	t.runner = runner
}

func (t *TaskStopTool) Name() string {
	return "task_stop"
}

func (t *TaskStopTool) Description() string {
	return "Stops a running background task by its ID. Works with both shell tasks and agent tasks."
}

func (t *TaskStopTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"task_id": {
					Type:        genai.TypeString,
					Description: "ID of the task to stop. Use task_output with action='list' to see available task IDs.",
				},
				"reason": {
					Type:        genai.TypeString,
					Description: "Optional reason for stopping the task (for logging purposes).",
				},
			},
			Required: []string{"task_id"},
		},
	}
}

func (t *TaskStopTool) Validate(args map[string]any) error {
	taskID, ok := GetString(args, "task_id")
	if !ok || taskID == "" {
		return NewValidationError("task_id", "is required")
	}
	return nil
}

func (t *TaskStopTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	taskID, _ := GetString(args, "task_id")
	reason := GetStringDefault(args, "reason", "")

	// Check if this is an agent task
	if isAgentTaskID(taskID) {
		return t.stopAgent(taskID, reason)
	}

	// Fall back to shell task
	return t.stopShellTask(taskID, reason)
}

// stopAgent stops a running agent
func (t *TaskStopTool) stopAgent(agentID, reason string) (ToolResult, error) {
	if t.runner == nil {
		return NewErrorResult("agent runner not configured"), nil
	}

	// Check if the runner supports cancellation
	canceller, ok := t.runner.(AgentCanceller)
	if !ok {
		return NewErrorResult("agent cancellation not supported"), nil
	}

	// Try to cancel the agent
	if err := canceller.Cancel(agentID); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to stop agent: %s", err)), nil
	}

	result := fmt.Sprintf("Agent %s stopped", agentID)
	if reason != "" {
		result += fmt.Sprintf(" (reason: %s)", reason)
	}

	return NewSuccessResultWithData(result, map[string]any{
		"task_id": agentID,
		"type":    "agent",
		"stopped": true,
		"reason":  reason,
	}), nil
}

// stopShellTask stops a running shell task
func (t *TaskStopTool) stopShellTask(taskID, reason string) (ToolResult, error) {
	if t.manager == nil {
		return NewErrorResult("task manager not configured"), nil
	}

	// Try to cancel the task
	if err := t.manager.Cancel(taskID); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to stop task: %s", err)), nil
	}

	result := fmt.Sprintf("Task %s stopped", taskID)
	if reason != "" {
		result += fmt.Sprintf(" (reason: %s)", reason)
	}

	return NewSuccessResultWithData(result, map[string]any{
		"task_id": taskID,
		"type":    "shell",
		"stopped": true,
		"reason":  reason,
	}), nil
}
