package undo

import (
	"sync"
)

// DefaultMaxChanges is the default maximum number of changes to track.
const DefaultMaxChanges = 100

// Tracker records file changes for undo functionality.
type Tracker struct {
	changes []FileChange
	maxSize int
	mu      sync.RWMutex
}

// NewTracker creates a new Tracker with the default max size.
func NewTracker() *Tracker {
	return &Tracker{
		changes: make([]FileChange, 0),
		maxSize: DefaultMaxChanges,
	}
}

// NewTrackerWithSize creates a new Tracker with a custom max size.
func NewTrackerWithSize(maxSize int) *Tracker {
	if maxSize <= 0 {
		maxSize = DefaultMaxChanges
	}
	return &Tracker{
		changes: make([]FileChange, 0),
		maxSize: maxSize,
	}
}

// Record adds a new change to the tracker.
func (t *Tracker) Record(change FileChange) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If at capacity, remove oldest change
	if len(t.changes) >= t.maxSize {
		t.changes = t.changes[1:]
	}

	t.changes = append(t.changes, change)
}

// GetLast returns the most recent change, or nil if none.
func (t *Tracker) GetLast() *FileChange {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(t.changes) == 0 {
		return nil
	}

	change := t.changes[len(t.changes)-1]
	return &change
}

// PopLast removes and returns the most recent change.
func (t *Tracker) PopLast() *FileChange {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.changes) == 0 {
		return nil
	}

	change := t.changes[len(t.changes)-1]
	t.changes = t.changes[:len(t.changes)-1]
	return &change
}

// List returns all tracked changes (oldest first).
func (t *Tracker) List() []FileChange {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]FileChange, len(t.changes))
	copy(result, t.changes)
	return result
}

// ListRecent returns the N most recent changes (newest first).
func (t *Tracker) ListRecent(n int) []FileChange {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if n <= 0 || len(t.changes) == 0 {
		return nil
	}

	if n > len(t.changes) {
		n = len(t.changes)
	}

	result := make([]FileChange, n)
	for i := 0; i < n; i++ {
		result[i] = t.changes[len(t.changes)-1-i]
	}
	return result
}

// Count returns the number of tracked changes.
func (t *Tracker) Count() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.changes)
}

// Clear removes all tracked changes.
func (t *Tracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changes = make([]FileChange, 0)
}

// GetByID returns a change by its ID, or nil if not found.
func (t *Tracker) GetByID(id string) *FileChange {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for i := len(t.changes) - 1; i >= 0; i-- {
		if t.changes[i].ID == id {
			change := t.changes[i]
			return &change
		}
	}
	return nil
}

// RemoveByID removes a change by its ID and returns it.
func (t *Tracker) RemoveByID(id string) *FileChange {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := len(t.changes) - 1; i >= 0; i-- {
		if t.changes[i].ID == id {
			change := t.changes[i]
			t.changes = append(t.changes[:i], t.changes[i+1:]...)
			return &change
		}
	}
	return nil
}
