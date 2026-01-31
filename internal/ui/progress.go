package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ProgressAction represents user actions on progress.
type ProgressAction int

const (
	ProgressActionNone ProgressAction = iota
	ProgressActionCancel
	ProgressActionPause
	ProgressActionResume
)

// ProgressItem represents a single item in a batch operation.
type ProgressItem struct {
	Name    string
	Status  ProgressStatus
	Error   string
	Message string
}

// ProgressStatus represents the status of a progress item.
type ProgressStatus int

const (
	ProgressStatusPending ProgressStatus = iota
	ProgressStatusInProgress
	ProgressStatusCompleted
	ProgressStatusFailed
	ProgressStatusSkipped
)

// ProgressUpdateMsg is sent to update progress.
type ProgressUpdateMsg struct {
	Current     int
	Total       int
	CurrentItem string
	Message     string
	Items       []ProgressItem
}

// ProgressCompleteMsg is sent when progress completes.
type ProgressCompleteMsg struct {
	TotalItems   int
	SuccessCount int
	FailureCount int
	SkippedCount int
	Duration     time.Duration
}

// ProgressActionMsg is sent when user performs an action.
type ProgressActionMsg struct {
	Action ProgressAction
}

// ProgressModel is the UI for displaying batch operation progress.
type ProgressModel struct {
	title       string
	current     int
	total       int
	currentItem string
	message     string
	items       []ProgressItem
	startTime   time.Time
	isPaused    bool
	isComplete  bool
	styles      *Styles
	width       int
	height      int

	// Completion stats
	successCount int
	failureCount int
	skippedCount int
	duration     time.Duration

	// Callback for actions
	onAction func(action ProgressAction)
}

// NewProgressModel creates a new progress model.
func NewProgressModel(styles *Styles) ProgressModel {
	return ProgressModel{
		styles:    styles,
		startTime: time.Now(),
	}
}

// SetSize sets the size of the progress view.
func (m *ProgressModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// Start starts a new progress operation.
func (m *ProgressModel) Start(title string, total int) {
	m.title = title
	m.current = 0
	m.total = total
	m.currentItem = ""
	m.message = ""
	m.items = nil
	m.startTime = time.Now()
	m.isPaused = false
	m.isComplete = false
	m.successCount = 0
	m.failureCount = 0
	m.skippedCount = 0
}

// Update updates the progress state.
func (m *ProgressModel) UpdateProgress(current int, currentItem, message string) {
	m.current = current
	m.currentItem = currentItem
	m.message = message
}

// AddItem adds an item to the progress list.
func (m *ProgressModel) AddItem(item ProgressItem) {
	m.items = append(m.items, item)

	// Update counts
	switch item.Status {
	case ProgressStatusCompleted:
		m.successCount++
	case ProgressStatusFailed:
		m.failureCount++
	case ProgressStatusSkipped:
		m.skippedCount++
	}
}

// Complete marks the progress as complete.
func (m *ProgressModel) Complete() {
	m.isComplete = true
	m.duration = time.Since(m.startTime)
}

// SetActionCallback sets the callback for user actions.
func (m *ProgressModel) SetActionCallback(callback func(ProgressAction)) {
	m.onAction = callback
}

// Init initializes the progress model.
func (m ProgressModel) Init() tea.Cmd {
	return nil
}

// Update handles input events for the progress model.
func (m ProgressModel) Update(msg tea.Msg) (ProgressModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "c", "ctrl+c":
			if !m.isComplete {
				if m.onAction != nil {
					m.onAction(ProgressActionCancel)
				}
				return m, func() tea.Msg {
					return ProgressActionMsg{Action: ProgressActionCancel}
				}
			}

		case "p":
			if !m.isComplete {
				if m.isPaused {
					m.isPaused = false
					if m.onAction != nil {
						m.onAction(ProgressActionResume)
					}
				} else {
					m.isPaused = true
					if m.onAction != nil {
						m.onAction(ProgressActionPause)
					}
				}
			}

		case "q", "enter":
			if m.isComplete {
				// Close the progress view
				return m, nil
			}
		}

	case ProgressUpdateMsg:
		m.current = msg.Current
		m.total = msg.Total
		m.currentItem = msg.CurrentItem
		m.message = msg.Message
		if msg.Items != nil {
			m.items = msg.Items
		}

	case ProgressCompleteMsg:
		m.isComplete = true
		m.successCount = msg.SuccessCount
		m.failureCount = msg.FailureCount
		m.skippedCount = msg.SkippedCount
		m.duration = msg.Duration

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	}

	return m, nil
}

