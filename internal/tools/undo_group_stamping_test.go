package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gokin/internal/testkit"
	"gokin/internal/undo"
)

// The per-request group is stamped by the message processor and read back by
// /undo all, but everything between those two points is the tool-execution
// path — and nothing exercised it. The unit tests for the group form record
// changes by hand, which proves the manager works and says nothing about
// whether a real tool dispatch actually lands inside the active group. If the
// stamp were lost anywhere in that path, /undo all would silently degrade to
// reverting one change, and every existing test would still pass.
func TestActiveGroupSurvivesRealToolDispatch(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	existing := filepath.Join(dir, "existing.go")
	if err := os.WriteFile(existing, []byte("package main\n\nconst answer = 41\n"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := undo.NewManager()
	registry := NewRegistry()
	write := NewWriteTool(dir)
	write.SetUndoManager(mgr)
	edit := NewEditTool(dir)
	edit.SetUndoManager(mgr)
	if err := registry.Register(write); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(edit); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(registry, nil, 30*time.Second)
	ctx := context.Background()

	// What the message processor does around one user turn.
	mgr.SetActiveGroup("msg-integration")

	created := filepath.Join(dir, "created.go")
	if res, err := executor.InvokeTool(ctx, "write", map[string]any{
		"file_path": created,
		"content":   "package main\n\nfunc added() {}\n",
	}); err != nil || !res.Success {
		t.Fatalf("write through the executor failed: %v %+v", err, res)
	}
	if res, err := executor.InvokeTool(ctx, "edit", map[string]any{
		"file_path":  existing,
		"old_string": "41",
		"new_string": "42",
	}); err != nil || !res.Success {
		t.Fatalf("edit through the executor failed: %v %+v", err, res)
	}

	mgr.ClearActiveGroup()

	changes := mgr.List()
	if len(changes) != 2 {
		t.Fatalf("expected both dispatches to record a change, got %d", len(changes))
	}
	for _, c := range changes {
		if c.GroupID != "msg-integration" {
			t.Fatalf("a change reached the stack outside the active group (%q on %s) — "+
				"/undo all would silently revert less than the request did", c.GroupID, c.FilePath)
		}
	}

	// And the group form reverts exactly that turn.
	reverted, err := mgr.UndoLastGroup()
	if err != nil {
		t.Fatalf("UndoLastGroup: %v", err)
	}
	if len(reverted) != 2 {
		t.Fatalf("expected 2 changes reverted, got %d", len(reverted))
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Error("the created file survived the group undo")
	}
	if b, err := os.ReadFile(existing); err != nil || string(b) != "package main\n\nconst answer = 41\n" {
		t.Errorf("the edited file was not restored: %v %q", err, string(b))
	}
}
