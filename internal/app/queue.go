package app

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"time"
)

// Priority represents the priority level of a task.
type Priority int

const (
	PriorityLow Priority = iota
	PriorityNormal
	PriorityHigh
)

// String returns the string representation of the priority.
func (p Priority) String() string {
	switch p {
	case PriorityHigh:
		return "HIGH"
	case PriorityNormal:
		return "NORMAL"
	case PriorityLow:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

// QueueTask represents a queued task with priority.
type QueueTask struct {
	ID        string
	Message   string
	Priority  Priority
	CreatedAt time.Time
	Context   context.Context
	// Callback to execute the task
	Execute func(ctx context.Context) error
	// Callback for completion
	OnComplete func(error)
	// Index for heap (needed by container/heap)
	index int
}

// priorityQueue implements heap.Interface and holds Tasks.
type priorityQueue []*QueueTask

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	// Higher priority comes first
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority > pq[j].Priority
	}
	// If same priority, older tasks come first (FIFO)
	return pq[i].CreatedAt.Before(pq[j].CreatedAt)
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*QueueTask)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*pq = old[0 : n-1]
	return item
}

// QueueManager manages prioritized task queue.
type QueueManager struct {
	queue priorityQueue

	// Task tracking
	tasks       map[string]*QueueTask
	taskCounter int
	mu          sync.RWMutex

	// Processing
	processing      bool
	currentTask     *QueueTask
	processingQueue chan *QueueTask

	// Configuration
	maxQueueSize int
	enabled      bool

	// Metrics
	totalQueued       int
	totalProcessed    int
	totalDropped      int
	highPriorityCount int

	// Callbacks
	onTaskStart    func(task *QueueTask)
	onTaskComplete func(task *QueueTask, err error)
}

// NewQueueManager creates a new queue manager.
func NewQueueManager(maxQueueSize int) *QueueManager {
	qm := &QueueManager{
		queue:           make(priorityQueue, 0),
		tasks:           make(map[string]*QueueTask),
		processingQueue: make(chan *QueueTask, 1),
		maxQueueSize:    maxQueueSize,
		enabled:         true,
	}
	heap.Init(&qm.queue)
	return qm
}

// Enabled returns whether the queue is enabled.
func (qm *QueueManager) Enabled() bool {
	qm.mu.RLock()
	defer qm.mu.RUnlock()
	return qm.enabled
}

// SetEnabled enables or disables the queue.
func (qm *QueueManager) SetEnabled(enabled bool) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	qm.enabled = enabled
}

// Enqueue adds a task to the queue with the given priority.
// Returns the task ID and an error if the queue is full.
func (qm *QueueManager) Enqueue(ctx context.Context, message string, priority Priority, execute func(context.Context) error, onComplete func(error)) (string, error) {
	if !qm.Enabled() {
		// Queue disabled - execute immediately using provided context
		go func(ctx context.Context) {
			err := execute(ctx)
			if onComplete != nil {
				onComplete(err)
			}
		}(ctx)
		return "direct", nil
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	// Check queue size
	if qm.queue.Len() >= qm.maxQueueSize {
		// Queue is full - need to make room
		// Don't drop high priority tasks
		if priority != PriorityHigh {
			qm.totalDropped++
			return "", fmt.Errorf("queue full (max %d), task dropped", qm.maxQueueSize)
		}

		// Remove lowest priority task to make room for high priority
		if qm.queue.Len() > 0 {
			oldest := qm.queue.Pop().(*QueueTask)
			delete(qm.tasks, oldest.ID)
			qm.totalDropped++
		}
	}

	// Create task
	qm.taskCounter++
	taskID := fmt.Sprintf("queue_%d_%d", time.Now().Unix(), qm.taskCounter)

	task := &QueueTask{
		ID:         taskID,
		Priority:   priority,
		Execute:    execute,
		OnComplete: onComplete,
	}

	qm.tasks[taskID] = task
	heap.Push(&qm.queue, task)
	qm.totalQueued++

	if priority == PriorityHigh {
		qm.highPriorityCount++
	}

	return taskID, nil
}

// Dequeue removes and returns the highest priority task.
// Returns nil if the queue is empty.
func (qm *QueueManager) Dequeue() *QueueTask {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.queue.Len() == 0 {
		return nil
	}

	task := heap.Pop(&qm.queue).(*QueueTask)
	delete(qm.tasks, task.ID)
	return task
}

// Peek returns the highest priority task without removing it.
// Returns nil if the queue is empty.
func (qm *QueueManager) Peek() *QueueTask {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	if qm.queue.Len() == 0 {
		return nil
	}

	return qm.queue[0]
}

// GetTask returns a task by ID.
func (qm *QueueManager) GetTask(id string) (*QueueTask, bool) {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	task, ok := qm.tasks[id]
	return task, ok
}

// Remove removes a task from the queue by ID.
// Returns true if the task was found and removed.
func (qm *QueueManager) Remove(id string) bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	task, ok := qm.tasks[id]
	if !ok {
		return false
	}

	// Remove from heap
	heap.Remove(&qm.queue, task.index)
	delete(qm.tasks, id)

	return true
}

