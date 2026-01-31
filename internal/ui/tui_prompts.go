package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderPermissionPrompt renders the permission prompt UI (Claude Code style — no bordered boxes).
func (m Model) renderPermissionPrompt() string {
	if m.permRequest == nil {
		return ""
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorWarning)
	labelStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	valueStyle := lipgloss.NewStyle().Foreground(ColorText)
	markerStyle := lipgloss.NewStyle().Foreground(ColorDim)

	// Risk indicator
	var riskLabel string
	var riskStyle lipgloss.Style

	switch m.permRequest.RiskLevel {
	case "high":
		riskLabel = "HIGH RISK"
		riskStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorError)
	case "medium":
		riskLabel = "MEDIUM RISK"
		riskStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorWarning)
	default:
		riskLabel = "LOW RISK"
		riskStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorSuccess)
	}

	var builder strings.Builder

	// Title line
	builder.WriteString(titleStyle.Render("? Permission Required") + "  " + riskStyle.Render(riskLabel))
	builder.WriteString("\n")

	// Tool info with ⎿ marker
	builder.WriteString(markerStyle.Render("  ⎿  ") + labelStyle.Render("Tool: ") + valueStyle.Render(m.permRequest.ToolName))
	builder.WriteString("\n")

	// Command/Details
	if len(m.permRequest.Args) > 0 {
		detail := ""
		for _, key := range []string{"command", "file_path", "path", "pattern", "url"} {
			if val, ok := m.permRequest.Args[key].(string); ok && val != "" {
				if len(val) > 60 {
					val = val[:57] + "..."
				}
				detail = val
				break
			}
		}
		if detail != "" {
			builder.WriteString(markerStyle.Render("     ") + labelStyle.Render("Detail: ") + valueStyle.Render(detail))
			builder.WriteString("\n")
		}
	}

	// Reason
	if m.permRequest.Reason != "" {
		reason := m.permRequest.Reason
		if len(reason) > 60 {
			reason = reason[:57] + "..."
		}
		builder.WriteString(markerStyle.Render("     ") + labelStyle.Render("Reason: ") + valueStyle.Render(reason))
		builder.WriteString("\n")
	}

	builder.WriteString("\n")

	// Options
	options := []struct {
		label    string
		shortcut string
	}{
		{"Allow once", "y"},
		{"Allow for session", "a"},
		{"Deny", "n"},
	}

	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary)
	normalStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	for i, opt := range options {
		prefix := "  "
		style := normalStyle
		if i == m.permSelectedOption {
			prefix = "> "
			style = selectedStyle
		}

		optLabel := fmt.Sprintf("[%s] %s", opt.shortcut, opt.label)
		builder.WriteString(prefix + style.Render(optLabel))
		builder.WriteString("\n")
	}

	builder.WriteString("\n")
	builder.WriteString("\n")
	builder.WriteString(normalStyle.Render("  Commands: Ctrl+P"))
	builder.WriteString("\n")

	return builder.String()
}

// formatArgsPreview creates a preview of tool arguments for display.
// Shows up to 5 arguments with smart truncation for strings.
func formatArgsPreview(args map[string]any, maxLen int) string {
	if len(args) == 0 {
		return ""
	}

	// Priority keys to show first (most informative)
	priorityKeys := []string{"file_path", "path", "command", "pattern", "content", "query", "url"}
	shown := make(map[string]bool)
	var parts []string

	// Add priority keys first
	for _, key := range priorityKeys {
		if val, ok := args[key]; ok {
			formatted := formatArgPreviewValue(key, val)
			if formatted != "" {
				parts = append(parts, formatted)
				shown[key] = true
			}
			if len(parts) >= 5 {
				break
			}
		}
	}

	// Add remaining keys
	for key, val := range args {
		if shown[key] {
			continue
		}
		if len(parts) >= 5 {
			break
		}
		formatted := formatArgPreviewValue(key, val)
		if formatted != "" {
			parts = append(parts, formatted)
		}
	}

	if len(parts) == 0 {
		return ""
	}

	// If there are more arguments not shown
	remaining := len(args) - len(parts)
	if remaining > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(ColorDim).Render(fmt.Sprintf("(+%d more)", remaining)))
	}

	result := strings.Join(parts, "\n")
	return result
}

