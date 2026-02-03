package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	// progressBarStaleTimeout is the time after which progress bar auto-hides if no updates
	progressBarStaleTimeout = 30 * time.Second
)

// ToolProgressBarModel displays a progress bar for long-running tool execution.
type ToolProgressBarModel struct {
	toolName       string
	progress       float64 // 0.0-1.0, -1 for indeterminate
	currentStep    string
	elapsed        time.Duration
	totalBytes     int64
	processedBytes int64
	cancellable    bool
	visible        bool
	frame          int // for indeterminate animation
	styles         *Styles
	lastUpdateTime time.Time // for auto-hide on stale progress
}

// NewToolProgressBarModel creates a new progress bar model.
func NewToolProgressBarModel(styles *Styles) *ToolProgressBarModel {
	return &ToolProgressBarModel{
		styles:   styles,
		progress: -1, // indeterminate by default
	}
}

// Update updates the progress bar from a ToolProgressMsg.
func (m *ToolProgressBarModel) Update(msg ToolProgressMsg) {
	m.toolName = msg.Name
	m.elapsed = msg.Elapsed
	m.progress = msg.Progress
	m.currentStep = msg.CurrentStep
	m.totalBytes = msg.TotalBytes
	m.processedBytes = msg.ProcessedBytes
	m.cancellable = msg.Cancellable
	m.visible = true
	m.lastUpdateTime = time.Now()
}

// Show makes the progress bar visible.
func (m *ToolProgressBarModel) Show(toolName string) {
	m.toolName = toolName
	m.visible = true
	m.progress = -1 // start indeterminate
	m.currentStep = ""
	m.elapsed = 0
	m.frame = 0
	m.lastUpdateTime = time.Now()
}

// Hide hides the progress bar.
func (m *ToolProgressBarModel) Hide() {
	m.visible = false
	m.progress = -1
	m.currentStep = ""
	m.toolName = ""
	m.elapsed = 0
}

// IsVisible returns whether the progress bar is visible.
func (m *ToolProgressBarModel) IsVisible() bool {
	return m.visible
}

// IsCancellable returns whether the current operation can be cancelled.
func (m *ToolProgressBarModel) IsCancellable() bool {
	return m.cancellable
}

// Tick advances the animation frame and returns a command for the next tick.
// Also checks for stale progress and auto-hides if no updates for too long.
func (m *ToolProgressBarModel) Tick() tea.Cmd {
	if m.visible {
		m.frame++

		// Auto-hide if no updates for too long (tool might have crashed)
		if !m.lastUpdateTime.IsZero() && time.Since(m.lastUpdateTime) > progressBarStaleTimeout {
			m.Hide()
		}
	}
	return nil
}

// spinnerFrames contains braille spinner animation frames.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// View renders the progress bar as a compact single line.
func (m *ToolProgressBarModel) View(width int) string {
	if !m.visible || m.toolName == "" {
		return ""
	}

	// Styles
	dimStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	accentStyle := lipgloss.NewStyle().
		Foreground(ColorHighlight)

	progressStyle := lipgloss.NewStyle().
		Foreground(ColorSuccess)

	// Animated spinner
	spinnerIdx := m.frame % len(spinnerFrames)
	spinner := accentStyle.Render(spinnerFrames[spinnerIdx])

	// Tool name
	toolName := capitalizeToolName(m.toolName)

	// Time elapsed
	elapsedStr := formatElapsed(m.elapsed)

	// Build compact single line
	var builder strings.Builder
	builder.WriteString(spinner)
	builder.WriteString(" ")
	builder.WriteString(dimStyle.Render(toolName))

	// Progress indicator
	if m.progress >= 0 {
		// Determinate: show percentage with mini bar
		percent := int(m.progress * 100)
		barWidth := 20
		filled := int(m.progress * float64(barWidth))
		if filled > barWidth {
			filled = barWidth
		}
		miniBar := strings.Repeat("━", filled) + strings.Repeat("─", barWidth-filled)
		builder.WriteString(" ")
		builder.WriteString(progressStyle.Render(miniBar))
		builder.WriteString(dimStyle.Render(fmt.Sprintf(" %d%%", percent)))
	} else {
		// Indeterminate: just show running dots
		dots := strings.Repeat(".", (m.frame/3)%4)
		builder.WriteString(dimStyle.Render(dots))
		// Pad to keep width stable
		builder.WriteString(strings.Repeat(" ", 3-len(dots)))
	}

	// Current step (shortened)
	if m.currentStep != "" {
		step := m.currentStep
		maxLen := 25
		if len(step) > maxLen {
			step = step[:maxLen-1] + "…"
		}
		builder.WriteString(" ")
		builder.WriteString(dimStyle.Render(step))
	}

	// Bytes if available
	if m.totalBytes > 0 {
		builder.WriteString(dimStyle.Render(fmt.Sprintf(" %s/%s",
			formatBytes(m.processedBytes), formatBytes(m.totalBytes))))
	} else if m.processedBytes > 0 {
		builder.WriteString(dimStyle.Render(fmt.Sprintf(" %s", formatBytes(m.processedBytes))))
	}

	// Elapsed time (right side)
	builder.WriteString(" ")
	builder.WriteString(dimStyle.Render(elapsedStr))

	return builder.String()
}

// formatBytes formats bytes for display.
func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case b >= GB:
		return fmt.Sprintf("%.1fGB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.1fMB", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.1fKB", float64(b)/KB)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
