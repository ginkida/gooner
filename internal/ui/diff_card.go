package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	diffmatchpatch "github.com/sergi/go-diff/diffmatchpatch"
)

// diffCardMinWidth is the lower bound below which the inline diff card is
// omitted. The full-screen DiffPreviewModel still opens — the inline card
// is purely a chat-stream record. Below ~60 cols its rounded border eats
// more horizontal real estate than it gives back.
const diffCardMinWidth = 60

// renderInlineDiffCard produces a compact one-shot record of a proposed
// diff for emission into the chat history. Mockup scene C style, scoped
// to a summary-only variant: tool + path + +N/-M counts + how to interact.
// The full preview lives behind `d` (the existing DiffPreviewModel).
//
// Returns "" when the terminal is too narrow.
func renderInlineDiffCard(width int, msg DiffPreviewRequestMsg) string {
	if width < diffCardMinWidth {
		return ""
	}

	displayOld := visibleTerminalControlText(msg.OldContent)
	displayNew := visibleTerminalControlText(msg.NewContent)
	adds, dels := countDiffLines(displayOld, displayNew)

	toolLabel := safeKeyEntryText(msg.ToolName)
	if toolLabel == "" {
		toolLabel = "edit"
	}
	if msg.IsNewFile {
		toolLabel = "create"
	}

	// Title row: "▸ edit  path"   →  right-aligned pill "+N / −M".
	arrowStyle := lipgloss.NewStyle().Foreground(ColorPrimary).Bold(true)
	toolStyle := lipgloss.NewStyle().Foreground(ColorPrimary).Bold(true)
	pathStyle := lipgloss.NewStyle().Foreground(ColorText)

	displayPath := safeKeyEntryText(msg.FilePath)
	prettyPathStr := prettyPath(displayPath)
	if prettyPathStr == "" {
		prettyPathStr = displayPath
	}
	// Card chrome (rounded border + 1-cell hpadding) takes 4 cols total.
	innerWidth := width - 4
	pathBudget := max(innerWidth-18-12, 20) // tool + spaces + pill estimate
	prettyPathStr = shortenPath(prettyPathStr, pathBudget)

	titleLeft := arrowStyle.Render("▸") + " " +
		toolStyle.Render(toolLabel) + " " +
		pathStyle.Render(prettyPathStr)
	pill := renderDiffCountPill(adds, dels)

	pad := max(innerWidth-lipgloss.Width(titleLeft)-lipgloss.Width(pill), 1)
	title := titleLeft + strings.Repeat(" ", pad) + pill

	// Diff preview: a few representative changed lines so users can
	// recognise the change without opening the full modal. Truncates per
	// line; falls back to "no preview" (empty slice) when the diff is
	// effectively whitespace-only after normalisation.
	preview := extractDiffPreviewLines(displayOld, displayNew, 3, innerWidth-2)

	// Action hint row: subtle pills for ↵ / d / esc.
	hint := renderDiffActionHints()

	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1).
		Width(innerWidth)

	var body string
	if len(preview) > 0 {
		body = title + "\n\n" + strings.Join(preview, "\n") + "\n\n" + hint
	} else {
		body = title + "\n\n" + hint
	}
	return cardStyle.Render(body)
}

