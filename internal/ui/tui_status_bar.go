package ui

import (
	"fmt"
	"strings"
	"time"

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
// Shows only warning indicators when dangerous.
func (m Model) renderStatusBarMinimal() string {
	var parts []string

	// Only show warnings - nothing when safe
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	bgStyle := lipgloss.NewStyle().Foreground(ColorInfo)

	if !m.permissionsEnabled {
		parts = append(parts, yoloStyle.Render("YOLO"))
	}

	if !m.sandboxEnabled {
		parts = append(parts, sandboxStyle.Render("!SANDBOX"))
	}

	// Background tasks indicator
	if bgCount := len(m.backgroundTasks); bgCount > 0 {
		parts = append(parts, bgStyle.Render(fmt.Sprintf("[%d bg]", bgCount)))
	}

	return strings.Join(parts, " • ")
}

// renderStatusBarCompact renders a compact status bar for narrow terminals (60-79 chars).
// Shows warnings + model + tokens%.
func (m Model) renderStatusBarCompact() string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	modelStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	bgStyle := lipgloss.NewStyle().Foreground(ColorInfo)

	var leftParts []string

	// Only show warnings when dangerous
	if !m.permissionsEnabled {
		leftParts = append(leftParts, yoloStyle.Render("YOLO"))
	}

	if !m.sandboxEnabled {
		leftParts = append(leftParts, sandboxStyle.Render("!SANDBOX"))
	}

	// Background tasks indicator
	if bgCount := len(m.backgroundTasks); bgCount > 0 {
		leftParts = append(leftParts, bgStyle.Render(fmt.Sprintf("[%d bg]", bgCount)))
	}

	// Model
	if m.currentModel != "" {
		leftParts = append(leftParts, modelStyle.Render(shortenModelName(m.currentModel)))
	}

	var rightParts []string

	// Token usage (compact %)
	if m.showTokens && m.tokenUsage != nil {
		usageColor := ColorMuted
		if m.tokenUsage.PercentUsed > 0.8 {
			usageColor = ColorError
		} else if m.tokenUsage.PercentUsed > 0.5 {
			usageColor = ColorWarning
		}
		usageStyle := lipgloss.NewStyle().Foreground(usageColor)
		rightParts = append(rightParts, usageStyle.Render(fmt.Sprintf("%.0f%%", m.tokenUsage.PercentUsed*100)))
	}

	// Session time
	elapsed := time.Since(m.sessionStart)
	rightParts = append(rightParts, dimStyle.Render(formatSessionTime(elapsed)))

	left := strings.Join(leftParts, " • ")
	right := strings.Join(rightParts, " ")

	padding := safePadding(m.width, lipgloss.Width(left), lipgloss.Width(right))
	if padding < 0 {
		padding = 0
	}

	return left + strings.Repeat(" ", padding) + right
}

// renderStatusBarMedium renders a medium status bar for standard terminals (80-119 chars).
// Shows warnings + model + branch + tokens%.
func (m Model) renderStatusBarMedium() string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	planStyle := lipgloss.NewStyle().Foreground(ColorInfo)
	bgStyle := lipgloss.NewStyle().Foreground(ColorInfo)
	modelStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	gitStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	var leftParts []string

	// Only show warnings when dangerous (no SAFE/SECURE indicator)
	if !m.permissionsEnabled {
		leftParts = append(leftParts, yoloStyle.Render("YOLO"))
	}

	if !m.sandboxEnabled {
		leftParts = append(leftParts, sandboxStyle.Render("!SANDBOX"))
	}

	// Plan mode (subtle)
	if m.planningModeEnabled {
		leftParts = append(leftParts, planStyle.Render("PLAN"))
	}

	// Background tasks indicator
	if bgCount := len(m.backgroundTasks); bgCount > 0 {
		leftParts = append(leftParts, bgStyle.Render(fmt.Sprintf("[%d bg]", bgCount)))
	}

	// Model
	if m.currentModel != "" {
		leftParts = append(leftParts, modelStyle.Render(shortenModelName(m.currentModel)))
	}

	// Git branch
	if m.gitBranch != "" {
		branch := m.gitBranch
		if len(branch) > 15 {
			branch = branch[:12] + "..."
		}
		leftParts = append(leftParts, gitStyle.Render(branch))
	}

	var rightParts []string

	// Token usage
	if m.showTokens && m.tokenUsage != nil {
		usageColor := ColorMuted
		if m.tokenUsage.PercentUsed > 0.8 {
			usageColor = ColorError
		} else if m.tokenUsage.PercentUsed > 0.5 {
			usageColor = ColorWarning
		}
		usageStyle := lipgloss.NewStyle().Foreground(usageColor)
		rightParts = append(rightParts, usageStyle.Render(fmt.Sprintf("%.0f%%", m.tokenUsage.PercentUsed*100)))
	}

	// Time
	elapsed := time.Since(m.sessionStart)
	rightParts = append(rightParts, dimStyle.Render(formatSessionTime(elapsed)))

	left := strings.Join(leftParts, " • ")
	right := strings.Join(rightParts, " ")

	padding := safePadding(m.width, lipgloss.Width(left), lipgloss.Width(right))
	if padding < 0 {
		padding = 0
	}

	return left + strings.Repeat(" ", padding) + right
}

