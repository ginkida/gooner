package tools

import (
	"context"
	"fmt"

	"gooner/internal/tasks"

	"google.golang.org/genai"
)

// KillShellTool kills running background tasks.
type KillShellTool struct {
	manager *tasks.Manager
}

// NewKillShellTool creates a new kill shell tool.
func NewKillShellTool() *KillShellTool {
	return &KillShellTool{}
}

// SetManager sets the task manager.
func (t *KillShellTool) SetManager(manager *tasks.Manager) {
	t.manager = manager
}

func (t *KillShellTool) Name() string {
	return "kill_shell"
}

func (t *KillShellTool) Description() string {
	return "Kill a running background shell task by its ID"
}

func (t *KillShellTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"shell_id": {
					Type:        genai.TypeString,
					Description: "The ID of the background shell task to kill",
				},
			},
			Required: []string{"shell_id"},
		},
	}
}

func (t *KillShellTool) Validate(args map[string]any) error {
	if _, ok := GetString(args, "shell_id"); !ok {
		return NewValidationError("shell_id", "shell_id is required")
	}
	return nil
}

func (t *KillShellTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.manager == nil {
		return NewErrorResult("task manager not configured"), nil
	}

	shellID, _ := GetString(args, "shell_id")

	// Check if task exists
	info, ok := t.manager.GetInfo(shellID)
	if !ok {
		return NewErrorResult(fmt.Sprintf("task not found: %s", shellID)), nil
	}

	// Check if task is already completed
	if info.Status != "running" {
		return NewSuccessResultWithData(
			fmt.Sprintf("Task %s is already %s (command: %s, duration: %s)",
				shellID, info.Status, info.Command, info.Duration),
			map[string]any{
				"shell_id":    shellID,
				"status":      info.Status,
				"command":     info.Command,
				"was_running": false,
			},
		), nil
	}

	// Cancel the task
	if err := t.manager.Cancel(shellID); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to kill task: %s", err)), nil
	}

	return NewSuccessResultWithData(
		fmt.Sprintf("Killed task %s (command: %s, duration: %s)",
			shellID, info.Command, info.Duration),
		map[string]any{
			"shell_id":    shellID,
			"status":      "killed",
			"command":     info.Command,
			"duration":    info.Duration.String(),
			"was_running": true,
		},
	), nil
}
