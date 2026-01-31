package commands

import (
	"context"
	"fmt"
	"strings"

	"gooner/internal/agent"
)

// TreeStatsCommand shows tree planner statistics.
type TreeStatsCommand struct{}

func (c *TreeStatsCommand) Name() string        { return "tree-stats" }
func (c *TreeStatsCommand) Description() string { return "Show tree planner statistics" }
func (c *TreeStatsCommand) Usage() string       { return "/tree-stats" }
func (c *TreeStatsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryPlanning,
		Icon:     "tree",
		Priority: 0,
	}
}

func (c *TreeStatsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	// Get tree planner from app
	planner := app.GetTreePlanner()
	if planner == nil {
		return "Tree planner is not available", nil
	}

	var sb strings.Builder

	// Header
	sb.WriteString("🌳 Tree Planner Statistics\n")
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n\n")

	// Get stats
	stats := planner.GetStats()

	// Mode status
	planningEnabled := app.IsPlanningModeEnabled()
	modeStatus := "Reactive (normal)"
	if planningEnabled {
		modeStatus = "Planning (tree-based)"
	}
	sb.WriteString("Mode\n")
	sb.WriteString(fmt.Sprintf("  Current Mode:    %s\n", modeStatus))
	sb.WriteString(fmt.Sprintf("  Toggle:          /plan\n\n"))

	// Configuration
	sb.WriteString("📋 Configuration\n")
	if alg, ok := stats["algorithm"]; ok {
		sb.WriteString(fmt.Sprintf("  Algorithm:       %v\n", alg))
	}
	sb.WriteString("\n")

	// Active trees
	sb.WriteString("📊 Statistics\n")
	if trees, ok := stats["active_trees"]; ok {
		sb.WriteString(fmt.Sprintf("  Active Trees:    %v\n", trees))
	}
	if nodes, ok := stats["total_nodes"]; ok {
		sb.WriteString(fmt.Sprintf("  Total Nodes:     %v\n", nodes))
	}
	if replans, ok := stats["total_replans"]; ok {
		sb.WriteString(fmt.Sprintf("  Total Replans:   %v\n", replans))
	}
	sb.WriteString("\n")

	// Tips
	sb.WriteString("Tips\n")
	sb.WriteString("  - Use /plan to enable planning mode for complex tasks\n")
	sb.WriteString("  - Planning mode builds a decision tree before execution\n")
	sb.WriteString("  - Automatic replanning on failures\n")

	return sb.String(), nil
}

// TreePlannerProvider interface for getting tree planner
type TreePlannerProvider interface {
	GetTreePlanner() *agent.TreePlanner
	IsPlanningModeEnabled() bool
}
