package agent

import (
	"container/heap"
	"sync"
)

// TaskPriority represents the priority level of a task.
type TaskPriority int

const (
	PriorityLow    TaskPriority = 1
	PriorityNormal TaskPriority = 5
	PriorityHigh   TaskPriority = 10
)

// CoordinatedTask represents a task managed by the Coordinator.
type CoordinatedTask struct {
	ID           string
	Prompt       string
	AgentType    AgentType
	Priority     TaskPriority
	Dependencies []string      // IDs of tasks that must complete first
	Status       TaskStatus
	Result       *AgentResult
	index        int           // Index in heap
}

// TaskStatus represents the status of a coordinated task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusBlocked   TaskStatus = "blocked"   // Waiting on dependencies
	TaskStatusReady     TaskStatus = "ready"     // Ready to run (dependencies met)
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
)

// TaskQueue is a priority queue for coordinated tasks.
type TaskQueue struct {
	items []*CoordinatedTask
	mu    sync.RWMutex
}

// NewTaskQueue creates a new priority queue.
func NewTaskQueue() *TaskQueue {
	tq := &TaskQueue{
		items: make([]*CoordinatedTask, 0),
	}
	heap.Init(tq)
	return tq
}

// Len returns the number of items in the queue.
func (tq *TaskQueue) Len() int {
	return len(tq.items)
}

// Less compares two items by priority (higher priority first).
func (tq *TaskQueue) Less(i, j int) bool {
	return tq.items[i].Priority > tq.items[j].Priority
}

// Swap swaps two items.
func (tq *TaskQueue) Swap(i, j int) {
	tq.items[i], tq.items[j] = tq.items[j], tq.items[i]
	tq.items[i].index = i
	tq.items[j].index = j
}

// Push adds an item to the queue.
func (tq *TaskQueue) Push(x any) {
	item := x.(*CoordinatedTask)
	item.index = len(tq.items)
	tq.items = append(tq.items, item)
}

// Pop removes and returns the highest priority item.
func (tq *TaskQueue) Pop() any {
	n := len(tq.items)
	item := tq.items[n-1]
	tq.items[n-1] = nil // Avoid memory leak
	item.index = -1
	tq.items = tq.items[:n-1]
	return item
}

// PushTask adds a task to the queue (thread-safe).
func (tq *TaskQueue) PushTask(task *CoordinatedTask) {
	tq.mu.Lock()
	defer tq.mu.Unlock()
	heap.Push(tq, task)
}

// PopTask removes and returns the highest priority task (thread-safe).
func (tq *TaskQueue) PopTask() *CoordinatedTask {
	tq.mu.Lock()
	defer tq.mu.Unlock()

	if tq.Len() == 0 {
		return nil
	}
	return heap.Pop(tq).(*CoordinatedTask)
}

// PeekTask returns the highest priority task without removing it.
func (tq *TaskQueue) PeekTask() *CoordinatedTask {
	tq.mu.RLock()
	defer tq.mu.RUnlock()

	if tq.Len() == 0 {
		return nil
	}
	return tq.items[0]
}

// UpdatePriority updates the priority of a task.
func (tq *TaskQueue) UpdatePriority(task *CoordinatedTask, priority TaskPriority) {
	tq.mu.Lock()
	defer tq.mu.Unlock()

	task.Priority = priority
	heap.Fix(tq, task.index)
}

// RemoveTask removes a specific task from the queue.
func (tq *TaskQueue) RemoveTask(taskID string) *CoordinatedTask {
	tq.mu.Lock()
	defer tq.mu.Unlock()

	for i, task := range tq.items {
		if task.ID == taskID {
			return heap.Remove(tq, i).(*CoordinatedTask)
		}
	}
	return nil
}

// GetReadyTasks returns all tasks that are ready to run.
func (tq *TaskQueue) GetReadyTasks() []*CoordinatedTask {
	tq.mu.RLock()
	defer tq.mu.RUnlock()

	ready := make([]*CoordinatedTask, 0)
	for _, task := range tq.items {
		if task.Status == TaskStatusReady {
			ready = append(ready, task)
		}
	}
	return ready
}

// Size returns the current queue size (thread-safe).
func (tq *TaskQueue) Size() int {
	tq.mu.RLock()
	defer tq.mu.RUnlock()
	return tq.Len()
}
