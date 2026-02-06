package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/genai"
)

// GitBranchTool manages git branches.
type GitBranchTool struct {
	workDir string
}

// NewGitBranchTool creates a new GitBranchTool instance.
func NewGitBranchTool(workDir string) *GitBranchTool {
	return &GitBranchTool{workDir: workDir}
}

func (t *GitBranchTool) Name() string { return "git_branch" }

func (t *GitBranchTool) Description() string {
	return "Manages git branches: list, create, delete, switch (checkout), and merge branches."
}

func (t *GitBranchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Branch action: 'list', 'create', 'delete', 'switch', 'merge', 'current'",
					Enum:        []string{"list", "create", "delete", "switch", "merge", "current"},
				},
				"name": {
					Type:        genai.TypeString,
					Description: "Branch name (for create, delete, switch, merge)",
				},
				"from": {
					Type:        genai.TypeString,
					Description: "Base branch to create from (for create, default: current branch)",
				},
				"force": {
					Type:        genai.TypeBoolean,
					Description: "Force delete or force switch (discards local changes)",
				},
				"all": {
					Type:        genai.TypeBoolean,
					Description: "Show remote branches too (for list)",
				},
			},
			Required: []string{"action"},
		},
	}
}

func (t *GitBranchTool) Validate(args map[string]any) error {
	action, ok := GetString(args, "action")
	if !ok || action == "" {
		return NewValidationError("action", "is required")
	}

	switch action {
	case "create", "delete", "switch", "merge":
		name, _ := GetString(args, "name")
		if name == "" {
			return NewValidationError("name", "is required for "+action)
		}
	case "list", "current":
		// no extra params needed
	default:
		return NewValidationError("action", "must be one of: list, create, delete, switch, merge, current")
	}

	return nil
}

func (t *GitBranchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	action, _ := GetString(args, "action")

	switch action {
	case "list":
		return t.listBranches(ctx, args)
	case "create":
		return t.createBranch(ctx, args)
	case "delete":
		return t.deleteBranch(ctx, args)
	case "switch":
		return t.switchBranch(ctx, args)
	case "merge":
		return t.mergeBranch(ctx, args)
	case "current":
		return t.currentBranch(ctx)
	default:
		return NewErrorResult(fmt.Sprintf("unknown action: %s", action)), nil
	}
}

func (t *GitBranchTool) listBranches(ctx context.Context, args map[string]any) (ToolResult, error) {
	cmdArgs := []string{"branch", "--format=%(refname:short) %(objectname:short) %(subject)"}
	if GetBoolDefault(args, "all", false) {
		cmdArgs = append(cmdArgs, "-a")
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("git branch list failed: %s\n%s", err, string(output))), nil
	}

	// Get current branch
	currentCmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
	currentCmd.Dir = t.workDir
	currentOutput, _ := currentCmd.Output()
	current := strings.TrimSpace(string(currentOutput))

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var result strings.Builder
	result.WriteString(fmt.Sprintf("Branches (%d), current: %s\n\n", len(lines), current))

	for _, line := range lines {
		parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(parts) >= 2 {
			name := parts[0]
			hash := parts[1]
			subject := ""
			if len(parts) >= 3 {
				subject = parts[2]
			}
			marker := "  "
			if name == current {
				marker = "* "
			}
			result.WriteString(fmt.Sprintf("%s%s %s %s\n", marker, name, hash, subject))
		}
	}

	return NewSuccessResult(result.String()), nil
}

func (t *GitBranchTool) createBranch(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := GetString(args, "name")
	from, _ := GetString(args, "from")

	cmdArgs := []string{"checkout", "-b", name}
	if from != "" {
		cmdArgs = append(cmdArgs, from)
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create branch: %s\n%s", err, string(output))), nil
	}

	return NewSuccessResult(fmt.Sprintf("Created and switched to branch '%s'", name)), nil
}

func (t *GitBranchTool) deleteBranch(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := GetString(args, "name")
	force := GetBoolDefault(args, "force", false)

	flag := "-d"
	if force {
		flag = "-D"
	}

	cmd := exec.CommandContext(ctx, "git", "branch", flag, name)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "not fully merged") {
			return NewErrorResult(fmt.Sprintf("branch '%s' is not fully merged. Use force=true to delete anyway.", name)), nil
		}
		return NewErrorResult(fmt.Sprintf("failed to delete branch: %s\n%s", err, outStr)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Deleted branch '%s'", name)), nil
}

func (t *GitBranchTool) switchBranch(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := GetString(args, "name")
	force := GetBoolDefault(args, "force", false)

	cmdArgs := []string{"checkout", name}
	if force {
		cmdArgs = []string{"checkout", "-f", name}
	}

	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := string(output)
		if strings.Contains(outStr, "local changes") {
			return NewErrorResult(fmt.Sprintf("cannot switch to '%s': you have local changes. Commit or stash them first, or use force=true.", name)), nil
		}
		return NewErrorResult(fmt.Sprintf("failed to switch branch: %s\n%s", err, outStr)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Switched to branch '%s'", name)), nil
}

func (t *GitBranchTool) mergeBranch(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := GetString(args, "name")

	cmd := exec.CommandContext(ctx, "git", "merge", name)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(output))

	if err != nil {
		if strings.Contains(outStr, "CONFLICT") {
			// Get conflict details
			conflictCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=U")
			conflictCmd.Dir = t.workDir
			conflictOutput, _ := conflictCmd.Output()

			return NewErrorResult(fmt.Sprintf("Merge conflict when merging '%s'.\n\nConflicting files:\n%s\nResolve conflicts and commit, or run 'git merge --abort' to cancel.",
				name, strings.TrimSpace(string(conflictOutput)))), nil
		}
		return NewErrorResult(fmt.Sprintf("merge failed: %s\n%s", err, outStr)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Merged '%s' into current branch.\n%s", name, outStr)), nil
}

func (t *GitBranchTool) currentBranch(ctx context.Context) (ToolResult, error) {
	cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
	cmd.Dir = t.workDir
	output, err := cmd.Output()
	if err != nil {
		// Might be in detached HEAD
		hashCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
		hashCmd.Dir = t.workDir
		hashOutput, hashErr := hashCmd.Output()
		if hashErr != nil {
			return NewErrorResult("not in a git repository"), nil
		}
		return NewSuccessResult(fmt.Sprintf("HEAD detached at %s", strings.TrimSpace(string(hashOutput)))), nil
	}

	branch := strings.TrimSpace(string(output))

	// Also get upstream info
	upstreamCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", branch+"@{upstream}")
	upstreamCmd.Dir = t.workDir
	upstreamOutput, upstreamErr := upstreamCmd.Output()

	result := fmt.Sprintf("Current branch: %s", branch)
	if upstreamErr == nil {
		upstream := strings.TrimSpace(string(upstreamOutput))
		result += fmt.Sprintf("\nTracking: %s", upstream)

		// Check ahead/behind
		abCmd := exec.CommandContext(ctx, "git", "rev-list", "--left-right", "--count", branch+"..."+upstream)
		abCmd.Dir = t.workDir
		abOutput, abErr := abCmd.Output()
		if abErr == nil {
			parts := strings.Fields(strings.TrimSpace(string(abOutput)))
			if len(parts) == 2 {
				result += fmt.Sprintf(" (ahead %s, behind %s)", parts[0], parts[1])
			}
		}
	}

	return NewSuccessResult(result), nil
}
