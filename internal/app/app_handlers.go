package app

import (
	"context"
	"fmt"
	"time"

	"gokin/internal/config"
	"gokin/internal/logging"
	"gokin/internal/permission"
	"gokin/internal/plan"
	"gokin/internal/ui"
)

// PermissionTimeout is the maximum time to wait for a permission response.
const PermissionTimeout = 5 * time.Minute

// promptPermission is called by the permission manager to ask the user for permission.
// It sends a request to the TUI and waits for a response with timeout.
func (a *App) promptPermission(ctx context.Context, req *permission.Request) (permission.Decision, error) {
	if a.program == nil {
		return permission.DecisionAllow, nil
	}

	// Send permission request to TUI
	a.program.Send(ui.PermissionRequestMsg{
		ToolName:  req.ToolName,
		Args:      req.Args,
		RiskLevel: req.RiskLevel.String(),
		Reason:    req.Reason,
	})

	// Wait for response from TUI with timeout to prevent deadlock
	select {
	case decision := <-a.permResponseChan:
		return decision, nil
	case <-ctx.Done():
		return permission.DecisionDeny, ctx.Err()
	case <-time.After(PermissionTimeout):
		logging.Warn("permission prompt timed out", "tool", req.ToolName)
		return permission.DecisionDeny, fmt.Errorf("permission prompt timed out after %v", PermissionTimeout)
	}
}

// handlePermissionDecision is called by the TUI when the user makes a permission decision.
func (a *App) handlePermissionDecision(decision ui.PermissionDecision) {
	// Convert UI decision to permission.Decision
	var permDecision permission.Decision
	switch decision {
	case ui.PermissionAllow:
		permDecision = permission.DecisionAllow
	case ui.PermissionAllowSession:
		permDecision = permission.DecisionAllowSession
	case ui.PermissionDeny:
		permDecision = permission.DecisionDeny
	case ui.PermissionDenySession:
		permDecision = permission.DecisionDenySession
	default:
		permDecision = permission.DecisionDeny
	}

	// Send decision to the waiting promptPermission call with timeout
	select {
	case a.permResponseChan <- permDecision:
	case <-time.After(30 * time.Second):
		logging.Warn("permission response channel timeout - no listener")
	}
}

// QuestionTimeout is the maximum time to wait for a question response.
const QuestionTimeout = 5 * time.Minute

// promptQuestion is called by the AskUserTool to ask the user a question.
func (a *App) promptQuestion(ctx context.Context, question string, options []string, defaultOpt string) (string, error) {
	if a.program == nil {
		return "", fmt.Errorf("program not running")
	}

	// Send question request to TUI
	a.program.Send(ui.QuestionRequestMsg{
		Question: question,
		Options:  options,
		Default:  defaultOpt,
	})

	// Wait for response from TUI with timeout to prevent deadlock
	select {
	case answer := <-a.questionResponseChan:
		return answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(QuestionTimeout):
		logging.Warn("question prompt timed out")
		return "", fmt.Errorf("question prompt timed out after %v", QuestionTimeout)
	}
}

// handleQuestionAnswer is called by the TUI when the user answers a question.
func (a *App) handleQuestionAnswer(answer string) {
	// Send answer to the waiting promptQuestion call with timeout
	select {
	case a.questionResponseChan <- answer:
	case <-time.After(30 * time.Second):
		logging.Warn("question response channel timeout - no listener")
	}
}

// PlanApprovalTimeout is the maximum time to wait for a plan approval response.
const PlanApprovalTimeout = 10 * time.Minute

