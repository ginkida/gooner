package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gokin/internal/agent"
	"gokin/internal/audit"
	"gokin/internal/cache"
	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/commands"
	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/git"
	"gokin/internal/hooks"
	"gokin/internal/logging"
	"gokin/internal/mcp"
	"gokin/internal/memory"
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

// Builder provides a fluent interface for constructing App instances.
// This breaks up the massive New() function and makes dependency injection clearer.
type Builder struct {
	cfg     *config.Config
	workDir string
	ctx     context.Context
	cancel  context.CancelFunc

	// Optional components (nil means not configured)
	configDir        string
	configDirErr     error
	geminiClient     client.Client
	registry         *tools.Registry
	executor         *tools.Executor
	session          *chat.Session
	tuiModel         *ui.Model
	projectInfo      *appcontext.ProjectInfo
	projectMemory    *appcontext.ProjectMemory
	promptBuilder    *appcontext.PromptBuilder
	contextManager   *appcontext.ContextManager
	permManager      *permission.Manager
	planManager      *plan.Manager
	hooksManager     *hooks.Manager
	taskManager      *tasks.Manager
	undoManager      *undo.Manager
	agentRunner      *agent.Runner
	commandHandler   *commands.Handler
	searchCache      *cache.SearchCache
	rateLimiter      *ratelimit.Limiter
	auditLogger      *audit.Logger
	fileWatcher      *watcher.Watcher
	semanticIdx      *semantic.EnhancedIndexer
	incrementalIdx   *semantic.IncrementalIndexer
	backgroundIdx    *semantic.BackgroundIndexer
	taskRouter       *router.Router
	taskOrchestrator *TaskOrchestrator // Unified Task Orchestrator

	// Phase 4: UI Auto-Update System
	uiUpdateManager *UIUpdateManager

	// Phase 5: Agent System Improvements (6→10)
	agentTypeRegistry *agent.AgentTypeRegistry
	strategyOptimizer *agent.StrategyOptimizer
	metaAgent         *agent.MetaAgent
	coordinator       *agent.Coordinator

	// Phase 6: Tree Planner
	treePlanner *agent.TreePlanner

	// Phase 7: Delegation Metrics (adaptive delegation rules)
	delegationMetrics *agent.DelegationMetrics

	// Phase 2: Learning infrastructure
	sharedMemory    *agent.SharedMemory
	exampleStore    *memory.ExampleStore
	promptOptimizer *agent.PromptOptimizer
	smartRouter     *router.SmartRouter

	// Session persistence
	sessionManager *chat.SessionManager

	// MCP (Model Context Protocol)
	mcpManager   *mcp.Manager
	contextAgent *appcontext.ContextAgent

	// Context Predictor (predictive file loading)
	contextPredictor *appcontext.ContextPredictor

	// For error collection during build
	buildErrors []error
	mu          sync.Mutex

	// Cached app instance (created once, reused)
	cachedApp *App
}

// NewBuilder creates a new Builder with the given config and work directory.
func NewBuilder(cfg *config.Config, workDir string) *Builder {
	ctx, cancel := context.WithCancel(context.Background())

	return &Builder{
		cfg:         cfg,
		workDir:     workDir,
		ctx:         ctx,
		cancel:      cancel,
		buildErrors: make([]error, 0),
	}
}

// Build constructs the App instance, returning any errors encountered.
func (b *Builder) Build() (*App, error) {
	// Initialize core components
	if err := b.initConfigDir(); err != nil {
		b.addError(err)
	}
	// Check allowed directories BEFORE creating tools and validators
	// This ensures permissions are loaded before PathValidator is created
	if err := b.checkAllowedDirs(); err != nil {
		b.addError(err)
	}
	if err := b.initClient(); err != nil {
		b.addError(err)
		return nil, b.finalizeError()
	}
	// Validate Ollama model availability (auto-pull if missing)
	if err := b.validateOllamaModel(); err != nil {
		b.addError(err)
		return nil, b.finalizeError()
	}
	if err := b.initTools(); err != nil {
		b.addError(err)
	}
	if err := b.initSession(); err != nil {
		b.addError(err)
	}
	if err := b.initManagers(); err != nil {
		b.addError(err)
	}
	if err := b.initIntegrations(); err != nil {
		b.addError(err)
	}
	if err := b.initUI(); err != nil {
		b.addError(err)
	}
	if err := b.initUIUpdateSystem(); err != nil {
		b.addError(err)
	}
	if err := b.wireDependencies(); err != nil {
		b.addError(err)
		return nil, b.finalizeError()
	}

	return b.assembleApp(), nil
}

// initConfigDir initializes the configuration directory.
func (b *Builder) initConfigDir() error {
	configDir, err := appcontext.GetConfigDir()
	b.configDir = configDir
	b.configDirErr = err
	if err != nil {
		logging.Debug("failed to get config dir", "error", err)
		// Don't fail - continue with optional features disabled
	}
	return nil
}

// checkAllowedDirs checks if additional directories should be allowed
// and prompts the user on first run. This happens BEFORE tool creation
// so that PathValidator gets the correct allowed directories.
func (b *Builder) checkAllowedDirs() error {
	// Skip if allowed_dirs is already configured
	if len(b.cfg.Tools.AllowedDirs) > 0 {
		return nil
	}

	// Get home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil // Skip if we can't get home dir
	}

	// First run - prompt about allowing home directory
	fmt.Printf("\nAllowed directories setup (first run)\n\n")
	fmt.Printf("   Current working directory: %s\n", b.workDir)
	fmt.Printf("   AI can only access the working directory.\n\n")
	fmt.Printf("   Do you want to allow access to home directory (%s)?\n", homeDir)
	fmt.Printf("   This will allow AI to work with files in any of your projects.\n\n")
	fmt.Printf("   [1] Yes, allow access to home directory\n")
	fmt.Printf("   [2] No, allow only current directory\n")
	fmt.Printf("   [3] Specify another directory\n\n")
	fmt.Printf("   Choice [1/2/3]: ")

	var response string
	fmt.Scanln(&response)
	response = strings.TrimSpace(strings.ToLower(response))

	var dirToAdd string
	switch response {
	case "1", "y", "yes":
		dirToAdd = homeDir
	case "3":
		fmt.Printf("   Enter path: ")
		fmt.Scanln(&dirToAdd)
		dirToAdd = strings.TrimSpace(dirToAdd)
		if dirToAdd == "" {
			// User cancelled - save workDir to prevent re-prompting
			dirToAdd = b.workDir
			fmt.Printf("   Only working directory allowed.\n")
		} else if strings.HasPrefix(dirToAdd, "~/") {
			// Expand ~ if present
			dirToAdd = filepath.Join(homeDir, dirToAdd[2:])
		}
	default:
		// "2" or any other - save workDir to prevent re-prompting
		dirToAdd = b.workDir
		fmt.Printf("   Only current working directory allowed.\n")
	}

	if b.cfg.AddAllowedDir(dirToAdd) {
		logging.Debug("saving config", "path", config.GetConfigPath(), "allowed_dir", dirToAdd)
		if err := b.cfg.Save(); err != nil {
			fmt.Printf("   Failed to save: %s\n", err)
			fmt.Printf("   Directory added only for current session.\n\n")
			logging.Error("failed to save config", "error", err)
		} else {
			fmt.Printf("   Added: %s\n", dirToAdd)
			fmt.Printf("   Saved to: %s\n\n", config.GetConfigPath())
			logging.Debug("config saved successfully", "allowed_dirs", b.cfg.Tools.AllowedDirs)
		}
	} else {
		fmt.Printf("   Directory already in allowed list.\n\n")
	}

	return nil
}

