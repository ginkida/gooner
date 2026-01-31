package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gokin/internal/tasks"

	"google.golang.org/genai"
)

// TaskOutputTool retrieves output from background tasks (both shell and agent tasks).
type TaskOutputTool struct {
	manager *tasks.Manager
	runner  AgentRunner // For agent tasks
}

// NewTaskOutputTool creates a new task output tool.
func NewTaskOutputTool() *TaskOutputTool {
	return &TaskOutputTool{}
}

// SetManager sets the task manager for shell tasks.
func (t *TaskOutputTool) SetManager(manager *tasks.Manager) {
	t.manager = manager
}

// SetRunner sets the agent runner for agent tasks.
func (t *TaskOutputTool) SetRunner(runner AgentRunner) {
	t.runner = runner
}

func (t *TaskOutputTool) Name() string {
	return "task_output"
}

func (t *TaskOutputTool) Description() string {
	return "Get output from a background task or list all tasks"
}

func (t *TaskOutputTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"task_id": {
					Type:        genai.TypeString,
					Description: "ID of the task to get output from. If not provided, lists all tasks. Supports both shell task IDs and agent IDs (prefixed with 'agent_').",
				},
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: 'get' (default), 'list', 'cancel'",
					Enum:        []string{"get", "list", "cancel"},
				},
				"block": {
					Type:        genai.TypeBoolean,
					Description: "If true, wait for task completion before returning. Default: false",
				},
				"timeout_ms": {
					Type:        genai.TypeInteger,
					Description: "Timeout in milliseconds when blocking. Default: 60000 (1 minute). Max: 600000 (10 minutes).",
				},
			},
		},
	}
}

func (t *TaskOutputTool) Validate(args map[string]any) error {
	action := GetStringDefault(args, "action", "get")

	if action == "get" || action == "cancel" {
		if _, ok := GetString(args, "task_id"); !ok {
			return NewValidationError("task_id", "task_id is required for this action")
		}
	}

	return nil
}

func (t *TaskOutputTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	action := GetStringDefault(args, "action", "get")
	taskID, _ := GetString(args, "task_id")
	block := GetBoolDefault(args, "block", false)
	timeoutMs := GetIntDefault(args, "timeout_ms", 60000)

	// Clamp timeout to reasonable range
	if timeoutMs < 100 {
		timeoutMs = 100
	}
	if timeoutMs > 600000 {
		timeoutMs = 600000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	switch action {
	case "list":
		return t.listTasks()
	case "cancel":
		return t.cancelTask(ctx, taskID)
	default:
		return t.getTaskOutput(ctx, taskID, block, timeout)
	}
}

func (t *TaskOutputTool) getTaskOutput(ctx context.Context, taskID string, block bool, timeout time.Duration) (ToolResult, error) {
	// Check if this is an agent task (agent IDs typically contain "agent" prefix or UUID format)
	if t.runner != nil && isAgentTaskID(taskID) {
		return t.getAgentOutput(ctx, taskID, block, timeout)
	}

	// Fall back to shell task manager
	if t.manager == nil {
		return NewErrorResult("task manager not configured"), nil
	}

	// If blocking, wait for completion
	if block {
		return t.waitForShellTask(ctx, taskID, timeout)
	}

	info, ok := t.manager.GetInfo(taskID)
	if !ok {
		return NewErrorResult(fmt.Sprintf("task not found: %s", taskID)), nil
	}

	return t.formatShellTaskResult(info), nil
}

// isAgentTaskID checks if the task ID is for an agent task
func isAgentTaskID(taskID string) bool {
	// Agent IDs are UUIDs like "550e8400-e29b-41d4-a716-446655440000"
	// Shell task IDs are like "shell_1", "task_1"
	return strings.Contains(taskID, "-") && len(taskID) > 20
}

// getAgentOutput retrieves output from an agent task
func (t *TaskOutputTool) getAgentOutput(ctx context.Context, agentID string, block bool, timeout time.Duration) (ToolResult, error) {
	// If blocking, wait for completion with timeout
	if block {
		return t.waitForAgentTask(ctx, agentID, timeout)
	}

	// Non-blocking: just get current status
	result, ok := t.runner.GetResult(agentID)
	if !ok {
		return NewErrorResult(fmt.Sprintf("agent not found: %s", agentID)), nil
	}

	return t.formatAgentResult(result), nil
}

// waitForAgentTask waits for an agent to complete with timeout
func (t *TaskOutputTool) waitForAgentTask(ctx context.Context, agentID string, timeout time.Duration) (ToolResult, error) {
	// Create timeout context
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Poll for completion
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout or cancelled
			result, ok := t.runner.GetResult(agentID)
			if !ok {
				return NewErrorResult(fmt.Sprintf("agent not found: %s", agentID)), nil
			}
			// Return partial result with timeout indicator
			var builder strings.Builder
			builder.WriteString("**Timeout waiting for agent completion**\n\n")
			builder.WriteString(t.formatAgentResult(result).Content)
			return NewSuccessResultWithData(builder.String(), map[string]any{
				"agent_id":  agentID,
				"status":    string(result.Status),
				"completed": result.Completed,
				"timeout":   true,
			}), nil

		case <-ticker.C:
			result, ok := t.runner.GetResult(agentID)
			if !ok {
				return NewErrorResult(fmt.Sprintf("agent not found: %s", agentID)), nil
			}
			if result.Completed {
				return t.formatAgentResult(result), nil
			}
		}
	}
}

