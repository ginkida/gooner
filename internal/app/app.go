package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/genai"

	"gokin/internal/agent"
	"gokin/internal/audit"
	"gokin/internal/cache"
	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/codeintel"
	"gokin/internal/commands"
	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/git"
	"gokin/internal/harness"
	"gokin/internal/hooks"
	"gokin/internal/hybrid"
	"gokin/internal/logging"
	"gokin/internal/loops"
	"gokin/internal/mcp"
	"gokin/internal/memory"
	"gokin/internal/permission"
	"gokin/internal/plan"
	"gokin/internal/ratelimit"
	"gokin/internal/repl"
	"gokin/internal/router"
	"gokin/internal/tasks"
	"gokin/internal/tools"
	"gokin/internal/toolusage"
	"gokin/internal/ui"
	"gokin/internal/undo"
	"gokin/internal/watcher"
)

// hybridRuntime is the narrow lifecycle boundary App owns. Keeping it as an
// interface makes auto-mode wiring testable without weakening repl.NewManager's
// production-only secure-backend requirement.
type hybridRuntime interface {
	Execute(context.Context, string) (repl.Result, error)
	Reset(context.Context) error
	Stats() repl.Stats
	SetCallHandler(repl.CallHandler)
	Close() error
}

const (
	runtimeEngineModeUnset uint32 = iota
	runtimeEngineModeAuto
	runtimeEngineModeTools
	runtimeEngineModeHybrid
)

func normalizeRuntimeEngineMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "tools":
		return "tools"
	case "hybrid":
		return "hybrid"
	default:
		return "auto"
	}
}

func encodeRuntimeEngineMode(mode string) uint32 {
	switch normalizeRuntimeEngineMode(mode) {
	case "tools":
		return runtimeEngineModeTools
	case "hybrid":
		return runtimeEngineModeHybrid
	default:
		return runtimeEngineModeAuto
	}
}

func decodeRuntimeEngineMode(mode uint32) string {
	switch mode {
	case runtimeEngineModeTools:
		return "tools"
	case runtimeEngineModeHybrid:
		return "hybrid"
	default:
		return "auto"
	}
}

// runtimeEngineModeLocked returns and, for minimal test Apps that bypass the
// Builder, seeds the immutable process mode. The caller must hold a.mu whenever
// another goroutine could replace a.config.
func (a *App) runtimeEngineModeLocked() string {
	if code := a.runtimeEngineMode.Load(); code != runtimeEngineModeUnset {
		return decodeRuntimeEngineMode(code)
	}
	configured := "auto"
	if a.config != nil {
		configured = a.config.Engine.Mode
	}
	a.runtimeEngineMode.CompareAndSwap(runtimeEngineModeUnset, encodeRuntimeEngineMode(configured))
	return decodeRuntimeEngineMode(a.runtimeEngineMode.Load())
}

func (a *App) runtimeEngineModeSnapshot() string {
	if a == nil {
		return "auto"
	}
	if code := a.runtimeEngineMode.Load(); code != runtimeEngineModeUnset {
		return decodeRuntimeEngineMode(code)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runtimeEngineModeLocked()
}

var errConfigConflict = errors.New("configuration changed while this update was being prepared; retry the action")

// ErrSessionProviderMismatch marks an exact/continued session whose persisted
// provider history cannot be replayed safely by the current provider.
var ErrSessionProviderMismatch = errors.New("session provider mismatch")

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
	config  *config.Config
	workDir string
	client  client.Client
	// runtimeEngineMode is fixed when this App is assembled. engine.mode
	// determines the physical registry shape and owns the secure Python worker
	// lifecycle, so changing only the config pointer cannot be a truthful live
	// transition. ApplyConfig may persist a different value for the next launch,
	// but every component in this process continues to use this boot value.
	// Zero is reserved for tests/minimal Apps and is seeded once from config.
	runtimeEngineMode atomic.Uint32
	// clientMu is a LEAF lock guarding the `client` field ONLY, so a background
	// goroutine (session-memory onUpdate -> pushTurnContext, the /loop spawner)
	// can read the client without a.mu — which it must NOT take, because some
	// pushTurnContext callers already hold a.mu (re-acquiring self-deadlocks, the
	// v0.100.42 invariant). Lock order is a.mu -> clientMu: the two writers
	// (ApplyConfig, failover) hold a.mu AND take clientMu around the swap; a.mu-
	// guarded readers stay safe via a.mu, lock-free readers take clientMu via
	// currentClientLocked. Never take a.mu while holding clientMu.
	clientMu sync.RWMutex
	registry *tools.Registry
	executor *tools.Executor
	session  *chat.Session
	tui      *ui.Model
	program  *tea.Program
	// programMu guards writes/reads of `program` independently of a.mu.
	// Keeping these mutexes separate matters: safeSendToProgram takes
	// programMu, but any caller may already hold a.mu (e.g. ApplyConfig
	// does so for the whole function). Before this split, both reads went
	// through a.mu and ApplyConfig self-deadlocked on the non-reentrant
	// mutex — visible to users as `/provider deepseek` hanging with
	// "Generating 11.8s" forever.
	programMu sync.RWMutex
	// programSendOrderMu orders synchronous worker sends together with the
	// non-blocking sends reserved by callbacks running inside Bubble Tea's
	// Update. Program.Send uses an unbuffered channel: a callback must return to
	// the event loop before that loop can receive its reply. Each reservation
	// publishes a completion channel as the next sender's predecessor, keeping
	// message order deterministic without re-entering Update.
	programSendOrderMu sync.Mutex
	programSendTail    <-chan struct{}

	// Application context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Context management
	projectInfo    *appcontext.ProjectInfo
	contextManager *appcontext.ContextManager
	// tokenRefreshTimeout bounds the count_tokens call in refreshTokenCount.
	// Zero means the 15s default; tests override it to drive the timeout path.
	tokenRefreshTimeout time.Duration
	promptBuilder       *appcontext.PromptBuilder
	contextAgent        *appcontext.ContextAgent
	// runSystemPromptMu guards invocation-scoped prompt customization. The
	// canonical/default instruction remains in chat.Session; only the active
	// provider client receives the composed replacement/append value. Keeping
	// these separate prevents CLI-only instructions and secrets from leaking
	// into resumable session files.
	runSystemPromptMu          sync.RWMutex
	runSystemPromptReplacement string
	runSystemPromptReplace     bool
	runSystemPromptAppend      string
	runStructuredOutputPrompt  string

	// hookFailures tracks per-hook failing streaks so a broken user hook
	// toasts ONCE per streak, not once per tool call (see hooks_visibility.go).
	// Self-locking leaf state — never held with a.mu.
	hookFailures hookFailureTracker
	// persistenceFailures applies the same once-per-streak anti-spam contract to
	// hot-path plan saves. A plan can save after every mutating tool call, so a
	// broken directory must be visible without producing one toast per edit.
	persistenceFailures hookFailureTracker

	// Permission management
	permManager *permission.Manager
	// permPending correlates each in-flight promptPermission call to its OWN
	// response channel, keyed by a unique request ID. A single shared channel
	// used to let a concurrent second permission prompt's auto-deny (tui.go's
	// "don't overwrite an active modal, auto-deny the new one" guard) resolve
	// the WRONG — still-displayed, user-visible — request whenever two
	// prompts were in flight at once (e.g. the coordinate tool's parallel
	// sub-agents each hitting a LevelAsk tool). permPendingMu is a leaf lock —
	// never held while acquiring a.mu.
	permPendingMu sync.Mutex
	permPending   map[string]chan permission.Decision
	permReqSeq    atomic.Int64

	// Question handling. Each waiter owns a channel keyed by request ID; a late
	// answer for an expired prompt is dropped instead of leaking into the next
	// ask_user call.
	questionPendingMu sync.Mutex
	questionPending   map[string]chan string
	questionReqSeq    atomic.Int64

	// Diff preview handling
	diffResponseChan      chan ui.DiffDecision
	multiDiffResponseChan chan map[string]ui.DiffDecision
	diffBatchDecision     ui.DiffDecision
	diffPendingMu         sync.Mutex
	diffPending           map[string]chan ui.DiffDecision
	diffReqSeq            atomic.Int64
	multiDiffPendingMu    sync.Mutex
	multiDiffPending      map[string]chan map[string]ui.DiffDecision
	multiDiffReqSeq       atomic.Int64

	// Plan management
	planManager *plan.Manager
	// Plan approvals use the same per-request ownership as permissions and
	// questions. The response keeps UI intent/feedback intact; side effects are
	// applied by the correlated waiter against its own *plan.Plan.
	planPendingMu sync.Mutex
	planPending   map[string]chan planApprovalResponse
	planReqSeq    atomic.Int64

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
	totalInputTokens         int
	totalOutputTokens        int
	totalCacheCreationTokens int
	totalCacheReadTokens     int
	totalEstimatedCost       float64
	costTracked              bool

	// Response metadata tracking
	responseStartTime    time.Time
	responseToolsUsed    []string
	responseTouchedPaths []string
	responseCommands     []string
	responseEvidence     responseEvidenceLedger

	// Session persistence
	sessionManager *chat.SessionManager
	// sessionLeaseMu serializes ownership changes for the active persisted
	// session. A switch holds both the old and target writer leases until the
	// old state is synchronously flushed and the target state is restored.
	// Callers must not enter these methods while holding a.mu; the switch may
	// briefly take a.mu to publish session-derived scratchpad state.
	sessionLeaseMu sync.Mutex
	sessionLease   *chat.SessionWriterLease

	// New feature integrations
	searchCache *cache.SearchCache
	rateLimiter *ratelimit.Limiter
	auditLogger *audit.Logger
	// toolUsage is the LIFETIME per-tool invocation counter. Unlike
	// toolMetrics it is deliberately NOT reset by /clear — its whole
	// purpose is to answer whether a tool has ever been reached for.
	toolUsage   *toolusage.Ledger
	fileWatcher *watcher.Watcher
	// codeIntelProvider owns the lazy, workspace-scoped gopls process. It is
	// separate from user MCP servers and must be closed explicitly.
	codeIntelProvider codeintel.ReadOnlyProvider
	// replManager owns the foreground session's isolated Python kernel. It is
	// never shared with cloned sub-agent registries.
	replManager hybridRuntime
	// deferredHybrid keeps auto mode process-free until a request is actually
	// eligible for the computation plane.
	deferredHybrid *deferredHybridInit
	// runtimeREPLCapabilityDisabled records the early invocation decision that
	// let Builder omit repl_exec before the final registry ceiling existed. It
	// is distinct from runtime absence after an auto-mode lazy sandbox probe fails.
	runtimeREPLCapabilityDisabled bool
	// harnessStore owns bounded session prompt patches plus project-scoped
	// episodic memory and inert skill proposals. It cannot mutate policy.
	harnessStore *harness.Store

	// Task router for intelligent task routing
	taskRouter *router.Router

	// Agent Scratchpad (shared)
	scratchpad string

	// Unified Task Orchestrator
	orchestrator *TaskOrchestrator
	reliability  *ReliabilityManager
	policy       *PolicyEngine
	phaseMetrics *PhaseMetrics
	toolMetrics  *ToolMetrics

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
	mcpManager        *mcp.Manager
	mcpInitialSummary string // One-shot toast describing initial MCP connect results
	// mcpMutationMu is an outer transaction lock for runtime add/remove. It
	// covers manager + registry + client tool declarations + config commit as
	// one ordered operation; status/list calls never take it. Never acquire it
	// while holding a.mu because mutation helpers acquire a.mu through App APIs.
	mcpMutationMu sync.Mutex

	// Loops (autonomous recurring task system, v0.81+).
	// Initialized in builder.go after configDir is known. The actual
	// background scheduler is started by App.Run; nil-safe everywhere
	// in case the loop subsystem is disabled or not yet wired.
	loopManager *loops.Manager
	loopRunner  *loops.Runner
	loopMemory  *loops.MemoryWriter // human-readable per-loop markdown

	// headlessDirect makes processMessageWithContext execute through the
	// plain executor instead of the task router — a determinism POLICY for
	// headless/eval runs (post-unification, routed output and journaling
	// work too; see headless.go). Guarded by a.mu.
	headlessDirect bool
	// Policy callbacks are shared with interactive execution. These fields
	// latch fail-closed outcomes only while one RunHeadless invocation is active,
	// preventing a denied tool call, recovered panic, or failed finalization gate
	// from being masked by later model prose. Guarded by a.mu.
	headlessRunActive     bool
	headlessPolicyFailure *headlessPolicyFailure
	headlessTerminal      *headlessTerminalOutcome
	headlessFinalResult   string
	// Invocation-scoped accounting is separate from the session ledger. The
	// latter intentionally treats metadata-free input estimates as a lower
	// bound, which is correct for /stats but would make a second headless turn
	// report zero input. These fields add each completed model exchange while
	// the active headless token owns the foreground. Guarded by a.mu.
	headlessInvocationUsage HeadlessUsage
	headlessInvocationCost  HeadlessCost
	// headlessAgentUsageScope is the exact token inherited by agents spawned
	// from the current headless context. owner ties the lazily-created scope to
	// headlessTerminal's per-run pointer so a later invocation cannot reuse it.
	headlessAgentUsageScope      agent.InvocationScope
	headlessAgentUsageScopeOwner *headlessTerminalOutcome
	headlessCostIncomplete       bool
	// toolCapabilityCeiling is the exact per-process CLI capability set.
	// restricted distinguishes an explicit empty --tools set from no ceiling.
	// Schema filtering and executor runtime enforcement both consume it;
	// delegated agents inherit its intersection. Guarded by a.mu.
	toolCapabilityCeiling    []string
	toolCapabilityRestricted bool
	// The raw CLI inputs are retained so the ceiling can be recomputed against
	// a registry that changes at runtime (MCP connect/reconnect/tools_changed).
	// Without that, a deny-only run would freeze the ceiling to the boot-time
	// tool list and permanently block every later-registered MCP tool the user
	// never denied. A nil allow input means "everything the registry has minus
	// the denies"; a non-nil one is an exact set that must NOT grow.
	toolCapabilityAllowInput []string
	toolCapabilityDenyInput  []string

	// Discuss-mode (discuss_mode.go): flags for the foreground "don't jump to
	// implementation during analysis" gate. turnDiscuss is set at turn start
	// (processMessageWithContext) from the intent classifier; discussConfirmed
	// flips when the user OKs the first mutation via actionConfirmPrompt.
	// discussGate() = turnDiscuss && !discussConfirmed drives both the executor
	// Step 4.7 gate and the incomplete-work nudge suppression. atomic.Bool (not
	// plain + a.mu): discussGate() is read by the executor deep in the turn
	// pipeline where taking a.mu is unsafe, AND the banner in turnContextContent
	// can be read off-turn (session-memory callback) — atomics give a race-free,
	// lock-free, deadlock-free read on every path. See discuss_mode.go.
	turnDiscuss atomic.Bool
	// turnToolBudgetHit is set by the executor's OnToolBudgetExhausted
	// callback (executor goroutine) and consumed at end of turn; the
	// budgetAutoContinues streak is capped per REAL user request.
	turnToolBudgetHit   atomic.Bool
	budgetAutoContinues int32
	discussConfirmed    atomic.Bool
	// structuredCorrectionActive marks the tool-free JSON-format correction
	// turns. Those run under an empty capability ceiling that Gokin itself
	// installs, so a tool call during them is refused by OUR policy — recording
	// that as the invocation's policy failure reported `policy_blocked` to the
	// user and discarded an answer the next correction might well have fixed.
	structuredCorrectionActive atomic.Bool

	// presenter is WHERE agent output goes (agent_events.go). The builder
	// installs the TUI presenter; RunHeadless swaps in the stdout presenter.
	// The execution handler is built ONCE and resolves this per event.
	// Guarded by a.mu.
	presenter agentPresenter

	// stopHookActive marks the next turn as a Stop-hook-driven continuation:
	// stop hooks are skipped at that turn's end so a hook that always fails
	// cannot loop the agent forever (one continuation per user turn).
	// Guarded by a.mu.
	stopHookActive bool

	// Streaming token estimation
	streamedChars           int // Accumulated chars during current streaming session
	streamedEstimatedTokens int // Accumulated estimated tokens during current streaming session

	// Session Memory
	sessionMemory *appcontext.SessionMemoryManager
	workingMemory *appcontext.WorkingMemoryManager

	// Persistent stores (for flush on shutdown)
	memoryStore      *memory.Store
	memoryAutoInject atomic.Bool
	// memoryAllowGlobal is separate from auto-inject: the latter controls
	// whether memory enters context at all, while this flag is the explicit
	// cross-project opt-in used by per-turn retrieval.
	memoryAllowGlobal atomic.Bool
	// relevantMemoryContext is query-aware durable recall for the current turn.
	// It has its own leaf lock because session-memory callbacks can rebuild the
	// client turn context while another goroutine starts or clears a turn; those
	// paths must never re-enter a.mu (same invariant as clientMu/grantedDirsMu).
	relevantMemoryMu      sync.RWMutex
	relevantMemoryContext string
	errorStore            *memory.ErrorStore
	exampleStore          *memory.ExampleStore

	// Pattern detection throttling
	knownPatterns     map[string]bool
	lastPatternNotify time.Time

	// === Task 5.7: Project Context Auto-Injection ===
	detectedProjectContext string // Computed once at startup

	// === Task 5.8: Tool Usage Pattern Learning ===
	toolPatterns []toolPattern // Detected repeating tool sequences
	recentTools  []string      // Last 20 tool names used
	messageCount int           // Total messages processed (for periodic hint injection)

	// Current tool context for progress bar display
	currentToolContext string

	// Error context for retry awareness
	lastError     string    // Last error message for context on retry
	lastErrorTime time.Time // When the last error occurred

	// Lock ordering (must be acquired in this order to prevent deadlock):
	//   0. mcpMutationMu / sessionModeCycleMu / sessionGovernanceMu (independent outer transactions;
	//      neither is ever acquired under mu)
	//   1. mu
	//   2. processingMu
	//   3. pendingMu
	//   4. rateLimitRetryMu
	//   5. stepHeartbeatMu (RWMutex, prefer RLock)
	//   6. stepRollbackMu
	//   7. sessionArchiveMu
	//   8. diffPromptMu
	//   9. grantedDirsMu (independent leaf — never held while acquiring a.mu;
	//      applyGrantedDirsToTools snapshots under it, releases, THEN reads a.mu)
	// Never hold a later lock while acquiring an earlier one.
	// sessionModeCycleMu keeps every accepted Shift+Tab/palette cycle as one
	// read-current -> apply-next transaction. It is deliberately outside mu:
	// applySessionMode delegates to toggle methods which acquire mu themselves.
	sessionModeCycleMu sync.Mutex
	mu                 sync.Mutex
	// configRevision is guarded by mu and assigned while the corresponding
	// config snapshot is committed. It therefore records mutation order, not
	// the later scheduler-dependent order of Bubble Tea sends.
	configRevision uint64
	diffPromptMu   sync.Mutex

	// Session-only directory grants (Claude-Code-style /add-dir + ask-on-access).
	// NOT persisted unless the user passes --persist (then also in
	// config.Tools.AllowedDirs). Reset on /clear. Guarded by grantedDirsMu.
	grantedDirs   []string
	grantedDirsMu sync.Mutex
	// dirCtxSnapshot is the effective allowed-dir list (config + session grants)
	// cached for the model's turn-context block. Read by directoryAccessContext
	// under grantedDirsMu ONLY — never a.mu — so it can be assembled even when a
	// caller of pushTurnContext already holds a.mu (turnContextContent must not
	// re-acquire a.mu, or it self-deadlocks). Refreshed by applyGrantedDirsToTools.
	dirCtxSnapshot []string

	running    bool
	processing bool // Guards against concurrent message processing
	// shuttingDown closes foreground admission before graceful shutdown starts.
	// It is guarded by mu. foregroundWorkers then joins every turn that crossed
	// that admission boundary before session/client teardown.
	shuttingDown      bool
	foregroundWorkers GoroutineTracker
	// dropSteerLeftovers is set by explicit cancellation and cleared only when
	// a new foreground operation is accepted. It is guarded by mu and provides
	// defense in depth for embedders that invoke the execution handler directly.
	dropSteerLeftovers bool

	// Processing cancellation for ESC interrupt
	processingCancel context.CancelFunc
	processingMu     sync.Mutex

	// Plan execution watchdog
	stepHeartbeatMu   sync.RWMutex
	lastStepHeartbeat time.Time

	// inFlightDelegatedSteps counts currently-running executeDelegatedStep
	// calls (round 6). Delegated plan steps can run IN PARALLEL when 2+ are
	// simultaneously ready (message_processor.go's non-safe-mode branch), in
	// which case planManager.GetCurrentStepID() — a single shared field — is
	// ambiguous: it reflects whichever step goroutine most recently called
	// SetCurrentStepID, not necessarily the step whose sub-agent activity is
	// being reported right now. handleSubAgentActivity uses this counter to
	// gate step-effect recording (RunLedger/rollback) to ONLY the safe case
	// — exactly one delegated step in flight — rather than risk attributing
	// a tool call to the wrong step under parallel execution. The heartbeat
	// touch itself doesn't need this gating (it's a coarse liveness signal
	// the watchdog only consults when a plan is executing at all).
	inFlightDelegatedSteps atomic.Int32

	// Step rollback snapshots (rollback-first guardrail)
	stepRollbackMu        sync.Mutex
	stepRollbackSnapshots map[string]*stepRollbackSnapshot

	// Execution journal and recovery
	journal *ExecutionJournal

	// Session memory governance
	sessionGovernanceMu      sync.Mutex
	sessionArchiveMu         sync.Mutex
	sessionArchivedMessages  int
	sessionArchiveOperations int
	lastSessionArchive       time.Time

	// Signal handler cleanup
	signalCleanup func()

	// Session pre-load flag (set by ResumeLastSession before Run)
	sessionPreloaded bool

	// Type-ahead pending queue (FIFO, bounded) — see pending_queue.go for the
	// accessors; never touch the slice directly.
	pendingQueue []pendingRequest
	pendingMu    sync.Mutex
	// recoveryEpoch invalidates a retry persistence operation that began before
	// /clear established a new conversation root but reached the lease boundary
	// afterwards.
	recoveryEpoch atomic.Uint64
	// recoveryTimerMu owns the one-live-timer-per-durable-generation registry.
	// It is an independent leaf lock: timer helpers never hold it while acquiring
	// sessionLeaseMu or any of the application locks listed above.
	recoveryTimerMu sync.Mutex
	recoveryTimers  map[recoveryTimerKey]*recoveryTimerRegistration
	// recoveryAwaitingDispatch contains only claims made by this process which
	// have not yet started or entered the FIFO. Persisted claimed records absent
	// from this map remain ambiguous and are never released automatically.
	recoveryAwaitingDispatch map[recoveryTimerKey]struct{}

	// Automatic retry tracking for rate-limit failures.
	rateLimitRetryMu    sync.Mutex
	rateLimitRetryCount map[string]int

	// Auto-resume tracking for timeout/retry-exhausted errors.
	// When the agent hits a model round timeout or exhausts its in-loop retry
	// budget, it compacts the context and retries — up to maxAutoResumeAttempts
	// times. This prevents the "agent stopped at 14m with GLM" and "same error
	// repeated (4x)" failures on long-running tasks.
	autoResumeMu    sync.Mutex
	autoResumeCount map[string]int
}

// toolPattern, detectPatterns, getToolHints, recordToolUsage are in pattern_detector.go
// detectProjectContext, extractGoModInfo, extractPackageJSONInfo, readFirstLines, fileExists, readFileHead are in project_detector.go

// New creates a new application instance.
func New(cfg *config.Config, workDir string) (*App, error) {
	return NewBuilder(cfg, workDir).Build()
}

// NewWithOptions creates an application with explicit startup behavior. Use
// NonInteractive for headless/automation entry points so construction cannot
// pause for input or start an implicit model download.
func NewWithOptions(cfg *config.Config, workDir string, options BuildOptions) (*App, error) {
	return NewBuilderWithOptions(cfg, workDir, options).Build()
}

// markOnboardingWelcomeSeen persists the first-launch flag only after the TUI
// has real geometry and the user interacts with the visible welcome surface.
// A failed save restores the in-memory flag so the introduction is offered on
// the next launch instead of being lost permanently.
func (a *App) markOnboardingWelcomeSeen() {
	a.mu.Lock()
	if !a.config.UI.ShowWelcome {
		a.mu.Unlock()
		return
	}
	a.config.UI.ShowWelcome = false
	err := a.config.Save()
	if err != nil {
		a.config.UI.ShowWelcome = true
	} else {
		a.nextConfigRevisionLocked()
	}
	a.mu.Unlock()

	if err != nil {
		logging.Warn("failed to persist onboarding welcome state", "error", err)
	}
}

// Run starts the application.
func (a *App) Run() error {
	return a.RunWithInitialPrompt("")
}

