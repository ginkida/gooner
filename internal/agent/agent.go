package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gokin/internal/client"
	"gokin/internal/config"
	ctxmgr "gokin/internal/context"
	"gokin/internal/donegate"
	"gokin/internal/hooks"
	"gokin/internal/logging"
	"gokin/internal/memory"
	"gokin/internal/permission"
	"gokin/internal/pinned"
	"gokin/internal/skills"
	"gokin/internal/tools"

	"google.golang.org/genai"
)

const (
	// DefaultMaxHistorySize is the default maximum number of messages in history before forced compaction.
	DefaultMaxHistorySize = 200

	// MaxTurnLimit is the absolute maximum number of turns an agent can take.
	// This prevents infinite loops even if mental loop detection fails.
	MaxTurnLimit = 100

	// Long-loop stability guardrails.
	stagnationTurnThreshold   = 3
	repeatedPlanTurnThreshold = 3
)

// Agent represents an isolated executor for subtasks.
type Agent struct {
	ID           string
	Type         AgentType
	Model        string
	client       client.Client
	registry     *tools.Registry
	baseRegistry tools.ToolRegistry
	workDir      string
	// outputBaseDir remains anchored to the durable runner workspace even when
	// workDir is later replaced by a temporary isolated worktree.
	outputBaseDir  string
	originalPrompt string // Preserved for continuation after compaction
	messenger      tools.Messenger
	permissions    *permission.Manager
	hooks          *hooks.Manager
	timeout        time.Duration
	// modelRoundTimeout is the configurable hard cap for one provider round.
	// Guarded by stateMu so ApplyConfig can update live agents safely between
	// rounds without racing their execution goroutines.
	modelRoundTimeout time.Duration
	history           []*genai.Content
	status            AgentStatus
	startTime         time.Time
	endTime           time.Time
	maxTurns          int
	thoroughness      tools.Thoroughness
	outputStyle       tools.OutputStyle
	// invocationScope is rebound under stateMu at the start of every Run. A
	// persisted agent ID can be resumed by a different top-level invocation, so
	// attribution belongs to the run lease rather than the Agent's lifetime.
	invocationScope InvocationScope

	// === IMPROVEMENT 4: Progress tracking ===
	currentStep      int
	totalSteps       int
	stepDescription  string
	progressMu       sync.Mutex
	progressCallback func(progress *AgentProgress)

	// Mental loop detection tracking
	callHistory     map[string]int  // Map of tool_name:arguments -> count
	callHistoryMu   sync.Mutex      // Protects callHistory and loopEarlyWarned
	loopIntervened  bool            // Flag to indicate if loop intervention occurred
	loopCooldown    int             // Consecutive non-loop turns since last intervention
	loopThreshold   int             // Broad loop threshold (default: 8, quick: 4, thorough: 15)
	loopEarlyWarned map[string]bool // Tracks keys that already got an early-warning message

	// Distinct-target coverage tracking (read/grep). Closes the arg-perturbing
	// escape that exact-args/plan/no-progress all miss: a loop that drifts the
	// read offset or rotates grep patterns over the SAME target re-covers ground
	// it already has, while producing a distinct normalizeCallKey each call.
	// Counts PER-TARGET, PROGRESS-GATED redundant re-coverage: a revisit is
	// redundant only when NOTHING else happened since the previous visit to that
	// target (no new ground covered, no other tool called). Legitimate workflows
	// — read→edit→verify, zoom-in re-reads, exploration greps interleaved with
	// reading the results — all advance the coverage generation and never
	// accumulate. All guarded by callHistoryMu.
	coverage      *tools.CoverageState // shared coverage core (tools/coverage.go); caller holds callHistoryMu
	coverageTrips int                  // re-coverage interventions fired this request (attempt counter)

	// Context summarization settings (adjusted by thoroughness)
	pruneProtectChars  int // Chars protected from pruning (default: 120000)
	summarizeProtect   int // Recent messages protected during summarization (default: 4)
	summarizeMinMsgs   int // Minimum messages before summarization kicks in (default: 6)
	pruneMinOutputSize int // Minimum tool output size to consider for pruning (default: 200)
	maxHistorySize     int // Max messages before forced compaction (default: 200)
	// Per-call bound for the summarize/token-count API calls made during
	// pre-emptive compaction. Zero ⇒ agentCompactionAPITimeout. Overridable in tests.
	compactionAPITimeout time.Duration

	// Project context injection for sub-agents
	projectContext string             // Injected project guidelines/instructions
	onText         func(text string)  // Streaming callback for real-time output
	onTextMu       sync.Mutex         // Protects onText from interleaving
	outputWriter   *AgentOutputWriter // Current invocation's live transcript
	onThinking     func(text string)  // Streaming callback for thinking/reasoning output
	onThinkingMu   sync.Mutex         // Protects onThinking from interleaving
	Thought        string             // Accumulated reasoning/thought for the current turn
	onRateLimit    func(rl *client.RateLimitMetadata)
	onInput        func(prompt string) (string, error)

	// Model capability adaptation
	weakModelMode bool // When true, include more guidance for weaker models

	// recentFilesProvider returns up to N file paths that the session has
	// already read. Injected post-compaction into the continuation hint so the
	// model doesn't waste a turn re-reading files whose content it still has
	// in compacted form. Nil-safe — no hint is added when unset.
	recentFilesProvider func(limit int) []string

	// modifiedFilesProvider returns up to N file paths written/edited in this
	// session. Post-compaction, surfaces these so the model knows what it has
	// already changed and doesn't redundantly overwrite or forget edits made
	// before the summary. Nil-safe.
	modifiedFilesProvider func(limit int) []string

	// Pre-emptive compaction state: EMA of tokens added per turn. Lets
	// checkAndSummarize compact when "current % + 3 × EMA" crosses threshold,
	// catching imminent overflow before we're mid-stream. lastTokenCount ≤ 0
	// means we don't have a previous observation yet.
	lastTokenCount    int
	tokenGrowthEMA    float64
	tokenGrowthSample int
	preemptMu         sync.Mutex

	// Cached precise token count to skip redundant API calls.
	cachedPreciseCount   int
	cachedPreciseHistLen int

	// Plan approval callback for context compaction
	onPlanApproved func(planSummary string) // Called when plan is built, allows context clearing

	// Scratchpad update callback
	onScratchpadUpdate func(content string)

	// Context management
	ctxCfg          *config.ContextConfig
	tokenCounter    *ctxmgr.TokenCounter
	summarizer      *ctxmgr.Summarizer
	compactor       *ctxmgr.ResultCompactor
	fileTracker     *ctxmgr.FileActivityTracker
	relevanceScorer *ctxmgr.RelevanceScorer

	// invokedSkills is this agent's session-local, immutable snapshot ledger.
	// It is deliberately not inherited from the foreground session or shared
	// with sibling agents: every cloned SkillTool is rebound to this stable
	// pointer during construction.
	invokedSkills *skills.InvocationLedger

	// Self-reflection for error recovery
	reflector         *Reflector
	recoveryExecutor  *RecoveryExecutor
	autoFixAttempts   map[string]int
	autoFixAttemptsMu sync.Mutex
	learning          *memory.ProjectLearning
	fixCache          *FixCache // Session-local error→fix cache

	// Autonomous delegation strategy
	delegation *DelegationStrategy

	// Tree planning (Phase 6)
	treePlanner     *TreePlanner
	activePlan      *PlanTree
	lastPlanTree    *PlanTree // preserved after activePlan is cleared
	planningMode    bool
	requireApproval bool
	planGoal        *PlanGoal

	// Phase 2: Shared memory for inter-agent communication
	sharedMemory *SharedMemory

	// Phase 2: Tools used tracking for progress
	toolsUsed            []string
	statefulToolAttempts int
	toolsMu              sync.Mutex

	// State protection for concurrent access to status, history, startTime, endTime
	stateMu sync.RWMutex

	// Per-run token usage, accumulated across model rounds in executeLoop and
	// surfaced on AgentResult so callers (notably the /loop scheduler) can show
	// what an unattended run consumed. Cached input remains part of total input
	// because providers still bill and quota it; the cached subset is tracked
	// separately for discounted cost calculation. Mutated only on the run goroutine
	// (under stateMu, alongside the history append) and read after executeLoop
	// returns on the same goroutine — guarded by stateMu for discipline.
	usageInputTokens     int
	usageOutputTokens    int
	usageCacheReadTokens int
	usageEstimatedCost   float64
	usageCostTracked     bool

	// pendingSteers holds steering messages (e.g. MetaAgent stuck-interventions)
	// queued from another goroutine, drained into history at the top of each loop
	// iteration so the model actually SEES the nudge instead of it being a UI-only
	// toast. Guarded by stateMu.
	pendingSteers []string

	// Explicit cancellation for background agents (set by Runner)
	cancelFunc      context.CancelFunc
	cancelRequested bool // latched when Cancel races registration of cancelFunc

	// runCtx is the LIVE run context for the current/most-recent Run() call —
	// the one cancelFunc actually kills. AgentMessenger reads this (via
	// Runner.messengerCtxFor) to derive its helper-agent contexts, instead of
	// a context captured once at Runner construction (app lifetime). Without
	// this, Esc/task_stop cancelling THIS agent's run left any ask_agent/
	// delegate helper it had spawned running for a further 3-5 minutes on a
	// context nothing could reach (v0.100.79 messenger cancellation fix).
	// Guarded by stateMu like the other run-lifecycle fields.
	runCtx context.Context

	// Agent Scratchpad (Phase 7)
	Scratchpad string

	// Pinned Context (Custom Improvement)
	PinnedContext string

	// Tool activity callback for UI updates. On the "end" event, success +
	// summary carry the tool's OUTCOME (✓/✗ and a short result line) so the UI
	// can show meaningful sub-agent output instead of a bare tool name. These
	// are zero-valued for non-"end" events.
	onToolActivity func(agentID, toolName string, args map[string]any, status string, success bool, summary string)

	// First authorization boundary refused in the current Run invocation. A
	// model can still finish its conversation after a refusal, so this cannot be
	// inferred from the terminal AgentStatus.
	runPolicyBlock *tools.PolicyBlock

	// Checkpoint support
	store              *AgentStore
	autoCheckpoint     bool // Enable auto-checkpoint every N turns
	checkpointInterval int  // Number of turns between auto-checkpoints
	lastCheckpointTurn int  // Last turn when checkpoint was saved

	// Workspace isolation state
	isolatedWorkspace        *isolatedWorkspace
	requestedToolsRestricted bool
	allowedRequestedTools    map[string]struct{}

	// Done-gate integration.
	doneGatePolicy *donegate.Policy
	touchedSeen    map[string]bool
	touchedPaths   []string
	// mutationGen counts every successful mutation (incl. re-edits of an
	// already-touched file), so the gate re-checks after a re-edit that the
	// touched-path SET alone would miss. Guarded by toolsMu.
	mutationGen int
}

// SetOnRateLimit sets the rate limit callback.
func (a *Agent) SetOnRateLimit(cb func(rl *client.RateLimitMetadata)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.onRateLimit = cb
}

// ContextHealth represents a snapshot of the agent's current context state.
type ContextHealth struct {
	TotalTokens       int
	MaxTokens         int
	PercentUsed       float64
	SystemTokens      int
	InstructionTokens int
	HistoryTokens     int
	ToolTokens        int
	ActiveFiles       []string
	LastPruningTime   time.Time
	PruningAlert      string
}

// contextHealthCountTimeout bounds the token count behind the health panel.
// The panel is a readout; it is never worth stalling the turn that produced
// the history it is describing. A var, not a const, so a test can shorten it
// and prove the bound actually exists.
var contextHealthCountTimeout = 5 * time.Second

// GetContextHealth returns a snapshot of the agent's context health.
func (a *Agent) GetContextHealth() ContextHealth {
	// Snapshot under the lock, count OUTSIDE it. CountContents reaches the
	// provider's count_tokens endpoint whenever the hash misses its cache,
	// which is the normal case here because history grows every turn — and
	// holding stateMu across a network call blocks the agent's own history
	// append, since Go parks new readers behind a waiting writer. One slow
	// count would stall the loop producing the very history being counted.
	a.stateMu.RLock()
	h := ContextHealth{MaxTokens: a.ctxCfg.MaxInputTokens}
	counter := a.tokenCounter
	tracker := a.fileTracker
	history := make([]*genai.Content, len(a.history))
	copy(history, a.history)
	a.stateMu.RUnlock()

	if counter != nil {
		// This used to run on context.Background(): no deadline and no
		// cancellation, on a path that fires from the live rate-limit response
		// callback. Nothing else bounded it — the shared HTTP client carries no
		// Client.Timeout on purpose, so SSE can stream.
		ctx, cancel := context.WithTimeout(context.Background(), contextHealthCountTimeout)
		usage, err := counter.CountContents(ctx, history)
		cancel()
		if err != nil {
			// Degrade to the local estimate rather than to zero: a zero here is
			// read downstream as "no data" and silently replaced by the MAIN
			// session's numbers, so a timed-out sub-agent would be described by
			// someone else's context.
			usage = ctxmgr.EstimateContentsTokens(history)
		}
		h.TotalTokens = usage
		if h.MaxTokens > 0 {
			h.PercentUsed = float64(usage) / float64(h.MaxTokens)
		}

		// Estimate breakdown (simplified)
		h.SystemTokens = 2000 // typical base
		if len(history) > 0 {
			h.HistoryTokens = usage - h.SystemTokens
		}
	}

	if tracker != nil {
		h.ActiveFiles = tracker.GetActiveFiles(10)
	}

	return h
}

