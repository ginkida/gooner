package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HierarchicalTask represents a task that can have subtasks.
type HierarchicalTask struct {
	ID        string
	Title     string
	Status    HierarchicalTaskStatus
	Progress  float64 // 0.0 - 1.0
	StartTime time.Time
	EndTime   time.Time
	Subtasks  []*HierarchicalTask
	Parent    *HierarchicalTask
	Error     string
	Message   string // Current status message
}

// HierarchicalTaskStatus represents the status of a hierarchical task.
type HierarchicalTaskStatus int

const (
	HierarchicalTaskPending HierarchicalTaskStatus = iota
	HierarchicalTaskRunning
	HierarchicalTaskCompleted
	HierarchicalTaskFailed
	HierarchicalTaskSkipped
)

// HierarchicalProgressModel manages hierarchical task progress display.
type HierarchicalProgressModel struct {
	tasks     map[string]*HierarchicalTask
	rootTasks []string // IDs of root-level tasks
	spinner   spinner.Model
	width     int
	height    int
	collapsed map[string]bool // Track collapsed tasks

	mu sync.RWMutex
}

// NewHierarchicalProgressModel creates a new hierarchical progress model.
func NewHierarchicalProgressModel() HierarchicalProgressModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorGradient1).Bold(true)

	return HierarchicalProgressModel{
		tasks:     make(map[string]*HierarchicalTask),
		rootTasks: make([]string, 0),
		spinner:   s,
		collapsed: make(map[string]bool),
	}
}

// SetSize sets the display dimensions.
func (m *HierarchicalProgressModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// StartTask begins tracking a new task.
func (m *HierarchicalProgressModel) StartTask(id, title string, parentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task := &HierarchicalTask{
		ID:        id,
		Title:     title,
		Status:    HierarchicalTaskRunning,
		StartTime: time.Now(),
	}

	m.tasks[id] = task

	if parentID == "" {
		m.rootTasks = append(m.rootTasks, id)
	} else if parent, ok := m.tasks[parentID]; ok {
		task.Parent = parent
		parent.Subtasks = append(parent.Subtasks, task)
	}
}

// UpdateProgress updates task progress.
func (m *HierarchicalProgressModel) UpdateProgress(id string, progress float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if task, ok := m.tasks[id]; ok {
		task.Progress = progress
		m.recalculateParentProgress(task.Parent)
	}
}

// UpdateMessage updates the task's status message.
func (m *HierarchicalProgressModel) UpdateMessage(id string, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if task, ok := m.tasks[id]; ok {
		task.Message = message
	}
}

// CompleteTask marks a task as completed.
func (m *HierarchicalProgressModel) CompleteTask(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if task, ok := m.tasks[id]; ok {
		task.EndTime = time.Now()
		if err != nil {
			task.Status = HierarchicalTaskFailed
			task.Error = err.Error()
		} else {
			task.Status = HierarchicalTaskCompleted
			task.Progress = 1.0
		}
		m.recalculateParentProgress(task.Parent)
	}
}

// SkipTask marks a task as skipped.
func (m *HierarchicalProgressModel) SkipTask(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if task, ok := m.tasks[id]; ok {
		task.Status = HierarchicalTaskSkipped
		task.EndTime = time.Now()
		m.recalculateParentProgress(task.Parent)
	}
}

// ToggleCollapsed toggles the collapsed state of a task.
func (m *HierarchicalProgressModel) ToggleCollapsed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collapsed[id] = !m.collapsed[id]
}

// Clear removes all tasks.
func (m *HierarchicalProgressModel) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tasks = make(map[string]*HierarchicalTask)
	m.rootTasks = make([]string, 0)
	m.collapsed = make(map[string]bool)
}

// GetTask returns a task by ID.
func (m *HierarchicalProgressModel) GetTask(id string) (*HierarchicalTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	task, ok := m.tasks[id]
	return task, ok
}

// HasActiveTasks returns true if there are running tasks.
func (m *HierarchicalProgressModel) HasActiveTasks() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, task := range m.tasks {
		if task.Status == HierarchicalTaskRunning {
			return true
		}
	}
	return false
}

// recalculateParentProgress recalculates a parent's progress based on its subtasks.
func (m *HierarchicalProgressModel) recalculateParentProgress(parent *HierarchicalTask) {
	if parent == nil || len(parent.Subtasks) == 0 {
		return
	}

	var totalProgress float64
	var completedCount int

	for _, sub := range parent.Subtasks {
		switch sub.Status {
		case HierarchicalTaskCompleted:
			totalProgress += 1.0
			completedCount++
		case HierarchicalTaskSkipped:
			completedCount++ // Count skipped as done
		case HierarchicalTaskFailed:
			completedCount++ // Count failed as done (can't progress)
		default:
			totalProgress += sub.Progress
		}
	}

	parent.Progress = totalProgress / float64(len(parent.Subtasks))

	// Recursively update grandparent
	if parent.Parent != nil {
		m.recalculateParentProgress(parent.Parent)
	}
}

// Init initializes the model.
func (m *HierarchicalProgressModel) Init() tea.Cmd {
	return m.spinner.Tick
}

// Update handles messages.
func (m *HierarchicalProgressModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)

	case HierarchicalProgressStartMsg:
		m.StartTask(msg.ID, msg.Title, msg.ParentID)

	case HierarchicalProgressUpdateMsg:
		m.UpdateProgress(msg.ID, msg.Progress)
		if msg.Message != "" {
			m.UpdateMessage(msg.ID, msg.Message)
		}

	case HierarchicalProgressCompleteMsg:
		m.CompleteTask(msg.ID, msg.Error)
	}

	return nil
}