// promptPlanApproval is called by the plan manager to request user approval.
func (a *App) promptPlanApproval(ctx context.Context, p *plan.Plan) (plan.ApprovalDecision, error) {
	if a.program == nil {
		return plan.ApprovalApproved, nil
	}

	// Convert plan steps to UI format
	steps := make([]ui.PlanStepInfo, len(p.Steps))
	for i, step := range p.Steps {
		steps[i] = ui.PlanStepInfo{
			ID:          step.ID,
			Title:       step.Title,
			Description: step.Description,
		}
	}

	// Build plan approval request with optional contract fields
	msg := ui.PlanApprovalRequestMsg{
		Title:       p.Title,
		Description: p.Description,
		Steps:       steps,
	}

	// Include contract fields if the plan has a contract specification
	if p.Contract != nil {
		msg.ContractName = p.Contract.Name
		msg.Intent = p.Contract.Intent

		// Format boundaries for display
		for _, b := range p.Contract.Boundaries {
			s := fmt.Sprintf("[%s] %s: %s", b.Type, b.Name, b.Description)
			if b.Constraint != "" {
				s += fmt.Sprintf(" (%s)", b.Constraint)
			}
			msg.Boundaries = append(msg.Boundaries, s)
		}

		// Format invariants for display
		for _, inv := range p.Contract.Invariants {
			msg.Invariants = append(msg.Invariants, fmt.Sprintf("[%s] %s: %s", inv.Type, inv.Name, inv.Description))
		}

		// Format examples for display
		for _, ex := range p.Contract.Examples {
			s := ex.Name
			if ex.Input != "" {
				s += ": " + ex.Input
			}
			if ex.ExpectedOutput != "" {
				s += " -> " + ex.ExpectedOutput
			}
			msg.Examples = append(msg.Examples, s)
		}
	}

	// Send plan approval request to TUI
	a.program.Send(msg)

	// Wait for response from TUI with timeout to prevent deadlock
	select {
	case decision := <-a.planApprovalChan:
		return decision, nil
	case <-ctx.Done():
		return plan.ApprovalRejected, ctx.Err()
	case <-time.After(PlanApprovalTimeout):
		logging.Warn("plan approval prompt timed out")
		return plan.ApprovalRejected, fmt.Errorf("plan approval prompt timed out after %v", PlanApprovalTimeout)
	}
}

// handlePlanApproval is called by the TUI when the user makes a plan approval decision.
func (a *App) handlePlanApproval(decision ui.PlanApprovalDecision) {
	a.handlePlanApprovalWithFeedback(decision, "")
}

// handlePlanApprovalWithFeedback is called by the TUI when the user makes a plan approval decision with feedback.
func (a *App) handlePlanApprovalWithFeedback(decision ui.PlanApprovalDecision, feedback string) {
	// Convert UI decision to plan.ApprovalDecision
	var planDecision plan.ApprovalDecision
	switch decision {
	case ui.PlanApproved:
		planDecision = plan.ApprovalApproved
	case ui.PlanRejected:
		planDecision = plan.ApprovalRejected
		// Save the rejected plan for context
		if currentPlan := a.planManager.GetCurrentPlan(); currentPlan != nil {
			a.planManager.SaveRejectedPlan(currentPlan)
		}
	case ui.PlanModifyRequested:
		planDecision = plan.ApprovalModified
		// Save the rejected plan for context
		if currentPlan := a.planManager.GetCurrentPlan(); currentPlan != nil {
			a.planManager.SaveRejectedPlan(currentPlan)
		}
		// Handle feedback - send as a new message to the model
		if feedback != "" {
			feedbackMsg := fmt.Sprintf("Please modify the plan according to this feedback:\n\n%s", feedback)
			// Process feedback as a new message
			go a.handleSubmit(feedbackMsg)
		}
	default:
		planDecision = plan.ApprovalRejected
	}

	// Send decision to the waiting promptPlanApproval call with timeout
	select {
	case a.planApprovalChan <- planDecision:
	case <-time.After(30 * time.Second):
		logging.Warn("plan approval response channel timeout - no listener")
	}
}

