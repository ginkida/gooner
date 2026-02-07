package setup

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gokin/internal/auth"

	"github.com/ollama/ollama/api"
)

// ANSI color codes for enhanced output
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

const (
	welcomeMessage = `
%s╔═══════════════════════════════════════════════════════════════╗
║                                                               ║
║                    %sWelcome to Gokin!%s                        ║
║   AI coding assistant: Gemini, GLM, DeepSeek & Ollama   ║
║                                                               ║
╚═══════════════════════════════════════════════════════════════╝%s

Gokin helps you work with code:
  • Read, create, and edit files
  • Execute terminal commands
  • Search your project (glob, grep, tree)
  • Manage Git (commit, log, diff)
  • And much more!

Choose your AI provider to get started.
`

	authChoiceMessage = `
%sChoose AI provider:%s

  %s[1]%s Gemini (Google Account)
                       • Use your Gemini subscription
                       • Login with Google Account (OAuth)
                       • No API key needed

  %s[2]%s Gemini (API Key) • Google's Gemini models
                       • Free tier available
                       • Get key at: https://aistudio.google.com/apikey

  %s[3]%s GLM (Cloud)      • GLM-4 models
                       • Budget-friendly (~$3/month)
                       • Get key from your GLM provider

  %s[4]%s DeepSeek (Cloud) • DeepSeek Chat & Reasoner models
                       • Powerful coding assistant
                       • Get key at: https://platform.deepseek.com/api_keys

  %s[5]%s Ollama (Local)   • Run LLMs locally, no API key needed
                       • Privacy-focused, works offline
                       • Requires: ollama serve

%sEnter your choice (1-5):%s `
)

// Spinner animation frames
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// detectEnvAPIKeys checks for API key environment variables and returns the first found.
// Returns (envVarName, backend, apiKey) or empty strings if none found.
func detectEnvAPIKeys() (string, string, string) {
	envKeys := []struct {
		envVar  string
		backend string
	}{
		{"GOKIN_API_KEY", "gemini"},
		{"GEMINI_API_KEY", "gemini"},
		{"ANTHROPIC_API_KEY", "anthropic"},
		{"DEEPSEEK_API_KEY", "deepseek"},
	}

	for _, ek := range envKeys {
		if key := os.Getenv(ek.envVar); key != "" {
			return ek.envVar, ek.backend, key
		}
	}
	return "", "", ""
}

// RunSetupWizard runs the enhanced first-time setup wizard.
func RunSetupWizard() error {
	// Print colorful welcome message
	fmt.Printf(welcomeMessage, colorCyan, colorBold, colorCyan, colorReset)

	reader := bufio.NewReader(os.Stdin)

	// Check for existing env var API keys
	if envVar, backend, apiKey := detectEnvAPIKeys(); envVar != "" {
		fmt.Printf("\n%s✓ Found %s in environment.%s\n", colorGreen, envVar, colorReset)
		fmt.Printf("%sUse it for setup? [Y/n]:%s ", colorCyan, colorReset)

		answer, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("error reading input: %w", err)
		}

		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer == "" || answer == "y" || answer == "yes" {
			// Validate the key
			done := make(chan bool)
			var validationErr error
			go func() {
				validationErr = validateAPIKeyReal(backend, apiKey)
				done <- true
			}()
			spin("Validating API key...", done)

			if validationErr != nil {
				fmt.Printf("\n%s⚠ Key validation failed: %s%s\n", colorRed, validationErr, colorReset)
				fmt.Printf("%sContinuing with manual setup...%s\n\n", colorYellow, colorReset)
			} else {
				// Save to config
				configPath, err := getConfigPath()
				if err != nil {
					return fmt.Errorf("failed to get config path: %w", err)
				}

				if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
					return fmt.Errorf("failed to create directory: %w", err)
				}

				defaultModel := "gemini-3-flash-preview"
				switch backend {
				case "deepseek":
					defaultModel = "deepseek-chat"
				case "anthropic":
					defaultModel = "claude-sonnet-4-5-20250929"
				}

				content := fmt.Sprintf("api:\n  api_key: %s\n  backend: %s\nmodel:\n  provider: %s\n  name: %s\n", apiKey, backend, backend, defaultModel)
				if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
					return fmt.Errorf("failed to save config: %w", err)
				}

				fmt.Printf("\n%s✓ Configured with %s!%s\n", colorGreen, envVar, colorReset)
				fmt.Printf("  %sConfig:%s %s\n", colorYellow, colorReset, configPath)
				showNextSteps()
				return nil
			}
		}
	}

	for {
		fmt.Printf(authChoiceMessage, colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorCyan, colorReset)

		choice, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("error reading input: %w", err)
		}

		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			return setupGeminiOAuth()
		case "2":
			return setupAPIKey(reader, "gemini")
		case "3":
			return setupAPIKey(reader, "glm")
		case "4":
			return setupAPIKey(reader, "deepseek")
		case "5":
			return setupOllama(reader)
		default:
			fmt.Printf("\n%s⚠ Invalid choice. Please enter 1, 2, 3, 4, or 5.%s\n", colorRed, colorReset)
		}
	}
}

