package setup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
║       AI coding assistant powered by Gemini, GLM & Ollama     ║
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

  %s[1]%s Gemini (Cloud)   • Google's Gemini models
                       • Free tier available
                       • Get key at: https://aistudio.google.com/apikey

  %s[2]%s GLM (Cloud)      • GLM-4 models
                       • Budget-friendly (~$3/month)
                       • Get key from your GLM provider

  %s[3]%s Ollama (Local)   • Run LLMs locally, no API key needed
                       • Privacy-focused, works offline
                       • Requires: ollama serve

%sEnter your choice (1, 2, or 3):%s `
)

// Spinner animation frames
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// RunSetupWizard runs the enhanced first-time setup wizard.
func RunSetupWizard() error {
	// Print colorful welcome message
	fmt.Printf(welcomeMessage, colorCyan, colorBold, colorCyan, colorReset)

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf(authChoiceMessage, colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorCyan, colorReset)

		choice, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("error reading input: %w", err)
		}

		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			return setupAPIKey(reader, "gemini")
		case "2":
			return setupAPIKey(reader, "glm")
		case "3":
			return setupOllama(reader)
		default:
			fmt.Printf("\n%s⚠ Invalid choice. Please enter 1, 2, or 3.%s\n", colorRed, colorReset)
		}
	}
}

func setupAPIKey(reader *bufio.Reader, backend string) error {
	keyType := "Gemini"
	keyURL := "https://aistudio.google.com/apikey"
	if backend == "glm" {
		keyType = "GLM"
		keyURL = "your GLM provider"
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

	// Show loading spinner
	done := make(chan bool)
	go func() {
		time.Sleep(500 * time.Millisecond)
		done <- true
	}()
	spin("Validating API key...", done)

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
	if backend == "glm" {
		defaultModel = "glm-4.7"
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
