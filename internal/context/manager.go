package context

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/logging"

	"google.golang.org/genai"
)

// Sentinel errors returned by ForceSummarize / OptimizeContext to let the
// caller distinguish "nothing to do" from a real failure. CompactCommand
// uses these to render specific user-facing messages (the prior behavior
// was a silent success that looked like the command did nothing).
var (
	// ErrSummarizerUnavailable: ContextManager has no summarizer wired (e.g.
	// summarization disabled in config, or provider didn't expose one).
	ErrSummarizerUnavailable = errors.New("summarizer not configured")
	// ErrHistoryTooShort: history has fewer messages than the strategy's
	// MinMessagesForSummary threshold. /compact is a no-op in this case —
	// summarizing a short history would lose more than it saves.
	ErrHistoryTooShort = errors.New("history too short to summarize")
	// ErrNothingToSummarize: the summary strategy produced an empty plan
	// (e.g., all messages are pinned or already-summarized markers).
	ErrNothingToSummarize = errors.New("no summarizable messages found")
	// ErrSummarizationInProgress prevents /compact from starting a second
	// provider call while an automatic compaction is already in flight.
	ErrSummarizationInProgress = errors.New("context summarization already in progress")
)

// tokenSnapshot records token usage at a point in time for trend prediction.
type tokenSnapshot struct {
	tokens    int
	timestamp time.Time
}

// ContextManager orchestrates context management including token counting,
// auto-summarization, and optimization.
type ContextManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	session      *chat.Session
	tokenCounter *TokenCounter
	summarizer   *Summarizer
	config       *config.ContextConfig
	client       client.Client

	mu            sync.RWMutex
	currentTokens int
	lastUsage     *TokenUsage
	updateVersion uint64 // Any usage/history update; prevents stale watcher writes.
	// sessionCountVersion advances only for Session change callbacks. Keeping it
	// separate from updateVersion prevents an exact ObserveAPIUsage update from
	// being mistaken for another history-count request and then overwritten by
	// a second local estimate.
	sessionCountVersion uint64

	// Provider usage is the only exact GLM token measurement available on the
	// Anthropic-compatible endpoint. Keep the newest prompt/completion split and
	// bind it to the post-turn history snapshot so a subsequent local
	// CountTokens estimate cannot immediately overwrite an exact measurement.
	observedAPIGeneration   uint64
	appliedAPIGeneration    uint64
	observedAPIInput        int
	observedAPIOutput       int
	observedOutputEstimated bool
	authoritativeHistoryKey string

	// Async token counting
	lastEstimatedTokens int // Cached estimate for fast path
	lastHistoryLen      int // History length at last count
	// sessionCountRunning coalesces bursty Session change callbacks into one
	// latest-only worker. It is separate from tokenCountSem: the bool prevents
	// stale queued work, while the semaphore bounds provider calls shared with
	// PrepareForRequest's precise counter.
	sessionCountRunning atomic.Bool

	// Background summarization
	summarizing    atomic.Bool   // Whether summarization is in progress (lock-free)
	summarizeDone  chan struct{} // Signal when summarization completes
	lastSummaryDur time.Duration // Duration of last summarization

	// New components
	metrics            *ContextMetrics
	summaryCache       *SummaryCache
	messageScorer      *MessageScorer
	summaryStrategy    SummaryStrategy
	responseCompressor *ResponseCompressor
	keyFiles           map[string]bool // Files critical to the session, always preserved
	tokenHistory       []tokenSnapshot // Token usage history for trend prediction

	// Semaphore to limit concurrent async token count goroutines
	tokenCountSem chan struct{}

	// modelRoundTimeout is the hard cap inherited by LLM-backed compaction.
	// Guarded by mu and updated live by /timeout via App.
	modelRoundTimeout time.Duration

	// Plan manager for task-aware summarization
	planManager PlanManagerProvider

	// Notification callback when context is compacted
	OnCompact func(oldTokens, newTokens, removedMessages int, reason string)

	// Notification callback when background optimization starts (for UI feedback)
	OnOptimizeStart func(reason string)

	// Notification callback when a background compaction attempt FAILS.
	// Every trigger path (backgroundOptimize's OptimizeContext call AND
	// tryAutoCompact's IncrementalCompact call) previously only Warn-logged a
	// failure — invisible to the user. On a quota-limited provider (GLM's 5h
	// Coding-Plan cap) that means repeated background summarization attempts
	// can silently keep burning quota while context keeps growing toward
	// EmergencyTruncate/overflow, with zero on-screen signal that anything is
	// wrong. Guard fires at most once per attempt (not a retry loop itself).
	OnOptimizeFailed func(reason string, err error)
}

// NewContextManager creates a new context manager.
func NewContextManager(
	ctx context.Context,
	session *chat.Session,
	c client.Client,
	cfg *config.ContextConfig,
) *ContextManager {
	ctx, cancel := context.WithCancel(ctx)
	tokenCounter := NewTokenCounter(c, c.GetModel(), cfg)

	var summarizer *Summarizer
	if cfg != nil && cfg.EnableAutoSummary {
		summarizer = NewSummarizer(c)
	}

	// Initialize new components
	metrics := NewContextMetrics()
	summaryCache := NewSummaryCache(100, 30*time.Minute)
	messageScorer := NewMessageScorer()
	summaryStrategy := DefaultSummaryStrategy()

	// Configure response compressor
	maxToolChars := 10000
	if cfg != nil && cfg.ToolResultMaxChars > 0 {
		maxToolChars = cfg.ToolResultMaxChars
	}
	responseCompressor := NewResponseCompressor(maxToolChars)

	// Customize strategy from config
	if cfg != nil {
		if cfg.SummarizationRatio > 0 {
			summaryStrategy.TargetRatio = cfg.SummarizationRatio
		}
		if cfg.MaxInputTokens > 0 {
			summaryStrategy.MaxHistorySize = cfg.MaxInputTokens / 2000 // Rough estimate
		}
	}

	// Enable semantic scoring when auto-summary is available (requires LLM)
	if cfg != nil && cfg.EnableAutoSummary && c != nil {
		messageScorer.SetSemanticClient(c)
	}

	return &ContextManager{
		ctx:                ctx,
		cancel:             cancel,
		session:            session,
		tokenCounter:       tokenCounter,
		summarizer:         summarizer,
		config:             cfg,
		client:             c,
		metrics:            metrics,
		summaryCache:       summaryCache,
		messageScorer:      messageScorer,
		summaryStrategy:    summaryStrategy,
		responseCompressor: responseCompressor,
		keyFiles:           make(map[string]bool),
		tokenHistory:       make([]tokenSnapshot, 0, 20),
		tokenCountSem:      make(chan struct{}, 3),
		modelRoundTimeout:  config.DefaultModelRoundTimeout,
	}
}