// formatArgPreviewValue formats a single argument value with smart truncation.
func formatArgPreviewValue(key string, val any) string {
	switch v := val.(type) {
	case string:
		return fmt.Sprintf("%s: %s", key, smartTruncateString(v, 60))
	case float64:
		return fmt.Sprintf("%s: %.0f", key, v)
	case int:
		return fmt.Sprintf("%s: %d", key, v)
	case bool:
		return fmt.Sprintf("%s: %v", key, v)
	case []any:
		return fmt.Sprintf("%s: [%d items]", key, len(v))
	case map[string]any:
		return fmt.Sprintf("%s: {%d fields}", key, len(v))
	default:
		return ""
	}
}

// smartTruncateString truncates a string showing first 60 chars and last 15 chars for long strings.
func smartTruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return fmt.Sprintf("%q", s)
	}

	// For very long strings, show beginning and end
	headLen := 45
	tailLen := 12

	head := s[:headLen]
	tail := s[len(s)-tailLen:]

	// Clean up any partial escape sequences
	head = strings.TrimSuffix(head, "\\")
	tail = strings.TrimPrefix(tail, "\\")

	return fmt.Sprintf("%q...%q", head, tail)
}

// renderQuestionPrompt renders the question prompt UI.
func (m Model) renderQuestionPrompt() string {
	if m.questionRequest == nil {
		return ""
	}

	var builder strings.Builder

	// Title - using modal styles
	builder.WriteString(m.styles.ModalTitle.Render(" Question"))
	builder.WriteString("\n\n")

	// Question text
	builder.WriteString("  " + m.questionRequest.Question)
	builder.WriteString("\n\n")

	// If custom input mode or no options, show input
	if m.questionCustomInput || len(m.questionRequest.Options) == 0 {
		builder.WriteString("  " + m.questionInputModel.View())
		builder.WriteString("\n\n")
		if m.questionCustomInput {
			builder.WriteString(m.styles.StatusBar.Render("Enter to submit, Esc to go back"))
		} else {
			builder.WriteString(m.styles.StatusBar.Render("Type your answer and press Enter"))
		}
		return builder.String()
	}

	// Options - using modal styles
	for i, opt := range m.questionRequest.Options {
		prefix := "  "
		style := m.styles.ModalNormal
		if i == m.questionSelectedOption {
			prefix = "> "
			style = m.styles.ModalSelected
		}

		label := fmt.Sprintf("%d. %s", i+1, opt)
		if opt == m.questionRequest.Default {
			label += " " + m.styles.ModalDefault.Render("(default)")
		}
		builder.WriteString(fmt.Sprintf("%s%s\n", prefix, style.Render(label)))
	}

	// "Other" option
	otherIdx := len(m.questionRequest.Options)
	prefix := "  "
	style := m.styles.ModalNormal
	if m.questionSelectedOption == otherIdx {
		prefix = "> "
		style = m.styles.ModalSelected
	}
	builder.WriteString(fmt.Sprintf("%s%s\n", prefix, style.Render("Other (custom answer)")))

	builder.WriteString("\n")
	builder.WriteString(m.styles.StatusBar.Render("Use arrows to select, Enter to confirm, or Ctrl+P for more"))

	return builder.String()
}

