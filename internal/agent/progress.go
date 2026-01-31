package agent

import (
	"fmt"
	"time"
)

// AgentProgress represents the current progress of an agent.
type AgentProgress struct {
	AgentID            string
	AgentType          AgentType
	CurrentStep        int
	TotalSteps         int
	CurrentAction      string
	StartTime          time.Time
	Elapsed            time.Duration
	EstimatedRemaining time.Duration
	ToolsUsed          []string
	Status             AgentStatus
}

// ProgressCallback is called when agent progress is updated.
type ProgressCallback func(progress *AgentProgress)

// SetProgressCallback sets the callback for progress updates.
func (a *Agent) SetProgressCallback(callback ProgressCallback) {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()
	a.progressCallback = callback
}

// updateProgress sends a progress update if callback is set.
func (a *Agent) updateProgress() {
	a.progressMu.Lock()
	callback := a.progressCallback
	a.progressMu.Unlock()

	if callback == nil {
		return
	}

	progress := &AgentProgress{
		AgentID:       a.ID,
		AgentType:     a.Type,
		CurrentStep:   a.currentStep,
		TotalSteps:    a.totalSteps,
		CurrentAction: a.stepDescription,
		StartTime:     a.startTime,
		Elapsed:       time.Since(a.startTime),
		Status:        a.status,
	}

	callback(progress)
}

// SetProgress sets the current progress state.
func (a *Agent) SetProgress(step int, total int, description string) {
	a.progressMu.Lock()
	a.currentStep = step
	a.totalSteps = total
	a.stepDescription = description
	a.progressMu.Unlock()

	a.updateProgress()
}

// IncrementStep increments the current step and updates description.
func (a *Agent) IncrementStep(description string) {
	a.progressMu.Lock()
	a.currentStep++
	a.stepDescription = description
	a.progressMu.Unlock()

	a.updateProgress()
}

// GetProgress returns the current progress state.
func (a *Agent) GetProgress() AgentProgress {
	a.progressMu.Lock()
	defer a.progressMu.Unlock()

	// Get tools used
	a.toolsMu.Lock()
	toolsUsed := make([]string, len(a.toolsUsed))
	copy(toolsUsed, a.toolsUsed)
	a.toolsMu.Unlock()

	return AgentProgress{
		AgentID:       a.ID,
		AgentType:     a.Type,
		CurrentStep:   a.currentStep,
		TotalSteps:    a.totalSteps,
		CurrentAction: a.stepDescription,
		StartTime:     a.startTime,
		Elapsed:       time.Since(a.startTime),
		Status:        a.status,
		ToolsUsed:     toolsUsed,
	}
}

// FormatProgress returns a formatted progress string.
func (p *AgentProgress) FormatProgress() string {
	if p.TotalSteps <= 0 {
		return fmt.Sprintf("[%s] Step %d: %s", p.AgentType, p.CurrentStep, p.CurrentAction)
	}

	percent := float64(p.CurrentStep) / float64(p.TotalSteps) * 100
	return fmt.Sprintf("[%s] %d/%d (%.0f%%): %s",
		p.AgentType, p.CurrentStep, p.TotalSteps, percent, p.CurrentAction)
}

// EstimateRemaining estimates the remaining time based on elapsed time and progress.
func (p *AgentProgress) EstimateRemaining() time.Duration {
	if p.CurrentStep <= 0 || p.TotalSteps <= 0 {
		return 0
	}

	avgTimePerStep := p.Elapsed / time.Duration(p.CurrentStep)
	stepsRemaining := p.TotalSteps - p.CurrentStep
	return avgTimePerStep * time.Duration(stepsRemaining)
}
