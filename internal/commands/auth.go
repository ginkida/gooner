package commands

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"gokin/internal/auth"
	"gokin/internal/config"
)

// LoginCommand sets the API key.
type LoginCommand struct{}

func (c *LoginCommand) Name() string        { return "login" }
func (c *LoginCommand) Description() string { return "Set API key for Gemini or GLM" }
func (c *LoginCommand) Usage() string {
	return `/login                    - Show current status
/login gemini <api_key>   - Set Gemini API key
/login glm <api_key>      - Set GLM API key

Tip: Use /oauth-login for Google account authentication (uses your Gemini subscription)`
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

Get your free Gemini API key at: https://aistudio.google.com/apikey

Alternatively, use /oauth-login to sign in with your Google account
(this uses your Gemini subscription instead of API credits).`, nil
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
	if cfg.API.HasOAuthToken("gemini") {
		geminiStatus = fmt.Sprintf("OAuth (%s)", cfg.API.GeminiOAuth.Email)
	} else if cfg.API.GeminiKey != "" {
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
	sb.WriteString("  /oauth-login         - Login via Google account\n")
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
/logout deepseek  - Remove DeepSeek key
/logout ollama    - Remove Ollama key
/logout all       - Remove all keys`
}
func (c *LogoutCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "logout",
		Priority: 10,
		HasArgs:  true,
		ArgHint:  "[gemini|glm|deepseek|ollama|all]",
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
	case "deepseek":
		cfg.API.DeepSeekKey = ""
		if currentProvider == "deepseek" {
			cfg.API.APIKey = ""
		}
	case "ollama":
		cfg.API.OllamaKey = ""
		if currentProvider == "ollama" {
			cfg.API.APIKey = ""
		}
	case "all":
		cfg.API.GeminiKey = ""
		cfg.API.GLMKey = ""
		cfg.API.DeepSeekKey = ""
		cfg.API.OllamaKey = ""
		cfg.API.APIKey = ""
	default:
		return fmt.Sprintf("Unknown provider: %s\n\nUsage: /logout [gemini|glm|deepseek|ollama|all]", target), nil
	}

	// Collect available providers with keys
	availableProviders := []string{}
	providerModels := map[string]string{
		"gemini":   "gemini-3-flash-preview",
		"glm":      "glm-4.7",
		"deepseek": "deepseek-chat",
		"ollama":   "llama3.2",
	}

	if cfg.API.GeminiKey != "" {
		availableProviders = append(availableProviders, "gemini")
	}
	if cfg.API.GLMKey != "" {
		availableProviders = append(availableProviders, "glm")
	}
	if cfg.API.DeepSeekKey != "" {
		availableProviders = append(availableProviders, "deepseek")
	}
	if cfg.API.OllamaKey != "" || cfg.API.OllamaBaseURL != "" {
		availableProviders = append(availableProviders, "ollama")
	}

	// Save config directly without re-initializing client
	// (ApplyConfig would fail if no API key is available)
	if err := cfg.Save(); err != nil {
		return fmt.Sprintf("Failed to save: %v", err), nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("✓ %s API key removed.\n", strings.Title(target)))

	// Build response based on available providers
	if len(availableProviders) == 0 {
		result.WriteString("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		result.WriteString("No API keys configured.\n\n")
		result.WriteString("Choose AI provider:\n")
		result.WriteString("  /login gemini <key>   - Google Gemini\n")
		result.WriteString("  /login deepseek <key> - DeepSeek\n")
		result.WriteString("  /login glm <key>      - GLM (Zhipu AI)\n")
		result.WriteString("  /login ollama         - Ollama (local)\n")
	} else if target == currentProvider || target == "all" {
		// Active provider was removed, need to switch
		result.WriteString("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		result.WriteString("Choose AI provider:\n\n")

		for i, provider := range availableProviders {
			marker := "  "
			if i == 0 {
				marker = "→ "
			}
			result.WriteString(fmt.Sprintf("%s/provider %s\n", marker, provider))
		}

		// Auto-switch to first available
		if len(availableProviders) > 0 {
			newProvider := availableProviders[0]
			cfg.API.ActiveProvider = newProvider
			cfg.Model.Provider = newProvider
			cfg.Model.Name = providerModels[newProvider]
			cfg.Save()

			result.WriteString(fmt.Sprintf("\n✓ Auto-switched to %s\n", newProvider))
		}
	}

	return result.String(), nil
}

// ProviderCommand switches between providers.
type ProviderCommand struct{}

func (c *ProviderCommand) Name() string        { return "provider" }
func (c *ProviderCommand) Description() string { return "Switch AI provider" }
func (c *ProviderCommand) Usage() string {
	return `/provider          - Show current provider
/provider gemini   - Switch to Gemini
/provider deepseek - Switch to DeepSeek
/provider glm      - Switch to GLM
/provider ollama   - Switch to Ollama (local)`
}
func (c *ProviderCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "provider",
		Priority: 20,
		HasArgs:  true,
		ArgHint:  "[gemini|deepseek|glm|ollama]",
	}
}