// Len returns the number of tasks in the queue.
func (qm *QueueManager) Len() int {
	qm.mu.RLock()
	defer qm.mu.RUnlock()
	return qm.queue.Len()
}

// Clear removes all tasks from the queue.
func (qm *QueueManager) Clear() {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	qm.queue = make(priorityQueue, 0)
	qm.tasks = make(map[string]*QueueTask)
	heap.Init(&qm.queue)
}

// GetStats returns queue statistics.
type QueueStats struct {
	Length            int
	TotalQueued       int
	TotalProcessed    int
	TotalDropped      int
	HighPriorityCount int
	AverageWaitTime   time.Duration
}

func (qm *QueueManager) GetStats() QueueStats {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	return QueueStats{
		Length:            qm.queue.Len(),
		TotalQueued:       qm.totalQueued,
		TotalProcessed:    qm.totalProcessed,
		TotalDropped:      qm.totalDropped,
		HighPriorityCount: qm.highPriorityCount,
	}
}

// SetCallbacks sets the callbacks for task lifecycle events.
func (qm *QueueManager) SetCallbacks(onStart func(*QueueTask), onComplete func(*QueueTask, error)) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	qm.onTaskStart = onStart
	qm.onTaskComplete = onComplete
}

// ProcessQueue processes tasks from the queue in priority order.
// This should be run as a goroutine.
func (qm *QueueManager) ProcessQueue(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		task := qm.Dequeue()
		if task == nil {
			// No tasks - wait a bit, respecting context cancellation
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		// Process the task
		qm.processTask(ctx, task)
	}
}

// processTask executes a single task.
func (qm *QueueManager) processTask(ctx context.Context, task *QueueTask) {
	qm.mu.Lock()
	qm.processing = true
	qm.currentTask = task
	onStart := qm.onTaskStart
	onComplete := qm.onTaskComplete
	qm.mu.Unlock()

	// Notify start
	if onStart != nil {
		onStart(task)
	}

	// Execute the task
	err := task.Execute(ctx)

	// Notify completion
	if onComplete != nil {
		onComplete(task, err)
	}

	// Call task-specific completion callback
	if task.OnComplete != nil {
		task.OnComplete(err)
	}

	qm.mu.Lock()
	qm.processing = false
	qm.currentTask = nil
	qm.totalProcessed++
	qm.mu.Unlock()
}

// GetCurrentTask returns the currently processing task.
func (qm *QueueManager) GetCurrentTask() *QueueTask {
	qm.mu.RLock()
	defer qm.mu.RUnlock()
	return qm.currentTask
}

// IsProcessing returns whether a task is currently being processed.
func (qm *QueueManager) IsProcessing() bool {
	qm.mu.RLock()
	defer qm.mu.RUnlock()
	return qm.processing
}

// ListTasks returns all tasks in priority order.
func (qm *QueueManager) ListTasks() []*QueueTask {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	tasks := make([]*QueueTask, 0, qm.queue.Len())
	for _, task := range qm.queue {
		tasks = append(tasks, task)
	}

	return tasks
}
