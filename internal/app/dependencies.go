package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gooner/internal/logging"
)

// TaskStatus represents the current state of a task in the dependency graph.
type TaskStatus int

const (
	TaskStatusPending TaskStatus = iota
	TaskStatusReady
	TaskStatusRunning
	TaskStatusCompleted
	TaskStatusFailed
	TaskStatusSkipped
)

func (s TaskStatus) String() string {
	switch s {
	case TaskStatusPending:
		return "⏳ Pending"
	case TaskStatusReady:
		return "✅ Ready"
	case TaskStatusRunning:
		return "🔄 Running"
	case TaskStatusCompleted:
		return "✓ Completed"
	case TaskStatusFailed:
		return "❌ Failed"
	case TaskStatusSkipped:
		return "⊘ Skipped"
	default:
		return "Unknown"
	}
}

// Task represents a task in the dependency graph.
type DependencyTask struct {
	ID           string
	Message      string
	Priority     int
	Dependencies []string
	Status       TaskStatus
	Error        error
	Result       interface{}
	CreatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Execute      func(ctx context.Context) error
}

// TaskDependencies manages a DAG (Directed Acyclic Graph) of tasks with dependencies.
type TaskDependencies struct {
	tasks    map[string]*DependencyTask
	mu       sync.RWMutex
	taskChan chan *DependencyTask

	// UI callback for status changes
	onStatusChange func(taskID string, status TaskStatus)
}

// NewTaskDependencies creates a new task dependency manager.
func NewTaskDependencies() *TaskDependencies {
	return &TaskDependencies{
		tasks:    make(map[string]*DependencyTask),
		taskChan: make(chan *DependencyTask, 100),
	}
}

// AddTask adds a task to the dependency graph.
func (td *TaskDependencies) AddTask(task *DependencyTask) error {
	td.mu.Lock()
	defer td.mu.Unlock()

	if task.ID == "" {
		return fmt.Errorf("task ID cannot be empty")
	}

	if _, exists := td.tasks[task.ID]; exists {
		return fmt.Errorf("task with ID %s already exists", task.ID)
	}

	task.Status = TaskStatusPending
	task.CreatedAt = time.Now()
	td.tasks[task.ID] = task

	return nil
}

// AddTaskWithDependencies adds a task with its dependencies.
func (td *TaskDependencies) AddTaskWithDependencies(task *DependencyTask, dependencies []string) error {
	td.mu.Lock()
	defer td.mu.Unlock()

	if err := td.addTaskUnsafe(task); err != nil {
		return err
	}

	// Validate dependencies exist
	for _, depID := range dependencies {
		if _, exists := td.tasks[depID]; !exists {
			return fmt.Errorf("dependency %s not found for task %s", depID, task.ID)
		}
	}

	task.Dependencies = dependencies
	return nil
}

// addTaskUnsafe adds a task without locking (must be called with lock held).
func (td *TaskDependencies) addTaskUnsafe(task *DependencyTask) error {
	if task.ID == "" {
		return fmt.Errorf("task ID cannot be empty")
	}

	if _, exists := td.tasks[task.ID]; exists {
		return fmt.Errorf("task with ID %s already exists", task.ID)
	}

	task.Status = TaskStatusPending
	task.CreatedAt = time.Now()
	td.tasks[task.ID] = task
	return nil
}

// GetTask retrieves a task by ID.
func (td *TaskDependencies) GetTask(id string) (*DependencyTask, bool) {
	td.mu.RLock()
	defer td.mu.RUnlock()
	task, exists := td.tasks[id]
	return task, exists
}