// View renders the progress view.
func (m ProgressModel) View() string {
	var builder strings.Builder

	// Animated spinner
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinnerIdx := int(time.Now().UnixMilli()/100) % len(spinners)
	spinnerStyle := lipgloss.NewStyle().Foreground(ColorGradient1).Bold(true)

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight)

	if m.isComplete {
		builder.WriteString(headerStyle.Render("✅ " + m.title + " Complete"))
	} else if m.isPaused {
		builder.WriteString(headerStyle.Render("⏸️ " + m.title + " (Paused)"))
	} else {
		builder.WriteString(spinnerStyle.Render(spinners[spinnerIdx]) + " " + headerStyle.Render(m.title))
	}
	builder.WriteString("\n")

	// Progress bar
	barWidth := m.width - 30
	if barWidth < 20 {
		barWidth = 20
	}
	if barWidth > 60 {
		barWidth = 60
	}

	progress := float64(m.current) / float64(m.total)
	if m.total == 0 {
		progress = 0
	}

	filled := int(progress * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}

	// Bar colors
	var barColor lipgloss.Color
	if m.isComplete {
		if m.failureCount > 0 {
			barColor = ColorWarning
		} else {
			barColor = ColorSuccess
		}
	} else {
		barColor = ColorPrimary
	}

	filledStyle := lipgloss.NewStyle().Foreground(barColor)
	emptyStyle := lipgloss.NewStyle().Foreground(ColorDim)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	// Use modern bar characters ▓░
	bar := filledStyle.Render(strings.Repeat("▓", filled))
	bar += emptyStyle.Render(strings.Repeat("░", barWidth-filled))

	percentStr := fmt.Sprintf("%3.0f%%", progress*100)
	countStr := fmt.Sprintf("%d/%d", m.current, m.total)

	// Format with separators
	elapsed := time.Since(m.startTime)
	if m.isComplete {
		elapsed = m.duration
	}
	elapsedStr := formatProgressDuration(elapsed)

	builder.WriteString(fmt.Sprintf(" %s %s %s %s %s %s\n", bar, percentStr, dimStyle.Render("│"), countStr, dimStyle.Render("│"), elapsedStr))

	// Current item with indent
	if !m.isComplete && m.currentItem != "" {
		itemStyle := lipgloss.NewStyle().Foreground(ColorAccent)
		builder.WriteString(fmt.Sprintf(" └─ %s\n", itemStyle.Render(shortenPath(m.currentItem, 50))))
	}

	// Message
	if m.message != "" {
		msgStyle := lipgloss.NewStyle().Foreground(ColorMuted)
		builder.WriteString(fmt.Sprintf("    %s\n", msgStyle.Render(m.message)))
	}

	builder.WriteString("\n")

	// Stats
	if m.isComplete || m.successCount > 0 || m.failureCount > 0 {
		statsStyle := lipgloss.NewStyle().Foreground(ColorMuted)
		successStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
		failStyle := lipgloss.NewStyle().Foreground(ColorError)
		skipStyle := lipgloss.NewStyle().Foreground(ColorWarning)

		stats := []string{
			successStyle.Render(fmt.Sprintf("✓ %d success", m.successCount)),
		}
		if m.failureCount > 0 {
			stats = append(stats, failStyle.Render(fmt.Sprintf("✗ %d failed", m.failureCount)))
		}
		if m.skippedCount > 0 {
			stats = append(stats, skipStyle.Render(fmt.Sprintf("○ %d skipped", m.skippedCount)))
		}

		builder.WriteString(statsStyle.Render("  ") + strings.Join(stats, "  │  "))
		builder.WriteString("\n")
	}

	// Recent items (show last few)
	if len(m.items) > 0 {
		borderStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(0, 1).
			MaxWidth(m.width - 4).
			MaxHeight(10)

		var itemsContent strings.Builder
		start := len(m.items) - 5
		if start < 0 {
			start = 0
		}

		for _, item := range m.items[start:] {
			line := m.formatItemLine(item)
			itemsContent.WriteString(line)
			itemsContent.WriteString("\n")
		}

		builder.WriteString(borderStyle.Render(itemsContent.String()))
		builder.WriteString("\n")
	}

	builder.WriteString("\n")

	// Footer with actions
	m.renderActions(&builder)

	return builder.String()
}

// formatItemLine formats a progress item for display.
func (m *ProgressModel) formatItemLine(item ProgressItem) string {
	var icon string
	var style lipgloss.Style

	switch item.Status {
	case ProgressStatusPending:
		icon = "○"
		style = lipgloss.NewStyle().Foreground(ColorMuted)
	case ProgressStatusInProgress:
		icon = "◐"
		style = lipgloss.NewStyle().Foreground(ColorAccent)
	case ProgressStatusCompleted:
		icon = "●"
		style = lipgloss.NewStyle().Foreground(ColorSuccess)
	case ProgressStatusFailed:
		icon = "✗"
		style = lipgloss.NewStyle().Foreground(ColorError)
	case ProgressStatusSkipped:
		icon = "○"
		style = lipgloss.NewStyle().Foreground(ColorWarning)
	}

	line := fmt.Sprintf("  %s %s", icon, style.Render(item.Name))
	if item.Error != "" {
		errStyle := lipgloss.NewStyle().Foreground(ColorError)
		line += " - " + errStyle.Render(item.Error)
	} else if item.Message != "" {
		msgStyle := lipgloss.NewStyle().Foreground(ColorMuted)
		line += " " + msgStyle.Render(item.Message)
	}

	return line
}

// renderActions renders the available actions.
func (m *ProgressModel) renderActions(builder *strings.Builder) {
	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	keyStyle := lipgloss.NewStyle().
		Foreground(ColorSecondary).
		Bold(true)

	if m.isComplete {
		builder.WriteString(hintStyle.Render("Press ") + keyStyle.Render("Enter") + hintStyle.Render(" to close"))
	} else {
		hints := []string{
			keyStyle.Render("Esc") + " Cancel",
		}
		if m.isPaused {
			hints = append(hints, keyStyle.Render("p")+" Resume")
		} else {
			hints = append(hints, keyStyle.Render("p")+" Pause")
		}
		builder.WriteString(hintStyle.Render(strings.Join(hints, "  │  ")))
	}
}

// IsComplete returns whether the progress is complete.
func (m ProgressModel) IsComplete() bool {
	return m.isComplete
}

// GetStats returns the completion statistics.
func (m ProgressModel) GetStats() (success, failure, skipped int) {
	return m.successCount, m.failureCount, m.skippedCount
}

// formatProgressDuration formats a duration for progress display.
func formatProgressDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}
