package plan

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gooner/internal/contract"
)

// ApprovalDecision represents the user's decision on a plan.
type ApprovalDecision int

const (
	ApprovalPending ApprovalDecision = iota
	ApprovalApproved
	ApprovalRejected
	ApprovalModified
)

// ApprovalHandler is called to get user approval for a plan.
type ApprovalHandler func(ctx context.Context, plan *Plan) (ApprovalDecision, error)

// StepHandler is called before executing each step.
// It can be used to show progress or confirm individual steps.
type StepHandler func(step *Step)

// Manager manages plan mode state and execution.
type Manager struct {
	enabled         bool
	requireApproval bool

	currentPlan      *Plan
	lastRejectedPlan *Plan  // Store the last rejected plan for context
	lastFeedback     string // Store the last user feedback for plan modifications
	approvalHandler  ApprovalHandler
	onStepStart      StepHandler
	onStepComplete   StepHandler
	onProgressUpdate func(progress *ProgressUpdate) // Progress update handler
	undoExtension    *ManagerUndoExtension          // Undo/redo support

	// Contract capabilities (merged from contract.Manager)
	contractStore    *contract.Store
	contractVerifier *contract.Verifier

	// Context-clear signaling for plan execution
	contextClearRequested bool
	approvedPlanSnapshot  *Plan

	mu sync.RWMutex
}

// NewManager creates a new plan manager.
func NewManager(enabled, requireApproval bool) *Manager {
	return &Manager{
		enabled:         enabled,
		requireApproval: requireApproval,
	}
}

// SetApprovalHandler sets the handler for plan approval.
func (m *Manager) SetApprovalHandler(handler ApprovalHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvalHandler = handler
}

// SetStepHandlers sets the step lifecycle handlers.
func (m *Manager) SetStepHandlers(onStart, onComplete StepHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStepStart = onStart
	m.onStepComplete = onComplete
}

// IsEnabled returns whether plan mode is enabled.
func (m *Manager) IsEnabled() bool {
	return m.enabled
}

// SetEnabled enables or disables plan mode.
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enabled
}

// IsActive returns true if there's an active plan.
func (m *Manager) IsActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentPlan != nil && !m.currentPlan.IsComplete()
}

// GetCurrentPlan returns the current plan.
func (m *Manager) GetCurrentPlan() *Plan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentPlan
}

// CreatePlan creates a new plan and sets it as current.
func (m *Manager) CreatePlan(title, description, request string) *Plan {
	m.mu.Lock()
	defer m.mu.Unlock()

	plan := NewPlan(title, description)
	plan.Request = request
	m.currentPlan = plan
	return plan
}

// SetPlan sets the current plan.
func (m *Manager) SetPlan(plan *Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentPlan = plan
}

// ClearPlan clears the current plan and returns it if it existed.
func (m *Manager) ClearPlan() *Plan {
	m.mu.Lock()
	defer m.mu.Unlock()
	plan := m.currentPlan
	m.currentPlan = nil
	return plan
}

// GetLastRejectedPlan returns the last rejected plan (if saved).
func (m *Manager) GetLastRejectedPlan() *Plan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastRejectedPlan
}

// SaveRejectedPlan saves a plan as rejected for later reference.
func (m *Manager) SaveRejectedPlan(plan *Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastRejectedPlan = plan
}

// SetFeedback stores user feedback for plan modifications.
func (m *Manager) SetFeedback(feedback string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastFeedback = feedback
}

// GetFeedback returns the last user feedback and clears it.
func (m *Manager) GetFeedback() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	feedback := m.lastFeedback
	m.lastFeedback = ""
	return feedback
}

// HasFeedback returns true if there's pending user feedback.
func (m *Manager) HasFeedback() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastFeedback != ""
}

// RequestApproval requests user approval for the current plan.
func (m *Manager) RequestApproval(ctx context.Context) (ApprovalDecision, error) {
	m.mu.RLock()
	plan := m.currentPlan
	handler := m.approvalHandler
	m.mu.RUnlock()

	if plan == nil {
		return ApprovalRejected, nil
	}

	if !m.requireApproval {
		return ApprovalApproved, nil
	}

	if handler == nil {
		// No handler, auto-approve
		return ApprovalApproved, nil
	}

	return handler(ctx, plan)
}

