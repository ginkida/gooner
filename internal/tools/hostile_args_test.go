package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"gokin/internal/testkit"
)

// Tool arguments are the third surface fed directly by the model, after the
// terminal's size and the UI's message stream. Whatever the schema says, what
// arrives is whatever the model emitted: a number where a path belongs, a
// nested object where a string belongs, an explicit null, an empty string, a
// hundred-kilobyte value. A panic here is caught by the executor's recovery and
// surfaces as a "panic:" tool result, so it does not crash the process — it
// just hands the model a failure it cannot understand or act on, mid-task.
//
// The parameter names come from each tool's OWN declaration rather than a list
// kept here, so a newly registered tool is covered the day it lands.
func TestToolsSurviveHostileArguments(t *testing.T) {
	dir := testkit.ResolvedTempDir(t)

	hostile := []any{
		123, int64(-1), float64(1e18), float64(-1), "x", "", true, false, nil,
		map[string]any{"nested": map[string]any{"deep": 1}},
		[]any{1, "a", nil},
		[]string{"a"},
		strings.Repeat("A", 100000),
	}

	// Execute is fuzzed only for tools that cannot reach outside the temp
	// workspace: no process spawning, no network, no mutation, no agent spawn.
	// A tool missing from this set still gets its Validate fuzzed, which is
	// where argument-shape handling lives.
	executeSafe := map[string]bool{
		"read": true, "grep": true, "glob": true, "list_dir": true, "tree": true,
		"diff": true, "env": true, "todo": true, "tools_list": true,
		"git_status": true, "git_diff": true, "git_log": true, "git_blame": true,
		"review_changes": true, "check_impact": true, "history_search": true,
	}

	executed := 0
	for _, tool := range DefaultRegistry(dir).List() {
		name := tool.Name()
		var params []string
		if decl := tool.Declaration(); decl != nil && decl.Parameters != nil {
			for k := range decl.Parameters.Properties {
				params = append(params, k)
			}
		}

		variants := []map[string]any{nil, {}}
		for _, hv := range hostile {
			all := map[string]any{}
			for _, p := range params {
				all[p] = hv
			}
			variants = append(variants, all)
		}
		for _, p := range params {
			for _, hv := range hostile {
				variants = append(variants, map[string]any{p: hv})
			}
		}

		for vi, args := range variants {
			passed := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s.Validate panicked on variant %d (%v): %v", name, vi, args, r)
					}
				}()
				passed = tool.Validate(args) == nil
			}()

			// The executor gates Execute behind Validate, so args it rejects are
			// an unreachable path — fuzzing those would prove nothing. What
			// matters is the hostile shape a tool's own contract ACCEPTS.
			if !passed || !executeSafe[name] {
				continue
			}
			executed++
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s.Execute panicked on validated args %v: %v", name, args, r)
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = tool.Execute(ctx, args)
			}()
		}
	}

	if executed == 0 {
		t.Fatal("no validated-hostile call reached Execute — the fuzz is not exercising anything")
	}
}
