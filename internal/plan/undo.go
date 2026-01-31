package plan

import (
	"fmt"
	"time"
)

// UndoState stores the state before plan execution for undo functionality.
type UndoState struct {
	PlanID      string       `json:"plan_id"`
	PlanTitle   string       `json:"plan_title"`
	Description string       `json:"description"`
	Request     string       `json:"request"`
	Steps       []*Step      `json:"steps"`
	Timestamp   time.Time    `json:"timestamp"`
	Executed    []int        `json:"executed"` // IDs of steps that were executed
}

// ManagerUndoExtension extends the plan Manager with undo/redo capabilities.
type ManagerUndoExtension struct {
	manager *Manager
	history []*UndoState
	maxHistory int
}

// NewManagerUndoExtension creates a new undo extension for a plan manager.
func NewManagerUndoExtension(manager *Manager, maxHistory int) *ManagerUndoExtension {
	if maxHistory <= 0 {
		maxHistory = 10 // Default to 10 history entries
	}
	return &ManagerUndoExtension{
		manager: manager,
		history: make([]*UndoState, 0, maxHistory),
		maxHistory: maxHistory,
	}
}

// SaveCheckpoint saves the current plan state before execution for potential undo.
func (e *ManagerUndoExtension) SaveCheckpoint() error {
	plan := e.manager.GetCurrentPlan()
	if plan == nil {
		return fmt.Errorf("no active plan to checkpoint")
	}

	// Create undo state
	state := &UndoState{
		PlanID:      plan.ID,
		PlanTitle:   plan.Title,
		Description: plan.Description,
		Request:     plan.Request,
		Steps:       make([]*Step, len(plan.Steps)),
		Timestamp:   time.Now(),
		Executed:    make([]int, 0),
	}

	// Copy steps
	copy(state.Steps, plan.Steps)

	// Add to history
	e.history = append(e.history, state)

	// Trim history if needed
	if len(e.history) > e.maxHistory {
		e.history = e.history[1:]
	}

	return nil
}

// RecordExecutedStep records that a step has been executed.
func (e *ManagerUndoExtension) RecordExecutedStep(stepID int) {
	if len(e.history) == 0 {
		return
	}

	lastState := e.history[len(e.history)-1]
	lastState.Executed = append(lastState.Executed, stepID)
}

// GetLastCheckpoint returns the most recent checkpoint.
func (e *ManagerUndoExtension) GetLastCheckpoint() *UndoState {
	if len(e.history) == 0 {
		return nil
	}
	return e.history[len(e.history)-1]
}

// GetHistory returns all saved checkpoints.
func (e *ManagerUndoExtension) GetHistory() []*UndoState {
	return e.history
}

// ClearHistory clears all saved checkpoints.
func (e *ManagerUndoExtension) ClearHistory() {
	e.history = make([]*UndoState, 0, e.maxHistory)
}

// CanUndo returns true if there's a checkpoint to restore.
func (e *ManagerUndoExtension) CanUndo() bool {
	return len(e.history) > 0
}
