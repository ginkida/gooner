package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gokin/internal/client"
	"gokin/internal/config"
	ctxmgr "gokin/internal/context"
	"gokin/internal/logging"
	"gokin/internal/memory"
	"gokin/internal/permission"
	"gokin/internal/tools"

	"google.golang.org/genai"
)

const (
	// MaxHistorySize is the maximum number of messages in history before forced compaction.
	// This prevents unbounded memory growth during long sessions.
	MaxHistorySize = 200

	// MaxTurnLimit is the absolute maximum number of turns an agent can take.
	// This prevents infinite loops even if mental loop detection fails.
	MaxTurnLimit = 100
)

// Agent represents an isolated executor for subtasks.
type Agent struct {
	ID           string
	Type         AgentType
	Model        string
	client       client.Client
	registry     *tools.Registry
	baseRegistry tools.ToolRegistry
	messenger    tools.Messenger
	permissions  *permission.Manager
	timeout      time.Duration
	history      []*genai.Content
	status       AgentStatus
	startTime    time.Time
	endTime      time.Time
	maxTurns     int

	// === IMPROVEMENT 4: Progress tracking ===
	currentStep      int
	totalSteps       int
	stepDescription  string
	progressMu       sync.Mutex
	progressCallback func(progress *AgentProgress)

	// Mental loop detection tracking
	callHistory    map[string]int // Map of tool_name:arguments -> count
	callHistoryMu  sync.Mutex     // Protects callHistory map
	loopIntervened bool           // Flag to indicate if loop intervention occurred

	// Project context injection for sub-agents
	projectContext string            // Injected project guidelines/instructions
	onText         func(text string) // Streaming callback for real-time output
	onTextMu       sync.Mutex        // Protects onText from interleaving
	onInput        func(prompt string) (string, error)

	// Plan approval callback for context compaction
	onPlanApproved func(planSummary string) // Called when plan is built, allows context clearing

	// Scratchpad update callback
	onScratchpadUpdate func(content string)

	// Context management
	ctxCfg       *config.ContextConfig
	tokenCounter *ctxmgr.TokenCounter
	summarizer   *ctxmgr.Summarizer
	compactor    *ctxmgr.ResultCompactor

	// Self-reflection for error recovery
	reflector *Reflector
	learning  *memory.ProjectLearning

	// Autonomous delegation strategy
	delegation *DelegationStrategy

	// Tree planning (Phase 6)
	treePlanner     *TreePlanner
	activePlan      *PlanTree
	planningMode    bool
	requireApproval bool
	planGoal        *PlanGoal

	// Phase 2: Shared memory for inter-agent communication
	sharedMemory *SharedMemory

	// Phase 2: Tools used tracking for progress
	toolsUsed []string
	toolsMu   sync.Mutex

	// State protection for concurrent access to status, history, startTime, endTime
	stateMu sync.RWMutex

	// Agent Scratchpad (Phase 7)
	Scratchpad string

	// Pinned Context (Custom Improvement)
	PinnedContext string

	// Tool activity callback for UI updates
	onToolActivity func(agentID, toolName string, args map[string]any, status string)

	// Checkpoint support
	store              *AgentStore
	autoCheckpoint     bool // Enable auto-checkpoint every N turns
	checkpointInterval int  // Number of turns between auto-checkpoints
	lastCheckpointTurn int  // Last turn when checkpoint was saved
}

// NewAgent creates a new agent with the specified type and filtered tools.
func NewAgent(agentType AgentType, c client.Client, baseRegistry tools.ToolRegistry, workDir string, maxTurns int, model string, permManager *permission.Manager, ctxCfg *config.ContextConfig) *Agent {
	id := generateAgentID()

	// Create filtered registry based on agent type
	filteredRegistry := createFilteredRegistry(agentType, baseRegistry)

	if maxTurns <= 0 {
		maxTurns = 30 // default
	}

	// Use a different model if specified
	agentClient := c
	if model != "" {
		modelName := mapModelName(model)
		if modelName != "" {
			agentClient = c.WithModel(modelName)
		}
	}

	agent := &Agent{
		ID:           id,
		Type:         agentType,
		Model:        model,
		client:       agentClient,
		registry:     filteredRegistry,
		baseRegistry: baseRegistry,
		permissions:  permManager,
		timeout:      2 * time.Minute,
		history:      make([]*genai.Content, 0),
		status:       AgentStatusPending,
		maxTurns:     maxTurns,
		callHistory:  make(map[string]int),
		ctxCfg:       ctxCfg,
	}

	// Wire up RequestTool tool if it exists in the registry
	if rt, ok := agent.registry.Get("request_tool"); ok {
		if rtt, ok := rt.(*tools.RequestToolTool); ok {
			rtt.SetRequester(agent)
		}
	}

	// Wire up PinContext tool (Custom Improvement)
	if pt, ok := agent.registry.Get("pin_context"); ok {
		if ptt, ok := pt.(*tools.PinContextTool); ok {
			ptt.SetUpdater(agent.SetPinnedContext)
		}
	}

	// Wire up HistorySearch tool (Custom Improvement)
	if ht, ok := agent.registry.Get("history_search"); ok {
		if htt, ok := ht.(*tools.HistorySearchTool); ok {
			htt.SetHistoryGetter(func() []*genai.Content { return agent.history })
		}
	}

	// Initialize context management tools if config provided
	if ctxCfg != nil {
		agent.tokenCounter = ctxmgr.NewTokenCounter(agent.client, agent.Model, ctxCfg)
		agent.summarizer = ctxmgr.NewSummarizer(agent.client)
		agent.compactor = ctxmgr.NewResultCompactor(ctxCfg.ToolResultMaxChars)
	}

	// Initialize project learning
	if pl, err := memory.NewProjectLearning(workDir); err == nil {
		agent.learning = pl
		// Inject into memorize tool if it exists
		if mt, ok := agent.registry.Get("memorize"); ok {
			if mtt, ok := mt.(interface{ SetLearning(*memory.ProjectLearning) }); ok {
				mtt.SetLearning(pl)
			}
		}
	}

	// Initialize self-reflection capability with LLM client for semantic analysis
	agent.reflector = NewReflector()
	agent.reflector.SetClient(agentClient)

	// Wire up scratchpad if it exists
	if t, ok := agent.registry.Get("update_scratchpad"); ok {
		if ust, ok := t.(*tools.UpdateScratchpadTool); ok {
			ust.SetUpdater(func(content string) {
				agent.Scratchpad = content
				if agent.onScratchpadUpdate != nil {
					agent.onScratchpadUpdate(content)
				}
			})
		}
	}

	// Initialize delegation strategy (messenger set later)
	agent.delegation = NewDelegationStrategy(agentType, nil)

	return agent
}

// NewAgentWithDynamicType creates a new agent with a dynamic type configuration.
func NewAgentWithDynamicType(dynType *DynamicAgentType, c client.Client, baseRegistry tools.ToolRegistry, workDir string, maxTurns int, model string, permManager *permission.Manager, ctxCfg *config.ContextConfig) *Agent {
	id := generateAgentID()

	// Create filtered registry based on dynamic type's allowed tools
	filteredRegistry := createFilteredRegistryFromList(dynType.AllowedTools, baseRegistry)

	if maxTurns <= 0 {
		maxTurns = 30
	}

	agentClient := c
	if model != "" {
		modelName := mapModelName(model)
		if modelName != "" {
			agentClient = c.WithModel(modelName)
		}
	}

	agent := &Agent{
		ID:           id,
		Type:         AgentType(dynType.Name), // Use dynamic type name
		Model:        model,
		client:       agentClient,
		registry:     filteredRegistry,
		baseRegistry: baseRegistry,
		permissions:  permManager,
		timeout:      2 * time.Minute,
		history:      make([]*genai.Content, 0),
		status:       AgentStatusPending,
		maxTurns:     maxTurns,
		callHistory:  make(map[string]int),
		ctxCfg:       ctxCfg,
		// Store custom prompt for dynamic type
		projectContext: dynType.SystemPrompt,
	}

	// Wire up RequestTool tool if it exists
	if rt, ok := agent.registry.Get("request_tool"); ok {
		if rtt, ok := rt.(*tools.RequestToolTool); ok {
			rtt.SetRequester(agent)
		}
	}

	// Wire up PinContext tool (Custom Improvement)
	if pt, ok := agent.registry.Get("pin_context"); ok {
		if ptt, ok := pt.(*tools.PinContextTool); ok {
			ptt.SetUpdater(agent.SetPinnedContext)
		}
	}

	// Wire up HistorySearch tool (Custom Improvement)
	if ht, ok := agent.registry.Get("history_search"); ok {
		if htt, ok := ht.(*tools.HistorySearchTool); ok {
			htt.SetHistoryGetter(func() []*genai.Content { return agent.history })
		}
	}

	// Initialize context management
	if ctxCfg != nil {
		agent.tokenCounter = ctxmgr.NewTokenCounter(agent.client, agent.Model, ctxCfg)
		agent.summarizer = ctxmgr.NewSummarizer(agent.client)
		agent.compactor = ctxmgr.NewResultCompactor(ctxCfg.ToolResultMaxChars)
	}

	// Initialize project learning
	if pl, err := memory.NewProjectLearning(workDir); err == nil {
		agent.learning = pl
		// Inject into memorize tool if it exists
		if mt, ok := agent.registry.Get("memorize"); ok {
			if mtt, ok := mt.(interface{ SetLearning(*memory.ProjectLearning) }); ok {
				mtt.SetLearning(pl)
			}
		}
	}

	// Initialize self-reflection capability with LLM client for semantic analysis
	agent.reflector = NewReflector()
	agent.reflector.SetClient(agentClient)

	agent.delegation = NewDelegationStrategy(AgentTypeGeneral, nil)

	return agent
}

