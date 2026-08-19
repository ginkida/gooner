package undo

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gokin/internal/logging"

	"gokin/internal/fileutil"
)

// Manager provides undo and redo functionality.
type Manager struct {
	tracker     *Tracker
	undone      []FileChange // stack of undone changes for redo
	maxRedo     int
	activeGroup string // Current group ID stamped on all recorded changes
	recordCount uint64 // successful external Record calls
	mutations   uint64 // all successful history mutations
	mu          sync.Mutex
}

// HistorySnapshot is an atomic, content-free view of undo history used to
// delimit higher-level transactions such as a plan execution.
type HistorySnapshot struct {
	ChangeIDs   []string
	RecordCount uint64
	Mutations   uint64
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

// SetActiveGroup sets the group ID that will be stamped on all subsequent recorded changes.
// Use this before a multi-file operation to group related changes for atomic undo.
func (m *Manager) SetActiveGroup(groupID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeGroup = groupID
}

// ClearActiveGroup removes the active group, returning to ungrouped recording.
func (m *Manager) ClearActiveGroup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeGroup = ""
}

// Record records a new file change. If an active group is set, the change
// is stamped with the group ID for later atomic undo via UndoGroup.
func (m *Manager) Record(change FileChange) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activeGroup != "" && change.GroupID == "" {
		change.GroupID = m.activeGroup
	}
	m.tracker.Record(change)
	// Clear redo stack when new changes are made
	m.undone = make([]FileChange, 0)
	m.recordCount++
	m.mutations++
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
	m.mutations++

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
	m.mutations++

	return &change, nil
}

// UndoGroup reverts all changes with the given group ID atomically.
// Changes are undone in reverse order. If any revert fails, already-reverted
// changes are re-applied to maintain consistency.
func (m *Manager) UndoGroup(groupID string) ([]*FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.undoGroupLocked(groupID)
}

// UndoChanges reverts exactly the requested recorded changes as one
// transaction. IDs may refer to changes that are not at the top of the global
// history; unrelated later changes are preserved. A later change that touches
// one of the same paths makes the operation fail closed instead of overwriting
// work performed after the requested changes.
func (m *Manager) UndoChanges(ids []string) ([]*FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	requested, err := uniqueChangeIDs(ids)
	if err != nil {
		return nil, err
	}

	selected := make([]*FileChange, 0, len(requested))
	found := make(map[string]struct{}, len(requested))
	firstIndex := len(m.tracker.changes)
	targetPaths := make(map[string]struct{})
	for i := len(m.tracker.changes) - 1; i >= 0; i-- {
		change := m.tracker.changes[i]
		if _, ok := requested[change.ID]; !ok {
			continue
		}
		changeCopy := change
		selected = append(selected, &changeCopy) // newest first
		found[change.ID] = struct{}{}
		firstIndex = i
		for _, path := range changePaths(&change) {
			targetPaths[path] = struct{}{}
		}
	}
	if len(found) != len(requested) {
		return nil, fmt.Errorf("cannot undo requested changes: %s", missingChangeIDs(requested, found))
	}

	// Content preflight catches ordinary overlapping edits. This identity check
	// also catches the rarer case where a later edit happened to produce exactly
	// the same bytes and would otherwise be silently discarded.
	for i := firstIndex; i < len(m.tracker.changes); i++ {
		change := &m.tracker.changes[i]
		if _, ok := requested[change.ID]; ok {
			continue
		}
		for _, path := range changePaths(change) {
			if _, overlaps := targetPaths[path]; overlaps {
				return nil, fmt.Errorf(
					"cannot undo requested changes: later change %s also touches %s",
					change.ID, path,
				)
			}
		}
	}

	if err := preflightUndoChanges(selected); err != nil {
		return nil, fmt.Errorf("failed to undo requested changes: %w", err)
	}

	reverted := make([]*FileChange, 0, len(selected))
	for _, change := range selected {
		if err := m.revertChangeUnchecked(change); err != nil {
			if rbErr := rollbackUndoChanges(reverted); rbErr != nil {
				return nil, fmt.Errorf(
					"failed to undo requested changes AND rollback incomplete (check logs): %w",
					err,
				)
			}
			return nil, fmt.Errorf("failed to undo requested changes (rolled back): %w", err)
		}
		reverted = append(reverted, change)
	}

	remaining := make([]FileChange, 0, len(m.tracker.changes)-len(selected))
	for _, change := range m.tracker.changes {
		if _, ok := requested[change.ID]; !ok {
			remaining = append(remaining, change)
		}
	}
	m.tracker.changes = remaining
	// Exact transactions must remain exactly redoable even when they contain
	// more entries than the ordinary rolling redo limit. Discard older,
	// unrelated redo entries first; allow this transaction itself to exceed the
	// soft limit rather than silently retaining only its tail.
	redoRoom := m.maxRedo - len(reverted)
	if redoRoom < 0 {
		redoRoom = 0
	}
	if len(m.undone) > redoRoom {
		m.undone = m.undone[len(m.undone)-redoRoom:]
	}
	for _, change := range reverted {
		m.undone = append(m.undone, *change)
	}
	m.mutations++

	return reverted, nil
}

