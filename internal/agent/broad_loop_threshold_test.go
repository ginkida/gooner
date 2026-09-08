package agent

import "testing"

// Every local-inspection tool in the shared set must get a broad-loop ceiling
// well above the base, because reading/searching/blaming many distinct files in
// one request is exploration, not a loop. Iterating the actual set (not a
// hand-copied list) keeps this test from sharing the function's blind spot.
func TestBroadLoopThreshold_ExplorationToolsGetHigherCeiling(t *testing.T) {
	a := &Agent{loopThreshold: 8}

	if len(broadLoopExplorationTools) == 0 {
		t.Fatal("broadLoopExplorationTools is empty")
	}
	for tool := range broadLoopExplorationTools {
		got := a.broadLoopThreshold(tool)
		if got <= a.loopThreshold {
			t.Errorf("%s: broad threshold %d must exceed base %d", tool, got, a.loopThreshold)
		}
		if got != a.loopThreshold*4 {
			t.Errorf("%s: broad threshold = %d, want %d (4× base)", tool, got, a.loopThreshold*4)
		}
	}

	// Mutating/expensive tools — and read-only-but-not-free tools (web/ask_user)
	// — keep the base: a runaway loop there must be caught early.
	for _, tool := range []string{"write", "edit", "bash", "delete", "git_commit", "web_search", "web_fetch", "ask_user"} {
		if _, isExplorer := broadLoopExplorationTools[tool]; isExplorer {
			t.Fatalf("%s should NOT be in broadLoopExplorationTools", tool)
		}
		if got := a.broadLoopThreshold(tool); got != a.loopThreshold {
			t.Errorf("%s: broad threshold = %d, want base %d", tool, got, a.loopThreshold)
		}
	}

	// The confirmed-omitted siblings from review must now be covered — incl.
	// the v0.100.95 semantic/review inspection tools (Kimi-friendliness).
	for _, tool := range []string{
		"diff", "git_status", "git_diff", "git_log", "git_blame", "check_impact", "history_search",
		"go_search", "go_to_definition", "find_references", "review_changes",
	} {
		if got := a.broadLoopThreshold(tool); got != a.loopThreshold*4 {
			t.Errorf("%s: review-flagged explorer must get 4× ceiling, got %d", tool, got)
		}
	}
}

// Regression: with the base threshold (8), reading 9 DISTINCT files in one
// request used to trip the broad-loop guard ("STOP, you're stuck") and skip the
// 9th read — a false positive on routine multi-file work. The read-only ceiling
// must clear that, while the SAME count of writes is still caught (writes don't
// get the bump), so genuine stagnation detection is preserved.
func TestBroadLoopThreshold_NineDistinctReadsNotALoop(t *testing.T) {
	a := &Agent{loopThreshold: 8}
	const distinctReads = 9 // a routine multi-file task

	if distinctReads > a.broadLoopThreshold("read") {
		t.Fatalf("%d distinct reads still trips broad loop (ceiling %d) — false positive not fixed",
			distinctReads, a.broadLoopThreshold("read"))
	}
	if distinctReads <= a.broadLoopThreshold("write") {
		t.Fatalf("9 writes no longer trips broad loop (ceiling %d) — weakened genuine detection",
			a.broadLoopThreshold("write"))
	}
}

// The ceiling scales with mode (quick=4 / broad=8 / thorough=15) so a thorough
// run tolerates even more exploration, and an unknown tool falls through to base.
func TestBroadLoopThreshold_ScalesWithModeAndDefaults(t *testing.T) {
	for _, base := range []int{4, 8, 15} {
		a := &Agent{loopThreshold: base}
		if got := a.broadLoopThreshold("read"); got != base*4 {
			t.Errorf("base %d: read ceiling = %d, want %d", base, got, base*4)
		}
		if got := a.broadLoopThreshold("some_unknown_tool"); got != base {
			t.Errorf("base %d: unknown tool ceiling = %d, want base %d", base, got, base)
		}
	}
}