// handlePlanProgressUpdate is called when plan execution progress is made.
func (a *App) handlePlanProgressUpdate(progress *plan.ProgressUpdate) {
	if a.program == nil {
		return
	}

	// Send progress update to TUI
	a.program.Send(ui.PlanProgressMsg{
		PlanID:        progress.PlanID,
		CurrentStepID: progress.CurrentStepID,
		CurrentTitle:  progress.CurrentTitle,
		TotalSteps:    progress.TotalSteps,
		Completed:     progress.Completed,
		Progress:      progress.Progress,
		Status:        progress.Status,
	})
}

// handleModelSelect is called by the TUI when the user selects a model.
func (a *App) handleModelSelect(modelID string) {
	a.client.SetModel(modelID)

	// Save model selection to config for persistence across sessions
	if a.config != nil {
		a.config.Model.Name = modelID
		// Also update provider based on model name for correct client creation on restart
		a.config.Model.Provider = config.DetectProvider(modelID)
		if err := a.config.Save(); err != nil {
			logging.Warn("failed to save model selection to config", "error", err)
		} else {
			logging.Debug("model selection saved to config", "model", modelID, "provider", a.config.Model.Provider)
		}
	}
}

// DiffDecisionTimeout is the maximum time to wait for a diff decision response.
const DiffDecisionTimeout = 5 * time.Minute

// promptDiffDecision is called by tools to request user approval for file changes.
// It sends a request to the TUI and waits for a response with timeout.
func (a *App) promptDiffDecision(ctx context.Context, filePath, oldContent, newContent, toolName string, isNewFile bool) (ui.DiffDecision, error) {
	if a.program == nil {
		return ui.DiffApply, nil
	}

	// Send diff preview request to TUI
	a.program.Send(ui.DiffPreviewRequestMsg{
		FilePath:   filePath,
		OldContent: oldContent,
		NewContent: newContent,
		ToolName:   toolName,
		IsNewFile:  isNewFile,
	})

	// Wait for response from TUI with timeout to prevent deadlock
	select {
	case decision := <-a.diffResponseChan:
		return decision, nil
	case <-ctx.Done():
		return ui.DiffReject, ctx.Err()
	case <-time.After(DiffDecisionTimeout):
		logging.Warn("diff decision prompt timed out", "file", filePath)
		return ui.DiffReject, fmt.Errorf("diff decision prompt timed out after %v", DiffDecisionTimeout)
	}
}

// handleDiffDecision is called by the TUI when the user makes a diff preview decision.
func (a *App) handleDiffDecision(decision ui.DiffDecision) {
	// Send decision to the waiting promptDiffDecision call with timeout
	select {
	case a.diffResponseChan <- decision:
	case <-time.After(30 * time.Second):
		logging.Warn("diff response channel timeout - no listener")
	}
}

// handleApplyCodeBlock handles applying a code block to a file.
func (a *App) handleApplyCodeBlock(filename, content string) {
	if filename == "" || content == "" {
		return
	}

	// Use the write tool to apply the code block
	writeTool, ok := a.registry.Get("write")
	if !ok {
		logging.Debug("write tool not found")
		return
	}

	// Execute the write tool
	go func() {
		ctx := a.ctx
		args := map[string]any{
			"file_path": filename,
			"content":   content,
		}

		result, err := writeTool.Execute(ctx, args)
		if err != nil {
			if a.program != nil {
				a.program.Send(ui.ErrorMsg(err))
			}
			return
		}

		if !result.Success {
			if a.program != nil {
				a.program.Send(ui.ErrorMsg(fmt.Errorf("%s", result.Error)))
			}
		}
	}()
}

// diffHandlerAdapter wraps App to implement tools.DiffHandler interface.
type diffHandlerAdapter struct {
	app *App
}

func (d *diffHandlerAdapter) PromptDiff(ctx context.Context, filePath, oldContent, newContent, toolName string, isNewFile bool) (bool, error) {
	decision, err := d.app.promptDiffDecision(ctx, filePath, oldContent, newContent, toolName, isNewFile)
	if err != nil {
		return false, err
	}
	return decision == ui.DiffApply, nil
}
