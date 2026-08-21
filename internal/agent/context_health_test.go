package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"gokin/internal/config"
	ctxmgr "gokin/internal/context"
	"gokin/internal/testkit"
)

// blockingCountClient parks inside CountTokens until released, standing in for
// a provider whose count_tokens endpoint has stopped answering.
type blockingCountClient struct {
	*testkit.MockClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingCountClient) CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error) {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.release:
		return &genai.CountTokensResponse{TotalTokens: 1}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func healthAgent(t *testing.T, counter *ctxmgr.TokenCounter) *Agent {
	t.Helper()
	return &Agent{
		ctxCfg:       &config.ContextConfig{MaxInputTokens: 100000},
		tokenCounter: counter,
		history: []*genai.Content{
			genai.NewContentFromText("a reasonably sized turn of conversation", genai.RoleUser),
		},
	}
}

// TestGetContextHealthDoesNotHoldStateLockAcrossTheCount pins the reason the
// count moved out of the lock. CountContents reaches the provider whenever its
// hash misses the cache, which is the normal case because history grows every
// turn, and the health panel is refreshed from the LIVE rate-limit response
// callback. Holding stateMu across that call blocks the agent's own history
// append — Go parks new readers behind a waiting writer — so one slow count
// stalled the loop producing the very history being counted.
func TestGetContextHealthDoesNotHoldStateLockAcrossTheCount(t *testing.T) {
	cl := &blockingCountClient{
		MockClient: testkit.NewMockClient(),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	a := healthAgent(t, ctxmgr.NewTokenCounter(cl, "test-model", nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.GetContextHealth()
	}()

	select {
	case <-cl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the token count never started")
	}

	// The agent's own writer must still get in while the count is parked.
	locked := make(chan struct{})
	go func() {
		a.stateMu.Lock()
		a.history = append(a.history, genai.NewContentFromText("next turn", genai.RoleModel))
		a.stateMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(3 * time.Second):
		t.Fatal("history append blocked behind an in-flight token count — stateMu is held across the network call")
	}

	close(cl.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("GetContextHealth never returned")
	}
}

// TestGetContextHealthDegradesToAnEstimate: with the count now bounded, its
// failure path is reachable, and reporting zero is not a safe default — a zero
// reads downstream as "no data" and is silently replaced by the MAIN session's
// numbers, so a failed sub-agent count would be described by someone else's
// context.
func TestGetContextHealthDegradesToAnEstimate(t *testing.T) {
	// A nil client makes CountContents fail immediately, the same shape a
	// timeout produces.
	a := healthAgent(t, ctxmgr.NewTokenCounter(nil, "test-model", nil))

	h := a.GetContextHealth()
	if h.TotalTokens <= 0 {
		t.Fatalf("a failed count must fall back to a local estimate, got %d", h.TotalTokens)
	}
	if h.PercentUsed <= 0 {
		t.Errorf("PercentUsed should follow the estimate, got %v", h.PercentUsed)
	}
}

// TestGetContextHealthIsBounded pins the deadline itself. Without it the two
// tests above still pass — the first releases the client and the second never
// blocks — so a revert to context.Background(), which has neither a deadline
// nor cancellation, would go unnoticed on a path that fires from the live
// response callback.
func TestGetContextHealthIsBounded(t *testing.T) {
	original := contextHealthCountTimeout
	contextHealthCountTimeout = 50 * time.Millisecond
	t.Cleanup(func() { contextHealthCountTimeout = original })

	cl := &blockingCountClient{
		MockClient: testkit.NewMockClient(),
		entered:    make(chan struct{}),
		release:    make(chan struct{}), // deliberately never closed
	}
	a := healthAgent(t, ctxmgr.NewTokenCounter(cl, "test-model", nil))

	done := make(chan ContextHealth, 1)
	go func() { done <- a.GetContextHealth() }()

	select {
	case h := <-done:
		if h.TotalTokens <= 0 {
			t.Errorf("a timed-out count must still report the local estimate, got %d", h.TotalTokens)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("GetContextHealth did not return — the token count is unbounded")
	}
}
