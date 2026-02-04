package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// GitContext contains git repository context for prompt injection.
type GitContext struct {
	Branch         string
	RecentCommits  []CommitInfo
	StagedFiles    []string
	ModifiedFiles  []string
	UntrackedFiles []string
	Stashes        int
	RemoteBranch   string
	AheadBehind    AheadBehind
}

// CommitInfo represents information about a git commit.
type CommitInfo struct {
	Hash    string
	Author  string
	Subject string
	Date    time.Time
}

// AheadBehind tracks how many commits ahead/behind remote.
type AheadBehind struct {
	Ahead  int
	Behind int
}

// GitContextProvider provides git context for AI prompts.
type GitContextProvider struct {
	workDir    string
	maxCommits int
	cache      *GitContext
	cacheTime  time.Time
	cacheTTL   time.Duration
	mu         sync.RWMutex
}

// NewGitContextProvider creates a new git context provider.
func NewGitContextProvider(workDir string) *GitContextProvider {
	return &GitContextProvider{
		workDir:    workDir,
		maxCommits: 5,
		cacheTTL:   10 * time.Second,
	}
}

// SetMaxCommits sets the maximum number of recent commits to include.
func (p *GitContextProvider) SetMaxCommits(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxCommits = n
}

// GetContext returns the current git context.
func (p *GitContextProvider) GetContext(ctx context.Context) (*GitContext, error) {
	p.mu.RLock()
	if p.cache != nil && time.Since(p.cacheTime) < p.cacheTTL {
		cache := p.cache
		p.mu.RUnlock()
		return cache, nil
	}
	p.mu.RUnlock()

	// Refresh context
	gc, err := p.refresh(ctx)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.cache = gc
	p.cacheTime = time.Now()
	p.mu.Unlock()

	return gc, nil
}

// refresh fetches fresh git context.
func (p *GitContextProvider) refresh(ctx context.Context) (*GitContext, error) {
	gc := &GitContext{}

	// Check if git repo
	if !IsGitRepo(p.workDir) {
		return nil, fmt.Errorf("not a git repository")
	}

	// Get current branch
	branch, err := p.runGit(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("failed to get branch: %w", err)
	}
	gc.Branch = strings.TrimSpace(branch)

	// Get remote tracking branch
	remote, _ := p.runGit(ctx, "rev-parse", "--abbrev-ref", "@{upstream}")
	gc.RemoteBranch = strings.TrimSpace(remote)

	// Get ahead/behind counts
	if gc.RemoteBranch != "" {
		aheadBehind, _ := p.runGit(ctx, "rev-list", "--left-right", "--count", gc.Branch+"..."+gc.RemoteBranch)
		parts := strings.Fields(strings.TrimSpace(aheadBehind))
		if len(parts) == 2 {
			fmt.Sscanf(parts[0], "%d", &gc.AheadBehind.Ahead)
			fmt.Sscanf(parts[1], "%d", &gc.AheadBehind.Behind)
		}
	}

	// Get recent commits
	gc.RecentCommits = p.getRecentCommits(ctx, p.maxCommits)

	// Get status
	p.parseStatus(ctx, gc)

	// Count stashes
	stashList, _ := p.runGit(ctx, "stash", "list")
	if stashList != "" {
		gc.Stashes = len(strings.Split(strings.TrimSpace(stashList), "\n"))
	}

	return gc, nil
}

// getRecentCommits retrieves the n most recent commits.
func (p *GitContextProvider) getRecentCommits(ctx context.Context, n int) []CommitInfo {
	output, err := p.runGit(ctx, "log", "--format=%H|%an|%at|%s", fmt.Sprintf("-n%d", n))
	if err != nil {
		return nil
	}

	var commits []CommitInfo
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}

		var timestamp int64
		fmt.Sscanf(parts[2], "%d", &timestamp)

		// Shorten hash to 8 chars
		hash := parts[0]
		if len(hash) > 8 {
			hash = hash[:8]
		}

		commits = append(commits, CommitInfo{
			Hash:    hash,
			Author:  parts[1],
			Date:    time.Unix(timestamp, 0),
			Subject: parts[3],
		})
	}

	return commits
}

// parseStatus parses git status output.
func (p *GitContextProvider) parseStatus(ctx context.Context, gc *GitContext) {
	output, err := p.runGit(ctx, "status", "--porcelain")
	if err != nil {
		return
	}

	for _, line := range strings.Split(output, "\n") {
		if len(line) < 3 {
			continue
		}

		indexStatus := line[0]
		workTreeStatus := line[1]
		file := strings.TrimSpace(line[3:])

		// Handle renamed files (format: "R  old -> new")
		if strings.Contains(file, " -> ") {
			parts := strings.Split(file, " -> ")
			if len(parts) == 2 {
				file = parts[1]
			}
		}

		// Staged files (index has changes)
		switch indexStatus {
		case 'M', 'A', 'D', 'R', 'C':
			gc.StagedFiles = append(gc.StagedFiles, file)
		}

		// Modified in work tree
		switch workTreeStatus {
		case 'M':
			gc.ModifiedFiles = append(gc.ModifiedFiles, file)
		case '?':
			gc.UntrackedFiles = append(gc.UntrackedFiles, file)
		}
	}
}

