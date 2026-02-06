package plan

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Status represents the status of a plan or step.
type Status int

const (
	StatusPending Status = iota
	StatusInProgress
	StatusCompleted
	StatusFailed
	StatusSkipped
	StatusPaused // Temporarily paused, can be resumed
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusInProgress:
		return "in_progress"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusSkipped:
		return "skipped"
	case StatusPaused:
		return "paused"
	default:
		return "unknown"
	}
}

// Icon returns a display icon for the status.
func (s Status) Icon() string {
	switch s {
	case StatusPending:
		return "○"
	case StatusInProgress:
		return "◐"
	case StatusCompleted:
		return "●"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "⊘"
	case StatusPaused:
		return "⏸"
	default:
		return "?"
	}
}

// Step represents a single step in a plan.
type Step struct {
	ID          int       `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      Status    `json:"status"`
	Output      string    `json:"output"`
	Error       string    `json:"error"`
	StartTime   time.Time `json:"start_time,omitempty"`
	EndTime     time.Time `json:"end_time,omitempty"`
	Parallel    bool      `json:"parallel"` // Can execute in parallel with other steps
	DependsOn   []int     `json:"depends_on,omitempty"` // Step IDs this step depends on
	Children    []*Step   `json:"children,omitempty"`    // Nested sub-steps
}

// Duration returns the step execution duration.
func (s *Step) Duration() time.Duration {
	if s.StartTime.IsZero() {
		return 0
	}
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

// Plan represents an execution plan.
type Plan struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Steps       []*Step   `json:"steps"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Request     string    `json:"request"` // Original user request

	// Context snapshot from planning conversation (preserved across session clear)
	ContextSnapshot string `json:"context_snapshot,omitempty"`

	mu sync.RWMutex
}

// SetContextSnapshot stores a summary of the planning conversation context.
func (p *Plan) SetContextSnapshot(snapshot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ContextSnapshot = snapshot
	p.UpdatedAt = time.Now()
}

// GetContextSnapshot returns the saved context snapshot.
func (p *Plan) GetContextSnapshot() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ContextSnapshot
}

// NewPlan creates a new plan.
func NewPlan(title, description string) *Plan {
	return &Plan{
		ID:          fmt.Sprintf("plan_%d", time.Now().UnixNano()),
		Title:       title,
		Description: description,
		Steps:       make([]*Step, 0),
		Status:      StatusPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
}

// AddStep adds a step to the plan.
func (p *Plan) AddStep(title, description string) *Step {
	return p.AddStepWithOptions(title, description, false, nil)
}

// AddStepWithOptions adds a step to the plan with options.
func (p *Plan) AddStepWithOptions(title, description string, parallel bool, dependsOn []int) *Step {
	p.mu.Lock()
	defer p.mu.Unlock()

	step := &Step{
		ID:          len(p.Steps) + 1,
		Title:       title,
		Description: description,
		Status:      StatusPending,
		Parallel:    parallel,
		DependsOn:   dependsOn,
	}
	p.Steps = append(p.Steps, step)
	p.UpdatedAt = time.Now()
	return step
}

// GetStep returns a step by ID.
func (p *Plan) GetStep(id int) *Step {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, step := range p.Steps {
		if step.ID == id {
			return step
		}
	}
	return nil
}

// CurrentStep returns the current in-progress step, or next pending step.
func (p *Plan) CurrentStep() *Step {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, step := range p.Steps {
		if step.Status == StatusInProgress {
			return step
		}
	}
	for _, step := range p.Steps {
		if step.Status == StatusPending {
			return step
		}
	}
	return nil
}

// NextStep returns the next pending step.
func (p *Plan) NextStep() *Step {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, step := range p.Steps {
		if step.Status == StatusPending {
			return step
		}
	}
	return nil
}

// StartStep marks a step as in progress.
func (p *Plan) StartStep(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, step := range p.Steps {
		if step.ID == id {
			step.Status = StatusInProgress
			step.StartTime = time.Now()
			p.Status = StatusInProgress
			p.UpdatedAt = time.Now()
			break
		}
	}
}

// CompleteStep marks a step as completed.
func (p *Plan) CompleteStep(id int, output string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, step := range p.Steps {
		if step.ID == id {
			step.Status = StatusCompleted
			step.Output = output
			step.EndTime = time.Now()
			p.UpdatedAt = time.Now()
			break
		}
	}

	// Check if all steps are completed
	allCompleted := true
	for _, step := range p.Steps {
		if step.Status != StatusCompleted && step.Status != StatusSkipped {
			allCompleted = false
			break
		}
	}
	if allCompleted {
		p.Status = StatusCompleted
	}
}

// FailStep marks a step as failed.
func (p *Plan) FailStep(id int, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, step := range p.Steps {
		if step.ID == id {
			step.Status = StatusFailed
			step.Error = errMsg
			step.EndTime = time.Now()
			p.Status = StatusFailed
			p.UpdatedAt = time.Now()
			break
		}
	}
}

// SkipStep marks a step as skipped.
func (p *Plan) SkipStep(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, step := range p.Steps {
		if step.ID == id {
			step.Status = StatusSkipped
			p.UpdatedAt = time.Now()
			break
		}
	}
}

// Progress returns the completion progress (0.0 to 1.0).
func (p *Plan) Progress() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.Steps) == 0 {
		return 0
	}

	completed := 0
	for _, step := range p.Steps {
		if step.Status == StatusCompleted || step.Status == StatusSkipped {
			completed++
		}
	}
	return float64(completed) / float64(len(p.Steps))
}