// renderPlanApproval renders the plan approval UI.
func (m Model) renderPlanApproval() string {
	if m.planRequest == nil {
		return ""
	}

	var builder strings.Builder

	// Styles
	borderStyle := lipgloss.NewStyle().Foreground(ColorPlan)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorPlan)
	planTitleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorText)
	descStyle := lipgloss.NewStyle().Foreground(ColorMuted).Italic(true)
	stepNumStyle := lipgloss.NewStyle().Foreground(ColorDim)
	stepTitleStyle := lipgloss.NewStyle().Foreground(ColorText)
	stepDescStyle := lipgloss.NewStyle().Foreground(ColorDim).Italic(true)
	infoStyle := lipgloss.NewStyle().Foreground(ColorInfo)

	// Calculate width
	panelWidth := m.width - 4
	if panelWidth < 40 {
		panelWidth = m.width // Full width on very narrow terminals
	}
	if panelWidth > 90 {
		panelWidth = 90
	}

	// Header
	stepCount := len(m.planRequest.Steps)
	headerInfo := fmt.Sprintf(" %d steps ", stepCount)
	headerTitle := " Plan Approval "

	// Calculate width using lipgloss.Width for styled text
	styledTitleWidth := lipgloss.Width(titleStyle.Render(headerTitle))
	styledInfoWidth := lipgloss.Width(infoStyle.Render(headerInfo))
	headerDashCount := panelWidth - styledTitleWidth - styledInfoWidth - 4
	if headerDashCount < 0 {
		headerDashCount = 0
	}

	builder.WriteString(borderStyle.Render("╭─"))
	builder.WriteString(titleStyle.Render(headerTitle))
	builder.WriteString(borderStyle.Render(strings.Repeat("─", headerDashCount)))
	builder.WriteString(infoStyle.Render(headerInfo))
	builder.WriteString(borderStyle.Render("─╮"))
	builder.WriteString("\n")

	// Empty line
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString(strings.Repeat(" ", panelWidth-1))
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString("\n")

	// Plan title
	titleLine := "  " + planTitleStyle.Render(m.planRequest.Title)
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString(titleLine)
	padding := panelWidth - 1 - lipgloss.Width(titleLine)
	if padding > 0 {
		builder.WriteString(strings.Repeat(" ", padding))
	}
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString("\n")

	// Description (if present)
	if m.planRequest.Description != "" {
		desc := m.planRequest.Description
		if len(desc) > panelWidth-6 {
			desc = desc[:panelWidth-9] + "..."
		}
		descLine := "  " + descStyle.Render(desc)
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(descLine)
		padding := panelWidth - 1 - lipgloss.Width(descLine)
		if padding > 0 {
			builder.WriteString(strings.Repeat(" ", padding))
		}
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")
	}

	// Empty line
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString(strings.Repeat(" ", panelWidth-1))
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString("\n")

	// Steps header
	stepsHeader := "  Steps:"
	stepsHeaderPadding := panelWidth - 1 - lipgloss.Width(infoStyle.Render(stepsHeader))
	if stepsHeaderPadding < 0 {
		stepsHeaderPadding = 0
	}
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString(infoStyle.Render(stepsHeader))
	builder.WriteString(strings.Repeat(" ", stepsHeaderPadding))
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString("\n")

	// Steps list
	for _, step := range m.planRequest.Steps {
		stepNum := fmt.Sprintf("  %d.", step.ID)
		stepLine := stepNumStyle.Render(stepNum) + " " + stepTitleStyle.Render(step.Title)

		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(stepLine)
		padding := panelWidth - 1 - lipgloss.Width(stepLine)
		if padding > 0 {
			builder.WriteString(strings.Repeat(" ", padding))
		}
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")

		// Step description (truncated)
		if step.Description != "" {
			desc := step.Description
			maxDescLen := panelWidth - 10
			if len(desc) > maxDescLen {
				desc = desc[:maxDescLen-3] + "..."
			}
			descLine := "     " + stepDescStyle.Render(desc)
			builder.WriteString(borderStyle.Render("│"))
			builder.WriteString(descLine)
			padding := panelWidth - 1 - lipgloss.Width(descLine)
			if padding > 0 {
				builder.WriteString(strings.Repeat(" ", padding))
			}
			builder.WriteString(borderStyle.Render("│"))
			builder.WriteString("\n")
		}
	}

	// Contract info (if present)
	if m.planRequest.ContractName != "" {
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(strings.Repeat(" ", panelWidth-1))
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")

		contractLine := "  Contract: " + m.planRequest.ContractName
		styledContractLine := lipgloss.NewStyle().Foreground(ColorAccent).Render(contractLine)
		contractPadding := panelWidth - 1 - lipgloss.Width(styledContractLine)
		if contractPadding < 0 {
			contractPadding = 0
		}
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(styledContractLine)
		builder.WriteString(strings.Repeat(" ", contractPadding))
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")
	}

	// Empty line before options
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString(strings.Repeat(" ", panelWidth-1))
	builder.WriteString(borderStyle.Render("│"))
	builder.WriteString("\n")

	// If in feedback mode, show feedback input
	if m.planFeedbackMode {
		feedbackLine := "  Enter your feedback:"
		styledFeedbackLine := infoStyle.Bold(true).Render(feedbackLine)
		feedbackPadding := panelWidth - 1 - lipgloss.Width(styledFeedbackLine)
		if feedbackPadding < 0 {
			feedbackPadding = 0
		}
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(styledFeedbackLine)
		builder.WriteString(strings.Repeat(" ", feedbackPadding))
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")

		inputLine := "  " + m.planFeedbackInput.View()
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(inputLine)
		padding := panelWidth - 1 - lipgloss.Width(inputLine)
		if padding > 0 {
			builder.WriteString(strings.Repeat(" ", padding))
		}
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")

		// Footer
		builder.WriteString(borderStyle.Render("╰" + strings.Repeat("─", panelWidth-1) + "╯"))
		builder.WriteString("\n")
		builder.WriteString(m.styles.StatusBar.Render("  Enter to submit • Esc to cancel"))
		return builder.String()
	}

	// Options section
	options := []struct {
		key  string
		text string
		icon string
	}{
		{"y", "Approve", "✓"},
		{"n", "Reject", "✗"},
		{"m", "Request changes", "✎"},
	}

	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorPlan).Background(ColorBorder)
	normalStyle := lipgloss.NewStyle().Foreground(ColorText)
	keyStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true)

	for i, opt := range options {
		prefix := "    "
		style := normalStyle
		icon := lipgloss.NewStyle().Foreground(ColorDim).Render(opt.icon)
		if i == m.planSelectedOption {
			prefix = "  > "
			style = selectedStyle
			icon = lipgloss.NewStyle().Foreground(ColorPlan).Bold(true).Render(opt.icon)
		}

		optLine := prefix + icon + " " + style.Render(opt.text) + " " + keyStyle.Render("("+opt.key+")")
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString(optLine)
		padding := panelWidth - 1 - lipgloss.Width(optLine)
		if padding > 0 {
			builder.WriteString(strings.Repeat(" ", padding))
		}
		builder.WriteString(borderStyle.Render("│"))
		builder.WriteString("\n")
	}

	// Footer
	builder.WriteString(borderStyle.Render("╰" + strings.Repeat("─", panelWidth-1) + "╯"))
	builder.WriteString("\n")
	builder.WriteString(lipgloss.NewStyle().Foreground(ColorDim).Render("  ↑↓ Navigate • Enter Confirm • y/n/m Quick action"))

	return builder.String()
}

