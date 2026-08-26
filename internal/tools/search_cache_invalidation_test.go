package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gokin/internal/testkit"
)

// A no-match grep is cached against ZERO files, so per-file invalidation can
// never reach it — the executor has to drop grep and glob wholesale on any
// write. Nothing tested that, and the failure it prevents is the worst kind:
// the agent searches for a symbol, does not find it, writes it, searches again
// to confirm, and is handed its own stale "No matches found". From there it
// concludes the write never happened and either re-implements the work or
// abandons the task. Nothing errors; the model is simply lied to about the
// state of the tree it just changed.
func TestGrepSeesAFileTheExecutorJustWrote(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	registry := NewRegistry()
	if err := registry.Register(NewGrepTool(dir)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewWriteTool(dir)); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(registry, nil, 30*time.Second)
	executor.SetToolCache(NewToolResultCache(DefaultCacheConfig()))
	ctx := context.Background()

	const marker = "UNIQUELY_NAMED_SYMBOL_XYZ"
	grepArgs := map[string]any{"pattern": marker, "path": dir}

	// 1. The search that finds nothing — and gets cached against no files.
	first, err := executor.InvokeTool(ctx, "grep", grepArgs)
	if err != nil {
		t.Fatalf("first grep: %v", err)
	}
	// grep echoes the pattern in its header, so presence of the marker string
	// proves nothing — the no-match verdict is what must hold here.
	if !strings.Contains(first.Content, "No matches found") {
		t.Fatalf("expected a no-match verdict before the file exists:\n%s", first.Content)
	}

	// 2. The agent writes the very thing it was looking for.
	target := filepath.Join(dir, "added.go")
	if res, err := executor.InvokeTool(ctx, "write", map[string]any{
		"file_path": target,
		"content":   "package main\n\nfunc " + marker + "() {}\n",
	}); err != nil || !res.Success {
		t.Fatalf("write through the executor failed: %v %+v", err, res)
	}

	// 3. The confirming search must see it.
	second, err := executor.InvokeTool(ctx, "grep", grepArgs)
	if err != nil {
		t.Fatalf("second grep: %v", err)
	}
	if strings.Contains(second.Content, "No matches found") || !strings.Contains(second.Content, "added.go") {
		t.Fatalf("grep served a stale no-match after the executor's own write — "+
			"the model would conclude its work never happened:\n%s", second.Content)
	}
}

// The same blind spot on glob: a pattern that matched nothing is cached against
// zero files, so creating a file that matches it must still be visible.
func TestGlobSeesAFileTheExecutorJustWrote(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)
	registry := NewRegistry()
	if err := registry.Register(NewGlobTool(dir)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewWriteTool(dir)); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(registry, nil, 30*time.Second)
	executor.SetToolCache(NewToolResultCache(DefaultCacheConfig()))
	ctx := context.Background()

	globArgs := map[string]any{"pattern": "*.md", "path": dir}
	if _, err := executor.InvokeTool(ctx, "glob", globArgs); err != nil {
		t.Fatalf("first glob: %v", err)
	}

	target := filepath.Join(dir, "NOTES.md")
	if res, err := executor.InvokeTool(ctx, "write", map[string]any{
		"file_path": target, "content": "# notes\n",
	}); err != nil || !res.Success {
		t.Fatalf("write failed: %v %+v", err, res)
	}

	second, err := executor.InvokeTool(ctx, "glob", globArgs)
	if err != nil {
		t.Fatalf("second glob: %v", err)
	}
	if !strings.Contains(second.Content, "NOTES.md") {
		t.Fatalf("glob served a stale listing after the executor's own write:\n%s", second.Content)
	}
}
