package commands

import (
	"context"
	"fmt"
	"strings"
)

// DiffCommand shows pending git changes inline. Designed as the
// "what's about to commit" preview that pairs naturally with
// /commit — users hit /diff first to verify scope, then /commit
// when satisfied. Without this, the only way to see pending changes
// in-TUI was to ask the agent ("show me the diff") which costs an
// LLM round trip and is non-deterministic.
//
// Modes (mirrors `git diff` itself):
//
//	/diff             - Working tree (unstaged + staged) summary + content
//	/diff --stat      - Stats only (file list + +/- counts), no content
//	/diff --staged    - Staged-only content (git diff --cached)
//	/diff <file>      - Single-file diff
type DiffCommand struct{}

func (c *DiffCommand) Name() string        { return "diff" }
func (c *DiffCommand) Description() string { return "Show pending git changes" }
func (c *DiffCommand) Usage() string {
	return `/diff             - Show working-tree diff (with content)
/diff --stat      - Stats only (no content)
/diff --staged    - Staged changes only
/diff <file>      - Single file`
}

func (c *DiffCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category:    CategoryGit,
		Icon:        "diff",
		Priority:    11, // sits right after /commit (which is 10)
		RequiresGit: true,
		HasArgs:     true,
		ArgHint:     "[--stat | --staged | <file>]",
	}
}

// diffMaxOutputBytes caps the rendered diff so a sprawling refactor
// doesn't dump 50K lines into the TUI scrollback. The git command
// still ran fully — we just truncate the displayed text and append a
// "... truncated, run `git diff` directly for the full output" tail.
const diffMaxOutputBytes = 32 * 1024

func (c *DiffCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	workDir := app.GetWorkDir()
	if !isGitRepo(workDir) {
		return "Not a git repository.", nil
	}

	statOnly := false
	staged := false
	var filePath string

	// Parse args. Positional file path is allowed alongside flags so
	// `/diff --staged path/to/file.go` works.
	for _, a := range args {
		switch a {
		case "--stat":
			statOnly = true
		case "--staged", "--cached":
			staged = true
		default:
			// Anything else is treated as a file path. Multiple paths
			// would be confusing in this surface — take the last one
			// silently (git accepts the same).
			filePath = a
		}
	}

	// Build git args.
	gitArgs := []string{"diff"}
	if staged {
		gitArgs = append(gitArgs, "--cached")
	}
	if statOnly {
		gitArgs = append(gitArgs, "--stat")
	} else {
		// Color is preserved in raw form; git's `--color` would inject
		// ANSI sequences but we render through the TUI's normal text
		// path which already styles diff blocks via diff_utils. Ask
		// for `--color=never` so we don't double-up.
		gitArgs = append(gitArgs, "--color=never")
	}
	if filePath != "" {
		gitArgs = append(gitArgs, "--", filePath)
	}

	out, err := runGitCommandCtx(ctx, workDir, gitArgs...)
	if err != nil {
		return fmt.Sprintf("Failed to get diff: %v", err), nil
	}
	out = strings.TrimRight(out, "\n")

	// Detect "no changes" state. An empty result means no changes to files
	// git is TRACKING — it does not mean the tree is clean, because `git
	// diff` never reports untracked files. Saying "clean" to someone whose
	// new file is sitting right there is the same false answer as telling
	// them they have no sessions while one is on disk unread.
	if out == "" {
		return c.emptyMessage(ctx, workDir, staged, filePath), nil
	}

	// Always include a one-line stats header at the top so users have
	// at-a-glance scope context before scrolling content. We compute
	// it separately because asking git for both --stat and content in
	// one call requires --stat-only mode or piping to a parser; a
	// second cheap call is simpler and the perf cost is negligible.
	var header string
	if !statOnly {
		statArgs := append([]string{"diff"}, []string{}...)
		if staged {
			statArgs = append(statArgs, "--cached")
		}
		statArgs = append(statArgs, "--stat")
		if filePath != "" {
			statArgs = append(statArgs, "--", filePath)
		}
		stat, _ := runGitCommandCtx(ctx, workDir, statArgs...)
		stat = strings.TrimRight(stat, "\n")
		if stat != "" {
			header = stat + "\n\n"
		}
	}

	body := truncateDiffOutput(out)
	scope := "working tree"
	if staged {
		scope = "staged"
	}
	if filePath != "" {
		scope = filePath
	}

	return fmt.Sprintf("Diff (%s):\n\n%s%s", scope, header, body), nil
}

