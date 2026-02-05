package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gokin/internal/agent"
	"gokin/internal/audit"
	"gokin/internal/cache"
	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/commands"
	"gokin/internal/config"
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

	"google.golang.org/genai"
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
	fileWatcher     *watcher.Watcher
	semanticIndexer *semantic.EnhancedIndexer

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

	// Only add system prompt if we didn't restore a session
	if !sessionRestored {
		// Generate dynamic system prompt
		systemPrompt := a.promptBuilder.Build()
		systemPrompt += modelEnhancement

		a.session.AddUserMessage(systemPrompt)
		a.session.AddModelMessage("I understand. I'm ready to help you with your code. What would you like to do?")
	} else if modelEnhancement != "" {
		// For restored sessions, inject a reminder about response quality
		a.session.AddUserMessage(modelEnhancement + "\n\n[Session restored. Remember to always provide detailed responses after using tools.]")
		a.session.AddModelMessage("Understood. I'll continue to provide detailed, helpful responses. What would you like me to help with?")
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

	// Start background indexing for semantic search if enabled
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

// processMessageWithContext processes a user message with the given context.
func (a *App) processMessageWithContext(ctx context.Context, message string) {
	defer func() {
		a.mu.Lock()
		a.processing = false
		a.mu.Unlock()

		// Check for pending message and process it
		a.pendingMu.Lock()
		pending := a.pendingMessage
		a.pendingMessage = ""
		a.pendingMu.Unlock()

		if pending != "" {
			// Notify user that we're processing pending message
			a.safeSendToProgram(ui.StreamTextMsg("\n📤 Processing queued message...\n"))
			// Recursively handle the pending message
			go a.handleSubmit(pending)
		}
	}()

	// Track response start time and reset tools used
	a.mu.Lock()
	a.responseStartTime = time.Now()
	a.responseToolsUsed = nil
	a.streamedChars = 0 // Reset streaming accumulator
	a.mu.Unlock()

	// Prepare context (check tokens, optimize if needed)
	if a.contextManager != nil {
		if err := a.contextManager.PrepareForRequest(ctx); err != nil {
			logging.Debug("failed to prepare context", "error", err)
		}

		// Send token usage to UI BEFORE request (after optimization)
		// This shows the actual context size that will be sent
		a.sendTokenUsageUpdate()
	}

	// Get current history
	history := a.session.GetHistory()

	// === IMPROVEMENT 1: Use Task Router for intelligent routing ===
	var newHistory []*genai.Content
	var response string
	var err error

	if a.taskRouter != nil {
		// Route the task intelligently
		newHistory, response, err = a.taskRouter.Execute(ctx, history, message)

		// Log routing decision for debugging
		if analysis := a.taskRouter.GetAnalysis(message); analysis != nil {
			logging.Debug("task routed",
				"complexity", analysis.Score,
				"type", analysis.Type,
				"strategy", analysis.Strategy,
				"reasoning", analysis.Reasoning)
		}
	} else {
		// Fallback to standard executor
		newHistory, response, err = a.executor.Execute(ctx, history, message)
	}

	if err != nil {
		a.safeSendToProgram(ui.ErrorMsg(err))
		return
	}

	// Update session history
	a.session.SetHistory(newHistory)

	// Check for context-clear request after plan approval
	if a.planManager != nil && a.planManager.IsContextClearRequested() {
		approvedPlan := a.planManager.ConsumeContextClearRequest()
		if approvedPlan != nil && a.config.Plan.ClearContext {
			a.executePlanWithClearContext(ctx, approvedPlan)
			return
		}
	}

	// Save session after each message
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Debug("failed to save session after message", "error", err)
		}
	}

	// Update token count after processing and send to UI
	if a.contextManager != nil {
		if err := a.contextManager.UpdateTokenCount(ctx); err != nil {
			logging.Debug("failed to update token count", "error", err)
		}

		// Send final token usage to UI
		a.sendTokenUsageUpdate()

		// Track cumulative token usage for /cost command
		usage := a.contextManager.GetTokenUsage()
		a.mu.Lock()
		a.totalInputTokens = usage.InputTokens
		// Use API usage metadata if available, otherwise estimate
		apiInput, apiOutput := a.executor.GetLastTokenUsage()
		if apiOutput > 0 {
			a.totalOutputTokens += apiOutput
		} else if response != "" {
			// Fallback: estimate output tokens from response length (approx 4 chars per token)
			a.totalOutputTokens += len(response) / 4
		}
		if apiInput > 0 {
			a.totalInputTokens = apiInput
		}
		a.mu.Unlock()
	}

	// Note: response text is already streamed via OnText callback in executor handler
	// Don't send it again here to avoid duplicate output
	_ = response // Used for token counting above

	// Signal completion - copy program reference under lock
	a.mu.Lock()
	program := a.program
	duration := time.Since(a.responseStartTime)
	toolsUsed := make([]string, len(a.responseToolsUsed))
	copy(toolsUsed, a.responseToolsUsed)
	inputTokens := a.totalInputTokens
	outputTokens := a.totalOutputTokens
	a.mu.Unlock()

	if program != nil {
		program.Send(ui.ResponseDoneMsg{})

		// Send response metadata
		program.Send(ui.ResponseMetadataMsg{
			Model:        a.config.Model.Name,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Duration:     duration,
			ToolsUsed:    toolsUsed,
		})
	}

	// Update todos display
	todoTool, ok := a.registry.Get("todo")
	if ok {
		if tt, ok := todoTool.(*tools.TodoTool); ok {
			items := tt.GetItems()
			var display []string
			for _, item := range items {
				var icon string
				switch item.Status {
				case "pending":
					icon = "○"
				case "in_progress":
					icon = "◐"
				case "completed":
					icon = "●"
				}
				display = append(display, fmt.Sprintf("%s %s", icon, item.Content))
			}
			if program != nil {
				program.Send(ui.TodoUpdateMsg(display))
			}
		}
	}
}

