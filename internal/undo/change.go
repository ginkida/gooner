package undo

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// FileChange represents a single file modification.
type FileChange struct {
	ID         string    `json:"id"`
	FilePath   string    `json:"file_path"`
	Tool       string    `json:"tool"` // "write" or "edit"
	Timestamp  time.Time `json:"timestamp"`
	OldContent []byte    `json:"old_content"` // nil for new files
	NewContent []byte    `json:"new_content"`
	WasNew     bool      `json:"was_new"` // file was created (didn't exist before)
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
