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
	return "Records changes to the repository with a commit message. Supports auto-generating commit messages from staged changes."
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
					Description: "The commit message. If auto_message is true, this is optional.",
				},
				"all": {
					Type:        genai.TypeBoolean,
					Description: "If true, automatically stage modified and deleted files (equivalent to git commit -a)",
				},
				"amend": {
					Type:        genai.TypeBoolean,
					Description: "If true, amend the previous commit instead of creating a new one",
				},
				"auto_message": {
					Type:        genai.TypeBoolean,
					Description: "If true, analyze staged diff and auto-generate a conventional commit message",
				},
			},
		},
	}
}

func (t *GitCommitTool) Validate(args map[string]any) error {
	autoMessage := GetBoolDefault(args, "auto_message", false)
	message, ok := GetString(args, "message")

	if !autoMessage && (!ok || strings.TrimSpace(message) == "") {
		return NewValidationError("message", "is required when auto_message is false")
	}

	return nil
}

func (t *GitCommitTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	message, _ := GetString(args, "message")
	all := GetBoolDefault(args, "all", false)
	amend := GetBoolDefault(args, "amend", false)
	autoMessage := GetBoolDefault(args, "auto_message", false)

	// Auto-generate commit message from diff
	if autoMessage {
		generated, err := t.generateCommitMessage(ctx, all)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("failed to generate commit message: %s", err)), nil
		}
		if message == "" {
			message = generated
		} else {
			// Use provided message as prefix, append generated details
			message = message + "\n\n" + generated
		}
	}

	if strings.TrimSpace(message) == "" {
		return NewErrorResult("commit message is empty"), nil
	}

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

// generateCommitMessage analyzes staged changes and generates a conventional commit message.
func (t *GitCommitTool) generateCommitMessage(ctx context.Context, includeUnstaged bool) (string, error) {
	// Get staged diff (or all diff if --all flag)
	diffArgs := []string{"diff", "--cached", "--stat"}
	if includeUnstaged {
		diffArgs = []string{"diff", "HEAD", "--stat"}
	}

	diffCmd := exec.CommandContext(ctx, "git", diffArgs...)
	diffCmd.Dir = t.workDir
	statOutput, err := diffCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff --stat failed: %w", err)
	}

	statStr := strings.TrimSpace(string(statOutput))
	if statStr == "" {
		return "", fmt.Errorf("no changes to commit")
	}

	// Get detailed diff for analysis (limited to 4000 chars)
	detailArgs := []string{"diff", "--cached"}
	if includeUnstaged {
		detailArgs = []string{"diff", "HEAD"}
	}

	detailCmd := exec.CommandContext(ctx, "git", detailArgs...)
	detailCmd.Dir = t.workDir
	detailOutput, err := detailCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff failed: %w", err)
	}

	diffStr := string(detailOutput)
	if len(diffStr) > 4000 {
		diffStr = diffStr[:4000] + "\n... (truncated)"
	}

	// Analyze the changes
	return t.analyzeChanges(statStr, diffStr), nil
}

// analyzeChanges generates a commit message from diff analysis.
func (t *GitCommitTool) analyzeChanges(stat, diff string) string {
	lines := strings.Split(stat, "\n")

	var files []string
	var totalAdded, totalRemoved int

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "files changed") || strings.Contains(line, "file changed") {
			// Parse summary line
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "insertions(+)" || p == "insertion(+)" {
					if i > 0 {
						fmt.Sscanf(parts[i-1], "%d", &totalAdded)
					}
				}
				if p == "deletions(-)" || p == "deletion(-)" {
					if i > 0 {
						fmt.Sscanf(parts[i-1], "%d", &totalRemoved)
					}
				}
			}
			continue
		}
		// Parse file line: " file.go | 10 ++++---"
		if idx := strings.Index(line, "|"); idx > 0 {
			files = append(files, strings.TrimSpace(line[:idx]))
		}
	}

	// Determine change type
	changeType := detectChangeType(files, diff)

	// Determine scope from file paths
	scope := detectScope(files)

	// Build message
	var msg strings.Builder
	msg.WriteString(changeType)
	if scope != "" {
		msg.WriteString("(" + scope + ")")
	}
	msg.WriteString(": ")

	// Generate description based on analysis
	desc := generateDescription(files, totalAdded, totalRemoved, diff)
	msg.WriteString(desc)

	return msg.String()
}

// detectChangeType determines the conventional commit type from changes.
func detectChangeType(files []string, diff string) string {
	diffLower := strings.ToLower(diff)

	// Check for test files
	allTests := true
	hasTests := false
	for _, f := range files {
		if strings.Contains(f, "_test.go") || strings.Contains(f, "test_") || strings.Contains(f, ".test.") {
			hasTests = true
		} else {
			allTests = false
		}
	}
	if allTests && hasTests {
		return "test"
	}

	// Check for docs
	allDocs := true
	for _, f := range files {
		lower := strings.ToLower(f)
		if !strings.HasSuffix(lower, ".md") && !strings.Contains(lower, "doc") && !strings.Contains(lower, "readme") {
			allDocs = false
			break
		}
	}
	if allDocs {
		return "docs"
	}

	// Check diff content for fix patterns
	if strings.Contains(diffLower, "fix") || strings.Contains(diffLower, "bug") ||
		strings.Contains(diffLower, "error") || strings.Contains(diffLower, "issue") {
		return "fix"
	}

	// Check for refactoring patterns
	if strings.Contains(diffLower, "refactor") || strings.Contains(diffLower, "rename") ||
		strings.Contains(diffLower, "move") || strings.Contains(diffLower, "extract") {
		return "refactor"
	}

	// Check for CI/CD
	for _, f := range files {
		if strings.Contains(f, ".github/") || strings.Contains(f, "ci") || strings.Contains(f, "Dockerfile") {
			return "ci"
		}
	}

	return "feat"
}

// detectScope determines the scope from file paths.
func detectScope(files []string) string {
	if len(files) == 0 {
		return ""
	}

	// Find common directory
	packages := make(map[string]int)
	for _, f := range files {
		parts := strings.Split(f, "/")
		if len(parts) >= 2 {
			// Use the most specific package dir
			pkg := parts[len(parts)-2]
			if pkg == "internal" && len(parts) >= 3 {
				pkg = parts[len(parts)-2]
			}
			packages[pkg]++
		}
	}

	// Return the most common package
	var bestPkg string
	var bestCount int
	for pkg, count := range packages {
		if count > bestCount {
			bestPkg = pkg
			bestCount = count
		}
	}

	return bestPkg
}

// generateDescription creates a human-readable description of changes.
func generateDescription(files []string, added, removed int, _ string) string {
	if len(files) == 1 {
		action := "update"
		if added > 0 && removed == 0 {
			action = "add"
		} else if removed > added {
			action = "remove"
		}
		return fmt.Sprintf("%s %s", action, files[0])
	}

	// Multiple files
	action := "update"
	if added > removed*2 {
		action = "add"
	} else if removed > added*2 {
		action = "remove"
	}

	if len(files) <= 3 {
		return fmt.Sprintf("%s %s", action, strings.Join(files, ", "))
	}

	return fmt.Sprintf("%s %d files (%d+ %d-)", action, len(files), added, removed)
}
