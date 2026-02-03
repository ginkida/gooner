package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"gokin/internal/logging"
)

// OrchestratorTaskStatus represents the state of a task.
type OrchestratorTaskStatus int

const (
	OrchStatusPending OrchestratorTaskStatus = iota
	OrchStatusReady
	OrchStatusRunning
	OrchStatusCompleted
	OrchStatusFailed
	OrchStatusSkipped
)

// OrchestratorTask priority.
type OrchPriority int

const (
	OrchPriorityLow OrchPriority = iota
	OrchPriorityNormal
	OrchPriorityHigh
)

// OrchestratorTask represents a unified unit of work.
type OrchestratorTask struct {
	ID           string
	Name         string
	Priority     OrchPriority
	Dependencies []string
	Execute      func(ctx context.Context) error
	OnComplete   func(err error)

	Status      OrchestratorTaskStatus
	Error       error
	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time

	mu sync.RWMutex
}

// TaskOrchestrator manages the execution of tasks with priorities and dependencies.
type TaskOrchestrator struct {
	tasks         map[string]*OrchestratorTask
	maxConcurrent int
	timeout       time.Duration

	activeCount int
	taskChan    chan *OrchestratorTask

	onStatusChange func(taskID string, status OrchestratorTaskStatus)

	mu sync.RWMutex
	wg sync.WaitGroup
}

// NewTaskOrchestrator creates a new orchestrator.
func NewTaskOrchestrator(maxConcurrent int, timeout time.Duration) *TaskOrchestrator {
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}
	return &TaskOrchestrator{
		tasks:         make(map[string]*OrchestratorTask),
		maxConcurrent: maxConcurrent,
		timeout:       timeout,
		taskChan:      make(chan *OrchestratorTask, 100),
	}
}

// Submit adds a single task.
func (o *TaskOrchestrator) Submit(task *OrchestratorTask) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if task.ID == "" {
		task.ID = fmt.Sprintf("task_%d", time.Now().UnixNano())
	}
	if _, exists := o.tasks[task.ID]; exists {
		return fmt.Errorf("task %s already exists", task.ID)
	}

	task.Status = OrchStatusPending
	task.CreatedAt = time.Now()
	o.tasks[task.ID] = task

	go o.schedule()
	return nil
}

// SubmitGroup adds multiple tasks that might have inter-dependencies.
func (o *TaskOrchestrator) SubmitGroup(tasks []*OrchestratorTask) error {
	for _, task := range tasks {
		if err := o.Submit(task); err != nil {
			return err
		}
	}
	return nil
}

// SetOnStatusChange sets the callback for UI updates.
func (o *TaskOrchestrator) SetOnStatusChange(fn func(string, OrchestratorTaskStatus)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.onStatusChange = fn
}

// Start begins processing the queue.
func (o *TaskOrchestrator) Start(ctx context.Context) {
	logging.Info("TaskOrchestrator started", "max_concurrent", o.maxConcurrent)
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-o.taskChan:
			o.executeTask(ctx, task)
		}
	}
}

// schedule checks for ready tasks and sends them to taskChan.
func (o *TaskOrchestrator) schedule() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.activeCount >= o.maxConcurrent {
		return
	}

	// Find all ready tasks
	var readyTasks []*OrchestratorTask
	for _, t := range o.tasks {
		t.mu.RLock()
		if t.Status == OrchStatusPending && o.isReady(t) {
			readyTasks = append(readyTasks, t)
		}
		t.mu.RUnlock()
	}

	// Sort by priority (High first), then by creation time
	sort.Slice(readyTasks, func(i, j int) bool {
		if readyTasks[i].Priority != readyTasks[j].Priority {
			return readyTasks[i].Priority > readyTasks[j].Priority
		}
		return readyTasks[i].CreatedAt.Before(readyTasks[j].CreatedAt)
	})

	// Fill available slots
	for _, t := range readyTasks {
		if o.activeCount >= o.maxConcurrent {
			break
		}

		t.mu.Lock()
		t.Status = OrchStatusReady
		t.mu.Unlock()

		o.activeCount++
		o.taskChan <- t
	}
}

func (o *TaskOrchestrator) isReady(t *OrchestratorTask) bool {
	for _, depID := range t.Dependencies {
		dep, exists := o.tasks[depID]
		if !exists {
			// Missing dependency - treat as failed, task cannot proceed
			logging.Warn("task has missing dependency", "task_id", t.ID, "missing_dep", depID)
			return false
		}
		dep.mu.RLock()
		status := dep.Status
		dep.mu.RUnlock()
		if status != OrchStatusCompleted {
			return false
		}
	}
	return true
}