// executePlanWithClearContext dispatches plan execution to either delegated
// sub-agent mode or direct monolithic execution.
func (a *App) executePlanWithClearContext(ctx context.Context, approvedPlan *plan.Plan) {
	// Enter execution mode - this blocks creation of new plans during execution
	if a.planManager != nil {
		a.planManager.SetExecutionMode(true)
	}

	if a.agentRunner != nil {
		sharedMem := a.agentRunner.GetSharedMemory()
		if sharedMem != nil {
			completedCount := approvedPlan.CompletedCount()
			if completedCount == 0 {
				// Clear SharedMemory for fresh plan execution
				sharedMem.Clear()
				logging.Debug("shared memory cleared for new plan execution", "plan_id", approvedPlan.ID)
			} else {
				// Resuming plan: restore completed steps to SharedMemory
				a.restoreSharedMemoryFromPlan(sharedMem, approvedPlan)
			}
		}
	}

	if a.config.Plan.DelegateSteps && a.agentRunner != nil {
		a.executePlanDelegated(ctx, approvedPlan)
	} else {
		a.executePlanDirectly(ctx, approvedPlan)
	}
}

// restoreSharedMemoryFromPlan repopulates SharedMemory with results from completed steps.
// This is used when resuming a plan to give sub-agents access to previous step results.
func (a *App) restoreSharedMemoryFromPlan(sharedMem *agent.SharedMemory, p *plan.Plan) {
	steps := p.GetStepsSnapshot()
	restored := 0
	for _, step := range steps {
		if step.Status == plan.StatusCompleted && step.Output != "" {
			sharedMem.Write(
				fmt.Sprintf("step_%d_result", step.ID),
				map[string]string{
					"title":  step.Title,
					"output": step.Output,
				},
				agent.SharedEntryTypeFact,
				fmt.Sprintf("plan_step_%d", step.ID),
			)
			restored++
		}
	}
	if restored > 0 {
		logging.Debug("shared memory restored from completed steps",
			"plan_id", p.ID, "steps_restored", restored)
	}
}

