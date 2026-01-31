package commands

import (
	"context"
	"fmt"
	"strings"

	"gooner/internal/config"
)

// LoginCommand sets the API key.
type LoginCommand struct{}

func (c *LoginCommand) Name() string        { return "login" }
func (c *LoginCommand) Description() string { return "Set API key for Gemini or GLM" }
func (c *LoginCommand) Usage() string {
	return `/login                    - Show current status
/login gemini <api_key>   - Set Gemini API key
/login glm <api_key>      - Set GLM API key`
}
func (c *LoginCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "key",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "gemini|glm <key>",
	}
}

func (c *LoginCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	// No args - show current status and usage
	if len(args) == 0 {
		return c.showStatus(cfg), nil
	}

	// Parse: /login <provider> <key>
	provider := strings.ToLower(args[0])

	// Validate provider
	if provider != "gemini" && provider != "glm" {
		return fmt.Sprintf(`Unknown provider: %s

Usage:
  /login gemini <api_key>   - Set Gemini API key
  /login glm <api_key>      - Set GLM API key

Get your Gemini API key at: https://aistudio.google.com/apikey`, provider), nil
	}

	// Check for API key
	if len(args) < 2 {
		if provider == "gemini" {
			return `Usage: /login gemini <api_key>

Get your free Gemini API key at: https://aistudio.google.com/apikey`, nil
		}
		return `Usage: /login glm <api_key>

Get your GLM API key from your provider.`, nil
	}

	apiKey := args[1]

	// Validate key format
	if len(apiKey) < 10 {
		return "Invalid API key format (too short).", nil
	}

	// Set the key for the provider
	cfg.API.SetProviderKey(provider, apiKey)

	// Set as active provider
	cfg.API.ActiveProvider = provider

	// Set default model for the provider
	if provider == "glm" {
		cfg.Model.Provider = "glm"
		cfg.Model.Name = "glm-4.7"
	} else {
		cfg.Model.Provider = "gemini"
		cfg.Model.Name = "gemini-3-flash-preview"
	}

	// Save config
	if err := app.ApplyConfig(cfg); err != nil {
		return fmt.Sprintf("Failed to save: %v", err), nil
	}

	providerName := "Gemini"
	if provider == "glm" {
		providerName = "GLM"
	}

	return fmt.Sprintf(`%s API key saved!

Active provider: %s
Model: %s

Use /provider to switch providers
Use /model to switch models`, providerName, providerName, cfg.Model.Name), nil
}

func (c *LoginCommand) showStatus(cfg *config.Config) string {
	var sb strings.Builder

	sb.WriteString("API Key Status:\n\n")

	// Gemini status
	geminiStatus := "not configured"
	if cfg.API.GeminiKey != "" {
		geminiStatus = "configured " + maskKey(cfg.API.GeminiKey)
	} else if cfg.API.APIKey != "" && cfg.API.GetActiveProvider() == "gemini" {
		geminiStatus = "configured " + maskKey(cfg.API.APIKey)
	}

	// GLM status
	glmStatus := "not configured"
	if cfg.API.GLMKey != "" {
		glmStatus = "configured " + maskKey(cfg.API.GLMKey)
	} else if cfg.API.APIKey != "" && cfg.API.GetActiveProvider() == "glm" {
		glmStatus = "configured " + maskKey(cfg.API.APIKey)
	}

	activeProvider := cfg.API.GetActiveProvider()

	geminiMarker := "  "
	glmMarker := "  "
	if activeProvider == "gemini" {
		geminiMarker = "> "
	} else if activeProvider == "glm" {
		glmMarker = "> "
	}

	sb.WriteString(fmt.Sprintf("%sGemini: %s\n", geminiMarker, geminiStatus))
	sb.WriteString(fmt.Sprintf("%sGLM:    %s\n", glmMarker, glmStatus))

	sb.WriteString(fmt.Sprintf("\nActive: %s\n", activeProvider))
	sb.WriteString(fmt.Sprintf("Model:  %s\n", cfg.Model.Name))

	sb.WriteString("\nCommands:\n")
	sb.WriteString("  /login gemini <key>  - Set Gemini API key\n")
	sb.WriteString("  /login glm <key>     - Set GLM API key\n")
	sb.WriteString("  /provider            - Switch provider\n")
	sb.WriteString("  /model               - Switch model\n")

	return sb.String()
}

// maskKey masks an API key for display (shows first 4 and last 4 chars).
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// LogoutCommand removes saved credentials.
type LogoutCommand struct{}

func (c *LogoutCommand) Name() string { return "logout" }
func (c *LogoutCommand) Description() string {
	return "Remove API key"
}
func (c *LogoutCommand) Usage() string {
	return `/logout           - Remove active provider key
/logout gemini    - Remove Gemini key
/logout glm       - Remove GLM key
/logout all       - Remove all keys`
}
func (c *LogoutCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "logout",
		Priority: 10,
		HasArgs:  true,
		ArgHint:  "[gemini|glm|all]",
	}
}

