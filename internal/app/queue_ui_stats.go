package app

import (
	"time"
)

// GetUIStats returns queue statistics for UI visualization
type UIQueueStats struct {
	Queued         int
	Pending        int
	Ready          int
	Running        int
	Completed      int
	Failed         int
	TotalProcessed int
	AvgWaitTime    time.Duration
	AvgProcessTime time.Duration
	Throughput     float64 // tasks per second
}

// GetUIStats returns queue statistics for UI visualization
func (qm *QueueManager) GetUIStats() UIQueueStats {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	// Calculate throughput
	var throughput float64
	if qm.totalProcessed > 0 {
		throughput = float64(qm.totalProcessed)
	}

	return UIQueueStats{
		Queued:         qm.queue.Len(),
		Pending:        qm.queue.Len(),
		Ready:          0,
		Running:        boolToInt(qm.currentTask != nil),
		Completed:      qm.totalProcessed,
		Failed:         0,
		TotalProcessed: qm.totalProcessed,
		AvgWaitTime:    0,
		AvgProcessTime: 0,
		Throughput:     throughput,
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
