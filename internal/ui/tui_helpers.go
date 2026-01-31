package ui

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// getMacOSBattery removed as battery monitoring is disabled.

// SetBadgeCmd returns a tea.Cmd that sets the iTerm2 badge.
func SetBadgeCmd(badge string) tea.Cmd {
	return func() tea.Msg {
		if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
			encoded := base64.StdEncoding.EncodeToString([]byte(badge))
			fmt.Printf("\033]1337;SetBadgeFormat=%s\a", encoded)
		}
		return nil
	}
}

// ClearBadgeCmd returns a tea.Cmd that clears the iTerm2 badge.
func ClearBadgeCmd() tea.Cmd {
	return func() tea.Msg {
		if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
			fmt.Print("\033]1337;SetBadgeFormat=\a")
		}
		return nil
	}
}

// Welcome displays a minimalist welcome message.
func (m *Model) Welcome() {
	borderStyle := lipgloss.NewStyle().Foreground(ColorDim)
	titleStyle := lipgloss.NewStyle().Foreground(ColorPrimary).Bold(true)
	subtitleStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	infoStyle := lipgloss.NewStyle().Foreground(ColorText)
	accentStyle := lipgloss.NewStyle().Foreground(ColorAccent)
	readyStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	// Build info line: path • model • context%
	dir := m.workDir
	if dir == "" {
		dir = "."
	}
	dir = shortenPath(dir, 30)

	modelName := m.currentModel
	if modelName == "" {
		modelName = "no model"
	}

	contextStr := "0%"
	if m.tokenUsage != nil {
		contextStr = fmt.Sprintf("%.0f%%", m.tokenUsage.PercentUsed*100)
	}

	infoLine := infoStyle.Render(dir) + subtitleStyle.Render(" • ") +
		accentStyle.Render(modelName) + subtitleStyle.Render(" • ") +
		infoStyle.Render(contextStr)

	// Calculate box width based on content
	boxWidth := 49
	infoLineLen := len(dir) + 3 + len(modelName) + 3 + len(contextStr)
	if infoLineLen+6 > boxWidth {
		boxWidth = infoLineLen + 6
	}

	// Render compact card
	m.output.AppendLine(borderStyle.Render("╭" + strings.Repeat("─", boxWidth) + "╮"))
	m.output.AppendLine(borderStyle.Render("│") + strings.Repeat(" ", boxWidth) + borderStyle.Render("│"))

	// Title
	titleText := "GOKIN"
	titlePad := (boxWidth - len(titleText)) / 2
	m.output.AppendLine(borderStyle.Render("│") +
		strings.Repeat(" ", titlePad) + titleStyle.Render(titleText) +
		strings.Repeat(" ", boxWidth-titlePad-len(titleText)) + borderStyle.Render("│"))

	// Subtitle
	subText := "AI-Powered Code Assistant"
	subPad := (boxWidth - len(subText)) / 2
	m.output.AppendLine(borderStyle.Render("│") +
		strings.Repeat(" ", subPad) + subtitleStyle.Render(subText) +
		strings.Repeat(" ", boxWidth-subPad-len(subText)) + borderStyle.Render("│"))

	m.output.AppendLine(borderStyle.Render("│") + strings.Repeat(" ", boxWidth) + borderStyle.Render("│"))

	// Info line
	infoPad := (boxWidth - infoLineLen) / 2
	m.output.AppendLine(borderStyle.Render("│") +
		strings.Repeat(" ", infoPad) + infoLine +
		strings.Repeat(" ", boxWidth-infoPad-infoLineLen) + borderStyle.Render("│"))

	m.output.AppendLine(borderStyle.Render("│") + strings.Repeat(" ", boxWidth) + borderStyle.Render("│"))

	// Ready message
	readyText := "Ready. Press Ctrl+P for commands."
	readyPad := (boxWidth - len(readyText)) / 2
	m.output.AppendLine(borderStyle.Render("│") +
		strings.Repeat(" ", readyPad) + readyStyle.Render(readyText) +
		strings.Repeat(" ", boxWidth-readyPad-len(readyText)) + borderStyle.Render("│"))

	// Planning mode hint
	planHint := "Shift+Tab toggles planning mode."
	planPad := (boxWidth - len(planHint)) / 2
	m.output.AppendLine(borderStyle.Render("│") +
		strings.Repeat(" ", planPad) + readyStyle.Render(planHint) +
		strings.Repeat(" ", boxWidth-planPad-len(planHint)) + borderStyle.Render("│"))

	m.output.AppendLine(borderStyle.Render("│") + strings.Repeat(" ", boxWidth) + borderStyle.Render("│"))
	m.output.AppendLine(borderStyle.Render("╰" + strings.Repeat("─", boxWidth) + "╯"))
	m.output.AppendLine("")
}

