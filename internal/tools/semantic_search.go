package tools

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/semantic"
)

// SemanticSearchTool performs semantic search across indexed files.
type SemanticSearchTool struct {
	indexer *semantic.EnhancedIndexer
	workDir string
	topK    int
}

// NewSemanticSearchTool creates a new semantic search tool.
func NewSemanticSearchTool(indexer *semantic.EnhancedIndexer, workDir string, topK int) *SemanticSearchTool {
	if topK <= 0 {
		topK = 10
	}
	return &SemanticSearchTool{
		indexer: indexer,
		workDir: workDir,
		topK:    topK,
	}
}

func (t *SemanticSearchTool) Name() string {
	return "semantic_search"
}

func (t *SemanticSearchTool) Description() string {
	return "Performs semantic search across the codebase using embeddings. Finds code that is conceptually similar to the query, even if it doesn't contain the exact keywords."
}

func (t *SemanticSearchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        genai.TypeString,
					Description: "The natural language query describing what you're looking for (e.g., 'error handling for API requests', 'user authentication logic')",
				},
				"top_k": {
					Type:        genai.TypeInteger,
					Description: "Number of results to return (default: 10)",
				},
			},
			Required: []string{"query"},
		},
	}
}

func (t *SemanticSearchTool) Validate(args map[string]any) error {
	query, ok := GetString(args, "query")
	if !ok || query == "" {
		return NewValidationError("query", "is required")
	}
	return nil
}

func (t *SemanticSearchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	query, _ := GetString(args, "query")
	topK := GetIntDefault(args, "top_k", t.topK)

	if t.indexer == nil {
		return NewErrorResult("semantic search is not initialized - enable it in config"), nil
	}

	// Check if any files are indexed
	if t.indexer.GetIndexedFileCount() == 0 {
		return NewErrorResult("no files indexed yet - try running /semantic-index first or wait for background indexing"), nil
	}

	// Perform search
	results, err := t.indexer.Search(ctx, query, topK)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("search failed: %s", err)), nil
	}

	if len(results) == 0 {
		return NewSuccessResult("No results found for the query"), nil
	}

	// Format results
	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d results for: %q\n\n", len(results), query))

	for i, result := range results {
		output.WriteString(fmt.Sprintf("### Result %d (score: %.3f)\n", i+1, result.Score))
		output.WriteString(fmt.Sprintf("**File:** %s (lines %d-%d)\n", result.FilePath, result.LineStart, result.LineEnd))
		output.WriteString("```\n")

		// Truncate content if too long
		content := result.Content
		if len(content) > 500 {
			lines := strings.Split(content, "\n")
			if len(lines) > 15 {
				content = strings.Join(lines[:15], "\n") + "\n... (truncated)"
			}
		}
		output.WriteString(content)
		output.WriteString("\n```\n\n")
	}

	return NewSuccessResultWithData(output.String(), results), nil
}
