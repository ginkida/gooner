package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gokin/internal/audit"
	"gokin/internal/cache"
	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/format"
	"gokin/internal/hooks"
	"gokin/internal/logging"
	"gokin/internal/permission"
	"gokin/internal/robustness"
	"gokin/internal/security"
	"gokin/internal/skills"

	"google.golang.org/genai"
)

// Limits for parallel tool execution to prevent resource exhaustion.
const (
	// MaxFunctionCallsPerResponse caps how many function calls are EXECUTED per
	// response (a resource bound). Calls beyond the cap are NOT dropped — they
	// are paired with an explicit "not executed" result so no tool_use block is
	// left orphaned (see executeTools). 20 matches the parallel-read guidance in
	// the system prompt; actual concurrency stays bounded by
	// MaxConcurrentToolExecutions.
	MaxFunctionCallsPerResponse = 20
	// MaxConcurrentToolExecutions limits parallel goroutines for tool execution.
	MaxConcurrentToolExecutions = 5
	// defaultModelRoundTimeout caps a single model round to prevent zombie requests.
	// Keep this generous for reasoning-heavy models; SSE idle timeout still protects
	// against truly stalled streams. Shared with the agent loop via the client
	// package so the two can't drift (was a too-tight 5m that killed long thinking).
	defaultModelRoundTimeout = client.DefaultModelRoundTimeout

	// defaultToolExecTimeout is the fallback per-tool execution timeout used
	// when the executor is constructed with a non-positive timeout (e.g. a
	// config whose tools.timeout was serialized as 0s). Without this guard a
	// zero base produced context.WithTimeout(ctx, 0) — an already-expired
	// context that made EVERY tool call fail instantly with "context deadline
	// exceeded". Matches DefaultConfig().Tools.Timeout.
	defaultToolExecTimeout = 2 * time.Minute
)

var nestedToolCallSeq atomic.Uint64

// ResultCompactor interface for compacting tool results.
type ResultCompactor interface {
	CompactForType(toolName string, result ToolResult) ToolResult
}

// ResponseCompressor compresses structured fields of a genai.FunctionResponse
// that ResultCompactor can't reach. Applied after the per-tool Content-level
// compaction but before the response lands in history. Intentionally an
// interface (not a direct dep on internal/context) to avoid an import cycle.
type ResponseCompressor interface {
	CompressContent(part *genai.Part) *genai.Part
}

// CostCalculator prices one completed provider response. It is injected by
// the app to keep tools independent from internal/context (which imports tools).
type CostCalculator func(provider, model string, inputTokens, outputTokens, cacheReadTokens int) (cost float64, tracked bool)

type resultSummaryProvider interface {
	SummarizeForPrune(toolName, content string) string
}

// FallbackConfig holds configurable fallback text for when AI doesn't respond after tool use.
type FallbackConfig struct {
	ToolResponseText  string // Text shown when tools were used but no response
	EmptyResponseText string // Text shown when no response at all
}

// DefaultFallbackConfig returns the default fallback configuration.
func DefaultFallbackConfig() FallbackConfig {
	return FallbackConfig{
		ToolResponseText:  "I've completed the requested operations. Based on the tools I used, here's what I found:\n\nPlease let me know if you'd like me to analyze this further or if you have other questions.",
		EmptyResponseText: "I'm here to help with software development tasks. What would you like me to do?",
	}
}

// SmartFallbackConfig contains tool-specific fallback messages.
var SmartFallbackConfig = map[string]struct {
	Success string // Message when tool succeeds but model doesn't respond
	Empty   string // Message when tool returns empty results
	Error   string // Message template for errors
}{
	"read": {
		Success: "I've read the file. Here's a summary of what I found:\n\n[The file was read successfully. Key observations should be provided above.]\n\nWould you like me to explain any specific part?",
		Empty:   "The file appears to be empty or could not be read. Would you like me to:\n1. Check if the file exists\n2. Try reading a different file\n3. Search for similar files",
		Error:   "I encountered an issue reading the file: %s\n\nSuggestions:\n- Check if the file path is correct\n- Verify file permissions\n- Try reading a different file",
	},
	"grep": {
		Success: "Here's what I found from searching:\n\n[Search results should be analyzed above.]\n\nWould you like me to read any of these files in detail?",
		Empty:   "No matches found for this pattern. This could mean:\n1. The pattern doesn't exist in this codebase\n2. Try a different search pattern\n3. The content might be in different files\n\nWould you like me to try a different search?",
		Error:   "Search encountered an issue: %s\n\nSuggestions:\n- Check regex syntax\n- Try a simpler pattern\n- Narrow down the search path",
	},
	"glob": {
		Success: "I found these files. Here's an overview:\n\n[File list should be categorized above.]\n\nWhich files would you like me to examine?",
		Empty:   "No files match this pattern. Possible reasons:\n1. Pattern syntax might be incorrect\n2. Files might have different extensions\n3. Directory might not exist\n\nTry patterns like:\n- `**/*.go` for Go files\n- `**/*.ts` for TypeScript\n- `**/config.*` for config files",
		Error:   "File search failed: %s\n\nCheck:\n- Pattern syntax (use ** for recursive)\n- Directory path exists\n- Permissions on directories",
	},
	"bash": {
		Success: "Command executed. Here's what happened:\n\n[Command output should be explained above.]\n\nIs there anything else you'd like me to run?",
		Empty:   "The command completed successfully but produced no output. This is often normal for:\n- File operations (cp, mv, mkdir)\n- Configuration changes\n- Silent success modes\n\nThe operation was likely successful.",
		Error:   "Command failed: %s\n\nSuggestions:\n- Check command syntax\n- Verify paths and permissions\n- Look at the error message for clues",
	},
	"write": {
		Success: "File written successfully. The changes include:\n\n[File contents should be summarized above.]\n\nWould you like me to verify the file or make additional changes?",
		Empty:   "File operation completed.",
		Error:   "Failed to write file: %s\n\nCheck:\n- Directory exists\n- Write permissions\n- Disk space",
	},
	"edit": {
		Success: "File edited. Here's what changed:\n\n[Changes should be detailed above.]\n\nWould you like me to verify the changes or make additional modifications?",
		Empty:   "No changes were needed - the content already matches.",
		Error:   "Edit failed: %s\n\nThis usually means:\n- The search text wasn't found\n- File content has changed\n- Try reading the file first to see current content",
	},
}

// FormatToolPanic returns a user-facing message for a recovered tool panic.
// The "panic:" prefix is preserved so error-classifier matchers in
// context/compactor.go and context/tool_summarizer.go still recognise the
// result as an error during compaction. The hint about the log file gives
// the user a path to a full stack trace without bloating the model context.
//
// Exported so packages outside `tools` (notably agent.go's parallel tool
// execution panic handler) can produce identical user-facing strings.
func FormatToolPanic(toolName string, r any) string {
	return fmt.Sprintf("panic: tool %q crashed: %v (likely a bug; full stack trace in gokin log)", toolName, r)
}

// formatToolPanic is the package-local alias kept for legacy callers.
func formatToolPanic(toolName string, r any) string {
	return FormatToolPanic(toolName, r)
}

// GetSmartFallback returns an appropriate fallback message based on tool and result.
func GetSmartFallback(toolName string, result ToolResult, toolsUsed []string) string {
	// Get tool-specific config
	config, hasConfig := SmartFallbackConfig[toolName]

	// Build context about what tools were used
	var toolContext string
	if len(toolsUsed) > 0 {
		toolContext = fmt.Sprintf("\n\n**Tools used:** %s", strings.Join(toolsUsed, ", "))
	}

	// Handle case where result is empty/default (tools were called but result not tracked properly)
	// This is a graceful fallback - don't show error messages for missing data
	isEmptyResult := result.Content == "" && result.Error == "" && !result.Success

	// Handle error cases - but only if there's an actual error message
	if !result.Success && result.Error != "" {
		if hasConfig {
			return "[Auto] " + fmt.Sprintf(config.Error, result.Error) + toolContext
		}
		return "[Auto] " + fmt.Sprintf("Operation encountered an issue: %s\n\nWould you like me to try a different approach?", result.Error) + toolContext
	}

	// Handle empty/default result - provide helpful generic message
	if isEmptyResult {
		if hasConfig {
			return "[Auto] " + config.Success + toolContext
		}
		return "[Auto] I've completed the requested operations. The results should be shown above.\n\nIs there anything specific you'd like me to explain or analyze further?" + toolContext
	}

	// Handle empty content results (tool ran but returned nothing)
	if result.Content == "" || result.Content == "(empty file)" || result.Content == "(no output)" || result.Content == "No matches found." {
		if hasConfig {
			return "[Auto] " + config.Empty + toolContext
		}
		return "[Auto] The operation completed but returned no results. This may be expected depending on the context." + toolContext
	}

	// Handle success with content
	if hasConfig {
		return "[Auto] " + config.Success + toolContext
	}

	return "[Auto] I've completed the operation. The results are shown above. Would you like me to analyze them further?" + toolContext
}

// ToolRegistry is an interface for tool registries.
// Both Registry and LazyRegistry implement this interface.
type ToolRegistry interface {
	Get(name string) (Tool, bool)
	List() []Tool
	Names() []string
	Declarations() []*genai.FunctionDeclaration
	GeminiTools() []*genai.Tool
	Register(tool Tool) error
}

// Executor handles the function calling loop with enhanced safety and user awareness.
type Executor struct {
	registry  ToolRegistry
	client    client.Client
	workDir   string
	timeout   time.Duration
	timeoutMu sync.RWMutex
	// toolWaitBudgetResolver overrides the outer executor deadline for tools
	// whose dominant work is an app-owned interactive wait. A resolved
	// non-positive budget means no executor deadline; the parent context still
	// cancels immediately on Esc/shutdown.
	toolWaitBudgetResolver func(tool string, args map[string]any) (time.Duration, bool)
	modelRoundTimeout      time.Duration
	handler                *ExecutionHandler
	compactor              ResultCompactor
	permissions            *permission.Manager
	hooks                  *hooks.Manager
	auditLogger            *audit.Logger
	sessionID              string
	sessionIDMu            sync.RWMutex
	fallback               FallbackConfig
	clientMu               sync.RWMutex // Protects client field

	// Enhanced safety features
	safetyValidator  SafetyValidator
	preFlightChecks  bool                     // Enable pre-flight safety checks
	notificationMgr  *NotificationManager     // User notifications
	redactor         *security.SecretRedactor // Redact secrets from output
	unrestrictedMode atomic.Bool              // Full freedom when both sandbox and permissions are off

	// Out-of-workspace access gate (Claude-Code-style ask-on-access). Both nil
	// in non-app harnesses -> gate disabled (the tool's own PathValidator still
	// enforces default-deny). dirAccessChecker reports whether an absolute path
	// is already inside the workspace or a granted dir; dirGrantHandler prompts
	// the user and returns whether access is now allowed (granting propagates to
	// the shared registry, rebuilding the live tool's validator).
	dirAccessChecker func(path string) bool
	dirGrantHandler  func(ctx context.Context, toolName, path string) (bool, error)

	// Discuss-mode action gate (foreground interactive only). discussGate reports
	// whether the CURRENT turn is an analysis/discussion the user has NOT yet
	// confirmed for implementation — when true, the first code-MUTATING tool
	// (IsImplementationTool) pauses for a one-time confirm via actionConfirmHandler
	// instead of silently implementing. Both nil in non-app harnesses
	// (tests/headless) -> gate disabled. Read on the turn goroutine (the executor
	// runs synchronously inside the app turn), so no lock is needed.
	discussGate          func() bool
	actionConfirmHandler func(ctx context.Context, toolName, target string) (bool, error)

	// Token usage from last response (from API usage metadata).
	// Protected by tokenMu for concurrent read/write safety.
	tokenMu                 sync.Mutex
	lastInputTokens         int
	lastOutputTokens        int
	lastCacheCreationTokens int
	lastCacheReadTokens     int
	lastModelTurns          int
	lastEstimatedCost       float64
	lastCostTracked         bool
	lastProvider            string
	lastModel               string
	costCalculator          CostCalculator
	// maxInputTokens is the active model's context window. Used as the in-loop
	// prune trigger; 0 = unset (no pruning). Protected by tokenMu.
	maxInputTokens int

	// Prompt cache break detection
	cacheTracker *client.CacheTracker

	// Circuit breakers for tools
	breakerMu    sync.Mutex
	toolBreakers map[string]*robustness.CircuitBreaker

	// Tool result cache
	toolCache *ToolResultCache

	// Search cache (grep/glob results) for invalidation on writes
	searchCache *cache.SearchCache

	// File read deduplication tracker
	readTracker *FileReadTracker

	// File write tracker — records paths modified by write/edit/refactor so
	// injectContinuationHint can remind the model after compaction.
	writeTracker *FileWriteTracker

	// phaseObserver (optional) receives per-tool timing and outcome. Wired from
	// internal/app so the tools package stays free of metrics dependencies.
	phaseObserver func(tool string, d time.Duration, success bool)

	// planModeCheck (optional) reports whether the agent is in Claude Code-style
	// plan mode. When it returns true, doExecuteTool blocks every call whose
	// tool name is not in the plan-mode allow-list — defense in depth against
	// a model that hallucinates a tool call for a tool the filtered schema
	// didn't expose (e.g., from pre-trained priors). Nil means "always full
	// access". Wired from internal/app.
	planModeCheck func() bool

	// toolStatsLookup (optional) returns observed p95 and success rate for a
	// tool, or ok=false if there aren't enough samples yet (threshold enforced
	// on the caller side — typically 5 samples). Used by the executor to
	// downgrade parallel groups to sequential when a batched tool fails often,
	// and to cap concurrency for slow tools.
	toolStatsLookup func(tool string) (p95 time.Duration, successRate float64, ok bool)

	// Auto-formatter for write/edit operations
	formatter *Formatter

	// Delta-check guardrail for mutating operations.
	deltaCheckEnabled     bool
	deltaCheckWarnOnly    bool
	deltaCheckTimeout     time.Duration
	deltaCheckMaxModules  int
	deltaCheckMu          sync.Mutex
	deltaCheckBlocked     bool
	deltaCheckBlockReason string
	deltaCheckLastHash    string
	deltaCheckLastResult  *deltaCheckResult
	deltaBaselineCaptured bool
	deltaBaselinePaths    map[string]struct{}
	deltaPendingPaths     map[string]struct{}
	// deltaCheckGracedCalls counts how many mutating calls have been allowed
	// through the barrier WHILE blocked, since the last failure. Without this,
	// the very first failed delta-check (e.g. "cannot find package
	// internal/storage" because storage.go hasn't been written yet) would
	// freeze every subsequent write — including the writes that would actually
	// fix the build. The grace window lets the model finish a multi-file
	// workflow before the barrier kicks in.
	deltaCheckGracedCalls int

	// Retry-time side effect deduplication (write/bash tools).
	sideEffectDedupEnabled bool
	sideEffectLedgerMu     sync.Mutex
	sideEffectsByCallID    map[string]ToolResult
	sideEffectsBySignature map[string]ToolResult
	sideEffectSigByCallID  map[string]string
	replayByCallID         map[string]ToolResult
	replayBySignature      map[string]ToolResult
	replaySigByCallID      map[string]string

	// Pending background task completion notifications
	pendingNotifications []string
	pendingNotifMu       sync.Mutex

	// User steering messages: mid-turn follow-ups the user typed while this
	// turn was running. Drained at the top of each loop iteration and appended
	// to history as user turns (mirrors agent.drainSteers) so the model can
	// adjust course mid-task — Claude-Code-style "message during work".
	// userSteerSuppress > 0 closes the acceptance window for INTERNAL Execute
	// calls that reuse this executor (completion review, done-gate auto-fix):
	// a user message typed during those must become the NEXT turn via the
	// pending queue, not get spliced into an internal exchange whose history
	// may be discarded (the review's error path drops newHistory — an accepted
	// steer there would vanish silently).
	userSteers        []string
	userSteerActive   bool
	userSteerSuppress int
	userSteerMu       sync.Mutex
	inlineFinalSteers atomic.Bool
	// userSteerCallbackMu serializes cancellation with extraction of the final
	// leftover batch. It is deliberately released before the external callback:
	// that callback may synchronously send to the UI whose Esc handler is the
	// caller of CancelUserSteering.
	userSteerCallbackMu sync.Mutex

	// Checkpoint journal for tool execution recovery on API failure.
	checkpoint *CheckpointJournal

	// Smart validation: semantic validators for post-write checks.
	semanticValidators *SemanticValidatorRegistry

	// Smart validation: context enrichment for post-write hints.
	contextEnricher *ContextEnricher

	// Smart validation: self-review injection for multi-file mutations.
	selfReviewThreshold  int
	mutatedFilesThisTurn map[string]bool
	mutatedFilesMu       sync.Mutex

	// Smart validation: persistent learning from validator warnings.
	errorLearner ErrorLearner

	// Managed Go diagnostics run after successful Go source mutations. The
	// provider is intentionally attached only to the foreground workspace; tool
	// registries cloned for isolated agents do not inherit its process/index.
	goDiagnosticsMu       sync.RWMutex
	goDiagnosticsProvider GoDiagnosticsProvider
	goDiagnosticsGate     chan struct{}
	goDiagnosticsTimeout  time.Duration
	goDiagnosticsStalled  atomic.Bool

	// Hosted-provider per-turn tool budget. The field/setter retain their legacy
	// Kimi names for config compatibility. Stagnation only catches identical
	// repeats; this cap also catches diverse runaway exploration. 0 disables it.
	kimiToolBudget int

	// Optional deep-compressor for structured function-response fields.
	// ResultCompactor trims Content strings per-tool; this compresses
	// anything else (Data maps, nested arrays) before the response joins
	// history. nil = no-op.
	responseCompressor ResponseCompressor
}

// Keep mid-turn steering bounded. Additional input falls back to the app's
// bounded pending FIFO via TryQueueUserSteer's false return.
const maxQueuedUserSteers = 5

// ExecutionInfo holds information about an active tool execution
type ExecutionInfo struct {
	StartTime      time.Time
	ToolName       string
	Args           map[string]any
	SafetyLevel    SafetyLevel
	Summary        *ExecutionSummary
	PreFlightCheck *PreFlightCheck
	ParentTool     string // For nested tool calls
}

// ExecutionHandler provides callbacks for execution events.
type ExecutionHandler struct {
	// OnText is called when text is streamed from the model.
	OnText func(text string)

	// OnThinking is called when thinking/reasoning content is streamed.
	OnThinking func(text string)

	// OnToolStart is called when a tool begins execution.
	OnToolStart func(name string, args map[string]any)

	// OnToolEnd is called when a tool finishes execution.
	OnToolEnd func(name string, args map[string]any, result ToolResult)

	// OnToolProgress is called periodically during long-running tool execution.
	// This helps keep the UI alive and prevents timeout during long operations.
	OnToolProgress func(name string, elapsed time.Duration)

	// OnToolDetailedProgress is called when a tool reports granular progress.
	// Tools use ProgressCallback to report progress, currentStep, and bytes processed.
	OnToolDetailedProgress func(name string, progress float64, currentStep string)

	// OnToolValidating is called before tool execution to show safety checks.
	OnToolValidating func(name string, check *PreFlightCheck)

	// OnToolApproved is called when user approves a tool execution.
	OnToolApproved func(name string, summary *ExecutionSummary)

	// OnToolDenied is called when user denies a tool execution.
	OnToolDenied func(name string, reason string)

	// OnToolPolicyBlocked is called exactly once when a tool invocation is
	// refused by a permission, safety, hook, or plan boundary. Unlike
	// OnToolDenied it is machine-typed and also covers non-interactive policy
	// gates, allowing process runners to return a fail-closed status without
	// parsing presentation strings.
	OnToolPolicyBlocked func(name string, block PolicyBlock)

	// OnError is called when an error occurs.
	OnError func(err error)

	// OnWarning is called when a warning is issued.
	OnWarning func(warning string)

	// OnToolBudgetExhausted fires ONCE per turn when the hosted-provider
	// per-turn tool budget forces finalization. The app uses it to offer an
	// AUTO-CONTINUATION (a fresh turn = a fresh budget) when the model's own
	// todo list still has unfinished items — instead of making the user type
	// "continue" by hand (v0.100.104 field report).
	OnToolBudgetExhausted func()

	// OnRetrySafetyEvent receives machine-readable exactly-once decisions.
	// Unlike OnWarning, this contract is stable enough for execution journals,
	// reliability evals, and recovery telemetry; consumers must not parse the
	// human-facing warning text to decide whether a tool actually executed.
	OnRetrySafetyEvent func(event RetrySafetyEvent)

	// OnInlineDiff is called after successful edit to show inline diff.
	OnInlineDiff func(filePath, oldText, newText string)

	// OnLoopIteration is called at the start of each executor loop iteration (from 2nd onwards).
	OnLoopIteration func(iteration int, totalToolsUsed int)

	// OnTokenUpdate is called when the streaming response provides token usage
	// from the API. inputTokens is the FULL prompt-side context including
	// cache-read/creation tokens (see client.StreamHandler.OnTokenUpdate) —
	// suitable for context-usage display, not for billing.
	OnTokenUpdate func(inputTokens, outputTokens int)

	// OnFilePeek is called to show a transient high-resolution snippet of a file.
	OnFilePeek func(filePath, title, content, action string)

	// OnMemoryNotify is called when a memory operation completes (save, recall, forget).
	OnMemoryNotify func(action, summary string)

	// OnSteerLeftover is called when user steering messages arrived AFTER the
	// last loop iteration's drain (the model finished, but the user typed
	// something in that final window). The app re-dispatches them as a new
	// turn so the follow-up isn't silently lost. Nil = leftovers are dropped.
	OnSteerLeftover func(messages []string)
}

// RetrySafetyEventKind identifies an exactly-once decision made before a
// potentially stateful tool crosses its execution boundary.
type RetrySafetyEventKind string

const (
	RetrySafetyDuplicateReused  RetrySafetyEventKind = "duplicate_side_effect_reused"
	RetrySafetyCheckpointReplay RetrySafetyEventKind = "checkpoint_replayed"
	RetrySafetyDivergentBlocked RetrySafetyEventKind = "divergent_side_effect_blocked"
)

// RetrySafetyEvent is emitted only for a retry/recovery guard decision. A
// normal tool execution emits no event and continues through OnToolStart.
type RetrySafetyEvent struct {
	Kind      RetrySafetyEventKind `json:"kind"`
	Tool      string               `json:"tool"`
	CallID    string               `json:"call_id,omitempty"`
	Reason    string               `json:"reason,omitempty"`
	Remaining int                  `json:"remaining_checkpoints,omitempty"`
}

func (e *Executor) reportRetrySafetyEvent(event RetrySafetyEvent) {
	if e.handler != nil && e.handler.OnRetrySafetyEvent != nil {
		e.handler.OnRetrySafetyEvent(event)
	}
}

// NewExecutor creates a new tool executor.
func NewExecutor(registry *Registry, c client.Client, timeout time.Duration) *Executor {
	return newExecutorInternal(registry, c, timeout)
}

// NewExecutorLazy creates a new tool executor with a LazyRegistry.
func NewExecutorLazy(registry *LazyRegistry, c client.Client, timeout time.Duration) *Executor {
	return newExecutorInternal(registry, c, timeout)
}

// newExecutorInternal creates the executor with any ToolRegistry implementation.
func newExecutorInternal(registry ToolRegistry, c client.Client, timeout time.Duration) *Executor {
	// A non-positive base timeout would make adaptiveToolTimeout return 0 and
	// every tool run under an already-expired context. Clamp to a sane default
	// so a misconfigured (or zero-serialized) tools.timeout can't brick all
	// tool execution.
	if timeout <= 0 {
		logging.Warn("executor created with non-positive tool timeout; using default",
			"given", timeout, "default", defaultToolExecTimeout)
		timeout = defaultToolExecTimeout
	}
	return &Executor{
		registry:               registry,
		client:                 c,
		timeout:                timeout,
		modelRoundTimeout:      defaultModelRoundTimeout,
		handler:                &ExecutionHandler{},
		fallback:               DefaultFallbackConfig(),
		safetyValidator:        NewDefaultSafetyValidator(),
		preFlightChecks:        true,
		notificationMgr:        NewNotificationManager(),
		toolBreakers:           make(map[string]*robustness.CircuitBreaker),
		redactor:               security.NewSecretRedactor(),
		deltaCheckEnabled:      true,
		deltaCheckWarnOnly:     false,
		deltaCheckTimeout:      90 * time.Second,
		deltaCheckMaxModules:   8,
		deltaBaselinePaths:     make(map[string]struct{}),
		deltaPendingPaths:      make(map[string]struct{}),
		sideEffectsByCallID:    make(map[string]ToolResult),
		sideEffectsBySignature: make(map[string]ToolResult),
		sideEffectSigByCallID:  make(map[string]string),
		checkpoint:             NewCheckpointJournal(),
		cacheTracker:           client.NewCacheTracker(),
	}
}

