package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// getStatusBarLayout determines the appropriate layout based on terminal width.
func (m Model) getStatusBarLayout() StatusBarLayout {
	switch {
	case m.width >= 120:
		return StatusBarLayoutFull
	case m.width >= 80:
		return StatusBarLayoutMedium
	case m.width >= 60:
		return StatusBarLayoutCompact
	default:
		return StatusBarLayoutMinimal
	}
}

// safePadding calculates padding ensuring it's never negative.
func safePadding(available, left, right int) int {
	padding := available - left - right
	if padding < 1 {
		return 1
	}
	return padding
}

// renderStatusBar renders the enhanced status bar with adaptive layout.
func (m Model) renderStatusBar() string {
	layout := m.getStatusBarLayout()

	switch layout {
	case StatusBarLayoutMinimal:
		return m.renderStatusBarMinimal()
	case StatusBarLayoutCompact:
		return m.renderStatusBarCompact()
	case StatusBarLayoutMedium:
		return m.renderStatusBarMedium()
	default:
		return m.renderStatusBarFull()
	}
}

// renderStatusBarMinimal renders a minimal status bar for very narrow terminals (< 60 chars).
// Shows only safety-critical warnings.
func (m Model) renderStatusBarMinimal() string {
	var parts []string

	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)

	if !m.permissionsEnabled {
		parts = append(parts, yoloStyle.Render("YOLO"))
	}

	if !m.sandboxEnabled {
		parts = append(parts, sandboxStyle.Render("!SANDBOX"))
	}

	return strings.Join(parts, " ")
}

// renderStatusBarCompact renders a compact status bar for narrow terminals (60-79 chars).
// Shows: warnings + model + context%.
func (m Model) renderStatusBarCompact() string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)

	var leftParts []string

	if !m.permissionsEnabled {
		leftParts = append(leftParts, yoloStyle.Render("YOLO"))
	}
	if !m.sandboxEnabled {
		leftParts = append(leftParts, sandboxStyle.Render("!SANDBOX"))
	}

	// Model
	if m.currentModel != "" {
		leftParts = append(leftParts, dimStyle.Render(shortenModelName(m.currentModel)))
	}

	var rightParts []string

	// Context % (compact, no label)
	contextPct := m.getContextPercent()
	if contextPct > 0 {
		usageColor := ColorDim
		if contextPct > 80 {
			usageColor = ColorError
		} else if contextPct > 50 {
			usageColor = ColorWarning
		}
		rightParts = append(rightParts, lipgloss.NewStyle().Foreground(usageColor).Render(fmt.Sprintf("%.0f%%", contextPct)))
	}

	left := strings.Join(leftParts, " · ")
	right := strings.Join(rightParts, " ")

	padding := safePadding(m.width, lipgloss.Width(left), lipgloss.Width(right))
	return left + strings.Repeat(" ", padding) + right
}

// renderStatusBarMedium renders a medium status bar for standard terminals (80-119 chars).
// Shows: model · context% · [warnings].
func (m Model) renderStatusBarMedium() string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)

	var leftParts []string

	if !m.permissionsEnabled {
		leftParts = append(leftParts, yoloStyle.Render("YOLO"))
	}
	if !m.sandboxEnabled {
		leftParts = append(leftParts, sandboxStyle.Render("!SANDBOX"))
	}

	// Model
	if m.currentModel != "" {
		leftParts = append(leftParts, dimStyle.Render(shortenModelName(m.currentModel)))
	}

	// Context %
	contextPct := m.getContextPercent()
	if contextPct > 0 {
		usageColor := ColorDim
		if contextPct > 80 {
			usageColor = ColorError
		} else if contextPct > 50 {
			usageColor = ColorWarning
		}
		leftParts = append(leftParts, lipgloss.NewStyle().Foreground(usageColor).Render(fmt.Sprintf("%.0f%%", contextPct)))
	}

	// Mode indicator (plan mode only — it's the one that changes behavior)
	if m.planningModeEnabled {
		leftParts = append(leftParts, dimStyle.Render("PLAN"))
	}

	// Background tasks (compact)
	if bgCount := len(m.backgroundTasks); bgCount > 0 {
		leftParts = append(leftParts, dimStyle.Render(fmt.Sprintf("%d bg", bgCount)))
	}

	left := strings.Join(leftParts, " · ")

	padding := safePadding(m.width, lipgloss.Width(left), 0)
	return left + strings.Repeat(" ", padding)
}

// renderStatusBarFull renders the full status bar for wide terminals (>= 120 chars).
// Shows: model · context% · [mode] · [warnings] on left; retry/MCP warnings on right.
func (m Model) renderStatusBarFull() string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)

	var leftParts []string

	if !m.permissionsEnabled {
		leftParts = append(leftParts, yoloStyle.Render("YOLO"))
	}
	if !m.sandboxEnabled {
		leftParts = append(leftParts, sandboxStyle.Render("!SANDBOX"))
	}

	// Model
	if m.currentModel != "" {
		leftParts = append(leftParts, dimStyle.Render(shortenModelName(m.currentModel)))
	}

	// Context %
	contextPct := m.getContextPercent()
	if contextPct > 0 {
		usageColor := ColorDim
		if contextPct > 80 {
			usageColor = ColorError
		} else if contextPct > 50 {
			usageColor = ColorWarning
		}
		leftParts = append(leftParts, lipgloss.NewStyle().Foreground(usageColor).Render(fmt.Sprintf("%.0f%%", contextPct)))
	}

	// Plan mode
	if m.planningModeEnabled {
		leftParts = append(leftParts, dimStyle.Render("PLAN"))
	}

	// Background tasks
	if bgCount := len(m.backgroundTasks); bgCount > 0 {
		leftParts = append(leftParts, dimStyle.Render(fmt.Sprintf("%d bg", bgCount)))
	}

	var rightParts []string

	// Retry indicator (important — shows active retries)
	if m.retryAttempt > 0 && m.retryMax > 0 {
		retryStyle := lipgloss.NewStyle().Foreground(ColorWarning)
		rightParts = append(rightParts, retryStyle.Render(fmt.Sprintf("↻ %d/%d", m.retryAttempt, m.retryMax)))
	}

	// MCP health (only when unhealthy)
	if m.mcpTotal > 0 && m.mcpHealthy < m.mcpTotal {
		mcpColor := ColorWarning
		if m.mcpHealthy == 0 {
			mcpColor = ColorError
		}
		rightParts = append(rightParts, lipgloss.NewStyle().Foreground(mcpColor).Render(fmt.Sprintf("MCP %d/%d", m.mcpHealthy, m.mcpTotal)))
	}

	left := strings.Join(leftParts, " · ")
	right := strings.Join(rightParts, " · ")

	padding := safePadding(m.width, lipgloss.Width(left), lipgloss.Width(right))
	return left + strings.Repeat(" ", padding) + right
}

// getContextPercent returns the context usage percentage from available sources.
func (m Model) getContextPercent() float64 {
	if m.tokenUsagePercent > 0 {
		return m.tokenUsagePercent
	}
	if m.showTokens && m.tokenUsage != nil {
		return m.tokenUsage.PercentUsed * 100
	}
	return 0
}

// shortenModelName returns a shortened model name.
func shortenModelName(name string) string {
	name = strings.ReplaceAll(name, "gemini-", "")
	name = strings.ReplaceAll(name, "-preview", "")
	name = strings.ReplaceAll(name, "-latest", "")
	return name
}
