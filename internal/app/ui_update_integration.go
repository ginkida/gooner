package app

import (
	"time"

	"gokin/internal/logging"
)

// initializeUIUpdateSystem initializes the UI Auto-Update System.
// This should be called after a.program is set in Run().
func (a *App) initializeUIUpdateSystem() {
	if a.program == nil {
		logging.Debug("UI update system not initialized: program not set")
		return
	}

	// Create UI update manager
	a.uiUpdateManager = NewUIUpdateManager(a.program, a)

	// Set up orchestrator callbacks
	if a.orchestrator != nil {
		a.setupOrchestratorCallbacks()
	}
}

// setupOrchestratorCallbacks sets up UI callbacks for the unified task orchestrator.
func (a *App) setupOrchestratorCallbacks() {
	if a.orchestrator == nil {
		return
	}

	a.orchestrator.SetOnStatusChange(func(taskID string, status OrchestratorTaskStatus) {
		if a.uiUpdateManager == nil {
			return
		}

		// Map orchestrator status to UI broadcast
		switch status {
		case OrchStatusRunning:
			// For orchestrator, we might need more details.
			// For now, simple broadcast.
			a.uiUpdateManager.BroadcastTaskStart(taskID, "Task "+taskID, "orchestrator")
		case OrchStatusCompleted:
			a.uiUpdateManager.BroadcastTaskComplete(taskID, true, 0, nil, "orchestrator")
		case OrchStatusFailed:
			a.uiUpdateManager.BroadcastTaskComplete(taskID, false, 0, nil, "orchestrator")
		case OrchStatusSkipped:
			a.uiUpdateManager.BroadcastTaskComplete(taskID, false, 0, nil, "orchestrator")
		}
	})

	logging.Debug("orchestrator UI callbacks configured")
}

// NotifyPlanStepStarted notifies the UI that a plan step has started.
func (a *App) NotifyPlanStepStarted(stepID int, title string) {
	// Plan executor integration removed
}

// NotifyPlanStepCompleted notifies the UI that a plan step has completed.
func (a *App) NotifyPlanStepCompleted(stepID int, title string, success bool, duration time.Duration, err error) {
	// Plan executor integration removed
}

// NotifyPlanStepProgress notifies the UI about plan step progress.
func (a *App) NotifyPlanStepProgress(stepID int, progress float64, message string) {
	// Plan executor integration removed
}

// BroadcastTaskStart broadcasts a task start event to the UI.
func (a *App) BroadcastTaskStart(taskID, message, taskType string) {
	if a.uiUpdateManager != nil {
		a.uiUpdateManager.BroadcastTaskStart(taskID, message, taskType)
	}
}

// BroadcastTaskComplete broadcasts a task completion event to the UI.
func (a *App) BroadcastTaskComplete(taskID string, success bool, duration time.Duration, err error, taskType string) {
	if a.uiUpdateManager != nil {
		a.uiUpdateManager.BroadcastTaskComplete(taskID, success, duration, err, taskType)
	}
}

// BroadcastTaskProgress broadcasts a task progress event to the UI.
func (a *App) BroadcastTaskProgress(taskID string, progress float64, message string) {
	if a.uiUpdateManager != nil {
		a.uiUpdateManager.BroadcastTaskProgress(taskID, progress, message)
	}
}

// GetUIUpdateManager returns the UI update manager (for testing/external access).
func (a *App) GetUIUpdateManager() *UIUpdateManager {
	return a.uiUpdateManager
}

// wirePlanExecutorNotifications wires up plan executor to send UI notifications.
func (a *App) wirePlanExecutorNotifications() {
	if a.planManager == nil {
		return
	}
	logging.Debug("plan executor notifications wired to UI")
}
