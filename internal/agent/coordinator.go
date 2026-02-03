package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"gokin/internal/logging"
)

// Constants for resource management
const (
	// MaxCoordinatorTasks is the maximum number of completed tasks to keep
	MaxCoordinatorTasks = 100
)

// Coordinator manages multiple agents with dependencies and parallelism.
type Coordinator struct {
	runner       *Runner
	tasks        map[string]*CoordinatedTask
	dependencies map[string][]string // taskID -> dependent task IDs
	queue        *TaskQueue
	maxParallel  int

	// Tracking running agents
	running   map[string]string // agentID -> taskID
	completed map[string]bool   // completed taskIDs

	// Callbacks
	onTaskStart    func(task *CoordinatedTask)
	onTaskComplete func(task *CoordinatedTask, result *AgentResult)
	onAllComplete  func(results map[string]*AgentResult)

	// Event-driven channels for efficient processing
	taskReadyCh chan struct{} // Signals when a task becomes ready
	agentDoneCh chan string   // Signals when an agent completes (carries agentID)

	// Reflection for error learning feedback loop
	reflector *Reflector

	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc
}

// CoordinatorConfig holds configuration for the coordinator.
type CoordinatorConfig struct {
	MaxParallel int // Maximum concurrent agents (default: 3)
}

// NewCoordinator creates a new coordinator.
func NewCoordinator(runner *Runner, config *CoordinatorConfig) *Coordinator {
	if config == nil {
		config = &CoordinatorConfig{MaxParallel: 3}
	}
	if config.MaxParallel <= 0 {
		config.MaxParallel = 3
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Coordinator{
		runner:       runner,
		tasks:        make(map[string]*CoordinatedTask),
		dependencies: make(map[string][]string),
		queue:        NewTaskQueue(),
		maxParallel:  config.MaxParallel,
		running:      make(map[string]string),
		completed:    make(map[string]bool),
		taskReadyCh:  make(chan struct{}, 100), // Buffered to avoid blocking
		agentDoneCh:  make(chan string, 100),   // Buffered for agent completions
		reflector:    NewReflector(),           // Initialize reflector for feedback loop
		ctx:          ctx,
		cancel:       cancel,
	}
}

// SetReflector sets the reflector for error learning feedback loop.
func (c *Coordinator) SetReflector(r *Reflector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reflector = r
}

// cleanupCompletedTasks removes old completed tasks to prevent unbounded memory growth.
func (c *Coordinator) cleanupCompletedTasks() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Count completed tasks
	completedCount := len(c.completed)
	if completedCount <= MaxCoordinatorTasks {
		return
	}

	// Find and remove oldest completed tasks
	removeCount := completedCount - MaxCoordinatorTasks/2
	removed := 0
	for taskID := range c.completed {
		if removed >= removeCount {
			break
		}
		// Remove from all maps
		delete(c.tasks, taskID)
		delete(c.completed, taskID)
		delete(c.dependencies, taskID)
		removed++
	}

	if removed > 0 {
		logging.Debug("coordinator cleaned up old tasks", "removed", removed)
	}
}

// generateTaskID creates a unique task ID.
func generateTaskID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "task_" + hex.EncodeToString(b)
}

// AddTask adds a new task to the coordinator.
func (c *Coordinator) AddTask(prompt string, agentType AgentType, priority TaskPriority, deps []string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	taskID := generateTaskID()
	task := &CoordinatedTask{
		ID:           taskID,
		Prompt:       prompt,
		AgentType:    agentType,
		Priority:     priority,
		Dependencies: deps,
		Status:       TaskStatusPending,
	}

	c.tasks[taskID] = task

	// Build reverse dependency map
	for _, depID := range deps {
		c.dependencies[depID] = append(c.dependencies[depID], taskID)
	}

	// Check if task is ready
	if c.areDependenciesMet(task) {
		task.Status = TaskStatusReady
		c.queue.PushTask(task)
		// Signal that a task is ready (non-blocking)
		select {
		case c.taskReadyCh <- struct{}{}:
		default:
		}
	} else {
		task.Status = TaskStatusBlocked
	}

	logging.Debug("coordinator: task added",
		"task_id", taskID,
		"agent_type", agentType,
		"priority", priority,
		"dependencies", deps,
		"status", task.Status)

	return taskID
}

