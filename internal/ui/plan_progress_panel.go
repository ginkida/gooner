package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// PlanStepStatus represents the status of a plan step.
type PlanStepStatus int

const (
	PlanStepPending PlanStepStatus = iota
	PlanStepInProgress
	PlanStepCompleted
	PlanStepFailed
	PlanStepSkipped
)

// PlanStepState holds the state of a single plan step.
type PlanStepState struct {
	ID          int
	Title       string
	Description string
	Status      PlanStepStatus
	StartedAt   time.Time
	CompletedAt time.Time
	Output      string
	Error       string
}

// ActivityEntry represents a single activity log entry.
type ActivityEntry struct {
	Timestamp time.Time
	Type      string // "tool", "file", "info"
	Message   string
}

// PlanProgressPanel displays detailed plan execution progress.
type PlanProgressPanel struct {
	visible       bool
	planID        string
	planTitle     string
	planDesc      string
	steps         []PlanStepState
	currentStepID int
	startedAt     time.Time
	styles        *Styles
	collapsed     bool // Compact mode
	frame         int  // For animations

	// Live activity feed
	activities    []ActivityEntry
	maxActivities int    // Max entries to keep (default: 5)
	currentTool   string // Currently executing tool
	currentInfo   string // Tool info (file path, command, etc.)
}

// NewPlanProgressPanel creates a new plan progress panel.
func NewPlanProgressPanel(styles *Styles) *PlanProgressPanel {
	return &PlanProgressPanel{
		visible:       false,
		steps:         make([]PlanStepState, 0),
		styles:        styles,
		collapsed:     false,
		frame:         0,
		activities:    make([]ActivityEntry, 0),
		maxActivities: 5,
	}
}

// StartPlan initializes a new plan execution.
func (p *PlanProgressPanel) StartPlan(planID, title, description string, steps []PlanStepInfo) {
	p.visible = true
	p.planID = planID
	p.planTitle = title
	p.planDesc = description
	p.startedAt = time.Now()
	p.currentStepID = 0
	p.collapsed = false

	p.steps = make([]PlanStepState, len(steps))
	for i, step := range steps {
		p.steps[i] = PlanStepState{
			ID:          step.ID,
			Title:       step.Title,
			Description: step.Description,
			Status:      PlanStepPending,
		}
	}
}

// StartStep marks a step as in progress.
func (p *PlanProgressPanel) StartStep(stepID int) {
	for i := range p.steps {
		if p.steps[i].ID == stepID {
			p.steps[i].Status = PlanStepInProgress
			p.steps[i].StartedAt = time.Now()
			p.currentStepID = stepID
			break
		}
	}
}

// CompleteStep marks a step as completed.
func (p *PlanProgressPanel) CompleteStep(stepID int, output string) {
	for i := range p.steps {
		if p.steps[i].ID == stepID {
			p.steps[i].Status = PlanStepCompleted
			p.steps[i].CompletedAt = time.Now()
			p.steps[i].Output = output
			break
		}
	}
}

// FailStep marks a step as failed.
func (p *PlanProgressPanel) FailStep(stepID int, errorMsg string) {
	for i := range p.steps {
		if p.steps[i].ID == stepID {
			p.steps[i].Status = PlanStepFailed
			p.steps[i].CompletedAt = time.Now()
			p.steps[i].Error = errorMsg
			break
		}
	}
}

// SkipStep marks a step as skipped.
func (p *PlanProgressPanel) SkipStep(stepID int) {
	for i := range p.steps {
		if p.steps[i].ID == stepID {
			p.steps[i].Status = PlanStepSkipped
			p.steps[i].CompletedAt = time.Now()
			break
		}
	}
}

// EndPlan marks the plan as finished.
func (p *PlanProgressPanel) EndPlan() {
	// Don't hide immediately - let user see final state
	// Panel will be hidden when a new response starts
}

// Hide hides the panel.
func (p *PlanProgressPanel) Hide() {
	p.visible = false
}

// IsVisible returns whether the panel is visible.
func (p *PlanProgressPanel) IsVisible() bool {
	return p.visible
}

// Toggle toggles collapsed mode.
func (p *PlanProgressPanel) Toggle() {
	p.collapsed = !p.collapsed
}

// Tick updates the animation frame.
func (p *PlanProgressPanel) Tick() {
	p.frame++
}