// validateOllamaModel checks if the configured model is available.
// Called only when backend is "ollama".
func (b *Builder) validateOllamaModel() error {
	// Skip if not Ollama backend
	if b.cfg.API.Backend != "ollama" {
		return nil
	}

	ollamaClient, ok := b.geminiClient.(*client.OllamaClient)
	if !ok {
		return nil
	}

	modelName := b.cfg.Model.Name
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()

	available, err := ollamaClient.IsModelAvailable(ctx, modelName)
	if err != nil {
		// Server unavailable — skip validation, error will appear later
		logging.Debug("ollama healthcheck failed, skipping model validation", "error", err)
		return nil
	}

	if available {
		return nil
	}

	// Model not found — ask user
	return b.promptModelPull(ollamaClient, modelName)
}

// promptModelPull asks user to download a missing model.
func (b *Builder) promptModelPull(c *client.OllamaClient, modelName string) error {
	fmt.Printf("\nModel '%s' is not installed.\n\n", modelName)
	fmt.Printf("Would you like to download it now? [Y/n] ")

	var response string
	fmt.Scanln(&response)
	response = strings.TrimSpace(strings.ToLower(response))

	if response != "" && response != "y" && response != "yes" {
		return fmt.Errorf("model '%s' is not available. Run: ollama pull %s", modelName, modelName)
	}

	fmt.Printf("\nDownloading %s...\n", modelName)

	err := c.PullModel(b.ctx, modelName, func(p client.PullProgress) {
		if p.Total > 0 {
			fmt.Printf("\r  %s: %.1f%%    ", p.Status, p.Percent)
		} else {
			fmt.Printf("\r  %s...    ", p.Status)
		}
	})

	if err != nil {
		return fmt.Errorf("failed to download model: %w", err)
	}

	fmt.Printf("\n\nModel '%s' downloaded successfully!\n\n", modelName)
	return nil
}

// initClient creates the appropriate API client based on model configuration.
func (b *Builder) initClient() error {
	var err error
	// Use the factory to create the appropriate client (Gemini or Anthropic/GLM-4.7)
	b.geminiClient, err = client.NewClient(b.ctx, b.cfg, b.cfg.Model.Name)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Debug: log which client was created
	if _, ok := b.geminiClient.(*client.GeminiClient); ok {
		logging.Debug("client created", "type", "Gemini", "model", b.cfg.Model.Name)
	} else if _, ok := b.geminiClient.(*client.AnthropicClient); ok {
		logging.Debug("client created", "type", "Anthropic (GLM-4.7)", "model", b.cfg.Model.Name)
	} else {
		logging.Debug("client created", "type", "Unknown", "model", b.cfg.Model.Name)
	}

	return nil
}

// initTools creates the tool registry and executor.
func (b *Builder) initTools() error {
	b.registry = tools.DefaultRegistry(b.workDir)
	b.geminiClient.SetTools(b.registry.GeminiTools())

	b.executor = tools.NewExecutor(b.registry, b.geminiClient, b.cfg.Tools.Timeout)
	compactor := appcontext.NewResultCompactor(b.cfg.Context.ToolResultMaxChars)
	b.executor.SetCompactor(compactor)

	toolCache := tools.NewToolResultCache(tools.DefaultCacheConfig())
	b.executor.SetToolCache(toolCache)

	return nil
}

// initSession creates the chat session and context management.
func (b *Builder) initSession() error {
	b.session = chat.NewSession()

	b.projectInfo = appcontext.DetectProject(b.workDir)

	b.projectMemory = appcontext.NewProjectMemory(b.workDir)
	if err := b.projectMemory.Load(); err != nil {
		logging.Debug("project memory not loaded", "error", err)
	}

	b.promptBuilder = appcontext.NewPromptBuilder(b.workDir, b.projectInfo)
	b.promptBuilder.SetProjectMemory(b.projectMemory)
	b.promptBuilder.SetPlanAutoDetect(b.cfg.Plan.AutoDetect)

	b.contextManager = appcontext.NewContextManager(b.session, b.geminiClient, &b.cfg.Context)
	b.contextAgent = appcontext.NewContextAgent(b.contextManager, b.session, b.configDir)

	// Initialize context predictor for predictive file loading
	b.contextPredictor = appcontext.NewContextPredictor(b.workDir)
	logging.Debug("context predictor initialized")

	// Start session watcher for auto-updating token counts
	b.contextManager.StartSessionWatcher()

	// Create session manager for auto-save/load
	if b.cfg.Session.Enabled {
		smConfig := chat.SessionManagerConfig{
			Enabled:      b.cfg.Session.Enabled,
			SaveInterval: b.cfg.Session.SaveInterval,
			AutoLoad:     b.cfg.Session.AutoLoad,
		}
		var err error
		b.sessionManager, err = chat.NewSessionManager(b.session, smConfig)
		if err != nil {
			logging.Warn("session persistence disabled", "error", err)
			b.sessionManager = nil
		}
	}

	return nil
}