// areDependenciesMet checks if all dependencies are completed.
func (c *Coordinator) areDependenciesMet(task *CoordinatedTask) bool {
	for _, depID := range task.Dependencies {
		if !c.completed[depID] {
			return false
		}
	}
	return true
}

// Start begins processing tasks.
func (c *Coordinator) Start() {
	go c.processLoop()
}

// Stop stops the coordinator.
func (c *Coordinator) Stop() {
	c.cancel()
}

// processLoop is the main coordination loop.
// Uses event-driven approach with fallback ticker to reduce CPU usage.
func (c *Coordinator) processLoop() {
	// Fallback ticker for periodic checks (30s instead of 100ms)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-c.taskReadyCh:
			// A task became ready - process it
			c.processReadyTasks()

			// Check if all done
			if c.isAllComplete() {
				c.notifyAllComplete()
				return
			}

		case agentID := <-c.agentDoneCh:
			// An agent completed - handle it
			c.handleAgentCompletion(agentID)

			// Check if all done
			if c.isAllComplete() {
				c.notifyAllComplete()
				return
			}

		case <-ticker.C:
			// Fallback: periodic check for any missed events
			c.processReadyTasks()
			c.checkCompletedAgents()

			// Check if all done
			if c.isAllComplete() {
				c.notifyAllComplete()
				return
			}
		}
	}
}

// processReadyTasks starts ready tasks up to maxParallel.
func (c *Coordinator) processReadyTasks() {
	c.mu.Lock()
	defer c.mu.Unlock()

	runningCount := len(c.running)
	availableSlots := c.maxParallel - runningCount

	for availableSlots > 0 {
		task := c.queue.PopTask()
		if task == nil {
			break
		}

		if task.Status != TaskStatusReady {
			continue
		}

		// Start the task
		task.Status = TaskStatusRunning
		c.startTask(task)
		availableSlots--
	}
}

// startTask spawns an agent for a task.
func (c *Coordinator) startTask(task *CoordinatedTask) {
	logging.Info("coordinator: starting task",
		"task_id", task.ID,
		"agent_type", task.AgentType,
		"prompt", truncate(task.Prompt, 100))

	if c.onTaskStart != nil {
		c.onTaskStart(task)
	}

	// Spawn async agent
	agentID := c.runner.SpawnAsync(c.ctx, string(task.AgentType), task.Prompt, 30, "")
	c.running[agentID] = task.ID
}

// checkCompletedAgents checks for completed agents and updates tasks.
func (c *Coordinator) checkCompletedAgents() {
	c.mu.Lock()
	// Note: explicit unlock at end of function to allow cleanup outside lock

	for agentID, taskID := range c.running {
		result, ok := c.runner.GetResult(agentID)
		if !ok || !result.Completed {
			continue
		}

		task := c.tasks[taskID]
		if task == nil {
			continue
		}

		// Update task status
		if result.Status == AgentStatusCompleted {
			task.Status = TaskStatusCompleted
			// Record success for learned solutions (feedback loop)
			c.recordReflectionFeedback(result, true)
		} else {
			task.Status = TaskStatusFailed
			// Record failure for learned solutions (feedback loop)
			c.recordReflectionFeedback(result, false)
		}
		task.Result = result

		// Mark completed
		c.completed[taskID] = true
		delete(c.running, agentID)

		logging.Info("coordinator: task completed",
			"task_id", taskID,
			"status", task.Status,
			"duration", result.Duration)

		if c.onTaskComplete != nil {
			c.onTaskComplete(task, result)
		}

		// Unblock dependent tasks
		c.unblockDependents(taskID)
	}

	// Check if cleanup is needed (threshold reached)
	needsCleanup := len(c.completed) > MaxCoordinatorTasks
	c.mu.Unlock()

	// Cleanup old completed tasks if needed (after releasing lock)
	if needsCleanup {
		c.cleanupCompletedTasks()
	}
}

