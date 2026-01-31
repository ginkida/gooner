package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gooner/internal/config"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

// HelpCommand shows help for commands.
type HelpCommand struct {
	handler *Handler
}

func (c *HelpCommand) Name() string        { return "help" }
func (c *HelpCommand) Description() string { return "Show help for commands" }
func (c *HelpCommand) Usage() string       { return "/help [command]" }
func (c *HelpCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryGettingStarted,
		Icon:     "help",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "[command]",
	}
}

func (c *HelpCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if len(args) > 0 {
		// Show help for specific command
		cmd, exists := c.handler.GetCommand(args[0])
		if !exists {
			return fmt.Sprintf("%sUnknown command: /%s%s\nUse /help to see all commands.", colorRed, args[0], colorReset), nil
		}
		return fmt.Sprintf("%s/%s%s - %s\n\n%sUsage:%s %s\n\n%sDescription:%s %s",
			colorGreen, cmd.Name(), colorReset, colorBold, cmd.Description(), colorReset,
			cmd.Usage(), colorCyan, colorReset, cmd.Description()), nil
	}

	// Show all commands organized by category
	var sb strings.Builder

	// Define categories and their commands (with colors)
	categories := []struct {
		name     string
		icon     string
		commands []string
	}{
		{"Getting Started", "🚀", []string{"help", "quickstart"}},
		{"Session", "📋", []string{"clear", "compact", "save", "resume", "sessions", "model"}},
		{"History & Undo", "⏪", []string{"undo"}},
		{"Git", "🔀", []string{"init", "commit", "pr"}},
		{"Auth", "🔐", []string{"login", "logout"}},
		{"Context", "📝", []string{"instructions"}},
		{"Semantic Search", "🔍", []string{"semantic-stats", "semantic-reindex", "semantic-cleanup"}},
		{"Contracts", "📜", []string{"contract"}},
		{"Interactive", "🖥️", []string{"browse", "clear-todos"}},
		{"Utility", "🔧", []string{"doctor", "config", "stats", "theme"}},
	}

	// Build a map for quick lookup
	cmds := c.handler.ListCommands()
	cmdMap := make(map[string]Command)
	for _, cmd := range cmds {
		cmdMap[cmd.Name()] = cmd
	}

	sb.WriteString(fmt.Sprintf(`
%s╔═══════════════════════════════════════════════════════════════╗
║                     📚 Gooner Commands                       ║
╚═══════════════════════════════════════════════════════════════╝%s

%sNavigation:%s
  • %s/help <command>%s - Command details
  • %s/quickstart%s     - Quick start with examples
  • %s/tour%s           - Interactive tutorial

`, colorCyan, colorReset, colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset))

	for _, cat := range categories {
		var catCmds []Command
		for _, name := range cat.commands {
			if cmd, ok := cmdMap[name]; ok {
				catCmds = append(catCmds, cmd)
				delete(cmdMap, name) // Remove from map to track uncategorized
			}
		}

		if len(catCmds) == 0 {
			continue
		}

		sb.WriteString(fmt.Sprintf("%s%s %s%s\n", colorBold, cat.icon, cat.name, colorReset))
		sb.WriteString(strings.Repeat("─", 55) + "\n")

		for _, cmd := range catCmds {
			sb.WriteString(fmt.Sprintf("  %s/%s%s\n", colorGreen, cmd.Name(), colorReset))
			sb.WriteString(fmt.Sprintf("      %s%s%s\n", colorCyan, cmd.Description(), colorReset))
		}
		sb.WriteString("\n")
	}

	// Show any uncategorized commands
	if len(cmdMap) > 0 {
		sb.WriteString(fmt.Sprintf("%s⚙️  Other Commands%s\n", colorBold, colorReset))
		sb.WriteString(strings.Repeat("─", 55) + "\n")

		// Sort remaining commands
		var remaining []Command
		for _, cmd := range cmdMap {
			remaining = append(remaining, cmd)
		}
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].Name() < remaining[j].Name()
		})

		for _, cmd := range remaining {
			sb.WriteString(fmt.Sprintf("  %s/%s%s\n", colorGreen, cmd.Name(), colorReset))
			sb.WriteString(fmt.Sprintf("      %s%s%s\n", colorCyan, cmd.Description(), colorReset))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("%s─── Keyboard Shortcuts ───%s\n", colorYellow, colorReset))
	sb.WriteString(fmt.Sprintf("  %sCtrl+P%s        - Command palette\n", colorGreen, colorReset))
	sb.WriteString(fmt.Sprintf("  %sCtrl+C%s        - Exit\n", colorGreen, colorReset))
	sb.WriteString(fmt.Sprintf("  %sCtrl+L%s        - Clear screen\n", colorGreen, colorReset))
	sb.WriteString(fmt.Sprintf("  %sEsc%s           - Cancel operation\n\n", colorGreen, colorReset))

	sb.WriteString(fmt.Sprintf("Tip: Use %s/quickstart%s to get started!\n", colorGreen, colorReset))

	return sb.String(), nil
}

