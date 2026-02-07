package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/genai"

	"gokin/internal/agent"
	"gokin/internal/audit"
	"gokin/internal/cache"
	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/commands"
	"gokin/internal/config"
	"gokin/internal/git"
	appcontext "gokin/internal/context"
	"gokin/internal/hooks"
	"gokin/internal/logging"
	"gokin/internal/mcp"
	"gokin/internal/permission"
	"gokin/internal/plan"
	"gokin/internal/ratelimit"
	"gokin/internal/router"
	"gokin/internal/semantic"
	"gokin/internal/tasks"
	"gokin/internal/tools"
	"gokin/internal/ui"
	"gokin/internal/undo"
	"gokin/internal/watcher"
)

// SystemPrompt is the default system prompt for the assistant.
const SystemPrompt = `You are Gokin, an AI assistant for software development. You help users work with code by:
- Reading and understanding code files
- Writing and editing code
- Running shell commands
- Searching for files and content
- Managing tasks

You have access to the following tools:
- read: Read file contents with line numbers
- write: Create or overwrite files
- edit: Search and replace text in files
- bash: Execute shell commands
- glob: Find files matching patterns
- grep: Search file contents with regex
- todo: Track tasks and progress
- diff: Compare files and show differences
- tree: Display directory structure
- env: Check environment variables

CRITICAL RESPONSE GUIDELINES:
1. **ALWAYS provide a direct answer** to the user's question after using tools
2. **NEVER just read files silently** - always explain what you found
3. **Be specific and actionable** - give concrete recommendations
4. **Structure your response**:
   - First: Direct answer to the question
   - Then: Evidence from code (what you read)
   - Finally: Specific suggestions or next steps
5. **If analyzing code**: summarize key points, highlight issues, suggest improvements
6. **If asked to explain**: break down complex concepts clearly
7. **Use markdown formatting** for better readability (code blocks, lists, headers)
8. **After using ANY tool** (read, list_dir, grep, etc.) you MUST provide a response summarizing what you found
9. **Even if the tool returns empty results**, explain what that means
10. **Never leave a conversation hanging** - always conclude with a clear answer or question

Examples of GOOD responses:
- "This code does X. I noticed 3 potential issues: 1)... 2)... 3)..."
- "Based on the files I read, here's what I found: [summary]. Suggestions: [list]"
- "The architecture uses [pattern]. Main components are: [list]. To improve: [suggestions]"
- "I listed the directory and found: [files]. Here's the project structure: [analysis]"
- "The search returned [results]. This means: [conclusion]"

Examples of BAD responses (avoid these):
- Reading files and saying nothing
- "OK" or "Done" without explanation
- Just listing files without analysis
- Using tools without providing ANY response afterward
- Calling list_dir/read/grep and then stopping

Additional Guidelines:
- Always read files before editing them
- Use the todo tool to track multi-step tasks
- Prefer editing existing files over creating new ones
- When executing commands, explain what they do
- Handle errors gracefully and suggest fixes

The user's working directory is: %s`

// App is the main application orchestrator.
type App struct {
	config   *config.Config
	workDir  string
	client   client.Client
	registry *tools.Registry
	executor *tools.Executor
	session  *chat.Session
	tui      *ui.Model
	program  *tea.Program

	// Application context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Context management
	projectInfo    *appcontext.ProjectInfo
	contextManager *appcontext.ContextManager
	promptBuilder  *appcontext.PromptBuilder
	contextAgent   *appcontext.ContextAgent

	// Permission management
	permManager      *permission.Manager
	permResponseChan chan permission.Decision

	// Question handling
	questionResponseChan chan string

	// Diff preview handling
	diffResponseChan chan ui.DiffDecision

	// Plan management
	planManager      *plan.Manager
	planApprovalChan chan plan.ApprovalDecision

	// Hooks management
	hooksManager *hooks.Manager

	// Task management
	taskManager *tasks.Manager

	// Undo management
	undoManager *undo.Manager

	// Agent management
	agentRunner *agent.Runner

	// Command handler
	commandHandler *commands.Handler

	// Token tracking
	totalInputTokens  int
	totalOutputTokens int

	// Response metadata tracking
	responseStartTime time.Time
	responseToolsUsed []string

	// Session persistence
	sessionManager *chat.SessionManager

	// New feature integrations
	searchCache     *cache.SearchCache
	rateLimiter     *ratelimit.Limiter
	auditLogger     *audit.Logger
	fileWatcher       *watcher.Watcher
	semanticIndexer   *semantic.EnhancedIndexer
	backgroundIndexer *semantic.BackgroundIndexer

	// Task router for intelligent task routing
	taskRouter *router.Router

	// Agent Scratchpad (shared)
	scratchpad string

	// Unified Task Orchestrator (replacing QueueManager, DependencyManager, ParallelExecutor)
	orchestrator *TaskOrchestrator

	uiUpdateManager *UIUpdateManager // Coordinates periodic UI updates

	// === PHASE 5: Agent System Improvements (6→10) ===
	coordinator       *agent.Coordinator       // Task orchestration
	agentTypeRegistry *agent.AgentTypeRegistry // Dynamic agent types
	strategyOptimizer *agent.StrategyOptimizer // Strategy learning
	metaAgent         *agent.MetaAgent         // Agent monitoring

	// === PHASE 6: Tree Planner ===
	treePlanner         *agent.TreePlanner // Tree-based planning
	planningModeEnabled bool               // toggle for planning mode

	// MCP (Model Context Protocol)
	mcpManager *mcp.Manager

	// Streaming token estimation
	streamedChars int // Accumulated chars during current streaming session

	// === Task 5.7: Project Context Auto-Injection ===
	detectedProjectContext string // Computed once at startup

	// === Task 5.8: Tool Usage Pattern Learning ===
	toolPatterns []toolPattern // Detected repeating tool sequences
	recentTools  []string      // Last 20 tool names used
	messageCount int           // Total messages processed (for periodic hint injection)

	mu         sync.Mutex
	running    bool
	processing bool // Guards against concurrent message processing

	// Processing cancellation for ESC interrupt
	processingCancel context.CancelFunc
	processingMu     sync.Mutex

	// Signal handler cleanup
	signalCleanup func()

	// Pending message queue
	pendingMessage string
	pendingMu      sync.Mutex
}

