package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ToolOutputConfig configures the tool output display behavior.
type ToolOutputConfig struct {
	MaxCollapsedLines int     // Maximum lines to show when collapsed (default: 10)
	MaxCollapsedChars int     // Maximum characters when collapsed (default: 500)
	HeadRatio         float64 // Ratio of lines from start (default: 0.66)
	ShowLineNumbers   bool    // Whether to show line numbers in expanded view
	ExpandHint        string  // Text to show for expand hint
	CollapseHint      string  // Text to show for collapse hint
}

// DefaultToolOutputConfig returns the default configuration.
func DefaultToolOutputConfig() ToolOutputConfig {
	return ToolOutputConfig{
		MaxCollapsedLines: 10,
		MaxCollapsedChars: 500,
		HeadRatio:         0.66,
		ShowLineNumbers:   true,
		ExpandHint:        "e",
		CollapseHint:      "e",
	}
}

// ToolOutputEntry represents a single tool output entry.
type ToolOutputEntry struct {
	ToolName    string
	FullContent string
	Expanded    bool
	Index       int
}

// ToolOutputModel manages the display of tool output with expand/collapse functionality.
type ToolOutputModel struct {
	entries     []ToolOutputEntry
	config      ToolOutputConfig
	styles      *Styles
	AllExpanded bool
}

// NewToolOutputModel creates a new tool output model.
func NewToolOutputModel(styles *Styles) *ToolOutputModel {
	return &ToolOutputModel{
		entries: make([]ToolOutputEntry, 0),
		config:  DefaultToolOutputConfig(),
		styles:  styles,
	}
}

// SetConfig updates the configuration.
func (m *ToolOutputModel) SetConfig(config ToolOutputConfig) {
	m.config = config
}

// AddEntry adds a new tool output entry.
func (m *ToolOutputModel) AddEntry(toolName, content string) int {
	entry := ToolOutputEntry{
		ToolName:    toolName,
		FullContent: content,
		Expanded:    false,
		Index:       len(m.entries),
	}
	m.entries = append(m.entries, entry)
	return entry.Index
}

// ToggleExpand toggles the expand state of an entry.
func (m *ToolOutputModel) ToggleExpand(index int) bool {
	if index < 0 || index >= len(m.entries) {
		return false
	}
	m.entries[index].Expanded = !m.entries[index].Expanded
	return m.entries[index].Expanded
}

// IsExpanded returns whether an entry is expanded.
func (m *ToolOutputModel) IsExpanded(index int) bool {
	if index < 0 || index >= len(m.entries) {
		return false
	}
	return m.entries[index].Expanded
}

// GetLatestIndex returns the index of the most recent entry.
func (m *ToolOutputModel) GetLatestIndex() int {
	if len(m.entries) == 0 {
		return -1
	}
	return len(m.entries) - 1
}

// GetEntry returns the entry at the given index safely.
// Returns nil if the index is out of bounds.
func (m *ToolOutputModel) GetEntry(index int) *ToolOutputEntry {
	if index < 0 || index >= len(m.entries) {
		return nil
	}
	return &m.entries[index]
}

// ToggleLatest toggles the expand state of the most recent entry.
func (m *ToolOutputModel) ToggleLatest() bool {
	return m.ToggleExpand(m.GetLatestIndex())
}

// ToggleAll toggles all entries between expanded and collapsed.
func (m *ToolOutputModel) ToggleAll() {
	m.AllExpanded = !m.AllExpanded
	for i := range m.entries {
		m.entries[i].Expanded = m.AllExpanded
	}
}

// GetSummary returns a compact summary for the entry at the given index.
func (m *ToolOutputModel) GetSummary(index int) string {
	if index < 0 || index >= len(m.entries) {
		return ""
	}
	entry := m.entries[index]
	lineCount := strings.Count(entry.FullContent, "\n") + 1
	info := entry.ToolName
	if entry.FullContent != "" {
		info += fmt.Sprintf(": %d lines", lineCount)
	}
	return fmt.Sprintf("[%s]", info)
}

// NeedsTruncation checks if the content needs truncation.
func (m *ToolOutputModel) NeedsTruncation(content string) bool {
	if len(content) > m.config.MaxCollapsedChars {
		return true
	}
	lines := strings.Split(content, "\n")
	return len(lines) > m.config.MaxCollapsedLines
}

