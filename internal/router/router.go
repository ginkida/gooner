package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gokin/internal/agent"
	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/hybrid"
	"gokin/internal/logging"
	"gokin/internal/tools"

	"google.golang.org/genai"
)

// PlanChecker interface for checking planning state
type PlanChecker interface {
	IsActive() bool
	IsEnabled() bool
}

// routingRecord stores a routing decision and its outcome for learning.
type routingRecord struct {
	message   string
	taskType  TaskType
	strategy  ExecutionStrategy
	success   bool
	timestamp time.Time
}

// Router determines the optimal execution strategy for incoming tasks
// and routes them to the appropriate handler (direct, executor, or sub-agent).
type Router struct {
	analyzer    *TaskAnalyzer
	executor    *tools.Executor
	agentRunner AgentRunner
	// client is swapped by SetClient when the provider/key/model changes
	// (ApplyConfig on /login, /provider, /model). clientMu guards it because
	// Execute reads it on the request goroutine while ApplyConfig writes it on
	// the app goroutine — without the lock a /login mid-request races, and the
	// stale pre-swap client would keep getting the per-request thinking budget
	// and tools (so the new key silently went unused until restart).
	clientMu sync.RWMutex
	client   client.Client
	// thinkingMode (config.ThinkingMode{Auto,On,Off}) is the user's reasoning
	// intent; selectThinkingBudget honors it. Guarded by clientMu — same
	// write-on-ApplyConfig / read-on-request shape as client.
	thinkingMode string
	// engineMode controls whether the optional computation plane is disabled,
	// explicit, or exposed adaptively for the current request.
	engineMode string
	// planMode mirrors the app's planning-mode toggle. When true, Execute must
	// NOT re-add mutating tools via per-request FilteredGeminiTools — plan mode's
	// hard schema defense (write/edit/bash removed) would otherwise be defeated.
	// Guarded by clientMu, same write-on-toggle / read-on-request shape.
	planMode bool
	workDir  string

	// Tool filtering
	registry  *tools.Registry // Tool registry for per-request filtering
	isGitRepo bool            // Whether working dir is a git repo

	// Plan awareness
	planChecker PlanChecker

	// Configuration
	enabled            bool
	decomposeThreshold int
	parallelThreshold  int
	costAware          bool
	fastModel          string
	modelCapability    *ModelCapability

	// Learned routing
	routingHistory []routingRecord
	historyMu      sync.RWMutex

	// Context awareness
	recentErrors     int
	recentOps        int
	conversationMode string // "exploring", "implementing", "debugging", "refactoring"

	// Session-depth counters feed the adaptive thinking-budget logic. Executor
	// calls RecordTurn after each model turn; ResetDepth clears on /clear.
	depthMu       sync.RWMutex
	depthTurns    int
	depthTools    int
	depthPressure bool
}

// AgentRunner interface for spawning agents (implemented by agent.Runner)
type AgentRunner interface {
	Spawn(ctx context.Context, agentType string, prompt string, maxTurns int, model string) (string, error)
	SpawnAsync(ctx context.Context, agentType string, prompt string, maxTurns int, model string) string
	GetResult(agentID string) (*agent.AgentResult, bool)
}

// AgentSideEffectError means a sub-agent failed after crossing at least one
// potentially-stateful tool boundary, or its final result became unavailable
// after the agent had started. Re-running the top-level prompt with a fresh
// sub-agent could duplicate an edit, bash command, remote mutation, or unknown
// MCP/plugin action, so automatic retry layers must treat this error as unsafe.
//
// Result is the caller-owned snapshot returned by AgentRunner.GetResult and
// retains partial output, touched paths, and other failure metadata for UI and
// diagnostics. It is nil only when result provenance itself is unavailable.
type AgentSideEffectError struct {
	AgentID              string
	Cause                error
	Result               *agent.AgentResult
	PartialOutput        string
	StatefulToolAttempts int
	SideEffectsUnknown   bool
}

func (e *AgentSideEffectError) Error() string {
	if e == nil {
		return "sub-agent side-effect state is unsafe to retry"
	}
	reason := "sub-agent stopped before finishing"
	if e.Cause != nil && strings.TrimSpace(e.Cause.Error()) != "" {
		reason += ": " + e.Cause.Error()
	}
	switch {
	case e.SideEffectsUnknown:
		reason += " (the started agent's side-effect provenance is unavailable; automatic retry blocked)"
	case e.StatefulToolAttempts > 0:
		reason += fmt.Sprintf(" (after %d stateful tool attempt(s); automatic retry blocked)", e.StatefulToolAttempts)
	default:
		reason += " (automatic retry blocked)"
	}
	if strings.TrimSpace(e.PartialOutput) != "" {
		reason += " (partial output preserved in error metadata)"
	}
	return reason
}

func (e *AgentSideEffectError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// AutomaticRetrySafe is a marker consumed by outer request/recovery layers.
// A false value is intentionally stronger than ordinary retry taxonomy: the
// underlying cause may be transient while replaying already-started work is not.
func (*AgentSideEffectError) AutomaticRetrySafe() bool { return false }

// IsAutomaticRetryUnsafe recognizes the marker through wrapped error chains.
// Keeping the predicate here lets callers block retries without depending on
// the concrete AgentSideEffectError type.
func IsAutomaticRetryUnsafe(err error) bool {
	var safety interface{ AutomaticRetrySafe() bool }
	return errors.As(err, &safety) && !safety.AutomaticRetrySafe()
}

func newAgentSideEffectError(agentID string, cause error, result *agent.AgentResult, unknown bool) *AgentSideEffectError {
	if cause == nil {
		cause = errors.New("sub-agent result unavailable")
	}
	unsafeErr := &AgentSideEffectError{
		AgentID:            agentID,
		Cause:              cause,
		Result:             result,
		SideEffectsUnknown: unknown,
	}
	if result != nil {
		unsafeErr.PartialOutput = result.Output
		unsafeErr.StatefulToolAttempts = result.StatefulToolAttempts
	}
	return unsafeErr
}

// RouterConfig holds configuration for the router
type RouterConfig struct {
	Enabled            bool
	DecomposeThreshold int              // Default: 4
	ParallelThreshold  int              // Default: 7
	CostAware          bool             // Enable cost-aware model selection
	FastModel          string           // Model for simple tasks (e.g., "glm-5-turbo", "deepseek-v4-flash")
	ModelCapability    *ModelCapability // Model capability for adaptive routing
	EngineMode         string           // tools, auto (default), or hybrid
}

// NewRouter creates a new task router
func NewRouter(cfg *RouterConfig, executor *tools.Executor, agentRunner AgentRunner, client client.Client, registry *tools.Registry, isGitRepo bool, workDir string) *Router {
	if cfg == nil {
		cfg = &RouterConfig{
			Enabled:            true,
			DecomposeThreshold: 4,
			ParallelThreshold:  7,
		}
	}

	return &Router{
		analyzer:           NewTaskAnalyzer(cfg.DecomposeThreshold, cfg.ParallelThreshold),
		executor:           executor,
		agentRunner:        agentRunner,
		client:             client,
		workDir:            workDir,
		registry:           registry,
		isGitRepo:          isGitRepo,
		enabled:            cfg.Enabled,
		decomposeThreshold: cfg.DecomposeThreshold,
		parallelThreshold:  cfg.ParallelThreshold,
		costAware:          cfg.CostAware,
		fastModel:          cfg.FastModel,
		modelCapability:    cfg.ModelCapability,
		engineMode:         cfg.EngineMode,
		routingHistory:     make([]routingRecord, 0, 100),
	}
}

// SetPlanChecker sets the plan checker for plan-aware routing.
func (r *Router) SetPlanChecker(checker PlanChecker) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	r.planChecker = checker
}

