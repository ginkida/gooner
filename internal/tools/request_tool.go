package tools

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// RequestToolTool allows an agent to request a tool it doesn't currently have.
type RequestToolTool struct {
	requester ToolRequester
}

// NewRequestToolTool creates a new request_tool tool.
func NewRequestToolTool() *RequestToolTool {
	return &RequestToolTool{}
}

// SetRequester sets the component that can fulfill tool requests.
func (t *RequestToolTool) SetRequester(requester ToolRequester) {
	t.requester = requester
}

func (t *RequestToolTool) Name() string {
	return "request_tool"
}

func (t *RequestToolTool) Description() string {
	return "Requests a tool from the system that is not currently in your toolkit. Use 'tools_list' to see what's available."
}

func (t *RequestToolTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"tool_name": {
					Type:        genai.TypeString,
					Description: "The name of the tool to request",
				},
			},
			Required: []string{"tool_name"},
		},
	}
}

func (t *RequestToolTool) Validate(args map[string]any) error {
	toolName, ok := GetString(args, "tool_name")
	if !ok || toolName == "" {
		return NewValidationError("tool_name", "is required")
	}
	return nil
}

func (t *RequestToolTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.requester == nil {
		return NewErrorResult("tool requester not initialized for this agent"), nil
	}

	toolName, _ := GetString(args, "tool_name")

	if err := t.requester.RequestTool(toolName); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to request tool: %s", err)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Tool '%s' has been successfully added to your toolkit. You can use it now.", toolName)), nil
}
