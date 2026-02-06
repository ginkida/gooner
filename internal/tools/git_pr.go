package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/genai"
)

// GitPRTool creates and manages pull requests via gh CLI.
type GitPRTool struct {
	workDir string
}

// NewGitPRTool creates a new GitPRTool instance.
func NewGitPRTool(workDir string) *GitPRTool {
	return &GitPRTool{workDir: workDir}
}

func (t *GitPRTool) Name() string { return "git_pr" }

func (t *GitPRTool) Description() string {
	return "Creates and manages GitHub pull requests using gh CLI. Supports auto-generating PR descriptions from commit history."
}

func (t *GitPRTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "PR action: 'create', 'list', 'view', 'checks', 'merge', 'close'",
					Enum:        []string{"create", "list", "view", "checks", "merge", "close"},
				},
				"title": {
					Type:        genai.TypeString,
					Description: "PR title (for create). If empty with auto_description=true, will be auto-generated.",
				},
				"body": {
					Type:        genai.TypeString,
					Description: "PR body/description (for create)",
				},
				"base": {
					Type:        genai.TypeString,
					Description: "Base branch for PR (default: main/master)",
				},
				"draft": {
					Type:        genai.TypeBoolean,
					Description: "Create as draft PR",
				},
				"pr_number": {
					Type:        genai.TypeString,
					Description: "PR number (for view, checks, merge, close)",
				},
				"auto_description": {
					Type:        genai.TypeBoolean,
					Description: "Auto-generate PR title and description from commits (default: false)",
				},
			},
			Required: []string{"action"},
		},
	}
}

func (t *GitPRTool) Validate(args map[string]any) error {
	action, ok := GetString(args, "action")
	if !ok || action == "" {
		return NewValidationError("action", "is required")
	}

	switch action {
	case "create":
		// Title can be auto-generated
	case "view", "checks", "merge", "close":
		pr, _ := GetString(args, "pr_number")
		if pr == "" {
			return NewValidationError("pr_number", "is required for "+action)
		}
	case "list":
		// no extra params
	}

	return nil
}

func (t *GitPRTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	// Check if gh is available
	if _, err := exec.LookPath("gh"); err != nil {
		return NewErrorResult("gh CLI is not installed. Install it from https://cli.github.com/"), nil
	}

	action, _ := GetString(args, "action")

	switch action {
	case "create":
		return t.createPR(ctx, args)
	case "list":
		return t.listPRs(ctx, args)
	case "view":
		return t.viewPR(ctx, args)
	case "checks":
		return t.checksPR(ctx, args)
	case "merge":
		return t.mergePR(ctx, args)
	case "close":
		return t.closePR(ctx, args)
	default:
		return NewErrorResult(fmt.Sprintf("unknown action: %s", action)), nil
	}
}

func (t *GitPRTool) createPR(ctx context.Context, args map[string]any) (ToolResult, error) {
	title, _ := GetString(args, "title")
	body, _ := GetString(args, "body")
	base := GetStringDefault(args, "base", "")
	draft := GetBoolDefault(args, "draft", false)
	autoDesc := GetBoolDefault(args, "auto_description", false)

	// Auto-generate title and body from commits
	if autoDesc || (title == "" && body == "") {
		generatedTitle, generatedBody := t.generatePRDescription(ctx, base)
		if title == "" {
			title = generatedTitle
		}
		if body == "" {
			body = generatedBody
		}
	}

	if title == "" {
		return NewErrorResult("title is required (or use auto_description=true)"), nil
	}

	// Ensure branch is pushed
	pushCmd := exec.CommandContext(ctx, "git", "push", "-u", "origin", "HEAD")
	pushCmd.Dir = t.workDir
	pushOutput, err := pushCmd.CombinedOutput()
	if err != nil {
		outStr := string(pushOutput)
		if !strings.Contains(outStr, "Everything up-to-date") {
			return NewErrorResult(fmt.Sprintf("failed to push branch: %s\n%s", err, outStr)), nil
		}
	}

	// Build gh pr create command
	cmdArgs := []string{"pr", "create", "--title", title}
	if body != "" {
		cmdArgs = append(cmdArgs, "--body", body)
	}
	if base != "" {
		cmdArgs = append(cmdArgs, "--base", base)
	}
	if draft {
		cmdArgs = append(cmdArgs, "--draft")
	}

	cmd := exec.CommandContext(ctx, "gh", cmdArgs...)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create PR: %s\n%s", err, string(output))), nil
	}

	return NewSuccessResult(fmt.Sprintf("Pull request created:\n%s", strings.TrimSpace(string(output)))), nil
}

func (t *GitPRTool) listPRs(ctx context.Context, _ map[string]any) (ToolResult, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--limit", "20")
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to list PRs: %s\n%s", err, string(output))), nil
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		return NewSuccessResult("No open pull requests."), nil
	}
	return NewSuccessResult(result), nil
}

