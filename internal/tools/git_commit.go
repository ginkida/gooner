package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/genai"
)

// GitCommitTool creates git commits.
type GitCommitTool struct {
	workDir string
}

// NewGitCommitTool creates a new GitCommitTool instance.
func NewGitCommitTool(workDir string) *GitCommitTool {
	return &GitCommitTool{
		workDir: workDir,
	}
}

func (t *GitCommitTool) Name() string {
	return "git_commit"
}

func (t *GitCommitTool) Description() string {
	return "Records changes to the repository with a commit message."
}

func (t *GitCommitTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"message": {
					Type:        genai.TypeString,
					Description: "The commit message",
				},
				"all": {
					Type:        genai.TypeBoolean,
					Description: "If true, automatically stage modified and deleted files (equivalent to git commit -a)",
				},
				"amend": {
					Type:        genai.TypeBoolean,
					Description: "If true, amend the previous commit instead of creating a new one",
				},
			},
			Required: []string{"message"},
		},
	}
}

func (t *GitCommitTool) Validate(args map[string]any) error {
	message, ok := GetString(args, "message")
	if !ok || strings.TrimSpace(message) == "" {
		return NewValidationError("message", "is required and cannot be empty")
	}

	return nil
}

func (t *GitCommitTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	message, _ := GetString(args, "message")
	all := GetBoolDefault(args, "all", false)
	amend := GetBoolDefault(args, "amend", false)

	// Build git commit command
	cmdArgs := []string{"commit"}

	if all {
		cmdArgs = append(cmdArgs, "-a")
	}

	if amend {
		cmdArgs = append(cmdArgs, "--amend")
	}

	cmdArgs = append(cmdArgs, "-m", message)

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		// Check for common issues
		if strings.Contains(outStr, "nothing to commit") {
			return NewErrorResult("nothing to commit, working tree clean"), nil
		}
		if strings.Contains(outStr, "no changes added") {
			return NewErrorResult("no changes added to commit (use git_add first or set all=true)"), nil
		}
		return NewErrorResult(fmt.Sprintf("git commit failed: %s\n%s", err, outStr)), nil
	}

	result := strings.TrimSpace(string(output))

	// Get the commit hash for the response
	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	hashCmd.Dir = t.workDir
	if hashOutput, err := hashCmd.Output(); err == nil {
		hash := strings.TrimSpace(string(hashOutput))
		if amend {
			result = fmt.Sprintf("Amended commit %s: %s", hash, message)
		} else {
			result = fmt.Sprintf("Created commit %s: %s", hash, message)
		}
	}

	return NewSuccessResult(result), nil
}