// executePlanDirectly clears the conversation context and re-executes
// with a focused plan execution prompt after a plan is approved.
// This is the original monolithic execution path.
func (a *App) executePlanDirectly(ctx context.Context, approvedPlan *plan.Plan) {
	logging.Debug("executing plan directly (monolithic)",
		"plan_id", approvedPlan.ID,
		"title", approvedPlan.Title,
		"steps", approvedPlan.StepCount())

	// Ensure execution mode is reset on any exit path (including panics and early returns)
	defer func() {
		if a.planManager != nil {
			a.planManager.SetExecutionMode(false)
		}
	}()

	// 1. Save context snapshot before clearing (preserves planning decisions)
	contextSnapshot := a.extractContextSnapshot()
	if contextSnapshot != "" {
		approvedPlan.SetContextSnapshot(contextSnapshot)
		logging.Debug("context snapshot saved", "plan_id", approvedPlan.ID, "snapshot_len", len(contextSnapshot))
	}

	// 2. Convert plan steps to PlanStepInfo
	steps := make([]appcontext.PlanStepInfo, 0, len(approvedPlan.Steps))
	for _, s := range approvedPlan.Steps {
		steps = append(steps, appcontext.PlanStepInfo{
			ID:          s.ID,
			Title:       s.Title,
			Description: s.Description,
		})
	}

	// 3. Build plan execution prompt (includes context snapshot if available)
	planPrompt := a.promptBuilder.BuildPlanExecutionPromptWithContext(
		approvedPlan.Title, approvedPlan.Description, steps, contextSnapshot)

	// 3b. Save plan to persistent storage before clearing session
	// This ensures plan can be resumed if app crashes during execution
	if a.planManager != nil {
		if err := a.planManager.SaveCurrentPlan(); err != nil {
			logging.Warn("failed to save plan before execution", "error", err)
		}
	}

	// 4. Clear session history
	a.session.Clear()

	// 4. Inject plan context as system prompt
	a.session.AddUserMessage(planPrompt)
	a.session.AddModelMessage("I understand the approved plan. Executing step 1 now.")

	// 5. Notify UI about context clear
	if a.program != nil {
		a.program.Send(ui.StreamTextMsg("\n--- Context cleared for plan execution ---\n"))
	}

	// 6. Execute via standard executor (bypass taskRouter for focused execution)
	history := a.session.GetHistory()
	executeMsg := "Begin executing the plan now. Start with step 1."
	newHistory, response, err := a.executor.Execute(ctx, history, executeMsg)
	if err != nil {
		if a.program != nil {
			a.program.Send(ui.ErrorMsg(err))
		}
		return
	}

	// 7. Update session
	a.session.SetHistory(newHistory)

	// 8. Save session after plan execution
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Debug("failed to save session after plan execution", "error", err)
		}
	}

	// 9. Update token count and send to UI
	if a.contextManager != nil {
		if err := a.contextManager.UpdateTokenCount(ctx); err != nil {
			logging.Debug("failed to update token count", "error", err)
		}
		a.sendTokenUsageUpdate()

		usage := a.contextManager.GetTokenUsage()
		a.mu.Lock()
		a.totalInputTokens = usage.InputTokens
		// Use API usage metadata if available, otherwise estimate
		apiInput, apiOutput := a.executor.GetLastTokenUsage()
		if apiOutput > 0 {
			a.totalOutputTokens += apiOutput
		} else if response != "" {
			a.totalOutputTokens += len(response) / 4
		}
		if apiInput > 0 {
			a.totalInputTokens = apiInput
		}
		a.mu.Unlock()
	}

	_ = response // Used for token counting above

	// 10. Signal completion
	if a.program != nil {
		a.program.Send(ui.ResponseDoneMsg{})
	}

	// Note: SetExecutionMode(false) is handled by defer at function start

	// Send final metadata after verification
	if a.program != nil {
		a.mu.Lock()
		duration := time.Since(a.responseStartTime)
		toolsUsed := make([]string, len(a.responseToolsUsed))
		copy(toolsUsed, a.responseToolsUsed)
		inputTokens := a.totalInputTokens
		outputTokens := a.totalOutputTokens
		a.mu.Unlock()

		a.program.Send(ui.ResponseMetadataMsg{
			Model:        a.config.Model.Name,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Duration:     duration,
			ToolsUsed:    toolsUsed,
		})
	}
}