func (c *ProviderCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	currentProvider := cfg.API.GetActiveProvider()

	// Provider configurations
	providerInfo := []struct {
		name        string
		model       string
		description string
	}{
		{"gemini", "gemini-3-flash-preview", "Google Gemini"},
		{"deepseek", "deepseek-chat", "DeepSeek"},
		{"glm", "glm-4.7", "GLM (Zhipu AI)"},
		{"ollama", "llama3.2", "Ollama (local)"},
	}

	providerModels := make(map[string]string)
	validProviders := make(map[string]bool)
	for _, p := range providerInfo {
		providerModels[p.name] = p.model
		validProviders[p.name] = true
	}

	// No args - show current status
	if len(args) == 0 {
		var sb strings.Builder
		sb.WriteString("AI Providers:\n\n")

		for _, p := range providerInfo {
			marker := "  "
			if p.name == currentProvider {
				marker = "→ "
			}

			status := "not configured"
			if cfg.API.HasProvider(p.name) {
				status = "✓ ready"
			}

			sb.WriteString(fmt.Sprintf("%s%-10s %-16s %s\n", marker, p.name, p.description, status))
		}

		sb.WriteString(fmt.Sprintf("\nCurrent: %s (%s)\n", currentProvider, cfg.Model.Name))
		sb.WriteString("\nUsage: /provider <name>")

		return sb.String(), nil
	}

	// Switch provider
	newProvider := strings.ToLower(args[0])

	if !validProviders[newProvider] {
		return fmt.Sprintf("Unknown provider: %s\n\nAvailable: gemini, deepseek, glm, ollama", newProvider), nil
	}

	if newProvider == currentProvider {
		return fmt.Sprintf("Already using %s", newProvider), nil
	}

	// Check if provider has a key (ollama doesn't require key for local)
	if newProvider != "ollama" && !cfg.API.HasProvider(newProvider) {
		return fmt.Sprintf("%s is not configured.\n\nUse: /login %s <api_key>", newProvider, newProvider), nil
	}

	// Switch provider
	cfg.API.ActiveProvider = newProvider
	cfg.Model.Provider = newProvider
	cfg.Model.Name = providerModels[newProvider]

	if err := app.ApplyConfig(cfg); err != nil {
		return fmt.Sprintf("Failed to save: %v", err), nil
	}

	return fmt.Sprintf("✓ Switched to %s (%s)", newProvider, cfg.Model.Name), nil
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
	if cfg.API.HasOAuthToken("gemini") {
		geminiStatus = fmt.Sprintf("OAuth (%s)", cfg.API.GeminiOAuth.Email)
	} else if cfg.API.HasProvider("gemini") {
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

// OAuthLoginCommand handles /oauth-login for Google Account authentication
type OAuthLoginCommand struct{}

func (c *OAuthLoginCommand) Name() string        { return "oauth-login" }
func (c *OAuthLoginCommand) Description() string { return "Login to Gemini using Google account (OAuth)" }
func (c *OAuthLoginCommand) Usage() string {
	return `/oauth-login - Login to Gemini using your Google account

This uses your Gemini subscription (not API credits).
A browser window will open for Google authentication.`
}
func (c *OAuthLoginCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "google",
		Priority: 5,
	}
}

func (c *OAuthLoginCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	// Check if already logged in via OAuth
	if cfg.API.HasOAuthToken("gemini") {
		return fmt.Sprintf("Already logged in via OAuth as %s.\n\nUse /oauth-logout to sign out first.", cfg.API.GeminiOAuth.Email), nil
	}

	// Create OAuth manager
	manager := auth.NewGeminiOAuthManager()

	// Generate auth URL
	authURL, err := manager.GenerateAuthURL()
	if err != nil {
		return "", fmt.Errorf("failed to generate auth URL: %w", err)
	}

	// Start callback server
	server := auth.NewCallbackServer(auth.GeminiOAuthCallbackPort, manager.GetState())
	if err := server.Start(); err != nil {
		return "", fmt.Errorf("failed to start callback server: %w", err)
	}
	defer server.Stop()

	// Try to open browser
	browserOpened := openBrowser(authURL)

	var sb strings.Builder
	sb.WriteString("Opening browser for Google authentication...\n\n")
	if !browserOpened {
		sb.WriteString("Could not open browser automatically.\n")
		sb.WriteString("Please open this URL in your browser:\n\n")
		sb.WriteString(authURL + "\n\n")
	}
	sb.WriteString("Waiting for authentication (timeout: 5 minutes)...")

	// Note: We can't actually show this message before waiting since Execute blocks.
	// The user will see the result after the flow completes.

	// Wait for callback
	code, err := server.WaitForCode(auth.OAuthCallbackTimeout)
	if err != nil {
		return "", fmt.Errorf("authentication failed: %w", err)
	}

	// Exchange code for tokens
	token, err := manager.ExchangeCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("failed to exchange code: %w", err)
	}

	// Save to config
	cfg.API.GeminiOAuth = &config.OAuthTokenConfig{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    token.ExpiresAt.Unix(),
		Email:        token.Email,
	}
	cfg.API.ActiveProvider = "gemini"
	cfg.Model.Provider = "gemini"

	if err := app.ApplyConfig(cfg); err != nil {
		return "", fmt.Errorf("failed to save config: %w", err)
	}

	email := token.Email
	if email == "" {
		email = "Google Account"
	}

	expiresIn := time.Until(token.ExpiresAt).Round(time.Minute)

	return fmt.Sprintf(`Logged in as %s via OAuth

Provider: gemini (OAuth)
Model: %s
Token expires in: %v

Use /status to check your configuration.
Use /oauth-logout to sign out.`, email, cfg.Model.Name, expiresIn), nil
}