// extractDiffPreviewLines walks the line-mode diff and returns up to
// maxLines styled preview rows. Picks first deletions then first
// additions — the most contextually relevant slice for a small card.
// Each row is pre-styled with the "+ " / "- " prefix in Success / Error
// and truncated to lineWidth so the card doesn't break the border.
func extractDiffPreviewLines(oldContent, newContent string, maxLines, lineWidth int) []string {
	dmp := diffmatchpatch.New()
	a, b, lineArray := dmp.DiffLinesToChars(oldContent, newContent)
	diffs := dmp.DiffMain(a, b, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	addStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
	delStyle := lipgloss.NewStyle().Foreground(ColorError)

	// Split the budget so we always show at least one of each side when
	// available. With maxLines=3: dels gets 1, adds gets 2 (slight bias
	// toward the after-state, which is what the user is approving).
	delsBudget := max(maxLines/3, 1)
	addsBudget := maxLines - delsBudget

	collect := func(typ diffmatchpatch.Operation, budget int, prefix string, style lipgloss.Style) []string {
		var out []string
		for _, d := range diffs {
			if d.Type != typ {
				continue
			}
			for line := range strings.SplitSeq(strings.TrimSuffix(d.Text, "\n"), "\n") {
				if len(out) >= budget {
					return out
				}
				if strings.TrimSpace(line) == "" {
					continue
				}
				out = append(out, style.Render(prefix+truncateSafeForWidth(line, lineWidth-len(prefix))))
			}
		}
		return out
	}

	dels := collect(diffmatchpatch.DiffDelete, delsBudget, "- ", delStyle)
	adds := collect(diffmatchpatch.DiffInsert, addsBudget, "+ ", addStyle)

	// Show deletions first (what's leaving), then additions (what's
	// arriving). Reads as a tiny before/after.
	return append(dels, adds...)
}

// countDiffLines returns the rough added / removed line counts between two
// content strings. Uses go-diff's line-mode helper so the numbers match
// what the full-screen DiffPreviewModel would show. Cheap enough to call
// on every diff-preview request even for moderately large files.
func countDiffLines(oldContent, newContent string) (adds, dels int) {
	dmp := diffmatchpatch.New()
	a, b, lineArray := dmp.DiffLinesToChars(oldContent, newContent)
	diffs := dmp.DiffMain(a, b, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	for _, d := range diffs {
		// Strip the trailing newline that DiffCharsToLines re-injects so a
		// "single line, no trailing newline" chunk doesn't count as zero.
		lines := strings.Split(strings.TrimSuffix(d.Text, "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			continue
		}
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			adds += len(lines)
		case diffmatchpatch.DiffDelete:
			dels += len(lines)
		}
	}
	return adds, dels
}

// renderDiffCountPill returns a "+N / −M" indicator styled with success +
// error tokens. Skips a side when its count is zero so a pure addition
// renders as "+N" and a pure deletion as "−M" — avoids the visual lie of
// "+22 / −0" reading like "and zero deletions".
func renderDiffCountPill(adds, dels int) string {
	addStyle := lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true)
	delStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	sepStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var parts []string
	if adds > 0 {
		parts = append(parts, addStyle.Render(fmt.Sprintf("+%d", adds)))
	}
	if dels > 0 {
		parts = append(parts, delStyle.Render(fmt.Sprintf("−%d", dels)))
	}
	if len(parts) == 0 {
		// Both zero — touched file content was identical post-normalisation.
		// Surface that explicitly rather than render an empty pill.
		return lipgloss.NewStyle().Foreground(ColorDim).Render("no change")
	}
	return strings.Join(parts, sepStyle.Render(" / "))
}

// renderDiffActionHints returns the muted "↵ accept · d full diff · esc
// reject" hint line shown inside the inline diff card. Keystroke labels
// pop in highlight; verbs sit in muted body text so the eye lands on
// keys first.
func renderDiffActionHints() string {
	keyStyle := lipgloss.NewStyle().Foreground(ColorHighlight).Bold(true)
	verbStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	acceptStyle := lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true)
	rejectStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	sepStyle := lipgloss.NewStyle().Foreground(ColorDim)

	// Keys must match the diff_preview modal: y/Y accept, n/N/esc reject,
	// A apply-all, R reject-all. There is no Enter-accept or "d full diff".
	parts := []string{
		acceptStyle.Render("y accept"),
		rejectStyle.Render("n reject"),
		keyStyle.Render("A/R") + " " + verbStyle.Render("all"),
	}
	return strings.Join(parts, sepStyle.Render("  ·  "))
}