// View renders the hierarchical progress.
func (m *HierarchicalProgressModel) View() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.rootTasks) == 0 {
		return ""
	}

	var sb strings.Builder

	for _, id := range m.rootTasks {
		if task, ok := m.tasks[id]; ok {
			m.renderTaskLocked(&sb, task, 0)
		}
	}

	return sb.String()
}

// renderTaskLocked renders a single task and its subtasks (must be called with lock held).
func (m *HierarchicalProgressModel) renderTaskLocked(sb *strings.Builder, task *HierarchicalTask, depth int) {
	indent := strings.Repeat("  ", depth)

	// Status icon
	var icon string
	var iconStyle lipgloss.Style

	switch task.Status {
	case HierarchicalTaskPending:
		icon = "○"
		iconStyle = lipgloss.NewStyle().Foreground(ColorMuted)
	case HierarchicalTaskRunning:
		icon = m.spinner.View()
		iconStyle = lipgloss.NewStyle().Foreground(ColorRunning)
	case HierarchicalTaskCompleted:
		icon = "✓"
		iconStyle = lipgloss.NewStyle().Foreground(ColorSuccess)
	case HierarchicalTaskFailed:
		icon = "✗"
		iconStyle = lipgloss.NewStyle().Foreground(ColorError)
	case HierarchicalTaskSkipped:
		icon = "−"
		iconStyle = lipgloss.NewStyle().Foreground(ColorWarning)
	}

	// Title style based on status
	titleStyle := lipgloss.NewStyle().Foreground(ColorText)
	if task.Status == HierarchicalTaskCompleted {
		titleStyle = titleStyle.Foreground(ColorMuted)
	} else if task.Status == HierarchicalTaskFailed {
		titleStyle = titleStyle.Foreground(ColorError)
	}

	// Collapse indicator for tasks with subtasks
	collapseIndicator := ""
	if len(task.Subtasks) > 0 {
		if m.collapsed[task.ID] {
			collapseIndicator = "▸ "
		} else {
			collapseIndicator = "▾ "
		}
	}

	// Progress bar for running tasks
	progressBar := ""
	if task.Status == HierarchicalTaskRunning && task.Progress > 0 {
		barWidth := 20
		filled := int(task.Progress * float64(barWidth))
		if filled > barWidth {
			filled = barWidth
		}

		filledStyle := lipgloss.NewStyle().Foreground(ColorPrimary)
		emptyStyle := lipgloss.NewStyle().Foreground(ColorDim)

		progressBar = fmt.Sprintf(" [%s%s] %d%%",
			filledStyle.Render(strings.Repeat("█", filled)),
			emptyStyle.Render(strings.Repeat("░", barWidth-filled)),
			int(task.Progress*100))
	}

	// Duration for completed/failed tasks
	duration := ""
	if task.Status == HierarchicalTaskCompleted || task.Status == HierarchicalTaskFailed {
		d := task.EndTime.Sub(task.StartTime)
		duration = " " + lipgloss.NewStyle().Foreground(ColorDim).Render(fmt.Sprintf("(%s)", formatHierarchicalDuration(d)))
	}

	// Build the line
	sb.WriteString(indent)
	sb.WriteString(collapseIndicator)
	sb.WriteString(iconStyle.Render(icon))
	sb.WriteString(" ")
	sb.WriteString(titleStyle.Render(task.Title))
	sb.WriteString(progressBar)
	sb.WriteString(duration)
	sb.WriteString("\n")

	// Status message
	if task.Message != "" && task.Status == HierarchicalTaskRunning {
		msgStyle := lipgloss.NewStyle().Foreground(ColorDim).Italic(true)
		sb.WriteString(indent + "    " + msgStyle.Render(task.Message) + "\n")
	}

	// Error message
	if task.Error != "" {
		errStyle := lipgloss.NewStyle().Foreground(ColorError)
		sb.WriteString(indent + "    " + errStyle.Render(task.Error) + "\n")
	}

	// Render subtasks if not collapsed
	if !m.collapsed[task.ID] {
		for _, sub := range task.Subtasks {
			m.renderTaskLocked(sb, sub, depth+1)
		}
	}
}

// formatHierarchicalDuration formats a duration for display.
func formatHierarchicalDuration(d time.Duration) string {
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

// Message types for hierarchical progress updates.

// HierarchicalProgressStartMsg starts a new task.
type HierarchicalProgressStartMsg struct {
	ID       string
	Title    string
	ParentID string
}

// HierarchicalProgressUpdateMsg updates task progress.
type HierarchicalProgressUpdateMsg struct {
	ID       string
	Progress float64
	Message  string
}

// HierarchicalProgressCompleteMsg completes a task.
type HierarchicalProgressCompleteMsg struct {
	ID    string
	Error error
}

// Convenience functions for creating progress messages.

// StartTaskMsg creates a message to start a new task.
func StartTaskMsg(id, title string) tea.Msg {
	return HierarchicalProgressStartMsg{ID: id, Title: title}
}

// StartSubtaskMsg creates a message to start a subtask.
func StartSubtaskMsg(id, title, parentID string) tea.Msg {
	return HierarchicalProgressStartMsg{ID: id, Title: title, ParentID: parentID}
}

// UpdateTaskProgressMsg creates a message to update task progress.
func UpdateTaskProgressMsg(id string, progress float64, message string) tea.Msg {
	return HierarchicalProgressUpdateMsg{ID: id, Progress: progress, Message: message}
}

// CompleteTaskMsg creates a message to mark a task complete.
func CompleteTaskMsg(id string, err error) tea.Msg {
	return HierarchicalProgressCompleteMsg{ID: id, Error: err}
}
