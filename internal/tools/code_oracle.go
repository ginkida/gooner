package tools

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// CodeOracleTool provides high-level architectural analysis.
type CodeOracleTool struct {
	workDir  string
	registry ToolListerRegistry
}

// ToolListerRegistry is an interface that matches both Registry and LazyRegistry.
type ToolListerRegistry interface {
	Get(name string) (Tool, bool)
}

// NewCodeOracleTool creates a new CodeOracleTool instance.
func NewCodeOracleTool(workDir string, registry ToolListerRegistry) *CodeOracleTool {
	return &CodeOracleTool{
		workDir:  workDir,
		registry: registry,
	}
}

func (t *CodeOracleTool) Name() string {
	return "code_oracle"
}

func (t *CodeOracleTool) Description() string {
	return `Architectural analysis tool. Answers high-level questions about code structure and flow.
Combines semantic search, grep, and code graph analysis to provide a comprehensive report.`
}

func (t *CodeOracleTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        genai.TypeString,
					Description: "The architectural question to answer (e.g., 'How is authentication implemented?')",
				},
			},
			Required: []string{"query"},
		},
	}
}

func (t *CodeOracleTool) Validate(args map[string]any) error {
	query, ok := GetString(args, "query")
	if !ok || query == "" {
		return NewValidationError("query", "is required")
	}
	return nil
}

func (t *CodeOracleTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	query, _ := GetString(args, "query")

	var report strings.Builder
	report.WriteString(fmt.Sprintf("# Code Oracle Report for: %s\n\n", query))

	// 1. Semantic Search for context
	semanticTool, ok := t.registry.Get("semantic_search")
	if ok {
		res, err := semanticTool.Execute(ctx, map[string]any{"query": query, "top_k": 5})
		if err == nil && res.Success {
			report.WriteString("## Relevant Components (Semantic Search)\n")
			report.WriteString(res.Content)
			report.WriteString("\n\n")
		}
	}

	// 2. Identify key symbols and check dependencies
	// This is a simplified version - in a real implementation we might parse the semantic search results
	// to find symbols and then call code_graph.

	graphTool, ok := t.registry.Get("code_graph")
	if ok {
		// Try to find a symbol in the query
		words := strings.Fields(query)
		for _, word := range words {
			if len(word) > 3 && (strings.Contains(word, "Service") || strings.Contains(word, "Controller") || strings.Contains(word, "Manager")) {
				res, err := graphTool.Execute(ctx, map[string]any{"action": "dependencies", "symbol": word})
				if err == nil && res.Success {
					report.WriteString(fmt.Sprintf("## Dependencies for Symbol: %s\n", word))
					report.WriteString(res.Content)
					report.WriteString("\n\n")
				}
			}
		}
	}

	report.WriteString("## Conclusion\n")
	report.WriteString("The query involves the components listed above. You should examine them to understand the full architectural flow.")

	return NewSuccessResult(report.String()), nil
}