// toolPattern, detectPatterns, getToolHints, recordToolUsage are in pattern_detector.go
// detectProjectContext, extractGoModInfo, extractPackageJSONInfo, readFirstLines, fileExists, readFileHead are in project_detector.go

// New creates a new application instance.
func New(cfg *config.Config, workDir string) (*App, error) {
	return NewBuilder(cfg, workDir).Build()
}

// Run starts the application.
func (a *App) Run() error {
	// NOTE: Allowed dirs prompt is now done in Builder.checkAllowedDirs()
	// before tool creation, so PathValidator gets correct directories.

	// Configure logging to file to avoid TUI interference
	configDir, err := appcontext.GetConfigDir()
	if err == nil && a.config.Logging.Level != "" {
		level := logging.ParseLevel(a.config.Logging.Level)
		if err := logging.EnableFileLogging(configDir, level); err != nil {
			// Silently continue with logging disabled
			logging.DisableLogging()
		}
	} else {
		// Disable logging if no config dir or level not set
		logging.DisableLogging()
	}

	// === Task 5.7: Detect project context once at startup ===
	a.detectedProjectContext = a.detectProjectContext()
	if a.detectedProjectContext != "" && a.promptBuilder != nil {
		a.promptBuilder.SetDetectedContext(a.detectedProjectContext)
		logging.Debug("project context auto-detected", "length", len(a.detectedProjectContext))
	}

	// Run on_start hooks with proper context
	if a.hooksManager != nil {
		a.hooksManager.RunOnStart(a.ctx)
	}

	// Load input history
	if err := a.tui.LoadInputHistory(); err != nil {
		logging.Debug("failed to load input history", "error", err)
	}

	// Auto-load previous session if enabled
	var sessionRestored bool
	if a.sessionManager != nil {
		state, info, err := a.sessionManager.LoadLast()
		if err == nil && state != nil {
			// Check if session has meaningful content (more than just system prompt)
			if len(state.History) > 2 {
				if restoreErr := a.sessionManager.RestoreFromState(state); restoreErr != nil {
					logging.Warn("failed to restore session", "error", restoreErr)
				} else {
					sessionRestored = true
					// Sync scratchpad from restored session
					a.scratchpad = a.session.GetScratchpad()
					if a.agentRunner != nil {
						a.agentRunner.SetSharedScratchpad(a.scratchpad)
					}
					// Notify TUI about restored scratchpad
					a.safeSendToProgram(ui.ScratchpadMsg(a.scratchpad))

					// Notify user about restored session
					a.tui.AddSystemMessage(fmt.Sprintf("Restored session from %s (%d messages)",
						info.LastActive.Format("2006-01-02 15:04"), len(state.History)))
				}
			}
		}
	}

	// Build model-specific enhancement
	modelEnhancement := a.buildModelEnhancement()

	// Set system instruction via native API parameter (not as user message)
	if !sessionRestored {
		systemPrompt := a.promptBuilder.Build()
		systemPrompt += modelEnhancement
		a.client.SetSystemInstruction(systemPrompt)
		a.session.SystemInstruction = systemPrompt
	} else {
		// Restored session: clean up legacy system prompt messages from history
		a.stripLegacySystemMessages()

		// Use saved system instruction or rebuild
		if a.session.SystemInstruction != "" {
			a.client.SetSystemInstruction(a.session.SystemInstruction)
		} else {
			// Legacy session without SystemInstruction — rebuild
			systemPrompt := a.promptBuilder.Build()
			systemPrompt += modelEnhancement
			a.client.SetSystemInstruction(systemPrompt)
			a.session.SystemInstruction = systemPrompt
		}
	}

	// Start session manager for periodic saves
	if a.sessionManager != nil {
		a.sessionManager.Start(a.ctx)
	}

	// Show welcome message
	a.tui.Welcome()

	// Check for paused plans and notify user
	if a.planManager != nil && a.planManager.HasPausedPlan() {
		plans, err := a.planManager.ListResumablePlans()
		if err == nil && len(plans) > 0 {
			// Show notification about resumable plan
			latestPlan := plans[0] // Most recent
			msg := fmt.Sprintf("Paused plan found: %s (%d/%d steps complete)\nUse /resume-plan to continue.",
				latestPlan.Title, latestPlan.Completed, latestPlan.StepCount)
			a.tui.AddSystemMessage(msg)
			logging.Info("paused plan available for resume",
				"plan_id", latestPlan.ID,
				"title", latestPlan.Title,
				"progress", fmt.Sprintf("%d/%d", latestPlan.Completed, latestPlan.StepCount))
		}
	}

	// Create and run the program
	a.program = a.tui.GetProgram()

	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	// === PHASE 4: Initialize UI Auto-Update System ===
	a.initializeUIUpdateSystem()

	// Start background processes
	if a.orchestrator != nil {
		go a.orchestrator.Start(a.ctx)
	}
	if a.contextAgent != nil {
		go a.contextAgent.Start(a.ctx)
	}

	// Set app reference in TUI for data providers
	a.tui.SetApp(a)

	// Set up signal handling for graceful shutdown
	a.signalCleanup = a.setupSignalHandler()

	// Start periodic task cleanup goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if a.taskManager != nil {
					cleaned := a.taskManager.Cleanup(30 * time.Minute)
					if cleaned > 0 {
						logging.Debug("cleaned up completed tasks", "count", cleaned)
					}
				}
			case <-a.ctx.Done():
				return
			}
		}
	}()

	// Start file watcher if enabled
	if a.fileWatcher != nil {
		a.fileWatcher.SetOnFileChange(func(path string, op watcher.Operation) {
			// Invalidate cache on file changes
			if a.searchCache != nil {
				a.searchCache.InvalidateByPath(path)
			}
		})
		if err := a.fileWatcher.Start(); err != nil {
			logging.Warn("failed to start file watcher", "error", err)
		}
	}

	// Start background semantic indexer (watcher-driven incremental indexing)
	if a.backgroundIndexer != nil {
		if err := a.backgroundIndexer.Start(); err != nil {
			logging.Warn("failed to start background semantic indexer", "error", err)
		}
	}

	// Start initial indexing for semantic search if enabled
	if a.semanticIndexer != nil && a.config.Semantic.IndexOnStart {
		go func() {
			logging.Debug("starting background semantic indexing")

			// Use LoadOrIndex for intelligent loading from cache
			maxAge := 24 * time.Hour // Cache is fresh for 24 hours
			if err := a.semanticIndexer.LoadOrIndex(a.ctx, true, maxAge); err != nil {
				logging.Error("semantic indexing failed", "error", err)
			} else {
				stats := a.semanticIndexer.GetStats()
				logging.Debug("semantic indexing complete",
					"files", stats.FileCount,
					"chunks", stats.ChunkCount)
			}
		}()
	}

	_, runErr := a.program.Run()
	a.tui.Cleanup()

	a.mu.Lock()
	a.running = false
	a.mu.Unlock()

	// === PHASE 4: Stop UI Auto-Update System ===
	if a.uiUpdateManager != nil {
		a.uiUpdateManager.Stop()
		logging.Debug("UI update manager stopped")
	}

	// Stop session manager
	if a.sessionManager != nil {
		a.sessionManager.Stop()
	}

	return runErr
}

