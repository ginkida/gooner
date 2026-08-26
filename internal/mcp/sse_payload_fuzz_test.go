package mcp

import (
	"strings"
	"testing"
)

// extractSSEPayloads parses responses from THIRD-PARTY MCP servers, which makes
// it the least controlled input in the tree: gokin neither writes those servers
// nor pins their versions, and a Streamable HTTP body can be truncated by a
// dropped connection at any byte. A panic here takes down the receive loop for
// that server, and a hang would take the caller's whole timeout with it.
func TestExtractSSEPayloadsSurvivesHostileBodies(t *testing.T) {
	bodies := []string{
		"", "\n", "\r\n",
		"data:", "data: ", "data:\n\n",
		"data: {\"unterminated",
		"data: null\n\n", "data: []\n\n",
		"event: message\ndata: {}\n\n",
		"data: {}\ndata: {}\n\n",
		": comment only\n\n",
		"data: " + strings.Repeat("A", 100000),
		strings.Repeat("data: {}\n\n", 1000),
		"\x00\x01\x02", "data: \x00\n\n",
		strings.Repeat("\n", 10000),
		strings.Repeat("data: ", 5000),
		"data: {}\r\n\r\n",
	}
	for i, body := range bodies {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("extractSSEPayloads panicked on body %d (%d bytes): %v", i, len(body), r)
				}
			}()
			_ = extractSSEPayloads([]byte(body))
		}()
	}
}
