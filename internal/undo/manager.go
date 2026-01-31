package undo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gooner/internal/fileutil"
)

// Manager provides undo and redo functionality.
type Manager struct {
	tracker *Tracker
	undone  []FileChange // stack of undone changes for redo
	maxRedo int
	mu      sync.Mutex
}

// NewManager creates a new undo/redo Manager.
func NewManager() *Manager {
	return &Manager{
		tracker: NewTracker(),
		undone:  make([]FileChange, 0),
		maxRedo: 50,
	}
}

// NewManagerWithTracker creates a Manager with a custom tracker.
func NewManagerWithTracker(tracker *Tracker) *Manager {
	return &Manager{
		tracker: tracker,
		undone:  make([]FileChange, 0),
		maxRedo: 50,
	}
}

// Record records a new file change.
func (m *Manager) Record(change FileChange) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tracker.Record(change)
	// Clear redo stack when new changes are made
	m.undone = make([]FileChange, 0)
}

// Undo reverts the last change and returns information about it.
func (m *Manager) Undo() (*FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	change := m.tracker.PopLast()
	if change == nil {
		return nil, fmt.Errorf("nothing to undo")
	}

	// Perform the undo operation
	if err := m.revertChange(change); err != nil {
		// Put change back if undo failed
		m.tracker.Record(*change)
		return nil, fmt.Errorf("failed to undo: %w", err)
	}

	// Add to redo stack
	if len(m.undone) >= m.maxRedo {
		m.undone = m.undone[1:]
	}
	m.undone = append(m.undone, *change)

	return change, nil
}

// Redo re-applies the last undone change.
func (m *Manager) Redo() (*FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.undone) == 0 {
		return nil, fmt.Errorf("nothing to redo")
	}

	// Pop from redo stack
	change := m.undone[len(m.undone)-1]
	m.undone = m.undone[:len(m.undone)-1]

	// Perform the redo operation (apply the change again)
	if err := m.applyChange(&change); err != nil {
		// Put change back in redo stack if redo failed
		m.undone = append(m.undone, change)
		return nil, fmt.Errorf("failed to redo: %w", err)
	}

	// Add back to tracker
	m.tracker.Record(change)

	return &change, nil
}

// CanUndo returns whether there are changes to undo.
func (m *Manager) CanUndo() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tracker.Count() > 0
}

// CanRedo returns whether there are changes to redo.
func (m *Manager) CanRedo() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.undone) > 0
}

// List returns the list of undoable changes.
func (m *Manager) List() []FileChange {
	return m.tracker.List()
}

// ListRecent returns the N most recent changes.
func (m *Manager) ListRecent(n int) []FileChange {
	return m.tracker.ListRecent(n)
}

// Count returns the number of undoable changes.
func (m *Manager) Count() int {
	return m.tracker.Count()
}

// Clear clears all tracked changes and redo history.
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracker.Clear()
	m.undone = make([]FileChange, 0)
}

// GetTracker returns the underlying tracker.
func (m *Manager) GetTracker() *Tracker {
	return m.tracker
}

// RestoreChanges restores the undo stack from a list of changes.
func (m *Manager) RestoreChanges(changes []FileChange) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tracker.Clear()
	for _, change := range changes {
		m.tracker.Record(change)
	}
	m.undone = make([]FileChange, 0)
}

// GetUndone returns the redo stack (undone changes).
func (m *Manager) GetUndone() []FileChange {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]FileChange, len(m.undone))
	copy(result, m.undone)
	return result
}

// SetRedoStack restores the redo stack from checkpoint.
func (m *Manager) SetRedoStack(stack []FileChange) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.undone = append([]FileChange{}, stack...)
}

// revertChange reverts a file change to its previous state.
func (m *Manager) revertChange(change *FileChange) error {
	if change.WasNew {
		// File was created - delete it
		if err := os.Remove(change.FilePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	// File was modified - restore old content atomically
	dir := filepath.Dir(change.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return fileutil.AtomicWrite(change.FilePath, change.OldContent, 0644)
}

// applyChange applies a file change (for redo).
func (m *Manager) applyChange(change *FileChange) error {
	dir := filepath.Dir(change.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return fileutil.AtomicWrite(change.FilePath, change.NewContent, 0644)
}