// handleSubmit handles user message submission.
func (a *App) handleSubmit(message string) {
	a.mu.Lock()
	if a.processing {
		// Copy program reference while holding lock
		program := a.program
		a.mu.Unlock()

		// Save as pending message instead of discarding
		a.pendingMu.Lock()
		a.pendingMessage = message
		a.pendingMu.Unlock()

		// Notify user that message is queued (using copied reference)
		if program != nil {
			program.Send(ui.StreamTextMsg("📥 Message queued - will process after current request completes\n"))
		}
		return
	}
	a.processing = true

	// Parse command BEFORE unlocking to avoid race condition
	// (parsing is fast and doesn't need to be concurrent)
	name, args, isCmd := a.commandHandler.Parse(message)
	a.mu.Unlock()

	// Now safely start the goroutine
	if isCmd {
		go a.executeCommand(name, args)
		return
	}

	// Create cancelable context for this request
	a.processingMu.Lock()
	ctx, cancel := context.WithCancel(a.ctx)
	a.processingCancel = cancel
	a.processingMu.Unlock()

	// Process message normally (coordinator is now integrated in agent system)
	go func() {
		defer func() {
			a.processingMu.Lock()
			a.processingCancel = nil
			a.processingMu.Unlock()
		}()
		a.processMessageWithContext(ctx, message)
	}()
}

