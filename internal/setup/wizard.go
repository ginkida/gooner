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
║         AI coding assistant powered by Gemini & GLM           ║
║                                                               ║
╚═══════════════════════════════════════════════════════════════╝%s

Gokin helps you work with code:
  • Read, create, and edit files
  • Execute terminal commands
  • Search your project (glob, grep, tree)
  • Manage Git (commit, log, diff)
  • And much more!

To get started, you need to set up your API key.
`

	authChoiceMessage = `
%sAuthentication method:%s

  %s[1]%s Gemini API Key   • For Google Gemini models
                       • Get key at: https://aistudio.google.com/apikey

  %s[2]%s GLM API Key      • For GLM-4 models
                       • Get key from your GLM provider

%sEnter your choice (1 or 2):%s `
)

// Spinner animation frames
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// RunSetupWizard runs the enhanced first-time setup wizard.
func RunSetupWizard() error {
	// Print colorful welcome message
	fmt.Printf(welcomeMessage, colorCyan, colorBold, colorCyan, colorReset)

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf(authChoiceMessage, colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorCyan, colorReset)

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
		default:
			fmt.Printf("\n%s⚠ Invalid choice. Please enter 1 or 2.%s\n", colorRed, colorReset)
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

func showNextSteps() {
	fmt.Printf(`
%s─── Next Steps ───%s

  1. Run %sgokin%s in your project directory
  2. Start chatting with the AI assistant
  3. Use %s/help%s to see available commands

%sHappy coding!%s 🚀
`, colorCyan, colorReset, colorBold, colorReset, colorBold, colorReset, colorGreen, colorReset)
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
