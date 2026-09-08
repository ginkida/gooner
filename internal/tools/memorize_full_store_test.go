package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A full memory store used to be indistinguishable from a satisfied one. The
// write was dropped, nothing propagated, and the flush then reported that
// nothing needed writing — so the tool answered "Memorized fact: X (already
// current — nothing to write)", which claims both that the fact was stored and
// that it had been stored before. The model believes it, the fact is gone, and
// every later memorize is lost the same silent way.
func TestMemorize_FullStoreIsReportedNotClaimedAsMemorized(t *testing.T) {
	pl := newLearningForTest(t)
	// Fill through the real writer rather than the limit constant, which is
	// unexported: whatever the limit is, this stops exactly at it.
	filled := 0
	for pl.SetPreference(fmt.Sprintf("fact:filler-%06d", filled), "value") {
		filled++
		if filled > 1_000_000 {
			t.Fatal("the store accepted a million entries; it is not bounded at all")
		}
	}
	if filled == 0 {
		t.Fatal("the store refused its first entry; the test is not measuring a full store")
	}

	result, err := NewMemorizeTool(pl).Execute(context.Background(), map[string]any{
		"type":    "fact",
		"key":     "the-one-that-matters",
		"content": "a durable fact discovered mid-task",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Success {
		t.Fatalf("reported success for a fact it did not store: %q", result.Content)
	}
	if strings.Contains(result.Content, "already current") {
		t.Fatalf("told the model the fact was already saved: %q", result.Content)
	}
	// The model can act on this itself, which is the point of naming it.
	if !strings.Contains(result.Error+result.Content, "forget") {
		t.Fatalf("must point at the action that frees room: %q / %q", result.Content, result.Error)
	}
}

// The ordinary path must stay ordinary — a store with room reports a real save.
func TestMemorize_StoreWithRoomStillSaves(t *testing.T) {
	result, err := NewMemorizeTool(newLearningForTest(t)).Execute(context.Background(), map[string]any{
		"type":    "fact",
		"key":     "build-command",
		"content": "go build ./...",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Success {
		t.Fatalf("a store with room must accept a fact: %q / %q", result.Content, result.Error)
	}
}