// recordReflectionFeedback records success/failure for learned error solutions.
func (c *Coordinator) recordReflectionFeedback(result *AgentResult, success bool) {
	if c.reflector == nil {
		return
	}

	// Check if this result used a learned solution (has LearnedEntryID in metadata)
	// The agent would have stored this in the result metadata during error recovery
	if result.Metadata != nil {
		if entryID, ok := result.Metadata["learned_entry_id"].(string); ok && entryID != "" {
			var err error
			if success {
				err = c.reflector.RecordSolutionSuccess(entryID)
			} else {
				err = c.reflector.RecordSolutionFailure(entryID)
			}
			if err != nil {
				logging.Warn("coordinator: failed to record reflection feedback",
					"entry_id", entryID,
					"success", success,
					"error", err)
			} else {
				logging.Debug("coordinator: recorded reflection feedback",
					"entry_id", entryID,
					"success", success)
			}
		}
	}
}

// unblockDependents moves blocked tasks to ready if dependencies are met.
func (c *Coordinator) unblockDependents(completedID string) {
	dependents := c.dependencies[completedID]
	for _, depTaskID := range dependents {
		task := c.tasks[depTaskID]
		if task == nil || task.Status != TaskStatusBlocked {
			continue
		}

		if c.areDependenciesMet(task) {
			task.Status = TaskStatusReady
			c.queue.PushTask(task)

			// Signal that a task is ready (non-blocking)
			select {
			case c.taskReadyCh <- struct{}{}:
			default:
			}

			logging.Debug("coordinator: task unblocked",
				"task_id", depTaskID,
				"unblocked_by", completedID)
		}
	}
}

// handleAgentCompletion handles a single agent completion event.
func (c *Coordinator) handleAgentCompletion(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	taskID, ok := c.running[agentID]
	if !ok {
		return
	}

	result, ok := c.runner.GetResult(agentID)
	if !ok || !result.Completed {
		return
	}

	task := c.tasks[taskID]
	if task == nil {
		return
	}

	// Update task status
	if result.Status == AgentStatusCompleted {
		task.Status = TaskStatusCompleted
	} else {
		task.Status = TaskStatusFailed
	}
	task.Result = result

	// Mark completed
	c.completed[taskID] = true
	delete(c.running, agentID)

	logging.Info("coordinator: task completed",
		"task_id", taskID,
		"status", task.Status,
		"duration", result.Duration)

	if c.onTaskComplete != nil {
		c.onTaskComplete(task, result)
	}

	// Unblock dependent tasks
	c.unblockDependents(taskID)
}

// isAllComplete checks if all tasks are done.
func (c *Coordinator) isAllComplete() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.running) > 0 {
		return false
	}

	for _, task := range c.tasks {
		if task.Status != TaskStatusCompleted && task.Status != TaskStatusFailed {
			return false
		}
	}

	return true
}

// notifyAllComplete calls the completion callback.
func (c *Coordinator) notifyAllComplete() {
	if c.onAllComplete == nil {
		return
	}

	results := make(map[string]*AgentResult)
	c.mu.RLock()
	for taskID, task := range c.tasks {
		results[taskID] = task.Result
	}
	c.mu.RUnlock()

	c.onAllComplete(results)
}

// Wait blocks until all tasks are complete.
func (c *Coordinator) Wait() map[string]*AgentResult {
	resultChan := make(chan map[string]*AgentResult, 1)

	c.mu.Lock()
	c.onAllComplete = func(results map[string]*AgentResult) {
		resultChan <- results
	}
	c.mu.Unlock()

	select {
	case results := <-resultChan:
		return results
	case <-c.ctx.Done():
		return nil
	}
}