// StartStep marks a step as started.
func (m *Manager) StartStep(stepID int) {
	m.mu.Lock()
	plan := m.currentPlan
	onStart := m.onStepStart
	onProgress := m.onProgressUpdate
	m.mu.Unlock()

	if plan == nil {
		return
	}

	plan.StartStep(stepID)

	// Send progress update
	if onProgress != nil {
		step := plan.GetStep(stepID)
		if step != nil {
			// Use thread-safe methods for accessing plan data
			onProgress(&ProgressUpdate{
				PlanID:        plan.ID,
				CurrentStepID: stepID,
				CurrentTitle:  step.Title,
				TotalSteps:    plan.StepCount(),
				Completed:     plan.CompletedCount(),
				Progress:      plan.Progress(),
				Status:        "in_progress",
			})
		}
	}

	if onStart != nil {
		step := plan.GetStep(stepID)
		if step != nil {
			onStart(step)
		}
	}
}

// CompleteStep marks a step as completed.
func (m *Manager) CompleteStep(stepID int, output string) {
	m.mu.Lock()
	plan := m.currentPlan
	onComplete := m.onStepComplete
	onProgress := m.onProgressUpdate
	m.mu.Unlock()

	if plan == nil {
		return
	}

	plan.CompleteStep(stepID, output)

	// Send progress update
	if onProgress != nil {
		step := plan.GetStep(stepID)
		if step != nil {
			status := "in_progress"
			if plan.Progress() >= 1.0 {
				status = "completed"
			}
			// Use thread-safe methods for accessing plan data
			onProgress(&ProgressUpdate{
				PlanID:        plan.ID,
				CurrentStepID: stepID,
				CurrentTitle:  step.Title,
				TotalSteps:    plan.StepCount(),
				Completed:     plan.CompletedCount(),
				Progress:      plan.Progress(),
				Status:        status,
			})
		}
	}

	if onComplete != nil {
		step := plan.GetStep(stepID)
		if step != nil {
			onComplete(step)
		}
	}
}

// FailStep marks a step as failed.
func (m *Manager) FailStep(stepID int, errMsg string) {
	m.mu.Lock()
	plan := m.currentPlan
	onProgress := m.onProgressUpdate
	m.mu.Unlock()

	if plan == nil {
		return
	}

	plan.FailStep(stepID, errMsg)

	// Send progress update
	if onProgress != nil {
		step := plan.GetStep(stepID)
		if step != nil {
			// Use thread-safe methods for accessing plan data
			onProgress(&ProgressUpdate{
				PlanID:        plan.ID,
				CurrentStepID: stepID,
				CurrentTitle:  step.Title,
				TotalSteps:    plan.StepCount(),
				Completed:     plan.CompletedCount(),
				Progress:      plan.Progress(),
				Status:        "failed",
			})
		}
	}
}

// SkipStep marks a step as skipped.
func (m *Manager) SkipStep(stepID int) {
	m.mu.Lock()
	plan := m.currentPlan
	onProgress := m.onProgressUpdate
	m.mu.Unlock()

	if plan == nil {
		return
	}

	plan.SkipStep(stepID)

	// Send progress update
	if onProgress != nil {
		step := plan.GetStep(stepID)
		if step != nil {
			// Use thread-safe methods for accessing plan data
			onProgress(&ProgressUpdate{
				PlanID:        plan.ID,
				CurrentStepID: stepID,
				CurrentTitle:  step.Title,
				TotalSteps:    plan.StepCount(),
				Completed:     plan.CompletedCount(),
				Progress:      plan.Progress(),
				Status:        "skipped",
			})
		}
	}
}

// GetProgress returns the current plan's progress.
func (m *Manager) GetProgress() (current, total int, percent float64) {
	m.mu.RLock()
	plan := m.currentPlan
	m.mu.RUnlock()

	if plan == nil {
		return 0, 0, 0
	}

	total = plan.StepCount()
	current = plan.CompletedCount()
	percent = plan.Progress()
	return
}

// AddStep adds a step to the current plan.
func (m *Manager) AddStep(title, description string) *Step {
	m.mu.RLock()
	plan := m.currentPlan
	m.mu.RUnlock()

	if plan == nil {
		return nil
	}

	return plan.AddStep(title, description)
}

// NextStep returns the next pending step.
func (m *Manager) NextStep() *Step {
	m.mu.RLock()
	plan := m.currentPlan
	m.mu.RUnlock()

	if plan == nil {
		return nil
	}

	return plan.NextStep()
}

