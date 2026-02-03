package app

// GetCompletedCount returns completed and failed task counts for UI
func (pe *ParallelExecutor) GetCompletedCount() (completed int, failed int) {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return pe.completedCount, pe.failedCount
}

// GetSubmittedCount returns submitted task count for UI
func (pe *ParallelExecutor) GetSubmittedCount() int {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return pe.submittedCount
}