// SetPlanManager sets the plan manager for task-aware summarization.
func (m *ContextManager) SetPlanManager(pm PlanManagerProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.planManager = pm
}

// syncTaskContext extracts current task info from the plan manager
// and updates the summarizer's task context.
func (m *ContextManager) syncTaskContext() {
	if m.summarizer == nil {
		return
	}
	m.mu.RLock()
	pm := m.planManager
	m.mu.RUnlock()

	if pm == nil {
		m.summarizer.SetTaskContext(nil)
		return
	}

	contract := pm.GetActiveContractContext()
	if contract == "" {
		m.summarizer.SetTaskContext(nil)
		return
	}

	// Parse the contract text into structured TaskContext. The real generator
	// (plan/executor.go GetActiveContractContext) emits "Current Step Contract:"
	// followed by "- Step N: Title" and "- Success criteria: a; b" — NOT the
	// "Current step:" shape the old cases looked for, so CurrentStep/
	// SuccessCriteria were always empty (dead branches). Track the contract
	// section and parse the emitted lines.
	tc := &TaskContext{}
	inContract := false
	for line := range strings.SplitSeq(contract, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "**Plan:**"):
			tc.Title = strings.TrimSpace(strings.TrimPrefix(line, "**Plan:**"))
		case strings.HasPrefix(line, "Plan:"):
			tc.Title = strings.TrimSpace(strings.TrimPrefix(line, "Plan:"))
		case strings.HasPrefix(line, "**Goal:**"):
			tc.Description = strings.TrimSpace(strings.TrimPrefix(line, "**Goal:**"))
		case strings.HasPrefix(line, "Goal:"):
			tc.Description = strings.TrimSpace(strings.TrimPrefix(line, "Goal:"))
		case strings.HasPrefix(line, "Current Step Contract:"):
			inContract = true
		case inContract && strings.HasPrefix(line, "- Step "):
			tc.CurrentStep = strings.TrimPrefix(line, "- ")
		case inContract && strings.HasPrefix(line, "- Success criteria: "):
			for _, c := range strings.Split(strings.TrimPrefix(line, "- Success criteria: "), "; ") {
				if c = strings.TrimSpace(c); c != "" {
					tc.SuccessCriteria = append(tc.SuccessCriteria, c)
				}
			}
		// Legacy shapes kept as harmless compat.
		case strings.HasPrefix(line, "**Current step:**"):
			tc.CurrentStep = strings.TrimSpace(strings.TrimPrefix(line, "**Current step:**"))
		case strings.HasPrefix(line, "Current step:"):
			tc.CurrentStep = strings.TrimSpace(strings.TrimPrefix(line, "Current step:"))
		}
	}

	if tc.Title == "" && tc.Description == "" {
		// Fallback: use the raw contract text as description
		tc.Description = contract
		if runes := []rune(tc.Description); len(runes) > 500 {
			tc.Description = string(runes[:500]) + "..."
		}
	}

	m.summarizer.SetTaskContext(tc)
}

// SetClient updates the underlying client for token counting and summarization.
func (m *ContextManager) SetClient(c client.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.client = c
	m.tokenCounter.SetClient(c)
	if m.summarizer != nil {
		m.summarizer.SetClient(c)
	}
	if m.messageScorer != nil {
		m.messageScorer.SetSemanticClient(c)
	}
}

// SetConfig updates the context configuration.
func (m *ContextManager) SetConfig(cfg *config.ContextConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg
	m.tokenCounter.SetConfig(cfg)
	if m.summarizer != nil {
		m.summarizer.SetConfig(cfg)
	}
}

// SetModelRoundTimeout updates the hard cap for future LLM-backed compaction
// calls. In-flight calls retain the deadline they started with.
func (m *ContextManager) SetModelRoundTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = config.DefaultModelRoundTimeout
	}
	m.mu.Lock()
	m.modelRoundTimeout = timeout
	messageScorer := m.messageScorer
	m.mu.Unlock()
	if messageScorer != nil {
		messageScorer.SetSemanticTimeout(timeout)
	}
}

// ModelRoundTimeout returns the effective compaction model-round cap.
func (m *ContextManager) ModelRoundTimeout() time.Duration {
	m.mu.RLock()
	timeout := m.modelRoundTimeout
	m.mu.RUnlock()
	if timeout <= 0 {
		return config.DefaultModelRoundTimeout
	}
	return timeout
}

// StartSessionWatcher starts monitoring session changes for auto-updating token counts.
func (m *ContextManager) StartSessionWatcher() {
	m.session.SetChangeHandler(m.onSessionChange)
}

// onSessionChange is called when session history changes.
func (m *ContextManager) onSessionChange(event chat.ChangeEvent) {
	// Invalidate token cache when history changes
	m.tokenCounter.InvalidateCache()

	// Track key files from new messages
	if event.NewCount > event.OldCount {
		m.trackKeyFiles(event)
	}

	// Reserve a unique version, then wake the latest-only counter. A new
	// goroutine per event used to bypass tokenCountSem entirely and could queue
	// dozens of stale provider calls during a burst of tool/history updates.
	m.mu.Lock()
	m.updateVersion++
	m.sessionCountVersion++
	m.mu.Unlock()
	m.startSessionTokenCountWorker()
}