// createFilteredRegistryFromList creates a registry with only the specified tools.
func createFilteredRegistryFromList(allowedTools []string, baseRegistry tools.ToolRegistry) *tools.Registry {
	filtered := tools.NewRegistry()

	if len(allowedTools) == 0 {
		// All tools allowed - copy all from base registry
		for _, tool := range baseRegistry.List() {
			_ = filtered.Register(tool)
		}
		return filtered
	}

	allowedMap := make(map[string]bool)
	for _, name := range allowedTools {
		allowedMap[name] = true
	}

	for _, tool := range baseRegistry.List() {
		if allowedMap[tool.Name()] {
			_ = filtered.Register(tool)
		}
	}

	return filtered
}

// SetProjectContext injects project guidelines for sub-agent system prompts.
func (a *Agent) SetProjectContext(ctx string) {
	a.projectContext = ctx
}

// SetOnText sets the streaming callback for real-time output.
func (a *Agent) SetOnText(onText func(string)) {
	a.onText = onText
}

// SetOnScratchpadUpdate sets the callback for scratchpad updates.
func (a *Agent) SetOnScratchpadUpdate(fn func(string)) {
	a.onScratchpadUpdate = fn
}

// SetPinnedContext sets the pinned context for the agent.
func (a *Agent) SetPinnedContext(content string) {
	a.PinnedContext = content
}

// GetPinnedContext returns the pinned context.
func (a *Agent) GetPinnedContext() string {
	return a.PinnedContext
}

// SetOnToolActivity sets the callback for tool activity reporting.
func (a *Agent) SetOnToolActivity(fn func(agentID, toolName string, args map[string]any, status string)) {
	a.onToolActivity = fn
}

// SetStore sets the agent store for checkpoint persistence.
func (a *Agent) SetStore(store *AgentStore) {
	a.store = store
}

// EnableAutoCheckpoint enables automatic checkpointing every N turns.
func (a *Agent) EnableAutoCheckpoint(interval int) {
	a.autoCheckpoint = true
	a.checkpointInterval = interval
	if a.checkpointInterval <= 0 {
		a.checkpointInterval = 5 // Default: every 5 turns
	}
}

// DisableAutoCheckpoint disables automatic checkpointing.
func (a *Agent) DisableAutoCheckpoint() {
	a.autoCheckpoint = false
}

// Close flushes pending data (project learning) to prevent data loss on shutdown.
func (a *Agent) Close() error {
	if a.learning != nil {
		return a.learning.Flush()
	}
	return nil
}

// maybeAutoCheckpoint saves a checkpoint if auto-checkpoint is enabled and interval has passed.
func (a *Agent) maybeAutoCheckpoint() {
	if !a.autoCheckpoint || a.store == nil {
		return
	}

	turnCount := a.GetTurnCount()
	if turnCount-a.lastCheckpointTurn >= a.checkpointInterval {
		if _, err := a.SaveCheckpoint("auto"); err != nil {
			logging.Warn("auto-checkpoint failed", "agent_id", a.ID, "error", err)
		} else {
			a.lastCheckpointTurn = turnCount
			logging.Debug("auto-checkpoint saved", "agent_id", a.ID, "turn", turnCount)
		}
	}
}

// SetOnInput sets the callback for requesting user input.
func (a *Agent) SetOnInput(onInput func(string) (string, error)) {
	a.onInput = onInput
}

// SetOnPlanApproved sets a callback for when a plan is built and ready.
// The callback receives a plan summary and should clear/compact context.
func (a *Agent) SetOnPlanApproved(callback func(planSummary string)) {
	a.onPlanApproved = callback
}

// SetMessenger sets the messenger for inter-agent communication.
func (a *Agent) SetMessenger(m tools.Messenger) {
	a.messenger = m

	// Wire up AskAgentTool if it exists in the registry
	if askTool, ok := a.registry.Get("ask_agent"); ok {
		if aat, ok := askTool.(*tools.AskAgentTool); ok {
			aat.SetMessenger(m)
		}
	}

	// Wire up delegation strategy with messenger
	if a.delegation != nil {
		if am, ok := m.(*AgentMessenger); ok {
			a.delegation.SetMessenger(am)
		}
	}
}

// SetTreePlanner sets the tree planner for planned execution mode.
func (a *Agent) SetTreePlanner(tp *TreePlanner) {
	a.treePlanner = tp

	if tp != nil {
		tp.SetCallbacks(
			func(tree *PlanTree, node *PlanNode) {
				a.IncrementStep("Executing step: " + node.Action.Prompt)
				if a.onText != nil {
					a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
				}
			},
			func(tree *PlanTree, node *PlanNode, success bool) {
				if a.onText != nil {
					a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
				}
			},
			func(tree *PlanTree, ctx *ReplanContext) {
				if a.onText != nil {
					a.safeOnText(fmt.Sprintf("\n[Replanning: %s]\n", ctx.Error))
					a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
				}
			},
			func(action *PlannedAction) {
				// Record planning progress
				if a.onText != nil {
					a.safeOnText(fmt.Sprintf("  • %s: %s\n", action.AgentType, action.Prompt))
				}
				a.SetProgress(0, 0, "Planning: "+action.Prompt)
			},
		)
	}
}

// SetSharedMemory sets the shared memory instance for inter-agent communication.
func (a *Agent) SetSharedMemory(sm *SharedMemory) {
	a.sharedMemory = sm
}

// GetSharedMemory returns the shared memory instance.
func (a *Agent) GetSharedMemory() *SharedMemory {
	return a.sharedMemory
}

// AddToolUsed tracks a tool that was used during execution.
func (a *Agent) AddToolUsed(toolName string) {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	a.toolsUsed = append(a.toolsUsed, toolName)
}

// GetToolsUsed returns the list of tools used during execution.
func (a *Agent) GetToolsUsed() []string {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	result := make([]string, len(a.toolsUsed))
	copy(result, a.toolsUsed)
	return result
}

// SetPlanGoal sets the goal for the plan.
func (a *Agent) SetPlanGoal(goal *PlanGoal) {
	a.planGoal = goal
}

// SetRequireApproval sets whether plan approval is required.
func (a *Agent) SetRequireApproval(required bool) {
	a.requireApproval = required
}

// EnablePlanningMode enables tree-based planning for agent execution.
func (a *Agent) EnablePlanningMode(goal *PlanGoal) {
	a.planningMode = true
	a.planGoal = goal
}

// DisablePlanningMode disables tree-based planning.
func (a *Agent) DisablePlanningMode() {
	a.planningMode = false
	a.planGoal = nil
	a.activePlan = nil
}

// GetActivePlan returns the currently active plan tree.
func (a *Agent) GetActivePlan() *PlanTree {
	return a.activePlan
}

// IsPlanningMode returns whether the agent is in planning mode.
func (a *Agent) IsPlanningMode() bool {
	return a.planningMode
}

// mapModelName maps user-friendly model names to actual Gemini model names.
func mapModelName(name string) string {
	switch strings.ToLower(name) {
	case "flash", "haiku":
		return "gemini-3-flash-preview"
	case "pro", "sonnet":
		return "gemini-3-pro-preview"
	case "ultra", "opus":
		return "gemini-3-pro-preview" // Use pro for ultra/opus for now
	default:
		return name // Return as is if already a full model name
	}
}