// RunWithInitialPrompt starts the interactive application and submits an
// optional first user message through the same UI/ownership pipeline as Enter.
func (a *App) RunWithInitialPrompt(initialPrompt string) error {
	// Interactive runs own the active session lease for their full lifetime,
	// including any runtime /resume switches. Keep this defer at the outermost
	// boundary so early returns and panics do not strand an in-process lease.
	defer func() {
		if releaseErr := a.ReleaseSessionWriterLease(); releaseErr != nil {
			logging.Warn("failed to release session writer lease on app exit", "error", releaseErr)
		}
	}()

	// NOTE: Allowed dirs prompt is now done in Builder.checkAllowedDirs()
	// before tool creation, so PathValidator gets correct directories.

	// Configure logging to file to avoid TUI interference
	configDir, err := appcontext.GetConfigDir()
	if a.config.Debug {
		// CLI debug logging is configured before Builder construction so it
		// also captures startup. Do not replace its explicit sink here.
	} else if err == nil && a.config.Logging.Level != "" {
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
	if !a.config.Bare {
		a.detectedProjectContext = a.detectProjectContext()
		if a.detectedProjectContext != "" && a.promptBuilder != nil {
			a.promptBuilder.SetDetectedContext(a.detectedProjectContext)
			logging.Debug("project context auto-detected", "length", len(a.detectedProjectContext))
		}
	}

	// Sync plan-mode signal into prompt builder, tool schema, and TUI
	// status bar. Built once here (not on every Toggle) because startup
	// is the only place where the initial state needs to be pushed without
	// a prior toggle event. Subsequent flips go through TogglePlanningMode
	// which handles all three surfaces.
	if a.promptBuilder != nil {
		a.promptBuilder.SetPlanMode(a.planningModeEnabled)
	}
	if a.client != nil {
		a.client.SetTools(a.toolsForCurrentMode())
	}
	if a.tui != nil {
		a.tui.SetPlanningModeEnabled(a.planningModeEnabled)
	}

	// Run on_start hooks with proper context
	if a.hooksManager != nil {
		a.hooksManager.RunOnStart(a.ctx)
	}

	// Load input history
	if !a.config.Bare {
		if err := a.tui.LoadInputHistory(); err != nil {
			logging.Debug("failed to load input history", "error", err)
		}
	}

	// Auto-load previous session if enabled (skip if already pre-loaded via
	// ResumeLastSession). For recent sessions (< autoResumeCutoff), restore
	// automatically — matches the common case of "closed terminal, reopened
	// to continue". For older sessions, we only hint at availability so
	// unrelated work from days ago doesn't surprise-load into a fresh task.
	const autoResumeCutoff = 12 * time.Hour
	var sessionRestored bool
	if a.sessionPreloaded {
		sessionRestored = true
	} else if a.sessionManager != nil && !a.config.Bare {
		// Bound startup auto-resume: LoadLast reads + parses session metadata
		// from disk; a pathological session dir (thousands of files / a hung
		// network FS) must not stall cold start unboundedly. Cap it at 5s; on
		// timeout skip auto-resume (the user can /resume). The buffered channel
		// means the worker never blocks even after we move on, so it can't leak.
		type loadLastResult struct {
			state *chat.SessionState
			info  *chat.SessionInfo
			err   error
		}
		llCh := make(chan loadLastResult, 1)
		a.safeGo("startup-loadlast", func() {
			s, i, e := a.sessionManager.LoadLast()
			llCh <- loadLastResult{s, i, e}
		})
		var state *chat.SessionState
		var info *chat.SessionInfo
		var err error
		select {
		case r := <-llCh:
			state, info, err = r.state, r.info, r.err
		case <-time.After(5 * time.Second):
			logging.Warn("session auto-resume skipped: LoadLast exceeded 5s")
			err = context.DeadlineExceeded
		}
		if err == nil && state != nil && len(state.History) > 0 {
			age := time.Since(info.LastActive)
			// Cross-provider guard: a session tagged with a different
			// provider than the current active one will 400 on first
			// request because wire-format details (thinking signatures,
			// tool_use ID shapes, cache_control markers) don't round-
			// trip across providers. Empty state.Provider = legacy
			// session from before v0.71.4; treat as compatible.
			currentProvider := runtimeProviderForConfig(a.config)
			providerMismatch := state.Provider != "" && currentProvider != "" && state.Provider != currentProvider

			if age > autoResumeCutoff {
				// Too old for surprise restore — surface a hint so the user
				// can explicitly /resume it if they want.
				a.tui.AddSystemMessage(fmt.Sprintf(
					"Previous session available from %s (%d messages). Use /resume %s to load it.",
					humanizeAge(age), len(state.History), info.ID))
			} else if providerMismatch {
				// Don't auto-load a session built for a different provider.
				// Surface what we skipped and let the user explicitly
				// /resume if they want to attempt the conversion.
				a.tui.AddSystemMessage(fmt.Sprintf(
					"Skipped auto-resume: previous session was on %s but you're now on %s (history formats are incompatible). Use /resume %s to try anyway, or /clear to stay fresh.",
					state.Provider, currentProvider, info.ID))
			} else if restoredState, restoreErr := a.SwitchSession(a.ctx, state, false); restoreErr != nil {
				logging.Warn("failed to restore session", "session_id", info.ID, "error", restoreErr)
				if errors.Is(restoreErr, chat.ErrSessionWriterLeaseBusy) {
					a.tui.AddSystemMessage(fmt.Sprintf(
						"Skipped auto-resume: session %s is already open in another Gokin process. Started a fresh protected session instead.",
						info.ID))
				}
			} else {
				state = restoredState
				sessionRestored = true
				// Notify user about restored session
				a.tui.AddSystemMessage(fmt.Sprintf("Restored session from %s (%d messages)",
					humanizeAge(age), len(state.History)))
			}
		}
	}

	// After session restore, check if context exceeds model limits and compact if needed.
	// Some models (MiniMax, weak models) silently return empty responses on context overflow
	// instead of returning a 400 error, so we must proactively manage context size.
	if sessionRestored && a.contextManager != nil {
		history := a.session.GetHistory()
		tokens := appcontext.EstimateContentsTokens(history)
		limits := appcontext.GetModelLimits(a.config.Model.Name)
		if limits.MaxInputTokens > 0 && tokens > int(float64(limits.MaxInputTokens)*0.8) {
			before := len(history)
			truncated := a.contextManager.EmergencyTruncate()
			if truncated > 0 {
				logging.Info("compacted restored session to fit model context",
					"messages_before", before,
					"messages_after", len(a.session.GetHistory()),
					"tokens_estimated", tokens,
					"model_limit", limits.MaxInputTokens)
				a.tui.AddSystemMessage(fmt.Sprintf("Compacted session to fit model context (removed %d messages)", truncated))
			}
		}
	}

	// Set system instruction via native API parameter (not as user message).
	if sessionRestored {
		// Restored session: clean up legacy system prompt messages from history
		a.stripLegacySystemMessages()
	}
	// Plan mode and run-scoped customization rebuild the current canonical
	// prompt; an ordinary resume keeps its saved prompt for compatibility.
	a.applyStartupSystemInstruction(sessionRestored)

	// Deliver any disk-restored working memory as per-turn context (it is
	// NOT part of the system prompt — see roadmap #7 / pushTurnContext).
	a.pushTurnContext()

	// Start session manager for periodic saves
	if a.sessionManager != nil {
		// Surface a background-autosave failure the first time a failing streak
		// starts — previously Warn-log only, so a full disk / unwritable session
		// dir silently stopped persisting history and the user only found out
		// after restart. Fires at most once per streak (SessionManager resets on
		// the next successful save), so it can't spam a per-message toast.
		a.sessionManager.SetOnSaveFailed(func(saveErr error) {
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusWarning,
				Message: sessionSaveFailureMessage(saveErr),
			})
			logging.Warn("session autosave failing (surfaced to UI)", "error", saveErr)
		})
		a.sessionManager.Start(a.ctx)
	}

	// Save session on panic before re-panicking. Without the logging here a
	// fatal panic would leave the user with: 1) no clue what crashed (just
	// the runtime's panic dump on stderr), and 2) potentially no session
	// either (if Save itself failed silently). We log both before re-raising
	// so the same crash is also captured in the gokin log file.
	defer func() {
		if r := recover(); r != nil {
			stack := make([]byte, 8192)
			length := runtime.Stack(stack, false)
			logging.Error("app run panic — attempting session save before crashing",
				"panic", r, "stack", string(stack[:length]))
			if a.sessionManager != nil {
				if saveErr := a.sessionManager.Save(); saveErr != nil {
					logging.Error("session save failed during panic recovery — unsaved work may be lost",
						"error", saveErr, "panic", r)
				}
			}
			panic(r)
		}
	}()

	// Show one-time onboarding welcome on first launch.
	if a.config.UI.ShowWelcome {
		a.tui.ShowFirstLaunchWelcome(func() {
			a.safeGo("persist-onboarding-welcome", a.markOnboardingWelcomeSeen)
		})
		a.tui.AddSystemMessage("Type a message to get started, or try a command:\n• /help — see all available commands\n• /doctor — verify your setup\n• /quickstart — guided examples")
	}

	// Post-upgrade banner: when LastSeenVersion differs from the
	// current build, show a one-line "what version you're now on"
	// notice and point the user at /whats-new for the release notes.
	// First-install (LastSeenVersion=="") is silent — the welcome
	// banner already handles introductions; doubling up would feel
	// noisy. We persist the new value regardless so the same banner
	// doesn't fire twice for a single upgrade.
	currentVersion := strings.TrimSpace(a.config.Version)
	if currentVersion != "" && a.config.UI.LastSeenVersion != currentVersion {
		if a.config.UI.LastSeenVersion != "" {
			direction := "↑ Upgraded"
			// Best-effort downgrade detection — semver-aware compare
			// would be cleaner but `strings.Compare` is good enough
			// for the user-facing copy: a downgrade from v0.74.1 to
			// v0.74.0 reads as "Downgraded" because string-order
			// matches semver order for the gokin "MAJOR.MINOR.PATCH"
			// range we ship.
			if a.config.UI.LastSeenVersion > currentVersion {
				direction = "↓ Downgraded"
			}
			a.tui.AddSystemMessage(fmt.Sprintf(
				"%s from %s to %s — run /whats-new to see release notes",
				direction, a.config.UI.LastSeenVersion, currentVersion))
		}
		a.config.UI.LastSeenVersion = currentVersion
		if err := a.config.Save(); err != nil {
			logging.Warn("failed to persist last-seen version", "error", err)
		}
	}

	a.journalEvent("app_started", map[string]any{
		"workdir": a.workDir,
	})

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

	// Show recovery hint if previous run was interrupted mid-processing.
	if a.journal != nil {
		if snap, err := a.journal.LoadRecovery(); err == nil && snap != nil && snap.Processing {
			age := time.Since(snap.Timestamp)
			msg := fmt.Sprintf("⚠️  Previous session was interrupted %s (%d messages). Run /recovery to review, /journal to see the last events.",
				humanizeAge(age), snap.HistoryLen)
			a.tui.AddSystemMessage(msg)
		}
	}

	// Auto-resume agents from error checkpoints (silent, debug log only)
	if a.agentRunner != nil {
		a.agentRunner.ResumeErrorCheckpoints(a.ctx)
	}

	// Create and run the program. Publish the program reference under
	// programMu (not a.mu) so safeSendToProgram can observe it from
	// goroutines that may already hold a.mu (e.g., ApplyConfig).
	a.tui.SetInitialPrompt(initialPrompt)
	a.programMu.Lock()
	a.program = a.tui.GetProgram()
	a.programMu.Unlock()

	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	// Only now is the restored session fully selected and the UI program
	// published. Resume durable scheduled retries with their exact checkpoint
	// lineage; claimed entries remain fail-closed for manual inspection.
	a.resumePersistedRecoveries(true)

	// === PHASE 4: Initialize UI Auto-Update System ===
	a.initializeUIUpdateSystem()

	// Start background processes
	if a.orchestrator != nil {
		a.safeGo("orchestrator", func() { a.orchestrator.Start(a.ctx) })
	}
	if a.contextAgent != nil {
		a.safeGo("context-agent", func() { a.contextAgent.Start(a.ctx) })
	}

	// Start the loops scheduler. The Runner.Start method spawns its
	// own goroutine internally and exits cleanly on a.ctx.Done() —
	// no extra safeGo wrap needed, but Runner.run() does its own
	// recover + PanicStack logging per CLAUDE.md reliability invariants.
	if a.loopManager != nil && a.loopRunner == nil && a.agentRunner != nil {
		spawner := newLoopSpawner(a)
		a.loopRunner = loops.NewRunner(a.loopManager, spawner, a.isLoopRunnerIdle)
		// Only this workspace's loops may fire here — every iteration is
		// spawned against a.workDir, so another project's loop would run an
		// unattended agent in the wrong repository.
		a.loopRunner.SetWorkDir(a.workDir)
		a.loopRunner.SetIterationStartHook(a.onLoopIterationStart)
		a.loopRunner.SetIterationDoneHook(a.onLoopIterationDone)
		a.loopRunner.SetIterationPersistFailedHook(a.onLoopIterationPersistFailed)
		// Wire markdown cleanup on /loop remove so we don't leak
		// orphaned .gokin/loops/<id>.md files.
		if a.loopMemory != nil {
			a.loopManager.SetOnRemove(func(id string) {
				if err := a.loopMemory.DeleteLoop(id); err != nil {
					logging.Warn("loops: failed to delete markdown on remove",
						"loop_id", id, "error", err)
				}
			})
		}
		a.loopRunner.Start(a.ctx)

		a.announceRestoredLoops()
	}

	// Set app reference in TUI for data providers
	a.tui.SetApp(a)

	// Set up signal handling for graceful shutdown
	a.signalCleanup = a.setupSignalHandler()

	// Start periodic task cleanup goroutine
	a.safeGo("periodic-cleanup", func() {
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
				// Clean up old agent results and their output files from disk
				if a.agentRunner != nil {
					if cleaned := a.agentRunner.Cleanup(30 * time.Minute); cleaned > 0 {
						logging.Debug("cleaned up agent results and output files", "count", cleaned)
					}
					a.agentRunner.CleanupOldCheckpoints(24 * time.Hour)
				}
				// Clean up old plan files (completed: 7 days, paused: 21 days)
				if a.planManager != nil {
					if cleaned, err := a.planManager.CleanupOldPlans(7 * 24 * time.Hour); err == nil && cleaned > 0 {
						logging.Debug("cleaned up old plans", "count", cleaned)
					}
				}
			case <-a.ctx.Done():
				return
			}
		}
	})

	// Deferred MCP summary: the UI wasn't running yet when ConnectAll completed
	// in the builder, so we replay the result as a toast once the program loop
	// is ready to receive it. Short wait keeps the toast from racing the splash.
	if a.mcpInitialSummary != "" {
		summary := a.mcpInitialSummary
		a.mcpInitialSummary = ""
		a.safeGo("mcp-initial-summary", func() {
			select {
			case <-time.After(800 * time.Millisecond):
			case <-a.ctx.Done():
				return
			}
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusInfo,
				Message: summary,
			})
		})
	}

	// Start file watcher if enabled
	if a.fileWatcher != nil {
		a.fileWatcher.SetOnFileChange(func(path string, op watcher.Operation) {
			// Invalidate cache on file changes. a.searchCache is written
			// under a.mu by ApplyConfig (/set searchcache on, /model,
			// /provider, ...) — this callback runs on the watcher's own
			// background goroutine, so it must snapshot under the same lock
			// rather than reading the field bare.
			a.mu.Lock()
			sc := a.searchCache
			a.mu.Unlock()
			if sc != nil {
				sc.InvalidateByPath(path)
			}
		})
		if err := a.fileWatcher.Start(); err != nil {
			logging.Warn("failed to start file watcher", "error", err)
		}
	}

	_, runErr := a.program.Run()

	// Bubble Tea invokes handleQuit from inside Model.Update. Waiting for
	// foreground workers there deadlocks because those workers may be blocked in
	// Program.Send until Update returns. The callback only closes admission and
	// cancels work; perform the blocking teardown after the event loop exits.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
	a.gracefulShutdown(shutdownCtx)
	shutdownCancel()

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

// prepareSteerMessage decides whether a message typed while a request is in
// flight may be STEERED into the current turn, and returns the steer-ready
// text. Two rules:
//   - Slash commands are NEVER steered: they must EXECUTE, not become
//     model-visible text ("[user follow-up] /tasks"). Queued instead — the
//     dequeue path re-enters handleSubmit with processing=false, which parses
//     and runs them as commands (the pre-steering behavior).
//   - @file references are expanded HERE, mirroring the normal-path
//     expandAtReferences call — the steer path returns early and would
//     otherwise hand the model a bare unresolved @token.
func (a *App) prepareSteerMessage(message string) (string, bool) {
	if a.commandHandler != nil {
		if _, _, isCmd := a.commandHandler.Parse(message); isCmd {
			return "", false
		}
	}
	return a.expandAtReferences(message), true
}

// handleSubmit handles user message submission.
func (a *App) handleSubmit(message string) {
	a.handleSubmitWithIntent(message, true)
}

// handleSubmitWithIntent distinguishes a live user action from a programmatic
// retry. While a cancelled foreground is still unwinding, explicit input is
// queued with provenance instead of reopening the gate; a late timer/resubmit
// remains blocked by that cancellation boundary.
func (a *App) handleSubmitWithIntent(message string, explicitUser bool) {
	if explicitUser {
		// A REAL user message resets the tool-budget auto-continue streak —
		// the cap is per user request, not per session.
		a.resetBudgetAutoContinueOnUserInput(message)
	}
	a.sessionLeaseMu.Lock()
	a.mu.Lock()
	if a.shuttingDown {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		return
	}
	if explicitUser && a.processing && a.dropSteerLeftovers {
		pos, ok := a.enqueuePostCancelUserPending(message)
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		a.acknowledgeQueuedUserMessage(message, pos, ok)
		return
	}
	if !explicitUser && a.dropSteerLeftovers {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		logging.Debug("programmatic resubmit blocked by active cancellation gate")
		return
	}
	// When no cancelled foreground remains, a live Enter owns the next
	// foreground directly and may intentionally repeat an exact recovery.
	if explicitUser && !a.processing {
		a.dropSteerLeftovers = false
	}
	a.mu.Unlock()
	a.sessionLeaseMu.Unlock()
	if a.tryResumeScheduledRecovery(message) {
		return
	}
	a.sessionLeaseMu.Lock()
	acceptedLineage := conversationLineage{epoch: a.recoveryEpoch.Load()}
	if a.session != nil {
		acceptedLineage.sessionID = a.session.GetID()
	}
	a.mu.Lock()
	if a.shuttingDown {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		return
	}
	// Esc may have landed after the optimistic check above. Preserve the same
	// explicit provenance instead of steering into the cancelled executor or
	// appending an indistinguishable callback-owned FIFO entry.
	if explicitUser && a.processing && a.dropSteerLeftovers {
		pos, ok := a.enqueuePostCancelUserPending(message)
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		a.acknowledgeQueuedUserMessage(message, pos, ok)
		return
	}
	if !explicitUser && a.dropSteerLeftovers {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		logging.Debug("programmatic resubmit blocked by concurrent cancellation")
		return
	}
	if a.processing {
		// Claude-Code-style "message during work": a follow-up typed while a
		// request is in flight is injected into the CURRENT turn via the
		// executor's steer channel, not queued for a later turn. The executor
		// drains it into history at the top of the next loop iteration so the
		// model can adjust course mid-task. The user sees their message echoed
		// immediately and a "steered" toast.
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()

		if steerMsg, steerable := a.prepareSteerMessage(message); steerable &&
			a.executor != nil && a.executor.TryQueueUserSteer(steerMsg) {
			a.journalEvent("request_steered", map[string]any{
				"message_preview": previewForJournal(message),
			})
			a.safeSendToProgramAsync(ui.StreamTextMsg(
				"💬 Steered into current turn — the agent will see this on its next step\n"))
			return
		}

		// The executor may be absent or already outside its steer-acceptance
		// window. Re-enter the lease barrier before committing fallback ownership:
		// Esc or the old finalizer may have changed both gate and processing while
		// TryQueueUserSteer was deciding.
		a.commitSubmitAfterSteerMiss(message, explicitUser)
		return
	}
	a.processing = true
	a.dropSteerLeftovers = false
	ctx := a.claimForegroundContextLocked()
	ctx = withConversationLineage(ctx, acceptedLineage.sessionID, acceptedLineage.epoch)

	// Parse command BEFORE unlocking to avoid race condition
	// (parsing is fast and doesn't need to be concurrent)
	name, args, isCmd := a.commandHandler.Parse(message)
	a.mu.Unlock()
	a.sessionLeaseMu.Unlock()
	a.startAcceptedSubmit(ctx, message, name, args, isCmd)
}

// commitSubmitAfterSteerMiss is the ownership commit point after an optimistic
// steering attempt returned false. It must repeat the full lease->app barrier:
// cancellation or foreground completion may have occurred while steering made
// its decision, so a blind FIFO append can be drained, stranded, or overtaken.
func (a *App) commitSubmitAfterSteerMiss(message string, explicitUser bool) {
	a.sessionLeaseMu.Lock()
	commitLineage := conversationLineage{epoch: a.recoveryEpoch.Load()}
	if a.session != nil {
		commitLineage.sessionID = a.session.GetID()
	}
	a.mu.Lock()
	if a.shuttingDown {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		return
	}
	if explicitUser && a.processing && a.dropSteerLeftovers {
		pos, ok := a.enqueuePostCancelUserPending(message)
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		a.acknowledgeQueuedUserMessage(message, pos, ok)
		return
	}
	if !explicitUser && a.dropSteerLeftovers {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		logging.Debug("programmatic resubmit blocked after steer miss cancellation")
		return
	}
	if a.processing {
		pos, ok := a.enqueuePending(message)
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		a.acknowledgeQueuedUserMessage(message, pos, ok)
		return
	}
	a.processing = true
	a.dropSteerLeftovers = false
	ctx := a.claimForegroundContextLocked()
	ctx = withConversationLineage(ctx, commitLineage.sessionID, commitLineage.epoch)
	name, args, isCmd := a.commandHandler.Parse(message)
	a.mu.Unlock()
	a.sessionLeaseMu.Unlock()
	a.startAcceptedSubmit(ctx, message, name, args, isCmd)
}

func (a *App) acknowledgeQueuedUserMessage(message string, pos int, ok bool) {
	if !ok {
		logging.Debug("pending queue full — message rejected", "len", len(message))
		a.safeSendToProgramAsync(ui.QueuedMessageRejectedMsg{
			Message: message,
			Reason:  fmt.Sprintf("Queue full (%d waiting)", pos),
			Waiting: pos,
		})
		return
	}
	a.journalEvent("request_queued", map[string]any{
		"message_preview": previewForJournal(message),
		"queue_position":  pos,
	})
	a.saveRecoverySnapshot()
	feedback := []tea.Msg{ui.QueuedCountMsg(pos)}
	if pos == 1 {
		feedback = append(feedback, ui.StreamTextMsg("📥 Queued — will process after the current request\n"))
	} else {
		feedback = append(feedback, ui.StreamTextMsg(fmt.Sprintf("📥 Queued (#%d in line)\n", pos)))
	}
	a.safeSendToProgramAsync(feedback...)
}

// claimForegroundContextLocked installs cancellation ownership for a request
// while the caller still holds a.mu. CancelProcessing takes a.mu before
// processingMu, so the processing flag, accepted request, and cancel function
// become visible as one state transition with no Esc/Ctrl+C handoff gap.
func (a *App) claimForegroundContextLocked() context.Context {
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	a.processingMu.Lock()
	a.processingCancel = cancel
	a.processingMu.Unlock()
	return ctx
}

// startAcceptedSubmit starts a message whose foreground ownership has already
// been claimed under a.mu and whose cancellation context is already installed.
// Separating claim from launch lets a completed turn publish its queue handoff
// without an idle or ownerless interval where input can overtake it or Esc can
// miss it.
func (a *App) startAcceptedSubmit(ctx context.Context, message, name string, args []string, isCmd bool) {
	a.startAcceptedSubmitWithRecovery(ctx, message, name, args, isCmd, nil)
}

func (a *App) startAcceptedSubmitWithRecovery(ctx context.Context, message, name string, args []string, isCmd bool, recovery []tools.ToolCheckpoint) {
	a.startAcceptedSubmitWithRecoveryIdentity(
		ctx, message, name, args, isCmd,
		"", "", "", a.recoveryEpoch.Load(), recovery)
}

func (a *App) startAcceptedSubmitWithRecoveryIdentity(
	ctx context.Context,
	message, name string,
	args []string,
	isCmd bool,
	recoveryID, recoverySessionID, recoveryMemoryQuery string,
	recoveryEpoch uint64,
	recovery []tools.ToolCheckpoint,
) {
	if !a.foregroundWorkers.Add() {
		// Shutdown sealed admission after this request claimed processing but
		// before its launcher ran. It has not touched history or tools yet, so
		// release the orphaned claim without dispatching queued work. A durable
		// recovery claim is also proven-unstarted at this exact boundary and can
		// safely return to scheduled instead of blocking the next launch as an
		// ambiguous claimed mutation.
		a.processingMu.Lock()
		if a.processingCancel != nil {
			a.processingCancel()
			a.processingCancel = nil
		}
		a.processingMu.Unlock()
		a.mu.Lock()
		a.processing = false
		a.dropSteerLeftovers = true
		a.mu.Unlock()
		if recoveryID != "" && recoverySessionID != "" {
			if err := a.releaseClaimedRecovery(
				recoveryID, recoverySessionID, recoveryEpoch,
				"shutdown_before_foreground_start", false); err != nil {
				logging.Warn("shutdown could not release proven-unstarted recovery",
					"recovery_id", recoveryID,
					"session_id", recoverySessionID,
					"error", err)
			}
		}
		return
	}
	workerName := "message-processing"
	if isCmd {
		workerName = "command-execution"
	}
	a.safeGo(workerName, func() {
		defer a.foregroundWorkers.Done()
		a.runAcceptedSubmitWithRecoveryIdentity(
			ctx, message, name, args, isCmd,
			recoveryID, recoverySessionID, recoveryMemoryQuery, recoveryEpoch, recovery)
	})
}