// ClearCommand clears the conversation history.
type ClearCommand struct{}

func (c *ClearCommand) Name() string        { return "clear" }
func (c *ClearCommand) Description() string { return "Clear conversation history" }
func (c *ClearCommand) Usage() string       { return "/clear" }
func (c *ClearCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryModelSession,
		Icon:     "clear",
		Priority: 10,
	}
}

func (c *ClearCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	app.ClearConversation()
	// Also clear todos
	if todoTool := app.GetTodoTool(); todoTool != nil {
		todoTool.ClearItems()
	}
	return "Conversation and todos cleared.", nil
}

// CompactCommand forces context compaction.
type CompactCommand struct{}

func (c *CompactCommand) Name() string        { return "compact" }
func (c *CompactCommand) Description() string { return "Force context compaction/summarization" }
func (c *CompactCommand) Usage() string       { return "/compact" }
func (c *CompactCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryModelSession,
		Icon:     "compress",
		Priority: 20,
	}
}

func (c *CompactCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cm := app.GetContextManager()
	if cm == nil {
		return "Context manager not available.", nil
	}

	err := cm.ForceSummarize(ctx)
	if err != nil {
		return fmt.Sprintf("Compaction failed: %v", err), nil
	}

	return "Context compacted successfully.", nil
}

// SaveCommand saves the current session.
type SaveCommand struct{}

func (c *SaveCommand) Name() string        { return "save" }
func (c *SaveCommand) Description() string { return "Save current session" }
func (c *SaveCommand) Usage() string       { return "/save [name]" }
func (c *SaveCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryModelSession,
		Icon:     "save",
		Priority: 30,
		HasArgs:  true,
		ArgHint:  "[name]",
	}
}

func (c *SaveCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	hm, err := app.GetHistoryManager()
	if err != nil {
		return fmt.Sprintf("Failed to get history manager: %v", err), nil
	}

	session := app.GetSession()
	if session == nil {
		return "No active session.", nil
	}

	// Use custom name if provided
	originalID := session.ID
	if len(args) > 0 {
		session.ID = args[0]
	}

	err = hm.SaveFull(session)
	if err != nil {
		session.ID = originalID // Restore original ID
		return fmt.Sprintf("Failed to save session: %v", err), nil
	}

	savedID := session.ID
	session.ID = originalID // Restore original ID

	return fmt.Sprintf("Session saved as: %s", savedID), nil
}

// ResumeCommand resumes a saved session.
type ResumeCommand struct{}

func (c *ResumeCommand) Name() string        { return "resume" }
func (c *ResumeCommand) Description() string { return "Resume a saved session" }
func (c *ResumeCommand) Usage() string       { return "/resume <session_id>" }
func (c *ResumeCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryModelSession,
		Icon:     "resume",
		Priority: 40,
		HasArgs:  true,
		ArgHint:  "<id>",
	}
}

