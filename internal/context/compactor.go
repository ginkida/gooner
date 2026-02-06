package context

import (
	"fmt"
	"strings"

	"gokin/internal/tools"
)

// ResultCompactor compacts tool results to reduce token usage.
type ResultCompactor struct {
	maxChars      int
	headLines     int
	tailLines     int
	smartTruncate bool
}

// NewResultCompactor creates a new result compactor.
func NewResultCompactor(maxChars int) *ResultCompactor {
	if maxChars <= 0 {
		maxChars = 10000 // default
	}
	return &ResultCompactor{
		maxChars:      maxChars,
		headLines:     10,
		tailLines:     5,
		smartTruncate: true,
	}
}

// Compact compacts a tool result if it exceeds the maximum size.
// IMPORTANT: Error messages and stack traces are NEVER truncated.
func (c *ResultCompactor) Compact(result tools.ToolResult) tools.ToolResult {
	// NEVER compact error results - preserve full error context
	if !result.Success {
		return result
	}

	if len(result.Content) <= c.maxChars {
		return result
	}

	// Check if content contains error indicators - preserve if so
	if c.containsErrorIndicators(result.Content) {
		return c.compactWithErrorPreservation(result)
	}

	// Try smart truncation first
	if c.smartTruncate {
		compacted := c.smartTruncateContent(result.Content)
		if compacted != result.Content {
			return tools.ToolResult{
				Content: compacted,
				Data:    result.Data,
				Success: true,
			}
		}
	}

	// Fall back to simple truncation
	truncated := result.Content[:c.maxChars]
	truncated += fmt.Sprintf("\n...[truncated, showing %d of %d chars]", c.maxChars, len(result.Content))

	return tools.ToolResult{
		Content: truncated,
		Data:    result.Data,
		Success: true,
	}
}

// containsErrorIndicators checks if content contains error-related information.
func (c *ResultCompactor) containsErrorIndicators(content string) bool {
	lowerContent := strings.ToLower(content)
	errorIndicators := []string{
		"error:", "error(", "err:", "err =",
		"panic:", "fatal:", "failed:",
		"stack trace:", "stacktrace:",
		"exception:", "traceback:",
		"at line", "at file",
		"undefined:", "cannot use",
		"--- fail", "fail:",
		"permission denied", "access denied",
		"not found", "no such file",
		"syntax error", "parse error",
		"compilation failed", "build failed",
	}
	for _, indicator := range errorIndicators {
		if strings.Contains(lowerContent, indicator) {
			return true
		}
	}
	return false
}

// compactWithErrorPreservation compacts content while preserving error information.
func (c *ResultCompactor) compactWithErrorPreservation(result tools.ToolResult) tools.ToolResult {
	content := result.Content
	lines := strings.Split(content, "\n")

	// Identify error-related lines
	var errorLines []string
	var normalLines []string

	for _, line := range lines {
		if c.isErrorLine(line) {
			errorLines = append(errorLines, line)
		} else {
			normalLines = append(normalLines, line)
		}
	}

	// Always preserve ALL error lines
	var builder strings.Builder

	// If we have error lines, they take priority
	if len(errorLines) > 0 {
		builder.WriteString("=== Error Information (preserved) ===\n")
		builder.WriteString(strings.Join(errorLines, "\n"))
		builder.WriteString("\n\n")
	}

	// Add truncated normal output if there's room
	errorLen := builder.Len()
	remainingChars := c.maxChars - errorLen

	if remainingChars > 500 && len(normalLines) > 0 {
		normalContent := strings.Join(normalLines, "\n")
		if len(normalContent) > remainingChars {
			// Truncate normal content
			builder.WriteString("=== Output (truncated) ===\n")
			truncated := normalContent[:remainingChars-100]
			builder.WriteString(truncated)
			builder.WriteString(fmt.Sprintf("\n...[%d chars omitted]", len(normalContent)-remainingChars+100))
		} else {
			builder.WriteString("=== Output ===\n")
			builder.WriteString(normalContent)
		}
	}

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: result.Success,
	}
}