func (a *App) runAcceptedSubmitWithRecoveryIdentity(
	ctx context.Context,
	message, name string,
	args []string,
	isCmd bool,
	recoveryID, recoverySessionID, recoveryMemoryQuery string,
	recoveryEpoch uint64,
	recovery []tools.ToolCheckpoint,
) {
	// executeCommandCtx and processMessageWithMemoryQuery each own their normal
	// terminal finalizer. This guard covers the setup before either pipeline is
	// entered (journal, recovery snapshot, @reference expansion). A panic there
	// must not leave the application permanently busy merely because safeGo
	// recovered the goroutine at its outer boundary.
	pipelineStarted := false
	defer func() {
		if panicValue := recover(); panicValue != nil {
			logging.Error("accepted foreground setup panicked",
				"panic", panicValue,
				"stack", logging.PanicStack())
			if !pipelineStarted {
				if recoveryID != "" && recoverySessionID != "" {
					if err := a.releaseClaimedRecovery(
						recoveryID, recoverySessionID, recoveryEpoch,
						"foreground_setup_panic_before_start", true); err != nil {
						logging.Warn("setup panic left durable recovery claimed",
							"recovery_id", recoveryID,
							"session_id", recoverySessionID,
							"error", err)
					}
				}
				a.processingMu.Lock()
				a.processingCancel = nil
				a.processingMu.Unlock()
				a.finishForegroundProcessing(func() {
					a.safeSendToProgram(ui.ErrorMsg(fmt.Errorf(
						"internal error before request execution: %v — your work was saved, please retry",
						panicValue)))
				})
			}
		}
	}()

	if recoverySessionID != "" {
		ctx = withConversationLineage(ctx, recoverySessionID, recoveryEpoch)
	} else if _, ok := conversationLineageFromContext(ctx); !ok {
		lineage := a.captureConversationLineage()
		ctx = withConversationLineage(ctx, lineage.sessionID, lineage.epoch)
	}
	ctx = withPersistedSideEffectRecovery(ctx, recoveryID, recoverySessionID, recovery)
	if ctx.Err() != nil {
		// Add succeeded, but cancellation won the scheduler gap before either
		// command or model pipeline began. No tool could have run, so a durable
		// recovery is still provably unstarted and may return to scheduled. Once
		// a pipeline is entered, cancellation remains conservatively claimed.
		if recoveryID != "" && recoverySessionID != "" {
			if err := a.releaseClaimedRecovery(
				recoveryID, recoverySessionID, recoveryEpoch,
				"cancelled_before_foreground_pipeline", false); err != nil {
				logging.Warn("pre-start cancellation left durable recovery claimed",
					"recovery_id", recoveryID,
					"session_id", recoverySessionID,
					"error", err)
			}
		}
		a.processingMu.Lock()
		a.processingCancel = nil
		a.processingMu.Unlock()
		a.finishForegroundProcessing(func() {
			a.safeSendToProgram(ui.ResponseDoneMsg{})
		})
		return
	}
	// Journal and recovery AFTER unlock (saveRecoverySnapshot takes a.mu internally)
	a.journalEvent("request_accept", map[string]any{
		"message_preview": previewForJournal(message),
	})
	a.saveRecoverySnapshot()

	if isCmd {
		ctx = commands.WithRawInvocation(ctx, message)
		pipelineStarted = true
		a.executeCommandCtx(ctx, name, args)
		return
	}

	// Forgot-slash detection: if the message starts with a word that's
	// a registered command AND the rest looks like args rather than a
	// sentence, flag it. Users kept reporting "/provider kimi didn't
	// switch, Generating started" when they actually typed `provider
	// kimi` without the slash — it went to the LLM which then ran a
	// 20s reasoning round producing an unhelpful response. We don't
	// block the LLM call (their intent might genuinely be a message),
	// but we surface a toast so they know which command they meant and
	// can cancel + retype with the slash.
	if hint := a.detectUnslashedCommand(message); hint != "" {
		a.safeSendToProgramAsync(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: hint,
		})
	}

	// Expand @path references into inline file content for the AGENT only. The
	// UI already echoed the raw @path text to scrollback (before onSubmit), so
	// the display stays clean while the agent receives the referenced files.
	// Best-effort: unchanged when there are no resolvable @refs.
	isRecovery := recoveryID != "" || recoverySessionID != "" || len(recovery) > 0
	agentMessage, memoryQuery := a.prepareAgentMessageForSubmit(
		message, recoveryMemoryQuery, isRecovery)

	// Process message normally (coordinator is now integrated in agent system).
	if isRecovery && a.recoveryEpoch.Load() != recoveryEpoch {
		logging.Info("discarded recovery invalidated before pipeline start",
			"recovery_id", recoveryID, "session_id", recoverySessionID)
		a.finishMessageProcessing()
		return
	}
	pipelineStarted = true
	a.processMessageWithMemoryQuery(ctx, agentMessage, memoryQuery)
}

func (a *App) prepareAgentMessageForSubmit(message, recoveryMemoryQuery string, recovery bool) (string, string) {
	if !recovery {
		return a.expandAtReferences(message), message
	}
	// Durable recovery.Message is already the exact expanded executable payload
	// from the failed turn. Expanding it again could read a changed @file (or
	// interpret @tokens inside its contents) under an old checkpoint generation.
	// Keep it byte-for-byte and retain the original user query for memory lookup.
	if recoveryMemoryQuery == "" {
		recoveryMemoryQuery = message
	}
	return message, recoveryMemoryQuery
}

// handleResubmit dispatches a PROGRAMMATIC re-entry (rate-limit retry,
// auto-resume, pending-queue dispatch, file-command prompt expansion). Unlike
// a live user follow-up, a programmatic message must NEVER be steered into
// whatever turn happens to be running when its timer fires — if the user
// started a NEW task in the meantime, injecting the OLD failed message
// mid-turn would derail the new task. Busy → pending FIFO (the pre-steering
// behavior); idle → the normal submit path. The processing check races a
// concurrent submit by design: the loser lands in handleSubmit's processing
// branch, where prepareSteerMessage still applies the command guard, so the
// worst case is a plain-text retry steered into a just-started turn — the
// exact pre-check behavior, never a swallowed command.
func (a *App) handleResubmit(message string) {
	a.sessionLeaseMu.Lock()
	a.mu.Lock()
	if a.dropSteerLeftovers {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		logging.Debug("programmatic resubmit blocked by active cancellation gate")
		return
	}
	busy := a.processing
	if busy {
		pos, ok := a.enqueuePending(message)
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		if !ok {
			logging.Debug("pending queue full — programmatic resubmit dropped", "len", len(message))
			a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusRetry,
				Message: fmt.Sprintf("Queue full (%d waiting) — retry not queued", pos)})
			return
		}
		a.journalEvent("request_queued", map[string]any{
			"message_preview": previewForJournal(message),
			"queue_position":  pos,
			"source":          "programmatic_resubmit",
		})
		a.safeSendToProgram(ui.QueuedCountMsg(pos))
		return
	}
	a.mu.Unlock()
	a.sessionLeaseMu.Unlock()
	a.handleSubmitWithIntent(message, false)
}

// handleRecoveryResubmit is the side-effect-aware variant used only for
// rate-limit and timeout auto-retries. Its checkpoint snapshot travels with
// the queued request, so a user turn that runs while the timer is waiting
// cannot erase or replace the retry's replay generation.
type recoveryDispatchOutcome uint8

const (
	recoveryDispatchIgnored recoveryDispatchOutcome = iota
	recoveryDispatchStarted
	recoveryDispatchQueued
	recoveryDispatchReleased
	recoveryDispatchBlocked
)

func (a *App) handleRecoveryResubmit(message, memoryQuery, recoveryID, recoverySessionID string, recoveryEpoch uint64, checkpoints []tools.ToolCheckpoint) recoveryDispatchOutcome {
	// Serialize the lineage check and enqueue/start decision with /clear and
	// session switching. In particular, a switch cannot slip between a durable
	// claim and this proven-unstarted dispatch decision.
	a.sessionLeaseMu.Lock()
	a.mu.Lock()
	if a.recoveryEpoch.Load() != recoveryEpoch ||
		(recoverySessionID != "" && (a.session == nil || a.session.GetID() != recoverySessionID)) {
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		logging.Warn("recovery resubmit ignored after session switch",
			"recovery_id", recoveryID, "session_id", recoverySessionID)
		return recoveryDispatchIgnored
	}
	if a.dropSteerLeftovers {
		// Esc has closed the foreground handoff gate. A timer that claimed just
		// after that durable boundary must not install a new owner or enter FIFO.
		// Return a durable claim to scheduled, but leave it unscheduled in-process:
		// explicit cancellation pauses automation until restart or /recovery.
		a.mu.Unlock()
		if recoveryID == "" {
			a.sessionLeaseMu.Unlock()
			logging.Debug("in-process recovery blocked by active cancellation gate")
			return recoveryDispatchBlocked
		}
		key := recoveryTimerKey{sessionID: recoverySessionID, recoveryID: recoveryID}
		commitUncertain, releaseErr := a.releaseClaimedRecoveriesLocked([]recoveryTimerKey{key})
		a.sessionLeaseMu.Unlock()
		if releaseErr == nil || commitUncertain {
			a.finalizeReleasedRecoveries([]recoveryTimerKey{key}, "cancel_gate_before_dispatch", false)
			message := "Safe retry was paused by cancellation; use /recovery to resume or inspect it"
			if commitUncertain {
				message = "Safe retry was paused, but storage could not confirm directory durability; inspect /recovery before restarting"
				logging.Warn("cancel-gated recovery release committed with uncertain durability",
					"recovery_id", recoveryID, "error", releaseErr)
			}
			a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusWarning, Message: message})
			return recoveryDispatchReleased
		}
		logging.Warn("cancel-gated recovery claim could not be released",
			"recovery_id", recoveryID, "error", releaseErr)
		a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusWarning,
			Message: "Safe retry remains claimed after cancellation; inspect /recovery"})
		return recoveryDispatchBlocked
	}
	busy := a.processing
	if !busy {
		a.processing = true
		a.dropSteerLeftovers = false
		ctx := a.claimForegroundContextLocked()
		a.markRecoveryDispatched(recoverySessionID, recoveryID)
		a.mu.Unlock()
		a.sessionLeaseMu.Unlock()
		a.startAcceptedSubmitWithRecoveryIdentity(
			ctx, message, "", nil, false,
			recoveryID, recoverySessionID, memoryQuery, recoveryEpoch, checkpoints)
		return recoveryDispatchStarted
	}
	pos, ok := a.enqueueRecoveryPending(message, memoryQuery, recoveryID, recoverySessionID, recoveryEpoch, checkpoints)
	a.mu.Unlock()
	if !ok {
		if recoveryID == "" {
			a.sessionLeaseMu.Unlock()
			logging.Debug("pending queue full — in-process recovery resubmit rejected", "len", len(message))
			a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusWarning,
				Message: fmt.Sprintf("Queue full (%d waiting) — non-durable retry was not queued", pos)})
			return recoveryDispatchBlocked
		}
		key := recoveryTimerKey{sessionID: recoverySessionID, recoveryID: recoveryID}
		commitUncertain, releaseErr := a.releaseClaimedRecoveriesLocked([]recoveryTimerKey{key})
		a.sessionLeaseMu.Unlock()
		if releaseErr == nil || commitUncertain {
			a.finalizeReleasedRecoveries([]recoveryTimerKey{key}, "queue_full_before_dispatch", true)
			logging.Debug("pending queue full — recovery returned to durable schedule", "len", len(message))
			a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusRetry,
				Message: fmt.Sprintf("Queue full (%d waiting) — safe retry returned to its schedule", pos)})
			if commitUncertain {
				logging.Warn("unstarted recovery release committed with uncertain durability",
					"recovery_id", recoveryID, "error", releaseErr)
			}
			return recoveryDispatchReleased
		}
		logging.Warn("pending queue full and recovery claim could not be released",
			"recovery_id", recoveryID, "error", releaseErr)
		a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusWarning,
			Message: fmt.Sprintf("Queue full (%d waiting) — safe retry remains claimed; inspect /recovery", pos)})
		return recoveryDispatchBlocked
	}
	a.markRecoveryDispatched(recoverySessionID, recoveryID)
	a.sessionLeaseMu.Unlock()
	a.journalEvent("request_queued", map[string]any{
		"message_preview": previewForJournal(message),
		"queue_position":  pos,
		"source":          "side_effect_recovery",
	})
	a.safeSendToProgram(ui.QueuedCountMsg(pos))
	return recoveryDispatchQueued
}

// detectUnslashedCommand returns a user-facing hint when the input looks
// like a slash command the user typed without the leading slash (e.g.
// `provider kimi` instead of `/provider kimi`). Returns "" when the input
// is plain natural language and no warning should fire.
//
// Heuristic:
//   - first word (lowercased) exactly matches a registered command name
//   - total word count ≤ 4 (a sentence is almost always longer)
//   - no sentence punctuation (. , ? ! : ;) anywhere — natural language
//     prompts usually have at least one
//
// False-positive risk is very low: only messages that look exactly like
// command invocations trigger. A user asking the model "provider ideas"
// has 2 words and no punctuation, so it WOULD fire and hint /provider.
// That's acceptable — the toast is advisory and the LLM call still runs,
// so the user sees both the hint and the model's response.
func (a *App) detectUnslashedCommand(message string) string {
	message = strings.TrimSpace(message)
	if message == "" || strings.HasPrefix(message, "/") {
		return ""
	}
	words := strings.Fields(message)
	if len(words) == 0 || len(words) > 4 {
		return ""
	}
	// Any sentence punctuation disqualifies — natural-language tells.
	if strings.ContainsAny(message, ".,?!:;") {
		return ""
	}
	firstWord := strings.ToLower(words[0])
	if a.commandHandler == nil {
		return ""
	}
	if _, exists := a.commandHandler.GetCommand(firstWord); !exists {
		return ""
	}
	return fmt.Sprintf("Looks like you meant /%s — this was sent to the model. Prefix with '/' to run as a command next time.", firstWord)
}

func (a *App) executeCommandCtx(ctx context.Context, name string, args []string) {
	responseDoneSent := false
	defer func() {
		// Recover from panics in command execution. Without this, a panic
		// in a command (e.g. nil-deref in ApplyConfig during /login, or a
		// malformed result from a file-based command) crashes the whole
		// CLI. safeGo already wraps this goroutine in a recover, but that
		// only logs — it does NOT send ResponseDoneMsg, so the UI would
		// stay stuck in "Generating" forever. We handle both here: convert
		// the panic into an ErrorMsg + ResponseDoneMsg so the user sees
		// what happened and the UI returns to StateInput.
		panicValue := recover()
		if panicValue != nil {
			logging.Error("command execution panicked",
				"command", name,
				"panic", panicValue,
				"stack", logging.PanicStack())
		}
		// Clear this command's cancel handle before the shared finalizer can
		// atomically hand ownership to a queued operation and install its handle.
		a.processingMu.Lock()
		a.processingCancel = nil
		a.processingMu.Unlock()
		a.finishForegroundProcessing(func() {
			if panicValue != nil {
				a.safeSendToProgram(ui.ErrorMsg(fmt.Errorf("internal error in /%s: %v", name, panicValue)))
			}
			// GUARANTEE ResponseDoneMsg reaches the UI before a queued turn is
			// dispatched. Otherwise a late completion from this command could
			// reset the new turn back to StateInput.
			if !responseDoneSent {
				a.safeSendToProgram(ui.ResponseDoneMsg{})
			}
		})
	}()
	// The request may have been accepted from the FIFO while Program.Send was
	// blocked and then cancelled before this goroutine was scheduled. Preserve
	// the normal terminal UI/finalizer path, but never invoke a context-insensitive
	// command after cancellation already owns the handoff.
	if ctx.Err() != nil {
		return
	}

	a.mu.Lock()
	a.diffBatchDecision = ui.DiffPending
	a.mu.Unlock()
	a.journalEvent("command_started", map[string]any{
		"command": name,
		"args":    args,
	})

	// Surface a status hint for commands that make an LLM API call or
	// similarly slow IO. Without this, /compact (which triggers a summary
	// generation roundtrip) looked indistinguishable from "the message went
	// to the model and the model is thinking" — the user couldn't tell if
	// their command had actually started or was lost.
	if cmd, ok := a.commandHandler.GetCommand(name); ok {
		// MetadataProvider (optional interface in commands/metadata.go) lets
		// us extract metadata without touching every concrete command's
		// Command interface implementation.
		if metaProvider, hasMeta := cmd.(commands.MetadataProvider); hasMeta {
			if meta := metaProvider.GetMetadata(); meta.LongRunning {
				label := meta.LongRunningLabel
				if label == "" {
					label = fmt.Sprintf("Running /%s — this may take a moment...", name)
				}
				a.safeSendToProgram(ui.StatusUpdateMsg{
					Type:    ui.StatusInfo,
					Message: label,
				})
			}
		}
	}

	result, err := a.commandHandler.Execute(ctx, name, args, a)

	if err != nil {
		a.journalEvent("command_failed", map[string]any{
			"command": name,
			"error":   err.Error(),
		})
		a.safeSendToProgram(ui.ErrorMsg(err))
	} else {
		a.journalEvent("command_completed", map[string]any{
			"command": name,
		})
		// Record for autocomplete frecency — successful commands bubble up in
		// future suggestions. Failures excluded so typos don't taint ranking.
		if a.tui != nil {
			a.tui.RecordRecentCommand(name)
		}
		// Handle special command markers
		if browsePath, ok := strings.CutPrefix(result, "__browse:"); ok {
			a.safeSendToProgram(ui.FileBrowserRequestMsg{StartPath: browsePath})
		} else if result == commands.SettingsMarker {
			// /settings: open the interactive modal (also reachable via Ctrl+S
			// and the palette "Open Settings" action).
			a.openSettingsModal()
		} else if result == commands.ModelSelectorMarker {
			// /model with no args: open the interactive model selector (the
			// same picker as Ctrl+K). The Update handler guards modal state.
			a.safeSendToProgram(ui.OpenModelSelectorMsg{})
		} else if provider, ok := strings.CutPrefix(result, commands.LoginKeyMarker); ok {
			// /login <provider> with no key: open the masked key-entry modal so
			// the key is captured securely instead of being typed as a message.
			msg := ui.OpenKeyEntryMsg{Provider: provider}
			if p := config.GetProvider(provider); p != nil {
				msg.DisplayName = p.DisplayName
				msg.SetupURL = p.SetupKeyURL
			}
			a.safeSendToProgram(msg)
		} else if prompt, ok := strings.CutPrefix(result, commands.PromptMarker); ok {
			// File-based command: the expansion is a MODEL PROMPT, not
			// display text. It is the continuation of the already-accepted slash
			// command, so stage it ahead of later type-ahead; the shared finalizer
			// atomically hands it the foreground slot after ResponseDone. A
			// separate goroutine here made ordering scheduler-dependent and could
			// run a later queued message before its originating command expanded.
			// Serialize the cancellation check and prepend with CancelProcessing's
			// processingMu section. If Esc wins, the continuation is skipped; if
			// this prepend wins, Esc subsequently drains it. A late result from a
			// context-insensitive command can therefore never resurrect cancelled
			// work after the authoritative queue drain.
			a.processingMu.Lock()
			if ctx.Err() == nil {
				a.prependPending(prompt)
			} else {
				// A cancelled /skill command may have prepared one-shot tool
				// rules for this exact continuation. If the handoff is not
				// queued, discard them so a later unrelated prompt cannot
				// inherit an abandoned grant or denial.
				tools.ResetSkillPermissionGrants(a.registry)
			}
			a.processingMu.Unlock()
		} else {
			// Display command result as assistant message
			a.safeSendToProgram(ui.StreamTextMsg(result))
		}
	}
	responseDoneSent = true
	a.safeSendToProgram(ui.ResponseDoneMsg{})
}

// handleQuit handles quit request.
func (a *App) handleQuit() {
	a.journalEvent("app_quit", nil)
	a.beginShutdown()
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

// GetMCPManager returns the MCP manager (may be nil when MCP is disabled).
func (a *App) GetMCPManager() *mcp.Manager {
	return a.mcpManager
}

// LockMCPConfigMutation and UnlockMCPConfigMutation implement the optional
// commands-side transaction boundary used only by MCP add/remove helpers.
// Keeping the lock on App lets slash commands and model-facing mcp_admin calls
// share one sequence without serializing read-only MCP actions.
func (a *App) LockMCPConfigMutation() {
	a.mcpMutationMu.Lock()
}

func (a *App) UnlockMCPConfigMutation() {
	a.mcpMutationMu.Unlock()
}

// EnableMCP creates a fresh empty MCP manager at runtime and persists
// mcp.enabled=true to config, so the user can turn MCP on from chat without
// editing YAML + restarting. Idempotent — returns nil if already enabled.
func (a *App) EnableMCP() error {
	a.mu.Lock()
	if a.mcpManager != nil {
		a.mu.Unlock()
		return nil
	}
	a.mcpManager = mcp.NewManager(nil)
	a.mcpManager.SetToolsChangedCallback(func(name string) {
		a.SyncMCPToolsForServer(name)
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusStreamResume,
			Message: fmt.Sprintf("MCP %q: tool list updated", name),
		})
	})
	// Boot-parity wiring (review catch): without these a runtime-enabled
	// manager ran with NO health monitor and NO health toasts until restart.
	healthToasts := newHealthToastLimiter()
	a.mcpManager.SetHealthChangeCallback(func(name string, healthy bool) {
		if !healthToasts.ShouldEmit(name, time.Now()) {
			return
		}
		if healthy {
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusStreamResume,
				Message: fmt.Sprintf("MCP server %q reconnected", name),
			})
		} else {
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRecoverableError,
				Message: fmt.Sprintf("MCP server %q disconnected", name),
			})
		}
	})
	if a.config != nil && a.config.MCP.HealthCheckInterval > 0 {
		// Safe on an empty manager (no servers = cheap no-op ticks); the boot
		// path only starts it when connected>0, but a runtime-enabled manager
		// gains servers AFTER this point and would otherwise never be
		// monitored until restart.
		a.mcpManager.StartHealthCheck(a.ctx, a.config.MCP.HealthCheckInterval)
	}
	if a.config == nil {
		a.config = config.DefaultConfig()
	}
	a.config.MCP.Enabled = true
	saveErr := a.config.Save()
	a.mu.Unlock()
	if saveErr != nil {
		return fmt.Errorf("MCP enabled in session but config save failed: %w", saveErr)
	}
	a.safeSendToProgram(ui.StatusUpdateMsg{
		Type:    ui.StatusInfo,
		Message: "MCP enabled. Use /mcp add or /mcp preset to add servers.",
	})
	return nil
}

// DisableMCP shuts down all MCP server connections, nils the manager, and
// persists mcp.enabled=false. Idempotent — returns nil if already disabled.
func (a *App) DisableMCP() error {
	a.mu.Lock()
	if a.mcpManager == nil {
		// Already disabled at runtime — still persist the flag so config and
		// session state can't stay out of sync (nil-guarded for tests).
		var saveErr error
		if a.config != nil && a.config.MCP.Enabled {
			a.config.MCP.Enabled = false
			saveErr = a.config.Save()
		}
		a.mu.Unlock()
		return saveErr
	}
	mgr := a.mcpManager
	a.mcpManager = nil
	if a.config != nil {
		a.config.MCP.Enabled = false
	}
	// Unregister all MCP tools from the registry so the model stops seeing them.
	if a.registry != nil {
		for _, t := range a.registry.List() {
			if mt, ok := t.(*mcp.MCPTool); ok {
				a.registry.Unregister(mt.Name())
			}
		}
		if a.client != nil {
			a.client.SetTools(a.planModeToolsLocked(a.planningModeEnabled))
		}
	}
	saveErr := a.config.Save()
	a.mu.Unlock()

	// Shutdown outside the lock — Close() can block on network I/O.
	if mgr != nil {
		_ = mgr.Shutdown(context.Background())
	}
	if saveErr != nil {
		return fmt.Errorf("MCP disabled in session but config save failed: %w", saveErr)
	}
	a.safeSendToProgram(ui.StatusUpdateMsg{
		Type:    ui.StatusInfo,
		Message: "MCP disabled. All MCP tools removed.",
	})
	return nil
}

// GetToolRegistry returns the tool registry — exposed so /mcp add/remove
// can register and unregister MCP-provided tools at runtime.
func (a *App) GetToolRegistry() *tools.Registry {
	return a.registry
}

// GetMainClient returns the primary API client so commands can push fresh
// tool declarations after the registry changes (e.g. MCP add/remove).
func (a *App) GetMainClient() client.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// clientSnapshot returns the current client under clientMu (the LEAF lock), safe
// to call from a background goroutine that must NOT take a.mu — e.g.
// pushTurnContext, which is sometimes invoked by a caller already holding a.mu.
// Writers (ApplyConfig, failover) update a.client under a.mu AND clientMu, so
// this never observes a torn interface value.
func (a *App) clientSnapshot() client.Client {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.client
}

// setClientLocked swaps a.client. The caller MUST already hold a.mu; this adds
// the clientMu write so background clientSnapshot readers can't observe a torn
// value. Order a.mu -> clientMu is preserved.
func (a *App) setClientLocked(c client.Client) {
	a.clientMu.Lock()
	a.client = c
	a.clientMu.Unlock()
}

// GetLoopManager returns the loops manager for the /loop command. Nil
// when the loop subsystem is not wired (e.g. early in App lifecycle, or
// in unit-test builds that skip loops). The /loop command nil-checks
// and returns a clear "unavailable" message.
func (a *App) GetLoopManager() commands.LoopManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loopManager == nil {
		return nil
	}
	return a.loopManager
}

// GetAgentTaskRunner returns the agent runner for the /tasks command. Nil
// when the agent subsystem is not wired. The typed-nil guard matters: a nil
// *agent.Runner stuffed into the interface would be non-nil to the caller.
func (a *App) GetAgentTaskRunner() commands.AgentTaskRunner {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentRunner == nil {
		return nil
	}
	return a.agentRunner
}

// GetBackgroundShellRunner returns the task manager for the /tasks command's
// background-shell-task section. Nil when the tasks subsystem isn't wired.
// Typed-nil guard for the same reason as GetAgentTaskRunner: a nil
// *tasks.Manager stuffed into the interface would be non-nil to the caller,
// and its methods dereference the receiver (m.mu) — a real panic risk, not
// just a lint nicety.
func (a *App) GetBackgroundShellRunner() commands.BackgroundShellRunner {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.taskManager == nil {
		return nil
	}
	return a.taskManager
}