// AddPendingNotification queues a background task completion notification.
func (e *Executor) AddPendingNotification(msg string) {
	e.pendingNotifMu.Lock()
	defer e.pendingNotifMu.Unlock()
	e.pendingNotifications = append(e.pendingNotifications, msg)
}

// drainPendingNotifications returns and clears all queued notifications.
func (e *Executor) drainPendingNotifications() []string {
	e.pendingNotifMu.Lock()
	defer e.pendingNotifMu.Unlock()
	if len(e.pendingNotifications) == 0 {
		return nil
	}
	n := e.pendingNotifications
	e.pendingNotifications = nil
	return n
}

// TryQueueUserSteer queues a message only while executeLoop is accepting
// steering. The active check and append share one lock with loop shutdown:
// either the loop owns the message, or the caller gets false and can enqueue it
// as a fresh top-level request. There is no "accepted after final drain" gap.
func (e *Executor) TryQueueUserSteer(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	e.userSteerMu.Lock()
	defer e.userSteerMu.Unlock()
	if !e.userSteerActive || e.userSteerSuppress > 0 {
		return false
	}
	if len(e.userSteers) >= maxQueuedUserSteers {
		return false
	}
	e.userSteers = append(e.userSteers, msg)
	return true
}

// SuspendUserSteering closes the steer-acceptance window for the duration of
// an INTERNAL Execute on this executor (completion review, done-gate auto-fix)
// and returns the function that reopens it. While suspended, a live user
// message falls through to the app's pending queue and becomes the next turn
// instead of being injected into the internal exchange. Counter-based so
// nested/overlapping internal calls compose.
func (e *Executor) SuspendUserSteering() (resume func()) {
	e.userSteerMu.Lock()
	e.userSteerSuppress++
	e.userSteerMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.userSteerMu.Lock()
			e.userSteerSuppress--
			e.userSteerMu.Unlock()
		})
	}
}

// drainUserSteers returns and clears all queued user steering messages.
// Called only from the executor's own loop goroutine.
func (e *Executor) drainUserSteers() []string {
	e.userSteerMu.Lock()
	defer e.userSteerMu.Unlock()
	if len(e.userSteers) == 0 {
		return nil
	}
	s := e.userSteers
	e.userSteers = nil
	return s
}

// HasUserSteers reports whether any steering messages are queued.
func (e *Executor) HasUserSteers() bool {
	e.userSteerMu.Lock()
	defer e.userSteerMu.Unlock()
	return len(e.userSteers) > 0
}

// SetInlineFinalUserSteers controls whether an accepted final-window steer
// receives another model iteration inside this Execute call. Interactive mode
// keeps the historical behavior of redispatching it as the next App turn;
// detached headless workers enable this so they can acknowledge delivery
// before their one synchronous turn returns.
func (e *Executor) SetInlineFinalUserSteers(enabled bool) (restore func()) {
	previous := e.inlineFinalSteers.Swap(enabled)
	return func() { e.inlineFinalSteers.Store(previous) }
}

// takeFinalUserSteersOrClose closes the acceptance window atomically with the
// final empty check. Messages accepted before this boundary are returned for
// one more model iteration; messages arriving afterward get false from
// TryQueueUserSteer and remain owned by the caller as a next-turn request.
// This removes the accepted-after-final-drain gap for headless/live attach.
func (e *Executor) takeFinalUserSteersOrClose() []string {
	e.userSteerCallbackMu.Lock()
	defer e.userSteerCallbackMu.Unlock()
	e.userSteerMu.Lock()
	defer e.userSteerMu.Unlock()
	if len(e.userSteers) == 0 {
		e.userSteerActive = false
		return nil
	}
	steers := e.userSteers
	e.userSteers = nil
	return steers
}

// CancelUserSteering closes the current turn's acceptance window and discards
// messages not yet injected into model history. It serializes with extraction
// of the final leftover batch but never waits for an external callback, which
// may itself be blocked on the UI thread performing this cancellation.
func (e *Executor) CancelUserSteering() {
	e.userSteerCallbackMu.Lock()
	e.userSteerMu.Lock()
	e.userSteerActive = false
	e.userSteers = nil
	e.userSteerMu.Unlock()
	e.userSteerCallbackMu.Unlock()
}

// SetClient updates the underlying client.
// SetPlanModeCheck wires the plan-mode predicate. The callback is consulted
// on every tool dispatch and must be cheap — typically reads a boolean
// guarded by App.mu. Pass nil to disable plan-mode gating (main agent in
// normal mode, all sub-agents, tests).
func (e *Executor) SetPlanModeCheck(check func() bool) {
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	e.planModeCheck = check
}

func (e *Executor) SetClient(c client.Client) {
	e.clientMu.Lock()
	e.client = c
	e.clientMu.Unlock()
}

// SetWorkDir sets the working directory used by guardrails that need repository context.
func (e *Executor) SetWorkDir(workDir string) {
	e.workDir = strings.TrimSpace(workDir)
}

// SetDeltaCheckConfig configures automatic post-edit delta-check behavior.
func (e *Executor) SetDeltaCheckConfig(enabled bool, timeout time.Duration, warnOnly bool, maxModules int) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	if maxModules <= 0 {
		maxModules = 8
	}

	e.deltaCheckMu.Lock()
	defer e.deltaCheckMu.Unlock()
	e.deltaCheckEnabled = enabled
	e.deltaCheckTimeout = timeout
	e.deltaCheckWarnOnly = warnOnly
	e.deltaCheckMaxModules = maxModules
}

// SetSemanticValidators configures post-write semantic validators.
// Registry returns the executor's tool registry (read-only use: e.g. the
// app's end-of-turn incomplete-todo check for budget auto-continuation).
func (e *Executor) Registry() ToolRegistry {
	return e.registry
}

func (e *Executor) SetSemanticValidators(r *SemanticValidatorRegistry) {
	e.semanticValidators = r
}

// SetGoDiagnosticsProvider configures managed post-mutation Go diagnostics.
// Passing nil disables the check. The provider must be scoped to e.workDir;
// isolated agent executors deliberately receive no provider unless they create
// one for their own workspace.
func (e *Executor) SetGoDiagnosticsProvider(provider GoDiagnosticsProvider) {
	e.goDiagnosticsMu.Lock()
	e.goDiagnosticsProvider = provider
	if provider == nil {
		e.goDiagnosticsGate = nil
	} else {
		e.goDiagnosticsGate = make(chan struct{}, 1)
		e.goDiagnosticsGate <- struct{}{}
	}
	e.goDiagnosticsStalled.Store(false)
	e.goDiagnosticsMu.Unlock()
}

// SetContextEnricher configures post-write context enrichment.
func (e *Executor) SetContextEnricher(ce *ContextEnricher) {
	e.contextEnricher = ce
}

// GetContextEnricher returns the context enricher for late-binding (e.g. predictor wiring).
func (e *Executor) GetContextEnricher() *ContextEnricher {
	return e.contextEnricher
}

// ErrorLearner persists error/warning patterns for future prevention.
type ErrorLearner interface {
	LearnError(errorType, pattern, solution string, tags []string) error
}

// SetErrorLearner configures persistent learning from validator warnings.
func (e *Executor) SetErrorLearner(el ErrorLearner) {
	e.errorLearner = el
}

// SetSelfReviewThreshold sets the number of mutated files per turn
// that triggers a self-review injection. 0 disables.
func (e *Executor) SetSelfReviewThreshold(n int) {
	e.selfReviewThreshold = n
	if n > 0 {
		e.mutatedFilesThisTurn = make(map[string]bool)
	}
}

// SetKimiToolBudget sets the per-turn tool call cap applied to hosted models.
// The legacy name is kept for source/config compatibility. n <= 0 disables
// the cap entirely. Values below 10 are clamped up so legitimate multi-step
// tasks retain enough room to finish.
func (e *Executor) SetKimiToolBudget(n int) {
	if n <= 0 {
		e.kimiToolBudget = 0
		return
	}
	if n < 10 {
		n = 10
	}
	e.kimiToolBudget = n
}

// getClient returns the current client under read lock.
func (e *Executor) getClient() client.Client {
	e.clientMu.RLock()
	c := e.client
	e.clientMu.RUnlock()
	return c
}

// SetToolTimeout updates the generic per-call timeout for future executions.
// In-flight calls keep their already-derived context.
func (e *Executor) SetToolTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultToolExecTimeout
	}
	e.timeoutMu.Lock()
	e.timeout = timeout
	e.timeoutMu.Unlock()
}

// InvokeTool routes an internal orchestration request through the same control
// plane as a model-emitted function call. Hybrid REPL callbacks use this narrow
// entrypoint so nested delegation retains capability ceilings, validation,
// permissions, hooks, circuit breakers, audit callbacks, timeouts, and retry
// safety. Tool failures remain structured ToolResults; Go errors are reserved
// for an invalid dispatcher invocation.
func (e *Executor) InvokeTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if e == nil || e.registry == nil {
		return ToolResult{}, fmt.Errorf("tool executor is unavailable")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolResult{}, fmt.Errorf("tool name is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if args == nil {
		args = make(map[string]any)
	}
	ctx = context.WithValue(ctx, internalToolInvocationKey{executor: e}, true)
	call := &genai.FunctionCall{
		ID:   fmt.Sprintf("internal-%d", nestedToolCallSeq.Add(1)),
		Name: name,
		Args: ClonePermissionArgs(args),
	}
	return e.executeTool(ctx, call), nil
}

// SetToolWaitBudgetResolver wires dynamic interactive wait budgets. The
// returned duration is the inner prompt's own budget; the executor adds its
// completion grace. Returning (0, true) means the prompt intentionally waits
// indefinitely while still respecting parent cancellation.
func (e *Executor) SetToolWaitBudgetResolver(
	resolver func(tool string, args map[string]any) (time.Duration, bool),
) {
	e.timeoutMu.Lock()
	e.toolWaitBudgetResolver = resolver
	e.timeoutMu.Unlock()
}

func (e *Executor) toolTimeout() time.Duration {
	e.timeoutMu.RLock()
	timeout := e.timeout
	e.timeoutMu.RUnlock()
	if timeout <= 0 {
		return defaultToolExecTimeout
	}
	return timeout
}

func (e *Executor) resolveToolExecutionTimeout(
	base, p95 time.Duration,
	haveStats bool,
	toolName string,
	args map[string]any,
) (time.Duration, bool) {
	e.timeoutMu.RLock()
	resolver := e.toolWaitBudgetResolver
	e.timeoutMu.RUnlock()
	if resolver != nil {
		if waitBudget, ok := resolver(toolName, args); ok {
			if waitBudget <= 0 {
				return 0, false
			}
			return waitBudget + toolTimeoutCompletionGrace, true
		}
	}
	timeout := toolExecutionTimeout(base, p95, haveStats, toolName, args)
	if toolName == "task" && taskNeedsModelRoundHeadroom(args) {
		minimum := e.ModelRoundTimeout() + config.DefaultAgentTimeoutHeadroom + toolTimeoutCompletionGrace
		if timeout < minimum {
			timeout = minimum
		}
	}
	if toolName == "coordinate" {
		if _, explicit := GetInt(args, "timeout_minutes"); !explicit {
			minimum := coordinateImplicitTimeout(args, e.ModelRoundTimeout()) +
				coordinateCleanupTimeout + toolTimeoutCompletionGrace
			if timeout < minimum {
				timeout = minimum
			}
		}
	}
	return timeout, true
}

// SetModelRoundTimeout sets timeout for a single model round.
// Non-positive values reset to the default timeout.
func (e *Executor) SetModelRoundTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultModelRoundTimeout
	}
	e.timeoutMu.Lock()
	e.modelRoundTimeout = timeout
	e.timeoutMu.Unlock()
}

// ModelRoundTimeout returns the effective hard cap used for new model rounds.
func (e *Executor) ModelRoundTimeout() time.Duration {
	e.timeoutMu.RLock()
	timeout := e.modelRoundTimeout
	e.timeoutMu.RUnlock()
	if timeout <= 0 {
		return defaultModelRoundTimeout
	}
	return timeout
}

// SetSideEffectDedup enables/disables deduplication for side-effecting tools.
// Intended for retry windows to avoid duplicate write/bash effects.
func (e *Executor) SetSideEffectDedup(enabled bool) {
	e.sideEffectLedgerMu.Lock()
	defer e.sideEffectLedgerMu.Unlock()
	e.sideEffectDedupEnabled = enabled
	if enabled {
		// The checkpoint journal is the authoritative replay generation. Unlike
		// the legacy result maps below, it preserves duplicate signatures as an
		// ordered multiset and consumes each recorded outcome exactly once.
		if e.checkpoint != nil {
			e.replayByCallID = nil
			e.replayBySignature = nil
			e.replaySigByCallID = nil
			e.checkpoint.BeginReplay()
			return
		}
		// Defensive fallback for embedded executors without a journal.
		e.replayByCallID = cloneToolResultMap(e.sideEffectsByCallID)
		e.replayBySignature = cloneToolResultMap(e.sideEffectsBySignature)
		e.replaySigByCallID = cloneStringMap(e.sideEffectSigByCallID)
		return
	}
	e.clearSideEffectReplayLocked()
}

func cloneToolResultMap(src map[string]ToolResult) map[string]ToolResult {
	dst := make(map[string]ToolResult, len(src))
	for key, result := range src {
		dst[key] = result
	}
	return dst
}

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (e *Executor) clearSideEffectReplayLocked() {
	e.replayByCallID = nil
	e.replayBySignature = nil
	e.replaySigByCallID = nil
	if e.checkpoint != nil {
		e.checkpoint.EndReplay()
	}
}

// GetCheckpointJournal returns the checkpoint journal for inspection or persistence.
func (e *Executor) GetCheckpointJournal() *CheckpointJournal {
	return e.checkpoint
}

// ResetSideEffectLedger clears deduplication ledger for a new top-level request.
func (e *Executor) ResetSideEffectLedger() {
	e.sideEffectLedgerMu.Lock()
	e.sideEffectsByCallID = make(map[string]ToolResult)
	e.sideEffectsBySignature = make(map[string]ToolResult)
	e.sideEffectSigByCallID = make(map[string]string)
	e.replayByCallID = nil
	e.replayBySignature = nil
	e.replaySigByCallID = nil

	// Also clear the checkpoint journal — it belongs to the previous request.
	if e.checkpoint != nil {
		e.checkpoint.Clear()
	}
	e.sideEffectLedgerMu.Unlock()

	// Self-review bookkeeping has the same lifecycle as the side-effect
	// ledger: one user request may call Execute repeatedly while retrying a
	// provider failure. Resetting in executeLoop would forget mutations made by
	// an earlier attempt whose checkpoint is intentionally retained.
	e.mutatedFilesMu.Lock()
	e.mutatedFilesThisTurn = make(map[string]bool)
	e.mutatedFilesMu.Unlock()
}

// PrepareSideEffectRecovery starts a new top-level retry from an exact
// checkpoint generation. It clears unrelated in-memory ledger entries (which
// may belong to an intervening user request), restores the captured journal,
// and opens a one-shot replay window without ever exposing a half-restored
// generation to concurrent tool execution.
func (e *Executor) PrepareSideEffectRecovery(entries []ToolCheckpoint) {
	e.sideEffectLedgerMu.Lock()
	e.sideEffectsByCallID = make(map[string]ToolResult)
	e.sideEffectsBySignature = make(map[string]ToolResult)
	e.sideEffectSigByCallID = make(map[string]string)
	e.replayByCallID = nil
	e.replayBySignature = nil
	e.replaySigByCallID = nil
	e.sideEffectDedupEnabled = true
	if e.checkpoint != nil {
		e.checkpoint.Clear()
		for _, entry := range entries {
			e.checkpoint.RecordSerializedResult(
				entry.CallID,
				entry.ToolName,
				entry.Args,
				entry.Result,
				entry.Signature,
				entry.Timestamp,
			)
		}
		e.checkpoint.BeginReplay()
	}
	e.sideEffectLedgerMu.Unlock()

	e.mutatedFilesMu.Lock()
	e.mutatedFilesThisTurn = make(map[string]bool)
	e.mutatedFilesMu.Unlock()
}

// SetSafetyValidator sets the safety validator.
func (e *Executor) SetSafetyValidator(validator SafetyValidator) {
	e.safetyValidator = validator
}

// EnablePreFlightChecks enables or disables pre-flight safety checks.
func (e *Executor) EnablePreFlightChecks(enabled bool) {
	e.preFlightChecks = enabled
}

// SetUnrestrictedMode enables or disables unrestricted mode.
// When enabled (both sandbox and permissions are off), preflight check errors
// are converted to warnings and don't block execution.
func (e *Executor) SetUnrestrictedMode(enabled bool) {
	e.unrestrictedMode.Store(enabled)
}

// IsUnrestrictedMode returns whether unrestricted mode is enabled.
func (e *Executor) IsUnrestrictedMode() bool {
	return e.unrestrictedMode.Load()
}

// SetFallbackConfig sets the fallback configuration for tool responses.
func (e *Executor) SetFallbackConfig(config FallbackConfig) {
	e.fallback = config
}

// SetHandler sets the execution event handler.
func (e *Executor) SetHandler(handler *ExecutionHandler) {
	if handler != nil {
		e.handler = handler
	}
}

// SetCompactor sets the result compactor for tool output optimization.
func (e *Executor) SetCompactor(compactor ResultCompactor) {
	e.compactor = compactor
}

// SetResponseCompressor installs the optional deep compressor applied to
// each function-response before it joins history. Nil = disabled.
func (e *Executor) SetResponseCompressor(c ResponseCompressor) {
	e.responseCompressor = c
}

// SetPermissions sets the permission manager for tool execution.
func (e *Executor) SetPermissions(mgr *permission.Manager) {
	e.permissions = mgr
}

// SetHooks sets the hooks manager for tool execution.
func (e *Executor) SetHooks(mgr *hooks.Manager) {
	e.hooks = mgr
}

// SetDirAccessChecker wires the out-of-workspace access pre-filter (reports
// whether an absolute path is already inside the workspace or a granted dir).
func (e *Executor) SetDirAccessChecker(fn func(path string) bool) {
	e.dirAccessChecker = fn
}

// SetDirGrantHandler wires the ask-on-access prompt (prompts the user to grant
// access to an out-of-workspace path; returns whether access is now allowed).
func (e *Executor) SetDirGrantHandler(fn func(ctx context.Context, toolName, path string) (bool, error)) {
	e.dirGrantHandler = fn
}

// SetDiscussGate wires the discuss-mode predicate (reports whether the current
// foreground turn is an unconfirmed analysis/discussion — see the field doc).
func (e *Executor) SetDiscussGate(fn func() bool) {
	e.discussGate = fn
}

// SetActionConfirmHandler wires the ask-once confirm shown before the first
// code-mutating tool in a discuss-mode turn (returns whether to implement now).
func (e *Executor) SetActionConfirmHandler(fn func(ctx context.Context, toolName, target string) (bool, error)) {
	e.actionConfirmHandler = fn
}

// SetFormatter sets the auto-formatter for post-write/edit formatting.
func (e *Executor) SetFormatter(f *Formatter) {
	e.formatter = f
}

// SetAuditLogger sets the audit logger for tool execution.
func (e *Executor) SetAuditLogger(logger *audit.Logger) {
	e.auditLogger = logger
}

// SetSessionID sets the session ID for audit logging.
func (e *Executor) SetSessionID(id string) {
	e.sessionIDMu.Lock()
	e.sessionID = id
	e.sessionIDMu.Unlock()
}

// SetToolCache sets the tool result cache for caching read-only tool results.
func (e *Executor) SetToolCache(c *ToolResultCache) {
	e.toolCache = c
}

// SetSearchCache sets the search cache for invalidation on write operations.
func (e *Executor) SetSearchCache(c *cache.SearchCache) {
	e.searchCache = c
}

// SetReadTracker sets the file read deduplication tracker.
func (e *Executor) SetReadTracker(tracker *FileReadTracker) {
	e.readTracker = tracker
}

// SetWriteTracker sets the file write tracker. When set, successful write/edit/
// refactor/copy/move calls record the modified path so the agent can surface
// them in post-compaction continuation hints.
func (e *Executor) SetWriteTracker(tracker *FileWriteTracker) {
	e.writeTracker = tracker
}

// GetReadTracker returns the executor's read tracker, or nil if none is
// configured. Consumers (Agent, commands) read this to surface which files
// the session has already seen — useful for post-compaction hints and
// cache-hit heuristics.
func (e *Executor) GetReadTracker() *FileReadTracker {
	return e.readTracker
}

// ResetSession resets all per-conversation accumulator state so that /clear
// starts fresh. Circuit breakers and tool caches are intentionally preserved
// (connection failures and cache hits persist across /clear).
func (e *Executor) ResetSession() {
	if e.readTracker != nil {
		e.readTracker.Reset()
	}
	if e.writeTracker != nil {
		e.writeTracker.Reset()
	}

	e.deltaCheckMu.Lock()
	e.deltaBaselineCaptured = false
	e.deltaCheckLastResult = nil
	e.deltaCheckBlocked = false
	// Drain block-reason and last-hash too. Both were technically harmless
	// (block-reason only displayed when blocked=true; last-hash only consulted
	// when LastResult != nil), but leaving them set kept stale text alive for
	// debug logs and could surface in future code paths that don't pre-check.
	e.deltaCheckBlockReason = ""
	e.deltaCheckLastHash = ""
	// Pending-paths was a real leak: paths recorded as "modified since last
	// successful delta check" survived /clear, so the first write in a new
	// conversation could fire a delta check based on prior-conversation
	// state. Drain it (and the baseline set) defensively.
	e.deltaPendingPaths = make(map[string]struct{})
	e.deltaBaselinePaths = make(map[string]struct{})
	// Grace counter belongs to the prior conversation's failure state — drain
	// it so a fresh /clear gives the model a clean grace window.
	e.deltaCheckGracedCalls = 0
	e.deltaCheckMu.Unlock()
}

// SetPhaseObserver wires a callback that's invoked after each tool execution
// with the tool name and its wall-clock duration. The app-layer metrics
// collector uses this to build p50/p95 charts per tool without making
// internal/tools depend on internal/app. Nil-safe: no calls when unset.
func (e *Executor) SetPhaseObserver(fn func(tool string, d time.Duration, success bool)) {
	e.phaseObserver = fn
}

// SetToolStatsLookup wires a callback that returns observed p95 / success
// rate per tool. The executor uses this to make adaptive concurrency
// decisions (serialize unreliable tools, cap concurrency for slow ones).
func (e *Executor) SetToolStatsLookup(fn func(tool string) (p95 time.Duration, successRate float64, ok bool)) {
	e.toolStatsLookup = fn
}

// GetNotificationManager returns the notification manager.
func (e *Executor) GetNotificationManager() *NotificationManager {
	return e.notificationMgr
}

// GetLastTokenUsage returns aggregate provider-reported usage for the most
// recent Execute call. A tool loop can make several separately billed model
// rounds, so both input and output are summed across those rounds.
func (e *Executor) GetLastTokenUsage() (int, int) {
	e.tokenMu.Lock()
	in, out := e.lastInputTokens, e.lastOutputTokens
	e.tokenMu.Unlock()
	return in, out
}

// GetLastCacheMetrics returns aggregate cache metrics for the most recent
// Execute call, summed across every model round in its tool loop.
func (e *Executor) GetLastCacheMetrics() (int, int) {
	e.tokenMu.Lock()
	creation, read := e.lastCacheCreationTokens, e.lastCacheReadTokens
	e.tokenMu.Unlock()
	return creation, read
}

// GetLastModelTurns returns the number of separately billed provider rounds in
// the latest Execute call. This makes tool-loop latency visible: a cheap local
// computation can still be inefficient if the model takes several rounds to
// construct and repair it.
func (e *Executor) GetLastModelTurns() int {
	e.tokenMu.Lock()
	turns := e.lastModelTurns
	e.tokenMu.Unlock()
	return turns
}

// SetCostCalculator wires per-response pricing for mixed-provider tool loops.
func (e *Executor) SetCostCalculator(calculator CostCalculator) {
	e.tokenMu.Lock()
	e.costCalculator = calculator
	e.tokenMu.Unlock()
}

