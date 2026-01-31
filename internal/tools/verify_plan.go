package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gooner/internal/contract"
	"gooner/internal/plan"

	"google.golang.org/genai"
)

// VerifyPlanTool allows the AI to verify a plan's contract examples and invariants.
type VerifyPlanTool struct {
	planManager *plan.Manager
}

// NewVerifyPlanTool creates a new verify plan tool.
func NewVerifyPlanTool() *VerifyPlanTool {
	return &VerifyPlanTool{}
}

// SetPlanManager sets the plan manager.
func (t *VerifyPlanTool) SetPlanManager(manager *plan.Manager) {
	t.planManager = manager
}

func (t *VerifyPlanTool) Name() string { return "verify_plan" }

func (t *VerifyPlanTool) Description() string {
	return "Verify a plan's contract by running its examples and invariant checks"
}

func (t *VerifyPlanTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"plan_id": {
					Type:        genai.TypeString,
					Description: "Plan ID to verify, or 'active' for the current active plan",
				},
			},
			Required: []string{"plan_id"},
		},
	}
}

func (t *VerifyPlanTool) Validate(args map[string]any) error {
	if _, ok := GetString(args, "plan_id"); !ok {
		return NewValidationError("plan_id", "plan_id is required")
	}
	return nil
}

func (t *VerifyPlanTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.planManager == nil {
		return NewErrorResult("plan manager not configured"), nil
	}

	verifier := t.planManager.GetContractVerifier()
	if verifier == nil {
		return NewErrorResult("contract verifier not available"), nil
	}

	planID, _ := GetString(args, "plan_id")

	// Get the plan
	var p *plan.Plan
	if planID == "active" {
		p = t.planManager.GetCurrentPlan()
		if p == nil {
			return NewErrorResult("no active plan"), nil
		}
	} else {
		// Only the active plan is accessible via the manager
		p = t.planManager.GetCurrentPlan()
		if p == nil || p.ID != planID {
			return NewErrorResult(fmt.Sprintf("plan not found: %s (only active plan is available)", planID)), nil
		}
	}

	if p.Contract == nil {
		return NewErrorResult("plan has no contract to verify"), nil
	}

	// Build a full contract.Contract from plan's ContractSpec for verification
	c := &contract.Contract{
		ID:         p.ContractID,
		Name:       p.Contract.Name,
		Version:    1,
		Status:     contract.StatusActive,
		Intent:     p.Contract.Intent,
		Boundaries: p.Contract.Boundaries,
		Invariants: p.Contract.Invariants,
		Examples:   p.Contract.Examples,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	// If we have a contract ID and store, try to load the persisted contract
	store := t.planManager.GetContractStore()
	if p.ContractID != "" && store != nil {
		if loaded, err := store.Load(p.ContractID); err == nil {
			c = loaded
		}
	}

	// Run verification
	result, err := verifier.Verify(ctx, c)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("verification failed: %v", err)), nil
	}

	// Store result in persisted contract if available
	if store != nil && p.ContractID != "" {
		c.LastVerification = result
		if result.Passed {
			c.Status = contract.StatusVerified
		} else {
			c.Status = contract.StatusFailed
		}
		_ = store.Save(c)
	}

	// Format report
	return NewSuccessResult(formatVerificationReport(c, result)), nil
}

// formatVerificationReport produces a detailed verification report.
func formatVerificationReport(c *contract.Contract, result *contract.VerificationResult) string {
	var sb strings.Builder

	status := "PASSED"
	if !result.Passed {
		status = "FAILED"
	}

	sb.WriteString(fmt.Sprintf("Verification %s for contract '%s'\n\n", status, c.Name))

	if len(result.ExampleResults) > 0 {
		sb.WriteString("Examples:\n")
		for _, er := range result.ExampleResults {
			icon := "[PASS]"
			if !er.Passed {
				icon = "[FAIL]"
			}
			sb.WriteString(fmt.Sprintf("  %s %s", icon, er.Name))
			if er.Error != "" {
				sb.WriteString(fmt.Sprintf(" - %s", er.Error))
			}
			if er.Output != "" && !er.Passed {
				sb.WriteString(fmt.Sprintf("\n    Output: %s", er.Output))
			}
			sb.WriteString("\n")
		}
	}

	if len(result.InvariantResults) > 0 {
		sb.WriteString("\nInvariants:\n")
		for _, ir := range result.InvariantResults {
			icon := "[PASS]"
			if !ir.Passed {
				icon = "[FAIL]"
			}
			sb.WriteString(fmt.Sprintf("  %s %s", icon, ir.Name))
			if ir.Error != "" {
				sb.WriteString(fmt.Sprintf(" - %s", ir.Error))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\nSummary: %s\n", result.Summary))

	return sb.String()
}