func setupAPIKey(reader *bufio.Reader, backend string) error {
	keyType := "Gemini"
	keyURL := "https://aistudio.google.com/apikey"
	switch backend {
	case "glm":
		keyType = "GLM"
		keyURL = "your GLM provider"
	case "deepseek":
		keyType = "DeepSeek"
		keyURL = "https://platform.deepseek.com/api_keys"
	}

	fmt.Printf("\n%s─── %s API Key Setup ───%s\n", colorCyan, keyType, colorReset)
	fmt.Printf("\n%sGet your key at:%s\n", colorYellow, colorReset)
	fmt.Printf("  %s%s%s\n\n", colorBold, keyURL, colorReset)
	fmt.Printf("%sEnter API key:%s ", colorGreen, colorReset)

	apiKey, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	apiKey = strings.TrimSpace(apiKey)

	if len(apiKey) < 10 {
		return fmt.Errorf("invalid API key format (too short)")
	}

	// Validate API key with a real API call
	done := make(chan bool)
	var validationErr error
	go func() {
		validationErr = validateAPIKeyReal(backend, apiKey)
		done <- true
	}()
	spin("Validating API key...", done)

	if validationErr != nil {
		return fmt.Errorf("API key validation failed: %w", validationErr)
	}

	// Save to config
	configPath, err := getConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Set default model based on backend
	defaultModel := "gemini-3-flash-preview"
	switch backend {
	case "glm":
		defaultModel = "glm-4.7"
	case "deepseek":
		defaultModel = "deepseek-chat"
	}

	content := fmt.Sprintf("api:\n  api_key: %s\n  backend: %s\nmodel:\n  provider: %s\n  name: %s\n", apiKey, backend, backend, defaultModel)
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n%s✓ %s API key saved!%s\n", colorGreen, keyType, colorReset)
	fmt.Printf("  %sConfig:%s %s\n", colorYellow, colorReset, configPath)

	// Show next steps
	showNextSteps()

	return nil
}