// emptyMessage tailors the "no changes" copy to whatever the user asked for,
// so `/diff --staged` doesn't claim "working tree is clean" when it's the
// staging area that's empty — and so neither claim is made at all while
// untracked files exist. The staged view is exempt: an untracked file is by
// definition not staged, so "nothing staged" stays true there and its existing
// hint already points at `git add`.
func (c *DiffCommand) emptyMessage(ctx context.Context, workDir string, staged bool, filePath string) string {
	if staged {
		if filePath != "" {
			return fmt.Sprintf("No staged changes in %s.", filePath)
		}
		return "Nothing staged. Run `git add <files>` first, or `/diff` to see unstaged changes."
	}

	untracked, err := untrackedFiles(ctx, workDir, filePath)
	switch {
	case err != nil:
		// Never upgrade a failed check into a clean bill of health.
		return fmt.Sprintf("No changes to tracked files. Could not check for new "+
			"files: %v", err)
	case len(untracked) > 0 && filePath != "":
		return fmt.Sprintf("%s is a new file that git is not tracking yet, so `git diff` "+
			"shows nothing for it — all of its content is new. Run `git add %s` to see it "+
			"in a diff.", filePath, filePath)
	case len(untracked) > 0:
		var b strings.Builder
		fmt.Fprintf(&b, "No changes to tracked files, but %d new file(s) git is not "+
			"tracking yet:\n", len(untracked))
		for i, f := range untracked {
			if i == maxListedUntracked {
				fmt.Fprintf(&b, "  ... and %d more\n", len(untracked)-i)
				break
			}
			fmt.Fprintf(&b, "  - %s\n", f)
		}
		b.WriteString("\nRun `git add <files>` to include them in a diff.")
		return b.String()
	case filePath != "":
		return fmt.Sprintf("No changes in %s.", filePath)
	default:
		return "Working tree is clean."
	}
}

// maxListedUntracked keeps a freshly-scaffolded directory from filling the
// screen; the count above the list stays exact.
const maxListedUntracked = 10

// untrackedFiles lists files git is not tracking, scoped to filePath when one
// was given. core.quotepath=off is load-bearing, not decoration: with the
// default, a non-ASCII name comes back C-quoted, which is how such files
// silently vanish from a listing that is supposed to prove they exist.
func untrackedFiles(ctx context.Context, workDir, filePath string) ([]string, error) {
	args := []string{"-c", "core.quotepath=off", "ls-files", "--others", "--exclude-standard"}
	if filePath != "" {
		args = append(args, "--", filePath)
	}
	out, err := runGitCommandCtx(ctx, workDir, args...)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(out)
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

// truncateDiffOutput caps the body to keep the TUI snappy; long
// outputs append a tail telling the user how to see the full thing.
func truncateDiffOutput(s string) string {
	if len(s) <= diffMaxOutputBytes {
		return s
	}
	cut := s[:utf8SafeByteCut(s, diffMaxOutputBytes)]
	// Cut at the last newline to avoid mid-line slice.
	if idx := strings.LastIndexByte(cut, '\n'); idx > 0 {
		cut = cut[:idx]
	}
	return cut + "\n\n... truncated. Run `git diff` directly for the full output."
}

// Compile-time check.
var _ Command = (*DiffCommand)(nil)