// GetAllTasks returns all tasks.
func (td *TaskDependencies) GetAllTasks() []*DependencyTask {
	td.mu.RLock()
	defer td.mu.RUnlock()

	tasks := make([]*DependencyTask, 0, len(td.tasks))
	for _, task := range td.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// GetTaskStatus returns the current status of a task.
func (td *TaskDependencies) GetTaskStatus(id string) TaskStatus {
	td.mu.RLock()
	defer td.mu.RUnlock()

	if task, exists := td.tasks[id]; exists {
		return task.Status
	}
	return TaskStatusPending
}

// MarkTaskStatus updates the status of a task.
func (td *TaskDependencies) MarkTaskStatus(id string, status TaskStatus, err error) {
	td.mu.Lock()
	defer td.mu.Unlock()

	if task, exists := td.tasks[id]; exists {
		oldStatus := task.Status
		task.Status = status
		task.Error = err
		now := time.Now()

		switch status {
		case TaskStatusRunning:
			task.StartedAt = &now
		case TaskStatusCompleted, TaskStatusFailed, TaskStatusSkipped:
			task.CompletedAt = &now
		}

		// Trigger UI callback if status changed
		if oldStatus != status && td.onStatusChange != nil {
			td.onStatusChange(id, status)
		}
	}
}

// BuildExecutionOrder builds the execution order using Kahn's algorithm.
// Returns tasks grouped by execution level (tasks in the same level can run in parallel).
func (td *TaskDependencies) BuildExecutionOrder() ([][]string, error) {
	td.mu.RLock()
	defer td.mu.RUnlock()

	if len(td.tasks) == 0 {
		return nil, nil
	}

	// Calculate in-degree for each task
	inDegree := make(map[string]int)
	adjList := make(map[string][]string)

	for id := range td.tasks {
		inDegree[id] = 0
		adjList[id] = []string{}
	}

	for id, task := range td.tasks {
		for _, dep := range task.Dependencies {
			adjList[dep] = append(adjList[dep], id)
			inDegree[id]++
		}
	}

	// Find tasks with no dependencies (in-degree = 0)
	queue := make([]string, 0)
	for id, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}

	// Build execution levels
	var levels [][]string
	visited := make(map[string]bool)

	for len(queue) > 0 {
		level := queue
		levels = append(levels, level)
		queue = make([]string, 0)

		for _, taskId := range level {
			if visited[taskId] {
				continue
			}
			visited[taskId] = true

			// Reduce in-degree for dependent tasks
			for _, dependentId := range adjList[taskId] {
				inDegree[dependentId]--
				if inDegree[dependentId] == 0 {
					queue = append(queue, dependentId)
				}
			}
		}
	}

	// Check for cycles
	if len(visited) != len(td.tasks) {
		return nil, fmt.Errorf("cyclic dependency detected")
	}

	return levels, nil
}

// ExecutePlan represents the execution plan for tasks with dependencies.
type ExecutePlan struct {
	Levels         [][]string
	TotalTasks     int
	ExecutionDepth int
	MaxParallel    int
}

// GetPlan returns the execution plan.
func (td *TaskDependencies) GetPlan() (*ExecutePlan, error) {
	levels, err := td.BuildExecutionOrder()
	if err != nil {
		return nil, err
	}

	if len(levels) == 0 {
		return &ExecutePlan{
			Levels:         [][]string{},
			TotalTasks:     0,
			ExecutionDepth: 0,
			MaxParallel:    0,
		}, nil
	}

	maxParallel := 0
	for _, level := range levels {
		if len(level) > maxParallel {
			maxParallel = len(level)
		}
	}

	return &ExecutePlan{
		Levels:         levels,
		TotalTasks:     len(td.tasks),
		ExecutionDepth: len(levels),
		MaxParallel:    maxParallel,
	}, nil
}

// DependencyStats represents statistics about task dependencies.
type DependencyStats struct {
	TotalTasks      int
	Pending         int
	Ready           int
	Running         int
	Completed       int
	Failed          int
	Skipped         int
	ExecutionLevels int
	HasCycles       bool
}

// GetStats returns statistics about the dependency graph.
func (td *TaskDependencies) GetStats() DependencyStats {
	td.mu.RLock()
	defer td.mu.RUnlock()

	stats := DependencyStats{
		TotalTasks: len(td.tasks),
	}

	for _, task := range td.tasks {
		switch task.Status {
		case TaskStatusPending:
			stats.Pending++
		case TaskStatusReady:
			stats.Ready++
		case TaskStatusRunning:
			stats.Running++
		case TaskStatusCompleted:
			stats.Completed++
		case TaskStatusFailed:
			stats.Failed++
		case TaskStatusSkipped:
			stats.Skipped++
		}
	}

	// Check for cycles
	_, err := td.BuildExecutionOrder()
	stats.HasCycles = err != nil

	// Calculate execution depth
	if levels, err := td.BuildExecutionOrder(); err == nil {
		stats.ExecutionLevels = len(levels)
	}

	return stats
}