// generateAgentID creates a unique identifier for an agent.
func generateAgentID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// createFilteredRegistry creates a registry with only allowed tools for the agent type.
func createFilteredRegistry(agentType AgentType, baseRegistry tools.ToolRegistry) *tools.Registry {
	allowedTools := agentType.AllowedTools()

	// If nil, all tools are allowed (general type)
	if allowedTools == nil {
		// Copy all tools to a new Registry
		filtered := tools.NewRegistry()
		for _, tool := range baseRegistry.List() {
			_ = filtered.Register(tool)
		}
		return filtered
	}

	// Create new registry with filtered tools
	filtered := tools.NewRegistry()
	allowedMap := make(map[string]bool)
	for _, name := range allowedTools {
		allowedMap[name] = true
	}

	for _, tool := range baseRegistry.List() {
		if allowedMap[tool.Name()] {
			_ = filtered.Register(tool)
		}
	}

	return filtered
}

// RequestTool dynamically adds a tool from the base registry to the agent's active registry.
func (a *Agent) RequestTool(name string) error {
	// Check if already in active registry
	if _, ok := a.registry.Get(name); ok {
		return nil // Already have this tool
	}

	tool, ok := a.baseRegistry.Get(name)
	if !ok {
		return fmt.Errorf("tool not found in system: %s", name)
	}

	return a.registry.Register(tool)
}

// SendMessage sends a message to another agent via the messenger.
func (a *Agent) SendMessage(msgType string, toRole string, content string, data map[string]any) (string, error) {
	if a.messenger == nil {
		return "", fmt.Errorf("messenger not initialized for this agent")
	}
	return a.messenger.SendMessage(msgType, toRole, content, data)
}

// ReceiveResponse waits for a response to a previously sent message.
func (a *Agent) ReceiveResponse(ctx context.Context, messageID string) (string, error) {
	if a.messenger == nil {
		return "", fmt.Errorf("messenger not initialized for this agent")
	}
	return a.messenger.ReceiveResponse(ctx, messageID)
}

// Run executes the agent with the given prompt and returns the result.
func (a *Agent) Run(ctx context.Context, prompt string) (*AgentResult, error) {
	a.stateMu.Lock()
	a.status = AgentStatusRunning
	a.startTime = time.Now()
	a.stateMu.Unlock()

	// Initialize progress
	a.SetProgress(0, a.maxTurns, "Starting agent execution")

	result := &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusRunning,
		Completed: false,
	}

	// Build system prompt for the agent
	systemPrompt := a.buildSystemPrompt()

	// Initialize history with system context
	a.stateMu.Lock()
	a.history = []*genai.Content{
		genai.NewContentFromText(systemPrompt, genai.RoleUser),
		genai.NewContentFromText("I understand. I'll help with the task using only my allowed tools.", genai.RoleModel),
	}
	a.stateMu.Unlock()

	// Execute the prompt through the function calling loop
	var finalOutput strings.Builder
	_, output, err := a.executeLoop(ctx, prompt, &finalOutput)
	if err != nil {
		a.stateMu.Lock()
		a.status = AgentStatusFailed
		a.endTime = time.Now()
		endTime := a.endTime
		startTime := a.startTime
		a.stateMu.Unlock()

		// Clear callHistory to prevent memory leak
		a.clearCallHistory()

		result.Status = AgentStatusFailed
		result.Error = err.Error()
		result.Output = output // Preserve partial output on failure
		result.Duration = endTime.Sub(startTime)

		// Update progress with failure
		a.SetProgress(a.currentStep, a.totalSteps, "Failed: "+err.Error())

		return result, err
	}

	a.stateMu.Lock()
	a.status = AgentStatusCompleted
	a.endTime = time.Now()
	endTime := a.endTime
	startTime := a.startTime
	a.stateMu.Unlock()

	// Clear callHistory to prevent memory leak on long-running sessions
	a.clearCallHistory()

	result.Status = AgentStatusCompleted
	result.Output = output
	result.Duration = endTime.Sub(startTime)
	result.Completed = true

	// Update progress with completion
	a.SetProgress(a.totalSteps, a.totalSteps, "Completed")

	return result, nil
}

// clearCallHistory clears the call history map to prevent memory leaks.
func (a *Agent) clearCallHistory() {
	a.callHistoryMu.Lock()
	a.callHistory = make(map[string]int)
	a.callHistoryMu.Unlock()
}

// buildSystemPrompt creates the system prompt based on agent type.
func (a *Agent) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString("You are a specialized sub-agent with limited tool access.\n")
	sb.WriteString(fmt.Sprintf("Agent Type: %s\n", a.Type))
	sb.WriteString("Available tools: ")

	toolNames := a.registry.Names()
	sb.WriteString(strings.Join(toolNames, ", "))
	sb.WriteString("\n\n")

	// Inject Pinned Context if provided (Custom Improvement)
	if a.PinnedContext != "" {
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("                         PINNED CONTEXT\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString(a.PinnedContext)
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n\n")
	}

	// Inject project-specific knowledge
	if a.learning != nil {
		sb.WriteString(a.learning.FormatForPrompt())
		sb.WriteString("\n")
	}

	// Universal instructions for all agents
	sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
	sb.WriteString("                         MANDATORY RULES\n")
	sb.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
	sb.WriteString("1. ALWAYS use tools to complete your task - don't just say you can't\n")
	sb.WriteString("2. After using ANY tool, provide a CLEAR summary of what you found\n")
	sb.WriteString("3. NEVER respond with just 'OK' or 'Done' - always explain\n")
	sb.WriteString("4. Structure responses with markdown: headers, bullets, code blocks\n")
	sb.WriteString("5. Include specific file:line references when discussing code\n\n")

	switch a.Type {
	case AgentTypeExplore:
		sb.WriteString(a.buildExplorePrompt())
	case AgentTypeBash:
		sb.WriteString(a.buildBashPrompt())
	case AgentTypeGeneral:
		sb.WriteString(a.buildGeneralPrompt())
	case AgentTypePlan:
		sb.WriteString(a.buildPlanPrompt())
	case AgentTypeGuide:
		sb.WriteString(a.buildGuidePrompt())
	default:
		sb.WriteString("Complete the assigned task using available tools.\n")
	}

	// Inject project context if provided (for delegated sub-agents)
	if a.projectContext != "" {
		sb.WriteString("\n")
		sb.WriteString(a.projectContext)
		sb.WriteString("\n")
	}

	// Inject scratchpad if not empty
	if a.Scratchpad != "" {
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("                         YOUR SCRATCHPAD\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("This is your persistent memory. Use it to store facts, thoughts, or plans.\n\n")
		sb.WriteString(a.Scratchpad)
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
	}

	// Inject tool usage guides for available tools
	sb.WriteString(a.buildToolGuidesSection())

	return sb.String()
}

// buildToolGuidesSection creates a section with usage guides for available tools.
func (a *Agent) buildToolGuidesSection() string {
	var sb strings.Builder

	toolNames := a.registry.Names()
	if len(toolNames) == 0 {
		return ""
	}

	// Only include guides for tools that have them
	var guidesIncluded []string
	for _, name := range toolNames {
		if guide, ok := ctxmgr.GetToolGuide(name); ok {
			guidesIncluded = append(guidesIncluded, name)
			if len(guidesIncluded) == 1 {
				// Header on first guide
				sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
				sb.WriteString("                     TOOL USAGE GUIDELINES\n")
				sb.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
			}

			sb.WriteString(fmt.Sprintf("### %s\n", name))
			sb.WriteString(fmt.Sprintf("**When to use:** %s\n\n", guide.WhenToUse))
			sb.WriteString(fmt.Sprintf("**How to respond:** %s\n\n", guide.HowToRespond))
			if guide.CommonMistakes != "" {
				sb.WriteString(fmt.Sprintf("**Avoid:** %s\n\n", guide.CommonMistakes))
			}
		}
	}

	// Add relevant chain patterns based on agent type
	if len(guidesIncluded) > 0 {
		sb.WriteString("\n### Tool Chain Patterns\n")
		switch a.Type {
		case AgentTypeExplore:
			if pattern, ok := ctxmgr.ToolChainPatterns["explore_code"]; ok {
				sb.WriteString(pattern)
				sb.WriteString("\n")
			}
			if pattern, ok := ctxmgr.ToolChainPatterns["find_usage"]; ok {
				sb.WriteString(pattern)
				sb.WriteString("\n")
			}
		case AgentTypeBash:
			if pattern, ok := ctxmgr.ToolChainPatterns["debug_error"]; ok {
				sb.WriteString(pattern)
				sb.WriteString("\n")
			}
		case AgentTypeGeneral:
			if pattern, ok := ctxmgr.ToolChainPatterns["implement_feature"]; ok {
				sb.WriteString(pattern)
				sb.WriteString("\n")
			}
		case AgentTypePlan:
			if pattern, ok := ctxmgr.ToolChainPatterns["understand_architecture"]; ok {
				sb.WriteString(pattern)
				sb.WriteString("\n")
			}
		}
	}

	return sb.String()
}

func (a *Agent) buildExplorePrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         EXPLORE AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Explore and analyze the codebase to answer questions.

RECOMMENDED APPROACH:
1. glob - Find relevant files first
2. read - Read key files to understand structure
3. grep - Search for specific patterns/usages
4. Analyze and summarize findings

RESPONSE FORMAT:
## Summary
[Direct answer to the question in 1-2 sentences]

## Key Findings
- **Finding 1** (file.go:123): Description
- **Finding 2** (other.go:45): Description

## Code Examples
` + "```" + `go
// Relevant code snippet with explanation
` + "```" + `

## Architecture
[How components connect, data flow, dependencies]

## Recommendations
[What to look at next, potential issues, suggestions]

═══════════════════════════════════════════════════════════════════════

EXAMPLE - GOOD RESPONSE:
User: "How does authentication work?"

## Summary
Authentication uses JWT tokens validated by middleware in auth/middleware.go.

## Key Findings
- **Token validation** (auth/middleware.go:45): Validates JWT on every request
- **Token generation** (auth/service.go:78): Creates tokens with 24h expiry
- **User lookup** (auth/repo.go:32): Fetches user from database

## Code Examples
` + "```" + `go
// middleware.go:45-52
func ValidateToken(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        token := r.Header.Get("Authorization")
        claims, err := validateJWT(token)
        // ...
    })
}
` + "```" + `