// executePlanDelegated executes an approved plan by spawning a sub-agent per step.
// Each step runs in isolation with project context injected, and only compact
// summaries are stored in the main session.
func (a *App) executePlanDelegated(ctx context.Context, approvedPlan *plan.Plan) {
	logging.Debug("executing plan via sub-agent delegation",
		"plan_id", approvedPlan.ID,
		"title", approvedPlan.Title,
		"steps", approvedPlan.StepCount())

	// Ensure execution mode is reset on any exit path (including panics and early returns)
	defer func() {
		if a.planManager != nil {
			a.planManager.SetExecutionMode(false)
			a.planManager.SetCurrentStepID(-1)
		}
	}()

	// Skip diff approval prompts for delegated plan execution —
	// the plan itself was already approved by the user.
	ctx = tools.ContextWithSkipDiff(ctx)

	// Build compact project context for sub-agents
	projectCtx := a.promptBuilder.BuildSubAgentPrompt()

	totalSteps := len(approvedPlan.Steps)

	// Get SharedMemory for inter-step communication
	var sharedMem *agent.SharedMemory
	if a.agentRunner != nil {
		sharedMem = a.agentRunner.GetSharedMemory()
	}

	// Save context snapshot if not already present (e.g., first execution, not resume)
	// Priority: 1) SharedMemory structured snapshot, 2) Plan string snapshot, 3) Extract new
	contextSnapshot := ""

	// First, try to get structured snapshot from SharedMemory
	if sharedMem != nil {
		if formattedSnapshot := sharedMem.GetContextSnapshotForPrompt(); formattedSnapshot != "" {
			contextSnapshot = formattedSnapshot
			logging.Debug("using structured context snapshot from shared memory",
				"plan_id", approvedPlan.ID, "snapshot_len", len(contextSnapshot))
		}
	}

	// Fall back to plan's string snapshot
	if contextSnapshot == "" {
		contextSnapshot = approvedPlan.GetContextSnapshot()
	}

	// If still empty and first execution, extract new snapshot
	if contextSnapshot == "" && approvedPlan.CompletedCount() == 0 {
		contextSnapshot = a.extractContextSnapshot()
		if contextSnapshot != "" {
			approvedPlan.SetContextSnapshot(contextSnapshot)
			logging.Debug("context snapshot saved for delegated plan",
				"plan_id", approvedPlan.ID, "snapshot_len", len(contextSnapshot))
		}
	}

	// Notify UI with plan banner
	if a.program != nil {
		a.program.Send(ui.StreamTextMsg(
			fmt.Sprintf("\n━━━ Executing plan: %s (%d steps) ━━━\n\n", approvedPlan.Title, totalSteps)))
	}

	for _, step := range approvedPlan.Steps {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Skip non-pending steps
		if step.Status != plan.StatusPending {
			continue
		}

		// Mark step as started and track current step ID
		a.planManager.StartStep(step.ID)
		a.planManager.SetCurrentStepID(step.ID)

		// Update plan progress in status bar
		if a.program != nil {
			a.program.Send(ui.PlanProgressMsg{
				PlanID:        approvedPlan.ID,
				CurrentStepID: step.ID,
				CurrentTitle:  step.Title,
				TotalSteps:    totalSteps,
				Completed:     approvedPlan.CompletedCount(),
				Progress:      approvedPlan.Progress(),
				Status:        "in_progress",
			})

			// Notify UI of step start with structured header
			header := fmt.Sprintf("──── Step %d/%d: %s ────\n", step.ID, totalSteps, step.Title)
			a.program.Send(ui.StreamTextMsg(header))
		}

		// Build step prompt with full plan context
		prevSummary := a.planManager.GetPreviousStepsSummary(step.ID, 2000)

		// Get SharedMemory context for this sub-agent
		sharedMemCtx := ""
		if sharedMem != nil {
			sharedMemCtx = sharedMem.GetForContext(fmt.Sprintf("plan_step_%d", step.ID), 20)
		}

		stepPrompt := buildStepPrompt(&StepPromptContext{
			Step:            step,
			PrevSummary:     prevSummary,
			PlanTitle:       approvedPlan.Title,
			PlanDescription: approvedPlan.Description,
			PlanRequest:     approvedPlan.Request,
			ContextSnapshot: contextSnapshot,
			SharedMemoryCtx: sharedMemCtx,
			TotalSteps:      totalSteps,
			CompletedCount:  approvedPlan.CompletedCount(),
		})

		// Stream sub-agent text to TUI
		onText := func(text string) {
			if a.program != nil {
				a.program.Send(ui.StreamTextMsg(text))
			}
		}

		// Spawn sub-agent for this step with retry on retryable errors
		var result *agent.AgentResult
		var err error
		const maxRetries = 3
		backoffDurations := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

		for attempt := 0; attempt < maxRetries; attempt++ {
			_, result, err = a.agentRunner.SpawnWithContext(
				ctx, "general", stepPrompt, 30, "", projectCtx, onText, true)

			// Retry on retryable errors (timeout, network, rate limit, etc.)
			if err != nil && isRetryableError(err) && attempt < maxRetries-1 {
				backoff := backoffDurations[attempt]
				logging.Warn("sub-agent error, retrying step",
					"step_id", step.ID, "attempt", attempt+1, "error", err.Error(), "backoff", backoff)
				if a.program != nil {
					a.program.Send(ui.StreamTextMsg(
						fmt.Sprintf("\n⚠️ Step %d failed (attempt %d/%d): %s\nRetrying in %v...\n",
							step.ID, attempt+1, maxRetries, err.Error(), backoff)))
				}

				// Wait with backoff, but respect context cancellation
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					err = ctx.Err()
					break
				}
			}
			break
		}

		if err != nil || result == nil || result.Status == agent.AgentStatusFailed {
			errMsg := "unknown error"
			if err != nil {
				errMsg = err.Error()
			} else if result != nil {
				errMsg = result.Error
			}

			// Check if this is a retryable error after all retries exhausted
			if err != nil && isRetryableError(err) {
				// Pause the step instead of failing — user can resume later
				a.planManager.PauseStep(step.ID, errMsg)

				if a.program != nil {
					a.program.Send(ui.StreamTextMsg(
						fmt.Sprintf("\n⏸ Step %d paused after %d attempts: %s\n"+
							"Use /resume-plan to continue when ready.\n",
							step.ID, maxRetries, errMsg)))
					a.program.Send(ui.PlanProgressMsg{
						PlanID:        approvedPlan.ID,
						CurrentStepID: step.ID,
						CurrentTitle:  step.Title,
						TotalSteps:    totalSteps,
						Completed:     approvedPlan.CompletedCount(),
						Progress:      approvedPlan.Progress(),
						Status:        "paused",
					})
					a.program.Send(ui.ResponseDoneMsg{})
				}

				logging.Info("plan paused due to retryable error",
					"step_id", step.ID, "error", errMsg)
				return // Exit but don't mark as failed — can be resumed
			}

			// Non-retryable error: preserve partial output if available
			if result != nil && result.Output != "" {
				a.planManager.CompleteStep(step.ID, "(partial) "+result.Output)
				logging.Debug("step failed but partial output preserved",
					"step_id", step.ID, "output_len", len(result.Output))
			} else {
				a.planManager.FailStep(step.ID, errMsg)
			}

			if a.program != nil {
				a.program.Send(ui.StreamTextMsg(
					fmt.Sprintf("\n  Step %d failed: %s\n", step.ID, errMsg)))
			}

			if a.config.Plan.AbortOnStepFailure {
				if a.program != nil {
					a.program.Send(ui.StreamTextMsg("Aborting plan due to step failure.\n"))
				}
				break
			}
			continue
		}

		// Store compact output in step and mark complete
		output := result.Output
		if len(output) > 2000 {
			output = output[:2000] + "..."
		}
		a.planManager.CompleteStep(step.ID, output)

		// Store step result in SharedMemory for inter-step communication
		if sharedMem != nil {
			// Store the step output as a fact for other steps to reference
			sharedMem.Write(
				fmt.Sprintf("step_%d_result", step.ID),
				map[string]string{
					"title":  step.Title,
					"output": output,
				},
				agent.SharedEntryTypeFact,
				fmt.Sprintf("plan_step_%d", step.ID),
			)
			logging.Debug("step result stored in shared memory",
				"step_id", step.ID, "output_len", len(output))
		}

		if a.program != nil {
			a.program.Send(ui.StreamTextMsg(
				fmt.Sprintf("  Step %d complete\n\n", step.ID)))
			a.program.Send(ui.PlanProgressMsg{
				PlanID:        approvedPlan.ID,
				CurrentStepID: step.ID,
				CurrentTitle:  step.Title,
				TotalSteps:    totalSteps,
				Completed:     approvedPlan.CompletedCount(),
				Progress:      approvedPlan.Progress(),
				Status:        "in_progress",
			})
		}
	}

	// Signal plan completion
	if a.program != nil {
		completedCount := approvedPlan.CompletedCount()
		a.program.Send(ui.StreamTextMsg(
			fmt.Sprintf("\n━━━ Plan complete: %d/%d steps done ━━━\n", completedCount, totalSteps)))
		a.program.Send(ui.PlanProgressMsg{
			PlanID:     approvedPlan.ID,
			TotalSteps: totalSteps,
			Completed:  completedCount,
			Progress:   approvedPlan.Progress(),
			Status:     "completed",
		})
		a.program.Send(ui.ResponseDoneMsg{})
	}

	// Note: SetExecutionMode(false) is handled by defer at function start

	// Save session
	if a.sessionManager != nil {
		_ = a.sessionManager.SaveAfterMessage()
	}
}