// RenderEntry renders a tool output entry.
func (m *ToolOutputModel) RenderEntry(index int) string {
	if index < 0 || index >= len(m.entries) {
		return ""
	}

	entry := m.entries[index]
	content := entry.FullContent

	if content == "" {
		return ""
	}

	if entry.Expanded || !m.NeedsTruncation(content) {
		// Show full content
		return m.renderFull(content, entry.Expanded && m.NeedsTruncation(content))
	}

	// Show truncated content
	return m.renderTruncated(content)
}

// RenderContent renders content directly without storing it.
func (m *ToolOutputModel) RenderContent(content string, expanded bool) string {
	if content == "" {
		return ""
	}

	if expanded || !m.NeedsTruncation(content) {
		return m.renderFull(content, expanded && m.NeedsTruncation(content))
	}

	return m.renderTruncated(content)
}

// renderFull renders the full content, optionally with line numbers.
func (m *ToolOutputModel) renderFull(content string, _ bool) string {
	var result strings.Builder

	if m.config.ShowLineNumbers {
		lines := strings.Split(content, "\n")
		lineNumWidth := len(fmt.Sprintf("%d", len(lines)))
		lineNumStyle := lipgloss.NewStyle().Foreground(ColorDim)

		for i, line := range lines {
			lineNum := fmt.Sprintf("%*d", lineNumWidth, i+1)
			result.WriteString(lineNumStyle.Render(lineNum) + " │ " + line)
			if i < len(lines)-1 {
				result.WriteString("\n")
			}
		}
	} else {
		result.WriteString(content)
	}

	// No collapse hint needed

	return result.String()
}

// renderTruncated renders truncated content with head and tail.
func (m *ToolOutputModel) renderTruncated(content string) string {
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	// Calculate how many lines from head and tail
	headCount := int(float64(m.config.MaxCollapsedLines) * m.config.HeadRatio)
	tailCount := m.config.MaxCollapsedLines - headCount

	if headCount < 1 {
		headCount = 1
	}
	if tailCount < 1 {
		tailCount = 1
	}

	var result strings.Builder

	// Head lines
	for i := 0; i < headCount && i < totalLines; i++ {
		line := lines[i]
		if len(line) > 100 {
			line = line[:97] + "..."
		}
		result.WriteString(line)
		if i < headCount-1 || tailCount > 0 {
			result.WriteString("\n")
		}
	}

	// Hidden lines indicator - clean and subtle
	hiddenLines := totalLines - headCount - tailCount
	if hiddenLines > 0 {
		dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
		hint := dimStyle.Render(fmt.Sprintf("  ... +%d lines ...", hiddenLines))
		result.WriteString("\n" + hint + "\n")
	}

	// Tail lines
	startTail := totalLines - tailCount
	if startTail < headCount {
		startTail = headCount
	}
	for i := startTail; i < totalLines; i++ {
		line := lines[i]
		if len(line) > 100 {
			line = line[:97] + "..."
		}
		result.WriteString(line)
		if i < totalLines-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}

// ExecutionStatusRenderer renders tool execution status with enhanced user feedback
type ExecutionStatusRenderer struct {
	styles *Styles
}

// NewExecutionStatusRenderer creates a new execution status renderer
func NewExecutionStatusRenderer(styles *Styles) *ExecutionStatusRenderer {
	return &ExecutionStatusRenderer{styles: styles}
}

// RenderValidation renders safety validation results
func (r *ExecutionStatusRenderer) RenderValidation(toolName string, warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}

	var result strings.Builder

	headerStyle := lipgloss.NewStyle().
		Foreground(ColorWarning).
		Bold(true)

	result.WriteString(headerStyle.Render(fmt.Sprintf("⚠ Safety Warnings for %s:", toolName)))
	result.WriteString("\n")

	for _, warning := range warnings {
		warningStyle := lipgloss.NewStyle().Foreground(ColorWarning)
		result.WriteString(warningStyle.Render("  • " + warning))
		result.WriteString("\n")
	}

	return result.String()
}