func (m *ContextManager) startSessionTokenCountWorker() {
	if !m.sessionCountRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Error("panic in session token-count worker", "error", r)
				m.sessionCountRunning.Store(false)
				// An event may have arrived while the flag was still true. Retry
				// from the authoritative latest snapshot after releasing ownership.
				if m.ctx.Err() == nil {
					m.startSessionTokenCountWorker()
				}
			}
		}()
		m.runSessionTokenCountWorker()
	}()
}

func (m *ContextManager) runSessionTokenCountWorker() {
	for {
		// Read history before the versions. If a mutation lands between the two,
		// its callback advances sessionCountVersion and this pass is retried.
		history := m.session.GetHistory()
		m.mu.RLock()
		applyVersion := m.updateVersion
		requestVersion := m.sessionCountVersion
		m.mu.RUnlock()

		if m.tokenCountSem != nil {
			select {
			case m.tokenCountSem <- struct{}{}:
			case <-m.ctx.Done():
				m.sessionCountRunning.Store(false)
				return
			}
		}
		var tokens int
		var isEstimate bool
		var err error
		func() {
			if m.tokenCountSem != nil {
				defer func() { <-m.tokenCountSem }()
			}
			countCtx, cancelCount := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancelCount()
			tokens, isEstimate, err = m.tokenCounter.CountContentsWithAccuracy(countCtx, history)
		}()
		if m.ctx.Err() != nil {
			m.sessionCountRunning.Store(false)
			return
		}
		if err != nil {
			tokens = EstimateContentsTokens(history)
			isEstimate = true
			logging.Debug("failed to count tokens on session change, using estimate", "error", err)
		}

		latest := false
		m.mu.Lock()
		if m.updateVersion == applyVersion && m.sessionCountVersion == requestVersion {
			m.currentTokens = tokens
			usage := m.tokenCounter.GetUsage(tokens)
			usage.IsEstimate = isEstimate
			m.lastUsage = &usage
			latest = true
		}
		currentRequestVersion := m.sessionCountVersion
		m.mu.Unlock()

		if latest {
			m.startAutoCompact(tokens)
		}
		if currentRequestVersion != requestVersion {
			continue // Coalesce every intermediate event into the newest snapshot.
		}

		// Lost-wakeup-safe handoff: publish idle, then recheck the version. An
		// event racing before Store(false) sees us running; we reclaim ownership.
		// An event racing after it starts its own worker and our CAS fails.
		m.sessionCountRunning.Store(false)
		m.mu.RLock()
		newer := m.sessionCountVersion != requestVersion
		m.mu.RUnlock()
		if newer && m.sessionCountRunning.CompareAndSwap(false, true) {
			continue
		}
		return
	}
}

func (m *ContextManager) startAutoCompact(currentTokens int) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Error("panic in session auto-compaction worker", "error", r)
			}
		}()
		// Auto-compaction is an independent model round. Do not inherit the
		// token-count helper's 30-second deadline: that silently overrode the
		// live tools.model_round_timeout and made this one compaction path fail
		// far earlier than /compact and the other background triggers.
		m.tryAutoCompact(m.ctx, currentTokens)
	}()
}

// PrepareForRequest prepares the context before sending a request.
// Uses fast estimation for immediate decisions; precise count runs async.
func (m *ContextManager) PrepareForRequest(ctx context.Context) error {
	startTime := time.Now()

	history := m.session.GetHistory()

	// Compress large function responses
	history = m.responseCompressor.CompressContents(history)

	// Fast path: use estimation if history delta is small (< 5 messages since last count)
	m.mu.RLock()
	historyDelta := len(history) - m.lastHistoryLen
	cachedTokens := m.lastEstimatedTokens
	m.mu.RUnlock()

	var tokens int
	isEstimate := false

	if cachedTokens > 0 && historyDelta >= 0 && historyDelta < 5 {
		// Use cached estimate + delta estimation for speed
		tokens = cachedTokens + EstimateContentsTokens(history[len(history)-max(historyDelta, 0):])
		isEstimate = true
		m.metrics.RecordEstimation()
	} else {
		// Need full count - use estimate first, then async precise count
		tokens = EstimateContentsTokens(history)
		isEstimate = true
		m.metrics.RecordEstimation()
	}

	m.mu.Lock()
	m.updateVersion++
	countVersion := m.updateVersion
	m.currentTokens = tokens
	m.lastEstimatedTokens = tokens
	m.lastHistoryLen = len(history)
	usage := m.tokenCounter.GetUsage(tokens)
	usage.IsEstimate = isEstimate
	m.lastUsage = &usage
	m.tokenHistory = append(m.tokenHistory, tokenSnapshot{tokens: tokens, timestamp: time.Now()})
	if len(m.tokenHistory) > 20 {
		m.tokenHistory = m.tokenHistory[len(m.tokenHistory)-20:]
	}
	m.mu.Unlock()

	// Emergency: if estimated tokens EXCEED limit, truncate synchronously (no LLM needed)
	if usage.ExceedsLimit {
		logging.Warn("context exceeds token limit, performing emergency truncation",
			"tokens", tokens, "limit", usage.MaxTokens)
		m.EmergencyTruncate()
		// Re-estimate after truncation
		history = m.session.GetHistory()
		tokens = EstimateContentsTokens(history)
		m.mu.Lock()
		m.currentTokens = tokens
		m.lastEstimatedTokens = tokens
		m.lastHistoryLen = len(history)
		usage = m.tokenCounter.GetUsage(tokens)
		usage.IsEstimate = true
		m.lastUsage = &usage
		m.mu.Unlock()
	}

	// Launch async precise token count in background (bounded by semaphore)
	select {
	case m.tokenCountSem <- struct{}{}:
		go func() {
			defer func() { <-m.tokenCountSem }()
			defer func() {
				if r := recover(); r != nil {
					logging.Error("panic in async token count", "error", r)
				}
			}()

			asyncCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()

			precise, isEstimate, err := m.tokenCounter.CountContentsWithAccuracy(asyncCtx, history)
			if err != nil {
				return // Keep using estimate
			}
			m.metrics.RecordAPICount()

			m.mu.Lock()
			if m.updateVersion == countVersion {
				m.lastEstimatedTokens = precise
				m.currentTokens = precise
				u := m.tokenCounter.GetUsage(precise)
				u.IsEstimate = isEstimate
				m.lastUsage = &u
			}
			m.mu.Unlock()
		}()
	default:
		// Semaphore full, skip async count — use estimate
	}

	// Record metrics
	m.metrics.RecordPrepare(time.Since(startTime), tokens)

	// Optimize if near limit — launch in background to avoid blocking
	if usage.NearLimit && m.summarizer != nil && m.config.EnableAutoSummary {
		m.notifyOptimizeStart("context near token limit")
		m.backgroundOptimize(ctx)
	}

	// Predictive: check if next request will exceed limit based on trend
	if !usage.NearLimit && m.summarizer != nil && m.config.EnableAutoSummary {
		if predicted := m.predictTokensAfterRequest(); predicted > 0 {
			predUsage := m.tokenCounter.GetUsage(predicted)
			if predUsage.PercentUsed > 0.85 {
				logging.Info("predictive summarization triggered",
					"current_tokens", tokens,
					"predicted_tokens", predicted,
					"predicted_pct", predUsage.PercentUsed)
				m.notifyOptimizeStart("predictive — approaching token limit")
				m.backgroundOptimize(ctx)
			}
		}
	}

	// Proactive summarization when approaching message limit (sliding window)
	if m.summarizer != nil && m.config.EnableAutoSummary {
		maxHistory := config.DefaultMaxSessionHistory
		if historyLen := len(history); historyLen > maxHistory*3/4 {
			logging.Info("message count summarization triggered",
				"history_len", historyLen,
				"max_history", maxHistory)
			m.notifyOptimizeStart("conversation history growing large")
			m.backgroundOptimize(ctx)
		}
	}

	return nil
}