// isRetryableError checks if an error is retryable (network, timeout, rate limit).
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	retryable := []string{
		"deadline exceeded",
		"timeout",
		"connection refused",
		"connection reset",
		"temporary failure",
		"rate limit",
		"503",
		"502",
		"429",
		"network",
		"eof",
		"context canceled",
		"i/o timeout",
		"no such host",
		"tls handshake",
	}
	for _, pattern := range retryable {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// buildStepPrompt constructs the prompt for a single plan step sub-agent.
// StepPromptContext holds context for building step prompts.
type StepPromptContext struct {
	Step            *plan.Step
	PrevSummary     string
	PlanTitle       string
	PlanDescription string
	PlanRequest     string
	ContextSnapshot string
	SharedMemoryCtx string
	TotalSteps      int
	CompletedCount  int
}

func buildStepPrompt(ctx *StepPromptContext) string {
	var sb strings.Builder

	// Plan overview (helps sub-agent understand the overall goal)
	sb.WriteString("# Plan Execution Context\n\n")
	sb.WriteString(fmt.Sprintf("**Plan:** %s\n", ctx.PlanTitle))
	if ctx.PlanDescription != "" {
		sb.WriteString(fmt.Sprintf("**Goal:** %s\n", ctx.PlanDescription))
	}
	if ctx.PlanRequest != "" && len(ctx.PlanRequest) < 500 {
		sb.WriteString(fmt.Sprintf("**Original Request:** %s\n", ctx.PlanRequest))
	}
	sb.WriteString(fmt.Sprintf("**Progress:** Step %d of %d (%d completed)\n\n",
		ctx.Step.ID, ctx.TotalSteps, ctx.CompletedCount))

	// Context from planning discussion (key decisions)
	if ctx.ContextSnapshot != "" {
		sb.WriteString("## Key Decisions from Planning\n")
		sb.WriteString(ctx.ContextSnapshot)
		sb.WriteString("\n")
	}

	// Shared memory from previous steps (inter-agent knowledge)
	if ctx.SharedMemoryCtx != "" {
		sb.WriteString(ctx.SharedMemoryCtx)
	}

	// Current step details
	sb.WriteString(fmt.Sprintf("## Current Step %d: %s\n", ctx.Step.ID, ctx.Step.Title))
	if ctx.Step.Description != "" {
		sb.WriteString(ctx.Step.Description)
		sb.WriteString("\n")
	}

	// Previous steps summary (compact)
	if ctx.PrevSummary != "" {
		sb.WriteString("\n## Previous Steps Summary\n")
		sb.WriteString(ctx.PrevSummary)
	}

	sb.WriteString("\n## Execution Rules\n")
	sb.WriteString("- Read files before editing\n")
	sb.WriteString("- Execute exactly what this step describes\n")
	sb.WriteString("- Build upon work from previous steps\n")
	sb.WriteString("- Provide a brief summary of what was done\n")
	sb.WriteString("- Report any issues or deviations from the plan\n")

	return sb.String()
}

// Handlers are in app_handlers.go

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

// extractContextSnapshot creates a summary of the current session context.
// This preserves key decisions and findings from the planning conversation.
// It also creates a structured ContextSnapshot and saves it to SharedMemory.
func (a *App) extractContextSnapshot() string {
	history := a.session.GetHistory()
	if len(history) < 4 {
		return "" // Not enough context to summarize
	}

	// Create structured snapshot for SharedMemory
	snapshot := agent.NewContextSnapshot()

	var sb strings.Builder
	sb.WriteString("## Context from Planning Discussion\n\n")

	// Extract key points from recent messages (skip system prompt)
	messageCount := 0
	maxMessages := 6 // Last 3 turns (6 messages)

	for i := len(history) - 1; i >= 0 && messageCount < maxMessages; i-- {
		content := history[i]
		if content == nil || len(content.Parts) == 0 {
			continue
		}

		role := "User"
		if content.Role == "model" {
			role = "Assistant"
		}

		// Extract text content from parts
		for _, part := range content.Parts {
			if part != nil && part.Text != "" {
				text := part.Text

				// Extract structured information from assistant messages
				if content.Role == "model" {
					a.extractSnapshotFromText(snapshot, text)
				} else {
					// User messages often contain requirements
					a.extractRequirementsFromText(snapshot, text)
				}

				// Truncate long messages for string output
				if len(text) > 500 {
					text = text[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf("**%s**: %s\n\n", role, text))
				messageCount++
				break
			}
		}

		// Also check for function calls (tool results) to extract key files
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil {
				a.extractKeyFilesFromToolResult(snapshot, part.FunctionResponse)
			}
		}
	}

	// Save structured snapshot to SharedMemory
	if a.agentRunner != nil {
		if sharedMem := a.agentRunner.GetSharedMemory(); sharedMem != nil {
			sharedMem.SaveContextSnapshot(snapshot, "planning_phase")
			logging.Debug("structured context snapshot saved to shared memory",
				"key_files", len(snapshot.KeyFiles),
				"discoveries", len(snapshot.Discoveries),
				"requirements", len(snapshot.Requirements),
				"decisions", len(snapshot.Decisions))
		}
	}

	return sb.String()
}

// extractSnapshotFromText extracts structured information from assistant text.
func (a *App) extractSnapshotFromText(snapshot *agent.ContextSnapshot, text string) {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)

		// Look for decisions (architectural patterns)
		if strings.Contains(lower, "decided") || strings.Contains(lower, "will use") ||
			strings.Contains(lower, "approach:") || strings.Contains(lower, "решено") ||
			strings.Contains(lower, "использ") {
			if len(line) > 20 && len(line) < 300 {
				snapshot.AddDecision(line)
			}
		}

		// Look for discoveries
		if strings.Contains(lower, "found") || strings.Contains(lower, "discovered") ||
			strings.Contains(lower, "noticed") || strings.Contains(lower, "обнаруж") ||
			strings.Contains(lower, "нашёл") || strings.Contains(lower, "нашел") {
			if len(line) > 20 && len(line) < 300 {
				snapshot.AddDiscovery(line)
			}
		}

		// Look for error patterns
		if strings.Contains(lower, "error:") || strings.Contains(lower, "failed:") ||
			strings.Contains(lower, "ошибка:") {
			if len(line) > 10 && len(line) < 200 {
				// Try to extract error pattern and add with empty solution for now
				snapshot.ErrorPatterns[line] = ""
			}
		}
	}
}