// GetPreviousStepsSummary returns a compact summary of completed steps for context injection.
// Each completed step's output is truncated to maxLen characters.
func (m *Manager) GetPreviousStepsSummary(currentStepID int, maxLen int) string {
	m.mu.RLock()
	plan := m.currentPlan
	m.mu.RUnlock()

	if plan == nil {
		return ""
	}

	var sb strings.Builder
	for _, step := range plan.Steps {
		if step.ID >= currentStepID {
			break
		}
		if step.Status == StatusCompleted {
			sb.WriteString(fmt.Sprintf("Step %d (%s): ", step.ID, step.Title))
			output := step.Output
			if len(output) > maxLen {
				output = output[:maxLen] + "..."
			}
			if output == "" {
				output = "completed"
			}
			sb.WriteString(output)
			sb.WriteString("\n")
		} else if step.Status == StatusFailed {
			sb.WriteString(fmt.Sprintf("Step %d (%s): FAILED\n", step.ID, step.Title))
		}
	}
	return sb.String()
}

// SetProgressUpdateHandler sets the progress update handler.
func (m *Manager) SetProgressUpdateHandler(handler func(progress *ProgressUpdate)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onProgressUpdate = handler
}

// EnableUndo enables undo/redo support for plan execution.
func (m *Manager) EnableUndo(maxHistory int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.undoExtension = NewManagerUndoExtension(m, maxHistory)
}

// DisableUndo disables undo/redo support.
func (m *Manager) DisableUndo() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.undoExtension = nil
}

// SavePlanCheckpoint saves a checkpoint before plan execution.
func (m *Manager) SavePlanCheckpoint() error {
	m.mu.RLock()
	undoExt := m.undoExtension
	m.mu.RUnlock()

	if undoExt == nil {
		return nil // Undo not enabled, ignore
	}

	return undoExt.SaveCheckpoint()
}

// GetUndoExtension returns the undo extension (if enabled).
func (m *Manager) GetUndoExtension() *ManagerUndoExtension {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.undoExtension
}

// RequestContextClear sets the context-clear flag and snapshots the approved plan.
// Called from tool execution when a plan is approved with context clearing enabled.
func (m *Manager) RequestContextClear(plan *Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contextClearRequested = true
	m.approvedPlanSnapshot = plan
}

// IsContextClearRequested returns whether a context clear has been requested.
func (m *Manager) IsContextClearRequested() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.contextClearRequested
}

// ConsumeContextClearRequest reads the context-clear flag and clears it (consume-once).
// Returns the approved plan snapshot, or nil if no request was pending.
func (m *Manager) ConsumeContextClearRequest() *Plan {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.contextClearRequested {
		return nil
	}
	m.contextClearRequested = false
	plan := m.approvedPlanSnapshot
	m.approvedPlanSnapshot = nil
	return plan
}

// SetContractStore sets the contract store for persistence.
func (m *Manager) SetContractStore(store *contract.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contractStore = store
}

// SetContractVerifier sets the contract verifier.
func (m *Manager) SetContractVerifier(verifier *contract.Verifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contractVerifier = verifier
}

// GetContractStore returns the contract store.
func (m *Manager) GetContractStore() *contract.Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.contractStore
}

// GetContractVerifier returns the contract verifier.
func (m *Manager) GetContractVerifier() *contract.Verifier {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.contractVerifier
}

// GetActiveContractContext returns formatted contract text for context injection.
// Returns empty string if no active plan has a contract.
func (m *Manager) GetActiveContractContext() string {
	m.mu.RLock()
	plan := m.currentPlan
	m.mu.RUnlock()

	if plan == nil || plan.Contract == nil {
		return ""
	}

	spec := plan.Contract
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Contract: %s\n", spec.Name))
	sb.WriteString(fmt.Sprintf("Intent: %s\n", spec.Intent))

	if len(spec.Boundaries) > 0 {
		sb.WriteString("\nBoundaries:\n")
		for _, b := range spec.Boundaries {
			sb.WriteString(fmt.Sprintf("  - [%s] %s: %s", b.Type, b.Name, b.Description))
			if b.Constraint != "" {
				sb.WriteString(fmt.Sprintf(" (constraint: %s)", b.Constraint))
			}
			sb.WriteString("\n")
		}
	}

	if len(spec.Invariants) > 0 {
		sb.WriteString("\nInvariants:\n")
		for _, inv := range spec.Invariants {
			sb.WriteString(fmt.Sprintf("  - [%s] %s: %s\n", inv.Type, inv.Name, inv.Description))
		}
	}

	if len(spec.Examples) > 0 {
		sb.WriteString("\nExamples:\n")
		for _, ex := range spec.Examples {
			sb.WriteString(fmt.Sprintf("  - %s", ex.Name))
			if ex.Input != "" {
				sb.WriteString(fmt.Sprintf(": %s", ex.Input))
			}
			if ex.ExpectedOutput != "" {
				sb.WriteString(fmt.Sprintf(" -> %s", ex.ExpectedOutput))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