func (c *LogoutCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	target := ""
	if len(args) > 0 {
		target = strings.ToLower(args[0])
	}

	if target == "" {
		// Remove active provider key
		target = cfg.API.GetActiveProvider()
	}

	currentProvider := cfg.API.GetActiveProvider()

	switch target {
	case "gemini":
		cfg.API.GeminiKey = ""
		if currentProvider == "gemini" {
			cfg.API.APIKey = ""
		}
	case "glm":
		cfg.API.GLMKey = ""
		if currentProvider == "glm" {
			cfg.API.APIKey = ""
		}
	case "all":
		cfg.API.GeminiKey = ""
		cfg.API.GLMKey = ""
		cfg.API.APIKey = ""
	default:
		return fmt.Sprintf("Unknown provider: %s\n\nUsage: /logout [gemini|glm|all]", target), nil
	}

	// If we removed the active provider's key, try to switch to another provider
	if target == currentProvider || target == "all" {
		// Check if another provider has a key
		if target != "all" {
			if currentProvider == "gemini" && cfg.API.GLMKey != "" {
				cfg.API.ActiveProvider = "glm"
				cfg.Model.Provider = "glm"
				cfg.Model.Name = "glm-4.7"
			} else if currentProvider == "glm" && cfg.API.GeminiKey != "" {
				cfg.API.ActiveProvider = "gemini"
				cfg.Model.Provider = "gemini"
				cfg.Model.Name = "gemini-3-flash-preview"
			}
		}
	}

	// Save config directly without re-initializing client
	// (ApplyConfig would fail if no API key is available)
	if err := cfg.Save(); err != nil {
		return fmt.Sprintf("Failed to save: %v", err), nil
	}

	var result string
	if target == "all" {
		result = "All API keys removed.\n\nUse /login to add a new API key."
	} else {
		result = fmt.Sprintf("%s API key removed.", strings.Title(target))
		// Check if we switched providers
		newProvider := cfg.API.GetActiveProvider()
		if newProvider != currentProvider && cfg.API.HasProvider(newProvider) {
			result += fmt.Sprintf("\nSwitched to %s.", newProvider)
		} else if !cfg.API.HasProvider(newProvider) {
			result += "\n\nNo API keys configured. Use /login to add a new API key."
		}
	}

	return result, nil
}

// ProviderCommand switches between providers.
type ProviderCommand struct{}

func (c *ProviderCommand) Name() string        { return "provider" }
func (c *ProviderCommand) Description() string { return "Switch AI provider" }
func (c *ProviderCommand) Usage() string {
	return `/provider         - Show current provider
/provider gemini  - Switch to Gemini
/provider glm     - Switch to GLM`
}
func (c *ProviderCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "provider",
		Priority: 20,
		HasArgs:  true,
		ArgHint:  "[gemini|glm]",
	}
}

func (c *ProviderCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	currentProvider := cfg.API.GetActiveProvider()

	// No args - show current status
	if len(args) == 0 {
		var sb strings.Builder
		sb.WriteString("Providers:\n\n")

		providers := []struct {
			name  string
			model string
		}{
			{"gemini", "gemini-3-flash-preview"},
			{"glm", "glm-4.7"},
		}

		for _, p := range providers {
			marker := "  "
			if p.name == currentProvider {
				marker = "> "
			}

			status := "not configured"
			if cfg.API.HasProvider(p.name) {
				status = "ready"
			}

			sb.WriteString(fmt.Sprintf("%s%-8s %s\n", marker, p.name, status))
		}

		sb.WriteString(fmt.Sprintf("\nCurrent: %s (%s)\n", currentProvider, cfg.Model.Name))
		sb.WriteString("\nUsage: /provider gemini  or  /provider glm")

		return sb.String(), nil
	}

	// Switch provider
	newProvider := strings.ToLower(args[0])

	if newProvider != "gemini" && newProvider != "glm" {
		return fmt.Sprintf("Unknown provider: %s\n\nAvailable: gemini, glm", newProvider), nil
	}

	if newProvider == currentProvider {
		return fmt.Sprintf("Already using %s", newProvider), nil
	}

	// Check if provider has a key
	if !cfg.API.HasProvider(newProvider) {
		return fmt.Sprintf("%s is not configured.\n\nUse: /login %s <api_key>", newProvider, newProvider), nil
	}

	// Switch provider
	cfg.API.ActiveProvider = newProvider

	// Set default model for new provider
	if newProvider == "glm" {
		cfg.Model.Provider = "glm"
		cfg.Model.Name = "glm-4.7"
	} else {
		cfg.Model.Provider = "gemini"
		cfg.Model.Name = "gemini-3-flash-preview"
	}

	if err := app.ApplyConfig(cfg); err != nil {
		return fmt.Sprintf("Failed to save: %v", err), nil
	}

	return fmt.Sprintf("Switched to %s (%s)", newProvider, cfg.Model.Name), nil
}

// StatusCommand shows current configuration status.
type StatusCommand struct{}

func (c *StatusCommand) Name() string        { return "status" }
func (c *StatusCommand) Description() string { return "Show configuration status" }
func (c *StatusCommand) Usage() string       { return "/status" }
func (c *StatusCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "status",
		Priority: 30,
	}
}

func (c *StatusCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	var sb strings.Builder

	sb.WriteString("Configuration Status\n")
	sb.WriteString("====================\n\n")

	// Provider & Model
	provider := cfg.API.GetActiveProvider()
	sb.WriteString(fmt.Sprintf("Provider: %s\n", provider))
	sb.WriteString(fmt.Sprintf("Model:    %s\n\n", cfg.Model.Name))

	// API Keys
	sb.WriteString("API Keys:\n")

	geminiStatus := "not set"
	if cfg.API.HasProvider("gemini") {
		key := cfg.API.GeminiKey
		if key == "" {
			key = cfg.API.APIKey
		}
		geminiStatus = maskKey(key)
	}

	glmStatus := "not set"
	if cfg.API.HasProvider("glm") {
		key := cfg.API.GLMKey
		if key == "" {
			key = cfg.API.APIKey
		}
		glmStatus = maskKey(key)
	}

	sb.WriteString(fmt.Sprintf("  Gemini: %s\n", geminiStatus))
	sb.WriteString(fmt.Sprintf("  GLM:    %s\n", glmStatus))

	// Config path
	sb.WriteString(fmt.Sprintf("\nConfig: %s\n", config.GetConfigPath()))

	return sb.String(), nil
}
