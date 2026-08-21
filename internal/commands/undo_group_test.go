package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gokin/internal/undo"
)

// recordGroupedCreations writes n files and records each as a creation under a
// single request group, mirroring what the message processor stamps for every
// change one user message produces.
func recordGroupedCreations(t *testing.T, mgr *undo.Manager, dir, groupID string, names ...string) []string {
	t.Helper()
	mgr.SetActiveGroup(groupID)
	defer mgr.ClearActiveGroup()

	paths := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}
		if !mgr.Record(undo.FileChange{
			FilePath:   path,
			Tool:       "write",
			NewContent: []byte("new"),
			WasNew:     true,
		}) {
			t.Fatalf("Record declined %s", name)
		}
		paths = append(paths, path)
	}
	return paths
}

// TestUndoAllRevertsWholeRequest wires up machinery that was written and then
// never called from anywhere: the message processor has always stamped a
// per-request group on every change "for atomic undo", and Manager.UndoLastGroup
// has always implemented it — preflighting the whole set before its first write
// and rolling back on failure — but no command ever reached it. /undo reverted
// one change at a time, so a request that touched five files needed the user to
// guess "/undo 5", which is a plain loop with none of those guarantees.
func TestUndoAllRevertsWholeRequest(t *testing.T) {
	dir := t.TempDir()
	mgr := undo.NewManager()
	paths := recordGroupedCreations(t, mgr, dir, "msg-1", "a.go", "b.go", "c.go")
	app := &undoFakeApp{fakeAppForMCP: &fakeAppForMCP{}, mgr: mgr}

	got, err := (&UndoCommand{}).Execute(context.Background(), []string{"all"}, app)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(got, "Undone 3 change(s) from the last request") {
		t.Fatalf("result does not report the whole request:\n%s", got)
	}
	if !strings.Contains(got, "/redo all") {
		t.Fatalf("result does not say how to re-apply the group:\n%s", got)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not reverted", filepath.Base(path))
		}
	}
	if n := mgr.Count(); n != 0 {
		t.Errorf("group undo left %d change(s) on the stack", n)
	}
}

// TestUndoSingleReportsRemainingFromSameRequest covers the discoverability half.
// The per-request group was write-only, so a user who undid one file had no way
// to learn that four more from the same request were still applied.
func TestUndoSingleReportsRemainingFromSameRequest(t *testing.T) {
	dir := t.TempDir()
	mgr := undo.NewManager()
	recordGroupedCreations(t, mgr, dir, "msg-1", "a.go", "b.go", "c.go")
	app := &undoFakeApp{fakeAppForMCP: &fakeAppForMCP{}, mgr: mgr}

	got, err := (&UndoCommand{}).Execute(context.Background(), nil, app)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(got, "2 more change(s) from that same request remain") {
		t.Fatalf("single undo does not disclose the rest of the request:\n%s", got)
	}
	if !strings.Contains(got, "/undo all") {
		t.Fatalf("single undo does not point at the atomic form:\n%s", got)
	}
}

// TestUndoSingleStaysQuietWithoutAGroup: a change recorded outside a request
// (a slash command's own edit) has no group, and must not grow a hint about one.
func TestUndoSingleStaysQuietWithoutAGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "solo.go")
	if err := os.WriteFile(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	mgr := undo.NewManager()
	mgr.Record(undo.FileChange{FilePath: path, Tool: "write", NewContent: []byte("new"), WasNew: true})
	app := &undoFakeApp{fakeAppForMCP: &fakeAppForMCP{}, mgr: mgr}

	got, err := (&UndoCommand{}).Execute(context.Background(), nil, app)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if strings.Contains(got, "same request") {
		t.Fatalf("ungrouped change must not claim a request:\n%s", got)
	}
}

// TestRedoAllRestoresWholeRequest closes the round trip at the command layer.
// Before this, /undo all reverted a five-file request in one atomic step and
// then getting it back took the user counting out /redo 5 — the exact asymmetry
// the group path exists to remove.
func TestRedoAllRestoresWholeRequest(t *testing.T) {
	dir := t.TempDir()
	mgr := undo.NewManager()
	paths := recordGroupedCreations(t, mgr, dir, "msg-1", "a.go", "b.go", "c.go")
	app := &undoFakeApp{fakeAppForMCP: &fakeAppForMCP{}, mgr: mgr}

	if _, err := (&UndoCommand{}).Execute(context.Background(), []string{"all"}, app); err != nil {
		t.Fatalf("undo all: %v", err)
	}
	got, err := (&RedoCommand{}).Execute(context.Background(), []string{"all"}, app)
	if err != nil {
		t.Fatalf("redo all: %v", err)
	}
	if !strings.Contains(got, "Redone 3 change(s) from the last request") {
		t.Fatalf("result does not report the whole request:\n%s", got)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not restored: %v", filepath.Base(path), err)
		}
	}
	if n := mgr.Count(); n != 3 {
		t.Errorf("expected the request back on the undo stack, got %d", n)
	}
}

// TestUndoAllPointsAtTheSymmetricRedo: the hint has to name a form that exists
// and covers the same set, otherwise the user is sent to count steps by hand.
func TestUndoAllPointsAtTheSymmetricRedo(t *testing.T) {
	dir := t.TempDir()
	mgr := undo.NewManager()
	recordGroupedCreations(t, mgr, dir, "msg-1", "a.go", "b.go")
	app := &undoFakeApp{fakeAppForMCP: &fakeAppForMCP{}, mgr: mgr}

	got, err := (&UndoCommand{}).Execute(context.Background(), []string{"all"}, app)
	if err != nil {
		t.Fatalf("undo all: %v", err)
	}
	if !strings.Contains(got, "/redo all") {
		t.Fatalf("undo all must point at the symmetric redo:\n%s", got)
	}
}