// NewAgent creates a new agent with the specified type and filtered tools.
func NewAgent(agentType AgentType, c client.Client, baseRegistry tools.ToolRegistry, workDir string, maxTurns int, model string, permManager *permission.Manager, ctxCfg *config.ContextConfig) *Agent {
	id := generateAgentID()

	// Create filtered registry based on agent type
	allowedTools := agentType.AllowedTools()
	filteredRegistry := createFilteredRegistry(agentType, baseRegistry)

	if maxTurns <= 0 {
		maxTurns = 15 // default — keep low to prevent excessive exploration
	}

	// Always run the sub-agent on its OWN client clone. Two reasons: (1) a model
	// override needs a clone anyway; (2) even at the same model, an isolated
	// client lets the runner set this agent's type-derived thinking budget
	// without racing the foreground or sibling agents on a shared client.
	// WithModel(GetModel()) is a same-model clone. Nil-guarded — some tests and
	// the plan-handoff path construct an agent without a client.
	agentClient := c
	if c != nil {
		modelName := mapModelName(model)
		if modelName == "" {
			modelName = c.GetModel()
		}
		if modelName != "" {
			agentClient = c.WithModel(modelName)
		}
	}

	agent := &Agent{
		ID:                 id,
		Type:               agentType,
		Model:              model,
		client:             agentClient,
		registry:           filteredRegistry,
		baseRegistry:       baseRegistry,
		workDir:            workDir,
		outputBaseDir:      workDir,
		permissions:        permManager,
		timeout:            config.DefaultAgentTimeout,
		modelRoundTimeout:  client.DefaultModelRoundTimeout,
		history:            make([]*genai.Content, 0),
		status:             AgentStatusPending,
		maxTurns:           maxTurns,
		loopThreshold:      8,
		pruneProtectChars:  120000,
		summarizeProtect:   4,
		summarizeMinMsgs:   6,
		pruneMinOutputSize: 200,
		maxHistorySize:     DefaultMaxHistorySize,
		callHistory:        make(map[string]int),
		loopEarlyWarned:    make(map[string]bool),
		coverage:           tools.NewCoverageState(),
		ctxCfg:             ctxCfg,
		recoveryExecutor:   NewRecoveryExecutor(2),
		autoFixAttempts:    make(map[string]int),
		fixCache:           NewFixCache(),
		invokedSkills:      skills.NewInvocationLedger(),
		// A non-nil AllowedTools list is an explicit capability ceiling. The
		// model-facing request_tool may resolve a late-bound tool within that
		// ceiling, but cannot turn a restricted role into a general agent.
		requestedToolsRestricted: allowedTools != nil,
		allowedRequestedTools:    requestedToolSet(allowedTools),
	}
	agent.bindSkillToolLedger()

	// Apply per-agent-type context budgets before other wiring
	agent.applyAgentTypeDefaults()

	// Wire up RequestTool tool if it exists in the registry
	if rt, ok := agent.registry.Get("request_tool"); ok {
		if rtt, ok := rt.(*tools.RequestToolTool); ok {
			rtt.SetRequester(agent)
		}
	}
	if bt, ok := agent.registry.Get("bash"); ok {
		if bashTool, ok := bt.(*tools.BashTool); ok {
			bashTool.SetWorkspaceBoundary(workDir)
		}
	}

	// Wire up PinContext tool (Custom Improvement)
	if pt, ok := agent.registry.Get("pin_context"); ok {
		if ptt, ok := pt.(*tools.PinContextTool); ok {
			ptt.SetWorkDir(workDir)
			ptt.SetUpdater(agent.SetPinnedContext)
		}
	}

	// Wire up HistorySearch tool (Custom Improvement)
	if ht, ok := agent.registry.Get("history_search"); ok {
		if htt, ok := ht.(*tools.HistorySearchTool); ok {
			htt.SetHistoryGetter(func() []*genai.Content {
				agent.stateMu.RLock()
				snap := make([]*genai.Content, len(agent.history))
				copy(snap, agent.history)
				agent.stateMu.RUnlock()
				return snap
			})
		}
	}

	// Wire up SharedMemory tool with this agent ID.
	if smt, ok := agent.registry.Get("shared_memory"); ok {
		if smtt, ok := smt.(*tools.SharedMemoryTool); ok {
			smtt.SetAgentID(agent.ID)
		}
	}

	// Initialize context management tools if config provided
	if ctxCfg != nil {
		agent.tokenCounter = ctxmgr.NewTokenCounter(agent.client, agent.Model, ctxCfg)
		agent.summarizer = ctxmgr.NewSummarizer(agent.client)
		agent.compactor = ctxmgr.NewResultCompactor(ctxCfg.ToolResultMaxChars)
	}

	// Initialize relevance scoring for smarter compaction
	agent.fileTracker = ctxmgr.NewFileActivityTracker()
	agent.relevanceScorer = ctxmgr.NewRelevanceScorer()

	// Initialize project learning
	if pl, err := memory.GetSharedProjectLearning(workDir); err == nil {
		agent.learning = pl
		// Inject into memorize tool if it exists
		if mt, ok := agent.registry.Get("memorize"); ok {
			if mtt, ok := mt.(interface{ SetLearning(*memory.ProjectLearning) }); ok {
				mtt.SetLearning(pl)
			}
		}
		// Also wire the `memory` tool so `memory list` surfaces memorized facts.
		if mt, ok := agent.registry.Get("memory"); ok {
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
				agent.stateMu.Lock()
				agent.Scratchpad = content
				cb := agent.onScratchpadUpdate
				agent.stateMu.Unlock()
				if cb != nil {
					cb(content)
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

	// Always run on an isolated client clone (same rationale as NewAgent) so the
	// runner can set this agent's thinking budget without racing a shared client.
	// Nil-guarded for the no-client construction paths.
	agentClient := c
	if c != nil {
		modelName := mapModelName(model)
		if modelName == "" {
			modelName = c.GetModel()
		}
		if modelName != "" {
			agentClient = c.WithModel(modelName)
		}
	}

	agent := &Agent{
		ID:                 id,
		Type:               AgentType(dynType.Name), // Use dynamic type name
		Model:              model,
		client:             agentClient,
		registry:           filteredRegistry,
		baseRegistry:       baseRegistry,
		workDir:            workDir,
		outputBaseDir:      workDir,
		permissions:        permManager,
		timeout:            2 * time.Minute,
		modelRoundTimeout:  client.DefaultModelRoundTimeout,
		history:            make([]*genai.Content, 0),
		status:             AgentStatusPending,
		maxTurns:           maxTurns,
		loopThreshold:      8,
		pruneProtectChars:  120000,
		summarizeProtect:   4,
		summarizeMinMsgs:   6,
		pruneMinOutputSize: 200,
		maxHistorySize:     DefaultMaxHistorySize,
		callHistory:        make(map[string]int),
		loopEarlyWarned:    make(map[string]bool),
		coverage:           tools.NewCoverageState(),
		ctxCfg:             ctxCfg,
		recoveryExecutor:   NewRecoveryExecutor(2),
		autoFixAttempts:    make(map[string]int),
		fixCache:           NewFixCache(),
		invokedSkills:      skills.NewInvocationLedger(),
		// Dynamic types with a non-empty tool list have the same immutable-by-
		// default capability ceiling as built-in restricted roles. An empty list
		// retains the constructor's existing "all tools" semantics.
		requestedToolsRestricted: len(dynType.AllowedTools) > 0,
		allowedRequestedTools:    requestedToolSet(dynType.AllowedTools),
		// Store custom prompt for dynamic type
		projectContext: dynType.SystemPrompt,
	}
	agent.bindSkillToolLedger()

	// Apply per-agent-type context budgets before other wiring
	agent.applyAgentTypeDefaults()

	// Wire up RequestTool tool if it exists
	if rt, ok := agent.registry.Get("request_tool"); ok {
		if rtt, ok := rt.(*tools.RequestToolTool); ok {
			rtt.SetRequester(agent)
		}
	}
	if bt, ok := agent.registry.Get("bash"); ok {
		if bashTool, ok := bt.(*tools.BashTool); ok {
			bashTool.SetWorkspaceBoundary(workDir)
		}
	}

	// Wire up PinContext tool (Custom Improvement)
	if pt, ok := agent.registry.Get("pin_context"); ok {
		if ptt, ok := pt.(*tools.PinContextTool); ok {
			ptt.SetWorkDir(workDir)
			ptt.SetUpdater(agent.SetPinnedContext)
		}
	}

	// Wire up HistorySearch tool (Custom Improvement)
	if ht, ok := agent.registry.Get("history_search"); ok {
		if htt, ok := ht.(*tools.HistorySearchTool); ok {
			htt.SetHistoryGetter(func() []*genai.Content {
				agent.stateMu.RLock()
				snap := make([]*genai.Content, len(agent.history))
				copy(snap, agent.history)
				agent.stateMu.RUnlock()
				return snap
			})
		}
	}

	// Wire up SharedMemory tool with this agent ID.
	if smt, ok := agent.registry.Get("shared_memory"); ok {
		if smtt, ok := smt.(*tools.SharedMemoryTool); ok {
			smtt.SetAgentID(agent.ID)
		}
	}

	// Initialize context management
	if ctxCfg != nil {
		agent.tokenCounter = ctxmgr.NewTokenCounter(agent.client, agent.Model, ctxCfg)
		agent.summarizer = ctxmgr.NewSummarizer(agent.client)
		agent.compactor = ctxmgr.NewResultCompactor(ctxCfg.ToolResultMaxChars)
	}

	// Initialize relevance scoring for smarter compaction
	agent.fileTracker = ctxmgr.NewFileActivityTracker()
	agent.relevanceScorer = ctxmgr.NewRelevanceScorer()

	// Initialize project learning
	if pl, err := memory.GetSharedProjectLearning(workDir); err == nil {
		agent.learning = pl
		// Inject into memorize tool if it exists
		if mt, ok := agent.registry.Get("memorize"); ok {
			if mtt, ok := mt.(interface{ SetLearning(*memory.ProjectLearning) }); ok {
				mtt.SetLearning(pl)
			}
		}
		// Also wire the `memory` tool so `memory list` surfaces memorized facts.
		if mt, ok := agent.registry.Get("memory"); ok {
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
				agent.stateMu.Lock()
				agent.Scratchpad = content
				cb := agent.onScratchpadUpdate
				agent.stateMu.Unlock()
				if cb != nil {
					cb(content)
				}
			})
		}
	}

	agent.delegation = NewDelegationStrategy(AgentType(dynType.Name), nil)

	return agent
}

// createFilteredRegistryFromList creates a registry with only the specified tools.
// foregroundOnlyTools never reach a sub-agent, whatever its type allows.
//
// repl_exec drives ONE Python kernel bound to the foreground workspace, and
// Manager.Execute serializes every cell behind a single mutex — so handing it
// to sub-agents means shared globals, one agent's action=reset wiping another's
// mid-analysis state, and a long cell blocking everyone else's REPL work.
// AgentTypeGeneral allows every tool, so without this the sharing was live
// rather than theoretical. Excluding it here also keeps its schema out of
// sub-agent prompts instead of advertising a tool that would only fail.
var foregroundOnlyTools = map[string]bool{
	"repl_exec": true,
}

func createFilteredRegistryFromList(allowedTools []string, baseRegistry tools.ToolRegistry) *tools.Registry {
	filtered := tools.NewRegistry()

	if len(allowedTools) == 0 {
		// All tools allowed - copy all from base registry
		for _, tool := range baseRegistry.List() {
			if foregroundOnlyTools[tool.Name()] {
				continue
			}
			_ = filtered.Register(cloneToolForAgent(tool))
		}
		return bindTaskToolCapabilityCeiling(filtered)
	}

	allowedMap := make(map[string]bool)
	for _, name := range allowedTools {
		allowedMap[name] = true
	}

	for _, tool := range baseRegistry.List() {
		if allowedMap[tool.Name()] && !foregroundOnlyTools[tool.Name()] {
			_ = filtered.Register(cloneToolForAgent(tool))
		}
	}

	return bindTaskToolCapabilityCeiling(filtered)
}

// bindTaskToolCapabilityCeiling turns the agent's final, already-intersected
// registry into the immutable parent authority passed to nested task calls.
// This must happen after filtering: the foreground runner's base registry is
// broader and must never be recoverable through delegation.
func bindTaskToolCapabilityCeiling(registry *tools.Registry) *tools.Registry {
	if registry == nil {
		return registry
	}
	if tool, ok := registry.Get("task"); ok {
		if taskTool, ok := tool.(*tools.TaskTool); ok {
			taskTool.SetToolCapabilityCeiling(registry.Names())
		}
	}
	return registry
}

// cloneToolForAgent returns an agent-local tool instance for tools that carry
// per-agent callbacks/state. Stateless/shared tools are returned as-is.
func cloneToolForAgent(tool tools.Tool) tools.Tool {
	return cloneToolForAgentWithWorkDir(tool, "")
}

func cloneToolForAgentWithWorkDir(tool tools.Tool, workDir string) tools.Tool {
	return tools.CloneToolForWorkDir(tool, workDir)
}

// bindSkillToolLedger connects the agent-local SkillTool clone to the stable
// ledger owned by this Agent. Constructors call it before the agent can run.
func (a *Agent) bindSkillToolLedger() {
	if a == nil || a.registry == nil {
		return
	}
	if a.invokedSkills == nil {
		a.invokedSkills = skills.NewInvocationLedger()
	}
	tool, ok := a.registry.Get("skill")
	if !ok {
		return
	}
	if skillTool, ok := tool.(*tools.SkillTool); ok {
		skillTool.SetInvocationLedger(a.invokedSkills)
	}
}

// invokedSkillSnapshot returns a defensive newest-first snapshot without
// holding stateMu while taking the ledger's own lock. The ledger pointer is
// stable for every constructed Agent; the nil case supports focused tests that
// use an Agent literal.
func (a *Agent) invokedSkillSnapshot() []skills.Invocation {
	a.stateMu.RLock()
	ledger := a.invokedSkills
	a.stateMu.RUnlock()
	if ledger == nil {
		return nil
	}
	return ledger.SnapshotNewestFirst()
}

// applyAgentTypeDefaults sets context budget defaults per agent type.
// Explore/bash agents are short-lived and don't need large history windows,
// while plan agents need more history to track multi-step plans.
// Called once during construction; ApplyThoroughness may further adjust these.
func (a *Agent) applyAgentTypeDefaults() {
	switch a.Type {
	case AgentTypeExplore:
		a.maxHistorySize = 50
		a.pruneProtectChars = 40000
		a.summarizeProtect = 2
		a.pruneMinOutputSize = 300
	case AgentTypeBash:
		a.maxHistorySize = 30
		a.pruneProtectChars = 30000
		a.summarizeProtect = 2
		a.pruneMinOutputSize = 400
		// Verification agents often spend most of their wall time compiling.
		// The normal shared budget leaves room for both a long provider round
		// and tool-result processing; thorough mode raises it further below.
		a.timeout = config.DefaultAgentTimeout
	case AgentTypePlan:
		a.maxHistorySize = 100
		a.pruneProtectChars = 150000
		a.summarizeProtect = 6
	case AgentTypeGuide:
		a.maxHistorySize = 40
		a.pruneProtectChars = 40000
		a.summarizeProtect = 2
		// default (AgentTypeGeneral and dynamic types): keep constructor defaults
	}
}

// SetThoroughness sets the exploration thoroughness level.
func (a *Agent) SetThoroughness(t tools.Thoroughness) {
	a.thoroughness = t
}

// ApplyThoroughness sets thoroughness, adjusts maxTurns (if still at default),
// sets per-agent timeout, loop detection threshold, and tool result compaction
// based on type and thoroughness level.
func (a *Agent) ApplyThoroughness(t tools.Thoroughness, defaultMaxTurns int) {
	a.thoroughness = t
	canOverrideMaxTurns := a.maxTurns == defaultMaxTurns

	switch a.Type {
	case AgentTypeExplore:
		switch t {
		case tools.ThoroughnessQuick:
			if canOverrideMaxTurns {
				a.maxTurns = 8
			}
			a.timeout = 1 * time.Minute
		case tools.ThoroughnessThorough:
			if canOverrideMaxTurns {
				a.maxTurns = 50
			}
			a.timeout = config.DefaultThoroughAgentTimeout
		}
	case AgentTypeBash:
		switch t {
		case tools.ThoroughnessQuick:
			if canOverrideMaxTurns {
				a.maxTurns = 5
			}
			a.timeout = 3 * time.Minute
		case tools.ThoroughnessThorough:
			if canOverrideMaxTurns {
				a.maxTurns = 20
			}
			a.timeout = config.DefaultThoroughAgentTimeout
		}
	case AgentTypeGeneral:
		switch t {
		case tools.ThoroughnessQuick:
			a.timeout = 2 * time.Minute
		case tools.ThoroughnessThorough:
			a.timeout = config.DefaultThoroughAgentTimeout
		}
	case AgentTypePlan:
		switch t {
		case tools.ThoroughnessQuick:
			a.timeout = 2 * time.Minute
		case tools.ThoroughnessThorough:
			a.timeout = config.DefaultThoroughAgentTimeout
		}
	}
	// Historical per-type thorough budgets (Explore 5m, General/Plan 10m)
	// became shorter than the 20m normal budget after model-round hardening.
	// Thorough is an explicit request for MORE depth, so give every type —
	// including guide/dynamic types outside the switch — the shared deep-work
	// floor. EffectiveRunTimeout below also follows a user-raised round cap.
	if t == tools.ThoroughnessThorough && a.timeout < config.DefaultThoroughAgentTimeout {
		a.timeout = config.DefaultThoroughAgentTimeout
	}

	// Adjust loop detection threshold per thoroughness
	switch t {
	case tools.ThoroughnessQuick:
		a.loopThreshold = 4
	case tools.ThoroughnessThorough:
		a.loopThreshold = 15
	default:
		a.loopThreshold = 8
	}

	// Adjust tool result compaction per thoroughness
	if a.compactor != nil {
		switch t {
		case tools.ThoroughnessQuick:
			a.compactor.SetMaxChars(10000)
		case tools.ThoroughnessThorough:
			a.compactor.SetMaxChars(50000)
		}
	}

	// Adjust tree planner settings per thoroughness
	if a.treePlanner != nil {
		a.treePlanner.ApplyThoroughness(t)
	}

	// Adjust delegation settings per thoroughness
	if a.delegation != nil {
		a.delegation.ApplyThoroughness(t)
	}

	// Adjust context summarization settings per thoroughness
	a.applyContextThoroughness(t)
}

// applyContextThoroughness adjusts context summarization, history limits,
// recovery attempts, checkpoint interval, and compactor head/tail lines.
func (a *Agent) applyContextThoroughness(t tools.Thoroughness) {
	switch t {
	case tools.ThoroughnessQuick:
		a.pruneProtectChars = 60000
		a.summarizeProtect = 2
		a.summarizeMinMsgs = 4
		a.pruneMinOutputSize = 300
		a.maxHistorySize = 100
		a.recoveryExecutor = NewRecoveryExecutor(1)
		a.checkpointInterval = 10
		if a.compactor != nil {
			a.compactor.SetHeadTailLines(5, 2)
		}
	case tools.ThoroughnessThorough:
		a.pruneProtectChars = 200000
		a.summarizeProtect = 6
		a.summarizeMinMsgs = 8
		a.pruneMinOutputSize = 100
		a.maxHistorySize = 300
		a.recoveryExecutor = NewRecoveryExecutor(3)
		a.checkpointInterval = 3
		if a.compactor != nil {
			a.compactor.SetHeadTailLines(15, 10)
		}
	default:
		a.pruneProtectChars = 120000
		a.summarizeProtect = 4
		a.summarizeMinMsgs = 6
		a.pruneMinOutputSize = 200
		a.maxHistorySize = DefaultMaxHistorySize
		a.recoveryExecutor = NewRecoveryExecutor(2)
		a.checkpointInterval = 5
	}
}

// GetTimeout returns the agent's timeout duration.
func (a *Agent) GetTimeout() time.Duration {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.timeout
}

// EffectiveRunTimeout returns the outer wall-clock budget for a new run.
// Normal/thorough agents follow a user-raised model-round timeout so the outer
// context cannot make that configured cap unreachable. Only quick is an
// intentionally stricter latency budget.
func (a *Agent) EffectiveRunTimeout() time.Duration {
	timeout := a.GetTimeout()
	if a.thoroughness == tools.ThoroughnessQuick {
		return timeout
	}
	minimum := a.ModelRoundTimeout() + config.DefaultAgentTimeoutHeadroom
	if timeout > 0 && timeout < minimum {
		return minimum
	}
	return timeout
}

// SetOutputStyle sets the response output style.
func (a *Agent) SetOutputStyle(s tools.OutputStyle) {
	a.stateMu.Lock()
	a.outputStyle = s
	a.stateMu.Unlock()
}

// SetProjectContext injects project guidelines for sub-agent system prompts.
func (a *Agent) SetProjectContext(ctx string) {
	a.stateMu.Lock()
	a.projectContext = ctx
	a.stateMu.Unlock()
}

// SetOnText sets the streaming callback for real-time output.
func (a *Agent) SetOnText(onText func(string)) {
	a.stateMu.Lock()
	a.onText = onText
	a.stateMu.Unlock()
}

// SetOnThinking sets the streaming callback for thinking/reasoning output.
func (a *Agent) SetOnThinking(onThinking func(string)) {
	a.stateMu.Lock()
	a.onThinking = onThinking
	a.stateMu.Unlock()
}

// SetWeakModelMode enables additional guidance for weaker models
// (more tool examples, tool guides for lightweight agents, directive rules).
func (a *Agent) SetWeakModelMode(enabled bool) {
	a.stateMu.Lock()
	a.weakModelMode = enabled
	a.stateMu.Unlock()
}

// SetRecentFilesProvider wires a callback that returns up to `limit` recently
// read file paths. When set, the agent uses it in injectContinuationHint to
// remind the model about files whose content was compacted away.
func (a *Agent) SetRecentFilesProvider(fn func(limit int) []string) {
	a.stateMu.Lock()
	a.recentFilesProvider = fn
	a.stateMu.Unlock()
}

// SetModifiedFilesProvider wires a callback that returns up to `limit` recently
// modified file paths. Used by injectContinuationHint so the model knows which
// files it has already written/edited after a compaction.
func (a *Agent) SetModifiedFilesProvider(fn func(limit int) []string) {
	a.stateMu.Lock()
	a.modifiedFilesProvider = fn
	a.stateMu.Unlock()
}

// SetOnScratchpadUpdate sets the callback for scratchpad updates.
func (a *Agent) SetOnScratchpadUpdate(fn func(string)) {
	a.stateMu.Lock()
	a.onScratchpadUpdate = fn
	a.stateMu.Unlock()
}

// SetPinnedContext updates the agent-local prompt state. The pin_context tool
// owns persistence so it can report storage failures before mutating this state.
func (a *Agent) SetPinnedContext(content string) {
	a.stateMu.Lock()
	a.PinnedContext = content
	a.stateMu.Unlock()
}

// LoadPinnedContext loads pinned context from disk if it exists.
func (a *Agent) LoadPinnedContext() {
	a.stateMu.RLock()
	workDir := a.workDir
	a.stateMu.RUnlock()

	if workDir == "" {
		return
	}
	content, err := pinned.Load(workDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logging.Warn("failed to restore pinned context", "error", err)
		}
		return
	}
	// Apply an empty durable-clear marker as well as non-empty content.
	a.stateMu.Lock()
	a.PinnedContext = content
	a.stateMu.Unlock()
}

// GetPinnedContext returns the pinned context.
func (a *Agent) GetPinnedContext() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.PinnedContext
}

// SetOnToolActivity sets the callback for tool activity reporting.
func (a *Agent) SetOnToolActivity(fn func(agentID, toolName string, args map[string]any, status string, success bool, summary string)) {
	a.stateMu.Lock()
	a.onToolActivity = fn
	a.stateMu.Unlock()
}

// SetStore sets the agent store for checkpoint persistence.
func (a *Agent) SetStore(store *AgentStore) {
	a.store = store
}

// EnableAutoCheckpoint enables automatic checkpointing.
// If interval > 0, sets the checkpoint interval explicitly.
// If interval <= 0, uses the existing interval (set by ApplyThoroughness) or defaults to 5.
func (a *Agent) EnableAutoCheckpoint(interval int) {
	a.autoCheckpoint = true
	if interval > 0 {
		a.checkpointInterval = interval
	}
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
// QueueSteer enqueues a steering message (e.g. a MetaAgent stuck-intervention)
// to be injected into the agent's history at the top of the next loop iteration.
// Safe to call from another goroutine. Deduplicates an identical pending nudge.
func (a *Agent) QueueSteer(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if slices.Contains(a.pendingSteers, msg) {
		return
	}
	a.pendingSteers = append(a.pendingSteers, msg)
}

// drainSteers appends any queued steering messages to history as user turns.
// Called at the top of each loop iteration under no external lock.
func (a *Agent) drainSteers() {
	a.stateMu.Lock()
	steers := a.pendingSteers
	a.pendingSteers = nil
	for _, s := range steers {
		a.history = append(a.history, genai.NewContentFromText(
			"[guidance] "+s, genai.RoleUser))
	}
	a.stateMu.Unlock()
	for _, s := range steers {
		logging.Info("injected steering message into agent", "agent_id", a.ID, "msg", s)
	}
}

// agentVerificationTools run a check that could surface a broken build/test.
// Any of them in the run means the agent at least had the chance to verify, so
// we don't nudge (conservative — avoids nagging agents that did verify).
var agentVerificationTools = map[string]bool{
	"bash": true, "verify_code": true, "run_tests": true, "run_command": true,
}

// needsVerificationNudge reports whether the agent changed code this run but
// never ran any verification command — the sub-agent analogue of "claimed done
// without verifying" that the foreground done-gate catches.
func (a *Agent) needsVerificationNudge() bool {
	mutated, verified := false, false
	for _, t := range a.GetToolsUsed() {
		// Use the canonical mutation-tool set so this never drifts from the
		// done-gate's touched-path detection (donegate.IsMutationTool).
		if donegate.IsMutationTool(t) {
			mutated = true
		}
		if agentVerificationTools[t] {
			verified = true
		}
	}
	return mutated && !verified
}

// appendVerificationNudge injects a one-time reminder to verify code changes
// before finishing, using the agent's own tools (bash/verify_code).
func (a *Agent) appendVerificationNudge() {
	const nudge = "[guidance] Before you finish: you changed code but haven't run any build/test/lint this turn. Run the narrowest meaningful check now (e.g. the project's build or targeted tests), fix the first real failure, then complete. Do not claim the work is done or verified until a check has actually passed."
	a.stateMu.Lock()
	a.history = append(a.history, genai.NewContentFromText(nudge, genai.RoleUser))
	a.stateMu.Unlock()
	logging.Info("verify-before-done nudge injected into sub-agent", "agent_id", a.ID, "type", a.Type)
}

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
	a.stateMu.Lock()
	a.onInput = onInput
	a.stateMu.Unlock()
}

// SetOnPlanApproved sets a callback for when a plan is built and ready.
// The callback receives a plan summary and should clear/compact context.
func (a *Agent) SetOnPlanApproved(callback func(planSummary string)) {
	a.stateMu.Lock()
	a.onPlanApproved = callback
	a.stateMu.Unlock()
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
				a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
			},
			func(tree *PlanTree, node *PlanNode, success bool) {
				a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
			},
			func(tree *PlanTree, ctx *ReplanContext) {
				a.safeOnText(fmt.Sprintf("\n[Replanning: %s]\n", ctx.Error))
				a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
			},
			func(action *PlannedAction) {
				// Record planning progress
				a.safeOnText(fmt.Sprintf("  • %s: %s\n", action.AgentType, action.Prompt))
				a.SetProgress(0, 0, "Planning: "+action.Prompt)
			},
		)
	}
}

// SetSharedMemory sets the shared memory instance for inter-agent communication.
func (a *Agent) SetSharedMemory(sm *SharedMemory) {
	a.stateMu.Lock()
	a.sharedMemory = sm
	a.stateMu.Unlock()
}

// GetSharedMemory returns the shared memory instance.
func (a *Agent) GetSharedMemory() *SharedMemory {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
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

// ToolsUsedCount returns how many tools have run this agent — a cheap, lock-safe
// progress signal for the incomplete-work continuation (a rising count between
// nudges means the model is acting, not just narrating).
func (a *Agent) ToolsUsedCount() int {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	return len(a.toolsUsed)
}

// MutatingToolCount returns how many code/repo-MUTATING tools (IsImplementationTool)
// this agent ran — the /loop churn signal ("did this iteration change anything").
// Derived from the existing toolsUsed list (no extra hot-loop tracking).
func (a *Agent) MutatingToolCount() int {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	n := 0
	for _, name := range a.toolsUsed {
		if tools.IsImplementationTool(name) {
			n++
		}
	}
	return n
}

// StatefulToolAttemptCount returns how many potentially stateful tools crossed
// the execution boundary in the current Run. It is intentionally broader than
// MutatingToolCount: tools.IsWriteTool treats bash and unknown MCP/plugin tools
// as stateful unless they are explicitly allow-listed as side-effect-free.
func (a *Agent) StatefulToolAttemptCount() int {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	return a.statefulToolAttempts
}

func (a *Agent) recordStatefulToolAttempt(toolName string) {
	if !tools.IsWriteTool(toolName) {
		return
	}
	a.toolsMu.Lock()
	a.statefulToolAttempts++
	a.toolsMu.Unlock()
}

// SetPlanGoal sets the goal for the plan.
func (a *Agent) SetPlanGoal(goal *PlanGoal) {
	a.stateMu.Lock()
	a.planGoal = goal
	a.stateMu.Unlock()
}

// SetRequireApproval sets whether plan approval is required.
func (a *Agent) SetRequireApproval(required bool) {
	a.stateMu.Lock()
	a.requireApproval = required
	a.stateMu.Unlock()
}

// EnablePlanningMode enables tree-based planning for agent execution.
func (a *Agent) EnablePlanningMode(goal *PlanGoal) {
	a.stateMu.Lock()
	a.planningMode = true
	a.planGoal = goal
	a.stateMu.Unlock()
}

// DisablePlanningMode disables tree-based planning.
func (a *Agent) DisablePlanningMode() {
	a.stateMu.Lock()
	a.planningMode = false
	a.planGoal = nil
	if a.activePlan != nil {
		a.lastPlanTree = a.activePlan
	}
	a.activePlan = nil
	a.stateMu.Unlock()
}

// GetActivePlan returns the currently active plan tree.
func (a *Agent) GetActivePlan() *PlanTree {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.activePlan
}

// IsPlanningMode returns whether the agent is in planning mode.
func (a *Agent) IsPlanningMode() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.planningMode
}