// GetLastEstimatedCost returns the authoritative per-round ledger for the
// latest Execute call. tracked=false means pricing was unavailable.
func (e *Executor) GetLastEstimatedCost() (cost float64, tracked bool) {
	e.tokenMu.Lock()
	cost, tracked = e.lastEstimatedCost, e.lastCostTracked
	e.tokenMu.Unlock()
	return cost, tracked
}

// GetLastProviderIdentity returns the backend that served the latest response
// round of the most recent Execute call. Empty values mean no response arrived.
func (e *Executor) GetLastProviderIdentity() (provider, model string) {
	e.tokenMu.Lock()
	provider, model = e.lastProvider, e.lastModel
	e.tokenMu.Unlock()
	return provider, model
}

// GetCacheTracker returns the prompt cache break tracker.
func (e *Executor) GetCacheTracker() *client.CacheTracker {
	return e.cacheTracker
}

// Execute processes a user message through the function calling loop.
func (e *Executor) Execute(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	// Skill tool rules are scoped to this user request. Durable instructions
	// remain in the invocation ledger, but grants and denies must be
	// re-established by an explicit/model skill load every turn.
	BeginSkillPermissionTurn(e.registry)

	// Add user message to history
	userContent := genai.NewContentFromText(message, genai.RoleUser)
	history = append(history, userContent)

	return e.executeLoop(ctx, history)
}

// executeLoop runs the function calling loop until a final text response is received.
func (e *Executor) executeLoop(ctx context.Context, history []*genai.Content) ([]*genai.Content, string, error) {
	// Snapshot client once per executeLoop to avoid racing with SetClient.
	cl := e.getClient()
	e.userSteerMu.Lock()
	e.userSteerActive = true
	e.userSteerMu.Unlock()
	defer e.finishUserSteering()

	// Reset token counters for this execution cycle.
	// OutputTokens accumulates across rounds (each round generates new output).
	// Usage is scoped to this top-level Execute call. Each model round below is
	// a separate provider request and contributes to these aggregate counters.
	e.tokenMu.Lock()
	e.lastInputTokens = 0
	e.lastOutputTokens = 0
	e.lastCacheCreationTokens = 0
	e.lastCacheReadTokens = 0
	e.lastModelTurns = 0
	e.lastEstimatedCost = 0
	e.lastCostTracked = false
	e.lastProvider = ""
	e.lastModel = ""
	e.tokenMu.Unlock()

	// NOTE: checkpoint journal is NOT cleared here — it persists across retries
	// within the same handleSubmit() so that write operations completed before
	// an API failure can be replayed from cache instead of re-executed.
	// The journal is cleared in ResetSideEffectLedger() at the start of each
	// new top-level user request.

	// === IMPROVEMENT 3: Dynamic max iterations based on context complexity ===
	maxIterations := e.calculateMaxIterations(history)
	turnLimitEnabled := true
	// invocationTurnCap is true only when a caller (the --max-turns CLI flag)
	// asked for an EXACT provider-round budget. The adaptive interactive cap
	// below is a different thing: it bounds OUTER iterations, one of which may
	// legitimately contain dozens of chained SendFunctionResponse rounds, and
	// reaching it has always ended the turn with a soft "may be INCOMPLETE"
	// marker rather than a hard failure. Counting inner rounds against it — or
	// turning it into a typed error — would make a normal 20-tool interactive
	// turn die and discard a partial answer the user can still use.
	invocationTurnCap := false
	if invocationLimit, ok := MaxTurnsFromContext(ctx); ok {
		if invocationLimit == 0 {
			turnLimitEnabled = false
		} else {
			maxIterations = invocationLimit
			invocationTurnCap = true
		}
	}
	// A "turn" is a provider model round, including chained responses after
	// tool results. The outer loop alone cannot enforce this: one outer
	// iteration may contain dozens of SendFunctionResponse rounds.
	maxModelTurns := maxIterations
	modelTurns := 0
	defer func() {
		e.tokenMu.Lock()
		e.lastModelTurns = modelTurns
		e.tokenMu.Unlock()
	}()

	var finalText string
	maxBudgetUSD, budgetEnabled := MaxBudgetUSDFromContext(ctx)
	budgetLedger, _ := InvocationBudgetLedgerFromContext(ctx)
	maxTurnsFailure := func() ([]*genai.Content, string, error) {
		marker := "\n\n⚠ Reached the model/tool turn limit — this work is INCOMPLETE. Re-run with a higher --max-turns value or ask me to continue."
		finalText += marker
		if e.handler != nil && e.handler.OnText != nil {
			e.handler.OnText(marker)
		}
		// Keep history protocol-valid when the limit is reached immediately
		// after tool results: the prior model tool_use and user tool_result stay
		// paired, and this local terminal model message closes the turn.
		history = append(history, genai.NewContentFromText(
			strings.TrimSpace(marker), genai.RoleModel))
		return history, finalText, &MaxTurnsExceededError{Limit: maxModelTurns}
	}
	appendUnexecutedResults := func(calls []*genai.FunctionCall, reason string) {
		if len(calls) == 0 {
			return
		}
		parts := make([]*genai.Part, 0, len(calls))
		for _, call := range calls {
			if call == nil {
				continue
			}
			result := genai.NewPartFromFunctionResponse(
				call.Name,
				NewErrorResult(reason).ToMap(),
			)
			result.FunctionResponse.ID = call.ID
			parts = append(parts, result)
		}
		if len(parts) > 0 {
			history = append(history, &genai.Content{
				Role:  genai.RoleUser,
				Parts: parts,
			})
		}
	}
	budgetFailure := func(calls []*genai.FunctionCall, spent float64) ([]*genai.Content, string, error) {
		appendUnexecutedResults(calls,
			"not executed: the invocation cost budget was reached before this tool call could run")
		marker := fmt.Sprintf(
			"\n\n⚠ Reached the invocation cost budget ($%.6f limit; $%.6f recorded) — this work is INCOMPLETE.",
			maxBudgetUSD, spent)
		finalText += marker
		if e.handler != nil && e.handler.OnText != nil {
			e.handler.OnText(marker)
		}
		history = append(history, genai.NewContentFromText(
			strings.TrimSpace(marker), genai.RoleModel))
		return history, finalText, &BudgetExceededError{
			LimitUSD: maxBudgetUSD,
			SpentUSD: spent,
		}
	}
	costUnavailableFailure := func(calls []*genai.FunctionCall, provider, model string) ([]*genai.Content, string, error) {
		appendUnexecutedResults(calls,
			"not executed: pricing is unavailable, so the invocation cost budget cannot be enforced safely")
		failure := &CostUnavailableError{Provider: provider, Model: model}
		if budgetLedger != nil {
			if latched := budgetLedger.MarkCostUnavailable(provider, model); latched != nil {
				var typed *CostUnavailableError
				if errors.As(latched, &typed) {
					failure = typed
				}
			}
		}
		marker := "\n\n⚠ " + failure.Error() + " — no further model or tool work was performed."
		finalText += marker
		if e.handler != nil && e.handler.OnText != nil {
			e.handler.OnText(marker)
		}
		history = append(history, genai.NewContentFromText(
			strings.TrimSpace(marker), genai.RoleModel))
		return history, finalText, failure
	}
	// carriedText accumulates text across max_tokens auto-continuations so the
	// final answer is the WHOLE response, not just the last continued chunk.
	var carriedText string
	truncationContinuations := 0
	// Incomplete-work continuation (incomplete_work.go): when the model stops
	// with no tool calls but its OWN todo list is unfinished, nudge it to keep
	// going instead of ending the turn ("narrates 'continuing…' then stops").
	// Progress-aware: incompleteWorkStuck resets whenever a tool ran since the
	// last nudge, so steady multi-step work is never capped — only narration
	// without action is.
	incompleteWorkStuck := 0
	toolsUsedAtLastIncompleteNudge := -1
	var toolsUsed []string          // Track which tools were used for smart fallback
	var lastToolResult ToolResult   // Track the last tool result for context
	var recentToolPatterns []string // Track tool name patterns per iteration for stagnation detection
	var lastWorkingMemoryNote string
	var lastKimiRecoveryNote string
	// synthesisNudgeInjected ensures the per-turn consolidation reminder
	// fires at most once. Without this guard the prompt would repeat every
	// iteration past the threshold, training Kimi to output boilerplate
	// synthesis blocks instead of actually thinking.
	synthesisNudgeInjected := false
	// intentNudgeInjected gates the "announce your plan before tools"
	// reminder to the first iteration only. Firing it twice in a turn
	// just adds noise — the model saw it already.
	intentNudgeInjected := false
	// todoNudgeInjected gates the live-checklist reminder. It is a soft
	// Claude-Code contract nudge for Kimi/DeepSeek-class models that start
	// mutating across multiple tool calls without using todo.
	todoNudgeInjected := false
	// Once the hosted-provider tool budget is reached, give the model one
	// separately billed round to synthesize a final answer. If it ignores that
	// instruction and emits tools again, pair those calls with synthetic errors
	// locally and stop without paying for an unbounded sequence of reminders.
	toolBudgetFinalizationRounds := 0
	amnesiaWarned := map[string]bool{}
	stagnationRecoveries := map[string]int{}
	coverage := newExecutorCoverageTracker(e.workDir)
	const (
		stagnationLimit           = 5  // Consecutive identical tool patterns before aborting
		stagnationWindowSize      = 15 // Rolling window for amnesia (non-consecutive) repeats
		stagnationWindowRepeatMin = 5  // Repeats within window to trigger a warning (not an abort)
	)
	// max_tokens auto-continuation budget + prompt live in max_tokens.go, shared
	// verbatim with the sub-agent loop (Tier-4 slice 2) so they cannot drift.
	const maxTruncationContinuations = MaxTruncationContinuations
	streamRetries := 0
	partialStreamRetries := 0
	retryPolicy := client.DefaultStreamRetryPolicy()
	// Retries are orchestrated at App/message processor level to avoid retry multiplication.
	retryPolicy.MaxRetries = 0
	retryPolicy.MaxPartialRetries = 0
	accountedResponses := 0
	allResponseCostsTracked := true
	recordResponseAccounting := func(resp *client.Response) (costTracked bool, spent float64) {
		if resp == nil {
			e.tokenMu.Lock()
			spent = e.lastEstimatedCost
			e.tokenMu.Unlock()
			return true, spent
		}
		// Capture token usage metadata from this API round. Provider usage values
		// are cumulative within one response, not across requests; a tool loop's
		// rounds are separately billed and therefore must all be accumulated.
		provider := ""
		if identified, ok := cl.(client.ProviderIdentity); ok {
			provider = identified.GetProvider()
		}
		model := cl.GetModel()
		e.tokenMu.Lock()
		calculator := e.costCalculator
		e.tokenMu.Unlock()
		cost := 0.0
		costTracked = false
		promptInput := resp.TotalInputTokens()
		if calculator != nil {
			cost, costTracked = calculator(provider, model, promptInput, resp.OutputTokens, resp.CacheReadInputTokens)
		}

		e.tokenMu.Lock()
		accountedResponses++
		allResponseCostsTracked = allResponseCostsTracked && costTracked
		e.lastProvider = provider
		e.lastModel = model
		if promptInput > 0 {
			e.lastInputTokens += promptInput
		}
		if resp.OutputTokens > 0 {
			e.lastOutputTokens += resp.OutputTokens
		}
		if resp.CacheCreationInputTokens > 0 {
			e.lastCacheCreationTokens += resp.CacheCreationInputTokens
		}
		if resp.CacheReadInputTokens > 0 {
			e.lastCacheReadTokens += resp.CacheReadInputTokens
		}
		if costTracked {
			e.lastEstimatedCost += max(cost, 0)
		}
		e.lastCostTracked = accountedResponses > 0 && allResponseCostsTracked
		spent = e.lastEstimatedCost
		e.tokenMu.Unlock()
		if budgetEnabled && costTracked {
			_, spent = budgetLedger.AddSpend(max(cost, 0))
		}

		// Track prompt cache usage for cache break detection.
		if e.cacheTracker != nil && (resp.CacheCreationInputTokens > 0 || resp.CacheReadInputTokens > 0) {
			e.cacheTracker.RecordUsage(resp.CacheCreationInputTokens, resp.CacheReadInputTokens)
		}
		return costTracked, spent
	}

	provider := ""
	if identified, ok := cl.(client.ProviderIdentity); ok {
		provider = identified.GetProvider()
	}
	modelName := cl.GetModel()
	if budgetEnabled {
		e.tokenMu.Lock()
		calculator := e.costCalculator
		e.tokenMu.Unlock()
		if calculator == nil {
			return costUnavailableFailure(nil, provider, modelName)
		}
		if _, tracked := calculator(provider, modelName, 0, 0, 0); !tracked {
			return costUnavailableFailure(nil, provider, modelName)
		}
	}

	i := 0
	for ; !turnLimitEnabled || i < maxIterations; i++ {
		if e.readTracker != nil {
			e.readTracker.IncrementTurn()
		}

		// Check context cancellation between iterations
		select {
		case <-ctx.Done():
			return history, finalText, client.ContextErr(ctx)
		default:
		}

		// Drain any user steering messages (mid-turn follow-ups the user typed
		// while this turn was running) into history as user turns. The model
		// sees them on THIS iteration and can adjust course — Claude-Code-style
		// "message during work". Mirrors agent.drainSteers. The UI echo + toast
		// already fired in handleSubmit when the steer was queued, so we only
		// log here — no second warning toast (would duplicate the feedback).
		if steers := e.drainUserSteers(); len(steers) > 0 {
			for _, s := range steers {
				history = append(history, genai.NewContentFromText(
					"[user follow-up] "+s, genai.RoleUser))
				logging.Info("injected user steer into turn", "msg", s)
			}
		}

		// Notify about loop iteration progress (from 2nd iteration onwards)
		if i > 0 && e.handler != nil && e.handler.OnLoopIteration != nil {
			e.handler.OnLoopIteration(i+1, len(toolsUsed))
		}

		// Get response from model
		if budgetEnabled {
			_, spent := budgetLedger.Snapshot()
			if spent >= maxBudgetUSD {
				return budgetFailure(nil, spent)
			}
		}
		if invocationTurnCap && modelTurns >= maxModelTurns {
			return maxTurnsFailure()
		}
		if budgetEnabled {
			if err := budgetLedger.BeginRound(ctx); err != nil {
				if errors.Is(err, ErrBudgetExceeded) {
					_, spent := budgetLedger.Snapshot()
					return budgetFailure(nil, spent)
				}
				var unavailable *CostUnavailableError
				if errors.As(err, &unavailable) {
					return costUnavailableFailure(nil, unavailable.Provider, unavailable.Model)
				}
				return history, finalText, err
			}
		}
		modelTurns++
		resp, err := e.getModelResponse(ctx, cl, history)
		// Usage delivered before a terminal stream error is still billable.
		// Publish it to the shared ledger before releasing the round lease so a
		// waiting child/foreground request observes the updated spend.
		responseCostTracked, responseSpent := recordResponseAccounting(resp)
		if budgetEnabled {
			budgetLedger.EndRound()
		}
		if err != nil {
			// ProcessStream returns the accumulated partial Response alongside an
			// error. Usage metadata received before the failure is still billable.
			ft := client.DetectFailureTelemetry(err)
			logging.Warn("model round failed",
				"reason", ft.Reason,
				"partial", ft.Partial,
				"timeout", ft.Timeout,
				"provider", ft.Provider,
				"retry_count", streamRetries,
				"partial_retry_count", partialStreamRetries,
				"error", err)

			decision := client.DecideStreamRetry(
				retryPolicy,
				err,
				streamRetries,
				partialStreamRetries,
				ctx,
				client.StreamRetryOptions{AllowPartial: false}, // partial continuation is handled at App level
			)
			if decision.ShouldRetry {
				if decision.Partial {
					partialStreamRetries++
				} else {
					streamRetries++
				}
				if e.handler != nil && e.handler.OnWarning != nil {
					e.handler.OnWarning(fmt.Sprintf(
						"Request failed (%s), retrying (%d/%d) in %s...",
						decision.Reason,
						streamRetries,
						retryPolicy.MaxRetries,
						decision.Delay.Round(time.Second),
					))
				}
				if err := waitForRetry(ctx, decision.Delay); err != nil {
					return history, finalText, err
				}
				continue // retry outer loop
			}
			// Preserve partial response in history — include all structured content
			// (text, thinking, function calls) not just text.
			if resp != nil {
				if parts := e.buildResponsePartsRaw(resp); len(parts) > 0 {
					history = append(history, &genai.Content{
						Role:  genai.RoleModel,
						Parts: parts,
					})
				}
			}
			if budgetEnabled && resp != nil {
				if !responseCostTracked {
					return costUnavailableFailure(resp.FunctionCalls, provider, modelName)
				}
				if responseSpent > maxBudgetUSD {
					return budgetFailure(resp.FunctionCalls, responseSpent)
				}
			}
			partialText := carriedText
			if resp != nil {
				partialText += resp.Text
			}
			return history, partialText, fmt.Errorf("model response error (%s): %w", ft.Reason, err)
		}
		streamRetries = 0        // reset on success
		partialStreamRetries = 0 // reset on success

		// Text-based tool call fallback for models without native function calling
		// (shared with the sub-agent loop via client.ApplyTextToolCallFallback).
		if n := client.ApplyTextToolCallFallback(cl, resp); n > 0 {
			logging.Info("fallback: parsed tool calls from text", "count", n)
		}

		// Add model response to history
		modelContent := &genai.Content{
			Role:  genai.RoleModel,
			Parts: e.buildResponseParts(resp),
		}

		history = append(history, modelContent)
		if budgetEnabled {
			if !responseCostTracked {
				return costUnavailableFailure(resp.FunctionCalls, provider, modelName)
			}
			if responseSpent > maxBudgetUSD ||
				(responseSpent >= maxBudgetUSD && len(resp.FunctionCalls) > 0) {
				return budgetFailure(resp.FunctionCalls, responseSpent)
			}
		}

		// Intent announcement nudge: if the very first assistant-turn of
		// this request jumped straight into tool calls with no preceding
		// prose, queue a reminder for the NEXT round asking for a
		// one-line plan. Kimi-family only — Strong-tier models typically
		// self-narrate and don't need this. Once per turn, gated by
		// len(toolsUsed)==0 rather than i==0 so outer-loop retries on
		// transient errors (which bump i without executing tools) don't
		// miss the first real response.
		if !intentNudgeInjected && shouldInjectIntentNudge(len(toolsUsed), cl.GetModel(), resp) {
			intentNudgeInjected = true
			e.AddPendingNotification(intentNudgeMessage)
		}

		// Handle chained function calls in an inner loop
		for len(resp.FunctionCalls) > 0 {
			// Check context cancellation between chained tool calls (Esc pressed)
			select {
			case <-ctx.Done():
				return history, finalText, client.ContextErr(ctx)
			default:
			}

			// Track tools being called, including distinguishing arguments.
			// This prevents false stagnation when the same tool is called
			// multiple times with different targets (e.g., writing 5 different
			// files, running 5 different bash commands, searching 5 patterns).
			iterationTools := make([]string, 0, len(resp.FunctionCalls))
			for _, fc := range resp.FunctionCalls {
				toolsUsed = append(toolsUsed, fc.Name)
				toolKey := fc.Name + ":" + stagnationFingerprint(fc.Name, fc.Args)
				iterationTools = append(iterationTools, toolKey)
			}
			sort.Strings(iterationTools)
			pattern := strings.Join(iterationTools, ",")
			recentToolPatterns = append(recentToolPatterns, pattern)

			var results []*genai.FunctionResponse
			budgetStopAfterPair := false
			stagnationStopAfterPair := false

			// Hosted-provider tool budget: cap total tool calls per turn to
			// prevent runaway exploration. Unlike stagnation (which catches
			// identical repeats), this catches diverse-but-wasteful fan-out.
			// We count already-consumed calls in toolsUsed, not toolsUsed +
			// current batch, so Kimi gets to finish a small final batch if
			// it's right at the edge — the clamp is on the NEXT batch.
			if e.shouldEnforceKimiToolBudget(cl.GetModel(), len(toolsUsed)-len(resp.FunctionCalls)) {
				toolBudgetFinalizationRounds++
				if toolBudgetFinalizationRounds == 1 && e.handler != nil && e.handler.OnToolBudgetExhausted != nil {
					e.handler.OnToolBudgetExhausted()
				}
				logging.Warn("hosted model tool budget exceeded: forcing finalization",
					"budget", e.kimiToolBudget,
					"consumed", len(toolsUsed)-len(resp.FunctionCalls),
					"model", cl.GetModel(),
					"finalization_round", toolBudgetFinalizationRounds)
				if e.handler != nil && e.handler.OnWarning != nil {
					e.handler.OnWarning(fmt.Sprintf(
						"Tool budget (%d) reached — forcing final answer instead of running %d more tools",
						e.kimiToolBudget, len(resp.FunctionCalls)))
				}
				results = e.buildKimiToolBudgetExhaustedResults(resp.FunctionCalls)
				budgetStopAfterPair = toolBudgetFinalizationRounds > 1
			}

			// Stagnation detection: if the same tool pattern repeats too many times, abort
			if results == nil && len(recentToolPatterns) >= stagnationLimit {
				tail := recentToolPatterns[len(recentToolPatterns)-stagnationLimit:]
				allSame := true
				for _, p := range tail[1:] {
					if p != tail[0] {
						allSame = false
						break
					}
				}
				if allSame {
					recoveryAttempt := stagnationRecoveries[pattern]
					if shouldAttemptStagnationRecovery(resp.FunctionCalls, recoveryAttempt) {
						recoveryAttempt++
						stagnationRecoveries[pattern] = recoveryAttempt
						hintBudget, _, _ := stagnationHintBudget(resp.FunctionCalls)
						finalize := recoveryAttempt > hintBudget
						logging.Warn("executor stagnation detected: sending recovery hint instead of aborting",
							"pattern", pattern,
							"count", stagnationLimit,
							"model", cl.GetModel(),
							"recovery_attempt", recoveryAttempt,
							"finalize", finalize)
						if e.handler != nil && e.handler.OnWarning != nil {
							e.handler.OnWarning(buildStagnationWarningMessage(resp.FunctionCalls, stagnationLimit, finalize))
						}
						results = e.buildStagnationRecoveryResults(resp.FunctionCalls, stagnationLimit, finalize)
					} else {
						logging.Warn("executor stagnation detected: same tool pattern repeated",
							"pattern", pattern,
							"count", stagnationLimit,
							"model", cl.GetModel(),
							"recovery_attempts", recoveryAttempt)
						if _, recoverySafe, ok := stagnationHintBudget(resp.FunctionCalls); ok && recoverySafe {
							// Side-effect-free/idempotent loop (the force-finalize
							// class): every repeat was a no-op, so nothing is
							// half-applied — end the turn HONESTLY instead of
							// killing it with an error card (v0.100.100, 5th field
							// report: a model looped on the identical `memory`
							// call through ALL hints + finalize demands and the
							// backstop still discarded the turn as an error).
							// Mirrors the tool-budget stop: pair the calls with
							// synthetic results so no orphaned tool_use poisons
							// history, then break with an honest final text.
							// The turn still ENDS (quota protected) — only the
							// form changes from a dead error to a preserved turn.
							if e.handler != nil && e.handler.OnWarning != nil {
								e.handler.OnWarning(fmt.Sprintf(
									"Loop guard: stopping the turn — %s kept repeating with no progress",
									describeStagnationTarget(resp.FunctionCalls[0].Name, resp.FunctionCalls[0].Args)))
							}
							results = e.buildStagnationStopResults(resp.FunctionCalls)
							stagnationStopAfterPair = true
						} else {
							errMsg := fmt.Sprintf("executor stagnation: tool pattern %q repeated %d times consecutively", pattern, stagnationLimit)
							if recoveryAttempt > 0 {
								errMsg += " after recovery hint"
							}
							return history, "", errors.New(errMsg)
						}
					}
				}
			}

			// Amnesia detection: model may interleave other tools but keep coming back
			// to the same pattern (classic "forgot I tried this" loop). Warn — don't
			// abort — so the user can step in before consecutive stagnation kicks in.
			// Suppress the amnesia warning once the stagnation recovery is
			// already engaged for this pattern — the recovery hint covers it,
			// and two near-identical "you're looping" toasts just add noise.
			if len(recentToolPatterns) >= stagnationWindowSize && !amnesiaWarned[pattern] && stagnationRecoveries[pattern] == 0 {
				windowStart := len(recentToolPatterns) - stagnationWindowSize
				count := 0
				for _, p := range recentToolPatterns[windowStart:] {
					if p == pattern {
						count++
					}
				}
				if count >= stagnationWindowRepeatMin {
					amnesiaWarned[pattern] = true
					logging.Warn("executor amnesia detected: pattern repeated within window",
						"pattern", pattern, "count", count, "window", stagnationWindowSize)
					if e.handler != nil && e.handler.OnWarning != nil {
						e.handler.OnWarning(fmt.Sprintf(
							"Model may be looping: tool pattern repeated %d times in last %d iterations",
							count, stagnationWindowSize))
					}
				}
			}

			// Re-coverage guard (within-turn): the exact-pattern stagnation
			// check above only catches IDENTICAL batches — a loop that
			// perturbs args every call (drifting read offset, rotating grep
			// pattern) makes a fresh fingerprint each round and escapes it.
			// Mirror of the agent-level distinct-target coverage guard
			// (v0.87.0): per-target and progress-gated, so paging, distinct
			// files, and read→edit cycles never trip it. Only observed for
			// batches that will actually execute — a batch already replaced
			// by the kimi budget or stagnation recovery must not leave ghost
			// coverage.
			if results == nil {
				if offender := coverage.observeBatch(resp.FunctionCalls); offender != nil {
					if coverage.trips < maxCoverageRecoveryAttempts {
						coverage.trips++
						logging.Warn("executor re-coverage loop detected: sending recovery hint instead of aborting",
							"tool", offender.name,
							"target", offender.target,
							"redundant", offender.redundant,
							"model", cl.GetModel(),
							"trip", coverage.trips)
						if e.handler != nil && e.handler.OnWarning != nil {
							e.handler.OnWarning(fmt.Sprintf(
								"Loop guard: %s re-covered %s %d times with shifting arguments — sent a recovery hint",
								offender.name, offender.target, offender.redundant))
						}
						results = e.buildCoverageRecoveryResults(resp.FunctionCalls, offender)
						coverage.resetWindow()
					} else {
						logging.Warn("executor re-coverage loop: recovery budget exhausted, aborting",
							"tool", offender.name,
							"target", offender.target,
							"redundant", offender.redundant,
							"model", cl.GetModel(),
							"trips", coverage.trips)
						return history, "", fmt.Errorf(
							"executor re-coverage loop: %s re-covered %q %d times with shifting arguments after %d recovery hints",
							offender.name, offender.target, offender.redundant, coverage.trips)
					}
				}
			}

			if results == nil {
				results, err = e.executeTools(ctx, resp.FunctionCalls)
				if err != nil {
					return history, "", fmt.Errorf("tool execution error: %w", err)
				}
			}

			// Track last result for smart fallback
			if len(results) > 0 {
				lastResult := results[len(results)-1]
				respMap := lastResult.Response // Already map[string]any
				content, _ := respMap["content"].(string)
				success, _ := respMap["success"].(bool)
				errMsg, _ := respMap["error"].(string)
				lastToolResult = ToolResult{Content: content, Success: success, Error: errMsg}
			}

			// Self-review injection: if many files were mutated this turn,
			// add a notification nudging the model to verify consistency.
			if e.selfReviewThreshold > 0 {
				e.mutatedFilesMu.Lock()
				count := len(e.mutatedFilesThisTurn)
				var files []string
				if count >= e.selfReviewThreshold {
					for f := range e.mutatedFilesThisTurn {
						files = append(files, f)
					}
					sort.Strings(files)
					e.mutatedFilesThisTurn = make(map[string]bool)
				}
				e.mutatedFilesMu.Unlock()

				if count >= e.selfReviewThreshold {
					hint := fmt.Sprintf("[smart-validation] You modified %d files this turn: %s. Briefly verify version consistency and correctness across these files.", count, strings.Join(files, ", "))
					e.AddPendingNotification(hint)
				}
			}

			if note := e.buildModelWorkingMemoryNotification(cl.GetModel(), resp.FunctionCalls, results); note != "" && note != lastWorkingMemoryNote {
				lastWorkingMemoryNote = note
				e.AddPendingNotification(note)
			}

			if note := buildKimiToolErrorRecoveryNotification(cl.GetModel(), results); note != "" && note != lastKimiRecoveryNote {
				lastKimiRecoveryNote = note
				e.AddPendingNotification(note)
			}

			if !todoNudgeInjected && shouldInjectTodoNudge(cl.GetModel(), toolsUsed) {
				todoNudgeInjected = true
				e.AddPendingNotification(todoNudgeMessage)
			}

			// Synthesis nudge: when Kimi has issued >=3 tool calls in this
			// turn without any intervening final text, inject a once-per-turn
			// consolidation reminder. It is appended to the current tool result
			// instead of inserted as a separate user message; Anthropic-compatible
			// APIs require tool_result blocks to immediately follow tool_use blocks.
			// Not emitted for non-Kimi models — Strong-tier models self-
			// consolidate, and spamming GLM/Opus with pause reminders
			// regresses their behaviour.
			if !synthesisNudgeInjected && shouldInjectSynthesisNudge(cl.GetModel(), len(toolsUsed)) {
				synthesisNudgeInjected = true
				e.AddPendingNotification(buildSynthesisNudgeMessage(len(toolsUsed)))
			}

			if notifications := e.drainPendingNotifications(); len(notifications) > 0 {
				notifText := "[System: " + strings.Join(notifications, "; ") + "]"
				appendNotificationToFunctionResults(results, notifText)
			}

			// In-loop context guard: when the previous round's input is nearing
			// the model's window, pre-emptively prune older tool outputs in place
			// so this turn doesn't overflow and 400 mid-work (which would force a
			// wasteful full-turn EmergencyTruncate that also destroys the cache).
			// Sub-agents already do this via checkAndSummarize; the foreground
			// loop had no equivalent before v0.98.x.
			// Read both fields under tokenMu (both are tokenMu-protected — a bare
			// gate read would be a data race with SetMaxInputTokens).
			e.tokenMu.Lock()
			lastIn, maxIn := e.lastInputTokens, e.maxInputTokens
			e.tokenMu.Unlock()
			if maxIn > 0 && lastIn > 0 && float64(lastIn) > foregroundPruneTriggerRatio*float64(maxIn) {
				if freed := pruneOldToolOutputs(history, foregroundPruneProtect, foregroundPruneMinOutput); freed > 0 {
					logging.Info("in-loop prune: shrank old tool outputs to fit context",
						"freed_chars", freed, "last_input_tokens", lastIn, "max_input_tokens", maxIn)
					if e.handler != nil && e.handler.OnWarning != nil {
						e.handler.OnWarning("Context filling up — pruned older tool outputs to keep going")
					}
				}
			}

			// Add function results to history BEFORE sending to model.
			// This ensures tool results are preserved even if the API call fails.
			funcResultParts := make([]*genai.Part, len(results))
			for j, result := range results {
				part := genai.NewPartFromFunctionResponse(result.Name, result.Response)
				part.FunctionResponse.ID = result.ID
				// Deep compression for structured response fields that
				// ResultCompactor's string-based Content pass can't reach
				// (nested Data maps, arrays). Safe when not wired: nil
				// compressor leaves the part untouched.
				if e.responseCompressor != nil {
					compressed := e.responseCompressor.CompressContent(part)
					// Defensive: an implementation that returns nil or
					// drops the FunctionResponse would silently break the
					// call/response pairing expected by the client. Fall
					// back to the uncompressed part in those cases.
					if compressed != nil && compressed.FunctionResponse != nil {
						// Re-apply ID — CompressFunctionResponse clones
						// the inner struct but preserves ID from input,
						// so this is usually a no-op. Kept explicit so
						// a future compressor that resets ID can't break
						// the tool-call/result pairing.
						compressed.FunctionResponse.ID = result.ID
						part = compressed
					}
				}
				funcResultParts[j] = part
			}
			funcResultContent := &genai.Content{
				Role:  genai.RoleUser,
				Parts: funcResultParts,
			}

			historyBeforeResults := len(history)
			history = append(history, funcResultContent)

			// The model ignored the first "finalize now" response. The current
			// assistant tool_use entries are now paired in history, so it is safe to
			// end locally. Sending another model request here recreates the runaway
			// quota burn this guard exists to stop.
			if budgetStopAfterPair {
				resp.FunctionCalls = nil
				resp.Text = fmt.Sprintf(
					"Stopped after %d tool calls: the model continued requesting tools after the per-turn budget and finalization reminder. The completed work is preserved; ask me to continue in a new turn if more work is needed.",
					len(toolsUsed))
				break
			}
			if stagnationStopAfterPair {
				target := "a tool call"
				if len(resp.FunctionCalls) > 0 && resp.FunctionCalls[0] != nil {
					target = describeStagnationTarget(resp.FunctionCalls[0].Name, resp.FunctionCalls[0].Args)
				}
				resp.FunctionCalls = nil
				resp.Text = fmt.Sprintf(
					"⚠ Loop guard stopped this turn: the identical %s kept repeating with no progress, despite recovery hints. Everything completed before the loop is preserved above. To continue, re-ask with a narrower request (or tell me which single step to do next).",
					target)
				break
			}

			// Send function responses back to model.
			// Pass history without the just-appended tool results since
			// all client implementations append results themselves.
			for {
				if budgetEnabled {
					_, spent := budgetLedger.Snapshot()
					if spent >= maxBudgetUSD {
						return budgetFailure(nil, spent)
					}
				}
				if invocationTurnCap && modelTurns >= maxModelTurns {
					return maxTurnsFailure()
				}
				if budgetEnabled {
					if err := budgetLedger.BeginRound(ctx); err != nil {
						if errors.Is(err, ErrBudgetExceeded) {
							_, spent := budgetLedger.Snapshot()
							return budgetFailure(nil, spent)
						}
						var unavailable *CostUnavailableError
						if errors.As(err, &unavailable) {
							return costUnavailableFailure(nil, unavailable.Provider, unavailable.Model)
						}
						return history, finalText, err
					}
				}
				modelTurns++
				roundCtx, roundCancel := e.withModelRoundTimeout(ctx)
				stream, err := cl.SendFunctionResponse(roundCtx, history[:historyBeforeResults], results)
				if err != nil {
					roundCancel()
					if budgetEnabled {
						budgetLedger.EndRound()
					}
					return history, "", fmt.Errorf("function response error: %w", err)
				}

				// Collect the response
				resp, err = e.collectStreamWithHandler(roundCtx, stream)
				roundCancel()
				// Account every response exactly once, including partial failed
				// streams, before another invocation may acquire the lease.
				responseCostTracked, responseSpent = recordResponseAccounting(resp)
				if budgetEnabled {
					budgetLedger.EndRound()
				}
				if err == nil {
					// Every successful function-response request is separately
					// billed. Record it before resp is replaced by the next tool
					// round (and before an empty-response retry).
					if budgetEnabled && !responseCostTracked {
						return costUnavailableFailure(nil, provider, modelName)
					}
					if budgetEnabled && responseSpent > maxBudgetUSD &&
						(resp == nil || (resp.Text == "" && len(resp.FunctionCalls) == 0)) {
						return budgetFailure(nil, responseSpent)
					}
					if resp != nil && resp.Text == "" && len(resp.FunctionCalls) == 0 {
						logging.Warn("model returned empty response after tool results",
							"retry_count", streamRetries,
							"max_retries", retryPolicy.MaxRetries,
							"tools", len(results))
						if streamRetries < retryPolicy.MaxRetries {
							delay := client.CalculateBackoff(retryPolicy.BaseDelay, streamRetries, retryPolicy.MaxDelay)
							streamRetries++
							if e.handler != nil && e.handler.OnWarning != nil {
								e.handler.OnWarning(fmt.Sprintf(
									"Model returned an empty response after tool results, retrying (%d/%d) in %s...",
									streamRetries,
									retryPolicy.MaxRetries,
									delay.Round(time.Second),
								))
							}
							if err := waitForRetry(ctx, delay); err != nil {
								return history, "", err
							}
							continue
						}
						return history, "", &client.EmptyModelResponseError{AfterToolResults: true}
					}
					streamRetries = 0
					partialStreamRetries = 0
					break
				}
				// The failed stream may already have delivered cumulative usage.
				// Account it once before deciding whether to retry this request.

				ft := client.DetectFailureTelemetry(err)
				logging.Warn("function response round failed",
					"reason", ft.Reason,
					"partial", ft.Partial,
					"timeout", ft.Timeout,
					"provider", ft.Provider,
					"retry_count", streamRetries,
					"partial_retry_count", partialStreamRetries,
					"error", err)

				decision := client.DecideStreamRetry(
					retryPolicy,
					err,
					streamRetries,
					partialStreamRetries,
					ctx,
					client.StreamRetryOptions{AllowPartial: false}, // partial continuation is handled at App level
				)
				if !decision.ShouldRetry {
					// Preserve partial response from chained tool call — include all
					// structured content (text, thinking, function calls).
					if resp != nil {
						if parts := e.buildResponsePartsRaw(resp); len(parts) > 0 {
							history = append(history, &genai.Content{
								Role:  genai.RoleModel,
								Parts: parts,
							})
						}
					}
					if budgetEnabled && resp != nil {
						if !responseCostTracked {
							return costUnavailableFailure(resp.FunctionCalls, provider, modelName)
						}
						if responseSpent > maxBudgetUSD {
							return budgetFailure(resp.FunctionCalls, responseSpent)
						}
					}
					partialText := carriedText
					if resp != nil {
						partialText += resp.Text
					}
					return history, partialText, fmt.Errorf("function response error (%s): %w", ft.Reason, err)
				}

				if decision.Partial {
					partialStreamRetries++
				} else {
					streamRetries++
				}
				if e.handler != nil && e.handler.OnWarning != nil {
					e.handler.OnWarning(fmt.Sprintf(
						"Request after tool results failed (%s), retrying (%d/%d) in %s...",
						decision.Reason,
						streamRetries,
						retryPolicy.MaxRetries,
						decision.Delay.Round(time.Second),
					))
				}
				if err := waitForRetry(ctx, decision.Delay); err != nil {
					return history, "", err
				}
			}

			// Text-based tool call fallback for chained calls
			if len(resp.FunctionCalls) == 0 && resp.Text != "" {
				if fallbackClient, ok := cl.(interface{ NeedsToolCallFallback() bool }); ok && fallbackClient.NeedsToolCallFallback() {
					if parsed := client.ParseToolCallsFromText(resp.Text); len(parsed) > 0 {
						resp.FunctionCalls = parsed
						resp.Text = ""
						logging.Info("fallback: parsed chained tool calls from text", "count", len(parsed))
					}
				}
			}

			// Add model response to history
			modelContent = &genai.Content{
				Role:  genai.RoleModel,
				Parts: e.buildResponseParts(resp),
			}

			history = append(history, modelContent)
			if budgetEnabled {
				if !responseCostTracked {
					return costUnavailableFailure(resp.FunctionCalls, provider, modelName)
				}
				if responseSpent > maxBudgetUSD ||
					(responseSpent >= maxBudgetUSD && len(resp.FunctionCalls) > 0) {
					return budgetFailure(resp.FunctionCalls, responseSpent)
				}
			}
		}

		// No more function calls, we have the final response
		finalText = resp.Text

		// If the model hit max_tokens while emitting text, ask it to continue
		// instead of surfacing a fake "done" turn. This is common when a model
		// starts a plan/explanation and gets cut before it can call the next tool.
		if resp.FinishReason == genai.FinishReasonMaxTokens {
			carriedText += resp.Text
			finalText = carriedText
			if resp.Text != "" && truncationContinuations < maxTruncationContinuations {
				truncationContinuations++
				logging.Warn("response truncated by max_tokens limit, requesting continuation",
					"output_tokens", resp.OutputTokens,
					"continuation", truncationContinuations,
					"max_continuations", maxTruncationContinuations)
				if e.handler != nil && e.handler.OnWarning != nil {
					e.handler.OnWarning(fmt.Sprintf(
						"Response hit max_tokens; continuing automatically (%d/%d)...",
						truncationContinuations,
						maxTruncationContinuations,
					))
				}
				history = append(history, genai.NewContentFromText(
					TruncationContinuationPrompt,
					genai.RoleUser,
				))
				continue
			}

			finalText += "\n\n⚠ Response truncated (max_tokens limit reached). Use /config to increase max_output_tokens."
			logging.Warn("response truncated by max_tokens limit",
				"output_tokens", resp.OutputTokens,
				"continuations", truncationContinuations)
		} else if carriedText != "" {
			finalText = carriedText + resp.Text
		}

		// Incomplete-work continuation: the model produced a final answer — or an
		// EMPTY response — with no tool calls, but its OWN todo list still has
		// unfinished items. It announced (or skipped) the next step without taking
		// it. Nudge it to act and keep going instead of ending the turn.
		//
		// This is evaluated BEFORE the empty-response break below so an empty 200
		// arriving right after a nudge RE-ASKS (bounded) instead of abandoning
		// unfinished work — the executor-specific gap that the empty-AFTER-tools
		// path already rescued but this top-level path (SendMessageWithHistory) did
		// not. (max_tokens has its own continuation above; skip here so we don't
		// double-continue.) Progress-aware + bounded: a model that narrates without
		// running a tool N times in a row stops; one that completes todos between
		// nudges resets the counter and runs free.
		// actionMode is false ONLY in a foreground discuss-mode turn the user
		// hasn't confirmed (discussGate true): then pending todos are intentional
		// and the nudge is suppressed (don't shove the model into implementation
		// the discuss-gate is holding back). Nil gate (tests/headless/autonomous)
		// -> always action.
		actionMode := e.discussGate == nil || !e.discussGate()
		incDec := DecideIncompleteWorkContinuation(e.registry,
			resp.FinishReason == genai.FinishReasonMaxTokens, len(toolsUsed),
			toolsUsedAtLastIncompleteNudge, incompleteWorkStuck, actionMode)
		incompleteWorkStuck = incDec.Stuck
		toolsUsedAtLastIncompleteNudge = incDec.LastNudge
		if incDec.Continue {
			// Carry the text produced so far so the next iteration's
			// `finalText = carriedText + resp.Text` doesn't silently drop it.
			// finalText already == (carriedText + this resp.Text), so this also
			// folds in any earlier max_tokens-carried prefix. An empty finalText
			// here is harmless.
			carriedText = finalText
			if e.handler != nil && e.handler.OnWarning != nil {
				e.handler.OnWarning(fmt.Sprintf("Continuing — %d task(s) still to do…", incDec.Count))
			}
			logging.Info("incomplete-work continuation: model stopped with unfinished todos",
				"incomplete", incDec.Count, "attempt", incompleteWorkStuck, "empty_response", finalText == "")
			history = append(history, genai.NewContentFromText(
				IncompleteWorkContinuationPrompt(incDec.Count, incDec.Summary), genai.RoleUser))
			continue
		}
		if incDec.Exhausted {
			logging.Info("incomplete-work continuation budget exhausted — model narrating without acting",
				"incomplete", incDec.Count)
		}

		// A follow-up accepted after this iteration's top-of-loop drain still
		// belongs to the active request. Atomically close admission only when
		// none exists; otherwise append every accepted message and give the
		// model another iteration. The prior final response remains in history,
		// while the terminal result becomes the response to the latest steer.
		if e.inlineFinalSteers.Load() {
			steers := e.takeFinalUserSteersOrClose()
			if len(steers) == 0 {
				// Admission was atomically closed; fall through to completion.
			} else {
				for _, steer := range steers {
					history = append(history, genai.NewContentFromText(
						"[user follow-up] "+steer, genai.RoleUser))
					logging.Info("injected final-window user steer into turn", "msg", steer)
				}
				finalText = ""
				carriedText = ""
				continue
			}
		}

		// Detect empty response — model returned no text and no function calls, and
		// the incomplete-work gate above did NOT continue (no unfinished todos, or
		// the continuation budget is exhausted). Break; the fallback below provides
		// a message.
		if finalText == "" {
			logging.Warn("model returned empty response",
				"finish_reason", string(resp.FinishReason),
				"input_tokens", resp.InputTokens,
				"output_tokens", resp.OutputTokens,
				"iteration", i)
			break
		}

		break
	}

	// CRITICAL: Ensure we always have a response
	if finalText == "" {
		// Check if tools were used in THIS request (not all history)
		// toolsUsed is populated during execution, so it only contains current turn's tools
		if len(toolsUsed) > 0 {
			// Use smart fallback based on the last tool used
			lastToolName := toolsUsed[len(toolsUsed)-1]
			finalText = GetSmartFallback(lastToolName, lastToolResult, toolsUsed)
			if e.handler != nil && e.handler.OnText != nil {
				e.handler.OnText(finalText)
			}
		} else {
			// No tools used and no text — model returned empty response
			finalText = fmt.Sprintf("⚠ Model returned an empty response. This may indicate:\n"+
				"- The model doesn't support the current configuration\n"+
				"- ThinkingBudget may need adjustment (try /config)\n"+
				"- Try switching to a different model with /model\n\n"+
				"Model: %s", cl.GetModel())
			if e.handler != nil && e.handler.OnText != nil {
				e.handler.OnText(finalText)
			}
		}
	}

	// If the loop ran to the iteration cap (rather than breaking out because the
	// model finished), the work is almost certainly incomplete — the model was
	// still calling tools when we cut it off. Mark it so partial output isn't
	// silently handed back as a finished answer (and so the done-gate can react).
	if turnLimitEnabled && i >= maxIterations {
		logging.Warn("executor hit iteration cap with model still active",
			"maxIterations", maxIterations, "toolsUsed", len(toolsUsed))
		if invocationTurnCap {
			// An explicit --max-turns budget is a contract: report it as the
			// typed max_turns failure the automation caller can branch on.
			return maxTurnsFailure()
		}
		// The adaptive interactive cap is advisory. Keep the historical soft
		// ending so the partial answer reaches the user instead of an error
		// card, and so the done-gate can still see the work as unfinished.
		marker := "\n\n⚠ Reached the tool-iteration limit for this turn — this work may be INCOMPLETE. Re-run or ask me to continue to finish the remaining steps."
		finalText += marker
	}

	return history, finalText, nil
}