func (c *ResumeCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if len(args) == 0 {
		return "Usage: /resume <session_id>\nUse /sessions to list available sessions.", nil
	}

	hm, err := app.GetHistoryManager()
	if err != nil {
		return fmt.Sprintf("Failed to get history manager: %v", err), nil
	}

	sessionID := args[0]
	state, err := hm.LoadFull(sessionID)
	if err != nil {
		return fmt.Sprintf("Failed to load session '%s': %v", sessionID, err), nil
	}

	session := app.GetSession()
	if session == nil {
		return "No active session to restore into.", nil
	}

	err = session.RestoreFromState(state)
	if err != nil {
		return fmt.Sprintf("Failed to restore session: %v", err), nil
	}

	return fmt.Sprintf("Session '%s' restored. %d messages loaded.", sessionID, len(state.History)), nil
}

// SessionsCommand lists saved sessions.
type SessionsCommand struct{}

func (c *SessionsCommand) Name() string        { return "sessions" }
func (c *SessionsCommand) Description() string { return "List saved sessions" }
func (c *SessionsCommand) Usage() string       { return "/sessions" }
func (c *SessionsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryModelSession,
		Icon:     "list",
		Priority: 50,
	}
}

func (c *SessionsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	hm, err := app.GetHistoryManager()
	if err != nil {
		return fmt.Sprintf("Failed to get history manager: %v", err), nil
	}

	sessions, err := hm.ListSessions()
	if err != nil {
		return fmt.Sprintf("Failed to list sessions: %v", err), nil
	}

	if len(sessions) == 0 {
		return "No saved sessions found.", nil
	}

	var sb strings.Builder
	sb.WriteString("Saved sessions:\n")
	for _, info := range sessions {
		summary := info.Summary
		if len(summary) > 50 {
			summary = summary[:47] + "..."
		}
		if summary == "" {
			summary = "(no summary)"
		}
		sb.WriteString(fmt.Sprintf("  %s (%d messages) - %s\n", info.ID, info.MessageCount, summary))
	}

	return sb.String(), nil
}

// UndoCommand undoes the last file change.
type UndoCommand struct{}

func (c *UndoCommand) Name() string        { return "undo" }
func (c *UndoCommand) Description() string { return "Undo the last file change" }
func (c *UndoCommand) Usage() string       { return "/undo" }
func (c *UndoCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryContext,
		Icon:     "undo",
		Priority: 0,
	}
}

func (c *UndoCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	um := app.GetUndoManager()
	if um == nil {
		return "Undo manager not available.", nil
	}

	change, err := um.Undo()
	if err != nil {
		return fmt.Sprintf("Undo failed: %v", err), nil
	}

	return fmt.Sprintf("Undone change to: %s", change.FilePath), nil
}

// InitCommand initializes GOONER.md for the project.
type InitCommand struct{}

func (c *InitCommand) Name() string        { return "init" }
func (c *InitCommand) Description() string { return "Initialize GOONER.md for this project" }
func (c *InitCommand) Usage() string       { return "/init" }
func (c *InitCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryGit,
		Icon:     "init",
		Priority: 0,
	}
}

func (c *InitCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	workDir := app.GetWorkDir()
	goonerPath := filepath.Join(workDir, "GOONER.md")

	// Check if file already exists
	if _, err := os.Stat(goonerPath); err == nil {
		return "GOONER.md already exists. Edit it manually or delete to reinitialize.", nil
	}

	template := `# Project Instructions for Gooner

## Project Overview
<!-- Describe your project here -->

## Coding Guidelines
<!-- Add specific coding standards for this project -->

## Important Files
<!-- List key files the AI should be aware of -->

## Testing
<!-- Describe how to run tests -->

## Build & Deploy
<!-- Add build and deployment instructions -->
`

	if err := os.WriteFile(goonerPath, []byte(template), 0644); err != nil {
		return fmt.Sprintf("Failed to create GOONER.md: %v", err), nil
	}

	return "Created GOONER.md - edit it to add project-specific instructions.", nil
}

// DoctorCommand checks environment and configuration.
type DoctorCommand struct{}