// RedoChanges re-applies exactly the requested changes as one transaction.
// ids must be in original execution order (oldest first).
func (m *Manager) RedoChanges(ids []string) ([]*FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	requested, err := uniqueChangeIDs(ids)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]FileChange, len(requested))
	for _, change := range m.undone {
		if _, ok := requested[change.ID]; ok {
			byID[change.ID] = change
		}
	}
	if len(byID) != len(requested) {
		found := make(map[string]struct{}, len(byID))
		for id := range byID {
			found[id] = struct{}{}
		}
		return nil, fmt.Errorf("cannot redo requested changes: %s", missingChangeIDs(requested, found))
	}

	selected := make([]*FileChange, 0, len(ids))
	for _, id := range ids {
		change := byID[id]
		changeCopy := change
		selected = append(selected, &changeCopy)
	}
	if err := preflightRedoChanges(selected); err != nil {
		return nil, fmt.Errorf("failed to redo requested changes: %w", err)
	}

	applied := make([]*FileChange, 0, len(selected))
	for _, change := range selected {
		if err := m.applyChangeUnchecked(change); err != nil {
			rollback := make([]*FileChange, 0, len(applied))
			for i := len(applied) - 1; i >= 0; i-- {
				rollback = append(rollback, applied[i])
			}
			rbErr := preflightUndoChanges(rollback)
			if rbErr == nil {
				for _, appliedChange := range rollback {
					if undoErr := m.revertChangeUnchecked(appliedChange); undoErr != nil {
						rbErr = undoErr
						break
					}
				}
			}
			if rbErr != nil {
				return nil, fmt.Errorf(
					"failed to redo requested changes AND rollback incomplete (check logs): %w",
					err,
				)
			}
			return nil, fmt.Errorf("failed to redo requested changes (rolled back): %w", err)
		}
		applied = append(applied, change)
	}

	remainingUndone := make([]FileChange, 0, len(m.undone)-len(selected))
	for _, change := range m.undone {
		if _, ok := requested[change.ID]; !ok {
			remainingUndone = append(remainingUndone, change)
		}
	}
	m.undone = remainingUndone
	for _, change := range selected {
		m.tracker.RecordUnlocked(*change)
	}
	m.mutations++

	return applied, nil
}

// UndoLastGroup reverts all changes belonging to the same group as the most recent change.
// If the most recent change has no group, it behaves like a single Undo.
func (m *Manager) UndoLastGroup() ([]*FileChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	lastChange := m.tracker.GetLastUnlocked()
	if lastChange == nil {
		return nil, fmt.Errorf("nothing to undo")
	}

	if lastChange.GroupID == "" {
		// No group — single undo via internal path
		change := m.tracker.PopLastUnlocked()
		if change == nil {
			return nil, fmt.Errorf("nothing to undo")
		}
		if err := m.revertChange(change); err != nil {
			m.tracker.RecordUnlocked(*change)
			return nil, fmt.Errorf("failed to undo: %w", err)
		}
		if len(m.undone) >= m.maxRedo {
			m.undone = m.undone[1:]
		}
		m.undone = append(m.undone, *change)
		m.mutations++
		return []*FileChange{change}, nil
	}

	return m.undoGroupLocked(lastChange.GroupID)
}

