package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/semantic"
)

// CodeGraphTool provides code graph and dependency analysis.
type CodeGraphTool struct {
	workDir string
}

// NewCodeGraphTool creates a new CodeGraphTool instance.
func NewCodeGraphTool() *CodeGraphTool {
	return &CodeGraphTool{}
}

// SetWorkDir sets the working directory.
func (t *CodeGraphTool) SetWorkDir(workDir string) {
	t.workDir = workDir
}

func (t *CodeGraphTool) Name() string {
	return "code_graph"
}

func (t *CodeGraphTool) Description() string {
	return "Analyzes code dependencies and relationships. Can find circular dependencies, build dependency graphs, and analyze code structure."
}

func (t *CodeGraphTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: 'build', 'deps', 'dependents', 'cycles'",
					Enum:        []string{"build", "deps", "dependents", "cycles"},
				},
				"file_path": {
					Type:        genai.TypeString,
					Description: "File path for dependency analysis (for deps/dependents actions)",
				},
				"limit": {
					Type:        genai.TypeInteger,
					Description: "Limit number of results (default: 50)",
				},
			},
			Required: []string{"action"},
		},
	}
}

func (t *CodeGraphTool) Validate(args map[string]any) error {
	action, ok := GetString(args, "action")
	if !ok || action == "" {
		return NewValidationError("action", "is required")
	}

	if action == "deps" || action == "dependents" {
		filePath, _ := GetString(args, "file_path")
		if filePath == "" {
			return NewValidationError("file_path", "is required for this action")
		}
	}

	return nil
}

func (t *CodeGraphTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	action, _ := GetString(args, "action")

	switch action {
	case "build":
		return t.buildGraph(ctx)
	case "deps":
		return t.getDependencies(ctx, args)
	case "dependents":
		return t.getDependents(ctx, args)
	case "cycles":
		return t.findCycles(ctx)
	default:
		return NewErrorResult(fmt.Sprintf("unknown action: %s", action)), nil
	}
}

func (t *CodeGraphTool) buildGraph(ctx context.Context) (ToolResult, error) {
	graph, err := semantic.BuildDependencyGraph(t.workDir)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to build graph: %s", err)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Built code graph with %d nodes", len(graph.GetAllNodes()))), nil
}

func (t *CodeGraphTool) getDependencies(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")

	graph, err := semantic.BuildDependencyGraph(t.workDir)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to build graph: %s", err)), nil
	}

	relPath, _ := filepath.Rel(t.workDir, filePath)
	nodeID := fmt.Sprintf("file:%s", relPath)
	deps := graph.GetDependencies(nodeID)

	if len(deps) == 0 {
		return NewSuccessResult(fmt.Sprintf("No dependencies found for %s", filePath)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Dependencies of %s:\n- %s",
		filePath, strings.Join(deps, "\n- "))), nil
}

func (t *CodeGraphTool) getDependents(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")

	graph, err := semantic.BuildDependencyGraph(t.workDir)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to build graph: %s", err)), nil
	}

	relPath, _ := filepath.Rel(t.workDir, filePath)
	nodeID := fmt.Sprintf("file:%s", relPath)
	dependents := graph.GetDependents(nodeID)

	if len(dependents) == 0 {
		return NewSuccessResult(fmt.Sprintf("No dependents found for %s", filePath)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Files that depend on %s:\n- %s",
		filePath, strings.Join(dependents, "\n- "))), nil
}

func (t *CodeGraphTool) findCycles(ctx context.Context) (ToolResult, error) {
	graph, err := semantic.BuildDependencyGraph(t.workDir)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to build graph: %s", err)), nil
	}

	cycles := graph.FindCircularDeps()
	if len(cycles) == 0 {
		return NewSuccessResult("No circular dependencies found"), nil
	}

	var output []string
	for i, cycle := range cycles {
		output = append(output, fmt.Sprintf("Cycle %d: %s", i+1, strings.Join(cycle, " -> ")))
	}

	return NewSuccessResult(fmt.Sprintf("Found %d circular dependency cycle(s):\n%s",
		len(cycles), strings.Join(output, "\n"))), nil
}

// PatternSearchTool searches for code patterns and anti-patterns.
type PatternSearchTool struct {
	workDir string
}

// NewPatternSearchTool creates a new PatternSearchTool instance.
func NewPatternSearchTool() *PatternSearchTool {
	return &PatternSearchTool{}
}

// SetWorkDir sets the working directory.
func (t *PatternSearchTool) SetWorkDir(workDir string) {
	t.workDir = workDir
}

func (t *PatternSearchTool) Name() string {
	return "pattern_search"
}

func (t *PatternSearchTool) Description() string {
	return "Searches for code patterns like singletons, anti-patterns, and custom patterns. Can detect god functions, deep nesting, and code smells."
}

func (t *PatternSearchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern_type": {
					Type:        genai.TypeString,
					Description: "Pattern type: 'singletons', 'anti_patterns', 'custom'",
					Enum:        []string{"singletons", "anti_patterns", "custom"},
				},
				"custom_pattern": {
					Type:        genai.TypeString,
					Description: "Custom regex pattern (for pattern_type='custom')",
				},
				"pattern_name": {
					Type:        genai.TypeString,
					Description: "Name for custom pattern",
				},
				"limit": {
					Type:        genai.TypeInteger,
					Description: "Limit results (default: 50)",
				},
			},
			Required: []string{"pattern_type"},
		},
	}
}

func (t *PatternSearchTool) Validate(args map[string]any) error {
	patternType, ok := GetString(args, "pattern_type")
	if !ok || patternType == "" {
		return NewValidationError("pattern_type", "is required")
	}

	if patternType == "custom" {
		customPattern, _ := GetString(args, "custom_pattern")
		if customPattern == "" {
			return NewValidationError("custom_pattern", "is required for custom pattern type")
		}
	}

	return nil
}

func (t *PatternSearchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	patternType, _ := GetString(args, "pattern_type")
	limit := GetIntDefault(args, "limit", 50)

	matcher := semantic.NewPatternMatcher(t.workDir)

	var results []semantic.PatternResult
	var err error

	switch patternType {
	case "singletons":
		results, err = matcher.FindSingletons(ctx)
	case "anti_patterns":
		results, err = matcher.FindAntiPatterns(ctx)
	case "custom":
		customPattern, _ := GetString(args, "custom_pattern")
		patternName, _ := GetString(args, "pattern_name")
		if patternName == "" {
			patternName = "custom"
		}
		results, err = matcher.FindPattern(ctx, customPattern, patternName)
	default:
		return NewErrorResult(fmt.Sprintf("unknown pattern type: %s", patternType)), nil
	}

	if err != nil {
		return NewErrorResult(fmt.Sprintf("pattern search failed: %s", err)), nil
	}

	if len(results) == 0 {
		return NewSuccessResult(fmt.Sprintf("No matches found for pattern type: %s", patternType)), nil
	}

	// Limit results
	if len(results) > limit {
		results = results[:limit]
	}

	// Format results
	var output []string
	for _, r := range results {
		output = append(output, fmt.Sprintf("%s:%d - %s\n  Pattern: %s (confidence: %.2f)",
			r.FilePath, r.Line, r.Match, r.PatternName, r.Confidence))
	}

	return NewSuccessResult(fmt.Sprintf("Found %d match(es):\n\n%s",
		len(results), strings.Join(output, "\n\n"))), nil
}