func (c *DoctorCommand) Name() string        { return "doctor" }
func (c *DoctorCommand) Description() string { return "Check environment and configuration" }
func (c *DoctorCommand) Usage() string       { return "/doctor" }
func (c *DoctorCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "doctor",
		Priority: 0,
	}
}

func (c *DoctorCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`
%s╔═══════════════════════════════════════════════════════════════╗
║                    🔍 System Diagnostics                    ║
╚═══════════════════════════════════════════════════════════════╝%s
`, colorCyan, colorReset))

	sb.WriteString(fmt.Sprintf("\n%s─── Authentication ───%s\n", colorCyan, colorReset))

	cfg := app.GetConfig()
	issues := []string{}
	solutions := []string{}

	// Check API key
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GLM_API_KEY")
	}
	if apiKey == "" && cfg != nil {
		apiKey = cfg.API.APIKey
	}

	backend := "gemini"
	if cfg != nil && cfg.API.Backend != "" {
		backend = cfg.API.Backend
	}

	sb.WriteString(fmt.Sprintf("  Backend: %s%s%s\n", colorGreen, backend, colorReset))
	if apiKey != "" {
		sb.WriteString(fmt.Sprintf("  Status: %s✓ API key configured%s\n", colorGreen, colorReset))
	} else {
		sb.WriteString(fmt.Sprintf("  Status: %s✗ API key not configured%s\n", colorRed, colorReset))
		issues = append(issues, "API key not found")
		solutions = append(solutions, "Use /login <api_key> or set GEMINI_API_KEY/GLM_API_KEY")
	}

	sb.WriteString(fmt.Sprintf("\n%s─── Environment ───%s\n", colorCyan, colorReset))

	// Config file
	configPath := config.GetConfigPath()
	if _, err := os.Stat(configPath); err == nil {
		sb.WriteString(fmt.Sprintf("  %s✓%s Config: %s\n", colorGreen, colorReset, configPath))
	} else {
		sb.WriteString(fmt.Sprintf("  %s○%s Config not found (using defaults)\n", colorYellow, colorReset))
	}

	// Git
	if _, err := exec.LookPath("git"); err == nil {
		sb.WriteString(fmt.Sprintf("  %s✓%s git installed\n", colorGreen, colorReset))
	} else {
		sb.WriteString(fmt.Sprintf("  %s✗%s git not installed\n", colorRed, colorReset))
		issues = append(issues, "Git not installed")
		solutions = append(solutions, "Install git: apt install git / brew install git")
	}

	// GitHub CLI
	if _, err := exec.LookPath("gh"); err == nil {
		sb.WriteString(fmt.Sprintf("  %s✓%s gh (GitHub CLI) installed\n", colorGreen, colorReset))
	} else {
		sb.WriteString(fmt.Sprintf("  %s○%s gh (GitHub CLI) not installed (optional for /pr)\n", colorYellow, colorReset))
	}

	// GOONER.md
	workDir := app.GetWorkDir()
	goonerPath := filepath.Join(workDir, "GOONER.md")
	if _, err := os.Stat(goonerPath); err == nil {
		sb.WriteString(fmt.Sprintf("  %s✓%s GOONER.md found\n", colorGreen, colorReset))
	} else {
		sb.WriteString(fmt.Sprintf("  %s○%s GOONER.md not found (use /init to create)\n", colorYellow, colorReset))
	}

	// Data directories
	dataDir, _ := getDataDir()
	sb.WriteString(fmt.Sprintf("\n%s─── Directories ───%s\n", colorCyan, colorReset))
	sb.WriteString(fmt.Sprintf("  Data: %s\n", dataDir))

	// Summary
	sb.WriteString(fmt.Sprintf("\n%s─── Summary ───%s\n", colorCyan, colorReset))

	if len(issues) == 0 {
		sb.WriteString(fmt.Sprintf("  %s✓ All systems working properly!%s\n", colorGreen, colorReset))
	} else {
		sb.WriteString(fmt.Sprintf("  %s⚠ Issues detected:%s\n", colorYellow, colorReset))
		for i, issue := range issues {
			sb.WriteString(fmt.Sprintf("    %d. %s\n", i+1, issue))
		}

		sb.WriteString(fmt.Sprintf("\n%sSolutions:%s\n", colorGreen, colorReset))
		for i, solution := range solutions {
			sb.WriteString(fmt.Sprintf("    %d. %s\n", i+1, solution))
		}
	}

	sb.WriteString(fmt.Sprintf("\n%sCommands to fix issues:%s\n", colorCyan, colorReset))
	sb.WriteString(fmt.Sprintf("  %s/login%s    - Set up authentication\n", colorGreen, colorReset))
	sb.WriteString(fmt.Sprintf("  %s/test%s     - Test all settings\n", colorGreen, colorReset))
	sb.WriteString(fmt.Sprintf("  %s/init%s     - Create GOONER.md\n", colorGreen, colorReset))

	return sb.String(), nil
}