// IsComplete returns true if the plan is complete.
func (p *Plan) IsComplete() bool {
	return p.Status == StatusCompleted || p.Status == StatusFailed
}

// Format returns a formatted string representation of the plan using tree view.
func (p *Plan) Format() string {
	return p.RenderTree()
}

// RenderTree returns a tree view of the plan with status indicators.
// Symbols: "✓" (completed), "→" (in progress), "○" (pending), "✗" (failed), "⊘" (skipped).
// Parallel steps are visually marked with "║" borders.
func (p *Plan) RenderTree() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var builder strings.Builder

	builder.WriteString(fmt.Sprintf("## %s\n", p.Title))
	if p.Description != "" {
		builder.WriteString(fmt.Sprintf("%s\n", p.Description))
	}
	builder.WriteString("\n")

	for _, step := range p.Steps {
		renderStepTree(&builder, step, "")
	}

	progress := p.progressLocked()
	completedCount := 0
	for _, step := range p.Steps {
		if step.Status == StatusCompleted || step.Status == StatusSkipped {
			completedCount++
		}
	}
	builder.WriteString(fmt.Sprintf("\nProgress: %.0f%% (%d/%d steps)\n",
		progress*100, completedCount, len(p.Steps)))

	return builder.String()
}

// progressLocked returns the completion progress without acquiring the lock.
// Caller must hold at least a read lock.
func (p *Plan) progressLocked() float64 {
	if len(p.Steps) == 0 {
		return 0
	}

	completed := 0
	for _, step := range p.Steps {
		if step.Status == StatusCompleted || step.Status == StatusSkipped {
			completed++
		}
	}
	return float64(completed) / float64(len(p.Steps))
}

// renderStepTree renders a single step and its children recursively.
func renderStepTree(builder *strings.Builder, step *Step, indent string) {
	icon := stepTreeIcon(step.Status)

	if step.Parallel {
		builder.WriteString(fmt.Sprintf("%s║ %s Step %d: %s  ║  (parallel)\n", indent, icon, step.ID, step.Title))
	} else {
		builder.WriteString(fmt.Sprintf("%s%s Step %d: %s\n", indent, icon, step.ID, step.Title))
	}

	childIndent := indent + "  "
	for _, child := range step.Children {
		renderStepTree(builder, child, childIndent)
	}
}

// stepTreeIcon returns the tree icon for a given status.
func stepTreeIcon(s Status) string {
	switch s {
	case StatusCompleted:
		return "✓"
	case StatusInProgress:
		return "→"
	case StatusPending:
		return "○"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "⊘"
	default:
		return "○"
	}
}

// StepCount returns the number of steps.
func (p *Plan) StepCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.Steps)
}

// GetStepsSnapshot returns a snapshot of all steps (thread-safe).
// The returned slice is a deep copy that can be safely modified without affecting the original.
func (p *Plan) GetStepsSnapshot() []*Step {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snapshot := make([]*Step, len(p.Steps))
	for i, step := range p.Steps {
		if step != nil {
			snapshot[i] = deepCopyStep(step)
		}
	}
	return snapshot
}

// deepCopyStep performs a deep copy of a Step, including its Children.
func deepCopyStep(step *Step) *Step {
	stepCopy := *step
	if len(step.Children) > 0 {
		stepCopy.Children = make([]*Step, len(step.Children))
		for i, child := range step.Children {
			if child != nil {
				stepCopy.Children[i] = deepCopyStep(child)
			}
		}
	}
	return &stepCopy
}

// CompletedCount returns the number of completed steps.
func (p *Plan) CompletedCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, step := range p.Steps {
		if step.Status == StatusCompleted {
			count++
		}
	}
	return count
}

// PendingCount returns the number of pending or paused steps (resumable).
func (p *Plan) PendingCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, step := range p.Steps {
		if step.Status == StatusPending || step.Status == StatusPaused || step.Status == StatusFailed {
			count++
		}
	}
	return count
}

// PauseStep marks a step as paused (can be resumed later).
func (p *Plan) PauseStep(id int, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, step := range p.Steps {
		if step.ID == id {
			step.Status = StatusPaused
			step.Error = reason
			step.EndTime = time.Now()
			p.Status = StatusPaused
			p.UpdatedAt = time.Now()
			break
		}
	}
}