func (t *GitPRTool) viewPR(ctx context.Context, args map[string]any) (ToolResult, error) {
	prNum, _ := GetString(args, "pr_number")
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to view PR: %s\n%s", err, string(output))), nil
	}
	return NewSuccessResult(strings.TrimSpace(string(output))), nil
}

func (t *GitPRTool) checksPR(ctx context.Context, args map[string]any) (ToolResult, error) {
	prNum, _ := GetString(args, "pr_number")
	cmd := exec.CommandContext(ctx, "gh", "pr", "checks", prNum)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Checks command may exit non-zero if checks are failing
		return NewSuccessResult(fmt.Sprintf("PR #%s checks:\n%s", prNum, strings.TrimSpace(string(output)))), nil
	}
	return NewSuccessResult(fmt.Sprintf("PR #%s checks:\n%s", prNum, strings.TrimSpace(string(output)))), nil
}

func (t *GitPRTool) mergePR(ctx context.Context, args map[string]any) (ToolResult, error) {
	prNum, _ := GetString(args, "pr_number")
	cmd := exec.CommandContext(ctx, "gh", "pr", "merge", prNum, "--merge")
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to merge PR: %s\n%s", err, string(output))), nil
	}
	return NewSuccessResult(fmt.Sprintf("PR #%s merged.\n%s", prNum, strings.TrimSpace(string(output)))), nil
}

func (t *GitPRTool) closePR(ctx context.Context, args map[string]any) (ToolResult, error) {
	prNum, _ := GetString(args, "pr_number")
	cmd := exec.CommandContext(ctx, "gh", "pr", "close", prNum)
	cmd.Dir = t.workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to close PR: %s\n%s", err, string(output))), nil
	}
	return NewSuccessResult(fmt.Sprintf("PR #%s closed.\n%s", prNum, strings.TrimSpace(string(output)))), nil
}

// generatePRDescription creates a PR title and body from commit history.
func (t *GitPRTool) generatePRDescription(ctx context.Context, base string) (string, string) {
	if base == "" {
		base = t.detectDefaultBranch(ctx)
	}

	// Get commits since base
	logCmd := exec.CommandContext(ctx, "git", "log", base+"..HEAD", "--oneline", "--no-decorate")
	logCmd.Dir = t.workDir
	logOutput, err := logCmd.Output()
	if err != nil {
		return "Update", ""
	}

	commits := strings.Split(strings.TrimSpace(string(logOutput)), "\n")
	if len(commits) == 0 || (len(commits) == 1 && commits[0] == "") {
		return "Update", ""
	}

	// Generate title from first commit or summary
	var title string
	if len(commits) == 1 {
		parts := strings.SplitN(commits[0], " ", 2)
		if len(parts) >= 2 {
			title = parts[1]
		} else {
			title = commits[0]
		}
	} else {
		// Multiple commits - summarize
		title = fmt.Sprintf("Update: %d changes", len(commits))
		// Try to find a common theme
		if firstParts := strings.SplitN(commits[0], " ", 2); len(firstParts) >= 2 {
			first := firstParts[1]
			if len(first) <= 70 {
				title = first
			}
		}
	}

	// Truncate title
	if len(title) > 70 {
		title = title[:67] + "..."
	}

	// Generate body
	var body strings.Builder
	body.WriteString("## Summary\n\n")

	// Get diff stats
	statCmd := exec.CommandContext(ctx, "git", "diff", "--stat", base+"..HEAD")
	statCmd.Dir = t.workDir
	statOutput, _ := statCmd.Output()

	// List commits as bullet points
	for _, commit := range commits {
		parts := strings.SplitN(commit, " ", 2)
		if len(parts) >= 2 {
			body.WriteString(fmt.Sprintf("- %s\n", parts[1]))
		}
	}

	if len(statOutput) > 0 {
		body.WriteString("\n## Changed files\n\n```\n")
		body.WriteString(strings.TrimSpace(string(statOutput)))
		body.WriteString("\n```\n")
	}

	body.WriteString("\n## Test plan\n\n- [ ] Tests pass\n- [ ] Manual verification\n")

	return title, body.String()
}

// detectDefaultBranch finds the default branch name (main or master).
func (t *GitPRTool) detectDefaultBranch(ctx context.Context) string {
	// Try to detect from remote
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "--short")
	cmd.Dir = t.workDir
	output, err := cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(output))
		// Strip "origin/" prefix
		if idx := strings.Index(branch, "/"); idx >= 0 {
			return branch[idx+1:]
		}
		return branch
	}

	// Fallback: check if main or master exists
	for _, name := range []string{"main", "master"} {
		checkCmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", name)
		checkCmd.Dir = t.workDir
		if err := checkCmd.Run(); err == nil {
			return name
		}
	}

	return "main"
}