// ConfigCommand shows current configuration.
type ConfigCommand struct{}

func (c *ConfigCommand) Name() string        { return "config" }
func (c *ConfigCommand) Description() string { return "Show current configuration" }
func (c *ConfigCommand) Usage() string       { return "/config" }
func (c *ConfigCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "config",
		Priority: 10,
	}
}

func (c *ConfigCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Configuration not available.", nil
	}

	var sb strings.Builder
	sb.WriteString("Current Configuration:\n\n")

	// API
	sb.WriteString("API:\n")
	sb.WriteString(fmt.Sprintf("  Backend: %s\n", cfg.API.Backend))
	if cfg.API.APIKey != "" {
		// Show only last 4 characters
		if len(cfg.API.APIKey) > 4 {
			sb.WriteString(fmt.Sprintf("  API Key: ****%s\n", cfg.API.APIKey[len(cfg.API.APIKey)-4:]))
		} else {
			sb.WriteString("  API Key: ****\n")
		}
	} else {
		sb.WriteString("  API Key: (not set)\n")
	}

	// Model
	sb.WriteString("\nModel:\n")
	sb.WriteString(fmt.Sprintf("  Name: %s\n", cfg.Model.Name))
	sb.WriteString(fmt.Sprintf("  Temperature: %.1f\n", cfg.Model.Temperature))
	sb.WriteString(fmt.Sprintf("  Max Output Tokens: %d\n", cfg.Model.MaxOutputTokens))

	// UI
	sb.WriteString("\nUI:\n")
	sb.WriteString(fmt.Sprintf("  Show Token Usage: %v\n", cfg.UI.ShowTokenUsage))
	sb.WriteString(fmt.Sprintf("  Stream Output: %v\n", cfg.UI.StreamOutput))
	sb.WriteString(fmt.Sprintf("  Theme: %s\n", cfg.UI.Theme))

	// Context
	sb.WriteString("\nContext:\n")
	sb.WriteString(fmt.Sprintf("  Max Input Tokens: %d\n", cfg.Context.MaxInputTokens))
	sb.WriteString(fmt.Sprintf("  Auto-Summary: %v\n", cfg.Context.EnableAutoSummary))

	// Permissions
	sb.WriteString("\nPermissions:\n")
	sb.WriteString(fmt.Sprintf("  Enabled: %v\n", cfg.Permission.Enabled))
	sb.WriteString(fmt.Sprintf("  Default Policy: %s\n", cfg.Permission.DefaultPolicy))

	// Plan
	sb.WriteString("\nPlan:\n")
	sb.WriteString(fmt.Sprintf("  Delegate Steps: %v\n", cfg.Plan.DelegateSteps))
	sb.WriteString(fmt.Sprintf("  Clear Context: %v\n", cfg.Plan.ClearContext))

	// Config path
	configPath := config.GetConfigPath()
	sb.WriteString(fmt.Sprintf("\nConfig file: %s\n", configPath))

	return sb.String(), nil
}

// PermissionsCommand toggles permission prompts.
type PermissionsCommand struct{}

