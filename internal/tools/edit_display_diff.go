package tools

import (
	"fmt"
	"strings"

	diffmatchpatch "github.com/sergi/go-diff/diffmatchpatch"

	"gokin/internal/undo"
)

// Display-diff payload for edit results (the Claude-Code "Update(file) ⎿
// Added 9 lines, removed 2 lines + colored -/+ region" shape). Computed by
// the TOOL (the only place that holds both the old and new file contents)
// and carried on ToolResult.Data for the UI — the MODEL-facing Content keeps
// the v0.88 "Updated region" snippet instead (weak models act on the fresh
// region, not a diff).
const (
	// maxDisplayDiffLines caps the rendered diff body; the full change is
	// still on disk and in the expand store. Overflow is disclosed.
	maxDisplayDiffLines = 24
	// displayDiffContext is how many unchanged lines pad each hunk.
	displayDiffContext = 2
	// maxDisplayDiffBytes skips diff computation entirely for very large
	// files — a display nicety must never burn real CPU on a 10MB target.
	maxDisplayDiffBytes = 1 << 20 // 1MB
)

// editDisplayData returns the ToolResult.Data payload for a successful edit:
// display_diff (numbered ±context hunks), diff_added, diff_removed. Nil when
// nothing changed or the file is too large to diff cheaply.
func editDisplayData(oldContent, newContent string) map[string]any {
	if oldContent == newContent ||
		len(oldContent) > maxDisplayDiffBytes || len(newContent) > maxDisplayDiffBytes {
		return nil
	}
	diffText, added, removed := buildDisplayDiff(oldContent, newContent)
	if added == 0 && removed == 0 {
		return nil
	}
	return map[string]any{
		"display_diff": diffText,
		"diff_added":   added,
		"diff_removed": removed,
	}
}

// editSuccess wraps an edit's success status with the display-diff payload
// when one is worth showing — the single seam all five edit success paths
// return through.
func editSuccess(status, oldContent, newContent string) ToolResult {
	// An edit stores the file before AND after, so its pinned cost is roughly
	// twice the file and the undo stack refuses it past the ceiling. The read
	// itself is unavoidable here — you cannot replace text you have not loaded
	// — so only the record is declined, and this seam is where every one of the
	// five success paths can say so exactly once.
	if undo.SnapshotTooLargeLen(len(oldContent), len(newContent)) {
		status += " (" + undoSnapshotDeclinedNote() + ")"
	}
	if d := editDisplayData(oldContent, newContent); d != nil {
		return NewSuccessResultWithData(status, d)
	}
	return NewSuccessResult(status)
}

// displayDiffLine is one annotated line of the line-based diff.
type displayDiffLine struct {
	op   diffmatchpatch.Operation
	num  int // new-file line number for +/context, old-file number for -
	text string
}

// buildDisplayDiff computes a compact, line-numbered unified-style diff.
// Counts (added/removed) reflect the FULL diff; only the display text is
// capped at maxDisplayDiffLines with the overflow disclosed.
func buildDisplayDiff(oldContent, newContent string) (string, int, int) {
	dmp := diffmatchpatch.New()
	a, b, lineArray := dmp.DiffLinesToChars(oldContent, newContent)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lineArray)

	var annotated []displayDiffLine
	added, removed := 0, 0
	oldLine, newLine := 1, 1
	for _, d := range diffs {
		for _, line := range splitDiffLines(d.Text) {
			switch d.Type {
			case diffmatchpatch.DiffDelete:
				annotated = append(annotated, displayDiffLine{d.Type, oldLine, line})
				oldLine++
				removed++
			case diffmatchpatch.DiffInsert:
				annotated = append(annotated, displayDiffLine{d.Type, newLine, line})
				newLine++
				added++
			default:
				annotated = append(annotated, displayDiffLine{d.Type, newLine, line})
				oldLine++
				newLine++
			}
		}
	}

	// Select hunks: changed lines plus displayDiffContext of surrounding
	// context, separated by an ellipsis row where gaps were skipped.
	keep := make([]bool, len(annotated))
	for i, l := range annotated {
		if l.op == diffmatchpatch.DiffEqual {
			continue
		}
		for j := max(0, i-displayDiffContext); j <= min(len(annotated)-1, i+displayDiffContext); j++ {
			keep[j] = true
		}
	}

	var sb strings.Builder
	shown, skippedGap := 0, false
	for i, l := range annotated {
		if !keep[i] {
			skippedGap = sb.Len() > 0
			continue
		}
		if shown >= maxDisplayDiffLines {
			fmt.Fprintf(&sb, "⋯ diff truncated (+%d/-%d total)\n", added, removed)
			skippedGap = false
			break
		}
		if skippedGap {
			sb.WriteString("⋯\n")
			skippedGap = false
		}
		marker := " "
		switch l.op {
		case diffmatchpatch.DiffDelete:
			marker = "-"
		case diffmatchpatch.DiffInsert:
			marker = "+"
		}
		text := l.text
		if runes := []rune(text); len(runes) > 160 {
			text = string(runes[:159]) + "…"
		}
		fmt.Fprintf(&sb, "%s %4d  %s\n", marker, l.num, text)
		shown++
	}
	return strings.TrimRight(sb.String(), "\n"), added, removed
}

// splitDiffLines splits a diff chunk into its lines, dropping the trailing
// empty element a trailing newline produces (every dmp line-mode chunk ends
// with \n except possibly the file's last).
func splitDiffLines(text string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}