// notifyOptimizeStart fires the OnOptimizeStart callback if registered.
func (m *ContextManager) notifyOptimizeStart(reason string) {
	if m.OnOptimizeStart != nil {
		m.OnOptimizeStart(reason)
	}
}

// notifyOptimizeFailed fires the OnOptimizeFailed callback if registered.
func (m *ContextManager) notifyOptimizeFailed(reason string, err error) {
	if m.OnOptimizeFailed != nil {
		m.OnOptimizeFailed(reason, err)
	}
}

// backgroundOptimize runs context optimization in a background goroutine.
// Only one optimization runs at a time; concurrent calls are no-ops.
func (m *ContextManager) backgroundOptimize(_ context.Context) {
	finish, started := m.beginSummarization()
	if !started {
		return // Already running
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Error("panic in background optimization", "error", r)
			}
			finish()
		}()

		// Use the manager's lifecycle context, not the request context.
		// The request context is cancelled when the API call returns, but
		// background summarization may still be running — using the request
		// ctx causes it to abort prematurely on fast responses.
		sumCtx, cancel := m.compactionContext(m.ctx)
		defer cancel()

		start := time.Now()
		if err := m.OptimizeContext(sumCtx); err != nil {
			logging.Warn("background context optimization failed", "error", err)
			// ErrHistoryTooShort/ErrNothingToSummarize are normal "nothing to
			// do" outcomes (the same sentinel-error convention /compact uses),
			// not failures worth alarming the user about.
			if !errors.Is(err, ErrHistoryTooShort) && !errors.Is(err, ErrNothingToSummarize) {
				m.notifyOptimizeFailed("context optimization", err)
			}
		}
		m.mu.Lock()
		m.lastSummaryDur = time.Since(start)
		m.mu.Unlock()
	}()
}

// beginSummarization serializes every LLM-backed compaction path. Session
// changes can arrive in bursts, and each change starts an asynchronous token
// count; without one shared gate they could all cross the threshold together
// and spend several provider calls summarizing the same history. The same gate
// is also used by backgroundOptimize so incremental and full compaction cannot
// overlap.
func (m *ContextManager) beginSummarization() (finish func(), started bool) {
	if !m.summarizing.CompareAndSwap(false, true) {
		return nil, false
	}

	done := make(chan struct{})
	m.mu.Lock()
	m.summarizeDone = done
	m.mu.Unlock()

	return func() {
		close(done)
		m.summarizing.Store(false)
	}, true
}

// compactionBudget is the default summarization model-round cap. Runtime
// /timeout changes are read from ContextManager.modelRoundTimeout instead.
const compactionBudget = config.DefaultModelRoundTimeout

// boundedCompactionCtx caps a summarization context at compactionBudget so a
// slow/unreachable model endpoint can't stall a caller for minutes: SendMessage
// is otherwise bounded only by the ~120s transport ResponseHeaderTimeout (the
// shared HTTP client has no Client.Timeout, by design, to preserve SSE
// streaming). Deadline-aware: a tighter parent deadline is preserved, never
// extended. This closes the
// /compact sibling of the v0.100.8 login-hang — ForceSummarize reached
// OptimizeContext on the deadline-less command ctx.
func boundedCompactionCtxWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = compactionBudget
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= timeout {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, timeout, client.NewModelRoundTimeoutError(timeout))
}

func boundedCompactionCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return boundedCompactionCtxWithTimeout(ctx, compactionBudget)
}

func (m *ContextManager) compactionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return boundedCompactionCtxWithTimeout(ctx, m.ModelRoundTimeout())
}