// GetBlameContext gets blame information for a specific file and line range.
func (p *GitContextProvider) GetBlameContext(ctx context.Context, file string, startLine, endLine int) ([]BlameInfo, error) {
	args := []string{"blame", "-L", fmt.Sprintf("%d,%d", startLine, endLine), "--porcelain", file}
	output, err := p.runGit(ctx, args...)
	if err != nil {
		return nil, err
	}

	return parseBlameOutput(output), nil
}

// BlameInfo contains blame information for a line.
type BlameInfo struct {
	Hash   string
	Author string
	Line   int
	Text   string
}

// parseBlameOutput parses git blame porcelain output.
func parseBlameOutput(output string) []BlameInfo {
	var result []BlameInfo
	lines := strings.Split(output, "\n")

	var current BlameInfo
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		// Commit hash line (40 hex chars + line numbers)
		if len(line) >= 40 && isHex(line[:40]) {
			if current.Hash != "" {
				result = append(result, current)
			}
			current = BlameInfo{Hash: line[:8]} // Short hash
			fmt.Sscanf(line[41:], "%d", &current.Line)
		} else if strings.HasPrefix(line, "author ") {
			current.Author = strings.TrimPrefix(line, "author ")
		} else if strings.HasPrefix(line, "\t") {
			current.Text = strings.TrimPrefix(line, "\t")
		}
	}

	if current.Hash != "" {
		result = append(result, current)
	}

	return result
}

// isHex checks if a string is hexadecimal.
func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// runGit executes a git command and returns its output.
func (p *GitContextProvider) runGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = p.workDir
	output, err := cmd.Output()
	return string(output), err
}

// FormatForPrompt formats the git context for inclusion in AI prompts.
func (gc *GitContext) FormatForPrompt() string {
	if gc == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Git Context\n\n")

	// Branch info
	sb.WriteString(fmt.Sprintf("**Branch:** `%s`", gc.Branch))
	if gc.RemoteBranch != "" {
		sb.WriteString(fmt.Sprintf(" (tracking `%s`)", gc.RemoteBranch))
	}
	sb.WriteString("\n")

	// Ahead/behind
	if gc.AheadBehind.Ahead > 0 || gc.AheadBehind.Behind > 0 {
		var parts []string
		if gc.AheadBehind.Ahead > 0 {
			parts = append(parts, fmt.Sprintf("%d ahead", gc.AheadBehind.Ahead))
		}
		if gc.AheadBehind.Behind > 0 {
			parts = append(parts, fmt.Sprintf("%d behind", gc.AheadBehind.Behind))
		}
		sb.WriteString(fmt.Sprintf("**Remote:** %s\n", strings.Join(parts, ", ")))
	}

	// Working tree status
	hasChanges := len(gc.StagedFiles) > 0 || len(gc.ModifiedFiles) > 0 || len(gc.UntrackedFiles) > 0
	if hasChanges {
		sb.WriteString("\n**Working Tree:**\n")
		if len(gc.StagedFiles) > 0 {
			sb.WriteString(fmt.Sprintf("- Staged: %d file(s)\n", len(gc.StagedFiles)))
			for _, f := range limitList(gc.StagedFiles, 5) {
				sb.WriteString(fmt.Sprintf("  - `%s`\n", f))
			}
		}
		if len(gc.ModifiedFiles) > 0 {
			sb.WriteString(fmt.Sprintf("- Modified: %d file(s)\n", len(gc.ModifiedFiles)))
			for _, f := range limitList(gc.ModifiedFiles, 5) {
				sb.WriteString(fmt.Sprintf("  - `%s`\n", f))
			}
		}
		if len(gc.UntrackedFiles) > 0 {
			sb.WriteString(fmt.Sprintf("- Untracked: %d file(s)\n", len(gc.UntrackedFiles)))
		}
	}

	// Stashes
	if gc.Stashes > 0 {
		sb.WriteString(fmt.Sprintf("\n**Stashes:** %d\n", gc.Stashes))
	}

	// Recent commits
	if len(gc.RecentCommits) > 0 {
		sb.WriteString("\n**Recent Commits:**\n")
		for _, c := range gc.RecentCommits {
			ago := formatTimeAgo(c.Date)
			sb.WriteString(fmt.Sprintf("- `%s` %s (%s, %s)\n", c.Hash, c.Subject, c.Author, ago))
		}
	}

	return sb.String()
}

// FormatCompact returns a compact one-line summary.
func (gc *GitContext) FormatCompact() string {
	if gc == nil {
		return ""
	}

	parts := []string{gc.Branch}

	// Changes count
	changes := len(gc.StagedFiles) + len(gc.ModifiedFiles)
	if changes > 0 {
		parts = append(parts, fmt.Sprintf("%d changes", changes))
	}

	// Ahead/behind
	if gc.AheadBehind.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("↑%d", gc.AheadBehind.Ahead))
	}
	if gc.AheadBehind.Behind > 0 {
		parts = append(parts, fmt.Sprintf("↓%d", gc.AheadBehind.Behind))
	}

	return strings.Join(parts, " | ")
}

// limitList returns at most n items from a list.
func limitList(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[:n]
}

// formatTimeAgo formats a time as relative duration.
func formatTimeAgo(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		weeks := int(d.Hours() / 24 / 7)
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}
}

// InvalidateCache clears the cached context.
func (p *GitContextProvider) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache = nil
}

// IsClean returns true if the working tree has no changes.
func (gc *GitContext) IsClean() bool {
	return len(gc.StagedFiles) == 0 && len(gc.ModifiedFiles) == 0 && len(gc.UntrackedFiles) == 0
}
