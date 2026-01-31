package context

import (
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// ResponseCompressor intelligently compresses function responses to save tokens.
type ResponseCompressor struct {
	maxChars    int
	keepHeaders []string // Important patterns to keep
}

// NewResponseCompressor creates a new response compressor.
func NewResponseCompressor(maxChars int) *ResponseCompressor {
	return &ResponseCompressor{
		maxChars: maxChars,
		keepHeaders: []string{
			"error", "failed", "exception", "warning",
			"success", "completed", "done",
			"file not found", "no such file",
		},
	}
}

// CompressFunctionResponse compresses a function response part.
func (rc *ResponseCompressor) CompressFunctionResponse(part *genai.FunctionResponse) *genai.FunctionResponse {
	if part == nil || part.Response == nil {
		return part
	}

	compressed := &genai.FunctionResponse{
		Name:     part.Name,
		Response: make(map[string]any),
	}

	// Process each field in the response
	for key, value := range part.Response {
		compressed.Response[key] = rc.compressValue(key, value)
	}

	return compressed
}

// compressValue compresses a response value based on its type and content.
func (rc *ResponseCompressor) compressValue(key string, value any) any {
	// Keep important fields as-is
	if rc.isImportantField(key) {
		return value
	}

	switch v := value.(type) {
	case string:
		return rc.compressString(key, v)
	case map[string]any:
		return rc.compressMap(v)
	case []any:
		return rc.compressArray(v)
	default:
		return value
	}
}

// compressString compresses a string value.
func (rc *ResponseCompressor) compressString(key string, s string) string {
	// If under limit, keep as-is
	if len(s) <= rc.maxChars {
		return s
	}

	// Check if it contains important keywords
	lower := strings.ToLower(s)
	for _, keyword := range rc.keepHeaders {
		if strings.Contains(lower, keyword) {
			// Keep more context for important messages
			if len(s) <= rc.maxChars*2 {
				return s
			}
		}
	}

	// Truncate with ellipsis
	truncated := s[:rc.maxChars]
	// Try to truncate at a reasonable break point
	lastNewline := strings.LastIndex(truncated, "\n")
	lastPeriod := strings.LastIndex(truncated, ".")
	bestBreak := max(lastNewline, lastPeriod)

	if bestBreak > rc.maxChars/2 {
		truncated = truncated[:bestBreak]
	}

	return truncated + "... [truncated]"
}

// compressMap compresses a map value.
func (rc *ResponseCompressor) compressMap(m map[string]any) map[string]any {
	compressed := make(map[string]any)

	for key, value := range m {
		compressed[key] = rc.compressValue(key, value)
	}

	return compressed
}

// compressArray compresses an array value.
func (rc *ResponseCompressor) compressArray(arr []any) []any {
	if len(arr) <= 10 {
		return arr // Keep small arrays as-is
	}

	// For large arrays, keep first 3, last 3, and note the count
	result := make([]any, 0, 7)
	result = append(result, arr[:3]...)

	// Add a note about skipped items
	note := map[string]any{
		"_note":    "[...]",
		"_skipped": len(arr) - 6,
		"_total":   len(arr),
	}
	result = append(result, note)

	result = append(result, arr[len(arr)-3:]...)

	return result
}

// isImportantField checks if a field should be kept as-is.
func (rc *ResponseCompressor) isImportantField(key string) bool {
	importantKeys := []string{
		"error", "success", "failed", "status",
		"exit_code", "return_code",
		"path", "file_path",
		"command", "tool",
	}

	lower := strings.ToLower(key)
	for _, important := range importantKeys {
		if strings.Contains(lower, important) {
			return true
		}
	}

	return false
}

// CompressContent compresses a content part if it's a function response.
func (rc *ResponseCompressor) CompressContent(part *genai.Part) *genai.Part {
	if part.FunctionResponse != nil {
		return &genai.Part{
			FunctionResponse: rc.CompressFunctionResponse(part.FunctionResponse),
		}
	}
	return part
}

// CompressContents compresses all function responses in a content slice.
func (rc *ResponseCompressor) CompressContents(contents []*genai.Content) []*genai.Content {
	if len(contents) == 0 {
		return contents
	}

	compressed := make([]*genai.Content, len(contents))

	for i, content := range contents {
		newContent := &genai.Content{
			Role:  content.Role,
			Parts: make([]*genai.Part, len(content.Parts)),
		}

		for j, part := range content.Parts {
			newContent.Parts[j] = rc.CompressContent(part)
		}

		compressed[i] = newContent
	}

	return compressed
}

// SmartTruncate compresses a text response intelligently based on its structure.
func SmartTruncate(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}

	// Check for structured data (JSON, YAML)
	if strings.HasPrefix(strings.TrimSpace(text), "{") ||
		strings.HasPrefix(strings.TrimSpace(text), "[") {
		return truncateStructured(text, maxChars)
	}

	// Check for code blocks
	if strings.Contains(text, "```") {
		return truncateCodeBlock(text, maxChars)
	}

	// Check for list items
	lines := strings.Split(text, "\n")
	if len(lines) > 10 {
		return truncateList(lines, maxChars)
	}

	// Default: truncate with smart break point
	truncated := text[:maxChars]
	lastNewline := strings.LastIndex(truncated, "\n")
	lastPeriod := strings.LastIndex(truncated, ".")
	bestBreak := max(lastNewline, lastPeriod)

	if bestBreak > maxChars/2 {
		truncated = truncated[:bestBreak]
	}

	return truncated + "... [truncated]"
}

// truncateStructured truncates structured data (JSON/YAML) more carefully.
func truncateStructured(text string, maxChars int) string {
	// Try to find a reasonable break point
	brackets := 0
	for i, char := range text {
		if i >= maxChars {
			break
		}
		if char == '{' || char == '[' {
			brackets++
		} else if char == '}' || char == ']' {
			brackets--
		}
	}

	// Truncate at a balanced bracket point if possible
	truncated := text[:maxChars]
	return truncated + "... [truncated]"
}

// truncateCodeBlock truncates code while preserving the language marker.
func truncateCodeBlock(text string, maxChars int) string {
	// Find the first code block
	startIdx := strings.Index(text, "```")
	if startIdx == -1 {
		return text[:maxChars] + "... [truncated]"
	}

	// Keep the language marker and some code
	endIdx := strings.Index(text[startIdx+3:], "```")
	if endIdx == -1 {
		// Unclosed code block, truncate in the middle
		return text[:maxChars] + "... [truncated]"
	}

	// Keep first part of code block
	codeBlock := text[:startIdx+3+endIdx]
	if len(codeBlock) > maxChars {
		return text[:maxChars] + "... [truncated]"
	}

	return codeBlock + "\n... [truncated]"
}

// truncateList truncates a list while keeping some items from start and end.
func truncateList(lines []string, maxChars int) string {
	// Keep first 3 lines
	var result strings.Builder
	currentLen := 0

	for _, line := range lines[:3] {
		result.WriteString(line)
		result.WriteString("\n")
		currentLen += len(line) + 1
	}

	// Add skip note
	skipNote := fmt.Sprintf("\n... [%d items skipped] ...\n", len(lines)-6)
	result.WriteString(skipNote)
	currentLen += len(skipNote)

	// Add last 3 lines if space permits
	if currentLen < maxChars {
		for _, line := range lines[len(lines)-3:] {
			if currentLen+len(line)+1 > maxChars {
				break
			}
			result.WriteString(line)
			result.WriteString("\n")
			currentLen += len(line) + 1
		}
	}

	return result.String()
}
