package app

import (
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gooner/internal/logging"
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
	defer m.mu.Unlock()

	if m.running {
		return
	}

	m.wg.Wait()
	m.running = true
	m.stopChan = make(chan struct{})
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

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logging.Debug("UI update manager stopped gracefully")
	case <-time.After(10 * time.Second):
		logging.Warn("UI update manager stop timed out")
		if m.eventBroadcaster != nil {
			m.eventBroadcaster.Stop()
		}
	}
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
