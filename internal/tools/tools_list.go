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
	lister       ToolLister // For lazy registry support
}

// NewToolsListTool creates a new tools_list tool.
func NewToolsListTool(registry *Registry) *ToolsListTool {
	return &ToolsListTool{baseRegistry: registry}
}

// NewToolsListToolLazy creates a new tools_list tool with ToolLister interface.
// This avoids cyclic dependency when used with LazyRegistry.
func NewToolsListToolLazy(lister ToolLister) *ToolsListTool {
	return &ToolsListTool{lister: lister}
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
	var output strings.Builder
	output.WriteString("Available tools in the system:\n\n")

	// Use lister if available (lazy registry), otherwise use baseRegistry
	if t.lister != nil {
		// Use declarations to avoid instantiating tools
		decls := t.lister.Declarations()
		for _, decl := range decls {
			// Truncate description for display
			desc := decl.Description
			if len(desc) > 100 {
				desc = desc[:97] + "..."
			}
			output.WriteString(fmt.Sprintf("- **%s**: %s\n", decl.Name, desc))
		}
	} else if t.baseRegistry != nil {
		tools := t.baseRegistry.List()
		for _, tool := range tools {
			output.WriteString(fmt.Sprintf("- **%s**: %s\n", tool.Name(), tool.Description()))
		}
	} else {
		return NewErrorResult("no registry or lister configured"), nil
	}

	output.WriteString("\nIf you need a tool that is not in your current toolkit, use 'request_tool' to request it.")

	return NewSuccessResult(output.String()), nil
}
