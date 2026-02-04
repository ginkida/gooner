package tools

import (
	"context"

	"google.golang.org/genai"
)

// PinContextTool allows the agent to pin information to the system prompt.
type PinContextTool struct {
	updater func(content string)
}

// NewPinContextTool creates a new PinContextTool.
func NewPinContextTool(updater func(content string)) *PinContextTool {
	return &PinContextTool{
		updater: updater,
	}
}

// SetUpdater sets the function to update pinned context.
func (t *PinContextTool) SetUpdater(fn func(string)) {
	t.updater = fn
}

func (t *PinContextTool) Name() string {
	return "pin_context"
}

func (t *PinContextTool) Description() string {
	return `Pins a snippet of information to your system prompt for the rest of the session.
Use this for "hot memory" — to keep track of your current high-level goal, important file paths, or complex constraints that you don't want to lose focus on.

PARAMETERS:
- content (required): The information to pin. Providing an empty string or 'clear' will unpin all context.
- clear (optional): If true, clears the pinned context rather than setting it.`
}

func (t *PinContextTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"content": {
					Type:        genai.TypeString,
					Description: "Text to pin to system prompt",
				},
				"clear": {
					Type:        genai.TypeBoolean,
					Description: "If true, clear existing pinned context",
				},
			},
			Required: []string{"content"},
		},
	}
}

func (t *PinContextTool) Validate(args map[string]any) error {
	_, ok := GetString(args, "content")
	if !ok {
		return NewValidationError("content", "is required")
	}
	return nil
}

func (t *PinContextTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	content, _ := GetString(args, "content")
	clear, _ := args["clear"].(bool)

	if t.updater == nil {
		return NewErrorResult("pinned context not supported by this agent"), nil
	}

	if clear || content == "clear" {
		t.updater("")
		return NewSuccessResult("Pinned context cleared."), nil
	}

	t.updater(content)
	return NewSuccessResult("Information pinned to system prompt."), nil
}