// GetAuditRunner returns the agent runner for /audit's find-then-verify
// recipe. Nil when the agent subsystem isn't wired. Typed-nil guard for the
// same reason as GetAgentTaskRunner.
func (a *App) GetAuditRunner() commands.AuditRunner {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentRunner == nil {
		return nil
	}
	return a.agentRunner
}

// GetHooksManager returns the hooks manager for the /hooks command. Nil
// when hooks aren't wired.
func (a *App) GetHooksManager() *hooks.Manager {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hooksManager
}

// SyncMCPToolsForServer reconciles the tool registry against the current
// state of the named MCP server in a.mcpManager. Called from the MCP
// tools-changed callback when a server emits notifications/tools/list_changed
// (or when tools are manually refreshed via /mcp refresh).
//
// Steps: unregister the registry entries that used to belong to this server
// but are no longer in the manager's tool list; register any newly-arrived
// tools; re-apply the per-server permission override; push the refreshed
// declaration set to the main client so the LLM sees the change.
func (a *App) SyncMCPToolsForServer(serverName string) {
	if a == nil || a.mcpManager == nil || a.registry == nil {
		return
	}

	// Snapshot the current live tool set from the manager.
	live := make(map[string]*mcp.MCPTool)
	for _, t := range a.mcpManager.GetTools() {
		if mt, ok := t.(*mcp.MCPTool); ok && mt.GetServerName() == serverName {
			live[mt.Name()] = mt
		}
	}

	// Unregister registry entries belonging to this server that are no
	// longer present in live.
	for _, t := range a.registry.List() {
		mt, ok := t.(*mcp.MCPTool)
		if !ok || mt.GetServerName() != serverName {
			continue
		}
		name := mt.Name()
		if _, stillLive := live[name]; !stillLive {
			a.registry.Unregister(name)
			permission.ClearToolRiskOverride(name)
		}
	}

	// Read the per-server permission level from the MCP manager instead of
	// a.config. mcp.Manager.GetServerConfig is protected by its own mutex;
	// a.config.MCP.Servers is a plain slice that /mcp add/remove may be
	// mutating concurrently, which would race with iteration here.
	var permLevel string
	if cfg, ok := a.mcpManager.GetServerConfig(serverName); ok && cfg != nil {
		permLevel = cfg.PermissionLevel
	}
	level := permission.ParseRiskLevel(permLevel)
	for name, t := range live {
		if err := a.registry.Register(t); err == nil {
			permission.SetToolRiskOverride(name, level)
			continue
		}
		// Already registered — still refresh the override in case server's
		// trust level was changed in config.
		permission.SetToolRiskOverride(name, level)
	}

	// The registry just changed shape. Recompute the capability ceiling before
	// pushing the schema so a deny-only run keeps offering the MCP tools the
	// user never denied, instead of freezing at the boot-time tool list.
	a.refreshToolCapabilityCeiling()

	// Push fresh declarations to the main client.
	if c := a.GetMainClient(); c != nil {
		c.SetTools(a.toolsForCurrentMode())
	}
}

// toolsForCurrentMode returns the idle tool schema that matches the active
// plan, feature, invocation-capability, and engine policies. Auto mode has no
// request at this point, so hybrid tools remain hidden until toolsForMessage.
// This must be the single source of truth for every non-request SetTools call
// that targets the main client.
//
// Safe to call from any goroutine: it takes only short App snapshots, and the
// registry operations it invokes take their own internal locks.
func (a *App) toolsForCurrentMode() []*genai.Tool {
	return a.toolsForMessage("")
}

// toolsForMessage returns a truthful hybrid-filtered schema for one request. Auto
// mode keeps the REPL absent from ordinary turns and exposes it only when the
// shared hybrid policy recognizes collection-scale computation. The direct
// harness stays internal to rlm.harness unless hybrid mode is explicit.
func (a *App) toolsForMessage(message string) []*genai.Tool {
	return a.toolsForMessageDecision(message, hybrid.Decision{})
}

func (a *App) toolsForMessageDecision(message string, policyDecision hybrid.Decision) []*genai.Tool {
	// Snapshot plan + engine together: ApplyConfig swaps the config pointer
	// under a.mu while request preparation can run concurrently.
	a.mu.Lock()
	planModeEnabled := a.planningModeEnabled
	mode := a.runtimeEngineModeLocked()
	planEnabled := true
	memoryEnabled := true
	if a.config != nil {
		planEnabled = a.config.Plan.Enabled
		memoryEnabled = a.config.Memory.Enabled
	}
	a.mu.Unlock()
	base := a.baseToolsForPlanMode(planModeEnabled, planEnabled, memoryEnabled)
	base = a.filterHybridToolsDecision(base, mode, message, policyDecision)
	ceiling, restricted := a.toolCapabilitySnapshot()
	if !restricted {
		return base
	}
	return filterToolSchemaByCeiling(base, ceiling)
}

// planModeToolsLocked is the lock-free variant of toolsForCurrentMode — it
// takes the plan-mode flag as an argument instead of reading it. Required
// for callers that already hold `a.mu` (e.g. TogglePlanningMode), because
// IsPlanningModeEnabled takes the same mutex and would self-deadlock on
// Go's non-reentrant sync.Mutex.
func (a *App) planModeToolsLocked(planModeEnabled bool) []*genai.Tool {
	mode := a.runtimeEngineModeLocked()
	planEnabled := true
	memoryEnabled := true
	if a.config != nil {
		planEnabled = a.config.Plan.Enabled
		memoryEnabled = a.config.Memory.Enabled
	}
	base := a.baseToolsForPlanMode(planModeEnabled, planEnabled, memoryEnabled)
	base = a.filterHybridTools(base, mode, "")
	if !a.toolCapabilityRestricted {
		return base
	}
	return filterToolSchemaByCeiling(base, a.toolCapabilityCeiling)
}

func (a *App) filterHybridTools(base []*genai.Tool, mode, message string) []*genai.Tool {
	return a.filterHybridToolsDecision(base, mode, message, hybrid.Decision{})
}

func (a *App) filterHybridToolsDecision(
	base []*genai.Tool,
	mode, message string,
	policyDecision hybrid.Decision,
) []*genai.Tool {
	decision := policyDecision
	if decision.Reason == "" {
		decision = hybrid.Decide(mode, message)
	}
	exclude := make([]string, 0, 2)
	deferredUnavailable := a.deferredHybrid != nil && !a.deferredHybrid.canAdvertise()
	if !decision.Enabled || deferredUnavailable {
		exclude = append(exclude, "repl_exec")
	}
	if !strings.EqualFold(strings.TrimSpace(mode), "hybrid") || deferredUnavailable {
		exclude = append(exclude, "harness")
	}
	if len(exclude) == 0 {
		return base
	}
	return tools.FilterGeminiToolsExcluding(base, exclude...)
}

type hybridPolicySnapshot struct {
	Mode           string
	Strategy       hybrid.Strategy
	REPLEligible   bool
	REPLExposed    bool
	HarnessExposed bool
	Reason         string
}

func (p hybridPolicySnapshot) journalDetails() map[string]any {
	return map[string]any{
		"mode":            p.Mode,
		"strategy":        p.Strategy,
		"repl_eligible":   p.REPLEligible,
		"repl_enabled":    p.REPLExposed,
		"harness_enabled": p.HarnessExposed,
		"exposure_gap":    p.REPLEligible && !p.REPLExposed,
		"reason":          p.Reason,
	}
}

// hybridPolicyForSchema separates classification from actual capability. The
// final schema is authoritative: auto may classify a request as eligible while
// secure-runtime probing, plan mode, or an invocation capability ceiling keeps
// repl_exec unavailable. Conflating those states made eval reports claim that
// a tool had been offered when the model could not possibly call it.
func (a *App) hybridPolicyForSchema(message string, schema []*genai.Tool) hybridPolicySnapshot {
	mode := "auto"
	if a != nil {
		a.mu.Lock()
		mode = a.runtimeEngineModeLocked()
		a.mu.Unlock()
	}
	decision := hybrid.Decide(mode, message)
	return hybridPolicySnapshotForDecision(mode, decision, schema)
}

func hybridPolicySnapshotForDecision(
	mode string,
	decision hybrid.Decision,
	schema []*genai.Tool,
) hybridPolicySnapshot {
	return hybridPolicySnapshot{
		Mode:           mode,
		Strategy:       decision.Strategy,
		REPLEligible:   decision.Enabled,
		REPLExposed:    toolSchemaContains(schema, "repl_exec"),
		HarnessExposed: toolSchemaContains(schema, "harness"),
		Reason:         decision.Reason,
	}
}

func toolSchemaContains(schema []*genai.Tool, name string) bool {
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

func toolSchemaDeclarationNames(schema []*genai.Tool) []string {
	set := make(map[string]struct{})
	for _, envelope := range schema {
		if envelope == nil {
			continue
		}
		for _, declaration := range envelope.FunctionDeclarations {
			if declaration != nil && declaration.Name != "" {
				set[declaration.Name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// baseToolsForPlanMode applies feature and plan-mode gates but no invocation
// capability ceiling. It does not touch App mutex-protected fields.
func (a *App) baseToolsForPlanMode(planModeEnabled, planEnabled, memoryEnabled bool) []*genai.Tool {
	if planModeEnabled {
		return a.registry.PlanModeGeminiTools()
	}
	// Feature-gate the schema: a tool the model must NOT call because its
	// feature is off in config is dropped, so the model can't call a disabled
	// tool and hit a confusing "unavailable" error. plan.enabled off → the
	// plan-mode control tools (a model could otherwise enter_plan_mode and
	// strand itself read-only with no interactive approval); memory.enabled
	// off → the memory/memorize tools (their store isn't wired, so they error).
	// Default config enables both, so this is a no-op there.
	exclude := map[string]bool{}
	if !planEnabled {
		for n := range tools.PlanModeControlToolNames {
			exclude[n] = true
		}
	}
	if !memoryEnabled {
		exclude["memory"] = true
		exclude["memorize"] = true
	}
	if len(exclude) == 0 {
		return a.registry.GeminiTools()
	}
	return a.registry.GeminiToolsExcluding(exclude)
}

// GetRuntimeHealthReport returns runtime reliability and provider health diagnostics.
func (a *App) GetRuntimeHealthReport() string {
	var sb strings.Builder
	sb.WriteString("Runtime health:\n")

	if a.reliability != nil {
		s := a.reliability.Snapshot()
		mode := "normal"
		if s.Degraded {
			mode = fmt.Sprintf("degraded (%v remaining)", s.DegradedRemaining)
		}
		fmt.Fprintf(&sb, "- mode: %s\n", mode)
		fmt.Fprintf(&sb, "- consecutive_failures: %d\n", s.ConsecutiveFailures)
		fmt.Fprintf(&sb, "- window_failures: %d (start: %s)\n",
			s.WindowFailures, s.WindowStartedAt.Format("2006-01-02 15:04:05"))
	}

	age := a.stepHeartbeatAge()
	if age > 0 {
		fmt.Fprintf(&sb, "- step_heartbeat_age: %v\n", age.Round(time.Second))
	} else {
		sb.WriteString("- step_heartbeat_age: n/a\n")
	}

	journalStatus := "healthy"
	recoveryStatus := "healthy"
	if a.journal == nil {
		journalStatus = "unavailable"
		recoveryStatus = "unavailable"
	} else {
		if a.persistenceFailures.isFailing("execution_journal") {
			journalStatus = "failing"
		}
		if a.persistenceFailures.isFailing("recovery_snapshot") {
			recoveryStatus = "failing"
		}
	}
	planStatus := "unavailable"
	if a.planManager != nil {
		if a.planManager.GetPlanStore() == nil {
			planStatus = "disabled"
		} else if a.persistenceFailures.isFailing("current_plan") {
			planStatus = "failing"
		} else {
			planStatus = "healthy"
		}
	}
	sb.WriteString("- persistence:\n")
	fmt.Fprintf(&sb, "  - execution_journal: %s\n", journalStatus)
	fmt.Fprintf(&sb, "  - recovery_snapshot: %s\n", recoveryStatus)
	fmt.Fprintf(&sb, "  - plan_store: %s\n", planStatus)

	sb.WriteString("\n")
	sb.WriteString(client.GetProviderHealthReport())
	return sb.String()
}

// GetUIRuntimeStatus returns a compact runtime snapshot for TUI status bar rendering.
func (a *App) GetUIRuntimeStatus() ui.RuntimeStatusSnapshot {
	out := ui.RuntimeStatusSnapshot{
		Mode:           "normal",
		RequestBreaker: "n/a",
		StepBreaker:    "n/a",
	}

	// a.config is swapped by ApplyConfig under a.mu, and this method runs on the
	// Bubble Tea status goroutine (runtimeStatusCmd, ~1/sec) — a DISTINCT
	// goroutine from the app one — so the read must be synchronized or it races
	// the swap (the router.go clientMu precedent for the same ApplyConfig-reader
	// class). Snapshot the pointer under the lock; cfg is immutable post-publish.
	a.mu.Lock()
	cfg := a.config
	a.mu.Unlock()
	if cfg != nil {
		out.Provider = runtimeProviderForConfig(cfg)
	}
	if a.reliability != nil {
		s := a.reliability.Snapshot()
		if s.Degraded {
			out.Mode = "degraded"
			out.DegradedRemaining = s.DegradedRemaining
		}
		out.ConsecutiveFailure = s.ConsecutiveFailures
	}
	if a.policy != nil {
		s := a.policy.Snapshot()
		if s.RequestBreakerState != "" {
			out.RequestBreaker = s.RequestBreakerState
		}
		if s.StepBreakerState != "" {
			out.StepBreaker = s.StepBreakerState
		}
	}

	age := a.stepHeartbeatAge()
	if age > 0 {
		out.HasHeartbeat = true
		out.HeartbeatAge = age
	}

	// Active loops for the status-bar badge. loopManager is boot-set (builder,
	// never reassigned); Active()/FiringState() lock internally and are cheap
	// (in-memory). Loops persist across restarts, so this is the persistent
	// "a loop is alive in the background" chrome signal.
	if a.loopManager != nil {
		out.ActiveLoops = len(a.loopManager.Active())
		if _, _, firing := a.loopManager.FiringState(); firing {
			out.LoopFiring = true
		}
	}

	// MCP health for the status-bar badge. mcpManager is boot-set (builder,
	// never reassigned); GetServerStatus locks internally and is cheap
	// (in-memory snapshot, no network).
	if a.mcpManager != nil {
		for _, s := range a.mcpManager.GetServerStatus() {
			out.MCPTotal++
			if s.Connected && s.Healthy {
				out.MCPHealthy++
			}
		}
	}

	return out
}

// GetPolicyReport returns current policy engine state.
func (a *App) GetPolicyReport() string {
	if a.policy == nil {
		return "Policy engine not initialized."
	}
	s := a.policy.Snapshot()
	return fmt.Sprintf("Policy engine:\n- request_breaker: %s\n- step_breaker: %s",
		s.RequestBreakerState, s.StepBreakerState)
}

// GetLedgerReport returns run ledger details for the active plan.
func (a *App) GetLedgerReport() string {
	if a.planManager == nil {
		return "Ledger unavailable: plan manager not initialized."
	}
	p := a.planManager.GetCurrentPlan()
	if p == nil {
		return "Ledger: no active plan."
	}

	steps := p.GetStepsSnapshot()
	ledger := p.GetRunLedgerSnapshot()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Run ledger for %s (%s)\n", p.Title, p.ID)

	for _, step := range steps {
		entry := ledger[step.ID]
		if entry == nil {
			fmt.Fprintf(&sb, "- step %d (%s): no side effects recorded\n", step.ID, step.Title)
			continue
		}
		status := "incomplete"
		if entry.Completed {
			status = "completed"
		} else if entry.PartialEffects {
			status = "partial_effects"
		}
		fmt.Fprintf(&sb,
			"- step %d (%s): %s, tool_calls=%d, files=%d, commands=%d, duplicates=%d\n",
			step.ID, step.Title, status, entry.ToolCalls, len(entry.FilesTouched), len(entry.Commands), entry.DuplicateEffects,
		)
	}

	return sb.String()
}

// GetPlanProofReport returns contract/evidence proof diagnostics for one plan step.
// stepID: 0 means auto-select current/most-recent actionable step.
func (a *App) GetPlanProofReport(stepID int) string {
	if a.planManager == nil {
		return "Plan proof unavailable: plan manager not initialized."
	}
	p := a.planManager.GetCurrentPlan()
	if p == nil {
		return "Plan proof: no active plan."
	}

	steps := p.GetStepsSnapshot()
	if len(steps) == 0 {
		return fmt.Sprintf("Plan proof: active plan %s has no steps.", p.ID)
	}
	target := selectPlanProofStep(steps, a.planManager.GetCurrentStepID(), stepID)
	if target == nil {
		return fmt.Sprintf("Plan proof: step %d not found.", stepID)
	}

	ledger := p.GetRunLedgerSnapshot()
	entry := ledger[target.ID]

	var sb strings.Builder
	fmt.Fprintf(&sb, "Plan proof for %s (%s)\n", p.Title, p.ID)
	fmt.Fprintf(&sb, "Step %d: %s\n", target.ID, target.Title)
	fmt.Fprintf(&sb, "- status: %s\n", target.Status.String())
	if !target.StartTime.IsZero() {
		fmt.Fprintf(&sb, "- started: %s\n", target.StartTime.Format("2006-01-02 15:04:05"))
	}
	if !target.EndTime.IsZero() {
		fmt.Fprintf(&sb, "- ended: %s\n", target.EndTime.Format("2006-01-02 15:04:05"))
	}
	if strings.TrimSpace(target.Error) != "" {
		fmt.Fprintf(&sb, "- error: %s\n", target.Error)
	}

	sb.WriteString("\nContract:\n")
	if len(target.Inputs) > 0 {
		fmt.Fprintf(&sb, "- inputs: %s\n", strings.Join(target.Inputs, "; "))
	}
	if ea := strings.TrimSpace(target.ExpectedArtifact); ea != "" {
		fmt.Fprintf(&sb, "- expected_artifact: %s\n", ea)
	}
	if len(target.ExpectedArtifactPaths) > 0 {
		fmt.Fprintf(&sb, "- expected_artifact_paths: %s\n", strings.Join(target.ExpectedArtifactPaths, ", "))
	} else {
		sb.WriteString("- expected_artifact_paths: (none)\n")
	}
	if len(target.SuccessCriteria) > 0 {
		fmt.Fprintf(&sb, "- success_criteria: %s\n", strings.Join(target.SuccessCriteria, "; "))
	}
	if len(target.VerifyCommands) > 0 {
		fmt.Fprintf(&sb, "- verify_commands: %s\n", strings.Join(target.VerifyCommands, "; "))
	}
	if rb := strings.TrimSpace(target.Rollback); rb != "" {
		fmt.Fprintf(&sb, "- rollback: %s\n", rb)
	}

	sb.WriteString("\nProof:\n")
	if note := strings.TrimSpace(target.VerificationNote); note != "" {
		fmt.Fprintf(&sb, "- verification_note: %s\n", note)
	} else {
		sb.WriteString("- verification_note: (empty)\n")
	}
	fmt.Fprintf(&sb, "- evidence_items: %d\n", len(target.Evidence))
	for _, evidence := range target.Evidence {
		evidence = strings.TrimSpace(evidence)
		if evidence == "" {
			continue
		}
		if raw, ok := strings.CutPrefix(evidence, "proof_json="); ok {
			raw = strings.TrimSpace(raw)
			if pretty := prettyProofJSON(raw); pretty != "" {
				sb.WriteString("- proof_json:\n")
				sb.WriteString(pretty)
				sb.WriteString("\n")
				continue
			}
		}
		sb.WriteString("- ")
		sb.WriteString(evidence)
		sb.WriteString("\n")
	}

	sb.WriteString("\nRun ledger:\n")
	if entry == nil {
		sb.WriteString("- no ledger entry for step\n")
	} else {
		fmt.Fprintf(&sb, "- tool_calls: %d\n", entry.ToolCalls)
		fmt.Fprintf(&sb, "- tools: %d\n", len(entry.Tools))
		if len(entry.Tools) > 0 {
			sb.WriteString("  ")
			sb.WriteString(strings.Join(entry.Tools, ", "))
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "- files_touched: %d\n", len(entry.FilesTouched))
		if len(entry.FilesTouched) > 0 {
			sb.WriteString("  ")
			sb.WriteString(strings.Join(entry.FilesTouched, ", "))
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "- commands: %d\n", len(entry.Commands))
		if len(entry.Commands) > 0 {
			for _, cmd := range entry.Commands {
				sb.WriteString("  - ")
				sb.WriteString(cmd)
				sb.WriteString("\n")
			}
		}
		fmt.Fprintf(&sb, "- duplicate_effects: %d\n", entry.DuplicateEffects)
		fmt.Fprintf(&sb, "- partial_effects: %t\n", entry.PartialEffects)
		fmt.Fprintf(&sb, "- completed: %t\n", entry.Completed)
	}

	if a.config != nil && a.config.Plan.RequireExpectedArtifactPaths {
		sb.WriteString("\nStrict mode:\n- require_expected_artifact_paths: enabled\n")
	} else {
		sb.WriteString("\nStrict mode:\n- require_expected_artifact_paths: disabled\n")
	}

	return sb.String()
}

func selectPlanProofStep(steps []*plan.Step, currentStepID int, requestedStepID int) *plan.Step {
	if requestedStepID > 0 {
		for _, step := range steps {
			if step != nil && step.ID == requestedStepID {
				return step
			}
		}
		return nil
	}
	if currentStepID > 0 {
		for _, step := range steps {
			if step != nil && step.ID == currentStepID {
				return step
			}
		}
	}
	for _, step := range steps {
		if step != nil && (step.Status == plan.StatusInProgress || step.Status == plan.StatusPaused || step.Status == plan.StatusFailed) {
			return step
		}
	}
	for i := len(steps) - 1; i >= 0; i-- {
		step := steps[i]
		if step == nil {
			continue
		}
		if len(step.Evidence) > 0 || strings.TrimSpace(step.VerificationNote) != "" {
			return step
		}
	}
	for _, step := range steps {
		if step != nil {
			return step
		}
	}
	return nil
}

func prettyProofJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	b, err := json.MarshalIndent(payload, "  ", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// GetJournalReport returns recent execution journal entries.
func (a *App) GetJournalReport() string {
	if a.journal == nil {
		return "Execution journal is not initialized."
	}
	entries, err := a.journal.Tail(30)
	if err != nil {
		return fmt.Sprintf("Failed to read execution journal: %v", err)
	}
	if len(entries) == 0 {
		return "Execution journal is empty."
	}
	var sb strings.Builder
	sb.WriteString("Execution journal (latest 30):\n")
	for _, e := range entries {
		fmt.Fprintf(&sb, "- %s  %s", e.Timestamp.Format("15:04:05"), e.Event)
		if detail := journalEntryDisplayDetail(e); detail != "" {
			fmt.Fprintf(&sb, " | %s", detail)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// journalEntryDisplayDetail keeps /journal useful for failure diagnosis while
// avoiding needless repetition of user prompts. Structured timeout/recovery
// metadata wins over message_preview; older events gracefully fall back to the
// fields they already stored.
func journalEntryDisplayDetail(entry JournalEntry) string {
	if len(entry.Details) == 0 {
		return ""
	}
	if entry.Event == "request_failed" || entry.Event == "auto_resume_scheduled" ||
		entry.Event == "rate_limit_auto_retry_scheduled" {
		reason, _ := entry.Details["failure_reason"].(string)
		if reason == "" {
			reason, _ = entry.Details["reason"].(string)
		}
		if reason != "" {
			parts := []string{reason}
			if provider, _ := entry.Details["provider"].(string); provider != "" {
				parts = append(parts, provider)
			}
			if timeout, _ := entry.Details["timeout"].(string); timeout != "" && timeout != "0s" {
				parts = append(parts, timeout)
			}
			if partial, _ := entry.Details["partial"].(bool); partial {
				parts = append(parts, "partial response")
			}
			if attempt, ok := entry.Details["attempt"]; ok {
				label := fmt.Sprintf("attempt %v", attempt)
				if maximum, maxOK := entry.Details["max_attempts"]; maxOK {
					label += fmt.Sprintf("/%v", maximum)
				}
				parts = append(parts, label)
			}
			return strings.Join(parts, " · ")
		}
	}
	if preview, _ := entry.Details["message_preview"].(string); preview != "" {
		return preview
	}
	reason, _ := entry.Details["reason"].(string)
	return reason
}

// GetRecoveryReport returns the latest persisted recovery snapshot.
func (a *App) GetRecoveryReport() string {
	var sb strings.Builder
	if a.journal == nil {
		sb.WriteString("Recovery snapshot unavailable: journal not initialized.")
	} else if snap, err := a.journal.LoadRecovery(); err != nil {
		fmt.Fprintf(&sb, "Failed to load recovery snapshot: %v", err)
	} else if snap == nil {
		sb.WriteString("No recovery snapshot found.")
	} else {
		pendingDesc := fmt.Sprintf("%q", snap.PendingMessage)
		if n := len(snap.PendingMessages); n > 1 {
			pendingDesc = fmt.Sprintf("%d queued, next: %q", n, snap.PendingMessages[0])
		}
		fmt.Fprintf(&sb,
			"Recovery snapshot:\n- updated: %s\n- session: %s\n- processing: %v\n- pending: %s\n- history_len: %d\n- plan_id: %s\n- current_step: %d",
			snap.Timestamp.Format("2006-01-02 15:04:05"), snap.SessionID, snap.Processing, pendingDesc, snap.HistoryLen, snap.PlanID, snap.CurrentStepID,
		)
	}

	if a.session != nil {
		recoveries := a.session.GetPendingRecoveries()
		if len(recoveries) > 0 {
			sb.WriteString("\n\nDurable side-effect recoveries:")
			for _, recovery := range recoveries {
				fmt.Fprintf(&sb,
					"\n- id: %s | state: %s | kind: %s | attempt: %d | checkpoints: %d | not_before: %s",
					recovery.ID, recovery.State, recovery.Kind, recovery.Attempt,
					len(recovery.Checkpoints), recovery.NotBefore.Format("2006-01-02 15:04:05"),
				)
			}
		}
	}
	return sb.String()
}

// GetObservabilityReport returns a unified operational report.
func (a *App) GetObservabilityReport() string {
	var sb strings.Builder
	sb.WriteString(a.GetRuntimeHealthReport())
	sb.WriteString("\n\n")
	sb.WriteString(a.GetPolicyReport())
	sb.WriteString("\n\n")
	sb.WriteString(a.GetLedgerReport())
	sb.WriteString("\n\n")
	sb.WriteString(a.GetRecoveryReport())
	sb.WriteString("\n\n")
	sb.WriteString(a.GetSessionGovernanceReport())
	sb.WriteString("\n\n")
	sb.WriteString(a.GetJournalReport())
	return sb.String()
}

// GetSessionGovernanceReport returns memory-governance statistics.
func (a *App) GetSessionGovernanceReport() string {
	a.sessionArchiveMu.Lock()
	defer a.sessionArchiveMu.Unlock()
	last := "never"
	if !a.lastSessionArchive.IsZero() {
		last = a.lastSessionArchive.Format("2006-01-02 15:04:05")
	}
	return fmt.Sprintf(
		"Session memory governance:\n- archive_ops: %d\n- archived_messages: %d\n- last_archive: %s\n- soft_limit: %d\n- keep_tail: %d",
		a.sessionArchiveOperations,
		a.sessionArchivedMessages,
		last,
		sessionGovernanceSoftLimit,
		sessionGovernanceKeepTail,
	)
}

// GetMemoryReport returns a summary of stored memories for the /memory command.
func (a *App) GetMemoryReport() string {
	if a.memoryStore == nil {
		return "Memory store not configured."
	}
	return a.memoryStore.GetReport()
}

// GetAgentTypeRegistry returns the agent type registry.
func (a *App) GetAgentTypeRegistry() *agent.AgentTypeRegistry {
	return a.agentTypeRegistry
}

// AppInterface implementation for commands package

// RefreshTokenCount recalculates token count and sends update to UI.
func (a *App) RefreshTokenCount() {
	a.refreshTokenCount()
}

// GetSession returns the current session.
func (a *App) GetSession() *chat.Session {
	return a.session
}

// ResumeLastSession loads and restores the most recent session before Run() is called.
func (a *App) ResumeLastSession() error {
	if a.sessionManager == nil {
		return fmt.Errorf("session manager not configured")
	}

	state, info, err := a.sessionManager.LoadLatest()
	if err != nil {
		return fmt.Errorf("failed to load last session: %w", err)
	}
	if state == nil || len(state.History) == 0 {
		return fmt.Errorf("no previous session found")
	}
	return a.restoreLoadedSession(state, info, "previous session")
}

// ResumeSession loads one exact session ID for the current workspace. Unlike
// ResumeLastSession it never falls back to another snapshot: missing, corrupt,
// foreign-project, and provider-incompatible state all fail before any model
// or tool call. This is the safe boundary used by headless --resume <id>.
func (a *App) ResumeSession(sessionID string) error {
	if a.sessionManager == nil {
		return fmt.Errorf("session manager not configured")
	}
	state, info, err := a.sessionManager.LoadSession(sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}
	if state == nil {
		return fmt.Errorf("session %q was not found", sessionID)
	}
	return a.restoreLoadedSession(state, info, fmt.Sprintf("session %q", sessionID))
}

func (a *App) restoreLoadedSession(state *chat.SessionState, info *chat.SessionInfo, label string) error {
	if state == nil {
		return fmt.Errorf("cannot restore an empty session state")
	}
	if len(state.History) == 0 {
		return fmt.Errorf("%s has no conversation history", label)
	}

	// Cross-provider guard — mirrors the automatic startup auto-resume check
	// in Run() (below). ResumeLastSession backs the --resume CLI flag
	// (interactive AND headless), and setting a.sessionPreloaded = true
	// below makes Run()'s OWN guard a no-op (`if a.sessionPreloaded {
	// sessionRestored = true }`), so this is the ONLY place that can catch
	// a cross-provider restore on this path. Without it, `gokin --provider
	// deepseek --resume` against a session authored under a different
	// provider (e.g. kimi/glm) silently drops assistant turns on the next
	// request (unsigned thinking-signature replay across providers). Empty
	// state.Provider = legacy session predating the provider tag; treat as
	// compatible.
	currentProvider := runtimeProviderForConfig(a.config)
	if state.Provider != "" && currentProvider != "" && state.Provider != currentProvider {
		return fmt.Errorf("%w: %s was on provider %s but you're now on %s — history formats are incompatible; start a new session, or switch back with --provider %s",
			ErrSessionProviderMismatch,
			label, state.Provider, currentProvider, state.Provider)
	}

	if err := a.sessionManager.RestoreFromState(state); err != nil {
		return fmt.Errorf("failed to restore session: %w", err)
	}

	a.mu.Lock()
	a.scratchpad = a.session.GetScratchpad()
	a.sessionPreloaded = true
	a.mu.Unlock()

	if a.agentRunner != nil {
		a.agentRunner.SetSharedScratchpad(a.scratchpad)
	}
	if a.executor != nil {
		a.executor.SetSessionID(state.ID)
	}

	// Restore tool checkpoints into the executor's journal
	a.restoreToolCheckpoints()

	loadedID := state.ID
	messageCount := len(state.History)
	if info != nil {
		loadedID = info.ID
		messageCount = info.MessageCount
	}
	logging.Info("pre-loaded session", "session_id", loadedID, "messages", messageCount)
	return nil
}

// GetHistoryManager returns a new history manager.
func (a *App) GetHistoryManager() (*chat.HistoryManager, error) {
	return chat.NewHistoryManager()
}

// GetContextManager returns the context manager.
func (a *App) GetContextManager() *appcontext.ContextManager {
	return a.contextManager
}

// announceRestoredLoops surfaces any loops restored from disk at startup so a
// user who set up a loop in a prior session, closed gokin, and reopened it
// knows loops are about to start firing in the background (otherwise an
// iteration triggering looks like the model "spontaneously" doing work).
//
// It MUST NOT block the caller: it runs during App.Run() BEFORE a.program.Run()
// starts draining Bubble Tea's unbuffered msgs channel, so a synchronous
// safeSendToProgram here would deadlock the entire startup forever (the screen
// stuck on "Starting Gokin..."). The active-loop toast is therefore replayed
// from a goroutine after a short delay, exactly like the MCP initial summary —
// both are latent landmines that only fire once a user has restored state at
// boot. Returning promptly is the invariant pinned by the regression test.
func (a *App) announceRestoredLoops() {
	if a.loopManager == nil {
		return
	}
	if active := a.loopManager.Active(); len(active) > 0 {
		count := len(active)
		a.safeGo("loops-restore-hint", func() {
			select {
			case <-time.After(800 * time.Millisecond):
			case <-a.ctx.Done():
				return
			}
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusInfo,
				Message: fmt.Sprintf("Resumed %d active loop(s) — /loop to view, /loop pause <id> to halt.", count),
			})
		})
	} else if all := a.loopManager.List(); len(all) > 0 {
		// All loops are non-running (paused/stopped/completed). Less urgent —
		// debug log only; the user can /loop list to see them whenever.
		logging.Debug("loops: restored from disk but none active", "total", len(all))
	}
}

// safeGo runs fn in a new goroutine with panic recovery. A panic in a
// background goroutine would otherwise crash the whole process — use
// this wrapper for any long-lived or periodic worker goroutine.
//
// Captures a stack snapshot via logging.PanicStack so post-mortem debugging
// doesn't require reproducing the panic. Without the stack, a periodic-
// cleanup or watchdog crash showed up in logs as just "panic: nil map"
// with no indication which line of which file actually faulted.
func (a *App) safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Error("background goroutine recovered from panic",
					"name", name,
					"panic", r,
					"stack", logging.PanicStack())
			}
		}()
		fn()
	}()
}

// humanizeAge formats a duration as a short, colloquial age string. Used for
// user-facing messages about interrupted runs and previous-session hints so
// they read naturally regardless of how long ago they happened. Negative
// ages (clock skew) collapse to "just now".
func humanizeAge(age time.Duration) string {
	age = age.Round(time.Minute)
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%d min ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(age.Hours()))
	case age < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(age.Hours())/24)
	default:
		return fmt.Sprintf("%d+ days ago", int(age.Hours())/24)
	}
}

// reserveProgramSend appends one sender to the global delivery order. The
// returned predecessor closes when every earlier reservation has finished;
// the caller must close done on every exit path.
func (a *App) reserveProgramSend() (predecessor <-chan struct{}, done chan struct{}) {
	a.programSendOrderMu.Lock()
	predecessor = a.programSendTail
	done = make(chan struct{})
	a.programSendTail = done
	a.programSendOrderMu.Unlock()
	return predecessor, done
}

func waitForProgramSend(predecessor <-chan struct{}) {
	if predecessor != nil {
		<-predecessor
	}
}

// sendProgramMessage performs one raw Bubble Tea send after ordering has been
// established by the caller. It must never be called directly from a Model
// Update callback because Program.Send's message channel is unbuffered.
func (a *App) sendProgramMessage(msg tea.Msg) {
	a.programMu.RLock()
	program := a.program
	a.programMu.RUnlock()
	if program == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			logging.Debug("sendProgramMessage recovered from panic (shutdown race)", "error", r)
		}
	}()
	program.Send(msg)
}