func (o *TaskOrchestrator) executeTask(ctx context.Context, t *OrchestratorTask) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() {
			o.mu.Lock()
			o.activeCount--
			o.mu.Unlock()
			o.schedule() // Check for new ready tasks
		}()

		now := time.Now()
		t.mu.Lock()
		t.Status = OrchStatusRunning
		t.StartedAt = &now
		t.mu.Unlock()

		if o.onStatusChange != nil {
			o.onStatusChange(t.ID, OrchStatusRunning)
		}

		// Execute with timeout
		taskCtx, cancel := context.WithTimeout(ctx, o.timeout)
		defer cancel()

		err := t.Execute(taskCtx)

		t.mu.Lock()
		compNow := time.Now()
		t.CompletedAt = &compNow
		if err != nil {
			t.Status = OrchStatusFailed
			t.Error = err
			logging.Error("task failed", "id", t.ID, "error", err)
		} else {
			t.Status = OrchStatusCompleted
		}
		t.mu.Unlock()

		if o.onStatusChange != nil {
			status := OrchStatusCompleted
			if err != nil {
				status = OrchStatusFailed
			}
			o.onStatusChange(t.ID, status)
		}

		if t.OnComplete != nil {
			t.OnComplete(err)
		}

		// If task failed, skip dependent tasks
		if err != nil {
			o.skipDependents(t.ID)
		}
	}()
}

func (o *TaskOrchestrator) skipDependents(failedID string) {
	// Collect tasks to skip while holding the lock
	var toSkip []string

	o.mu.Lock()
	for _, t := range o.tasks {
		t.mu.Lock()
		if t.Status == OrchStatusPending {
			for _, depID := range t.Dependencies {
				if depID == failedID {
					t.Status = OrchStatusSkipped
					t.Error = fmt.Errorf("dependency %s failed", failedID)
					toSkip = append(toSkip, t.ID)
					if o.onStatusChange != nil {
						o.onStatusChange(t.ID, OrchStatusSkipped)
					}
					break
				}
			}
		}
		t.mu.Unlock()
	}
	o.mu.Unlock()

	// Recursively skip dependents without holding the lock
	for _, id := range toSkip {
		o.skipDependents(id)
	}
}

// GetStats returns current orchestrator metrics.
func (o *TaskOrchestrator) GetStats() map[string]int {
	o.mu.RLock()
	defer o.mu.RUnlock()

	stats := make(map[string]int)
	for _, t := range o.tasks {
		t.mu.RLock()
		switch t.Status {
		case OrchStatusPending:
			stats["pending"]++
		case OrchStatusRunning:
			stats["running"]++
		case OrchStatusCompleted:
			stats["completed"]++
		case OrchStatusFailed:
			stats["failed"]++
		case OrchStatusSkipped:
			stats["skipped"]++
		}
		t.mu.RUnlock()
	}
	stats["active_count"] = o.activeCount
	return stats
}

// Wait blocks until all submitted tasks have completed.
func (o *TaskOrchestrator) Wait() {
	o.wg.Wait()
}

// Cleanup removes completed, failed, and skipped tasks from the task map.
// Returns the number of tasks removed.
func (o *TaskOrchestrator) Cleanup() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	var toRemove []string
	for id, t := range o.tasks {
		t.mu.RLock()
		status := t.Status
		t.mu.RUnlock()

		if status == OrchStatusCompleted || status == OrchStatusFailed || status == OrchStatusSkipped {
			toRemove = append(toRemove, id)
		}
	}

	for _, id := range toRemove {
		delete(o.tasks, id)
	}

	return len(toRemove)
}

// GetTask returns a task by ID.
func (o *TaskOrchestrator) GetTask(id string) (*OrchestratorTask, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	t, ok := o.tasks[id]
	return t, ok
}

// CancelTask marks a pending task as skipped.
func (o *TaskOrchestrator) CancelTask(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	t, exists := o.tasks[id]
	if !exists {
		return fmt.Errorf("task %s not found", id)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.Status != OrchStatusPending {
		return fmt.Errorf("task %s is not pending (status: %d)", id, t.Status)
	}

	t.Status = OrchStatusSkipped
	t.Error = fmt.Errorf("cancelled by user")

	if o.onStatusChange != nil {
		o.onStatusChange(id, OrchStatusSkipped)
	}

	return nil
}