// executeCommand executes a slash command.
func (a *App) executeCommand(name string, args []string) {
	defer func() {
		a.mu.Lock()
		a.processing = false
		a.mu.Unlock()
	}()

	ctx := a.ctx
	result, err := a.commandHandler.Execute(ctx, name, args, a)

	// Copy program reference under lock for safe access
	a.mu.Lock()
	program := a.program
	a.mu.Unlock()

	if program != nil {
		if err != nil {
			program.Send(ui.ErrorMsg(err))
		} else {
			// Display command result as assistant message
			program.Send(ui.StreamTextMsg(result))
		}
		program.Send(ui.ResponseDoneMsg{})
	}
}

// handleQuit handles quit request.
func (a *App) handleQuit() {
	// Use graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
	defer cancel()
	a.gracefulShutdown(ctx)
}

// processMessage processes a user message asynchronously (uses app context).
func (a *App) processMessage(message string) {
	a.processMessageWithContext(a.ctx, message)
}

// processMessageWithContext and related methods are in message_processor.go

// GetPlanManager returns the plan manager.
func (a *App) GetPlanManager() *plan.Manager {
	return a.planManager
}

// GetTreePlanner returns the tree planner.
func (a *App) GetTreePlanner() *agent.TreePlanner {
	return a.treePlanner
}

// GetAgentTypeRegistry returns the agent type registry.
func (a *App) GetAgentTypeRegistry() *agent.AgentTypeRegistry {
	return a.agentTypeRegistry
}

// AppInterface implementation for commands package

// GetSession returns the current session.
func (a *App) GetSession() *chat.Session {
	return a.session
}

// GetHistoryManager returns a new history manager.
func (a *App) GetHistoryManager() (*chat.HistoryManager, error) {
	return chat.NewHistoryManager()
}

// GetContextManager returns the context manager.
func (a *App) GetContextManager() *appcontext.ContextManager {
	return a.contextManager
}

// safeSendToProgram safely sends a message to the Bubbletea program.
// It copies the program reference under lock to prevent race conditions.
func (a *App) safeSendToProgram(msg tea.Msg) {
	a.mu.Lock()
	program := a.program
	a.mu.Unlock()

	if program != nil {
		program.Send(msg)
	}
}

// sendTokenUsageUpdate sends a token usage update to the UI.
// This can be called from any goroutine safely.
func (a *App) sendTokenUsageUpdate() {
	if a.contextManager == nil || !a.config.UI.ShowTokenUsage {
		return
	}

	a.mu.Lock()
	program := a.program
	a.mu.Unlock()

	if program == nil {
		return
	}

	usage := a.contextManager.GetTokenUsage()
	if usage == nil {
		return
	}
	program.Send(ui.TokenUsageMsg{
		Tokens:      usage.InputTokens,
		MaxTokens:   usage.MaxTokens,
		PercentUsed: usage.PercentUsed,
		NearLimit:   usage.NearLimit,
		IsEstimate:  usage.IsEstimate,
	})
}

// refreshTokenCount recalculates token count from session history and sends update to UI.
// More expensive than sendTokenUsageUpdate - call after history changes.
func (a *App) refreshTokenCount() {
	if a.contextManager == nil {
		return
	}
	ctx := context.Background()
	if err := a.contextManager.UpdateTokenCount(ctx); err != nil {
		return
	}
	a.sendTokenUsageUpdate()
}

// GetUndoManager returns the undo manager.
func (a *App) GetUndoManager() *undo.Manager {
	return a.undoManager
}

// GetWorkDir returns the working directory.
func (a *App) GetWorkDir() string {
	return a.workDir
}

// ClearConversation clears the session history.
func (a *App) ClearConversation() {
	a.session.Clear()

	// Re-set system instruction via API parameter
	systemPrompt := a.promptBuilder.Build()
	a.client.SetSystemInstruction(systemPrompt)
	a.session.SystemInstruction = systemPrompt
}

