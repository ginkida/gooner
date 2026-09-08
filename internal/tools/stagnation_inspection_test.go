package tools

import (
	"testing"

	"google.golang.org/genai"
)

// v0.100.95 Kimi-friendliness sweep: the same empty-fingerprint class as the
// todo field report, generalized to every dedicated inspection tool a
// reasoning-heavy model (Kimi K3) runs repeatedly with DISTINCT args as
// exploration progress. Before this, they fell into stagnationFingerprint's
// default "" case, so N distinct targets collapsed to one pattern and tripped
// the 5-repeat abort.
func TestStagnationFingerprint_InspectionToolsKeyOnTarget(t *testing.T) {
	cases := []struct {
		tool  string
		argsA map[string]any
		argsB map[string]any
	}{
		{"list_dir", map[string]any{"directory_path": "internal/app"}, map[string]any{"directory_path": "internal/ui"}},
		{"tree", map[string]any{"path": "internal/client"}, map[string]any{"path": "internal/tools"}},
		{"git_status", map[string]any{"path": "internal/app"}, map[string]any{"path": "internal/ui"}},
		{"git_diff", map[string]any{"file": "a.go"}, map[string]any{"file": "b.go"}},
		{"git_log", map[string]any{"file": "a.go"}, map[string]any{"file": "b.go"}},
		{"git_blame", map[string]any{"file": "a.go"}, map[string]any{"file": "b.go"}},
		{"review_changes", map[string]any{"file": "a.go"}, map[string]any{"file": "b.go"}},
		{"check_impact", map[string]any{"symbol": "Foo"}, map[string]any{"symbol": "Bar"}},
		{"go_search", map[string]any{"query": "func Foo"}, map[string]any{"query": "func Bar"}},
		{"go_to_definition", map[string]any{"file": "a.go", "symbol": "Foo"}, map[string]any{"file": "a.go", "symbol": "Bar"}},
		{"find_references", map[string]any{"file": "a.go", "symbol": "Foo"}, map[string]any{"file": "b.go", "symbol": "Foo"}},
	}
	for _, c := range cases {
		fpA := stagnationFingerprint(c.tool, c.argsA)
		fpB := stagnationFingerprint(c.tool, c.argsB)
		if fpA == "" || fpB == "" {
			t.Errorf("%s: distinct targets produced an empty fingerprint (%q/%q)", c.tool, fpA, fpB)
		}
		if fpA == fpB {
			t.Errorf("%s: distinct targets collapsed to one fingerprint %q", c.tool, fpA)
		}
		// Identical repeat still collapses (a genuine loop).
		if again := stagnationFingerprint(c.tool, c.argsA); again != fpA {
			t.Errorf("%s: identical args produced different fingerprints %q vs %q", c.tool, fpA, again)
		}
	}
}

// A truly-identical inspection loop (same args 5x) earns graceful hints, not a
// hard turn-kill — the read-only-bash principle (v0.100.89) applied to the
// dedicated read-only tools. They ARE read-only, so they get the force-finalize
// phase too (unlike edit/todo).
func TestStagnationRecovery_ReadOnlyInspectionToolsAreHintEligible(t *testing.T) {
	for _, tool := range []string{
		"git_status", "git_diff", "git_log", "git_blame", "diff",
		"review_changes", "check_impact", "go_search",
		"go_to_definition", "find_references", "history_search",
	} {
		if got := maxStagnationRecoveryAttempts(tool); got != 2 {
			t.Errorf("maxStagnationRecoveryAttempts(%s) = %d, want 2", tool, got)
		}
		call := []*genai.FunctionCall{{Name: tool, Args: map[string]any{}}}
		if !shouldAttemptStagnationRecovery(call, 0) {
			t.Errorf("%s must earn a recovery hint at attempt 0", tool)
		}
		// Read-only ⇒ 2 hints + 2 finalize before abort (attempt 4 aborts).
		if !shouldAttemptStagnationRecovery(call, 3) {
			t.Errorf("%s should still recover at attempt 3 (hints + finalize)", tool)
		}
		if shouldAttemptStagnationRecovery(call, 4) {
			t.Errorf("%s budget must be bounded — attempt 4 aborts", tool)
		}
		if _, readOnly, ok := stagnationHintBudget(call); !ok || !readOnly {
			t.Errorf("%s hint budget = readOnly:%v ok:%v, want ok + readOnly (force-finalize eligible)", tool, readOnly, ok)
		}
	}
}
