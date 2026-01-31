package ui

import (
	"fmt"
)

// formatTokens formats token counts in a human-readable way.
// Examples: 1234 -> "1.2K", 1234567 -> "1.2M", 123 -> "123"
func formatTokens(tokens int) string {
	const (
		K = 1000
		M = 1000000
		B = 1000000000
	)

	switch {
	case tokens >= B:
		return fmt.Sprintf("%.1fB", float64(tokens)/float64(B))
	case tokens >= M:
		return fmt.Sprintf("%.1fM", float64(tokens)/float64(M))
	case tokens >= K:
		return fmt.Sprintf("%.1fK", float64(tokens)/float64(K))
	default:
		return fmt.Sprintf("%d", tokens)
	}
}
