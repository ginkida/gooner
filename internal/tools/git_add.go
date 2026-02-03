package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/genai"
)

// GitAddTool stages files for git commit.
type GitAddTool struct {
	workDir string
}

// NewGitAddTool creates a new GitAddTool instance.
func NewGitAddTool(workDir string) *GitAddTool {
	return &GitAddTool{
		workDir: workDir,
	}
}

func (t *GitAddTool) Name() string {
	return "git_add"
}

func (t *GitAddTool) Description() string {
	return "Adds file contents to the staging area for the next commit."
}

func (t *GitAddTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"paths": {
					Type:        genai.TypeArray,
					Description: "List of file paths to stage",
					Items: &genai.Schema{
						Type: genai.TypeString,
					},
				},
				"all": {
					Type:        genai.TypeBoolean,
					Description: "If true, stage all changes (equivalent to git add -A)",
				},
				"update": {
					Type:        genai.TypeBoolean,
					Description: "If true, only update tracked files (equivalent to git add -u)",
				},
			},
		},
	}
}

func (t *GitAddTool) Validate(args map[string]any) error {
	// Check that at least one option is specified
	paths, _ := args["paths"].([]any)
	all := GetBoolDefault(args, "all", false)
	update := GetBoolDefault(args, "update", false)

	if len(paths) == 0 && !all && !update {
		return NewValidationError("paths", "at least one of paths, all, or update must be specified")
	}

	return nil
}

func (t *GitAddTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	all := GetBoolDefault(args, "all", false)
	update := GetBoolDefault(args, "update", false)

	// Build git add command
	cmdArgs := []string{"add"}

	if all {
		cmdArgs = append(cmdArgs, "-A")
	} else if update {
		cmdArgs = append(cmdArgs, "-u")
	} else {
		// Get paths
		pathsRaw, ok := args["paths"].([]any)
		if !ok || len(pathsRaw) == 0 {
			return NewErrorResult("no paths specified"), nil
		}

		for _, p := range pathsRaw {
			if path, ok := p.(string); ok && path != "" {
				cmdArgs = append(cmdArgs, path)
			}
		}
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("git add failed: %s\n%s", err, string(output))), nil
	}

	// Get status after add to show what was staged
	statusCmd := exec.CommandContext(ctx, "git", "status", "--short")
	statusCmd.Dir = t.workDir
	statusOutput, _ := statusCmd.Output()

	result := "Files staged for commit."
	if len(statusOutput) > 0 {
		result = fmt.Sprintf("Files staged for commit:\n%s", strings.TrimSpace(string(statusOutput)))
	}

	return NewSuccessResult(result), nil
}