// finishUserSteering atomically closes the acceptance window and takes any
// messages that arrived after the loop's final in-iteration drain. The callback
// runs outside the mutex because the app may immediately enqueue them.
func (e *Executor) finishUserSteering() {
	e.userSteerCallbackMu.Lock()
	e.userSteerMu.Lock()
	e.userSteerActive = false
	leftover := e.userSteers
	e.userSteers = nil
	e.userSteerMu.Unlock()
	handler := e.handler
	e.userSteerCallbackMu.Unlock()

	if len(leftover) > 0 && handler != nil && handler.OnSteerLeftover != nil {
		handler.OnSteerLeftover(leftover)
	}
}

// calculateMaxIterations determines the optimal iteration limit based on context complexity.
// More complex tasks with longer history get more iterations.
func (e *Executor) calculateMaxIterations(history []*genai.Content) int {
	baseLimit := 20

	// Count conversation turns (pairs of user/model messages)
	turnCount := len(history) / 2

	// Estimate complexity based on history length
	switch {
	case turnCount > 30:
		// Very long conversation - allow more iterations
		return baseLimit + 30
	case turnCount > 20:
		// Long conversation - moderately more iterations
		return baseLimit + 20
	case turnCount > 10:
		// Medium conversation - slightly more iterations
		return baseLimit + 10
	default:
		// Short conversation - base limit
		return baseLimit
	}
}

// getModelResponse gets an initial response from the model.
// The cl parameter is a snapshot of the client taken at the start of the execute loop.
func (e *Executor) getModelResponse(ctx context.Context, cl client.Client, history []*genai.Content) (*client.Response, error) {
	if len(history) == 0 {
		return nil, fmt.Errorf("cannot get model response: empty history")
	}

	var message string
	lastContent := history[len(history)-1]
	if lastContent.Role == genai.RoleUser {
		for _, part := range lastContent.Parts {
			if part.Text != "" {
				message = part.Text
				break
			}
		}
	}

	historyWithoutLast := history[:len(history)-1]

	roundCtx, cancel := e.withModelRoundTimeout(ctx)
	defer cancel()

	stream, err := cl.SendMessageWithHistory(roundCtx, historyWithoutLast, message)
	if err != nil {
		return nil, err
	}

	return e.collectStreamWithHandler(roundCtx, stream)
}

func (e *Executor) withModelRoundTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := e.ModelRoundTimeout()
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

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return client.ContextErr(ctx)
	}
}

// collectStreamWithHandler collects streaming response while calling handlers.
func (e *Executor) collectStreamWithHandler(ctx context.Context, stream *client.StreamingResponse) (*client.Response, error) {
	// Note: Don't pass OnError here - the executor handles error display
	// to avoid duplicate error messages
	var onText func(string)
	var onThinking func(string)
	var onTokenUpdate func(int, int)
	if e.handler != nil {
		onText = e.handler.OnText
		onThinking = e.handler.OnThinking
		onTokenUpdate = e.handler.OnTokenUpdate
	}
	return client.ProcessStream(ctx, stream, &client.StreamHandler{
		OnText:        onText,
		OnThinking:    onThinking,
		OnTokenUpdate: onTokenUpdate,
	})
}