// undoGroupLocked implements group undo. Caller must hold m.mu.
func (m *Manager) undoGroupLocked(groupID string) ([]*FileChange, error) {
	// Collect all changes with this group ID (newest first)
	var grouped []*FileChange
	for i := len(m.tracker.changes) - 1; i >= 0; i-- {
		if m.tracker.changes[i].GroupID == groupID {
			change := m.tracker.changes[i]
			grouped = append(grouped, &change)
		}
	}

	if len(grouped) == 0 {
		return nil, fmt.Errorf("no changes found for group %s", groupID)
	}

	// Validate the entire group before the first destructive write. Checking
	// each change only as it is reverted can leave a group temporarily (or, if
	// rollback itself fails, permanently) half-undone when a later path was
	// edited outside gokin. The virtual state also handles multiple changes to
	// the same path: each successful simulated revert feeds the next older
	// change without touching disk.
	if err := preflightUndoChanges(grouped); err != nil {
		return nil, fmt.Errorf("failed to undo group: %w", err)
	}

	// Revert all in reverse order (newest first)
	var reverted []*FileChange
	for _, change := range grouped {
		if err := m.revertChange(change); err != nil {
			// Rollback is the redo half of the group transaction. Preflight the
			// whole rollback order before its first write for the same reason the
			// forward group undo is preflighted above.
			rollbackFailed := false
			rollback := make([]*FileChange, 0, len(reverted))
			for j := len(reverted) - 1; j >= 0; j-- {
				rollback = append(rollback, reverted[j])
			}
			if rbErr := preflightRedoChanges(rollback); rbErr != nil {
				logging.Error("undo group rollback preflight failed", "error", rbErr)
				rollbackFailed = true
			} else {
				for _, rollbackChange := range rollback {
					if rbErr := m.applyChange(rollbackChange); rbErr != nil {
						logging.Error("undo group rollback failed",
							"file", rollbackChange.FilePath,
							"error", rbErr)
						rollbackFailed = true
					}
				}
			}
			if rollbackFailed {
				return nil, fmt.Errorf("failed to undo group AND rollback incomplete (check logs): %w", err)
			}
			return nil, fmt.Errorf("failed to undo group (rolled back): %w", err)
		}
		reverted = append(reverted, change)
	}

	// Remove grouped changes from tracker
	remaining := make([]FileChange, 0, len(m.tracker.changes))
	for _, c := range m.tracker.changes {
		if c.GroupID != groupID {
			remaining = append(remaining, c)
		}
	}
	m.tracker.changes = remaining

	// Add to redo stack
	for _, change := range reverted {
		if len(m.undone) >= m.maxRedo {
			m.undone = m.undone[1:]
		}
		m.undone = append(m.undone, *change)
	}
	m.mutations++

	return reverted, nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tracker.List()
}

// ListRecent returns the N most recent changes.
func (m *Manager) ListRecent(n int) []FileChange {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tracker.ListRecent(n)
}

// ChangeIDs returns the current undo history IDs in execution order (oldest
// first). It is intentionally content-free so callers can capture lightweight
// transaction boundaries without copying file snapshots.
func (m *Manager) ChangeIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, len(m.tracker.changes))
	for i := range m.tracker.changes {
		ids[i] = m.tracker.changes[i].ID
	}
	return ids
}

// Snapshot returns IDs and revision counters under one lock so no concurrent
// Record can land between the boundary's identity and count observations.
func (m *Manager) Snapshot() HistorySnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshot := HistorySnapshot{
		ChangeIDs:   make([]string, len(m.tracker.changes)),
		RecordCount: m.recordCount,
		Mutations:   m.mutations,
	}
	for i := range m.tracker.changes {
		snapshot.ChangeIDs[i] = m.tracker.changes[i].ID
	}
	return snapshot
}

// Count returns the number of undoable changes.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tracker.Count()
}