// OptimizeContext optimizes the context by summarizing old messages.
func (m *ContextManager) OptimizeContext(ctx context.Context) error {
	ctx, cancel := m.compactionContext(ctx)
	defer cancel()

	startTime := time.Now()

	// Snapshot strategy under read-lock — SetSummaryStrategy is concurrent
	// write under m.mu.Lock(), so direct field reads here would race
	// (caught by `go test -race` if a strategy swap lands mid-compact).
	m.mu.RLock()
	strategy := m.summaryStrategy
	m.mu.RUnlock()

	history, historyVersion := m.session.GetHistoryWithVersion()
	if len(history) <= strategy.MinMessagesForSummary {
		// Not enough messages to summarize. Surface this so /compact can
		// tell the user "you only have N messages, threshold is M" instead
		// of silently succeeding with zero tokens freed.
		return ErrHistoryTooShort
	}

	// Sync task context for task-aware summarization
	m.syncTaskContext()

	// Create summary plan using strategy
	plan := CreateSummaryPlanWithContext(ctx, history, strategy, m.messageScorer)

	if len(plan.ToSummarize) == 0 {
		// All messages are pinned or already-summarized. Surface so the
		// caller can explain the no-op rather than reporting bogus success.
		return ErrNothingToSummarize
	}

	// Check cache first
	messageHash := HashMessages(plan.ToSummarize)
	cachedSummary, found := m.summaryCache.Get(messageHash)

	var summary *genai.Content
	var fromCache bool

	if found {
		// Use cached summary
		summary = cachedSummary.Summary
		fromCache = true
		logging.Debug("using cached summary", "tokens", cachedSummary.TokenCount)
		originalTokens := EstimateContentsTokens(plan.ToSummarize)
		m.metrics.RecordSummary(0, cachedSummary.TokenCount, originalTokens, true)
	} else {
		// Generate new summary
		startSummary := time.Now()
		var err error
		summary, err = m.summarizer.Summarize(ctx, plan.ToSummarize)
		if err != nil {
			return err
		}

		// Count summary tokens
		summaryTokens, err := m.tokenCounter.CountContents(ctx, []*genai.Content{summary})
		if err != nil {
			summaryTokens = EstimateContentsTokens([]*genai.Content{summary})
		}

		// Cache the summary
		originalTokens := EstimateContentsTokens(plan.ToSummarize)
		m.summaryCache.Put(messageHash, summary, summaryTokens, 0, 0)

		// Record metrics
		m.metrics.RecordSummary(time.Since(startSummary), summaryTokens, originalTokens, false)
		fromCache = false
	}

	// Apply the summary plan. Commit with optimistic concurrency: this runs in a
	// BACKGROUND goroutine (backgroundOptimize) from the history snapshot captured
	// above, while the foreground turn may append its response + tool results and
	// commit them via SetHistory. An unconditional SetHistory here could land
	// AFTER the turn's write and silently overwrite the just-finished turn with a
	// pre-turn compacted snapshot (a lost update — both hold s.mu, so -race won't
	// catch it). Skip on version mismatch; the next turn re-triggers compaction
	// against the fresh history.
	newHistory := ApplySummaryPlan(plan, summary)
	committedHistory, committed := m.session.SetHistoryIfVersionWithSkillCarry(
		newHistory, historyVersion, len(plan.KeepStart)+1,
	)
	if !committed {
		logging.Debug("context: skipped compaction commit — history changed during summarization (retries next turn)")
		return nil
	}
	newHistory = committedHistory

	// Recount tokens
	tokens, isEstimate, err := m.tokenCounter.CountContentsWithAccuracy(ctx, newHistory)
	if err != nil {
		tokens = EstimateContentsTokens(newHistory)
		isEstimate = true
	}

	m.mu.Lock()
	m.updateVersion++
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	usage.IsEstimate = isEstimate
	m.lastUsage = &usage
	m.mu.Unlock()

	// Record optimization metrics
	oldTokens := EstimateContentsTokens(history)
	m.metrics.RecordOptimize(time.Since(startTime), oldTokens, tokens)

	logging.Info("context optimized",
		"before", oldTokens,
		"after", tokens,
		"saved", oldTokens-tokens,
		"from_cache", fromCache)

	if m.OnCompact != nil {
		m.OnCompact(oldTokens, tokens, len(history)-len(newHistory), "Full Strategy Compaction")
	}

	return nil
}

// GetContextHealth returns a snapshot of context health.
func (m *ContextManager) GetContextHealth() (int, int, float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	total := m.currentTokens
	max := 0
	if m.tokenCounter != nil {
		max = m.tokenCounter.GetLimits().MaxInputTokens
	}

	percent := 0.0
	if max > 0 {
		percent = float64(total) / float64(max)
	}

	return total, max, percent
}

// GetTokenUsage returns the current token usage.
func (m *ContextManager) GetTokenUsage() *TokenUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.lastUsage == nil {
		return &TokenUsage{}
	}
	usage := *m.lastUsage
	return &usage
}

// GetCurrentTokens returns the current token count.
func (m *ContextManager) GetCurrentTokens() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentTokens
}

// UpdateTokenCount updates the token count after a request.
func (m *ContextManager) UpdateTokenCount(ctx context.Context) error {
	history := m.session.GetHistory()
	historyKey := m.tokenCounter.hashContents(history)

	tokens, isEstimate, err := m.tokenCounter.CountContentsWithAccuracy(ctx, history)
	if err != nil {
		tokens = EstimateContentsTokens(history)
		isEstimate = true
	}

	m.mu.Lock()
	m.updateVersion++ // Invalidate any in-flight watcher updates
	usage := m.tokenCounter.GetUsage(tokens)
	usage.IsEstimate = isEstimate
	if isEstimate && m.observedAPIGeneration > m.appliedAPIGeneration {
		// The provider measured the request that produced the history snapshot
		// we have just committed. Preserve its exact prompt/completion split.
		m.appliedAPIGeneration = m.observedAPIGeneration
		m.authoritativeHistoryKey = historyKey
		usage = m.usageWithContext(m.observedAPIInput, m.observedAPIOutput, m.observedOutputEstimated)
	} else if isEstimate && historyKey != "" && historyKey == m.authoritativeHistoryKey {
		// Repeated UI/config refresh of the same history must stay stable and
		// exact instead of flickering back to the character heuristic.
		usage = m.usageWithContext(m.observedAPIInput, m.observedAPIOutput, m.observedOutputEstimated)
	}
	m.currentTokens = usage.InputTokens
	m.lastUsage = &usage
	m.mu.Unlock()

	return nil
}