// Route determines the best execution strategy and returns a routing decision
func (r *Router) Route(message string) *RoutingDecision {
	return r.RouteWithContext(context.Background(), message)
}

// RouteWithContext is like Route but propagates the caller's context to
// LLM-backed decomposition. Prefer this when a context is available so the
// operation can be cancelled by parent timeout.
func (r *Router) RouteWithContext(ctx context.Context, message string) *RoutingDecision {
	return r.routeWithContext(ctx, message, true)
}

// routeWithContext optionally derives request tool sets for callers that use
// the RoutingDecision directly. Execute derives its model-visible schema from
// the original policy message later, so calculating SuggestedToolSets while
// routing its possibly augmented retry message would be both unused and an
// avoidable second hybrid-policy scan.
func (r *Router) routeWithContext(ctx context.Context, message string, includeSuggestedTools bool) *RoutingDecision {
	analysis := r.analyzer.Analyze(message)

	// Model capability adjustments: weaker models decompose earlier
	r.applyCapabilityAdjustments(analysis)

	// If a plan is actively being executed, prefer simpler strategies
	// to avoid nested planning/coordination
	r.historyMu.RLock()
	planChecker := r.planChecker
	r.historyMu.RUnlock()
	planActive := planChecker != nil && planChecker.IsActive()
	if planActive {
		logging.Debug("plan active, using simplified routing",
			"original_score", analysis.Score,
			"original_strategy", analysis.Strategy)
		// Reduce complexity score when plan is active to prevent nested decomposition
		if analysis.Score > r.decomposeThreshold {
			analysis.Score = r.decomposeThreshold - 1
		}
		// Don't use sub-agents during plan execution (plan steps are already sub-agents)
		if analysis.Strategy == StrategySubAgent {
			analysis.Strategy = StrategyExecutor
		}
	}

	// Context-aware adjustment: high error rate suggests debugging mode
	if r.GetErrorRate() > 0.3 && analysis.Strategy != StrategySubAgent {
		logging.Debug("high error rate detected, preferring executor for debugging",
			"error_rate", r.GetErrorRate(),
			"mode", r.GetConversationMode())
		// In debugging mode, prefer executor over direct/sub-agent
		// because it allows iterative tool use
		if analysis.Strategy == StrategyDirect {
			analysis.Strategy = StrategyExecutor
		}
	}

	logging.Debug("task routed",
		"message", message,
		"complexity", analysis.Score,
		"type", analysis.Type,
		"strategy", analysis.Strategy,
		"reasoning", analysis.Reasoning)

	// Adjust strategy based on learned history
	r.adjustStrategyFromHistory(analysis)

	decision := &RoutingDecision{
		Analysis:    analysis,
		Message:     message,
		ShouldRoute: true,
	}

	// Check for automatic decomposition for high-complexity tasks
	if analysis.Score >= r.decomposeThreshold {
		decomposition := r.analyzer.DecomposeWithContext(ctx, message)
		if len(decomposition.Subtasks) > 1 {
			decision.Handler = HandlerCoordinated
			decision.Decomposition = decomposition
			decision.Reasoning = fmt.Sprintf("Auto-decomposition: %d subtasks (%s)",
				len(decomposition.Subtasks), decomposition.Reasoning)
			if includeSuggestedTools {
				decision.SuggestedToolSets = r.selectToolSetsForMessage(analysis, message)
			}

			logging.Info("task decomposed",
				"message", message,
				"subtasks", len(decomposition.Subtasks),
				"can_parallel", decomposition.CanParallel)

			return decision
		}
	}

	// Determine which handler to use
	switch analysis.Strategy {
	case StrategyDirect:
		decision.Handler = HandlerDirect
		decision.Reasoning = "Direct AI response without tools"

	case StrategySingleTool:
		decision.Handler = HandlerExecutor
		decision.Reasoning = "Expecting a single tool call"

	case StrategyExecutor:
		decision.Handler = HandlerExecutor
		decision.Reasoning = "Standard execution via function calling loop"

	case StrategySubAgent:
		decision.Handler = HandlerSubAgent
		decision.SubAgentType = r.selectSubAgentType(analysis.Type)
		decision.Background = analysis.Type == TaskTypeBackground
		decision.Reasoning = fmt.Sprintf("Using sub-agent type '%s'", decision.SubAgentType)

	default:
		decision.Handler = HandlerExecutor
		decision.Reasoning = "Standard strategy"
	}

	// Cost-aware model selection
	if r.costAware && r.fastModel != "" {
		decision.SuggestedModel = r.selectCostAwareModel(analysis)
	}

	// Dynamic thinking budget based on complexity
	decision.ThinkingBudget = r.selectThinkingBudget(analysis)

	// Per-request tool filtering
	if includeSuggestedTools {
		decision.SuggestedToolSets = r.selectToolSetsForMessage(analysis, message)
	}

	return decision
}

// SetClient swaps the client used for per-request configuration (thinking
// budget, tools). Called from ApplyConfig when the provider, API key, or model
// changes so a /login takes effect in-process without a restart. SmartRouter
// embeds *Router, so it inherits this.
func (r *Router) SetClient(c client.Client) {
	r.clientMu.Lock()
	r.client = c
	r.clientMu.Unlock()
}

// getClient returns the current client under the read lock.
func (r *Router) getClient() client.Client {
	r.clientMu.RLock()
	defer r.clientMu.RUnlock()
	return r.client
}

// SetThinkingMode sets the reasoning intent (auto/on/off) honored by
// selectThinkingBudget. Wired from the builder and ApplyConfig so a /thinking or
// /set change takes effect in-process. SmartRouter inherits it via embedding.
func (r *Router) SetThinkingMode(mode string) {
	r.clientMu.Lock()
	r.thinkingMode = config.ResolveThinkingMode(mode)
	r.clientMu.Unlock()
}