// AddSystemMessage adds a system message to the output.
func (m *Model) AddSystemMessage(msg string) {
	infoStyle := lipgloss.NewStyle().
		Foreground(ColorInfo).
		Bold(true).
		MarginBottom(1)
	m.output.AppendLine(infoStyle.Render("  " + msg))
}

// renderTodos renders the current todo items.
func (m Model) renderTodos() string {
	if len(m.todoItems) == 0 {
		return ""
	}

	// Style for the todo box - Enhanced
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorGradient2).
		Padding(0, 1).
		MarginBottom(1)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorAccent)

	itemStyle := lipgloss.NewStyle().
		Foreground(ColorText).
		PaddingLeft(1)

	var builder strings.Builder
	builder.WriteString(titleStyle.Render(" Tasks"))
	builder.WriteString("\n")

	for _, item := range m.todoItems {
		builder.WriteString(itemStyle.Render(item))
		builder.WriteString("\n")
	}

	return boxStyle.Render(strings.TrimSuffix(builder.String(), "\n"))
}

// getCommandHint returns a hint for the current command input.
func (m Model) getCommandHint(input string) string {
	// Extract command name
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return ""
	}

	cmd := strings.TrimPrefix(parts[0], "/")

	// Command hints map
	hints := map[string]string{
		"help":        "Show all available commands and their usage",
		"clear":       "Clear the current conversation history",
		"save":        "Save the current session to disk",
		"resume":      "Resume a previously saved session",
		"sessions":    "List all saved sessions",
		"undo":        "Undo the last file change",
		"commit":      "Create a git commit with AI-generated message",
		"pr":          "Create a pull request",
		"checkpoint":  "Save a checkpoint of the current state",
		"checkpoints": "List all saved checkpoints",
		"restore":     "Restore a previously saved checkpoint",
		"init":        "Initialize project configuration",
		"doctor":      "Run diagnostics to check for issues",
		"config":      "Show or edit configuration",
		"cost":        "Show token usage and estimated costs",
		"model":       "Switch AI model",
		"browse":      "Browse project files",
		"git-status":  "Show git repository status",
		"clear-todos": "Clear the todo list",
		"plan":        "Toggle planning mode (or press Shift+Tab)",
		"copy":        "Copy text, --last for AI response, --all for full chat",
		"paste":       "Paste from clipboard",
	}

	if hint, ok := hints[cmd]; ok {
		return hint
	}

	// Partial match for autocomplete
	for fullCmd, hint := range hints {
		if strings.HasPrefix(fullCmd, cmd) {
			return hint
		}
	}

	return ""
}

// shortenPath shortens a path to fit within maxLen while preserving the filename.
// Uses smart truncation: shows directory prefix + ... + filename
func shortenPath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}

	// Try to show ~/... for home directory paths
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}

	if len(path) <= maxLen {
		return path
	}

	// Smart truncation: preserve filename and show as much context as possible
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		// No slash, truncate from the left
		return "..." + path[len(path)-maxLen+3:]
	}

	filename := path[lastSlash:] // includes leading /

	// If filename alone fits, show directory prefix + ... + filename
	if len(filename) < maxLen-6 { // 6 = len("...") + some prefix
		availableForDir := maxLen - len(filename) - 3
		if availableForDir > 3 {
			return path[:availableForDir] + "..." + filename
		}
	}

	// Fallback: truncate from the left to always show filename
	return "..." + path[len(path)-maxLen+3:]
}

// extractToolInfo extracts displayable info from tool arguments.
// This is used as a fallback when the switch statement doesn't match.
func extractToolInfo(args map[string]any) string {
	if args == nil {
		return ""
	}

	// Priority keys - check in order of preference
	priorityKeys := []string{
		"file_path",
		"path",
		"directory_path",
		"command",
		"pattern",
		"query",
		"url",
	}

	for _, key := range priorityKeys {
		if val, ok := args[key]; ok {
			switch v := val.(type) {
			case string:
				if v != "" {
					return shortenPath(v, 50)
				}
			}
		}
	}
	return ""
}

// formatDuration formats a duration for display.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