// DependencyManager manages task dependencies with queue integration.
type DependencyManager struct {
	deps *TaskDependencies
}

// NewDependencyManager creates a new dependency manager.
func NewDependencyManager() *DependencyManager {
	return &DependencyManager{
		deps: NewTaskDependencies(),
	}
}

// AddTask adds a task to the dependency manager.
func (dm *DependencyManager) AddTask(task *DependencyTask) error {
	return dm.deps.AddTask(task)
}

// AddTaskWithDependencies adds a task with dependencies.
func (dm *DependencyManager) AddTaskWithDependencies(task *DependencyTask, dependencies []string) error {
	return dm.deps.AddTaskWithDependencies(task, dependencies)
}

// GetTask retrieves a task by ID.
func (dm *DependencyManager) GetTask(id string) (*DependencyTask, bool) {
	return dm.deps.GetTask(id)
}

// GetPlan returns the execution plan.
func (dm *DependencyManager) GetPlan() (*ExecutePlan, error) {
	return dm.deps.GetPlan()
}

// GetStats returns statistics.
func (dm *DependencyManager) GetStats() DependencyStats {
	return dm.deps.GetStats()
}

// ExecuteDependencies executes tasks respecting dependencies.
// maxParallel specifies the maximum number of tasks to run concurrently per level.
func (dm *DependencyManager) ExecuteDependencies(ctx context.Context, maxParallel int) error {
	levels, err := dm.deps.BuildExecutionOrder()
	if err != nil {
		return fmt.Errorf("failed to build execution order: %w", err)
	}

	logging.Info("executing tasks with dependencies",
		"total_levels", len(levels),
		"total_tasks", len(dm.deps.tasks),
		"max_parallel", maxParallel)

	for levelIdx, level := range levels {
		logging.Info("starting execution level",
			"level", levelIdx+1,
			"tasks", len(level))

		// Execute tasks in this level in parallel
		semaphore := make(chan struct{}, maxParallel)
		var wg sync.WaitGroup
		errors := make([]error, 0)

		for _, taskID := range level {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()

				// Acquire semaphore
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				task, exists := dm.deps.GetTask(id)
				if !exists {
					logging.Error("task not found", "id", id)
					return
				}

				// Mark as running
				dm.deps.MarkTaskStatus(id, TaskStatusRunning, nil)

				logging.Debug("executing task",
					"id", id,
					"message", task.Message)

				// Execute task
				var err error
				if task.Execute != nil {
					err = task.Execute(ctx)
				}

				if err != nil {
					logging.Warn("task failed",
						"id", id,
						"error", err)
					dm.deps.MarkTaskStatus(id, TaskStatusFailed, err)
					errors = append(errors, err)
				} else {
					logging.Debug("task completed",
						"id", id)
					dm.deps.MarkTaskStatus(id, TaskStatusCompleted, nil)
				}
			}(taskID)
		}

		// Wait for all tasks in this level to complete
		wg.Wait()

		// If any task failed, mark dependent tasks as skipped
		if len(errors) > 0 {
			dm.markDependentTasksSkipped(level)
		}

		logging.Info("execution level completed",
			"level", levelIdx+1,
			"failed", len(errors))
	}

	return nil
}

// markDependentTasksSkipped marks all tasks that depend on failed tasks as skipped.
func (dm *DependencyManager) markDependentTasksSkipped(currentLevel []string) {
	// Find all failed tasks in current level
	failedTasks := make(map[string]bool)
	for _, taskID := range currentLevel {
		if task, exists := dm.deps.GetTask(taskID); exists {
			if task.Status == TaskStatusFailed {
				failedTasks[taskID] = true
			}
		}
	}

	if len(failedTasks) == 0 {
		return
	}

	// Mark all tasks that depend on failed tasks as skipped
	for _, task := range dm.deps.GetAllTasks() {
		if task.Status == TaskStatusPending {
			for _, dep := range task.Dependencies {
				if failedTasks[dep] {
					logging.Info("marking task as skipped due to failed dependency",
						"id", task.ID,
						"failed_dependency", dep)
					dm.deps.MarkTaskStatus(task.ID, TaskStatusSkipped, fmt.Errorf("dependency %s failed", dep))
					break
				}
			}
		}
	}
}