// SetEngineMode synchronizes request-level hybrid eligibility with the App's
// boot-pinned engine mode when the long-lived router receives a new client.
// Changing engine.mode itself requires a restart because registry shape and
// secure-runtime ownership cannot be partially rebuilt in place.
func (r *Router) SetEngineMode(mode string) {
	r.clientMu.Lock()
	r.engineMode = mode
	r.clientMu.Unlock()
}

func (r *Router) getEngineMode() string {
	r.clientMu.RLock()
	defer r.clientMu.RUnlock()
	return r.engineMode
}

// SetModelCapability updates the model-dependent routing policy after a live
// /model or /provider switch. The router outlives its client, so leaving the
// boot-time capability in place applies the previous model's decomposition,
// tool filtering, and thinking multiplier to the new one.
func (r *Router) SetModelCapability(capability *ModelCapability) {
	r.clientMu.Lock()
	r.modelCapability = capability
	r.clientMu.Unlock()
}

func (r *Router) getModelCapability() *ModelCapability {
	r.clientMu.RLock()
	capability := r.modelCapability
	r.clientMu.RUnlock()
	return capability
}

// CurrentModelCapability returns a value snapshot of the routing capability.
// The boolean is false when no profile has been configured.
func (r *Router) CurrentModelCapability() (ModelCapability, bool) {
	capability := r.getModelCapability()
	if capability == nil {
		return ModelCapability{}, false
	}
	return *capability, true
}

// getThinkingMode returns the resolved mode (defaults to auto when unset).
func (r *Router) getThinkingMode() string {
	r.clientMu.RLock()
	defer r.clientMu.RUnlock()
	return config.ResolveThinkingMode(r.thinkingMode)
}

// SetPlanMode mirrors the app's planning-mode toggle into the router so
// Execute keeps mutating tools out of the API schema while plan mode is active.
// Wired from the builder, ApplyConfig, and the plan-mode toggle.
func (r *Router) SetPlanMode(enabled bool) {
	r.clientMu.Lock()
	r.planMode = enabled
	r.clientMu.Unlock()
}

// isPlanMode reports whether planning mode is active.
func (r *Router) isPlanMode() bool {
	r.clientMu.RLock()
	defer r.clientMu.RUnlock()
	return r.planMode
}

// Execute routes the task to the appropriate handler and returns the result
func (r *Router) Execute(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	return r.ExecuteWithPolicyMessage(ctx, history, message, message)
}

// ExecuteWithPolicyMessage routes message while deriving request-scoped tool
// exposure from policyMessage. Automatic retries may prepend internal
// continuation context to the executable message; that scaffolding must not
// silently change the hybrid policy selected for the user's original request.
func (r *Router) ExecuteWithPolicyMessage(
	ctx context.Context,
	history []*genai.Content,
	message string,
	policyMessage string,
) ([]*genai.Content, string, error) {
	if strings.TrimSpace(policyMessage) == "" {
		policyMessage = message
	}
	mode := r.getEngineMode()
	policyDecision := hybrid.Decide(mode, policyMessage)
	autoDecision := policyDecision
	if strings.EqualFold(strings.TrimSpace(mode), "hybrid") {
		// Explicit hybrid exposure is not a reason to steer ordinary targeted
		// work into the REPL, so its hint still uses auto eligibility. Every
		// other accepted/default mode either disables the REPL or already uses
		// the auto decision, making a second prompt scan unnecessary.
		autoDecision = hybrid.Decide("auto", policyMessage)
	}
	return r.ExecuteWithPolicyDecisions(
		ctx, history, message, mode, policyDecision, autoDecision,
	)
}

// ExecuteWithPolicyDecisions is ExecuteWithPolicyMessage with an immutable
// engine-mode snapshot and request-level hybrid decisions supplied by the
// caller. App request setup already computes these to establish the
// authoritative schema ceiling and journal event; reusing them here keeps
// routing, schema exposure, and prompt guidance consistent while avoiding
// repeated prompt scans.
func (r *Router) ExecuteWithPolicyDecisions(
	ctx context.Context,
	history []*genai.Content,
	message string,
	engineMode string,
	policyDecision hybrid.Decision,
	autoDecision hybrid.Decision,
) ([]*genai.Content, string, error) {
	// Preserve the user's original message for the history record — the routing
	// scaffolding below augments `message` with analysis/thinking hints we don't
	// want persisted.
	originalMessage := message
	decision := r.routeWithContext(ctx, message, false)

	cl := r.getClient()

	// Apply thinking budget for this request
	cl.SetThinkingBudget(decision.ThinkingBudget)

	// Apply per-request tool filtering. In plan mode ALWAYS apply the plan-mode
	// (read-only) set instead — per-request FilteredGeminiTools filters by
	// ToolSet with no plan-mode awareness, so it would re-add write/edit/bash to
	// the schema and defeat plan mode's hard schema defense. Setting the plan set
	// unconditionally (even with no SuggestedToolSets) keeps the schema safe
	// regardless of what tools a prior request left on the shared client.
	replExposed := false
	if r.registry != nil {
		var schema []*genai.Tool
		switch {
		case r.isPlanMode():
			schema = r.registry.PlanModeGeminiTools()
			if !policyDecision.Enabled {
				schema = tools.FilterGeminiToolsExcluding(schema, "repl_exec", "harness")
			}
		default:
			// RouteWithContext classifies the executable retry message for
			// handler selection, while hybrid exposure stays anchored to the
			// original request through policyMessage.
			sets := r.selectToolSetsForDecision(decision.Analysis, engineMode, policyDecision)
			if len(sets) > 0 {
				schema = r.registry.FilteredGeminiTools(sets...)
			}
		}
		if schema != nil {
			if ceiling, restricted := tools.ToolSchemaCeilingFromContext(ctx, r.executor); restricted {
				schema = tools.FilterGeminiToolsByCapability(schema, ceiling)
			}
			// The per-request schema is pushed onto the SHARED client, so it
			// replaces whatever the invocation capability ceiling
			// (--tools/--disallowedTools) filtered at startup. Re-apply it here
			// or the model is handed tools the executor will refuse at call
			// time.
			if ceiling, restricted := tools.ToolCapabilityCeilingFromContext(ctx); restricted {
				schema = tools.FilterGeminiToolsByCapability(schema, ceiling)
			}
			replExposed = schemaContainsDeclaration(schema, "repl_exec")
			cl.SetTools(schema)
		}
	}

	// Add tool usage hint based on task type
	if hint := r.toolHintForDecision(decision.Analysis, autoDecision, replExposed); hint != "" {
		message = hint + "\n\n" + message
	}

	// Add thinking hint for complex tasks
	if decision.Analysis.Score >= 4 || decision.Analysis.Strategy == StrategySubAgent {
		message = "Before acting, analyze the problem step by step and consider edge cases.\n\n" + message
	}

	// Direct/Executor call Execute with the AUGMENTED message below — capture
	// where the injected user turn will land so it can be restored to
	// originalMessage before the result is persisted (see
	// restoreOriginalUserMessage).
	priorLen := len(history)

	// Skill authority is one-shot per user request, and Executor.Execute is the
	// only handler that promotes-and-clears it. Direct, sub-agent and
	// coordinated turns bypass that boundary entirely, so an explicit `/skill`
	// handoff routed to one of them would stay pending and activate in a later,
	// unrelated request. Close the turn here for exactly those handlers.
	if decision.Handler != HandlerExecutor {
		tools.BeginSkillPermissionTurn(r.registry)
	}

	switch decision.Handler {
	case HandlerDirect:
		// Direct AI response without tools
		newHistory, resp, err := r.executeDirect(ctx, history, message)
		return restoreOriginalUserMessage(newHistory, priorLen, originalMessage), resp, err

	case HandlerExecutor:
		// Standard function calling loop
		newHistory, resp, err := r.executor.Execute(ctx, history, message)
		return restoreOriginalUserMessage(newHistory, priorLen, originalMessage), resp, err

	case HandlerSubAgent:
		// Spawn a sub-agent. The sub-agent path returns a MINIMAL history (just
		// the synthesized response), so extend the prior conversation rather
		// than letting the caller replace it.
		h, resp, err := r.executeViaSubAgent(ctx, message, decision.SubAgentType, decision.Background)
		var sideEffectErr *AgentSideEffectError
		if err != nil && len(h) > 0 && errors.As(err, &sideEffectErr) {
			// A failed stateful run remains an error (outer retries must stop), but
			// its partial model output is still a valid conversation artifact. Extend
			// it with the original user turn before returning; handing the App the
			// raw minimal slice would persist a model-only history on an empty/new
			// session and lose the continuation anchor on an established one.
			extended, _, _ := extendConversation(history, originalMessage, h, resp, nil)
			return extended, resp, err
		}
		return extendConversation(history, originalMessage, h, resp, err)

	case HandlerCoordinated:
		// Execute via coordinator with decomposed subtasks (also minimal history).
		h, resp, err := r.executeCoordinated(ctx, decision.Decomposition)
		return extendConversation(history, originalMessage, h, resp, err)

	default:
		newHistory, resp, err := r.executor.Execute(ctx, history, message)
		return restoreOriginalUserMessage(newHistory, priorLen, originalMessage), resp, err
	}
}