const (
	// foregroundPruneTriggerRatio: prune in-loop tool outputs once the previous
	// round's input tokens exceed this fraction of the model's context window.
	// 0.75 leaves headroom before an actual context-overflow 400.
	foregroundPruneTriggerRatio = 0.75
	// foregroundPruneProtect: most-recent history messages never pruned (the
	// model's active working set).
	foregroundPruneProtect = 6
	// foregroundPruneMinOutput: only tool outputs larger than this (runes) are
	// prune candidates — small results aren't worth stubbing.
	foregroundPruneMinOutput = 1000
)

// SetMaxInputTokens records the active model's context window so the executor
// can pre-emptively prune old tool outputs mid-loop before a long turn overflows
// it (which otherwise 400s and forces a wasteful full-turn truncation that also
// destroys the prompt cache). 0 = unset (pruning disabled).
func (e *Executor) SetMaxInputTokens(n int) {
	e.tokenMu.Lock()
	e.maxInputTokens = n
	e.tokenMu.Unlock()
}

// MaxInputTokens returns the context window currently used by the executor's
// in-loop pruning guard. It is primarily useful for runtime diagnostics and
// for verifying that live model switches propagated their limits.
func (e *Executor) MaxInputTokens() int {
	e.tokenMu.Lock()
	defer e.tokenMu.Unlock()
	return e.maxInputTokens
}

// pruneOldToolOutputs shrinks large, older FunctionResponse Content in the
// in-loop history to reclaim context budget without failing the turn. It NEVER
// removes a part — only replaces oversized content with a stub — so every
// tool_use stays paired with its tool_result (Anthropic-compat providers 400 on
// an orphaned tool_use). Protects the first 3 messages (system/greeting/task)
// and the last `protect` messages (recent working set). Returns chars freed.
func pruneOldToolOutputs(history []*genai.Content, protect, minOutputSize int) int {
	start := 3
	end := len(history) - protect
	if end <= start {
		return 0
	}
	freed := 0
	for i := start; i < end; i++ {
		msg := history[i]
		if msg == nil {
			continue
		}
		for _, part := range msg.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Response == nil {
				continue
			}
			content, ok := part.FunctionResponse.Response["content"].(string)
			if !ok || len([]rune(content)) <= minOutputSize {
				continue
			}
			// A successful bounded skill response is the rendered instruction
			// payload, not reconstructible command output. Without consumption state,
			// globally preserving this bounded payload is the safe P0 behavior; full
			// post-compaction carry-forward remains separate work.
			if part.FunctionResponse.Name == "skill" && len(content) <= skills.MaxRenderedSkillBytes {
				if success, _ := part.FunctionResponse.Response["success"].(bool); success {
					continue
				}
			}
			stub := fmt.Sprintf("[older %s output pruned to fit context — re-read if needed]", part.FunctionResponse.Name)
			part.FunctionResponse.Response["content"] = stub
			freed += len([]rune(content)) - len([]rune(stub)) // rune units, matching the candidate threshold
		}
	}
	return freed
}

// executeTools executes a list of function calls with enhanced safety checks.
func (e *Executor) executeTools(ctx context.Context, calls []*genai.FunctionCall) ([]*genai.FunctionResponse, error) {
	// Cap how many calls we EXECUTE per turn (resource bound), but never drop a
	// call silently: the assistant history turn already records every emitted
	// tool_use, so a dropped call leaves an unpaired tool_use block that the
	// Anthropic-compat providers (all four) reject with a 400, failing the whole
	// turn. Defer the overflow and pair each with an explicit "not executed"
	// result below, so the model re-issues them next turn instead of assuming
	// they ran.
	var deferredCalls []*genai.FunctionCall
	if len(calls) > MaxFunctionCallsPerResponse {
		logging.Warn("deferring excess function calls beyond per-turn cap",
			"original_count", len(calls),
			"max_allowed", MaxFunctionCallsPerResponse)
		deferredCalls = calls[MaxFunctionCallsPerResponse:]
		calls = calls[:MaxFunctionCallsPerResponse]
	}

	results := make([]*genai.FunctionResponse, len(calls))

	// Classify tools into dependency groups for optimal parallel execution.
	// Read-only tools (read, grep, glob, etc.) run in parallel within their group.
	// Write tools (edit, write, bash, etc.) run sequentially in their own group.
	// Groups execute sequentially to preserve write-before-read ordering.
	groups := defaultClassifier.classifyDependencies(calls)

	// Adaptive downgrade: if any tool in a parallel group has observed
	// success rate below 50% (with enough samples to trust the number),
	// switch the group to sequential. A batch of flaky tools tends to
	// compound into one giant failure; serial execution lets us stop early
	// on the first error and not multiply tokens on unrelated retries.
	if e.toolStatsLookup != nil {
		for i := range groups {
			if !groups[i].Parallel || len(groups[i].Calls) <= 1 {
				continue
			}
			if shouldSerializeGroup(groups[i].Calls, e.toolStatsLookup) {
				groups[i].Parallel = false
			}
		}
	}

	callToIdx := make(map[*genai.FunctionCall]int) // map call pointer to results index
	for i, call := range calls {
		callToIdx[call] = i
	}

	for _, group := range groups {
		if ctx.Err() != nil {
			for _, call := range group.Calls {
				idx := callToIdx[call]
				if results[idx] == nil {
					results[idx] = &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: NewErrorResult("cancelled").ToMap()}
				}
			}
			continue
		}
		if group.Parallel && len(group.Calls) > 1 {
			// Execute read-only tools in parallel
			var wg sync.WaitGroup
			var mu sync.Mutex
			semaphore := make(chan struct{}, MaxConcurrentToolExecutions)

			for _, call := range group.Calls {
				idx := callToIdx[call]
				wg.Add(1)
				go func(i int, fc *genai.FunctionCall) {
					defer wg.Done()
					select {
					case semaphore <- struct{}{}:
						defer func() { <-semaphore }()
					case <-ctx.Done():
						mu.Lock()
						results[i] = &genai.FunctionResponse{ID: fc.ID, Name: fc.Name, Response: NewErrorResult("cancelled").ToMap()}
						mu.Unlock()
						return
					}
					defer func() {
						if r := recover(); r != nil {
							stack := make([]byte, 4096)
							length := runtime.Stack(stack, false)
							logging.Error("tool execution panic", "tool", fc.Name, "panic", r, "stack", string(stack[:length]))
							mu.Lock()
							results[i] = &genai.FunctionResponse{
								ID: fc.ID, Name: fc.Name,
								Response: NewErrorResult(formatToolPanic(fc.Name, r)).ToMap(),
							}
							mu.Unlock()
						}
					}()
					// Use a goroutine-local context to avoid racing on the
					// shared ctx variable when multiple tools run in parallel.
					toolCtx := ctx
					if e.handler != nil && e.handler.OnFilePeek != nil {
						toolCtx = ContextWithFilePeekCallback(toolCtx, e.handler.OnFilePeek)
					}

					result := e.executeTool(toolCtx, fc)
					mu.Lock()
					results[i] = &genai.FunctionResponse{ID: fc.ID, Name: fc.Name, Response: result.ToMap()}
					mu.Unlock()
				}(idx, call)
			}
			wg.Wait()
		} else {
			// Execute sequentially (write tools or single tool)
			for _, call := range group.Calls {
				idx := callToIdx[call]
				select {
				case <-ctx.Done():
					results[idx] = &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: NewErrorResult("cancelled").ToMap()}
					continue
				default:
				}
				// Use a tool-local context (same pattern as the parallel path) so
				// the shared ctx variable is never mutated across iterations.
				toolCtx := ctx
				if e.handler != nil && e.handler.OnFilePeek != nil {
					toolCtx = ContextWithFilePeekCallback(toolCtx, e.handler.OnFilePeek)
				}
				// Recover from panics in sequential execution (parallel path already has this)
				func() {
					defer func() {
						if r := recover(); r != nil {
							stack := make([]byte, 4096)
							length := runtime.Stack(stack, false)
							logging.Error("panic in sequential tool execution", "tool", call.Name, "panic", r, "stack", string(stack[:length]))
							results[idx] = &genai.FunctionResponse{
								ID: call.ID, Name: call.Name,
								Response: NewErrorResult(formatToolPanic(call.Name, r)).ToMap(),
							}
						}
					}()
					result := e.executeTool(toolCtx, call)
					results[idx] = &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: result.ToMap()}
				}()
			}
		}
	}

	// Pair every deferred (un-executed) call with an explicit result so the
	// assistant's tool_use blocks are never left orphaned — otherwise the next
	// round 400s on Anthropic-compat providers. The message tells the model the
	// call was skipped (not run) so it re-issues it rather than assuming success.
	for _, call := range deferredCalls {
		results = append(results, &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
			Response: NewErrorResult(fmt.Sprintf(
				"not executed: at most %d tool calls run per turn — re-request this call in your next turn",
				MaxFunctionCallsPerResponse)).ToMap(),
		})
	}

	return results, nil
}

// executeTool executes a single tool call with enhanced safety and user awareness.
func (e *Executor) executeTool(ctx context.Context, call *genai.FunctionCall) ToolResult {
	// Step 0: Check circuit breaker
	e.breakerMu.Lock()
	breaker, ok := e.toolBreakers[call.Name]
	if !ok {
		// Initialize with default threshold (5 failures) and timeout (1 minute)
		breaker = robustness.NewCircuitBreaker(5, 1*time.Minute)
		e.toolBreakers[call.Name] = breaker
	}
	e.breakerMu.Unlock()

	var result ToolResult
	err := breaker.Execute(ctx, func() error {
		res := e.doExecuteTool(ctx, call)
		result = res
		if !res.Success && res.PolicyBlock == nil && isExecutionFailure(res.Error) {
			return fmt.Errorf("tool execution failed: %s", res.Error)
		}
		return nil // validation/permission errors don't count as circuit breaker failures
	})

	if err != nil {
		if errors.Is(err, robustness.ErrCircuitOpen) {
			// Try self-healing before returning circuit open error
			if healed := e.trySelfHeal(ctx, call); healed.Success {
				logging.Info("self-healing succeeded", "tool", call.Name)
				return healed
			}
			return NewErrorResult(fmt.Sprintf("Circuit breaker for '%s' is open. Too many failures. Please wait before trying again.", call.Name))
		}
		// If it's not circuit open error, it's the wrapped tool error, which is already in 'result'
	}

	if result.PolicyBlock != nil && e.handler != nil && e.handler.OnToolPolicyBlocked != nil {
		e.handler.OnToolPolicyBlocked(call.Name, *result.PolicyBlock)
	}

	return result
}

// isExecutionFailure returns true if the error message represents a real tool execution
// failure (as opposed to validation, permission, or lookup errors that should not
// trip the circuit breaker).
func isExecutionFailure(errMsg string) bool {
	for _, prefix := range []string{
		"validation error:", "Permission denied:", "permission error:",
		"Permission scope changed before execution:",
		"Safety check failed:", "Safety check unavailable:", "unknown tool:",
		// A user-configured hook refusing a call is policy, not a tool
		// malfunction — must not trip the circuit breaker.
		"hook blocked:",
	} {
		if strings.HasPrefix(errMsg, prefix) {
			return false
		}
	}
	return true
}