// isErrorLine checks if a line contains error-related information.
func (c *ResultCompactor) isErrorLine(line string) bool {
	lowerLine := strings.ToLower(line)

	// Direct error indicators
	errorPrefixes := []string{
		"error", "err:", "panic", "fatal", "failed",
		"warning:", "warn:", "exception", "traceback",
	}
	for _, prefix := range errorPrefixes {
		if strings.HasPrefix(lowerLine, prefix) || strings.Contains(lowerLine, " "+prefix) {
			return true
		}
	}

	// Stack trace indicators
	stackIndicators := []string{
		".go:", ".py:", ".js:", ".ts:", ".java:", // file:line patterns
		"at line", "at file", "in function",
		"goroutine ", "runtime.", "panic(",
	}
	for _, indicator := range stackIndicators {
		if strings.Contains(lowerLine, indicator) {
			return true
		}
	}

	// Go-specific error patterns
	if strings.Contains(line, ":") {
		// file.go:123:45: error message
		parts := strings.SplitN(line, ":", 4)
		if len(parts) >= 3 && strings.HasSuffix(parts[0], ".go") {
			return true
		}
	}

	return false
}

// smartTruncateContent performs smart line-based truncation.
func (c *ResultCompactor) smartTruncateContent(content string) string {
	lines := strings.Split(content, "\n")

	// If not many lines, just do char truncation
	if len(lines) <= c.headLines+c.tailLines+1 {
		if len(content) > c.maxChars {
			return content[:c.maxChars] + "\n...[truncated]"
		}
		return content
	}

	// Keep head and tail lines
	headLines := lines[:c.headLines]
	tailLines := lines[len(lines)-c.tailLines:]
	omittedCount := len(lines) - c.headLines - c.tailLines

	// Build truncated content
	var builder strings.Builder
	builder.WriteString(strings.Join(headLines, "\n"))
	builder.WriteString(fmt.Sprintf("\n\n...[%d lines omitted]...\n\n", omittedCount))
	builder.WriteString(strings.Join(tailLines, "\n"))

	result := builder.String()

	// If still too long, apply char limit
	if len(result) > c.maxChars {
		result = result[:c.maxChars] + "\n...[truncated]"
	}

	return result
}

// CompactForType compacts content based on the tool type.
func (c *ResultCompactor) CompactForType(toolName string, result tools.ToolResult) tools.ToolResult {
	if !result.Success || len(result.Content) <= c.maxChars {
		return result
	}

	switch toolName {
	case "read":
		return c.compactFileContent(result)
	case "bash":
		return c.compactCommandOutput(result)
	case "grep":
		return c.compactSearchResults(result)
	case "glob":
		return c.compactFileList(result)
	case "tree":
		return c.compactTreeOutput(result)
	default:
		return c.Compact(result)
	}
}

// compactFileContent optimizes file content by preserving function/type signatures.
func (c *ResultCompactor) compactFileContent(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 50 {
		return c.Compact(result)
	}

	// Extract function/type signatures and keep them
	var signatures []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isSignatureLine(trimmed) {
			signatures = append(signatures, line)
		}
	}

	headCount := 30
	tailCount := 10
	omittedCount := len(lines) - headCount - tailCount

	var builder strings.Builder
	builder.WriteString(strings.Join(lines[:headCount], "\n"))

	// Insert extracted signatures if we found any in the omitted section
	if len(signatures) > 0 {
		omittedSigs := filterOmittedSignatures(signatures, lines, headCount, len(lines)-tailCount)
		if len(omittedSigs) > 0 {
			builder.WriteString(fmt.Sprintf("\n\n...[%d lines omitted, key signatures preserved:]...\n", omittedCount))
			builder.WriteString(strings.Join(omittedSigs, "\n"))
			builder.WriteString("\n\n")
		} else {
			builder.WriteString(fmt.Sprintf("\n\n...[%d lines omitted]...\n\n", omittedCount))
		}
	} else {
		builder.WriteString(fmt.Sprintf("\n\n...[%d lines omitted]...\n\n", omittedCount))
	}

	builder.WriteString(strings.Join(lines[len(lines)-tailCount:], "\n"))

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: true,
	}
}

// compactCommandOutput optimizes command output with error-aware truncation.
func (c *ResultCompactor) compactCommandOutput(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 30 {
		return c.Compact(result)
	}

	// If output contains errors, keep more from the tail (where errors usually appear)
	headCount := 5
	tailCount := 20
	if c.containsErrorIndicators(result.Content) {
		headCount = 3
		tailCount = 25
	}

	omittedCount := len(lines) - headCount - tailCount

	var builder strings.Builder
	builder.WriteString(strings.Join(lines[:headCount], "\n"))
	builder.WriteString(fmt.Sprintf("\n\n...[%d lines omitted]...\n\n", omittedCount))
	builder.WriteString(strings.Join(lines[len(lines)-tailCount:], "\n"))

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: true,
	}
}

