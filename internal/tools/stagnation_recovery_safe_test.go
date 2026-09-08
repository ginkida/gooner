package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gokin/internal/client"

	"google.golang.org/genai"
)

// v0.100.99: the recovery whack-a-mole is closed at the root — every
// side-effect-free tool (IsParallelSafeTool) plus the idempotent state/query
// tools earn graceful recovery (hints + force-finalize) on a truly-identical
// loop, instead of a hard turn-kill. Field reports hit it as read-only
// inspection, todo, memorize, then `memory` (a memory-update task looping on
// the kv-store tool). Recovery never re-executes the tool, so the only tools
// that keep the immediate hard abort are genuine mutations.
func TestStagnationRecovery_IdempotentToolsAreRecoverySafe(t *testing.T) {
	// The reported tool + its adjacent idempotent state/query siblings.
	recoverable := []string{
		"memory", "memorize", "skill", "mcp_admin", "todo",
		// Side-effect-free (IsParallelSafeTool) — sampled.
		"read", "grep", "git_status", "go_search", "review_changes", "web_fetch",
		// Idempotent inspection not in parallelSafeTools.
		"check_impact",
	}
	for _, tool := range recoverable {
		if !isStagnationRecoverySafe(tool) {
			t.Errorf("isStagnationRecoverySafe(%s) = false, want true", tool)
		}
		if got := maxStagnationRecoveryAttempts(tool); got < 2 {
			t.Errorf("maxStagnationRecoveryAttempts(%s) = %d, want >=2 (graceful recovery)", tool, got)
		}
		call := []*genai.FunctionCall{{Name: tool, Args: map[string]any{"x": 1}}}
		if !shouldAttemptStagnationRecovery(call, 0) {
			t.Errorf("%s must earn a recovery hint at attempt 0", tool)
		}
		// All recovery-safe tools except edit are force-finalize eligible.
		if _, readOnly, ok := stagnationHintBudget(call); !ok || !readOnly {
			t.Errorf("%s hint budget = readOnly:%v ok:%v, want ok + readOnly (force-finalize)", tool, readOnly, ok)
		}
	}

	// Genuine mutations keep the immediate hard abort (recovery could mask a
	// broken mutating loop / a dishonest success claim).
	for _, tool := range []string{"write", "delete", "move", "copy", "git_commit", "git_add", "ssh", "refactor", "batch"} {
		if isStagnationRecoverySafe(tool) {
			t.Errorf("%s must NOT be recovery-safe (genuine mutation)", tool)
		}
		if got := maxStagnationRecoveryAttempts(tool); got != 0 {
			t.Errorf("maxStagnationRecoveryAttempts(%s) = %d, want 0 (hard abort backstop)", tool, got)
		}
	}

	// edit stays the special case: 1 hint, NO force-finalize.
	if got := maxStagnationRecoveryAttempts("edit"); got != 1 {
		t.Errorf("maxStagnationRecoveryAttempts(edit) = %d, want 1", got)
	}
	editCall := []*genai.FunctionCall{{Name: "edit", Args: map[string]any{"file_path": "a.go", "old_string": "x"}}}
	if _, editReadOnly, _ := stagnationHintBudget(editCall); editReadOnly {
		t.Error("edit must stay force-finalize EXCLUDED")
	}
}

// The exact v0.100.100 field report, end-to-end: a model loops on the
// IDENTICAL `memory` call through every hint and finalize demand. The turn
// must end GRACEFULLY — honest final text, calls paired in history, nil error —
// never the dead "Agent Got Stuck" error card.
func TestExecutorExecuteLoop_IdenticalMemoryLoopGracefulStop(t *testing.T) {
	registry := NewRegistry()
	memTool := &scriptedStaticTool{name: "memory", content: "Saved."}
	if err := registry.Register(memTool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	args := map[string]any{"action": "remember", "key": "project-facts", "content": "gokin uses Go 1.25"}
	responses := make([]*client.StreamingResponse, 0, 9)
	for i := range 9 {
		responses = append(responses, buildExecutorTestStream(client.ResponseChunk{
			FunctionCalls: []*genai.FunctionCall{{ID: fmt.Sprintf("m%d", i), Name: "memory", Args: args}},
			Done:          true,
			FinishReason:  genai.FinishReasonStop,
		}))
	}
	cl := &scriptedExecutorClient{model: "k3", responses: responses}

	exec := NewExecutor(registry, cl, time.Second)
	exec.preFlightChecks = false

	history, text, err := exec.Execute(context.Background(), nil, "update the durable project memory")
	if err != nil {
		t.Fatalf("Execute() error = %v, want graceful stop (nil) — the memory loop is idempotent", err)
	}
	if !strings.Contains(text, "Loop guard stopped this turn") {
		t.Fatalf("final text = %q, want the honest loop-guard stop note", text)
	}
	if memTool.calls != 4 {
		t.Fatalf("memory executions = %d, want 4 (hints/finalize never re-execute)", memTool.calls)
	}
	// No orphaned tool_use: the final batch is paired in history.
	last := history[len(history)-1]
	paired := false
	for _, p := range last.Parts {
		if p.FunctionResponse != nil {
			paired = true
		}
	}
	if !paired {
		t.Fatal("final batch must be paired with synthetic results in history")
	}
}

// Contrast: a MUTATING loop (write) keeps the hard error abort — recovery
// could mask a broken mutating loop, and an "honest text" ending could imply
// half-applied work succeeded.
func TestExecutorExecuteLoop_IdenticalWriteLoopStillHardAborts(t *testing.T) {
	registry := NewRegistry()
	writeTool := &scriptedStaticTool{name: "write", content: "ok"}
	if err := registry.Register(writeTool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	args := map[string]any{"file_path": "/tmp/x.txt", "content": "same"}
	responses := make([]*client.StreamingResponse, 0, 6)
	for i := range 6 {
		responses = append(responses, buildExecutorTestStream(client.ResponseChunk{
			FunctionCalls: []*genai.FunctionCall{{ID: fmt.Sprintf("w%d", i), Name: "write", Args: args}},
			Done:          true,
			FinishReason:  genai.FinishReasonStop,
		}))
	}
	cl := &scriptedExecutorClient{model: "k3", responses: responses}

	exec := NewExecutor(registry, cl, time.Second)
	exec.preFlightChecks = false

	_, _, err := exec.Execute(context.Background(), nil, "write the file")
	if err == nil {
		t.Fatal("Execute() error = nil, want hard stagnation abort for a mutating loop")
	}
	if !strings.Contains(err.Error(), "executor stagnation") {
		t.Fatalf("err = %v, want executor stagnation", err)
	}
}

// The likely ROOT CAUSE of the memory loop: with the keyed store disabled,
// every memory call returned the same unfixable "enable it in config and
// restart" error — something only the USER can do — and the model retried
// forever. The error must forbid retries and point at an action available NOW.
func TestMemoryTool_DisabledStoreErrorForbidsRetryAndOffersMemorize(t *testing.T) {
	mt := NewMemoryTool() // store never set — disabled
	res, err := mt.Execute(context.Background(), map[string]any{
		"action": "remember", "key": "k", "content": "c",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Success {
		t.Fatal("disabled store must be an error result")
	}
	msg := res.Error + res.Content
	if !strings.Contains(msg, "do NOT retry") {
		t.Fatalf("error must forbid retries, got: %q", msg)
	}
	if !strings.Contains(msg, "memorize") {
		t.Fatalf("error must point at the available-now alternative (memorize), got: %q", msg)
	}
}