// renderModelSelector renders the model selector UI.
func (m Model) renderModelSelector() string {
	var builder strings.Builder

	// Title - using modal styles
	builder.WriteString(m.styles.ModalTitle.Render(" Select Model"))
	builder.WriteString("\n\n")

	// Current model info
	builder.WriteString(fmt.Sprintf("  Current: %s\n\n", m.styles.Spinner.Render(m.currentModel)))

	// Model options - using modal styles
	for i, model := range m.availableModels {
		prefix := "  "
		style := m.styles.ModalNormal
		if i == m.modelSelectedIndex {
			prefix = "> "
			style = m.styles.ModalSelected
		}

		// Show number for quick select
		label := fmt.Sprintf("%d. %s", i+1, model.Name)
		if model.ID == m.currentModel {
			label += " " + m.styles.ModalDefault.Render("(current)")
		}
		builder.WriteString(fmt.Sprintf("%s%s\n", prefix, style.Render(label)))
		builder.WriteString(fmt.Sprintf("     %s\n", m.styles.ModalMuted.Render(model.Description)))
	}

	builder.WriteString("\n")
	builder.WriteString(m.styles.StatusBar.Render("/: Navigate | Enter: Select | Esc: Cancel | Ctrl+P: commands"))

	return builder.String()
}