// doExecuteTool handles the actual execution logic (previously body of executeTool).
func (e *Executor) doExecuteTool(ctx context.Context, call *genai.FunctionCall) (result ToolResult) {
	toolStart := time.Now()
	// A runtime sandbox/permission toggle may happen from the UI while this call
	// is validating. Use one coherent mode for the whole authorization preflight;
	// the BashTool separately binds its own exact runtime policy snapshot.
	unrestrictedMode := e.IsUnrestrictedMode()
	unrestrictedBypassUsed := false
	defer func() {
		// Fire phase observer even on early returns. Success is determined by
		// the final ToolResult — unknown-tool / validation errors all count as
		// failures in the metrics.
		if e.phaseObserver != nil {
			e.phaseObserver(call.Name, time.Since(toolStart), result.Success)
		}
	}()

	// Invocation capability ceiling (CLI --tools/--disallowed-tools and
	// delegated-agent inheritance). This is a runtime gate in addition to
	// schema filtering: a hallucinated/pretrained tool call must not recover
	// authority merely because it was omitted from the advertised schema.
	if ceiling, restricted := ToolCapabilityCeilingFromContext(ctx); restricted &&
		!toolCapabilityAllows(ceiling, call.Name) {
		return NewPolicyBlockedResult(
			PolicyBlockPermission,
			fmt.Sprintf("tool %q is outside this invocation's capability ceiling", call.Name),
		)
	}

	// Enforce the exact schema advertised by this executor for this request.
	// Models can hallucinate familiar tools that were deliberately omitted by
	// adaptive/feature policy; omission must remain a runtime denial. Trusted
	// internal InvokeTool calls bypass only this model-schema boundary and still
	// pass capability, plan, validation, permission, hook, and audit gates.
	if ceiling, restricted := ToolSchemaCeilingFromContext(ctx, e); restricted &&
		!toolCapabilityAllows(ceiling, call.Name) && !isInternalToolInvocation(ctx, e) {
		return NewPolicyBlockedResult(
			PolicyBlockPermission,
			fmt.Sprintf("tool %q was not available in this request's tool schema", call.Name),
		)
	}

	// Step 1: Basic tool lookup and validation
	tool, ok := e.registry.Get(call.Name)
	if !ok {
		return NewErrorResult(formatUnknownToolError(call.Name, e.registry.Names()))
	}

	// Plan-mode gate (defense in depth). The LLM should never emit a call for
	// a non-read-only tool because the schema filter strips those — but models
	// occasionally hallucinate calls based on pre-training, especially when
	// the user's prompt names a write-style operation. Without this block, a
	// hallucinated `write` or `bash` call would slip past the schema and
	// actually execute. With it, the model gets a targeted error and re-plans.
	if e.planModeCheck != nil && e.planModeCheck() && !IsReadOnlyForPlanMode(call.Name) {
		reason := fmt.Sprintf(
			"tool %q is not allowed in plan mode — only read-only tools (read, glob, grep, list_dir, tree, diff, git_status/diff/log/blame, history_search, web_fetch, web_search, ask_user, etc.) + enter_plan_mode/exit_plan_mode are available. Call enter_plan_mode with your proposed plan when ready to request user approval.",
			call.Name,
		)
		return NewPolicyBlockedResult(PolicyBlockPlan, reason)
	}

	// Guard against nil args — some model responses may omit the args field
	// entirely, producing a nil map that panics on Validate.
	if call.Args == nil {
		call.Args = make(map[string]any)
	}

	if err := tool.Validate(call.Args); err != nil {
		return NewErrorResult(fmt.Sprintf("validation error: %s", err))
	}

	// Retry dedup guard: skip duplicate side-effect calls during retry windows.
	if deduped, reason, ok := e.lookupDuplicateSideEffect(call); ok {
		logging.Warn("skipping duplicate side-effect tool call",
			"tool", call.Name,
			"call_id", call.ID,
			"reason", reason)
		if e.handler != nil && e.handler.OnWarning != nil {
			e.handler.OnWarning(fmt.Sprintf("Skipped duplicate side-effect tool call '%s' (%s). Reusing previous result.", call.Name, reason))
		}
		e.reportRetrySafetyEvent(RetrySafetyEvent{
			Kind: RetrySafetyDuplicateReused, Tool: call.Name, CallID: call.ID, Reason: reason,
		})
		return deduped
	}

	// Checkpoint recovery is active only inside an explicit retry window. A
	// checkpoint is not a general cache for mutating tools: during a healthy
	// turn the model may intentionally repeat an identical command after the
	// workspace changed (for example, run the same test before and after an
	// edit). Unconditional signature replay returned the stale first result and
	// skipped that second verification entirely.
	cached, replayReason, replayState, replayRemaining := e.lookupCheckpointSideEffect(call)
	if replayState == checkpointReplayMatched {
		logging.Info("checkpoint recovery: reusing previous tool result",
			"tool", call.Name,
			"reason", replayReason)
		if e.handler != nil && e.handler.OnWarning != nil {
			e.handler.OnWarning(fmt.Sprintf("Recovered result for '%s' from checkpoint (%s).", call.Name, replayReason))
		}
		e.reportRetrySafetyEvent(RetrySafetyEvent{
			Kind: RetrySafetyCheckpointReplay, Tool: call.Name, CallID: call.ID, Reason: replayReason,
		})
		return cached
	}
	if replayState == checkpointReplayMismatch {
		reason := fmt.Sprintf(
			"Exactly-once recovery blocked divergent side effect %q: it does not match any of the %d unconsumed checkpoint(s). The tool was NOT executed because new mutations would invalidate the remaining saved outcomes. Replay the original pending mutations first; if they cannot be reconstructed, stop and ask the user to inspect /recovery and restart the task manually.",
			call.Name, replayRemaining)
		logging.Warn("checkpoint recovery blocked divergent side effect",
			"tool", call.Name,
			"call_id", call.ID,
			"remaining", replayRemaining)
		if e.handler != nil && e.handler.OnToolDenied != nil {
			e.handler.OnToolDenied(call.Name, reason)
		}
		if e.notificationMgr != nil {
			e.notificationMgr.NotifyDenied(call.Name, reason)
		}
		e.reportRetrySafetyEvent(RetrySafetyEvent{
			Kind: RetrySafetyDivergentBlocked, Tool: call.Name, CallID: call.ID,
			Reason: reason, Remaining: replayRemaining,
		})
		return NewPolicyBlockedResult(PolicyBlockSafety, reason)
	}

	// Hooks are part of actual execution, not replay. Running them before a
	// recovered result can repeat arbitrary hook-side effects even though the
	// tool itself is correctly skipped.
	if e.hooks != nil {
		preResults := e.hooks.RunPreTool(ctx, call.Name, call.Args)
		if blocked, ok := hooks.Blocked(preResults); ok {
			name := blocked.Hook.DisplayName()
			reason := strings.TrimSpace(blocked.Output)
			if reason == "" && blocked.Error != nil {
				reason = blocked.Error.Error()
			}
			logging.Info("tool call blocked by pre-tool hook",
				"tool", call.Name, "hook", name)
			return NewPolicyBlockedResult(PolicyBlockHook, fmt.Sprintf(
				"hook blocked: pre-tool hook %q refused %s: %s", name, call.Name, reason))
		}
	}

	// Pre-mutation barrier: if previous delta-check failed, block further mutating calls.
	if shouldRunDeltaCheckCall(call) {
		if reason, blocked := e.enforceDeltaCheckBarrier(ctx, call); blocked {
			if e.handler != nil && e.handler.OnToolDenied != nil {
				e.handler.OnToolDenied(call.Name, reason)
			}
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyDenied(call.Name, reason)
			}
			return NewPolicyBlockedResult(PolicyBlockSafety, reason)
		}
	}

	// Step 2: Pre-flight safety checks
	var preFlight *PreFlightCheck
	if e.preFlightChecks && e.safetyValidator != nil {
		var err error
		preFlight, err = e.safetyValidator.ValidateSafety(ctx, call.Name, call.Args)
		if err != nil {
			logging.Warn("safety check failed", "tool", call.Name, "error", err)
			if !unrestrictedMode {
				reason := fmt.Sprintf("Safety check unavailable: %s", err)
				if e.handler != nil && e.handler.OnToolDenied != nil {
					e.handler.OnToolDenied(call.Name, reason)
				}
				if e.notificationMgr != nil {
					e.notificationMgr.NotifyDenied(call.Name, reason)
				}
				return NewPolicyBlockedResult(PolicyBlockSafety, reason)
			}
			if e.handler != nil && e.handler.OnWarning != nil {
				e.handler.OnWarning(fmt.Sprintf("[%s] Safety validation unavailable and bypassed in unrestricted mode: %s", call.Name, err))
			}
			unrestrictedBypassUsed = true
		}

		// Notify user about validation
		if e.handler != nil && e.handler.OnToolValidating != nil {
			e.handler.OnToolValidating(call.Name, preFlight)
		}

		// Emit warnings to notification system
		if preFlight != nil && len(preFlight.Warnings) > 0 {
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyValidation(call.Name, preFlight)
			}
			if e.handler != nil && e.handler.OnWarning != nil {
				for _, warning := range preFlight.Warnings {
					e.handler.OnWarning(fmt.Sprintf("[%s] %s", call.Name, warning))
				}
			}
		}

		// Block execution if safety check failed (unless unrestricted mode is on)
		if preFlight != nil && !preFlight.IsValid {
			reasons := strings.Join(preFlight.Errors, "; ")

			if unrestrictedMode {
				// In unrestricted mode, convert errors to warnings and allow execution
				preFlight.Warnings = append(preFlight.Warnings, preFlight.Errors...)
				preFlight.Errors = nil
				preFlight.IsValid = true
				logging.Warn("unrestricted mode: safety check bypassed",
					"tool", call.Name,
					"original_errors", reasons)
				if e.handler != nil && e.handler.OnWarning != nil {
					e.handler.OnWarning(fmt.Sprintf("[%s] Bypassed in unrestricted mode: %s", call.Name, reasons))
				}
				unrestrictedBypassUsed = true
			} else {
				if e.handler != nil && e.handler.OnToolDenied != nil {
					e.handler.OnToolDenied(call.Name, reasons)
				}
				if e.notificationMgr != nil {
					e.notificationMgr.NotifyDenied(call.Name, reasons)
				}
				return NewPolicyBlockedResult(PolicyBlockSafety, fmt.Sprintf("Safety check failed: %s", reasons))
			}
		}
	}

	// Step 3: Get execution summary for user awareness
	var summary *ExecutionSummary
	if e.safetyValidator != nil {
		summary = e.safetyValidator.GetSummary(call.Name, call.Args)
	}

	// Step 4: Permission check
	// Capture hidden runtime state even when interactive permissions are
	// disabled. This binds Bash execution across hooks/cache/gates and prevents a
	// concurrent sandbox/workspace toggle from changing the invocation that was
	// validated above.
	authorizedPermissionArgs := PermissionArgsForTool(tool, call.Args)
	if e.permissions != nil {
		permissionCtx := permission.ContextWithWorkDir(ctx, e.workDir)
		resp, err := e.permissions.CheckWithTemporaryToolRules(
			permissionCtx,
			call.Name,
			ClonePermissionArgs(authorizedPermissionArgs),
			SkillPermissionGrants(e.registry),
			SkillPermissionDenies(e.registry),
		)
		if err != nil {
			if e.handler != nil && e.handler.OnToolDenied != nil {
				e.handler.OnToolDenied(call.Name, err.Error())
			}
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyDenied(call.Name, err.Error())
			}
			return NewPolicyBlockedResult(PolicyBlockPermission, fmt.Sprintf("permission error: %s", err))
		}
		if !resp.Allowed {
			reason := resp.Reason
			if reason == "" {
				reason = "permission denied"
			}
			if e.handler != nil && e.handler.OnToolDenied != nil {
				e.handler.OnToolDenied(call.Name, reason)
			}
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyDenied(call.Name, reason)
			}
			return NewPolicyBlockedResult(PolicyBlockPermission, fmt.Sprintf("Permission denied: %s", reason))
		}
	}

	// Step 4.6: Out-of-workspace access gate (Claude-Code-style ask-on-access).
	// For an ABSOLUTE path the call targets outside the workspace + granted dirs,
	// prompt the user. On grant, the handler propagated the dir to the shared
	// registry — the live tool's PathValidator is rebuilt — so execution proceeds.
	// On deny, return an actionable error (reaches the model). Only relative paths
	// (always workspace-relative) and in-scope paths skip silently. Disabled when
	// the callbacks are nil (tests/non-app harnesses) — the tool's own validator
	// still enforces default-deny.
	if e.dirAccessChecker != nil && e.dirGrantHandler != nil {
		for _, p := range extractFilePaths(call) {
			if !filepath.IsAbs(p) || e.dirAccessChecker(p) {
				continue
			}
			allowed, gErr := e.dirGrantHandler(ctx, call.Name, p)
			if gErr != nil {
				return NewPolicyBlockedResult(PolicyBlockPermission, fmt.Sprintf("directory access check failed for %s: %s", p, gErr))
			}
			if !allowed {
				return NewPolicyBlockedResult(PolicyBlockPermission, fmt.Sprintf("Access to %s is outside the workspace and is NOT granted. This is a hard permission boundary — retrying or rewriting the path will not help. Ask the user to run: /add-dir %s  (add --persist to keep it across restarts). Until then, work within the workspace.", p, filepath.Dir(p)))
			}
		}
	}

	// Step 4.7: Discuss-mode action gate (ask-once). When the turn is an
	// analysis/discussion the user has not yet confirmed for implementation, the
	// FIRST code-mutating tool pauses for a one-time confirm rather than silently
	// implementing. Read/grep/bash-verify flow free (not IsImplementationTool).
	// On confirm the app flips the turn to action (discussGate -> false) so the
	// rest of the turn proceeds untouched; on deny an actionable result keeps the
	// agent in analysis. Disabled when callbacks are nil (tests/headless).
	if e.discussGate != nil && e.actionConfirmHandler != nil && IsImplementationTool(call.Name) && e.discussGate() {
		target := call.Name
		if paths := extractFilePaths(call); len(paths) > 0 {
			target = paths[0]
		}
		allowed, cErr := e.actionConfirmHandler(ctx, call.Name, target)
		if cErr != nil {
			return NewPolicyBlockedResult(PolicyBlockPermission, fmt.Sprintf("action confirmation failed: %s", cErr))
		}
		if !allowed {
			return NewPolicyBlockedResult(PolicyBlockPermission, "Staying in analysis — I did NOT apply this change. We're discussing/analyzing, not implementing. When you want me to actually make changes, say so explicitly (e.g. \"implement it\" / \"сделай\") and I'll proceed. For now, keep analyzing and proposing.")
		}
	}

	// Step 4.5: Check tool result cache for read-only tools
	if e.toolCache != nil {
		if cached, hit := e.toolCache.Get(call.Name, call.Args); hit {
			serveCache := true
			// For read tool: dedup unchanged repeats to a stub, but DETECT
			// staleness — CheckAndRecord stats the file and its 3rd return
			// (fileChanged) is true when the file mutated on disk since the
			// cached read (an out-of-band bash / codegen / git-checkout write
			// that carries no file_path arg, so the normal write-path
			// invalidation never fired). Serving the cache then hands the model
			// PRE-WRITE bytes with no error — it reasons/edits from stale content
			// and clobbers the change. So on fileChanged we drop the cache entry
			// AND the tracker record we just wrote (so the real re-read below
			// serves FULL fresh content, not a dedup stub) and fall through.
			if call.Name == "read" && e.readTracker != nil && cached.Success {
				filePath, _ := call.Args["file_path"].(string)
				offset := GetIntDefault(call.Args, "offset", 1)
				limit := GetIntDefault(call.Args, "limit", 2000)
				if filePath != "" {
					isDup, origRec, fileChanged, dupCount := e.readTracker.CheckAndRecord(filePath, offset, limit, len(cached.Content))
					if fileChanged {
						e.toolCache.InvalidateByFile(filePath)
						e.readTracker.InvalidateFile(filePath)
						serveCache = false
					} else if isDup && origRec != nil {
						if stub, ok := dedupReadStub(origRec, filePath, dupCount); ok {
							cached.Content = stub
						}
						// dupCount == 2 (or other !ok cases): keep the full cached
						// content so a model that lost track of the file gets it
						// back instead of looping on a stub.
					}
				}
			}
			if serveCache {
				logging.Debug("tool cache hit", "tool", call.Name)
				return cached
			}
		}
	}

	// Step 5: Notify user about approved execution
	if summary != nil && summary.UserVisible {
		if e.notificationMgr != nil {
			e.notificationMgr.NotifyApproved(call.Name, summary)
		}
		if e.handler != nil && e.handler.OnToolApproved != nil {
			e.handler.OnToolApproved(call.Name, summary)
		}
	}

	// Step 7: Create execution context
	execInfo := &ExecutionInfo{}
	if summary != nil {
		execInfo.SafetyLevel = summary.RiskLevel
	}

	// Notify start
	if e.handler != nil && e.handler.OnToolStart != nil {
		e.handler.OnToolStart(call.Name, call.Args)
	}

	// If history suggests this tool is slow, give the user a heads-up before
	// it blocks the status bar for many seconds. Threshold matches adaptive
	// timeout floor — below 5s we don't consider it "slow" enough to warn.
	if e.handler != nil && e.handler.OnWarning != nil && e.toolStatsLookup != nil {
		if p95, _, ok := e.toolStatsLookup(call.Name); ok && p95 >= 5*time.Second {
			e.handler.OnWarning(fmt.Sprintf(
				"%s typically takes ~%v (p95 from session history)",
				call.Name, p95.Round(time.Second)))
		}
	}

	// If safety was bypassed only because this call began in unrestricted mode,
	// turning restrictions back on while hooks/prompts/gates were in flight must
	// fail closed immediately before dispatch. Bash additionally checks its exact
	// captured policy revision inside the serialized execution gate.
	if unrestrictedBypassUsed && !e.IsUnrestrictedMode() {
		return NewPolicyBlockedResult(PolicyBlockSafety,
			"Security mode changed during validation: unrestricted safety bypass is stale; retry under the current sandbox and permission policy")
	}

	// Step 8: Execute with timeout. If we have enough observations for this
	// tool, auto-tune the timeout via adaptiveToolTimeout so fast tools fail
	// fast while slow tools get historically-observed room to breathe.
	var observedP95 time.Duration
	var haveStats bool
	if e.toolStatsLookup != nil {
		p95, _, ok := e.toolStatsLookup(call.Name)
		observedP95 = p95
		haveStats = ok
	}
	baseToolTimeout := e.toolTimeout()
	toolTimeout, hasToolDeadline := e.resolveToolExecutionTimeout(
		baseToolTimeout, observedP95, haveStats, call.Name, call.Args,
	)
	var execCtx context.Context
	var cancel context.CancelFunc
	if hasToolDeadline {
		execCtx, cancel = context.WithTimeout(ctx, toolTimeout)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// Inject streaming callback so tools (e.g. task/agent) can stream output to UI
	if e.handler != nil && e.handler.OnText != nil {
		execCtx = ContextWithStreamingCallback(execCtx, StreamingCallback(e.handler.OnText))
	}

	// Inject progress callback so tools can report granular progress to UI
	if e.handler != nil && e.handler.OnToolDetailedProgress != nil {
		toolName := call.Name
		onDetailed := e.handler.OnToolDetailedProgress
		execCtx = ContextWithProgressCallback(execCtx, func(progress float64, currentStep string) {
			onDetailed(toolName, progress, currentStep)
		})
	}

	// Inject memory notification callback
	if e.handler != nil && e.handler.OnMemoryNotify != nil {
		execCtx = ContextWithMemoryNotify(execCtx, e.handler.OnMemoryNotify)
	}

	start := time.Now()

	// Start progress heartbeat for long-running operations
	// Use 500ms interval for responsive UI feedback (was 5s)
	done := make(chan struct{})
	if e.handler != nil && e.handler.OnToolProgress != nil {
		onProgress := e.handler.OnToolProgress // snapshot to avoid nil checks in goroutine
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logging.Error("panic in tool progress goroutine", "tool", call.Name, "panic", r)
				}
			}()

			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					onProgress(call.Name, time.Since(start))
				case <-done:
					return
				case <-execCtx.Done():
					return
				}
			}
		}()
	}

	// Check if tool supports streaming for large outputs.
	var err error
	type toolOutcome struct {
		result ToolResult
		err    error
	}
	runTool := func() (outcome toolOutcome) {
		defer func() {
			if r := recover(); r != nil {
				stack := make([]byte, 4096)
				n := runtime.Stack(stack, false)
				logging.Error("tool execution panic", "tool", call.Name, "panic", r, "stack", string(stack[:n]))
				outcome = toolOutcome{result: NewErrorResult(formatToolPanic(call.Name, r))}
			}
		}()
		if _, scoped := tool.(permissionScopedExecutor); scoped && authorizedPermissionArgs != nil {
			outcome.result, outcome.err = ExecuteWithPermissionScope(execCtx, tool, call.Args, authorizedPermissionArgs)
		} else if streamingTool, ok := tool.(StreamingTool); ok && streamingTool.SupportsStreaming() {
			outcome.result, outcome.err = e.executeStreamingTool(execCtx, streamingTool, call.Args)
		} else {
			outcome.result, outcome.err = tool.Execute(execCtx, call.Args)
		}
		return outcome
	}

	if canAbandonToolOnCancel(tool, call.Name) {
		// Explicitly side-effect-free tools may run behind a timeout backstop. A
		// broken implementation can leak a goroutine, but cannot mutate state
		// after control has returned to the user.
		toolDone := make(chan toolOutcome, 1)
		go func() { toolDone <- runTool() }()
		select {
		case outcome := <-toolDone:
			result, err = outcome.result, outcome.err
		case <-execCtx.Done():
			// Give a well-behaved tool a brief grace to deliver its own (more
			// informative) result/error on cancel.
			select {
			case outcome := <-toolDone:
				result, err = outcome.result, outcome.err
			case <-time.After(50 * time.Millisecond):
				err = execCtx.Err()
			}
		}
	} else {
		// Stateful and unknown tools must be joined. Returning while one keeps
		// running permits a late write/commit/remote action after the ledger has
		// recorded failure, so a retry can duplicate it. Built-ins use
		// context-aware I/O/process APIs and therefore still honor toolTimeout.
		outcome := runTool()
		result, err = outcome.result, outcome.err
	}
	close(done)
	duration := time.Since(start)

	if err != nil {
		// Provide informative timeout message with tool name and duration
		if errors.Is(err, context.DeadlineExceeded) {
			errMsg := fmt.Sprintf("tool '%s' timed out after %v (limit: %v)", call.Name, duration.Round(time.Millisecond), toolTimeout)
			logging.Error("tool execution timed out",
				"tool", call.Name,
				"duration", duration,
				"timeout", baseToolTimeout)
			result = NewErrorResult(errMsg)
		} else {
			logging.Error("tool execution failed",
				"tool", call.Name,
				"error", err,
				"duration", duration)
			result = NewErrorResult(err.Error())
		}
	}

	// A permission-scoped tool can refuse execution after authorization if its
	// hidden runtime scope changed before the execution lease was acquired.
	// Preserve that as a policy outcome rather than an ordinary tool failure.
	if !result.Success && result.PolicyBlock == nil &&
		strings.HasPrefix(result.Error, "Permission scope changed before execution:") {
		result = WithPolicyBlock(result, PolicyBlockPermission, result.Error)
	}

	// Enrich error results with actionable suggestions
	if !result.Success && result.Error != "" {
		if suggestion := getErrorSuggestion(result.Error); suggestion != "" {
			result.Error = result.Error + "\nSuggestion: " + suggestion
		}
	}

	// Enrich empty successful results with diagnostic hints
	if result.Success && result.Content == "" {
		if hint := getEmptyResultHint(call.Name); hint != "" {
			result.Content = hint
		}
	}

	// Enrich result with execution metadata
	if result.ExecutionSummary == nil && summary != nil {
		result.ExecutionSummary = summary
	}
	result.SafetyLevel = execInfo.SafetyLevel
	result.Duration = format.Duration(duration)

	// Step 10: Run post-tool or on-error hooks
	if e.hooks != nil {
		if result.Success {
			e.hooks.RunPostTool(ctx, call.Name, call.Args, result.Content)
		} else {
			e.hooks.RunOnError(ctx, call.Name, call.Args, result.Error)
		}
	}

	// Step 10.5: Auto-format written files
	if e.formatter != nil && result.Success {
		if call.Name == "write" || call.Name == "edit" {
			if filePath, ok := call.Args["file_path"].(string); ok {
				_ = e.formatter.Format(ctx, filePath)
			}
		}
	}

	// Step 10.7: Emit inline diff for edit operations
	if e.handler != nil && e.handler.OnInlineDiff != nil && result.Success {
		if call.Name == "edit" {
			oldText, _ := call.Args["old_string"].(string)
			newText, _ := call.Args["new_string"].(string)
			if oldText != "" && oldText != newText {
				filePath, _ := call.Args["file_path"].(string)
				e.handler.OnInlineDiff(filePath, oldText, newText)
			}
		}
	}

	// Preserve tool-declared paths before secret redaction and result
	// compaction. Both transformations are presentation concerns and may rewrite
	// arbitrary Data strings; post-mutation bookkeeping must use the exact paths
	// returned by the tool itself.
	mutation := snapshotGoMutation(result)
	declaredWrittenPaths := mutation.Paths
	if result.Success {
		declaredWrittenPaths = append([]string(nil), declaredWrittenPaths...)
	}

	// Step 11: apply redaction
	if e.redactor != nil {
		result.Content = e.redactor.Redact(result.Content)
		result.Error = e.redactor.Redact(result.Error)
		if result.Data != nil {
			result.Data = e.redactor.RedactAny(result.Data)
		}
	}

	// Step 12: Apply compaction if configured
	if e.compactor != nil && result.Success {
		policyBlock := result.PolicyBlock
		result = e.compactor.CompactForType(call.Name, result)
		// Compactors predate policy metadata and some construct a fresh result.
		// Never let presentation-size compaction erase a fail-closed outcome.
		result.PolicyBlock = policyBlock
	}

	// Step 12.5: Cache result or invalidate cache for write operations
	if isWriteOperation(call.Name) {
		// Invalidate all caches that might reference the modified file
		filePaths := extractFilePaths(call)
		if e.toolCache != nil {
			for _, p := range filePaths {
				e.toolCache.InvalidateByFile(p)
			}
			// Git state changes when files are modified
			e.toolCache.InvalidateByTool("git_status")
			e.toolCache.InvalidateByTool("git_diff")
			e.toolCache.InvalidateByTool("review_changes")
			// grep/glob can flip on ANY content/existence change, and a NO-MATCH
			// query is cached against ZERO files — so per-file invalidation
			// (InvalidateByFile) can never reach it. Drop grep+glob wholesale: a
			// stale "No matches found" after the agent's own write is the
			// investigation-critical false-negative (model concludes a just-written
			// symbol/file does not exist → re-implements or abandons the task).
			e.toolCache.InvalidateByTool("grep")
			e.toolCache.InvalidateByTool("glob")
			// tree/list_dir may be stale after file create/delete
			if call.Name == "write" || call.Name == "delete" || call.Name == "mkdir" ||
				call.Name == "copy" || call.Name == "move" {
				e.toolCache.InvalidateByTool("tree")
				e.toolCache.InvalidateByTool("list_dir")
			}
		}
		if e.searchCache != nil {
			for _, p := range filePaths {
				e.searchCache.InvalidateByPath(p)
			}
			// Same no-match blind spot as the toolCache: a grep/glob that matched
			// nothing tracks zero files, so InvalidateByPath/InvalidateByDir can't
			// reach it. Clear the dedicated search cache so grep.go/glob.go don't
			// serve a stale "no matches" for content the agent just wrote.
			e.searchCache.Clear()
		}
	} else if e.toolCache != nil && result.Success {
		// Only cache read-only tool results
		e.toolCache.Put(call.Name, call.Args, result)
	}

	// Step 12.7: Deduplicate read content for history optimization
	if e.readTracker != nil {
		if call.Name == "read" && result.Success {
			filePath, _ := call.Args["file_path"].(string)
			offset := GetIntDefault(call.Args, "offset", 1)
			limit := GetIntDefault(call.Args, "limit", 2000)
			if filePath != "" {
				isDup, origRec, _, dupCount := e.readTracker.CheckAndRecord(filePath, offset, limit, len(result.Content))
				if isDup && origRec != nil {
					if stub, ok := dedupReadStub(origRec, filePath, dupCount); ok {
						result.Content = stub
					}
					// !ok (dupCount == 2): keep result.Content as the full
					// freshly-read content to break an amnesia re-read loop.
				}
			}
		}
		// Invalidate read tracker on write operations (all path-like arg keys,
		// including source/destination for copy/move).
		if isWriteOperation(call.Name) {
			for _, key := range []string{"file_path", "path", "source", "destination", "new_path"} {
				if p, ok := call.Args[key].(string); ok && p != "" {
					e.readTracker.InvalidateFile(p)
				}
			}
		}
	}

	// Record successfully modified files in the write tracker.
	if result.Success && e.writeTracker != nil && isFileModifyingTool(call.Name) {
		for _, key := range []string{"file_path", "path", "destination", "new_path"} {
			if p, ok := call.Args[key].(string); ok && p != "" {
				e.writeTracker.Record(p)
			}
		}
	}

	// Tool-declared writes: a tool that mutates a file OUTSIDE the normal
	// write-tool dispatch (e.g. `memorize` writing .gokin/project-memory.md via
	// ProjectLearning, bypassing the executor's arg-based path extraction above)
	// can declare the files it wrote in result.Data["written_paths"]. Invalidate
	// the read-dedup for each (so the agent can re-read to VERIFY its own write —
	// the "cannot re-read, returns [Unchanged]" bug) and record them for
	// post-compaction hints. Generic + drift-proof (no per-tool name list) and
	// defensive (a malformed Data value is a silent no-op, never a panic).
	if result.Success {
		for _, p := range declaredWrittenPaths {
			if e.readTracker != nil {
				e.readTracker.InvalidateFile(p)
			}
			if e.writeTracker != nil {
				e.writeTracker.Record(p)
			}
			// Also drop the RESULT caches for the path — otherwise a verify-read
			// of an out-of-band write hits the TTL-only toolCache and serves
			// PRE-WRITE content (the read-dedup invalidation above only frees the
			// dedup stub, not the cached result). Mirrors Step 12.5.
			if e.toolCache != nil {
				e.toolCache.InvalidateByFile(p)
			}
			if e.searchCache != nil {
				e.searchCache.InvalidateByPath(p)
			}
		}
	}

	// Step 12.8: Fast, workspace-aware Go diagnostics. This runs before the
	// broader delta-check so cached gopls type information can give the model a
	// precise error close to the mutation that caused it.
	if result.Success && shouldRunDeltaCheckCall(call) {
		e.runGoDiagnosticsAfterMutation(ctx, call, mutation, &result)
	}

	// Step 12.9: Post-edit delta-check on changed modules (format/lint/build; no tests).
	if result.Success && shouldRunDeltaCheckCall(call) {
		if delta := e.runDeltaCheckAfterMutation(ctx, call); delta != nil {
			if !delta.Passed {
				logDeltaCheckFailure(*delta)
				if e.handler != nil && e.handler.OnWarning != nil {
					e.handler.OnWarning(delta.Summary)
				}
				if result.Content != "" {
					result.Content += "\n\n" + delta.Summary
				} else {
					result.Content = delta.Summary
				}
				if detail := strings.TrimSpace(delta.Details); detail != "" {
					result.Content += "\n" + trimDeltaOutput(detail, 1200)
				}
			}
		}
	}

	// Step 12.9b: Semantic validators (test quality, CI workflow, version consistency).
	if result.Success && shouldRunDeltaCheckCall(call) && e.semanticValidators != nil {
		for _, fp := range extractFilePaths(call) {
			content, err := os.ReadFile(fp)
			if err != nil {
				continue
			}
			warnings := e.semanticValidators.RunAll(ctx, fp, content, e.workDir)
			if formatted := FormatWarnings(warnings); formatted != "" {
				result.Content += "\n\n" + formatted
			}
			// Learn from warnings for future prevention
			if e.errorLearner != nil {
				for _, w := range warnings {
					if w.Severity == "error" || w.Severity == "warning" {
						_ = e.errorLearner.LearnError(
							"validation_"+w.Validator,
							w.Message,
							"Before writing "+filepath.Ext(fp)+" files, check: "+w.Message,
							[]string{w.Validator, filepath.Ext(fp)},
						)
					}
				}
			}
		}
	}

	// Step 12.9c: Context enrichment (inject canonical versions, function signatures).
	if result.Success && shouldRunDeltaCheckCall(call) && e.contextEnricher != nil {
		for _, fp := range extractFilePaths(call) {
			if hint := e.contextEnricher.Enrich(fp); hint != "" {
				result.Content += "\n\n" + hint
			}
		}
	}

	// Step 12.9d: Track mutated files for self-review injection.
	if result.Success && isWriteOperation(call.Name) && e.selfReviewThreshold > 0 {
		e.mutatedFilesMu.Lock()
		if e.mutatedFilesThisTurn == nil {
			e.mutatedFilesThisTurn = make(map[string]bool)
		}
		for _, fp := range selfReviewMutationPaths(call, mutation) {
			e.mutatedFilesThisTurn[fp] = true
		}
		e.mutatedFilesMu.Unlock()
	}

	// Post-checks may append compiler output, source snippets, or validator
	// messages after the first redaction pass. Redact once more before audit,
	// callbacks, notifications, and the model receive the final result.
	if e.redactor != nil {
		result.Content = e.redactor.Redact(result.Content)
		result.Error = e.redactor.Redact(result.Error)
	}

	// Post-mutation diagnostics and delta-checks are part of the operation the
	// user waits for. Refresh duration before audit/UI notification so a 20s
	// diagnostic timeout cannot appear as a 5ms edit in telemetry.
	duration = time.Since(start)
	result.Duration = format.Duration(duration)

	// Log the final, redacted/compacted result after post-mutation checks. This
	// both captures diagnostic failures and prevents the audit sink from seeing
	// raw secrets that Step 11 has already removed from the model/UI result.
	if e.auditLogger != nil {
		e.sessionIDMu.RLock()
		sessionID := e.sessionID
		e.sessionIDMu.RUnlock()
		entry := audit.NewEntry(sessionID, call.Name, call.Args)
		entry.Complete(result.Content, result.Success, result.Error, duration)

		if preFlight != nil {
			entry.Args["safety_warnings"] = preFlight.Warnings
			entry.Args["safety_level"] = string(execInfo.SafetyLevel)
		}

		if err := e.auditLogger.Log(entry); err != nil {
			logging.Warn("failed to write audit log", "error", err, "tool", call.Name)
		}
	}

	// Step 13: Notify completion and send notifications
	if e.handler != nil && e.handler.OnToolEnd != nil {
		e.handler.OnToolEnd(call.Name, call.Args, result)
	}

	if e.notificationMgr != nil {
		if result.Success {
			// Nil check for summary before calling String()
			var summaryStr string
			if summary != nil {
				summaryStr = summary.String()
			}
			e.notificationMgr.NotifySuccess(call.Name, summaryStr, summary, duration)
		} else {
			e.notificationMgr.NotifyError(call.Name, "Execution failed", result.Error)
		}
	}

	// Log execution metrics
	logging.Info("tool execution completed",
		"tool", call.Name,
		"success", result.Success,
		"duration", duration,
		"safety_level", execInfo.SafetyLevel)

	// Record side effects for retry deduplication.
	e.recordSideEffect(call, result)

	// Record in checkpoint journal for API failure recovery (write operations only).
	if e.checkpoint != nil && isWriteOperation(call.Name) {
		e.checkpoint.Record(call, result)
	}

	return result
}

type internalToolInvocationKey struct {
	executor *Executor
}

func isInternalToolInvocation(ctx context.Context, executor *Executor) bool {
	if ctx == nil {
		return false
	}
	trusted, _ := ctx.Value(internalToolInvocationKey{executor: executor}).(bool)
	return trusted
}

func (e *Executor) lookupDuplicateSideEffect(call *genai.FunctionCall) (ToolResult, string, bool) {
	if call == nil || !isWriteOperation(call.Name) {
		return ToolResult{}, "", false
	}

	e.sideEffectLedgerMu.Lock()
	defer e.sideEffectLedgerMu.Unlock()

	if !e.sideEffectDedupEnabled {
		return ToolResult{}, "", false
	}

	signature := sideEffectSignature(call.Name, call.Args)
	if call.ID != "" && signature != "" {
		if prev, ok := e.replayByCallID[call.ID]; ok && e.replaySigByCallID[call.ID] == signature {
			e.consumeSideEffectReplayLocked(call.ID, signature)
			if e.checkpoint != nil {
				_, _, _ = e.checkpoint.ConsumeReplay(call)
			}
			return prev, "tool_call_id", true
		}
	}

	if signature != "" {
		if prev, ok := e.replayBySignature[signature]; ok {
			e.consumeSideEffectReplayLocked(call.ID, signature)
			if e.checkpoint != nil {
				_, _, _ = e.checkpoint.ConsumeReplay(call)
			}
			return prev, "fingerprint", true
		}
	}

	return ToolResult{}, "", false
}

func (e *Executor) consumeSideEffectReplayLocked(callID, signature string) {
	if callID != "" {
		delete(e.replayByCallID, callID)
		if signature == "" {
			signature = e.replaySigByCallID[callID]
		}
		delete(e.replaySigByCallID, callID)
	}
	if signature == "" {
		return
	}
	delete(e.replayBySignature, signature)
	for id, sig := range e.replaySigByCallID {
		if sig == signature {
			delete(e.replayByCallID, id)
			delete(e.replaySigByCallID, id)
		}
	}
}

