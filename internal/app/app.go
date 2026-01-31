package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gooner/internal/agent"
	"gooner/internal/audit"
	"gooner/internal/cache"
	"gooner/internal/chat"
	"gooner/internal/client"
	"gooner/internal/commands"
	"gooner/internal/config"
	appcontext "gooner/internal/context"
	"gooner/internal/contract"
	"gooner/internal/hooks"
	"gooner/internal/logging"
	"gooner/internal/permission"
	"gooner/internal/plan"
	"gooner/internal/ratelimit"
	"gooner/internal/router"
	"gooner/internal/semantic"
	"gooner/internal/tasks"
	"gooner/internal/tools"
	"gooner/internal/ui"
	"gooner/internal/undo"
	"gooner/internal/watcher"

	"google.golang.org/genai"
)

// SystemPrompt is the default system prompt for the assistant.
const SystemPrompt = `You are Gooner, an AI assistant for software development. You help users work with code by:
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

	// === IMPROVEMENT 4: Priority queue for task management ===
	queueManager *QueueManager

	// === PHASE 3: Advanced features ===
	dependencyManager *DependencyManager // Task dependencies and DAG execution
	parallelExecutor  *ParallelExecutor  // Parallel task execution

	// === PHASE 4: UI Auto-Update System ===
	uiUpdateManager           *UIUpdateManager // Coordinates periodic UI updates
	parallelExecutorCallbacks map[string]any   // UI callbacks for parallel executor

	// === PHASE 5: Agent System Improvements (6→10) ===
	coordinator       *agent.Coordinator       // Task orchestration
	agentTypeRegistry *agent.AgentTypeRegistry // Dynamic agent types
	strategyOptimizer *agent.StrategyOptimizer // Strategy learning
	metaAgent         *agent.MetaAgent         // Agent monitoring

	// === PHASE 6: Tree Planner ===
	treePlanner         *agent.TreePlanner // Tree-based planning
	planningModeEnabled bool               // toggle for planning mode

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

	// Create and run the program
	a.program = a.tui.GetProgram()

	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	// === PHASE 4: Initialize UI Auto-Update System ===
	a.initializeUIUpdateSystem()

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

	if a.program != nil {
		if err != nil {
			a.program.Send(ui.ErrorMsg(err))
		} else {
			// Display command result as assistant message
			a.program.Send(ui.StreamTextMsg(result))
		}
		a.program.Send(ui.ResponseDoneMsg{})
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
			if a.program != nil {
				a.program.Send(ui.StreamTextMsg("\n📤 Processing queued message...\n"))
			}
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
		if a.program != nil {
			a.program.Send(ui.ErrorMsg(err))
		}
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

	// Signal completion
	if a.program != nil {
		a.program.Send(ui.ResponseDoneMsg{})

		// Send response metadata
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
			if a.program != nil {
				a.program.Send(ui.TodoUpdateMsg(display))
			}
		}
	}
}

// executePlanWithClearContext dispatches plan execution to either delegated
// sub-agent mode or direct monolithic execution.
func (a *App) executePlanWithClearContext(ctx context.Context, approvedPlan *plan.Plan) {
	if a.config.Plan.DelegateSteps && a.agentRunner != nil {
		a.executePlanDelegated(ctx, approvedPlan)
	} else {
		a.executePlanDirectly(ctx, approvedPlan)
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

	// 1. Convert plan steps to PlanStepInfo
	steps := make([]appcontext.PlanStepInfo, 0, len(approvedPlan.Steps))
	for _, s := range approvedPlan.Steps {
		steps = append(steps, appcontext.PlanStepInfo{
			ID:          s.ID,
			Title:       s.Title,
			Description: s.Description,
		})
	}

	// 2. Build plan execution prompt
	planPrompt := a.promptBuilder.BuildPlanExecutionPrompt(
		approvedPlan.Title, approvedPlan.Description, steps)

	// 3. Clear session history
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

	// Auto-verify contract if enabled and plan has one
	a.verifyContractAfterPlan(ctx, approvedPlan)

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

	// Skip diff approval prompts for delegated plan execution —
	// the plan itself was already approved by the user.
	ctx = tools.ContextWithSkipDiff(ctx)

	// Build compact project context for sub-agents
	projectCtx := a.promptBuilder.BuildSubAgentPrompt()

	totalSteps := len(approvedPlan.Steps)

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

		// Mark step as started
		a.planManager.StartStep(step.ID)

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

		// Build step prompt with previous step context
		prevSummary := a.planManager.GetPreviousStepsSummary(step.ID, 500)
		stepPrompt := buildStepPrompt(step, prevSummary)

		// Stream sub-agent text to TUI
		onText := func(text string) {
			if a.program != nil {
				a.program.Send(ui.StreamTextMsg(text))
			}
		}

		// Spawn sub-agent for this step with retry on timeout
		var result *agent.AgentResult
		var err error
		const maxRetries = 2
		for attempt := 0; attempt < maxRetries; attempt++ {
			_, result, err = a.agentRunner.SpawnWithContext(
				ctx, "general", stepPrompt, 30, "", projectCtx, onText, true)

			// Only retry on timeout errors
			if err != nil && strings.Contains(err.Error(), "deadline exceeded") && attempt < maxRetries-1 {
				logging.Warn("sub-agent timeout, retrying step",
					"step_id", step.ID, "attempt", attempt+1)
				if a.program != nil {
					a.program.Send(ui.StreamTextMsg(
						fmt.Sprintf("\nStep %d timed out, retrying...\n", step.ID)))
				}
				continue
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

			// Preserve partial output even on failure
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

	// Auto-verify contract if enabled and plan has one
	a.verifyContractAfterPlan(ctx, approvedPlan)

	// Send final done message after verification
	if a.program != nil {
		a.program.Send(ui.ResponseDoneMsg{})
	}

	// Save session
	if a.sessionManager != nil {
		_ = a.sessionManager.SaveAfterMessage()
	}
}

// buildStepPrompt constructs the prompt for a single plan step sub-agent.
func buildStepPrompt(step *plan.Step, prevSummary string) string {
	var sb strings.Builder
	sb.WriteString("Execute this plan step:\n\n")
	sb.WriteString(fmt.Sprintf("## Step %d: %s\n", step.ID, step.Title))
	if step.Description != "" {
		sb.WriteString(step.Description)
		sb.WriteString("\n")
	}

	if prevSummary != "" {
		sb.WriteString("\n## Previous Steps (completed):\n")
		sb.WriteString(prevSummary)
	}

	sb.WriteString("\n## Rules:\n")
	sb.WriteString("- Read files before editing\n")
	sb.WriteString("- Execute exactly what the step describes\n")
	sb.WriteString("- Provide a brief summary of what was done\n")

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

// verifyContractAfterPlan runs contract verification after plan execution completes.
// Only runs if the plan has a contract and auto_verify is enabled.
func (a *App) verifyContractAfterPlan(ctx context.Context, executedPlan *plan.Plan) {
	// Check if plan has a contract
	if !executedPlan.HasContract() {
		return
	}

	// Check if auto-verify is enabled
	if !a.config.Contract.AutoVerify {
		return
	}

	// Get contract verifier
	verifier := a.planManager.GetContractVerifier()
	if verifier == nil {
		logging.Warn("contract verifier not available, skipping auto-verification")
		return
	}

	// Get contract from store if we have a ContractID
	var c *contract.Contract
	store := a.planManager.GetContractStore()
	if executedPlan.ContractID != "" && store != nil {
		loaded, err := store.Load(executedPlan.ContractID)
		if err != nil {
			logging.Warn("failed to load contract for verification", "contract_id", executedPlan.ContractID, "error", err)
			return
		}
		c = loaded
	} else if executedPlan.Contract != nil {
		// Create a temporary contract from the plan's contract spec
		c = &contract.Contract{
			ID:         executedPlan.ID, // Use plan ID as contract ID
			Name:       executedPlan.Contract.Name,
			Intent:     executedPlan.Contract.Intent,
			Boundaries: executedPlan.Contract.Boundaries,
			Invariants: executedPlan.Contract.Invariants,
			Examples:   executedPlan.Contract.Examples,
		}
	} else {
		return
	}

	// Send verification start message to UI
	if a.program != nil {
		a.program.Send(ui.StreamTextMsg(
			fmt.Sprintf("\n━━━ Verifying contract: %s ━━━\n", c.Name)))
	}

	// Run verification
	result, err := verifier.Verify(ctx, c)
	if err != nil {
		logging.Error("contract verification failed", "error", err)
		if a.program != nil {
			a.program.Send(ui.StreamTextMsg(
				fmt.Sprintf("  ❌ Verification error: %s\n", err.Error())))
		}
		return
	}

	// Format and display results
	a.displayContractVerificationResults(c, result)

	// Save verification result to contract
	if store != nil && c.ID != "" {
		c.LastVerification = result
		if err := store.Save(c); err != nil {
			logging.Warn("failed to save verification result", "error", err)
		}
	}
}

// displayContractVerificationResults formats and sends verification results to the UI.
func (a *App) displayContractVerificationResults(c *contract.Contract, result *contract.VerificationResult) {
	if a.program == nil {
		return
	}

	// Overall status
	statusIcon := "✅"
	statusText := "PASSED"
	if !result.Passed {
		statusIcon = "❌"
		statusText = "FAILED"
	}

	a.program.Send(ui.StreamTextMsg(
		fmt.Sprintf("  %s Contract verification: %s\n", statusIcon, statusText)))
	a.program.Send(ui.StreamTextMsg(
		fmt.Sprintf("  Duration: %s\n", result.Duration)))

	// Show example results
	if len(result.ExampleResults) > 0 {
		a.program.Send(ui.StreamTextMsg("\n  Examples:\n"))
		for _, er := range result.ExampleResults {
			icon := "✓"
			if !er.Passed {
				icon = "✗"
			}
			a.program.Send(ui.StreamTextMsg(
				fmt.Sprintf("    %s %s", icon, er.Name)))
			if !er.Passed && er.Error != "" {
				a.program.Send(ui.StreamTextMsg(
					fmt.Sprintf(" — %s\n", er.Error)))
			} else {
				a.program.Send(ui.StreamTextMsg("\n"))
			}
		}
	}

	// Show invariant results
	if len(result.InvariantResults) > 0 {
		a.program.Send(ui.StreamTextMsg("\n  Invariants:\n"))
		for _, ir := range result.InvariantResults {
			icon := "✓"
			if !ir.Passed {
				icon = "✗"
			}
			a.program.Send(ui.StreamTextMsg(
				fmt.Sprintf("    %s %s\n", icon, ir.Name)))
		}
	}

	// Show summary if available
	if result.Summary != "" {
		a.program.Send(ui.StreamTextMsg(
			fmt.Sprintf("\n  Summary: %s\n", result.Summary)))
	}

	a.program.Send(ui.StreamTextMsg("\n"))
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

// sendTokenUsageUpdate sends a token usage update to the UI.
// This can be called from any goroutine safely.
func (a *App) sendTokenUsageUpdate() {
	if a.program == nil || a.contextManager == nil || !a.config.UI.ShowTokenUsage {
		return
	}
	usage := a.contextManager.GetTokenUsage()
	if usage == nil {
		return
	}
	a.program.Send(ui.TokenUsageMsg{
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
	defer a.mu.Unlock()

	if a.permManager == nil {
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

	return newEnabled
}

// TogglePlanningMode toggles the tree planning mode on/off.
func (a *App) TogglePlanningMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.planningModeEnabled = !a.planningModeEnabled

	// Update agent runner
	if a.agentRunner != nil {
		a.agentRunner.SetPlanningModeEnabled(a.planningModeEnabled)
	}

	// Update TUI display
	if a.tui != nil {
		a.tui.SetPlanningModeEnabled(a.planningModeEnabled)
	}

	if a.planningModeEnabled {
		logging.Debug("planning mode enabled")
	} else {
		logging.Debug("planning mode disabled")
	}

	return a.planningModeEnabled
}

// IsPlanningModeEnabled returns whether planning mode is active.
func (a *App) IsPlanningModeEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planningModeEnabled
}

// ToggleSandbox toggles the bash sandbox mode on/off.
func (a *App) ToggleSandbox() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.config.Tools.Bash.Sandbox = !a.config.Tools.Bash.Sandbox
	newEnabled := a.config.Tools.Bash.Sandbox

	// Save config
	if err := a.config.Save(); err != nil {
		logging.Warn("failed to save sandbox setting", "error", err)
	}

	if newEnabled {
		logging.Debug("sandbox enabled")
	} else {
		logging.Debug("sandbox disabled")
	}

	return newEnabled
}

// GetSandboxState returns whether sandbox mode is enabled.
func (a *App) GetSandboxState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config.Tools.Bash.Sandbox
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

	// 8. Update UI state (model name, etc.)
	if a.tui != nil {
		a.tui.SetCurrentModel(a.config.Model.Name)
		a.tui.SetShowTokens(a.config.UI.ShowTokenUsage)
		a.tui.SetPermissionsEnabled(a.config.Permission.Enabled)
		a.tui.SetSandboxEnabled(a.config.Tools.Bash.Sandbox)
		a.tui.SetPlanningModeEnabled(a.planningModeEnabled)
	}

	// 8b. Send ConfigUpdateMsg to Bubbletea program to refresh UI
	if a.program != nil {
		a.program.Send(ui.ConfigUpdateMsg{
			PermissionsEnabled: a.config.Permission.Enabled,
			SandboxEnabled:     a.config.Tools.Bash.Sandbox,
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
