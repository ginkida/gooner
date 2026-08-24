package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gokin/internal/donegate"
	"gokin/internal/plan"
)

// lintPlanBeforeApproval blocks approval when plan contracts are incomplete or unsafe.
// It runs fail-fast checks before the user sees the approval prompt.
func (a *App) lintPlanBeforeApproval(ctx context.Context, p *plan.Plan) error {
	if p == nil {
		return fmt.Errorf("plan is nil")
	}

	steps := p.GetStepsSnapshot()
	if len(steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}

	profile := donegate.DetectProfile(a.workDir)
	issues := make([]string, 0)

	// a.config is swapped by ApplyConfig under a.mu, and this handler runs
	// wherever exit_plan_mode was called — including a SUB-AGENT's goroutine,
	// which is distinct from the app one. Snapshot once, outside the per-step
	// loop; the config is immutable post-publish.
	cfg := a.snapshotConfig()
	requireArtifactPaths := cfg != nil && cfg.Plan.RequireExpectedArtifactPaths

	for _, step := range steps {
		if step == nil {
			issues = append(issues, "step is nil")
			continue
		}
		title := strings.TrimSpace(step.Title)
		if title == "" {
			issues = append(issues, fmt.Sprintf("step %d: missing title", step.ID))
			continue
		}

		// Check model-provided verify commands BEFORE injecting defaults
		verifyCommands := normalizeVerifyCommands(step.VerifyCommands)
		if len(verifyCommands) == 0 {
			issues = append(issues, fmt.Sprintf("step %d (%s): missing verify_commands", step.ID, title))
		}

		// Inject defaults after lint check so the plan can still execute
		step.EnsureContractDefaults()
		verifyCommands = normalizeVerifyCommands(step.VerifyCommands)
		for _, cmd := range verifyCommands {
			if safe, reason := a.validateVerifyCommandSafety(ctx, cmd, profile); !safe {
				issues = append(issues, fmt.Sprintf("step %d (%s): unsafe verify command %q (%s)", step.ID, title, cmd, reason))
			}
		}

		if requireArtifactPaths {
			if stepLikelyMutatingIntent(step) && len(normalizeExpectedArtifactPaths(step.ExpectedArtifactPaths)) == 0 {
				issues = append(issues, fmt.Sprintf(
					"step %d (%s): missing expected_artifact_paths for mutating step (strict mode)",
					step.ID, title,
				))
			}
		}
	}

	if len(issues) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("plan lint errors:\n")
	for _, issue := range issues {
		sb.WriteString("- ")
		sb.WriteString(issue)
		sb.WriteString("\n")
	}
	return errors.New(strings.TrimSpace(sb.String()))
}

func stepLikelyMutatingIntent(step *plan.Step) bool {
	if step == nil {
		return false
	}

	texts := []string{
		strings.TrimSpace(step.Title),
		strings.TrimSpace(step.Description),
		strings.TrimSpace(step.ExpectedArtifact),
		strings.Join(step.SuccessCriteria, " "),
	}
	lower := strings.ToLower(strings.Join(texts, " "))
	if strings.TrimSpace(lower) == "" {
		return false
	}

	nonMutating := []string{
		"analy", "investigat", "inspect", "review", "explain", "research",
		"audit", "read-only", "discover", "evaluate", "plan", "документ", "объясн",
	}
	for _, marker := range nonMutating {
		if strings.Contains(lower, marker) {
			return false
		}
	}

	mutating := []string{
		"add", "implement", "fix", "refactor", "update", "change", "create",
		"write", "edit", "delete", "remove", "rename", "migrate", "patch", "modify",
		"feature", "bug", "diff", "code", "file", "добав", "исправ", "обнов", "измен",
		"удал", "рефактор", "созда", "патч",
	}
	for _, marker := range mutating {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	return false
}