// CompactContextWithPlan clears the conversation and injects the plan summary.
// This is called when a plan is approved to free up context space.
func (a *App) CompactContextWithPlan(planSummary string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Clear the session
	a.session.Clear()

	// Re-set system instruction via API parameter
	systemPrompt := a.promptBuilder.Build()
	a.client.SetSystemInstruction(systemPrompt)
	a.session.SystemInstruction = systemPrompt

	// Inject the plan summary as a user message for execution context
	if planSummary != "" {
		a.session.AddUserMessage("Execute the approved plan. Summary:\n\n" + planSummary)
	}

	// Log the context compaction
	logging.Info("context compacted for plan execution",
		"session_id", a.session.ID,
		"plan_summary_length", len(planSummary))
}

// GetTodoTool returns the todo tool from the registry.
func (a *App) GetTodoTool() *tools.TodoTool {
	if todoTool, ok := a.registry.Get("todo"); ok {
		if tt, ok := todoTool.(*tools.TodoTool); ok && tt != nil {
			return tt
		}
	}
	return nil
}

// GetConfig returns the current configuration.
func (a *App) GetConfig() *config.Config {
	return a.config
}

// GetTokenStats returns token usage statistics for the session.
func (a *App) GetTokenStats() commands.TokenStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return commands.TokenStats{
		InputTokens:  a.totalInputTokens,
		OutputTokens: a.totalOutputTokens,
		TotalTokens:  a.totalInputTokens + a.totalOutputTokens,
	}
}

// GetModelSetter returns the client for model switching.
func (a *App) GetModelSetter() commands.ModelSetter {
	return a.client
}

// TogglePermissions toggles the permission system on/off.
func (a *App) TogglePermissions() bool {
	a.mu.Lock()

	if a.permManager == nil {
		a.mu.Unlock()
		return false
	}

	currentEnabled := a.permManager.IsEnabled()
	newEnabled := !currentEnabled
	a.permManager.SetEnabled(newEnabled)

	// Update TUI display
	if a.tui != nil {
		a.tui.SetPermissionsEnabled(newEnabled)
	}

	if newEnabled {
		logging.Debug("permissions enabled")
	} else {
		logging.Debug("permissions disabled")
	}

	// Update unrestricted mode based on new state
	a.updateUnrestrictedModeLocked()

	// Copy state for UI message before unlocking
	program := a.program
	sandboxEnabled := a.config.Tools.Bash.Sandbox
	planningModeEnabled := a.planningModeEnabled
	modelName := a.config.Model.Name
	a.mu.Unlock()

	// Send UI update message (program.Send is thread-safe)
	if program != nil {
		program.Send(ui.ConfigUpdateMsg{
			PermissionsEnabled:  newEnabled,
			SandboxEnabled:      sandboxEnabled,
			PlanningModeEnabled: planningModeEnabled,
			ModelName:           modelName,
		})
	}

	return newEnabled
}

// TogglePlanningMode toggles the tree planning mode on/off.
func (a *App) TogglePlanningMode() bool {
	a.mu.Lock()

	a.planningModeEnabled = !a.planningModeEnabled
	newEnabled := a.planningModeEnabled

	// Update agent runner
	if a.agentRunner != nil {
		a.agentRunner.SetPlanningModeEnabled(newEnabled)
	}

	// Update TUI display (direct setter for immediate effect)
	if a.tui != nil {
		a.tui.SetPlanningModeEnabled(newEnabled)
	}

	if newEnabled {
		logging.Debug("planning mode enabled")
	} else {
		logging.Debug("planning mode disabled")
	}

	// Copy state for UI message before unlocking
	program := a.program
	permissionsEnabled := a.permManager != nil && a.permManager.IsEnabled()
	sandboxEnabled := a.config.Tools.Bash.Sandbox
	modelName := a.config.Model.Name
	a.mu.Unlock()

	// Send UI update message to trigger proper refresh
	if program != nil {
		program.Send(ui.ConfigUpdateMsg{
			PermissionsEnabled:  permissionsEnabled,
			SandboxEnabled:      sandboxEnabled,
			PlanningModeEnabled: newEnabled,
			ModelName:           modelName,
		})
	}

	return newEnabled
}

// IsPlanningModeEnabled returns whether planning mode is active.
func (a *App) IsPlanningModeEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planningModeEnabled
}

