package app

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gokin/internal/commands"
	"gokin/internal/logging"
)

const (
	// GracefulShutdownTimeout is the maximum time to wait for graceful shutdown.
	GracefulShutdownTimeout = 10 * time.Second
	// ForcedShutdownTimeout is the time after which we force exit.
	ForcedShutdownTimeout = 15 * time.Second
)

// GoroutineTracker tracks running goroutines for graceful shutdown.
type GoroutineTracker struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// NewGoroutineTracker creates a new goroutine tracker.
func NewGoroutineTracker() *GoroutineTracker {
	return &GoroutineTracker{}
}

// Add registers a new goroutine to track.
func (t *GoroutineTracker) Add() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.wg.Add(1)
	return true
}

// Done marks a goroutine as completed.
func (t *GoroutineTracker) Done() {
	t.wg.Done()
}

// Wait waits for all tracked goroutines to complete.
func (t *GoroutineTracker) Wait() {
	t.wg.Wait()
}

// WaitWithTimeout waits for all goroutines with a timeout.
// Returns true if all goroutines completed, false if timed out.
func (t *GoroutineTracker) WaitWithTimeout(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Close prevents new goroutines from being added.
func (t *GoroutineTracker) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
}

// setupSignalHandler sets up signal handling for graceful shutdown.
// Returns a cleanup function that should be called when the app exits.
func (a *App) setupSignalHandler() func() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)

	// Done channel to signal goroutine termination
	done := make(chan struct{})

	go func() {
		select {
		case sig := <-sigChan:
			logging.Debug("received signal", "signal", sig)

			// Start shutdown timer for forced exit
			forceExitTimer := time.AfterFunc(ForcedShutdownTimeout, func() {
				logging.Warn("forced shutdown due to timeout")
				os.Exit(1)
			})
			defer forceExitTimer.Stop()

			// Create shutdown context with timeout
			shutdownCtx, cancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
			defer cancel()

			// Perform graceful shutdown
			a.gracefulShutdown(shutdownCtx)

			// If the signal is SIGQUIT, exit with core dump
			if sig == syscall.SIGQUIT {
				logging.Info("exiting with core dump")
				os.Exit(128 + int(syscall.SIGQUIT))
			}

			// Normal exit for other signals
			os.Exit(0)

		case <-done:
			// Cleanup requested, exit goroutine
			return

		case <-a.ctx.Done():
			// App context cancelled, exit goroutine
			return
		}
	}()

	// Return cleanup function
	return func() {
		signal.Stop(sigChan)
		close(done)
	}
}

// gracefulShutdown performs a graceful shutdown with timeout.
func (a *App) gracefulShutdown(ctx context.Context) {
	logging.Debug("starting graceful shutdown")

	// 1. Cancel all ongoing operations (this signals goroutines to stop)
	if a.cancel != nil {
		a.cancel()
	}

	// 2. Cleanup signal handler
	if a.signalCleanup != nil {
		a.signalCleanup()
		a.signalCleanup = nil
	}

	// 3. Stop UI update manager
	if a.uiUpdateManager != nil {
		logging.Debug("stopping UI update manager")
		a.uiUpdateManager.Stop()
	}

	// 4. Cancel all running background tasks
	if a.taskManager != nil {
		logging.Debug("cancelling background tasks")
		a.taskManager.CancelAll()
	}

	// 5. Shutdown MCP servers
	if a.mcpManager != nil {
		logging.Debug("shutting down MCP servers")
		if err := a.mcpManager.Shutdown(ctx); err != nil {
			logging.Debug("error shutting down MCP", "error", err)
		}
	}

	// 6. Stop file watcher
	if a.fileWatcher != nil {
		logging.Debug("stopping file watcher")
		if err := a.fileWatcher.Stop(); err != nil {
			logging.Debug("error stopping file watcher", "error", err)
		}
	}

	// 6b. Stop background semantic indexer
	if a.backgroundIndexer != nil {
		logging.Debug("stopping background semantic indexer")
		a.backgroundIndexer.Stop()
	}

	// 7. Save semantic search cache
	if a.semanticIndexer != nil {
		logging.Debug("saving semantic cache")
		if err := a.semanticIndexer.SaveCache(); err != nil {
			logging.Debug("error saving semantic cache", "error", err)
		}
	}

	// 7b. Save active plan for later resume
	if a.planManager != nil {
		plan := a.planManager.GetCurrentPlan()
		if plan != nil && !plan.IsComplete() {
			logging.Debug("saving active plan for resume", "plan_id", plan.ID, "status", plan.Status)
			if err := a.planManager.SaveCurrentPlan(); err != nil {
				logging.Debug("failed to save active plan", "error", err)
			}
		}
	}

	// 8. Cleanup spawned editor processes
	logging.Debug("cleaning up spawned processes")
	commands.CleanupSpawnedProcesses()

	// 10. Run on_exit hooks with timeout
	if a.hooksManager != nil {
		logging.Debug("running on_exit hooks")
		a.hooksManager.RunOnExit(ctx)
	}

	// 11. Save input history
	if a.tui != nil {
		if err := a.tui.SaveInputHistory(); err != nil {
			logging.Debug("failed to save input history", "error", err)
		}
	}

	// 12. Flush agent data (project learning) to prevent data loss
	if a.agentRunner != nil {
		logging.Debug("flushing agent data")
		a.agentRunner.Close()
	}

	// 13. Flush audit logger to ensure all entries are persisted
	if a.auditLogger != nil {
		logging.Debug("flushing audit logger")
		a.auditLogger.Flush()
	}

	// 14. Save session history
	a.saveSessionHistory()

	// 13. Close client
	if a.client != nil {
		a.client.Close()
	}

	// 14. Close logging last
	logging.Debug("shutdown complete")
	logging.Close()
}

// saveSessionHistory saves the current session to disk.
func (a *App) saveSessionHistory() {
	if a.session == nil {
		return
	}

	historyMgr, err := a.GetHistoryManager()
	if err != nil {
		logging.Debug("failed to create history manager", "error", err)
		return
	}

	if err := historyMgr.Save(a.session); err != nil {
		logging.Debug("failed to save session history", "error", err)
	}
}
