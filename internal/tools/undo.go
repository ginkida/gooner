package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"gokin/internal/undo"
)

// UndoTool provides undo/redo functionality for file operations.
type UndoTool struct {
	manager *undo.Manager
}

// NewUndoTool creates a new UndoTool instance.
func NewUndoTool() *UndoTool {
	return &UndoTool{
		manager: undo.NewManager(),
	}
}

// SetManager sets the undo manager.
func (t *UndoTool) SetManager(manager *undo.Manager) {
	t.manager = manager
}

// GetManager returns the undo manager.
func (t *UndoTool) GetManager() *undo.Manager {
	return t.manager
}

func (t *UndoTool) Name() string {
	return "undo"
}

func (t *UndoTool) Description() string {
	return "Manages undo/redo for file operations. Actions: 'undo' (revert last change), 'redo' (re-apply last undone change), 'list' (show recent changes)."
}

func (t *UndoTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "The action to perform: 'undo', 'redo', or 'list'",
					Enum:        []string{"undo", "redo", "list"},
				},
				"count": {
					Type:        genai.TypeInteger,
					Description: "For 'list' action: number of recent changes to show (default 10)",
				},
			},
			Required: []string{"action"},
		},
	}
}

func (t *UndoTool) Validate(args map[string]any) error {
	action, ok := GetString(args, "action")
	if !ok || action == "" {
		return NewValidationError("action", "is required")
	}

	switch action {
	case "undo", "redo", "list":
		// Valid actions
	default:
		return NewValidationError("action", "must be 'undo', 'redo', or 'list'")
	}

	return nil
}

func (t *UndoTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.manager == nil {
		return NewErrorResult("undo system not initialized"), nil
	}

	action, _ := GetString(args, "action")

	switch action {
	case "undo":
		return t.doUndo()
	case "redo":
		return t.doRedo()
	case "list":
		count := GetIntDefault(args, "count", 10)
		return t.doList(count)
	default:
		return NewErrorResult(fmt.Sprintf("unknown action: %s", action)), nil
	}
}

func (t *UndoTool) doUndo() (ToolResult, error) {
	if !t.manager.CanUndo() {
		return NewSuccessResult("Nothing to undo."), nil
	}

	change, err := t.manager.Undo()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("Undo failed: %s", err)), nil
	}

	var status string
	if change.WasNew {
		status = fmt.Sprintf("Undone: deleted created file %s", change.FilePath)
	} else {
		status = fmt.Sprintf("Undone: reverted changes to %s", change.FilePath)
	}

	return NewSuccessResult(status), nil
}

func (t *UndoTool) doRedo() (ToolResult, error) {
	if !t.manager.CanRedo() {
		return NewSuccessResult("Nothing to redo."), nil
	}

	change, err := t.manager.Redo()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("Redo failed: %s", err)), nil
	}

	var status string
	if change.WasNew {
		status = fmt.Sprintf("Redone: recreated file %s", change.FilePath)
	} else {
		status = fmt.Sprintf("Redone: re-applied changes to %s", change.FilePath)
	}

	return NewSuccessResult(status), nil
}

func (t *UndoTool) doList(count int) (ToolResult, error) {
	changes := t.manager.ListRecent(count)

	if len(changes) == 0 {
		return NewSuccessResult("No changes tracked."), nil
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Recent changes (%d total):\n\n", t.manager.Count()))

	for i, change := range changes {
		timeAgo := formatTimeAgo(change.Timestamp)
		action := "modified"
		if change.WasNew {
			action = "created"
		}
		builder.WriteString(fmt.Sprintf("%d. [%s] %s %s (%s)\n",
			i+1, change.ID[:8], action, change.FilePath, timeAgo))
	}

	if t.manager.CanUndo() {
		builder.WriteString("\nUse 'undo' to revert the most recent change.")
	}
	if t.manager.CanRedo() {
		builder.WriteString("\nUse 'redo' to re-apply an undone change.")
	}

	return NewSuccessResult(builder.String()), nil
}

// formatTimeAgo returns a human-readable time difference.
func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
