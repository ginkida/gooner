package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"
)

// applyCacheControl decides what the provider bills. Its three markers — the
// system block, the last tool, and the penultimate USER turn — are asserted
// nowhere on the wire, and the interaction that matters most is not a property
// of either feature alone: turn context is appended to the LAST user message,
// while the cache boundary must sit at or before len-3. If those ever met, the
// ephemeral per-turn snapshot would land INSIDE the cached prefix and every
// single turn would re-bill it. That is a silent, purely monetary failure —
// nothing breaks, the bill just grows.
func TestCacheControlMarkersOnTheWire(t *testing.T) {
	const marker = "EPHEMERAL-TURN-STATE"

	var mu sync.Mutex
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(raw, &body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	// kimi honours explicit cache_control; glm deliberately does not, so the
	// markers would be a no-op there and prove nothing. The provider identity
	// must be REAL — isProvider compares by equality, so the usual namespaced
	// test name would switch caching off and make this test vacuous.
	//
	// That has a consequence worth stating, because it already bit once while
	// writing this: a literal provider name shares the in-process provider
	// health map with every sibling test, so this request MUST SUCCEED. The
	// first version of the server replied with only message_stop, the client
	// scored that as an empty response, kimi's health was degraded, and
	// TestAdaptiveStreamRetryPolicyKimiDefaults failed several files away —
	// green in isolation, red in the suite. Keep the content delta below.
	c, err := NewAnthropicClient(AnthropicConfig{
		APIKey: "test", BaseURL: srv.URL, Provider: "kimi", Model: "k3",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetSystemInstruction("stable system prefix")
	c.SetTurnContext(marker)
	c.SetTools([]*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
		{Name: "read", Description: "read a file"},
		{Name: "write", Description: "write a file"},
	}}})

	history := []*genai.Content{
		genai.NewContentFromText("turn one", genai.RoleUser),
		genai.NewContentFromText("reply one", genai.RoleModel),
		genai.NewContentFromText("turn two", genai.RoleUser),
		genai.NewContentFromText("reply two", genai.RoleModel),
	}
	ctx := context.Background()
	stream, err := c.SendMessageWithHistory(ctx, history, "turn three")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stream.CollectContext(ctx)

	mu.Lock()
	got := body
	mu.Unlock()
	if got == nil {
		t.Fatal("no request body captured")
	}

	// 1. The system instruction must be array-of-blocks and carry the marker,
	//    or the largest stable span of the prompt is never cached at all.
	sysBlocks, ok := got["system"].([]any)
	if !ok || len(sysBlocks) == 0 {
		t.Fatalf("system was not converted to cacheable blocks: %T", got["system"])
	}
	if !strings.Contains(marshalString(t, sysBlocks[0]), "cache_control") {
		t.Errorf("system block carries no cache_control: %s", marshalString(t, sysBlocks[0]))
	}

	// 2. The LAST tool carries it, which caches the whole declaration array.
	tools, _ := got["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("no tools reached the request")
	}
	if !strings.Contains(marshalString(t, tools[len(tools)-1]), "cache_control") {
		t.Errorf("the last tool carries no cache_control: %s", marshalString(t, tools[len(tools)-1]))
	}
	for i := 0; i < len(tools)-1; i++ {
		if strings.Contains(marshalString(t, tools[i]), "cache_control") {
			t.Errorf("tool %d carries a marker; only the last one should", i)
		}
	}

	// 3. Exactly one message carries the boundary, it is a user turn, and it is
	//    at or before len-3 so the newest exchange stays uncached.
	msgs, _ := got["messages"].([]any)
	marked := []int{}
	for i, raw := range msgs {
		if strings.Contains(marshalString(t, raw), "cache_control") {
			marked = append(marked, i)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("expected exactly one cached message boundary, found %v of %d messages", marked, len(msgs))
	}
	at := marked[0]
	if at > len(msgs)-3 {
		t.Errorf("cache boundary at %d of %d is too late — the newest exchange must stay uncached", at, len(msgs))
	}
	if role, _ := msgs[at].(map[string]any)["role"].(string); role != "user" {
		t.Errorf("cache boundary sits on a %q turn; it must be a user turn", role)
	}

	// 4. The interaction. Turn context rides the last user message, and that
	//    message must NOT be the cached one, or the ephemeral snapshot is
	//    billed into the prefix on every turn.
	for i, raw := range msgs {
		text := marshalString(t, raw)
		if strings.Contains(text, marker) && strings.Contains(text, "cache_control") {
			t.Errorf("message %d carries BOTH the per-turn snapshot and the cache boundary — "+
				"the ephemeral block is inside the cached prefix and re-bills every turn", i)
		}
	}
}
