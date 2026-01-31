package tools

import (
	"context"
	"fmt"

	"gokin/internal/plan"
	"gokin/internal/undo"

	"google.golang.org/genai"
)

// UndoPlanTool allows undoing the last executed plan.
type UndoPlanTool struct {
	manager     *plan.Manager
	undoManager *undo.Manager
}

// NewUndoPlanTool creates a new undo plan tool.
func NewUndoPlanTool() *UndoPlanTool {
	return &UndoPlanTool{}
}

// SetManager sets the plan manager.
func (t *UndoPlanTool) SetManager(manager *plan.Manager) {
	t.manager = manager
}

// SetUndoManager sets the undo manager for file operations.
func (t *UndoPlanTool) SetUndoManager(undoManager *undo.Manager) {
	t.undoManager = undoManager
}

func (t *UndoPlanTool) Name() string {
	return "undo_plan"
}

func (t *UndoPlanTool) Description() string {
	return "Undo the last executed plan and restore the state before execution"
}

func (t *UndoPlanTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"confirm": {
					Type:        genai.TypeString,
					Description: "Confirmation to undo the plan (must be 'yes' or 'true')",
				},
			},
		},
	}
}

func (t *UndoPlanTool) Validate(args map[string]any) error {
	if confirm, ok := GetString(args, "confirm"); ok {
		if confirm != "yes" && confirm != "true" {
			return NewValidationError("confirm", "confirmation must be 'yes' or 'true'")
		}
	}
	return nil
}

func (t *UndoPlanTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.manager == nil {
		return NewErrorResult("plan manager not configured"), nil
	}

	undoExt := t.manager.GetUndoExtension()
	if undoExt == nil {
		return NewErrorResult("undo support is not enabled for this plan"), nil
	}

	if !undoExt.CanUndo() {
		return NewErrorResult("no plan execution history to undo"), nil
	}

	checkpoint := undoExt.GetLastCheckpoint()

	// Undo file changes through the undo manager
	var undoneFiles []string
	if t.undoManager != nil {
		// Undo all file changes made during plan execution
		for {
			change, err := t.undoManager.Undo()
			if err != nil {
				// No more changes to undo or error occurred
				break
			}
			if change != nil {
				undoneFiles = append(undoneFiles, change.FilePath)
			}
		}
	}

	// Clear the current plan
	t.manager.ClearPlan()

	// Remove checkpoint from history
	undoExt.ClearHistory()

	// Build result message
	resultMsg := fmt.Sprintf("Plan undone successfully: %s\n", checkpoint.PlanTitle)
	if len(undoneFiles) > 0 {
		resultMsg += fmt.Sprintf("\nReverted %d file changes:\n", len(undoneFiles))
		for _, file := range undoneFiles {
			resultMsg += fmt.Sprintf("  • %s\n", file)
		}
	}
	if len(checkpoint.Executed) > 0 {
		resultMsg += fmt.Sprintf("\nExecuted steps that were undone: %v\n", checkpoint.Executed)
	}

	return NewSuccessResultWithData(
		resultMsg,
		map[string]any{
			"undone":         true,
			"plan_id":        checkpoint.PlanID,
			"plan_title":     checkpoint.PlanTitle,
			"executed_steps": checkpoint.Executed,
			"undone_files":   undoneFiles,
		},
	), nil
}
