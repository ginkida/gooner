package app

import (
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gokin/internal/logging"
)

// UIUpdateManager coordinates periodic UI updates
type UIUpdateManager struct {
	program          *tea.Program
	eventBroadcaster *UIEventBroadcaster

	// Control
	stopChan chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	mu       sync.RWMutex
	running  bool
}

// NewUIUpdateManager creates a new UI update manager
func NewUIUpdateManager(program *tea.Program, app *App) *UIUpdateManager {
	return &UIUpdateManager{
		program:          program,
		eventBroadcaster: NewUIEventBroadcaster(program),
		stopChan:         make(chan struct{}),
		running:          false,
	}
}

// Start begins periodic UI updates
func (m *UIUpdateManager) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// Wait for any previous goroutines to finish without holding the lock
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check running state after waiting (another goroutine may have started)
	if m.running {
		return
	}

	m.running = true
	m.stopChan = make(chan struct{})
	// Note: sync.Once is intentionally left as-is. We create a fresh one
	// via pointer indirection to allow multiple Start/Stop cycles.
	m.stopOnce = sync.Once{}
}

// Stop stops periodic UI updates
func (m *UIUpdateManager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.mu.Unlock()

	m.stopOnce.Do(func() {
		close(m.stopChan)
	})

	// Stop event broadcaster first to unblock any waiting sends
	if m.eventBroadcaster != nil {
		m.eventBroadcaster.Stop()
	}

	// Wait for goroutines with timeout
	// Use a channel to avoid leaking the wait goroutine
	done := make(chan struct{})
	go func() {
		defer func() {
			// Recover from panic if channel is already closed
			recover()
		}()
		m.wg.Wait()
		select {
		case done <- struct{}{}:
		default:
		}
	}()

	select {
	case <-done:
		logging.Debug("UI update manager stopped gracefully")
	case <-time.After(5 * time.Second):
		logging.Warn("UI update manager stop timed out, continuing anyway")
	}
	close(done)
}

// IsRunning returns whether the update manager is running
func (m *UIUpdateManager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// BroadcastTaskStart broadcasts a task start event
func (m *UIUpdateManager) BroadcastTaskStart(taskID, message, planType string) {
	m.eventBroadcaster.BroadcastTaskStart(taskID, message, planType)
}

// BroadcastTaskComplete broadcasts a task completion event
func (m *UIUpdateManager) BroadcastTaskComplete(taskID string, success bool, duration time.Duration, err error, planType string) {
	m.eventBroadcaster.BroadcastTaskComplete(taskID, success, duration, err, planType)
}

// BroadcastTaskProgress broadcasts a task progress event
func (m *UIUpdateManager) BroadcastTaskProgress(taskID string, progress float64, message string) {
	m.eventBroadcaster.BroadcastTaskProgress(taskID, progress, message)
}

// Enable enables event broadcasting
func (m *UIUpdateManager) Enable() {
	m.eventBroadcaster.Enable()
}

// Disable disables event broadcasting
func (m *UIUpdateManager) Disable() {
	m.eventBroadcaster.Disable()
}
