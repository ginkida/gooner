package ui

import (
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// getTextSelectionHint returns a platform/terminal-specific hint for text selection.
func getTextSelectionHint() string {
	term := os.Getenv("TERM_PROGRAM")
	switch {
	case runtime.GOOS == "darwin" && term == "iTerm.app":
		return "Ctrl+G for select mode, or Option+drag"
	case runtime.GOOS == "darwin" && term == "Apple_Terminal":
		return "Ctrl+G for select mode, or Fn+drag"
	case runtime.GOOS == "darwin":
		return "Ctrl+G for select mode"
	default:
		return "Ctrl+G for select mode, or Shift+drag"
	}
}

// copyViaOSC52 copies text to clipboard via OSC 52 escape sequence.
func copyViaOSC52(text string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Fprintf(os.Stderr, "\033]52;c;%s\a", encoded)
}

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

// Welcome displays a minimalist welcome message using lipgloss borders.
func (m *Model) Welcome() {
	titleStyle := lipgloss.NewStyle().Foreground(ColorPrimary).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	infoStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	// Build info line: ~/project • model • 0%
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

	// 4 lines of content
	line1 := titleStyle.Render("GOKIN")
	line2 := infoStyle.Render(dir+" · "+modelName+" · "+contextStr)
	line3 := dimStyle.Render("Ctrl+P commands · Shift+Tab plan mode")
	line4 := dimStyle.Render(getTextSelectionHint())

	content := line1 + "\n" + line2 + "\n" + line3 + "\n" + line4

	// Wrap in lipgloss rounded border
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorDim).
		Padding(1, 2).
		Align(lipgloss.Center)

	m.output.AppendLine(boxStyle.Render(content))
	m.output.AppendLine("")

	// Suggestion prompts for new users
	suggestionStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	promptStyle := lipgloss.NewStyle().Foreground(ColorAccent).Italic(true)
	suggestions := suggestionStyle.Render("  Try: ") +
		promptStyle.Render("\"describe this project\"") +
		suggestionStyle.Render(" · ") +
		promptStyle.Render("\"find bugs in main.go\"") +
		suggestionStyle.Render(" · ") +
		promptStyle.Render("/help")
	m.output.AppendLine(suggestions)
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

// renderScratchpad renders the agent scratchpad.
func (m Model) renderScratchpad() string {
	if m.scratchpad == "" {
		return ""
	}

	// Style for the scratchpad box - Distinct from tasks
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorAccent).
		Padding(0, 1).
		MarginBottom(1).
		Width(m.width - 4)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorPrimary)

	contentStyle := lipgloss.NewStyle().
		Foreground(ColorText)

	var builder strings.Builder
	builder.WriteString(titleStyle.Render(" Scratchpad"))
	builder.WriteString("\n")
	builder.WriteString(contentStyle.Render(m.scratchpad))

	return boxStyle.Render(builder.String())
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

// shortenPath shortens a path to fit within maxLen runes while preserving the filename.
// Uses smart truncation: shows directory prefix + ... + filename.
// Unicode-safe: uses rune count instead of byte length.
func shortenPath(path string, maxLen int) string {
	if utf8.RuneCountInString(path) <= maxLen {
		return path
	}

	// Try to show ~/... for home directory paths
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}

	runes := []rune(path)
	if len(runes) <= maxLen {
		return path
	}

	// Smart truncation: preserve filename and show as much context as possible
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		// No slash, truncate from the left
		return "..." + string(runes[len(runes)-maxLen+3:])
	}

	filename := path[lastSlash:] // includes leading /
	filenameRunes := []rune(filename)

	// If filename alone fits, show directory prefix + ... + filename
	if len(filenameRunes) < maxLen-6 { // 6 = len("...") + some prefix
		availableForDir := maxLen - len(filenameRunes) - 3
		if availableForDir > 3 {
			dirRunes := []rune(path)
			return string(dirRunes[:availableForDir]) + "..." + filename
		}
	}

	// Fallback: truncate from the left to always show filename
	return "..." + string(runes[len(runes)-maxLen+3:])
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

// renderResponseMetadata renders the response metadata as a compact dim footer.
// Format: 1.2k in · 3.4k out · 4.1s
func (m Model) renderResponseMetadata(meta ResponseMetadataMsg) string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var parts []string

	// Input tokens
	if meta.InputTokens > 0 {
		var s string
		if meta.InputTokens >= 1000 {
			s = fmt.Sprintf("%.1fk in", float64(meta.InputTokens)/1000)
		} else {
			s = fmt.Sprintf("%d in", meta.InputTokens)
		}
		parts = append(parts, s)
	}

	// Output tokens
	if meta.OutputTokens > 0 {
		var s string
		if meta.OutputTokens >= 1000 {
			s = fmt.Sprintf("%.1fk out", float64(meta.OutputTokens)/1000)
		} else {
			s = fmt.Sprintf("%d out", meta.OutputTokens)
		}
		parts = append(parts, s)
	}

	// Duration
	if meta.Duration > 0 {
		if meta.Duration < time.Second {
			parts = append(parts, fmt.Sprintf("%dms", meta.Duration.Milliseconds()))
		} else {
			parts = append(parts, fmt.Sprintf("%.1fs", meta.Duration.Seconds()))
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return dimStyle.Render(strings.Join(parts, " · "))
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
