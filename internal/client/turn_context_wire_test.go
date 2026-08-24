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

// turn context is claimed to do three things, and each end of that claim was
// tested while the path between them was not: appendTurnContextBlock has unit
// coverage, App.pushTurnContext has its own, and nothing asserted that a client
// carrying turn context actually emits it ON THE WIRE. The third property is
// the one with a cost attached — the whole point of moving working memory out
// of the system block is that it must land on the LAST user message, which no
// cache_control marker covers, and must never be written back into history.
// Leak it into a cached position and every turn re-bills the prefix, silently,
// on exactly the providers this project runs on.
func TestTurnContextReachesBothSendPathsAndNeverPersists(t *testing.T) {
	const marker = "EPHEMERAL-TURN-STATE-MARKER"

	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err == nil {
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewAnthropicClient(AnthropicConfig{
		APIKey: "test", BaseURL: srv.URL, Provider: "glm", Model: "glm-5.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetTurnContext(marker)

	history := []*genai.Content{
		genai.NewContentFromText("first user turn", genai.RoleUser),
		genai.NewContentFromText("model reply", genai.RoleModel),
	}
	before := renderHistory(history)

	ctx := context.Background()
	stream, err := c.SendMessageWithHistory(ctx, history, "second user turn")
	if err != nil {
		t.Fatalf("SendMessageWithHistory: %v", err)
	}
	_, _ = stream.CollectContext(ctx)

	stream2, err := c.SendFunctionResponse(ctx, history, []*genai.FunctionResponse{
		{Name: "read", Response: map[string]any{"content": "file body"}},
	})
	if err != nil {
		t.Fatalf("SendFunctionResponse: %v", err)
	}
	_, _ = stream2.CollectContext(ctx)

	mu.Lock()
	captured := bodies
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("expected both send paths to reach the server, got %d requests", len(captured))
	}

	for i, body := range captured {
		msgs, _ := body["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("request %d carried no messages", i)
		}
		carrying := []int{}
		for j, raw := range msgs {
			if strings.Contains(marshalString(t, raw), marker) {
				carrying = append(carrying, j)
			}
		}
		if len(carrying) != 1 {
			t.Fatalf("request %d: turn context must appear exactly once, found at %v", i, carrying)
		}
		last := len(msgs) - 1
		if carrying[0] != last {
			t.Errorf("request %d: turn context landed on message %d of %d — anything but the last "+
				"position is covered by prefix caching and re-bills the whole prefix every turn",
				i, carrying[0], last)
		}
		if role, _ := msgs[last].(map[string]any)["role"].(string); role != "user" {
			t.Errorf("request %d: turn context must ride the last USER message, found role %q", i, role)
		}
	}

	if after := renderHistory(history); after != before {
		t.Errorf("turn context was written back into history:\nbefore %q\nafter  %q", before, after)
	}
}

func renderHistory(h []*genai.Content) string {
	var b strings.Builder
	for _, c := range h {
		b.WriteString(string(c.Role))
		for _, p := range c.Parts {
			b.WriteString("|" + p.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func marshalString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