// usageWithContext builds a TokenUsage whose PercentUsed / NearLimit /
// ExceedsLimit are computed from the FULL live context — prompt PLUS the
// completion that follows it (v0.100.108 field report: the bar read the
// last request's prompt only, so a reasoning-heavy K3 answer of tens of K
// tokens was invisible until the NEXT request, and the near-limit warning /
// auto-compaction trigger lagged by exactly that much). InputTokens keeps the
// raw prompt figure for billing/stats consumers.
func (m *ContextManager) usageWithContext(in, out int, isEstimate bool) TokenUsage {
	u := m.tokenCounter.GetUsage(in + out)
	u.InputTokens = in
	u.OutputTokens = out
	u.IsEstimate = isEstimate
	return u
}

// ObserveAPIUsage records prompt usage reported by the provider for an actual
// request. It is the highest-quality measurement available and supersedes any
// in-flight local count for an older snapshot.
func (m *ContextManager) ObserveAPIUsage(inputTokens int, outputTokens ...int) {
	if inputTokens <= 0 {
		return
	}
	output := 0
	if len(outputTokens) > 0 {
		output = max(outputTokens[0], 0)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateVersion++
	m.observedAPIGeneration++
	m.observedAPIInput = inputTokens
	m.observedAPIOutput = output
	m.observedOutputEstimated = false
	m.currentTokens = inputTokens
	usage := m.usageWithContext(inputTokens, output, false)
	m.lastUsage = &usage
}

// ObserveOutputEstimate attaches the live character-based completion estimate
// to the latest exact provider prompt measurement. Providers normally replace
// it with final output_tokens; if one omits that terminal field, the UI keeps
// the useful tail but truthfully retains the ≈ marker.
func (m *ContextManager) ObserveOutputEstimate(outputTokens int) {
	if outputTokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.observedAPIInput <= 0 {
		return
	}
	m.observedAPIOutput = outputTokens
	m.observedOutputEstimated = true
	usage := m.usageWithContext(m.observedAPIInput, outputTokens, true)
	m.lastUsage = &usage
}

// NeedsSummarization checks if the context needs summarization.
func (m *ContextManager) NeedsSummarization() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.lastUsage == nil {
		return false
	}
	return m.lastUsage.NearLimit
}

// GetTokenCounter returns the underlying token counter.
func (m *ContextManager) GetTokenCounter() *TokenCounter {
	return m.tokenCounter
}

// GetMetrics returns the context metrics.
func (m *ContextManager) GetMetrics() *ContextMetrics {
	return m.metrics
}

// GetCacheStats returns summary cache statistics.
func (m *ContextManager) GetCacheStats() CacheStats {
	return m.summaryCache.GetStats()
}

// SetSummaryStrategy sets a new summarization strategy.
func (m *ContextManager) SetSummaryStrategy(strategy SummaryStrategy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaryStrategy = strategy
}

// GetSummaryStrategy returns the current summarization strategy.
func (m *ContextManager) GetSummaryStrategy() SummaryStrategy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.summaryStrategy
}

// predictTokensAfterRequest estimates token count after the next request
// based on the trend of recent token snapshots.
func (m *ContextManager) predictTokensAfterRequest() int {
	m.mu.RLock()
	history := make([]tokenSnapshot, len(m.tokenHistory))
	copy(history, m.tokenHistory)
	m.mu.RUnlock()

	if len(history) < 3 {
		return 0 // Not enough data to predict
	}

	// Calculate average token growth per request
	totalGrowth := 0
	count := 0
	for i := 1; i < len(history); i++ {
		growth := history[i].tokens - history[i-1].tokens
		if growth > 0 {
			totalGrowth += growth
			count++
		}
	}

	if count == 0 {
		return 0
	}

	avgGrowth := totalGrowth / count
	current := history[len(history)-1].tokens

	// Predict: current + average growth for next request
	return current + avgGrowth
}

// ForceSummarize forces context summarization regardless of token count.
func (m *ContextManager) ForceSummarize(ctx context.Context) error {
	if m.summarizer == nil {
		return ErrSummarizerUnavailable
	}
	started, err := m.tryOptimizeContext(ctx)
	if !started {
		return ErrSummarizationInProgress
	}
	return err
}

// trackKeyFiles extracts file paths from session changes to track critical files.
func (m *ContextManager) trackKeyFiles(event chat.ChangeEvent) {
	history := m.session.GetHistory()
	if len(history) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Scan newly added messages (from OldCount to NewCount)
	start := max(event.OldCount, 0)
	if start >= len(history) {
		return
	}

	for i := start; i < len(history); i++ {
		content := history[i]
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				if path, ok := part.FunctionCall.Args["path"].(string); ok {
					m.keyFiles[path] = true
				}
				if path, ok := part.FunctionCall.Args["file_path"].(string); ok {
					m.keyFiles[path] = true
				}
			}
		}
	}
}

// tryAutoCompact triggers incremental compaction when tokens exceed the configured threshold.
func (m *ContextManager) tryAutoCompact(ctx context.Context, currentTokens int) {
	if m.summarizer == nil || m.config == nil || !m.config.EnableAutoSummary {
		return
	}

	threshold := m.config.AutoCompactThreshold
	if threshold <= 0 {
		threshold = 0.75 // default
	}

	usage := m.tokenCounter.GetUsage(currentTokens)
	if usage.PercentUsed < threshold {
		return
	}

	finish, started := m.beginSummarization()
	if !started {
		logging.Debug("auto-compaction skipped — summarization already running")
		return
	}
	defer finish()

	logging.Info("auto-compaction triggered",
		"tokens", currentTokens,
		"percentage", usage.PercentUsed,
		"threshold", threshold)

	// This is the ONE compaction trigger (of four: two PrepareForRequest
	// paths, message-count, and this one) that never called
	// notifyOptimizeStart — its start, and any failure, were completely
	// invisible: no toast, no chrome, Warn-only log. Every LLM-backed
	// summarization call here still costs real tokens against a
	// possibly-quota-limited provider even when it fails.
	m.notifyOptimizeStart("auto-compact — context over threshold")

	if err := m.IncrementalCompact(ctx); err != nil {
		logging.Warn("auto-compaction failed", "error", err)
		m.notifyOptimizeFailed("auto-compact", err)
	}
}