// restoreOriginalUserMessage undoes the routing-scaffolding augmentation
// (tool-usage/thinking hints prepended to `message` in Execute before it was
// sent to the model) in the history that gets PERSISTED. Executor.Execute
// always appends exactly one genai.NewContentFromText(message, RoleUser)
// turn at index priorLen before doing anything else, on every return path —
// success or failure — so newHistory[priorLen] is reliably that injected
// turn. Without this, HandlerDirect/HandlerExecutor (unlike
// HandlerSubAgent/HandlerCoordinated, which already use originalMessage via
// extendConversation) permanently baked the hint text into the user's
// persisted turn: session resume/reload showed the scaffolding instead of
// what the user actually typed, and session_memory.go's extractCurrentTask
// (which parses recent user messages) picked up hint noise instead of the
// real task.
//
// Defensive: if newHistory[priorLen] doesn't match the exact shape
// Executor.Execute produces (a single plain-text user Part), the history is
// returned unchanged rather than risk corrupting an unrelated turn.
func restoreOriginalUserMessage(newHistory []*genai.Content, priorLen int, originalMessage string) []*genai.Content {
	if priorLen < 0 || priorLen >= len(newHistory) {
		return newHistory
	}
	injected := newHistory[priorLen]
	if injected == nil || injected.Role != genai.RoleUser || len(injected.Parts) != 1 {
		return newHistory
	}
	part := injected.Parts[0]
	if part == nil || part.FunctionCall != nil || part.FunctionResponse != nil || part.Text == "" {
		return newHistory
	}
	newHistory[priorLen] = genai.NewContentFromText(originalMessage, genai.RoleUser)
	return newHistory
}

// extendConversation makes a minimal-history handler result (sub-agent /
// coordinated, which return only the synthesized response) EXTEND the prior
// conversation instead of replacing it. Without this, the caller's
// SetHistory(newHistory) wipes every prior turn AND the user's message — the
// next turn starts with amnesia. Direct/executor handlers already return a
// full extended history, so they bypass this.
func extendConversation(prior []*genai.Content, userMessage string, result []*genai.Content, response string, err error) ([]*genai.Content, string, error) {
	if err != nil {
		return result, response, err
	}
	extended := make([]*genai.Content, 0, len(prior)+1+len(result))
	extended = append(extended, prior...)
	if strings.TrimSpace(userMessage) != "" {
		extended = append(extended, genai.NewContentFromText(userMessage, genai.RoleUser))
	}
	extended = append(extended, result...)
	return extended, response, nil
}

// executeDirect gets a direct AI response without tool usage.
// executor.Execute already prepends the user message, so we must NOT add it here.
func (r *Router) executeDirect(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	return r.executor.Execute(ctx, history, message)
}