// lookupCheckpointSideEffect consults the durable checkpoint journal only
// while retry-time side-effect recovery is explicitly enabled. It takes the
// ledger lock before the journal lock, matching ResetSideEffectLedger, so a
// concurrent retry-mode transition cannot expose a half-cleared journal.
func (e *Executor) lookupCheckpointSideEffect(call *genai.FunctionCall) (ToolResult, string, checkpointReplayState, int) {
	if call == nil || !isWriteOperation(call.Name) || e.checkpoint == nil {
		return ToolResult{}, "", checkpointReplayInactive, 0
	}

	e.sideEffectLedgerMu.Lock()
	defer e.sideEffectLedgerMu.Unlock()
	if !e.sideEffectDedupEnabled {
		return ToolResult{}, "", checkpointReplayInactive, 0
	}
	result, reason, state := e.checkpoint.consumeReplayState(call)
	return result, reason, state, e.checkpoint.ReplayRemaining()
}

func (e *Executor) recordSideEffect(call *genai.FunctionCall, result ToolResult) {
	if call == nil || !isWriteOperation(call.Name) {
		return
	}

	e.sideEffectLedgerMu.Lock()
	defer e.sideEffectLedgerMu.Unlock()

	// Store result as-is: failed side-effecting calls can still have partial effects.
	if call.ID != "" {
		e.sideEffectsByCallID[call.ID] = result
	}
	if signature := sideEffectSignature(call.Name, call.Args); signature != "" {
		e.sideEffectsBySignature[signature] = result
		if call.ID != "" {
			e.sideEffectSigByCallID[call.ID] = signature
		}
	}
}

func sideEffectSignature(toolName string, args map[string]any) string {
	if toolName == "" {
		return ""
	}

	normalized := make(map[string]any, len(args))
	for k, v := range args {
		key := strings.ToLower(strings.TrimSpace(k))
		switch key {
		case "timestamp", "ts", "nonce", "request_id":
			continue
		default:
			normalized[k] = v
		}
	}

	encoded, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(toolName + "|" + string(encoded)))
	return hex.EncodeToString(sum[:])
}

// buildResponseParts returns Parts from a response.
// Adds a fallback " " text part if the response has no content to ensure valid history.
func (e *Executor) buildResponseParts(resp *client.Response) []*genai.Part {
	parts := e.buildResponsePartsRaw(resp)
	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(" "))
	}
	return parts
}

// buildResponsePartsRaw returns Parts from a response without a fallback placeholder.
// Used for partial response preservation where empty means "nothing to preserve".
func (e *Executor) buildResponsePartsRaw(resp *client.Response) []*genai.Part {
	return client.ResponsePartsForHistory(resp)
}

// executeStreamingTool executes a streaming tool and collects results.
// Streams chunks to the handler for real-time feedback.
func (e *Executor) executeStreamingTool(ctx context.Context, tool StreamingTool, args map[string]any) (ToolResult, error) {
	stream, err := tool.ExecuteStreaming(ctx, args)
	if err != nil {
		return NewErrorResult(err.Error()), nil
	}

	// Collect streaming output, optionally streaming to handler
	var content strings.Builder
	chunkCount := 0

streamLoop:
	for {
		select {
		case chunk, ok := <-stream.Chunks:
			if !ok {
				// The producer sends its terminal error before closing Chunks.
				// Check it here as well as in the Done branch: when both closed
				// channels are selectable, Go may choose Chunks first.
				select {
				case streamErr := <-stream.Error:
					if streamErr != nil {
						return NewErrorResult(streamErr.Error()), nil
					}
				default:
				}
				break streamLoop
			}
			content.WriteString(chunk)
			chunkCount++

			// Stream to handler for real-time feedback (every 10 chunks or significant content)
			if e.handler != nil && e.handler.OnText != nil && (chunkCount%10 == 0 || len(chunk) > 100) {
				// Note: OnText is for model responses, but we can use it for streaming tool output
				// This may need a separate handler in a future iteration
			}

		case err := <-stream.Error:
			if err != nil {
				return NewErrorResult(err.Error()), nil
			}

		case <-stream.Done:
			// Drain remaining chunks
			for chunk := range stream.Chunks {
				content.WriteString(chunk)
			}
			// Check for any error sent before complete() closed the channels.
			select {
			case streamErr := <-stream.Error:
				if streamErr != nil {
					return NewErrorResult(streamErr.Error()), nil
				}
			default:
			}
			break streamLoop

		case <-ctx.Done():
			// Drain chunks in background to unblock the producer goroutine.
			// Cap the drain so a stuck producer can't leak a goroutine forever.
			go func() {
				drainTimer := time.NewTimer(5 * time.Second)
				defer drainTimer.Stop()
				for {
					select {
					case _, ok := <-stream.Chunks:
						if !ok {
							return
						}
					case <-drainTimer.C:
						return
					}
				}
			}()
			return NewErrorResult("streaming cancelled"), client.ContextErr(ctx)
		}
	}

	// Closed producer channels may win the select race against ctx.Done. Never
	// convert a cancelled, partially collected stream into a successful result.
	if err := ctx.Err(); err != nil {
		return NewErrorResult("streaming cancelled"), client.ContextErr(ctx)
	}

	result := content.String()
	if result == "" {
		return NewSuccessResult("No output."), nil
	}
	return NewSuccessResult(result), nil
}

// trySelfHeal attempts alternative approaches when a tool's circuit breaker is open.
func (e *Executor) trySelfHeal(ctx context.Context, call *genai.FunctionCall) ToolResult {
	switch call.Name {
	case "read":
		// If read fails, try glob to find similar files
		if path, ok := call.Args["file_path"].(string); ok {
			globTool, exists := e.registry.Get("glob")
			if exists {
				globResult, err := globTool.Execute(ctx, map[string]any{
					"pattern": path + "*",
				})
				if err == nil && globResult.Success {
					return ToolResult{
						Content: fmt.Sprintf("File read failed (circuit open). Similar files found:\n%s", globResult.Content),
						Success: true,
					}
				}
			}
		}

	case "bash":
		// If bash fails repeatedly, suggest debug mode
		if cmd, ok := call.Args["command"].(string); ok {
			return ToolResult{
				Content: fmt.Sprintf("Bash execution failed repeatedly (circuit open). Suggestion: try 'bash -x %s' for debug output, or check if the command requires different permissions.", cmd),
				Success: true,
			}
		}

	case "grep":
		// If grep fails, try read with manual search
		if pattern, ok := call.Args["pattern"].(string); ok {
			return ToolResult{
				Content: fmt.Sprintf("Grep circuit open. Try using 'read' tool to read specific files and search for '%s' manually.", pattern),
				Success: true,
			}
		}
	}

	return ToolResult{Success: false}
}

// extractFilePaths returns all file paths from a tool call's arguments.
// Handles file_path, path, source, destination, and new_path argument names.
func extractFilePaths(call *genai.FunctionCall) []string {
	var paths []string
	for _, key := range []string{"file_path", "path", "source", "destination", "new_path"} {
		if p, ok := call.Args[key].(string); ok && p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// selfReviewMutationPaths prefers authoritative paths declared by a tool
// result. Batch/refactor dry-runs and no-ops deliberately declare an empty
// path set; falling back to their requested arguments would claim files were
// modified when they were not. Legacy scalar tools still use their call args.
func selfReviewMutationPaths(call *genai.FunctionCall, mutation goMutationSnapshot) []string {
	if mutation.ChangedKnown && !mutation.Changed {
		return nil
	}

	var paths []string
	if mutation.PathsDeclared {
		paths = append(paths, mutation.Paths...)
	} else if call != nil {
		// Modern multi-target tools always declare the paths they actually
		// changed. Missing declarations mean no confirmed mutation, not that
		// every requested match changed.
		if call.Name == "batch" || call.Name == "refactor" {
			return nil
		}
		paths = extractFilePaths(call)
	}

	filtered := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			path = filepath.Clean(path)
		}
		if path != "" && path != "." && !seen[path] {
			seen[path] = true
			filtered = append(filtered, path)
		}
	}
	return filtered
}

