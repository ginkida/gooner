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

	// Set up parallel executor callbacks (only if parallelExecutor exists)
	if a.parallelExecutor != nil {
		a.setupParallelExecutorCallbacks()
	}

	// Start the update manager
	a.uiUpdateManager.Start()

	logging.Debug("UI update system initialized")
}

// setupParallelExecutorCallbacks sets up UI callbacks for parallel executor.
func (a *App) setupParallelExecutorCallbacks() {
	if a.parallelExecutor == nil {
		return
	}

	// Store callbacks for later use
	a.parallelExecutorCallbacks = map[string]any{
		"OnTaskStart": func(taskID string) {
			if a.uiUpdateManager == nil || a.parallelExecutor == nil {
				return
			}
			a.parallelExecutor.mu.RLock()
			defer a.parallelExecutor.mu.RUnlock()
			if task, exists := a.parallelExecutor.activeTasks[taskID]; exists {
				a.uiUpdateManager.BroadcastTaskStart(taskID, task.Task.Name, "parallel")
			}
		},
		"OnTaskComplete": func(taskID string, err error) {
			if a.uiUpdateManager != nil {
				success := err == nil
				duration := time.Duration(0)
				a.uiUpdateManager.BroadcastTaskComplete(taskID, success, duration, err, "parallel")
			}
		},
		"OnTaskProgress": func(taskID string, progress float64, message string) {
			if a.uiUpdateManager != nil {
				a.uiUpdateManager.BroadcastTaskProgress(taskID, progress, message)
			}
		},
	}

	// Wire up callbacks to parallel executor
	if a.parallelExecutor != nil {
		onStart := func(taskID string) {
			if fn, ok := a.parallelExecutorCallbacks["OnTaskStart"].(func(string)); ok {
				fn(taskID)
			}
		}
		onComplete := func(taskID string, err error) {
			if fn, ok := a.parallelExecutorCallbacks["OnTaskComplete"].(func(string, error)); ok {
				fn(taskID, err)
			}
		}
		a.parallelExecutor.SetUICallbacks(onStart, onComplete)
	}

	logging.Debug("parallel executor UI callbacks configured")
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
