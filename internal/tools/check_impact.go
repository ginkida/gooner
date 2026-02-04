package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/genai"
)

// CheckImpactTool analyzes the impact of changing a symbol.
type CheckImpactTool struct {
	workDir string
}

// NewCheckImpactTool creates a new CheckImpactTool instance.
func NewCheckImpactTool(workDir string) *CheckImpactTool {
	return &CheckImpactTool{
		workDir: workDir,
	}
}

func (t *CheckImpactTool) Name() string {
	return "check_impact"
}

func (t *CheckImpactTool) Description() string {
	return `Blast Radius Analysis tool. Finds all usages and potential impacts of changing a symbol (function, variable, etc.).
It categorizes findings into Imports, Definitions, and Usages to help assess the risk of modification.`
}

func (t *CheckImpactTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"symbol": {
					Type:        genai.TypeString,
					Description: "The symbol name to analyze (e.g., 'Agent', 'Run', 'executeTool')",
				},
			},
			Required: []string{"symbol"},
		},
	}
}

func (t *CheckImpactTool) Validate(args map[string]any) error {
	symbol, ok := GetString(args, "symbol")
	if !ok || symbol == "" {
		return NewValidationError("symbol", "is required")
	}
	return nil
}

func (t *CheckImpactTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	symbol, _ := GetString(args, "symbol")

	// 1. Search for usages using ripgrep (if available) or grep
	cmd := exec.CommandContext(ctx, "grep", "-r", "--exclude-dir=.git", "-n", symbol, t.workDir)
	output, _ := cmd.CombinedOutput()

	lines := strings.Split(string(output), "\n")

	var report strings.Builder
	report.WriteString(fmt.Sprintf("# Impact Report for symbol: %s\n\n", symbol))

	categories := map[string][]string{
		"Definitions": {},
		"Imports":     {},
		"Usages":      {},
	}

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Simple heuristic categorization
		lowerLine := strings.ToLower(line)
		if strings.Contains(lowerLine, "func ") || strings.Contains(lowerLine, "type ") || strings.Contains(lowerLine, "var ") {
			categories["Definitions"] = append(categories["Definitions"], line)
		} else if strings.Contains(lowerLine, "import ") || strings.Contains(lowerLine, "require(") {
			categories["Imports"] = append(categories["Imports"], line)
		} else {
			categories["Usages"] = append(categories["Usages"], line)
		}
	}

	for cat, matches := range categories {
		if len(matches) > 0 {
			report.WriteString(fmt.Sprintf("## %s (%d)\n", cat, len(matches)))
			// Limit display to 10 per category
			limit := 10
			if len(matches) < limit {
				limit = len(matches)
			}
			for i := 0; i < limit; i++ {
				// Clean path for readability
				cleanLine := strings.TrimPrefix(matches[i], t.workDir)
				report.WriteString(fmt.Sprintf("- %s\n", cleanLine))
			}
			if len(matches) > limit {
				report.WriteString(fmt.Sprintf("- ... and %d more\n", len(matches)-limit))
			}
			report.WriteString("\n")
		}
	}

	if report.Len() < 100 { // Just the header
		return NewSuccessResult(fmt.Sprintf("No significant impact found for symbol: %s. It might be private or unused.", symbol)), nil
	}

	return NewSuccessResult(report.String()), nil
}