// writtenPathsFromResult extracts a tool's self-declared written paths from
// result.Data["written_paths"]. A tool that mutates a file outside the normal
// write-tool dispatch (memorize -> ProjectLearning) sets this so the executor
// can invalidate the read-dedup + record the write. Defensive: any non-conforming
// Data shape yields nil (no panic), so it is safe to call on every result.
func writtenPathsFromResult(result ToolResult) []string {
	m, ok := result.Data.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["written_paths"]
	if !ok {
		return nil
	}
	var out []string
	switch v := raw.(type) {
	case []string:
		for _, p := range v {
			if p != "" {
				out = append(out, p)
			}
		}
	case []any:
		for _, item := range v {
			if p, ok := item.(string); ok && p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// WrittenPathsFromResult is the exported form of writtenPathsFromResult, for the
// done-gate to merge a tool's self-declared written paths (batch/refactor pattern
// mode carry no path args) into the touched-path set. Same defensive contract:
// any non-conforming Data shape yields nil.
func WrittenPathsFromResult(result ToolResult) []string {
	return writtenPathsFromResult(result)
}

// isWriteOperation returns true if the tool modifies files/state.
func isWriteOperation(toolName string) bool {
	return IsWriteTool(toolName)
}

// fileModifyingTools is the subset of writeTools that produce a specific
// file path we can surface in post-compaction hints (excludes bash/git_commit
// whose "written path" is opaque or irrelevant as a hint).
var fileModifyingTools = map[string]bool{
	"write":    true,
	"edit":     true,
	"copy":     true,
	"move":     true,
	"refactor": true,
}

// isFileModifyingTool returns true if the tool produces a named file path
// worth recording in the write tracker.
func isFileModifyingTool(toolName string) bool {
	return fileModifyingTools[toolName]
}

// getEmptyResultHint returns a diagnostic hint when a tool returns empty results.
// This helps the model understand what happened and suggest alternatives.
func getEmptyResultHint(toolName string) string {
	hints := map[string]string{
		"read": "The file is empty or could not be read. Check if the file exists and has content.",
		"grep": "No matches found. Try a simpler pattern, expand search scope, or check pattern syntax.",
		"glob": "No files match this pattern. Try a broader pattern like '**/*' or check the directory exists.",
		"bash": "Command produced no output. This may be expected (e.g., mkdir, cp succeed silently). Run with verbose flags (-v) for more details.",
	}
	if hint, ok := hints[toolName]; ok {
		return hint
	}
	return ""
}

// errorSuggestions maps common error patterns to helpful suggestions.
var errorSuggestions = []struct {
	pattern    string
	suggestion string
}{
	{"permission denied", "Try running with appropriate permissions or check file ownership."},
	{"no such file", "Use glob to find the correct path."},
	{"command not found", "Check if the tool is installed: which <command>."},
	{"connection refused", "Check if the service is running."},
	{"timed out", "Increase the timeout when the tool supports it, use run_tests for test suites, or choose another long-running mechanism advertised by the available tool schema."},
}

// getErrorSuggestion returns a suggestion for a given error message.
func getErrorSuggestion(errMsg string) string {
	lower := strings.ToLower(errMsg)
	for _, es := range errorSuggestions {
		if strings.Contains(lower, es.pattern) {
			return es.suggestion
		}
	}
	return ""
}

// readIntArg extracts a positional integer argument from a tool call, tolerating
// both int and float64 (JSON unmarshal picks float64 by default). Missing or
// unparseable values return 0. Used to fingerprint paged reads — "0" is a fine
// sentinel for "no offset specified".
func readIntArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// stagnationFingerprint returns a short string that distinguishes different
// invocations of the same tool. For example, writing 5 different files should
// produce 5 different fingerprints, while writing the same file 5 times
// produces the same fingerprint (true stagnation).
func stagnationFingerprint(toolName string, args map[string]any) string {
	// Extract the key distinguishing argument per tool
	switch toolName {
	case "read":
		// For read, a large file is legitimately paged in chunks — same
		// file_path but different offset/limit means forward progress, not
		// stagnation. Include the paging coordinates so the guard fires only
		// when the model truly requests the same range over and over.
		// Regression this fixes: reading ~3000-line tui.go in 2000-line
		// chunks tripped the 5-repeat abort when offset just kept growing.
		fp, ok := args["file_path"].(string)
		if !ok || fp == "" {
			// Missing required arg — match write/edit/delete fallback so
			// five bad reads in a row still trigger the stagnation abort.
			return ""
		}
		return fmt.Sprintf("%s@%d+%d",
			filepath.Base(fp),
			readIntArg(args, "offset"),
			readIntArg(args, "limit"))
	case "write", "delete":
		if fp, ok := args["file_path"].(string); ok {
			return filepath.Base(fp)
		}
	case "edit":
		fp, _ := args["file_path"].(string)
		if fp == "" {
			return ""
		}
		base := filepath.Base(fp)
		// Distinguish edits by what they target so DIFFERENT edits to the same
		// file aren't mistaken for a stuck retry loop — mirrors read's
		// offset+limit. Truly identical edits still collapse to one fingerprint
		// (same args → same key), so a model retrying the same failing edit is
		// still caught; only legitimate multi-region editing is let through.
		if old, _ := args["old_string"].(string); old != "" {
			h := sha256.Sum256([]byte(old))
			return fmt.Sprintf("%s@%x", base, h[:4])
		}
		if edits, ok := args["edits"].([]any); ok && len(edits) > 0 {
			h := sha256.Sum256([]byte(fmt.Sprintf("%v", edits)))
			return fmt.Sprintf("%s@edits:%x", base, h[:4])
		}
		if ls, le := readIntArg(args, "line_start"), readIntArg(args, "line_end"); ls > 0 || le > 0 {
			return fmt.Sprintf("%s@L%d-%d", base, ls, le)
		}
		if ia, ok := GetInt(args, "insert_after_line"); ok {
			return fmt.Sprintf("%s@ins%d", base, ia)
		}
		return base
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			// Strip leading "cd /path && " prefix — models often prepend this,
			// making all commands look identical in the first 40 chars.
			// Use first occurrence of " && " to split cd from actual command.
			if _, after, ok := strings.Cut(cmd, " && "); ok && strings.HasPrefix(strings.TrimSpace(cmd), "cd ") {
				cmd = after
			}
			// Truncate for display/key size, but keep the key EXACT with a
			// short hash of the full command (v0.100.91 field report): a bare
			// 60-rune prefix collapsed DIFFERENT commands sharing a long
			// prefix (`… && git status --short` vs `… && git status
			// --porcelain`) into one "identical" pattern, so five distinct
			// inspection variants tripped the 5-consecutive-repeats abort as
			// if the model had repeated one call. Distinct args must be
			// distinct keys — same philosophy as normalizeCallKey.
			if runes := []rune(cmd); len(runes) > 60 {
				h := sha256.Sum256([]byte(cmd))
				cmd = fmt.Sprintf("%s#%x", string(runes[:60]), h[:3])
			}
			return cmd
		}
	case "grep":
		// pattern alone isn't enough — same regex against different paths is
		// legitimate exploration, not stagnation. Include path so e.g.
		// grep("TODO", "internal/app") and grep("TODO", "internal/ui")
		// don't trip the consecutive-repeat abort.
		if p, ok := args["pattern"].(string); ok {
			path, _ := args["path"].(string)
			if path == "" {
				return p
			}
			return p + "@" + path
		}
	case "glob":
		// Same reasoning as grep — searching the same pattern in different
		// directories is forward progress, not a stuck loop.
		if p, ok := args["pattern"].(string); ok {
			path, _ := args["path"].(string)
			if path == "" {
				return p
			}
			return p + "@" + path
		}
	case "copy", "move":
		src, _ := args["source"].(string)
		dst, _ := args["destination"].(string)
		if src != "" {
			return filepath.Base(src) + "->" + filepath.Base(dst)
		}
	case "git_add":
		if p, ok := args["path"].(string); ok {
			return p
		}
	case "web_fetch":
		if u, ok := args["url"].(string); ok {
			if runes := []rune(u); len(runes) > 50 {
				u = string(runes[:50])
			}
			return u
		}
	case "web_search":
		if q, ok := args["query"].(string); ok {
			return q
		}
	case "run_tests", "verify_code":
		// Iterating tests/verification: same path with a different filter is
		// forward progress (e.g., narrowing from a full suite down to a
		// failing test). Without including filter, 5 consecutive `run_tests`
		// against the same package with progressively narrower filters tripped
		// the stagnation abort right when the user wanted to drill in.
		path, _ := args["path"].(string)
		filter, _ := args["filter"].(string)
		if path == "" && filter == "" {
			return ""
		}
		return path + "|" + filter
	case "list_dir", "tree", "git_status":
		// Local inspection over a PATH — listing/statting different
		// directories is exploration progress, not a loop (v0.100.95, same
		// class as the todo empty-fingerprint bug: a reasoning-heavy Kimi K3
		// turn that inspects 5 distinct dirs collapsed to one `list_dir:`
		// pattern). Distinct paths ⇒ distinct fingerprints.
		path, _ := args["path"].(string)
		if path == "" {
			path, _ = args["directory_path"].(string)
		}
		return path
	case "git_diff", "git_log", "git_blame", "review_changes":
		// Inspecting different files' diffs/history is progress. Path-less
		// (whole-repo) calls collapse to one target — a genuine repeat.
		file, _ := args["file"].(string)
		return file
	case "check_impact":
		// Blast-radius of DIFFERENT symbols is distinct work.
		if s, ok := args["symbol"].(string); ok {
			return s
		}
	case "go_search":
		// Different search queries are distinct exploration.
		if q, ok := args["query"].(string); ok {
			return q
		}
	case "go_to_definition", "find_references":
		// Navigating to different symbols/files is progress.
		file, _ := args["file"].(string)
		symbol, _ := args["symbol"].(string)
		if file == "" && symbol == "" {
			return ""
		}
		return file + "|" + symbol
	case "todo":
		// Updating the task list with DIFFERENT items each time is progress,
		// not a loop — the model checks off a task and adds the next. Hash the
		// serialized todos so only a truly-identical re-write collapses to one
		// fingerprint (field report: a Kimi K3 turn that narrates + updates its
		// checklist frequently tripped the 5-repeat abort with `todo:` — an
		// empty default fingerprint that treated every distinct update as the
		// same call). Mirrors edit's edits-array hash.
		if todos, ok := args["todos"]; ok {
			if encoded, err := json.Marshal(todos); err == nil {
				h := sha256.Sum256(encoded)
				return fmt.Sprintf("todos@%x", h[:4])
			}
		}
		return ""
	}
	// Default: hash the full args so a tool WITHOUT an explicit case above
	// still keys on its distinguishing arguments — DISTINCT calls of any such
	// tool are distinct fingerprints, only a truly-identical repeat collapses.
	// This is the general form of the per-tool cases and permanently closes
	// the empty-fingerprint false-abort class (v0.100.98: field reports hit it
	// three times — todo, the inspection tools, then memorize — each a tool a
	// reasoning-heavy model calls repeatedly with DIFFERENT args as progress,
	// e.g. saving several distinct memories, invoking several skills). The
	// explicit cases above still win (incl. run_tests/verify_code's
	// intentional "" for a no-args whole-suite repeat), so their tuned
	// semantics are unchanged. Args are JSON-sourced ⇒ json.Marshal is
	// deterministic (sorted keys); a marshal failure falls back to "".
	if len(args) == 0 {
		return ""
	}
	if encoded, err := json.Marshal(args); err == nil {
		h := sha256.Sum256(encoded)
		return fmt.Sprintf("args@%x", h[:4])
	}
	return ""
}

// maxStagnationRecoveryAttempts returns how many recovery hints a looping tool
// earns before the executor falls back to the hard abort. The recovery itself
// NEVER re-executes the tool — it returns a "stop looping" hint (plus the cached
// content for read-only tools) — so handing out hints is always side-effect free.
//   - read-only idempotent tools: 3. A re-read/re-search loop is benign and the
//     model almost always course-corrects once it gets the content back. This is
//     model-agnostic; it used to be Kimi-only, which silently hard-aborted every
//     other provider's entire turn — the bug this fixes.
//   - edit: 1, paired with a tailored "old_string isn't matching" hint. A model
//     retrying the same failing edit needs to be told WHY, not killed.
//   - everything else: 0 — repeating an identical mutating/opaque call is treated
//     as a genuine fault and aborts as before (conservative for side effects).
//
// isStagnationRecoverySafe reports whether a truly-identical repeat loop of
// this tool earns GRACEFUL recovery (hints + force-finalize) instead of a hard
// turn-kill. Recovery NEVER re-executes the tool — it only returns a hint — so
// the sole risk is a dishonest "done" claim after a half-applied MUTATION.
// Thus every side-effect-free tool (IsParallelSafeTool) plus the idempotent
// state/query tools (re-running is a no-op) are recovery-safe; genuine
// mutations (write/edit/delete/move/copy/bash/git_commit/…) keep the immediate
// hard abort. This is the general form that closes the recovery whack-a-mole
// (v0.100.99 — field reports hit it as read-only inspection, todo, memorize,
// then memory; each an idempotent tool a reasoning model loops on).
func isStagnationRecoverySafe(name string) bool {
	if IsParallelSafeTool(name) {
		return true
	}
	return idempotentStateTools[name]
}

// idempotentStateTools are the tools outside parallelSafeTools whose repeat is
// still a no-op — they write idempotently or spawn a read helper. Kept as a set
// rather than a switch so the phantom-name guard can see it: this list carried
// "go_diagnostics" for three releases, which is a code-intelligence RPC method
// the provider calls, never a registered tool, so the entry matched nothing and
// the list read as covering one case more than it did.
var idempotentStateTools = map[string]bool{
	"memory":       true,
	"memorize":     true,
	"skill":        true,
	"todo":         true,
	"check_impact": true,
	"mcp_admin":    true,
}

func maxStagnationRecoveryAttempts(toolName string) int {
	switch toolName {
	case "read", "grep", "glob", "list_dir", "tree":
		// The most benign reads earn one extra hint before force-finalize.
		return 3
	case "edit":
		// The ONE exception: hint-then-abort with NO force-finalize — forcing a
		// "final answer" after a half-applied edit invites a dishonest success
		// claim (see stagnationHintBudget's edit-only exclusion).
		return 1
	default:
		if isStagnationRecoverySafe(toolName) {
			return 2
		}
		return 0
	}
}

// stagnationFinalizeAttempts is the bounded force-finalize phase appended to a
// READ-ONLY pattern's hint budget: once the hints are spent, the executor stops
// coaching "take the next step" and instead cuts the repeated call off,
// demanding the final answer NOW. Only a model that ignores these too hits the
// hard abort — a pure inspection loop must end in an honest answer, not a dead
// turn (field report: a narrate-then-recheck `git status && git log` loop that
// ignored both hints killed the whole turn). Mutating/opaque patterns AND edit
// keep hint-then-abort unchanged: forcing a "final answer" after a failed
// mutation invites a dishonest success claim.
const stagnationFinalizeAttempts = 2

// stagnationHintBudget returns the batch's hint budget (the most restrictive
// maxStagnationRecoveryAttempts across all calls, with read-only bash earning
// 2) and whether the WHOLE batch is read-only inspection — the only class
// eligible for the force-finalize phase. ok=false means no recovery at all
// (the immediate hard abort, e.g. any mutating/unknown call in the batch).
func stagnationHintBudget(calls []*genai.FunctionCall) (hints int, readOnly bool, ok bool) {
	if len(calls) == 0 {
		return 0, false, false
	}
	readOnly = true
	hints = -1
	for _, call := range calls {
		if call == nil {
			return 0, false, false
		}
		m := maxStagnationRecoveryAttempts(call.Name)
		if m == 0 && call.Name == "bash" {
			// A read-only INSPECTION command (`git status && git diff --stat`
			// re-run while the model narrates) is the same benign loop class
			// as read/grep — killing the whole turn for it was
			// disproportionate (field report). Budget 2, slightly tighter
			// than read's 3 since bash is opaquer; mutating/unknown commands
			// keep the immediate abort.
			if cmd, _ := GetString(call.Args, "command"); readOnlyBashCommand(cmd) {
				m = 2
			}
		}
		if call.Name == "edit" {
			// edit is the ONLY recovery-eligible tool excluded from the
			// force-finalize phase: forcing a "final answer" after a failed
			// edit invites a dishonest success claim about a half-applied
			// mutation. todo is force-finalize eligible (v0.100.96 field
			// report round 2 — a model wrote the IDENTICAL todo list 5×,
			// ignored both hints, and hit the hard abort's scary error card):
			// a todo write is an idempotent no-op with no half-done state, so
			// "STOP re-planning, write your answer NOW" salvages the turn into
			// an honest response instead of a dead "Agent Got Stuck" error.
			readOnly = false
		}
		if m == 0 {
			return 0, false, false
		}
		if hints < 0 || m < hints {
			hints = m
		}
	}
	return hints, readOnly, true
}

// shouldAttemptStagnationRecovery reports whether the current looping batch
// should get another recovery replacement (hint or force-finalize) instead of
// aborting the turn. A mixed batch containing any non-recoverable tool aborts
// immediately; read-only batches get hints + the finalize phase.
func shouldAttemptStagnationRecovery(calls []*genai.FunctionCall, attempts int) bool {
	hints, readOnly, ok := stagnationHintBudget(calls)
	if !ok {
		return false
	}
	total := hints
	if readOnly {
		total += stagnationFinalizeAttempts
	}
	return attempts < total
}

// toolBudgetFamilies are the model families subject to the per-turn tool cap.
// Hosted providers share the quota-burning runaway mode this guard prevents.
// The YAML key stays `kimi_tool_budget` for backwards compatibility.
var toolBudgetFamilies = map[string]bool{
	"anthropic": true,
	"deepseek":  true,
	"glm":       true,
	"kimi":      true,
	"minimax":   true,
}

// shouldEnforceKimiToolBudget reports whether the per-turn tool budget cap
// has been reached for the currently active hosted model family. Local models
// and a disabled budget (0) always return false. `consumed` is the count of tool calls
// already executed in this turn before the current batch — callers pass
// len(toolsUsed)-len(newCalls) so the cap applies to the NEXT batch rather
// than trimming an in-flight one.
func (e *Executor) shouldEnforceKimiToolBudget(model string, consumed int) bool {
	if e == nil || e.kimiToolBudget <= 0 {
		return false
	}
	family := client.GetModelProfile(model).Family
	// Anthropic model IDs are not enumerated in the local model-profile table;
	// recognize their stable public prefix rather than silently leaving the
	// native hosted provider uncapped.
	if !toolBudgetFamilies[family] && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-") {
		return false
	}
	return consumed >= e.kimiToolBudget
}

// buildKimiToolBudgetExhaustedResults synthesizes error-result FunctionResponses
// for the current batch of calls, one per call, carrying an explicit
// "budget reached — wrap up" hint. The IDs are preserved so the model's
// next-turn expectation (each function-call must be paired with a response)
// is satisfied, avoiding a malformed conversation history.
// buildStagnationStopResults pairs a hopeless recovery-safe loop's final batch
// with synthetic results so no orphaned tool_use poisons history when the turn
// is gracefully stopped (v0.100.100). Only ever used for side-effect-free /
// idempotent batches, so blocking the calls is guaranteed harmless.
func (e *Executor) buildStagnationStopResults(calls []*genai.FunctionCall) []*genai.FunctionResponse {
	results := make([]*genai.FunctionResponse, len(calls))
	const msg = "Loop guard: this identical call was blocked and the turn is ending now. Do not call it again."
	for i, call := range calls {
		toolName := "tool"
		callID := ""
		if call != nil {
			toolName = call.Name
			callID = call.ID
		}
		results[i] = &genai.FunctionResponse{
			ID:       callID,
			Name:     toolName,
			Response: NewErrorResult(msg).ToMap(),
		}
	}
	return results
}

func (e *Executor) buildKimiToolBudgetExhaustedResults(calls []*genai.FunctionCall) []*genai.FunctionResponse {
	results := make([]*genai.FunctionResponse, len(calls))
	msg := buildKimiToolBudgetMessage(e.kimiToolBudget)
	for i, call := range calls {
		toolName := "tool"
		callID := ""
		if call != nil {
			toolName = call.Name
			callID = call.ID
		}
		results[i] = &genai.FunctionResponse{
			ID:       callID,
			Name:     toolName,
			Response: NewErrorResult(msg).ToMap(),
		}
	}
	return results
}

func buildKimiToolBudgetMessage(budget int) string {
	return fmt.Sprintf(
		"Tool budget guard: this turn already issued %d tool calls — the configured per-turn cap for this model. Stop calling tools and write the final answer now, citing what you've already found. Do not re-request the same tools.",
		budget,
	)
}

// synthesisNudgeThreshold is the tool-call count at which the nudge
// fires. Three tools is the sweet spot: enough signal to consolidate
// (Read + grep + another read, or two greps + a read), not so many
// that the drift is already irreversible.
const synthesisNudgeThreshold = 3

// shouldInjectSynthesisNudge gates the per-turn synthesis reminder.
// Only families where empirical drift-after-3-tool-calls was observed —
// kimi and deepseek. GLM-5/Opus-class models self-consolidate and a
// reminder regresses their behaviour; we don't nudge them. Threshold
// comparison uses >= so the nudge lands immediately after the 3rd tool
// result, before the model picks a 4th call.
func shouldInjectSynthesisNudge(model string, totalToolsThisTurn int) bool {
	if totalToolsThisTurn < synthesisNudgeThreshold {
		return false
	}
	return isNudgeEligibleFamily(model)
}

// isNudgeEligibleFamily reports whether the given model belongs to a
// family that benefits from gokin's explicit runtime nudges (synthesis
// checkpoint, intent announcement, error-recovery hints). The list
// grows as we characterise new providers: adding a model family here
// must come with evidence of the specific drift the nudge prevents.
func isNudgeEligibleFamily(model string) bool {
	family := client.GetModelProfile(model).Family
	return family == "kimi" || family == "deepseek"
}

func buildSynthesisNudgeMessage(toolsCount int) string {
	return fmt.Sprintf(
		"Consolidation checkpoint: you've issued %d tool calls this turn without writing a final answer. Before any more tools, pause and write 2-3 concise lines: (1) Established — concrete facts you've verified, (2) Unknown — open questions the tools couldn't answer, (3) Next — the single next step. If Established already covers the user's request, STOP and write the final answer instead.",
		toolsCount,
	)
}

// intentNudgeMinPlanChars is the threshold for considering the
// assistant's pre-tool text "enough of a plan". 30 chars is one short
// sentence — low bar, but anything below it is almost certainly empty
// or a filler "Ok." / "Sure.". Tuned low on purpose so well-behaved
// models don't trip the nudge.
const intentNudgeMinPlanChars = 30

// intentNudgeMessage is the system reminder queued when the model skips
// the plan phase. Wording mirrors the Kimi operating-rules addendum so
// the model sees the same vocabulary in-prompt and in-runtime.
const intentNudgeMessage = "Plan-then-act reminder: your last response jumped to tool calls without a one-line plan. Before your next set of tool calls, write a single line starting with 'Plan:' that states the concrete objective (3-7 words), so the user can tell what you're about to do. Skip the plan only for a single trivial read/grep with no follow-up."

// shouldInjectIntentNudge decides whether to queue the announcement
// reminder. Fires only when:
//   - no tools have been executed yet in this turn (toolsExecuted == 0)
//     — covers the "first real response" case even when outer-loop
//     retries inflated the iteration counter
//   - the model produced tool calls (otherwise there's nothing to plan)
//   - the response text before the tool calls is under the threshold
//     (≈ no plan written)
//   - the model family is Kimi (Strong-tier models self-narrate)
func shouldInjectIntentNudge(toolsExecuted int, model string, resp *client.Response) bool {
	if toolsExecuted > 0 || resp == nil {
		return false
	}
	if len(resp.FunctionCalls) == 0 {
		return false
	}
	if !isNudgeEligibleFamily(model) {
		return false
	}
	// Count both the visible text and the thinking trace — thinking
	// counts as "plan visible to runtime" even when it doesn't show in
	// UI, since the model itself processed it.
	planLen := len(strings.TrimSpace(resp.Text)) + len(strings.TrimSpace(resp.Thinking))
	return planLen < intentNudgeMinPlanChars
}

const todoNudgeMinToolCalls = 2

const todoNudgeMessage = "Todo reminder: this has become multi-step coding work. Before continuing, call todo with the remaining plan and keep exactly one item in_progress, so the user can see progress like Claude Code."

func shouldInjectTodoNudge(model string, toolsUsed []string) bool {
	if len(toolsUsed) < todoNudgeMinToolCalls {
		return false
	}
	if !isNudgeEligibleFamily(model) {
		return false
	}

	hasMutation := false
	for _, toolName := range toolsUsed {
		switch toolName {
		case "todo":
			return false
		case "write", "edit", "move", "copy", "delete", "mkdir", "batch", "refactor":
			hasMutation = true
		}
	}
	return hasMutation
}

func (e *Executor) buildStagnationRecoveryResults(calls []*genai.FunctionCall, repeatCount int, finalize bool) []*genai.FunctionResponse {
	results := make([]*genai.FunctionResponse, len(calls))
	for i, call := range calls {
		toolName := "tool"
		callID := ""
		args := map[string]any{}
		if call != nil {
			toolName = call.Name
			callID = call.ID
			if call.Args != nil {
				args = call.Args
			}
		}

		msg := buildStagnationRecoveryMessage(toolName, args, repeatCount)
		if finalize {
			msg = buildStagnationFinalizeMessage(toolName, args, repeatCount)
		}
		result := NewErrorResult(msg)
		if cached, ok := e.lookupCachedReadOnlyResult(toolName, args); ok && cached.Content != "" {
			result = NewErrorResultWithContext(msg, cached.Content)
		}

		results[i] = &genai.FunctionResponse{
			ID:       callID,
			Name:     toolName,
			Response: result.ToMap(),
		}
	}
	return results
}

func (e *Executor) lookupCachedReadOnlyResult(toolName string, args map[string]any) (ToolResult, bool) {
	if e.toolCache == nil || toolName == "" {
		return ToolResult{}, false
	}
	return e.toolCache.Get(toolName, args)
}

// buildStagnationRecoveryMessage builds the hint returned in place of the
// looping tool call. It is tool-class aware so the model gets actionable advice
// for the specific way it's stuck — not a one-size-fits-all "stop" — while every
// branch keeps the literal "Do not call it again" phrase the loop-break path and
// its tests rely on.
func buildStagnationRecoveryMessage(toolName string, args map[string]any, repeatCount int) string {
	target := describeStagnationTarget(toolName, args)
	switch toolName {
	case "read":
		return fmt.Sprintf(
			"Loop guard: identical %s repeated %d times. Do not call it again — you already have this file's content in the conversation above; reuse it. To see other code, change offset/limit or read a different file. If you were about to edit, make the edit now.",
			target, repeatCount,
		)
	case "grep", "glob", "list_dir", "tree":
		return fmt.Sprintf(
			"Loop guard: identical %s repeated %d times. Do not call it again — the results are already above; reuse them. Re-running the same search will not help: try a different pattern or path, or proceed with what you found.",
			target, repeatCount,
		)
	case "edit":
		if old, _ := args["old_string"].(string); old != "" {
			return fmt.Sprintf(
				"Loop guard: the same %s was attempted %d times and keeps failing. Do not call it again unchanged — old_string matching is exact and whitespace-sensitive, so it is not matching the file. Read the current file to copy the exact text, then retry; or switch to line_start/line_end or regex mode.",
				target, repeatCount,
			)
		}
		return fmt.Sprintf(
			"Loop guard: the same %s was attempted %d times and is not making progress. Do not call it again unchanged — re-read the current file to recheck the exact line numbers and content, then retry with corrected coordinates or take a different approach.",
			target, repeatCount,
		)
	case "bash":
		return fmt.Sprintf(
			"Loop guard: the identical %s ran %d times in a row. Its output will not change until something else changes. Do not call it again — use the result you already have and take the NEXT step (make the edit, run the fix, or answer the user).",
			target, repeatCount,
		)
	case "todo":
		return fmt.Sprintf(
			"Loop guard: the task list was written %d times unchanged. Do not call todo again — the list is already set. STOP re-planning and DO the next task: run the tool (edit/bash/read) that actually advances the in-progress item, then update todo only after real work happens.",
			repeatCount,
		)
	default:
		return fmt.Sprintf(
			"Loop guard: identical %s request repeated %d times. This exact tool call already ran and repeating it will not make progress. Do not call it again. Reuse the earlier result, choose a different target, or answer the user.",
			target, repeatCount,
		)
	}
}

// buildStagnationFinalizeMessage is the post-hint escalation for read-only
// inspection loops: the hints were ignored, so stop coaching "take the next
// step" and demand the final answer. Keeps the literal "Do not call it again"
// phrase (the loop-break contract shared with the hint path) and is honest
// about the consequence of persisting.
func buildStagnationFinalizeMessage(toolName string, args map[string]any, repeatCount int) string {
	target := describeStagnationTarget(toolName, args)
	return fmt.Sprintf(
		"Loop guard FINAL: the identical %s has been blocked — it ran %d+ times and its output will not change. Do not call it again. STOP running tools NOW and write your complete final answer for the user from what you already learned this turn. If this identical call is issued again, the turn will be terminated.",
		target, repeatCount,
	)
}

func buildStagnationWarningMessage(calls []*genai.FunctionCall, repeatCount int, finalize bool) string {
	if finalize {
		target := "the repeated tool call"
		if len(calls) > 0 && calls[0] != nil {
			target = describeStagnationTarget(calls[0].Name, calls[0].Args)
		}
		return fmt.Sprintf(
			"Loop guard: %s ignored the loop hints — cut it off and asked for the final answer.",
			target,
		)
	}
	if len(calls) == 0 || calls[0] == nil {
		return fmt.Sprintf(
			"Loop guard: repeated the same tool pattern %d times. Sent a recovery hint instead of rerunning it.",
			repeatCount,
		)
	}
	if len(calls) == 1 {
		return fmt.Sprintf(
			"Loop guard: repeated %s %d times. Sent a recovery hint instead of rerunning it.",
			describeStagnationTarget(calls[0].Name, calls[0].Args),
			repeatCount,
		)
	}
	return fmt.Sprintf(
		"Loop guard: repeated the same %s pattern %d times. Sent a recovery hint instead of rerunning it.",
		describeStagnationTarget(calls[0].Name, calls[0].Args),
		repeatCount,
	)
}

func describeStagnationTarget(toolName string, args map[string]any) string {
	switch toolName {
	case "read":
		filePath, _ := args["file_path"].(string)
		target := "read"
		if filePath != "" {
			target += " " + filepath.Base(filePath)
		}
		offset := readIntArg(args, "offset")
		limit := readIntArg(args, "limit")
		if offset > 0 || limit > 0 {
			target = fmt.Sprintf("%s (offset %d, limit %d)", target, offset, limit)
		}
		return target
	case "grep":
		pattern, _ := args["pattern"].(string)
		target := "grep"
		if pattern != "" {
			target += fmt.Sprintf(" %q", pattern)
		}
		if path, ok := args["path"].(string); ok && path != "" {
			target += " in " + path
		} else if glob, ok := args["glob"].(string); ok && glob != "" {
			target += " matching " + glob
		}
		return target
	case "glob":
		pattern, _ := args["pattern"].(string)
		target := "glob"
		if pattern != "" {
			target += fmt.Sprintf(" %q", pattern)
		}
		if path, ok := args["path"].(string); ok && path != "" {
			target += " in " + path
		}
		return target
	case "list_dir", "tree":
		path, _ := args["path"].(string)
		if path == "" {
			return toolName
		}
		return fmt.Sprintf("%s %s", toolName, path)
	default:
		if fp := stagnationFingerprint(toolName, args); fp != "" {
			return fmt.Sprintf("%s (%s)", toolName, fp)
		}
		return toolName
	}
}

func (e *Executor) buildModelWorkingMemoryNotification(model string, calls []*genai.FunctionCall, results []*genai.FunctionResponse) string {
	if !shouldInjectKimiWorkingMemory(model, calls, results) {
		return ""
	}

	limit := min(len(calls), len(results))
	if limit == 0 {
		return ""
	}

	items := make([]string, 0, limit)
	for i := 0; i < limit && len(items) < 3; i++ {
		call := calls[i]
		result := results[i]
		if call == nil || result == nil {
			continue
		}

		target := describeStagnationTarget(call.Name, call.Args)
		summary := e.summarizeWorkingMemoryResult(call.Name, result)
		if target == "" || summary == "" {
			continue
		}
		items = append(items, trimInlineText(fmt.Sprintf("%s -> %s", target, summary), 240))
	}
	if len(items) == 0 {
		return ""
	}

	return "[Kimi working memory] Established: " +
		strings.Join(items, "; ") +
		". Before more tools: reuse these results, do not repeat the same read/grep/glob without a new hypothesis, prefer grep -> targeted read, and after 1-3 tool calls synthesize what is established and what remains unknown."
}

func shouldInjectKimiWorkingMemory(model string, calls []*genai.FunctionCall, results []*genai.FunctionResponse) bool {
	// Retained name for stability of log markers, but now eligibility
	// extends to any nudge-eligible family — DeepSeek benefits from the
	// same "don't re-explore, synthesise" reminder as Kimi.
	if !isNudgeEligibleFamily(model) {
		return false
	}
	if len(calls) == 0 || len(results) == 0 {
		return false
	}
	if len(calls) > 1 {
		return true
	}
	call := calls[0]
	if call == nil {
		return false
	}
	if isKimiExplorationTool(call.Name) {
		return true
	}
	return workingMemoryResponseSize(results[0]) >= 700
}

func buildKimiToolErrorRecoveryNotification(model string, results []*genai.FunctionResponse) string {
	// Name retained for log marker stability; eligibility now includes
	// deepseek so V4 users get the same concrete recovery steps Kimi
	// users do when read-before-edit / delta-check / ambiguity errors
	// fire. The message text is generic — not family-specific.
	if !isNudgeEligibleFamily(model) {
		return ""
	}
	for _, result := range results {
		if result == nil {
			continue
		}
		success, _ := result.Response["success"].(bool)
		if success {
			continue
		}
		errMsg, _ := result.Response["error"].(string)
		lower := strings.ToLower(strings.TrimSpace(errMsg))
		if lower == "" {
			continue
		}
		switch {
		case strings.Contains(lower, "read-before-edit"):
			return "[Kimi recovery] read-before-edit blocked the mutation. Next call must be read(file_path=...) for the exact file, then retry one edit using exact surrounding context. Do not issue another mutation first."
		case strings.Contains(lower, "delta-check guard") || strings.Contains(lower, "delta-check failed"):
			return "[Kimi recovery] delta-check failed after a mutation. Fix the reported build/typecheck/lint error before any unrelated edit, then rerun the same verification."
		case strings.Contains(lower, "old_string not found") ||
			(strings.Contains(lower, "ambiguous") && strings.Contains(lower, "old_string")) ||
			strings.Contains(lower, "fuzzy match"):
			return "[Kimi recovery] edit matching failed. Re-read the target region if needed, then retry with 3-5 exact surrounding lines in old_string or use line_start/line_end when the tool suggested it."
		}
	}
	return ""
}

func appendNotificationToFunctionResults(results []*genai.FunctionResponse, notification string) {
	notification = strings.TrimSpace(notification)
	if len(results) == 0 || notification == "" {
		return
	}

	result := results[len(results)-1]
	if result == nil {
		return
	}
	if result.Response == nil {
		result.Response = map[string]any{}
	}

	if errMsg, ok := result.Response["error"].(string); ok && strings.TrimSpace(errMsg) != "" {
		result.Response["error"] = appendTextBlock(errMsg, notification)
		return
	}
	if content, ok := result.Response["content"].(string); ok && strings.TrimSpace(content) != "" {
		result.Response["content"] = appendTextBlock(notification, content)
		return
	}
	result.Response["content"] = notification
}

func appendTextBlock(base, addition string) string {
	base = strings.TrimRight(base, "\n")
	addition = strings.TrimSpace(addition)
	if base == "" {
		return addition
	}
	if addition == "" {
		return base
	}
	return base + "\n\n" + addition
}

func isKimiExplorationTool(toolName string) bool {
	switch toolName {
	case "read", "grep", "glob", "list_dir", "tree":
		return true
	default:
		return false
	}
}

func workingMemoryResponseSize(result *genai.FunctionResponse) int {
	if result == nil {
		return 0
	}
	content, _ := result.Response["content"].(string)
	errMsg, _ := result.Response["error"].(string)
	return len(content) + len(errMsg)
}

func (e *Executor) summarizeWorkingMemoryResult(toolName string, result *genai.FunctionResponse) string {
	if result == nil {
		return ""
	}

	errMsg, _ := result.Response["error"].(string)
	if trimmedErr := strings.TrimSpace(errMsg); trimmedErr != "" {
		return trimInlineText("error: "+trimmedErr, 180)
	}

	content, _ := result.Response["content"].(string)
	if trimmedContent := strings.TrimSpace(content); trimmedContent != "" {
		if summarizer, ok := e.compactor.(resultSummaryProvider); ok {
			if summary := strings.TrimSpace(summarizer.SummarizeForPrune(toolName, trimmedContent)); summary != "" {
				return trimInlineText(summary, 180)
			}
		}
		return trimInlineText(trimmedContent, 180)
	}

	success, _ := result.Response["success"].(bool)
	if success {
		return "success"
	}
	return "no output"
}

func trimInlineText(text string, max int) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	runes := []rune(text)
	if max <= 0 || len(runes) <= max {
		return text
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

// adaptiveToolTimeout returns the effective timeout for a single tool call
// given the executor's baseline and observed p95 latency.
//
// Semantics:
//   - If no stats (ok=false) or non-positive p95: return base unchanged.
//   - Otherwise take max(base, 5×p95) so fast tools still fail fast while
//     historically-slow tools (e.g., a 5-minute build step) get room.
//   - Cap the result at 2×base so a genuinely hung tool can't run unbounded
//     even when its historical p95 was huge.
func adaptiveToolTimeout(base, p95 time.Duration, ok bool) time.Duration {
	// A non-positive base would make every result (and the base×2 cap) zero,
	// yielding an already-expired context. Fall back to a sane default so the
	// helper can never hand back a 0 timeout. The executor also clamps at
	// construction; this guards any other caller.
	if base <= 0 {
		base = defaultToolExecTimeout
	}
	if !ok || p95 <= 0 {
		return base
	}
	timeout := base
	if adaptive := p95 * 5; adaptive > timeout {
		timeout = adaptive
	}
	if ceiling := base * 2; timeout > ceiling {
		timeout = ceiling
	}
	return timeout
}

const toolTimeoutCompletionGrace = 5 * time.Second

// toolExecutionTimeout layers an explicit long-operation budget over the
// adaptive generic timeout. Verification tools own longer workload budgets
// because workspace compilation routinely exceeds the generic tool budget.
// Bash recognizes verification commands and gives them run_tests-sized
// headroom unless timeout_seconds is explicitly requested. The small common
// grace lets each inner tool deadline classify partial output before the
// executor cancels the call from outside.
func toolExecutionTimeout(base, p95 time.Duration, haveStats bool, toolName string, args map[string]any) time.Duration {
	timeout := adaptiveToolTimeout(base, p95, haveStats)

	var requested time.Duration
	switch toolName {
	case "run_tests":
		requested = DefaultRunTestsTimeout
		if seconds, ok := GetInt(args, "timeout_seconds"); ok {
			requested = time.Duration(seconds) * time.Second
		}
	case "verify_code":
		requested = DefaultVerifyCodeTimeout
	case "coordinate":
		// Coordinate performs bounded teardown after its own wait expires.
		// Reserve that cleanup inside the executor budget, then the common
		// completion grace below keeps the two deadlines from racing.
		requested = coordinateTimeout(args) + coordinateCleanupTimeout
	case "task":
		requested = taskForegroundTimeout(args)
	case "repl_exec":
		// Python computation has its own short inactivity watchdog. The wider
		// outer budget exists for synchronous rlm() callbacks, whose delegated
		// agent owns a long-running but cancellable task lifecycle.
		requested = config.DefaultThoroughAgentTimeout
	case "task_output":
		if GetBoolDefault(args, "block", false) {
			requested = taskOutputWaitTimeout(args)
		}
	case "ssh":
		requested = sshExecutionTimeout(args)
	case "ask_user":
		requested = 10 * time.Minute
	case "enter_plan_mode":
		requested = 10 * time.Minute
	case "write", "edit":
		requested = 5 * time.Minute
	case "bash":
		requested = base
		if seconds, ok := GetInt(args, "timeout_seconds"); ok {
			requested = time.Duration(seconds) * time.Second
		} else if commandLooksLikeValidation(GetStringDefault(args, "command", "")) &&
			requested < DefaultRunTestsTimeout {
			requested = DefaultRunTestsTimeout
		}
	}
	if requested > 0 && requested+toolTimeoutCompletionGrace > timeout {
		return requested + toolTimeoutCompletionGrace
	}
	return timeout
}
