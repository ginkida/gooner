package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"
)

// DiffTool compares two files or a file with provided content.
type DiffTool struct{}

// NewDiffTool creates a new DiffTool instance.
func NewDiffTool() *DiffTool {
	return &DiffTool{}
}

func (t *DiffTool) Name() string {
	return "diff"
}

func (t *DiffTool) Description() string {
	return "Compares two files or a file with provided content and shows the differences."
}

func (t *DiffTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file1": {
					Type:        genai.TypeString,
					Description: "Path to the first file",
				},
				"file2": {
					Type:        genai.TypeString,
					Description: "Path to the second file (optional if content is provided)",
				},
				"content": {
					Type:        genai.TypeString,
					Description: "Content to compare against file1 (alternative to file2)",
				},
				"context_lines": {
					Type:        genai.TypeInteger,
					Description: "Number of context lines around changes (default: 3)",
				},
			},
			Required: []string{"file1"},
		},
	}
}

func (t *DiffTool) Validate(args map[string]any) error {
	file1, ok := GetString(args, "file1")
	if !ok || file1 == "" {
		return NewValidationError("file1", "is required")
	}

	file2, hasFile2 := GetString(args, "file2")
	content, hasContent := GetString(args, "content")

	if (!hasFile2 || file2 == "") && (!hasContent || content == "") {
		return NewValidationError("file2/content", "either file2 or content must be provided")
	}

	return nil
}

func (t *DiffTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	file1, _ := GetString(args, "file1")
	file2, hasFile2 := GetString(args, "file2")
	content, _ := GetString(args, "content")
	contextLines := GetIntDefault(args, "context_lines", 3)

	// Read first file
	content1, err := os.ReadFile(file1)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error reading file1: %s", err)), nil
	}

	// Get second content
	var content2 []byte
	var label2 string
	if hasFile2 && file2 != "" {
		content2, err = os.ReadFile(file2)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("error reading file2: %s", err)), nil
		}
		label2 = file2
	} else {
		content2 = []byte(content)
		label2 = "(provided content)"
	}

	// Generate unified diff
	diff := unifiedDiff(file1, label2, string(content1), string(content2), contextLines)

	if diff == "" {
		return NewSuccessResult("Files are identical"), nil
	}

	return NewSuccessResult(diff), nil
}

// unifiedDiff generates a unified diff between two strings.
func unifiedDiff(label1, label2, text1, text2 string, contextLines int) string {
	lines1 := strings.Split(text1, "\n")
	lines2 := strings.Split(text2, "\n")

	// Simple line-by-line diff
	// For a production implementation, you might want to use a proper diff algorithm
	var result strings.Builder
	result.WriteString(fmt.Sprintf("--- %s\n", label1))
	result.WriteString(fmt.Sprintf("+++ %s\n", label2))

	// Find differences using longest common subsequence approach
	hunks := computeDiffHunks(lines1, lines2, contextLines)

	if len(hunks) == 0 {
		return ""
	}

	for _, hunk := range hunks {
		result.WriteString(hunk)
	}

	return result.String()
}

// computeDiffHunks computes diff hunks between two sets of lines.
func computeDiffHunks(lines1, lines2 []string, contextLines int) []string {
	var hunks []string

	// Create maps for quick lookup
	i, j := 0, 0
	var currentHunk strings.Builder
	var hunkStarted bool
	hunkStart1, hunkStart2 := 0, 0
	hunkLen1, hunkLen2 := 0, 0

	flushHunk := func() {
		if hunkStarted && currentHunk.Len() > 0 {
			header := fmt.Sprintf("@@ -%d,%d +%d,%d @@\n",
				hunkStart1+1, hunkLen1, hunkStart2+1, hunkLen2)
			hunks = append(hunks, header+currentHunk.String())
			currentHunk.Reset()
			hunkStarted = false
			hunkLen1, hunkLen2 = 0, 0
		}
	}

	// Simple diff: compare line by line
	for i < len(lines1) || j < len(lines2) {
		if i < len(lines1) && j < len(lines2) && lines1[i] == lines2[j] {
			// Lines match
			if hunkStarted {
				currentHunk.WriteString(" " + lines1[i] + "\n")
				hunkLen1++
				hunkLen2++
			}
			i++
			j++
		} else {
			// Lines differ
			if !hunkStarted {
				hunkStarted = true
				hunkStart1 = i
				hunkStart2 = j

				// Add context before
				start := max(0, i-contextLines)
				for k := start; k < i; k++ {
					currentHunk.WriteString(" " + lines1[k] + "\n")
					hunkLen1++
					hunkLen2++
				}
				hunkStart1 = start
				hunkStart2 = max(0, j-contextLines)
			}

			// Find where they sync up again
			if i < len(lines1) {
				found := false
				for k := j; k < min(j+10, len(lines2)); k++ {
					if lines1[i] == lines2[k] {
						// Output removed and added lines
						for ; j < k; j++ {
							currentHunk.WriteString("+" + lines2[j] + "\n")
							hunkLen2++
						}
						found = true
						break
					}
				}
				if !found {
					currentHunk.WriteString("-" + lines1[i] + "\n")
					hunkLen1++
					i++
				}
			} else if j < len(lines2) {
				currentHunk.WriteString("+" + lines2[j] + "\n")
				hunkLen2++
				j++
			}
		}

		// Flush hunk if we've had enough context after changes
		if hunkStarted && i < len(lines1) && j < len(lines2) {
			matchCount := 0
			for k := 0; k < contextLines*2 && i+k < len(lines1) && j+k < len(lines2); k++ {
				if lines1[i+k] == lines2[j+k] {
					matchCount++
				} else {
					break
				}
			}
			if matchCount >= contextLines*2 {
				// Add context after
				for k := 0; k < contextLines && i < len(lines1) && j < len(lines2); k++ {
					if lines1[i] == lines2[j] {
						currentHunk.WriteString(" " + lines1[i] + "\n")
						hunkLen1++
						hunkLen2++
						i++
						j++
					}
				}
				flushHunk()
			}
		}
	}

	flushHunk()
	return hunks
}
