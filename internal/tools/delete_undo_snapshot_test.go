package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gokin/internal/testkit"
	"gokin/internal/undo"
)

// sparseFile creates a file of the requested apparent size without writing that
// many bytes, so the size gate can be exercised without a multi-megabyte write.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

// TestDeleteLargeFileSkipsUndoSnapshotAndSaysSo pins the memory ceiling on the
// undo stack. delete's pre-read exists ONLY to make the deletion undoable, and
// the stack is memory-resident and bounded by a change COUNT, so an unbounded
// read here let a streamed multi-gigabyte delete become an equally large
// allocation held for the rest of the session. Oversized files must skip the
// snapshot AND disclose it — an operation the user believes is undoable but is
// not is worse than one that says up front that it cannot be taken back.
func TestDeleteLargeFileSkipsUndoSnapshotAndSaysSo(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	big := filepath.Join(dir, "big.bin")
	sparseFile(t, big, maxUndoSnapshotBytes+1)

	mgr := undo.NewManager()
	tool := NewDeleteTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{"path": big})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("delete failed: %s", res.Content)
	}
	if _, err := os.Stat(big); !os.IsNotExist(err) {
		t.Fatal("delete did not remove the file")
	}
	if !strings.Contains(res.Content, "not undoable") {
		t.Errorf("delete must disclose that the file was not snapshotted, got: %s", res.Content)
	}
	// Nothing may be recorded: a change whose content was never captured would
	// make /undo report success while restoring an empty file.
	if n := mgr.Count(); n != 0 {
		t.Errorf("expected no undo record for an unsnapshotted delete, got %d", n)
	}
}

// TestDeleteSmallFileStaysUndoable is the companion half: the ceiling must not
// cost ordinary source-tree work its undo.
func TestDeleteSmallFileStaysUndoable(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("precious"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	tool := NewDeleteTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{"path": small})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("delete failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "not undoable") {
		t.Errorf("a small file must stay undoable, got: %s", res.Content)
	}
	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo failed: %v", err)
	}
	if b, err := os.ReadFile(small); err != nil || string(b) != "precious" {
		t.Fatalf("undo did not restore the deleted file: %v", err)
	}
}

// TestBatchDeleteDisclosesUnsnapshottedFiles is the batch flavour of the same
// ceiling, where the exposure is larger: batch_delete pre-reads EVERY matched
// file, so an unbounded read could pin a whole directory of large files in
// memory at once. Oversized entries must skip the snapshot and the summary
// must say how many are not undoable.
func TestBatchDeleteDisclosesUnsnapshottedFiles(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	big := filepath.Join(dir, "big.bin")
	small := filepath.Join(dir, "small.txt")
	sparseFile(t, big, maxUndoSnapshotBytes+1)
	if err := os.WriteFile(small, []byte("precious"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	bt := NewBatchTool(dir)
	bt.SetUndoManager(mgr)

	res, err := bt.Execute(context.Background(), map[string]any{
		"operation": "delete",
		"files":     []any{big, small},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("batch delete failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Not undoable: 1") {
		t.Errorf("batch must disclose the unsnapshotted file, got:\n%s", res.Content)
	}
	// Only the small file may be undoable; the large one must not be recorded
	// with content it never captured.
	if n := mgr.Count(); n != 1 {
		t.Fatalf("expected exactly 1 undo record (the small file), got %d", n)
	}
	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo of the small file failed: %v", err)
	}
	if b, err := os.ReadFile(small); err != nil || string(b) != "precious" {
		t.Fatalf("undo did not restore the small file: %v", err)
	}
}