// hasProgram reports whether interactive Bubble Tea delivery is available.
// All production reads of a.program must go through this helper: Run installs
// the pointer concurrently with worker callbacks, and programMu—not a.mu—is
// the ownership lock for that lifecycle state.
func (a *App) hasProgram() bool {
	a.programMu.RLock()
	defer a.programMu.RUnlock()
	return a.program != nil
}

// safeSendToProgram safely sends a message to the Bubble Tea program from a
// worker/lifecycle goroutine. Delivery remains synchronous so callers that
// publish terminal state before starting the next operation retain that
// ordering contract.
//
// Guards the `program` reference with a DEDICATED mutex (`programMu`), not
// the primary `a.mu`. This distinction is load-bearing: callers such as
// ApplyConfig, TogglePermissions, and CompactContextWithPlan legitimately
// hold `a.mu` for the duration of their mutation, and a Go `sync.Mutex` is
// non-reentrant — routing this function through `a.mu` would self-deadlock
// those goroutines forever, visible to users as `/provider <other>` (and
// every other config-mutating command) hanging with "Generating 11s" that
// never resolves. `programMu` is independent, held briefly, and read-locked
// (RLock) on the hot path so concurrent sends don't serialize.
func (a *App) safeSendToProgram(msg tea.Msg) {
	predecessor, done := a.reserveProgramSend()
	defer close(done)
	waitForProgramSend(predecessor)
	a.sendProgramMessage(msg)
}

// safeSendToProgramAsync reserves the same ordered delivery stream but returns
// before Program.Send. Use it exactly for callbacks invoked from Bubble Tea's
// Update (submit/cancel/modal-open): the event loop must regain control before
// it can receive these messages. A group is delivered contiguously and in order.
func (a *App) safeSendToProgramAsync(messages ...tea.Msg) {
	if len(messages) == 0 {
		return
	}
	predecessor, done := a.reserveProgramSend()
	a.safeGo("bubbletea-async-send", func() {
		defer close(done)
		waitForProgramSend(predecessor)
		for _, msg := range messages {
			a.sendProgramMessage(msg)
		}
	})
}

// sendTokenUsageUpdate sends a token usage update to the UI.
// This can be called from any goroutine safely.
func (a *App) sendTokenUsageUpdate() {
	// a.config is swapped by ApplyConfig under a.mu; this is reachable
	// cross-goroutine (a background /loop iteration's token-usage callback runs
	// exactly when a runtime /settings toggle's ApplyConfig worker can fire), so snapshot
	// under the lock. Same ApplyConfig-reader race as GetUIRuntimeStatus.
	a.mu.Lock()
	cm := a.contextManager
	show := a.config != nil && a.config.UI.ShowTokenUsage
	a.mu.Unlock()
	if cm == nil || !show {
		return
	}

	usage := cm.GetTokenUsage()
	if usage == nil {
		return
	}

	a.safeSendToProgram(ui.TokenUsageMsg{
		Tokens:       usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		MaxTokens:    usage.MaxTokens,
		PercentUsed:  usage.PercentUsed,
		NearLimit:    usage.NearLimit,
		IsEstimate:   usage.IsEstimate,
	})

	// Also send full health data for observatory if context tracking is active
	a.sendContextHealthUpdate()
}

// handleRateLimitMetadata updates the app's rate limiters with metadata from provider.
func (a *App) handleRateLimitMetadata(rl *client.RateLimitMetadata) {
	limiter := a.rateLimiterSnapshot()
	if rl == nil || limiter == nil {
		return
	}

	limiter.UpdateLimits(
		rl.RequestsLimit, rl.RequestsRemaining, rl.RequestsReset,
		rl.TokensLimit, rl.TokensRemaining, rl.TokensReset,
	)

	// Refresh UI health observatory
	a.sendContextHealthUpdate()
}

// sendContextHealthUpdate sends a detailed context health update to the UI.
func (a *App) sendContextHealthUpdate() {
	var msg ui.ContextHealthMsg

	// Priority: check active agent in runner first (for sub-agents/plans)
	if a.agentRunner != nil {
		if agent := a.agentRunner.GetActiveAgent(""); agent != nil {
			health := agent.GetContextHealth()
			msg = ui.ContextHealthMsg{
				TotalTokens:       health.TotalTokens,
				MaxTokens:         health.MaxTokens,
				PercentUsed:       health.PercentUsed,
				SystemTokens:      health.SystemTokens,
				InstructionTokens: health.InstructionTokens,
				HistoryTokens:     health.HistoryTokens,
				ToolTokens:        health.ToolTokens,
				ActiveFiles:       health.ActiveFiles,
				LastPruningTime:   health.LastPruningTime,
				PruningAlert:      health.PruningAlert,
			}
		}
	}

	// Fallback to main context if agent not found or no data
	if msg.TotalTokens == 0 && a.contextManager != nil {
		total, max, percent := a.contextManager.GetContextHealth()
		msg = ui.ContextHealthMsg{
			TotalTokens: total,
			MaxTokens:   max,
			PercentUsed: percent,
			// Approximate breakdown for main session
			SystemTokens:  2000,
			HistoryTokens: total - 2000,
		}
	}

	// Add rate limit info if available
	if limiter := a.rateLimiterSnapshot(); limiter != nil {
		stats := limiter.Stats()
		msg.RequestsRemaining = int64(stats.AvailableRequests)
		msg.TokensRemaining = int64(stats.AvailableTokens)
	}

	a.safeSendToProgram(msg)
}

// refreshTokenCount recalculates token count from session history and sends update to UI.
// More expensive than sendTokenUsageUpdate - call after history changes.
func (a *App) refreshTokenCount() {
	if a.contextManager == nil {
		return
	}
	// Bound the count_tokens round-trip. On a cache miss UpdateTokenCount hits
	// the network, and a.ctx has no deadline — an unreachable/slow endpoint
	// would otherwise stall this call up to the transport's 120s
	// ResponseHeaderTimeout. UpdateTokenCount falls back to a local estimate on
	// error, so a timeout degrades gracefully rather than failing.
	timeout := a.tokenRefreshTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	defer cancel()
	if err := a.contextManager.UpdateTokenCount(ctx); err != nil {
		logging.Debug("failed to refresh token count", "error", err)
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
// clearTodosForNewConversation empties the foreground TodoTool and pushes the
// now-empty list to the UI. The todo list is conversation-scoped: without
// this, the Ctrl+T panel and the always-on "Ctrl+T tasks N" status-bar hint
// keep showing the PREVIOUS conversation's tasks after /clear — a violation of
// the "/clear = clean slate" invariant (same class as the permission-session
// and granted-dirs resets nearby).
func (a *App) clearTodosForNewConversation() {
	if a.registry == nil {
		return
	}
	todoTool, ok := a.registry.Get("todo")
	if !ok {
		return
	}
	tt, ok := todoTool.(*tools.TodoTool)
	if !ok {
		return
	}
	tt.ClearItems()
	// emitTodoUpdate renders the (now empty) list and sends TodoUpdateMsg +
	// clears the activity label — the UI drops both todo surfaces.
	a.emitTodoUpdate()
}

func (a *App) ClearConversation() {
	if err := a.ClearConversationChecked(); err != nil {
		logging.Warn("conversation clear did not reach a fully durable boundary", "error", err)
		message := "Conversation was not cleared because its durable session boundary could not be saved; no conversation state was discarded"
		if errors.Is(err, errRecoveryCommitUncertain) {
			message = "Conversation was cleared, but storage could not confirm rename durability — do not restart until session storage is healthy"
		}
		a.safeSendToProgramAsync(ui.StatusUpdateMsg{
			Type: ui.StatusWarning, Message: message,
		})
	}
}

// ClearConversationChecked performs a fail-closed conversation reset and
// reports whether its persistence boundary was definite. Commands that need
// an honest success result use this method through an optional interface;
// ClearConversation remains for compatibility with embedded integrations.
func (a *App) ClearConversationChecked() error {
	// Clear the executable retry lineage and persist that boundary synchronously
	// while holding the same lease mutex as persist/claim/session switch. A
	// periodic save is too late: a crash immediately after /clear must not
	// resurrect and auto-run a scheduled mutation from the old conversation.
	droppedRecoveries, clearPersistErr := a.clearConversationPersistenceBoundary()
	if clearPersistErr != nil && !errors.Is(clearPersistErr, errRecoveryCommitUncertain) {
		return fmt.Errorf("persist cleared recovery boundary: %w", clearPersistErr)
	}
	if droppedRecoveries > 0 {
		logging.Info("discarded queued recoveries at conversation clear", "count", droppedRecoveries)
	}

	// /clear is a true conversation boundary. Invalidate both automatic
	// session-summary memory (including its persisted file and any in-flight
	// stale writer) and keyed `scope=session` entries BEFORE rebuilding the
	// prompt; otherwise the new conversation starts with facts from the old one.
	if a.sessionMemory != nil {
		a.sessionMemory.Clear()
	}
	clearedKeyedSessionMemory := 0
	if a.memoryStore != nil {
		clearedKeyedSessionMemory = a.memoryStore.ClearSession()
	}
	if clearedKeyedSessionMemory > 0 && a.promptBuilder != nil {
		a.promptBuilder.Invalidate()
	}

	// Re-set system instruction via API parameter. The session retains only the
	// canonical prompt; invocation-scoped CLI content stays client-only.
	systemPrompt := a.buildDefaultSystemInstruction()
	a.applySystemInstruction(a.client, systemPrompt, true)

	// Clear per-session telemetry so /stats and /cost after /clear reflect the
	// new conversation instead of accumulating across unrelated tasks. Also
	// zeros the adaptive-budget depth counters, because old turn/tool
	// totals shouldn't inflate budget decisions for a fresh task.
	if a.phaseMetrics != nil {
		a.phaseMetrics.Reset()
	}
	if a.toolMetrics != nil {
		a.toolMetrics.Reset()
	}
	if a.taskRouter != nil {
		a.taskRouter.ResetDepth()
	}
	if a.executor != nil {
		a.executor.ResetSession()
	}
	a.clearTodosForNewConversation()
	// Session directory grants are conversation-scoped — /clear revokes them and
	// re-propagates (persisted config dirs are untouched).
	a.resetGrantedDirs()
	// Permission session state is conversation-scoped too: clear auto-approved
	// (allow-for-session) tools + the decision cache so a fresh conversation
	// re-prompts for caution-level tools. Without this, an "allow for session"
	// approval of write/edit/etc. silently persists across /clear into unrelated
	// conversations (ClearSession otherwise had no production caller).
	if a.permManager != nil {
		a.permManager.ClearSession()
	}
	// totalOutputTokens accumulates via +=; totalInputTokens is periodically
	// re-assigned but its last pre-clear value would persist until the next
	// exchange. Both must be zeroed so /cost shows only the fresh session.
	//
	// These fields (and lastError/lastErrorTime/stopHookActive below) are
	// a.mu-guarded on the message-processing path, and rateLimitRetryCount is
	// rateLimitRetryMu-guarded (rate_limit_retry.go). ClearConversation is
	// normally safe lock-free because every regular caller runs under
	// executeCommandCtx with a.processing=true (mutually exclusive with message
	// processing) — but the masked key-entry login worker (handleKeyEntrySubmit)
	// calls commandHandler.Execute("login") directly, OFF the processing gate, so
	// a concurrent user submit can race these writes. Lock each field write under
	// its own guard. pushTurnContext MUST stay OUTSIDE a.mu (it is non-reentrant
	// w.r.t. a.mu — self-deadlock, per the v0.100.42 invariant).
	a.mu.Lock()
	a.totalInputTokens = 0
	a.totalOutputTokens = 0
	a.totalCacheCreationTokens = 0
	a.totalCacheReadTokens = 0
	a.totalEstimatedCost = 0
	a.costTracked = false
	a.mu.Unlock()

	// Clear working memory so stale file/command context from the old
	// conversation doesn't leak into the new one — and push the now-empty
	// snapshot so the client's per-turn context is cleared too.
	if a.workingMemory != nil {
		a.workingMemory.Clear()
	}
	a.setRelevantMemoryContext("")
	a.pushTurnContext()

	// Drain stale rate-limit retry counters so exhausted keys from the
	// old conversation don't accumulate indefinitely (rateLimitRetryMu-guarded).
	a.rateLimitRetryMu.Lock()
	a.rateLimitRetryCount = make(map[string]int)
	a.rateLimitRetryMu.Unlock()

	// Same for auto-resume counters (autoResumeMu-guarded).
	a.autoResumeMu.Lock()
	a.autoResumeCount = make(map[string]int)
	a.autoResumeMu.Unlock()

	// Clear the carried-over error note + stop-hook marker under a.mu (their
	// guard). message_processor prepends "[Note: previous attempt failed with:
	// <lastError>...]" when lastError is < 2 min old — but /clear just wiped the
	// history, so that note would be false AND belong to an unrelated prior
	// conversation. A dropped Stop-hook continuation must not leave the next user
	// turn marked as a continuation (which would skip that turn's Stop hooks).
	a.mu.Lock()
	a.lastError = ""
	a.lastErrorTime = time.Time{}
	a.stopHookActive = false
	a.mu.Unlock()
	if clearPersistErr != nil {
		// By construction this is errRecoveryCommitUncertain (the early guard
		// above already returned on any OTHER error) — the whole clear
		// sequence ran unconditionally past that guard, so the conversation
		// genuinely cleared. Wrap it with the exported commands-package
		// marker (multi-%w — both sentinels stay errors.Is-reachable) so
		// commands.clearConversationChecked's callers can tell "cleared,
		// durability unconfirmed" apart from "did not clear" without this
		// package's unexported sentinel leaking across the boundary.
		return fmt.Errorf("%w: %w", commands.ErrConversationClearedUncertainDurability, clearPersistErr)
	}
	return nil
}

// CompactContextWithPlan clears the conversation and injects the plan summary.
// This is called when a plan is approved to free up context space.
//
// The root replacement shares /clear's durable recovery boundary so no older
// timer/checkpoint generation can enter the approved plan context.
func (a *App) CompactContextWithPlan(planSummary string) {
	systemPrompt := a.buildDefaultSystemInstruction()
	_, boundaryErr := a.replaceConversationPersistenceBoundary(func() {
		a.session.SetSystemInstruction(systemPrompt)

		// Inject the plan summary as a user message for execution context.
		if planSummary != "" {
			a.session.AddUserMessage("Execute the approved plan. Summary:\n\n" + planSummary)
		}
	})
	if boundaryErr != nil {
		logging.Warn("failed to persist compacted plan context", "error", boundaryErr)
		if errors.Is(boundaryErr, errRecoveryCommitUncertain) {
			a.applySystemInstruction(a.client, systemPrompt, false)
		}
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: "Plan context replacement could not be confirmed safely; plan execution was not started",
		})
		return
	}
	a.applySystemInstruction(a.client, systemPrompt, false)
	a.safeSendToProgram(ui.StreamTextMsg(
		"\n📋 Context cleared for plan execution. Previous conversation archived.\n"))

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

// GetConfig returns an independently owned snapshot. Commands stage edits on
// this candidate and hand it back to ApplyConfig; exposing the live pointer
// would let an unrelated config event publish an uncommitted value.
func (a *App) GetConfig() *config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot := a.config.Clone()
	if snapshot != nil {
		snapshot.SetSnapshotRevision(a.configRevision)
	}
	return snapshot
}

// GetRuntimeEngineMode reports the immutable mode owned by this process. It can
// differ from GetConfig().Engine.Mode after a config edit has staged the next
// launch's registry/runtime topology.
func (a *App) GetRuntimeEngineMode() string {
	return a.runtimeEngineModeSnapshot()
}

// RuntimeREPLCapabilityEnabled reports invocation authority rather than host
// availability. /doctor uses it to avoid starting a secure-runtime probe when
// --tools/--disallowedTools deliberately excludes repl_exec, including
// deny-only launches that skipped it before the final ceiling was materialized.
// It deliberately does not infer host availability from this policy signal.
func (a *App) RuntimeREPLCapabilityEnabled() bool {
	if a == nil || a.runtimeEngineModeSnapshot() == "tools" || a.runtimeREPLCapabilityDisabled {
		return false
	}
	a.mu.Lock()
	bare := a.config != nil && a.config.Bare
	a.mu.Unlock()
	if bare || a.registry == nil {
		return false
	}
	ceiling, restricted := a.toolCapabilitySnapshot()
	return !restricted || containsToolName(ceiling, "repl_exec")
}