// OAuthLogoutCommand handles /oauth-logout
type OAuthLogoutCommand struct{}

func (c *OAuthLogoutCommand) Name() string        { return "oauth-logout" }
func (c *OAuthLogoutCommand) Description() string { return "Remove OAuth credentials" }
func (c *OAuthLogoutCommand) Usage() string {
	return `/oauth-logout - Remove Google OAuth credentials

This will sign you out of your Google account for Gemini.
You can use /login gemini <key> to use an API key instead.`
}
func (c *OAuthLogoutCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthentication,
		Icon:     "logout",
		Priority: 15,
	}
}

func (c *OAuthLogoutCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Failed to get configuration.", nil
	}

	if !cfg.API.HasOAuthToken("gemini") {
		return "No OAuth credentials found.", nil
	}

	email := cfg.API.GeminiOAuth.Email

	// Remove OAuth token
	cfg.API.GeminiOAuth = nil

	// Save config
	if err := cfg.Save(); err != nil {
		return "", fmt.Errorf("failed to save config: %w", err)
	}

	var sb strings.Builder
	if email != "" {
		sb.WriteString(fmt.Sprintf("Logged out from %s\n\n", email))
	} else {
		sb.WriteString("OAuth credentials removed.\n\n")
	}

	// Check if API key is available
	if cfg.API.GeminiKey != "" {
		sb.WriteString("Falling back to API key authentication.\n")
	} else {
		sb.WriteString("No API key configured.\n")
		sb.WriteString("Use /login gemini <key> to set an API key.\n")
		sb.WriteString("Or use /oauth-login to sign in again.")
	}

	return sb.String(), nil
}

// openBrowser opens a URL in the default browser
func openBrowser(url string) bool {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		// Try xdg-open first, then common browsers
		if _, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command("xdg-open", url)
		} else if _, err := exec.LookPath("google-chrome"); err == nil {
			cmd = exec.Command("google-chrome", url)
		} else if _, err := exec.LookPath("firefox"); err == nil {
			cmd = exec.Command("firefox", url)
		} else {
			return false
		}
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return false
	}

	return cmd.Start() == nil
}