func setupGeminiOAuth() error {
	fmt.Printf("\n%s─── Gemini OAuth Setup ───%s\n", colorCyan, colorReset)
	fmt.Printf("\n%sThis will open your browser for Google authentication.%s\n", colorYellow, colorReset)
	fmt.Printf("%sYou'll use your Gemini subscription (not API credits).%s\n\n", colorYellow, colorReset)

	// Create OAuth manager
	manager := auth.NewGeminiOAuthManager()

	// Generate auth URL
	authURL, err := manager.GenerateAuthURL()
	if err != nil {
		return fmt.Errorf("failed to generate auth URL: %w", err)
	}

	// Start callback server
	server := auth.NewCallbackServer(auth.GeminiOAuthCallbackPort, manager.GetState())
	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start callback server: %w", err)
	}
	defer server.Stop()

	// Try to open browser
	browserOpened := openBrowserForOAuth(authURL)

	if browserOpened {
		fmt.Printf("%sOpening browser for authentication...%s\n", colorGreen, colorReset)
	} else {
		fmt.Printf("%sCould not open browser automatically.%s\n", colorYellow, colorReset)
		fmt.Printf("%sPlease open this URL in your browser:%s\n\n", colorYellow, colorReset)
		fmt.Printf("  %s%s%s\n\n", colorBold, authURL, colorReset)
	}

	fmt.Printf("%sWaiting for authentication (timeout: 5 minutes)...%s\n", colorYellow, colorReset)

	// Wait for callback with spinner
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	go func() {
		code, err := server.WaitForCode(auth.OAuthCallbackTimeout)
		if err != nil {
			errChan <- err
		} else {
			codeChan <- code
		}
	}()

	// Show spinner while waiting
	var code string
	select {
	case code = <-codeChan:
		// Got code
	case err := <-errChan:
		return fmt.Errorf("authentication failed: %w", err)
	}

	fmt.Printf("\n%s✓ Authentication successful!%s\n", colorGreen, colorReset)

	// Exchange code for tokens
	done := make(chan bool)
	go func() {
		time.Sleep(500 * time.Millisecond)
		done <- true
	}()
	spin("Exchanging authorization code...", done)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token, err := manager.ExchangeCode(ctx, code)
	if err != nil {
		return fmt.Errorf("failed to exchange code: %w", err)
	}

	// Save to config
	configPath, err := getConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Build config with OAuth
	// Note: Code Assist API supports: gemini-2.5-flash, gemini-2.5-pro, gemini-3-flash-preview, gemini-3-pro-preview
	content := fmt.Sprintf(`api:
  active_provider: gemini
  gemini_oauth:
    access_token: %s
    refresh_token: %s
    expires_at: %d
    email: %s
model:
  provider: gemini
  name: gemini-2.5-flash
`, token.AccessToken, token.RefreshToken, token.ExpiresAt.Unix(), token.Email)

	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	email := token.Email
	if email == "" {
		email = "Google Account"
	}

	fmt.Printf("\n%s✓ Logged in as %s via OAuth!%s\n", colorGreen, email, colorReset)
	fmt.Printf("  %sConfig:%s %s\n", colorYellow, colorReset, configPath)
	fmt.Printf("  %sModel:%s gemini-2.5-flash\n", colorYellow, colorReset)

	// Show next steps
	showNextSteps()

	return nil
}