// renderStatusBarFull renders the full status bar for wide terminals (>= 120 chars).
// Shows warnings + model + branch + tokens% + time + version.
func (m Model) renderStatusBarFull() string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	sandboxStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	planStyle := lipgloss.NewStyle().Foreground(ColorInfo)
	bgStyle := lipgloss.NewStyle().Foreground(ColorInfo)
	modelStyle := lipgloss.NewStyle().Foreground(ColorAccent)
	gitStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	versionStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var leftParts []string

	// Only show warnings when dangerous (no SAFE/SECURE indicator)
	if !m.permissionsEnabled {
		leftParts = append(leftParts, yoloStyle.Render("YOLO"))
	}

	if !m.sandboxEnabled {
		leftParts = append(leftParts, sandboxStyle.Render("!SANDBOX"))
	}

	// Plan mode (subtle)
	if m.planningModeEnabled {
		leftParts = append(leftParts, planStyle.Render("PLAN"))
	}

	// Background tasks indicator
	if bgCount := len(m.backgroundTasks); bgCount > 0 {
		leftParts = append(leftParts, bgStyle.Render(fmt.Sprintf("[%d bg]", bgCount)))
	}

	// Model
	if m.currentModel != "" {
		leftParts = append(leftParts, modelStyle.Render(shortenModelName(m.currentModel)))
	}

	// Git branch
	if m.gitBranch != "" {
		leftParts = append(leftParts, gitStyle.Render(m.gitBranch))
	}

	var rightParts []string

	// Token usage with bar
	if m.showTokens && m.tokenUsage != nil {
		usageColor := ColorSuccess
		if m.tokenUsage.PercentUsed > 0.8 {
			usageColor = ColorError
		} else if m.tokenUsage.PercentUsed > 0.5 {
			usageColor = ColorWarning
		}
		usageStyle := lipgloss.NewStyle().Foreground(usageColor)
		rightParts = append(rightParts, usageStyle.Render(fmt.Sprintf("%.0f%%", m.tokenUsage.PercentUsed*100)))
	}

	// Session time
	elapsed := time.Since(m.sessionStart)
	rightParts = append(rightParts, dimStyle.Render(formatSessionTime(elapsed)))

	// Version (показываем в конце справа)
	if m.version != "" {
		rightParts = append(rightParts, versionStyle.Render("v"+m.version))
	}

	left := strings.Join(leftParts, " • ")
	right := strings.Join(rightParts, " ")

	padding := safePadding(m.width, lipgloss.Width(left), lipgloss.Width(right))
	if padding < 0 {
		padding = 0
	}

	return left + strings.Repeat(" ", padding) + right
}

// renderTokenBar renders a visual token usage bar with modern characters.
func renderTokenBar(percent float64, width int) string {
	filled := int(percent * float64(width))
	if filled > width {
		filled = width
	}

	var barColor lipgloss.Color
	if percent > 0.9 {
		barColor = ColorError
	} else if percent > 0.7 {
		barColor = ColorWarning
	} else {
		barColor = ColorSuccess
	}

	filledStyle := lipgloss.NewStyle().Foreground(barColor)
	emptyStyle := lipgloss.NewStyle().Foreground(ColorDim)

	bar := filledStyle.Render(strings.Repeat("█", filled))
	bar += emptyStyle.Render(strings.Repeat("░", width-filled))

	return bar
}

// formatSessionTime formats elapsed session time.
func formatSessionTime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// shortenModelName returns a shortened model name.
func shortenModelName(name string) string {
	name = strings.ReplaceAll(name, "gemini-", "")
	name = strings.ReplaceAll(name, "-preview", "")
	name = strings.ReplaceAll(name, "-latest", "")
	return name
}