// CommitMCPConfigSnapshot adopts the independently staged MCP configuration
// after MCPAddCore/MCPRemoveCore have already updated the live manager,
// registry, and YAML. A full ApplyConfig would unnecessarily rebuild the model
// client; this lightweight commit only restores App's authoritative snapshot.
func (a *App) CommitMCPConfigSnapshot(cfg *config.Config) {
	cfg = cfg.Clone()
	if cfg == nil {
		return
	}
	a.mu.Lock()
	merged := a.config.Clone()
	if merged == nil {
		merged = config.DefaultConfig()
	}
	baseMCP, tracked := cfg.SnapshotMCP()
	merged.MCP = mergeMCPConfig(merged.MCP, cfg.MCP, baseMCP, tracked)
	saveErr := merged.Save()
	if saveErr != nil {
		logging.Warn("failed to save merged MCP config", "error", saveErr, "path", config.GetConfigPath())
	}
	a.config = merged
	// MCPAddCore/MCPRemoveCore receive a client snapshot before their network
	// work begins. ApplyConfig may replace that client while the operation is in
	// flight, so re-publish the authoritative registry to whichever client is
	// current at commit time. Holding a.mu also orders this against client swaps.
	if a.client != nil && a.registry != nil {
		a.client.SetTools(a.planModeToolsLocked(a.planningModeEnabled))
	}
	msg := ui.ConfigUpdateMsg{
		Revision:            a.nextConfigRevisionLocked(),
		Settings:            a.settingToggleSnapshotLocked(),
		PermissionsEnabled:  merged.Permission.Enabled,
		SandboxEnabled:      merged.Tools.Bash.Sandbox,
		PlanningModeEnabled: a.planningModeEnabled,
		CompactMode:         merged.UI.CompactMode,
		ReducedMotion:       merged.UI.ReducedMotion,
		ShowTokenUsage:      merged.UI.ShowTokenUsage,
		HintsEnabled:        merged.UI.HintsEnabled,
		ShowToolCalls:       merged.UI.ShowToolCalls,
		ModelName:           merged.Model.Name,
		ModelRoundTimeout:   merged.Tools.ModelRoundTimeout,
	}
	a.mu.Unlock()
	a.safeSendToProgram(msg)
	if saveErr != nil {
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: fmt.Sprintf("MCP changed for this session but could not be saved to %s", config.GetConfigPath()),
		})
	}
}

// mergeMCPConfig applies only the edits made since the caller's GetConfig
// snapshot. MCP tools may run concurrently, so replacing the whole server list
// here would make the last finisher silently erase a sibling add/remove.
func mergeMCPConfig(current, candidate, base config.MCPConfig, tracked bool) config.MCPConfig {
	cloneSection := func(section config.MCPConfig) config.MCPConfig {
		return (&config.Config{MCP: section}).Clone().MCP
	}
	if !tracked {
		return cloneSection(candidate)
	}

	merged := cloneSection(current)
	baseByName := make(map[string]config.MCPServerConfig, len(base.Servers))
	candidateByName := make(map[string]config.MCPServerConfig, len(candidate.Servers))
	for _, server := range base.Servers {
		baseByName[server.Name] = server
	}
	for _, server := range candidate.Servers {
		candidateByName[server.Name] = server
	}

	// An entry present in the base but absent from the candidate was explicitly
	// removed. Preserve all current entries that were not part of that removal.
	kept := merged.Servers[:0]
	for _, server := range merged.Servers {
		if _, existedAtBase := baseByName[server.Name]; existedAtBase {
			if _, remains := candidateByName[server.Name]; !remains {
				continue
			}
		}
		kept = append(kept, server)
	}
	merged.Servers = kept

	// Add new entries and replace entries changed relative to the caller's
	// base. Unchanged base entries intentionally leave newer sibling edits alone.
	for _, server := range candidate.Servers {
		baseServer, existedAtBase := baseByName[server.Name]
		if existedAtBase && reflect.DeepEqual(baseServer, server) {
			continue
		}
		replaced := false
		for i := range merged.Servers {
			if merged.Servers[i].Name == server.Name {
				merged.Servers[i] = cloneSection(config.MCPConfig{Servers: []config.MCPServerConfig{server}}).Servers[0]
				replaced = true
				break
			}
		}
		if !replaced {
			cloned := cloneSection(config.MCPConfig{Servers: []config.MCPServerConfig{server}})
			merged.Servers = append(merged.Servers, cloned.Servers[0])
		}
	}

	if candidate.Enabled != base.Enabled {
		merged.Enabled = candidate.Enabled
	}
	if candidate.HealthCheckInterval != base.HealthCheckInterval {
		merged.HealthCheckInterval = candidate.HealthCheckInterval
	}
	return merged
}

// CommitCredentialConfigSnapshot commits an intentional logout even when no
// replacement provider client can be constructed. It merges only credential /
// model identity state, drops every reference to the credential-bearing client,
// and preserves unrelated newer config commits.
func (a *App) CommitCredentialConfigSnapshot(cfg *config.Config) error {
	originalCandidate := cfg
	cfg = cfg.Clone()
	if cfg == nil {
		return fmt.Errorf("cannot commit nil credential config")
	}

	a.mu.Lock()
	if baseRevision, tracked := cfg.SnapshotRevision(); tracked && baseRevision != a.configRevision {
		a.mu.Unlock()
		return fmt.Errorf("%w (based on revision %d, current revision %d)", errConfigConflict, baseRevision, a.configRevision)
	}
	merged := a.config.Clone()
	if merged == nil {
		merged = config.DefaultConfig()
	}
	previousAPI := merged.API
	merged.API = cfg.API
	merged.Model = cfg.Model
	saveErr := merged.Save()
	if saveErr != nil {
		logging.Warn("failed to save credential removal", "error", saveErr, "path", config.GetConfigPath())
	}
	merged.ClearSnapshotRevision()
	a.config = merged

	// Evict pooled clients for every provider whose credential was removed.
	pool := client.GetPool(merged)
	for _, provider := range config.Providers {
		if provider.GetKey(&previousAPI) != "" && provider.GetKey(&merged.API) == "" {
			pool.FlushProvider(provider.Name)
		}
	}

	oldClient := a.client
	a.setClientLocked(nil)
	if a.executor != nil {
		a.executor.SetClient(nil)
	}
	if a.agentRunner != nil {
		a.agentRunner.SetClient(nil)
	}
	if a.contextManager != nil {
		a.contextManager.SetClient(nil)
	}
	if a.taskRouter != nil {
		a.taskRouter.SetClient(nil)
	}
	if a.sessionMemory != nil {
		a.sessionMemory.SetSummarizer(nil)
	}
	if a.session != nil {
		a.session.SetProvider(runtimeProviderForConfig(merged))
	}
	if a.promptBuilder != nil {
		provider := runtimeProviderForConfig(merged)
		a.promptBuilder.SetProvider(provider)
		a.promptBuilder.SetPrefixCachingEnabled(appcontext.ProviderSupportsPrefixCaching(provider))
	}

	msg := ui.ConfigUpdateMsg{
		Revision:            a.nextConfigRevisionLocked(),
		Settings:            a.settingToggleSnapshotLocked(),
		PermissionsEnabled:  merged.Permission.Enabled,
		SandboxEnabled:      merged.Tools.Bash.Sandbox,
		PlanningModeEnabled: a.planningModeEnabled,
		CompactMode:         merged.UI.CompactMode,
		ReducedMotion:       merged.UI.ReducedMotion,
		ShowTokenUsage:      merged.UI.ShowTokenUsage,
		HintsEnabled:        merged.UI.HintsEnabled,
		ShowToolCalls:       merged.UI.ShowToolCalls,
		ModelName:           merged.Model.Name,
		ModelRoundTimeout:   merged.Tools.ModelRoundTimeout,
	}
	originalCandidate.SetSnapshotRevision(msg.Revision)
	a.mu.Unlock()

	if oldClient != nil {
		a.safeGo("close-client-after-logout", func() { _ = oldClient.Close() })
	}
	a.safeSendToProgram(msg)
	if saveErr != nil {
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: fmt.Sprintf("Credentials removed for this session but could not be saved to %s", config.GetConfigPath()),
		})
	}
	return saveErr
}

// PersistCurrentConfig serializes the current authoritative snapshot under the
// config commit lock. Login uses it for an honest persistence confirmation
// without re-saving an older caller-owned candidate over newer settings.
func (a *App) PersistCurrentConfig() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.config == nil {
		return fmt.Errorf("configuration is unavailable")
	}
	return a.config.Clone().Save()
}

// shortActiveProviderName returns a display-friendly label for the active
// provider, intended for inline status messages like toasts ("Kimi rate
// limit — resumes in 30s"). We take the first word of the provider's
// registered DisplayName rather than the raw key: "glm" alone reads as
// lowercase unexpanded, and the full DisplayName ("GLM (BigModel /
// Z.AI)") is too long for a status-bar toast. Falls back to the raw key
// when no DisplayName is registered, and to "Provider" when no active
// provider is set — the message still parses ("Provider rate limit …").
func (a *App) shortActiveProviderName() string {
	if a == nil || a.config == nil {
		return "Provider"
	}
	name := runtimeProviderForConfig(a.config)
	if name == "" {
		return "Provider"
	}
	if def := config.GetProvider(name); def != nil && def.DisplayName != "" {
		if idx := strings.IndexAny(def.DisplayName, " ("); idx > 0 {
			return def.DisplayName[:idx]
		}
		return def.DisplayName
	}
	return name
}

// GetTokenStats returns token usage statistics for the session.
// GetLifetimeToolUsage reports the persisted per-tool invocation counts.
//
// The never-used set is computed against this App's own registry: a tool that
// was never registered here cannot be judged on not having been chosen.
func (a *App) GetLifetimeToolUsage() commands.LifetimeToolUsage {
	counts := a.toolUsage.Snapshot()
	var total int64
	for _, count := range counts {
		total += count
	}
	usage := commands.LifetimeToolUsage{Counts: counts, Total: total}
	if a.registry != nil {
		usage.NeverUsed = a.toolUsage.NeverUsed(a.registry.Names())
	}
	return usage
}

func (a *App) GetTokenStats() commands.TokenStats {
	var promptCache client.CacheStats
	if a.executor != nil {
		if tracker := a.executor.GetCacheTracker(); tracker != nil {
			promptCache = tracker.GetStats()
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return commands.TokenStats{
		InputTokens:                a.totalInputTokens,
		OutputTokens:               a.totalOutputTokens,
		CacheCreationInputTokens:   a.totalCacheCreationTokens,
		CacheReadInputTokens:       a.totalCacheReadTokens,
		PromptCacheBreaks:          promptCache.CacheBreaks,
		LastPromptCacheBreakReason: promptCache.LastBreakReason,
		TotalTokens:                a.totalInputTokens + a.totalOutputTokens,
		EstimatedCost:              a.totalEstimatedCost,
		CostTracked:                a.costTracked,
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

	// Keep the persisted source of truth in sync. Without this the next
	// ApplyConfig (step 8: permManager.SetEnabled(a.config.Permission.Enabled))
	// would silently revert this toggle mid-session — e.g. toggling YOLO off
	// then running /model would re-enable permission prompts. (ToggleSandbox
	// already keeps a.config.Tools.Bash.Sandbox in sync for the same reason.)
	a.config.Permission.Enabled = newEnabled

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

	// Persist and capture the UI snapshot in the same critical-section order as
	// the revision. Saving after unlock lets an older toggle overwrite a newer
	// config file and can marshal a concurrently mutated live pointer.
	saveErr := a.config.Clone().Save()
	// Copy state for UI message before unlocking
	sandboxEnabled := a.config.Tools.Bash.Sandbox
	planningModeEnabled := a.planningModeEnabled
	compactMode := a.config.UI.CompactMode
	reducedMotion := a.config.UI.ReducedMotion
	showTokenUsage := a.config.UI.ShowTokenUsage
	hintsEnabled := a.config.UI.HintsEnabled
	showToolCalls := a.config.UI.ShowToolCalls
	modelName := a.config.Model.Name
	settingsSnapshot := a.settingToggleSnapshotLocked()
	configRevision := a.nextConfigRevisionLocked()
	a.mu.Unlock()

	// Persist so the toggle survives restart (matches ToggleSandbox). Surface a
	// write failure honestly instead of silently losing it.
	if saveErr != nil {
		logging.Warn("failed to save permission setting", "error", saveErr)
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: fmt.Sprintf("Permissions toggled for this session only — couldn't save to %s", config.GetConfigPath()),
		})
	}

	a.safeSendToProgram(ui.ConfigUpdateMsg{
		Revision:            configRevision,
		Settings:            settingsSnapshot,
		PermissionsEnabled:  newEnabled,
		SandboxEnabled:      sandboxEnabled,
		PlanningModeEnabled: planningModeEnabled,
		CompactMode:         compactMode,
		ReducedMotion:       reducedMotion,
		ShowTokenUsage:      showTokenUsage,
		HintsEnabled:        hintsEnabled,
		ShowToolCalls:       showToolCalls,
		ModelName:           modelName,
	})

	return newEnabled
}

// TogglePlanningMode toggles the tree planning mode on/off.
func (a *App) TogglePlanningMode() bool {
	enabled, _ := a.togglePlanningModeWithRevision()
	return enabled
}

// togglePlanningModeWithRevision returns the state and config revision from
// the same committed mutation. Async UI feedback must carry both so a delayed
// completion cannot repaint a newer authoritative ConfigUpdateMsg.
func (a *App) togglePlanningModeWithRevision() (bool, uint64) {
	a.mu.Lock()

	a.planningModeEnabled = !a.planningModeEnabled
	newEnabled := a.planningModeEnabled
	if a.config != nil {
		a.config.Plan.Enabled = newEnabled
	}

	if a.planManager != nil {
		a.planManager.SetEnabled(newEnabled)
	}

	// Update agent runner
	if a.agentRunner != nil {
		a.agentRunner.SetPlanningModeEnabled(newEnabled)
	}

	// Keep the router in sync so its per-request tool filtering doesn't re-add
	// mutating tools to the schema while plan mode is active. Router's SetPlanMode
	// takes its own lock (not a.mu) so calling it here is deadlock-safe.
	if a.taskRouter != nil {
		a.taskRouter.SetPlanMode(newEnabled)
	}

	// Update TUI display (direct setter for immediate effect)
	if a.tui != nil {
		a.tui.SetPlanningModeEnabled(newEnabled)
	}

	// Re-push the tool schema with/without the plan-mode read-only filter
	// so the LLM physically cannot see write/execute tools while in plan
	// mode (and regains them the instant we flip back). Uses the same
	// toolsForCurrentMode helper as ApplyConfig to stay consistent.
	if a.client != nil {
		a.client.SetTools(a.planModeToolsLocked(newEnabled))
	}

	// Rebuild the system prompt so the plan-mode banner is injected/removed.
	// Without this, the tool schema would filter to read-only but the model
	// would still see the general "you can write/edit/run commands" guidance
	// and waste a round trying a blocked tool before reading the error.
	if a.promptBuilder != nil {
		a.promptBuilder.SetPlanMode(newEnabled)
		systemPrompt := a.buildDefaultSystemInstruction()
		a.applySystemInstruction(a.client, systemPrompt, true)
	}

	if newEnabled {
		logging.Debug("planning mode enabled")
	} else {
		logging.Debug("planning mode disabled")
	}

	// Copy state for UI message before unlocking
	permissionsEnabled := a.permManager != nil && a.permManager.IsEnabled()
	sandboxEnabled := a.config.Tools.Bash.Sandbox
	compactMode := a.config.UI.CompactMode
	reducedMotion := a.config.UI.ReducedMotion
	showTokenUsage := a.config.UI.ShowTokenUsage
	hintsEnabled := a.config.UI.HintsEnabled
	showToolCalls := a.config.UI.ShowToolCalls
	modelName := a.config.Model.Name
	settingsSnapshot := a.settingToggleSnapshotLocked()
	configRevision := a.nextConfigRevisionLocked()
	a.mu.Unlock()

	a.safeSendToProgram(ui.ConfigUpdateMsg{
		Revision:            configRevision,
		Settings:            settingsSnapshot,
		PermissionsEnabled:  permissionsEnabled,
		SandboxEnabled:      sandboxEnabled,
		PlanningModeEnabled: newEnabled,
		CompactMode:         compactMode,
		ReducedMotion:       reducedMotion,
		ShowTokenUsage:      showTokenUsage,
		HintsEnabled:        hintsEnabled,
		ShowToolCalls:       showToolCalls,
		ModelName:           modelName,
	})

	return newEnabled, configRevision
}

// IsPlanningModeEnabled returns whether planning mode is active.
func (a *App) IsPlanningModeEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planningModeEnabled
}

// disablePlanModeAfterApproval flips planning mode OFF and re-pushes the full
// tool schema to the client. Called when the user approves a plan — Claude
// Code's contract is that approval hands control back to the agent for
// execution, which needs write/exec tools restored. Runs as its own
// lock+release cycle so approving inside a command goroutine doesn't
// interact with other lock holders.
func (a *App) disablePlanModeAfterApproval() {
	a.mu.Lock()
	wasEnabled := a.planningModeEnabled
	a.planningModeEnabled = false
	if a.config != nil {
		a.config.Plan.Enabled = false
	}
	if a.planManager != nil {
		a.planManager.SetEnabled(false)
	}
	if a.agentRunner != nil {
		a.agentRunner.SetPlanningModeEnabled(false)
	}
	if a.taskRouter != nil {
		a.taskRouter.SetPlanMode(false)
	}
	if a.tui != nil {
		a.tui.SetPlanningModeEnabled(false)
	}
	// Capture for UI update BEFORE dropping the lock so the snapshot is
	// consistent.
	permissionsEnabled := a.permManager != nil && a.permManager.IsEnabled()
	sandboxEnabled := a.config.Tools.Bash.Sandbox
	compactMode := a.config.UI.CompactMode
	reducedMotion := a.config.UI.ReducedMotion
	showTokenUsage := a.config.UI.ShowTokenUsage
	hintsEnabled := a.config.UI.HintsEnabled
	showToolCalls := a.config.UI.ShowToolCalls
	modelName := a.config.Model.Name
	client := a.client
	settingsSnapshot := a.settingToggleSnapshotLocked()
	configRevision := uint64(0)
	if wasEnabled {
		configRevision = a.nextConfigRevisionLocked()
	}
	a.mu.Unlock()

	if !wasEnabled {
		return // idempotent — no need to notify UI or push tools
	}

	// Restore full tool schema so the post-approval execution round sees
	// write/edit/bash/etc. again. Done outside the lock to avoid blocking
	// other goroutines on the client's own mutex. planningModeEnabled was
	// already set to false under lock above, so toolsForCurrentMode returns
	// the full set — consistent with every other SetTools call site.
	if client != nil {
		client.SetTools(a.toolsForCurrentMode())
	}

	// Drop the plan-mode banner from the system prompt — execution rounds
	// should see the usual guidance. Complements the SetTools above so the
	// model understands it's now free to write/edit/bash, not just
	// discovering the tools are suddenly available.
	if a.promptBuilder != nil {
		a.promptBuilder.SetPlanMode(false)
		systemPrompt := a.buildDefaultSystemInstruction()
		a.applySystemInstruction(client, systemPrompt, true)
	}

	a.safeSendToProgram(ui.ConfigUpdateMsg{
		Revision:            configRevision,
		Settings:            settingsSnapshot,
		PermissionsEnabled:  permissionsEnabled,
		SandboxEnabled:      sandboxEnabled,
		PlanningModeEnabled: false,
		CompactMode:         compactMode,
		ReducedMotion:       reducedMotion,
		ShowTokenUsage:      showTokenUsage,
		HintsEnabled:        hintsEnabled,
		ShowToolCalls:       showToolCalls,
		ModelName:           modelName,
	})
}

// SessionMode is a high-level user-facing mode: the combined state of plan
// mode, permissions, and bash sandbox in one value. Exposed as a 3-state
// cycle on Shift+Tab so users don't have to juggle /plan, /permissions,
// and /sandbox individually. Matches Claude Code's Shift+Tab cycle ergonomic.
type SessionMode int

const (
	// SessionModeNormal — everything safe. Permissions on (agent asks
	// before bash/write/edit), sandbox on (bash runs contained), plan
	// mode off. This is the "I trust the user, they'll review each
	// risky op" default.
	SessionModeNormal SessionMode = iota

	// SessionModePlan — Claude Code-style read-only exploration. Plan
	// mode on; permissions/sandbox values don't really matter because
	// the tool schema is filtered to read-only. Agent must propose a
	// plan via enter_plan_mode and wait for approval before touching
	// anything.
	SessionModePlan

	// SessionModeYOLO — "just do it". Permissions off (no prompts),
	// sandbox off (bash runs unrestricted), plan mode off. For tight
	// iteration cycles where the user is actively watching and doesn't
	// want to click approve 50 times. Risky — flip back to Normal when
	// walking away.
	SessionModeYOLO
)

// String renders a mode for status bars, toasts, and debug logs.
func (m SessionMode) String() string {
	switch m {
	case SessionModePlan:
		return "plan"
	case SessionModeYOLO:
		return "yolo"
	default:
		return "normal"
	}
}

// currentSessionMode derives the canonical mode enum from the three
// independent flags. Priority: plan > yolo > normal — plan mode is the
// strongest constraint (read-only schema) and takes precedence over
// permission state when both are "off"-shaped. Caller must hold a.mu
// (or accept a best-effort snapshot).
func (a *App) currentSessionMode() SessionMode {
	if a.planningModeEnabled {
		return SessionModePlan
	}
	if a.permManager != nil && !a.permManager.IsEnabled() {
		return SessionModeYOLO
	}
	return SessionModeNormal
}

// CycleSessionMode advances Normal → Plan → YOLO → Normal and applies
// the associated state change. Designed for a single-keystroke shortcut
// (Shift+Tab): user taps once to go into plan mode, twice for YOLO,
// three times back to Normal. Slash commands (/plan, /permissions,
// /sandbox) remain available for granular control.
//
// Returns the **actual** mode observed after the cascaded toggles
// complete, not merely the intended target — if a concurrent slash
// command ran between the read and the apply, the actual end state
// might differ from the naive "next in cycle". Callers (in particular
// CycleSessionModeAsync) need the real state so the toast + status
// bar don't lie.
func (a *App) CycleSessionMode() SessionMode {
	mode, _ := a.cycleSessionModeWithRevision()
	return mode
}

// cycleSessionModeWithRevision returns one ownership tuple for the async UI
// completion. Serializing the whole cycle prevents two accepted keypresses
// from reading the same starting mode, while taking mode and revision under
// a.mu together lets the TUI reject a completion superseded by newer config.
func (a *App) cycleSessionModeWithRevision() (SessionMode, uint64) {
	a.sessionModeCycleMu.Lock()
	defer a.sessionModeCycleMu.Unlock()

	a.mu.Lock()
	current := a.currentSessionMode()
	a.mu.Unlock()

	next := SessionModeNormal
	switch current {
	case SessionModeNormal:
		next = SessionModePlan
	case SessionModePlan:
		next = SessionModeYOLO
	case SessionModeYOLO:
		next = SessionModeNormal
	}

	a.applySessionMode(next)

	// Re-read after apply — flags may not exactly match `next` under
	// concurrent state change (e.g. a /permissions off fired during
	// the cascade). Return the ground truth so the caller reports
	// what actually happened.
	a.mu.Lock()
	actual := a.currentSessionMode()
	revision := a.configRevision
	a.mu.Unlock()
	return actual, revision
}

// applySessionMode normalizes the three flags (planningModeEnabled,
// permissions.Enabled, sandbox) to the canonical combination for the
// given target mode. Idempotent — calling with the already-current mode
// is safe (each sub-toggle is a no-op when the value already matches).
//
// Implementation detail: rather than duplicating the locking dance from
// TogglePlanningMode / TogglePermissions / ToggleSandbox, this method
// delegates to those existing methods. They each already emit the right
// UI updates (tool-schema push for plan, permission state for perms,
// bash sandbox flag for sandbox) and handle the mu/session-prompt
// invariants correctly. Trade-off: three potentially redundant lock
// acquisitions per cycle, but a user pressing Shift+Tab doesn't notice
// three microseconds.
func (a *App) applySessionMode(mode SessionMode) {
	// Read current state to compute minimal deltas.
	a.mu.Lock()
	wantPlan := mode == SessionModePlan
	wantPerms := mode != SessionModeYOLO
	wantSandbox := mode != SessionModeYOLO
	havePlan := a.planningModeEnabled
	havePerms := a.permManager == nil || a.permManager.IsEnabled()
	haveSandbox := a.config.Tools.Bash.Sandbox
	a.mu.Unlock()

	// Order matters: flip plan mode FIRST so the tool schema is correct
	// before any subsequent toggle emits ConfigUpdateMsg — prevents a
	// brief window where the status bar says "normal" but tools are still
	// filtered (or vice-versa).
	if havePlan != wantPlan {
		a.TogglePlanningMode()
	}
	if havePerms != wantPerms {
		a.TogglePermissions()
	}
	if haveSandbox != wantSandbox {
		a.ToggleSandbox()
	}
}

// CycleSessionModeAsync runs CycleSessionMode in a background goroutine
// so the Bubble Tea event loop isn't blocked on the (briefly) cascaded
// toggles. Wired to Shift+Tab in the TUI. Sends a
// SessionModeCycledMsg back to the UI with the new mode so the status
// bar + toast can update.
func (a *App) CycleSessionModeAsync() {
	a.safeGo("cycle-session-mode", func() {
		newMode, revision := a.cycleSessionModeWithRevision()
		a.safeSendToProgram(ui.SessionModeCycledMsg{
			Mode:     newMode.String(),
			Revision: revision,
		})
	})
}

// TogglePlanningModeAsync toggles planning mode asynchronously.
// This is safe to call from UI callbacks as it doesn't block the Bubble Tea event loop.
func (a *App) TogglePlanningModeAsync() {
	a.safeGo("toggle-planning-mode", func() {
		newEnabled, revision := a.togglePlanningModeWithRevision()
		a.safeSendToProgram(ui.PlanningModeToggledMsg{
			Enabled:  newEnabled,
			Revision: revision,
		})
	})
}