// tryOptimizeContext runs a full compaction under the shared summarization
// gate. It is used by synchronous callers (manual /compact and ContextAgent);
// backgroundOptimize owns the same gate across its goroutine lifecycle.
func (m *ContextManager) tryOptimizeContext(ctx context.Context) (bool, error) {
	finish, started := m.beginSummarization()
	if !started {
		return false, nil
	}
	defer finish()
	return true, m.OptimizeContext(ctx)
}

// IncrementalCompact performs incremental compaction: summarizes oldest messages first,
// preserves recent messages and key file references.
func (m *ContextManager) IncrementalCompact(ctx context.Context) error {
	// Incremental compaction is an LLM model round just like OptimizeContext.
	// Bound direct callers too; tryAutoCompact previously inherited an unrelated
	// 30-second token-count deadline while other callers could be unbounded.
	ctx, cancel := m.compactionContext(ctx)
	defer cancel()

	// Sync task context for task-aware summarization
	m.syncTaskContext()

	// Snapshot strategy under read-lock — see OptimizeContext for rationale.
	m.mu.RLock()
	strategy := m.summaryStrategy
	m.mu.RUnlock()

	history, historyVersion := m.session.GetHistoryWithVersion()

	// Preserve last 50 messages in full fidelity
	preserveCount := min(50, len(history))

	if len(history) <= preserveCount {
		return nil // Nothing old enough to compact
	}

	// Split: old messages to summarize, recent to preserve
	// Adjust boundary so FunctionCall/FunctionResponse pairs are not split
	splitPoint := AdjustBoundaryForToolPairs(history, len(history)-preserveCount)
	oldMessages := history[:splitPoint]
	recentMessages := history[splitPoint:]

	if len(oldMessages) < strategy.MinMessagesForSummary {
		return nil
	}

	// Summarize old messages
	summary, err := m.summarizer.Summarize(ctx, oldMessages)
	if err != nil {
		return fmt.Errorf("incremental compaction failed: %w", err)
	}

	// Build new history: summary + recent
	newHistory := make([]*genai.Content, 0, 1+len(recentMessages))
	newHistory = append(newHistory, summary)
	newHistory = append(newHistory, recentMessages...)

	// Optimistic-concurrency commit (see OptimizeContext): this runs in a
	// background goroutine (async token-count → tryAutoCompact) built from the
	// snapshot above, concurrent with the foreground turn's own SetHistory. Skip
	// on version mismatch so a stale compaction can't wipe the just-finished turn.
	committedHistory, committed := m.session.SetHistoryIfVersionWithSkillCarry(newHistory, historyVersion, 1)
	if !committed {
		logging.Debug("context: skipped incremental compaction commit — history changed during summarization (retries next turn)")
		return nil
	}
	newHistory = committedHistory

	// Update token count
	tokens, isEstimate, err := m.tokenCounter.CountContentsWithAccuracy(ctx, newHistory)
	if err != nil {
		tokens = EstimateContentsTokens(newHistory)
		isEstimate = true
	}

	m.mu.Lock()
	m.updateVersion++
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	usage.IsEstimate = isEstimate
	m.lastUsage = &usage
	m.mu.Unlock()

	oldTokens := EstimateContentsTokens(history)
	logging.Info("incremental compaction done",
		"before", oldTokens,
		"after", tokens,
		"messages_summarized", len(oldMessages),
		"messages_preserved", len(recentMessages))

	if m.OnCompact != nil {
		m.OnCompact(oldTokens, tokens, len(history)-len(newHistory), "Incremental Compaction")
	}

	return nil
}