// executeViaSubAgent spawns a sub-agent to handle the task
func (r *Router) executeViaSubAgent(ctx context.Context, message string, agentType string, background bool) ([]*genai.Content, string, error) {
	if r.agentRunner == nil {
		return nil, "", fmt.Errorf("sub-agent requested but agent runner is not configured")
	}

	// Auto-infer thoroughness for explore/bash agents based on task complexity
	if agentType == "explore" || agentType == "bash" {
		analysis := r.analyzer.Analyze(message)
		switch {
		case analysis.Score <= 3:
			ctx = tools.WithThoroughness(ctx, tools.ThoroughnessQuick)
		case analysis.Score >= 7:
			ctx = tools.WithThoroughness(ctx, tools.ThoroughnessThorough)
		}
	}

	logging.Info("spawning sub-agent",
		"type", agentType,
		"background", background,
		"message", message)

	var agentID string
	var err error

	if background {
		// Spawn in background, return immediately
		agentID = r.agentRunner.SpawnAsync(ctx, agentType, message, 30, "")
		if agentID == "" {
			return nil, "", fmt.Errorf("background %s agent failed to start: runner returned an empty ID", agentType)
		}
		backgroundMsg := fmt.Sprintf("Background agent %s started for the task. ID: %s\nUse /tasks %s to view live output or /tasks stop %s to cancel.", agentType, agentID, agentID, agentID)
		return nil, backgroundMsg, nil
	}

	// Spawn and wait for completion
	agentID, err = r.agentRunner.Spawn(ctx, agentType, message, 30, "")

	// CRITICAL: Spawn is synchronous and stores the agent's PARTIAL result
	// (result.Output + result.Error) into the runner BEFORE returning a non-nil
	// err — agent.Run sets result.Error ONLY on its err!=nil path, so the real
	// failure shape is `err != nil`, NOT `err == nil && result.Error != ""`.
	// The old code returned on `if err != nil` here, BEFORE GetResult, so the
	// preserve-block below was structurally dead and minutes of sub-agent work
	// vanished (the "agent worked then suddenly stopped, nothing to continue
	// from" field report). Only a never-started spawn (empty agentID) is a true
	// failure with nothing to preserve. Mirrors loop_runner_adapter.
	if agentID == "" {
		if err != nil {
			return nil, "", fmt.Errorf("failed to spawn sub-agent: %w", err)
		}
		return nil, "", fmt.Errorf("sub-agent did not start")
	}

	// Get result (present even on a post-Run failure).
	result, ok := r.agentRunner.GetResult(agentID)
	if !ok {
		// The agent definitely started, but its terminal provenance disappeared.
		// We cannot prove that it stayed read-only, so a fresh automatic run must
		// fail closed instead of potentially duplicating an unknown side effect.
		if err != nil {
			return nil, "", newAgentSideEffectError(agentID, err, nil, true)
		}
		return nil, "", newAgentSideEffectError(agentID,
			fmt.Errorf("sub-agent %s did not return a result", agentID), nil, true)
	}

	// The failure reason comes from the Spawn err OR the stored result.Error.
	failureReason := result.Error
	if failureReason == "" && err != nil {
		failureReason = err.Error()
	}

	if failureReason != "" {
		failureCause := err
		if failureCause == nil {
			failureCause = errors.New(failureReason)
		}
		var partialHistory []*genai.Content
		partialResponse := ""
		if strings.TrimSpace(result.Output) != "" {
			partialResponse = result.Output +
				"\n\n⚠ The agent stopped before finishing: " + failureReason +
				"\nThe work above is partial — review it, then ask me to continue from here."
			partialHistory = []*genai.Content{
				genai.NewContentFromText(partialResponse, genai.RoleModel),
			}
		}
		if result.StatefulToolAttempts > 0 {
			// Partial prose is useful evidence, but it cannot turn a failed mutated
			// run into success: doing so hides the failure boundary, while returning
			// the raw transient error invites the App's outer retry to duplicate work.
			return partialHistory, partialResponse,
				newAgentSideEffectError(agentID, failureCause, result, false)
		}
		// The sub-agent stopped before finishing — but it may have done real work
		// first (the agent preserves it in result.Output). Surface that partial
		// work plus an actionable reason and keep the turn alive, instead of
		// discarding minutes of effort and ending with a bare error. This mirrors
		// the task-tool and plan-execution paths, which never throw away a failed
		// agent's output. Only a truly-empty failure propagates as an error.
		if partialResponse != "" {
			return partialHistory, partialResponse, nil
		}
		return nil, "", fmt.Errorf("sub-agent stopped before finishing: %s", failureReason)
	}

	// Format output
	var response string
	if result.Output != "" {
		response = result.Output
	} else {
		response = fmt.Sprintf("Agent %s completed the task", agentType)
	}

	// Return minimal history (just the result)
	history := []*genai.Content{
		genai.NewContentFromText(response, genai.RoleModel),
	}

	return history, response, nil
}

// selectSubAgentType chooses the appropriate sub-agent type based on task type
func (r *Router) selectSubAgentType(taskType TaskType) string {
	switch taskType {
	case TaskTypeExploration:
		return "explore"
	case TaskTypeBackground:
		return "bash"
	case TaskTypeRefactoring:
		return "general" // Refactoring needs write access
	case TaskTypeComplex:
		return "general"
	case TaskTypeMultiTool:
		return "general"
	default:
		return "general"
	}
}

// SubtaskResult holds the result of a subtask execution.
type SubtaskResult struct {
	ID      string
	AgentID string
	Output  string
	Error   string
	Success bool
}

// maxParallelSubtasks is the maximum number of subtasks to execute in parallel.
const maxParallelSubtasks = 5

// executeCoordinated executes decomposed subtasks via coordinator
func (r *Router) executeCoordinated(ctx context.Context, decomposition *DecompositionResult) ([]*genai.Content, string, error) {
	if r.agentRunner == nil {
		return nil, "", fmt.Errorf("agent runner not configured for coordination")
	}

	logging.Info("executing coordinated task",
		"subtasks", len(decomposition.Subtasks),
		"can_parallel", decomposition.CanParallel)

	var allOutputs strings.Builder
	allOutputs.WriteString("## Coordinated Execution\n\n")
	fmt.Fprintf(&allOutputs, "Task decomposed into %d subtasks.\n\n", len(decomposition.Subtasks))

	if decomposition.CanParallel {
		allOutputs.WriteString("**Mode:** Parallel execution\n\n")
	} else {
		allOutputs.WriteString("**Mode:** Sequential execution\n\n")
	}

	// Track completed tasks and their results
	completed := make(map[string]bool)
	subtaskResults := make(map[string]*SubtaskResult)
	var resultsMu sync.Mutex

	for {
		// Find ready tasks (dependencies met)
		var ready []Subtask
		for _, st := range decomposition.Subtasks {
			if completed[st.ID] {
				continue
			}

			// Check dependencies
			depsOK := true
			for _, dep := range st.Dependencies {
				if !completed[dep] {
					depsOK = false
					break
				}
			}

			if depsOK {
				ready = append(ready, st)
			}
		}

		if len(ready) == 0 {
			break // All done or blocked
		}

		// Execute ready tasks - parallel if allowed, sequential otherwise
		if decomposition.CanParallel && len(ready) > 1 {
			// Parallel execution with semaphore
			var wg sync.WaitGroup
			semaphore := make(chan struct{}, maxParallelSubtasks)

			for _, st := range ready {
				wg.Add(1)
				semaphore <- struct{}{} // Acquire semaphore

				go func(subtask Subtask) {
					defer wg.Done()
					defer func() { <-semaphore }()
					defer func() {
						if rec := recover(); rec != nil {
							logging.Error("subtask goroutine panic", "id", subtask.ID, "panic", rec)
							resultsMu.Lock()
							subtaskResults[subtask.ID] = &SubtaskResult{
								ID:    subtask.ID,
								Error: fmt.Sprintf("internal panic: %v", rec),
							}
							completed[subtask.ID] = true
							resultsMu.Unlock()
						}
					}()

					result := r.executeSubtask(ctx, subtask)

					resultsMu.Lock()
					subtaskResults[subtask.ID] = result
					completed[subtask.ID] = true
					resultsMu.Unlock()
				}(st)
			}

			wg.Wait()
		} else {
			// Sequential execution
			for _, st := range ready {
				result := r.executeSubtask(ctx, st)
				subtaskResults[st.ID] = result
				completed[st.ID] = true
			}
		}
	}

	// Write results in order
	successCount := 0
	failedCount := 0

	for _, st := range decomposition.Subtasks {
		result, ok := subtaskResults[st.ID]
		if !ok {
			continue
		}

		fmt.Fprintf(&allOutputs, "### Subtask: %s (%s)\n", st.ID, st.AgentType)
		fmt.Fprintf(&allOutputs, "Prompt: %s\n\n", st.Prompt)

		if result.Success {
			successCount++
			output := result.Output
			if runes := []rune(output); len(runes) > 1000 {
				output = string(runes[:1000]) + "...[truncated]"
			}
			allOutputs.WriteString("**Status:** Completed\n\n")
			fmt.Fprintf(&allOutputs, "**Result:**\n%s\n\n", output)
		} else {
			failedCount++
			fmt.Fprintf(&allOutputs, "**Status:** Stopped before finishing — %s\n\n", result.Error)
			// Surface whatever partial work the subtask produced so it isn't lost
			// and the user (or a follow-up turn) can continue from it.
			if strings.TrimSpace(result.Output) != "" {
				output := result.Output
				if runes := []rune(output); len(runes) > 1000 {
					output = string(runes[:1000]) + "...[truncated]"
				}
				fmt.Fprintf(&allOutputs, "**Partial work so far (continue from here):**\n%s\n\n", output)
			}
		}
	}

	// Summary
	allOutputs.WriteString("---\n")
	fmt.Fprintf(&allOutputs, "**Total:** %d succeeded, %d failed out of %d subtasks\n",
		successCount, failedCount, len(decomposition.Subtasks))

	response := allOutputs.String()
	history := []*genai.Content{
		genai.NewContentFromText(response, genai.RoleModel),
	}

	return history, response, nil
}