// mapModelName maps user-friendly shorthands to actual GLM/MiniMax/Kimi model
// names. Was previously a Gemini shorthand map; post-v0.65.0 only GLM/Kimi/
// MiniMax/Ollama providers exist, so the shorthands are adjusted accordingly.
func mapModelName(name string) string {
	switch strings.ToLower(name) {
	case "flash", "fast":
		return "glm-5-turbo"
	case "pro", "strong":
		return "glm-5.2"
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
	// Delegates so the two filters cannot drift: this one and the list form
	// were byte-identical apart from their nil-vs-empty check, which meant every
	// rule about what a sub-agent may hold had to be written twice.
	return createFilteredRegistryFromList(agentType.AllowedTools(), baseRegistry)
}

// RequestTool dynamically adds a tool from the base registry to the agent's active registry.
func (a *Agent) RequestTool(name string) error {
	// Check if already in active registry
	if _, ok := a.registry.Get(name); ok {
		return nil // Already have this tool
	}

	// Checked before the per-type authorization below, which it deliberately
	// does not depend on: an unrestricted type (AgentTypeGeneral returns a nil
	// allowlist) skips that check entirely and pulls straight from the base
	// registry, so a foreground-only tool filtered out of this agent's registry
	// would come back through here. The registry filter alone does not hold.
	if foregroundOnlyTools[name] {
		return fmt.Errorf("tool %q runs only in the foreground session and cannot be requested by an agent", name)
	}

	a.stateMu.RLock()
	restricted := a.requestedToolsRestricted
	_, authorized := a.allowedRequestedTools[name]
	a.stateMu.RUnlock()
	if restricted && !authorized {
		return fmt.Errorf("tool %q is outside this agent's authorized capabilities", name)
	}

	tool, ok := a.baseRegistry.Get(name)
	if !ok {
		return fmt.Errorf("tool not found in system: %s", name)
	}

	cloned := cloneToolForAgentWithWorkDir(tool, a.workDir)
	if taskTool, ok := cloned.(*tools.TaskTool); ok {
		ceiling := a.registry.Names()
		ceiling = append(ceiling, taskTool.Name())
		taskTool.SetToolCapabilityCeiling(ceiling)
	}
	if skillTool, ok := cloned.(*tools.SkillTool); ok {
		a.stateMu.RLock()
		ledger := a.invokedSkills
		a.stateMu.RUnlock()
		if ledger == nil {
			// Production constructors always initialize the ledger. Keep the
			// literal/test seam fail-safe without ever borrowing the base tool's
			// (possibly foreground-owned) ledger.
			a.stateMu.Lock()
			if a.invokedSkills == nil {
				a.invokedSkills = skills.NewInvocationLedger()
			}
			ledger = a.invokedSkills
			a.stateMu.Unlock()
		}
		skillTool.SetInvocationLedger(ledger)
	}
	return a.registry.Register(cloned)
}

// SetAllowedRequestedTools supplies explicit policy authority for tools that
// request_tool may add. It is intentionally fail-closed: a nil or empty list
// authorizes no additions; it never removes the capability check. This setter
// is runner-facing trusted configuration and is not exposed as a model tool.
func (a *Agent) SetAllowedRequestedTools(names []string) {
	a.stateMu.Lock()
	a.requestedToolsRestricted = true
	a.allowedRequestedTools = requestedToolSet(names)
	a.stateMu.Unlock()
}

func requestedToolSet(names []string) map[string]struct{} {
	if names == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = struct{}{}
		}
	}
	return allowed
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
	// A reusable/resumed Agent gets fresh skill permission rules for every run.
	// Skill instructions may persist in history; grants and denies do not.
	tools.BeginSkillPermissionTurn(a.registry)

	invocationScope := InvocationScopeFromContext(ctx)
	a.stateMu.Lock()
	a.status = AgentStatusRunning
	a.startTime = time.Now()
	a.runCtx = ctx
	// Scope is per RUN, not per persisted Agent ID. Resume reuses the logical
	// ID but belongs to the invocation whose context acquired the run lease.
	// Runner paths enter Run only after that lease is held.
	a.invocationScope = invocationScope
	// Agent instances can be resumed and Run again. These counters describe
	// this invocation, not the lifetime of the reusable Agent object.
	a.usageInputTokens = 0
	a.usageOutputTokens = 0
	a.usageCacheReadTokens = 0
	a.usageEstimatedCost = 0
	a.usageCostTracked = false
	a.runPolicyBlock = nil
	hasHistory := len(a.history) > 0
	if a.originalPrompt == "" {
		a.originalPrompt = prompt // Preserve for continuation after compaction
	}
	a.stateMu.Unlock()
	// Retry provenance belongs to this invocation, not the reusable Agent ID.
	// Reset it only after the run lease has entered Agent.Run, before any tool can
	// cross its execution boundary.
	a.toolsMu.Lock()
	a.statefulToolAttempts = 0
	a.toolsMu.Unlock()

	// Initialize progress
	a.SetProgress(0, a.maxTurns, "Starting agent execution")

	// Keep the live transcript in the durable base workspace, not inside an
	// isolated worktree that apply-back cleanup removes after completion.
	// Publish it before the first model chunk so GetResult/task_output can read
	// incremental output while the invocation is still running.
	outputWriter := NewAgentOutputWriter(a.outputBaseDir, a.ID)
	a.stateMu.Lock()
	previousOutputWriter := a.outputWriter
	a.outputWriter = outputWriter
	a.stateMu.Unlock()
	if previousOutputWriter != nil {
		previousOutputWriter.Close()
	}
	defer outputWriter.Close()

	result := &AgentResult{
		AgentID:         a.ID,
		Type:            a.Type,
		Model:           a.Model,
		Status:          AgentStatusRunning,
		Completed:       false,
		InvocationScope: invocationScope,
	}

	if !hasHistory {
		// Build fresh history only for new agents; resumed agents must preserve restored context.
		systemPrompt := a.buildSystemPrompt()
		a.stateMu.Lock()
		if len(a.history) == 0 {
			a.history = []*genai.Content{
				genai.NewContentFromText(systemPrompt, genai.RoleUser),
				genai.NewContentFromText("I understand. I'll help with the task using only my allowed tools.", genai.RoleModel),
			}
		}
		a.stateMu.Unlock()
	}

	// Execute the prompt through the function calling loop
	var finalOutput strings.Builder
	_, output, err := a.executeLoop(ctx, prompt, &finalOutput)
	result.PolicyBlock = a.policyBlockSnapshot()
	// Snapshot identity AFTER executeLoop: FallbackClient can switch provider
	// (and model) while serving the request. Capturing this before the first
	// request attributes the whole run to the failed primary provider.
	a.populateResultIdentity(result)

	// safeOnText has already written streamed chunks and lifecycle markers as
	// they happened. Preserve output from an internal/non-streaming path too.
	if outputWriter.TotalBytes() == 0 && output != "" {
		outputWriter.WriteString(output)
	}

	if err != nil {
		terminalStatus := AgentStatusFailed
		progress := "Failed: " + err.Error()
		if errors.Is(err, context.Canceled) {
			terminalStatus = AgentStatusCancelled
			progress = "Cancelled"
		}
		a.stateMu.Lock()
		a.status = terminalStatus
		a.endTime = time.Now()
		endTime := a.endTime
		startTime := a.startTime
		result.InputTokens = a.usageInputTokens
		result.OutputTokens = a.usageOutputTokens
		result.CacheReadInputTokens = a.usageCacheReadTokens
		result.EstimatedCost = a.usageEstimatedCost
		result.CostTracked = a.usageCostTracked
		a.stateMu.Unlock()
		result.MutatingToolCalls = a.MutatingToolCount()
		result.StatefulToolAttempts = a.StatefulToolAttemptCount()
		result.TouchedPaths = a.GetTouchedPaths()

		// Clear callHistory to prevent memory leak
		a.clearCallHistory()

		result.Status = terminalStatus
		result.Error = err.Error()
		result.Output = boundedAgentResultOutput(output, outputWriter.FilePath(), outputWriter.DiskTruncated())
		result.OutputFile = outputWriter.FilePath()
		result.Duration = endTime.Sub(startTime)
		result.Completed = true

		// Update progress with failure
		a.SetProgress(a.currentStep, a.totalSteps, progress)

		a.collectTreeMetrics(result)
		return result, err
	}

	a.stateMu.Lock()
	a.status = AgentStatusCompleted
	a.endTime = time.Now()
	endTime := a.endTime
	startTime := a.startTime
	result.InputTokens = a.usageInputTokens
	result.OutputTokens = a.usageOutputTokens
	result.CacheReadInputTokens = a.usageCacheReadTokens
	result.EstimatedCost = a.usageEstimatedCost
	result.CostTracked = a.usageCostTracked
	a.stateMu.Unlock()
	result.MutatingToolCalls = a.MutatingToolCount()
	result.StatefulToolAttempts = a.StatefulToolAttemptCount()
	result.TouchedPaths = a.GetTouchedPaths()

	// Clear callHistory to prevent memory leak on long-running sessions
	a.clearCallHistory()

	result.Status = AgentStatusCompleted
	result.Output = boundedAgentResultOutput(output, outputWriter.FilePath(), outputWriter.DiskTruncated())
	result.OutputFile = outputWriter.FilePath()
	result.Duration = endTime.Sub(startTime)
	result.Completed = true

	// Update progress with completion — read totalSteps under progressMu
	a.progressMu.Lock()
	total := a.totalSteps
	a.progressMu.Unlock()
	a.SetProgress(total, total, "Completed")

	a.collectTreeMetrics(result)
	return result, nil
}

// collectTreeMetrics gathers tree planner statistics into AgentResult.Metadata.
func (a *Agent) collectTreeMetrics(result *AgentResult) {
	a.stateMu.RLock()
	tree := a.activePlan
	if tree == nil {
		tree = a.lastPlanTree
	}
	a.stateMu.RUnlock()

	if tree == nil {
		return
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}

	tree.mu.RLock()
	result.Metadata["tree_total_nodes"] = tree.TotalNodes
	result.Metadata["tree_max_depth"] = tree.MaxDepth
	result.Metadata["tree_expanded_nodes"] = tree.ExpandedNodes
	result.Metadata["tree_replan_count"] = tree.ReplanCount

	succeeded := len(tree.GetSucceededPath())
	failed := 0
	var countFailed func(n *PlanNode)
	countFailed = func(n *PlanNode) {
		if n.Status == PlanNodeFailed {
			failed++
		}
		for _, child := range n.Children {
			countFailed(child)
		}
	}
	if tree.Root != nil {
		countFailed(tree.Root)
	}
	tree.mu.RUnlock()

	result.Metadata["tree_succeeded_nodes"] = succeeded
	result.Metadata["tree_failed_nodes"] = failed
}

// clearCallHistory clears the call history map to prevent memory leaks.
func (a *Agent) clearCallHistory() {
	a.callHistoryMu.Lock()
	a.callHistory = make(map[string]int)
	a.loopEarlyWarned = make(map[string]bool)
	a.coverage = tools.NewCoverageState()
	a.coverageTrips = 0
	a.callHistoryMu.Unlock()
}

// resetLoopDetection clears ALL per-call loop-detection state, including the
// intervention flag and cooldown. Called after compaction: summarization
// replaces the middle of the history the model reasons over, so repetition
// counts accumulated against the now-summarized calls no longer correspond to
// anything the model can still see. Keeping them would falsely trip the loop
// guard on a legitimate post-compaction re-read — the continuation hint even
// tells the model it MAY re-read for specific details. Real loops are not
// hidden: the executor's consecutive-stagnation guard still catches tight
// within-turn loops, and a genuine post-compaction loop simply re-accumulates
// from a clean slate that matches the model's own reset view of the context.
func (a *Agent) resetLoopDetection() {
	a.callHistoryMu.Lock()
	a.callHistory = make(map[string]int)
	a.loopEarlyWarned = make(map[string]bool)
	a.coverage = tools.NewCoverageState()
	a.coverageTrips = 0
	a.loopIntervened = false
	a.loopCooldown = 0
	a.callHistoryMu.Unlock()
}

// broadLoopExplorationTools are local code/repo INSPECTION tools that have no
// side effects and no external cost, and are legitimately called many times in a
// single multi-file request (a routine "fix this bug" / "understand X" task reads
// and greps and blames dozens of distinct files). They get a raised broad-loop
// ceiling (see broadLoopThreshold).
//
// Deliberately NOT the full workspace-isolation read-only set: web_fetch /
// web_search (network cost, rate limits), ask_user (UX), and the plan/memory
// tools are read-only but are NOT free-to-repeat exploration, so they keep the
// low base threshold where a runaway loop is caught early.
var broadLoopExplorationTools = toolNameSet(
	"read", "grep", "glob", "list_dir", "tree",
	"diff", "git_status", "git_diff", "git_log", "git_blame",
	"check_impact", "history_search",
	// Semantic + review inspection tools (v0.87.0) — also side-effect-free,
	// no external cost, and called many times over distinct symbols/files in
	// one understand/refactor task. Missing here, a reasoning-heavy Kimi K3
	// sub-agent navigating a codebase hit the low base ceiling (v0.100.95).
	"go_search", "go_diagnostics", "go_to_definition", "find_references",
	"review_changes",
)

// broadLoopThreshold returns the per-tool ceiling for the broad loop detector
// (same tool called N times with ANY args within one request).
//
// Local inspection tools (broadLoopExplorationTools) get a much higher ceiling
// (4× base) because counting their many legitimate calls against the low base
// threshold (8) made the broad detector fire a false "STOP, you're stuck —
// reconsider your whole approach" intervention and SKIP the 9th read on exactly
// the complex tasks where the agent must not be derailed. Mutating/expensive
// tools (write/edit/bash/delete/…) keep the base threshold — repeating one of
// those many times is suspicious much sooner.
//
// Note on safety: the exact-args loop (exactCount>3) catches only *identical*
// re-reads (same normalized args). A loop that perturbs args every call (a
// drifting read offset, rotating grep patterns) produces a distinct
// normalizeCallKey each time and so escapes exact-args, plan repetition, and
// no-progress (a successful read of a genuinely-new range resets noProgressTurns).
// For read/grep that escape is now closed by the distinct-target coverage guard
// (recordCoverageLocked + redundancyBudget): it counts REDUNDANT re-coverage of
// already-seen ranges/scopes and trips well below this broad ceiling. NOTE:
// normalizeCallKey still keeps distinct offsets distinct on purpose (do NOT
// re-add a docstring claiming exact-args backstops perturbing loops). The
// executor's within-turn stagnation guard had the same offset/pattern blind
// spot until the executor-side mirror landed (tools/executor_coverage.go).
// Residual, documented gap: a loop rotating over 3+ DISTINCT targets below
// the broad ceiling. The broad counter remains the hard backstop.
func (a *Agent) broadLoopThreshold(toolName string) int {
	if _, ok := broadLoopExplorationTools[toolName]; ok {
		return a.loopThreshold * 4
	}
	return a.loopThreshold
}

// coverageTarget canonicalizes a read/grep call to the GROUND it covers, dropping
// the perturbable args (read offset/limit, grep pattern) so that re-covering the
// same file/scope with different args maps to the same target. Returns ok=false
// for calls it can't canonicalize (never counted). Separate from normalizeCallKey
// by design — that key MUST keep distinct offsets distinct (loop_detection_keys_test).
func (a *Agent) coverageTarget(name string, args map[string]any) (string, bool) {
	switch name {
	case "read":
		norm := donegate.NormalizeTouchedPaths(a.workDir, donegate.ExtractTouchedPaths(args))
		if len(norm) == 0 {
			return "", false
		}
		return norm[0], true
	case "grep":
		if p, ok := args["path"].(string); ok && strings.TrimSpace(p) != "" {
			if norm := donegate.NormalizeTouchedPaths(a.workDir, []string{p}); len(norm) > 0 {
				return norm[0], true
			}
			return strings.TrimSpace(p), true
		}
		// Path-less grep searches the whole workspace — collapse to one scope so
		// rotating patterns over the default scope are still seen as re-coverage.
		return "<scope>", true
	}
	return "", false
}

// recordCoverageLocked updates coverage for a read/grep call and returns that
// TARGET's consecutive no-progress redundancy count, delegating to the shared
// coverage core (tools.CoverageState). It supplies the donegate-based canonical
// target; the span/progress/redundancy bookkeeping lives in the core, shared
// verbatim with the executor's within-turn guard (Tier-4 slice 1).
// Caller holds a.callHistoryMu.
func (a *Agent) recordCoverageLocked(name string, args map[string]any) int {
	target, ok := a.coverageTarget(name, args)
	if !ok {
		return 0
	}
	return a.coverage.RecordTarget(name, target, args)
}

// noteCoverageProgressLocked marks a progress signal for the coverage guard:
// any non-read/grep tool call (edit, bash, glob, todo, …) proves the model is
// not in a tight read/grep rotation, so it breaks every target's redundancy
// chain. Caller holds a.callHistoryMu.
func (a *Agent) noteCoverageProgressLocked() {
	a.coverage.NoteProgress()
}

// redundancyBudget is the consecutive no-progress re-coverage ceiling per tool,
// scaled by mode like broadLoopThreshold. read gets the full base (paging and
// zoom-ins are common); grep gets half with a floor of 2 — N+1 back-to-back
// searches of one scope with nothing in between (not even reading a result) is
// loop-like much sooner.
func (a *Agent) redundancyBudget(tool string) int {
	if tool == "grep" {
		if b := a.loopThreshold / 2; b >= 2 {
			return b
		}
		return 2
	}
	return a.loopThreshold
}

// buildSystemPrompt creates the system prompt based on agent type.
// proposalHonestyRule mirrors the foreground base prompt's
// verify-before-proposing rule for sub-agents (they build их own system
// prompt and never see the foreground universal rules).
const proposalHonestyRule = "\nProposal honesty: before suggesting an improvement, feature, or fix for this codebase, verify with tools (grep/read) that it does not already exist — most unverified suggestions turn out to be already implemented. Cite what you checked, or explicitly mark the idea 'not verified against the code'.\n"