// extractRequirementsFromText extracts requirements from user text.
func (a *App) extractRequirementsFromText(snapshot *agent.ContextSnapshot, text string) {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)

		// Look for requirements/constraints
		if strings.Contains(lower, "must") || strings.Contains(lower, "should") ||
			strings.Contains(lower, "need") || strings.Contains(lower, "require") ||
			strings.Contains(lower, "должен") || strings.Contains(lower, "нужно") ||
			strings.Contains(lower, "требован") {
			if len(line) > 15 && len(line) < 300 {
				snapshot.AddRequirement(line)
			}
		}
	}
}

// extractKeyFilesFromToolResult extracts key files from tool results.
func (a *App) extractKeyFilesFromToolResult(snapshot *agent.ContextSnapshot, fr *genai.FunctionResponse) {
	if fr == nil || fr.Name != "read" {
		return
	}

	// fr.Response is map[string]any - try to extract file path
	if fr.Response != nil {
		if path, ok := fr.Response["file_path"].(string); ok {
			// Add file with a placeholder summary (will be enriched later)
			if _, exists := snapshot.KeyFiles[path]; !exists {
				snapshot.KeyFiles[path] = "read during planning"
			}
		}
	}
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

	// Re-initialize with system prompt
	systemPrompt := a.promptBuilder.Build()
	a.session.AddUserMessage(systemPrompt)
	a.session.AddModelMessage("I understand. I'm ready to help you with your code. What would you like to do?")
}