// SetCurrentTool updates the currently executing tool.
func (p *PlanProgressPanel) SetCurrentTool(toolName, toolInfo string) {
	if toolName == "" {
		p.currentTool = ""
		p.currentInfo = ""
		return
	}

	p.currentTool = toolName
	p.currentInfo = toolInfo

	// Add to activity log
	msg := toolName
	if toolInfo != "" {
		if len(toolInfo) > 40 {
			toolInfo = toolInfo[:37] + "..."
		}
		msg += ": " + toolInfo
	}
	p.AddActivity("tool", msg)
}

// ClearCurrentTool clears the current tool.
func (p *PlanProgressPanel) ClearCurrentTool() {
	p.currentTool = ""
	p.currentInfo = ""
}

// AddActivity adds an entry to the activity log.
func (p *PlanProgressPanel) AddActivity(actType, message string) {
	entry := ActivityEntry{
		Timestamp: time.Now(),
		Type:      actType,
		Message:   message,
	}
	p.activities = append(p.activities, entry)

	// Trim to max size
	if len(p.activities) > p.maxActivities {
		p.activities = p.activities[len(p.activities)-p.maxActivities:]
	}
}

// ClearActivities clears the activity log.
func (p *PlanProgressPanel) ClearActivities() {
	p.activities = make([]ActivityEntry, 0)
}

// Progress returns the completion progress (0.0 to 1.0).
func (p *PlanProgressPanel) Progress() float64 {
	if len(p.steps) == 0 {
		return 0
	}

	completed := 0
	for _, step := range p.steps {
		if step.Status == PlanStepCompleted || step.Status == PlanStepSkipped {
			completed++
		}
	}
	return float64(completed) / float64(len(p.steps))
}

// CompletedCount returns the number of completed steps.
func (p *PlanProgressPanel) CompletedCount() int {
	count := 0
	for _, step := range p.steps {
		if step.Status == PlanStepCompleted || step.Status == PlanStepSkipped {
			count++
		}
	}
	return count
}

