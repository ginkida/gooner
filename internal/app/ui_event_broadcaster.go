package app

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gokin/internal/ui"
)

// sendTimeout is the maximum time to wait for a UI send operation
const sendTimeout = 100 * time.Millisecond

// UIEventBroadcaster broadcasts task execution events to UI
type UIEventBroadcaster struct {
	program *tea.Program
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.RWMutex
	enabled bool

	// Rate limiting to prevent excessive broadcasts
	lastBroadcast time.Time
	minInterval   time.Duration // Minimum interval between broadcasts (default 50ms)
}

// NewUIEventBroadcaster creates a new UI event broadcaster
func NewUIEventBroadcaster(program *tea.Program) *UIEventBroadcaster {
	ctx, cancel := context.WithCancel(context.Background())
	return &UIEventBroadcaster{
		program:     program,
		ctx:         ctx,
		cancel:      cancel,
		enabled:     true,
		minInterval: 50 * time.Millisecond, // Rate limit to prevent excessive broadcasts
	}
}

// NewUIEventBroadcasterWithContext creates a new UI event broadcaster with a parent context
func NewUIEventBroadcasterWithContext(ctx context.Context, program *tea.Program) *UIEventBroadcaster {
	childCtx, cancel := context.WithCancel(ctx)
	return &UIEventBroadcaster{
		program:     program,
		ctx:         childCtx,
		cancel:      cancel,
		enabled:     true,
		minInterval: 50 * time.Millisecond, // Rate limit to prevent excessive broadcasts
	}
}

// Stop stops the broadcaster and cancels any pending sends
func (b *UIEventBroadcaster) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
}

// sendAsync sends a message to the UI program with context awareness and timeout protection.
// This prevents goroutine leaks by ensuring sends don't block indefinitely.
// Rate limiting prevents excessive broadcasts that could overwhelm the UI.
func (b *UIEventBroadcaster) sendAsync(msg tea.Msg) {
	b.mu.Lock()
	if !b.enabled || b.program == nil {
		b.mu.Unlock()
		return
	}

	// Rate limiting: skip if too frequent
	if time.Since(b.lastBroadcast) < b.minInterval {
		b.mu.Unlock()
		return // Skip too frequent broadcasts
	}
	b.lastBroadcast = time.Now()

	program := b.program
	ctx := b.ctx
	b.mu.Unlock()

	// Check if already cancelled
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Send with timeout protection to prevent goroutine leaks
	go func() {
		// Create timeout context for this send
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer func() {
				// Recover from panic if channel is already closed
				recover()
			}()
			program.Send(msg)
			// Use select to avoid blocking on close if context already cancelled
			select {
			case <-sendCtx.Done():
				// Context cancelled, don't try to close
			default:
				close(done)
			}
		}()

		select {
		case <-sendCtx.Done():
			// Context cancelled or timeout - don't block
			// Inner goroutine will exit when program.Send returns
			return
		case <-done:
			// Send completed successfully
			return
		}
	}()
}

// Enable enables event broadcasting
func (b *UIEventBroadcaster) Enable() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enabled = true
}

// Disable disables event broadcasting
func (b *UIEventBroadcaster) Disable() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enabled = false
}

// IsEnabled returns whether broadcasting is enabled
func (b *UIEventBroadcaster) IsEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.enabled
}

// BroadcastTaskStart broadcasts a task start event
func (b *UIEventBroadcaster) BroadcastTaskStart(taskID, message, planType string) {
	if !b.IsEnabled() {
		return
	}

	b.sendAsync(ui.TaskStartedEvent{
		TaskID:   taskID,
		Message:  message,
		PlanType: planType,
	})
}

// BroadcastTaskComplete broadcasts a task completion event
func (b *UIEventBroadcaster) BroadcastTaskComplete(taskID string, success bool, duration time.Duration, err error, planType string) {
	if !b.IsEnabled() {
		return
	}

	b.sendAsync(ui.TaskCompletedEvent{
		TaskID:   taskID,
		Success:  success,
		Duration: duration,
		Error:    err,
		PlanType: planType,
	})
}

// BroadcastTaskProgress broadcasts a task progress event
func (b *UIEventBroadcaster) BroadcastTaskProgress(taskID string, progress float64, message string) {
	if !b.IsEnabled() {
		return
	}

	b.sendAsync(ui.TaskProgressEvent{
		TaskID:   taskID,
		Progress: progress,
		Message:  message,
	})
}

// SetProgram sets the tea.Program for broadcasting
func (b *UIEventBroadcaster) SetProgram(program *tea.Program) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.program = program
}
