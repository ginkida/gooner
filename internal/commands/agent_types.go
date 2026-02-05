package commands

import (
	"context"
	"fmt"
	"strings"

	"gokin/internal/agent"
)

// RegisterAgentTypeCommand registers a custom agent type.
type RegisterAgentTypeCommand struct{}

func (c *RegisterAgentTypeCommand) Name() string {
	return "register-agent-type"
}

func (c *RegisterAgentTypeCommand) Description() string {
	return "Register a custom agent type with specific tools"
}

func (c *RegisterAgentTypeCommand) Usage() string {
	return `/register-agent-type <name> "<description>" [--tools t1,t2,t3] [--prompt "text"]`
}

func (c *RegisterAgentTypeCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "robot",
		HasArgs:  true,
		ArgHint:  `<name> "<desc>" [--tools ...]`,
		Priority: 30,
	}
}

func (c *RegisterAgentTypeCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if len(args) < 2 {
		return "", fmt.Errorf("usage: %s", c.Usage())
	}

	registry := app.GetAgentTypeRegistry()
	if registry == nil {
		return "", fmt.Errorf("agent type registry not available")
	}

	// Parse arguments
	name := args[0]
	description := ""
	var tools []string
	prompt := ""

	// Parse remaining args
	i := 1
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--tools" && i+1 < len(args):
			i++
			tools = strings.Split(args[i], ",")
			for j := range tools {
				tools[j] = strings.TrimSpace(tools[j])
			}
		case arg == "--prompt" && i+1 < len(args):
			i++
			prompt = args[i]
		case description == "":
			// Remove quotes if present
			description = strings.Trim(arg, `"'`)
		}
		i++
	}

	if description == "" {
		return "", fmt.Errorf("description is required")
	}

	// Register the new agent type
	if err := registry.RegisterDynamic(name, description, tools, prompt); err != nil {
		return "", fmt.Errorf("failed to register agent type: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✓ Registered agent type: %s\n", name))
	sb.WriteString(fmt.Sprintf("  Description: %s\n", description))
	if len(tools) > 0 {
		sb.WriteString(fmt.Sprintf("  Tools: %s\n", strings.Join(tools, ", ")))
	} else {
		sb.WriteString("  Tools: (default for type)\n")
	}
	if prompt != "" {
		sb.WriteString(fmt.Sprintf("  Prompt: %s...\n", truncate(prompt, 50)))
	}

	return sb.String(), nil
}

// ListAgentTypesCommand lists all registered agent types.
type ListAgentTypesCommand struct{}

func (c *ListAgentTypesCommand) Name() string {
	return "list-agent-types"
}

func (c *ListAgentTypesCommand) Description() string {
	return "List all registered agent types (built-in and custom)"
}

func (c *ListAgentTypesCommand) Usage() string {
	return "/list-agent-types"
}

func (c *ListAgentTypesCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "list",
		Priority: 31,
	}
}

func (c *ListAgentTypesCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	registry := app.GetAgentTypeRegistry()
	if registry == nil {
		return "", fmt.Errorf("agent type registry not available")
	}

	var sb strings.Builder

	// Built-in types
	sb.WriteString("**Built-in Agent Types:**\n\n")
	builtinTypes := []struct {
		name string
		desc string
	}{
		{string(agent.AgentTypeExplore), registry.GetDescriptionForType(string(agent.AgentTypeExplore))},
		{string(agent.AgentTypeBash), registry.GetDescriptionForType(string(agent.AgentTypeBash))},
		{string(agent.AgentTypeGeneral), registry.GetDescriptionForType(string(agent.AgentTypeGeneral))},
		{string(agent.AgentTypePlan), registry.GetDescriptionForType(string(agent.AgentTypePlan))},
		{string(agent.AgentTypeGuide), registry.GetDescriptionForType(string(agent.AgentTypeGuide))},
	}

	for _, t := range builtinTypes {
		sb.WriteString(fmt.Sprintf("• **%s** — %s\n", t.name, t.desc))
	}

	// Dynamic types
	dynamicTypes := registry.ListDynamic()
	if len(dynamicTypes) > 0 {
		sb.WriteString("\n**Custom Agent Types:**\n\n")
		for _, dt := range dynamicTypes {
			sb.WriteString(fmt.Sprintf("• **%s** — %s\n", dt.Name, dt.Description))
			if len(dt.AllowedTools) > 0 {
				sb.WriteString(fmt.Sprintf("  Tools: %s\n", strings.Join(dt.AllowedTools, ", ")))
			}
		}
	} else {
		sb.WriteString("\n*No custom agent types registered.*\n")
		sb.WriteString("\nUse `/register-agent-type` to add custom types.\n")
	}

	return sb.String(), nil
}

// UnregisterAgentTypeCommand removes a custom agent type.
type UnregisterAgentTypeCommand struct{}

func (c *UnregisterAgentTypeCommand) Name() string {
	return "unregister-agent-type"
}

func (c *UnregisterAgentTypeCommand) Description() string {
	return "Remove a custom agent type"
}

func (c *UnregisterAgentTypeCommand) Usage() string {
	return "/unregister-agent-type <name>"
}

func (c *UnregisterAgentTypeCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "trash",
		HasArgs:  true,
		ArgHint:  "<name>",
		Priority: 32,
	}
}

func (c *UnregisterAgentTypeCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if len(args) < 1 {
		return "", fmt.Errorf("usage: %s", c.Usage())
	}

	registry := app.GetAgentTypeRegistry()
	if registry == nil {
		return "", fmt.Errorf("agent type registry not available")
	}

	name := args[0]

	// Check if it's a built-in type
	if registry.IsBuiltin(name) {
		return "", fmt.Errorf("cannot unregister built-in agent type: %s", name)
	}

	// Check if it exists
	if !registry.IsDynamic(name) {
		return "", fmt.Errorf("custom agent type not found: %s", name)
	}

	if err := registry.UnregisterDynamic(name); err != nil {
		return "", fmt.Errorf("failed to unregister agent type: %w", err)
	}

	return fmt.Sprintf("✓ Unregistered agent type: %s", name), nil
}

// truncate truncates a string to maxLen characters with ellipsis.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
