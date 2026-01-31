package app

// SetUICallbacks sets UI event callbacks for parallel executor
func (pe *ParallelExecutor) SetUICallbacks(
	onStart func(taskID string),
	onComplete func(taskID string, err error),
) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	pe.onTaskStart = onStart
	pe.onTaskComplete = onComplete
}

// BroadcastTaskStart broadcasts task start to UI
func (pe *ParallelExecutor) BroadcastTaskStart(taskID string) {
	pe.mu.RLock()
	onStart := pe.onTaskStart
	pe.mu.RUnlock()

	if onStart != nil {
		onStart(taskID)
	}
}

// BroadcastTaskComplete broadcasts task completion to UI
func (pe *ParallelExecutor) BroadcastTaskComplete(taskID string, err error) {
	pe.mu.RLock()
	onComplete := pe.onTaskComplete
	pe.mu.RUnlock()

	if onComplete != nil {
		onComplete(taskID, err)
	}
}

// NotifyTaskStarted notifies UI that a task has started
func (pe *ParallelExecutor) NotifyTaskStarted(taskID string) {
	pe.BroadcastTaskStart(taskID)
}

// NotifyTaskCompleted notifies UI that a task has completed
func (pe *ParallelExecutor) NotifyTaskCompleted(taskID string, success bool, err error) {
	pe.BroadcastTaskComplete(taskID, err)
}

// UpdateTaskProgress updates task progress and notifies UI
func (pe *ParallelExecutor) UpdateTaskProgress(taskID string, progress float64) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	if active, exists := pe.activeTasks[taskID]; exists {
		active.Progress = progress
	}
}