// TogglePlanningModeAsync toggles planning mode asynchronously.
// This is safe to call from UI callbacks as it doesn't block the Bubble Tea event loop.
func (a *App) TogglePlanningModeAsync() {
	go func() {
		a.mu.Lock()
		a.planningModeEnabled = !a.planningModeEnabled
		newEnabled := a.planningModeEnabled

		if a.agentRunner != nil {
			a.agentRunner.SetPlanningModeEnabled(newEnabled)
		}

		if a.tui != nil {
			a.tui.SetPlanningModeEnabled(newEnabled)
		}

		if newEnabled {
			logging.Debug("planning mode enabled")
		} else {
			logging.Debug("planning mode disabled")
		}

		program := a.program
		permissionsEnabled := a.permManager != nil && a.permManager.IsEnabled()
		sandboxEnabled := a.config.Tools.Bash.Sandbox
		modelName := a.config.Model.Name
		a.mu.Unlock()

		if program != nil {
			// Send toggled message for UI feedback
			program.Send(ui.PlanningModeToggledMsg{Enabled: newEnabled})
			// Send config update for status bar
			program.Send(ui.ConfigUpdateMsg{
				PermissionsEnabled:  permissionsEnabled,
				SandboxEnabled:      sandboxEnabled,
				PlanningModeEnabled: newEnabled,
				ModelName:           modelName,
			})
		}
	}()
}

// ToggleSandbox toggles the bash sandbox mode on/off.
func (a *App) ToggleSandbox() bool {
	a.mu.Lock()

	a.config.Tools.Bash.Sandbox = !a.config.Tools.Bash.Sandbox
	newEnabled := a.config.Tools.Bash.Sandbox

	// Save config
	if err := a.config.Save(); err != nil {
		logging.Warn("failed to save sandbox setting", "error", err)
	}

	// Update TUI display
	if a.tui != nil {
		a.tui.SetSandboxEnabled(newEnabled)
	}

	if newEnabled {
		logging.Debug("sandbox enabled")
	} else {
		logging.Debug("sandbox disabled")
	}

	// Update unrestricted mode based on new state
	a.updateUnrestrictedModeLocked()

	// Copy state for UI message before unlocking
	program := a.program
	permissionsEnabled := a.permManager != nil && a.permManager.IsEnabled()
	planningModeEnabled := a.planningModeEnabled
	modelName := a.config.Model.Name
	a.mu.Unlock()

	// Send UI update message to trigger proper refresh
	if program != nil {
		program.Send(ui.ConfigUpdateMsg{
			PermissionsEnabled:  permissionsEnabled,
			SandboxEnabled:      newEnabled,
			PlanningModeEnabled: planningModeEnabled,
			ModelName:           modelName,
		})
	}

	return newEnabled
}

// GetSandboxState returns whether sandbox mode is enabled.
func (a *App) GetSandboxState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config.Tools.Bash.Sandbox
}

// updateUnrestrictedModeLocked updates the executor's unrestricted mode based on
// current sandbox and permission states. Must be called with a.mu held.
func (a *App) updateUnrestrictedModeLocked() {
	if a.executor == nil {
		return
	}

	sandboxOff := !a.config.Tools.Bash.Sandbox
	permissionOff := a.permManager == nil || !a.permManager.IsEnabled()
	unrestrictedMode := sandboxOff && permissionOff

	// Update executor's unrestricted mode
	a.executor.SetUnrestrictedMode(unrestrictedMode)

	// Update bash tool's unrestricted mode
	if a.registry != nil {
		if bashTool, ok := a.registry.Get("bash"); ok {
			if bt, ok := bashTool.(*tools.BashTool); ok {
				bt.SetUnrestrictedMode(unrestrictedMode)
			}
		}
	}

	if unrestrictedMode {
		logging.Debug("unrestricted mode enabled: sandbox=off, permission=off")
	} else {
		logging.Debug("unrestricted mode disabled")
	}
}

// GetPermissionsState returns whether permissions are enabled.
func (a *App) GetPermissionsState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.permManager == nil {
		return false
	}
	return a.permManager.IsEnabled()
}

// GetProjectInfo returns the detected project information.
func (a *App) GetProjectInfo() *appcontext.ProjectInfo {
	return a.projectInfo
}

// GetSemanticIndexer returns the semantic search indexer.
func (a *App) GetSemanticIndexer() (*semantic.EnhancedIndexer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.semanticIndexer == nil {
		return nil, fmt.Errorf("semantic search not enabled")
	}

	return a.semanticIndexer, nil
}

