package undo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRedoLastGroupRoundTripsInOriginalOrder is the mirror of UndoLastGroup, and
// it is built around two sequential edits to the SAME file precisely because
// that is what pins the ordering claim: undo must revert newest-first and redo
// must re-apply oldest-first, and any other order makes the redo preflight fail
// because each change expects to find the previous one's result on disk.
func TestRedoLastGroupRoundTripsInOriginalOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("C"), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.SetActiveGroup("msg-1")
	m.Record(FileChange{FilePath: path, Tool: "edit", OldContent: []byte("A"), NewContent: []byte("B")})
	m.Record(FileChange{FilePath: path, Tool: "edit", OldContent: []byte("B"), NewContent: []byte("C")})
	m.ClearActiveGroup()

	undone, err := m.UndoLastGroup()
	if err != nil {
		t.Fatalf("UndoLastGroup: %v", err)
	}
	if len(undone) != 2 {
		t.Fatalf("expected 2 changes undone, got %d", len(undone))
	}
	if b, _ := os.ReadFile(path); string(b) != "A" {
		t.Fatalf("group undo did not walk back to the original content, got %q", string(b))
	}

	redone, err := m.RedoLastGroup()
	if err != nil {
		t.Fatalf("RedoLastGroup: %v", err)
	}
	if len(redone) != 2 {
		t.Fatalf("expected 2 changes redone, got %d", len(redone))
	}
	if b, _ := os.ReadFile(path); string(b) != "C" {
		t.Fatalf("group redo did not reach the final content, got %q", string(b))
	}
	// Both stacks must be back where they started, or a second round trip
	// would silently do the wrong thing.
	if n := m.Count(); n != 2 {
		t.Errorf("expected the group back on the undo stack, got %d", n)
	}
	if m.CanRedo() {
		t.Error("redo stack must be drained after redoing the whole group")
	}

	// A second full round trip must behave identically.
	if _, err := m.UndoLastGroup(); err != nil {
		t.Fatalf("second UndoLastGroup: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "A" {
		t.Fatalf("second group undo landed on %q", string(b))
	}
}

// TestRedoLastGroupWithoutAGroupIsASingleRedo pins the documented fallback: a
// change recorded outside any request has no group, and must not drag unrelated
// entries along with it.
func TestRedoLastGroupWithoutAGroupIsASingleRedo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "solo.txt")
	if err := os.WriteFile(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.Record(FileChange{FilePath: path, Tool: "write", NewContent: []byte("new"), WasNew: true})
	if _, err := m.Undo(); err != nil {
		t.Fatalf("Undo: %v", err)
	}

	redone, err := m.RedoLastGroup()
	if err != nil {
		t.Fatalf("RedoLastGroup: %v", err)
	}
	if len(redone) != 1 {
		t.Fatalf("expected exactly 1 change redone, got %d", len(redone))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("redo did not restore the file: %v", err)
	}
}
