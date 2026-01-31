package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

// GitLogTool shows git commit history.
type GitLogTool struct {
	workDir string
}

// NewGitLogTool creates a new GitLogTool instance.
func NewGitLogTool(workDir string) *GitLogTool {
	return &GitLogTool{
		workDir: workDir,
	}
}

func (t *GitLogTool) Name() string {
	return "git_log"
}

func (t *GitLogTool) Description() string {
	return "Shows git commit history with optional filtering by file, author, or date range."
}

func (t *GitLogTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"count": {
					Type:        genai.TypeInteger,
					Description: "Number of commits to show (default: 10, max: 100)",
				},
				"file": {
					Type:        genai.TypeString,
					Description: "Show history for a specific file (follows renames)",
				},
				"oneline": {
					Type:        genai.TypeBoolean,
					Description: "Use compact one-line format (default: true)",
				},
				"since": {
					Type:        genai.TypeString,
					Description: "Show commits since date (e.g., '2 weeks ago', '2024-01-01')",
				},
				"author": {
					Type:        genai.TypeString,
					Description: "Filter by author name or email",
				},
				"grep": {
					Type:        genai.TypeString,
					Description: "Filter by commit message pattern",
				},
			},
		},
	}
}

func (t *GitLogTool) Validate(args map[string]any) error {
	// All parameters are optional
	if count, ok := GetInt(args, "count"); ok {
		if count < 1 || count > 100 {
			return NewValidationError("count", "must be between 1 and 100")
		}
	}
	return nil
}

func (t *GitLogTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	count := GetIntDefault(args, "count", 10)
	if count > 100 {
		count = 100
	}

	file := GetStringDefault(args, "file", "")
	oneline := GetBoolDefault(args, "oneline", true)
	since := GetStringDefault(args, "since", "")
	author := GetStringDefault(args, "author", "")
	grepPattern := GetStringDefault(args, "grep", "")

	// Build git log command
	cmdArgs := []string{"log", fmt.Sprintf("-n%d", count)}

	if oneline {
		cmdArgs = append(cmdArgs, "--oneline")
	} else {
		cmdArgs = append(cmdArgs, "--format=%h %s (%an, %ar)")
	}

	if since != "" {
		cmdArgs = append(cmdArgs, "--since="+since)
	}

	if author != "" {
		cmdArgs = append(cmdArgs, "--author="+author)
	}

	if grepPattern != "" {
		cmdArgs = append(cmdArgs, "--grep="+grepPattern)
	}

	if file != "" {
		// Make path relative to workDir if absolute
		if filepath.IsAbs(file) {
			if rel, err := filepath.Rel(t.workDir, file); err == nil {
				file = rel
			}
		}
		cmdArgs = append(cmdArgs, "--follow", "--", file)
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return NewErrorResult(fmt.Sprintf("git log failed: %s", string(exitErr.Stderr))), nil
		}
		return NewErrorResult(fmt.Sprintf("git log failed: %s", err)), nil
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		return NewSuccessResult("No commits found matching the criteria."), nil
	}

	return NewSuccessResult(result), nil
}
