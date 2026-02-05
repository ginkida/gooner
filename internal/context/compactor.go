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

// compactFileContent optimizes file content truncation.
func (c *ResultCompactor) compactFileContent(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 50 {
		return c.Compact(result)
	}

	// For files, keep more from the beginning
	headCount := 30
	tailCount := 10
	omittedCount := len(lines) - headCount - tailCount

	var builder strings.Builder
	builder.WriteString(strings.Join(lines[:headCount], "\n"))
	builder.WriteString(fmt.Sprintf("\n\n...[%d lines omitted, file continues]...\n\n", omittedCount))
	builder.WriteString(strings.Join(lines[len(lines)-tailCount:], "\n"))

	return tools.ToolResult{
		Content: builder.String(),
		Data:    result.Data,
		Success: true,
	}
}

// compactCommandOutput optimizes command output truncation.
func (c *ResultCompactor) compactCommandOutput(result tools.ToolResult) tools.ToolResult {
	// Command output: keep recent output (tail) more than beginning
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 30 {
		return c.Compact(result)
	}

	headCount := 5
	tailCount := 20
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

// compactSearchResults optimizes grep/search results.
func (c *ResultCompactor) compactSearchResults(result tools.ToolResult) tools.ToolResult {
	lines := strings.Split(result.Content, "\n")
	if len(lines) <= 50 {
		return c.Compact(result)
	}

	// For search: show first matches, indicate more exist
	maxResults := 40
	omittedCount := len(lines) - maxResults

	var builder strings.Builder
	builder.WriteString(strings.Join(lines[:maxResults], "\n"))
	builder.WriteString(fmt.Sprintf("\n\n...[%d more matches not shown]", omittedCount))

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
