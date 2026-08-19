package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gokin/internal/testkit"
	"gokin/internal/undo"
)

// TestMkdirUndoRemovesIntermediateDirs closes the deferred "untidy empty dirs"
// gap: a parents=true mkdir of a/b/c brings three directories into existence
// but the change record named only the leaf, so /undo removed c and left a/
// and a/b/ behind with no way to reach them again.
func TestMkdirUndoRemovesIntermediateDirs(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	target := filepath.Join(dir, "a", "b", "c")

	mgr := undo.NewManager()
	tool := NewMkdirTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{"path": target})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("mkdir failed: %s", res.Content)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("mkdir did not create the target: %v", err)
	}

	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo failed: %v", err)
	}

	for _, leftover := range []string{
		target,
		filepath.Join(dir, "a", "b"),
		filepath.Join(dir, "a"),
	} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("mkdir undo left %s behind", leftover)
		}
	}
	// The climb must stop at the first directory that already existed — the
	// workspace root is not this call's to delete.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("undo removed the pre-existing workspace root: %v", err)
	}

	// Redo must bring the whole chain back, not just the leaf's parent.
	if _, err := mgr.Redo(); err != nil {
		t.Fatalf("redo failed: %v", err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("redo did not recreate the full directory chain: %v", err)
	}
}

// TestMkdirUndoKeepsOccupiedAncestors pins the other half: pruning is
// best-effort and never deletes content. A directory that gained a file since
// the mkdir stays, and so does everything above it.
func TestMkdirUndoKeepsOccupiedAncestors(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	preexisting := filepath.Join(dir, "keep")
	if err := os.Mkdir(preexisting, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(preexisting, "x", "y")

	mgr := undo.NewManager()
	tool := NewMkdirTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{"path": target})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("mkdir failed: %s", res.Content)
	}

	// Something else lands in the intermediate directory before the undo.
	occupied := filepath.Join(preexisting, "x")
	if err := os.WriteFile(filepath.Join(occupied, "note.txt"), []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo failed: %v", err)
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("undo did not remove the created leaf directory")
	}
	if b, err := os.ReadFile(filepath.Join(occupied, "note.txt")); err != nil || string(b) != "keep me" {
		t.Errorf("undo destroyed content in a non-empty ancestor: %v", err)
	}
	if _, err := os.Stat(preexisting); err != nil {
		t.Errorf("undo removed a pre-existing directory: %v", err)
	}
}
