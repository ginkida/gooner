package tools

import (
	"fmt"

	"gokin/internal/undo"
)

// maxUndoSnapshotBytes is the ceiling on how much file content a single undo
// record may hold. It belongs to the undo stack, not to any one tool, so it is
// defined once in internal/undo and enforced there for every recorder. What
// lives in this file is the tool-side half: gating a read that exists ONLY to
// make a change undoable, so those bytes are never loaded in the first place,
// and rendering the disclosure that keeps the decline honest.
const maxUndoSnapshotBytes = undo.MaxSnapshotBytes

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

// undoSnapshotDeclinedNote is the disclosure for a change the undo stack
// refused to hold. Used where the content had to be read for the operation
// itself, so only the record could be declined.
func undoSnapshotDeclinedNote() string {
	return fmt.Sprintf("not undoable — content exceeds the %s undo snapshot limit",
		humanByteSize(maxUndoSnapshotBytes))
}
