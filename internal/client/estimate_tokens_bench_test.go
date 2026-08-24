package client

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// estimateTokens runs on EVERY token count for a non-Anthropic base URL —
// which is every provider gokin ships except native Anthropic — and it walks
// the whole serialized request each time, so its cost scales with the entire
// accumulated history rather than with the new turn. On a 1M-context provider
// that history reaches megabytes, which is why it is worth a number rather
// than an assumption.
//
// Measured on an M4: ~24µs at 10KB of history, ~330µs at 400KB, ~1.1ms at
// 2.4MB. Linear, and small enough beside a network round trip that it is not
// worth optimising. These exist to catch a future change that makes it
// super-linear, not because anything here is slow today.
func benchHistory(turns int, chars int) []*genai.Content {
	body := strings.Repeat("the quick brown fox jumps over the lazy dog. ", chars/45+1)
	out := make([]*genai.Content, 0, turns)
	for i := 0; i < turns; i++ {
		role := genai.RoleUser
		if i%2 == 1 {
			role = genai.RoleModel
		}
		out = append(out, genai.NewContentFromText(fmt.Sprintf("turn %d: %s", i, body), genai.Role(role)))
	}
	return out
}

func benchClient(b *testing.B) *AnthropicClient {
	b.Helper()
	c, err := NewAnthropicClient(AnthropicConfig{
		APIKey: "k", BaseURL: "https://example.test/anthropic", Provider: "glm", Model: "glm-5.2",
	})
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func benchEstimate(b *testing.B, turns, chars int) {
	c := benchClient(b)
	h := benchHistory(turns, chars)
	total := 0
	for _, m := range h {
		for _, p := range m.Parts {
			total += len(p.Text)
		}
	}
	b.ReportMetric(float64(total)/1024/1024, "MB_history")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.estimateTokens(h, "glm-5.2", "system prompt", "", nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEstimateTokens_Small(b *testing.B)  { benchEstimate(b, 20, 500) }
func BenchmarkEstimateTokens_Medium(b *testing.B) { benchEstimate(b, 200, 2000) }
func BenchmarkEstimateTokens_Large(b *testing.B)  { benchEstimate(b, 600, 4000) }
