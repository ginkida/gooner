package commands

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommitCommand creates a git commit.
type CommitCommand struct{}

func (c *CommitCommand) Name() string        { return "commit" }
func (c *CommitCommand) Description() string { return "Create a git commit" }
func (c *CommitCommand) Usage() string       { return "/commit [-m message]" }
func (c *CommitCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category:    CategoryGit,
		Icon:        "commit",
		Priority:    10,
		RequiresGit: true,
		HasArgs:     true,
		ArgHint:     "-m \"msg\"",
	}
}

func (c *CommitCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	workDir := app.GetWorkDir()

	// Check if we're in a git repository
	if !isGitRepo(workDir) {
		return "Not a git repository.", nil
	}

	// Parse arguments
	var message string
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			message = args[i+1]
			i++
		}
	}

	// Get git status
	status, err := runGitCommand(workDir, "status", "--porcelain")
	if err != nil {
		return fmt.Sprintf("Failed to get git status: %v", err), nil
	}

	if strings.TrimSpace(status) == "" {
		return "No changes to commit.", nil
	}

	// Show what will be committed
	var result strings.Builder
	result.WriteString("Changes to commit:\n")

	// Get staged changes
	staged, _ := runGitCommand(workDir, "diff", "--cached", "--stat")
	if staged != "" {
		result.WriteString("\nStaged:\n")
		result.WriteString(staged)
	}

	// Get unstaged changes
	unstaged, _ := runGitCommand(workDir, "diff", "--stat")
	if unstaged != "" {
		result.WriteString("\nUnstaged:\n")
		result.WriteString(unstaged)
	}

	// Get untracked files
	untracked := getUntrackedFiles(status)
	if len(untracked) > 0 {
		result.WriteString("\nUntracked files:\n")
		for _, f := range untracked {
			result.WriteString(fmt.Sprintf("  %s\n", f))
		}
	}

	// If no message provided, we just show the status and ask user to provide message
	if message == "" {
		// Get recent commits for style reference
		log, _ := runGitCommand(workDir, "log", "-3", "--oneline")
		if log != "" {
			result.WriteString("\nRecent commits (for style reference):\n")
			result.WriteString(log)
		}

		result.WriteString("\nUse /commit -m \"your message\" to commit these changes.")
		return result.String(), nil
	}

	// Stage all changes
	_, err = runGitCommand(workDir, "add", "-A")
	if err != nil {
		return fmt.Sprintf("Failed to stage changes: %v", err), nil
	}

	// Create commit
	_, err = runGitCommand(workDir, "commit", "-m", message)
	if err != nil {
		return fmt.Sprintf("Failed to create commit: %v", err), nil
	}

	// Get the commit hash
	hash, _ := runGitCommand(workDir, "rev-parse", "--short", "HEAD")
	hash = strings.TrimSpace(hash)

	return fmt.Sprintf("Created commit %s: %s", hash, message), nil
}

// PRCommand creates a pull request.
type PRCommand struct{}

func (c *PRCommand) Name() string        { return "pr" }
func (c *PRCommand) Description() string { return "Create a pull request" }
func (c *PRCommand) Usage() string       { return "/pr [--title title] [--draft] [--base branch]" }
func (c *PRCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category:    CategoryGit,
		Icon:        "pr",
		Priority:    20,
		RequiresGit: true,
		HasArgs:     true,
		ArgHint:     "--title \"...\"",
	}
}

func (c *PRCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	workDir := app.GetWorkDir()

	// Check if we're in a git repository
	if !isGitRepo(workDir) {
		return "Not a git repository.", nil
	}

	// Check if gh CLI is available
	if !isGHAvailable() {
		return "GitHub CLI (gh) is not installed. Install it from https://cli.github.com/", nil
	}

	// Parse arguments
	var title, base string
	var draft bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--title", "-t":
			if i+1 < len(args) {
				title = args[i+1]
				i++
			}
		case "--base", "-b":
			if i+1 < len(args) {
				base = args[i+1]
				i++
			}
		case "--draft", "-d":
			draft = true
		}
	}

	// Get current branch
	currentBranch, err := runGitCommand(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Sprintf("Failed to get current branch: %v", err), nil
	}
	currentBranch = strings.TrimSpace(currentBranch)

	// Check if we're on main/master
	if currentBranch == "main" || currentBranch == "master" {
		return fmt.Sprintf("Cannot create PR from %s branch. Create a feature branch first.", currentBranch), nil
	}

	// Determine base branch
	if base == "" {
		base = detectBaseBranch(workDir)
	}

	// Get commits for this branch
	commits, _ := runGitCommand(workDir, "log", fmt.Sprintf("%s..HEAD", base), "--oneline")
	if strings.TrimSpace(commits) == "" {
		return fmt.Sprintf("No commits to create PR. Branch is up to date with %s.", base), nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Branch: %s -> %s\n\n", currentBranch, base))
	result.WriteString("Commits:\n")
	result.WriteString(commits)
	result.WriteString("\n")

	// Check if there are unpushed commits
	unpushed, _ := runGitCommand(workDir, "log", "@{upstream}..HEAD", "--oneline")
	if strings.TrimSpace(unpushed) != "" {
		result.WriteString("\nNote: You have unpushed commits. Push them first with: git push -u origin " + currentBranch)
		return result.String(), nil
	}

	// If no title, show info and ask for title
	if title == "" {
		result.WriteString("\nUse /pr --title \"Your PR title\" to create the pull request.")
		return result.String(), nil
	}

	// Build gh pr create command
	ghArgs := []string{"pr", "create", "--title", title, "--base", base}
	if draft {
		ghArgs = append(ghArgs, "--draft")
	}

	// Generate body from commits
	body := fmt.Sprintf("## Summary\n\nThis PR includes the following changes:\n\n%s", formatCommitsAsMarkdown(commits))
	ghArgs = append(ghArgs, "--body", body)

	// Create PR
	output, err := runCommand(workDir, "gh", ghArgs...)
	if err != nil {
		return fmt.Sprintf("Failed to create PR: %v\n%s", err, output), nil
	}

	return fmt.Sprintf("Pull request created: %s", strings.TrimSpace(output)), nil
}

// Helper functions

func isGitRepo(dir string) bool {
	_, err := runGitCommand(dir, "rev-parse", "--git-dir")
	return err == nil
}

func isGHAvailable() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

func runGitCommand(dir string, args ...string) (string, error) {
	return runCommand(dir, "git", args...)
}

func runCommand(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stderr.String(), err
	}

	return stdout.String(), nil
}

func getUntrackedFiles(status string) []string {
	var files []string
	lines := strings.Split(status, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "??") {
			file := strings.TrimPrefix(line, "?? ")
			files = append(files, file)
		}
	}
	return files
}

func detectBaseBranch(dir string) string {
	// Check if main exists
	_, err := runGitCommand(dir, "rev-parse", "--verify", "main")
	if err == nil {
		return "main"
	}

	// Fall back to master
	_, err = runGitCommand(dir, "rev-parse", "--verify", "master")
	if err == nil {
		return "master"
	}

	// Default to main
	return "main"
}

func formatCommitsAsMarkdown(commits string) string {
	var result strings.Builder
	lines := strings.Split(strings.TrimSpace(commits), "\n")
	for _, line := range lines {
		if line != "" {
			result.WriteString(fmt.Sprintf("- %s\n", line))
		}
	}
	return result.String()
}