// renderResponseMetadata renders the response metadata footer.
func (m Model) renderResponseMetadata(meta ResponseMetadataMsg) string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	accentStyle := lipgloss.NewStyle().Foreground(ColorAccent)
	mutedStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	var parts []string

	// Model name (shortened)
	if meta.Model != "" {
		modelName := shortenModelName(meta.Model)
		parts = append(parts, accentStyle.Render(modelName))
	}

	// Token count
	totalTokens := meta.InputTokens + meta.OutputTokens
	if totalTokens > 0 {
		var tokenStr string
		if totalTokens >= 1000 {
			tokenStr = fmt.Sprintf("%.1fk tokens", float64(totalTokens)/1000)
		} else {
			tokenStr = fmt.Sprintf("%d tokens", totalTokens)
		}
		parts = append(parts, mutedStyle.Render(tokenStr))
	}

	// Duration
	if meta.Duration > 0 {
		var durationStr string
		if meta.Duration < time.Second {
			durationStr = fmt.Sprintf("%dms", meta.Duration.Milliseconds())
		} else {
			durationStr = fmt.Sprintf("%.1fs", meta.Duration.Seconds())
		}
		parts = append(parts, mutedStyle.Render(durationStr))
	}

	// Tools used count
	if len(meta.ToolsUsed) > 0 {
		toolStr := fmt.Sprintf("%d tools", len(meta.ToolsUsed))
		parts = append(parts, mutedStyle.Render(toolStr))
	}

	if len(parts) == 0 {
		return ""
	}

	// Build the footer line with separators
	content := strings.Join(parts, dimStyle.Render(" | "))

	// Create centered divider line
	dividerChar := ""
	contentWidth := lipgloss.Width(content)
	availableWidth := m.width - 4
	if availableWidth < contentWidth+4 {
		availableWidth = contentWidth + 4
	}

	sideWidth := (availableWidth - contentWidth - 2) / 2
	if sideWidth < 3 {
		sideWidth = 3
	}

	leftDivider := dimStyle.Render(strings.Repeat(dividerChar, sideWidth))
	rightDivider := dimStyle.Render(strings.Repeat(dividerChar, sideWidth))

	return leftDivider + " " + content + " " + rightDivider
}

// AppendOutput appends text to the output.
func (m *Model) AppendOutput(text string) {
	m.output.AppendText(text)
}

// AppendOutputLine appends a line to the output.
func (m *Model) AppendOutputLine(text string) {
	m.output.AppendLine(text)
}

// AppendMarkdown appends markdown to the output.
func (m *Model) AppendMarkdown(text string) {
	m.output.AppendMarkdown(text)
}

// LoadInputHistory loads input history from file.
func (m *Model) LoadInputHistory() error {
	return m.input.LoadHistory()
}

// SaveInputHistory saves input history to file.
func (m *Model) SaveInputHistory() error {
	return m.input.SaveHistory()
}

// ClearOutput clears the output viewport.
func (m *Model) ClearOutput() {
	m.output.Clear()
}

// ResetInput clears the input field.
func (m *Model) ResetInput() {
	m.input.Reset()
}

// printWelcomeTips is no longer used - tips are in Welcome()
func (m *Model) printWelcomeTips() {}

// StreamText sends a stream text message.
func StreamText(text string) tea.Cmd {
	return func() tea.Msg {
		return StreamTextMsg(text)
	}
}

// ToolCall sends a tool call message.
func ToolCall(name string, args map[string]any) tea.Cmd {
	return func() tea.Msg {
		return ToolCallMsg{Name: name, Args: args}
	}
}

// ToolResult sends a tool result message.
func ToolResult(result string) tea.Cmd {
	return func() tea.Msg {
		return ToolResultMsg(result)
	}
}

// ToolProgress sends a tool progress message (heartbeat for long operations).
func ToolProgress(name string, elapsed time.Duration) tea.Cmd {
	return func() tea.Msg {
		return ToolProgressMsg{Name: name, Elapsed: elapsed}
	}
}

// ResponseDone signals that a response is complete.
func ResponseDone() tea.Cmd {
	return func() tea.Msg {
		return ResponseDoneMsg{}
	}
}

// SendError sends an error message.
func SendError(err error) tea.Cmd {
	return func() tea.Msg {
		return ErrorMsg(err)
	}
}

// UpdateTodos updates the todo list display.
func UpdateTodos(items []string) tea.Cmd {
	return func() tea.Msg {
		return TodoUpdateMsg(items)
	}
}

// PermissionRequest sends a permission request message.
func PermissionRequest(toolName string, args map[string]any, riskLevel, reason string) tea.Cmd {
	return func() tea.Msg {
		return PermissionRequestMsg{
			ToolName:  toolName,
			Args:      args,
			RiskLevel: riskLevel,
			Reason:    reason,
		}
	}
}

// QuestionRequest sends a question request message.
func QuestionRequest(question string, options []string, defaultOpt string) tea.Cmd {
	return func() tea.Msg {
		return QuestionRequestMsg{
			Question: question,
			Options:  options,
			Default:  defaultOpt,
		}
	}
}

// PlanApprovalRequest sends a plan approval request message.
func PlanApprovalRequest(title, description string, steps []PlanStepInfo) tea.Cmd {
	return func() tea.Msg {
		return PlanApprovalRequestMsg{
			Title:       title,
			Description: description,
			Steps:       steps,
		}
	}
}

// ResponseMetadata sends a response metadata message.
func ResponseMetadata(model string, inputTokens, outputTokens int, duration time.Duration, toolsUsed []string) tea.Cmd {
	return func() tea.Msg {
		return ResponseMetadataMsg{
			Model:        model,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Duration:     duration,
			ToolsUsed:    toolsUsed,
		}
	}
}
