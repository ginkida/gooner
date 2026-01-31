package commands

import (
	"context"
	"fmt"
	"strings"

	appcontext "gokin/internal/context"
)

// InstructionsCommand displays the loaded project instructions.
type InstructionsCommand struct{}

// Name returns the command name.
func (c *InstructionsCommand) Name() string {
	return "instructions"
}

// Description returns the command description.
func (c *InstructionsCommand) Description() string {
	return "Show loaded project instructions (GOKIN.md)"
}

// Usage returns the command usage.
func (c *InstructionsCommand) Usage() string {
	return "/instructions [--source]"
}

// GetMetadata returns command metadata for the palette.
func (c *InstructionsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryContext,
		Icon:     "note",
		Priority: 10,
		HasArgs:  true,
		ArgHint:  "[--source]",
	}
}

// Execute displays the project instructions.
func (c *InstructionsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	showSource := false

	// Parse args
	for _, arg := range args {
		if arg == "--source" || arg == "-s" {
			showSource = true
		}
	}

	workDir := app.GetWorkDir()
	projectMemory := appcontext.NewProjectMemory(workDir)
	if err := projectMemory.Load(); err != nil {
		return "", fmt.Errorf("failed to load project instructions: %w", err)
	}

	var output strings.Builder

	if !projectMemory.HasInstructions() {
		output.WriteString("# No Project Instructions Found\n\n")
		output.WriteString("Searched for:\n")
		output.WriteString("  - GOKIN.md\n")
		output.WriteString("  - .gokin/instructions.md\n")
		output.WriteString("  - .gokin/INSTRUCTIONS.md\n")
		output.WriteString("  - .gokin.md\n\n")
		output.WriteString("Create one of these files to provide project-specific context.")
		return output.String(), nil
	}

	// Show source info if requested
	if showSource {
		source := projectMemory.GetSourcePath()
		output.WriteString(fmt.Sprintf("# Source: %s\n\n", source))
	}

	output.WriteString("# Project Instructions\n\n")
	output.WriteString(projectMemory.GetInstructions())
	output.WriteString("\n")

	return output.String(), nil
}