// WaitWithTimeout waits for completion with a timeout.
func (c *Coordinator) WaitWithTimeout(timeout time.Duration) (map[string]*AgentResult, error) {
	resultChan := make(chan map[string]*AgentResult, 1)

	c.mu.Lock()
	c.onAllComplete = func(results map[string]*AgentResult) {
		resultChan <- results
	}
	c.mu.Unlock()

	select {
	case results := <-resultChan:
		return results, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("coordination timed out after %v", timeout)
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

// GetTask returns a task by ID.
func (c *Coordinator) GetTask(taskID string) *CoordinatedTask {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tasks[taskID]
}

// GetAllTasks returns all tasks.
func (c *Coordinator) GetAllTasks() []*CoordinatedTask {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tasks := make([]*CoordinatedTask, 0, len(c.tasks))
	for _, task := range c.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// GetStatus returns the current status summary.
func (c *Coordinator) GetStatus() *CoordinatorStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := &CoordinatorStatus{
		TotalTasks:     len(c.tasks),
		CompletedTasks: len(c.completed),
		RunningTasks:   len(c.running),
	}

	for _, task := range c.tasks {
		switch task.Status {
		case TaskStatusPending:
			status.PendingTasks++
		case TaskStatusBlocked:
			status.BlockedTasks++
		case TaskStatusReady:
			status.ReadyTasks++
		case TaskStatusFailed:
			status.FailedTasks++
		}
	}

	return status
}

// CoordinatorStatus represents the current state of coordination.
type CoordinatorStatus struct {
	TotalTasks     int
	PendingTasks   int
	BlockedTasks   int
	ReadyTasks     int
	RunningTasks   int
	CompletedTasks int
	FailedTasks    int
}

// SetCallbacks sets callback functions.
func (c *Coordinator) SetCallbacks(
	onStart func(*CoordinatedTask),
	onComplete func(*CoordinatedTask, *AgentResult),
	onAllComplete func(map[string]*AgentResult),
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onTaskStart = onStart
	c.onTaskComplete = onComplete
	c.onAllComplete = onAllComplete
}

// UIBroadcaster interface for sending task events to UI.
type UIBroadcaster interface {
	BroadcastTaskStarted(taskID, message, planType string)
	BroadcastTaskCompleted(taskID string, success bool, duration time.Duration, err error, planType string)
	BroadcastTaskProgress(taskID string, progress float64, message string)
}

// SetUIBroadcaster sets the UI broadcaster for sending task events.
func (c *Coordinator) SetUIBroadcaster(broadcaster UIBroadcaster) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Wire up callbacks to broadcast to UI
	c.onTaskStart = func(task *CoordinatedTask) {
		if broadcaster != nil {
			broadcaster.BroadcastTaskStarted(task.ID, task.Prompt, string(task.AgentType))
		}
	}

	c.onTaskComplete = func(task *CoordinatedTask, result *AgentResult) {
		if broadcaster != nil {
			var err error
			if result != nil && result.Error != "" {
				err = fmt.Errorf("%s", result.Error)
			}
			success := result != nil && result.Status == AgentStatusCompleted
			duration := time.Duration(0)
			if result != nil {
				duration = result.Duration
			}
			broadcaster.BroadcastTaskCompleted(task.ID, success, duration, err, string(task.AgentType))
		}
	}
}

// CancelTask cancels a specific task.
func (c *Coordinator) CancelTask(taskID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	task := c.tasks[taskID]
	if task == nil {
		return fmt.Errorf("task not found: %s", taskID)
	}

	// If running, cancel the agent
	for agentID, tid := range c.running {
		if tid == taskID {
			if err := c.runner.Cancel(agentID); err != nil {
				return err
			}
			delete(c.running, agentID)
		}
	}

	// Remove from queue if pending/ready
	c.queue.RemoveTask(taskID)
	task.Status = TaskStatusFailed
	task.Result = &AgentResult{
		AgentID: "",
		Type:    task.AgentType,
		Status:  AgentStatusCancelled,
		Error:   "cancelled by coordinator",
	}

	return nil
}

// truncate truncates a string to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