// View renders the plan progress panel.
func (p *PlanProgressPanel) View(width int) string {
	if !p.visible || len(p.steps) == 0 {
		return ""
	}

	// Styles
	borderStyle := lipgloss.NewStyle().Foreground(ColorPlan)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorPlan)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	mutedStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	var content strings.Builder

	// Panel width
	panelWidth := width - 4
	if panelWidth < 50 {
		panelWidth = 50
	}
	if panelWidth > 100 {
		panelWidth = 100
	}

	// Header with progress bar
	progress := p.Progress()
	elapsed := time.Since(p.startedAt)

	// Progress bar
	barWidth := 20
	filled := int(progress * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	progressBar := p.renderProgressBar(filled, barWidth, progress)

	// Header line
	header := fmt.Sprintf(" Plan: %s ", p.planTitle)
	if len(header) > panelWidth-30 {
		header = header[:panelWidth-33] + "... "
	}

	progressInfo := fmt.Sprintf(" %d/%d ", p.CompletedCount(), len(p.steps))
	elapsedStr := formatElapsed(elapsed)

	headerLine := titleStyle.Render(header) +
		progressBar + " " +
		mutedStyle.Render(progressInfo) +
		dimStyle.Render("("+elapsedStr+")")

	// Premium 3D-like border using distinct characters
	content.WriteString(borderStyle.Render("┏") + borderStyle.Render(strings.Repeat("━", panelWidth-1)) + borderStyle.Render("┓"))
	content.WriteString("\n")

	// Title line stylized as a title bar
	titleBar := "  " + headerLine
	content.WriteString(borderStyle.Render("┃"))
	content.WriteString(titleBar)
	padding := panelWidth - 1 - lipgloss.Width(titleBar)
	if padding > 0 {
		content.WriteString(strings.Repeat(" ", padding))
	}
	content.WriteString(borderStyle.Render("┃"))
	content.WriteString("\n")

	content.WriteString(borderStyle.Render("┣") + borderStyle.Render(strings.Repeat("━", panelWidth-1)) + borderStyle.Render("┫"))
	content.WriteString("\n")

	// Steps
	if !p.collapsed {
		for _, step := range p.steps {
			stepLine := p.renderStep(step, panelWidth-4)
			content.WriteString(borderStyle.Render("┃ "))
			content.WriteString(stepLine)
			padding := panelWidth - 3 - lipgloss.Width(stepLine)
			if padding > 0 {
				content.WriteString(strings.Repeat(" ", padding))
			}
			content.WriteString(borderStyle.Render(" ┃"))
			content.WriteString("\n")
		}
	} else {
		// Collapsed: show only current step
		for _, step := range p.steps {
			if step.Status == PlanStepInProgress {
				stepLine := p.renderStep(step, panelWidth-4)
				content.WriteString(borderStyle.Render("│ "))
				content.WriteString(stepLine)
				padding := panelWidth - 3 - lipgloss.Width(stepLine)
				if padding > 0 {
					content.WriteString(strings.Repeat(" ", padding))
				}
				content.WriteString(borderStyle.Render(" │"))
				content.WriteString("\n")
				break
			}
		}

		// Show compact summary
		summaryLine := dimStyle.Render(fmt.Sprintf("  ... %d more steps (press Tab to expand)", len(p.steps)-1))
		content.WriteString(borderStyle.Render("│"))
		content.WriteString(summaryLine)
		padding := panelWidth - 1 - lipgloss.Width(summaryLine)
		if padding > 0 {
			content.WriteString(strings.Repeat(" ", padding))
		}
		content.WriteString(borderStyle.Render("│"))
		content.WriteString("\n")
	}

	// Current tool activity (live indicator)
	if p.currentTool != "" {
		content.WriteString(borderStyle.Render("┣") + borderStyle.Render(strings.Repeat("─", panelWidth-1)) + borderStyle.Render("┫"))
		content.WriteString("\n")

		// Animated spinner
		spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		spinner := spinners[p.frame%len(spinners)]

		toolStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
		infoStyle := lipgloss.NewStyle().Foreground(ColorAccent)

		toolLine := "  " + toolStyle.Render(spinner+" "+p.currentTool)
		if p.currentInfo != "" {
			info := p.currentInfo
			maxInfoLen := panelWidth - lipgloss.Width(toolLine) - 6
			if maxInfoLen > 0 && len(info) > maxInfoLen {
				info = info[:maxInfoLen-3] + "..."
			}
			if maxInfoLen > 0 {
				toolLine += " " + infoStyle.Render(info)
			}
		}

		content.WriteString(borderStyle.Render("┃"))
		content.WriteString(toolLine)
		padding := panelWidth - 1 - lipgloss.Width(toolLine)
		if padding > 0 {
			content.WriteString(strings.Repeat(" ", padding))
		}
		content.WriteString(borderStyle.Render("┃"))
		content.WriteString("\n")
	}

	// Recent activity log
	if len(p.activities) > 0 {
		if p.currentTool == "" {
			content.WriteString(borderStyle.Render("┣") + borderStyle.Render(strings.Repeat("─", panelWidth-1)) + borderStyle.Render("┫"))
			content.WriteString("\n")
		}

		activityStyle := lipgloss.NewStyle().Foreground(ColorDim).Italic(true)

		// Show last few activities
		startIdx := 0
		if len(p.activities) > 3 {
			startIdx = len(p.activities) - 3
		}

		for i := startIdx; i < len(p.activities); i++ {
			act := p.activities[i]
			age := time.Since(act.Timestamp)
			ageStr := ""
			if age < time.Minute {
				ageStr = fmt.Sprintf("%ds ago", int(age.Seconds()))
			}

			actLine := "  " + activityStyle.Render("› "+act.Message)
			if ageStr != "" && panelWidth > 60 {
				actLine += " " + dimStyle.Render(ageStr)
			}

			content.WriteString(borderStyle.Render("┃"))
			content.WriteString(actLine)
			padding := panelWidth - 1 - lipgloss.Width(actLine)
			if padding > 0 {
				content.WriteString(strings.Repeat(" ", padding))
			}
			content.WriteString(borderStyle.Render("┃"))
			content.WriteString("\n")
		}
	}

	// Footer
	content.WriteString(borderStyle.Render("┗" + strings.Repeat("━", panelWidth-1) + "┛"))

	return content.String()
}

