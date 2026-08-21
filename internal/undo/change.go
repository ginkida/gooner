package undo

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"
)

// FileChange represents a single file modification.
type FileChange struct {
	ID         string      `json:"id"`
	FilePath   string      `json:"file_path"`
	Tool       string      `json:"tool"` // "write" or "edit"
	Timestamp  time.Time   `json:"timestamp"`
	OldContent []byte      `json:"old_content"` // nil for new files
	NewContent []byte      `json:"new_content"`
	WasNew     bool        `json:"was_new"`            // file was created (didn't exist before)
	GroupID    string      `json:"group_id,omitempty"` // Groups related changes for atomic undo
	Mode       os.FileMode `json:"mode,omitempty"`     // original file perm to restore on undo/redo (0 → 0644 fallback)
	// CreatedDirs lists every directory a "mkdir" actually brought into
	// existence, deepest-first. A parents=true mkdir of a/b/c creates three
	// directories but names only the leaf in FilePath, so an undo that
	// removed FilePath alone left the intermediate ones behind.
	CreatedDirs []string `json:"created_dirs,omitempty"`
}

// NewFileChange creates a new FileChange with a generated ID.
func NewFileChange(filePath, tool string, oldContent, newContent []byte, wasNew bool) *FileChange {
	return &FileChange{
		ID:         generateID(),
		FilePath:   filePath,
		Tool:       tool,
		Timestamp:  time.Now(),
		OldContent: oldContent,
		NewContent: newContent,
		WasNew:     wasNew,
	}
}

// generateID creates a unique identifier for a change.
func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Summary returns a human-readable summary of the change.
func (c *FileChange) Summary() string {
	if c.WasNew {
		return "created " + c.FilePath
	}
	return "modified " + c.FilePath
}

// SizeChange returns the size difference in bytes.
func (c *FileChange) SizeChange() int {
	return len(c.NewContent) - len(c.OldContent)
}

// MaxSnapshotBytes caps how much file content a SINGLE undo record may hold.
// The undo stack lives entirely in memory — nothing in this package is
// persisted — and Tracker bounds it by a COUNT of changes, never by bytes, so
// an unbounded snapshot pins that many bytes for the rest of the session. The
// ceiling sits alongside the repo's other read limits (10MB pdf/ipynb, 5MB
// images) and is deliberately generous enough that ordinary source-tree work
// stays fully undoable, while a dataset or a build artifact is refused.
const MaxSnapshotBytes = 10 << 20 // 10MB

// SnapshotTooLarge reports whether a change's two content sides together exceed
// what one record may hold. Both sides count: an edit stores the file before
// AND after, so the pinned cost is roughly twice the file.
func SnapshotTooLarge(oldContent, newContent []byte) bool {
	return SnapshotTooLargeLen(len(oldContent), len(newContent))
}

// SnapshotTooLargeLen is the same test over sizes alone, for callers that hold
// the content as strings and must not copy it into byte slices just to ask.
func SnapshotTooLargeLen(oldLen, newLen int) bool {
	return oldLen+newLen > MaxSnapshotBytes
}