// ToggleSandbox toggles the bash sandbox mode on/off.
func (a *App) ToggleSandbox() bool {
	a.mu.Lock()

	a.config.Tools.Bash.Sandbox = !a.config.Tools.Bash.Sandbox
	newEnabled := a.config.Tools.Bash.Sandbox

	// Update bash tool sandbox setting
	if a.registry != nil {
		if bashTool, ok := a.registry.Get("bash"); ok {
			if bt, ok := bashTool.(*tools.BashTool); ok {
				bt.SetSandboxEnabled(newEnabled)
			}
		}
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

	// Persist in commit/revision order; see TogglePermissions.
	saveErr := a.config.Clone().Save()
	// Copy state for UI message before unlocking
	permissionsEnabled := a.permManager != nil && a.permManager.IsEnabled()
	planningModeEnabled := a.planningModeEnabled
	compactMode := a.config.UI.CompactMode
	reducedMotion := a.config.UI.ReducedMotion
	showTokenUsage := a.config.UI.ShowTokenUsage
	hintsEnabled := a.config.UI.HintsEnabled
	showToolCalls := a.config.UI.ShowToolCalls
	modelName := a.config.Model.Name
	settingsSnapshot := a.settingToggleSnapshotLocked()
	configRevision := a.nextConfigRevisionLocked()
	a.mu.Unlock()

	if saveErr != nil {
		logging.Warn("failed to save sandbox setting", "error", saveErr)
	}

	a.safeSendToProgram(ui.ConfigUpdateMsg{
		Revision:            configRevision,
		Settings:            settingsSnapshot,
		PermissionsEnabled:  permissionsEnabled,
		SandboxEnabled:      newEnabled,
		PlanningModeEnabled: planningModeEnabled,
		CompactMode:         compactMode,
		ReducedMotion:       reducedMotion,
		ShowTokenUsage:      showTokenUsage,
		HintsEnabled:        hintsEnabled,
		ShowToolCalls:       showToolCalls,
		ModelName:           modelName,
	})

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

	// Diff preview (the "Apply" confirmation on edit/write) must follow the
	// live permission state, not just the config-file snapshot taken at build
	// time. Without this, toggling YOLO (permissions off) at runtime leaves
	// diffEnabled=true on the tools forever — the apply prompt keeps showing
	// even though the user opted out of all confirmations.
	diffEnabled := a.config.DiffPreview.Enabled && !permissionOff

	// Update executor's unrestricted mode
	a.executor.SetUnrestrictedMode(unrestrictedMode)

	// Update bash tool's unrestricted mode
	if a.registry != nil {
		if bashTool, ok := a.registry.Get("bash"); ok {
			if bt, ok := bashTool.(*tools.BashTool); ok {
				bt.SetUnrestrictedMode(unrestrictedMode)
			}
		}
		// Sync diff preview on edit/write tools with live permission state.
		if tool, ok := a.registry.Get("edit"); ok {
			if et, ok := tool.(*tools.EditTool); ok {
				et.SetDiffEnabled(diffEnabled)
			}
		}
		if tool, ok := a.registry.Get("write"); ok {
			if wt, ok := tool.(*tools.WriteTool); ok {
				wt.SetDiffEnabled(diffEnabled)
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

// nextConfigRevisionLocked advances the authoritative UI-config snapshot
// sequence. Callers must hold a.mu so revisions follow commit order even when
// sends from different goroutines are delivered out of order.
func (a *App) nextConfigRevisionLocked() uint64 {
	a.configRevision++
	if a.configRevision == 0 {
		a.configRevision++
	}
	return a.configRevision
}

func (a *App) settingToggleSnapshotLocked() map[string]bool {
	if a.config == nil {
		return nil
	}
	states := commands.SettableToggleStates(a.config)
	snapshot := make(map[string]bool, len(states))
	for _, state := range states {
		snapshot[state.Key] = state.On
	}
	// Plan mode is a live session mode as well as a persisted preference. The
	// UI must reflect the actual runtime after Shift+Tab/approval transitions.
	snapshot["plan"] = a.planningModeEnabled
	return snapshot
}

// ApplyUIConfig persists and publishes presentation-only settings without
// flushing the provider pool, rebuilding the model client, or refreshing token
// counts. A theme/layout/accessibility toggle must remain usable even when the
// configured provider is temporarily unavailable or invalid.
func (a *App) ApplyUIConfig(cfg *config.Config) error {
	return a.applyUIConfigForSetting(cfg, "")
}

// ApplyUIConfigForSetting commits one presentation toggle without letting an
// older candidate overwrite concurrently committed sibling UI fields.
func (a *App) ApplyUIConfigForSetting(cfg *config.Config, key string) error {
	return a.applyUIConfigForSetting(cfg, strings.ToLower(strings.TrimSpace(key)))
}

// ApplyModelRoundTimeout commits the local model-round watchdog without
// flushing the provider pool, rebuilding the model client, or refreshing token
// counts. Only this scalar is merged into the latest authoritative config, so a
// candidate prepared before an unrelated setting commit cannot overwrite it.
// Existing in-flight provider rounds keep their original context deadline;
// foreground and sub-agent loops use the new value on their next round.
func (a *App) ApplyModelRoundTimeout(cfg *config.Config) (persisted bool, err error) {
	if cfg == nil {
		return false, fmt.Errorf("cannot apply nil model round timeout config")
	}
	desired := cfg.Tools.ModelRoundTimeout
	if desired <= 0 {
		desired = config.DefaultModelRoundTimeout
	}

	a.mu.Lock()
	merged := a.config.Clone()
	if merged == nil {
		merged = config.DefaultConfig()
	}
	merged.Tools.ModelRoundTimeout = desired
	timeoutConfigPath := merged.ModelRoundTimeoutConfigPath()
	saveErr := merged.SaveModelRoundTimeout(desired)
	if saveErr != nil {
		logging.Warn("failed to save model round timeout", "error", saveErr, "path", timeoutConfigPath)
	}
	merged.ClearSnapshotRevision()
	a.config = merged
	if a.executor != nil {
		a.executor.SetModelRoundTimeout(desired)
	}
	if a.agentRunner != nil {
		a.agentRunner.SetModelRoundTimeout(desired)
	}
	if a.metaAgent != nil {
		a.metaAgent.SetModelRoundTimeout(desired)
	}
	if a.contextManager != nil {
		a.contextManager.SetModelRoundTimeout(desired)
	}
	if a.sessionMemory != nil {
		a.sessionMemory.SetModelRoundTimeout(desired)
	}
	if merged.Plan.PlanningTimeout <= 0 {
		if a.treePlanner != nil {
			a.treePlanner.SetPlanningTimeout(desired)
		}
		if a.agentRunner != nil {
			a.agentRunner.SetPlanningTimeout(desired)
		}
	}
	msg := ui.ConfigUpdateMsg{
		Revision:            a.nextConfigRevisionLocked(),
		Settings:            a.settingToggleSnapshotLocked(),
		PermissionsEnabled:  merged.Permission.Enabled,
		SandboxEnabled:      merged.Tools.Bash.Sandbox,
		PlanningModeEnabled: a.planningModeEnabled,
		CompactMode:         merged.UI.CompactMode,
		ReducedMotion:       merged.UI.ReducedMotion,
		ShowTokenUsage:      merged.UI.ShowTokenUsage,
		HintsEnabled:        merged.UI.HintsEnabled,
		ShowToolCalls:       merged.UI.ShowToolCalls,
		ModelName:           merged.Model.Name,
		ModelRoundTimeout:   desired,
	}
	a.mu.Unlock()

	a.safeSendToProgram(msg)
	if saveErr != nil {
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: fmt.Sprintf("Timeout applied for this session but NOT saved to %s — it will revert next launch", timeoutConfigPath),
		})
	}
	return saveErr == nil, nil
}

func (a *App) applyUIConfigForSetting(cfg *config.Config, key string) error {
	if cfg == nil {
		return fmt.Errorf("cannot apply nil UI config")
	}
	cfg = cfg.Clone()

	a.mu.Lock()
	merged := a.config.Clone()
	if merged == nil {
		merged = config.DefaultConfig()
	}
	// The keyed production path updates one owned field. The unkeyed public
	// compatibility path still applies all fast-path presentation fields.
	switch key {
	case "tokens":
		merged.UI.ShowTokenUsage = cfg.UI.ShowTokenUsage
	case "compactui":
		merged.UI.CompactMode = cfg.UI.CompactMode
	case "hints":
		merged.UI.HintsEnabled = cfg.UI.HintsEnabled
	case "toolcalls":
		merged.UI.ShowToolCalls = cfg.UI.ShowToolCalls
	case "markdown":
		merged.UI.MarkdownRendering = cfg.UI.MarkdownRendering
	case "bell":
		merged.UI.Bell = cfg.UI.Bell
	case "nativealerts":
		merged.UI.NativeNotifications = cfg.UI.NativeNotifications
	case "reducedmotion":
		merged.UI.ReducedMotion = cfg.UI.ReducedMotion
	default:
		merged.UI.ShowTokenUsage = cfg.UI.ShowTokenUsage
		merged.UI.CompactMode = cfg.UI.CompactMode
		merged.UI.ReducedMotion = cfg.UI.ReducedMotion
		merged.UI.HintsEnabled = cfg.UI.HintsEnabled
		merged.UI.ShowToolCalls = cfg.UI.ShowToolCalls
		merged.UI.MarkdownRendering = cfg.UI.MarkdownRendering
		merged.UI.Bell = cfg.UI.Bell
		merged.UI.NativeNotifications = cfg.UI.NativeNotifications
	}
	saveErr := merged.Save()
	if saveErr != nil {
		logging.Warn("failed to save UI config", "error", saveErr, "path", config.GetConfigPath())
	}
	a.config = merged
	msg := ui.ConfigUpdateMsg{
		Revision:            a.nextConfigRevisionLocked(),
		Settings:            a.settingToggleSnapshotLocked(),
		PermissionsEnabled:  merged.Permission.Enabled,
		SandboxEnabled:      merged.Tools.Bash.Sandbox,
		PlanningModeEnabled: a.planningModeEnabled,
		CompactMode:         merged.UI.CompactMode,
		ReducedMotion:       merged.UI.ReducedMotion,
		ShowTokenUsage:      merged.UI.ShowTokenUsage,
		HintsEnabled:        merged.UI.HintsEnabled,
		ShowToolCalls:       merged.UI.ShowToolCalls,
		ModelName:           merged.Model.Name,
	}
	tuiModel := a.tui
	var notificationManager *tools.NotificationManager
	if a.executor != nil {
		notificationManager = a.executor.GetNotificationManager()
	}
	markdownRendering := merged.UI.MarkdownRendering
	bellEnabled := merged.UI.Bell
	nativeNotifications := merged.UI.NativeNotifications
	a.mu.Unlock()

	// Before Run installs a Bubble Tea program (primarily builder/tests), apply
	// the same presentation snapshot directly. Once the program exists, only
	// its event loop mutates the model.
	a.programMu.RLock()
	hasProgram := a.program != nil
	a.programMu.RUnlock()
	if !hasProgram && tuiModel != nil {
		tuiModel.SetCompactMode(msg.CompactMode)
		tuiModel.SetReducedMotion(msg.ReducedMotion)
		tuiModel.SetShowTokens(msg.ShowTokenUsage)
		tuiModel.SetHintsEnabled(msg.HintsEnabled)
		tuiModel.SetShowToolCalls(msg.ShowToolCalls)
		tuiModel.SetMarkdownRendering(markdownRendering)
		tuiModel.SetBellEnabled(bellEnabled)
	}
	if notificationManager != nil {
		notificationManager.EnableNativeNotifications(nativeNotifications)
	}
	a.safeSendToProgram(msg)

	if saveErr != nil {
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: fmt.Sprintf("Setting applied for this session but NOT saved to %s — it will revert next launch", config.GetConfigPath()),
		})
	}
	return nil
}

// ApplyConfig saves the given configuration and re-initializes affected components.
//
// Defensive locking discipline: we release `a.mu` BEFORE the trailing
// safeSendToProgram call. The primary deadlock has been closed by routing
// safeSendToProgram through a dedicated `programMu` (so it no longer
// re-enters `a.mu`), but keeping the send outside the critical section
// remains the safer pattern — future code added to safeSendToProgram that
// takes a.mu again would otherwise silently reintroduce the hang. A test
// (TestApplyConfig_NoSelfDeadlock) pins the no-deadlock behavior.
func (a *App) ApplyConfig(cfg *config.Config) error {
	_, err := a.applyConfig(cfg)
	return err
}

// applyConfig is the revision-returning transactional implementation used by
// request-correlated app flows such as the model selector.
func (a *App) applyConfig(cfg *config.Config) (uint64, error) {
	if cfg == nil {
		return 0, fmt.Errorf("cannot apply nil config")
	}
	originalCandidate := cfg
	cfg = cfg.Clone()
	a.mu.Lock()
	// NOTE: no defer Unlock() — we release before safeSendToProgram. Every
	// early-return below unlocks explicitly.
	if baseRevision, tracked := cfg.SnapshotRevision(); tracked && baseRevision != a.configRevision {
		a.mu.Unlock()
		return 0, fmt.Errorf("%w (based on revision %d, current revision %d)", errConfigConflict, baseRevision, a.configRevision)
	}
	// Seed/capture this before publishing cfg. The configured value may be
	// persisted for the next launch, but the current process must retain the
	// registry/runtime topology it was built with.
	activeEngineMode := a.runtimeEngineModeLocked()
	requestedEngineMode := normalizeRuntimeEngineMode(cfg.Engine.Mode)
	engineConfigChanged := a.config == nil || a.config.Engine != cfg.Engine
	engineModeRestartRequired := engineConfigChanged && requestedEngineMode != activeEngineMode
	replConfigRestartRequired := engineConfigChanged && a.config != nil && a.config.Engine.REPL != cfg.Engine.REPL
	engineRestartRequired := engineModeRestartRequired || replConfigRestartRequired
	// --bare is an invocation boundary, not a persisted setting. Runtime
	// config changes may update provider/model/UI fields, but cannot escape the
	// physically minimal registry/prompt for this process.
	if a.config != nil && a.config.Bare {
		cfg.Bare = true
	}
	if a.config != nil && a.config.Debug {
		cfg.Debug = true
		cfg.DebugFile = a.config.DebugFile
		cfg.DebugFilter = a.config.DebugFilter
		cfg.DebugLevel = a.config.DebugLevel
	}

	requestedPlanMode := cfg.Plan.Enabled

	// Validate/build the replacement client before committing the candidate to
	// memory or disk. A failed rebuild must leave the previous authoritative
	// config intact; callers such as Settings can then report/rollback cleanly.
	// Force-evict ALL pooled clients for the incoming provider BEFORE
	// asking the factory for a fresh one. The pool keys on (provider,
	// model) only, so a changed API key — the whole reason ApplyConfig
	// was called from /login — would otherwise silently hit the cache
	// and return the OLD client with the OLD key. Invalidate for just
	// one (provider, model) pair is not enough: the new model name may
	// differ from the old one, and fallback chains create multiple
	// entries for the same provider. FlushProvider evicts every entry
	// matching the provider so the next NewClient is guaranteed to
	// build from the candidate.
	client.GetPool(cfg).FlushProvider(runtimeProviderForConfig(cfg))

	newClient, err := client.NewClient(a.ctx, cfg, cfg.Model.Name)
	if err != nil {
		a.mu.Unlock()
		return 0, fmt.Errorf("failed to re-initialize client: %w", err)
	}

	// The candidate is now runtime-valid. Persist and publish it in the same
	// locked commit order as its revision. Persistence failure is non-fatal for
	// this session but is surfaced after unlock.
	saveErr := cfg.Save()
	if saveErr != nil {
		logging.Warn("failed to save config to file", "error", saveErr, "path", config.GetConfigPath())
	}
	cfg.ClearSnapshotRevision()
	permissionPolicyChanged := a.config == nil ||
		a.config.Permission.DefaultPolicy != cfg.Permission.DefaultPolicy ||
		!reflect.DeepEqual(a.config.Permission.Rules, cfg.Permission.Rules)
	a.config = cfg
	memoryAutoInject := a.memoryStore != nil && cfg.Memory.Enabled && cfg.Memory.AutoInject
	memoryAllowGlobal := a.memoryStore != nil && cfg.Memory.Enabled && cfg.Memory.AllowGlobal
	a.memoryAutoInject.Store(memoryAutoInject)
	a.memoryAllowGlobal.Store(memoryAllowGlobal)
	// MemoryTool clones share this live policy pointer, so revocation reaches
	// already-running agents instead of only future registry clones.
	if a.registry != nil {
		if registered, ok := a.registry.Get("memory"); ok {
			if memoryTool, ok := registered.(*tools.MemoryTool); ok {
				memoryTool.SetAllowGlobal(memoryAllowGlobal)
			}
		}
	}
	// A config transition is also a memory-context boundary: never let the
	// previous turn's retrieved notes reappear after disable/re-enable.
	a.setRelevantMemoryContext("")
	a.planningModeEnabled = requestedPlanMode
	if a.planManager != nil {
		a.planManager.SetEnabled(requestedPlanMode)
	}
	if a.session != nil {
		a.session.SetProvider(runtimeProviderForConfig(a.config))
	}
	if a.promptBuilder != nil {
		switchedProvider := runtimeProviderForConfig(a.config)
		a.promptBuilder.SetProvider(switchedProvider)
		a.promptBuilder.SetPrefixCachingEnabled(appcontext.ProviderSupportsPrefixCaching(switchedProvider))
		a.promptBuilder.SetPlanMode(requestedPlanMode)
		a.promptBuilder.SetGlobalMemoryEnabled(memoryAllowGlobal)
		if memoryAutoInject {
			a.promptBuilder.SetMemoryStore(a.memoryStore)
		} else {
			a.promptBuilder.SetMemoryStore(nil)
		}
	}

	attachStatusCallback(newClient, &appStatusCallback{app: a})
	oldClient := a.client
	a.setClientLocked(newClient) // a.mu held; also guards clientMu for background readers
	if oldClient != nil {
		a.safeGo("close-old-client-after-apply-config", func() {
			if err := oldClient.Close(); err != nil {
				logging.Warn("failed to close old client", "error", err)
			}
		})
	}

	// 4. Update executor's client and sync tools
	if a.executor != nil {
		a.executor.SetClient(newClient)
		a.executor.SetToolTimeout(a.config.Tools.Timeout)
		a.executor.SetModelRoundTimeout(a.config.Tools.ModelRoundTimeout)
		// The executor caches the active context window for in-loop pruning.
		// Builder initializes it at boot, but /model and /provider replace the
		// model without rebuilding the executor. Keep the pruning threshold in
		// sync or switching to GLM-5.2 can retain a previous model's smaller
		// window (premature pruning), while switching away can retain 1M and
		// miss pruning before the new model overflows.
		a.executor.SetMaxInputTokens(effectiveMaxInputTokens(a.config))
		if a.registry != nil {
			if registered, ok := a.registry.Get("bash"); ok {
				if bash, ok := registered.(*tools.BashTool); ok {
					bash.SetTimeout(a.config.Tools.Timeout)
				}
			}
			// a.mu is held for the whole critical section (see NOTE at the top),
			// so this MUST use the lock-free planModeToolsLocked, never
			// toolsForCurrentMode() — that calls IsPlanningModeEnabled(), which
			// re-locks a.mu and self-deadlocks on Go's non-reentrant
			// sync.Mutex. Every post-boot full ApplyConfig caller (/login,
			// /provider, /model, runtime /set or /settings toggles, Ctrl+K,
			// /permissions, /sandbox) reaches this line, so the deadlock is
			// unconditional for that path. UI-only toggles use ApplyUIConfig.
			newClient.SetTools(a.planModeToolsLocked(a.planningModeEnabled))
		}
	}

	// 5. Recompute model-dependent behavior. These components outlive the
	// client and must not retain the previous model's capability profile.
	capability := router.InferModelCapability(runtimeProviderForConfig(a.config), a.config.Model.Name)
	threshold := a.config.Tools.SmartValidation.SelfReviewThreshold
	if a.config.Bare {
		threshold = 0
	}
	if threshold > 2 && (capability.SelfReviewBoost || a.config.Model.ForceWeakOptimizations) {
		threshold = 2
	}
	if a.executor != nil {
		a.executor.SetSelfReviewThreshold(threshold)
	}

	// Update agent runner.
	if a.agentRunner != nil {
		a.agentRunner.SetClient(newClient)
		a.agentRunner.SetPlanningModeEnabled(requestedPlanMode)
		a.agentRunner.SetContextConfig(&a.config.Context)
		a.agentRunner.SetModelRoundTimeout(a.config.Tools.ModelRoundTimeout)
		planningTimeout := a.config.Plan.PlanningTimeout
		if planningTimeout <= 0 {
			planningTimeout = a.config.Tools.ModelRoundTimeout
		}
		a.agentRunner.SetPlanningTimeout(planningTimeout)
		a.agentRunner.SetWorkspaceIsolationEnabled(a.config.Plan.WorkspaceIsolation)
		a.agentRunner.SetDoneGateConfig(a.config.DoneGate)
		a.agentRunner.SetThinkingMode(a.config.Model.ThinkingMode)
		a.agentRunner.SetWeakModelMode(capability.Tier < router.CapabilityStrong || a.config.Model.ForceWeakOptimizations)
		if a.config.DiffPreview.Enabled && a.config.Permission.Enabled {
			a.agentRunner.SetWorkspaceReviewHandler(a.reviewWorkspaceChanges)
		} else {
			a.agentRunner.SetWorkspaceReviewHandler(nil)
		}
	}
	if a.metaAgent != nil {
		a.metaAgent.SetModelRoundTimeout(a.config.Tools.ModelRoundTimeout)
	}
	if a.treePlanner != nil {
		planningTimeout := a.config.Plan.PlanningTimeout
		if planningTimeout <= 0 {
			planningTimeout = a.config.Tools.ModelRoundTimeout
		}
		a.treePlanner.SetPlanningTimeout(planningTimeout)
	}

	// 6. Update context manager
	if a.contextManager != nil {
		a.contextManager.SetConfig(&a.config.Context)
		a.contextManager.SetClient(newClient)
		a.contextManager.SetModelRoundTimeout(a.config.Tools.ModelRoundTimeout)
	}

	// 6a. Update the task router's client. Without this, routed requests kept
	// applying the per-request thinking budget / tools to the OLD pre-swap
	// client object — so after /login the new key reached the executor but the
	// router still poked a discarded client, a silent in-process staleness that
	// only cleared on restart.
	if a.taskRouter != nil {
		a.taskRouter.SetClient(newClient)
		a.taskRouter.SetPlanMode(requestedPlanMode)
		a.taskRouter.SetThinkingMode(a.config.Model.ThinkingMode)
		a.taskRouter.SetModelCapability(capability)
		// Replacing the model client must not partially live-switch the engine.
		// Keep router policy aligned with the boot-pinned registry and runtime.
		a.taskRouter.SetEngineMode(activeEngineMode)
	}

	// ApplyConfig can change plan mode via /set or /settings without going
	// through TogglePlanningMode. Keep the rebuilt client/session instruction
	// aligned with the tool schema and every long-lived runtime consumer.
	if a.promptBuilder != nil {
		systemPrompt := a.buildDefaultSystemInstruction()
		a.applySystemInstruction(newClient, systemPrompt, true)
	}

	// 6b. Update session-memory config live. The manager is always instantiated
	// (its Enabled flag lives inside its config, guarded by its own mutex), so a
	// /settings or /set toggle of `sessionmemory` takes effect THIS session — no
	// restart — which is what lets that toggle honestly advertise live=true.
	if a.sessionMemory != nil {
		a.sessionMemory.SetModelRoundTimeout(a.config.Tools.ModelRoundTimeout)
		a.sessionMemory.SetConfig(appcontext.SessionMemoryConfig{
			Enabled:                 a.config.SessionMemory.Enabled,
			MinTokensToInit:         a.config.SessionMemory.MinTokensToInit,
			MinTokensBetweenUpdates: a.config.SessionMemory.MinTokensBetweenUpdates,
			ToolCallsBetweenUpdates: a.config.SessionMemory.ToolCallsBetweenUpdates,
		})
		// Re-point the LLM summarizer at the (possibly new) client. ApplyConfig
		// swaps a.client, but the session-memory summarizer captured the OLD
		// client at boot — without this, after a /provider or /login the every-Nth
		// LLM extraction keeps hitting the old provider/key (silent cross-provider
		// 400, then a degrade to heuristic summaries for the rest of the session).
		// The sibling ContextManager re-points its summarizer in SetClient; this
		// closes the same gap for session memory. (a.mu is held here.)
		if a.client != nil {
			a.sessionMemory.SetSummarizer(appcontext.NewClientSessionSummarizer(a.client))
		}
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
		if permissionPolicyChanged {
			a.permManager.SetRules(permission.NewRulesFromConfig(
				a.config.Permission.DefaultPolicy,
				a.config.Permission.Rules,
			))
		}
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

	// 8b. Recompute unrestricted mode (sandbox off AND permissions off). The
	// keyboard toggles do this, but the /permissions and /sandbox SLASH
	// commands route through ApplyConfig — without this, full-freedom mode
	// would silently not engage (executor/bash keep blocking) until restart.
	a.updateUnrestrictedModeLocked()

	// 8c. Update UI state (model name, etc.)
	if a.tui != nil {
		a.tui.SetCurrentModel(a.config.Model.Name)
		a.tui.SetShowTokens(a.config.UI.ShowTokenUsage)
		a.tui.SetReducedMotion(a.config.UI.ReducedMotion)
		a.tui.SetHintsEnabled(a.config.UI.HintsEnabled)
		a.tui.SetShowToolCalls(a.config.UI.ShowToolCalls)
		a.tui.SetMarkdownRendering(a.config.UI.MarkdownRendering)
		a.tui.SetBellEnabled(a.config.UI.Bell)
		a.tui.SetPermissionsEnabled(a.config.Permission.Enabled)
		a.tui.SetSandboxEnabled(a.config.Tools.Bash.Sandbox)
		a.tui.SetPlanningModeEnabled(a.planningModeEnabled)
	}
	if a.executor != nil {
		if notificationManager := a.executor.GetNotificationManager(); notificationManager != nil {
			notificationManager.EnableNativeNotifications(a.config.UI.NativeNotifications)
		}
	}

	// Snapshot the UI-relevant state while we still hold the lock — we need
	// to release a.mu before calling safeSendToProgram (see function-level
	// comment on the re-entrancy deadlock).
	uiMsg := ui.ConfigUpdateMsg{
		Revision:            a.nextConfigRevisionLocked(),
		Settings:            a.settingToggleSnapshotLocked(),
		PermissionsEnabled:  a.config.Permission.Enabled,
		SandboxEnabled:      a.config.Tools.Bash.Sandbox,
		PlanningModeEnabled: a.planningModeEnabled,
		CompactMode:         a.config.UI.CompactMode,
		ReducedMotion:       a.config.UI.ReducedMotion,
		ShowTokenUsage:      a.config.UI.ShowTokenUsage,
		HintsEnabled:        a.config.UI.HintsEnabled,
		ShowToolCalls:       a.config.UI.ShowToolCalls,
		ModelName:           a.config.Model.Name,
		ModelRoundTimeout:   a.config.Tools.ModelRoundTimeout,
	}

	// 9. Update search cache
	if a.config.Cache.Enabled && a.searchCache == nil {
		a.searchCache = cache.NewSearchCache(a.config.Cache.Capacity, a.config.Cache.TTL)
		// Re-wire to tools (complex, but most tools check on use)
	}

	modelName := a.config.Model.Name
	a.mu.Unlock()
	// Apply the live session/working/relevant-memory snapshot to the replacement
	// client. The next user turn would also refresh it, but doing it here keeps
	// provider switches and background callbacks immediately consistent.
	a.pushTurnContext()

	// 8d. Send ConfigUpdateMsg to Bubbletea program OUTSIDE the locked
	// section — safeSendToProgram re-acquires a.mu internally, so calling
	// it under the lock would self-deadlock on Go's non-reentrant Mutex.
	a.safeSendToProgram(uiMsg)

	// Honest persistence-failure surface: the config was applied for this
	// session but not written to disk. Warn so the user knows the setting will
	// revert next launch (covers /model, /provider, /thinking, /sandbox, the
	// Ctrl+K selector — every ApplyConfig caller — in one place).
	if saveErr != nil {
		message := fmt.Sprintf("Setting applied for this session but NOT saved to %s — it will revert next launch", config.GetConfigPath())
		if engineRestartRequired {
			message = fmt.Sprintf("Other settings were applied for this session, but engine settings remain at their startup values; the configuration was NOT saved to %s", config.GetConfigPath())
		}
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: message,
		})
	} else if engineRestartRequired {
		message := "engine REPL settings saved; this process keeps its startup limits — run /restart to apply them"
		if engineModeRestartRequired {
			message = fmt.Sprintf(
				"engine.mode saved as %s; this process remains in %s mode — run /restart to apply it",
				requestedEngineMode, activeEngineMode,
			)
			if replConfigRestartRequired {
				message = fmt.Sprintf(
					"engine settings saved with mode %s; this process remains in %s mode with its startup REPL limits — run /restart to apply them",
					requestedEngineMode, activeEngineMode,
				)
			}
		}
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: message,
		})
	}

	// Refresh token counts for the new model limits OFF the command goroutine.
	// refreshTokenCount makes a count_tokens HTTP call that is cache-MISSED by a
	// provider switch's ClearConversation; running it synchronously here was the
	// /login (and /provider, /model) hang: it stalled ApplyConfig →
	// LoginCommand.Execute → executeCommandCtx, so the defer's ResponseDoneMsg
	// never fired and the UI showed "Generating" until the call hit the 120s
	// transport timeout. The config apply above is already complete and in
	// effect; the token count is a UI nicety that must not gate it.
	a.safeGo("apply-config-token-refresh", func() { a.refreshTokenCount() })

	logging.Info("configuration applied successfully", "model", modelName)
	originalCandidate.SetSnapshotRevision(uiMsg.Revision)
	return uiMsg.Revision, nil
}

