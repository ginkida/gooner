package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

// GitDiffTool shows differences between commits, branches, or files.
type GitDiffTool struct {
	workDir string
}

// NewGitDiffTool creates a new GitDiffTool instance.
func NewGitDiffTool(workDir string) *GitDiffTool {
	return &GitDiffTool{
		workDir: workDir,
	}
}

func (t *GitDiffTool) Name() string {
	return "git_diff"
}

func (t *GitDiffTool) Description() string {
	return "Shows differences between commits, branches, or working tree. Can show full diff or just file names with status."
}

func (t *GitDiffTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"from": {
					Type:        genai.TypeString,
					Description: "Starting commit/branch (default: HEAD). Use empty string for unstaged changes.",
				},
				"to": {
					Type:        genai.TypeString,
					Description: "Ending commit/branch. Omit to compare with working tree.",
				},
				"name_status": {
					Type:        genai.TypeBoolean,
					Description: "Only show file names and status (A=added, M=modified, D=deleted)",
				},
				"file": {
					Type:        genai.TypeString,
					Description: "Show diff for a specific file only",
				},
				"staged": {
					Type:        genai.TypeBoolean,
					Description: "Show staged changes (--cached). Ignored if from/to are specified.",
				},
			},
		},
	}
}

func (t *GitDiffTool) Validate(args map[string]any) error {
	// All parameters are optional
	return nil
}

func (t *GitDiffTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	from := GetStringDefault(args, "from", "")
	to := GetStringDefault(args, "to", "")
	nameStatus := GetBoolDefault(args, "name_status", false)
	file := GetStringDefault(args, "file", "")
	staged := GetBoolDefault(args, "staged", false)

	// Build git diff command
	cmdArgs := []string{"diff"}

	if nameStatus {
		cmdArgs = append(cmdArgs, "--name-status")
	}

	// Handle different diff modes
	if from != "" && to != "" {
		// Diff between two refs
		cmdArgs = append(cmdArgs, from+".."+to)
	} else if from != "" {
		// Diff from a ref to working tree
		cmdArgs = append(cmdArgs, from)
	} else if staged {
		// Staged changes
		cmdArgs = append(cmdArgs, "--cached")
	}
	// else: unstaged changes (no additional args needed)

	if file != "" {
		// Make path relative to workDir if absolute
		if filepath.IsAbs(file) {
			if rel, err := filepath.Rel(t.workDir, file); err == nil {
				file = rel
			}
		}
		cmdArgs = append(cmdArgs, "--", file)
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return NewErrorResult(fmt.Sprintf("git diff failed: %s", stderr)), nil
			}
		}
		return NewErrorResult(fmt.Sprintf("git diff failed: %s", err)), nil
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		return NewSuccessResult("No differences found."), nil
	}

	// Truncate if output is too large
	if len(result) > 50000 {
		result = result[:50000] + "\n\n... (output truncated)"
	}

	return NewSuccessResult(result), nil
}
