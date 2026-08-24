package app

import "gokin/internal/config"

// snapshotConfig returns the live config pointer, read under a.mu.
//
// ApplyConfig SWAPS a.config under that lock, so any reader on a goroutine
// other than the app's own must synchronize the read of the FIELD — the
// pointed-to config is immutable once published, so holding the lock only
// long enough to copy the pointer is sufficient and cannot deadlock against
// callbacks that re-enter arbitrary code.
//
// Readers on the request/command goroutine are already serialized with
// ApplyConfig and do not need this; the ones that do are callbacks invoked by
// other subsystems — the Bubble Tea status goroutine, a /loop iteration, a
// sub-agent's tool execution.
func (a *App) snapshotConfig() *config.Config {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config
}