// waitForShellTask waits for a shell task to complete with timeout
func (t *TaskOutputTool) waitForShellTask(ctx context.Context, taskID string, timeout time.Duration) (ToolResult, error) {
	// Create timeout context
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Poll for completion
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout or cancelled
			info, ok := t.manager.GetInfo(taskID)
			if !ok {
				return NewErrorResult(fmt.Sprintf("task not found: %s", taskID)), nil
			}
			// Return partial result with timeout indicator
			var builder strings.Builder
			builder.WriteString("**Timeout waiting for task completion**\n\n")
			builder.WriteString(t.formatShellTaskResult(info).Content)
			return NewSuccessResultWithData(builder.String(), map[string]any{
				"task_id":  taskID,
				"status":   info.Status,
				"running":  info.Status == "running",
				"timeout":  true,
			}), nil

		case <-ticker.C:
			info, ok := t.manager.GetInfo(taskID)
			if !ok {
				return NewErrorResult(fmt.Sprintf("task not found: %s", taskID)), nil
			}
			if info.Status != "running" {
				return t.formatShellTaskResult(info), nil
			}
		}
	}
}

// formatShellTaskResult formats a shell task result
func (t *TaskOutputTool) formatShellTaskResult(info tasks.Info) ToolResult {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Task: %s\n", info.ID))
	builder.WriteString(fmt.Sprintf("Status: %s\n", info.Status))
	builder.WriteString(fmt.Sprintf("Command: %s\n", info.Command))
	builder.WriteString(fmt.Sprintf("Duration: %s\n", info.Duration))

	if info.Error != "" {
		builder.WriteString(fmt.Sprintf("Error: %s\n", info.Error))
	}
	if info.ExitCode != 0 {
		builder.WriteString(fmt.Sprintf("Exit Code: %d\n", info.ExitCode))
	}

	if info.Output != "" {
		builder.WriteString("\nOutput:\n")
		builder.WriteString(info.Output)
	}

	return NewSuccessResultWithData(builder.String(), map[string]any{
		"task_id":   info.ID,
		"status":    info.Status,
		"command":   info.Command,
		"output":    info.Output,
		"error":     info.Error,
		"exit_code": info.ExitCode,
		"running":   info.Status == "running",
	})
}