// EmergencyTruncate performs hard truncation of history when token count exceeds the model limit.
// Unlike summarization, this is synchronous and guaranteed to fit within budget.
//
// Strategy: keep first 2 messages (system context) + a recent tail (continuity)
// + the highest-IMPORTANCE messages rescued from the older region that the
// recent-tail cut would otherwise drop wholesale. Pre-v0.86.6 this was
// recency-only — an old-but-critical tool result or error was discarded purely
// for being old, even though the budget had room. Now a slice of the budget is
// reserved to rescue scored middle messages (errors/edits/tool results rank
// highest via MessageScorer), so the survivor set is "recent + important", not
// just "recent". Mirrors the agent's forceCompactViaTruncation intent while
// keeping the emergency path's budget guarantee (it fires precisely because we
// blew the limit, so it must produce a result that fits).
func (m *ContextManager) EmergencyTruncate() int {
	history := m.session.GetHistory()
	if len(history) <= 4 {
		return 0
	}

	targetTokens := int(float64(m.tokenCounter.GetLimits().MaxInputTokens) * 0.7)

	// Nothing to do if the whole conversation already fits the budget. (Counts
	// preserved too, since those messages are also kept against the budget.)
	if EstimateContentsTokens(history) <= targetTokens {
		return 0
	}

	// Always preserve first 2 messages (system/greeting) and build from the end.
	preserved := history[:2]
	tail := history[2:]

	// Budget split: the recent tail gets the bulk (continuity matters most for
	// resuming the task); reserve ~25% of the remaining budget to rescue the
	// most important older messages that recency alone would drop.
	budget := max(0, targetTokens-EstimateContentsTokens(preserved))
	rescueBudget := budget / 4
	recencyBudget := budget - rescueBudget

	// Scan from newest to oldest within the recency budget, accumulating until
	// the next message would overflow it.
	tailTokens := 0
	cutoff := len(tail)
	for i := len(tail) - 1; i >= 0; i-- {
		msgTokens := EstimateContentsTokens([]*genai.Content{tail[i]})
		if tailTokens+msgTokens > recencyBudget {
			cutoff = i + 1
			break
		}
		tailTokens += msgTokens
	}

	// Adjust boundary so FunctionCall/FunctionResponse pairs are not split
	cutoff = AdjustBoundaryForToolPairs(tail, cutoff)

	recencyTail := tail[cutoff:]
	dropped := tail[:cutoff]

	// Rescue the highest-importance messages from the dropped region within the
	// reserved budget, returned in chronological order.
	rescued := m.selectImportantWithinBudget(dropped, rescueBudget)

	// Re-grounding: emergency truncation keeps only history[:2] + the rescued
	// middle + the recent tail, so the original task (~history[2]) and the
	// working file-set may get dropped — the model then drifts off-task. Splice
	// a concise re-grounding note where the task used to sit so it survives.
	regrounding := m.buildRegroundingNote(history)

	newHistory := make([]*genai.Content, 0, len(preserved)+1+len(rescued)+len(recencyTail))
	newHistory = append(newHistory, preserved...)
	if regrounding != nil {
		newHistory = append(newHistory, regrounding)
	}
	newHistory = append(newHistory, rescued...)
	newHistory = append(newHistory, recencyTail...)

	// Rescued messages are non-contiguous picks, so a rescued FunctionResponse
	// whose FunctionCall wasn't rescued (or vice versa) would be orphaned —
	// strict providers 400 on that. Prune orphaned tool parts as a final pass.
	newHistory = pruneOrphanedToolParts(newHistory)

	carryAt := len(preserved)
	if regrounding != nil {
		carryAt++
	}
	newHistory = m.session.SetHistoryWithSkillCarry(newHistory, carryAt)

	// Update cached token count — save old value BEFORE overwriting for the callback
	tokens := EstimateContentsTokens(newHistory)
	m.mu.Lock()
	oldTokensEstimate := m.lastEstimatedTokens
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	m.lastUsage = &usage
	m.lastEstimatedTokens = tokens
	m.lastHistoryLen = len(newHistory)
	m.mu.Unlock()

	removed := max(0, len(history)-len(newHistory))
	logging.Warn("emergency truncation performed",
		"removed", removed,
		"before", len(history),
		"after", len(newHistory),
		"estimated_tokens", tokens)

	if m.OnCompact != nil {
		m.OnCompact(oldTokensEstimate, tokens, removed, "Emergency Truncation (Limits hit)")
	}

	return removed
}

// buildRegroundingNote synthesizes a compact reminder of the original task and
// the working file-set, injected after a hard truncation so the model keeps the
// thread. Returns nil when no task message is found (nothing useful to say).
func (m *ContextManager) buildRegroundingNote(history []*genai.Content) *genai.Content {
	// Original task = the first non-empty user message in the conversation.
	task := ""
	for _, c := range history {
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && strings.TrimSpace(p.Text) != "" {
				task = p.Text
				break
			}
		}
		if task != "" {
			break
		}
	}
	if task == "" {
		return nil
	}
	if r := []rune(task); len(r) > 600 {
		task = string(r[:600]) + "…"
	}

	var sb strings.Builder
	sb.WriteString("[Context was truncated to fit the model's limit — re-grounding so you keep the thread.]\n\n")
	sb.WriteString("Original task: ")
	sb.WriteString(task)

	if files := m.GetKeyFiles(); len(files) > 0 {
		sort.Strings(files)
		if len(files) > 12 {
			files = files[:12]
		}
		sb.WriteString("\n\nKey files touched this session: ")
		sb.WriteString(strings.Join(files, ", "))
	}
	sb.WriteString("\n\nContinue from where you left off. Re-read a file if you need its current contents.")

	return genai.NewContentFromText(sb.String(), genai.RoleUser)
}

// selectImportantWithinBudget ranks the given messages by importance
// (MessageScorer: errors/system → Critical, edits → High, verbose reads → Low)
// and greedily keeps the highest-scored ones whose token cost fits the budget,
// returning them in chronological order so the spliced history stays ordered.
// Used by EmergencyTruncate to rescue valuable older messages instead of
// dropping the whole middle by age. Returns nil when there's no budget, no
// scorer, or no messages.
func (m *ContextManager) selectImportantWithinBudget(msgs []*genai.Content, budget int) []*genai.Content {
	if budget <= 0 || len(msgs) == 0 || m.messageScorer == nil {
		return nil
	}

	type scoredMsg struct {
		idx    int
		score  float64
		tokens int
		msg    *genai.Content
	}

	items := make([]scoredMsg, 0, len(msgs))
	for i, msg := range msgs {
		if msg == nil {
			continue
		}
		s := m.messageScorer.ScoreMessage(msg)
		items = append(items, scoredMsg{
			idx:    i,
			score:  float64(s.Priority) + s.Score,
			tokens: EstimateContentsTokens([]*genai.Content{msg}),
			msg:    msg,
		})
	}

	// Highest score first; stable on original index for ties.
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].score != items[b].score {
			return items[a].score > items[b].score
		}
		return items[a].idx < items[b].idx
	})

	picked := make([]scoredMsg, 0, len(items))
	used := 0
	for _, it := range items {
		if used+it.tokens > budget {
			continue // try a smaller one — a single huge message shouldn't block the rest
		}
		used += it.tokens
		picked = append(picked, it)
	}

	// Restore chronological order for splicing.
	sort.SliceStable(picked, func(a, b int) bool {
		return picked[a].idx < picked[b].idx
	})

	out := make([]*genai.Content, 0, len(picked))
	for _, it := range picked {
		out = append(out, it.msg)
	}
	return out
}

// Close cancels the lifecycle context, stopping any background goroutines.
func (m *ContextManager) Close() {
	m.cancel()
}

// GetKeyFiles returns the set of files tracked as critical to the session.
func (m *ContextManager) GetKeyFiles() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	files := make([]string, 0, len(m.keyFiles))
	for f := range m.keyFiles {
		files = append(files, f)
	}
	return files
}
