package client

import (
	"strings"
	"testing"
)

// The provider's stream is the fourth surface gokin does not control, after the
// terminal size, the UI message queue and the model's tool arguments — and the
// least trustworthy of them, since a compatible endpoint is free to emit
// anything the spec did not forbid. processStreamEvent runs per SSE event on a
// live turn, and it carries STATE across them in the accumulator, so the shapes
// that matter are not only malformed events but malformed ORDERS: a delta with
// nothing open, two starts in a row, a stop for a block that never began.
//
// A panic here is not a bad tool result; it is the streaming goroutine dying
// mid-answer with the turn's work already spent.
func TestProcessStreamEventSurvivesHostileEvents(t *testing.T) {
	c, err := NewAnthropicClient(AnthropicConfig{
		APIKey: "k", BaseURL: "https://example.test", Provider: "glm", Model: "glm-5.2",
	})
	if err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("A", 50000)
	events := []map[string]any{
		nil, {},
		{"type": nil}, {"type": 123}, {"type": ""},
		{"type": "unknown_future_event"},
		{"type": map[string]any{"nested": true}},

		// Block lifecycle with every field absent, null or wrongly typed.
		{"type": "content_block_start"},
		{"type": "content_block_start", "content_block": nil},
		{"type": "content_block_start", "content_block": "not-a-map"},
		{"type": "content_block_start", "content_block": map[string]any{}},
		{"type": "content_block_start", "content_block": map[string]any{"type": 5}},
		{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use"}},
		{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use", "id": 1, "name": nil}},
		{"type": "content_block_start", "content_block": map[string]any{"type": "thinking"}},
		{"type": "content_block_start", "index": "not-an-int"},
		{"type": "content_block_delta"},
		{"type": "content_block_delta", "delta": nil},
		{"type": "content_block_delta", "delta": "not-a-map"},
		{"type": "content_block_delta", "delta": map[string]any{}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta"}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": nil}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": 42}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": big}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "input_json_delta", "partial_json": "{\"unterminated"}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "input_json_delta", "partial_json": 7}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "thinking_delta", "thinking": nil}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "signature_delta", "signature": []any{1}}},
		{"type": "content_block_stop"},
		{"type": "content_block_stop", "index": -1},

		// Message-level and error events.
		{"type": "message_delta"},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": 9}},
		{"type": "message_delta", "usage": "not-a-map"},
		{"type": "message_stop"},
		{"type": "message_start", "message": nil},
		{"type": "error"},
		{"type": "error", "error": nil},
		{"type": "error", "error": "flat string"},
		{"type": "error", "error": map[string]any{"type": 1, "message": nil}},
		{"type": "ping"},
	}

	// Pairs cover "this event arriving right after that one".
	for i, a := range events {
		for j, b := range events {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic on events[%d] then events[%d]: %v\n  first=%v\n  second=%v", i, j, r, a, b)
					}
				}()
				acc := &toolCallAccumulator{}
				_ = c.processStreamEvent(a, acc)
				_ = c.processStreamEvent(b, acc)
			}()
		}
	}

	// Triples across the block lifecycle, where ordering carries the most state.
	life := events[7:30]
	for i, a := range life {
		for j, b := range life {
			for k, d := range life {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic on lifecycle triple %d/%d/%d: %v", i, j, k, r)
						}
					}()
					acc := &toolCallAccumulator{}
					_ = c.processStreamEvent(a, acc)
					_ = c.processStreamEvent(b, acc)
					_ = c.processStreamEvent(d, acc)
				}()
			}
		}
	}

	// One long stream through a SHARED accumulator: deeper state than any
	// bounded sequence reaches.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic while replaying the whole hostile stream three times: %v", r)
			}
		}()
		acc := &toolCallAccumulator{}
		for range 3 {
			for _, e := range events {
				_ = c.processStreamEvent(e, acc)
			}
		}
	}()
}
