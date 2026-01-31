package plan

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"gokin/internal/contract"
)

// ContractSpec holds optional contract fields for a plan.
type ContractSpec struct {
	Name       string               `json:"name"`
	Intent     string               `json:"intent"`
	Boundaries []contract.Boundary  `json:"boundaries,omitempty"`
	Invariants []contract.Invariant `json:"invariants,omitempty"`
	Examples   []contract.Example   `json:"examples,omitempty"`
}

// Status represents the status of a plan or step.
type Status int

const (
	StatusPending Status = iota
	StatusInProgress
	StatusCompleted
	StatusFailed
	StatusSkipped
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

	// Optional contract specification
	Contract   *ContractSpec `json:"contract,omitempty"`
	ContractID string        `json:"contract_id,omitempty"` // Links to persisted contract in store

	mu sync.RWMutex
}

// HasContract returns true if the plan has a contract specification.
func (p *Plan) HasContract() bool {
	return p.Contract != nil
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

// Format returns a formatted string representation of the plan.
func (p *Plan) Format() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var builder strings.Builder

	builder.WriteString(fmt.Sprintf("## %s\n", p.Title))
	if p.Description != "" {
		builder.WriteString(fmt.Sprintf("%s\n", p.Description))
	}
	builder.WriteString("\n")

	// Include contract info if present
	if p.Contract != nil {
		builder.WriteString(fmt.Sprintf("Contract: %s\n", p.Contract.Name))
		if p.Contract.Intent != "" {
			builder.WriteString(fmt.Sprintf("Intent: %s\n", p.Contract.Intent))
		}
		if len(p.Contract.Boundaries) > 0 {
			builder.WriteString(fmt.Sprintf("Boundaries: %d defined\n", len(p.Contract.Boundaries)))
		}
		if len(p.Contract.Invariants) > 0 {
			builder.WriteString(fmt.Sprintf("Invariants: %d defined\n", len(p.Contract.Invariants)))
		}
		if len(p.Contract.Examples) > 0 {
			builder.WriteString(fmt.Sprintf("Examples: %d defined\n", len(p.Contract.Examples)))
		}
		builder.WriteString("\n")
	}

	for _, step := range p.Steps {
		icon := step.Status.Icon()
		builder.WriteString(fmt.Sprintf("%s %d. %s\n", icon, step.ID, step.Title))
		if step.Description != "" {
			builder.WriteString(fmt.Sprintf("   %s\n", step.Description))
		}
	}

	progress := p.Progress()
	builder.WriteString(fmt.Sprintf("\nProgress: %.0f%% (%d/%d steps)\n",
		progress*100, int(progress*float64(len(p.Steps))), len(p.Steps)))

	return builder.String()
}

// StepCount returns the number of steps.
func (p *Plan) StepCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.Steps)
}

// GetStepsSnapshot returns a snapshot of all steps (thread-safe).
// The returned slice is a copy that can be safely iterated without holding locks.
func (p *Plan) GetStepsSnapshot() []*Step {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snapshot := make([]*Step, len(p.Steps))
	copy(snapshot, p.Steps)
	return snapshot
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
