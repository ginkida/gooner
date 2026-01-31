package tasks

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CompletionHandler is called when a task completes.
type CompletionHandler func(task *Task)

// Manager manages background tasks.
type Manager struct {
	tasks   map[string]*Task
	workDir string
	counter int

	onComplete CompletionHandler

	mu sync.RWMutex
}

// NewManager creates a new task manager.
func NewManager(workDir string) *Manager {
	return &Manager{
		tasks:   make(map[string]*Task),
		workDir: workDir,
	}
}

// SetCompletionHandler sets the handler called when tasks complete.
func (m *Manager) SetCompletionHandler(handler CompletionHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onComplete = handler
}

// Start starts a new background task and returns its ID.
func (m *Manager) Start(ctx context.Context, command string) (string, error) {
	m.mu.Lock()
	m.counter++
	id := fmt.Sprintf("task_%d_%d", time.Now().Unix(), m.counter)

	task := NewTask(id, command, m.workDir)
	m.tasks[id] = task
	onComplete := m.onComplete
	m.mu.Unlock()

	// Start the task
	if err := task.Start(ctx); err != nil {
		m.mu.Lock()
		delete(m.tasks, id)
		m.mu.Unlock()
		return "", err
	}

	// Monitor for completion
	go m.monitorTask(task, onComplete)

	return id, nil
}

// monitorTask waits for task completion and calls the handler.
func (m *Manager) monitorTask(task *Task, onComplete CompletionHandler) {
	// Poll for completion
	for !task.IsComplete() {
		time.Sleep(100 * time.Millisecond)
	}

	if onComplete != nil {
		onComplete(task)
	}
}

// Get returns a task by ID.
func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	return task, ok
}

// GetOutput returns the output of a task.
func (m *Manager) GetOutput(id string) (string, bool) {
	m.mu.RLock()
	task, ok := m.tasks[id]
	m.mu.RUnlock()

	if !ok {
		return "", false
	}
	return task.GetOutput(), true
}

// GetInfo returns information about a task.
func (m *Manager) GetInfo(id string) (Info, bool) {
	m.mu.RLock()
	task, ok := m.tasks[id]
	m.mu.RUnlock()

	if !ok {
		return Info{}, false
	}
	return task.GetInfo(), true
}

// Cancel cancels a running task.
func (m *Manager) Cancel(id string) error {
	m.mu.RLock()
	task, ok := m.tasks[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}

	task.Cancel()
	return nil
}

// List returns all tasks.
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]Info, 0, len(m.tasks))
	for _, task := range m.tasks {
		result = append(result, task.GetInfo())
	}
	return result
}

// ListRunning returns all running tasks.
func (m *Manager) ListRunning() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []Info
	for _, task := range m.tasks {
		if task.IsRunning() {
			result = append(result, task.GetInfo())
		}
	}
	return result
}

// ListCompleted returns all completed tasks.
func (m *Manager) ListCompleted() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []Info
	for _, task := range m.tasks {
		if task.IsComplete() {
			result = append(result, task.GetInfo())
		}
	}
	return result
}

// Cleanup removes completed tasks older than the given duration.
func (m *Manager) Cleanup(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	cutoff := time.Now().Add(-maxAge)

	for id, task := range m.tasks {
		if task.IsComplete() && task.EndTime.Before(cutoff) {
			delete(m.tasks, id)
			count++
		}
	}
	return count
}

// CancelAll cancels all running tasks.
func (m *Manager) CancelAll() {
	m.mu.RLock()
	tasks := make([]*Task, 0)
	for _, task := range m.tasks {
		if task.IsRunning() {
			tasks = append(tasks, task)
		}
	}
	m.mu.RUnlock()

	for _, task := range tasks {
		task.Cancel()
	}
}

// Count returns the number of tasks.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tasks)
}

// RunningCount returns the number of running tasks.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, task := range m.tasks {
		if task.IsRunning() {
			count++
		}
	}
	return count
}
