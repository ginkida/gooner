package commands

import (
	"context"
)

// PlanCommand toggles planning mode.
type PlanCommand struct{}

func (c *PlanCommand) Name() string {
	return "plan"
}

func (c *PlanCommand) Description() string {
	return "Toggle planning mode for complex multi-step tasks"
}

func (c *PlanCommand) Usage() string {
	return "/plan"
}

func (c *PlanCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	// Toggle planning mode
	enabled := app.TogglePlanningMode()

	if enabled {
		return "Planning mode ON — complex tasks will be broken into steps with approval\n\nTip: Press Shift+Tab to toggle quickly", nil
	}
	return "Planning mode OFF — direct execution\n\nTip: Press Shift+Tab to toggle quickly", nil
}

// GetMetadata returns command metadata for palette display.
func (c *PlanCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryPlanning,
		Icon:     "tree",
		ArgHint:  "",
		Priority: 0, // Top of planning category
	}
}
