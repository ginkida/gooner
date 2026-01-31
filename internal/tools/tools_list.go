package tools

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// ToolsListTool returns a list of all available tools in the registry.
type ToolsListTool struct {
	baseRegistry *Registry
}

// NewToolsListTool creates a new tools_list tool.
func NewToolsListTool(registry *Registry) *ToolsListTool {
	return &ToolsListTool{baseRegistry: registry}
}

func (t *ToolsListTool) Name() string {
	return "tools_list"
}

func (t *ToolsListTool) Description() string {
	return "Returns a list of all available tools in the system with their descriptions. Use this to discover tools you don't currently have access to."
}

func (t *ToolsListTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties:   map[string]*genai.Schema{},
		},
	}
}

func (t *ToolsListTool) Validate(args map[string]any) error {
	return nil
}

func (t *ToolsListTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.baseRegistry == nil {
		return NewErrorResult("base registry not initialized"), nil
	}

	tools := t.baseRegistry.List()
	var output strings.Builder
	output.WriteString("Available tools in the system:\n\n")

	for _, tool := range tools {
		output.WriteString(fmt.Sprintf("- **%s**: %s\n", tool.Name(), tool.Description()))
	}

	output.WriteString("\nIf you need a tool that is not in your current toolkit, use 'request_tool' to request it.")

	return NewSuccessResult(output.String()), nil
}