## Architecture
` + "```" + `
Request → Middleware → Validate JWT → Handler
                ↓
         auth/service.go (token ops)
                ↓
         auth/repo.go (user data)
` + "```" + `

## Recommendations
- Consider adding token refresh mechanism
- Rate limiting should be added to login endpoint

═══════════════════════════════════════════════════════════════════════

EXAMPLE - BAD RESPONSE (NEVER DO THIS):
User: "How does authentication work?"
[reads files, says nothing or just "It uses JWT"]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildBashPrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         BASH AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Execute shell commands safely and explain results.

APPROACH:
1. Understand what command to run
2. Execute the command
3. Analyze the output
4. Explain results clearly

RESPONSE FORMAT:
## Command Executed
` + "```" + `bash
[The command you ran]
` + "```" + `

## Results Summary
[What the command did and what output means]

## Details
[Specific output analysis, errors, warnings]

## Next Steps
[What to do based on results]

═══════════════════════════════════════════════════════════════════════

EXAMPLE - GOOD RESPONSE:
User: "Run the tests"

## Command Executed
` + "```" + `bash
go test ./...
` + "```" + `

## Results Summary
**45 passed**, **2 failed**, **3.2s** total runtime

## Failed Tests

### TestUserCreate (user_test.go:34)
- **Expected**: status 201
- **Got**: status 400
- **Cause**: Missing required field 'email' in test fixture

### TestDBConnection (db_test.go:12)
- **Error**: connection timeout
- **Cause**: Test database not running

## Next Steps
1. Fix TestUserCreate: Add email field to fixture at line 30
2. Fix TestDBConnection: Run ` + "`docker-compose up -d`" + ` first
3. Re-run tests after fixes

═══════════════════════════════════════════════════════════════════════

EXAMPLE - BAD RESPONSE (NEVER DO THIS):
User: "Run the tests"
[runs test, shows raw output only]
or
"Tests completed." [no details]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGeneralPrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         GENERAL AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Complete the assigned task using all available tools.

APPROACH:
1. Understand the task completely
2. Plan your approach (read before write)
3. Execute step by step
4. Verify your work
5. Summarize what was done

RESPONSE FORMAT:
## Task Summary
[What you were asked to do]

## Changes Made
- **file1.go**: [What changed and why]
- **file2.go**: [What changed and why]

## Verification
[How to verify the changes work]

## Summary
[Overall what was accomplished]

═══════════════════════════════════════════════════════════════════════

KEY RULES:
- ALWAYS read files before editing them
- Explain what you're changing and why
- Show before/after for significant changes
- Suggest how to verify the changes work

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildPlanPrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         PLAN AGENT (READ-ONLY)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Design an implementation plan for the requested feature.
NOTE: You are READ-ONLY - you cannot modify files.

APPROACH:
1. Explore codebase to understand patterns
2. Identify files that need modification
3. Consider architectural trade-offs
4. Create detailed step-by-step plan

PLAN FORMAT:
## Overview
[Brief description of what will be implemented]

## Files to Modify
1. **path/to/file.go** - [What changes needed]
2. **path/to/other.go** - [What changes needed]

## Implementation Steps
### Step 1: [Title]
- [ ] Task 1.1
- [ ] Task 1.2

### Step 2: [Title]
- [ ] Task 2.1
- [ ] Task 2.2

## Testing Strategy
- Unit tests for [components]
- Integration tests for [flows]

## Risks & Considerations
- [Potential issue 1]: Mitigation
- [Potential issue 2]: Mitigation

═══════════════════════════════════════════════════════════════════════

KEY RULES:
- Be specific about file paths and line numbers
- Consider existing patterns in the codebase
- Break down into small, verifiable steps
- Identify potential risks upfront

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGuidePrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         GUIDE AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Answer questions about Gokin CLI and its features.

APPROACH:
1. Search documentation for accurate info
2. Provide clear explanations with examples
3. Include usage instructions
4. Help with troubleshooting

RESPONSE FORMAT:
## Answer
[Clear, direct answer to the question]

## Details
[In-depth explanation if needed]

## Examples
` + "```" + `bash
# Example usage
gokin [command] [options]
` + "```" + `

## Related Information
[Other relevant features or documentation]

═══════════════════════════════════════════════════════════════════════

KEY RULES:
- Be accurate - verify information before stating
- Include practical examples
- Mention relevant config options
- Link to related features