func (a *Agent) buildSystemPrompt() string {
	// Snapshot mutable fields under stateMu to avoid races
	a.stateMu.RLock()
	pinnedCtx := a.PinnedContext
	scratchpad := a.Scratchpad
	projectContext := a.projectContext
	outputStyle := a.outputStyle
	sharedMemory := a.sharedMemory
	workDir := a.workDir
	a.stateMu.RUnlock()

	var sb strings.Builder

	sb.WriteString("You are a specialized sub-agent with limited tool access.\n")
	fmt.Fprintf(&sb, "Agent Type: %s\n", a.Type)
	sb.WriteString("Available tools: ")

	toolNames := a.registry.Names()
	sb.WriteString(strings.Join(toolNames, ", "))
	sb.WriteString("\n")
	availableTools := make(map[string]bool, len(toolNames))
	for _, name := range toolNames {
		availableTools[name] = true
	}
	backgroundAvailable := false
	if bash, ok := a.registry.Get("bash"); ok {
		if declaration := bash.Declaration(); declaration != nil && declaration.Parameters != nil {
			_, backgroundAvailable = declaration.Parameters.Properties["run_in_background"]
		}
	}
	if workDir != "" {
		fmt.Fprintf(&sb, "Working directory: %s\n", workDir)
	}
	sb.WriteString("\n")

	// Inject Pinned Context if provided (Custom Improvement)
	if pinnedCtx != "" {
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("                         PINNED CONTEXT\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString(pinnedCtx)
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n\n")
	}

	// Lightweight sub-agents (explore, bash, guide) get a minimal prompt:
	// skip learning, detailed rules, tool guides to reduce token overhead.
	// General/plan agents get the full prompt with all context.
	lightweight := a.Type == AgentTypeExplore || a.Type == AgentTypeBash || a.Type == AgentTypeGuide

	// Inject project-specific knowledge.
	// Include for non-lightweight agents always; also for lightweight in weak model mode.
	if (!lightweight || a.weakModelMode) && a.learning != nil {
		sb.WriteString(a.learning.FormatForPrompt())
		sb.WriteString("\n")
	}

	// Inject recent shared-memory context from other agents (if available).
	if sharedMemory != nil {
		if sharedCtx := sharedMemory.GetForContext(a.ID, 15); sharedCtx != "" {
			sb.WriteString(sharedCtx)
			sb.WriteString("\n")
		}
	}

	if lightweight {
		// Compact rules for short-lived sub-agents
		sb.WriteString("RULES: Use tools to complete the task. Summarize findings clearly with file:line refs.\n")
		sb.WriteString("If a tool fails, try an alternative approach. Never retry the same call.\n\n")
		if a.Type == AgentTypeBash {
			if availableTools["run_tests"] {
				sb.WriteString("For project test suites, prefer run_tests; it supports timeout_seconds and preserves parsed totals.\n")
			}
			if backgroundAvailable && availableTools["task_output"] {
				sb.WriteString("For other long commands, bash may use run_in_background=true; inspect completion with task_output.\n\n")
			} else {
				sb.WriteString("Background execution is unavailable in this workspace. Do not request or retry run_in_background; use run_tests or foreground bash with timeout_seconds.\n\n")
			}
		}
	} else {
		// Universal instructions for all agents
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("                         MANDATORY RULES\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
		sb.WriteString("1. ALWAYS use tools to complete your task - don't just say you can't\n")
		sb.WriteString("2. After using ANY tool, provide a CLEAR summary of what you found\n")
		sb.WriteString("3. NEVER respond with just 'OK' or 'Done' - always explain\n")
		sb.WriteString("4. Structure responses with markdown: headers, bullets, code blocks\n")
		sb.WriteString("5. Include specific file:line references when discussing code\n\n")

		// Tool limitations awareness
		sb.WriteString("## Tool Limitations\n")
		sb.WriteString("- bash: Output truncated at 30,000 characters. Use grep/head/tail for large outputs.\n")
		sb.WriteString("- grep: Returns max 500 matches. Use more specific patterns for large codebases.\n")
		sb.WriteString("- glob: Returns max 1000 files. Use specific patterns instead of `**/*`.\n")
		sb.WriteString("- read: Returns max 2000 lines. Use offset/limit for large files.\n\n")

		// Error recovery guidance
		sb.WriteString("## Error Recovery\n")
		sb.WriteString("- If a tool fails, analyze the error before retrying.\n")
		sb.WriteString("- If read fails with \"not found\", use glob to find the correct path.\n")
		sb.WriteString("- If bash fails, check if the command exists and try alternatives.\n")
		sb.WriteString("- Never retry the exact same call more than once.\n\n")

		// Effective patterns
		sb.WriteString("## Effective Patterns\n")
		sb.WriteString("- Find then read: glob to locate, then read specific files.\n")
		sb.WriteString("- Search then edit: grep to find occurrences, then edit with context.\n")
		sb.WriteString("- Verify after change: after write/edit, read to confirm.\n")
		if backgroundAvailable && availableTools["task_output"] {
			sb.WriteString("- For long-running operations, bash may use run_in_background=true; inspect completion with task_output.\n\n")
		} else if availableTools["run_tests"] {
			sb.WriteString("- For long tests use run_tests with timeout_seconds. Background execution is unavailable; do not retry it.\n\n")
		}
	}

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

	// Inject output style instructions (orthogonal to thoroughness)
	sb.WriteString(buildOutputStyleSection(outputStyle))

	// Inject project context if provided (for delegated sub-agents).
	// Lightweight agents get only working directory, not full project instructions.
	if projectContext != "" {
		if lightweight {
			// Extract just the working directory line from project context
			for line := range strings.SplitSeq(projectContext, "\n") {
				if strings.HasPrefix(line, "Working directory:") || strings.HasPrefix(line, "Project:") {
					sb.WriteString(line)
					sb.WriteString("\n")
				}
			}
		} else {
			sb.WriteString("\n")
			sb.WriteString(projectContext)
			sb.WriteString("\n")
		}
	}

	// Inject scratchpad if not empty
	if scratchpad != "" {
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("                         YOUR SCRATCHPAD\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("This is your persistent memory. Use it to store facts, thoughts, or plans.\n\n")
		sb.WriteString(scratchpad)
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
	}

	// Inject tool usage guides.
	// Include for non-lightweight agents always; also for lightweight when weak model needs guidance.
	if !lightweight || a.weakModelMode {
		sb.WriteString(a.buildToolGuidesSection())
	}

	// Proposal honesty (v0.100.105 field report: a session produced a 7-item
	// improvement list where 4 items ALREADY existed in the code — the model
	// never checked its own suggestions). Applies to every substantive agent.
	if !lightweight {
		sb.WriteString(proposalHonestyRule)
	}

	// For weak/medium models, add explicit guidance to prevent common mistakes.
	if a.weakModelMode {
		sb.WriteString(a.buildWeakModelGuidance())
	}

	return sb.String()
}

// buildWeakModelGuidance returns a concise set of rules that prevent
// the most common mistakes made by weaker models. Includes a GLM-specific
// section when the provider is z.ai — those models have a distinct set of
// failure modes (thinking-budget truncation, malformed tool_use ids,
// SSE error-object mid-stream) that benefit from explicit guidance.
func (a *Agent) buildWeakModelGuidance() string {
	base := `
## IMPORTANT RULES (read carefully)

### File editing
- ALWAYS use old_string/new_string with EXACT text from the file (copy-paste)
- Include enough surrounding context to make old_string unique
- NEVER guess file contents — read the file first

### Version consistency
- ALWAYS read go.mod before writing CI/workflow files
- Use the EXACT Go version from go.mod (e.g. go-version: '1.25.7')
- All workflow files must use the same version

### Testing patterns
- ALWAYS check errors: if err != nil { t.Fatal(err) } — never _ = err
- ALWAYS add testing.Short() guard before HTTP/network calls
- Every Test function MUST have at least one assertion (t.Error, t.Fatal, if check)

### Shell / CI
- GitHub Actions: use ${{ steps.ID.outputs.NAME }} for step outputs, not ${NAME}
- Heredocs: use << EOF (unquoted) when you need shell variable interpolation
- Pin actions to tags: uses: actions/checkout@v4, never @master
- Scripts: start with set -euo pipefail

### Security
- NEVER hardcode API keys, tokens, or passwords — use environment variables
- NEVER commit .env files or credentials

### Tool selection
- Before calling a tool, check if a previous call in this conversation already did the work
- If a tool fails twice with the same args, change approach — don't retry a third time
- Unknown tool names trigger a hint with the closest real name — use it, don't invent names
- For parallel-safe reads (grep/read/glob): batch them in one turn
- For writes (edit/write/bash): go one at a time and verify each result before the next
- Edit results SHOW the updated region of the file — NEVER call read on a file just to verify an edit; the result already proves what's on disk. Verify by building/testing, not by re-reading
- Content you already read is still in this conversation — re-read a file ONLY if a tool reported changing it
`
	if strings.HasPrefix(strings.ToLower(a.Model), "glm") {
		base += `
### GLM-specific
- Keep reasoning tight: thinking tokens cost the same as output tokens. Plan in 1–2 short paragraphs, then act.
- tool_use blocks need unique ids per call — do NOT reuse an id from an earlier turn
- If the previous response ended mid-sentence or mid-tool-call, continue from exactly that point — do not restart the task
- When answering without tools, prefer short factual replies over long essays; GLM over-generates when prompted to "explain"
`
	}
	return base
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

			fmt.Fprintf(&sb, "### %s\n", name)
			fmt.Fprintf(&sb, "**When to use:** %s\n\n", guide.WhenToUse)
			fmt.Fprintf(&sb, "**How to respond:** %s\n\n", guide.HowToRespond)
			if guide.CommonMistakes != "" {
				fmt.Fprintf(&sb, "**Avoid:** %s\n\n", guide.CommonMistakes)
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
	switch a.thoroughness {
	case tools.ThoroughnessQuick:
		return a.buildExplorePromptQuick()
	case tools.ThoroughnessThorough:
		return a.buildExplorePromptThorough()
	default:
		return a.buildExplorePromptNormal()
	}
}

func (a *Agent) buildExplorePromptQuick() string {
	return `═══════════════════════════════════════════════════════════════════════
                    EXPLORE AGENT (QUICK MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Answer fast. Minimal exploration.

RULES:
- Use 1-2 glob/grep calls max. Do NOT over-explore.
- Give a brief, direct answer. No deep analysis needed.
- Skip Architecture and Recommendations sections.

RESPONSE FORMAT:
## Summary
[Direct answer in 1-2 sentences]

## Key Findings
- **Finding** (file.go:123): Brief description

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildExplorePromptNormal() string {
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

func (a *Agent) buildExplorePromptThorough() string {
	return `═══════════════════════════════════════════════════════════════════════
                  EXPLORE AGENT (THOROUGH MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Perform a comprehensive, exhaustive exploration of the codebase.

RULES:
- Be extremely thorough. Check multiple directories and naming conventions.
- Cross-reference findings by reading actual code, not just file names.
- Verify assumptions — read implementations, not just interfaces.
- Consider edge cases, error paths, and alternative implementations.
- Search for related tests, configs, and documentation.

RECOMMENDED APPROACH:
1. glob - Broad search across multiple patterns and directories
2. grep - Search for usages, references, and cross-cutting concerns
3. read - Read all relevant files in full, not just snippets
4. Analyze data flow end-to-end, trace through call chains
5. Summarize with comprehensive detail

RESPONSE FORMAT:
## Summary
[Comprehensive answer with full context]

## Key Findings
- **Finding 1** (file.go:123): Detailed description with cross-references
- **Finding 2** (other.go:45): Detailed description with cross-references

## Code Examples
` + "```" + `go
// Key code with full context and explanation
` + "```" + `

## Architecture
[Full component diagram, data flow, dependencies, lifecycle]

## Cross-References
[Related files, tests, configs that interact with the findings]

## Edge Cases & Caveats
[Known limitations, error paths, race conditions, TODOs]

## Recommendations
[Detailed actionable suggestions with rationale]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildBashPrompt() string {
	switch a.thoroughness {
	case tools.ThoroughnessQuick:
		return a.buildBashPromptQuick()
	case tools.ThoroughnessThorough:
		return a.buildBashPromptThorough()
	default:
		return a.buildBashPromptNormal()
	}
}

func (a *Agent) buildBashPromptQuick() string {
	return `═══════════════════════════════════════════════════════════════════════
                      BASH AGENT (QUICK MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Execute the command and report the result briefly.

RULES:
- Run the command, report success/failure and key output.
- Skip detailed analysis. No Next Steps section.
- Keep response to 2-3 sentences max.

RESPONSE FORMAT:
## Result
[Command + outcome in 1-2 sentences]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildBashPromptNormal() string {
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

func (a *Agent) buildBashPromptThorough() string {
	return `═══════════════════════════════════════════════════════════════════════
                    BASH AGENT (THOROUGH MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Execute commands with deep analysis of results.

RULES:
- Analyze output thoroughly: errors, warnings, performance, edge cases.
- For failures: identify root cause, check related files, suggest fixes.
- Run follow-up commands if needed to gather more context.
- Check for related issues that might not be immediately obvious.

APPROACH:
1. Execute the primary command
2. Analyze output in detail (errors, warnings, patterns)
3. Run diagnostic commands if issues found
4. Provide root cause analysis and actionable fixes

RESPONSE FORMAT:
## Command Executed
` + "```" + `bash
[The command you ran]
` + "```" + `

## Results Summary
[What the command did and outcome overview]

## Detailed Analysis
[In-depth analysis of output, error patterns, performance metrics]

## Root Cause Analysis
[For failures: why it failed, what triggered the issue]

## Related Issues
[Other problems discovered, warnings worth noting, dependencies affected]

## Fix Recommendations
[Step-by-step actionable fixes with rationale]

## Verification
[Commands to verify the fixes work]

═══════════════════════════════════════════════════════════════════════
`
}

func buildOutputStyleSection(style tools.OutputStyle) string {
	switch style {
	case tools.OutputStyleConcise:
		return `
## Output Style: CONCISE
- Use bullet points, not paragraphs.
- Omit examples unless critical.
- No filler phrases ("Let me explain...", "Here's what I found...").
- Maximum 5-7 lines for the entire response.
`
	case tools.OutputStyleDetailed:
		return `
## Output Style: DETAILED
- Provide full explanations with context and rationale.
- Include code examples with surrounding context.
- Explain trade-offs, alternatives considered, and why.
- Add cross-references to related files and functions.
- Use paragraphs for complex explanations, not just bullets.
`
	default:
		return "" // normal — no override, use agent type's default format
	}
}

func (a *Agent) buildGeneralPrompt() string {
	switch a.thoroughness {
	case tools.ThoroughnessQuick:
		return a.buildGeneralPromptQuick()
	case tools.ThoroughnessThorough:
		return a.buildGeneralPromptThorough()
	default:
		return a.buildGeneralPromptNormal()
	}
}

func (a *Agent) buildGeneralPromptQuick() string {
	return `═══════════════════════════════════════════════════════════════════════
                    GENERAL AGENT (QUICK MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Complete the task as fast as possible with minimal overhead.

RULES:
- Skip deep exploration. Read only the files you need to edit.
- Make the change directly. No detailed planning.
- Brief summary only — no Verification or Recommendations sections.

RESPONSE FORMAT:
## Changes Made
- **file.go:N**: [What changed]

## Summary
[1-2 sentences]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGeneralPromptNormal() string {
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

func (a *Agent) buildGeneralPromptThorough() string {
	return `═══════════════════════════════════════════════════════════════════════
                  GENERAL AGENT (THOROUGH MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Complete the task with comprehensive analysis and verification.

RULES:
- Read surrounding code to understand context before editing.
- Verify assumptions by reading implementations, not just interfaces.
- Check for related files that need consistent changes (tests, configs, docs).
- Consider edge cases, error handling, and concurrency safety.
- Run verification commands (build, vet, tests) after making changes.

APPROACH:
1. Explore codebase to understand existing patterns
2. Identify all files that need modification (including tests)
3. Plan changes to maintain consistency
4. Execute changes step by step
5. Verify with build/vet/tests
6. Summarize with full detail

RESPONSE FORMAT:
## Task Summary
[What was requested and why]

## Analysis
[Existing code patterns, dependencies, constraints discovered]

## Changes Made
- **file1.go:N**: [What changed, why, and how it fits existing patterns]
- **file2.go:N**: [What changed, why, and how it fits existing patterns]

## Related Changes
[Tests updated, configs modified, documentation changes]

## Verification
` + "```" + `bash
# Commands run to verify
` + "```" + `
[Results and interpretation]

## Edge Cases Considered
[What edge cases were checked and how they're handled]

## Summary
[Comprehensive summary of all changes and their impact]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildPlanPrompt() string {
	switch a.thoroughness {
	case tools.ThoroughnessQuick:
		return a.buildPlanPromptQuick()
	case tools.ThoroughnessThorough:
		return a.buildPlanPromptThorough()
	default:
		return a.buildPlanPromptNormal()
	}
}

func (a *Agent) buildPlanPromptQuick() string {
	return `═══════════════════════════════════════════════════════════════════════
                    PLAN AGENT (QUICK MODE, READ-ONLY)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Outline a high-level implementation plan. No deep analysis.
NOTE: You are READ-ONLY - you cannot modify files.

RULES:
- Skim key files, don't read everything.
- List files to change and rough steps. Skip testing/risk sections.
- Keep plan to 10-15 lines max.

PLAN FORMAT:
## Overview
[1-2 sentences]

## Files to Modify
1. **path/to/file.go** - [Brief change]

## Steps
1. [Step description]
2. [Step description]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildPlanPromptNormal() string {
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
- Identify dependencies between steps (which steps must complete before others)
- Mark steps that can be parallelized vs sequential
- Identify potential risks upfront

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildPlanPromptThorough() string {
	return `═══════════════════════════════════════════════════════════════════════
                  PLAN AGENT (THOROUGH MODE, READ-ONLY)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Design a comprehensive, production-ready implementation plan.
NOTE: You are READ-ONLY - you cannot modify files.

RULES:
- Read all relevant files thoroughly — implementations, tests, configs.
- Trace call chains end-to-end to understand data flow.
- Identify all touch points: code, tests, configs, documentation.
- Analyze multiple approaches and justify the chosen one.
- Map step dependencies and mark parallelizable work.

APPROACH:
1. Deep exploration of codebase patterns, conventions, and architecture
2. Identify ALL files that need modification (including tests, configs, docs)
3. Analyze multiple implementation approaches with trade-offs
4. Create detailed step-by-step plan with dependencies
5. Consider edge cases, migration paths, and backward compatibility

PLAN FORMAT:
## Overview
[Description with context on why this change is needed]

## Current Architecture
[How the existing code works in the relevant area]

## Approach Analysis
### Option A: [Name]
- Pros: [...]
- Cons: [...]

### Option B: [Name]
- Pros: [...]
- Cons: [...]

**Chosen:** [Option] because [rationale]

## Files to Modify
1. **path/to/file.go:N** - [Detailed change description]
2. **path/to/other.go:N** - [Detailed change description]
3. **path/to/test.go** - [Test updates needed]

## Implementation Steps
### Step 1: [Title] (sequential)
- [ ] Task 1.1 — [Details with file:line references]
- [ ] Task 1.2 — [Details]

### Step 2: [Title] (can parallelize with Step 3)
- [ ] Task 2.1 — [Details]

### Step 3: [Title] (can parallelize with Step 2)
- [ ] Task 3.1 — [Details]

## Testing Strategy
- Unit tests: [specific test functions to add/modify]
- Integration tests: [specific flows to verify]
- Manual verification: [steps to test manually]

## Edge Cases & Error Handling
- [Edge case 1]: How it's handled
- [Edge case 2]: How it's handled

## Risks & Mitigations
- [Risk 1]: [Mitigation strategy]
- [Risk 2]: [Mitigation strategy]

## Migration / Backward Compatibility
[Any migration steps needed, backward compat considerations]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGuidePrompt() string {
	switch a.thoroughness {
	case tools.ThoroughnessQuick:
		return a.buildGuidePromptQuick()
	case tools.ThoroughnessThorough:
		return a.buildGuidePromptThorough()
	default:
		return a.buildGuidePromptNormal()
	}
}

func (a *Agent) buildGuidePromptQuick() string {
	return `═══════════════════════════════════════════════════════════════════════
                    GUIDE AGENT (QUICK MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Answer the question about Gokin CLI briefly and directly.

RULES:
- Direct answer only. No detailed exploration.
- Skip Details and Related Information sections.
- One example max, only if essential.

RESPONSE FORMAT:
## Answer
[Direct answer in 1-3 sentences]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGuidePromptNormal() string {
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

func (a *Agent) buildGuidePromptThorough() string {
	return `═══════════════════════════════════════════════════════════════════════
                  GUIDE AGENT (THOROUGH MODE)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Provide a comprehensive answer about Gokin CLI with full detail.

RULES:
- Search all relevant documentation, code, and configs.
- Verify information by reading actual implementations.
- Include multiple examples covering different use cases.
- Explain configuration options and their defaults.
- Cross-reference related features and how they interact.

APPROACH:
1. Search documentation and source code thoroughly
2. Verify claims by reading implementations
3. Provide clear explanations with multiple examples
4. Cover edge cases and common pitfalls
5. Include configuration reference

RESPONSE FORMAT:
## Answer
[Comprehensive answer with full context]

## How It Works
[Technical explanation of the implementation]

## Examples
` + "```" + `bash
# Basic usage
gokin [command] [options]
` + "```" + `

` + "```" + `bash
# Advanced usage
gokin [command] --flag [value]
` + "```" + `

## Configuration
` + "```" + `yaml
# Relevant config options with defaults
section:
  option: default_value  # Description
` + "```" + `

## Common Pitfalls
- [Pitfall 1]: How to avoid
- [Pitfall 2]: How to avoid

## Related Features
[Other features that interact with this, with cross-references]

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

	// Snapshot planning-related fields under stateMu to avoid races with setters
	a.stateMu.RLock()
	planningMode := a.planningMode
	planGoal := a.planGoal
	requireApproval := a.requireApproval
	onText := a.onText
	onInput := a.onInput
	onPlanApproved := a.onPlanApproved
	a.stateMu.RUnlock()

	// === Tree planning mode: Build plan tree if enabled ===
	if a.treePlanner != nil && planningMode {
		tree, err := a.treePlanner.BuildTree(ctx, prompt, planGoal)
		if err != nil {
			logging.Warn("failed to build plan tree, falling back to reactive mode", "error", err)
		} else {
			// activePlan is observed by progress/UI callers while executeLoop is
			// running. Publish it under the same lock used by every reader.
			a.stateMu.Lock()
			a.activePlan = tree
			a.stateMu.Unlock()
			if onText != nil {
				a.safeOnText(fmt.Sprintf("\n[Plan tree built: %d nodes, best path: %d steps]\n",
					tree.TotalNodes, len(tree.BestPath)))
			}

			// Notify plan approval callback for context compaction
			if onPlanApproved != nil {
				planSummary := a.treePlanner.GeneratePlanSummary(tree)
				invokeAgentPlanApproved(onPlanApproved, a.ID, planSummary)
			}

			// Set total steps to best path length using SetProgress for thread safety
			a.SetProgress(0, len(tree.BestPath), "Building plan...")

			// === Interactive Plan Review ===
			if onInput != nil && requireApproval {
				if err := a.requestPlanApproval(ctx, tree); err != nil {
					return a.history, output.String(), err
				}
			} else if onText != nil {
				// Show plan tree even if approval not required
				a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
			}
		}
		if budgetErr := invocationBudgetTerminalError(ctx); budgetErr != nil {
			return a.finishInvocationBudgetFailure(nil, output, budgetErr)
		}
	}

	loopRecoveryTurns := 0
	replanAttempts := 0
	noProgressTurns := 0
	repeatedPlanRecoveries := 0 // Per-category budget: max 2
	noProgressRecoveries := 0   // Per-category budget: max 2
	verifyNudged := false       // verify-before-done nudge fires at most once
	doneGateFixes := 0
	doneGateCheckedAtGen := -1
	claimCorrectionInjected := false
	lastCallPlanFingerprint := ""
	samePlanTurns := 0
	lastTextFingerprint := ""
	seenFailureFingerprints := make(map[string]struct{})
	// truncationContinuations bounds how many times a max_tokens-truncated TEXT
	// response is auto-continued before giving up — mirrors the foreground
	// executor fix so /loop and delegated agents resume mid-task instead of
	// stopping when the model runs out of output room. The budget + continuation
	// prompt are shared with the executor (tools/max_tokens.go, Tier-4 slice 2).
	truncationContinuations := 0
	const maxTruncationContinuations = tools.MaxTruncationContinuations
	// Incomplete-work continuation (tools/incomplete_work.go, mirrors the
	// executor): keep the sub-agent going when it stops with no tool calls but
	// its OWN (per-agent isolated) todo list is unfinished. Progress-aware via
	// the tool count, bounded by tools.MaxIncompleteWorkContinuations.
	incompleteWorkStuck := 0
	toolsUsedAtLastIncompleteNudge := -1

	// API retry state — agents have no outer retry layer (unlike executor+message_processor),
	// so we handle retries here to survive transient API errors (rate limits, timeouts, 500s).
	streamRetryPolicy := client.DefaultStreamRetryPolicy()
	streamRetryPolicy.MaxRetries = 3        // More generous than default (2) since agents do long work
	streamRetryPolicy.MaxPartialRetries = 2 // Allow partial stream retries
	var streamRetries, partialStreamRetries int
	// Provider overloads (GLM 1305 et al) are transient — wait them out with a
	// separate, patient budget instead of failing the sub-agent (which loses its
	// work). Counters reset on any successful round, so each round gets fresh
	// patience. The wait is ctx-cancellable and bounded by the policy's MaxTotal.
	overloadRetryPolicy := client.DefaultOverloadRetryPolicy()
	var overloadRetries int
	var overloadElapsed time.Duration
	var contextCompactAttempts int
	// Empty-after-tools retry budget — a transient empty 200 right after a
	// tool-results round is retried (side-effect-free) before the loop gives up.
	// Kept SEPARATE from streamRetries (the API-error budget) so the two can't
	// corrupt each other. Reset on any non-empty response.
	emptyAfterToolsRetries := 0

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
		if budgetErr := invocationBudgetTerminalError(ctx); budgetErr != nil {
			return a.finishInvocationBudgetFailure(nil, output, budgetErr)
		}

		// Inject any queued steering messages (MetaAgent stuck-interventions)
		// before this turn so the model actually acts on the nudge.
		a.drainSteers()

		// Auto-checkpoint if enabled
		a.maybeAutoCheckpoint()

		// Check tokens and summarize if needed to prevent context overflow.
		// We do this BEFORE getting model response to ensure we have room.
		if a.tokenCounter != nil && a.summarizer != nil && a.ctxCfg != nil && a.ctxCfg.EnableAutoSummary {
			if err := a.checkAndSummarize(ctx); err != nil {
				logging.Warn("auto-summarization failed", "agent_id", a.ID, "error", err)
				a.safeOnText("\n[Warning: context optimization failed — conversation may hit length limits]\n")
			}
			if budgetErr := invocationBudgetTerminalError(ctx); budgetErr != nil {
				return a.finishInvocationBudgetFailure(nil, output, budgetErr)
			}
		}

		// Update progress at start of each turn
		a.stateMu.RLock()
		hasPlan := a.activePlan != nil
		a.stateMu.RUnlock()
		if !hasPlan {
			a.SetProgress(i+1, a.maxTurns, fmt.Sprintf("Turn %d: Executing tools", i+1))
		}

		// === Planned mode: Execute from plan tree ===
		// Snapshot activePlan under stateMu to avoid races with external readers
		a.stateMu.RLock()
		planTree := a.activePlan
		a.stateMu.RUnlock()

		if planTree != nil {
			actions, err := a.treePlanner.GetReadyActions(planTree)
			if err != nil {
				// No more actions in plan, check if completed
				a.safeOnText("\n[Plan completed or no more actions available]\n")
				a.stateMu.Lock()
				a.lastPlanTree = planTree
				a.activePlan = nil // Exit planned mode
				a.stateMu.Unlock()
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
						var result *AgentResult
						defer func() {
							if r := recover(); r != nil {
								logging.Error("panic in parallel plan execution", "action", action.Type, "panic", r)
								result = &AgentResult{
									AgentID:   a.ID,
									Type:      action.AgentType,
									Status:    AgentStatusFailed,
									Error:     fmt.Sprintf("planned action panicked: %v", r),
									Completed: true,
								}
							}
							if result == nil {
								result = &AgentResult{
									AgentID:   a.ID,
									Type:      action.AgentType,
									Status:    AgentStatusFailed,
									Error:     "planned action returned no result",
									Completed: true,
								}
							}

							// Always settle the node, including panic paths. Leaving it in
							// Executing permanently blocks every dependent action.
							if err := a.treePlanner.RecordResult(planTree, action.NodeID, result); err != nil {
								logging.Warn("failed to record plan result", "error", err)
							}
							resMu.Lock()
							results = append(results, parallelResult{action, result})
							resMu.Unlock()
						}()

						a.safeOnText(fmt.Sprintf("\n[Executing planned step: %s %s]\n",
							action.Type, action.AgentType))

						result = a.executePlannedAction(ctx, action)
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
					if a.treePlanner.ShouldReplan(planTree, firstFailure.result) && replanAttempts < 3 {
						replanAttempts++

						// Build replan context with reflection — may invoke LLM (up to 30s)
						var reflection *Reflection
						if a.reflector != nil && firstFailure.action.ToolName != "" {
							a.safeOnText(fmt.Sprintf("\n[Analyzing %s failure...]\n", firstFailure.action.ToolName))
							reflection = a.reflector.Reflect(ctx, firstFailure.action.ToolName, firstFailure.action.ToolArgs, firstFailure.result.Error)
						}

						// Find the node in the tree for replanning
						node, nodeFound := planTree.GetNode(firstFailure.action.NodeID)
						if !nodeFound || node == nil {
							logging.Warn("failed node not found in tree, switching to reactive mode",
								"node_id", firstFailure.action.NodeID)
							a.stateMu.Lock()
							a.lastPlanTree = planTree
							a.activePlan = nil
							a.stateMu.Unlock()
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

						if err := a.treePlanner.Replan(ctx, planTree, replanCtx); err != nil {
							logging.Warn("replan failed", "error", err)
							a.stateMu.Lock()
							a.lastPlanTree = planTree
							a.activePlan = nil // Exit planned mode on replan failure
							a.stateMu.Unlock()
						}
					} else {
						// Max replans exceeded or should not replan
						a.safeOnText("\n[Plan failed, switching to reactive mode]\n")
						a.stateMu.Lock()
						a.lastPlanTree = planTree
						a.activePlan = nil
						a.stateMu.Unlock()
					}
				}
				continue
			} else {
				// No actions and no error — check if plan is stalled or genuinely complete
				blocked := a.treePlanner.GetBlockedNodes(planTree)
				if len(blocked) > 0 {
					// Plan stalled: pending steps exist but can't proceed
					var msg strings.Builder
					msg.WriteString("\n[Plan Execution Stalled — blocked steps cannot proceed]\n")
					for _, b := range blocked {
						stepLabel := b.Node.ID
						if b.Node.Action != nil && b.Node.Action.Prompt != "" {
							stepLabel = b.Node.Action.Prompt
						}
						fmt.Fprintf(&msg, "  • %s: %s\n", stepLabel, b.Reason)
					}
					msg.WriteString("[Switching to reactive mode]\n")
					a.safeOnText(msg.String())
				} else {
					a.safeOnText("\n[Plan completed]\n")
				}
				a.stateMu.Lock()
				a.lastPlanTree = planTree
				a.activePlan = nil
				a.stateMu.Unlock()
			}
		}

		// === Reactive mode: Get response from model with retry ===
		a.onThinkingMu.Lock()
		a.Thought = "" // New turn, reset thought
		a.onThinkingMu.Unlock()

		var resp *client.Response
		for {
			var apiErr error
			resp, apiErr = a.getModelResponse(ctx)
			if apiErr == nil {
				// Success — reset retry counters
				streamRetries = 0
				partialStreamRetries = 0
				overloadRetries = 0
				overloadElapsed = 0
				break
			}

			// Preserve usage metadata delivered before a partial/terminal stream
			// error. The provider bills that work even though no final answer was
			// produced; successful responses are accounted later on the normal path.
			a.stateMu.Lock()
			a.recordResponseUsageLocked(resp)
			a.stateMu.Unlock()

			if errors.Is(apiErr, tools.ErrBudgetExceeded) ||
				errors.Is(apiErr, tools.ErrCostUnavailable) {
				return a.finishInvocationBudgetFailure(resp, output, apiErr)
			}

			// Context cancelled (Esc pressed) — return immediately
			if ctx.Err() != nil {
				return a.history, output.String(), ctx.Err()
			}

			// Context too long — try compacting history before retry
			if client.IsContextTooLongError(apiErr) && contextCompactAttempts < 2 {
				contextCompactAttempts++
				freed := a.pruneToolOutputs(a.pruneProtectChars / 2)
				if freed > 0 {
					logging.Info("agent compacted history after context-too-long error",
						"agent_id", a.ID, "freed_chars", freed, "attempt", contextCompactAttempts)
					a.safeOnText(fmt.Sprintf("\n[Context too long — compacted %d chars, retrying...]\n", freed))
					continue // retry immediately after compaction
				}
			}

			ft := client.DetectFailureTelemetry(apiErr)
			logging.Warn("agent model response failed",
				"agent_id", a.ID,
				"reason", ft.Reason,
				"partial", ft.Partial,
				"provider", ft.Provider,
				"retry_count", streamRetries,
				"partial_retry_count", partialStreamRetries,
				"error", apiErr)

			// Overload errors (GLM 1305 et al) take the patient budget so a busy
			// provider doesn't kill the sub-agent mid-task; everything else uses
			// the normal fast-fail stream policy.
			overload := client.IsOverloadError(apiErr)
			var decision client.StreamRetryDecision
			if overload {
				decision = client.DecideOverloadRetry(overloadRetryPolicy, apiErr, overloadRetries, overloadElapsed, ctx)
			} else {
				decision = client.DecideStreamRetry(
					streamRetryPolicy,
					apiErr,
					streamRetries,
					partialStreamRetries,
					ctx,
					client.StreamRetryOptions{AllowPartial: true},
				)
			}

			if !decision.ShouldRetry {
				// If the client supports failover, reset its position so the NEXT
				// agent-level retry (if any) starts from the first provider again.
				client.ResetClientFallback(a.client)

				// Preserve partial response in history before failing
				if resp != nil {
					if parts := a.buildResponseParts(resp); len(parts) > 0 {
						a.stateMu.Lock()
						a.history = append(a.history, &genai.Content{
							Role:  genai.RoleModel,
							Parts: parts,
						})
						a.stateMu.Unlock()
					}
					// Streaming callbacks already exposed this text live. Keep the
					// same partial output in AgentResult too so task_output, callers,
					// and resumptions do not observe an empty failed result.
					output.WriteString(resp.Text)
				}
				return a.history, output.String(), fmt.Errorf("model response error: %w", apiErr)
			}

			if overload {
				overloadRetries++
				overloadElapsed += decision.Delay
			} else if decision.Partial {
				partialStreamRetries++
			} else {
				streamRetries++
			}

			if overload {
				a.safeOnText(fmt.Sprintf("\n[Provider overloaded — waiting %s, retry %d (will keep trying)...]\n",
					decision.Delay.Round(time.Second), overloadRetries))
			} else {
				a.safeOnText(fmt.Sprintf("\n[API error (%s), retrying %d/%d in %s...]\n",
					decision.Reason,
					streamRetries, streamRetryPolicy.MaxRetries,
					decision.Delay.Round(time.Second)))
			}

			// Wait with context cancellation support
			retryTimer := time.NewTimer(decision.Delay)
			select {
			case <-retryTimer.C:
			case <-ctx.Done():
				retryTimer.Stop()
				return a.history, output.String(), ctx.Err()
			}
		}

		// Check cancellation after model response (Esc may have been pressed during streaming)
		select {
		case <-ctx.Done():
			return a.history, output.String(), ctx.Err()
		default:
		}

		// Text-based tool-call fallback for models WITHOUT native function-calling
		// (Ollama models with !SupportsTools, prompted to emit {"tool":…,"args":…}
		// JSON via ToolCallFallbackPrompt). The foreground executor already does
		// this; the sub-agent loop MUST mirror it or such a model's tool calls stay
		// inert text and never run — the "sub-agent loops, shows a tool, no
		// meaningful output" failure. Runs BEFORE the response is recorded so
		// history captures the tool call (not the JSON) and the empty-after-tools /
		// max_tokens / function-call branches below see the parsed calls.
		if n := client.ApplyTextToolCallFallback(a.client, resp); n > 0 {
			logging.Info("agent fallback: parsed tool calls from text",
				"agent_id", a.ID, "model", a.Model, "count", n)
		}

		// Add model response to history (protected by mutex)
		modelContent := &genai.Content{
			Role:  genai.RoleModel,
			Parts: a.buildResponseParts(resp),
		}
		a.stateMu.Lock()
		a.history = append(a.history, modelContent)
		a.recordResponseUsageLocked(resp)
		a.stateMu.Unlock()

		turnMadeProgress := false

		// Accumulate text output (already streamed to UI via collectStream callbacks)
		if resp.Text != "" {
			output.WriteString(resp.Text)

			textFingerprint := normalizeProgressFingerprint(resp.Text)
			if textFingerprint != "" && textFingerprint != lastTextFingerprint {
				turnMadeProgress = true
			}
			if textFingerprint != "" {
				lastTextFingerprint = textFingerprint
			}
		}

		// Empty response (no text, no tool calls) right after a tool-results round
		// is a transient empty 200 — retry the SAME results (side-effect-free, no
		// tool re-execution) before giving up, mirroring the foreground executor's
		// empty-after-tools branch. Bounded; on exhaustion we fall through to the
		// normal break (the agent has NO outer retry layer — returning an error
		// here would fail the whole sub-agent, the opposite of the goal). max_tokens
		// is excluded (a truncation, handled by the block just below).
		if resp.Text != "" || len(resp.FunctionCalls) > 0 {
			emptyAfterToolsRetries = 0 // progress — reset the empty-retry budget
		} else if emptyAfterToolsRetries < streamRetryPolicy.MaxRetries && shouldRetryEmptyAfterTools(a.history, resp) {
			delay := client.CalculateBackoff(streamRetryPolicy.BaseDelay, emptyAfterToolsRetries, streamRetryPolicy.MaxDelay)
			emptyAfterToolsRetries++
			logging.Warn("agent empty response after tool results — retrying",
				"agent_id", a.ID, "agent_type", a.Type, "model", a.Model,
				"retry", emptyAfterToolsRetries, "max", streamRetryPolicy.MaxRetries)
			a.safeOnText(fmt.Sprintf("\n[Model returned empty after tool results — retrying %d/%d in %s...]\n",
				emptyAfterToolsRetries, streamRetryPolicy.MaxRetries, delay.Round(time.Second)))
			// Pop the empty placeholder model turn so getModelResponse re-sees the
			// tool-results turn and re-issues SendFunctionResponse (the true mirror
			// of the executor's re-send) rather than a "Continue." text message.
			a.popEmptyModelPlaceholder()
			retryTimer := time.NewTimer(delay)
			select {
			case <-retryTimer.C:
			case <-ctx.Done():
				retryTimer.Stop()
				return a.history, output.String(), ctx.Err()
			}
			continue
		}

		// Auto-continue a TEXT response cut off by the output-token limit
		// instead of stopping mid-task (mirrors the foreground executor fix).
		// The partial text is already in `output` + a.history (appended above),
		// so just ask the model to continue and re-loop. Bounded by
		// maxTruncationContinuations. A max_tokens response WITH function calls
		// continues naturally through the tool path below, so it's untouched.
		if resp.FinishReason == genai.FinishReasonMaxTokens && len(resp.FunctionCalls) == 0 {
			if truncationContinuations < maxTruncationContinuations && resp.Text != "" {
				truncationContinuations++
				a.stateMu.Lock()
				a.history = append(a.history, genai.NewContentFromText(
					tools.TruncationContinuationPrompt,
					genai.RoleUser,
				))
				a.stateMu.Unlock()
				logging.Info("agent response truncated by max_tokens — auto-continuing",
					"agent_type", a.Type, "continuation", truncationContinuations,
					"max_continuations", maxTruncationContinuations)
				continue
			}
			truncMsg := "\n\n⚠ Response truncated (max_tokens limit reached)."
			output.WriteString(truncMsg)
			a.safeOnText(truncMsg)
			logging.Warn("agent response truncated by max_tokens limit (continuation budget exhausted)",
				"agent_type", a.Type, "output_tokens", resp.OutputTokens)
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

			planFingerprint := normalizeToolPlanFingerprint(resp.FunctionCalls)
			if planFingerprint != "" && planFingerprint == lastCallPlanFingerprint {
				samePlanTurns++
			} else {
				samePlanTurns = 1
				lastCallPlanFingerprint = planFingerprint
			}

			// Mental Loop Detection (exact args match + broad tool counter)
			loopDetectedThisTurn := false
			loopSkipReason := "Skipped: loop detected, try a different approach"

			// Plan-level loop: same tool-call plan repeated across turns.
			if samePlanTurns >= repeatedPlanTurnThreshold {
				loopDetectedThisTurn = true
				loopSkipReason = fmt.Sprintf("Skipped: repeated tool plan detected for %d turns", samePlanTurns)
				logging.Warn("repeated tool plan detected",
					"agent_id", a.ID,
					"turns", samePlanTurns,
					"plan", planFingerprint)
				if repeatedPlanRecoveries < 2 {
					repeatedPlanRecoveries++
					recoveryMsg := a.buildStagnationRecoveryIntervention(
						repeatedPlanRecoveries,
						fmt.Sprintf("repeated tool plan (%d turns)", samePlanTurns),
					)
					a.safeOnText(fmt.Sprintf("\n[Execution stagnation detected — recovery #%d]\n", repeatedPlanRecoveries))
					a.stateMu.Lock()
					a.history = append(a.history, genai.NewContentFromText(recoveryMsg, genai.RoleUser))
					if loopRecoveryTurns < 3 && effectiveMaxTurns < MaxTurnLimit {
						loopRecoveryTurns++
						effectiveMaxTurns++
					}
					a.stateMu.Unlock()
					noProgressTurns = 0
				} else {
					// Recovery budget exhausted — the model keeps re-issuing the
					// IDENTICAL tool plan. Without this, the loop kept setting the
					// flag and skipping the batch every turn (the skip path doesn't
					// bump noProgressTurns), burning the rest of effectiveMaxTurns on
					// dead skip-and-retry rounds — each a real API call. Fail fast
					// with the partial output + an honest error, mirroring the
					// no-progress watchdog and the foreground executor's contract.
					return a.history, output.String(), fmt.Errorf(
						"execution stalled: repeated tool plan for %d turns (recovery budget exhausted)", samePlanTurns)
				}
			}

			for _, fc := range resp.FunctionCalls {
				if loopDetectedThisTurn {
					break
				}
				key := normalizeCallKey(fc.Name, fc.Args)
				broadKey := "tool:" + fc.Name

				a.callHistoryMu.Lock()
				a.callHistory[key]++
				a.callHistory[broadKey]++
				exactCount := a.callHistory[key]
				broadCount := a.callHistory[broadKey]
				intervened := a.loopIntervened
				// Distinct-target coverage (read/grep): record under the same lock
				// and snapshot the target's no-progress redundancy for the trip
				// check. Any OTHER tool call is a progress signal that breaks
				// every redundancy chain — a model interleaving edits/bash/glob
				// is not in a tight read/grep rotation.
				trackCoverage := fc.Name == "read" || fc.Name == "grep"
				coverageRedundancy := 0
				if trackCoverage {
					coverageRedundancy = a.recordCoverageLocked(fc.Name, fc.Args)
				} else {
					a.noteCoverageProgressLocked()
				}
				// Early warnings: one repetition before intervention kicks in. Fire
				// each warning at most once per key so we don't spam the user.
				earlyExactWarn := exactCount == 3 && !intervened && !a.loopEarlyWarned["exact:"+key]
				earlyBroadWarn := broadCount == a.broadLoopThreshold(fc.Name) && !intervened && !a.loopEarlyWarned["broad:"+broadKey]
				if earlyExactWarn {
					a.loopEarlyWarned["exact:"+key] = true
				}
				if earlyBroadWarn {
					a.loopEarlyWarned["broad:"+broadKey] = true
				}
				a.callHistoryMu.Unlock()

				if earlyExactWarn {
					a.safeOnText(fmt.Sprintf("\n[Heads up: %s called 3× with same args — one more will trigger loop recovery. Press ESC to intervene.]\n", fc.Name))
				}
				if earlyBroadWarn {
					a.safeOnText(fmt.Sprintf("\n[Heads up: %s used %d× this session — approaching broad-loop threshold. Press ESC to intervene.]\n", fc.Name, broadCount))
				}

				// Exact-match loop: same tool + same (normalized) args > 3 times
				if exactCount > 3 && !intervened {
					loopDetectedThisTurn = true
					logging.Warn("mental loop detected (exact)", "tool", fc.Name, "count", exactCount, "model", a.Model)
					a.interveneOnLoop(
						fmt.Sprintf("\n[Loop detected: %s called %d times with same args — intervening]\n", fc.Name, exactCount),
						func() string { return a.buildLoopRecoveryIntervention(fc.Name, fc.Args, exactCount) },
						func() { delete(a.callHistory, key) }, // fresh exact window after recovery
						&loopRecoveryTurns, &effectiveMaxTurns)
					continue
				}

				// Broad loop: same tool called > broad threshold times (any args).
				// Threshold is tool-class-aware — read-only exploration tools get a
				// higher ceiling so reading many distinct files isn't a false loop.
				if broadCount > a.broadLoopThreshold(fc.Name) && !intervened {
					loopDetectedThisTurn = true
					logging.Warn("broad loop detected", "tool", fc.Name, "total_calls", broadCount, "model", a.Model)
					a.interveneOnLoop(
						fmt.Sprintf("\n[Broad loop: %s used %d times — try a different approach]\n", fc.Name, broadCount),
						func() string {
							return fmt.Sprintf(
								"STOP. I've called `%s` %d times total in this session. "+
									"This strongly suggests I'm stuck. I need to:\n"+
									"1. Step back and reconsider my overall approach\n"+
									"2. Try a completely different tool or strategy\n"+
									"3. Summarize what I've learned so far and proceed differently\n",
								fc.Name, broadCount)
						},
						nil,
						&loopRecoveryTurns, &effectiveMaxTurns)
					continue
				}

				// Distinct-target coverage loop: read/grep re-covering ground it
				// already has (drifting offset / rotating pattern over the same
				// target) with zero progress in between. Escapes exact-args,
				// plan-repetition, and no-progress; the broad ceiling only catches
				// it far later. Graceful hint, not abort — mirrors the broad
				// branch + stagnation-recovery contract.
				if trackCoverage && coverageRedundancy >= a.redundancyBudget(fc.Name) && !intervened {
					loopDetectedThisTurn = true
					logging.Warn("re-coverage loop detected", "tool", fc.Name, "redundant_revisits", coverageRedundancy, "model", a.Model)
					var coverageAttempt int
					a.interveneOnLoop(
						fmt.Sprintf("\n[Re-coverage loop: %s keeps revisiting the same ground — narrowing approach]\n", fc.Name),
						func() string {
							return a.buildStagnationRecoveryIntervention(coverageAttempt, fmt.Sprintf("%s re-covering already-seen ground (%d redundant revisits)", fc.Name, coverageRedundancy))
						},
						func() {
							// Fresh window after recovery: wipe coverage so ghost
							// spans recorded for skipped/never-executed calls can't
							// poison the post-intervention retry. gen survives
							// (ResetTargets) — matches the executor's resetWindow.
							a.coverage.ResetTargets()
							a.coverageTrips++
							coverageAttempt = min(a.coverageTrips, 2)
						},
						&loopRecoveryTurns, &effectiveMaxTurns)
					continue
				}
			}

			// Reset intervention flag after several consecutive non-loop turns.
			// Requiring 3 clean turns prevents the model from oscillating
			// between "loop detected → one different call → resume looping".
			if !loopDetectedThisTurn {
				a.callHistoryMu.Lock()
				if a.loopIntervened {
					a.loopCooldown++
					if a.loopCooldown >= 3 {
						a.loopIntervened = false
						a.loopCooldown = 0
					}
				}
				a.callHistoryMu.Unlock()
			} else {
				// Loop was detected — skip real tool execution for this turn.
				// Synthesize dummy function responses so the API protocol
				// (function calls must be followed by function responses) is satisfied.
				var dummyParts []*genai.Part
				for _, fc := range resp.FunctionCalls {
					part := genai.NewPartFromFunctionResponse(fc.Name, map[string]any{
						"error": loopSkipReason,
					})
					part.FunctionResponse.ID = fc.ID
					dummyParts = append(dummyParts, part)
				}
				a.stateMu.Lock()
				a.history = append(a.history, &genai.Content{
					Role:  genai.RoleUser,
					Parts: dummyParts,
				})
				a.stateMu.Unlock()
				// Loop-skipped turns are NOT counted as no-progress: the loop
				// detection system handles this pathology with its own
				// intervention. Counting them here causes premature stagnation
				// exits when the agent's own recovery mechanisms are working.
				continue
			}

			// Update progress to show tool execution
			toolsList := make([]string, 0, len(resp.FunctionCalls))
			for _, fc := range resp.FunctionCalls {
				toolsList = append(toolsList, fc.Name)
			}
			a.SetProgress(i+1, a.maxTurns, fmt.Sprintf("Executing tools: %v", toolsList))

			if budgetErr := invocationBudgetTerminalError(ctx); budgetErr != nil {
				return a.finishInvocationBudgetFailure(resp, output, budgetErr)
			}
			results := a.executeTools(ctx, resp.FunctionCalls)

			successCount, newFailureSignals, repeatedFailureSignals := summarizeToolProgress(resp.FunctionCalls, results, seenFailureFingerprints)
			if successCount > 0 || newFailureSignals > 0 {
				turnMadeProgress = true
				noProgressTurns = 0
			} else {
				noProgressTurns++
			}

			if repeatedFailureSignals > 0 && newFailureSignals == 0 {
				logging.Debug("tool errors repeating without new signal",
					"agent_id", a.ID,
					"repeated_failures", repeatedFailureSignals,
					"no_progress_turns", noProgressTurns)
			}

			// Track file activity for relevance scoring
			if a.fileTracker != nil {
				a.stateMu.RLock()
				msgIdx := len(a.history)
				a.stateMu.RUnlock()
				for _, fc := range resp.FunctionCalls {
					a.fileTracker.RecordToolCall(fc.Name, fc.Args, msgIdx)
				}
			}

			// Add function response to history (with multimodal parts if present)
			// BEFORE honoring cancellation. The model's FunctionCall turn is
			// already durable at this point and tools may have crossed side-effect
			// boundaries. Returning first leaves an orphaned tool call in the
			// saved AgentState; a later resume then either loses the completed
			// outcome or gets a strict-provider 400 for the unpaired call.
			// Cancelled/abandoned reads already carry explicit placeholder
			// responses, while stateful tools are joined before executeTools
			// returns, so committing this complete response turn is safe.
			var funcParts []*genai.Part
			for _, result := range results {
				// Defense-in-depth: executeToolsParallel pre-populates every
				// slot so this should never be nil, but the consumer must
				// never assume it — the sibling executeDirectly (below)
				// guards the identical pattern for the same reason.
				if result.Response == nil {
					continue
				}
				part := genai.NewPartFromFunctionResponse(result.Response.Name, result.Response.Response)
				part.FunctionResponse.ID = result.Response.ID
				funcParts = append(funcParts, part)
				// Append inline image data so the LLM can "see" images
				for _, mp := range result.MultimodalData {
					funcParts = append(funcParts, genai.NewPartFromBytes(mp.Data, mp.MimeType))
				}
			}
			funcContent := &genai.Content{
				Role:  genai.RoleUser,
				Parts: funcParts,
			}
			a.stateMu.Lock()
			a.history = append(a.history, funcContent)
			a.stateMu.Unlock()

			// Esc may have been pressed during tool execution. Stop only after
			// the pair-complete history commit above so persistence/resume has an
			// exact record of what did (or did not) execute.
			select {
			case <-ctx.Done():
				return a.history, output.String(), ctx.Err()
			default:
			}

			// Detect permission denials and inject recovery guidance
			if permDeniedTools := detectPermissionDenials(results); len(permDeniedTools) > 0 {
				recoveryMsg := fmt.Sprintf(
					"Permission was denied for: %s. Do NOT retry these tools. Use a different approach or ask the user for guidance.",
					strings.Join(permDeniedTools, ", "))
				a.stateMu.Lock()
				a.history = append(a.history, genai.NewContentFromText(recoveryMsg, genai.RoleUser))
				a.stateMu.Unlock()
			}

			// Long-loop watchdog: deterministic recovery path for repeated no-progress turns.
			if !turnMadeProgress {
				if noProgressTurns >= stagnationTurnThreshold {
					if noProgressRecoveries < 2 {
						noProgressRecoveries++
						recoveryMsg := a.buildStagnationRecoveryIntervention(
							noProgressRecoveries,
							fmt.Sprintf("no meaningful progress for %d turns", noProgressTurns),
						)
						a.safeOnText(fmt.Sprintf("\n[No-progress streak detected — recovery #%d]\n", noProgressRecoveries))
						a.stateMu.Lock()
						a.history = append(a.history, genai.NewContentFromText(recoveryMsg, genai.RoleUser))
						if loopRecoveryTurns < 3 && effectiveMaxTurns < MaxTurnLimit {
							loopRecoveryTurns++
							effectiveMaxTurns++
						}
						a.stateMu.Unlock()
						noProgressTurns = 0
					} else {
						return a.history, output.String(), fmt.Errorf(
							"execution stalled: no progress for %d consecutive turns (recovery budget exhausted)",
							stagnationTurnThreshold,
						)
					}
				}
			}

			continue
		}

		// Incomplete-work continuation: the model stopped with no tool calls but
		// its OWN todo list still has unfinished items — keep it going instead of
		// finishing (the "narrates the next step then stops" failure). Checked
		// BEFORE the verify-nudge/done-gate: if declared work remains, finish it
		// before verifying intentionally-incomplete code. The agent's todo tool is
		// now isolated per-agent (clone.go), so this reads THIS agent's list, not
		// the foreground's. Bounded + progress-aware; skipped on max_tokens (its
		// own continuation handles that above).
		// actionMode=true: sub-agents are autonomous (the discuss-mode gate is a
		// foreground-interactive feature only — no human in this loop to discuss
		// with), so the incomplete-work nudge always applies here.
		incDec := tools.DecideIncompleteWorkContinuation(a.registry,
			resp.FinishReason == genai.FinishReasonMaxTokens, a.ToolsUsedCount(),
			toolsUsedAtLastIncompleteNudge, incompleteWorkStuck, true)
		incompleteWorkStuck = incDec.Stuck
		toolsUsedAtLastIncompleteNudge = incDec.LastNudge
		if incDec.Continue {
			a.stateMu.Lock()
			a.history = append(a.history, genai.NewContentFromText(
				tools.IncompleteWorkContinuationPrompt(incDec.Count, incDec.Summary), genai.RoleUser))
			a.stateMu.Unlock()
			logging.Info("incomplete-work continuation (sub-agent): model stopped with unfinished todos",
				"agent_type", a.Type, "incomplete", incDec.Count, "attempt", incompleteWorkStuck)
			continue
		}
		if incDec.Exhausted {
			logging.Info("incomplete-work continuation budget exhausted (sub-agent)",
				"agent_type", a.Type, "incomplete", incDec.Count)
		}

		// Verify-before-done (sub-agent analogue of the foreground done-gate):
		// if the agent changed code but never ran any verification command this
		// run, nudge it once to build/test before finishing. Conservative — it
		// won't nag an agent that ran any command, so false nudges are rare.
		if !verifyNudged && a.needsVerificationNudge() {
			verifyNudged = true
			a.appendVerificationNudge()
			continue
		}

		gateOutcome := a.runDoneGateAtBreak(ctx, prompt, output, &doneGateFixes, &doneGateCheckedAtGen)
		if gateOutcome == agentGateFixInjected {
			continue
		}
		// On agentGateExhausted the failure marker is already appended; fall
		// through to finish. Skipping the claim-correction branch here is what
		// prevents a second loop iteration from re-running the gate and writing
		// a DUPLICATE exhaustion marker.
		if gateOutcome != agentGateExhausted && !claimCorrectionInjected && a.injectClaimCorrectionIfNeeded(resp.Text) {
			claimCorrectionInjected = true
			continue
		}

		// No more function calls, we're done
		if turnMadeProgress {
			noProgressTurns = 0
		}
		break
	}

	// Notify user if the model produced no output
	if output.Len() == 0 {
		emptyMsg := "\n[Model returned an empty response — try rephrasing your request]\n"
		output.WriteString(emptyMsg)
		a.safeOnText(emptyMsg)
	}

	// Genuine turn exhaustion: the loop fell out via its `for i < effectiveMaxTurns`
	// condition WITHOUT ever hitting the "no more function calls, we're done"
	// break (every normal completion breaks with i < effectiveMaxTurns). Returning
	// nil here misclassified a cut-short runaway as SUCCESS — for a /loop iteration
	// that meant ConsecutiveFailures reset every cutoff, so a task that can't fit in
	// the turn budget "succeeds" forever, silently burning quota with no auto-pause.
	// Surface it as an error: agent.Run preserves the partial output (result.Output)
	// on the failure path, and the loop adapter then classifies it as a (non-
	// transient) task failure so the auto-pause breaker can do its job.
	if i >= effectiveMaxTurns {
		a.safeOnText("\n[Reached maximum turn limit — stopping]\n")
		return a.history, output.String(), fmt.Errorf("reached maximum turn limit (%d turns)", effectiveMaxTurns)
	}

	return a.history, output.String(), nil
}

// normalizeCallKey creates a stable key for loop detection by filtering out zero-value arguments.
// This catches semantic loops where arguments differ only in default/zero fields.
func normalizeCallKey(name string, args map[string]any) string {
	if len(args) == 0 {
		return name + ":{}"
	}
	keys := make([]string, 0, len(args))
	for k, v := range args {
		switch val := v.(type) {
		case string:
			if val == "" {
				continue
			}
		case float64:
			if val == 0 {
				continue
			}
		case bool:
			if !val {
				continue
			}
		case nil:
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	filtered := make([][2]any, 0, len(keys))
	for _, k := range keys {
		filtered = append(filtered, [2]any{k, args[k]})
	}
	argsJSON, _ := json.Marshal(filtered)
	return fmt.Sprintf("%s:%s", name, string(argsJSON))
}

func normalizeToolPlanFingerprint(calls []*genai.FunctionCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, fc := range calls {
		if fc == nil {
			continue
		}
		parts = append(parts, normalizeCallKey(fc.Name, fc.Args))
	}
	return strings.Join(parts, "|")
}

func normalizeProgressFingerprint(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 240 {
		text = string(runes[:240])
	}
	return text
}

func summarizeToolProgress(
	calls []*genai.FunctionCall,
	results []toolCallResult,
	seenFailureFingerprints map[string]struct{},
) (successCount, newFailureSignals, repeatedFailureSignals int) {
	if len(results) == 0 {
		return 0, 0, 0
	}

	for idx, result := range results {
		if result.Response == nil {
			continue
		}
		respMap := result.Response.Response
		success, _ := respMap["success"].(bool)
		if success {
			successCount++
			continue
		}

		toolName := result.Response.Name
		if idx < len(calls) && calls[idx] != nil && calls[idx].Name != "" {
			toolName = calls[idx].Name
		}
		errText := extractToolErrorFromMap(respMap)
		fp := normalizeProgressFingerprint(toolName + ":" + errText)
		if fp == "" {
			continue
		}
		if _, exists := seenFailureFingerprints[fp]; exists {
			repeatedFailureSignals++
			continue
		}
		seenFailureFingerprints[fp] = struct{}{}
		newFailureSignals++
	}

	return successCount, newFailureSignals, repeatedFailureSignals
}

// detectPermissionDenials checks tool call results for permission denials
// and returns the names of tools that were denied.
func detectPermissionDenials(results []toolCallResult) []string {
	var denied []string
	for _, result := range results {
		if result.Response == nil {
			continue
		}
		respMap := result.Response.Response
		if errText, ok := respMap["error"].(string); ok && strings.Contains(errText, "Permission denied") {
			denied = append(denied, result.Response.Name)
		}
	}
	return denied
}

func extractToolErrorFromMap(resp map[string]any) string {
	if resp == nil {
		return ""
	}
	if errText, ok := resp["error"].(string); ok && strings.TrimSpace(errText) != "" {
		return errText
	}
	if content, ok := resp["content"].(string); ok && strings.TrimSpace(content) != "" {
		return content
	}
	return ""
}

func recoveryAttemptKey(name string, args map[string]any, category, alternative string) string {
	base := normalizeCallKey(name, args)
	category = strings.TrimSpace(strings.ToLower(category))
	alternative = strings.TrimSpace(strings.ToLower(alternative))
	if category == "" {
		category = "unknown"
	}
	if alternative == "" {
		return fmt.Sprintf("%s|%s", base, category)
	}
	return fmt.Sprintf("%s|%s|alt=%s", base, category, alternative)
}

// buildLoopRecoveryIntervention creates a reflection-based intervention message for mental loop recovery.
// This helps the agent understand what went wrong and suggests alternative approaches.
// interveneOnLoop applies the shared loop-intervention contract for all three
// in-loop detectors (exact-args / broad / re-coverage):
//  1. under callHistoryMu: set loopIntervened, zero loopCooldown, run the
//     detector's extraLocked state reset (may be nil);
//  2. surface userMsg via safeOnText;
//  3. build the intervention (after the lock — builders may need values the
//     extraLocked closure computed) and append it to history as a user turn;
//  4. grant a bounded recovery turn (≤3 per request, capped at MaxTurnLimit).
//
// Every detector MUST go through this helper. A branch with its own copy of
// the skeleton drifts — e.g. forgetting loopCooldown=0 silently breaks the
// 3-clean-turns cooldown for that detector only (this was three verbatim
// copies before extraction; the review flagged the drift risk).
func (a *Agent) interveneOnLoop(userMsg string, buildIntervention func() string, extraLocked func(), loopRecoveryTurns, effectiveMaxTurns *int) {
	a.callHistoryMu.Lock()
	a.loopIntervened = true
	a.loopCooldown = 0
	if extraLocked != nil {
		extraLocked()
	}
	a.callHistoryMu.Unlock()

	a.safeOnText(userMsg)

	intervention := buildIntervention()

	a.stateMu.Lock()
	a.history = append(a.history, genai.NewContentFromText(intervention, genai.RoleUser))
	if *loopRecoveryTurns < 3 && *effectiveMaxTurns < MaxTurnLimit {
		*loopRecoveryTurns++
		*effectiveMaxTurns++
	}
	a.stateMu.Unlock()
}

func (a *Agent) buildLoopRecoveryIntervention(toolName string, args map[string]any, count int) string {
	var sb strings.Builder

	sb.WriteString("STOP. I've detected that I'm stuck in a loop.\n\n")
	sb.WriteString("**What I was doing:**\n")
	fmt.Fprintf(&sb, "- Calling `%s` with the same arguments %d times\n", toolName, count)

	// Extract key arguments for context
	if args != nil {
		if path, ok := args["path"].(string); ok {
			fmt.Fprintf(&sb, "- Path: `%s`\n", path)
		}
		if pattern, ok := args["pattern"].(string); ok {
			fmt.Fprintf(&sb, "- Pattern: `%s`\n", pattern)
		}
		if cmd, ok := args["command"].(string); ok {
			fmt.Fprintf(&sb, "- Command: `%s`\n", cmd)
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

func (a *Agent) buildStagnationRecoveryIntervention(attempt int, reason string) string {
	var sb strings.Builder

	sb.WriteString("EXECUTION WATCHDOG: progress has stalled.\n\n")
	fmt.Fprintf(&sb, "Recovery attempt: %d/2\n", attempt)
	if strings.TrimSpace(reason) != "" {
		sb.WriteString("Observed issue: ")
		sb.WriteString(reason)
		sb.WriteString("\n")
	}
	sb.WriteString("\nYou MUST change strategy now:\n")
	sb.WriteString("1. Do not repeat the previous tool plan.\n")
	sb.WriteString("2. Pick a different first tool to gather missing evidence.\n")
	sb.WriteString("3. Propose a 2-3 step micro-plan, then execute step 1 immediately.\n")
	sb.WriteString("4. If blocked, produce a concrete fallback path instead of retrying blindly.\n")
	sb.WriteString("5. Keep responses concise and evidence-based.\n")

	return sb.String()
}

// checkAndSummarize monitors token usage and triggers summarization if thresholds are met.
// updateTokenGrowthAndProjectOverflow maintains an EMA of tokens-added-per-turn
// and reports whether the session is projected to cross the compaction
// threshold within the next ~3 turns at the current growth rate. Pre-emptive
// compaction lets us run the (slow) summarization between turns instead of
// catching the user mid-stream when tokens actually exceed the limit.
//
// Returns true only once we have enough samples (≥ 3) to trust the EMA — on
// first few turns we fall back to the plain percent threshold check above.
func (a *Agent) updateTokenGrowthAndProjectOverflow(tokenCount, maxTokens int, threshold float64) bool {
	if maxTokens <= 0 {
		return false
	}
	a.preemptMu.Lock()
	defer a.preemptMu.Unlock()

	delta := 0
	if a.lastTokenCount > 0 {
		delta = tokenCount - a.lastTokenCount
	}
	a.lastTokenCount = tokenCount

	if delta <= 0 {
		// Shrinking or flat (e.g. right after a compaction) — don't feed
		// negative samples into the EMA, just record the current state.
		return false
	}

	// Standard EMA with α = 0.3. Smooths spikes but stays responsive to
	// sustained growth pattern changes.
	const alpha = 0.3
	if a.tokenGrowthSample == 0 {
		a.tokenGrowthEMA = float64(delta)
	} else {
		a.tokenGrowthEMA = alpha*float64(delta) + (1-alpha)*a.tokenGrowthEMA
	}
	a.tokenGrowthSample++

	if a.tokenGrowthSample < 2 {
		return false
	}

	const lookAheadTurns = 3
	projected := float64(tokenCount) + a.tokenGrowthEMA*lookAheadTurns
	projectedPercent := projected / float64(maxTokens)
	if projectedPercent >= threshold {
		logging.Info("pre-emptive compaction triggered",
			"agent_id", a.ID,
			"current_pct", fmt.Sprintf("%.1f%%", float64(tokenCount)/float64(maxTokens)*100),
			"projected_pct", fmt.Sprintf("%.1f%%", projectedPercent*100),
			"ema_growth", int(a.tokenGrowthEMA),
			"look_ahead_turns", lookAheadTurns)
		return true
	}
	return false
}

func (a *Agent) checkAndSummarize(ctx context.Context) error {
	// 0. Check hard limit on history size to prevent memory exhaustion
	a.stateMu.RLock()
	historyLen := len(a.history)
	a.stateMu.RUnlock()

	if historyLen > a.maxHistorySize {
		logging.Warn("history size exceeded maxHistorySize, forcing compaction",
			"agent_id", a.ID, "history_len", historyLen, "max", a.maxHistorySize)
		return a.forceCompactHistory(ctx)
	}

	// 1. Snapshot history under read lock for safe concurrent access
	a.stateMu.RLock()
	historySnapshot := make([]*genai.Content, len(a.history))
	copy(historySnapshot, a.history)
	a.stateMu.RUnlock()

	// 2. Fast path: estimate tokens locally to avoid API call when clearly under threshold
	limits := a.tokenCounter.GetLimits()
	threshold := limits.WarningThreshold
	if threshold == 0 {
		threshold = 0.8
	}

	estimatedTokens := ctxmgr.EstimateContentsTokens(historySnapshot)
	estimatedPercent := float64(estimatedTokens) / float64(limits.MaxInputTokens)
	if estimatedPercent < threshold*0.85 {
		return nil // Clearly under threshold — skip API call
	}

	// Near threshold — use precise API counting, but skip if history hasn't
	// grown much since the last precise count showed we're under threshold.
	histLen := len(historySnapshot)
	a.preemptMu.Lock()
	cachedCount := a.cachedPreciseCount
	cachedLen := a.cachedPreciseHistLen
	a.preemptMu.Unlock()
	if cachedCount > 0 && cachedLen > 0 && histLen-cachedLen < 4 {
		cachedPercent := float64(cachedCount) / float64(limits.MaxInputTokens)
		if cachedPercent < threshold*0.9 {
			return nil // Recent precise count says we're safe
		}
	}

	countCtx, countCancel := a.withCompactionTimeout(ctx)
	tokenCount, err := a.tokenCounter.CountContents(countCtx, historySnapshot)
	countCancel()
	if err != nil {
		return fmt.Errorf("failed to count tokens: %w", err)
	}

	// Cache the precise count for future checks.
	a.preemptMu.Lock()
	a.cachedPreciseCount = tokenCount
	a.cachedPreciseHistLen = histLen
	a.preemptMu.Unlock()

	percentUsed := float64(tokenCount) / float64(limits.MaxInputTokens)

	// Pre-emptive compaction: update EMA of per-turn growth, then check if
	// we're likely to overflow within the next few turns at the current rate.
	// If so, compact NOW between turns instead of forcing it mid-stream.
	// Requires at least 3 observations so we don't react to startup noise.
	preempt := a.updateTokenGrowthAndProjectOverflow(tokenCount, limits.MaxInputTokens, threshold)

	if percentUsed < threshold && !preempt {
		return nil
	}

	// 2.5. Try pruning old tool outputs first (cheaper than full summarization)
	freedChars := a.pruneToolOutputs(a.pruneProtectChars)
	if freedChars > 0 {
		logging.Info("pruned old tool outputs", "agent_id", a.ID, "freed_chars", freedChars)
		// Re-check: maybe pruning was enough
		a.stateMu.RLock()
		newSnapshot := make([]*genai.Content, len(a.history))
		copy(newSnapshot, a.history)
		a.stateMu.RUnlock()
		pruneCtx, pruneCancel := a.withCompactionTimeout(ctx)
		newCount, countErr := a.tokenCounter.CountContents(pruneCtx, newSnapshot)
		pruneCancel()
		if countErr == nil {
			newPercent := float64(newCount) / float64(limits.MaxInputTokens)
			if newPercent < threshold {
				return nil // Pruning was sufficient
			}
			historySnapshot = newSnapshot // Use pruned snapshot for summarization
			historyLen = len(newSnapshot)
			tokenCount = newCount
			percentUsed = newPercent
		}
	}

	logging.Info("context threshold reached, compacting history",
		"agent_id", a.ID,
		"usage", fmt.Sprintf("%.1f%%", percentUsed*100),
		"tokens", tokenCount)
	compactStart := time.Now()
	tokensBefore := tokenCount
	a.safeOnText(fmt.Sprintf("\n[Compacting context (%.0f%% used, %d tokens)...]\n", percentUsed*100, tokenCount))

	// 3. Summarize on snapshot (potentially slow API call — no lock held)
	if len(historySnapshot) <= a.summarizeMinMsgs {
		return nil
	}

	// Preserve first 3 messages: system prompt [0], greeting [1], original task prompt [2].
	// Summarize from [3] onward so the agent never forgets its task.
	preserveStart := 3
	if len(historySnapshot) <= preserveStart+2 {
		return nil // Not enough messages to summarize
	}

	protectRecent := a.summarizeProtect
	if protectRecent >= len(historySnapshot)-preserveStart {
		protectRecent = len(historySnapshot) - preserveStart - 1
	}
	historyToSummarize := historySnapshot[preserveStart : len(historySnapshot)-protectRecent]
	recentFromSnapshot := historySnapshot[len(historySnapshot)-protectRecent:]

	sumCtx, sumCancel := a.withCompactionTimeout(ctx)
	summary, err := a.summarizer.Summarize(sumCtx, historyToSummarize)
	sumCancel()
	if err != nil {
		return fmt.Errorf("summarization failed: %w", err)
	}
	invokedSkills := a.invokedSkillSnapshot()

	// 4. Reconstruct under write lock, preserving messages added since snapshot.
	// We snapshot a copy of the new history while holding the lock so we can
	// do an expensive token recount (potentially an API call) without blocking
	// concurrent reads/writes on the agent state.
	var recountSnapshot []*genai.Content
	func() {
		a.stateMu.Lock()
		defer a.stateMu.Unlock()

		// If history shrunk (another compaction ran), skip
		if len(a.history) < historyLen {
			return
		}

		// Messages appended by concurrent goroutines since our snapshot
		newMessages := a.history[historyLen:]

		newHistory := make([]*genai.Content, 0, preserveStart+1+len(recentFromSnapshot)+len(newMessages))
		newHistory = append(newHistory, a.history[:preserveStart]...) // System + greeting + original task
		newHistory = append(newHistory, summary)
		newHistory = append(newHistory, recentFromSnapshot...)
		newHistory = append(newHistory, newMessages...)

		// Drop any FunctionResponse orphaned by the summary: a FunctionCall in the
		// summarized middle whose paired FunctionResponse landed in
		// recentFromSnapshot (a tool pair straddling the protectRecent boundary)
		// would otherwise serialize as a response with no matching call → strict
		// provider 400 "tool_call_ids did not have response messages" → the
		// sub-agent dies mid-task. The two forced-compaction siblings already do
		// this; the PRE-EMPTIVE path (the one sub-agents and /loop hit most) was
		// missing it. Allocation-free when there are no orphans.
		newHistory = ensureToolPairConsistency(newHistory)
		// Active skills are standing instructions, not disposable tool output.
		// Reattach one bounded ordinary-user carry block immediately after the
		// summary. ReattachInvocations suppresses the block when the latest raw
		// successful skill response is already among the retained messages.
		newHistory = skills.ReattachInvocations(newHistory, invokedSkills, preserveStart+1)

		a.history = newHistory

		// Inject continuation hint after compaction
		a.injectContinuationHint()

		logging.Info("context history compacted", "agent_id", a.ID, "new_message_count", len(a.history))

		// Shallow snapshot for the recount outside the lock. Contents are
		// immutable after they enter history, so sharing pointers is safe.
		recountSnapshot = make([]*genai.Content, len(a.history))
		copy(recountSnapshot, a.history)
	}()

	// Post-compaction feedback: report freed space and elapsed time so the user
	// understands why the model paused. Recount is best-effort (skipped on error).
	duration := time.Since(compactStart).Round(100 * time.Millisecond)
	if len(recountSnapshot) > 0 && a.tokenCounter != nil {
		recountCtx, recountCancel := a.withCompactionTimeout(ctx)
		newCount, cntErr := a.tokenCounter.CountContents(recountCtx, recountSnapshot)
		recountCancel()
		if cntErr == nil && tokensBefore > 0 && newCount >= 0 {
			freedPct := 0
			if newCount < tokensBefore {
				freedPct = 100 * (tokensBefore - newCount) / tokensBefore
			}
			a.safeOnText(fmt.Sprintf("[Context compacted: freed %d%% (%d→%d tokens) in %s]\n", freedPct, tokensBefore, newCount, duration))
		} else {
			a.safeOnText(fmt.Sprintf("[Context compacted in %s]\n", duration))
		}
	} else {
		a.safeOnText(fmt.Sprintf("[Context compacted in %s]\n", duration))
	}

	return nil
}

// forceCompactHistory compacts history when MaxHistorySize is exceeded.
// Tries LLM summarization first (preserves context); falls back to
// importance-based truncation if summarization fails.
func (a *Agent) forceCompactHistory(ctx context.Context) error {
	a.stateMu.RLock()
	histLen := len(a.history)
	a.stateMu.RUnlock()

	if histLen <= 10 {
		return nil
	}

	a.safeOnText(fmt.Sprintf("\n[Force-compacting history (%d messages)...]\n", histLen))

	// Try LLM summarization first — much better context preservation.
	if a.summarizer != nil {
		if err := a.forceCompactViaSummary(ctx); err == nil {
			return nil
		}
		logging.Warn("force-compact summarization failed, falling back to truncation", "agent_id", a.ID)
	}

	return a.forceCompactViaTruncation()
}

// forceCompactViaSummary uses the LLM summarizer to compress middle history.
func (a *Agent) forceCompactViaSummary(ctx context.Context) error {
	ctx, cancel := a.withCompactionTimeout(ctx)
	defer cancel()

	a.stateMu.RLock()
	if len(a.history) <= 10 {
		a.stateMu.RUnlock()
		return nil
	}
	preserveStart := 3
	preserveEnd := 6
	if len(a.history) <= preserveStart+preserveEnd+2 {
		a.stateMu.RUnlock()
		return nil
	}
	toSummarize := make([]*genai.Content, len(a.history)-preserveStart-preserveEnd)
	copy(toSummarize, a.history[preserveStart:len(a.history)-preserveEnd])
	oldLen := len(a.history)
	a.stateMu.RUnlock()

	summary, err := a.summarizer.Summarize(ctx, toSummarize)
	if err != nil {
		return err
	}
	invokedSkills := a.invokedSkillSnapshot()

	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if len(a.history) < oldLen {
		return nil // concurrent compaction
	}

	newHistory := make([]*genai.Content, 0, preserveStart+1+preserveEnd)
	newHistory = append(newHistory, a.history[:preserveStart]...)
	newHistory = append(newHistory, summary)
	newHistory = append(newHistory, a.history[len(a.history)-preserveEnd:]...)
	newHistory = ensureToolPairConsistency(newHistory)
	newHistory = skills.ReattachInvocations(newHistory, invokedSkills, preserveStart+1)
	a.history = newHistory
	a.injectContinuationHint()

	logging.Info("history force-compacted (summarized)", "agent_id", a.ID,
		"old_len", oldLen, "new_len", len(a.history))
	return nil
}

// forceCompactViaTruncation is the fallback when summarization fails.
// Uses importance scoring to preserve the most valuable messages from the middle.
func (a *Agent) forceCompactViaTruncation() error {
	invokedSkills := a.invokedSkillSnapshot()
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	if len(a.history) <= 10 {
		return nil
	}

	keepStart := 3
	keepEnd := 6
	keepMiddle := 4

	if len(a.history) < keepStart+keepEnd+keepMiddle {
		return nil
	}

	middle := a.history[keepStart : len(a.history)-keepEnd]

	type scored struct {
		idx   int
		score float64
		msg   *genai.Content
	}

	var floatScores []float64
	if a.relevanceScorer != nil {
		floatScores = a.relevanceScorer.ScoreMessages(middle, a.fileTracker, keepStart)
	}

	scores := make([]scored, len(middle))
	for i, msg := range middle {
		var s float64
		if floatScores != nil {
			s = floatScores[i]
		} else {
			s = primitiveScore(msg)
		}
		scores[i] = scored{idx: i, score: s, msg: msg}
	}

	for i := 0; i < keepMiddle && i < len(scores); i++ {
		best := i
		for j := i + 1; j < len(scores); j++ {
			if scores[j].score > scores[best].score {
				best = j
			}
		}
		scores[i], scores[best] = scores[best], scores[i]
	}

	topN := scores[:keepMiddle]
	for i := range topN {
		for j := i + 1; j < len(topN); j++ {
			if topN[i].idx > topN[j].idx {
				topN[i], topN[j] = topN[j], topN[i]
			}
		}
	}

	newHistory := make([]*genai.Content, 0, keepStart+1+keepMiddle+keepEnd)
	newHistory = append(newHistory, a.history[:keepStart]...)

	truncateNotice := genai.NewContentFromText(
		"[Conversation compacted. Key tool results and errors preserved.]",
		genai.RoleUser)
	newHistory = append(newHistory, truncateNotice)

	for _, s := range topN {
		if s.msg != nil {
			newHistory = append(newHistory, s.msg)
		}
	}

	newHistory = append(newHistory, a.history[len(a.history)-keepEnd:]...)
	newHistory = ensureToolPairConsistency(newHistory)
	newHistory = skills.ReattachInvocations(newHistory, invokedSkills, keepStart+1)
	a.history = newHistory
	a.injectContinuationHint()

	logging.Info("history force-compacted (truncation fallback)", "agent_id", a.ID,
		"new_len", len(a.history))
	return nil
}

// primitiveScore is the fallback scoring when RelevanceScorer is not available.
func primitiveScore(msg *genai.Content) float64 {
	if msg == nil {
		return 0
	}
	var s float64
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		if part.FunctionResponse != nil {
			s += 3
		} else if part.FunctionCall != nil {
			s += 1
		} else if part.Text != "" {
			lower := strings.ToLower(part.Text)
			if strings.Contains(lower, "error") || strings.Contains(lower, "failed") {
				s += 2
			}
		}
	}
	return s
}

// ensureToolPairConsistency removes orphaned FunctionCall/FunctionResponse parts
// from a history slice. This is a local implementation to avoid import cycles
// with the context package.
func ensureToolPairConsistency(history []*genai.Content) []*genai.Content {
	// Collect all FunctionCall IDs and FunctionResponse IDs
	callIDs := make(map[string]bool)
	responseIDs := make(map[string]bool)
	for _, msg := range history {
		if msg == nil {
			continue
		}
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil && part.FunctionCall.ID != "" {
				callIDs[part.FunctionCall.ID] = true
			}
			if part.FunctionResponse != nil && part.FunctionResponse.ID != "" {
				responseIDs[part.FunctionResponse.ID] = true
			}
		}
	}

	// Quick check: count orphans to avoid unnecessary allocations
	orphans := 0
	for _, msg := range history {
		if msg == nil {
			continue
		}
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil && part.FunctionCall.ID != "" && !responseIDs[part.FunctionCall.ID] {
				orphans++
			}
			if part.FunctionResponse != nil && part.FunctionResponse.ID != "" && !callIDs[part.FunctionResponse.ID] {
				orphans++
			}
		}
	}

	if orphans == 0 {
		return history
	}

	// Remove orphaned parts
	result := make([]*genai.Content, 0, len(history))
	for _, msg := range history {
		if msg == nil {
			continue
		}
		keptParts := make([]*genai.Part, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			keep := true
			if part.FunctionCall != nil && part.FunctionCall.ID != "" {
				if !responseIDs[part.FunctionCall.ID] {
					keep = false
				}
			}
			if part.FunctionResponse != nil && part.FunctionResponse.ID != "" {
				if !callIDs[part.FunctionResponse.ID] {
					keep = false
				}
			}
			if keep {
				keptParts = append(keptParts, part)
			}
		}
		if len(keptParts) > 0 {
			if len(keptParts) == len(msg.Parts) {
				// No parts removed — reuse original Content
				result = append(result, msg)
			} else {
				// Clone Content to avoid mutating shared pointer
				result = append(result, &genai.Content{
					Role:  msg.Role,
					Parts: keptParts,
				})
			}
		}
	}

	logging.Debug("ensureToolPairConsistency removed orphaned parts", "count", orphans)
	return result
}

// pruneToolOutputs truncates old FunctionResponse contents in history,
// protecting the last protectChars characters of tool output.
// Returns estimated characters freed. Must NOT be called under stateMu lock.
func (a *Agent) pruneToolOutputs(protectChars int) int {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	minMsgs := max(a.summarizeMinMsgs, 4)
	if len(a.history) <= minMsgs {
		return 0
	}

	// Walk from end to start, accumulating recent tool output chars.
	// Skip first 2 (system context) and last N (recent messages).
	start := 2
	end := len(a.history) - a.summarizeProtect
	if end <= start {
		return 0
	}

	// First pass: collect truncation candidates from the end backwards
	type truncCandidate struct {
		msgIdx  int
		partIdx int
		content string
		name    string
	}
	var candidates []truncCandidate

	for i := end - 1; i >= start; i-- {
		msg := a.history[i]
		if msg == nil {
			continue
		}
		for j := len(msg.Parts) - 1; j >= 0; j-- {
			part := msg.Parts[j]
			if part == nil || part.FunctionResponse == nil {
				continue
			}
			contentStr := ""
			if resp := part.FunctionResponse.Response; resp != nil {
				if c, ok := resp["content"].(string); ok {
					contentStr = c
				}
			}
			if len(contentStr) <= a.pruneMinOutputSize {
				continue // Already small, skip
			}
			// Preserve only successful, bounded rendered workflows. Oversized custom
			// tools named "skill" remain normal prune candidates.
			if part.FunctionResponse.Name == "skill" && len(contentStr) <= skills.MaxRenderedSkillBytes {
				if success, _ := part.FunctionResponse.Response["success"].(bool); success {
					continue
				}
			}
			candidates = append(candidates, truncCandidate{
				msgIdx:  i,
				partIdx: j,
				content: contentStr,
				name:    part.FunctionResponse.Name,
			})
		}
	}

	// Pre-compute relevance scores for all history messages to protect high-value outputs.
	var msgScores []float64
	if a.relevanceScorer != nil {
		msgScores = a.relevanceScorer.ScoreMessages(a.history, a.fileTracker)
	}

	// Second pass: truncate candidates beyond the protection window.
	// Candidates are ordered from newest to oldest (we walked backwards).
	var freed int
	var protectedSoFar int
	for _, c := range candidates {
		// Skip high-value messages (score > 5.0) — they contain important context
		if msgScores != nil && c.msgIdx < len(msgScores) && msgScores[c.msgIdx] > 5.0 {
			continue
		}
		protectedSoFar += len(c.content)
		if protectedSoFar <= protectChars {
			continue // Still within protection window
		}
		// Truncate this tool output — produce informative summary instead of blank placeholder
		var replacement string
		if a.compactor != nil {
			replacement = a.compactor.SummarizeForPrune(c.name, c.content)
		} else {
			replacement = fmt.Sprintf("[%s output truncated, was %d chars]", c.name, len(c.content))
		}
		// Create a new Part instead of mutating in place — concurrent snapshot
		// readers (checkAndSummarize, getModelResponse) hold references to the
		// old Part and would see the data change underneath them.
		oldPart := a.history[c.msgIdx].Parts[c.partIdx]
		a.history[c.msgIdx].Parts[c.partIdx] = &genai.Part{
			FunctionResponse: &genai.FunctionResponse{
				ID:       oldPart.FunctionResponse.ID,
				Name:     oldPart.FunctionResponse.Name,
				Response: map[string]any{"content": replacement},
			},
		}
		freed += len(c.content) - len(replacement)
	}

	return freed
}

// injectContinuationHint appends a synthetic user message after compaction
// so the model continues without pausing. Includes the original task prompt
// to prevent the agent from forgetting what it was doing.
// Must be called under stateMu write lock.
func (a *Agent) injectContinuationHint() {
	if len(a.history) == 0 {
		return
	}

	hint := "[System: Conversation was automatically compacted to free context space."
	if a.originalPrompt != "" {
		// Truncate long prompts to avoid bloating the hint
		taskReminder := a.originalPrompt
		if runes := []rune(taskReminder); len(runes) > 500 {
			taskReminder = string(runes[:500]) + "..."
		}
		hint += "\nYour original task: " + taskReminder
	}
	// Re-inject weak-model guidance after compaction: the system prompt that
	// originally carried these rules is preserved at history[0], but weak models
	// consistently "forget" them once mid-conversation context is summarized.
	// Embedding the rules in the continuation hint puts them in the freshest
	// position in the context window.
	if a.weakModelMode {
		hint += "\n\nReminders for the remainder of this task:" + a.buildWeakModelGuidance()
	}
	// Already-read files registry: compaction drops raw file contents from
	// history, but the tracker still knows which paths we saw. Surfacing the
	// list stops the model from re-running `read` as its first action on
	// familiar files — saves tokens and avoids "I don't have this loaded"
	// false starts after a summary.
	if a.recentFilesProvider != nil {
		if files := a.recentFilesProvider(15); len(files) > 0 {
			hint += "\n\nAlready-read files in this session (content was compacted; re-read only if you need specific details):"
			for _, f := range files {
				hint += "\n- " + f
			}
		}
	}
	if a.modifiedFilesProvider != nil {
		if files := a.modifiedFilesProvider(10); len(files) > 0 {
			hint += "\n\nFiles you already modified in this task (do not overwrite unless the user asked for a change):"
			for _, f := range files {
				hint += "\n- " + f
			}
		}
	}
	hint += "\nContinue with your current task.]"

	last := a.history[len(a.history)-1]
	if last.Role == genai.RoleUser {
		// Append to existing user message to avoid consecutive same-role issues
		last.Parts = append(last.Parts, genai.NewPartFromText(hint))
	} else {
		a.history = append(a.history, genai.NewContentFromText(hint, genai.RoleUser))
	}

	// Compaction handoff contract: the model now reasons over a summarized
	// history, so stale per-call repetition counts must not survive into it.
	// This is the single chokepoint for every compaction path (checkAndSummarize,
	// forceCompactViaSummary, forceCompactViaTruncation all call this). Uses
	// callHistoryMu only; the codebase never nests callHistoryMu→stateMu, so
	// calling it here (under stateMu) cannot invert lock order.
	a.resetLoopDetection()
}

// collectStream collects a streaming response while firing onText and onThinking
// callbacks in real-time, so the TUI can display content as it arrives.
func (a *Agent) collectStream(ctx context.Context, stream *client.StreamingResponse) (*client.Response, error) {
	return client.ProcessStream(ctx, stream, &client.StreamHandler{
		OnText: func(text string) {
			a.safeOnText(text)
		},
		OnThinking: func(text string) {
			a.safeOnThinking(text)
		},
		OnRateLimit: func(rl *client.RateLimitMetadata) {
			a.safeOnRateLimit(rl)
		},
	})
}

// SetModelRoundTimeout sets the hard cap for one model API round. Non-positive
// values resolve to the shared default, matching tools.Executor. An in-flight
// round keeps the deadline it started with; the next round sees the new value.
func (a *Agent) SetModelRoundTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = client.DefaultModelRoundTimeout
	}
	a.stateMu.Lock()
	a.modelRoundTimeout = timeout
	reflector := a.reflector
	a.stateMu.Unlock()
	if reflector != nil {
		reflector.SetSemanticTimeout(timeout)
	}
}

// SetPlanningTimeout updates this agent's private tree-planner clone. It is
// safe to call while a run is active; an in-flight planning call keeps the
// context it already derived, while the next one sees the new atomic value.
func (a *Agent) SetPlanningTimeout(timeout time.Duration) {
	if a == nil || a.treePlanner == nil {
		return
	}
	a.treePlanner.SetPlanningTimeout(timeout)
}

// ModelRoundTimeout returns the effective hard cap used by new model rounds.
func (a *Agent) ModelRoundTimeout() time.Duration {
	a.stateMu.RLock()
	timeout := a.modelRoundTimeout
	a.stateMu.RUnlock()
	if timeout <= 0 {
		return client.DefaultModelRoundTimeout
	}
	return timeout
}

// withModelRoundTimeout mirrors the executor's helper EXACTLY: it wraps the round
// with a TYPED, non-retryable model-round-timeout cause (so a genuine round
// timeout fails cleanly instead of being misclassified as a retryable raw
// context.DeadlineExceeded and pointlessly retried into the same cap), clamps to
// the parent's remaining deadline when the parent is stricter, and uses
// cancel-only when no time remains (never a zero-length WithTimeout).
func (a *Agent) withModelRoundTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.ModelRoundTimeout()
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.WithCancel(parent)
		}
		if remaining < timeout {
			// Parent context is stricter; preserve its cause chain.
			return context.WithTimeout(parent, remaining)
		}
	}
	return context.WithTimeoutCause(parent, timeout, client.NewModelRoundTimeoutError(timeout))
}

// agentCompactionAPITimeout bounds a single summarization or token-count API
// call made during pre-emptive compaction (checkAndSummarize). Without it those
// calls inherit the raw parent context, so a slow or hung provider endpoint
// could stall a turn — or a /loop iteration / sub-agent — indefinitely.
// forceCompactViaSummary uses the same helper; this
// extends the documented "compaction never hangs" invariant to the pre-emptive
// path, which is the one sub-agents and /loop iterations hit most.
const agentCompactionAPITimeout = config.DefaultModelRoundTimeout

func (a *Agent) withCompactionTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := a.compactionAPITimeout
	if timeout <= 0 {
		timeout = a.ModelRoundTimeout()
	}
	if deadline, ok := parent.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			return context.WithTimeout(parent, remaining)
		}
	}
	return context.WithTimeoutCause(parent, timeout, client.NewModelRoundTimeoutError(timeout))
}

// getModelResponse gets a response from the model.
func (a *Agent) getModelResponse(ctx context.Context) (*client.Response, error) {
	// Refresh active skill carry-forward before an ordinary text round. Insert
	// before the current user message so the user's instruction remains the
	// actual message passed to SendMessageWithHistory. Never do this for a
	// pending FunctionResponse turn: inserting any message between a tool call
	// and its response can violate strict provider tool-pair protocols.
	invokedSkills := a.invokedSkillSnapshot()
	a.stateMu.Lock()
	historyLen := len(a.history)
	if historyLen == 0 {
		a.stateMu.Unlock()
		return nil, fmt.Errorf("empty history")
	}
	lastContent := a.history[historyLen-1]
	if lastContent == nil {
		a.stateMu.Unlock()
		return nil, fmt.Errorf("history ends with nil content")
	}
	if len(invokedSkills) > 0 && lastContent != nil && lastContent.Role == genai.RoleUser &&
		!contentHasFunctionResponse(lastContent) {
		// Refresh only the prefix, then append the saved current user message.
		// This keeps insertAt relative to exactly the slice from which old carry
		// messages are removed and guarantees the actual prompt remains last.
		prefix := skills.ReattachInvocations(a.history[:historyLen-1], invokedSkills, historyLen-1)
		refreshed := make([]*genai.Content, 0, len(prefix)+1)
		refreshed = append(refreshed, prefix...)
		refreshed = append(refreshed, lastContent)
		a.history = refreshed
		historyLen = len(a.history)
		lastContent = a.history[historyLen-1]
	}
	a.stateMu.Unlock()

	// Check if the last content contains function responses (tool results).
	// If so, use SendFunctionResponse instead of SendMessageWithHistory
	// to avoid sending an empty message string to APIs that reject it.
	if lastContent.Role == genai.RoleUser {
		var funcResponses []*genai.FunctionResponse
		var hasInlineData bool
		for _, part := range lastContent.Parts {
			if part.FunctionResponse != nil {
				funcResponses = append(funcResponses, &genai.FunctionResponse{
					ID:       part.FunctionResponse.ID,
					Name:     part.FunctionResponse.Name,
					Response: part.FunctionResponse.Response,
				})
			}
			if part.InlineData != nil {
				hasInlineData = true
			}
		}

		if len(funcResponses) > 0 {
			// Copy history under lock
			a.stateMu.RLock()
			historyWithoutLast := make([]*genai.Content, len(a.history)-1)
			copy(historyWithoutLast, a.history[:len(a.history)-1])
			a.stateMu.RUnlock()

			if hasInlineData {
				roundCtx, roundCancel := a.withModelRoundTimeout(ctx)
				defer roundCancel()
				stream, err := a.client.SendMessageWithHistory(roundCtx, append(historyWithoutLast, lastContent), "Continue processing the tool results above.")
				if err != nil {
					return nil, err
				}
				return a.collectStream(roundCtx, stream)
			}

			roundCtx, roundCancel := a.withModelRoundTimeout(ctx)
			defer roundCancel()
			stream, err := a.client.SendFunctionResponse(roundCtx, historyWithoutLast, funcResponses)
			if err != nil {
				return nil, err
			}
			return a.collectStream(roundCtx, stream)
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

	roundCtx, roundCancel := a.withModelRoundTimeout(ctx)
	defer roundCancel()

	stream, err := a.client.SendMessageWithHistory(roundCtx, historyWithoutLast, message)
	if err != nil {
		return nil, err
	}

	return a.collectStream(roundCtx, stream)
}

// executeTools executes the function calls with parallel execution for read-only tools.
func (a *Agent) executeTools(ctx context.Context, calls []*genai.FunctionCall) []toolCallResult {
	results := make([]toolCallResult, len(calls))

	// Build index for result placement
	callIndex := make(map[*genai.FunctionCall]int)
	for i, call := range calls {
		callIndex[call] = i
	}

	// Classify tools in the model-provided order. Reads separated by a write
	// are not interchangeable: a later read may intentionally observe that
	// write, so moving every read to the front produces stale results.
	classifier := NewToolDependencyClassifier()
	groups := classifier.ClassifyDependencies(calls)

	for _, group := range groups {
		if budgetErr := invocationBudgetTerminalError(ctx); budgetErr != nil {
			for _, call := range group.Calls {
				results[callIndex[call]] = budgetSkippedToolCallResult(call, budgetErr)
			}
			continue
		}
		if group.Parallel && len(group.Calls) > 1 {
			// Execute read-only tools in parallel
			a.executeToolsParallel(ctx, group.Calls, results, callIndex)
		} else {
			// Execute sequentially (write tools or single tool)
			for _, call := range group.Calls {
				idx := callIndex[call]
				if budgetErr := invocationBudgetTerminalError(ctx); budgetErr != nil {
					results[idx] = budgetSkippedToolCallResult(call, budgetErr)
					continue
				}
				results[idx] = a.executeToolCancellable(ctx, call)
			}
		}
	}

	return results
}

// sequentialToolCleanupGrace gives a context-aware tool a brief opportunity
// to return its own result after cancellation. A non-cooperative tool is then
// abandoned so one blocked syscall or MCP implementation cannot hang the
// entire sub-agent turn.
var sequentialToolCleanupGrace = 50 * time.Millisecond

type agentToolExecutionLease struct{ active atomic.Bool }
type agentToolExecutionLeaseKey struct{}

type rawAgentToolOutcome struct {
	result  tools.ToolResult
	elapsed time.Duration
}

func agentToolExecutionActive(ctx context.Context) bool {
	lease, _ := ctx.Value(agentToolExecutionLeaseKey{}).(*agentToolExecutionLease)
	return lease == nil || lease.active.Load()
}

func canAbandonAgentTool(name string) bool {
	return tools.IsParallelSafeTool(strings.ToLower(strings.TrimSpace(name)))
}

func cancelledToolCallResult(call *genai.FunctionCall) toolCallResult {
	return toolCallResult{
		Response: &genai.FunctionResponse{
			ID:       call.ID,
			Name:     call.Name,
			Response: tools.NewErrorResult("cancelled").ToMap(),
		},
	}
}

func (a *Agent) executeToolCancellable(ctx context.Context, call *genai.FunctionCall) toolCallResult {
	if ctx.Err() != nil {
		return cancelledToolCallResult(call)
	}
	if !canAbandonAgentTool(call.Name) {
		return a.executeToolSafely(ctx, call)
	}

	lease := &agentToolExecutionLease{}
	lease.active.Store(true)
	toolCtx := context.WithValue(ctx, agentToolExecutionLeaseKey{}, lease)

	done := make(chan rawAgentToolOutcome, 1)
	go func() {
		started := time.Now()
		done <- rawAgentToolOutcome{
			result:  a.executeToolResultSafely(toolCtx, call),
			elapsed: time.Since(started),
		}
	}()

	select {
	case outcome := <-done:
		return a.finishToolResultSafely(toolCtx, call, outcome.result, outcome.elapsed)
	case <-ctx.Done():
		timer := time.NewTimer(sequentialToolCleanupGrace)
		defer timer.Stop()
		select {
		case outcome := <-done:
			return a.finishToolResultSafely(toolCtx, call, outcome.result, outcome.elapsed)
		case <-timer.C:
			lease.active.Store(false)
			logging.Warn("sequential tool did not exit after cancellation", "tool", call.Name)
			return cancelledToolCallResult(call)
		}
	}
}

func (a *Agent) executeToolResultSafely(ctx context.Context, call *genai.FunctionCall) (result tools.ToolResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logging.Error("panic in sequential tool execution",
				"tool", call.Name,
				"panic", recovered,
				"stack", logging.PanicStack())
			result = tools.NewErrorResult(tools.FormatToolPanic(call.Name, recovered))
		}
	}()
	return a.executeTool(ctx, call)
}

func (a *Agent) finishToolResultSafely(ctx context.Context, call *genai.FunctionCall, raw tools.ToolResult, elapsed time.Duration) (result toolCallResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logging.Error("panic while finalizing sequential tool execution",
				"tool", call.Name,
				"panic", recovered,
				"stack", logging.PanicStack())
			result = toolCallResult{Response: &genai.FunctionResponse{
				ID:       call.ID,
				Name:     call.Name,
				Response: tools.NewErrorResult(tools.FormatToolPanic(call.Name, recovered)).ToMap(),
			}}
		}
	}()
	return a.finishToolWithReflection(ctx, call, raw, elapsed)
}

func (a *Agent) executeToolSafely(ctx context.Context, call *genai.FunctionCall) (result toolCallResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logging.Error("panic in sequential tool execution",
				"tool", call.Name,
				"panic", recovered,
				"stack", logging.PanicStack())
			result = toolCallResult{Response: &genai.FunctionResponse{
				ID:       call.ID,
				Name:     call.Name,
				Response: tools.NewErrorResult(tools.FormatToolPanic(call.Name, recovered)).ToMap(),
			}}
		}
	}()
	return a.executeToolWithReflection(ctx, call)
}

// parallelToolCleanupGrace is how long executeToolsParallel waits for
// straggler goroutines after ctx cancellation before giving up and freezing
// `results`. A package var (not a const) so tests can shrink it to exercise
// the abandon-and-freeze path without a real multi-second sleep.
var parallelToolCleanupGrace = 5 * time.Second

// executeToolsParallel executes multiple tools concurrently.
func (a *Agent) executeToolsParallel(ctx context.Context, calls []*genai.FunctionCall,
	results []toolCallResult, indexMap map[*genai.FunctionCall]int) {

	leases := make(map[*genai.FunctionCall]*agentToolExecutionLease, len(calls))
	for _, call := range calls {
		lease := &agentToolExecutionLease{}
		lease.active.Store(true)
		leases[call] = lease
	}

	// Pre-populate every slot with a placeholder BEFORE spawning goroutines.
	// Raw workers never write this slice. If cleanup expires, the caller can
	// return these placeholders while stragglers finish into a buffered channel
	// that no longer has an observer; this avoids both late state mutation and a
	// race with the caller reading results.
	for _, call := range calls {
		results[indexMap[call]] = cancelledToolCallResult(call)
	}

	type indexedRawOutcome struct {
		idx      int
		call     *genai.FunctionCall
		outcome  rawAgentToolOutcome
		executed bool
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 5) // Max 5 concurrent executions
	outcomes := make(chan indexedRawOutcome, len(calls))
	spawned := 0

	for _, call := range calls {
		// Check context before spawning goroutine to avoid unnecessary work
		if ctx.Err() != nil {
			continue
		}

		idx := indexMap[call]
		spawned++
		wg.Add(1)
		go func(i int, fc *genai.FunctionCall) {
			item := indexedRawOutcome{idx: i, call: fc}
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					logging.Error("panic in parallel tool execution",
						"tool", fc.Name, "panic", r, "stack", logging.PanicStack())
					item.executed = true
					item.outcome.result = tools.NewErrorResult(tools.FormatToolPanic(fc.Name, r))
				}
				outcomes <- item
			}()

			// Check context again before trying to acquire semaphore
			if ctx.Err() != nil {
				return
			}

			// Acquire semaphore slot with timeout to prevent goroutine leak
			acquired := false
			select {
			case semaphore <- struct{}{}:
				acquired = true
			case <-ctx.Done():
				return
			}

			if acquired {
				defer func() { <-semaphore }()
			}

			toolCtx := context.WithValue(ctx, agentToolExecutionLeaseKey{}, leases[fc])
			started := time.Now()
			item.executed = true
			item.outcome = rawAgentToolOutcome{
				result:  a.executeToolResultSafely(toolCtx, fc),
				elapsed: time.Since(started),
			}
		}(idx, call)
	}

	// Wait with timeout to prevent infinite blocking
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	completed := false
	select {
	case <-done:
		completed = true
	case <-ctx.Done():
		// Context cancelled, but goroutines should exit on their own
		// Wait a bit more for cleanup
		cleanupTimer := time.NewTimer(parallelToolCleanupGrace)
		select {
		case <-done:
			cleanupTimer.Stop()
			completed = true
		case <-cleanupTimer.C:
			logging.Warn("executeToolsParallel: some goroutines did not exit in time")
			for _, lease := range leases {
				lease.active.Store(false)
			}
		}
	}

	if !completed {
		return
	}

	// Every raw worker has exited, so finalization is owned exclusively by this
	// caller goroutine. Reflection, learning, done-gate bookkeeping, fix-cache,
	// and delegation can no longer outlive executeToolsParallel and pollute a
	// subsequent agent run.
	completedOutcomes := make(map[int]indexedRawOutcome, spawned)
	for range spawned {
		item := <-outcomes
		completedOutcomes[item.idx] = item
	}
	for _, call := range calls {
		idx := indexMap[call]
		item, ok := completedOutcomes[idx]
		if !ok || !item.executed {
			continue
		}
		toolCtx := context.WithValue(ctx, agentToolExecutionLeaseKey{}, leases[item.call])
		results[idx] = a.finishToolResultSafely(toolCtx, item.call, item.outcome.result, item.outcome.elapsed)
	}
}

// toolCallResult bundles a function response with optional multimodal parts
// (e.g., images) that should be sent alongside the response to the LLM.
type toolCallResult struct {
	Response       *genai.FunctionResponse
	MultimodalData []*tools.MultimodalPart
}

// executeToolWithReflection executes a tool with reflection and delegation on failure.
func (a *Agent) executeToolWithReflection(ctx context.Context, call *genai.FunctionCall) toolCallResult {
	toolStart := time.Now()
	result := a.executeTool(ctx, call)
	return a.finishToolWithReflection(ctx, call, result, time.Since(toolStart))
}

func (a *Agent) finishToolWithReflection(ctx context.Context, call *genai.FunctionCall, result tools.ToolResult, elapsed time.Duration) toolCallResult {
	if !agentToolExecutionActive(ctx) {
		// The caller already returned a cancellation placeholder. Do not let a
		// late completion alter done-gate state, learning, reflection, fix cache,
		// or delegation state for a subsequent run.
		return toolCallResult{Response: &genai.FunctionResponse{
			ID: call.ID, Name: call.Name, Response: result.ToMap(),
		}}
	}
	if result.PolicyBlock != nil {
		a.recordPolicyBlock(result.PolicyBlock)
	}
	a.recordToolExecution(call.Name, call.Args, result)

	// Feed durable tool-outcome signals into ProjectLearning. Runs on both
	// success and failure so success-rate EMA reflects reality. Debounced
	// save at the ProjectLearning layer (2s timer) absorbs any I/O cost.
	a.recordToolForLearning(call, result, elapsed)

	// On success, feed into fix cache for fix detection
	if result.Success && a.fixCache != nil {
		a.fixCache.RecordSuccess(call.Name, call.Args)
	}

	var reflection *Reflection

	// Apply self-reflection on errors to provide recovery suggestions
	if ctx.Err() == nil && !result.Success && result.PolicyBlock == nil && a.reflector != nil {
		// --- Fix Cache: fast path ---
		// Try session-local cache before full Reflect pipeline
		category := a.reflector.QuickCategorize(result.Content)
		var cacheHit *FixRecord
		if category != "" && a.fixCache != nil {
			cacheHit, _ = a.fixCache.Lookup(call.Name, category, result.Content)
		}

		if cacheHit != nil {
			// Cache hit: build synthetic reflection with cached fix info
			reflection = &Reflection{
				ToolName:     call.Name,
				Error:        result.Content,
				Category:     category,
				Suggestion:   "Apply the previously successful fix sequence.",
				ShouldRetry:  true,
				Intervention: FormatCachedFix(cacheHit),
			}
			// Record hit only; success/fail is determined by whether the same error recurs
			a.fixCache.RecordHit(cacheHit.Signature.Key())
			logging.Info("fix cache hit", "tool", call.Name, "category", category,
				"hit_count", cacheHit.HitCount)
		} else {
			// Cache miss: full Reflect pipeline — may invoke LLM (up to 30s)
			a.safeOnText(fmt.Sprintf("\n[Analyzing %s error...]\n", call.Name))
			reflection = a.reflector.Reflect(ctx, call.Name, call.Args, result.Content)
		}

		// Record error in fix cache for future fix detection
		if a.fixCache != nil && category == "" && reflection != nil {
			category = reflection.Category
		}
		turnIdx := a.GetTurnCount()
		if a.fixCache != nil && category != "" {
			a.fixCache.RecordError(call.Name, call.Args, category, result.Content, turnIdx)
		}

		// Auto-fix attempt before enrichment (only on cache miss).
		// Recovery attempts are budgeted per tool+category key.
		if cacheHit == nil && reflection != nil && a.recoveryExecutor != nil {
			key := recoveryAttemptKey(call.Name, call.Args, category, reflection.Alternative)
			a.autoFixAttemptsMu.Lock()
			attempt := a.autoFixAttempts[key]
			a.autoFixAttemptsMu.Unlock()

			// Snapshot both agentID and callback under stateMu to avoid races
			// with SetOnToolActivity (matches pattern at executeToolCall below).
			a.stateMu.RLock()
			onToolActivity := a.onToolActivity
			agentID := a.ID
			a.stateMu.RUnlock()
			args := map[string]any{"reason": reflection.Category}
			invokeAgentToolActivity(onToolActivity, agentID, call.Name, args, "tool_recovery", false, "")

			fixResult, handled := a.recoveryExecutor.AttemptAutoFix(ctx, a, call, reflection, attempt)
			if handled {
				a.autoFixAttemptsMu.Lock()
				a.autoFixAttempts[key]++
				a.autoFixAttemptsMu.Unlock()

				if fixResult.Success {
					// Fully recovered — return the successful result
					logging.Info("auto-fix recovered", "tool", call.Name, "category", reflection.Category)
					if reflection.LearnedEntryID != "" {
						a.reflector.RecordSolutionSuccess(reflection.LearnedEntryID)
					}
				} else {
					// Enriched context — return as error with extra context for the model
					logging.Info("auto-fix enriched context", "tool", call.Name, "category", reflection.Category)
				}

				// Compact if needed
				if a.compactor != nil {
					fixResult = a.compactor.CompactForType(call.Name, fixResult)
				}

				return toolCallResult{Response: &genai.FunctionResponse{
					ID: call.ID, Name: call.Name, Response: fixResult.ToMap(),
				}}
			}
		}

		if reflection.Intervention != "" {
			// Enrich the error result with reflection analysis
			result.Content = fmt.Sprintf("%s\n\n---\n**Self-Reflection:**\n%s",
				result.Content, reflection.Intervention)

			// Append aggregation guidance if error category is recurring
			if a.fixCache != nil && category != "" {
				if agg := a.fixCache.GetAggregation(category); agg != "" {
					result.Content += "\n\n" + agg
				}
			}

			// Log reflection
			logging.Info("agent reflected on error",
				"agent_id", a.ID,
				"tool", call.Name,
				"category", reflection.Category,
				"should_retry", reflection.ShouldRetry)
		}
	}

	// Check for autonomous delegation opportunity
	if ctx.Err() == nil && !result.Success && result.PolicyBlock == nil && a.delegation != nil && a.delegation.HasMessenger() {
		delCtx := &DelegationContext{
			AgentType:       a.Type,
			CurrentTurn:     a.GetTurnCount(),
			MaxTurns:        a.maxTurns,
			LastToolName:    call.Name,
			LastToolError:   result.Content,
			LastToolArgs:    call.Args,
			ReflectionInfo:  reflection,
			StuckCount:      a.delegation.GetStuckCount(),
			DelegationDepth: a.delegation.GetDepth(),
		}

		decision := a.delegation.Evaluate(delCtx)
		if decision.ShouldDelegate {
			// Execute delegation
			delegationStart := time.Now()
			delegationResponse, err := a.delegation.ExecuteDelegation(ctx, decision)
			delegationDuration := time.Since(delegationStart)

			if err == nil && delegationResponse != "" {
				// Append delegation result to the tool response
				result.Content = fmt.Sprintf("%s\n\n---\n**Delegated to %s agent:**\n%s",
					result.Content, decision.TargetType, delegationResponse)
				result.Success = true // Mark as recovered

				a.delegation.RecordDelegationResult(decision.TargetType, true, delegationDuration, "")
				logging.Info("delegation successful",
					"agent_id", a.ID,
					"delegated_to", decision.TargetType,
					"reason", decision.Reason)
			} else {
				errType := "empty_response"
				if err != nil {
					errType = err.Error()
				}
				a.delegation.RecordDelegationResult(decision.TargetType, false, delegationDuration, errType)
			}
		}
	}

	// Capture multimodal parts before compaction (compaction only affects text)
	multimodalData := result.MultimodalParts

	// Compact result if it's too large before converting to map
	if a.compactor != nil {
		policyBlock := result.PolicyBlock
		result = a.compactor.CompactForType(call.Name, result)
		result.PolicyBlock = policyBlock
	}

	return toolCallResult{
		Response: &genai.FunctionResponse{
			ID:       call.ID, // Must match tool_use.id for Anthropic/DeepSeek API
			Name:     call.Name,
			Response: result.ToMap(),
		},
		MultimodalData: multimodalData,
	}
}

// executeTool executes a single tool call with enhanced safety and retry logic.
// Named return `out` so the deferred tool-activity "end" event can report the
// real outcome (success + a short summary) — see subAgentToolOutcome.
func (a *Agent) executeTool(ctx context.Context, call *genai.FunctionCall) (out tools.ToolResult) {
	tool, ok := a.registry.Get(call.Name)
	if !ok {
		return tools.NewErrorResult(fmt.Sprintf("tool not available for this agent: %s", call.Name))
	}

	// Validate arguments
	if err := tool.Validate(call.Args); err != nil {
		return tools.NewErrorResult(fmt.Sprintf("validation error: %s", err))
	}

	// Check permissions before executing
	// Capture hidden runtime state even when this agent has no interactive
	// permission manager. Hooks and UI policy toggles may run before execution;
	// Bash must still execute only under the exact scope observed here.
	authorizedPermissionArgs := tools.PermissionArgsForTool(tool, call.Args)
	if a.permissions != nil {
		a.stateMu.RLock()
		permissionWorkDir := a.workDir
		a.stateMu.RUnlock()
		permissionCtx := permission.ContextWithWorkDir(ctx, permissionWorkDir)
		resp, err := a.permissions.CheckWithTemporaryToolRules(
			permissionCtx,
			call.Name,
			tools.ClonePermissionArgs(authorizedPermissionArgs),
			tools.SkillPermissionGrants(a.registry),
			tools.SkillPermissionDenies(a.registry),
		)
		if err != nil {
			return tools.NewPolicyBlockedResult(tools.PolicyBlockPermission, fmt.Sprintf("permission error: %s", err))
		}
		if !resp.Allowed {
			reason := resp.Reason
			if reason == "" {
				reason = "permission denied"
			}
			return tools.NewPolicyBlockedResult(tools.PolicyBlockPermission, fmt.Sprintf("Permission denied: %s", reason))
		}
	}

	a.stateMu.RLock()
	hookManager := a.hooks
	workDir := a.workDir
	a.stateMu.RUnlock()
	if hookManager != nil {
		preResults := hookManager.RunPreToolInDir(ctx, workDir, call.Name, call.Args)
		if blocked, ok := hooks.Blocked(preResults); ok {
			name := blocked.Hook.DisplayName()
			reason := strings.TrimSpace(blocked.Output)
			if reason == "" && blocked.Error != nil {
				reason = blocked.Error.Error()
			}
			return tools.NewPolicyBlockedResult(tools.PolicyBlockHook, fmt.Sprintf(
				"hook blocked: pre-tool hook %q refused %s: %s", name, call.Name, reason))
		}
	}
	// Permission prompts and pre-hooks may outlive cancellation. Re-check the
	// execution lease immediately before crossing into the tool so an abandoned
	// call cannot start late.
	if ctx.Err() != nil || !agentToolExecutionActive(ctx) {
		return tools.NewErrorResult("cancelled")
	}

	// Snapshot callback under stateMu to avoid races with SetOnToolActivity
	a.stateMu.RLock()
	onToolActivity := a.onToolActivity
	a.stateMu.RUnlock()

	// Report tool start to UI
	if ctx.Err() != nil || !agentToolExecutionActive(ctx) {
		return tools.NewErrorResult("cancelled")
	}
	invokeAgentToolActivity(onToolActivity, a.ID, call.Name, call.Args, "start", false, "")

	// Guarantee "end" event is sent regardless of outcome (panic, error,
	// success), carrying the tool's outcome so the UI can show meaningful
	// sub-agent output (✓/✗ + a short result line) instead of a bare name.
	defer func() {
		if !agentToolExecutionActive(ctx) {
			return
		}
		success, summary := subAgentToolOutcome(out)
		invokeAgentToolActivity(onToolActivity, a.ID, call.Name, call.Args, "end", success, summary)
	}()

	// No tool-level retry: API retries are handled by the client layer
	// (rate-limit retry, message processor retry, failover). Adding retries
	// here compounds with those layers and wastes tokens.
	//
	// Record retry provenance immediately before crossing the execution
	// boundary. Do not wait for a successful result: bash/remote/MCP tools may
	// commit a side effect and then fail, and cancellation can suppress later
	// reflection/bookkeeping. IsWriteTool fails closed for unknown tools.
	a.recordStatefulToolAttempt(call.Name)
	result, err := tools.ExecuteWithPermissionScope(ctx, tool, call.Args, authorizedPermissionArgs)
	if err != nil {
		if strings.HasPrefix(err.Error(), "Permission scope changed before execution:") {
			out = tools.NewPolicyBlockedResult(tools.PolicyBlockPermission, err.Error())
		} else {
			out = tools.NewErrorResult(err.Error())
		}
		if hookManager != nil && agentToolExecutionActive(ctx) {
			hookManager.RunOnErrorInDir(ctx, workDir, call.Name, call.Args, out.Error)
		}
		return
	}
	out = result
	if !out.Success && out.PolicyBlock == nil &&
		strings.HasPrefix(out.Error, "Permission scope changed before execution:") {
		out = tools.WithPolicyBlock(out, tools.PolicyBlockPermission, out.Error)
	}
	if hookManager != nil && agentToolExecutionActive(ctx) {
		if out.Success {
			hookManager.RunPostToolInDir(ctx, workDir, call.Name, call.Args, out.Content)
		} else {
			hookManager.RunOnErrorInDir(ctx, workDir, call.Name, call.Args, out.Error)
		}
	}
	return
}

func (a *Agent) recordPolicyBlock(block *tools.PolicyBlock) {
	if block == nil {
		return
	}
	a.stateMu.Lock()
	if a.runPolicyBlock == nil {
		copy := *block
		a.runPolicyBlock = &copy
	}
	a.stateMu.Unlock()
}

func (a *Agent) policyBlockSnapshot() *tools.PolicyBlock {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.runPolicyBlock == nil {
		return nil
	}
	copy := *a.runPolicyBlock
	return &copy
}

// buildResponseParts creates Parts from a response.
// Returns at least one part to avoid empty Parts which causes API errors.
func (a *Agent) buildResponseParts(resp *client.Response) []*genai.Part {
	parts := client.ResponsePartsForHistory(resp)

	// Ensure we never return empty parts - API requires at least one part
	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(" "))
	}

	return parts
}

// shouldRetryEmptyAfterTools reports whether resp is a transient empty response
// (no text, no function calls, not a max_tokens truncation) that arrived right
// after a tool-results round — the case worth a side-effect-free re-send.
// "After a tool-results round" is derived from history: history[n-1] is the
// just-appended empty " " placeholder, so the round was a function-response round
// iff the entry BEFORE it (history[n-2]) carries tool results. Pure (no agent
// state) so it's table-testable without the client/stream harness.
func shouldRetryEmptyAfterTools(history []*genai.Content, resp *client.Response) bool {
	if resp == nil || resp.Text != "" || len(resp.FunctionCalls) > 0 ||
		resp.FinishReason == genai.FinishReasonMaxTokens {
		return false
	}
	n := len(history)
	return n >= 2 && contentHasFunctionResponse(history[n-2])
}

// contentHasFunctionResponse reports whether c is a tool-results turn (a user
// content carrying at least one FunctionResponse part) — the same shape
// getModelResponse keys SendFunctionResponse off of.
func contentHasFunctionResponse(c *genai.Content) bool {
	if c == nil || c.Role != genai.RoleUser {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// popEmptyModelPlaceholder removes the trailing empty model placeholder that
// buildResponseParts appends for an empty response (a lone " " text part), so the
// next getModelResponse re-sees the tool-results turn and re-issues
// SendFunctionResponse — a side-effect-free re-send. Returns false (leaving
// history untouched) unless the last entry is EXACTLY that placeholder, so a real
// model turn is never truncated. Mutates a.history under a.stateMu.
func (a *Agent) popEmptyModelPlaceholder() bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	n := len(a.history)
	if n == 0 {
		return false
	}
	last := a.history[n-1]
	if last == nil || last.Role != genai.RoleModel || len(last.Parts) != 1 {
		return false
	}
	if p := last.Parts[0]; p == nil || p.FunctionCall != nil || p.Text != " " {
		return false
	}
	a.history = a.history[:n-1]
	return true
}

// GetStatus returns the current agent status.
func (a *Agent) GetStatus() AgentStatus {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.status
}

// GetEndTime returns the agent's end time (thread-safe).
func (a *Agent) GetEndTime() time.Time {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.endTime
}

// GetStartTime returns the agent's start time (thread-safe).
func (a *Agent) GetStartTime() time.Time {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.startTime
}

// GetTaskPreview returns the first line of the agent's original task prompt,
// rune-safely truncated to maxRunes. Empty until Run has been called.
func (a *Agent) GetTaskPreview(maxRunes int) string {
	a.stateMu.RLock()
	prompt := a.originalPrompt
	a.stateMu.RUnlock()

	prompt = strings.TrimSpace(prompt)
	if idx := strings.IndexByte(prompt, '\n'); idx >= 0 {
		prompt = strings.TrimSpace(prompt[:idx])
	}
	if maxRunes <= 0 {
		return prompt
	}
	runes := []rune(prompt)
	if len(runes) <= maxRunes {
		return prompt
	}
	return string(runes[:maxRunes]) + "…"
}

// Cancel cancels the agent's execution.
func (a *Agent) Cancel() {
	a.stateMu.Lock()
	if a.status != AgentStatusPending && a.status != AgentStatusRunning {
		a.stateMu.Unlock()
		return
	}
	cancel := a.cancelFunc
	if cancel == nil {
		// Async spawn registers the agent before its goroutine installs the
		// cancellable run context. Remember a cancellation in that narrow window
		// so task_stop cannot report success while letting the run start anyway.
		a.cancelRequested = true
	}
	a.stateMu.Unlock()

	// Do not publish a terminal status here. Agent.Run owns status/endTime and
	// Runner publishes Completed only after all finalization has finished.
	if cancel != nil {
		cancel()
	}
}

// SetCancelFunc sets the cancel function for explicit agent cancellation.
func (a *Agent) SetCancelFunc(cancel context.CancelFunc) {
	a.stateMu.Lock()
	a.cancelFunc = cancel
	requested := a.cancelRequested
	a.cancelRequested = false
	a.stateMu.Unlock()

	if requested && cancel != nil {
		cancel()
	}
}

// SetHooks installs the trusted tool-hook policy for this agent. The manager
// is shared and concurrency-safe; each invocation supplies the agent's own
// effective workDir so isolated workspaces do not run hooks in the foreground.
func (a *Agent) SetHooks(manager *hooks.Manager) {
	a.stateMu.Lock()
	a.hooks = manager
	a.stateMu.Unlock()
}

// RunContext returns the live run context set at the top of the
// current/most-recent Run() call ("" i.e. nil before the first run — callers
// must fall back to a safe default). This is the context a.cancelFunc
// actually kills, unlike a context captured once at construction time.
func (a *Agent) RunContext() context.Context {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.runCtx
}

// safeOnText streams text to the UI in a thread-safe manner.
func (a *Agent) safeOnText(text string) {
	a.stateMu.RLock()
	fn := a.onText
	outputWriter := a.outputWriter
	a.stateMu.RUnlock()
	if fn == nil && outputWriter == nil {
		return
	}
	a.onTextMu.Lock()
	defer a.onTextMu.Unlock()
	if outputWriter != nil {
		outputWriter.WriteString(text)
	}
	if fn == nil {
		return
	}
	func() {
		defer recoverAgentLifecycleCallback("text", a.ID)
		fn(text)
	}()
}

// safeOnThinking streams thinking content to the UI in a thread-safe manner.
func (a *Agent) safeOnThinking(text string) {
	a.stateMu.RLock()
	fn := a.onThinking
	a.stateMu.RUnlock()

	a.onThinkingMu.Lock()
	a.Thought += text // Accumulate thought
	a.onThinkingMu.Unlock()

	if fn == nil {
		return
	}
	a.onThinkingMu.Lock()
	defer a.onThinkingMu.Unlock()
	func() {
		defer recoverAgentLifecycleCallback("thinking", a.ID)
		fn(text)
	}()
}

func (a *Agent) safeOnRateLimit(rl *client.RateLimitMetadata) {
	a.stateMu.RLock()
	fn := a.onRateLimit
	a.stateMu.RUnlock()
	if fn == nil {
		return
	}
	defer recoverAgentLifecycleCallback("rate_limit", a.ID)
	fn(rl)
}

func invokeAgentToolActivity(
	callback func(agentID, toolName string, args map[string]any, status string, success bool, summary string),
	agentID, toolName string,
	args map[string]any,
	status string,
	success bool,
	summary string,
) {
	if callback == nil {
		return
	}
	defer recoverAgentLifecycleCallback("tool_activity", agentID)
	callback(agentID, toolName, args, status, success, summary)
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
	a.stateMu.RLock()
	plan := a.activePlan
	a.stateMu.RUnlock()

	if plan == nil || a.treePlanner == nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "no active plan or tree planner",
		}
	}

	// Find the node in the active plan
	node, ok := plan.GetNode(action.NodeID)
	if !ok {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "node not found in plan",
		}
	}

	a.safeOnText(fmt.Sprintf("\n[Expanding milestone: %s]\n", action.Prompt))

	// Expand the milestone into sub-tasks
	if err := a.treePlanner.ExpandMilestone(ctx, plan, node); err != nil {
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
	if a.delegation == nil || !a.delegation.HasMessenger() {
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
	// For non-delegation actions, run the prompt through the model in a loop
	// until there are no more tool calls (multi-round execution).
	var output strings.Builder
	const maxDirectRounds = 15

	// Add the action prompt to history
	promptContent := genai.NewContentFromText(action.Prompt, genai.RoleUser)
	a.stateMu.Lock()
	a.history = append(a.history, promptContent)
	a.stateMu.Unlock()

	for range maxDirectRounds {
		select {
		case <-ctx.Done():
			return &AgentResult{
				AgentID:   a.ID,
				Type:      a.Type,
				Status:    AgentStatusFailed,
				Error:     ctx.Err().Error(),
				Output:    output.String(),
				Duration:  time.Since(startTime),
				Completed: true,
			}
		default:
		}

		resp, err := a.getModelResponse(ctx)
		if err != nil {
			if errors.Is(err, tools.ErrBudgetExceeded) ||
				errors.Is(err, tools.ErrCostUnavailable) {
				_, terminalOutput, _ := a.finishInvocationBudgetFailure(resp, &output, err)
				return &AgentResult{
					AgentID:   a.ID,
					Type:      a.Type,
					Status:    AgentStatusFailed,
					Error:     err.Error(),
					Output:    terminalOutput,
					Duration:  time.Since(startTime),
					Completed: true,
				}
			}
			return &AgentResult{
				AgentID:   a.ID,
				Type:      a.Type,
				Status:    AgentStatusFailed,
				Error:     err.Error(),
				Output:    output.String(),
				Duration:  time.Since(startTime),
				Completed: true,
			}
		}

		if resp.Text != "" {
			output.WriteString(resp.Text)
		}

		// Add model response to history (required before function responses)
		modelContent := &genai.Content{
			Role:  genai.RoleModel,
			Parts: a.buildResponseParts(resp),
		}
		a.stateMu.Lock()
		a.history = append(a.history, modelContent)
		a.stateMu.Unlock()

		// No tool calls — model is done
		if len(resp.FunctionCalls) == 0 {
			break
		}

		// Execute tools and feed results back to the model
		results := a.executeTools(ctx, resp.FunctionCalls)

		// Track file activity for relevance scoring
		if a.fileTracker != nil {
			a.stateMu.RLock()
			msgIdx := len(a.history)
			a.stateMu.RUnlock()
			for _, fc := range resp.FunctionCalls {
				a.fileTracker.RecordToolCall(fc.Name, fc.Args, msgIdx)
			}
		}

		var funcParts []*genai.Part
		for _, r := range results {
			if r.Response != nil {
				part := genai.NewPartFromFunctionResponse(r.Response.Name, r.Response.Response)
				part.FunctionResponse.ID = r.Response.ID
				funcParts = append(funcParts, part)
				if r.Response.Response != nil {
					if content, ok := r.Response.Response["content"].(string); ok {
						output.WriteString("\n")
						output.WriteString(content)
					}
				}
			}
		}
		funcContent := &genai.Content{
			Role:  genai.RoleUser,
			Parts: funcParts,
		}
		a.stateMu.Lock()
		a.history = append(a.history, funcContent)
		a.stateMu.Unlock()
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

	if a.delegation != nil && a.delegation.HasMessenger() {
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

		// Check for test failures in output (exclude negated forms like "no errors")
		lower := strings.ToLower(response)
		hasFailure := (strings.Contains(lower, "fail") && !strings.Contains(lower, "no fail") && !strings.Contains(lower, "0 fail")) ||
			(strings.Contains(lower, "error") && !strings.Contains(lower, "no error") && !strings.Contains(lower, "0 error") && !strings.Contains(lower, "without error"))
		if hasFailure {
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
	a.stateMu.RLock()
	onInput := a.onInput
	a.stateMu.RUnlock()
	if onInput == nil {
		return nil
	}

	for {
		// Show current plan
		a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
		a.safeOnText("Commands: [Enter] approve | e <n> <prompt> | d <n> | a [type] <prompt> | c cancel\n")
		a.safeOnText("Types: explore, plan, general, bash, decompose (default: general)\n")

		response, err := invokeAgentInput(onInput, a.ID, "Plan approval > ")
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
				node, ok := tree.GetNode(tree.Root.ID)
				if ok && len(node.Children) > 0 {
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

func invokeAgentInput(callback func(string) (string, error), agentID, prompt string) (response string, err error) {
	if callback == nil {
		return "", nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logging.Error("agent input callback panicked",
				"agent_id", agentID,
				"panic", recovered,
				"stack", logging.PanicStack())
			err = fmt.Errorf("agent input callback panicked: %v", recovered)
		}
	}()
	return callback(prompt)
}

func invokeAgentPlanApproved(callback func(string), agentID, summary string) {
	if callback == nil {
		return
	}
	defer recoverAgentLifecycleCallback("plan_approved", agentID)
	callback(summary)
}
