package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/genai"
)

// HistorySearchTool searches through the agent's message history.
type HistorySearchTool struct {
	historyGetter func() []*genai.Content
}

// NewHistorySearchTool creates a new HistorySearchTool.
func NewHistorySearchTool(historyGetter func() []*genai.Content) *HistorySearchTool {
	return &HistorySearchTool{
		historyGetter: historyGetter,
	}
}

// SetHistoryGetter sets the function to retrieve message history.
func (t *HistorySearchTool) SetHistoryGetter(fn func() []*genai.Content) {
	t.historyGetter = fn
}

func (t *HistorySearchTool) Name() string {
	return "history_search"
}

func (t *HistorySearchTool) Description() string {
	return `Searches through the current session's message history using a regular expression.
Use this to recover specific details, file paths, or error messages from earlier in the conversation that might have been lost due to context truncation or summarization.

PARAMETERS:
- pattern (required): The regular expression pattern to search for in message history.

RETURNS:
- A list of matching message excerpts with their roles and order in history.`
}

func (t *HistorySearchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern": {
					Type:        genai.TypeString,
					Description: "Regex pattern to search for in history",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

func (t *HistorySearchTool) Validate(args map[string]any) error {
	pattern, ok := GetString(args, "pattern")
	if !ok || pattern == "" {
		return NewValidationError("pattern", "is required")
	}
	_, err := regexp.Compile(pattern)
	if err != nil {
		return NewValidationError("pattern", fmt.Sprintf("invalid regex: %v", err))
	}
	return nil
}

func (t *HistorySearchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	patternStr, _ := GetString(args, "pattern")
	re := regexp.MustCompile("(?i)" + patternStr)

	if t.historyGetter == nil {
		return NewErrorResult("history search not supported by this agent"), nil
	}

	history := t.historyGetter()
	var results []string

	for i, content := range history {
		for _, part := range content.Parts {
			var text string
			if part.Text != "" {
				text = part.Text
			} else if part.FunctionCall != nil {
				text = fmt.Sprintf("Tool Call: %s", part.FunctionCall.Name)
			} else if part.FunctionResponse != nil {
				text = fmt.Sprintf("Tool Response: %s", part.FunctionResponse.Name)
			}

			if text != "" && re.MatchString(text) {
				// Find matches and extract context
				locs := re.FindAllStringIndex(text, -1)
				for _, loc := range locs {
					start := loc[0] - 50
					if start < 0 {
						start = 0
					}
					end := loc[1] + 50
					if end > len(text) {
						end = len(text)
					}
					excerpt := text[start:end]
					results = append(results, fmt.Sprintf("[Message %d, Role: %s] ...%s...", i, content.Role, excerpt))
				}
			}
		}
	}

	if len(results) == 0 {
		return NewSuccessResult("No matches found in history."), nil
	}

	// Limit results
	if len(results) > 20 {
		results = append(results[:20], "... (truncated)")
	}

	return NewSuccessResult(strings.Join(results, "\n")), nil
}