═══════════════════════════════════════════════════════════════════════
`
}

// executeLoop runs the function calling loop for the agent.
func (a *Agent) executeLoop(ctx context.Context, prompt string, output *strings.Builder) ([]*genai.Content, string, error) {
	// Add user prompt to history (protected by mutex)
	userContent := genai.NewContentFromText(prompt, genai.RoleUser)
	a.stateMu.Lock()
	a.history = append(a.history, userContent)
	a.stateMu.Unlock()

	// Update progress
	a.SetProgress(1, a.maxTurns, "Processing request")

	// === Tree planning mode: Build plan tree if enabled ===
	if a.treePlanner != nil && a.planningMode {
		tree, err := a.treePlanner.BuildTree(ctx, prompt, a.planGoal)
		if err != nil {
			logging.Warn("failed to build plan tree, falling back to reactive mode", "error", err)
		} else {
			a.activePlan = tree
			if a.onText != nil {
				a.safeOnText(fmt.Sprintf("\n[Plan tree built: %d nodes, best path: %d steps]\n",
					tree.TotalNodes, len(tree.BestPath)))
			}

			// Notify plan approval callback for context compaction
			if a.onPlanApproved != nil {
				planSummary := a.treePlanner.GeneratePlanSummary(tree)
				a.onPlanApproved(planSummary)
			}

			// Set total steps to best path length using SetProgress for thread safety
			a.SetProgress(0, len(tree.BestPath), "Building plan...")

			// === Interactive Plan Review ===
			if a.onInput != nil && a.requireApproval {
				if err := a.requestPlanApproval(ctx, tree); err != nil {
					return a.history, output.String(), err
				}
			} else if a.onText != nil {
				// Show plan tree even if approval not required
				a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
			}
		}
	}

	loopRecoveryTurns := 0
	replanAttempts := 0
	var i int
	// Use min(maxTurns, MaxTurnLimit) to prevent infinite loops
	effectiveMaxTurns := a.maxTurns
	if effectiveMaxTurns > MaxTurnLimit {
		effectiveMaxTurns = MaxTurnLimit
		logging.Warn("maxTurns exceeds MaxTurnLimit, capping", "agent_id", a.ID,
			"requested", a.maxTurns, "capped", MaxTurnLimit)
	}
	for i = 0; i < effectiveMaxTurns; i++ {
		select {
		case <-ctx.Done():
			return a.history, output.String(), ctx.Err()
		default:
		}

		// Auto-checkpoint if enabled
		a.maybeAutoCheckpoint()

		// Check tokens and summarize if needed to prevent context overflow.
		// We do this BEFORE getting model response to ensure we have room.
		if a.tokenCounter != nil && a.summarizer != nil && a.ctxCfg != nil && a.ctxCfg.EnableAutoSummary {
			if err := a.checkAndSummarize(ctx); err != nil {
				logging.Warn("auto-summarization failed", "agent_id", a.ID, "error", err)
				if a.onText != nil {
					a.safeOnText("\n[Warning: context optimization failed — conversation may hit length limits]\n")
				}
			}
		}

		// Update progress at start of each turn
		if i > 0 && a.activePlan == nil {
			a.SetProgress(i+1, a.maxTurns, fmt.Sprintf("Turn %d: Executing tools", i+1))
		}

		// === Planned mode: Execute from plan tree ===
		if a.activePlan != nil {
			actions, err := a.treePlanner.GetReadyActions(a.activePlan)
			if err != nil {
				// No more actions in plan, check if completed
				a.safeOnText("\n[Plan completed or no more actions available]\n")
				a.activePlan = nil // Exit planned mode
			} else if len(actions) > 0 {
				type parallelResult struct {
					action *PlannedAction
					result *AgentResult
				}

				var wg sync.WaitGroup
				var resMu sync.Mutex
				results := make([]parallelResult, 0, len(actions))

				for _, act := range actions {
					wg.Add(1)
					go func(action *PlannedAction) {
						defer wg.Done()

						a.safeOnText(fmt.Sprintf("\n[Executing planned step: %s %s]\n",
							action.Type, action.AgentType))

						result := a.executePlannedAction(ctx, action)

						// Record result in tree (RecordResult is thread-safe)
						if err := a.treePlanner.RecordResult(a.activePlan, action.NodeID, result); err != nil {
							logging.Warn("failed to record plan result", "error", err)
						}

						resMu.Lock()
						results = append(results, parallelResult{action, result})
						resMu.Unlock()
					}(act)
				}
				wg.Wait()

				// Process results and collect failures
				var firstFailure *parallelResult
				for i := range results {
					res := &results[i]
					if res.result.Output != "" {
						output.WriteString(res.result.Output)
					}
					// Track first failure for potential replan
					if !res.result.IsSuccess() && firstFailure == nil {
						firstFailure = res
					}
				}

				// Handle failure with single replan attempt
				if firstFailure != nil {
					if a.treePlanner.ShouldReplan(a.activePlan, firstFailure.result) && replanAttempts < 3 {
						replanAttempts++

						// Build replan context with reflection
						var reflection *Reflection
						if a.reflector != nil && firstFailure.action.ToolName != "" {
							reflection = a.reflector.Reflect(firstFailure.action.ToolName, firstFailure.action.ToolArgs, firstFailure.result.Error)
						}

						// Find the node in the tree for replanning
						node, nodeFound := a.activePlan.GetNode(firstFailure.action.NodeID)
						if !nodeFound || node == nil {
							logging.Warn("failed node not found in tree, switching to reactive mode",
								"node_id", firstFailure.action.NodeID)
							a.activePlan = nil
							continue
						}

						replanCtx := &ReplanContext{
							FailedNode:    node,
							Error:         firstFailure.result.Error,
							Reflection:    reflection,
							AttemptNumber: replanAttempts,
						}

						a.safeOnText(fmt.Sprintf("\n[Replanning after failure of step \"%s\" (attempt %d)...]\n",
							firstFailure.action.Prompt, replanAttempts))

						if err := a.treePlanner.Replan(ctx, a.activePlan, replanCtx); err != nil {
							logging.Warn("replan failed", "error", err)
							a.activePlan = nil // Exit planned mode on replan failure
						}
					} else {
						// Max replans exceeded or should not replan
						a.safeOnText("\n[Plan failed, switching to reactive mode]\n")
						a.activePlan = nil
					}
				}
				continue
			}
		}

		// === Reactive mode: Get response from model ===
		resp, err := a.getModelResponse(ctx)
		if err != nil {
			return a.history, output.String(), fmt.Errorf("model response error: %w", err)
		}

		// Add model response to history (protected by mutex)
		modelContent := &genai.Content{
			Role:  genai.RoleModel,
			Parts: a.buildResponseParts(resp),
		}
		a.stateMu.Lock()
		a.history = append(a.history, modelContent)
		a.stateMu.Unlock()

		// Accumulate text output
		if resp.Text != "" {
			output.WriteString(resp.Text)
			// Stream text to UI in real-time
			if a.onText != nil {
				a.safeOnText(resp.Text)
			}
		}

		// If there are function calls, execute them
		if len(resp.FunctionCalls) > 0 {
			// Track progress for delegation strategy
			if a.delegation != nil {
				toolsList := make([]string, 0, len(resp.FunctionCalls))
				for _, fc := range resp.FunctionCalls {
					toolsList = append(toolsList, fc.Name)
				}
				a.delegation.TrackProgress(strings.Join(toolsList, ","))
			}

			// Mental Loop Detection
			for _, fc := range resp.FunctionCalls {
				argsJSON, _ := json.Marshal(fc.Args)
				key := fmt.Sprintf("%s:%s", fc.Name, string(argsJSON))

				a.callHistoryMu.Lock()
				a.callHistory[key]++
				count := a.callHistory[key]
				intervened := a.loopIntervened
				a.callHistoryMu.Unlock()

				if count > 3 && !intervened {
					logging.Warn("mental loop detected", "tool", fc.Name, "count", count)
					a.callHistoryMu.Lock()
					a.loopIntervened = true
					a.callHistoryMu.Unlock()

					// Notify user
					if a.onText != nil {
						a.safeOnText(fmt.Sprintf("\n[Loop detected: %s called %d times with same args — intervening]\n", fc.Name, count))
					}

					// Build reflection-based intervention with strategy switching
					intervention := a.buildLoopRecoveryIntervention(fc.Name, fc.Args, count)

					// Clear this specific call from history to allow retry with different args
					a.callHistoryMu.Lock()
					delete(a.callHistory, key)
					a.callHistoryMu.Unlock()

					a.stateMu.Lock()
					a.history = append(a.history, genai.NewContentFromText(intervention, genai.RoleUser))
					// Give bounded extra turns to recover (max 3)
					if loopRecoveryTurns < 3 {
						loopRecoveryTurns++
						a.maxTurns++
					}
					a.stateMu.Unlock()
					continue
				}
			}

			// Update progress to show tool execution
			toolsList := make([]string, 0, len(resp.FunctionCalls))
			for _, fc := range resp.FunctionCalls {
				toolsList = append(toolsList, fc.Name)
			}
			a.SetProgress(i+1, a.maxTurns, fmt.Sprintf("Executing tools: %v", toolsList))

			results := a.executeTools(ctx, resp.FunctionCalls)

			// Add function response to history
			funcParts := make([]*genai.Part, len(results))
			for j, result := range results {
				funcParts[j] = genai.NewPartFromFunctionResponse(result.Name, result.Response)
				funcParts[j].FunctionResponse.ID = result.ID
			}
			funcContent := &genai.Content{
				Role:  genai.RoleUser,
				Parts: funcParts,
			}
			a.stateMu.Lock()
			a.history = append(a.history, funcContent)
			a.stateMu.Unlock()

			continue
		}

		// No more function calls, we're done
		break
	}

	// Notify user if the model produced no output
	if output.Len() == 0 {
		emptyMsg := "\n[Model returned an empty response — try rephrasing your request]\n"
		output.WriteString(emptyMsg)
		if a.onText != nil {
			a.safeOnText(emptyMsg)
		}
	}

	// Notify user if we hit the max turn limit
	if i >= a.maxTurns {
		if a.onText != nil {
			a.safeOnText("\n[Reached maximum turn limit — stopping]\n")
		}
	}

	return a.history, output.String(), nil
}

// buildLoopRecoveryIntervention creates a reflection-based intervention message for mental loop recovery.
// This helps the agent understand what went wrong and suggests alternative approaches.
func (a *Agent) buildLoopRecoveryIntervention(toolName string, args map[string]any, count int) string {
	var sb strings.Builder

	sb.WriteString("STOP. I've detected that I'm stuck in a loop.\n\n")
	sb.WriteString("**What I was doing:**\n")
	sb.WriteString(fmt.Sprintf("- Calling `%s` with the same arguments %d times\n", toolName, count))

	// Extract key arguments for context
	if args != nil {
		if path, ok := args["path"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Path: `%s`\n", path))
		}
		if pattern, ok := args["pattern"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Pattern: `%s`\n", pattern))
		}
		if cmd, ok := args["command"].(string); ok {
			sb.WriteString(fmt.Sprintf("- Command: `%s`\n", cmd))
		}
	}

	sb.WriteString("\n**Why this isn't working:**\n")
	sb.WriteString("- Repeating the same action will give the same result\n")
	sb.WriteString("- I need to change my approach, not retry the same thing\n\n")

	// Suggest alternatives based on the tool
	sb.WriteString("**What I should try instead:**\n")
	switch toolName {
	case "read":
		sb.WriteString("- Use `glob` to find the correct file path first\n")
		sb.WriteString("- Check if the file exists with `bash ls -la <dir>`\n")
		sb.WriteString("- Try a different file that might have the information\n")
	case "grep":
		sb.WriteString("- Simplify my search pattern\n")
		sb.WriteString("- Use `glob` to confirm files exist first\n")
		sb.WriteString("- Try different keywords or regex patterns\n")
		sb.WriteString("- Search in a different directory\n")
	case "glob":
		sb.WriteString("- Try a broader pattern like `**/*`\n")
		sb.WriteString("- Check directory existence with `bash ls`\n")
		sb.WriteString("- Use `tree` to see the directory structure\n")
	case "bash":
		sb.WriteString("- Check if the command exists with `which <cmd>`\n")
		sb.WriteString("- Try a simpler version of the command first\n")
		sb.WriteString("- Use `read` to examine related files for clues\n")
	case "edit":
		sb.WriteString("- Read the file first to understand its current state\n")
		sb.WriteString("- Check if my old_string actually exists in the file\n")
		sb.WriteString("- Use `grep` to find the exact text I need to replace\n")
	case "write":
		sb.WriteString("- Read the target path first to understand what's there\n")
		sb.WriteString("- Check directory permissions\n")
		sb.WriteString("- Verify the parent directory exists\n")
	default:
		sb.WriteString("- Step back and reconsider my overall approach\n")
		sb.WriteString("- Try gathering more context before acting\n")
		sb.WriteString("- Use a different tool to achieve the same goal\n")
	}

	sb.WriteString("\nI will now try a DIFFERENT approach to achieve my goal.\n")

	return sb.String()
}

// checkAndSummarize monitors token usage and triggers summarization if thresholds are met.
func (a *Agent) checkAndSummarize(ctx context.Context) error {
	// 0. Check hard limit on history size to prevent memory exhaustion
	a.stateMu.RLock()
	historyLen := len(a.history)
	a.stateMu.RUnlock()

	if historyLen > MaxHistorySize {
		logging.Warn("history size exceeded MaxHistorySize, forcing compaction",
			"agent_id", a.ID, "history_len", historyLen, "max", MaxHistorySize)
		return a.forceCompactHistory(ctx)
	}

	// 1. Get current token usage
	tokenCount, err := a.tokenCounter.CountContents(ctx, a.history)
	if err != nil {
		return fmt.Errorf("failed to count tokens: %w", err)
	}

	limits := a.tokenCounter.GetLimits()
	threshold := limits.WarningThreshold
	if threshold == 0 {
		threshold = 0.8
	}

	percentUsed := float64(tokenCount) / float64(limits.MaxInputTokens)

	// 2. If below threshold, do nothing
	if percentUsed < threshold {
		return nil
	}

	logging.Info("context threshold reached, compacting history",
		"agent_id", a.ID,
		"usage", fmt.Sprintf("%.1f%%", percentUsed*100),
		"tokens", tokenCount)

	// 3. Summarize history
	// We keep the system prompt (first 2 messages) and the last few turns
	if len(a.history) <= 6 {
		return nil // Not enough history to summarize effectively
	}

	// Keep first 2 (system context) and last 4 (recent turns)
	historyToSummarize := a.history[2 : len(a.history)-4]
	remainingHistory := a.history[len(a.history)-4:]

	summary, err := a.summarizer.Summarize(ctx, historyToSummarize)
	if err != nil {
		return fmt.Errorf("summarization failed: %w", err)
	}

	// 4. Reconstruct history: [System Context] + [Summary] + [Recent Turns]
	newHistory := make([]*genai.Content, 0, len(remainingHistory)+3)
	newHistory = append(newHistory, a.history[0], a.history[1])
	newHistory = append(newHistory, summary)
	newHistory = append(newHistory, remainingHistory...)

	a.history = newHistory

	logging.Info("context history compacted", "agent_id", a.ID, "new_message_count", len(a.history))

	return nil
}

// forceCompactHistory aggressively compacts history when MaxHistorySize is exceeded.
// This is a safety measure to prevent memory exhaustion.
func (a *Agent) forceCompactHistory(ctx context.Context) error {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if len(a.history) <= 10 {
		return nil // Not enough to compact
	}

	// Keep only the first 2 messages (system context) and last 8 (recent turns)
	// Everything else is discarded (not summarized) to ensure we reduce memory
	keepStart := 2
	keepEnd := 8
	if len(a.history) < keepStart+keepEnd {
		return nil
	}

	newHistory := make([]*genai.Content, 0, keepStart+keepEnd+1)
	newHistory = append(newHistory, a.history[:keepStart]...)

	// Add a placeholder indicating history was truncated
	truncateNotice := genai.NewContentFromText(
		"[Previous conversation history was truncated to prevent memory exhaustion. "+
			"Context may be incomplete.]",
		genai.RoleUser)
	newHistory = append(newHistory, truncateNotice)

	newHistory = append(newHistory, a.history[len(a.history)-keepEnd:]...)

	a.history = newHistory
	logging.Info("history force-compacted", "agent_id", a.ID, "new_len", len(a.history))

	return nil
}

// getModelResponse gets a response from the model.
func (a *Agent) getModelResponse(ctx context.Context) (*client.Response, error) {
	// Read history under lock for thread safety
	a.stateMu.RLock()
	historyLen := len(a.history)
	if historyLen == 0 {
		a.stateMu.RUnlock()
		return nil, fmt.Errorf("empty history")
	}
	lastContent := a.history[historyLen-1]
	a.stateMu.RUnlock()

	// Check if the last content contains function responses (tool results).
	// If so, use SendFunctionResponse instead of SendMessageWithHistory
	// to avoid sending an empty message string to APIs that reject it.
	if lastContent.Role == genai.RoleUser {
		var funcResponses []*genai.FunctionResponse
		for _, part := range lastContent.Parts {
			if part.FunctionResponse != nil {
				funcResponses = append(funcResponses, &genai.FunctionResponse{
					ID:       part.FunctionResponse.ID,
					Name:     part.FunctionResponse.Name,
					Response: part.FunctionResponse.Response,
				})
			}
		}

		if len(funcResponses) > 0 {
			// Route through SendFunctionResponse for proper API formatting
			// Copy history under lock
			a.stateMu.RLock()
			historyWithoutLast := make([]*genai.Content, len(a.history)-1)
			copy(historyWithoutLast, a.history[:len(a.history)-1])
			a.stateMu.RUnlock()

			stream, err := a.client.SendFunctionResponse(ctx, historyWithoutLast, funcResponses)
			if err != nil {
				return nil, err
			}
			return stream.Collect()
		}
	}

	// Extract text message from last user content
	var message string
	if lastContent.Role == genai.RoleUser {
		for _, part := range lastContent.Parts {
			if part.Text != "" {
				message = part.Text
				break
			}
		}
	}

	// Safety: ensure message is not empty
	if message == "" {
		message = "Continue."
	}

	// Copy history under lock
	a.stateMu.RLock()
	historyWithoutLast := make([]*genai.Content, len(a.history)-1)
	copy(historyWithoutLast, a.history[:len(a.history)-1])
	a.stateMu.RUnlock()

	stream, err := a.client.SendMessageWithHistory(ctx, historyWithoutLast, message)
	if err != nil {
		return nil, err
	}

	return stream.Collect()
}

// executeTools executes the function calls with parallel execution for read-only tools.
func (a *Agent) executeTools(ctx context.Context, calls []*genai.FunctionCall) []*genai.FunctionResponse {
	results := make([]*genai.FunctionResponse, len(calls))

	// Build index for result placement
	callIndex := make(map[*genai.FunctionCall]int)
	for i, call := range calls {
		callIndex[call] = i
	}

	// Classify tools into parallel groups
	classifier := NewToolDependencyClassifier()
	// Optimize call order for better parallelism (reads before writes)
	calls = classifier.OptimizeForParallelism(calls)
	groups := classifier.ClassifyDependencies(calls)

	for _, group := range groups {
		if group.Parallel && len(group.Calls) > 1 {
			// Execute read-only tools in parallel
			a.executeToolsParallel(ctx, group.Calls, results, callIndex)
		} else {
			// Execute sequentially (write tools or single tool)
			for _, call := range group.Calls {
				idx := callIndex[call]
				results[idx] = a.executeToolWithReflection(ctx, call)
			}
		}
	}

	return results
}

// executeToolsParallel executes multiple tools concurrently.
func (a *Agent) executeToolsParallel(ctx context.Context, calls []*genai.FunctionCall,
	results []*genai.FunctionResponse, indexMap map[*genai.FunctionCall]int) {

	var wg sync.WaitGroup
	var mu sync.Mutex
	semaphore := make(chan struct{}, 5) // Max 5 concurrent executions

	for _, call := range calls {
		// Check context before spawning goroutine to avoid unnecessary work
		if ctx.Err() != nil {
			mu.Lock()
			results[indexMap[call]] = &genai.FunctionResponse{
				ID:       call.ID,
				Name:     call.Name,
				Response: tools.NewErrorResult("cancelled").ToMap(),
			}
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(fc *genai.FunctionCall) {
			defer wg.Done()

			// Check context again before trying to acquire semaphore
			if ctx.Err() != nil {
				mu.Lock()
				results[indexMap[fc]] = &genai.FunctionResponse{
					ID:       fc.ID,
					Name:     fc.Name,
					Response: tools.NewErrorResult("cancelled").ToMap(),
				}
				mu.Unlock()
				return
			}

			// Acquire semaphore slot with timeout to prevent goroutine leak
			acquired := false
			select {
			case semaphore <- struct{}{}:
				acquired = true
			case <-ctx.Done():
				mu.Lock()
				results[indexMap[fc]] = &genai.FunctionResponse{
					ID:       fc.ID,
					Name:     fc.Name,
					Response: tools.NewErrorResult("cancelled").ToMap(),
				}
				mu.Unlock()
				return
			}

			if acquired {
				defer func() { <-semaphore }()
			}

			result := a.executeToolWithReflection(ctx, fc)

			mu.Lock()
			results[indexMap[fc]] = result
			mu.Unlock()
		}(call)
	}

	// Wait with timeout to prevent infinite blocking
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines completed normally
	case <-ctx.Done():
		// Context cancelled, but goroutines should exit on their own
		// Wait a bit more for cleanup
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logging.Warn("executeToolsParallel: some goroutines did not exit in time")
		}
	}
}

// executeToolWithReflection executes a tool with reflection and delegation on failure.
func (a *Agent) executeToolWithReflection(ctx context.Context, call *genai.FunctionCall) *genai.FunctionResponse {
	result := a.executeTool(ctx, call)

	var reflection *Reflection

	// Apply self-reflection on errors to provide recovery suggestions
	if !result.Success && a.reflector != nil {
		reflection = a.reflector.Reflect(call.Name, call.Args, result.Content)
		if reflection.Intervention != "" {
			// Enrich the error result with reflection analysis
			result.Content = fmt.Sprintf("%s\n\n---\n**Self-Reflection:**\n%s",
				result.Content, reflection.Intervention)

			// Log reflection
			logging.Info("agent reflected on error",
				"agent_id", a.ID,
				"tool", call.Name,
				"category", reflection.Category,
				"should_retry", reflection.ShouldRetry)
		}
	}

	// Check for autonomous delegation opportunity
	if !result.Success && a.delegation != nil && a.delegation.messenger != nil {
		delCtx := &DelegationContext{
			AgentType:      a.Type,
			CurrentTurn:    a.currentStep,
			MaxTurns:       a.maxTurns,
			LastToolName:   call.Name,
			LastToolError:  result.Content,
			LastToolArgs:   call.Args,
			ReflectionInfo: reflection,
			StuckCount:     a.delegation.GetStuckCount(),
		}

		decision := a.delegation.Evaluate(delCtx)
		if decision.ShouldDelegate {
			// Execute delegation
			delegationResponse, err := a.delegation.ExecuteDelegation(ctx, decision)
			if err == nil && delegationResponse != "" {
				// Append delegation result to the tool response
				result.Content = fmt.Sprintf("%s\n\n---\n**Delegated to %s agent:**\n%s",
					result.Content, decision.TargetType, delegationResponse)
				result.Success = true // Mark as recovered

				logging.Info("delegation successful",
					"agent_id", a.ID,
					"delegated_to", decision.TargetType,
					"reason", decision.Reason)
			}
		}
	}

	// Compact result if it's too large before converting to map
	if a.compactor != nil {
		result = a.compactor.CompactForType(call.Name, result)
	}

	return &genai.FunctionResponse{
		ID:       call.ID, // Must match tool_use.id for Anthropic/DeepSeek API
		Name:     call.Name,
		Response: result.ToMap(),
	}
}

// executeTool executes a single tool call with enhanced safety and retry logic.
func (a *Agent) executeTool(ctx context.Context, call *genai.FunctionCall) tools.ToolResult {
	tool, ok := a.registry.Get(call.Name)
	if !ok {
		return tools.NewErrorResult(fmt.Sprintf("tool not available for this agent: %s", call.Name))
	}

	// Validate arguments
	if err := tool.Validate(call.Args); err != nil {
		return tools.NewErrorResult(fmt.Sprintf("validation error: %s", err))
	}

	// Check permissions before executing
	if a.permissions != nil {
		resp, err := a.permissions.Check(ctx, call.Name, call.Args)
		if err != nil {
			return tools.NewErrorResult(fmt.Sprintf("permission error: %s", err))
		}
		if !resp.Allowed {
			reason := resp.Reason
			if reason == "" {
				reason = "permission denied"
			}
			return tools.NewErrorResult(fmt.Sprintf("Permission denied: %s", reason))
		}
	}

	// Report tool start to UI
	if a.onToolActivity != nil {
		a.onToolActivity(a.ID, call.Name, call.Args, "start")
	}

	// === IMPROVEMENT 2: Retry mechanism with exponential backoff ===
	maxRetries := 3
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Execute the tool with parent context for cancellation support
		// The tool itself (e.g., bash) is responsible for implementing its own timeout
		result, err := tool.Execute(ctx, call.Args)
		if err == nil {
			// Report tool end to UI
			if a.onToolActivity != nil {
				a.onToolActivity(a.ID, call.Name, call.Args, "end")
			}

			// Success - return result
			return result
		}

		// Record error for potential retry
		lastErr = err

		// Check if this is a retryable error
		if !isRetryableError(err) || attempt == maxRetries-1 {
			// Not retryable or last attempt - return error immediately
			return tools.NewErrorResult(err.Error())
		}

		// Log retry attempt
		logging.Warn("tool execution failed, retrying",
			"tool", call.Name,
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err.Error())

		// Exponential backoff: 1s, 2s, 4s...
		backoffDuration := time.Duration(1<<uint(attempt)) * time.Second
		select {
		case <-time.After(backoffDuration):
			// Continue to next attempt
		case <-ctx.Done():
			// Context cancelled - stop retrying
			return tools.NewErrorResult("cancelled during retry backoff")
		}
	}

	// All retries exhausted
	return tools.NewErrorResult(fmt.Sprintf("failed after %d retries: %s", maxRetries, lastErr.Error()))
}

// isRetryableError determines if an error is worth retrying
func isRetryableError(err error) bool {
	return client.IsRetryableError(err)
}

// buildResponseParts creates Parts from a response.
// Returns at least one part to avoid empty Parts which causes API errors.
func (a *Agent) buildResponseParts(resp *client.Response) []*genai.Part {
	var parts []*genai.Part

	if resp.Text != "" {
		parts = append(parts, genai.NewPartFromText(resp.Text))
	}

	for _, fc := range resp.FunctionCalls {
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}

	// Ensure we never return empty parts - API requires at least one part
	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(" "))
	}

	return parts
}

// GetStatus returns the current agent status.
func (a *Agent) GetStatus() AgentStatus {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.status
}

// Cancel cancels the agent's execution.
func (a *Agent) Cancel() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.status == AgentStatusRunning {
		a.status = AgentStatusCancelled
		a.endTime = time.Now()
	}
}

// safeOnText streams text to the UI in a thread-safe manner.
func (a *Agent) safeOnText(text string) {
	if a.onText == nil {
		return
	}
	a.onTextMu.Lock()
	defer a.onTextMu.Unlock()
	a.onText(text)
}

// executePlannedAction executes a single planned action and returns the result.
func (a *Agent) executePlannedAction(ctx context.Context, action *PlannedAction) *AgentResult {
	if action == nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "nil action",
		}
	}

	startTime := time.Now()

	switch action.Type {
	case ActionToolCall:
		return a.executeToolAction(ctx, action, startTime)
	case ActionDelegate:
		return a.executeDelegateAction(ctx, action, startTime)
	case ActionVerify:
		return a.executeVerifyAction(ctx, action, startTime)
	case ActionDecompose:
		return a.executeDecomposeAction(ctx, action, startTime)
	default:
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   fmt.Sprintf("unknown action type: %s", action.Type),
		}
	}
}

// executeDecomposeAction handles a decomposition milestone.
func (a *Agent) executeDecomposeAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	if a.activePlan == nil || a.treePlanner == nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "no active plan or tree planner",
		}
	}

	// Find the node in the active plan
	node, ok := a.activePlan.GetNode(action.NodeID)
	if !ok {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "node not found in plan",
		}
	}

	if a.onText != nil {
		a.safeOnText(fmt.Sprintf("\n[Expanding milestone: %s]\n", action.Prompt))
	}

	// Expand the milestone into sub-tasks
	if err := a.treePlanner.ExpandMilestone(ctx, a.activePlan, node); err != nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   fmt.Sprintf("decomposition failed: %v", err),
		}
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    fmt.Sprintf("Milestone expanded: %s", action.Prompt),
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeToolAction executes a tool call action.
func (a *Agent) executeToolAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	// Generate unique ID for planned action tool calls
	idBytes := make([]byte, 12)
	rand.Read(idBytes)
	toolID := "toolu_" + hex.EncodeToString(idBytes)

	call := &genai.FunctionCall{
		ID:   toolID,
		Name: action.ToolName,
		Args: action.ToolArgs,
	}

	result := a.executeTool(ctx, call)

	status := AgentStatusCompleted
	errMsg := ""
	if !result.Success {
		status = AgentStatusFailed
		errMsg = result.Content
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    status,
		Output:    result.Content,
		Error:     errMsg,
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeDelegateAction delegates work to a sub-agent.
func (a *Agent) executeDelegateAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	if a.delegation == nil || a.delegation.messenger == nil {
		// No delegation support, execute directly with current agent
		return a.executeDirectly(ctx, action, startTime)
	}

	// Request delegation through messenger
	decision := &DelegationDecision{
		ShouldDelegate: true,
		TargetType:     string(action.AgentType),
		Reason:         "planned delegation",
		Query:          action.Prompt,
	}

	response, err := a.delegation.ExecuteDelegation(ctx, decision)
	if err != nil {
		return &AgentResult{
			AgentID:   a.ID,
			Type:      a.Type,
			Status:    AgentStatusFailed,
			Error:     fmt.Sprintf("delegation failed: %v", err),
			Duration:  time.Since(startTime),
			Completed: true,
		}
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    response,
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeDirectly executes an action without delegation.
func (a *Agent) executeDirectly(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	// For non-delegation actions, run the prompt through the model
	var output strings.Builder

	// Add the action prompt to history temporarily
	promptContent := genai.NewContentFromText(action.Prompt, genai.RoleUser)
	a.history = append(a.history, promptContent)

	// Get model response
	resp, err := a.getModelResponse(ctx)
	if err != nil {
		return &AgentResult{
			AgentID:   a.ID,
			Type:      a.Type,
			Status:    AgentStatusFailed,
			Error:     err.Error(),
			Duration:  time.Since(startTime),
			Completed: true,
		}
	}

	// Process response
	if resp.Text != "" {
		output.WriteString(resp.Text)
	}

	// Execute any function calls
	if len(resp.FunctionCalls) > 0 {
		results := a.executeTools(ctx, resp.FunctionCalls)
		for _, r := range results {
			if r.Response != nil {
				if content, ok := r.Response["content"].(string); ok {
					output.WriteString("\n")
					output.WriteString(content)
				}
			}
		}
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    output.String(),
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeVerifyAction runs verification checks.
func (a *Agent) executeVerifyAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	// Verification typically involves running tests or checking criteria
	var output strings.Builder

	// Use bash agent to run tests if available
	verifyPrompt := "Verify the implementation is complete. " + action.Prompt

	if a.delegation != nil && a.delegation.messenger != nil {
		decision := &DelegationDecision{
			ShouldDelegate: true,
			TargetType:     string(AgentTypeBash),
			Reason:         "verification",
			Query:          "Run tests to verify: " + verifyPrompt,
		}

		response, err := a.delegation.ExecuteDelegation(ctx, decision)
		if err != nil {
			return &AgentResult{
				AgentID:   a.ID,
				Type:      a.Type,
				Status:    AgentStatusFailed,
				Error:     fmt.Sprintf("verification failed: %v", err),
				Duration:  time.Since(startTime),
				Completed: true,
			}
		}

		output.WriteString(response)

		// Check for test failures in output
		if strings.Contains(strings.ToLower(response), "fail") ||
			strings.Contains(strings.ToLower(response), "error") {
			return &AgentResult{
				AgentID:   a.ID,
				Type:      a.Type,
				Status:    AgentStatusFailed,
				Output:    output.String(),
				Error:     "verification detected failures",
				Duration:  time.Since(startTime),
				Completed: true,
			}
		}
	} else {
		output.WriteString("Verification step (no test runner available)")
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    output.String(),
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// requestPlanApproval handles the interactive review and editing of a plan.
func (a *Agent) requestPlanApproval(ctx context.Context, tree *PlanTree) error {
	if a.onInput == nil || a.onText == nil {
		return nil
	}

	for {
		// Show current plan
		a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
		a.safeOnText("Commands: [Enter] approve | e <n> <prompt> | d <n> | a [type] <prompt> | c cancel\n")
		a.safeOnText("Types: explore, plan, general, bash, decompose (default: general)\n")

		response, err := a.onInput("Plan approval > ")
		if err != nil {
			return err
		}

		response = strings.TrimSpace(response)
		if response == "" {
			// Approved
			a.safeOnText("[Plan approved]\n")
			return nil
		}

		parts := strings.Fields(response)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "c", "cancel", "abort":
			return fmt.Errorf("plan rejected by user")
		case "e", "edit":
			if len(parts) < 3 {
				a.safeOnText("Usage: e <num> <new prompt>\n")
				continue
			}
			var num int
			if _, err := fmt.Sscanf(parts[1], "%d", &num); err != nil {
				a.safeOnText("Invalid step number\n")
				continue
			}
			if num < 1 || num > len(tree.BestPath) {
				a.safeOnText("Step number out of range\n")
				continue
			}

			newPrompt := strings.Join(parts[2:], " ")
			tree.BestPath[num-1].Action.Prompt = newPrompt
			a.safeOnText(fmt.Sprintf("Step %d updated\n", num))

		case "d", "delete":
			if len(parts) < 2 {
				a.safeOnText("Usage: d <num>\n")
				continue
			}
			var num int
			if _, err := fmt.Sscanf(parts[1], "%d", &num); err != nil {
				a.safeOnText("Invalid step number\n")
				continue
			}
			if num < 1 || num > len(tree.BestPath) {
				a.safeOnText("Step number out of range\n")
				continue
			}

			// Remove node from best path
			tree.BestPath = append(tree.BestPath[:num-1], tree.BestPath[num:]...)
			a.safeOnText(fmt.Sprintf("Step %d deleted\n", num))

		case "a", "add":
			if len(parts) < 2 {
				a.safeOnText("Usage: a <prompt>\n")
				continue
			}
			prompt := strings.Join(parts[1:], " ")
			agentType := AgentTypeGeneral

			// Check if first word of prompt is a known type
			if len(parts) > 2 {
				potentialType := ParseAgentType(parts[1])
				if potentialType != "" || parts[1] == "decompose" {
					agentType = potentialType
					if parts[1] == "decompose" {
						agentType = AgentTypePlan // Use plan agent for decompose milestones
					}
					prompt = strings.Join(parts[2:], " ")
				}
			}

			// Add as child of root for now (end of plan)
			tree.AddNode(tree.Root.ID, &PlannedAction{
				Type:      ActionDelegate,
				AgentType: agentType,
				Prompt:    prompt,
			})
			if agentType == "" { // Was decompose
				node, _ := tree.GetNode(tree.Root.ID)
				if len(node.Children) > 0 {
					lastChild := node.Children[len(node.Children)-1]
					lastChild.Action.Type = ActionDecompose
				}
			}
			tree.BestPath = a.treePlanner.SelectBestPath(tree)
			a.safeOnText("[Step added]\n")

		default:
			a.safeOnText(fmt.Sprintf("Unknown command: %s\n", cmd))
		}
	}
}