// formatAgentResult formats an agent result
func (t *TaskOutputTool) formatAgentResult(result AgentResult) ToolResult {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Agent: %s\n", result.AgentID))
	builder.WriteString(fmt.Sprintf("Type: %s\n", result.Type))
	builder.WriteString(fmt.Sprintf("Status: %s\n", result.Status))
	builder.WriteString(fmt.Sprintf("Duration: %s\n", result.Duration))

	if result.Error != "" {
		builder.WriteString(fmt.Sprintf("Error: %s\n", result.Error))
	}

	if result.Output != "" {
		builder.WriteString("\nOutput:\n")
		builder.WriteString(result.Output)
	}

	return NewSuccessResultWithData(builder.String(), map[string]any{
		"agent_id":  result.AgentID,
		"type":      result.Type,
		"status":    result.Status,
		"output":    result.Output,
		"error":     result.Error,
		"duration":  result.Duration.String(),
		"completed": result.Completed,
		"running":   result.Status == "running",
	})
}

func (t *TaskOutputTool) listTasks() (ToolResult, error) {
	var builder strings.Builder
	totalCount := 0

	// List shell tasks
	var shellTasks []tasks.Info
	if t.manager != nil {
		shellTasks = t.manager.List()
	}

	// List agent tasks
	var agentTasks []AgentResult
	if t.runner != nil {
		// Get all agent IDs and their results
		if lister, ok := t.runner.(AgentLister); ok {
			for _, agentID := range lister.ListAgents() {
				if result, ok := t.runner.GetResult(agentID); ok {
					agentTasks = append(agentTasks, result)
				}
			}
		}
	}

	totalCount = len(shellTasks) + len(agentTasks)

	if totalCount == 0 {
		return NewSuccessResult("No background tasks"), nil
	}

	builder.WriteString(fmt.Sprintf("Background Tasks (%d total):\n\n", totalCount))

	// Shell tasks
	if len(shellTasks) > 0 {
		builder.WriteString("**Shell Tasks:**\n")
		for _, info := range shellTasks {
			status := info.Status
			if status == "completed" {
				status = "done"
			}

			// Truncate command if too long
			cmd := info.Command
			if len(cmd) > 50 {
				cmd = cmd[:47] + "..."
			}

			builder.WriteString(fmt.Sprintf("  [%s] %s - %s (%s)\n", status, info.ID, cmd, info.Duration))
		}
		builder.WriteString("\n")
	}

	// Agent tasks
	if len(agentTasks) > 0 {
		builder.WriteString("**Agent Tasks:**\n")
		for _, result := range agentTasks {
			status := string(result.Status)
			if status == "completed" {
				status = "done"
			}

			builder.WriteString(fmt.Sprintf("  [%s] %s - %s (%s)\n", status, result.AgentID, result.Type, result.Duration.Round(time.Millisecond)))
		}
	}

	// JSON data for structured access
	shellData, _ := json.Marshal(shellTasks)
	agentData, _ := json.Marshal(agentTasks)

	return NewSuccessResultWithData(builder.String(), map[string]any{
		"shell_tasks": string(shellData),
		"agent_tasks": string(agentData),
		"count":       totalCount,
	}), nil
}

func (t *TaskOutputTool) cancelTask(ctx context.Context, taskID string) (ToolResult, error) {
	// Check if this is an agent task
	if t.runner != nil && isAgentTaskID(taskID) {
		if canceller, ok := t.runner.(AgentCanceller); ok {
			if err := canceller.Cancel(taskID); err != nil {
				return NewErrorResult(err.Error()), nil
			}
			return NewSuccessResult(fmt.Sprintf("Agent %s cancelled", taskID)), nil
		}
		return NewErrorResult("agent cancellation not supported"), nil
	}

	// Fall back to shell task manager
	if t.manager == nil {
		return NewErrorResult("task manager not configured"), nil
	}

	if err := t.manager.Cancel(taskID); err != nil {
		return NewErrorResult(err.Error()), nil
	}

	return NewSuccessResult(fmt.Sprintf("Task %s cancelled", taskID)), nil
}

// AgentLister is an interface for listing agents.
type AgentLister interface {
	ListAgents() []string
}

// AgentCanceller is an interface for cancelling agents.
type AgentCanceller interface {
	Cancel(agentID string) error
}