// executeSubtask executes a single subtask and returns the result.
func (r *Router) executeSubtask(ctx context.Context, st Subtask) *SubtaskResult {
	result := &SubtaskResult{
		ID: st.ID,
	}

	agentID, err := r.agentRunner.Spawn(ctx, st.AgentType, st.Prompt, 20, "")
	// Spawn stores the partial result before returning a non-nil err (the real
	// failure shape), so when the agent actually started (agentID != "") fetch
	// it and preserve its output — only a never-started spawn is a bare failure.
	if agentID == "" {
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Error = "subtask agent did not start"
		}
		result.Success = false
		return result
	}

	result.AgentID = agentID

	agentResult, ok := r.agentRunner.GetResult(agentID)
	if !ok {
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Error = "no result returned"
		}
		result.Success = false
		return result
	}

	failureReason := agentResult.Error
	if failureReason == "" && err != nil {
		failureReason = err.Error()
	}

	if failureReason != "" {
		result.Error = failureReason
		// Preserve partial work. A failed subtask often still produced real
		// output (built code, analysis, a half-finished edit) before it stopped;
		// the agent carefully keeps it in agentResult.Output. Dropping it here
		// silently loses minutes of work and leaves the user with a bare error
		// instead of something to continue from.
		result.Output = agentResult.Output
		result.Success = false
		return result
	}

	result.Output = agentResult.Output
	result.Success = true
	return result
}

// GetAnalysis returns the task analysis without executing
func (r *Router) GetAnalysis(message string) *TaskComplexity {
	return r.analyzer.Analyze(message)
}

// RecordRoutingOutcome records whether a routing decision was successful.
func (r *Router) RecordRoutingOutcome(message string, analysis *TaskComplexity, success bool) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()

	r.routingHistory = append(r.routingHistory, routingRecord{
		message:   message,
		taskType:  analysis.Type,
		strategy:  analysis.Strategy,
		success:   success,
		timestamp: time.Now(),
	})

	// Keep last 100 records
	if len(r.routingHistory) > 100 {
		r.routingHistory = r.routingHistory[len(r.routingHistory)-100:]
	}
}

// getStrategySuccessRate returns the success rate for a given strategy from history.
func (r *Router) getStrategySuccessRate(strategy ExecutionStrategy) float64 {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()

	total := 0
	successes := 0
	for _, rec := range r.routingHistory {
		if rec.strategy == strategy {
			total++
			if rec.success {
				successes++
			}
		}
	}

	if total < 3 {
		return 0.5 // Not enough data
	}
	return float64(successes) / float64(total)
}

// adjustStrategyFromHistory adjusts the routing strategy based on historical success rates.
func (r *Router) adjustStrategyFromHistory(analysis *TaskComplexity) {
	currentRate := r.getStrategySuccessRate(analysis.Strategy)

	// If current strategy has poor success rate (<30%), try alternatives
	if currentRate < 0.3 && currentRate > 0 {
		// Try upgrading to a more capable strategy
		switch analysis.Strategy {
		case StrategyDirect:
			altRate := r.getStrategySuccessRate(StrategyExecutor)
			if altRate > currentRate {
				logging.Debug("learned routing override",
					"from", analysis.Strategy,
					"to", StrategyExecutor,
					"current_rate", currentRate,
					"alt_rate", altRate)
				analysis.Strategy = StrategyExecutor
			}
		case StrategyExecutor:
			altRate := r.getStrategySuccessRate(StrategySubAgent)
			if altRate > currentRate {
				logging.Debug("learned routing override",
					"from", analysis.Strategy,
					"to", StrategySubAgent,
					"current_rate", currentRate,
					"alt_rate", altRate)
				analysis.Strategy = StrategySubAgent
			}
		}
	}
}

// RoutingDecision represents the routing decision for a task
type RoutingDecision struct {
	Analysis          *TaskComplexity
	Message           string
	Handler           HandlerType
	SubAgentType      string
	Background        bool
	ShouldRoute       bool
	Reasoning         string
	Decomposition     *DecompositionResult // For HandlerCoordinated
	LearnedExamples   []LearnedExample     // Similar past tasks (Phase 2)
	SuggestedModel    string               // Cost-aware model suggestion (empty = use default)
	ThinkingBudget    int32                // 0 = disabled, >0 = max thinking tokens
	SuggestedToolSets []tools.ToolSet      // Tool sets for this request
}

// LearnedExample contains information about a learned example for few-shot learning.
type LearnedExample struct {
	ID        string
	TaskType  string
	Prompt    string
	AgentType string
	Score     float64
}

// HandlerType represents the execution handler
type HandlerType string

const (
	HandlerDirect      HandlerType = "direct"
	HandlerExecutor    HandlerType = "executor"
	HandlerSubAgent    HandlerType = "sub_agent"
	HandlerCoordinated HandlerType = "coordinated"
)

// String returns the string representation
func (h HandlerType) String() string {
	return string(h)
}

