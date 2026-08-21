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

// TestCopyLargeFileSkipsUndoSnapshotAndSaysSo pins the same ceiling on copy,
// where the gap was starkest: copyFile streams through io.Copy, so gokin could
// already copy a multi-gigabyte file without ever holding it — and then read
// the whole result back into the undo stack.
func TestCopyLargeFileSkipsUndoSnapshotAndSaysSo(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	sparseFile(t, src, maxUndoSnapshotBytes+1)

	mgr := undo.NewManager()
	tool := NewCopyTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{
		"source":      src,
		"destination": dst,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("copy failed: %s", res.Content)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("copy did not produce the destination: %v", err)
	}
	if !strings.Contains(res.Content, "not undoable") {
		t.Errorf("copy must disclose the unsnapshotted file, got: %s", res.Content)
	}
	if n := mgr.Count(); n != 0 {
		t.Errorf("expected no undo record for an unsnapshotted copy, got %d", n)
	}
}

// TestCopyDirectoryRecordsOnlySnapshottableFiles covers the mixed tree: the
// small file stays undoable, the large one is skipped and counted.
func TestCopyDirectoryRecordsOnlySnapshottableFiles(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	src := filepath.Join(dir, "tree")
	if err := os.Mkdir(src, 0755); err != nil {
		t.Fatal(err)
	}
	sparseFile(t, filepath.Join(src, "big.bin"), maxUndoSnapshotBytes+1)
	if err := os.WriteFile(filepath.Join(src, "small.txt"), []byte("precious"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "tree-copy")

	mgr := undo.NewManager()
	tool := NewCopyTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{
		"source":      src,
		"destination": dst,
		"recursive":   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("copy failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "1 file too large to snapshot") {
		t.Errorf("copy must disclose exactly one unsnapshotted file, got: %s", res.Content)
	}
	if n := mgr.Count(); n != 1 {
		t.Fatalf("expected exactly 1 undo record (the small file), got %d", n)
	}
	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "small.txt")); !os.IsNotExist(err) {
		t.Error("undo did not remove the copied small file")
	}
}

// TestCopySmallFileStaysUndoable is the companion: the ceiling must not cost
// ordinary work its undo.
func TestCopySmallFileStaysUndoable(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("precious"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	tool := NewCopyTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{
		"source":      src,
		"destination": dst,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("copy failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "not undoable") {
		t.Errorf("a small copy must stay undoable, got: %s", res.Content)
	}
	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo failed: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("undo did not remove the copied file")
	}
}

// TestEditLargeFileDisclosesDeclinedSnapshot covers the last member of the
// family, and the one shaped differently: edit MUST read the file to replace
// text in it, so the read cannot be skipped the way delete's and copy's can.
// Only the record is declined — that is the half that would otherwise pin
// roughly twice the file for the rest of the session — and editSuccess, the
// single seam all five success paths return through, is where it is disclosed.
func TestEditLargeFileDisclosesDeclinedSnapshot(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	path := filepath.Join(dir, "big.txt")
	// Old + new together must clear the ceiling, so the file itself only needs
	// to be past half of it.
	body := strings.Repeat("abcdefghij", (maxUndoSnapshotBytes/2/10)+1)
	if err := os.WriteFile(path, []byte(body+"NEEDLE"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	tool := NewEditTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "NEEDLE",
		"new_string": "FOUND",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Content)
	}
	// The edit itself must still happen — the ceiling governs undo, not work.
	if b, err := os.ReadFile(path); err != nil || !strings.HasSuffix(string(b), "FOUND") {
		t.Fatalf("edit did not apply: %v", err)
	}
	if !strings.Contains(res.Content, "not undoable") {
		t.Errorf("edit must disclose the declined snapshot, got: %s", res.Content)
	}
	if n := mgr.Count(); n != 0 {
		t.Errorf("expected no undo record past the ceiling, got %d", n)
	}
}

// TestEditSmallFileStaysUndoable is the companion half.
func TestEditSmallFileStaysUndoable(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	path := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(path, []byte("hello NEEDLE"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	tool := NewEditTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "NEEDLE",
		"new_string": "FOUND",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("edit failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "not undoable") {
		t.Errorf("an ordinary edit must stay undoable, got: %s", res.Content)
	}
	if _, err := mgr.Undo(); err != nil {
		t.Fatalf("undo failed: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello NEEDLE" {
		t.Errorf("undo did not restore the file, got %q", string(b))
	}
}

// TestWriteOverLargeFileDisclosesDeclinedSnapshot: write reads the old content
// for the operation itself (append concatenates it, the diff preview shows it),
// so again only the record is refused.
func TestWriteOverLargeFileDisclosesDeclinedSnapshot(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxUndoSnapshotBytes+1)), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	tool := NewWriteTool(dir)
	tool.SetUndoManager(mgr)

	res, err := tool.Execute(context.Background(), map[string]any{
		"file_path": path,
		"content":   "replaced",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("write failed: %s", res.Content)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "replaced" {
		t.Fatalf("write did not apply: %v", err)
	}
	if !strings.Contains(res.Content, "not undoable") {
		t.Errorf("write must disclose the declined snapshot, got: %s", res.Content)
	}
	if n := mgr.Count(); n != 0 {
		t.Errorf("expected no undo record past the ceiling, got %d", n)
	}
}
