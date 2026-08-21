package tools

import "fmt"

// maxUndoSnapshotBytes caps how much file content a single undo record may hold
// in memory. The undo stack lives entirely in RAM (nothing is persisted) and is
// bounded only by a COUNT of changes, so a tool that snapshots an arbitrarily
// large file to make its work undoable can pin that many bytes for the rest of
// the session. Sitting alongside the repo's other read ceilings (10MB pdf/ipynb,
// 5MB images), this keeps ordinary source-tree work fully undoable while
// refusing to load a dataset or a build artifact into memory.
const maxUndoSnapshotBytes = 10 << 20 // 10MB

// undoSnapshotTooLarge reports whether a file of this size may be snapshotted
// for undo. Callers that decline a snapshot MUST say so in their result — an
// operation the user believes is undoable but is not is worse than one that
// says up front that it cannot be taken back.
func undoSnapshotTooLarge(size int64) bool {
	return size > maxUndoSnapshotBytes
}

// undoSnapshotSkippedNote renders the user-facing disclosure for a mutation
// whose content was too large to snapshot.
func undoSnapshotSkippedNote(size int64) string {
	return fmt.Sprintf("not undoable — %s exceeds the %s undo snapshot limit",
		humanByteSize(size), humanByteSize(maxUndoSnapshotBytes))
}

// humanByteSize renders a byte count in the largest unit that keeps it >= 1.
func humanByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// undoSnapshotSkippedSuffix renders the trailing disclosure for an operation
// that produced n files too large to snapshot, or "" when every file was
// captured. A caller that declines snapshots MUST surface this — an operation
// the user believes is reversible but partly is not is worse than one that
// says up front that it cannot be taken back.
func undoSnapshotSkippedSuffix(n int) string {
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return " (1 file too large to snapshot — that file is not undoable)"
	}
	return fmt.Sprintf(" (%d files too large to snapshot — those files are not undoable)", n)
}