// initManagers creates various manager components.
func (b *Builder) initManagers() error {
	// Permission manager
	if b.cfg.Permission.Enabled {
		rules := permission.NewRulesFromConfig(b.cfg.Permission.DefaultPolicy, b.cfg.Permission.Rules)
		b.permManager = permission.NewManager(rules, true)
	} else {
		b.permManager = permission.NewManager(nil, false)
	}
	b.executor.SetPermissions(b.permManager)

	// Set unrestricted mode when both sandbox and permissions are off
	// In this mode, preflight check errors become warnings (no blocking)
	sandboxOff := !b.cfg.Tools.Bash.Sandbox
	permissionOff := !b.cfg.Permission.Enabled
	b.executor.SetUnrestrictedMode(sandboxOff && permissionOff)
	if sandboxOff && permissionOff {
		logging.Debug("unrestricted mode enabled: sandbox=off, permission=off")
	}

	// Plan manager
	b.planManager = plan.NewManager(b.cfg.Plan.Enabled, b.cfg.Plan.RequireApproval)
	b.planManager.SetWorkDir(b.workDir)

	// Plan persistence store
	if b.configDirErr == nil {
		planStore, err := plan.NewPlanStore(b.configDir)
		if err != nil {
			logging.Warn("plan persistence disabled", "error", err)
		} else {
			b.planManager.SetPlanStore(planStore)
			logging.Debug("plan store initialized", "dir", b.configDir)

			// Auto-load most recent resumable plan (but don't execute)
			if loadedPlan, err := b.planManager.LoadPausedPlan(); err == nil && loadedPlan != nil {
				logging.Debug("loaded resumable plan from previous session",
					"id", loadedPlan.ID,
					"title", loadedPlan.Title,
					"status", loadedPlan.Status,
					"pending_steps", loadedPlan.PendingCount(),
				)
			}
		}
	}

	// Hooks manager
	b.hooksManager = hooks.NewManager(b.cfg.Hooks.Enabled, b.workDir)
	for _, hookCfg := range b.cfg.Hooks.Hooks {
		b.hooksManager.AddHook(&hooks.Hook{
			Name:        hookCfg.Name,
			Type:        hooks.Type(hookCfg.Type),
			ToolName:    hookCfg.ToolName,
			Command:     hookCfg.Command,
			Enabled:     hookCfg.Enabled,
			Condition:   hooks.Condition(hookCfg.Condition),
			FailOnError: hookCfg.FailOnError,
			DependsOn:   hookCfg.DependsOn,
		})
	}
	b.executor.SetHooks(b.hooksManager)

	// Task manager
	b.taskManager = tasks.NewManager(b.workDir)

	// Undo manager
	b.undoManager = undo.NewManager()

	// Agent runner
	b.agentRunner = agent.NewRunner(b.geminiClient, b.registry, b.workDir)
	b.agentRunner.SetPermissions(b.permManager)
	b.agentRunner.SetContextConfig(&b.cfg.Context)

	// Command handler
	b.commandHandler = commands.NewHandler()

	// Initialize task router
	routerCfg := &router.RouterConfig{
		Enabled:            true,
		DecomposeThreshold: 4,
		ParallelThreshold:  7,
	}
	b.taskRouter = router.NewRouter(routerCfg, b.executor, b.agentRunner, b.geminiClient, b.workDir)

	// Wire plan manager to router for plan-aware routing
	// When a plan is active, router avoids nested decomposition
	if b.planManager != nil {
		b.taskRouter.SetPlanChecker(b.planManager)
	}

	logging.Debug("task router initialized",
		"enabled", routerCfg.Enabled,
		"decompose_threshold", routerCfg.DecomposeThreshold,
		"parallel_threshold", routerCfg.ParallelThreshold)

	// Initialize Task Orchestrator (Unified)
	b.taskOrchestrator = NewTaskOrchestrator(5, 10*time.Minute)
	b.taskOrchestrator.SetOnStatusChange(func(id string, status OrchestratorTaskStatus) {
		// UI updates will be handled via App's program
	})
	logging.Debug("task orchestrator initialized", "max_concurrent", 5)

	// === PHASE 5: Agent System Improvements (6→10) ===

	// 1. Agent Type Registry (dynamic agent types)
	b.agentTypeRegistry = agent.NewAgentTypeRegistry()
	b.agentRunner.SetTypeRegistry(b.agentTypeRegistry)
	logging.Debug("agent type registry initialized")

	// 2. Strategy Optimizer (learns from outcomes)
	if b.configDirErr == nil {
		b.strategyOptimizer = agent.NewStrategyOptimizer(b.configDir)
		b.agentRunner.SetStrategyOptimizer(b.strategyOptimizer)
		logging.Debug("strategy optimizer initialized")

		// 2b. Delegation Metrics (adaptive delegation rules)
		b.delegationMetrics = agent.NewDelegationMetrics(b.configDir)
		b.agentRunner.SetDelegationMetrics(b.delegationMetrics)
		logging.Debug("delegation metrics initialized")
	}

	// 3. Coordinator for task orchestration
	coordConfig := &agent.CoordinatorConfig{MaxParallel: 3}
	b.coordinator = agent.NewCoordinator(b.agentRunner, coordConfig)
	b.coordinator.Start()
	logging.Debug("coordinator initialized", "max_parallel", 3)

	// 4. Meta-Agent (monitors and optimizes agents)
	metaConfig := agent.DefaultMetaAgentConfig()
	b.metaAgent = agent.NewMetaAgent(
		b.agentRunner,
		b.coordinator,
		b.strategyOptimizer,
		b.agentTypeRegistry,
		metaConfig,
	)
	b.metaAgent.Start()
	b.agentRunner.SetMetaAgent(b.metaAgent)
	logging.Debug("meta-agent initialized",
		"check_interval", metaConfig.CheckInterval,
		"stuck_threshold", metaConfig.StuckThreshold)

	// === PHASE 6: Tree Planner ===
	// 5. Tree Planner for planned execution mode
	treePlannerConfig := agent.DefaultTreePlannerConfig()
	if b.cfg.Plan.PlanningTimeout > 0 {
		treePlannerConfig.PlanningTimeout = b.cfg.Plan.PlanningTimeout
	}
	treePlannerConfig.UseLLMExpansion = b.cfg.Plan.UseLLMExpansion

	// Apply algorithm from config
	if b.cfg.Plan.Algorithm != "" {
		algo := agent.SearchAlgorithm(b.cfg.Plan.Algorithm)
		switch algo {
		case agent.SearchAlgorithmBeam, agent.SearchAlgorithmMCTS, agent.SearchAlgorithmAStar:
			treePlannerConfig.Algorithm = algo
		default:
			logging.Warn("unknown tree search algorithm, using beam",
				"algorithm", b.cfg.Plan.Algorithm)
		}
	}

	b.treePlanner = agent.NewTreePlanner(
		treePlannerConfig,
		b.strategyOptimizer,
		nil, // Reflector will be set per-agent
		b.geminiClient,
	)
	b.agentRunner.SetTreePlanner(b.treePlanner)
	b.agentRunner.SetPlanningModeEnabled(b.cfg.Plan.Enabled)
	b.agentRunner.SetRequireApprovalEnabled(b.cfg.Plan.RequireApproval)
	logging.Debug("tree planner initialized",
		"algorithm", treePlannerConfig.Algorithm,
		"beam_width", treePlannerConfig.BeamWidth,
		"max_depth", treePlannerConfig.MaxTreeDepth,
		"timeout", treePlannerConfig.PlanningTimeout)

	// === PHASE 2: Learning Infrastructure ===

	// 1. Shared Memory for inter-agent communication
	b.sharedMemory = agent.NewSharedMemory()
	b.agentRunner.SetSharedMemory(b.sharedMemory)
	logging.Debug("shared memory initialized")

	// 2. Example Store for few-shot learning
	if b.configDirErr == nil {
		var err error
		b.exampleStore, err = memory.NewExampleStore(b.configDir)
		if err != nil {
			logging.Warn("failed to create example store", "error", err)
		} else {
			// Wrap for runner interface
			b.agentRunner.SetExampleStore(&exampleStoreAdapter{store: b.exampleStore})
			logging.Debug("example store initialized")
		}
	}

	// 3. Prompt Optimizer
	if b.configDirErr == nil {
		b.promptOptimizer = agent.NewPromptOptimizer(b.configDir)
		b.agentRunner.SetPromptOptimizer(b.promptOptimizer)
		logging.Debug("prompt optimizer initialized")
	}

	// 4. Smart Router with adaptive selection
	smartRouterCfg := router.DefaultSmartRouterConfig()
	b.smartRouter = router.NewSmartRouter(smartRouterCfg, b.executor, b.agentRunner, b.geminiClient, b.workDir)
	if b.strategyOptimizer != nil {
		b.smartRouter.SetStrategyOptimizer(b.strategyOptimizer)
	}
	if b.exampleStore != nil {
		b.smartRouter.SetExampleStore(&routerExampleStoreAdapter{store: b.exampleStore})
	}
	logging.Debug("smart router initialized",
		"adaptive_enabled", smartRouterCfg.AdaptiveEnabled,
		"min_data_points", smartRouterCfg.MinDataPoints)

	return nil
}