// openBrowserForOAuth opens a URL in the default browser
func openBrowserForOAuth(url string) bool {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
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

func setupOllama(reader *bufio.Reader) error {
	fmt.Printf("\n%s─── Ollama Setup ───%s\n", colorCyan, colorReset)
	fmt.Printf(`
%sChoose Ollama mode:%s

  %s[1]%s Local        • Run on your machine (requires GPU)
                   • Free, private, works offline

  %s[2]%s Cloud        • Run on Ollama Cloud (no GPU needed)
                   • Requires API key from ollama.com

%sEnter your choice (1 or 2):%s `, colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorCyan, colorReset)

	choice, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	choice = strings.TrimSpace(choice)

	switch choice {
	case "1":
		return setupOllamaLocal(reader)
	case "2":
		return setupOllamaCloud(reader)
	default:
		fmt.Printf("\n%s⚠ Invalid choice, defaulting to Local.%s\n", colorYellow, colorReset)
		return setupOllamaLocal(reader)
	}
}

func setupOllamaLocal(reader *bufio.Reader) error {
	fmt.Printf("\n%s─── Ollama Local Setup ───%s\n", colorCyan, colorReset)
	fmt.Printf("\n%sPrerequisites:%s\n", colorYellow, colorReset)
	fmt.Printf("  1. Install Ollama: %scurl -fsSL https://ollama.ai/install.sh | sh%s\n", colorBold, colorReset)
	fmt.Printf("  2. Start server:   %sollama serve%s\n", colorBold, colorReset)
	fmt.Printf("  3. Pull a model:   %sollama pull llama3.2%s\n\n", colorBold, colorReset)

	// Check for installed models
	fmt.Printf("%sChecking installed models...%s\n", colorYellow, colorReset)

	models, err := detectInstalledOllamaModels("")
	if err != nil {
		fmt.Printf("  %s⚠ Could not connect to Ollama: %s%s\n", colorRed, err, colorReset)
		fmt.Printf("  %sMake sure Ollama is running: ollama serve%s\n\n", colorYellow, colorReset)
	} else if len(models) > 0 {
		fmt.Printf("  %s✓ Found %d installed model(s):%s\n", colorGreen, len(models), colorReset)
		for i, m := range models {
			if i < 5 { // Show first 5
				fmt.Printf("    • %s\n", m)
			}
		}
		if len(models) > 5 {
			fmt.Printf("    • ... and %d more\n", len(models)-5)
		}
		fmt.Println()
	} else {
		fmt.Printf("  %s⚠ No models installed. Run: ollama pull llama3.2%s\n\n", colorYellow, colorReset)
	}

	fmt.Printf("%sEnter model name (or press Enter for 'llama3.2'):%s ", colorGreen, colorReset)

	modelName, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "llama3.2"
	}

	// Ask for remote URL (optional)
	fmt.Printf("\n%sOllama server URL (press Enter for local 'http://localhost:11434'):%s ", colorGreen, colorReset)

	serverURL, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	serverURL = strings.TrimSpace(serverURL)

	// Save to config
	configPath, err := getConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	content := "api:\n  backend: ollama\n"
	if serverURL != "" {
		content += fmt.Sprintf("  ollama_base_url: %s\n", serverURL)
	}
	content += fmt.Sprintf("model:\n  provider: ollama\n  name: %s\n", modelName)

	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n%s✓ Ollama Local configured!%s\n", colorGreen, colorReset)
	fmt.Printf("  %sConfig:%s %s\n", colorYellow, colorReset, configPath)
	fmt.Printf("  %sModel:%s %s\n", colorYellow, colorReset, modelName)
	if serverURL != "" {
		fmt.Printf("  %sServer:%s %s\n", colorYellow, colorReset, serverURL)
	}

	showOllamaLocalNextSteps(modelName)

	return nil
}

func setupOllamaCloud(reader *bufio.Reader) error {
	fmt.Printf("\n%s─── Ollama Cloud Setup ───%s\n", colorCyan, colorReset)
	fmt.Printf("\n%sOllama Cloud runs models on remote servers — no local GPU needed.%s\n", colorYellow, colorReset)
	fmt.Printf("\n%sGet your API key:%s\n", colorYellow, colorReset)
	fmt.Printf("  1. Sign in: %sollama signin%s\n", colorBold, colorReset)
	fmt.Printf("  2. Or get key at: %shttps://ollama.com/settings/keys%s\n\n", colorBold, colorReset)

	fmt.Printf("%sEnter Ollama API key:%s ", colorGreen, colorReset)

	apiKey, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	apiKey = strings.TrimSpace(apiKey)
	if len(apiKey) < 10 {
		return fmt.Errorf("invalid API key format (too short)")
	}

	fmt.Printf("\n%sEnter model name (or press Enter for 'llama3.2'):%s ", colorGreen, colorReset)

	modelName, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "llama3.2"
	}

	// Save to config
	configPath, err := getConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	content := fmt.Sprintf(`api:
  backend: ollama
  ollama_base_url: "https://ollama.com"
  ollama_key: %s
model:
  provider: ollama
  name: %s
`, apiKey, modelName)

	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("\n%s✓ Ollama Cloud configured!%s\n", colorGreen, colorReset)
	fmt.Printf("  %sConfig:%s %s\n", colorYellow, colorReset, configPath)
	fmt.Printf("  %sModel:%s %s\n", colorYellow, colorReset, modelName)
	fmt.Printf("  %sEndpoint:%s https://ollama.com\n", colorYellow, colorReset)

	showOllamaCloudNextSteps()

	return nil
}