// renderStep renders a single step with status icon.
func (p *PlanProgressPanel) renderStep(step PlanStepState, maxWidth int) string {
	// Status icon with animation for in-progress
	var icon string
	var iconStyle lipgloss.Style

	switch step.Status {
	case PlanStepPending:
		icon = "○"
		iconStyle = lipgloss.NewStyle().Foreground(ColorDim)
	case PlanStepInProgress:
		// Animated spinner
		spinners := []string{"◐", "◓", "◑", "◒"}
		icon = spinners[p.frame%len(spinners)]
		iconStyle = lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	case PlanStepCompleted:
		icon = "✓"
		iconStyle = lipgloss.NewStyle().Foreground(ColorSuccess)
	case PlanStepFailed:
		icon = "✗"
		iconStyle = lipgloss.NewStyle().Foreground(ColorError)
	case PlanStepSkipped:
		icon = "↷"
		iconStyle = lipgloss.NewStyle().Foreground(ColorMuted)
	}

	// Step title
	titleStyle := lipgloss.NewStyle().Foreground(ColorText)
	if step.Status == PlanStepInProgress {
		titleStyle = titleStyle.Bold(true)
	} else if step.Status == PlanStepPending {
		titleStyle = lipgloss.NewStyle().Foreground(ColorDim)
	}

	title := step.Title
	maxTitleWidth := maxWidth - 5 // icon + space + padding

	// Add duration for completed/failed steps
	durationStr := ""
	if step.Status == PlanStepCompleted || step.Status == PlanStepFailed {
		if !step.CompletedAt.IsZero() && !step.StartedAt.IsZero() {
			duration := step.CompletedAt.Sub(step.StartedAt)
			durationStr = " " + lipgloss.NewStyle().Foreground(ColorDim).Render("("+formatElapsed(duration)+")")
			maxTitleWidth -= lipgloss.Width(durationStr)
		}
	}

	if len(title) > maxTitleWidth {
		title = title[:maxTitleWidth-3] + "..."
	}

	return iconStyle.Render(icon) + " " + titleStyle.Render(title) + durationStr
}

// renderProgressBar renders a visual progress bar.
func (p *PlanProgressPanel) renderProgressBar(filled, width int, progress float64) string {
	// Determine color based on progress
	var barColor lipgloss.Color
	if progress >= 1.0 {
		barColor = ColorSuccess
	} else if progress >= 0.5 {
		barColor = ColorWarning
	} else {
		barColor = ColorPlan
	}

	barStyle := lipgloss.NewStyle().Foreground(barColor)
	emptyStyle := lipgloss.NewStyle().Foreground(ColorDim)

	bar := barStyle.Render(strings.Repeat("█", filled)) +
		emptyStyle.Render(strings.Repeat("░", width-filled))

	return bar
}

// formatElapsed formats a duration for display.
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

// ViewCompact renders a compact single-line version for status bar.
func (p *PlanProgressPanel) ViewCompact() string {
	if !p.visible || len(p.steps) == 0 {
		return ""
	}

	progress := p.Progress()
	completed := p.CompletedCount()
	total := len(p.steps)

	// Find current step
	currentTitle := ""
	for _, step := range p.steps {
		if step.Status == PlanStepInProgress {
			currentTitle = step.Title
			if len(currentTitle) > 15 {
				currentTitle = currentTitle[:12] + "..."
			}
			break
		}
	}

	// Animated icon
	var icon string
	if progress >= 1.0 {
		icon = "✓"
	} else {
		spinners := []string{"◐", "◓", "◑", "◒"}
		icon = spinners[p.frame%len(spinners)]
	}

	planStyle := lipgloss.NewStyle().Foreground(ColorPlan).Bold(true)
	mutedStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	result := planStyle.Render(icon+" Plan") +
		mutedStyle.Render(fmt.Sprintf(" %d/%d", completed, total))

	if currentTitle != "" {
		result += lipgloss.NewStyle().Foreground(ColorDim).Render(" • " + currentTitle)
	}

	return result
}

// RenderStepNotification renders a notification for step status change.
func (p *PlanProgressPanel) RenderStepNotification(stepID int, status PlanStepStatus) string {
	var step *PlanStepState
	for i := range p.steps {
		if p.steps[i].ID == stepID {
			step = &p.steps[i]
			break
		}
	}

	if step == nil {
		return ""
	}

	var icon, verb string
	var color lipgloss.Color

	switch status {
	case PlanStepInProgress:
		icon = "▶"
		verb = "Starting"
		color = ColorWarning
	case PlanStepCompleted:
		icon = "✓"
		verb = "Completed"
		color = ColorSuccess
	case PlanStepFailed:
		icon = "✗"
		verb = "Failed"
		color = ColorError
	case PlanStepSkipped:
		icon = "↷"
		verb = "Skipped"
		color = ColorMuted
	default:
		return ""
	}

	style := lipgloss.NewStyle().Foreground(color)
	titleStyle := lipgloss.NewStyle().Foreground(ColorText)

	stepNum := fmt.Sprintf("[%d/%d]", stepID, len(p.steps))

	return style.Render(icon+" "+verb) + " " +
		lipgloss.NewStyle().Foreground(ColorDim).Render(stepNum) + " " +
		titleStyle.Render(step.Title)
}