// ApplyConfig saves the given configuration and re-initializes affected components.
func (a *App) ApplyConfig(cfg *config.Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 1. Save to file if path is available
	if err := cfg.Save(); err != nil {
		logging.Warn("failed to save config to file", "error", err)
		// Continue anyway as we want to apply it in-memory
	}

	// 2. Update internal config
	a.config = cfg

	// 3. Re-initialize client
	newClient, err := client.NewClient(a.ctx, a.config, a.config.Model.Name)
	if err != nil {
		return fmt.Errorf("failed to re-initialize client: %w", err)
	}
	a.client = newClient

	// 4. Update executor's client and sync tools
	if a.executor != nil {
		a.executor.SetClient(newClient)
		if a.registry != nil {
			newClient.SetTools(a.registry.GeminiTools())
		}
	}

	// 5. Update agent runner
	if a.agentRunner != nil {
		a.agentRunner.SetClient(newClient)
		a.agentRunner.SetContextConfig(&a.config.Context)
	}

	// 6. Update context manager
	if a.contextManager != nil {
		a.contextManager.SetConfig(&a.config.Context)
		a.contextManager.SetClient(newClient)
	}

	// 7. Update rate limiter
	if a.config.RateLimit.Enabled {
		if a.rateLimiter == nil {
			a.rateLimiter = ratelimit.NewLimiter(ratelimit.Config{
				Enabled:           true,
				RequestsPerMinute: a.config.RateLimit.RequestsPerMinute,
				TokensPerMinute:   a.config.RateLimit.TokensPerMinute,
				BurstSize:         a.config.RateLimit.BurstSize,
			})
		} else {
			// Update existing limiter (assuming it has a way to update config)
			// For now, recreate it or ignore if no update method
		}
		a.client.SetRateLimiter(a.rateLimiter)
	}

	// 8. Update permission manager (YOLO mode)
	if a.permManager != nil {
		a.permManager.SetEnabled(a.config.Permission.Enabled)
	}

	// 8a. Update bash tool sandbox mode
	if a.registry != nil {
		if bashTool, ok := a.registry.Get("bash"); ok {
			if bt, ok := bashTool.(*tools.BashTool); ok {
				bt.SetSandboxEnabled(a.config.Tools.Bash.Sandbox)
			}
		}
	}

	// 8c. Update UI state (model name, etc.)
	if a.tui != nil {
		a.tui.SetCurrentModel(a.config.Model.Name)
		a.tui.SetShowTokens(a.config.UI.ShowTokenUsage)
		a.tui.SetPermissionsEnabled(a.config.Permission.Enabled)
		a.tui.SetSandboxEnabled(a.config.Tools.Bash.Sandbox)
		a.tui.SetPlanningModeEnabled(a.planningModeEnabled)
	}

	// 8d. Send ConfigUpdateMsg to Bubbletea program to refresh UI
	if a.program != nil {
		a.program.Send(ui.ConfigUpdateMsg{
			PermissionsEnabled:  a.config.Permission.Enabled,
			SandboxEnabled:      a.config.Tools.Bash.Sandbox,
			PlanningModeEnabled: a.planningModeEnabled,
			ModelName:           a.config.Model.Name,
		})
	}

	// 9. Update search cache
	if a.config.Cache.Enabled && a.searchCache == nil {
		a.searchCache = cache.NewSearchCache(a.config.Cache.Capacity, a.config.Cache.TTL)
		// Re-wire to tools (complex, but most tools check on use)
	}

	logging.Info("configuration applied successfully", "model", a.config.Model.Name)
	return nil
}

// stripLegacySystemMessages removes old-style system prompt messages from session history.
// Before Phase 1, system prompt was injected as history[0] (user message) + history[1] (model ack).
// Now system prompt is passed via API parameter, so these legacy messages waste tokens.
func (a *App) stripLegacySystemMessages() {
	history := a.session.GetHistory()
	if len(history) < 2 {
		return
	}

	stripCount := 0

	// Check if first message is a legacy system prompt (user role, long text with system prompt markers)
	if history[0].Role == string(genai.RoleUser) && len(history[0].Parts) > 0 {
		text := history[0].Parts[0].Text
		if len(text) > 500 && (strings.Contains(text, "You are") || strings.Contains(text, "MANDATORY") || strings.Contains(text, "available tools")) {
			stripCount = 1
			// Also check for model acknowledgment
			if len(history) >= 2 && history[1].Role == string(genai.RoleModel) && len(history[1].Parts) > 0 {
				ackText := history[1].Parts[0].Text
				if len(ackText) < 200 && (strings.Contains(ackText, "understand") || strings.Contains(ackText, "I'll") || strings.Contains(ackText, "help")) {
					stripCount = 2
				}
			}
		}
	}

	if stripCount > 0 {
		a.session.SetHistory(history[stripCount:])
		logging.Info("stripped legacy system messages from restored session", "count", stripCount)
	}
}