func showNextSteps() {
	fmt.Printf(`
%s─── Next Steps ───%s

  1. Run %sgokin%s in your project directory
  2. Start chatting with the AI assistant
  3. Use %s/help%s to see available commands

%sHappy coding!%s 🚀
`, colorCyan, colorReset, colorBold, colorReset, colorBold, colorReset, colorGreen, colorReset)
}

func showOllamaLocalNextSteps(modelName string) {
	fmt.Printf(`
%s─── Next Steps ───%s

  1. Make sure Ollama is running: %sollama serve%s
  2. Pull your model if needed:   %sollama pull %s%s
  3. Run %sgokin%s in your project directory
  4. Use %s/help%s to see available commands

%sTip:%s List installed models with: %sollama list%s

%sHappy coding!%s 🚀
`, colorCyan, colorReset, colorBold, colorReset, colorBold, modelName, colorReset, colorBold, colorReset, colorBold, colorReset, colorYellow, colorReset, colorBold, colorReset, colorGreen, colorReset)
}

func showOllamaCloudNextSteps() {
	fmt.Printf(`
%s─── Next Steps ───%s

  1. Run %sgokin%s in your project directory
  2. Start chatting with the AI assistant
  3. Use %s/help%s to see available commands

%sTip:%s No local GPU needed — processing runs on Ollama Cloud!

%sHappy coding!%s 🚀
`, colorCyan, colorReset, colorBold, colorReset, colorBold, colorReset, colorYellow, colorReset, colorGreen, colorReset)
}

// spin shows a spinner animation while waiting for a task to complete.
func spin(message string, done <-chan bool) {
	i := 0
	for {
		select {
		case <-done:
			fmt.Printf("\r%s\r", strings.Repeat(" ", len(message)+10))
			return
		default:
			fmt.Printf("\r%s %s", spinnerFrames[i%len(spinnerFrames)], message)
			i++
			time.Sleep(80 * time.Millisecond)
		}
	}
}

// getConfigPath returns the path to the config file.
func getConfigPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "gokin", "config.yaml"), nil
}

// detectInstalledOllamaModels returns a list of installed Ollama models.
func detectInstalledOllamaModels(serverURL string) ([]string, error) {
	if serverURL == "" {
		serverURL = "http://localhost:11434"
	}

	baseURL, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}

	client := api.NewClient(baseURL, &http.Client{Timeout: 5 * time.Second})

	resp, err := client.List(context.Background())
	if err != nil {
		return nil, err
	}

	models := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		models = append(models, m.Name)
	}
	return models, nil
}

// validateAPIKeyReal tests an API key by making a lightweight API call.
func validateAPIKeyReal(backend, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch backend {
	case "gemini":
		// Test with Gemini models.list endpoint
		req, err := http.NewRequestWithContext(ctx, "GET",
			"https://generativelanguage.googleapis.com/v1beta/models?key="+apiKey, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("connection error: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("invalid API key (HTTP %d)", resp.StatusCode)
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("unexpected response (HTTP %d)", resp.StatusCode)
		}
		return nil

	case "deepseek":
		// Test with DeepSeek models endpoint
		req, err := http.NewRequestWithContext(ctx, "GET",
			"https://api.deepseek.com/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("connection error: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("invalid API key (HTTP %d)", resp.StatusCode)
		}
		return nil

	default:
		// For unknown backends, just check key length
		if len(apiKey) < 20 {
			return fmt.Errorf("API key seems too short for %s", backend)
		}
		return nil
	}
}
