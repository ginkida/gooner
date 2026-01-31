package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gooner/internal/contract"
	"gooner/internal/plan"
)

// ContractCommand handles /contract subcommands.
type ContractCommand struct{}

func (c *ContractCommand) Name() string        { return "contract" }
func (c *ContractCommand) Description() string { return "Manage contracts (new, list, show, approve, verify, evolve, lessons)" }
func (c *ContractCommand) Usage() string {
	return "/contract <subcommand> [args]\n" +
		"  new [name]       - Create a new draft contract\n" +
		"  list             - List all contracts\n" +
		"  show <id|name>   - Display full contract details\n" +
		"  approve <id|name> - Approve a draft contract\n" +
		"  verify <id|name>  - Run verification against contract\n" +
		"  evolve <id|name>  - Create evolved version\n" +
		"  lessons          - Show accumulated lessons"
}
func (c *ContractCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryContracts,
		Icon:     "contract",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "new|list|show|...",
	}
}

func (c *ContractCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	pm := app.GetPlanManager()
	if pm == nil {
		return "Plan manager is not available.", nil
	}

	store := pm.GetContractStore()
	if store == nil {
		return "Contract system is not enabled. Set contract.enabled: true in config.", nil
	}

	if len(args) == 0 {
		return c.Usage(), nil
	}

	subcommand := args[0]
	subArgs := args[1:]

	switch subcommand {
	case "new":
		return c.handleNew(subArgs, store)
	case "list":
		return c.handleList(store)
	case "show":
		return c.handleShow(subArgs, store)
	case "approve":
		return c.handleApprove(subArgs, store)
	case "verify":
		return c.handleVerify(ctx, subArgs, app, pm, store)
	case "evolve":
		return c.handleEvolve(subArgs, store)
	case "lessons":
		return c.handleLessons(store)
	default:
		return fmt.Sprintf("Unknown subcommand: %s\n\n%s", subcommand, c.Usage()), nil
	}
}