// renderShortcutsOverlay renders the keyboard shortcuts overlay.
func (m Model) renderShortcutsOverlay() string {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight).
		Padding(0, 1)

	categoryStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorAccent).
		MarginTop(1)

	keyStyle := lipgloss.NewStyle().
		Foreground(ColorSecondary).
		Width(16)

	descStyle := lipgloss.NewStyle().
		Foreground(ColorText).
		Width(50)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(1, 2)

	var builder strings.Builder

	builder.WriteString(titleStyle.Render("  Keyboard Shortcuts"))
	builder.WriteString("\n")

	// Input
	builder.WriteString(categoryStyle.Render("Input"))
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Enter"), descStyle.Render("Send message")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("?"), descStyle.Render("Show this help")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/"), descStyle.Render("Browse command history")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Tab"), descStyle.Render("Autocomplete commands & files")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Ctrl+U"), descStyle.Render("Clear input line")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Ctrl+R"), descStyle.Render("Search history (reverse)")))

	// Navigation
	builder.WriteString(categoryStyle.Render("Navigation"))
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("PgUp/PgDn"), descStyle.Render("Scroll history")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Ctrl+L"), descStyle.Render("Clear / Redraw")))

	// Code Blocks
	builder.WriteString(categoryStyle.Render("Code Blocks"))
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("[ / ]"), descStyle.Render("Previous/Next block")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Tab"), descStyle.Render("Apply code block")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("c"), descStyle.Render("Copy selected block")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("y"), descStyle.Render("Copy last AI response")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Shift+Y"), descStyle.Render("Copy chat history")))

	// Command Center
	builder.WriteString(categoryStyle.Render("Command Center"))
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Ctrl+P"), descStyle.Render("Command Palette (All Actions)")))

	// Slash Commands
	builder.WriteString(categoryStyle.Render("Slash Commands"))
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/help"), descStyle.Render("Show all available commands")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/clear"), descStyle.Render("Clear conversation history")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/save"), descStyle.Render("Save current session")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/sessions"), descStyle.Render("List saved sessions")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/model"), descStyle.Render("Switch AI model")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/cost"), descStyle.Render("Show token usage & costs")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/browse"), descStyle.Render("Browse project files")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/git-status"), descStyle.Render("Show git status")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/commit"), descStyle.Render("Create git commit")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/checkpoint"), descStyle.Render("Create checkpoint")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("/doctor"), descStyle.Render("Diagnose issues")))

	// Session
	builder.WriteString(categoryStyle.Render("Session"))
	builder.WriteString("\n")
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Ctrl+C"), descStyle.Render("Quit (graceful shutdown)")))
	builder.WriteString(fmt.Sprintf("  %s%s\n", keyStyle.Render("Ctrl+D"), descStyle.Render("Quit (alternative)")))

	builder.WriteString("\n")
	builder.WriteString(m.styles.StatusBar.Render("Press any key to close"))

	return boxStyle.Render(builder.String())
}