// buildModelEnhancement returns model-specific prompt enhancements.
func (a *App) buildModelEnhancement() string {
	modelName := a.config.Model.Name

	if strings.HasPrefix(modelName, "glm") {
		return "\n\n**GLM Model Note:** After every tool call, you MUST respond with analysis of results. Never call tools and stop silently. Structure: What I Found → Key Points → Next Steps."
	}

	if strings.Contains(modelName, "flash") {
		return "\n\n**Flash Model Note:** Keep responses detailed with specific file:line references despite speed optimizations."
	}

	// Ollama models: per-model prompting + tool calling fallback
	if a.config.API.Backend == "ollama" {
		var enhancement string

		// Add per-model prompt enhancement based on model profile
		enhancement += client.ModelPromptEnhancement(modelName)

		// Add tool calling fallback prompt for models without native tool support
		profile := client.GetModelProfile(modelName)
		if !profile.SupportsTools {
			// Use only the filtered tool set (same as selectToolSets in builder)
			decls := a.getActiveToolDeclarations()
			enhancement += client.ToolCallFallbackPrompt(decls)
		}

		return enhancement
	}

	return ""
}

// getActiveToolDeclarations returns declarations for the tools actually available
// to the current model (matching selectToolSets logic in builder).
func (a *App) getActiveToolDeclarations() []*genai.FunctionDeclaration {
	if a.config.API.Backend == "ollama" {
		sets := []tools.ToolSet{tools.ToolSetOllamaCore}
		if git.IsGitRepo(a.workDir) {
			sets = append(sets, tools.ToolSetGit)
		}
		return a.registry.FilteredDeclarations(sets...)
	}
	return a.registry.Declarations()
}

// handleApplyCodeBlock is in app_handlers.go

// CancelProcessing cancels the current processing request.
// Called when user presses ESC during processing.
func (a *App) CancelProcessing() {
	a.processingMu.Lock()
	defer a.processingMu.Unlock()
	if a.processingCancel != nil {
		a.processingCancel()
		a.processingCancel = nil
	}
}

// agentRunnerAdapter wraps agent.Runner to implement tools.AgentRunner interface.
type agentRunnerAdapter struct {
	runner *agent.Runner
}

func (a *agentRunnerAdapter) Spawn(ctx context.Context, agentType string, prompt string, maxTurns int, model string) (string, error) {
	return a.runner.Spawn(ctx, agentType, prompt, maxTurns, model)
}

func (a *agentRunnerAdapter) SpawnAsync(ctx context.Context, agentType string, prompt string, maxTurns int, model string) string {
	return a.runner.SpawnAsync(ctx, agentType, prompt, maxTurns, model)
}

func (a *agentRunnerAdapter) SpawnAsyncWithStreaming(ctx context.Context, agentType string, prompt string, maxTurns int, model string, onText func(string), onProgress func(id string, progress *tools.AgentProgress)) string {
	// Convert tools.AgentProgress callback to agent.AgentProgress callback
	var agentProgressCb func(id string, progress *agent.AgentProgress)
	if onProgress != nil {
		agentProgressCb = func(id string, progress *agent.AgentProgress) {
			if progress != nil {
				onProgress(id, &tools.AgentProgress{
					AgentID:       progress.AgentID,
					CurrentStep:   progress.CurrentStep,
					TotalSteps:    progress.TotalSteps,
					CurrentAction: progress.CurrentAction,
					Elapsed:       progress.Elapsed,
					ToolsUsed:     progress.ToolsUsed,
				})
			}
		}
	}
	return a.runner.SpawnAsyncWithStreaming(ctx, agentType, prompt, maxTurns, model, onText, agentProgressCb)
}

func (a *agentRunnerAdapter) Resume(ctx context.Context, agentID string, prompt string) (string, error) {
	return a.runner.Resume(ctx, agentID, prompt)
}

func (a *agentRunnerAdapter) ResumeAsync(ctx context.Context, agentID string, prompt string) (string, error) {
	return a.runner.ResumeAsync(ctx, agentID, prompt)
}

func (a *agentRunnerAdapter) GetResult(agentID string) (tools.AgentResult, bool) {
	result, ok := a.runner.GetResult(agentID)
	if !ok || result == nil {
		return tools.AgentResult{}, false
	}
	return tools.AgentResult{
		AgentID:   result.AgentID,
		Type:      string(result.Type),
		Status:    string(result.Status),
		Output:    result.Output,
		Error:     result.Error,
		Duration:  result.Duration,
		Completed: result.Completed,
	}, true
}

// diffHandlerAdapter is in app_handlers.go

// GetVersion returns the current application version.
func (a *App) GetVersion() string {
	return a.config.Version
}

// AddSystemMessage adds a system message to the TUI chat.
func (a *App) AddSystemMessage(msg string) {
	if a.tui != nil {
		a.tui.AddSystemMessage(msg)
	}
}