func (c *ContractCommand) handleNew(args []string, store *contract.Store) (string, error) {
	name := "new-contract"
	if len(args) > 0 {
		name = strings.Join(args, "-")
	}

	ct := &contract.Contract{
		ID:        fmt.Sprintf("contract_%d", time.Now().UnixNano()),
		Name:      name,
		Version:   1,
		Status:    contract.StatusDraft,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := store.Save(ct); err != nil {
		return fmt.Sprintf("Failed to create contract: %v", err), nil
	}

	return fmt.Sprintf("%sContract created:%s %s\n"+
		"%sID:%s %s\n"+
		"%sStatus:%s %s\n\n"+
		"Fill in the contract details through chat, then use /contract approve %s",
		colorGreen, colorReset, ct.Name,
		colorCyan, colorReset, ct.ID,
		colorYellow, colorReset, ct.Status.String(),
		ct.ID), nil
}

func (c *ContractCommand) handleList(store *contract.Store) (string, error) {
	contracts, err := store.List()
	if err != nil {
		return fmt.Sprintf("Failed to list contracts: %v", err), nil
	}

	if len(contracts) == 0 {
		return "No contracts found. Use /contract new [name] to create one.", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%sContracts (%d):%s\n\n", colorBold, len(contracts), colorReset))

	// Status icons
	statusIcon := map[string]string{
		"draft":            "○",
		"pending_approval": "◐",
		"approved":         "◑",
		"active":           "●",
		"verified":         "✓",
		"failed":           "✗",
		"evolved":          "↑",
		"archived":         "□",
	}

	for _, ct := range contracts {
		icon := statusIcon[ct.Status.String()]
		if icon == "" {
			icon = "?"
		}

		sb.WriteString(fmt.Sprintf("  %s %s%s%s (v%d) [%s%s%s]\n",
			icon,
			colorBold, ct.Name, colorReset,
			ct.Version,
			colorYellow, ct.Status.String(), colorReset))

		if ct.Intent != "" {
			intent := ct.Intent
			if len(intent) > 60 {
				intent = intent[:57] + "..."
			}
			sb.WriteString(fmt.Sprintf("    %s%s%s\n", colorCyan, intent, colorReset))
		}
	}

	return sb.String(), nil
}

func (c *ContractCommand) handleShow(args []string, store *contract.Store) (string, error) {
	if len(args) == 0 {
		return "Usage: /contract show <id|name>", nil
	}

	ct := c.findContract(args[0], store)
	if ct == nil {
		return fmt.Sprintf("Contract not found: %s", args[0]), nil
	}
	return ct.FormatForDisplay(), nil
}

func (c *ContractCommand) handleApprove(args []string, store *contract.Store) (string, error) {
	if len(args) == 0 {
		return "Usage: /contract approve <id|name>", nil
	}

	ct := c.findContract(args[0], store)
	if ct == nil {
		return fmt.Sprintf("Contract not found: %s", args[0]), nil
	}

	ct.Status = contract.StatusApproved
	ct.ApprovedAt = time.Now()
	ct.UpdatedAt = time.Now()

	if err := store.Save(ct); err != nil {
		return fmt.Sprintf("Failed to approve contract: %v", err), nil
	}

	return fmt.Sprintf("%sContract approved:%s %s\n"+
		"The contract is now available for plan integration.",
		colorGreen, colorReset, ct.Name), nil
}

func (c *ContractCommand) handleVerify(ctx context.Context, args []string, app AppInterface, pm *plan.Manager, store *contract.Store) (string, error) {
	var ct *contract.Contract
	if len(args) == 0 {
		// Try to get contract from active plan
		if currentPlan := pm.GetCurrentPlan(); currentPlan != nil && currentPlan.Contract != nil {
			ct = store.FindByName(currentPlan.Contract.Name)
		}
		if ct == nil {
			return "No active contract. Specify an ID or name: /contract verify <id|name>", nil
		}
	} else {
		ct = c.findContract(args[0], store)
		if ct == nil {
			return fmt.Sprintf("Contract not found: %s", args[0]), nil
		}
	}

	verifier := pm.GetContractVerifier()
	if verifier == nil {
		cfg := app.GetConfig()
		timeout := cfg.Contract.VerifyTimeout
		if timeout == 0 {
			timeout = 2 * 60 * 1000000000 // 2 minutes in nanoseconds
		}
		verifier = contract.NewVerifier(app.GetWorkDir(), timeout)
	}

	result, err := verifier.Verify(ctx, ct)
	if err != nil {
		return fmt.Sprintf("Verification failed: %v", err), nil
	}

	ct.LastVerification = result
	_ = store.Save(ct)

	// Format result
	var sb strings.Builder
	statusColor := colorGreen
	statusText := "PASSED"
	if !result.Passed {
		statusColor = colorRed
		statusText = "FAILED"
	}

	sb.WriteString(fmt.Sprintf("%sVerification %s%s for %s\n\n", statusColor, statusText, colorReset, ct.Name))

	if len(result.ExampleResults) > 0 {
		sb.WriteString(fmt.Sprintf("%sExamples:%s\n", colorCyan, colorReset))
		for _, er := range result.ExampleResults {
			icon := "✓"
			color := colorGreen
			if !er.Passed {
				icon = "✗"
				color = colorRed
			}
			sb.WriteString(fmt.Sprintf("  %s%s%s %s", color, icon, colorReset, er.Name))
			if er.Error != "" {
				sb.WriteString(fmt.Sprintf(" - %s%s%s", colorRed, er.Error, colorReset))
			}
			sb.WriteString("\n")
		}
	}

	if len(result.InvariantResults) > 0 {
		sb.WriteString(fmt.Sprintf("\n%sInvariants:%s\n", colorCyan, colorReset))
		for _, ir := range result.InvariantResults {
			icon := "✓"
			color := colorGreen
			if !ir.Passed {
				icon = "✗"
				color = colorRed
			}
			sb.WriteString(fmt.Sprintf("  %s%s%s %s", color, icon, colorReset, ir.Name))
			if ir.Error != "" {
				sb.WriteString(fmt.Sprintf(" - %s%s%s", colorRed, ir.Error, colorReset))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n%s%s%s\n", colorYellow, result.Summary, colorReset))

	return sb.String(), nil
}

func (c *ContractCommand) handleEvolve(args []string, store *contract.Store) (string, error) {
	if len(args) == 0 {
		return "Usage: /contract evolve <id|name>", nil
	}

	ct := c.findContract(args[0], store)
	if ct == nil {
		return fmt.Sprintf("Contract not found: %s", args[0]), nil
	}

	// Create evolved version
	evolved := &contract.Contract{
		ID:         fmt.Sprintf("contract_%d", time.Now().UnixNano()),
		Name:       ct.Name,
		Version:    ct.Version + 1,
		Status:     contract.StatusDraft,
		Intent:     ct.Intent,
		Boundaries: ct.Boundaries,
		Invariants: ct.Invariants,
		Examples:   ct.Examples,
		ParentID:   ct.ID,
		Lessons:    ct.Lessons,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	ct.Status = contract.StatusEvolved
	ct.ChildIDs = append(ct.ChildIDs, evolved.ID)
	ct.UpdatedAt = time.Now()

	_ = store.Save(ct)
	if err := store.Save(evolved); err != nil {
		return fmt.Sprintf("Failed to evolve contract: %v", err), nil
	}

	return fmt.Sprintf("%sContract evolved:%s %s -> %s (v%d)\n"+
		"Original marked as evolved. Edit the new version and approve it.",
		colorGreen, colorReset, ct.Name, evolved.ID, evolved.Version), nil
}

func (c *ContractCommand) handleLessons(store *contract.Store) (string, error) {
	contracts, err := store.List()
	if err != nil {
		return fmt.Sprintf("Failed to list contracts: %v", err), nil
	}

	var lessons []*contract.Lesson
	for _, ct := range contracts {
		for i := range ct.Lessons {
			lessons = append(lessons, &ct.Lessons[i])
		}
	}

	return contract.FormatLessons(lessons), nil
}

// findContract looks up a contract by ID or name.
func (c *ContractCommand) findContract(idOrName string, store *contract.Store) *contract.Contract {
	// Try by ID first
	ct, err := store.Load(idOrName)
	if err == nil {
		return ct
	}

	// Try by name
	return store.FindByName(idOrName)
}