// RenderStart renders the start of tool execution with user-friendly summary
func (r *ExecutionStatusRenderer) RenderStart(toolName string, summary interface{}) string {
	var result strings.Builder

	// Tool name with icon
	iconStyle := lipgloss.NewStyle().Foreground(ColorInfo)
	nameStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorPrimary)

	var icon string
	switch toolName {
	case "read":
		icon = "📖"
	case "write":
		icon = "✏️ "
	case "edit":
		icon = "🔧"
	case "bash":
		icon = "⚡"
	case "grep", "glob":
		icon = "🔍"
	case "git_log", "git_diff", "git_blame":
		icon = "📜"
	case "web_fetch", "web_search":
		icon = "🌐"
	case "batch":
		icon = "📦"
	default:
		icon = "🔧"
	}

	result.WriteString(iconStyle.Render(icon))
	result.WriteString(" ")
	result.WriteString(nameStyle.Render(fmt.Sprintf("%s", toolName)))

	// Add summary if available
	if s, ok := summary.(string); ok && s != "" {
		summaryStyle := lipgloss.NewStyle().Faint(true)
		result.WriteString(" ")
		result.WriteString(summaryStyle.Render("─ " + s))
	}

	result.WriteString("\n")

	return result.String()
}

// RenderProgress renders progress update for long-running operations
func (r *ExecutionStatusRenderer) RenderProgress(toolName string, elapsed time.Duration) string {
	progressStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true)

	return progressStyle.Render(fmt.Sprintf("  ⏳ %s running... %v", toolName, elapsed.Round(time.Second)))
}

// RenderSuccess renders successful tool execution
func (r *ExecutionStatusRenderer) RenderSuccess(toolName string, duration time.Duration) string {
	successStyle := lipgloss.NewStyle().
		Foreground(ColorSuccess).
		Bold(true)

	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder
	result.WriteString(successStyle.Render("  ✓ " + toolName))
	result.WriteString(" ")
	result.WriteString(dimStyle.Render(fmt.Sprintf("(%v)", duration.Round(time.Millisecond))))
	result.WriteString("\n")

	return result.String()
}

// RenderError renders failed tool execution
func (r *ExecutionStatusRenderer) RenderError(toolName string, errMsg string) string {
	errorStyle := lipgloss.NewStyle().
		Foreground(ColorError).
		Bold(true)

	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder
	result.WriteString(errorStyle.Render("  ✗ " + toolName))
	result.WriteString("\n")
	result.WriteString(dimStyle.Render("    Error: " + errMsg))
	result.WriteString("\n")

	return result.String()
}

// RenderDenied renders permission denied status
func (r *ExecutionStatusRenderer) RenderDenied(toolName, reason string) string {
	denyStyle := lipgloss.NewStyle().
		Foreground(ColorError).
		Bold(true)

	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder
	result.WriteString(denyStyle.Render("  🚫 " + toolName + " denied"))
	result.WriteString("\n")
	if reason != "" {
		result.WriteString(dimStyle.Render("    Reason: " + reason))
		result.WriteString("\n")
	}

	return result.String()
}

// RenderApproved renders permission approved status
func (r *ExecutionStatusRenderer) RenderApproved(toolName string, summary interface{}) string {
	approveStyle := lipgloss.NewStyle().
		Foreground(ColorSuccess).
		Bold(true)

	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder
	result.WriteString(approveStyle.Render("  ✓ " + toolName + " approved"))

	if s, ok := summary.(string); ok && s != "" {
		result.WriteString(" ")
		result.WriteString(dimStyle.Render("─ " + s))
	}
	result.WriteString("\n")

	return result.String()
}

// Clear clears all entries.
func (m *ToolOutputModel) Clear() {
	m.entries = make([]ToolOutputEntry, 0)
}

// EntryCount returns the number of entries.
func (m *ToolOutputModel) EntryCount() int {
	return len(m.entries)
}

// FormatToolOutput formats tool output with smart truncation.
// This is a convenience function that can be used directly without storing entries.
func FormatToolOutput(content string, maxLines int, expanded bool) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	if expanded || len(lines) <= maxLines {
		return content
	}

	// Smart truncation: 66% head, 33% tail
	headCount := int(float64(maxLines) * 0.66)
	tailCount := maxLines - headCount

	if headCount < 1 {
		headCount = 1
	}
	if tailCount < 1 {
		tailCount = 1
	}

	var result strings.Builder

	// Head
	for i := 0; i < headCount && i < len(lines); i++ {
		result.WriteString(lines[i])
		result.WriteString("\n")
	}

	// Hidden indicator - clean and subtle
	hiddenLines := len(lines) - headCount - tailCount
	if hiddenLines > 0 {
		dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
		hint := dimStyle.Render(fmt.Sprintf("... +%d lines ...", hiddenLines))
		result.WriteString(hint)
		result.WriteString("\n")
	}

	// Tail
	startTail := len(lines) - tailCount
	if startTail < headCount {
		startTail = headCount
	}
	for i := startTail; i < len(lines); i++ {
		result.WriteString(lines[i])
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}
