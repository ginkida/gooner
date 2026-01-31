package app

import (
	"context"
	"sync"
	"time"

	"gokin/internal/logging"
)

// Task represents a task to be executed in parallel.
type Task struct {
	ID       string
	Name     string
	Priority int
	Execute  func(ctx context.Context) error
	Timeout  time.Duration
}

// TaskResult represents the result of a task execution.
type TaskResult struct {
	TaskID    string
	Success   bool
	Error     error
	Duration  time.Duration
	Result    interface{}
	StartTime time.Time
	EndTime   time.Time
}

// ActiveTask represents a task that is currently running.
type ActiveTask struct {
	Task      *Task
	StartTime time.Time
	Progress  float64
	Status    string
}

// ParallelExecutor manages parallel task execution with concurrency control.
type ParallelExecutor struct {
	maxConcurrent int
	timeout       time.Duration
	activeTasks   map[string]*ActiveTask
	mu            sync.RWMutex
	cancelFuncs   map[string]context.CancelFunc

	// Metrics for UI
	submittedCount int
	completedCount int
	failedCount    int

	// UI callbacks
	onTaskStart    func(taskID string)
	onTaskComplete func(taskID string, err error)
}

// NewParallelExecutor creates a new parallel executor.
func NewParallelExecutor(maxConcurrent int, timeout time.Duration) *ParallelExecutor {
	return &ParallelExecutor{
		maxConcurrent:  maxConcurrent,
		timeout:        timeout,
		activeTasks:    make(map[string]*ActiveTask),
		cancelFuncs:    make(map[string]context.CancelFunc),
		submittedCount: 0,
	}
}

// ExecuteTasks executes multiple tasks in parallel with concurrency limiting.
func (pe *ParallelExecutor) ExecuteTasks(ctx context.Context, tasks []*Task) ([]TaskResult, error) {
	if len(tasks) == 0 {
		return []TaskResult{}, nil
	}

	logging.Info("starting parallel execution",
		"total_tasks", len(tasks),
		"max_concurrent", pe.maxConcurrent)

	startTime := time.Now()
	results := make([]TaskResult, len(tasks))
	semaphore := make(chan struct{}, pe.maxConcurrent)
	var wg sync.WaitGroup
	resultsChan := make(chan TaskResult, len(tasks))

	// Track submitted count
	pe.mu.Lock()
	pe.submittedCount += len(tasks)
	pe.mu.Unlock()

	// Submit tasks to pool
	for _, task := range tasks {
		wg.Add(1)
		t := task

		semaphore <- struct{}{} // Acquire
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }() // Release

			// Create task context with timeout
			taskCtx, cancel := context.WithTimeout(ctx, pe.timeout)

			// Store cancel func for potential cancellation
			pe.mu.Lock()
			pe.cancelFuncs[t.ID] = cancel
			pe.mu.Unlock()

			defer func() {
				pe.mu.Lock()
				delete(pe.cancelFuncs, t.ID)
				pe.mu.Unlock()
				cancel()
			}()

			// Track active task
			activeTask := &ActiveTask{
				Task:      t,
				StartTime: time.Now(),
				Status:    "running",
			}
			pe.addActiveTask(t.ID, activeTask)
			defer pe.removeActiveTask(t.ID)

			// Trigger UI callback
			if pe.onTaskStart != nil {
				pe.onTaskStart(t.ID)
			}

			// Execute task
			taskStart := time.Now()
			var err error
			if t.Execute != nil {
				err = t.Execute(taskCtx)
			}
			duration := time.Since(taskStart)

			// Update metrics
			pe.mu.Lock()
			if err != nil {
				pe.failedCount++
			} else {
				pe.completedCount++
			}
			pe.mu.Unlock()

			result := TaskResult{
				TaskID:    t.ID,
				Success:   err == nil,
				Error:     err,
				Duration:  duration,
				StartTime: taskStart,
				EndTime:   taskStart.Add(duration),
			}

			// Trigger UI callback
			if pe.onTaskComplete != nil {
				pe.onTaskComplete(t.ID, err)
			}

			resultsChan <- result
		}()
	}

	// Wait for all tasks
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	i := 0
	for result := range resultsChan {
		results[i] = result
		i++
	}

	totalDuration := time.Since(startTime)
	successCount := 0
	failedCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		} else {
			failedCount++
		}
	}

	logging.Info("parallel execution completed",
		"total", len(tasks),
		"success", successCount,
		"failed", failedCount,
		"duration", totalDuration)

	return results, nil
}

// addActiveTask adds a task to the active tasks map.
func (pe *ParallelExecutor) addActiveTask(id string, task *ActiveTask) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.activeTasks[id] = task
}

// removeActiveTask removes a task from the active tasks map.
func (pe *ParallelExecutor) removeActiveTask(id string) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	delete(pe.activeTasks, id)
}

// GetActiveTasks returns a snapshot of currently active tasks.
func (pe *ParallelExecutor) GetActiveTasks() map[string]*ActiveTask {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	tasks := make(map[string]*ActiveTask, len(pe.activeTasks))
	for k, v := range pe.activeTasks {
		tasks[k] = v
	}
	return tasks
}

// CancelAll cancels all currently running tasks.
func (pe *ParallelExecutor) CancelAll() {
	pe.mu.Lock()
	// Collect all cancel functions first to avoid modifying map during iteration
	cancels := make([]struct {
		id     string
		cancel context.CancelFunc
	}, 0, len(pe.cancelFuncs))
	for id, cancel := range pe.cancelFuncs {
		cancels = append(cancels, struct {
			id     string
			cancel context.CancelFunc
		}{id, cancel})
	}
	// Clear the map
	pe.cancelFuncs = make(map[string]context.CancelFunc)
	pe.mu.Unlock()

	// Call cancels outside of lock
	for _, c := range cancels {
		c.cancel()
		logging.Debug("cancelled task", "id", c.id)
	}
}