// selectThinkingBudget returns the thinking token budget based on task complexity
// and model capability. Weaker models get proportionally more thinking budget.
//
// The base budget scales by strategy (Direct=0 → SubAgent=4096). Two adaptive
// layers stack on top:
//
//  1. Session-depth scaling: once the session has accumulated state (many
//     turns, many tool calls, or a compaction has fired), the task in front
//     of the model is usually more involved than its prompt suggests. Scale
//     by min(1 + turns/10 + tools/20, 2.0).
//  2. Last-request pressure: if the previous request hit a stream-idle
//     timeout or returned an empty response, the budget was likely too
//     small — bump by 1.5x for the next one (decays on success).
//
// Hard-capped at 16384 to stop runaway thinking on pathological inputs.
func (r *Router) selectThinkingBudget(analysis *TaskComplexity) int32 {
	// User intent overrides the adaptive default at the extremes: off → never
	// reason; on → reason every turn (floored below). auto → the adaptive logic.
	mode := r.getThinkingMode()
	if mode == config.ThinkingModeOff {
		return 0
	}

	var budget int32
	switch analysis.Strategy {
	case StrategyDirect:
		// The cheap path: a short question answered on the fast model. Tools
		// are present (core + memory), so this zero is about cost, not about
		// capability — the earlier "no tools" reading of it is what let a terse
		// "fix the failing test" land here with reasoning off.
		budget = 0
	case StrategySingleTool:
		if analysis.Score <= 2 {
			budget = 0
		} else {
			budget = 2048
		}
	case StrategyExecutor:
		// The common code-touching path. Reasoning models benefit most here, so
		// floor it at the factory default (8192) rather than the old 1-2K, which
		// throttled coding turns well below where the model performs best.
		if analysis.Score >= 5 {
			budget = 8192
		} else {
			budget = 4096
		}
	case StrategySubAgent:
		budget = 8192
	}

	if capability := r.getModelCapability(); budget > 0 && capability != nil {
		budget = int32(float64(budget) * capability.ThinkingMultiplier)
	}
	if budget > 0 {
		budget = int32(float64(budget) * r.sessionDepthMultiplier())
	}
	if budget > 0 && r.pressureBoost() {
		budget = int32(float64(budget) * 1.5)
	}
	// Force mode: reason every turn, even on a Direct/simple task the adaptive
	// path would skip (budget 0). Floor it at a meaningful reasoning budget.
	if mode == config.ThinkingModeOn {
		const onFloor int32 = 4096
		if budget < onFloor {
			budget = onFloor
		}
	}
	const maxBudget int32 = 16384
	if budget > maxBudget {
		budget = maxBudget
	}
	return budget
}

// sessionDepthMultiplier returns a factor in [1.0, 2.0] that grows with
// accumulated session activity. Linear in (turns/10 + tools/20), clamped.
// Zero when no session stats are wired — caller gets neutral 1.0.
func (r *Router) sessionDepthMultiplier() float64 {
	turns, tools := r.sessionDepthStats()
	if turns == 0 && tools == 0 {
		return 1.0
	}
	mult := 1.0 + float64(turns)/10.0 + float64(tools)/20.0
	if mult > 2.0 {
		mult = 2.0
	}
	return mult
}

// sessionDepthStats returns current (turn count, tool call count) from the
// router-scoped counters. Hooks are called by Execute() on each turn; in
// tests they stay zero and the multiplier collapses to 1.0.
func (r *Router) sessionDepthStats() (turns, tools int) {
	r.depthMu.RLock()
	defer r.depthMu.RUnlock()
	return r.depthTurns, r.depthTools
}

// pressureBoost reports whether the previous request showed signs of
// budget starvation (stream-idle or empty-response). One-shot flag —
// cleared on next successful response.
func (r *Router) pressureBoost() bool {
	r.depthMu.RLock()
	defer r.depthMu.RUnlock()
	return r.depthPressure
}

// RecordTurn is invoked by the executor at the end of each model turn to
// feed the adaptive budget logic with session activity.
func (r *Router) RecordTurn(toolCalls int, pressure bool) {
	r.depthMu.Lock()
	defer r.depthMu.Unlock()
	r.depthTurns++
	r.depthTools += toolCalls
	r.depthPressure = pressure
}

// ResetDepth clears session activity counters — called on /clear or new
// session load so old state doesn't leak across conversations.
func (r *Router) ResetDepth() {
	r.depthMu.Lock()
	defer r.depthMu.Unlock()
	r.depthTurns = 0
	r.depthTools = 0
	r.depthPressure = false
}

// selectCostAwareModel returns the fast model for simple tasks, empty for complex ones.
func (r *Router) selectCostAwareModel(analysis *TaskComplexity) string {
	// Use fast model for direct responses and simple single-tool calls
	switch analysis.Strategy {
	case StrategyDirect:
		return r.fastModel
	case StrategySingleTool:
		// Single tool calls with low complexity can use fast model
		if analysis.Score <= 2 {
			return r.fastModel
		}
	}
	// Complex tasks use the default (primary) model
	return ""
}

// applyCapabilityAdjustments modifies task analysis based on model capability.
// Weaker models get lower decompose thresholds (complex tasks split earlier).
func (r *Router) applyCapabilityAdjustments(analysis *TaskComplexity) {
	capability := r.getModelCapability()
	if capability == nil || capability.DecomposeAdjust == 0 {
		return
	}

	effectiveThreshold := max(r.decomposeThreshold+capability.DecomposeAdjust, 2)

	// If task exceeds adjusted threshold but not original, force sub-agent strategy
	if analysis.Score >= effectiveThreshold && analysis.Score < r.decomposeThreshold {
		analysis.Strategy = StrategySubAgent
		analysis.Reasoning += fmt.Sprintf(" (model-adjusted: %s tier lowers decompose threshold to %d)",
			capability.Tier, effectiveThreshold)
	}
}

