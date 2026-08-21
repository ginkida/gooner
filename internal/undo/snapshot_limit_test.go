package undo

import (
	"strings"
	"testing"
)

// TestRecordDeclinesOversizedContent pins the ceiling at its root. The stack is
// memory-resident and bounded by a COUNT of changes, so without this a single
// tool that snapshots a large file pins it for the rest of the session — and
// every future recorder would have to remember a guard of its own.
func TestRecordDeclinesOversizedContent(t *testing.T) {
	m := NewManager()

	oversized := FileChange{
		FilePath:   "/tmp/huge.bin",
		Tool:       "edit",
		OldContent: []byte(strings.Repeat("a", MaxSnapshotBytes/2+1)),
		NewContent: []byte(strings.Repeat("b", MaxSnapshotBytes/2+1)),
	}
	if m.Record(oversized) {
		t.Error("Record must decline content past the snapshot ceiling")
	}
	if n := m.Count(); n != 0 {
		t.Errorf("declined change must not enter the stack, got %d", n)
	}

	// Both sides count together: an edit holds the file before AND after, so a
	// file just over half the ceiling is already past it.
	if !SnapshotTooLargeLen(MaxSnapshotBytes/2+1, MaxSnapshotBytes/2+1) {
		t.Error("the two content sides must be weighed together")
	}
}

// TestRecordAcceptsOrdinaryContent is the companion: the ceiling must not cost
// ordinary source-tree work its undo.
func TestRecordAcceptsOrdinaryContent(t *testing.T) {
	m := NewManager()

	if !m.Record(FileChange{
		FilePath:   "/tmp/main.go",
		Tool:       "edit",
		OldContent: []byte("package main"),
		NewContent: []byte("package main // edited"),
	}) {
		t.Fatal("Record must accept ordinary content")
	}
	if n := m.Count(); n != 1 {
		t.Errorf("expected the change to be recorded, got %d", n)
	}
}