// compactSearchResults optimizes grep/search results by prioritizing error-related matches.
func (c *ResultCompactor) compactSearchResults(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 50 {
		return c.Compact(result)
	}

	// Separate error-related matches from normal matches
	var errorMatches []string
	var normalMatches []string
	for _, line := range lines {
		if c.isErrorLine(line) {
			errorMatches = append(errorMatches, line)
		} else {
			normalMatches = append(normalMatches, line)
		}
	}

	maxResults := 40
	var builder strings.Builder

	// Error matches always come first
	if len(errorMatches) > 0 {
		builder.WriteString("=== Error-related matches ===\n")
		limit := maxResults / 2
		if limit > len(errorMatches) {
			limit = len(errorMatches)
		}
		builder.WriteString(strings.Join(errorMatches[:limit], "\n"))
		maxResults -= limit
		if len(errorMatches) > limit {
			builder.WriteString(fmt.Sprintf("\n... [%d more error matches]\n", len(errorMatches)-limit))
		}
		builder.WriteString("\n\n=== Other matches ===\n")
	}

	// Fill remaining with normal matches
	if maxResults > 0 && len(normalMatches) > 0 {
		limit := maxResults
		if limit > len(normalMatches) {
			limit = len(normalMatches)
		}
		builder.WriteString(strings.Join(normalMatches[:limit], "\n"))
		remaining := len(normalMatches) - limit
		if remaining > 0 {
			builder.WriteString(fmt.Sprintf("\n\n...[%d more matches not shown, total: %d]", remaining, len(lines)))
		}
	}

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: true,
	}
}

// compactFileList optimizes file listing output.
func (c *ResultCompactor) compactFileList(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 100 {
		return c.Compact(result)
	}

	// For file lists: show sample and count
	sampleSize := 50
	omittedCount := len(lines) - sampleSize

	var builder strings.Builder
	builder.WriteString(strings.Join(lines[:sampleSize], "\n"))
	builder.WriteString(fmt.Sprintf("\n\n...[%d more files not shown, total: %d files]", omittedCount, len(lines)))

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: true,
	}
}

// compactTreeOutput optimizes tree output.
func (c *ResultCompactor) compactTreeOutput(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 100 {
		return c.Compact(result)
	}

	// For tree: show top-level structure
	maxLines := 80
	omittedCount := len(lines) - maxLines

	var builder strings.Builder
	builder.WriteString(strings.Join(lines[:maxLines], "\n"))
	builder.WriteString(fmt.Sprintf("\n\n...[%d more items not shown]", omittedCount))

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: true,
	}
}

// isSignatureLine checks if a line is a function, type, or struct declaration.
func isSignatureLine(line string) bool {
	// Go signatures
	if strings.HasPrefix(line, "func ") || strings.HasPrefix(line, "type ") {
		return true
	}
	// Method signatures (func (receiver) Name)
	if strings.HasPrefix(line, "func (") {
		return true
	}
	// Interface/struct declarations
	if (strings.Contains(line, " struct {") || strings.Contains(line, " interface {")) && !strings.HasPrefix(line, "//") {
		return true
	}
	// Python/JS/TS signatures
	if strings.HasPrefix(line, "def ") || strings.HasPrefix(line, "class ") {
		return true
	}
	if strings.HasPrefix(line, "function ") || strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "const ") {
		return true
	}
	return false
}

// filterOmittedSignatures returns signatures that fall within the omitted line range.
func filterOmittedSignatures(signatures []string, allLines []string, startOmit, endOmit int) []string {
	sigSet := make(map[string]bool)
	for _, sig := range signatures {
		sigSet[sig] = true
	}

	var result []string
	for i := startOmit; i < endOmit && i < len(allLines); i++ {
		if sigSet[allLines[i]] {
			result = append(result, allLines[i])
		}
	}
	// Limit to 20 signatures to avoid excessive output
	if len(result) > 20 {
		result = result[:20]
	}
	return result
}