// CompactContextWithPlan clears the conversation and injects the plan summary.
// This is called when a plan is approved to free up context space.
func (a *App) CompactContextWithPlan(planSummary string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Clear the session
	a.session.Clear()

	// Re-initialize with system prompt
	systemPrompt := a.promptBuilder.Build()
	a.session.AddUserMessage(systemPrompt)

	// Inject the plan summary as context
	if planSummary != "" {
		a.session.AddModelMessage(fmt.Sprintf("I've analyzed the task and created a plan. Here's the summary:\n\n%s\n\nI'll now execute this plan step by step.", planSummary))
	} else {
		a.session.AddModelMessage("I understand. I'm ready to execute the plan.")
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

// buildModelEnhancement returns model-specific prompt enhancements.
// Different models have different tendencies - some forget instructions more easily.
func (a *App) buildModelEnhancement() string {
	modelName := a.config.Model.Name

	// GLM models (ZhipuAI) tend to forget instructions
	if strings.HasPrefix(modelName, "glm") {
		return `

════════════════════════════════════════════════════════════════════
                    GLM MODEL RESPONSE REQUIREMENTS
════════════════════════════════════════════════════════════════════

**YOU ARE USING GLM MODEL. FOLLOW THESE RULES STRICTLY:**

After calling ANY function (read, grep, glob, bash, etc.), you MUST:

1. **ANALYZE** - Look at the tool results carefully
2. **SUMMARIZE** - Extract key information from results
3. **EXPLAIN** - Tell the user what you found
4. **RECOMMEND** - Suggest next steps

**RESPONSE FORMAT:**
` + "```" + `
## What I Found
[Summarize tool results]

## Key Points
- Point 1
- Point 2

## Recommendations
[Suggest next steps]
` + "```" + `

**EXAMPLE:**

User: "What files are here?"
❌ WRONG: [run glob, stop]
✅ CORRECT: "Here are the files I found:
- **Source files**: main.go, handler.go, service.go
- **Config**: config.yaml
- **Tests**: main_test.go
The project appears to be a Go web server. Want me to read any specific file?"

**CRITICAL**: NEVER use tools and then stop without providing analysis!
════════════════════════════════════════════════════════════════════
`
	}

	// Gemini Flash models - faster but may give shorter responses
	if strings.Contains(modelName, "flash") {
		return `

**RESPONSE QUALITY REMINDER (Flash Model):**
While being fast, ensure your responses are still:
- Detailed enough to be useful
- Include specific file:line references
- Provide concrete recommendations
Don't sacrifice quality for speed.
`
	}

	// Gemini Pro/Ultra - generally good, but reminder doesn't hurt
	if strings.Contains(modelName, "pro") || strings.Contains(modelName, "ultra") {
		return `

**RESPONSE QUALITY STANDARD:**
You're using a capable model. Provide responses that:
- Thoroughly analyze tool results
- Give specific code references (file:line)
- Explain WHY things are the way they are
- Suggest concrete next steps
`
	}

	// Default for unknown models
	return ""
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