func (c *PermissionsCommand) Name() string        { return "permissions" }
func (c *PermissionsCommand) Description() string { return "Toggle permission prompts" }
func (c *PermissionsCommand) Usage() string {
	return `/permissions      - Show status
/permissions on   - Enable prompts
/permissions off  - YOLO mode`
}
func (c *PermissionsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "shield",
		Priority: 20,
		HasArgs:  true,
		ArgHint:  "on|off",
	}
}

func (c *PermissionsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Config not available", nil
	}

	// No args - show current status
	if len(args) == 0 {
		if cfg.Permission.Enabled {
			return "permissions: on", nil
		}
		return "permissions: off (YOLO)", nil
	}

	// Toggle based on argument
	switch strings.ToLower(args[0]) {
	case "on", "true", "1", "enable":
		cfg.Permission.Enabled = true
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "permissions: on", nil

	case "off", "false", "0", "disable":
		cfg.Permission.Enabled = false
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "permissions: off (YOLO)", nil

	default:
		return "/permissions on | off", nil
	}
}

// SandboxCommand toggles bash sandbox mode.
type SandboxCommand struct{}

func (c *SandboxCommand) Name() string        { return "sandbox" }
func (c *SandboxCommand) Description() string { return "Toggle bash sandbox mode" }
func (c *SandboxCommand) Usage() string {
	return `/sandbox      - Show status
/sandbox on   - Safe mode
/sandbox off  - Unrestricted`
}
func (c *SandboxCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "sandbox",
		Priority: 30,
		HasArgs:  true,
		ArgHint:  "on|off",
	}
}

func (c *SandboxCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Config not available", nil
	}

	// No args - show current status
	if len(args) == 0 {
		if cfg.Tools.Bash.Sandbox {
			return "sandbox: on", nil
		}
		return "sandbox: off (!SANDBOX)", nil
	}

	// Toggle based on argument
	switch strings.ToLower(args[0]) {
	case "on", "true", "1", "enable":
		cfg.Tools.Bash.Sandbox = true
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "sandbox: on", nil

	case "off", "false", "0", "disable":
		cfg.Tools.Bash.Sandbox = false
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "sandbox: off (!SANDBOX)", nil

	default:
		return "/sandbox on | off", nil
	}
}

// ClearTodosCommand clears all todo items.
type ClearTodosCommand struct{}

func (c *ClearTodosCommand) Name() string        { return "clear-todos" }
func (c *ClearTodosCommand) Description() string { return "Clear all todo items" }
func (c *ClearTodosCommand) Usage() string       { return "/clear-todos" }
func (c *ClearTodosCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryInteractive,
		Icon:     "clear",
		Priority: 10,
	}
}

func (c *ClearTodosCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	todoTool := app.GetTodoTool()
	if todoTool == nil {
		return "Todo tool not available.", nil
	}
	todoTool.ClearItems()
	return "Todo list cleared.", nil
}

// BrowseCommand opens an interactive file browser.
type BrowseCommand struct{}

func (c *BrowseCommand) Name() string        { return "browse" }
func (c *BrowseCommand) Description() string { return "Open interactive file browser" }
func (c *BrowseCommand) Usage() string       { return "/browse [path]" }
func (c *BrowseCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryInteractive,
		Icon:     "folder",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "[path]",
	}
}

func (c *BrowseCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	startPath := app.GetWorkDir()
	if len(args) > 0 {
		startPath = args[0]
		// Handle relative paths
		if !filepath.IsAbs(startPath) {
			startPath = filepath.Join(app.GetWorkDir(), startPath)
		}
	}

	// Verify path exists
	info, err := os.Stat(startPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	// If it's a file, use its directory
	if !info.IsDir() {
		startPath = filepath.Dir(startPath)
	}

	return fmt.Sprintf("Opening file browser at: %s\n\nUse the interactive browser to navigate. Press 'q' to close.", startPath), nil
}

// getDataDir returns the data directory for the application.
func getDataDir() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "gooner"), nil
}