// selectToolSets determines which tool sets to include based on task analysis.
// Uses both Strategy and TaskType to minimize tool declarations per request,
// reducing token overhead (~60 tokens per tool declaration).
func (r *Router) selectToolSets(analysis *TaskComplexity) []tools.ToolSet {
	// Base: core + memory are always included. Memory (remember/recall/
	// forget/list) is a fundamental capability — gating it per-strategy
	// silently broke "запомни это" / "recall X" requests because Direct/
	// SingleTool routes stripped the tool list, then the model honestly
	// reported "I can't access persistent memory". 4 tool declarations
	// ≈ 240 tokens per request (cached on repeat); worth it for
	// deterministic agent continuity.
	sets := []tools.ToolSet{tools.ToolSetCore, tools.ToolSetMemory}

	switch analysis.Strategy {
	case StrategyDirect:
		// Pure Q&A — only core, no git/fileops/web needed.
		return r.filterToolSetsByCapability(sets)

	case StrategySingleTool:
		// Single tool call: core covers read/write/edit/bash/glob/grep.
		if r.isGitRepo {
			sets = append(sets, tools.ToolSetGit)
		}
		return r.filterToolSetsByCapability(sets)

	case StrategyExecutor:
		// Task-type-aware filtering within executor strategy.
		switch analysis.Type {
		case TaskTypeQuestion:
			return r.filterToolSetsByCapability(sets)

		case TaskTypeExploration:
			if r.isGitRepo {
				sets = append(sets, tools.ToolSetGit)
			}
			return r.filterToolSetsByCapability(sets)

		case TaskTypeRefactoring:
			if r.isGitRepo {
				sets = append(sets, tools.ToolSetGit)
			}
			sets = append(sets, tools.ToolSetFileOps, tools.ToolSetAdvanced)
			return r.filterToolSetsByCapability(sets)

		case TaskTypeComplex:
			if r.isGitRepo {
				sets = append(sets, tools.ToolSetGit)
			}
			sets = append(sets, tools.ToolSetFileOps, tools.ToolSetWeb, tools.ToolSetAdvanced)
			return r.filterToolSetsByCapability(sets)

		default:
			if r.isGitRepo {
				sets = append(sets, tools.ToolSetGit)
			}
			sets = append(sets, tools.ToolSetFileOps)
			return r.filterToolSetsByCapability(sets)
		}

	case StrategySubAgent:
		if r.isGitRepo {
			sets = append(sets, tools.ToolSetGit)
		}
		sets = append(sets, tools.ToolSetFileOps, tools.ToolSetWeb,
			tools.ToolSetAdvanced, tools.ToolSetPlanning,
			tools.ToolSetAgent, tools.ToolSetMemory)
		return r.filterToolSetsByCapability(sets)
	}

	// Fallback: core + git + fileops
	if r.isGitRepo {
		sets = append(sets, tools.ToolSetGit)
	}
	sets = append(sets, tools.ToolSetFileOps)
	return r.filterToolSetsByCapability(sets)
}

// selectToolSetsForMessage layers request-level hybrid policy on top of the
// task router's ordinary capability selection. Hybrid is appended after model
// tier filtering: explicit mode is a user choice, and auto's high-precision
// gate already prevents weak/simple requests from paying for the declaration.
func (r *Router) selectToolSetsForMessage(analysis *TaskComplexity, message string) []tools.ToolSet {
	mode := r.getEngineMode()
	decision := hybrid.Decide(mode, message)
	return r.selectToolSetsForDecision(analysis, mode, decision)
}

func (r *Router) selectToolSetsForDecision(
	analysis *TaskComplexity,
	mode string,
	decision hybrid.Decision,
) []tools.ToolSet {
	sets := r.selectToolSets(analysis)
	if !decision.Enabled {
		return sets
	}
	sets = append(sets, tools.ToolSetHybrid)
	if strings.EqualFold(strings.TrimSpace(mode), "hybrid") {
		sets = append(sets, tools.ToolSetHarness)
	}
	return sets
}

// filterToolSetsByCapability drops tool sets that historically confused
// weaker models. The policy has been narrowed over time as specific
// "stripped but actually useful" bugs surfaced — each exclusion needs
// to justify itself against the alternative of a silent capability
// failure ("I can't access X") the user sees.
//
// Current allowlist:
//   - Weak: Core, Git, FileOps, Memory, Web, Planning
//   - Medium: also Advanced (Kimi/GLM-4/MiniMax are capable enough)
//   - Strong: no filtering
//
// Dropped for non-Strong tiers:
//   - ToolSetAgent (ask_agent/coordinate/shared_memory/update_scratchpad/
//     request_tool) — genuinely advanced orchestration primitives that
//     do confuse lower tiers. Sub-agents get them via a separate path.
//   - ToolSetSemantic — removed in v0.65.0 entirely; still safe to omit.
func (r *Router) filterToolSetsByCapability(sets []tools.ToolSet) []tools.ToolSet {
	capability := r.getModelCapability()
	if capability == nil || capability.Tier >= CapabilityStrong {
		return sets
	}

	weakAllow := map[tools.ToolSet]bool{
		tools.ToolSetCore:     true,
		tools.ToolSetGit:      true,
		tools.ToolSetFileOps:  true,
		tools.ToolSetMemory:   true,
		tools.ToolSetWeb:      true,
		tools.ToolSetPlanning: true,
	}
	mediumAllow := map[tools.ToolSet]bool{
		tools.ToolSetAdvanced: true,
	}

	var filtered []tools.ToolSet
	for _, s := range sets {
		if weakAllow[s] {
			filtered = append(filtered, s)
			continue
		}
		if capability.Tier == CapabilityMedium && mediumAllow[s] {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// toolHint returns an optional prompt hint based on task type.
func (r *Router) toolHint(analysis *TaskComplexity) string {
	switch analysis.Type {
	case TaskTypeExploration:
		return "For this task, prefer read, glob, grep, and tree for exploring code. Avoid write/edit unless explicitly asked."
	case TaskTypeRefactoring:
		return "For this task, prefer edit over write to make targeted changes. Use diff to verify."
	case TaskTypeQuestion:
		return "" // No hint for simple questions
	default:
		return ""
	}
}

func (r *Router) toolHintForRequest(analysis *TaskComplexity, message string, replExposed bool) string {
	return r.toolHintForDecision(analysis, hybrid.Decide("auto", message), replExposed)
}

func (r *Router) toolHintForDecision(
	analysis *TaskComplexity,
	autoDecision hybrid.Decision,
	replExposed bool,
) string {
	if hint := hybrid.AnalysisHintForDecision(autoDecision, replExposed); hint != "" {
		return hint
	}
	return r.toolHint(analysis)
}

func schemaContainsDeclaration(schema []*genai.Tool, name string) bool {
	for _, envelope := range schema {
		if envelope == nil {
			continue
		}
		for _, declaration := range envelope.FunctionDeclarations {
			if declaration != nil && declaration.Name == name {
				return true
			}
		}
	}
	return false
}

// TrackOperation records an operation outcome for context awareness.
func (r *Router) TrackOperation(toolName string, success bool) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()

	r.recentOps++
	if !success {
		r.recentErrors++
	}

	// Reset counters every 20 operations
	if r.recentOps >= 20 {
		r.recentOps = 0
		r.recentErrors = 0
	}

	// Update conversation mode based on tool usage patterns
	r.updateConversationMode(toolName)
}

// updateConversationMode infers the conversation mode from recent tool usage.
func (r *Router) updateConversationMode(toolName string) {
	switch {
	case toolName == "grep" || toolName == "glob" || toolName == "read" || toolName == "tree":
		r.conversationMode = "exploring"
	case toolName == "write" || toolName == "edit":
		r.conversationMode = "implementing"
	case toolName == "bash" && r.recentErrors > 2:
		r.conversationMode = "debugging"
	}
}

// GetConversationMode returns the current inferred conversation mode.
func (r *Router) GetConversationMode() string {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	return r.conversationMode
}

// GetErrorRate returns the recent error rate.
func (r *Router) GetErrorRate() float64 {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()

	if r.recentOps == 0 {
		return 0
	}
	return float64(r.recentErrors) / float64(r.recentOps)
}
