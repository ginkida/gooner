package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ========== Update Messages ==========
// Note: Dependency graph, parallel execution, and task queue visualization
// features have been removed. These types are kept for backward compatibility.

// DependencyGraphTickMsg is sent periodically (unused)
type DependencyGraphTickMsg time.Time

// DependencyGraphUpdatedMsg is sent when graph data is updated (unused)
type DependencyGraphUpdatedMsg struct {
	Data any
}

// ParallelExecutionTickMsg is sent periodically (unused)
type ParallelExecutionTickMsg time.Time

// ParallelExecutionUpdatedMsg is sent when parallel execution data is updated (unused)
type ParallelExecutionUpdatedMsg struct {
	Data any
}

// TaskQueueTickMsg is sent periodically (unused)
type TaskQueueTickMsg time.Time

// TaskQueueUpdatedMsg is sent when task queue data is updated (unused)
type TaskQueueUpdatedMsg struct {
	Data any
}

// ShowDependencyGraphMsg triggers dependency graph overlay (unused)
type ShowDependencyGraphMsg struct{}

// ShowParallelExecutionMsg triggers parallel execution overlay (unused)
type ShowParallelExecutionMsg struct{}

// ShowTaskQueueMsg triggers task queue overlay (unused)
type ShowTaskQueueMsg struct{}

// CloseOverlayMsg closes any open overlay
type CloseOverlayMsg struct{}

// ========== Task Execution Events ==========

// TaskStartedEvent is fired when a task starts execution
type TaskStartedEvent struct {
	TaskID   string
	Message  string
	PlanType string
}

// TaskCompletedEvent is fired when a task completes
type TaskCompletedEvent struct {
	TaskID   string
	Success  bool
	Duration time.Duration
	Error    error
	PlanType string
}

// TaskProgressEvent is fired for task progress updates
type TaskProgressEvent struct {
	TaskID   string
	Progress float64
	Message  string
}

// GetTaskEventsCmd returns a command that waits for task events
func GetTaskEventsCmd(taskID string) tea.Cmd {
	return func() tea.Msg {
		return nil
	}
}
