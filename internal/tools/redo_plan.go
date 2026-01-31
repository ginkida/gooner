package tools

import (
	"context"
	"fmt"

	"gokin/internal/plan"
	"gokin/internal/undo"

	"google.golang.org/genai"
)

// RedoPlanTool allows redoing a previously undone plan.
type RedoPlanTool struct {
	manager     *plan.Manager
	undoManager *undo.Manager
}

// NewRedoPlanTool creates a new redo plan tool.
func NewRedoPlanTool() *RedoPlanTool {
	return &RedoPlanTool{}
}

// SetManager sets the plan manager.
func (t *RedoPlanTool) SetManager(manager *plan.Manager) {
	t.manager = manager
}

// SetUndoManager sets the undo manager for redo operations.
func (t *RedoPlanTool) SetUndoManager(undoManager *undo.Manager) {
	t.undoManager = undoManager
}

func (t *RedoPlanTool) Name() string {
	return "redo_plan"
}

func (t *RedoPlanTool) Description() string {
	return "Redo a previously undone plan by re-executing it"
}

func (t *RedoPlanTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"plan_id": {
					Type:        genai.TypeString,
					Description: "Optional: specific plan ID to redo (default: last undone plan)",
				},
			},
		},
	}
}

func (t *RedoPlanTool) Validate(args map[string]any) error {
	// No required parameters
	return nil
}

func (t *RedoPlanTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.manager == nil {
		return NewErrorResult("plan manager not configured"), nil
	}

	// Redo file changes through the undo manager
	var redoneFiles []string
	if t.undoManager != nil {
		// Redo all undone file changes
		for {
			change, err := t.undoManager.Redo()
			if err != nil {
				// No more changes to redo or error occurred
				break
			}
			if change != nil {
				redoneFiles = append(redoneFiles, change.FilePath)
			}
		}
	}

	if len(redoneFiles) == 0 {
		return NewErrorResult("no changes to redo - no plans have been undone"), nil
	}

	// Build result message
	resultMsg := fmt.Sprintf("Plan redone successfully\n")
	if len(redoneFiles) > 0 {
		resultMsg += fmt.Sprintf("\nRe-applied %d file changes:\n", len(redoneFiles))
		for _, file := range redoneFiles {
			resultMsg += fmt.Sprintf("  • %s\n", file)
		}
	}

	return NewSuccessResultWithData(
		resultMsg,
		map[string]any{
			"redone":       true,
			"redone_files": redoneFiles,
		},
	), nil
}
