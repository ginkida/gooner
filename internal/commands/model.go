package commands

import (
	"context"
	"fmt"
	"strings"

	"gooner/internal/client"
)

// ModelCommand switches the current model.
type ModelCommand struct{}

func (c *ModelCommand) Name() string        { return "model" }
func (c *ModelCommand) Description() string { return "Switch AI model" }
func (c *ModelCommand) Usage() string {
	return `/model         - Show current model and available models
/model flash   - Switch to flash model
/model pro     - Switch to pro model`
}
func (c *ModelCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category:    CategoryModelSession,
		Icon:        "model",
		Priority:    0,
		RequiresAPI: true,
		HasArgs:     true,
		ArgHint:     "flash|pro",
	}
}

func (c *ModelCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	setter := app.GetModelSetter()
	if setter == nil {
		return "Model switching not available.", nil
	}

	currentModel := setter.GetModel()
	activeProvider := cfg.API.GetActiveProvider()

	// Get models for current provider
	providerModels := client.GetModelsForProvider(activeProvider)
	if len(providerModels) == 0 {
		return fmt.Sprintf("No models available for provider: %s", activeProvider), nil
	}

	// No args - show current model and available models for this provider
	if len(args) == 0 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Provider: %s\n", activeProvider))
		sb.WriteString(fmt.Sprintf("Model:    %s\n\n", currentModel))
		sb.WriteString("Available models:\n")

		for _, m := range providerModels {
			marker := "  "
			if m.ID == currentModel {
				marker = "> "
			}
			shortName := extractShortName(m.ID)
			sb.WriteString(fmt.Sprintf("%s%-8s %s\n", marker, shortName, m.Description))
		}

		sb.WriteString("\nUsage: /model <name>")
		if activeProvider == "gemini" {
			sb.WriteString("\nExamples: /model flash  or  /model pro")
		}
		sb.WriteString("\n\nUse /provider to switch providers")

		return sb.String(), nil
	}

	// Switch to specified model
	newModel := args[0]

	// Find matching model within current provider
	var matchedModel string
	for _, m := range providerModels {
		if m.ID == newModel {
			matchedModel = m.ID
			break
		}
		// Partial match (e.g., "flash" matches "gemini-3-flash-preview")
		if strings.Contains(m.ID, newModel) || strings.Contains(extractShortName(m.ID), newModel) {
			if matchedModel != "" {
				return fmt.Sprintf("Ambiguous model name '%s'. Please be more specific.", newModel), nil
			}
			matchedModel = m.ID
		}
	}

	if matchedModel == "" {
		return fmt.Sprintf("Unknown model: %s\n\n%s", newModel, c.formatProviderModels(providerModels)), nil
	}

	if matchedModel == currentModel {
		return fmt.Sprintf("Already using %s", currentModel), nil
	}

	// Update model in config and setter
	setter.SetModel(matchedModel)
	cfg.Model.Name = matchedModel

	if err := app.ApplyConfig(cfg); err != nil {
		return fmt.Sprintf("Failed to save: %v", err), nil
	}

	// Find model info for nice output
	var modelName string
	for _, m := range providerModels {
		if m.ID == matchedModel {
			modelName = m.Name
			break
		}
	}

	return fmt.Sprintf("Switched to %s (%s)", modelName, matchedModel), nil
}

func (c *ModelCommand) formatProviderModels(models []client.ModelInfo) string {
	var sb strings.Builder
	sb.WriteString("Available models:\n")
	for _, m := range models {
		shortName := extractShortName(m.ID)
		sb.WriteString(fmt.Sprintf("  %-8s %s\n", shortName, m.Description))
	}
	return sb.String()
}

// extractShortName extracts a short name from model ID
func extractShortName(modelID string) string {
	// GLM models
	if strings.HasPrefix(modelID, "glm") {
		return modelID // Return full ID for GLM (e.g., "glm-4.7")
	}

	// Gemini models
	if strings.Contains(modelID, "flash") {
		return "flash"
	}
	if strings.Contains(modelID, "pro") {
		return "pro"
	}

	return modelID
}