// Clear clears all tracked changes and redo history.
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracker.Clear()
	m.undone = make([]FileChange, 0)
	m.mutations++
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
	m.mutations++
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

func uniqueChangeIDs(ids []string) (map[string]struct{}, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("no change IDs supplied")
	}
	result := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("change ID cannot be empty")
		}
		if _, exists := result[id]; exists {
			return nil, fmt.Errorf("duplicate change ID %s", id)
		}
		result[id] = struct{}{}
	}
	return result, nil
}

func missingChangeIDs(requested, found map[string]struct{}) string {
	missing := make([]string, 0)
	for id := range requested {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	return fmt.Sprintf("change history is no longer available for IDs %v", missing)
}

func changePaths(change *FileChange) []string {
	if change.Tool == "move" {
		return []string{string(change.OldContent), change.FilePath}
	}
	return []string{change.FilePath}
}

func rollbackUndoChanges(reverted []*FileChange) error {
	rollback := make([]*FileChange, 0, len(reverted))
	for i := len(reverted) - 1; i >= 0; i-- {
		rollback = append(rollback, reverted[i])
	}
	if err := preflightRedoChanges(rollback); err != nil {
		return err
	}
	for _, change := range rollback {
		if err := applyChangeUncheckedStandalone(change); err != nil {
			return err
		}
	}
	return nil
}

// applyChangeUncheckedStandalone mirrors Manager.applyChangeUnchecked without
// requiring a Manager receiver. It is used only while rolling back a failed
// exact undo transaction under the manager lock.
func applyChangeUncheckedStandalone(change *FileChange) error {
	switch change.Tool {
	case "move":
		return applyMove(change)
	case "mkdir":
		return os.MkdirAll(change.FilePath, 0755)
	case "delete", "batch_delete":
		if err := os.Remove(change.FilePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(change.FilePath), 0755); err != nil {
		return err
	}
	return fileutil.AtomicWrite(change.FilePath, change.NewContent, permOrDefault(change.Mode))
}

// revertChange reverts a file change to its previous state.
func (m *Manager) revertChange(change *FileChange) error {
	if err := preflightUndoChanges([]*FileChange{change}); err != nil {
		return err
	}
	return m.revertChangeUnchecked(change)
}

// revertChangeUnchecked performs the filesystem mutation after its caller has
// established that the path still matches the state produced by the tool.
func (m *Manager) revertChangeUnchecked(change *FileChange) error {
	// Move is special: undo means rename back to original source path,
	// not write OldContent (which holds the source path, not file bytes).
	if change.Tool == "move" {
		return revertMove(change)
	}

	// Mkdir is special: a parents=true creation may have brought several
	// directories into existence while naming only the leaf, so the generic
	// WasNew branch below would remove the leaf and leave the rest behind.
	if change.Tool == "mkdir" {
		return revertMkdir(change)
	}

	if change.WasNew {
		// File was created - delete it
		if err := os.Remove(change.FilePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	// File was modified - restore old content atomically, preserving the
	// original file mode (0644 fallback for legacy changes with no recorded Mode).
	dir := filepath.Dir(change.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return fileutil.AtomicWrite(change.FilePath, change.OldContent, permOrDefault(change.Mode))
}

// revertMkdir removes the directory a mkdir created, then prunes the ancestor
// directories the SAME call created, deepest-first. Ancestors are best-effort:
// os.Remove refuses a non-empty directory, so anything that gained content
// since the mkdir is left in place rather than deleted, and the climb stops
// there because every remaining ancestor necessarily still holds it. Legacy
// changes carry no CreatedDirs and degrade to the original leaf-only removal.
func revertMkdir(change *FileChange) error {
	if err := os.Remove(change.FilePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, dir := range change.CreatedDirs {
		if dir == change.FilePath {
			continue
		}
		if err := os.Remove(dir); err != nil {
			return nil
		}
	}
	return nil
}

// permOrDefault returns the change's recorded original file mode, falling back
// to 0644 when unset (legacy persisted changes have no Mode). Without this,
// undo/redo of a modified executable/script silently stripped 0755 → 0644.
func permOrDefault(m os.FileMode) os.FileMode {
	if m == 0 {
		return 0644
	}
	return m
}

// applyChange applies a file change (for redo).
func (m *Manager) applyChange(change *FileChange) error {
	if err := preflightRedoChanges([]*FileChange{change}); err != nil {
		return err
	}
	return m.applyChangeUnchecked(change)
}

// applyChangeUnchecked performs the redo mutation after the expected pre-state
// has been verified. Keeping validation at the Manager boundary prevents redo
// from overwriting edits made after an undo.
func (m *Manager) applyChangeUnchecked(change *FileChange) error {
	switch change.Tool {
	case "move":
		return applyMove(change)
	case "mkdir":
		// Redo a directory creation as a DIRECTORY — the generic AtomicWrite below
		// would create a regular empty file at the path instead.
		return os.MkdirAll(change.FilePath, 0755)
	case "delete", "batch_delete":
		// Redo a deletion by RE-REMOVING the path — the generic AtomicWrite would
		// resurrect it as a 0-byte file. Both the single-delete and batch-delete
		// tools land here (they record distinct Tool tags). IsNotExist is a no-op.
		if err := os.Remove(change.FilePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	dir := filepath.Dir(change.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	return fileutil.AtomicWrite(change.FilePath, change.NewContent, permOrDefault(change.Mode))
}

type pathStateKind uint8

const (
	pathStateMissing pathStateKind = iota
	pathStateFile
	pathStateDirectory
	pathStateOther
)

// pathState deliberately represents a missing path separately from a regular
// file with zero bytes. bytes.Equal(nil, []byte{}) is true, but those two
// filesystem states have very different undo semantics.
type pathState struct {
	kind    pathStateKind
	content []byte
}

func missingPathState() pathState            { return pathState{kind: pathStateMissing} }
func filePathState(content []byte) pathState { return pathState{kind: pathStateFile, content: content} }
func directoryPathState() pathState          { return pathState{kind: pathStateDirectory} }
func otherPathState() pathState              { return pathState{kind: pathStateOther} }
func (s pathState) exists() bool             { return s.kind != pathStateMissing }
func (s pathState) matches(other pathState) bool {
	return s.kind == other.kind && (s.kind != pathStateFile || bytes.Equal(s.content, other.content))
}

func (s pathState) description() string {
	switch s.kind {
	case pathStateMissing:
		return "missing path"
	case pathStateFile:
		return fmt.Sprintf("regular file (%d bytes)", len(s.content))
	case pathStateDirectory:
		return "directory"
	default:
		return "non-regular filesystem entry"
	}
}

type pathStateView struct {
	states map[string]pathState
}

func newPathStateView() *pathStateView {
	return &pathStateView{states: make(map[string]pathState)}
}

func (v *pathStateView) get(path string) (pathState, error) {
	if state, ok := v.states[path]; ok {
		return state, nil
	}
	state, err := readPathState(path)
	if err != nil {
		return pathState{}, err
	}
	v.states[path] = state
	return state, nil
}

func (v *pathStateView) set(path string, state pathState) {
	v.states[path] = state
}

func readPathState(path string) (pathState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return missingPathState(), nil
		}
		return pathState{}, err
	}

	switch {
	case info.Mode().IsRegular():
		content, err := os.ReadFile(path)
		if err != nil {
			return pathState{}, err
		}
		return filePathState(content), nil
	case info.IsDir():
		return directoryPathState(), nil
	default:
		// Never follow or replace symlinks/devices/FIFOs as if they were the
		// regular file recorded by an edit tool.
		return otherPathState(), nil
	}
}

func preflightUndoChanges(changes []*FileChange) error {
	view := newPathStateView()
	for _, change := range changes {
		if err := simulateUndo(change, view); err != nil {
			return err
		}
	}
	return nil
}

func preflightRedoChanges(changes []*FileChange) error {
	view := newPathStateView()
	for _, change := range changes {
		if err := simulateRedo(change, view); err != nil {
			return err
		}
	}
	return nil
}

func simulateUndo(change *FileChange, view *pathStateView) error {
	switch change.Tool {
	case "move":
		source := string(change.OldContent)
		if source == "" {
			return fmt.Errorf("move undo: missing original source path")
		}
		if err := requirePathState(view, source, missingPathState(), "undo"); err != nil {
			return err
		}
		destination, err := requirePresentPath(view, change.FilePath, "undo")
		if err != nil {
			return err
		}
		view.set(source, destination)
		view.set(change.FilePath, missingPathState())
		return nil

	case "delete", "batch_delete":
		if err := requirePathState(view, change.FilePath, missingPathState(), "undo"); err != nil {
			return err
		}
		view.set(change.FilePath, filePathState(change.OldContent))
		return nil

	case "mkdir":
		if err := requirePathState(view, change.FilePath, directoryPathState(), "undo"); err != nil {
			return err
		}
		view.set(change.FilePath, missingPathState())
		return nil

	default:
		if err := requirePathState(view, change.FilePath, filePathState(change.NewContent), "undo"); err != nil {
			return err
		}
		if change.WasNew {
			view.set(change.FilePath, missingPathState())
		} else {
			view.set(change.FilePath, filePathState(change.OldContent))
		}
		return nil
	}
}

func simulateRedo(change *FileChange, view *pathStateView) error {
	switch change.Tool {
	case "move":
		source := string(change.OldContent)
		if source == "" {
			return fmt.Errorf("move redo: missing original source path")
		}
		sourceState, err := requirePresentPath(view, source, "redo")
		if err != nil {
			return err
		}
		if err := requirePathState(view, change.FilePath, missingPathState(), "redo"); err != nil {
			return err
		}
		view.set(source, missingPathState())
		view.set(change.FilePath, sourceState)
		return nil

	case "delete", "batch_delete":
		if err := requirePathState(view, change.FilePath, filePathState(change.OldContent), "redo"); err != nil {
			return err
		}
		view.set(change.FilePath, missingPathState())
		return nil

	case "mkdir":
		if err := requirePathState(view, change.FilePath, missingPathState(), "redo"); err != nil {
			return err
		}
		view.set(change.FilePath, directoryPathState())
		return nil

	default:
		expected := filePathState(change.OldContent)
		if change.WasNew {
			expected = missingPathState()
		}
		if err := requirePathState(view, change.FilePath, expected, "redo"); err != nil {
			return err
		}
		view.set(change.FilePath, filePathState(change.NewContent))
		return nil
	}
}

func requirePathState(view *pathStateView, path string, expected pathState, operation string) error {
	actual, err := view.get(path)
	if err != nil {
		return fmt.Errorf("%s: inspect %s: %w", operation, path, err)
	}
	if !actual.matches(expected) {
		return fmt.Errorf(
			"%s conflict for %s: working tree changed (expected %s, found %s)",
			operation, path, expected.description(), actual.description(),
		)
	}
	return nil
}

func requirePresentPath(view *pathStateView, path string, operation string) (pathState, error) {
	actual, err := view.get(path)
	if err != nil {
		return pathState{}, fmt.Errorf("%s: inspect %s: %w", operation, path, err)
	}
	if !actual.exists() {
		return pathState{}, fmt.Errorf(
			"%s conflict for %s: working tree changed (expected existing path, found %s)",
			operation, path, actual.description(),
		)
	}
	return actual, nil
}

// revertMove reverses a move by renaming change.FilePath (current dest)
// back to the original source path stored in change.OldContent.
func revertMove(change *FileChange) error {
	originalSource := string(change.OldContent)
	if originalSource == "" {
		return fmt.Errorf("move undo: missing original source path")
	}
	if err := os.MkdirAll(filepath.Dir(originalSource), 0755); err != nil {
		return err
	}
	return os.Rename(change.FilePath, originalSource)
}

// applyMove re-applies a move by renaming the original source path back to
// change.FilePath (the recorded destination).
func applyMove(change *FileChange) error {
	originalSource := string(change.OldContent)
	if originalSource == "" {
		return fmt.Errorf("move redo: missing original source path")
	}
	if err := os.MkdirAll(filepath.Dir(change.FilePath), 0755); err != nil {
		return err
	}
	return os.Rename(originalSource, change.FilePath)
}
