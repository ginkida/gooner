package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

// GitBlameTool shows line-by-line authorship of a file.
type GitBlameTool struct {
	workDir string
}

// NewGitBlameTool creates a new GitBlameTool instance.
func NewGitBlameTool(workDir string) *GitBlameTool {
	return &GitBlameTool{
		workDir: workDir,
	}
}

func (t *GitBlameTool) Name() string {
	return "git_blame"
}

func (t *GitBlameTool) Description() string {
	return "Shows line-by-line authorship information for a file, revealing who last modified each line."
}

func (t *GitBlameTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file": {
					Type:        genai.TypeString,
					Description: "Path to the file to blame (required)",
				},
				"start_line": {
					Type:        genai.TypeInteger,
					Description: "Start line number for range (optional)",
				},
				"end_line": {
					Type:        genai.TypeInteger,
					Description: "End line number for range (optional)",
				},
			},
			Required: []string{"file"},
		},
	}
}

func (t *GitBlameTool) Validate(args map[string]any) error {
	file, ok := GetString(args, "file")
	if !ok || file == "" {
		return NewValidationError("file", "is required")
	}

	startLine, hasStart := GetInt(args, "start_line")
	endLine, hasEnd := GetInt(args, "end_line")

	if hasStart && startLine < 1 {
		return NewValidationError("start_line", "must be >= 1")
	}

	if hasEnd && endLine < 1 {
		return NewValidationError("end_line", "must be >= 1")
	}

	if hasStart && hasEnd && endLine < startLine {
		return NewValidationError("end_line", "must be >= start_line")
	}

	return nil
}

func (t *GitBlameTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	file, _ := GetString(args, "file")
	startLine, hasStart := GetInt(args, "start_line")
	endLine, hasEnd := GetInt(args, "end_line")

	// Make path relative to workDir if absolute
	if filepath.IsAbs(file) {
		if rel, err := filepath.Rel(t.workDir, file); err == nil {
			file = rel
		}
	}

	// Build git blame command
	cmdArgs := []string{"blame"}

	// Line range: -L start,end
	if hasStart || hasEnd {
		start := 1
		end := 0 // 0 means to end of file
		if hasStart {
			start = startLine
		}
		if hasEnd {
			end = endLine
		}

		if end > 0 {
			cmdArgs = append(cmdArgs, fmt.Sprintf("-L%d,%d", start, end))
		} else {
			cmdArgs = append(cmdArgs, fmt.Sprintf("-L%d,", start))
		}
	}

	cmdArgs = append(cmdArgs, "--", file)

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(stderr, "no such path") || strings.Contains(stderr, "fatal") {
				return NewErrorResult(fmt.Sprintf("git blame failed: %s", stderr)), nil
			}
			return NewErrorResult(fmt.Sprintf("git blame failed: %s", stderr)), nil
		}
		return NewErrorResult(fmt.Sprintf("git blame failed: %s", err)), nil
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		return NewSuccessResult("No blame information available."), nil
	}

	return NewSuccessResult(result), nil
}