// openSettingsModal sends OpenSettingsMsg with a fresh toggle snapshot plus the
// current model/provider for the header. Shared by the /settings command, the
// Ctrl+S binding, and the palette "Open Settings" action so all three open the
// exact same screen.
func (a *App) openSettingsModal() {
	settingsMsg := ui.OpenSettingsMsg{Items: a.buildSettingItems()}
	if cfg := a.GetConfig(); cfg != nil {
		settingsMsg.Model = cfg.Model.Name
		settingsMsg.Provider = runtimeProviderForConfig(cfg)
	}
	// Ctrl+S and the palette invoke this callback from Bubble Tea's Update.
	// Reserve ordered delivery but return before Program.Send, otherwise the
	// event loop waits for itself on the unbuffered message channel. Slash
	// command ordering is preserved because its following ResponseDone send
	// waits for this reservation.
	a.safeSendToProgramAsync(settingsMsg)
}

// buildSettingItems snapshots the curated settings toggles for the /settings
// modal from the live config. The modal and /set share the same toggle table in
// the commands package, so the two can never drift on what's configurable.
func (a *App) buildSettingItems() []ui.SettingItem {
	cfg := a.GetConfig()
	if cfg == nil {
		return nil
	}
	states := commands.SettableToggleStates(cfg)
	items := make([]ui.SettingItem, 0, len(states))
	for _, s := range states {
		items = append(items, ui.SettingItem{Key: s.Key, Name: s.Name, Desc: s.Desc, Category: s.Category, On: s.On, Live: s.Live})
	}
	return items
}

// handleSettingToggle applies a single toggle flipped in the /settings modal.
// Invoked from the UI (Bubble Tea) goroutine, so persistence/runtime work runs
// on a worker — it must never jank the render loop. UI-only toggles take the
// lightweight ApplyUIConfig path; runtime toggles use full ApplyConfig.
func (a *App) handleSettingToggle(requestID, key string, on bool) {
	a.safeGo("settings-toggle-apply", func() {
		result := a.applySettingToggle(requestID, key, on)
		a.safeSendToProgram(result)
	})
}

// applySettingToggle is the transactional worker behind the optimistic
// Settings UI. A full ApplyConfig persists before rebuilding the client, so a
// rebuild failure would otherwise leave the requested value in memory/on disk
// even though runtime components never received it. Restore the prior boolean
// and persist the rollback before returning an authoritative result.
func (a *App) applySettingToggle(requestID, key string, on bool) ui.SettingToggleResultMsg {
	key = strings.ToLower(strings.TrimSpace(key))
	result := ui.SettingToggleResultMsg{RequestID: requestID, Key: key, On: on}
	cfg := a.GetConfig()
	if cfg == nil {
		result.Message = "Couldn't apply setting: configuration is unavailable"
		return result
	}

	oldOn, found := false, false
	for _, state := range commands.SettableToggleStates(cfg) {
		if state.Key == key {
			oldOn, found = state.On, true
			break
		}
	}
	if !found || !commands.ApplySettingToggle(cfg, key, on) {
		result.Message = "Couldn't apply setting: unknown setting"
		return result
	}

	if err := commands.ApplyConfigForSetting(a, cfg, key); err != nil {
		logging.Warn("failed to apply setting toggle; keeping authoritative value", "key", key, "error", err)
		result.On = oldOn
		if current := a.GetConfig(); current != nil {
			for _, state := range commands.SettableToggleStates(current) {
				if state.Key == key {
					result.On = state.On
					break
				}
			}
		}
		result.Message = fmt.Sprintf("Couldn't apply %s: %v — current value preserved", key, err)
		return result
	}

	result.Success = true
	return result
}

// handleKeyEntrySubmit applies a key entered in the masked /login modal by
// re-invoking the login command with the captured key. The key never reaches
// the model or plaintext scrollback — the command's result masks it. Runs on a
// worker so the UI goroutine that triggered it isn't blocked by ApplyConfig.
func (a *App) handleKeyEntrySubmit(requestID, provider, key string) {
	a.safeGo("login-key-entry-apply", func() {
		// The masked modal used to execute /login outside the foreground gate.
		// A provider switch could therefore clear the session while a model/tool
		// turn was still mutating it. Claim the same exclusive slot as every
		// slash command; if it is busy, keep the secret out of any plaintext FIFO
		// and ask the user to retry after the active turn settles.
		a.mu.Lock()
		if a.shuttingDown || a.processing || a.headlessRunActive {
			a.mu.Unlock()
			a.safeSendToProgram(ui.KeyEntryResultMsg{
				RequestID: requestID,
				Provider:  provider,
				Message:   "Login was not applied because another request is still running; wait or cancel it, then enter the key again",
			})
			return
		}
		a.processing = true
		a.dropSteerLeftovers = false
		foregroundCtx := a.claimForegroundContextLocked()
		if !a.foregroundWorkers.Add() {
			a.processingMu.Lock()
			a.processingCancel()
			a.processingCancel = nil
			a.processingMu.Unlock()
			a.processing = false
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
		defer a.foregroundWorkers.Done()

		var outcome ui.KeyEntryResultMsg
		outcome.RequestID = requestID
		outcome.Provider = provider
		defer func() {
			if panicValue := recover(); panicValue != nil {
				logging.Error("masked login execution panicked",
					"panic", panicValue, "stack", logging.PanicStack())
				outcome.Message = fmt.Sprintf("Login failed: internal error: %v", panicValue)
				outcome.Success = false
				outcome.Warning = false
				outcome.Output = ""
			}
			a.processingMu.Lock()
			a.processingCancel = nil
			a.processingMu.Unlock()
			a.finishForegroundProcessing(func() { a.safeSendToProgram(outcome) })
		}()

		ctx, cancel := context.WithTimeout(foregroundCtx, 60*time.Second)
		defer cancel()
		result, err := a.commandHandler.Execute(ctx, "login", []string{provider, key}, a)
		outcome.Success, outcome.Warning, outcome.Message = keyEntryCommandOutcome(result, err)
		outcome.Output = result
	})
}

func keyEntryCommandOutcome(result string, err error) (success, warning bool, message string) {
	if err != nil {
		return false, false, fmt.Sprintf("Login failed: %v", err)
	}
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return false, false, "Login failed without a response"
	}
	firstLine := trimmed
	if index := strings.IndexByte(firstLine, '\n'); index >= 0 {
		firstLine = firstLine[:index]
	}
	switch {
	case strings.HasPrefix(trimmed, "✓"):
		return true, false, strings.TrimSpace(strings.TrimPrefix(firstLine, "✓"))
	case strings.HasPrefix(trimmed, "⚠"):
		return true, true, strings.TrimSpace(strings.TrimPrefix(firstLine, "⚠"))
	default:
		return false, false, firstLine
	}
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

// toolContextSummary extracts a brief context string from tool args for progress display.
// Examples: "read internal/app/app.go", "bash go build ./...", "grep pattern in *.go"
func toolContextSummary(name string, args map[string]any) string {
	switch name {
	case "read":
		if fp, ok := args["file_path"].(string); ok {
			return filepath.Base(fp)
		}
	case "write", "edit":
		if fp, ok := args["file_path"].(string); ok {
			return filepath.Base(fp)
		}
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			if runes := []rune(cmd); len(runes) > 30 {
				cmd = string(runes[:30]) + "…"
			}
			return cmd
		}
	case "grep":
		if p, ok := args["pattern"].(string); ok {
			if runes := []rune(p); len(runes) > 20 {
				p = string(runes[:20]) + "…"
			}
			return p
		}
	case "glob":
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	}
	return ""
}

// buildModelEnhancement returns model-specific prompt enhancements.
func (a *App) buildModelEnhancement() string {
	if a == nil || a.config == nil {
		return ""
	}
	if a.config.Bare {
		return ""
	}
	modelName := a.config.Model.Name
	profile := client.GetModelProfile(modelName)
	var enhancement string

	if profile.Family == "glm" {
		enhancement += "\n\n**GLM Execution Policy:** For multi-step work, state a short plan and keep the todo list current. Use the 1M context deliberately: batch independent reads/searches, retain verified facts, and do not repeat a read/grep without a new question. After tool results, briefly synthesize what changed and choose the single next action; never stop immediately after a tool call. Before claiming completion, inspect the resulting diff or files and run the narrowest relevant verification. A tool succeeding is evidence, not completion: finish only when the user's requested behavior is implemented and verified."
	}

	if profile.Family == "kimi" {
		enhancement += "\n\n**Kimi Execution Policy:** Plan briefly before tools. Prefer grep -> targeted read over broad repeated exploration. Reuse tool results and do not repeat the same read/grep/glob without a new hypothesis. After 1-3 tool calls, synthesize what is established, what remains unknown, and the next best action."
	}

	if profile.Family == "deepseek" {
		// Same class of issues as Kimi: eagerness to re-read and
		// skipping the verification step. DeepSeek V4's 1M context
		// tempts over-exploration even more than Kimi's 262K, so the
		// nudge is sharper.
		enhancement += "\n\n**DeepSeek Execution Policy:** Match Claude-Code-style execution: for multi-step work, create a live todo list before editing and keep exactly one item in progress. Plan briefly before tools. 1M context is not a license to re-read — remember what you already loaded. Prefer grep -> targeted read, then edit. After code edits, run the narrowest verification (targeted go test / pytest / cargo check) and cite the result before finalising. Before the final answer, inspect git status or git diff when files changed, and do not claim changes you did not verify. After 3 tool calls: consolidate Established / Unknown / Next before continuing."
	}

	if strings.Contains(strings.ToLower(modelName), "flash") {
		enhancement += "\n\n**Flash Model Note:** Keep responses detailed with specific file:line references despite speed optimizations."
	}

	// Ollama models: per-model prompting + tool calling fallback
	if a.config.API.Backend == "ollama" {
		// Add per-model prompt enhancement based on model profile
		enhancement += client.ModelPromptEnhancement(modelName)

		// Add tool calling fallback prompt for models without native tool support
		if !profile.SupportsTools {
			// Use only the filtered tool set (same as selectToolSets in builder)
			decls := a.getActiveToolDeclarations()
			enhancement += client.ToolCallFallbackPrompt(decls)
		}
	}

	return enhancement
}

// refreshSystemInstruction rebuilds and reapplies the dynamic system instruction.
// This keeps active contract/memory/tool-hints in sync with current runtime state.
func (a *App) refreshSystemInstruction() {
	if a.promptBuilder == nil || a.client == nil || a.session == nil {
		return
	}
	systemPrompt := a.buildDefaultSystemInstruction()
	if strings.TrimSpace(systemPrompt) == "" && !a.hasRunSystemPromptCustomization() {
		return
	}
	a.applySystemInstruction(a.client, systemPrompt, true)
}

// getActiveToolDeclarations returns declarations for the tools actually available
// to the current model (matching selectToolSets logic in builder).
func (a *App) getActiveToolDeclarations() []*genai.FunctionDeclaration {
	if a.config.API.Backend == "ollama" {
		sets := []tools.ToolSet{tools.ToolSetOllamaCore}
		if a.runtimeEngineModeSnapshot() == "hybrid" {
			sets = append(sets, tools.ToolSetHybrid, tools.ToolSetHarness)
		}
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
	a.cancelProcessing()
}

// cancelProcessing performs the shared Esc/Ctrl+C cancellation lifecycle and
// reports whether it actually cancelled or discarded user-visible work. The
// return value lets the OS signal handler distinguish a first interrupt from a
// second/idle interrupt without retaining a stale raw context.CancelFunc.
func (a *App) cancelProcessing() bool {
	feedback := make([]tea.Msg, 0, 2)
	// One lease barrier covers the foreground cancel owner, recovery epoch, FIFO
	// drain, and durable claimed->scheduled release. A retry dispatcher takes the
	// same sessionLeaseMu before a.mu, while foreground handoff captures lineage
	// through that lease and then observes dropSteerLeftovers under a.mu. Esc
	// therefore sees and cancels either side of the handoff, never a newly-owned
	// retry that slipped between owner cancellation and the durable boundary.
	a.sessionLeaseMu.Lock()
	a.mu.Lock()
	a.processingMu.Lock()
	foregroundCancelled := a.processingCancel != nil
	if a.processingCancel != nil {
		a.processingCancel()
		a.processingCancel = nil
	}
	a.dropSteerLeftovers = true
	a.processingMu.Unlock()
	a.mu.Unlock()
	drained, remaining, released, releasedKeys, releaseErr := a.cancelPendingAtRecoveryBoundaryLocked()
	a.sessionLeaseMu.Unlock()
	a.finalizePendingRecoveryCancelBoundary(releasedKeys)
	cancelledAny := foregroundCancelled

	// Close the executor's steering window after the authoritative drain. A
	// callback that already extracted leftovers serializes its all-or-nothing
	// enqueue with dropSteerLeftovers under a.mu; callbacks that arrive later see
	// the flag and discard the cancelled turn's messages.
	if a.executor != nil {
		a.executor.CancelUserSteering()
	}

	// Esc must stop what the user SEES happening. When the foreground is idle
	// but a background /loop iteration is streaming its activity into the UI,
	// the foreground cancel above is a no-op — the iteration runs on the loop
	// runner's own context, which nothing user-facing could previously reach
	// (only the 15m timeout or app shutdown killed it; the field report:
	// "я застопил loop, но стриминг не прекратился даже после Esc"). Kill the
	// in-flight iteration ONLY when there was no foreground work to cancel —
	// if both run, the first Esc stops the foreground, a second stops the loop.
	if !foregroundCancelled && a.loopRunner != nil {
		if a.loopRunner.CancelInFlight("") {
			cancelledAny = true
			feedback = append(feedback, ui.StatusUpdateMsg{
				Type:    ui.StatusInfo,
				Message: "Loop iteration cancelled — the loop stays on its schedule (/loop pause to halt it).",
			})
		}
	}

	// Ordinary type-ahead was drained inside the same barrier. A queued durable
	// recovery was claimed before entering the FIFO, so Esc first persisted the
	// proven-unstarted claimed->scheduled transition. On a definite save failure
	// it stays claimed and manual-only; putting it back in the FIFO would let the
	// cancelled turn's finalizer run it immediately after Esc.
	if drained > 0 || released > 0 || releaseErr != nil || remaining > 0 {
		cancelledAny = true
		feedback = append(feedback, ui.QueuedCountMsg(remaining))
		if released > 0 {
			feedback = append(feedback, ui.StatusUpdateMsg{
				Type: ui.StatusInfo, Message: fmt.Sprintf(
					"Returned %d queued safe retry(s) to their durable schedule", released),
			})
		}
		if releaseErr != nil {
			message := "Some queued safe retries could not be released durably and remain claimed; inspect /recovery"
			if errors.Is(releaseErr, errRecoveryCommitUncertain) {
				message = "Queued safe retries were released, but storage could not confirm directory durability"
			}
			feedback = append(feedback, ui.StatusUpdateMsg{Type: ui.StatusWarning, Message: message})
		}
	}

	// A Stop-hook continuation may have been the thing we just cancelled (its
	// queued message drained, or its in-flight turn aborted before reaching
	// runStopHooks which clears the flag). Leaving stopHookActive=true would make
	// the NEXT user turn be treated as the continuation and silently skip its Stop
	// hooks. Reset under a.mu (the flag's guard) — never under pendingMu.
	a.mu.Lock()
	a.stopHookActive = false
	a.mu.Unlock()
	// CancelProcessing is invoked directly from Bubble Tea's Update. Reserve
	// the whole feedback batch in delivery order, then return so the event loop
	// can receive it instead of re-entering its own unbuffered Program.Send.
	a.safeSendToProgramAsync(feedback...)
	return cancelledAny
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
		AgentID:       result.AgentID,
		Type:          string(result.Type),
		Model:         result.Model,
		Provider:      result.Provider,
		EstimatedCost: result.EstimatedCost,
		CostTracked:   result.CostTracked,
		Status:        string(result.Status),
		Output:        result.Output,
		Error:         result.Error,
		Duration:      result.Duration,
		Completed:     result.Completed,
		OutputFile:    result.OutputFile,
		PolicyBlock:   cloneToolPolicyBlock(result.PolicyBlock),
	}, true
}

func cloneToolPolicyBlock(block *tools.PolicyBlock) *tools.PolicyBlock {
	if block == nil {
		return nil
	}
	copy := *block
	return &copy
}

// Cancel and ListAgents implement tools.AgentCanceller/tools.AgentLister.
// Without these, task_stop.go's stopAgent and task_output.go's cancelTask/
// listTasks type-assert `t.runner.(AgentCanceller)`/`.(AgentLister)` against
// this SAME adapter instance (wired once in builder.go, shared by task/
// task_output/task_stop) and ALWAYS fail — task_stop silently returned
// "agent cancellation not supported" (the agent kept running/burning turns
// indefinitely) and task_output's "Agent Tasks:" section was permanently
// empty to the model. *agent.Runner itself fully supports both; the adapter
// just never forwarded them.
func (a *agentRunnerAdapter) Cancel(agentID string) error {
	return a.runner.Cancel(agentID)
}

func (a *agentRunnerAdapter) ListAgents() []string {
	return a.runner.ListAgents()
}

// diffHandlerAdapter is in app_handlers.go

// GetUIDebugState returns a serializable snapshot of the TUI state.
//
// Round 8: this used to call a.tui.DebugState() directly — a.tui is shared
// with the Bubble Tea Update loop (a DIFFERENT goroutine, which owns every
// mutation to Model's fields, incl. backgroundTasks: both map insert/delete
// in handleBackgroundTask AND in-place field writes for progress updates).
// Calling DebugState() from this (command-execution) goroutine while a
// background task started/completed raced those mutations — a fatal,
// unrecoverable "concurrent map read and map write" crash, reachable via
// /debug-dump. Routed through a request/response message instead, so the
// snapshot is computed on the SAME goroutine as the mutations.
func (a *App) GetUIDebugState() (any, error) {
	if a.tui == nil {
		return nil, fmt.Errorf("TUI not initialized")
	}

	a.programMu.RLock()
	program := a.program
	a.programMu.RUnlock()

	if program == nil {
		// No running Bubble Tea loop (e.g. headless, or before Run() starts
		// the program) — nothing else can be concurrently mutating a.tui,
		// so a direct read is safe.
		return a.tui.DebugState(), nil
	}

	respCh := make(chan ui.UIDebugState, 1)
	a.safeSendToProgram(ui.DebugStateRequestMsg{Resp: respCh})

	select {
	case state := <-respCh:
		return state, nil
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("timed out waiting for TUI debug state")
	}
}

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

// sendAgentTreeUpdate snapshots the coordinator task tree and sends it to TUI.
func (a *App) sendAgentTreeUpdate() {
	a.sendAgentTreeUpdateFrom(a.coordinator)
}

// sendAgentTreeUpdateFrom snapshots the given coordinator's tasks into the
// Agent Tree panel. Parametrized (round 9): the coordinate tool builds a
// FRESH coordinator per call (builder.go factory) — those are the only
// coordinators that ever hold tasks, while a.coordinator (the boot instance)
// never receives any. Feeding the tree only from the boot instance left the
// panel 100% dead UI: auto-show, Ctrl+A, and two rounds of polish
// (v0.91.0 linger, v0.100.52 running-node fields) were unreachable.
func (a *App) sendAgentTreeUpdateFrom(coord *agent.Coordinator) {
	if !a.hasProgram() || coord == nil {
		return
	}

	tasks := coord.GetAllTasks()
	if len(tasks) == 0 {
		return
	}

	// Build dependency depth map
	depthMap := make(map[string]int)
	for _, t := range tasks {
		if len(t.Dependencies) == 0 {
			depthMap[t.ID] = 0
		}
	}
	// Simple BFS to compute depths
	changed := true
	for changed {
		changed = false
		for _, t := range tasks {
			if _, ok := depthMap[t.ID]; ok {
				continue
			}
			maxParentDepth := -1
			allResolved := true
			for _, depID := range t.Dependencies {
				d, ok := depthMap[depID]
				if !ok {
					allResolved = false
					break
				}
				if d > maxParentDepth {
					maxParentDepth = d
				}
			}
			if allResolved {
				depthMap[t.ID] = maxParentDepth + 1
				changed = true
			}
		}
	}

	nodes := make([]ui.AgentTreeNode, 0, len(tasks))
	for _, t := range tasks {
		node := ui.AgentTreeNode{
			ID:          t.ID,
			AgentType:   string(t.AgentType),
			Description: t.Prompt,
			Status:      string(t.Status),
			Depth:       depthMap[t.ID],
		}

		if runes := []rune(node.Description); len(runes) > 60 {
			node.Description = string(runes[:57]) + "..."
		}

		if t.Result != nil {
			node.Duration = t.Result.Duration
		}

		// Include live agent state if running: reasoning (thought), the real
		// start time (so elapsed renders correctly — the node's zero StartTime
		// otherwise showed a bogus decades-long duration), and the running
		// tool count (which drives the progress bar). Progress is set to -1
		// (indeterminate) so the bar animates instead of sitting empty.
		if t.Status == "running" && a.agentRunner != nil {
			agentID := coord.GetTaskAgentID(t.ID)
			if agentID != "" {
				node.Thought = a.agentRunner.GetThought(agentID)
				if ag := a.agentRunner.GetActiveAgent(agentID); ag != nil {
					node.StartTime = ag.GetStartTime()
					node.ToolsUsed = ag.ToolsUsedCount()
					node.Progress = -1
				}
			}
		}

		nodes = append(nodes, node)
	}

	a.safeSendToProgram(ui.AgentTreeUpdateMsg{Nodes: nodes})
}