// initUIUpdateSystem initializes the Phase 4 UI Auto-Update System.
func (b *Builder) initUIUpdateSystem() error {
	// This will be called after initUI() when we have access to the TUI model
	// For now, we'll create placeholder instances
	// Actual initialization will happen in wireDependencies()
	return nil
}

// initIntegrations initializes optional feature integrations.
func (b *Builder) initIntegrations() error {
	// Wire up task manager to bash tool and apply config
	if bashTool, ok := b.registry.Get("bash"); ok {
		if bt, ok := bashTool.(*tools.BashTool); ok {
			bt.SetTaskManager(b.taskManager)
			bt.SetSandboxEnabled(b.cfg.Tools.Bash.Sandbox)
			// Set unrestricted mode for bash tool (skip command validation)
			sandboxOff := !b.cfg.Tools.Bash.Sandbox
			permissionOff := !b.cfg.Permission.Enabled
			bt.SetUnrestrictedMode(sandboxOff && permissionOff)
			logging.Debug("bash tool configured",
				"sandbox", b.cfg.Tools.Bash.Sandbox,
				"unrestricted", sandboxOff && permissionOff,
				"blocked_commands", len(b.cfg.Tools.Bash.BlockedCommands))
		}
	}

	// Wire up path validation for read and edit tools
	if readTool, ok := b.registry.Get("read"); ok {
		if rt, ok := readTool.(*tools.ReadTool); ok {
			rt.SetWorkDir(b.workDir)
			// Wire context predictor for predictive file loading
			if b.contextPredictor != nil {
				rt.SetPredictor(b.contextPredictor)
			}
		}
	}
	if editTool, ok := b.registry.Get("edit"); ok {
		if et, ok := editTool.(*tools.EditTool); ok {
			et.SetWorkDir(b.workDir)
		}
	}

	// Set additional allowed directories from config
	if len(b.cfg.Tools.AllowedDirs) > 0 {
		if readTool, ok := b.registry.Get("read"); ok {
			if rt, ok := readTool.(*tools.ReadTool); ok {
				rt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if editTool, ok := b.registry.Get("edit"); ok {
			if et, ok := editTool.(*tools.EditTool); ok {
				et.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if writeTool, ok := b.registry.Get("write"); ok {
			if wt, ok := writeTool.(*tools.WriteTool); ok {
				wt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if listDirTool, ok := b.registry.Get("list_dir"); ok {
			if lt, ok := listDirTool.(*tools.ListDirTool); ok {
				lt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if globTool, ok := b.registry.Get("glob"); ok {
			if gt, ok := globTool.(*tools.GlobTool); ok {
				gt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if grepTool, ok := b.registry.Get("grep"); ok {
			if gt, ok := grepTool.(*tools.GrepTool); ok {
				gt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if treeTool, ok := b.registry.Get("tree"); ok {
			if tt, ok := treeTool.(*tools.TreeTool); ok {
				tt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		// Set allowed directories for file operation tools
		if copyTool, ok := b.registry.Get("copy"); ok {
			if ct, ok := copyTool.(*tools.CopyTool); ok {
				ct.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if moveTool, ok := b.registry.Get("move"); ok {
			if mt, ok := moveTool.(*tools.MoveTool); ok {
				mt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if deleteTool, ok := b.registry.Get("delete"); ok {
			if dt, ok := deleteTool.(*tools.DeleteTool); ok {
				dt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
		if mkdirTool, ok := b.registry.Get("mkdir"); ok {
			if mt, ok := mkdirTool.(*tools.MkdirTool); ok {
				mt.SetAllowedDirs(b.cfg.Tools.AllowedDirs)
			}
		}
	}

	// Wire up undo manager
	if writeTool, ok := b.registry.Get("write"); ok {
		if wt, ok := writeTool.(*tools.WriteTool); ok {
			wt.SetUndoManager(b.undoManager)
		}
	}
	if editTool, ok := b.registry.Get("edit"); ok {
		if et, ok := editTool.(*tools.EditTool); ok {
			et.SetUndoManager(b.undoManager)
		}
	}
	if undoTool, ok := b.registry.Get("undo"); ok {
		if ut, ok := undoTool.(*tools.UndoTool); ok {
			ut.SetManager(b.undoManager)
		}
	}
	if batchTool, ok := b.registry.Get("batch"); ok {
		if bt, ok := batchTool.(*tools.BatchTool); ok {
			bt.SetUndoManager(b.undoManager)
		}
	}
	// Wire up undo manager for file operation tools
	if copyTool, ok := b.registry.Get("copy"); ok {
		if ct, ok := copyTool.(*tools.CopyTool); ok {
			ct.SetUndoManager(b.undoManager)
		}
	}
	if moveTool, ok := b.registry.Get("move"); ok {
		if mt, ok := moveTool.(*tools.MoveTool); ok {
			mt.SetUndoManager(b.undoManager)
		}
	}
	if deleteTool, ok := b.registry.Get("delete"); ok {
		if dt, ok := deleteTool.(*tools.DeleteTool); ok {
			dt.SetUndoManager(b.undoManager)
		}
	}
	if mkdirTool, ok := b.registry.Get("mkdir"); ok {
		if mt, ok := mkdirTool.(*tools.MkdirTool); ok {
			mt.SetUndoManager(b.undoManager)
		}
	}

	// Wire up agent runner to task tool
	runnerAdapter := &agentRunnerAdapter{runner: b.agentRunner}
	if taskTool, ok := b.registry.Get("task"); ok {
		if tt, ok := taskTool.(*tools.TaskTool); ok {
			tt.SetRunner(runnerAdapter)
		}
	}

	// Wire up task_output tool
	if taskOutputTool, ok := b.registry.Get("task_output"); ok {
		if tot, ok := taskOutputTool.(*tools.TaskOutputTool); ok {
			tot.SetManager(b.taskManager)
			tot.SetRunner(runnerAdapter)
		}
	}

	// Wire up task_stop tool
	if taskStopTool, ok := b.registry.Get("task_stop"); ok {
		if tst, ok := taskStopTool.(*tools.TaskStopTool); ok {
			tst.SetManager(b.taskManager)
			tst.SetRunner(runnerAdapter)
		}
	}

	// Configure web search
	if webSearchTool, ok := b.registry.Get("web_search"); ok {
		if wst, ok := webSearchTool.(*tools.WebSearchTool); ok {
			if b.cfg.Web.SearchAPIKey != "" {
				wst.SetAPIKey(b.cfg.Web.SearchAPIKey)
			}
			if b.cfg.Web.SearchProvider == "google" {
				wst.SetProvider(tools.SearchProviderGoogle)
				wst.SetGoogleCX(b.cfg.Web.GoogleCX)
			}
		}
	}

	// Wire up kill_shell tool
	if killShellTool, ok := b.registry.Get("kill_shell"); ok {
		if kst, ok := killShellTool.(*tools.KillShellTool); ok {
			kst.SetManager(b.taskManager)
		}
	}

	// Initialize memory store
	if b.cfg.Memory.Enabled && b.configDirErr == nil {
		memoryStore, err := memory.NewStore(b.configDir, b.workDir, b.cfg.Memory.MaxEntries)
		if err != nil {
			logging.Warn("failed to create memory store", "error", err)
		} else {
			if memoryTool, ok := b.registry.Get("memory"); ok {
				if mt, ok := memoryTool.(*tools.MemoryTool); ok {
					mt.SetStore(memoryStore)
				}
			}
			if b.cfg.Memory.AutoInject {
				b.promptBuilder.SetMemoryStore(memoryStore)
			}
		}

		// Initialize error store for learning from errors (Phase 3)
		errorStore, err := memory.NewErrorStore(b.configDir)
		if err != nil {
			logging.Warn("failed to create error store", "error", err)
		} else {
			// Wire error store to agent runner for reflector integration
			b.agentRunner.SetErrorStore(errorStore)
			logging.Debug("error store initialized for learning from errors")
		}
	}

	// Wire context predictor to agent runner for enhanced error recovery
	if b.contextPredictor != nil {
		b.agentRunner.SetPredictor(&contextPredictorAdapter{predictor: b.contextPredictor})
		logging.Debug("context predictor wired to agent runner")
	}

	// Initialize search cache
	if b.cfg.Cache.Enabled {
		b.searchCache = cache.NewSearchCache(b.cfg.Cache.Capacity, b.cfg.Cache.TTL)
		if grepTool, ok := b.registry.Get("grep"); ok {
			if gt, ok := grepTool.(*tools.GrepTool); ok {
				gt.SetCache(b.searchCache)
			}
		}
		if globTool, ok := b.registry.Get("glob"); ok {
			if gt, ok := globTool.(*tools.GlobTool); ok {
				gt.SetCache(b.searchCache)
			}
		}
	}

	// Wire context predictor to search tools for predictive file loading
	if b.contextPredictor != nil {
		if grepTool, ok := b.registry.Get("grep"); ok {
			if gt, ok := grepTool.(*tools.GrepTool); ok {
				gt.SetPredictor(b.contextPredictor)
			}
		}
		if globTool, ok := b.registry.Get("glob"); ok {
			if gt, ok := globTool.(*tools.GlobTool); ok {
				gt.SetPredictor(b.contextPredictor)
			}
		}
	}

	// Initialize rate limiter
	if b.cfg.RateLimit.Enabled {
		b.rateLimiter = ratelimit.NewLimiter(ratelimit.Config{
			Enabled:           true,
			RequestsPerMinute: b.cfg.RateLimit.RequestsPerMinute,
			TokensPerMinute:   b.cfg.RateLimit.TokensPerMinute,
			BurstSize:         b.cfg.RateLimit.BurstSize,
		})
		b.geminiClient.SetRateLimiter(b.rateLimiter)
	}

	// Initialize audit logger
	if b.cfg.Audit.Enabled && b.configDirErr == nil {
		auditLogger, err := audit.NewLogger(b.configDir, b.session.ID, audit.Config{
			Enabled:       true,
			MaxEntries:    b.cfg.Audit.MaxEntries,
			MaxResultLen:  b.cfg.Audit.MaxResultLen,
			RetentionDays: b.cfg.Audit.RetentionDays,
		})
		if err != nil {
			logging.Warn("failed to create audit logger", "error", err)
		} else {
			b.auditLogger = auditLogger
			b.executor.SetAuditLogger(auditLogger)
		}
	}
	b.executor.SetSessionID(b.session.ID)

	// Initialize file watcher
	if b.cfg.Watcher.Enabled {
		gitIgnore := git.NewGitIgnore(b.workDir)
		_ = gitIgnore.Load()
		fileWatcher, err := watcher.NewWatcher(b.workDir, gitIgnore, watcher.Config{
			Enabled:    true,
			DebounceMs: b.cfg.Watcher.DebounceMs,
			MaxWatches: b.cfg.Watcher.MaxWatches,
		})
		if err != nil {
			logging.Warn("failed to create file watcher", "error", err)
		} else {
			b.fileWatcher = fileWatcher
		}
	}

	// Initialize semantic search
	if b.cfg.Semantic.Enabled && b.configDirErr == nil {
		// Always create a separate Gemini client for semantic search embeddings
		// This works regardless of which chat model is selected (Gemini, GLM-4.7, Claude)
		genaiClient, err := genai.NewClient(b.ctx, &genai.ClientConfig{
			APIKey:  b.cfg.API.APIKey,
			Backend: genai.BackendGeminiAPI,
		})
		if err != nil {
			logging.Error("failed to create Gemini client for semantic search", "error", err)
			// Continue without semantic search
		} else {
			embedder := semantic.NewEmbedder(genaiClient, b.cfg.Semantic.Model)
			// Use per-project cache (each project gets its own cache file)
			semanticCache := semantic.NewEmbeddingCache(b.configDir, b.workDir, b.cfg.Semantic.CacheTTL)
			// Store project path in cache for metadata
			semanticCache.SetProjectDir(b.workDir)

			// Create enhanced indexer with persistence
			b.semanticIdx = semantic.NewEnhancedIndexer(
				embedder,
				b.workDir,
				semanticCache,
				b.cfg.Semantic.MaxFileSize,
				b.configDir,
			)

			// Create incremental indexer wrapping enhanced indexer
			bgConfig := semantic.DefaultBackgroundIndexerConfig()
			b.incrementalIdx = semantic.NewIncrementalIndexer(
				b.semanticIdx,
				bgConfig.BatchSize,
				bgConfig.Workers,
			)

			// Create background indexer for watcher-driven incremental indexing
			b.backgroundIdx = semantic.NewBackgroundIndexer(
				b.incrementalIdx,
				b.fileWatcher,
				b.workDir,
				bgConfig,
			)
			b.backgroundIdx.SetOnError(func(err error) {
				logging.Warn("background semantic indexing error", "error", err)
			})

			b.registry.Register(tools.NewSemanticSearchTool(b.semanticIdx, b.workDir, b.cfg.Semantic.TopK))

			// Register semantic cleanup tool
			b.registry.Register(tools.NewSemanticCleanupTool(b.configDir, b.cfg.Semantic.CacheTTL))

			logging.Debug("semantic search initialized with per-project storage",
				"project", b.workDir,
				"cache_ttl", b.cfg.Semantic.CacheTTL)
		}
	}

	// Wire up plan mode tools
	if enterPlanTool, ok := b.registry.Get("enter_plan_mode"); ok {
		if ept, ok := enterPlanTool.(*tools.EnterPlanModeTool); ok {
			ept.SetManager(b.planManager)
		}
	}
	if updateProgressTool, ok := b.registry.Get("update_plan_progress"); ok {
		if upt, ok := updateProgressTool.(*tools.UpdatePlanProgressTool); ok {
			upt.SetManager(b.planManager)
		}
	}
	if getPlanStatusTool, ok := b.registry.Get("get_plan_status"); ok {
		if gpst, ok := getPlanStatusTool.(*tools.GetPlanStatusTool); ok {
			gpst.SetManager(b.planManager)
		}
	}
	if exitPlanTool, ok := b.registry.Get("exit_plan_mode"); ok {
		if ept, ok := exitPlanTool.(*tools.ExitPlanModeTool); ok {
			ept.SetManager(b.planManager)
		}
	}

	// Wire up shared_memory tool (Phase 2)
	if smt, ok := b.registry.Get("shared_memory"); ok {
		if t, ok := smt.(*tools.SharedMemoryTool); ok {
			if b.sharedMemory != nil {
				// Create adapter for shared memory
				adapter := &sharedMemoryToolAdapter{memory: b.sharedMemory}
				t.SetMemory(adapter)
				logging.Debug("shared_memory tool wired")
			}
		}
	}

	// Initialize MCP (Model Context Protocol)
	if b.cfg.MCP.Enabled && len(b.cfg.MCP.Servers) > 0 {
		// Convert config to MCP server configs
		mcpConfigs := make([]*mcp.ServerConfig, 0, len(b.cfg.MCP.Servers))
		for _, s := range b.cfg.MCP.Servers {
			mcpConfigs = append(mcpConfigs, &mcp.ServerConfig{
				Name:        s.Name,
				Transport:   s.Transport,
				Command:     s.Command,
				Args:        s.Args,
				Env:         s.Env,
				URL:         s.URL,
				Headers:     s.Headers,
				AutoConnect: s.AutoConnect,
				Timeout:     s.Timeout,
				MaxRetries:  s.MaxRetries,
				RetryDelay:  s.RetryDelay,
				ToolPrefix:  s.ToolPrefix,
			})
		}

		b.mcpManager = mcp.NewManager(mcpConfigs)

		// Connect to auto-connect servers
		if err := b.mcpManager.ConnectAll(b.ctx); err != nil {
			logging.Warn("some MCP servers failed to connect", "error", err)
			// Continue - graceful degradation
		}

		// Register MCP tools into the registry
		for _, tool := range b.mcpManager.GetTools() {
			if err := b.registry.Register(tool); err != nil {
				logging.Warn("failed to register MCP tool",
					"tool", tool.Name(), "error", err)
			}
		}

		// Refresh tools on client
		b.geminiClient.SetTools(b.registry.GeminiTools())

		logging.Debug("MCP initialized",
			"servers", len(b.cfg.MCP.Servers),
			"tools", len(b.mcpManager.GetTools()))
	}

	return nil
}

// initUI creates and configures the TUI model.
func (b *Builder) initUI() error {
	b.tuiModel = ui.NewModel()
	b.tuiModel.SetShowTokens(b.cfg.UI.ShowTokenUsage)

	enableMouse := b.cfg.UI.MouseMode != "disabled"
	b.tuiModel.SetMouseEnabled(enableMouse)

	// Filter models by current provider/backend
	provider := b.cfg.Model.Provider
	if provider == "" {
		provider = b.cfg.API.Backend
	}
	if provider == "" {
		provider = "gemini" // Default
	}

	var uiModels []ui.ModelInfo
	for _, m := range client.GetModelsForProvider(provider) {
		uiModels = append(uiModels, ui.ModelInfo{
			ID:          m.ID,
			Name:        m.Name,
			Description: m.Description,
		})
	}
	b.tuiModel.SetAvailableModels(uiModels)
	b.tuiModel.SetCurrentModel(b.cfg.Model.Name)

	// Set version for display in status bar
	if b.cfg.Version != "" {
		b.tuiModel.SetVersion(b.cfg.Version)
	}

	if b.projectInfo.Type != appcontext.ProjectTypeUnknown {
		b.tuiModel.SetProjectInfo(b.projectInfo.Type.String(), b.projectInfo.Name)
	}

	// Set git branch for status bar display
	if branch := git.GetCurrentBranch(b.workDir); branch != "" {
		b.tuiModel.SetGitBranch(branch)
	}

	return nil
}

// wireDependencies sets up callbacks and inter-component connections.
func (b *Builder) wireDependencies() error {
	app := b.assembleApp()

	// Set up status callback for clients
	statusCb := &appStatusCallback{app: app}
	if gc, ok := b.geminiClient.(*client.GeminiClient); ok {
		gc.SetStatusCallback(statusCb)
	}
	if ac, ok := b.geminiClient.(*client.AnthropicClient); ok {
		ac.SetStatusCallback(statusCb)
	}
	if oc, ok := b.geminiClient.(*client.OllamaClient); ok {
		oc.SetStatusCallback(statusCb)
	}

	// Set up executor handler
	b.executor.SetHandler(&tools.ExecutionHandler{
		OnText: func(text string) {
			if app.program != nil {
				app.program.Send(ui.StreamTextMsg(text))
			}

			// Track streamed text for token estimation
			app.mu.Lock()
			app.streamedChars += len(text)
			chars := app.streamedChars
			app.mu.Unlock()

			// Send estimated token update every ~2000 chars (~500 tokens)
			if chars%2000 < len(text) {
				app.sendTokenUsageUpdate()
			}
		},
		OnToolStart: func(name string, args map[string]any) {
			// Track tools used for response metadata
			app.mu.Lock()
			app.responseToolsUsed = append(app.responseToolsUsed, name)
			app.mu.Unlock()

			// Task 5.8: Record tool usage for pattern learning
			app.recordToolUsage(name)

			if app.program != nil {
				app.program.Send(ui.ToolCallMsg{Name: name, Args: args})
			}
		},
		OnToolEnd: func(name string, result tools.ToolResult) {
			if app.program != nil {
				app.program.Send(ui.ToolResultMsg(result.Content))
			}

			// Refresh token count after each tool completes (context grew)
			go app.refreshTokenCount()
		},
		OnToolProgress: func(name string, elapsed time.Duration) {
			// Heartbeat for long-running tools - keeps UI timeout from triggering
			if app.program != nil {
				app.program.Send(ui.ToolProgressMsg{Name: name, Elapsed: elapsed})
			}
		},
		OnError: func(err error) {
			if app.program != nil {
				app.program.Send(ui.ErrorMsg(err))
			}
		},
	})

	// Set up TUI callbacks
	b.tuiModel.SetCallbacks(app.handleSubmit, app.handleQuit)
	b.tuiModel.SetWorkDir(b.workDir)
	b.tuiModel.SetPermissionCallback(app.handlePermissionDecision)
	b.tuiModel.SetQuestionCallback(app.handleQuestionAnswer)
	b.tuiModel.SetPlanApprovalCallback(app.handlePlanApproval)
	b.tuiModel.SetModelSelectCallback(app.handleModelSelect)
	b.tuiModel.SetDiffDecisionCallback(app.handleDiffDecision)

	// Set up cancel callback for ESC interrupt
	b.tuiModel.SetCancelCallback(app.CancelProcessing)

	// Set up plan approval with feedback callback
	b.tuiModel.SetPlanApprovalWithFeedbackCallback(app.handlePlanApprovalWithFeedback)

	// Set up permissions toggle callback and initial state
	b.tuiModel.SetPermissionsEnabled(b.cfg.Permission.Enabled)
	b.tuiModel.SetPermissionsToggleCallback(app.TogglePermissions)

	// Set up sandbox toggle callback and initial state
	b.tuiModel.SetSandboxEnabled(b.cfg.Tools.Bash.Sandbox)
	b.tuiModel.SetSandboxToggleCallback(app.ToggleSandbox, app.GetSandboxState)

	// Set up planning mode toggle callback (async to avoid blocking UI)
	b.tuiModel.SetPlanningModeToggleCallback(app.TogglePlanningModeAsync)

	// Set up command palette integration
	hasAuth := b.cfg.API.APIKey != "" || b.cfg.API.GeminiKey != "" || b.cfg.API.GLMKey != "" || b.cfg.API.HasOAuthToken("gemini")
	paletteCtx := commands.NewPaletteContext(b.workDir, hasAuth)
	paletteProvider := commands.NewPaletteProvider(b.commandHandler, paletteCtx)
	b.tuiModel.SetPaletteProvider(paletteProvider)

	// Set up plan approval callback for context compaction
	b.agentRunner.SetOnPlanApproved(app.CompactContextWithPlan)

	// Set up background task tracking callbacks for UI
	b.agentRunner.SetOnAgentStart(func(id, agentType, description string) {
		if app.program != nil {
			// Truncate description if too long
			desc := description
			if len(desc) > 50 {
				desc = desc[:47] + "..."
			}
			app.program.Send(ui.BackgroundTaskMsg{
				ID:          id,
				Type:        "agent",
				Description: desc,
				Status:      "running",
			})
		}
	})
	b.agentRunner.SetOnAgentComplete(func(id string, result *agent.AgentResult) {
		if app.program != nil {
			status := "completed"
			if result != nil {
				switch result.Status {
				case agent.AgentStatusFailed:
					status = "failed"
				case agent.AgentStatusCancelled:
					status = "cancelled"
				}
			}
			app.program.Send(ui.BackgroundTaskMsg{
				ID:     id,
				Type:   "agent",
				Status: status,
			})
		}
	})

	b.agentRunner.SetOnScratchpadUpdate(func(content string) {
		app.mu.Lock()
		app.scratchpad = content
		app.mu.Unlock()

		if app.session != nil {
			app.session.SetScratchpad(content)
		}

		if app.program != nil {
			app.program.Send(ui.ScratchpadMsg(content))
		}
	})

	// Wire sub-agent activity to UI
	b.agentRunner.SetOnSubAgentActivity(func(agentID, agentType, toolName string, args map[string]any, status string) {
		if app.program != nil {
			app.program.Send(ui.SubAgentActivityMsg{
				AgentID:   agentID,
				AgentType: agentType,
				ToolName:  toolName,
				ToolArgs:  args,
				Status:    status,
			})
		}
	})

	// Set up apply code block callback
	b.tuiModel.SetApplyCodeBlockCallback(app.handleApplyCodeBlock)

	// Set up permission callback
	b.permManager.SetPromptHandler(app.promptPermission)

	// Set up ask_user tool
	if askUserTool, ok := b.registry.Get("ask_user"); ok {
		if aut, ok := askUserTool.(*tools.AskUserTool); ok {
			aut.SetHandler(app.promptQuestion)
		}
	}

	// Set up plan approval
	b.planManager.SetApprovalHandler(app.promptPlanApproval)

	// Set up plan progress updates
	b.planManager.SetProgressUpdateHandler(app.handlePlanProgressUpdate)

	// Set up diff preview (skip if permissions are disabled — no approval needed)
	if b.cfg.DiffPreview.Enabled && b.cfg.Permission.Enabled {
		diffAdapter := &diffHandlerAdapter{app: app}
		if writeTool, ok := b.registry.Get("write"); ok {
			if wt, ok := writeTool.(*tools.WriteTool); ok {
				wt.SetDiffHandler(diffAdapter)
				wt.SetDiffEnabled(true)
			}
		}
		if editTool, ok := b.registry.Get("edit"); ok {
			if et, ok := editTool.(*tools.EditTool); ok {
				et.SetDiffHandler(diffAdapter)
				et.SetDiffEnabled(true)
			}
		}
	}

	// === PHASE 4: Initialize UI Auto-Update System ===
	// Create UI update manager with app instance (will be set in assembleApp)
	// We need to defer this until after app is assembled
	logging.Debug("UI update system will be initialized after app assembly")

	// === PHASE 5: Wire UIBroadcaster to Coordinator ===
	if b.coordinator != nil {
		broadcaster := &uiBroadcasterAdapter{app: app}
		b.coordinator.SetUIBroadcaster(broadcaster)
		logging.Debug("coordinator UI broadcaster wired")
	}

	return nil
}

// assembleApp creates the final App instance from built components.
func (b *Builder) assembleApp() *App {
	// Return cached instance if already created
	if b.cachedApp != nil {
		return b.cachedApp
	}

	b.cachedApp = &App{
		config:               b.cfg,
		workDir:              b.workDir,
		client:               b.geminiClient,
		registry:             b.registry,
		executor:             b.executor,
		session:              b.session,
		tui:                  b.tuiModel,
		ctx:                  b.ctx,
		cancel:               b.cancel, // Use the saved cancel function
		projectInfo:          b.projectInfo,
		contextManager:       b.contextManager,
		promptBuilder:        b.promptBuilder,
		contextAgent:         b.contextAgent,
		permManager:          b.permManager,
		permResponseChan:     make(chan permission.Decision, 2),
		questionResponseChan: make(chan string, 1),
		diffResponseChan:     make(chan ui.DiffDecision, 1),
		planManager:          b.planManager,
		planApprovalChan:     make(chan plan.ApprovalDecision, 1),
		hooksManager:         b.hooksManager,
		taskManager:          b.taskManager,
		undoManager:          b.undoManager,
		agentRunner:          b.agentRunner,
		commandHandler:       b.commandHandler,
		sessionManager:       b.sessionManager,
		searchCache:          b.searchCache,
		rateLimiter:          b.rateLimiter,
		auditLogger:          b.auditLogger,
		fileWatcher:          b.fileWatcher,
		semanticIndexer:      b.semanticIdx,
		backgroundIndexer:    b.backgroundIdx,
		taskRouter:           b.taskRouter,
		orchestrator:         b.taskOrchestrator,
		// Phase 4: UI Auto-Update System (initialized separately)
		uiUpdateManager: nil, // Will be set after assembly
		// Phase 5: Agent System Improvements
		coordinator:       b.coordinator,
		agentTypeRegistry: b.agentTypeRegistry,
		strategyOptimizer: b.strategyOptimizer,
		metaAgent:         b.metaAgent,
		// Phase 6: Tree Planner
		treePlanner: b.treePlanner,
		// MCP (Model Context Protocol)
		mcpManager: b.mcpManager,
	}

	// Wire up user input callback for agents
	if b.agentRunner != nil {
		b.agentRunner.SetOnInput(func(prompt string) (string, error) {
			return b.cachedApp.promptQuestion(b.ctx, prompt, nil, "")
		})
	}

	return b.cachedApp
}

// addError records a non-fatal error during build.
func (b *Builder) addError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buildErrors = append(b.buildErrors, err)
}

// finalizeError combines all build errors into a single error.
func (b *Builder) finalizeError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buildErrors) == 0 {
		return nil
	}
	msg := fmt.Sprintf("app build failed with %d error(s)", len(b.buildErrors))
	for i, err := range b.buildErrors {
		msg += fmt.Sprintf("\n  %d. %s", i+1, err.Error())
	}
	return fmt.Errorf("%s", msg)
}

// ========== UIBroadcaster Adapter (Phase 5) ==========

// uiBroadcasterAdapter implements agent.UIBroadcaster for tea.Program.
type uiBroadcasterAdapter struct {
	app *App
}

// BroadcastTaskStarted sends a task started event to the UI.
func (a *uiBroadcasterAdapter) BroadcastTaskStarted(taskID, message, planType string) {
	if a.app != nil && a.app.program != nil {
		a.app.program.Send(ui.TaskStartedEvent{
			TaskID:   taskID,
			Message:  message,
			PlanType: planType,
		})
	}
}

// BroadcastTaskCompleted sends a task completed event to the UI.
func (a *uiBroadcasterAdapter) BroadcastTaskCompleted(taskID string, success bool, duration time.Duration, err error, planType string) {
	if a.app != nil && a.app.program != nil {
		a.app.program.Send(ui.TaskCompletedEvent{
			TaskID:   taskID,
			Success:  success,
			Duration: duration,
			Error:    err,
			PlanType: planType,
		})
	}
}

// BroadcastTaskProgress sends a task progress event to the UI.
func (a *uiBroadcasterAdapter) BroadcastTaskProgress(taskID string, progress float64, message string) {
	if a.app != nil && a.app.program != nil {
		a.app.program.Send(ui.TaskProgressEvent{
			TaskID:   taskID,
			Progress: progress,
			Message:  message,
		})
	}
}

// ========== Phase 2: Example Store Adapter ==========

// exampleStoreAdapter adapts memory.ExampleStore to agent.ExampleStoreInterface.
type exampleStoreAdapter struct {
	store *memory.ExampleStore
}

func (a *exampleStoreAdapter) LearnFromSuccess(taskType, prompt, agentType, output string, duration time.Duration, tokens int) error {
	return a.store.LearnFromSuccess(taskType, prompt, agentType, output, duration, tokens)
}

func (a *exampleStoreAdapter) GetSimilarExamples(prompt string, limit int) []agent.TaskExampleSummary {
	examples := a.store.GetSimilarExamples(prompt, limit)
	result := make([]agent.TaskExampleSummary, len(examples))
	for i, ex := range examples {
		result[i] = agent.TaskExampleSummary{
			ID:          ex.ID,
			TaskType:    ex.TaskType,
			InputPrompt: ex.InputPrompt,
			AgentType:   ex.AgentType,
			Duration:    ex.Duration,
			Score:       ex.Score,
		}
	}
	return result
}

func (a *exampleStoreAdapter) GetExamplesForContext(taskType, prompt string, limit int) string {
	return a.store.GetExamplesForContext(taskType, prompt, limit)
}

// routerExampleStoreAdapter adapts memory.ExampleStore to router.ExampleStoreInterface.
type routerExampleStoreAdapter struct {
	store *memory.ExampleStore
}

func (a *routerExampleStoreAdapter) GetSimilarExamples(prompt string, limit int) []router.ExampleSummary {
	examples := a.store.GetSimilarExamples(prompt, limit)
	result := make([]router.ExampleSummary, len(examples))
	for i, ex := range examples {
		result[i] = router.ExampleSummary{
			ID:          ex.ID,
			TaskType:    ex.TaskType,
			InputPrompt: ex.InputPrompt,
			AgentType:   ex.AgentType,
			Duration:    ex.Duration,
			Score:       ex.Score,
		}
	}
	return result
}

func (a *routerExampleStoreAdapter) GetExamplesForContext(taskType, prompt string, limit int) string {
	return a.store.GetExamplesForContext(taskType, prompt, limit)
}

func (a *routerExampleStoreAdapter) LearnFromSuccess(taskType, prompt, agentType, output string, duration time.Duration, tokens int) error {
	return a.store.LearnFromSuccess(taskType, prompt, agentType, output, duration, tokens)
}

// ========== Phase 2: Shared Memory Tool Adapter ==========

// sharedMemoryToolAdapter adapts agent.SharedMemory to tools.SharedMemoryInterface.
type sharedMemoryToolAdapter struct {
	memory *agent.SharedMemory
}

func (a *sharedMemoryToolAdapter) Write(key string, value any, entryType string, sourceAgent string) {
	a.memory.Write(key, value, agent.SharedEntryType(entryType), sourceAgent)
}

func (a *sharedMemoryToolAdapter) WriteWithTTL(key string, value any, entryType string, sourceAgent string, ttl time.Duration) {
	a.memory.WriteWithTTL(key, value, agent.SharedEntryType(entryType), sourceAgent, ttl)
}

func (a *sharedMemoryToolAdapter) Read(key string) (tools.SharedMemoryEntry, bool) {
	entry, ok := a.memory.Read(key)
	if !ok {
		return tools.SharedMemoryEntry{}, false
	}
	return tools.SharedMemoryEntry{
		Key:       entry.Key,
		Value:     entry.Value,
		Type:      string(entry.Type),
		Source:    entry.Source,
		Timestamp: entry.Timestamp,
		Version:   entry.Version,
	}, true
}

func (a *sharedMemoryToolAdapter) ReadByType(entryType string) []tools.SharedMemoryEntry {
	entries := a.memory.ReadByType(agent.SharedEntryType(entryType))
	result := make([]tools.SharedMemoryEntry, len(entries))
	for i, entry := range entries {
		result[i] = tools.SharedMemoryEntry{
			Key:       entry.Key,
			Value:     entry.Value,
			Type:      string(entry.Type),
			Source:    entry.Source,
			Timestamp: entry.Timestamp,
			Version:   entry.Version,
		}
	}
	return result
}

func (a *sharedMemoryToolAdapter) ReadAll() []tools.SharedMemoryEntry {
	entries := a.memory.ReadAll()
	result := make([]tools.SharedMemoryEntry, len(entries))
	for i, entry := range entries {
		result[i] = tools.SharedMemoryEntry{
			Key:       entry.Key,
			Value:     entry.Value,
			Type:      string(entry.Type),
			Source:    entry.Source,
			Timestamp: entry.Timestamp,
			Version:   entry.Version,
		}
	}
	return result
}

func (a *sharedMemoryToolAdapter) Delete(key string) bool {
	return a.memory.Delete(key)
}

func (a *sharedMemoryToolAdapter) GetForContext(agentID string, maxEntries int) string {
	return a.memory.GetForContext(agentID, maxEntries)
}

// ========== Context Predictor Adapter ==========

// contextPredictorAdapter adapts appcontext.ContextPredictor to agent.PredictorInterface.
type contextPredictorAdapter struct {
	predictor *appcontext.ContextPredictor
}

func (a *contextPredictorAdapter) PredictFiles(currentFile string, limit int) []agent.PredictedFile {
	predictions := a.predictor.PredictFiles(currentFile, limit)
	result := make([]agent.PredictedFile, len(predictions))
	for i, p := range predictions {
		result[i] = agent.PredictedFile{
			Path:       p.Path,
			Confidence: p.Confidence,
			Reason:     p.Reason,
		}
	}
	return result
}
