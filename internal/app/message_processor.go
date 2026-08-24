package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gokin/internal/agent"
	"gokin/internal/client"
	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/donegate"
	"gokin/internal/hybrid"
	"gokin/internal/logging"
	"gokin/internal/plan"
	"gokin/internal/router"
	"gokin/internal/tools"
	"gokin/internal/ui"

	"google.golang.org/genai"
)

const (
	// planStepOutputMaxChars is the max characters stored per step output.
	planStepOutputMaxChars = 8000
	// planStepVerifyTimeout bounds orchestrator-run verify_commands per step.
	planStepVerifyTimeout = 2 * time.Minute
	// planSummaryMaxChars is the max characters for previous steps summary context.
	planSummaryMaxChars = 2000
	// messageIdleTimeout is the minimum time without any model activity
	// (text, tool calls, thinking) before we cancel message processing.
	// Unlike a wall-clock timeout, this survives system sleep/wake cycles
	// because the heartbeat freezes during sleep and resumes on wake. The live
	// budget grows beyond a raised model-round cap via messageIdleBudget.
	messageIdleTimeout = config.DefaultModelWatchdogFloor
	// idleCheckInterval is how often we check for idle timeout.
	idleCheckInterval = 30 * time.Second
)

// startMessageIdleWatchdog cancels a foreground turn after prolonged absence
// of model/tool activity. The headless outcome is latched before cancellation:
// cancellation cleanup intentionally clears lastError, so reversing this order
// would make an internally timed-out automation run report false success.
func (a *App) startMessageIdleWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	headlessTurn *headlessTerminalOutcome,
) {
	a.safeGo("idle-timeout-watcher", func() {
		ticker := time.NewTicker(idleCheckInterval)
		defer ticker.Stop()
		a.watchMessageIdle(ctx, cancel, headlessTurn, a.messageIdleBudget(), ticker.C)
	})
}

func (a *App) messageIdleBudget() time.Duration {
	modelRound := config.DefaultModelRoundTimeout
	if a != nil && a.executor != nil {
		modelRound = a.executor.ModelRoundTimeout()
	} else if a != nil && a.config != nil && a.config.Tools.ModelRoundTimeout > 0 {
		modelRound = a.config.Tools.ModelRoundTimeout
	}
	return config.ModelWatchdogTimeout(modelRound)
}

// watchMessageIdle contains the deterministic watchdog state machine. Keeping
// the tick source injectable at this internal boundary avoids timing-heavy
// tests while production still uses a normal time.Ticker.
func (a *App) watchMessageIdle(
	ctx context.Context,
	cancel context.CancelFunc,
	headlessTurn *headlessTerminalOutcome,
	timeout time.Duration,
	ticks <-chan time.Time,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if age := a.stepHeartbeatAge(); age > timeout {
				logging.Warn("message processing idle timeout",
					"idle", age.Round(time.Second).String())
				a.recordHeadlessTerminalOutcomeForTurn(
					headlessTurn,
					"timeout",
					fmt.Sprintf("message processing idle timeout: no model activity for %v", age.Round(time.Second)),
				)
				cancel()
				return
			}
		}
	}
}

// isHeadlessTimeoutFailure recognizes every terminal timeout category emitted
// by the model/executor stack. Explicit cancellation is deliberately excluded:
// an operator abort remains status=cancelled rather than being relabeled.
func isHeadlessTimeoutFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, client.ErrModelRoundTimeout) ||
		client.IsStreamIdleTimeout(err) ||
		client.IsHTTPTimeout(err) {
		return true
	}

	switch client.DetectFailureTelemetry(err).Reason {
	case string(client.FailureReasonStreamIdleTimeout),
		string(client.FailureReasonModelRoundTimeout),
		string(client.FailureReasonHTTPTimeout):
		return true
	default:
		return false
	}
}

// finishForegroundProcessing is the shared terminal transition for model turns
// and slash commands. It releases the foreground slot, lets the caller publish
// its final UI messages, then hands the FIFO head to the normal submit path.
// Keeping those steps together prevents one operation type from leaving queued
// user input stranded after success, error, panic, or early validation failure.
func (a *App) finishForegroundProcessing(beforePending func()) {
	// Shutdown owns terminal persistence now. Do not synchronously send final UI
	// messages or dequeue another request: handleQuit runs in Bubble Tea's Update
	// callback, where Program.Send cannot complete until Update returns.
	a.mu.Lock()
	if a.shuttingDown {
		a.processing = false
		a.dropSteerLeftovers = true
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	if beforePending != nil {
		func() {
			defer func() {
				if panicValue := recover(); panicValue != nil {
					a.mu.Lock()
					a.processing = false
					a.mu.Unlock()
					panic(panicValue)
				}
			}()
			beforePending()
		}()
	}

	// A headless invocation owns the foreground until its synchronous session
	// save and terminal result encoding are complete. The model pipeline ends
	// before those steps, so keep the slot occupied here and let
	// releaseHeadlessForeground perform the normal FIFO handoff. Otherwise a
	// queued interactive turn can start under the headless presenter/token and
	// contaminate its output, policy outcome, and usage ledger.
	a.mu.Lock()
	if a.headlessRunActive {
		a.processing = true
		a.mu.Unlock()
		a.saveRecoverySnapshot()
		return
	}
	a.mu.Unlock()

	dispatchLineage := a.captureConversationLineage()

	// Transfer ownership while holding a.mu. Keeping processing=true when a
	// pending item exists removes the idle gap in which later input could claim
	// the foreground and overtake the FIFO head.
	a.mu.Lock()
	if a.shuttingDown {
		a.processing = false
		a.dropSteerLeftovers = true
		a.mu.Unlock()
		return
	}
	var pendingRequest pendingRequest
	var remaining int
	var ok bool
	var staleRecoveries int
	discardedLate := 0
	if a.dropSteerLeftovers {
		// Esc drained all pre-boundary work. The cancelled turn's late finalizer
		// may reopen the gate only for a request carrying explicit post-Esc user
		// provenance; unmarked callbacks/hooks/timers are discarded.
		pendingRequest, remaining, ok, discardedLate = a.dequeuePostCancelUserPending()
		if ok {
			a.dropSteerLeftovers = false
		}
	} else {
		pendingRequest, remaining, ok, staleRecoveries = a.dequeuePendingRequestForSession(
			dispatchLineage.sessionID, dispatchLineage.epoch)
	}
	pending := pendingRequest.message
	var name string
	var args []string
	var isCmd bool
	var nextCtx context.Context
	if ok {
		a.processing = true
		a.dropSteerLeftovers = false
		// Install the next turn's cancellation owner before publishing any UI
		// handoff messages. Program.Send may block behind the event loop; Esc in
		// that interval must cancel this accepted FIFO head, not miss it after it
		// has already been removed from the queue.
		nextCtx = a.claimForegroundContextLocked()
		nextCtx = withConversationLineage(
			nextCtx, dispatchLineage.sessionID, dispatchLineage.epoch)
		if pendingRequest.recoverySessionID == "" {
			name, args, isCmd = a.commandHandler.Parse(pending)
		}
	} else {
		a.processing = false
	}
	a.mu.Unlock()
	if discardedLate > 0 {
		logging.Warn("discarded unowned FIFO work behind cancellation gate", "count", discardedLate)
	}
	if ok && pendingRequest.recoverySessionID == "" && !isCmd {
		claimed, checkpoints, matched, claimErr := a.claimQueuedRecoveryForPrompt(
			pending, dispatchLineage.epoch)
		if matched && claimErr == nil {
			pendingRequest.recoveryID = claimed.ID
			pendingRequest.recoverySessionID = claimed.SessionID
			pendingRequest.recoveryMemoryQuery = claimed.UserMessage
			pendingRequest.recoveryEpoch = dispatchLineage.epoch
			pendingRequest.recoveryCheckpoints = checkpoints
			pending = claimed.Message
			nextCtx = withConversationLineage(
				nextCtx, claimed.SessionID, dispatchLineage.epoch)
		} else if matched {
			logging.Warn("queued duplicate recovery prompt was blocked", "error", claimErr)
			a.safeSendToProgram(ui.QueuedCountMsg(remaining))
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusWarning,
				Message: "A queued repeat matched safe-retry state that was already claimed or inconsistent; it was not run with a fresh ledger",
			})
			a.processingMu.Lock()
			if a.processingCancel != nil {
				a.processingCancel()
				a.processingCancel = nil
			}
			a.processingMu.Unlock()
			a.finishForegroundProcessing(nil)
			a.saveRecoverySnapshot()
			return
		}
	}
	if staleRecoveries > 0 {
		logging.Warn("deferred claimed recovery owned by another conversation", "count", staleRecoveries)
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: "A safe retry belongs to another conversation and remains queued; switch back or inspect /recovery",
		})
	}

	if ok {
		a.safeSendToProgram(ui.QueuedCountMsg(remaining))
		a.safeSendToProgram(ui.StreamTextMsg("\n📤 Processing queued message...\n"))
		a.startAcceptedSubmitWithRecoveryIdentity(
			nextCtx, pending, name, args, isCmd,
			pendingRequest.recoveryID,
			pendingRequest.recoverySessionID,
			pendingRequest.recoveryMemoryQuery,
			pendingRequest.recoveryEpoch,
			pendingRequest.recoveryCheckpoints,
		)
	}
	a.saveRecoverySnapshot()
}

// finishMessageProcessing releases ownership of a foreground turn on every
// exit path, including validation failures before the model pipeline starts.
// It is deferred directly so recover can convert a synchronous pipeline panic
// into visible UI feedback instead of leaving the prompt permanently busy.
func (a *App) finishMessageProcessing() {
	panicValue := recover()
	if panicValue != nil {
		logging.Error("panic in message processing", "panic", panicValue, "stack", logging.PanicStack())
		a.recordHeadlessTerminalOutcome("panic", fmt.Sprintf("panic in message processing: %v", panicValue))
	}
	// Clear only this completed turn's cancel handle before a FIFO handoff can
	// install the next one.
	a.processingMu.Lock()
	a.processingCancel = nil
	a.processingMu.Unlock()
	a.finishForegroundProcessing(func() {
		if a.executor != nil {
			a.executor.SetSideEffectDedup(false)
		}
		// Busy ownership is already released before UI delivery. Even a broken
		// or shutting-down presenter cannot leave subsequent submits stuck busy.
		if panicValue != nil {
			a.safeSendToProgram(ui.ErrorMsg(fmt.Errorf("internal error: %v — your work was saved, please retry", panicValue)))
		}
	})
}

// processMessageWithContext handles user messages with full context management.
func (a *App) processMessageWithContext(ctx context.Context, message string) {
	a.processMessageWithMemoryQuery(ctx, message, message)
}

// processMessageWithMemoryQuery separates the model payload from the text used
// for durable-memory retrieval. Interactive @path expansion can append a large
// file to the payload; searching memory with that file would be both expensive
// and semantically wrong, so the caller supplies the original user request.
func (a *App) processMessageWithMemoryQuery(ctx context.Context, message, memoryQuery string) {
	defer a.finishMessageProcessing()
	// A FIFO handoff can be cancelled while its UI status send is waiting for
	// the event loop. Keep the common finalizer, but do not enter the model
	// pipeline when that accepted turn was already cancelled before launch.
	if ctx.Err() != nil {
		return
	}
	if ceiling, restricted := a.toolCapabilitySnapshot(); restricted {
		if inherited, inheritedRestricted :=
			tools.ToolCapabilityCeilingFromContext(ctx); inheritedRestricted {
			ceiling = intersectCapabilityNames(ceiling, inherited)
		}
		ctx = tools.ContextWithToolCapabilityCeiling(ctx, ceiling)
	}

	if a.session == nil {
		a.safeSendToProgram(ui.ErrorMsg(fmt.Errorf("session not initialized — try /clear or restart")))
		return
	}
	turnLineage, ok := conversationLineageFromContext(ctx)
	if !ok {
		turnLineage = a.captureConversationLineage()
	}
	abortIfConversationChanged := func() bool {
		if a.conversationLineageMatches(turnLineage) {
			return false
		}
		err := fmt.Errorf("%w; discarded the stale turn instead of applying it to the new session", errRecoveryConversationChanged)
		logging.Warn("discarded turn after conversation boundary",
			"session_id", turnLineage.sessionID, "epoch", turnLineage.epoch)
		a.recordHeadlessTerminalOutcome("conversation_changed", err.Error())
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: "The conversation changed while this request was running; its stale result and automatic retries were discarded",
		})
		a.safeSendToProgram(ui.ResponseDoneMsg{})
		return true
	}
	if abortIfConversationChanged() {
		return
	}
	turnRecovery, recoveryTurn := sideEffectRecoveryContextFromContext(ctx)

	a.journalEvent("request_started", map[string]any{
		"message_preview": previewForJournal(message),
	})
	a.saveRecoverySnapshot()

	// Discuss-mode (discuss_mode.go): classify this turn as analysis vs implement
	// and push the stance into turn-context so the model is TOLD the stance and
	// the executor's Step 4.7 gate / incomplete-work suppression engage. Foreground
	// interactive only; headless/eval always acts. pushTurnContext refreshes the
	// client's ephemeral turn-context (never the cached prefix) with the banner.
	a.beginTurnIntent(message)
	// Clear a leftover tool-budget flag from a PRIOR turn that ended on an
	// error path (before the end-of-turn auto-continue check consumed it) —
	// otherwise the NEXT normal turn would false-trigger an auto-continue.
	a.turnToolBudgetHit.Store(false)
	// Recall durable facts that are relevant to THIS request. Generic hot memory
	// remains available in the stable system prefix; query-aware recall belongs
	// beside the user turn so GLM prefix caching is not invalidated every message.
	a.updateRelevantMemoryForTurn(memoryQuery)
	a.pushTurnContext()

	// Group all file changes from this message for atomic undo.
	if a.undoManager != nil {
		groupID := fmt.Sprintf("msg-%d", time.Now().UnixNano())
		a.undoManager.SetActiveGroup(groupID)
		defer a.undoManager.ClearActiveGroup()
	}

	// Activity-based idle timeout: cancel if no model activity (text, tool calls,
	// thinking) for messageIdleTimeout. Unlike wall-clock context.WithTimeout,
	// this survives system sleep/wake — heartbeat freezes during sleep, and
	// resumes when callbacks fire on wake.
	// PlanningTimeout is for individual plan-generation LLM calls (by default
	// it follows the model-round cap)
	// and must NOT be used here — it would kill normal conversations.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.touchStepHeartbeat()
	headlessTurn := a.activeHeadlessTerminalToken()
	a.startMessageIdleWatchdog(ctx, cancel, headlessTurn)

	// Track response start time and reset tools used
	a.mu.Lock()
	a.responseStartTime = time.Now()
	a.responseToolsUsed = nil
	a.responseTouchedPaths = nil
	a.responseCommands = nil
	a.responseEvidence = responseEvidenceLedger{}
	a.streamedChars = 0           // Reset streaming accumulator
	a.streamedEstimatedTokens = 0 // Reset streaming token estimate
	a.messageCount++
	a.diffBatchDecision = ui.DiffPending
	currentMsgCount := a.messageCount
	a.mu.Unlock()

	// A fresh user turn owns a fresh side-effect ledger. An automatic retry
	// instead restores the exact checkpoint generation captured when its failed
	// attempt ended; clearing it here would allow duplicate writes/bash calls.
	if a.executor != nil {
		if recoveryTurn {
			a.executor.PrepareSideEffectRecovery(turnRecovery.checkpoints)
		} else {
			a.executor.ResetSideEffectLedger()
			a.executor.SetSideEffectDedup(false)
		}

	}

	// Reset stale in_progress todos from previous turn.
	// The model will re-set them to in_progress as it works.
	if tt := a.GetTodoTool(); tt != nil {
		tt.ResetInProgress()
	}

	// === Task 5.8: Inject tool hints every 10 messages ===
	if currentMsgCount > 0 && currentMsgCount%10 == 0 && a.promptBuilder != nil {
		hints := a.getToolHints()
		a.promptBuilder.SetToolHints(hints)
		if hints != "" {
			logging.Debug("tool hints injected", "message_count", currentMsgCount, "hints_length", len(hints))
		}
	}

	// Set last user message for conditional planning protocol injection.
	if a.promptBuilder != nil {
		a.promptBuilder.SetLastMessage(message)
	}

	// Resolve the request-aware schema before rebuilding the system prompt and
	// recording cache state. The immutable decisions stay anchored to this
	// actual user/recovery payload across automatic retries, whose continuation
	// scaffolding must not alter tool exposure. This also covers direct/headless
	// execution, which deliberately bypasses Router.Execute.
	mode := a.runtimeEngineModeSnapshot()
	policyDecision := hybrid.Decide(mode, message)
	autoDecision := policyDecision
	if strings.EqualFold(strings.TrimSpace(mode), "hybrid") {
		// Explicit exposure must not turn into blanket steering: only prompts
		// that independently satisfy auto policy receive the REPL usage hint.
		autoDecision = hybrid.Decide("auto", message)
	}
	turnTools := a.toolsForMessageDecision(message, policyDecision)
	// The App has now applied feature, plan, invocation, and lazy-hybrid gates.
	// Make that exact schema a request capability ceiling: Router retries may
	// narrow it by strategy, but cannot re-add a declaration the authoritative
	// request policy hid. Preserve any inherited ceiling as an intersection.
	turnToolNames := toolSchemaDeclarationNames(turnTools)
	if inherited, restricted := tools.ToolCapabilityCeilingFromContext(ctx); restricted {
		turnToolNames = intersectCapabilityNames(turnToolNames, inherited)
		turnTools = tools.FilterGeminiToolsByCapability(turnTools, turnToolNames)
	}
	ctx = tools.ContextWithToolSchemaCeiling(ctx, a.executor, turnToolNames)
	if a.client != nil {
		a.client.SetTools(turnTools)
	}
	hybridPolicy := hybridPolicySnapshotForDecision(mode, policyDecision, turnTools)
	// In explicit hybrid mode exposure is unconditional, while steering remains
	// request-specific. Journal the strategy that actually selected the hint.
	hybridPolicy.Strategy = autoDecision.Strategy
	a.journalEvent("engine_policy", hybridPolicy.journalDetails())
	directHybridHint := hybrid.AnalysisHintForDecision(autoDecision, hybridPolicy.REPLExposed)

	// Keep dynamic system instruction in sync (contract/memory/hints can change between turns).
	a.refreshSystemInstruction()

	// Record the exact post-refresh cacheable state. The old call ran before
	// refresh and passed an empty tools payload, so it detected system changes a
	// turn late and could never diagnose MCP/tool-schema cache breaks.
	if a.executor != nil {
		if ct := a.executor.GetCacheTracker(); ct != nil {
			toolsJSON, err := json.Marshal(turnTools)
			if err != nil {
				logging.Debug("failed to serialize tools for prompt-cache tracking", "error", err)
			}
			ct.RecordState(a.session.GetSystemInstruction(), string(toolsJSON))
		}
	}

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

	// Inject error context if retrying after a recent failure
	a.mu.Lock()
	if recoveryTurn {
		// Durable/in-process recovery.Message is the exact executable payload and
		// its retry budget is keyed by that stable identity. Prefixing it again
		// would both violate replay exactness and reset the persisted attempt
		// budget under a new hash on every failure.
		a.lastError = ""
		a.lastErrorTime = time.Time{}
	} else if a.lastError != "" && time.Since(a.lastErrorTime) < 2*time.Minute {
		message = fmt.Sprintf("[Note: previous attempt failed with: %s. The context from that attempt is preserved in history.]\n\n%s", a.lastError, message)
		a.lastError = "" // Clear after use
	}
	a.mu.Unlock()

	// === IMPROVEMENT 1: Use Task Router for intelligent routing ===
	// Auto-retry transient errors with adaptive stream retry policy.
	runtimeProvider := runtimeProviderForConfig(a.config)
	retryPolicy := client.AdaptiveStreamRetryPolicy(runtimeProvider)
	// Provider overloads (GLM 1305 et al) are transient and self-resolving, so
	// they get a separate, far more patient retry budget than ordinary errors —
	// the agent waits the server out instead of giving up after a few seconds.
	overloadPolicy := client.DefaultOverloadRetryPolicy()
	overloadLabel := strings.ToUpper(runtimeProvider)
	if overloadLabel == "" {
		overloadLabel = "Provider"
	}
	originalMessage := message
	retryMessage := originalMessage

	var newHistory []*genai.Content
	var response string
	var err error
	contextTruncated := false
	requestRetryCount := 0
	partialIdleRetryCount := 0
	overloadRetryCount := 0
	var overloadElapsed time.Duration
	var turnUsage turnUsageAccumulator
	usageCommitted := false
	// Several fail-closed finalization paths (done-gate refusal, recovered
	// panic, context-clear handoff) return before the normal footer/metadata
	// block. Provider work has already been performed and billed at that point.
	// Commit the accumulated ledger exactly once on every exit so headless JSON
	// cannot report zero usage for a failed-but-executed turn.
	defer func() {
		if usageCommitted {
			return
		}
		if turnUsage.empty() {
			return
		}
		turnUsage.commit(a)
		usageCommitted = true
	}()

	a.mu.Lock()
	headlessDirect := a.headlessDirect
	a.mu.Unlock()

	for {
		if abortIfConversationChanged() {
			return
		}
		history = a.session.GetHistory() // Re-read history on each attempt (partial saves possible)
		currentMessage := retryMessage
		unaugmentedMessage := currentMessage
		if (a.taskRouter == nil || headlessDirect) && directHybridHint != "" {
			currentMessage = directHybridHint + "\n\n" + currentMessage
		}
		execFn := func() error {
			// Headless runs execute directly: routed strategies stream
			// through the nil TUI program and skip the journal (headless.go).
			if a.taskRouter != nil && !headlessDirect {
				// Route the task intelligently
				newHistory, response, err = a.taskRouter.ExecuteWithPolicyDecisions(
					ctx, history, currentMessage, mode, policyDecision, autoDecision)

				// Log routing decision for debugging
				if analysis := a.taskRouter.GetAnalysis(message); analysis != nil {
					logging.Debug("task routed",
						"complexity", analysis.Score,
						"type", analysis.Type,
						"strategy", analysis.Strategy,
						"reasoning", analysis.Reasoning)
					// Surface non-trivial routing decisions. Direct / single-tool are
					// the common boring case — no toast. Sub-agent / executor-with-
					// complexity deserves a one-time heads-up so the user knows why
					// the first response may take longer than a simple Q&A.
					if requestRetryCount == 0 && analysis.Strategy != router.StrategyDirect &&
						analysis.Strategy != router.StrategySingleTool {
						msg := fmt.Sprintf("Routing: %s (complexity %d)", analysis.Strategy, analysis.Score)
						if analysis.Reasoning != "" {
							msg = fmt.Sprintf("Routing: %s — %s", analysis.Strategy, analysis.Reasoning)
						}
						a.safeSendToProgram(ui.StatusUpdateMsg{
							Type:    ui.StatusInfo,
							Message: msg,
						})
					}
				}
			} else {
				// Fallback to standard executor
				newHistory, response, err = a.executeTracked(
					ctx, history, currentMessage, &turnUsage)
				if directHybridHint != "" {
					newHistory = restoreDirectPromptScaffolding(
						newHistory, len(history), unaugmentedMessage)
				}
			}
			return err
		}
		if a.policy != nil {
			err = a.policy.ExecuteRequest(ctx, execFn)
		} else {
			err = execFn()
		}
		if abortIfConversationChanged() {
			return
		}

		if err == nil {
			// Detect empty response from model (common with MiniMax and weak models
			// when context is too large — they return 200 OK with empty text instead
			// of a proper 400 context-too-long error).
			if !contextTruncated && a.contextManager != nil &&
				strings.HasPrefix(response, "⚠ Model returned an empty response") {
				history := a.session.GetHistory()
				tokens := appcontext.EstimateContentsTokens(history)
				limits := appcontext.GetModelLimits(a.config.Model.Name)
				// If context is >50% of limit, try compaction + retry
				if limits.MaxInputTokens > 0 && tokens > limits.MaxInputTokens/2 {
					contextTruncated = true
					// Persist any partial history from the just-finished
					// (empty-response) attempt before we EmergencyTruncate
					// the session. Otherwise tool side-effects that ran
					// during the round (writes, edits, etc.) are recorded
					// only on disk, but the session history loses the
					// matching FunctionCall/FunctionResponse pairs — the
					// next attempt's model has no record those tools ran
					// and may re-execute them.
					if len(newHistory) > len(history) {
						cleaned := stripOrphanFunctionCalls(newHistory)
						a.session.SetHistory(cleaned)
						a.syncToolCheckpoints()
						if a.sessionManager != nil {
							_ = a.sessionManager.SaveAfterMessage()
						}
					}
					removed := a.contextManager.EmergencyTruncate()
					if removed > 0 {
						a.safeSendToProgram(ui.StatusUpdateMsg{
							Type:    ui.StatusRetry,
							Message: fmt.Sprintf("Empty response — compacted %d messages, retrying", removed),
						})
						response = ""
						continue
					}
				}
			}
			break
		}

		ft := client.DetectFailureTelemetry(err)
		logging.Warn("request attempt failed",
			"reason", ft.Reason,
			"partial", ft.Partial,
			"timeout", ft.Timeout,
			"provider", ft.Provider,
			"request_retries", requestRetryCount,
			"partial_retries", partialIdleRetryCount,
			"error", err)

		// Don't retry if context cancelled (user abort)
		if ctx.Err() != nil {
			err = client.ContextErr(ctx)
			break
		}

		// A routed/delegated executor can fail after a stateful tool already
		// started while its checkpoints live outside the foreground executor's
		// exact replay ledger. Retrying the whole request here could spawn a new
		// agent and repeat bash/edit/MCP mutations. Fail closed before context
		// truncation, generic retry classification, or any backoff scheduling.
		if isAutomaticRetryUnsafe(err) {
			logging.Error("automatic request retry blocked after uncertain side effect",
				"error", err,
				"message_preview", previewForJournal(message))
			break
		}

		// Context too long — emergency truncate and retry (once only)
		if client.IsContextTooLongError(err) && a.contextManager != nil && !contextTruncated {
			contextTruncated = true
			if a.executor != nil {
				a.executor.SetSideEffectDedup(true)
			}
			removed := a.contextManager.EmergencyTruncate()
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRetry,
				Message: fmt.Sprintf("Context too long — truncated %d messages, retrying", removed),
				Details: map[string]any{"attempt": 1, "maxAttempts": 1},
			})
			continue
		}

		// Overload errors (GLM 1305 et al) take the patient budget — wait the
		// provider out rather than burning the small generic retry budget and
		// stopping the agent. Everything else uses the normal fast-fail policy.
		// Exclude a tripped request breaker: its "rate limited or down" message
		// would substring-match the overload keywords, but it signals genuine
		// repeated hard failures (or a provider that's down) — not a transient
		// capacity spike — and must keep its own fast-fail handling.
		overload := client.IsOverloadError(err) && !errors.Is(err, ErrRequestCircuitOpen)
		var decision client.StreamRetryDecision
		if overload {
			decision = client.DecideOverloadRetry(overloadPolicy, err, overloadRetryCount, overloadElapsed, ctx)
		} else {
			decision = client.DecideStreamRetry(
				retryPolicy,
				err,
				requestRetryCount,
				partialIdleRetryCount,
				ctx,
				client.StreamRetryOptions{AllowPartial: true},
			)
		}
		if !decision.ShouldRetry {
			break
		}
		if a.executor != nil {
			a.executor.SetSideEffectDedup(true)
		}

		if overload {
			overloadRetryCount++
			overloadElapsed += decision.Delay
			// An overload isn't the hard repeated failure the request circuit
			// breaker exists to catch — keep it from tripping on transient
			// capacity errors so the next attempt actually runs. The capped
			// backoff already spaces attempts out; the overload breaker (below)
			// still records pressure for telemetry.
			if a.policy != nil {
				a.policy.ResetRequestBreaker()
			}
		} else if decision.Partial {
			partialIdleRetryCount++
		} else {
			requestRetryCount++
		}

		// Automatic cross-provider failover used to live here — it would
		// switch from e.g. kimi to glm after 2 retries or a tripped overload
		// breaker. Removed intentionally: users reported surprise billing
		// errors from providers they weren't actively using (e.g. "GLM
		// insufficient balance" while on kimi). Sticking to the chosen
		// provider makes failures attributable and predictable. If the
		// user wants a fallback chain, they can opt in explicitly via
		// `model.fallback_providers` in config.yaml — that path still
		// builds a FallbackClient in client.NewClient and works normally.
		//
		// `overloadTripped` still gets recorded so /policy reports remain
		// accurate, but we no longer act on it by switching providers.
		if isOverloadError(err) && a.policy != nil {
			_ = a.policy.RecordOverload()
		}

		// Save partial history before retry (preserves tool side effects).
		// Strip orphan tool_calls — if the model emitted FunctionCalls but
		// the executor never produced matching FunctionResponses (e.g. the
		// retry was triggered mid-execution), persisting the orphans
		// permanently breaks the session: every subsequent API call returns
		// 400 "tool_call_ids did not have response messages" and the user
		// has to /clear. The terminal-error path at line 403+ already does
		// this via stripOrphanFunctionCalls; the retry path was missing it.
		if len(newHistory) > len(history) {
			cleaned := stripOrphanFunctionCalls(newHistory)
			a.session.SetHistory(cleaned)
			a.syncToolCheckpoints()
			if a.sessionManager != nil {
				_ = a.sessionManager.SaveAfterMessage()
			}
			// Anchor the next attempt to what THIS attempt actually produced,
			// not to which retry-decision branch fired. Previously each
			// branch set retryMessage independently — overload always reset
			// it to the bare originalMessage (discarding a still-valid
			// continuation anchor from a prior partial-stall retry), and a
			// plain failure left it untouched (keeping a stale anchor from
			// several iterations back after a partial->plain transition). A
			// branch transition mid-retry-loop could desync the anchor from
			// history.
			retryMessage = nextRetryMessageAfterProgress(originalMessage, history, cleaned)
		} else {
			retryMessage = originalMessage
		}

		// Warn user about retry
		backoff := decision.Delay

		// Show retry as a non-intrusive status toast instead of inline text.
		// This keeps the output clean and focused on the model's actual response.
		var retryMsg string
		if overload {
			retryMsg = fmt.Sprintf("%s overloaded — waiting %v, retrying (attempt %d, will keep trying)", overloadLabel, backoff.Round(time.Second), overloadRetryCount)
		} else if decision.Partial {
			retryMsg = fmt.Sprintf("Stream stalled — retry %d/%d in %v", partialIdleRetryCount, retryPolicy.MaxPartialRetries, backoff.Round(time.Second))
		} else if ft.Reason == string(client.FailureReasonStreamIdleTimeout) {
			retryMsg = fmt.Sprintf("Stream stalled — retry %d/%d in %v", requestRetryCount, retryPolicy.MaxRetries, backoff.Round(time.Second))
		} else {
			retryMsg = fmt.Sprintf("Retry %d/%d in %v (%s)", requestRetryCount, retryPolicy.MaxRetries, backoff.Round(time.Second), ft.Reason)
		}
		attemptNum, maxAttempts := requestRetryCount, retryPolicy.MaxRetries
		if overload {
			attemptNum, maxAttempts = overloadRetryCount, overloadPolicy.MaxRetries
		}
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusRetry,
			Message: retryMsg,
			Details: map[string]any{
				"attempt":     attemptNum,
				"maxAttempts": maxAttempts,
				"provider":    runtimeProvider,
			},
		})

		backoffTimer := time.NewTimer(backoff)
		select {
		case <-backoffTimer.C:
			continue
		case <-ctx.Done():
			backoffTimer.Stop()
			err = client.ContextErr(ctx)
		}
		break
	}

	if err != nil {
		// Terminal failures return before normal turn finalization. Commit every
		// attempt collected above so failed-request spend remains visible.
		turnUsage.commit(a)
		usageCommitted = true
		// Provider/round deadlines remain recoverable in the interactive UI,
		// but one-shot automation must exit non-zero with a typed timeout. This
		// runs only after the in-turn retry budget is exhausted, so a transient
		// timeout that recovered is still a successful turn.
		if isHeadlessTimeoutFailure(err) {
			a.recordHeadlessTerminalOutcomeForTurn(headlessTurn, "timeout", err.Error())
		} else if errors.Is(err, tools.ErrMaxTurnsExceeded) {
			a.recordHeadlessTerminalOutcomeForTurn(headlessTurn, "max_turns", err.Error())
		} else if errors.Is(err, tools.ErrBudgetExceeded) {
			a.recordHeadlessTerminalOutcomeForTurn(headlessTurn, "budget_exceeded", err.Error())
		} else if errors.Is(err, tools.ErrCostUnavailable) {
			a.recordHeadlessTerminalOutcomeForTurn(headlessTurn, "cost_unavailable", err.Error())
		}

		cancelled := errors.Is(err, context.Canceled)
		ft := client.DetectFailureTelemetry(err)
		failureProvider := ft.Provider
		if failureProvider == "" {
			failureProvider = runtimeProvider
		}
		journalEvent := "request_failed"
		if cancelled {
			journalEvent = "request_cancelled"
		}
		a.journalEvent(journalEvent, map[string]any{
			"error":           err.Error(),
			"message_preview": previewForJournal(message),
			"failure_reason":  ft.Reason,
			"partial":         ft.Partial,
			"timeout":         ft.Timeout.String(),
			"provider":        ft.Provider,
		})
		// Don't count user cancellation (Esc) as a failure — only real API errors
		if a.reliability != nil && !cancelled {
			a.reliability.RecordFailure()
		}
		if errors.Is(err, ErrRequestCircuitOpen) {
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRecoverableError,
				Message: "Too many failures — pausing requests, will retry automatically",
			})
		}

		// Save history on error only if it ends with a model message (complete turn).
		// If it ends with a user/tool-result message, the model never responded —
		// saving it would create an orphaned tool_result that breaks subsequent API calls.
		if len(newHistory) > len(history) {
			if last := newHistory[len(newHistory)-1]; last != nil && last.Role == genai.RoleModel {
				// Last message is a model turn — its tool_calls may be
				// orphaned (execution failed before we appended results).
				// Strip orphans so the next API call doesn't fail with
				// "tool_call_ids did not have response messages" on every
				// subsequent turn.
				cleaned := stripOrphanFunctionCalls(newHistory)
				a.session.SetHistory(cleaned)
				// Persist the tool-checkpoint journal alongside the saved
				// partial history — the success path (after the loop) and BOTH
				// retry-path saves do this; the terminal-error branches omitted
				// it. Without it, tools that executed (side-effects on disk) lose
				// their durable checkpoint when ResetSideEffectLedger clears the
				// in-memory journal next request → the model can re-run them
				// (duplicate writes / bash / git commits).
				a.syncToolCheckpoints()
				if a.sessionManager != nil {
					_ = a.sessionManager.SaveAfterMessage()
				}
			} else {
				// Trim back to the last model message to avoid orphaned tool results
				trimmed := trimToLastModelMessage(newHistory, len(history))
				if len(trimmed) > len(history) {
					a.session.SetHistory(trimmed)
					a.syncToolCheckpoints() // see note above — symmetric with retry/success saves
					if a.sessionManager != nil {
						_ = a.sessionManager.SaveAfterMessage()
					}
				}
			}
		}
		if cancelled {
			// Explicit cancellation is an intentional terminal state, not a
			// provider failure. Do not surface it as ErrorMsg or carry it into the
			// next prompt as "previous attempt failed". ResponseDone is still
			// required for OS-signal cancellation, whose UI path does not perform
			// the TUI Esc transition itself.
			a.clearRateLimitRetry(message)
			a.clearAutoResume(originalMessage)
			a.mu.Lock()
			a.lastError = ""
			a.lastErrorTime = time.Time{}
			a.mu.Unlock()
			a.safeSendToProgram(ui.ResponseDoneMsg{})
			return
		}
		if isAutomaticRetryUnsafe(err) {
			// The failed delegated run may already have changed files, but its
			// tool-attempt ledger cannot yet be replayed exactly by the foreground
			// executor. Never turn this into a rate-limit timer or auto-resume.
			// A claimed durable recovery is intentionally left claimed/manual-only;
			// clearing it here would falsely certify that replay is safe.
			a.clearRateLimitRetry(message)
			a.clearAutoResume(originalMessage)
			a.mu.Lock()
			a.lastError = ""
			a.lastErrorTime = time.Time{}
			a.mu.Unlock()
			a.journalEvent("automatic_retry_blocked_unsafe_side_effect", map[string]any{
				"error":           err.Error(),
				"message_preview": previewForJournal(message),
			})
			if headlessTurn != nil {
				a.recordHeadlessTerminalOutcomeForTurn(
					headlessTurn, "unsafe_recovery", err.Error())
			}
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type: ui.StatusWarning,
				Message: "The delegated run may already have changed the workspace. " +
					"Automatic retry was blocked; inspect the diff, then explicitly continue.",
			})
			a.safeSendToProgram(ui.ResponseDoneMsg{})
			a.safeSendToProgram(ui.ErrorMsg(err))
			return
		}
		// Store error for context injection on retry
		a.mu.Lock()
		a.lastError = err.Error()
		a.lastErrorTime = time.Now()
		a.mu.Unlock()

		if a.reliability != nil && a.reliability.IsDegraded() {
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRetry,
				Message: fmt.Sprintf("Safe mode enabled (%v) — reduced concurrency for stability", a.reliability.DegradedRemaining().Round(time.Second)),
			})
		}

		if client.IsRateLimitError(err) {
			attempt, delay, ok := a.scheduleRateLimitAutoRetry(message)
			if ok {
				a.journalEvent("rate_limit_auto_retry_scheduled", map[string]any{
					"attempt":        attempt,
					"max_attempts":   maxAutoRateLimitRetries,
					"delay":          delay.String(),
					"failure_reason": ft.Reason,
					"partial":        ft.Partial,
					"timeout":        ft.Timeout.String(),
					"provider":       failureProvider,
				})
				// Name the provider in the toast so the user can tell at a
				// glance whether it's their active backend or something in
				// the (opt-in) fallback chain that got throttled.
				provider := a.shortActiveProviderName()
				a.safeSendToProgram(ui.StatusUpdateMsg{
					Type:    ui.StatusRateLimit,
					Message: fmt.Sprintf("%s rate limit — auto-retry in %v (%d/%d)", provider, delay.Round(time.Second), attempt, maxAutoRateLimitRetries),
					Details: map[string]any{
						"waitTime": delay,
						"provider": provider,
					},
				})
				a.safeSendToProgram(ui.ResponseDoneMsg{})

				retryMsg, retryWait := message, delay
				retrySessionID := turnLineage.sessionID
				retryEpoch := turnLineage.epoch
				recoveryCheckpoints := a.sideEffectRecoverySnapshot()
				persisted, persistErr := a.persistPendingRecovery(
					"rate_limit", retryMsg, memoryQuery,
					recoveryCheckpoints, attempt, retryWait,
					turnRecovery.recoveryID, retrySessionID, retryEpoch,
				)
				if persistErr == nil {
					if headlessTurn != nil {
						// One-shot automation must not return and then mutate the
						// workspace from an App-global background timer. The durable
						// scheduled record remains available to the next explicit exact
						// headless invocation.
						a.recordHeadlessTerminalOutcomeForTurn(
							headlessTurn, "rate_limit", err.Error())
						a.safeSendToProgram(ui.ErrorMsg(err))
					} else {
						a.schedulePersistedRecovery(persisted, false)
						// Replacing a claimed generation with this scheduled
						// attempt re-opens the global claim slot. Restore any
						// sibling timers paused while the prior attempt ran.
						a.resumePersistedRecoveries(false)
					}
				} else if errors.Is(persistErr, errRecoveryConversationChanged) {
					logging.Info("rate-limit retry cancelled by conversation clear")
					if headlessTurn != nil {
						a.recordHeadlessTerminalOutcomeForTurn(
							headlessTurn, "conversation_changed", persistErr.Error())
					}
				} else if errors.Is(persistErr, errRecoveryPersistenceFailed) {
					if headlessTurn != nil {
						a.recordHeadlessTerminalOutcomeForTurn(
							headlessTurn, "rate_limit", err.Error())
						a.safeSendToProgram(ui.ErrorMsg(err))
						return
					}
					logging.Warn("rate-limit retry is in-process only", "error", persistErr)
					a.safeSendToProgram(ui.StatusUpdateMsg{
						Type:    ui.StatusWarning,
						Message: "Retry could not be saved for restart recovery; it will continue only while this process stays open",
					})
					a.safeGo("rate-limit-auto-retry", func() {
						timer := time.NewTimer(retryWait)
						defer timer.Stop()
						select {
						case <-timer.C:
							a.handleRecoveryResubmit(retryMsg, memoryQuery, turnRecovery.recoveryID, retrySessionID, retryEpoch, recoveryCheckpoints)
						case <-a.ctx.Done():
							return
						}
					})
				} else {
					logging.Warn("rate-limit retry blocked by unsafe or inconsistent recovery state", "error", persistErr)
					if headlessTurn != nil {
						a.recordHeadlessTerminalOutcomeForTurn(
							headlessTurn, "unsafe_recovery", persistErr.Error())
						a.safeSendToProgram(ui.ErrorMsg(persistErr))
					}
					a.safeSendToProgram(ui.StatusUpdateMsg{
						Type:    ui.StatusWarning,
						Message: "Automatic retry was blocked because its exact safe-replay state could not be verified; inspect /recovery before retrying",
					})
				}
				return
			}
		}

		a.clearRateLimitRetry(message)

		// Auto-resume: if the error is a model round timeout, HTTP timeout,
		// stream-idle, or retry-exhausted transient error, compact the context
		// and retry — up to maxAutoResumeAttempts times. This prevents the
		// "agent stopped at 14m" and "same error repeated (4x)" failures on
		// long-running tasks. The compaction is the key recovery: a smaller
		// context → less reasoning → the model finishes within the timeout.
		// Mirrors the rate-limit auto-retry pattern (safeGo + timer).
		// The failed executor may have committed partial assistant text and
		// completed tool pairs. Resume from that progress instead of replaying
		// the bare request, which invites duplicate prose and tool intent. Budget
		// the exact executable payload from its first schedule — this also keeps
		// the cap intact when persistence fails and recovery stays in-process.
		// A recovery turn keeps its already-anchored payload byte-for-byte stable,
		// so later attempts use the same retry identity.
		resumeMsg := nextAutoResumeMessageAfterProgress(
			originalMessage, history, a.session.GetHistory(), recoveryTurn)
		if resumeAttempt, resumeDelay, ok := a.scheduleAutoResume(resumeMsg, err); ok {
			reason := autoResumeReason(err)
			a.journalEvent("auto_resume_scheduled", map[string]any{
				"attempt":        resumeAttempt,
				"max_attempts":   maxAutoResumeAttempts,
				"delay":          resumeDelay.String(),
				"reason":         reason,
				"failure_reason": ft.Reason,
				"partial":        ft.Partial,
				"timeout":        ft.Timeout.String(),
				"error":          err.Error(),
				"provider":       failureProvider,
			})
			logging.Info("auto-resume scheduled after terminal error",
				"reason", reason,
				"attempt", resumeAttempt,
				"max_attempts", maxAutoResumeAttempts,
				"delay", resumeDelay)

			// Compact the context BEFORE the retry — this is the actual
			// recovery mechanism. Without it, retrying with the same large
			// context would hit the same timeout.
			removed := a.performAutoResumeCompaction()

			// An unchanged FIRST retry is worthwhile even for model timeout:
			// provider latency/reasoning length are nondeterministic, and compact
			// prompts have nothing to remove. Stop only after that unchanged retry
			// also times out, avoiding an unproductive second 14m cycle. Other
			// transient errors retain both attempts because provider recovery alone
			// can resolve them.
			if removed == 0 && shouldSkipUnchangedAutoResume(err, resumeAttempt) {
				logging.Info("auto-resume skipped: repeated model timeout with unchanged context",
					"reason", reason, "attempt", resumeAttempt, "error", err.Error())
				a.refundAutoResume(resumeMsg)
				a.safeSendToProgram(ui.StatusUpdateMsg{
					Type: ui.StatusWarning,
					Message: "Model timed out again with an already-compact context; automatic retry stopped. " +
						"Use /timeout 20m for this workload or continue with a narrower prompt.",
				})
				a.safeSendToProgram(ui.ResponseDoneMsg{})
				a.safeSendToProgram(ui.ErrorMsg(err))
				return
			}

			provider := a.shortActiveProviderName()
			compactNote := ""
			if removed > 0 {
				compactNote = fmt.Sprintf(", compacted %d messages", removed)
			} else if errors.Is(err, client.ErrModelRoundTimeout) {
				compactNote = ", context already compact — retrying transiently"
			}
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRetry,
				Message: fmt.Sprintf("%s — auto-resume %d/%d in %v%s", reason, resumeAttempt, maxAutoResumeAttempts, resumeDelay.Round(time.Second), compactNote),
				Details: map[string]any{
					"attempt":           resumeAttempt,
					"maxAttempts":       maxAutoResumeAttempts,
					"reason":            reason,
					"provider":          provider,
					"compactedMessages": removed,
				},
			})
			a.safeSendToProgram(ui.ResponseDoneMsg{})

			resumeWait := resumeDelay
			resumeSessionID := turnLineage.sessionID
			resumeEpoch := turnLineage.epoch
			recoveryCheckpoints := a.sideEffectRecoverySnapshot()
			persisted, persistErr := a.persistPendingRecovery(
				"auto_resume", resumeMsg, memoryQuery,
				recoveryCheckpoints, resumeAttempt, resumeWait,
				turnRecovery.recoveryID, resumeSessionID, resumeEpoch,
			)
			if persistErr == nil {
				if headlessTurn != nil {
					a.recordHeadlessTerminalOutcomeForTurn(
						headlessTurn, "auto_resume", err.Error())
					a.safeSendToProgram(ui.ErrorMsg(err))
				} else {
					a.schedulePersistedRecovery(persisted, false)
					a.resumePersistedRecoveries(false)
				}
			} else if errors.Is(persistErr, errRecoveryConversationChanged) {
				logging.Info("auto-resume retry cancelled by conversation clear")
				if headlessTurn != nil {
					a.recordHeadlessTerminalOutcomeForTurn(
						headlessTurn, "conversation_changed", persistErr.Error())
				}
			} else if errors.Is(persistErr, errRecoveryPersistenceFailed) {
				if headlessTurn != nil {
					a.recordHeadlessTerminalOutcomeForTurn(
						headlessTurn, "auto_resume", err.Error())
					a.safeSendToProgram(ui.ErrorMsg(err))
					return
				}
				logging.Warn("auto-resume retry is in-process only", "error", persistErr)
				a.safeSendToProgram(ui.StatusUpdateMsg{
					Type:    ui.StatusWarning,
					Message: "Auto-resume could not be saved for restart recovery; it will continue only while this process stays open",
				})
				a.safeGo("auto-resume-retry", func() {
					timer := time.NewTimer(resumeWait)
					defer timer.Stop()
					select {
					case <-timer.C:
						a.handleRecoveryResubmit(resumeMsg, memoryQuery, turnRecovery.recoveryID, resumeSessionID, resumeEpoch, recoveryCheckpoints)
					case <-a.ctx.Done():
						return
					}
				})
			} else {
				logging.Warn("auto-resume blocked by unsafe or inconsistent recovery state", "error", persistErr)
				if headlessTurn != nil {
					a.recordHeadlessTerminalOutcomeForTurn(
						headlessTurn, "unsafe_recovery", persistErr.Error())
					a.safeSendToProgram(ui.ErrorMsg(persistErr))
				}
				a.safeSendToProgram(ui.StatusUpdateMsg{
					Type:    ui.StatusWarning,
					Message: "Auto-resume was blocked because its exact safe-replay state could not be verified; inspect /recovery before retrying",
				})
			}
			return
		}

		a.clearAutoResume(originalMessage)

		a.safeSendToProgram(ui.ErrorMsg(err))
		return
	}

	a.clearRateLimitRetry(message)
	a.clearAutoResume(originalMessage)

	if a.reliability != nil {
		a.reliability.RecordSuccess()
	}

	// Update session history. Defensive shrink-guard: a normal turn result must
	// EXTEND the conversation. If a handler ever returns a shorter slice than we
	// fed in (e.g. a minimal-history router path that wasn't extended), committing
	// it verbatim would wipe prior turns — keep the existing history instead.
	if len(newHistory) >= len(history) {
		a.session.SetHistory(newHistory)
	} else {
		logging.Warn("dropping non-extending turn result to avoid history wipe",
			"got", len(newHistory), "had", len(history))
	}
	a.applyToolOutputHygiene()

	// Extract session memory if thresholds are met
	if a.sessionMemory != nil && a.sessionMemory.ShouldExtract(a.totalInputTokens) {
		a.sessionMemory.ExtractAsync(a.session.GetHistory(), a.totalInputTokens)
	}

	// Check for context-clear request after plan approval
	if a.planManager != nil && a.planManager.IsContextClearRequested() {
		approvedPlan := a.planManager.ConsumeContextClearRequest()
		if approvedPlan != nil && a.config.Plan.ClearContext {
			a.executePlanWithClearContext(ctx, approvedPlan)
			return
		}
	}

	formatCorrection := isStructuredOutputCorrection(ctx)
	if !formatCorrection {
		if !a.runCompletionReviewIfNeeded(ctx, message, &response, &turnUsage) {
			return
		}
	}
	// Completion review may replace the draft response. Capture the resulting
	// terminal answer before the done-gate starts any internal auto-fix model
	// exchanges; JSON headless output must not concatenate those hidden calls.
	a.recordHeadlessFinalResult(response)

	// Hard done-gate before response completion.
	if !formatCorrection {
		if !a.enforceDoneGate(ctx, message) {
			return
		}

		// Evidence footer: deterministic audit-trail streamed after the model's
		// prose answer for code-change turns. Runs AFTER done-gate so a failing
		// verification never gets a "✓ Verified" claim under it. Skipped when
		// the response already names every touched file + verification signal.
		// Intentionally NOT mixed into `response` — working memory already
		// derives its own "Files changed / Verification" lines from the raw
		// touched-paths + commands snapshots, so folding the footer back in
		// would produce redundant entries on the next turn.
		if footer := a.buildEvidenceFooterIfEnabled(response); footer != "" {
			a.safeSendToProgram(ui.StreamTextMsg(footer))
		}

		a.updateWorkingMemoryFromTurn(message, response)
	}

	// A successful recovered turn is terminal for its claimed durable record.
	// Clear it before queuing the normal session save so a second restart cannot
	// rediscover a completed retry.
	if clearErr := a.clearPendingRecovery(turnRecovery.recoveryID, turnRecovery.sessionID, "success"); clearErr != nil {
		a.reportPendingRecoveryClearFailure(headlessTurn, turnRecovery, clearErr)
	}

	// Sync tool checkpoints and save session after each message
	a.syncToolCheckpoints()
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Debug("failed to save session after message", "error", err)
		}
	}

	// Update context token count after processing and send to UI.
	estimatedContextInput := 0
	if a.contextManager != nil {
		if err := a.contextManager.UpdateTokenCount(ctx); err != nil {
			logging.Debug("failed to update token count", "error", err)
		}

		// Send final token usage to UI
		a.sendTokenUsageUpdate()

		if usage := a.contextManager.GetTokenUsage(); usage != nil {
			estimatedContextInput = usage.InputTokens
		}
	}

	// Router strategies account delegated usage through their agent callback.
	// A direct call always has at least one executeTracked sample. Keep a
	// defensive fallback for focused test seams/custom routers that return a
	// response without publishing usage.
	if turnUsage.empty() && (estimatedContextInput > 0 || response != "") {
		turnUsage.add(0, 0, 0, 0, estimatedContextInput, response, 0, false)
	}

	// Session totals back /stats and /cost. Provider-reported values are true
	// per-request deltas and accumulate; the local input estimate is the current
	// context size, so it replaces rather than adds when API metadata is absent.
	cost, _ := turnUsage.commit(a)
	usageCommitted = true

	// Direct-executor turns stream their text live through the presenter
	// (OnText). Minimal-history router strategies (sub-agent, coordinated)
	// do NOT — they hand the final response back as a plain string, which
	// used to be silently dropped: headless printed NOTHING and the TUI
	// never displayed the routed answer. Deliver it through the presenter
	// exactly when no streaming happened this turn (streamedChars==0 ⟺
	// nothing reached the user yet), so direct turns can't double-print.
	a.deliverUnstreamedResponse(response)

	// End-of-turn hooks: a FailOnError stop hook can request ONE bounded
	// continuation (see runStopHooks).
	if !formatCorrection {
		a.runStopHooks(ctx, response)
	}

	// Tool-budget auto-continuation: a turn that exhausted the per-turn tool
	// budget with UNFINISHED todos queues a synthetic "continue" (fresh turn =
	// fresh budget) so the user doesn't have to type it — bounded per real
	// user request (see budget_auto_continue.go).
	if !formatCorrection {
		a.maybeAutoContinueAfterBudget()
	}

	// Signal completion - copy metadata under lock
	a.mu.Lock()
	duration := time.Since(a.responseStartTime)
	toolsUsed := make([]string, len(a.responseToolsUsed))
	copy(toolsUsed, a.responseToolsUsed)
	a.mu.Unlock()

	// Feed the router's adaptive thinking-budget logic with this turn's
	// activity. Pressure is true when we had to retry the request at least
	// once — a proxy for "budget probably wasn't enough / provider stalled".
	if a.taskRouter != nil {
		pressure := requestRetryCount > 0 || partialIdleRetryCount > 0
		a.taskRouter.RecordTurn(len(toolsUsed), pressure)
	}

	// Record end-to-end latency so /stats can show p50/p95. Per-phase samples
	// (router/LLM/tools) are fed from their respective components — this is
	// the outer wall-clock observation.
	if a.phaseMetrics != nil {
		a.phaseMetrics.Record(PhaseEndToEnd, duration)
	}

	a.safeSendToProgram(ui.ResponseDoneMsg{})

	// Send response metadata with the cost already committed above.
	metadataModel := a.config.Model.Name
	metadataProvider := ""
	fallbackUsed := false
	if turnUsage.samples > 0 && a.executor != nil {
		if provider, model := a.executor.GetLastProviderIdentity(); provider != "" {
			configuredProvider := runtimeProviderForConfig(a.config)
			if !strings.EqualFold(strings.TrimSpace(provider), strings.TrimSpace(configuredProvider)) {
				metadataProvider = provider
				metadataModel = model
				fallbackUsed = true
			}
		}
	}
	a.safeSendToProgram(ui.ResponseMetadataMsg{
		Model:                metadataModel,
		Provider:             metadataProvider,
		FallbackUsed:         fallbackUsed,
		InputTokens:          turnUsage.input,
		OutputTokens:         turnUsage.output,
		CacheReadInputTokens: turnUsage.cacheRead,
		Duration:             duration,
		ToolsUsed:            toolsUsed,
		Cost:                 cost,
	})

	// Notify on long message processing completion (for background terminals)
	if duration > 30*time.Second {
		if nm := a.executor.GetNotificationManager(); nm != nil {
			nm.NotifySuccess("assistant", fmt.Sprintf("Response ready (%s)", formatDuration(duration)), nil, duration)
		}
	}

	// Update todos display
	a.emitTodoUpdate()
}

func (a *App) reportPendingRecoveryClearFailure(headlessTurn *headlessTerminalOutcome, recovery sideEffectRecoveryContext, clearErr error) {
	if clearErr == nil {
		return
	}
	a.journalEvent("side_effect_recovery_clear_failed", map[string]any{
		"recovery_id": recovery.recoveryID,
		"session_id":  recovery.sessionID,
		"error":       clearErr.Error(),
	})
	logging.Warn("successful recovered turn could not clear its durable claim", "error", clearErr)
	// A non-interactive caller must never observe exit 0 after a successful
	// mutation whose durable replay marker is still claimed or commit-uncertain.
	// RunHeadless turns this latched terminal outcome into its non-zero contract.
	a.recordHeadlessTerminalOutcomeForTurn(
		headlessTurn, "recovery_persistence", clearErr.Error())
	message := "Recovered work completed, but its durable claimed marker could not be cleared; automatic retries remain paused — inspect /recovery"
	if errors.Is(clearErr, errRecoveryCommitUncertain) {
		message = "Recovered work completed and its claim was cleared, but storage could not confirm directory durability — avoid restarting until session storage is healthy"
	}
	a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusWarning, Message: message})
}

// resolveTurnTokenUsage selects the metadata shown for one completed user
// request. Provider usage wins; local estimates fill only missing fields. This
// must remain separate from App's cumulative session counters, otherwise each
// response footer re-bills and re-displays every previous turn.
func resolveTurnTokenUsage(apiInput, apiOutput, cacheRead, estimatedContextInput int, response string) (input, output, cached int) {
	input = apiInput
	if input <= 0 {
		input = max(estimatedContextInput, 0)
	}
	output = max(apiOutput, 0)
	if output == 0 && response != "" {
		output = len([]rune(response)) / 4
	}
	cached = min(max(cacheRead, 0), input)
	return input, output, cached
}

// turnUsageAccumulator records every model exchange that belongs to one
// foreground operation. Raw provider input remains separate because the
// cumulative session ledger treats it differently from local estimates;
// resolved input/output are additive and are therefore suitable for a single
// headless invocation, including retries and internal review calls.
type turnUsageAccumulator struct {
	apiInput      int
	input         int
	output        int
	cacheCreation int
	cacheRead     int
	cost          float64
	costTracked   bool
	// costIncomplete is true when at least one sample had no provider price.
	// A mixed priced/unpriced invocation must not advertise a partial sum as a
	// fully tracked cost.
	costIncomplete bool
	samples        int
	modelRounds    int
}

func (u *turnUsageAccumulator) add(
	apiInput, apiOutput, cacheCreation, cacheRead, estimatedInput int,
	response string,
	cost float64,
	costTracked bool,
) {
	if u == nil {
		return
	}
	input, output, cached := resolveTurnTokenUsage(
		apiInput, apiOutput, cacheRead, estimatedInput, response)
	u.apiInput += max(apiInput, 0)
	u.input += input
	u.output += output
	u.cacheCreation += max(cacheCreation, 0)
	u.cacheRead += cached
	if costTracked {
		u.cost += max(cost, 0)
		u.costTracked = true
	} else {
		u.costIncomplete = true
	}
	u.samples++
}

func (u *turnUsageAccumulator) empty() bool {
	return u == nil || u.samples == 0
}

func (u *turnUsageAccumulator) commit(a *App) (float64, bool) {
	if u == nil || a == nil || u.samples == 0 {
		return 0, false
	}
	cost, tracked := a.commitSessionUsage(
		u.apiInput, u.input, u.output, u.cacheCreation, u.cacheRead, u.cost, u.costTracked)
	a.recordHeadlessModelRounds(u.modelRounds)
	if !tracked || (u.costTracked && u.costIncomplete) {
		a.markHeadlessCostIncomplete()
		tracked = false
	}
	return cost, tracked
}

// estimateModelRequestInput is a deterministic per-request fallback used only
// when the provider omits usage metadata. It intentionally counts each retry
// separately; a session-wide context-size lower bound cannot represent an
// invocation that made several model calls.
func estimateModelRequestInput(history []*genai.Content, prompt string) int {
	estimated := appcontext.EstimateContentsTokens(history)
	if prompt != "" {
		estimated += max(len([]rune(prompt))/4, 1)
	}
	return max(estimated, 0)
}

// executeTracked is the single direct-Executor boundary for foreground model
// calls. Its defer captures the executor's final counters on success, error,
// retry, empty response, and panic before the outer recovery path runs.
func (a *App) executeTracked(
	ctx context.Context,
	history []*genai.Content,
	prompt string,
	usage *turnUsageAccumulator,
) (newHistory []*genai.Content, response string, err error) {
	estimatedInput := estimateModelRequestInput(history, prompt)
	defer func() {
		if usage == nil || a == nil || a.executor == nil {
			return
		}
		input, output := a.executor.GetLastTokenUsage()
		cacheCreation, cacheRead := a.executor.GetLastCacheMetrics()
		cost, tracked := a.executor.GetLastEstimatedCost()
		modelRounds := a.executor.GetLastModelTurns()
		// A budget pricing preflight fails before any provider request. Do not
		// feed the local diagnostic marker through the ordinary no-metadata
		// token fallback: that would invent billable usage and session spend for
		// a request that never left the process. A post-response pricing failure
		// has a recorded provider/model identity and remains fully accounted.
		if errors.Is(err, tools.ErrCostUnavailable) {
			provider, model := a.executor.GetLastProviderIdentity()
			if provider == "" && model == "" {
				return
			}
		}
		usage.add(input, output, cacheCreation, cacheRead, estimatedInput, response, cost, tracked)
		usage.modelRounds += max(modelRounds, 0)
	}()
	return a.executor.Execute(ctx, history, prompt)
}

func (a *App) recordHeadlessModelRounds(rounds int) {
	if a == nil || rounds <= 0 {
		return
	}
	a.mu.Lock()
	if a.headlessRunActive {
		a.headlessInvocationUsage.ModelRounds += rounds
	}
	a.mu.Unlock()
}

// accumulateTurnTokenUsage updates the session ledger without mixing the two
// different meanings of input usage. Provider metadata is a billable
// per-request delta and is accumulated. The local fallback is the current
// context size, not a delta; it therefore acts only as a lower bound. Using a
// plain assignment for the fallback made totals go backwards when a provider
// omitted usage after earlier turns had reported it.
func (a *App) accumulateTurnTokenUsage(apiInput, turnInput, turnOutput, cacheCreation, cacheRead int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	turnInput = max(turnInput, 0)
	turnOutput = max(turnOutput, 0)
	cacheRead = min(max(cacheRead, 0), turnInput)
	if a.headlessRunActive {
		// turnInput is the resolved per-request amount: provider metadata when
		// available, otherwise that request's local estimate. Using apiInput here
		// would drop fallback work from a mixed metadata/retry invocation.
		invocationInput := turnInput
		a.headlessInvocationUsage.InputTokens += invocationInput
		a.headlessInvocationUsage.OutputTokens += turnOutput
		a.headlessInvocationUsage.CacheReadInputTokens += min(cacheRead, invocationInput)
		a.headlessInvocationUsage.TotalTokens =
			a.headlessInvocationUsage.InputTokens + a.headlessInvocationUsage.OutputTokens
	}

	if apiInput > 0 {
		a.totalInputTokens += apiInput
	} else if turnInput > a.totalInputTokens {
		a.totalInputTokens = turnInput
	}
	a.totalOutputTokens += turnOutput
	a.totalCacheCreationTokens += max(cacheCreation, 0)
	a.totalCacheReadTokens += cacheRead
	// Cached input is a subset of billable input. Some compatible providers
	// expose cache metrics even when the ordinary input field is absent; keep
	// the public session totals internally consistent in that case.
	if a.totalCacheReadTokens > a.totalInputTokens {
		a.totalInputTokens = a.totalCacheReadTokens
	}
}

// commitSessionUsage applies one top-level request's aggregate usage and cost
// exactly once. It is shared by success and terminal-error paths so retries do
// not disappear merely because the final attempt failed.
func (a *App) commitSessionUsage(apiInput, turnInput, output, cacheCreation, cacheRead int, cost float64, costTracked bool) (float64, bool) {
	apiInput = max(apiInput, 0)
	turnInput = max(turnInput, 0)
	output = max(output, 0)
	cacheRead = min(max(cacheRead, 0), turnInput)
	a.accumulateTurnTokenUsage(apiInput, turnInput, output, cacheCreation, cacheRead)

	if !costTracked && a.contextManager != nil {
		if tc := a.contextManager.GetTokenCounter(); tc != nil {
			cost = tc.CalculateCostWithCache(turnInput, output, cacheRead)
			costTracked = true
		}
	}
	if costTracked {
		cost = a.commitTrackedCost(cost)
	}
	return cost, costTracked
}

// commitTrackedCost applies a provider-priced (including known-zero local)
// cost to both the cumulative session ledger and the active headless
// invocation. Callers must only use this when pricing is known.
func (a *App) commitTrackedCost(cost float64) float64 {
	cost = max(cost, 0)
	a.mu.Lock()
	a.totalEstimatedCost += cost
	a.costTracked = true
	if a.headlessRunActive {
		a.headlessInvocationCost.EstimatedUSD += cost
		a.headlessInvocationCost.Tracked = true
	}
	a.mu.Unlock()
	return cost
}

func (a *App) markHeadlessCostIncomplete() {
	a.mu.Lock()
	if a.headlessRunActive {
		a.headlessCostIncomplete = true
		a.headlessInvocationCost.Tracked = false
	}
	a.mu.Unlock()
}

// accumulateAgentResultUsage folds one delegated agent invocation into the
// session ledger. Delegated model calls bypass Executor, so without this hook
// /stats omitted their usage entirely. Call once for every Spawn attempt,
// including failed attempts: providers bill those completed model rounds too.
func (a *App) accumulateAgentResultUsage(result *agent.AgentResult) {
	// Retained as the internal/test compatibility seam. Unscoped results update
	// cumulative session totals but can never be attributed to whichever
	// headless invocation happens to be active when an old async agent finishes.
	a.accumulateScopedAgentResultUsage(result)
}

// emitTodoUpdate renders the current todo list and pushes it to the UI checklist
// widget. Called both at turn finalization and live (the instant the todo tool
// runs, via OnToolEnd) so the user can watch items flip in real time.
func (a *App) emitTodoUpdate() {
	todoTool, ok := a.registry.Get("todo")
	if !ok {
		return
	}
	tt, ok := todoTool.(*tools.TodoTool)
	if !ok {
		return
	}
	items := tt.GetItems()
	display := make([]string, 0, len(items))
	for _, item := range items {
		var icon string
		switch item.Status {
		case "in_progress":
			icon = "◐"
		case "completed":
			icon = "●"
		default:
			icon = "○"
		}
		display = append(display, fmt.Sprintf("%s %s", icon, item.Content))
	}
	a.safeSendToProgram(ui.TodoUpdateMsg(display))

	// Live "doing X" status: surface the in-progress item's active form so the
	// status bar always shows WHAT the agent is working on ("Implementing
	// backup/restore"), not just a generic "Generating". Empty when nothing is
	// in progress — the label falls back to the phase-aware default and is
	// cleared at turn end.
	activity := ""
	if cur := tt.GetCurrentTask(); cur != nil {
		if activity = cur.ActiveForm; activity == "" {
			activity = cur.Content
		}
	}
	a.safeSendToProgram(ui.ActivityLabelMsg(activity))
}

// executePlanWithClearContext dispatches plan execution to either delegated
// sub-agent mode or direct monolithic execution.
func (a *App) executePlanWithClearContext(ctx context.Context, approvedPlan *plan.Plan) {
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	a.startPlanWatchdog(execCtx, execCancel, approvedPlan.ID)

	// Enter execution mode - this blocks creation of new plans during execution
	if a.planManager != nil {
		a.planManager.SetExecutionMode(true)
		if err := a.planManager.TransitionCurrentPlanLifecycle(plan.LifecycleExecuting); err != nil {
			logging.Warn("failed to transition plan lifecycle to executing", "error", err)
		}
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

	if err := a.preparePlanExecutionBoundary(approvedPlan); err != nil {
		logging.Warn("plan execution blocked: context boundary was not durable", "error", err)
		if a.planManager != nil {
			a.planManager.PausePlan()
			a.planManager.SetExecutionMode(false)
			a.planManager.SetCurrentStepID(-1)
		}
		a.recordHeadlessTerminalOutcome("persistence", err.Error())
		a.safeSendToProgram(ui.StatusUpdateMsg{
			Type:    ui.StatusWarning,
			Message: "Plan execution was paused because its fresh context could not be saved safely; fix session storage and resume the plan",
		})
		a.safeSendToProgram(ui.ErrorMsg(err))
		a.safeSendToProgram(ui.ResponseDoneMsg{})
		return
	}

	// Capture the exact shared undo-history delta for this execution segment.
	// The plan may pause and resume across multiple turns; the plan extension
	// appends each segment's stable change IDs to one transaction.
	if a.planManager != nil && a.undoManager != nil {
		if err := a.planManager.BeginPlanUndoCapture(a.undoManager.Snapshot()); err != nil {
			logging.Warn("plan file-change undo capture unavailable",
				"plan_id", approvedPlan.ID,
				"error", err)
		} else {
			// The closure is load-bearing: deferred CALL ARGUMENTS are evaluated
			// at defer time, so `defer Finish(undoManager.Snapshot())` would
			// capture the pre-execution snapshot as the "after" side and make
			// every plan transaction empty — undo_plan would find nothing to
			// revert.
			defer func() {
				a.planManager.FinishPlanUndoCapture(a.undoManager.Snapshot())
			}()
		}
	}

	delegated := a.config.Plan.DelegateSteps && a.agentRunner != nil
	if delegated && a.shouldUseSafeMode() {
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("⚠️ Safe mode active (%v): running plan without delegation.\n", a.reliability.DegradedRemaining())))
		delegated = false
	}

	if delegated {
		a.executePlanDelegated(execCtx, approvedPlan)
	} else {
		a.executePlanDirectly(execCtx, approvedPlan)
	}
}

func (a *App) preparePlanExecutionBoundary(approvedPlan *plan.Plan) error {
	if approvedPlan == nil {
		return fmt.Errorf("approved plan is unavailable")
	}
	contextSnapshot := a.extractContextSnapshot()
	if contextSnapshot != "" {
		approvedPlan.SetContextSnapshot(contextSnapshot)
		logging.Debug("context snapshot saved",
			"plan_id", approvedPlan.ID, "snapshot_len", len(contextSnapshot))
	}

	stepInfos := make([]appcontext.PlanStepInfo, 0, len(approvedPlan.Steps))
	for _, step := range approvedPlan.Steps {
		stepInfos = append(stepInfos, appcontext.PlanStepInfo{
			ID:          step.ID,
			Title:       step.Title,
			Description: step.Description,
		})
	}
	planPrompt := a.promptBuilder.BuildPlanExecutionPromptWithContext(
		approvedPlan.Title, approvedPlan.Description, stepInfos, contextSnapshot)
	if a.planManager != nil {
		if err := a.saveCurrentPlanWithVisibility("plan execution boundary"); err != nil {
			return fmt.Errorf("save approved plan before context replacement: %w", err)
		}
	}
	if _, err := a.replaceConversationPersistenceBoundary(func() {
		a.session.AddUserMessage(planPrompt)
		a.session.AddModelMessage("I understand the approved plan. I will execute each step as instructed.")
	}); err != nil {
		return fmt.Errorf("persist plan context boundary: %w", err)
	}
	return nil
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

// executePlanDirectly executes an approved plan step-by-step using the main
// session executor. Each step gets a targeted prompt, and the orchestrator
// manages progress, SharedMemory, and auto-completion automatically.
// Unlike delegated mode, all steps share the same session history for continuity.
func (a *App) executePlanDirectly(ctx context.Context, approvedPlan *plan.Plan) {
	planStart := time.Now()

	logging.Debug("executing plan directly (step-by-step)",
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

	// Skip diff approval prompts — the plan itself was already approved
	ctx = tools.ContextWithSkipDiff(ctx)

	totalSteps := len(approvedPlan.Steps)

	// Get SharedMemory for inter-step communication
	var sharedMem *agent.SharedMemory
	if a.agentRunner != nil {
		sharedMem = a.agentRunner.GetSharedMemory()
	}

	// 5. Notify UI with plan banner and step overview
	var bannerBuf strings.Builder
	fmt.Fprintf(&bannerBuf, "\n━━━ Executing plan: %s (%d steps) ━━━\n", approvedPlan.Title, totalSteps)
	if approvedPlan.Description != "" {
		fmt.Fprintf(&bannerBuf, "    %s\n", approvedPlan.Description)
	}
	bannerBuf.WriteString("\n")
	for i, s := range approvedPlan.Steps {
		fmt.Fprintf(&bannerBuf, "  %d. %s\n", i+1, s.Title)
	}
	bannerBuf.WriteString("\n")
	a.safeSendToProgram(ui.StreamTextMsg(bannerBuf.String()))

	// 6. Execute steps using NextReadySteps for dependency-aware + parallel execution
	const maxRetries = 3
	backoffDurations := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

	// Auto-resume: when all ready steps are exhausted but paused steps remain,
	// wait a cooldown period and retry them automatically.
	const maxAutoResumeRounds = 2
	const autoResumeCooldown = 60 * time.Second
	autoResumeCount := 0
	maxExecutionRounds := maxPlanExecutionRounds(totalSteps)
	executionRounds := 0

	for {
		executionRounds++
		if executionRounds > maxExecutionRounds {
			a.planManager.PausePlan()
			a.safeSendToProgram(ui.StreamTextMsg(
				"\n⏸ Plan paused — execution safety limit reached. Use /resume-plan to continue.\n"))
			a.safeSendToProgram(ui.PlanProgressMsg{
				PlanID:     approvedPlan.ID,
				TotalSteps: totalSteps,
				Completed:  approvedPlan.CompletedCount(),
				Progress:   approvedPlan.Progress(),
				Status:     "paused",
				Reason:     "execution safety limit reached",
			})
			a.safeSendToProgram(ui.ResponseDoneMsg{})
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		readySteps := approvedPlan.NextReadySteps()
		if len(readySteps) == 0 {
			// No ready steps. Check if there are paused steps to auto-resume.
			if approvedPlan.HasPausedSteps() && autoResumeCount < maxAutoResumeRounds {
				autoResumeCount++
				a.safeSendToProgram(ui.StreamTextMsg(
					fmt.Sprintf("\n⏳ Waiting %v before auto-resuming paused steps (round %d/%d)...\n",
						autoResumeCooldown, autoResumeCount, maxAutoResumeRounds)))

				cooldownTimer := time.NewTimer(autoResumeCooldown)
				select {
				case <-cooldownTimer.C:
				case <-ctx.Done():
					cooldownTimer.Stop()
					return
				}

				resumed := a.planManager.ResumePausedSteps()
				a.safeSendToProgram(ui.StreamTextMsg(
					fmt.Sprintf("🔄 Resumed %d paused step(s)\n\n", resumed)))
				continue
			}

			// Auto-resume exhausted or no paused steps — exit loop
			if approvedPlan.HasPausedSteps() {
				a.planManager.PausePlan()
				a.safeSendToProgram(ui.StreamTextMsg(
					"\n⏸ Plan paused — auto-resume exhausted. Use /resume-plan to continue.\n"))
				a.safeSendToProgram(ui.PlanProgressMsg{
					PlanID:     approvedPlan.ID,
					TotalSteps: totalSteps,
					Completed:  approvedPlan.CompletedCount(),
					Progress:   approvedPlan.Progress(),
					Status:     "paused",
					Reason:     "auto-resume exhausted",
				})
				a.safeSendToProgram(ui.ResponseDoneMsg{})
				return
			}
			break // All steps done — proceed to summary
		}

		// Direct mode shares a single session, so run sequentially for reliability.
		for _, step := range readySteps {
			a.executeDirectStep(ctx, step, approvedPlan, totalSteps, sharedMem, maxRetries, backoffDurations)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}

	// Enforce final verification gate before producing terminal plan summary.
	if !a.enforceDoneGate(ctx, approvedPlan.Request) {
		return
	}

	// 7. Plan completion summary
	planDuration := time.Since(planStart)
	summary := a.formatPlanSummary(approvedPlan, planDuration)
	a.updateWorkingMemoryFromTurn(approvedPlan.Request, summary)
	a.safeSendToProgram(ui.StreamTextMsg(summary))

	completedCount := approvedPlan.CompletedCount()
	statusText := "completed"
	if completedCount < approvedPlan.StepCount() {
		statusText = "paused"
	}

	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:     approvedPlan.ID,
		TotalSteps: approvedPlan.StepCount(),
		Completed:  completedCount,
		Progress:   approvedPlan.Progress(),
		Status:     statusText,
		Reason:     "plan execution round finished",
	})

	// Send response metadata so UI shows duration and token usage
	a.mu.Lock()
	inputTokens := a.totalInputTokens
	outputTokens := a.totalOutputTokens
	a.mu.Unlock()
	a.safeSendToProgram(ui.ResponseMetadataMsg{
		Model:        a.config.Model.Name,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Duration:     planDuration,
	})

	a.safeSendToProgram(ui.ResponseDoneMsg{})
	a.journalEvent("request_completed", map[string]any{
		"message_preview": previewForJournal(approvedPlan.Request),
	})
	a.enforceSessionMemoryGovernance("request_completed")

	a.finalizePlanLifecycleState(approvedPlan)

	// Clear plan if fully completed
	if approvedPlan.IsComplete() && a.planManager != nil {
		a.planManager.ClearPlan()
		logging.Debug("plan auto-cleared after completion", "plan_id", approvedPlan.ID)
	}

	// Save session
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Warn("failed to save session after message", "error", err)
		}
	}
}

// getStepTimeout returns the timeout to use for a step.
func (a *App) getStepTimeout(step *plan.Step) time.Duration {
	if step.Timeout > 0 {
		return step.Timeout
	}
	if a.config.Plan.DefaultStepTimeout > 0 {
		return a.config.Plan.DefaultStepTimeout
	}
	timeout := config.DefaultAgentTimeout
	if a.executor != nil {
		minimum := a.executor.ModelRoundTimeout() + config.DefaultAgentTimeoutHeadroom
		if minimum > timeout {
			timeout = minimum
		}
	}
	return timeout
}

// executeDirectStep executes a single step in the direct (same-session) mode.
func (a *App) executeDirectStep(ctx context.Context, step *plan.Step, approvedPlan *plan.Plan, totalSteps int, sharedMem *agent.SharedMemory, maxRetries int, backoffDurations []time.Duration) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Evaluate conditional steps
	if step.Condition != "" && step.ShouldSkip(approvedPlan) {
		a.planManager.SkipStep(step.ID)
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("  Step %d skipped (condition: %s)\n", step.ID, step.Condition)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "skipped",
			Reason:        step.Condition,
		})
		return
	}
	step.EnsureContractDefaults()

	// Idempotency guard: if previous attempt already produced side effects for
	// this step but never reached completion, avoid automatic re-execution.
	if approvedPlan.HasPartialEffects(step.ID) || approvedPlan.HasDuplicateRisk(step.ID) {
		reason := "partial effects from previous attempt detected"
		if approvedPlan.HasDuplicateRisk(step.ID) {
			reason = "duplicate side effects detected across retries"
		}
		a.planManager.PauseStep(step.ID, reason)
		a.journalEvent("plan_step_paused", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"reason":  reason,
		})
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("⏸ Step %d paused for safety: %s. Review and /resume-plan when ready.\n", step.ID, reason)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "paused",
			Reason:        reason,
		})
		return
	}

	if requiresHumanCheckpoint(step) && !step.CheckpointPassed {
		reason := "checkpoint required: high-risk step needs operator confirmation"
		a.planManager.PauseStep(step.ID, reason)
		a.journalEvent("plan_checkpoint_pause", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"title":   step.Title,
		})
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("⏸ Step %d requires checkpoint approval.\nWhy: %s\nRun /resume-plan to approve and continue.\n",
				step.ID, step.Title)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "paused",
			Reason:        reason,
		})
		return
	}

	// Compact history if needed before executing next step
	if a.contextManager != nil {
		if err := a.contextManager.PrepareForRequest(ctx); err != nil {
			logging.Debug("failed to prepare context before step", "step_id", step.ID, "error", err)
		}
	}

	// Mark step as started and track current step ID
	a.planManager.StartStep(step.ID)
	a.planManager.SetCurrentStepID(step.ID)
	a.refreshSystemInstruction()
	a.touchStepHeartbeat()
	a.journalEvent("plan_step_started", map[string]any{
		"plan_id":   approvedPlan.ID,
		"step_id":   step.ID,
		"step":      step.Title,
		"execution": "direct",
	})
	a.saveRecoverySnapshot()
	a.startStepRollbackSnapshot(approvedPlan, step)

	// Update plan progress in status bar
	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:        approvedPlan.ID,
		CurrentStepID: step.ID,
		CurrentTitle:  step.Title,
		TotalSteps:    totalSteps,
		Completed:     approvedPlan.CompletedCount(),
		Progress:      approvedPlan.Progress(),
		Status:        "in_progress",
		Reason:        "step started",
	})

	var headerBuf strings.Builder
	fmt.Fprintf(&headerBuf, "──── Step %d/%d: %s ────\n", step.ID, totalSteps, step.Title)
	if step.Description != "" {
		fmt.Fprintf(&headerBuf, "     %s\n", step.Description)
	}
	a.safeSendToProgram(ui.StreamTextMsg(headerBuf.String()))

	// Build step-specific prompt with context from previous steps
	prevSummary := a.planManager.GetPreviousStepsSummary(step.ID, planSummaryMaxChars)
	stepMsg := buildDirectStepMessage(step, prevSummary, totalSteps)

	// Per-step timeout
	stepTimeout := a.getStepTimeout(step)
	stepCtx, stepCancel := context.WithTimeout(ctx, stepTimeout)
	defer stepCancel()

	// Execute step with retry logic
	var response string
	var err error
	errCat := plan.ErrorUnknown
	var stepUsage turnUsageAccumulator
	usageCommitted := false
	commitStepUsage := func() {
		if usageCommitted || stepUsage.empty() {
			return
		}
		stepUsage.commit(a)
		step.TokensUsed = stepUsage.input + stepUsage.output
		approvedPlan.SetStepUsage(step.ID, step.TokensUsed)
		usageCommitted = true
	}
	// executeTracked records usage even when Executor.Execute panics. Commit the
	// captured sample while unwinding so a recovered plan step never loses work
	// that reached the model. The explicit commit after the loop keeps normal
	// failure/verification returns observable before their state transition.
	defer commitStepUsage()

	for attempt := range maxRetries {
		history, histVersion := a.session.GetHistoryWithVersion()
		var newHistory []*genai.Content
		execFn := func() error {
			newHistory, response, err = a.executeTracked(
				stepCtx, history, stepMsg, &stepUsage)
			return err
		}
		if a.policy != nil {
			err = a.policy.ExecutePlanStep(stepCtx, execFn)
		} else {
			err = execFn()
		}

		if err == nil {
			// Success — update session history with version check.
			// For parallel plan steps, another step may have updated history
			// while we were executing. In that case, append only the new entries
			// that our execution produced to the current history.
			if !a.session.SetHistoryIfVersion(newHistory, histVersion) {
				// Version changed — merge by appending our new entries
				delta := newHistory[len(history):]
				currentHistory := a.session.GetHistory()
				merged := make([]*genai.Content, len(currentHistory)+len(delta))
				copy(merged, currentHistory)
				copy(merged[len(currentHistory):], delta)
				a.session.SetHistory(merged)
			}
			break
		}

		// Classify the error
		errCat = plan.ClassifyError(err, err.Error())
		if errors.Is(err, ErrStepCircuitOpen) {
			errCat = plan.ErrorTransient
		}

		// Retry only on transient errors
		if errCat == plan.ErrorTransient && attempt < maxRetries-1 {
			// executeTracked may have completed one or more tool calls before the
			// provider failed on a later model turn. Retrying the whole step in
			// that state can repeat writes, commands, or external actions with a
			// fresh tool-call ID, bypassing the executor's per-call deduplication.
			// Stop at the attempt boundary and require an explicit resume instead.
			if approvedPlan.HasPartialEffects(step.ID) || approvedPlan.HasDuplicateRisk(step.ID) {
				reason := fmt.Sprintf(
					"automatic retry blocked after attempt %d: tool effects were recorded before the transient error; review the workspace and resume explicitly",
					attempt+1,
				)
				if approvedPlan.HasDuplicateRisk(step.ID) {
					reason = fmt.Sprintf(
						"automatic retry blocked after attempt %d: duplicate tool effects were detected; review the workspace and resume explicitly",
						attempt+1,
					)
				}
				commitStepUsage()
				if a.reliability != nil && !errors.Is(err, context.Canceled) {
					a.reliability.RecordFailure()
				}
				a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, reason)
				logging.Warn("step retry blocked after partial tool effects",
					"step_id", step.ID, "attempt", attempt+1, "error", err.Error())
				return
			}

			backoff := backoffDurations[attempt]
			logging.Warn("step execution error, retrying",
				"step_id", step.ID, "attempt", attempt+1, "error", err.Error(),
				"category", errCat.String(), "backoff", backoff)
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRetry,
				Message: fmt.Sprintf("Step %d failed — retry %d/%d in %v", step.ID, attempt+1, maxRetries, backoff),
				Details: map[string]any{"attempt": attempt + 1, "maxAttempts": maxRetries},
			})

			backoffTimer := time.NewTimer(backoff)
			select {
			case <-backoffTimer.C:
				continue
			case <-ctx.Done():
				backoffTimer.Stop()
				err = ctx.Err()
				errCat = plan.ClassifyError(err, err.Error())
				break
			}
		}
		break
	}
	commitStepUsage()

	// Handle step failure
	if err != nil {
		errMsg := err.Error()
		if a.reliability != nil && !errors.Is(err, context.Canceled) {
			a.reliability.RecordFailure()
		}

		// Transient error after all attempts → pause step, plan continues with other steps
		if errCat == plan.ErrorTransient {
			reason := fmt.Sprintf("%s (after %d attempts; will auto-retry later)", errMsg, maxRetries)
			a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, reason)

			logging.Info("step paused due to transient error, plan continues",
				"step_id", step.ID, "error", errMsg, "category", errCat.String())
			return
		}

		// Fatal/logic/unknown error
		a.planManager.FailStep(step.ID, errMsg)
		a.commitStepRollbackSnapshot(approvedPlan.ID, step.ID)
		a.journalEvent("plan_step_failed", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"reason":  errMsg,
		})

		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("\n  Step %d failed (%s): %s\n", step.ID, errCat.String(), errMsg)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "failed",
			Reason:        errMsg,
		})

		// Attempt adaptive replan on fatal errors
		if errCat == plan.ErrorFatal && a.planManager.HasReplanHandler() {
			if replanErr := a.planManager.RequestReplan(ctx, step); replanErr == nil {
				a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusRetry, Message: "Plan adjusted after step failure — continuing"})
				logging.Info("plan replanned after fatal step error",
					"step_id", step.ID, "plan_version", approvedPlan.Version)
				return // Don't abort — the loop will pick up new steps
			} else {
				logging.Warn("replan attempt failed", "error", replanErr)
			}
		}

		if a.config.Plan.AbortOnStepFailure {
			a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusRecoverableError, Message: "Aborting plan due to step failure"})
		}
		return
	}

	// Step succeeded — store output and mark complete
	if a.reliability != nil {
		a.reliability.RecordSuccess()
	}

	output := response
	if runes := []rune(output); len(runes) > planStepOutputMaxChars {
		output = string(runes[:planStepOutputMaxChars]) + "..."
	}

	verificationSummary, verificationOutput, verificationOK, verificationReason := a.runStepVerificationCommands(ctx, approvedPlan, step)
	if !verificationOK {
		a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, verificationReason)
		return
	}
	if verificationOutput != "" {
		if output != "" {
			output += "\n\n"
		}
		output += verificationOutput
		if runes := []rune(output); len(runes) > planStepOutputMaxChars {
			output = string(runes[:planStepOutputMaxChars]) + "..."
		}
	}
	if verificationSummary != "" {
		a.journalEvent("plan_step_verification_passed", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"summary": verificationSummary,
		})
	}

	evidence, verificationNote, evidenceOK, evidenceReason := a.buildStepCompletionEvidence(approvedPlan, step, output)
	if !evidenceOK {
		a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, evidenceReason)
		return
	}
	a.planManager.RecordStepVerification(step.ID, evidence, verificationNote)

	a.planManager.CompleteStep(step.ID, output)
	a.commitStepRollbackSnapshot(approvedPlan.ID, step.ID)
	a.journalEvent("plan_step_completed", map[string]any{
		"plan_id": approvedPlan.ID,
		"step_id": step.ID,
		"output":  previewForJournal(output),
	})

	// Store step result in SharedMemory for inter-step communication
	if sharedMem != nil {
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

	a.safeSendToProgram(ui.StreamTextMsg(
		fmt.Sprintf("  Step %d complete: %s\n\n", step.ID, step.Title)))
	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:        approvedPlan.ID,
		CurrentStepID: step.ID,
		CurrentTitle:  step.Title,
		TotalSteps:    totalSteps,
		Completed:     approvedPlan.CompletedCount(),
		Progress:      approvedPlan.Progress(),
		Status:        "completed",
		Reason:        "step completed",
	})

	// Update token count after each step
	if a.contextManager != nil {
		if err := a.contextManager.UpdateTokenCount(ctx); err != nil {
			logging.Debug("failed to update token count", "error", err)
		}
		a.sendTokenUsageUpdate()
	}

	// Save session after each completed step (crash recovery)
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Warn("failed to save session after message", "error", err)
		}
	}
	a.enforceSessionMemoryGovernance("plan_step_completed")
}

// buildDirectStepMessage creates a focused prompt for executing a single step
// in the direct (same-session) execution mode.
func buildDirectStepMessage(step *plan.Step, prevSummary string, totalSteps int) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Execute step %d of %d: **%s**\n\n", step.ID, totalSteps, step.Title)

	if step.Description != "" {
		sb.WriteString(step.Description)
		sb.WriteString("\n\n")
	}

	sb.WriteString("Step Contract:\n")
	if len(step.Inputs) > 0 {
		sb.WriteString("- Inputs:\n")
		for _, in := range step.Inputs {
			fmt.Fprintf(&sb, "  - %s\n", in)
		}
	}
	if step.ExpectedArtifact != "" {
		fmt.Fprintf(&sb, "- Expected artifact: %s\n", step.ExpectedArtifact)
	}
	if len(step.ExpectedArtifactPaths) > 0 {
		sb.WriteString("- Expected artifact paths:\n")
		for _, path := range step.ExpectedArtifactPaths {
			fmt.Fprintf(&sb, "  - %s\n", path)
		}
	}
	if len(step.SuccessCriteria) > 0 {
		sb.WriteString("- Success criteria:\n")
		for _, c := range step.SuccessCriteria {
			fmt.Fprintf(&sb, "  - %s\n", c)
		}
	}
	if len(step.VerifyCommands) > 0 {
		sb.WriteString("- Verify commands:\n")
		for _, cmd := range step.VerifyCommands {
			fmt.Fprintf(&sb, "  - %s\n", cmd)
		}
	}
	if step.Rollback != "" {
		fmt.Fprintf(&sb, "- Rollback: %s\n", step.Rollback)
	}
	sb.WriteString("\n")

	if contractJSON := buildStepContractJSON(step, totalSteps); contractJSON != "" {
		sb.WriteString("StepContractJSON:\n")
		sb.WriteString("```json\n")
		sb.WriteString(contractJSON)
		sb.WriteString("\n```\n\n")
	}

	if prevSummary != "" {
		sb.WriteString("Previous steps summary:\n")
		sb.WriteString(prevSummary)
		sb.WriteString("\n")
	}

	sb.WriteString("Rules:\n")
	sb.WriteString("- Execute ONLY this step, nothing else\n")
	sb.WriteString("- Always READ files before editing them\n")
	sb.WriteString("- Do NOT call update_plan_progress or exit_plan_mode — the orchestrator handles this\n")
	sb.WriteString("- Provide a brief summary of what was done at the end\n")
	sb.WriteString("- Include evidence: changed files, executed commands, and verification facts\n")
	sb.WriteString("- Treat verify_commands as mandatory completion gate\n")
	sb.WriteString("- Report any issues or deviations from the plan\n")

	return sb.String()
}

// executePlanDelegated executes an approved plan by spawning a sub-agent per step.
// Each step runs in isolation with project context injected, and only compact
// summaries are stored in the main session.
func (a *App) executePlanDelegated(ctx context.Context, approvedPlan *plan.Plan) {
	planStart := time.Now()

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

	// Notify UI with plan banner and step overview
	var delegatedBannerBuf strings.Builder
	fmt.Fprintf(&delegatedBannerBuf, "\n━━━ Executing plan: %s (%d steps, delegated) ━━━\n", approvedPlan.Title, totalSteps)
	if approvedPlan.Description != "" {
		fmt.Fprintf(&delegatedBannerBuf, "    %s\n", approvedPlan.Description)
	}
	delegatedBannerBuf.WriteString("\n")
	for i, s := range approvedPlan.Steps {
		fmt.Fprintf(&delegatedBannerBuf, "  %d. %s\n", i+1, s.Title)
	}
	delegatedBannerBuf.WriteString("\n")
	a.safeSendToProgram(ui.StreamTextMsg(delegatedBannerBuf.String()))

	// Auto-resume: when all ready steps are exhausted but paused steps remain,
	// wait a cooldown period and retry them automatically.
	const maxAutoResumeRounds = 2
	const autoResumeCooldown = 60 * time.Second
	autoResumeCount := 0
	maxExecutionRounds := maxPlanExecutionRounds(totalSteps)
	executionRounds := 0

	for {
		executionRounds++
		if executionRounds > maxExecutionRounds {
			a.planManager.PausePlan()
			a.safeSendToProgram(ui.StreamTextMsg(
				"\n⏸ Plan paused — execution safety limit reached. Use /resume-plan to continue.\n"))
			a.safeSendToProgram(ui.PlanProgressMsg{
				PlanID:     approvedPlan.ID,
				TotalSteps: totalSteps,
				Completed:  approvedPlan.CompletedCount(),
				Progress:   approvedPlan.Progress(),
				Status:     "paused",
				Reason:     "execution safety limit reached",
			})
			a.safeSendToProgram(ui.ResponseDoneMsg{})
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
		}

		readySteps := approvedPlan.NextReadySteps()
		if len(readySteps) == 0 {
			// No ready steps. Check if there are paused steps to auto-resume.
			if approvedPlan.HasPausedSteps() && autoResumeCount < maxAutoResumeRounds {
				autoResumeCount++
				a.safeSendToProgram(ui.StreamTextMsg(
					fmt.Sprintf("\n⏳ Waiting %v before auto-resuming paused steps (round %d/%d)...\n",
						autoResumeCooldown, autoResumeCount, maxAutoResumeRounds)))

				cooldownTimer := time.NewTimer(autoResumeCooldown)
				select {
				case <-cooldownTimer.C:
				case <-ctx.Done():
					cooldownTimer.Stop()
					return
				}

				resumed := a.planManager.ResumePausedSteps()
				a.safeSendToProgram(ui.StreamTextMsg(
					fmt.Sprintf("🔄 Resumed %d paused step(s)\n\n", resumed)))
				continue
			}

			// Auto-resume exhausted or no paused steps — exit loop
			if approvedPlan.HasPausedSteps() {
				a.planManager.PausePlan()
				a.safeSendToProgram(ui.StreamTextMsg(
					"\n⏸ Plan paused — auto-resume exhausted. Use /resume-plan to continue.\n"))
				a.safeSendToProgram(ui.PlanProgressMsg{
					PlanID:     approvedPlan.ID,
					TotalSteps: totalSteps,
					Completed:  approvedPlan.CompletedCount(),
					Progress:   approvedPlan.Progress(),
					Status:     "paused",
					Reason:     "auto-resume exhausted",
				})
				a.safeSendToProgram(ui.ResponseDoneMsg{})
				return
			}
			break // All steps done — proceed to summary
		}

		// Delegated steps publish tool activity through one process-wide callback.
		// Running ready steps concurrently makes the callback's single
		// CurrentStepID ambiguous, so writes can be attributed to the wrong ledger
		// (or skipped), defeating rollback and retry idempotency. Keep delegation
		// sequential until activity events carry an explicit step identity.
		if len(readySteps) > 1 {
			a.safeSendToProgram(ui.StatusUpdateMsg{
				Type:    ui.StatusRetry,
				Message: "Running delegated steps sequentially to preserve rollback and retry safety",
			})
		}
		for _, step := range readySteps {
			if ctx.Err() != nil {
				return
			}
			a.executeDelegatedStep(ctx, step, approvedPlan, totalSteps, sharedMem, contextSnapshot)
		}
	}

	// Enforce final verification gate before producing terminal plan summary.
	if !a.enforceDoneGate(ctx, approvedPlan.Request) {
		return
	}

	// Plan completion summary
	planDuration := time.Since(planStart)
	summary := a.formatPlanSummary(approvedPlan, planDuration)
	a.updateWorkingMemoryFromTurn(approvedPlan.Request, summary)
	a.safeSendToProgram(ui.StreamTextMsg(summary))

	delegatedCompletedCount := approvedPlan.CompletedCount()
	delegatedStatusText := "completed"
	if delegatedCompletedCount < approvedPlan.StepCount() {
		delegatedStatusText = "paused"
	}

	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:     approvedPlan.ID,
		TotalSteps: approvedPlan.StepCount(),
		Completed:  delegatedCompletedCount,
		Progress:   approvedPlan.Progress(),
		Status:     delegatedStatusText,
		Reason:     "plan execution round finished",
	})

	// Send response metadata so UI shows duration
	a.safeSendToProgram(ui.ResponseMetadataMsg{
		Model:    a.config.Model.Name,
		Duration: planDuration,
	})

	a.safeSendToProgram(ui.ResponseDoneMsg{})

	a.finalizePlanLifecycleState(approvedPlan)

	// Clear plan if fully completed
	if approvedPlan.IsComplete() && a.planManager != nil {
		a.planManager.ClearPlan()
		logging.Debug("plan auto-cleared after completion", "plan_id", approvedPlan.ID)
	}

	// Note: SetExecutionMode(false) is handled by defer at function start

	// Save session
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Warn("failed to save session after message", "error", err)
		}
	}
}

// executeDelegatedStep executes a single step via sub-agent delegation.
func (a *App) executeDelegatedStep(ctx context.Context, step *plan.Step, approvedPlan *plan.Plan, totalSteps int, sharedMem *agent.SharedMemory, contextSnapshot string) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Evaluate conditional steps
	if step.Condition != "" && step.ShouldSkip(approvedPlan) {
		a.planManager.SkipStep(step.ID)
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("  Step %d skipped (condition: %s)\n", step.ID, step.Condition)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "skipped",
			Reason:        step.Condition,
		})
		return
	}
	step.EnsureContractDefaults()

	// Idempotency guard for delegated execution.
	if approvedPlan.HasPartialEffects(step.ID) || approvedPlan.HasDuplicateRisk(step.ID) {
		reason := "partial effects from previous attempt detected"
		if approvedPlan.HasDuplicateRisk(step.ID) {
			reason = "duplicate side effects detected across retries"
		}
		a.planManager.PauseStep(step.ID, reason)
		a.journalEvent("plan_step_paused", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"reason":  reason,
		})
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("⏸ Step %d paused for safety: %s. Review and /resume-plan when ready.\n", step.ID, reason)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "paused",
			Reason:        reason,
		})
		return
	}

	if requiresHumanCheckpoint(step) && !step.CheckpointPassed {
		reason := "checkpoint required: high-risk step needs operator confirmation"
		a.planManager.PauseStep(step.ID, reason)
		a.journalEvent("plan_checkpoint_pause", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"title":   step.Title,
		})
		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("⏸ Step %d requires checkpoint approval.\nWhy: %s\nRun /resume-plan to approve and continue.\n",
				step.ID, step.Title)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "paused",
			Reason:        reason,
		})
		return
	}

	// Mark step as started and track current step ID
	a.planManager.StartStep(step.ID)
	a.planManager.SetCurrentStepID(step.ID)
	// See the field doc on inFlightDelegatedSteps: gates whether
	// handleSubAgentActivity can safely attribute this sub-agent's tool
	// calls to THIS step (only when exactly one delegated step is in
	// flight — parallel-ready-steps execution makes GetCurrentStepID()
	// ambiguous).
	a.inFlightDelegatedSteps.Add(1)
	defer a.inFlightDelegatedSteps.Add(-1)
	a.refreshSystemInstruction()
	a.touchStepHeartbeat()
	a.journalEvent("plan_step_started", map[string]any{
		"plan_id":   approvedPlan.ID,
		"step_id":   step.ID,
		"step":      step.Title,
		"execution": "delegated",
	})
	a.saveRecoverySnapshot()
	a.startStepRollbackSnapshot(approvedPlan, step)

	// Update plan progress in status bar
	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:        approvedPlan.ID,
		CurrentStepID: step.ID,
		CurrentTitle:  step.Title,
		TotalSteps:    totalSteps,
		Completed:     approvedPlan.CompletedCount(),
		Progress:      approvedPlan.Progress(),
		Status:        "in_progress",
		Reason:        "step started",
	})

	// Notify UI of step start with structured header
	var delegatedHeaderBuf strings.Builder
	fmt.Fprintf(&delegatedHeaderBuf, "──── Step %d/%d: %s ────\n", step.ID, totalSteps, step.Title)
	if step.Description != "" {
		fmt.Fprintf(&delegatedHeaderBuf, "     %s\n", step.Description)
	}
	a.safeSendToProgram(ui.StreamTextMsg(delegatedHeaderBuf.String()))

	// Build step prompt with full plan context
	prevSummary := a.planManager.GetPreviousStepsSummary(step.ID, planSummaryMaxChars)
	projectCtx := ""
	if a.promptBuilder != nil {
		memoryQuery := strings.Join([]string{
			approvedPlan.Request,
			approvedPlan.Title,
			step.Title,
			step.Description,
		}, " ")
		projectCtx = a.buildSubAgentProjectContext(memoryQuery)
	}

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
		a.safeSendToProgram(ui.StreamTextMsg(text))
	}

	// Per-step timeout
	stepTimeout := a.getStepTimeout(step)
	stepCtx, stepCancel := context.WithTimeout(ctx, stepTimeout)
	defer stepCancel()

	// Spawn sub-agent for this step with retry on transient errors
	var result *agent.AgentResult
	var err error
	errCat := plan.ErrorUnknown
	delegatedTokensUsed := 0
	const maxRetries = 3
	backoffDurations := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

	for attempt := range maxRetries {
		// A policy/circuit breaker may reject this attempt without invoking
		// execFn. Do not accidentally re-account the previous attempt's result.
		result = nil
		execFn := func() error {
			_, result, err = a.agentRunner.SpawnWithContext(
				stepCtx, "general", stepPrompt, 30, "", projectCtx, onText, true,
				func(progress *agent.AgentProgress) {
					a.safeSendToProgram(ui.PlanProgressMsg{
						PlanID:        approvedPlan.ID,
						CurrentStepID: step.ID,
						CurrentTitle:  step.Title,
						TotalSteps:    totalSteps,
						Completed:     approvedPlan.CompletedCount(),
						Progress:      approvedPlan.Progress(),
						Status:        "in_progress",
						SubStepInfo:   progress.FormatProgress(),
						Reason:        "agent progress update",
					})
				})
			return err
		}
		if a.policy != nil {
			err = a.policy.ExecutePlanStep(stepCtx, execFn)
		} else {
			err = execFn()
		}
		if result != nil {
			delegatedTokensUsed += max(result.InputTokens, 0) + max(result.OutputTokens, 0)
			// Persist eagerly: every failure/paused branch below returns before
			// the success-path assignment, but those attempts were still billed.
			step.TokensUsed = delegatedTokensUsed
			approvedPlan.SetStepUsage(step.ID, step.TokensUsed)
		}

		if err != nil {
			errCat = plan.ClassifyError(err, err.Error())
			if errors.Is(err, ErrStepCircuitOpen) {
				errCat = plan.ErrorTransient
			}
			if errCat == plan.ErrorTransient && attempt < maxRetries-1 {
				// A delegated agent may have completed writes before a later
				// provider round failed. Its tool_start events are recorded in the
				// step ledger while delegation is sequential. Never respawn the whole
				// step automatically once such effects exist: new tool-call IDs would
				// bypass executor-level replay and repeat the mutation.
				if approvedPlan.HasPartialEffects(step.ID) || approvedPlan.HasDuplicateRisk(step.ID) {
					reason := fmt.Sprintf(
						"automatic retry blocked after attempt %d: delegated tool effects were recorded before the transient error; review the workspace and resume explicitly",
						attempt+1,
					)
					if approvedPlan.HasDuplicateRisk(step.ID) {
						reason = fmt.Sprintf(
							"automatic retry blocked after attempt %d: duplicate delegated tool effects were detected; review the workspace and resume explicitly",
							attempt+1,
						)
					}
					if a.reliability != nil && !errors.Is(err, context.Canceled) {
						a.reliability.RecordFailure()
					}
					a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, reason)
					logging.Warn("delegated step retry blocked after partial tool effects",
						"step_id", step.ID, "attempt", attempt+1, "error", err.Error())
					return
				}

				backoff := backoffDurations[attempt]
				logging.Warn("sub-agent error, retrying step",
					"step_id", step.ID, "attempt", attempt+1, "error", err.Error(),
					"category", errCat.String(), "backoff", backoff)
				a.safeSendToProgram(ui.StatusUpdateMsg{
					Type:    ui.StatusRetry,
					Message: fmt.Sprintf("Step %d failed — retry %d/%d in %v", step.ID, attempt+1, maxRetries, backoff),
					Details: map[string]any{"attempt": attempt + 1, "maxAttempts": maxRetries},
				})

				backoffTimer := time.NewTimer(backoff)
				select {
				case <-backoffTimer.C:
					continue
				case <-ctx.Done():
					backoffTimer.Stop()
					err = ctx.Err()
					errCat = plan.ClassifyError(err, err.Error())
					break
				}
			}
		}
		break
	}

	if err != nil || result == nil || result.Status == agent.AgentStatusFailed {
		if a.reliability != nil && !errors.Is(err, context.Canceled) {
			a.reliability.RecordFailure()
		}

		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
		} else if result != nil {
			errMsg = result.Error
			errCat = plan.ClassifyError(err, errMsg)
		}

		// Transient error after all retries → pause step, plan continues with other steps
		if errCat == plan.ErrorTransient {
			reason := fmt.Sprintf("%s (after %d attempts; will auto-retry later)", errMsg, maxRetries)
			a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, reason)

			logging.Info("step paused due to transient error, plan continues",
				"step_id", step.ID, "error", errMsg, "category", errCat.String())
			return
		}

		// Non-transient error: preserve partial output if available
		if result != nil && result.Output != "" {
			a.planManager.CompleteStep(step.ID, "(partial) "+result.Output)
			a.commitStepRollbackSnapshot(approvedPlan.ID, step.ID)
			logging.Debug("step failed but partial output preserved",
				"step_id", step.ID, "output_len", len(result.Output))
		} else {
			a.planManager.FailStep(step.ID, errMsg)
			a.commitStepRollbackSnapshot(approvedPlan.ID, step.ID)
			a.journalEvent("plan_step_failed", map[string]any{
				"plan_id": approvedPlan.ID,
				"step_id": step.ID,
				"reason":  errMsg,
			})
		}

		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("\n  Step %d failed (%s): %s\n", step.ID, errCat.String(), errMsg)))
		a.safeSendToProgram(ui.PlanProgressMsg{
			PlanID:        approvedPlan.ID,
			CurrentStepID: step.ID,
			CurrentTitle:  step.Title,
			TotalSteps:    totalSteps,
			Completed:     approvedPlan.CompletedCount(),
			Progress:      approvedPlan.Progress(),
			Status:        "failed",
			Reason:        errMsg,
		})

		// Attempt adaptive replan on fatal errors
		if errCat == plan.ErrorFatal && a.planManager.HasReplanHandler() {
			if replanErr := a.planManager.RequestReplan(ctx, step); replanErr == nil {
				a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusRetry, Message: "Plan adjusted after step failure — continuing"})
				logging.Info("plan replanned after fatal step error",
					"step_id", step.ID, "plan_version", approvedPlan.Version)
				return // Don't abort — the loop will pick up new steps
			} else {
				logging.Warn("replan attempt failed", "error", replanErr)
			}
		}

		if a.config.Plan.AbortOnStepFailure {
			a.safeSendToProgram(ui.StatusUpdateMsg{Type: ui.StatusRecoverableError, Message: "Aborting plan due to step failure"})
		}
		return
	}

	// Store compact output in step and mark complete
	if a.reliability != nil {
		a.reliability.RecordSuccess()
	}

	output := result.Output
	if runes := []rune(output); len(runes) > planStepOutputMaxChars {
		output = string(runes[:planStepOutputMaxChars]) + "..."
	}

	// Record provider-reported usage across every delegated attempt. Fall back
	// to an output-length estimate only for legacy/custom runners that do not
	// expose usage. `step` is a deep copy, so persist by ID.
	step.TokensUsed = delegatedTokensUsed
	if step.TokensUsed == 0 {
		step.TokensUsed = len([]rune(output)) / 4
	}
	approvedPlan.SetStepUsage(step.ID, step.TokensUsed)

	verificationSummary, verificationOutput, verificationOK, verificationReason := a.runStepVerificationCommands(ctx, approvedPlan, step)
	if !verificationOK {
		a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, verificationReason)
		return
	}
	if verificationOutput != "" {
		if output != "" {
			output += "\n\n"
		}
		output += verificationOutput
		if runes := []rune(output); len(runes) > planStepOutputMaxChars {
			output = string(runes[:planStepOutputMaxChars]) + "..."
		}
	}
	if verificationSummary != "" {
		a.journalEvent("plan_step_verification_passed", map[string]any{
			"plan_id": approvedPlan.ID,
			"step_id": step.ID,
			"summary": verificationSummary,
		})
	}

	evidence, verificationNote, evidenceOK, evidenceReason := a.buildStepCompletionEvidence(approvedPlan, step, output)
	if !evidenceOK {
		a.pauseStepWithRollback(ctx, approvedPlan, step, totalSteps, evidenceReason)
		return
	}
	a.planManager.RecordStepVerification(step.ID, evidence, verificationNote)

	a.planManager.CompleteStep(step.ID, output)
	a.commitStepRollbackSnapshot(approvedPlan.ID, step.ID)
	a.journalEvent("plan_step_completed", map[string]any{
		"plan_id": approvedPlan.ID,
		"step_id": step.ID,
		"output":  previewForJournal(output),
	})

	// Extract agent metrics from result metadata
	if result.Metadata != nil {
		metrics := &plan.StepAgentMetrics{
			Duration: result.Duration,
		}
		if v, ok := result.Metadata["tree_total_nodes"].(int); ok {
			metrics.TotalNodes = v
		}
		if v, ok := result.Metadata["tree_max_depth"].(int); ok {
			metrics.MaxDepth = v
		}
		if v, ok := result.Metadata["tree_expanded_nodes"].(int); ok {
			metrics.ExpandedNodes = v
		}
		if v, ok := result.Metadata["tree_replan_count"].(int); ok {
			metrics.ReplanCount = v
		}
		if v, ok := result.Metadata["tree_succeeded_nodes"].(int); ok {
			metrics.SucceededNodes = v
		}
		if v, ok := result.Metadata["tree_failed_nodes"].(int); ok {
			metrics.FailedNodes = v
		}
		if metrics.TotalNodes > 0 {
			step.AgentMetrics = metrics
			// `step` is a deep copy — persist to the real plan step by ID.
			approvedPlan.SetStepAgentMetrics(step.ID, metrics)
		}
	}

	// Store step result in SharedMemory for inter-step communication
	if sharedMem != nil {
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

	a.safeSendToProgram(ui.StreamTextMsg(
		fmt.Sprintf("  Step %d complete: %s\n\n", step.ID, step.Title)))
	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:        approvedPlan.ID,
		CurrentStepID: step.ID,
		CurrentTitle:  step.Title,
		TotalSteps:    totalSteps,
		Completed:     approvedPlan.CompletedCount(),
		Progress:      approvedPlan.Progress(),
		Status:        "completed",
		Reason:        "step completed",
	})

	// Save session after each completed step (crash recovery)
	if a.sessionManager != nil {
		if err := a.sessionManager.SaveAfterMessage(); err != nil {
			logging.Warn("failed to save session after message", "error", err)
		}
	}
	a.enforceSessionMemoryGovernance("plan_step_completed")
}

// isOverloadError detects provider-side overload signals that should feed the
// rate-window breaker: z.ai GLM 1305, "overloaded" / "too many requests"
// substrings, HTTP 529 "Site is overloaded". Bounded to these patterns so
// generic retryable errors (timeouts, connection resets) don't inflate the
// rate counter — those are already handled by the classic retry loop.
//
// Numeric codes are checked with word-ish boundaries (" 1305", "(1305)" etc.)
// so `"timeout after 1305ms"` or `"elapsed 529ms"` don't false-match.
func isOverloadError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "overloaded") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many requests") {
		return true
	}
	return containsCode(s, "1305") || containsCode(s, "529")
}

// containsCode reports whether `code` appears in `text` as a standalone
// numeric token — flanked by non-alphanumeric characters or string edges.
// Prevents substring matches inside timings like "1305ms" (letter follows)
// or "11305" (digit precedes).
func containsCode(text, code string) bool {
	i := 0
	for i <= len(text)-len(code) {
		idx := strings.Index(text[i:], code)
		if idx < 0 {
			return false
		}
		pos := i + idx
		leftOK := pos == 0 || !isAlnum(text[pos-1])
		rightEnd := pos + len(code)
		rightOK := rightEnd == len(text) || !isAlnum(text[rightEnd])
		if leftOK && rightOK {
			return true
		}
		i = pos + 1
	}
	return false
}

func isAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func (a *App) shouldUseSafeMode() bool {
	return a.reliability != nil && a.reliability.IsDegraded()
}

// nextRetryMessageAfterProgress anchors the next retry attempt to what THIS
// attempt actually produced, not to which retry-decision branch fired
// (round-5 fix — see CLAUDE.md). Executor.Execute always appends the user's
// message to history before making any Send* call and returns that appended
// history on every error path, so len(cleaned) > len(preAttempt) alone is
// true on nearly every retry — including a bare immediate failure with zero
// model content. Scoping to cleaned[len(preAttempt):] (the portion THIS
// attempt actually added) avoids two problems: misattributing an OLDER,
// unrelated session turn's content as "the interrupted response" when this
// attempt produced nothing new, and needlessly wrapping a genuinely
// zero-progress retry in interruption boilerplate.
func nextRetryMessageAfterProgress(originalMessage string, preAttempt, cleaned []*genai.Content) string {
	// newPortion stays nil (not "the whole cleaned slice") unless cleaned is
	// STRICTLY longer than preAttempt. If stripOrphanFunctionCalls ever
	// shrinks cleaned to at-or-below preAttempt's length (e.g. it stripped
	// an orphan from the already-persisted prefix, not just this attempt's
	// tail — a violation of the pre-existing "persisted history is orphan-
	// free" invariant, not something this attempt itself would cause), a
	// naive fallback to the whole cleaned slice would re-scan OLDER,
	// unrelated turns for "the interrupted response" — the exact
	// misattribution this function exists to avoid. nil safely falls
	// through to the originalMessage branch below instead.
	var newPortion []*genai.Content
	if len(cleaned) > len(preAttempt) {
		newPortion = cleaned[len(preAttempt):]
	}
	if lastModelText(newPortion) == "" && lastModelToolContext(newPortion) == "" {
		return originalMessage
	}
	return buildContinuationRetryMessage(originalMessage, newPortion)
}

// restoreDirectPromptScaffolding removes the request-scoped hybrid hint from
// the persisted user turn. Executor.Execute appends the exact message it was
// given at priorLen; keep the optimization provider-visible while session
// resume, memory extraction, and user-facing history retain the user's text.
func restoreDirectPromptScaffolding(newHistory []*genai.Content, priorLen int, originalMessage string) []*genai.Content {
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

// nextAutoResumeMessageAfterProgress gives the first durable timeout recovery
// the same continuation anchor as an in-process stream retry. Once executing a
// persisted/in-process recovery, the payload itself is the stable retry
// identity; keep it unchanged even if that attempt made more progress so its
// bounded retry counter cannot migrate to a new message hash each round.
func nextAutoResumeMessageAfterProgress(
	originalMessage string,
	preAttempt, committed []*genai.Content,
	recoveryTurn bool,
) string {
	if recoveryTurn {
		return originalMessage
	}
	return nextRetryMessageAfterProgress(originalMessage, preAttempt, committed)
}

func buildContinuationRetryMessage(baseMessage string, history []*genai.Content) string {
	baseMessage = strings.TrimSpace(baseMessage)
	last := lastModelText(history)

	// Also gather tool call context from the last model message
	toolContext := lastModelToolContext(history)

	if last == "" && toolContext == "" {
		return "[System note: previous response was interrupted. Continue from where you stopped without repeating completed parts.]\n\n" + baseMessage
	}

	var anchor string
	if last != "" {
		anchor = lastCompleteSentence(last)
		if anchor == "" {
			anchor = truncateTail(last, 220)
		}
		anchor = strings.TrimSpace(anchor)
	}

	var parts []string
	parts = append(parts, "[System note: previous response was interrupted by a stream timeout.")
	if anchor != "" {
		parts = append(parts, fmt.Sprintf(" Last complete sentence: %q.", anchor))
	}
	if toolContext != "" {
		parts = append(parts, " "+toolContext)
	}
	parts = append(parts, " Continue from where you stopped without repeating completed parts.]")

	return strings.Join(parts, "") + "\n\n" + baseMessage
}

// lastModelToolContext extracts tool call information from the last model response
// to help the model resume with awareness of what tools were called before the interruption.
func lastModelToolContext(history []*genai.Content) string {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg == nil || msg.Role != genai.RoleModel {
			continue
		}
		var tools []string
		for _, part := range msg.Parts {
			if part != nil && part.FunctionCall != nil {
				tools = append(tools, part.FunctionCall.Name)
			}
		}
		if len(tools) > 0 {
			return fmt.Sprintf("Tools already called in interrupted response: [%s].", strings.Join(tools, ", "))
		}
	}
	return ""
}

func lastModelText(history []*genai.Content) string {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg == nil || msg.Role != genai.RoleModel {
			continue
		}

		var sb strings.Builder
		for _, part := range msg.Parts {
			// Thinking is provider protocol state, not assistant-facing prose.
			// Quoting it into a synthetic user continuation can leak hidden
			// reasoning and, for signed-thinking providers, detach it from the
			// signature that makes replay valid.
			if part != nil && !part.Thought && strings.TrimSpace(part.Text) != "" {
				sb.WriteString(part.Text)
			}
		}

		text := strings.TrimSpace(sb.String())
		if text != "" {
			return text
		}
	}
	return ""
}

func lastCompleteSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	for i := len(text) - 1; i >= 0; i-- {
		switch text[i] {
		case '.', '!', '?', '\n':
			return strings.TrimSpace(text[:i+1])
		}
	}
	return ""
}

// truncateTail returns the last `max` runes of text, prefixed with "..." when
// truncation occurs. Rune-aware so multibyte content (Cyrillic, CJK, emoji)
// stays intact — byte slicing here would corrupt the recovery hint that
// downstream prompts inject verbatim.
func truncateTail(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return "..." + strings.TrimSpace(string(runes[len(runes)-max:]))
}

// trimToLastModelMessage trims history back to the last model message,
// ensuring we don't persist orphaned tool results. minLen is the minimum
// length to preserve (original history before this request).
//
// Beyond just truncating, this also strips orphan FunctionCall parts
// from the KEPT model message — i.e. calls whose corresponding tool
// results were not appended to history before the failure. Leaving them
// in place causes every subsequent API call to fail with:
//
//	"an assistant message with 'tool_calls' must be followed by tool
//	 messages responding to each 'tool_call_id'. The following
//	 tool_call_ids did not have response messages: ..."
//
// The `client.sanitizeToolPairs` last-defense runs per-request but
// cannot retroactively fix a corrupt session — once the orphan call is
// persisted to `session.History`, every turn reproduces the 400 until
// the user /clears. So we repair at the persistence boundary (here)
// in addition to the per-request defense.
func trimToLastModelMessage(history []*genai.Content, minLen int) []*genai.Content {
	for i := len(history) - 1; i >= minLen; i-- {
		if history[i] != nil && history[i].Role == genai.RoleModel {
			trimmed := history[:i+1]
			return stripOrphanFunctionCalls(trimmed)
		}
	}
	return history[:minLen]
}

// stripOrphanFunctionCalls returns a shallow-copy of `history` where
// each model-role message has its FunctionCall parts filtered down to
// only those whose IDs have a paired FunctionResponse somewhere later
// in the slice. Parts with empty IDs are kept as-is — the client-side
// sanitizer's fallbackToolID path handles them.
//
// Preserves ordering and message identity for anything it doesn't
// change; allocates a new Content + Parts slice only for messages that
// actually had orphan parts removed.
func stripOrphanFunctionCalls(history []*genai.Content) []*genai.Content {
	// First pass: collect all response IDs appearing anywhere in history.
	responseIDs := make(map[string]bool)
	for _, msg := range history {
		if msg == nil {
			continue
		}
		for _, part := range msg.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.ID != "" {
				responseIDs[part.FunctionResponse.ID] = true
			}
		}
	}

	// Second pass: filter model messages, drop orphan calls.
	out := make([]*genai.Content, 0, len(history))
	for _, msg := range history {
		if msg == nil {
			continue
		}
		if msg.Role != genai.RoleModel {
			out = append(out, msg)
			continue
		}
		// Scan first to see if anything needs filtering; if clean, reuse
		// the original pointer to keep allocations flat.
		needsFilter := false
		for _, part := range msg.Parts {
			if part != nil && part.FunctionCall != nil && part.FunctionCall.ID != "" {
				if !responseIDs[part.FunctionCall.ID] {
					needsFilter = true
					break
				}
			}
		}
		if !needsFilter {
			out = append(out, msg)
			continue
		}
		kept := make([]*genai.Part, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if part != nil && part.FunctionCall != nil && part.FunctionCall.ID != "" {
				if !responseIDs[part.FunctionCall.ID] {
					continue // orphan
				}
			}
			kept = append(kept, part)
		}
		// Drop the entire message if nothing remains (was all orphans).
		if len(kept) == 0 {
			continue
		}
		out = append(out, &genai.Content{Role: msg.Role, Parts: kept})
	}
	return out
}

func maxPlanExecutionRounds(totalSteps int) int {
	if totalSteps <= 0 {
		return 40
	}
	rounds := totalSteps * 12
	if rounds < 40 {
		return 40
	}
	if rounds > 600 {
		return 600
	}
	return rounds
}

func requiresHumanCheckpoint(step *plan.Step) bool {
	if step == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(step.Title + " " + step.Description))
	if text == "" {
		return false
	}
	keywords := []string{
		"migration", "migrate", "drop table", "drop database",
		"mass update", "bulk delete", "deploy", "production",
		"billing", "payment",
	}
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// finalizePlanLifecycleState applies a validated terminal/non-terminal lifecycle state
// after an execution round finishes.
func (a *App) finalizePlanLifecycleState(p *plan.Plan) {
	if a.planManager == nil || p == nil {
		return
	}

	target := plan.LifecycleFailed
	switch {
	case p.Status == plan.StatusPaused || p.HasPausedSteps():
		target = plan.LifecyclePaused
	case p.Status == plan.StatusCompleted:
		target = plan.LifecycleCompleted
	case p.Status == plan.StatusFailed:
		target = plan.LifecycleFailed
	case p.StepCount() > 0 && p.CompletedCount() == p.StepCount():
		target = plan.LifecycleCompleted
	}

	if err := a.planManager.TransitionCurrentPlanLifecycle(target); err != nil {
		logging.Warn("failed to finalize plan lifecycle state", "target", string(target), "error", err)
	}
}

// formatPlanSummary generates a rich summary after plan execution completes.
func (a *App) formatPlanSummary(p *plan.Plan, duration time.Duration) string {
	var sb strings.Builder
	steps := p.GetStepsSnapshot()

	sb.WriteString("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString("  Plan Execution Summary\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Fprintf(&sb, "  Plan: %s\n", p.Title)
	fmt.Fprintf(&sb, "  Duration: %s", formatDuration(duration))
	if p.Version > 0 {
		fmt.Fprintf(&sb, "  (v%d)", p.Version)
	}
	sb.WriteString("\n\n")

	completed, failed, skipped, totalTokens := 0, 0, 0, 0
	for _, step := range steps {
		totalTokens += step.TokensUsed
		switch step.Status {
		case plan.StatusCompleted:
			completed++
			fmt.Fprintf(&sb, "  ✓ Step %d: %s (%s)\n", step.ID, step.Title, formatDuration(step.Duration()))
		case plan.StatusFailed:
			failed++
			errMsg := step.Error
			if runes := []rune(errMsg); len(runes) > 80 {
				errMsg = string(runes[:80]) + "..."
			}
			fmt.Fprintf(&sb, "  ✗ Step %d: %s — %s\n", step.ID, step.Title, errMsg)
		case plan.StatusSkipped:
			skipped++
			fmt.Fprintf(&sb, "  ⊘ Step %d: %s (skipped)\n", step.ID, step.Title)
		case plan.StatusPaused:
			fmt.Fprintf(&sb, "  ⏸ Step %d: %s (paused)\n", step.ID, step.Title)
		default:
			fmt.Fprintf(&sb, "  ○ Step %d: %s (pending)\n", step.ID, step.Title)
		}
		if step.AgentMetrics != nil {
			m := step.AgentMetrics
			fmt.Fprintf(&sb, "    Agent: %d nodes, depth %d", m.TotalNodes, m.MaxDepth)
			if m.ReplanCount > 0 {
				fmt.Fprintf(&sb, ", %d replans", m.ReplanCount)
			}
			sb.WriteString("\n")
		}
	}

	fmt.Fprintf(&sb, "\n  Results: %d completed", completed)
	if failed > 0 {
		fmt.Fprintf(&sb, ", %d failed", failed)
	}
	if skipped > 0 {
		fmt.Fprintf(&sb, ", %d skipped", skipped)
	}
	fmt.Fprintf(&sb, " / %d total\n", len(steps))
	if totalTokens > 0 {
		fmt.Fprintf(&sb, "  Tokens used: ~%d\n", totalTokens)
	}
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	return sb.String()
}

// formatDuration formats a duration as a human-readable string.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

func (a *App) pauseStepWithRollback(ctx context.Context, approvedPlan *plan.Plan, step *plan.Step, totalSteps int, reason string) {
	if approvedPlan == nil || step == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "step paused by contract gate"
	}

	rollbackApplied, rollbackSummary := a.rollbackStepSnapshot(ctx, approvedPlan, step)
	finalReason := reason
	if rollbackSummary != "" {
		finalReason = reason + " | " + rollbackSummary
	}

	a.planManager.PauseStep(step.ID, finalReason)
	a.journalEvent("plan_step_paused", map[string]any{
		"plan_id":          approvedPlan.ID,
		"step_id":          step.ID,
		"reason":           finalReason,
		"rollback_applied": rollbackApplied,
		"rollback_summary": rollbackSummary,
	})

	message := fmt.Sprintf("⏸ Step %d paused: %s\n", step.ID, reason)
	if rollbackSummary != "" {
		if rollbackApplied {
			message += fmt.Sprintf("↩ %s\n", rollbackSummary)
		} else {
			message += fmt.Sprintf("⚠ Rollback issue: %s\n", rollbackSummary)
		}
	}
	a.safeSendToProgram(ui.StreamTextMsg(message))
	a.safeSendToProgram(ui.PlanProgressMsg{
		PlanID:        approvedPlan.ID,
		CurrentStepID: step.ID,
		CurrentTitle:  step.Title,
		TotalSteps:    totalSteps,
		Completed:     approvedPlan.CompletedCount(),
		Progress:      approvedPlan.Progress(),
		Status:        "paused",
		Reason:        finalReason,
	})
}

func (a *App) runStepVerificationCommands(ctx context.Context, approvedPlan *plan.Plan, step *plan.Step) (string, string, bool, string) {
	if step == nil {
		return "", "", false, "missing step context for verification"
	}

	required := normalizeVerifyCommands(step.VerifyCommands)
	if len(required) == 0 {
		return "", "", false, fmt.Sprintf("step %d is missing verify_commands contract", step.ID)
	}

	bashTool, ok := a.registry.Get("bash")
	if !ok {
		return "", "", false, "cannot run verify_commands: bash tool is unavailable"
	}

	verifyCtx, cancel := context.WithTimeout(ctx, planStepVerifyTimeout)
	defer cancel()
	projectProfile := donegate.DetectProfile(a.workDir)

	lines := make([]string, 0, len(required))
	for i, cmd := range required {
		if safe, safetyReason := a.validateVerifyCommandSafety(verifyCtx, cmd, projectProfile); !safe {
			return "", "", false, fmt.Sprintf("unsafe verify command blocked: %s (%s)", cmd, safetyReason)
		}

		wrapped := cmd
		if wd := strings.TrimSpace(a.workDir); wd != "" {
			wrapped = "cd " + shellQuote(wd) + " && " + cmd
		}

		args := map[string]any{
			"command":     wrapped,
			"description": fmt.Sprintf("plan step %d verify %d/%d", step.ID, i+1, len(required)),
		}

		if a.permManager != nil && a.permManager.IsEnabled() {
			resp, err := a.permManager.Check(verifyCtx, "bash", args)
			if err != nil {
				return "", "", false, fmt.Sprintf("verify command blocked: %v", err)
			}
			if resp != nil && !resp.Allowed {
				reason := strings.TrimSpace(resp.Reason)
				if reason == "" {
					reason = "permission denied"
				}
				return "", "", false, fmt.Sprintf("verify command blocked: %s", reason)
			}
		}

		a.safeSendToProgram(ui.StreamTextMsg(
			fmt.Sprintf("  ↳ verify %d/%d: %s\n", i+1, len(required), cmd)))

		if approvedPlan != nil {
			approvedPlan.RecordStepEffect(step.ID, "bash", map[string]any{"command": wrapped})
			if a.planManager != nil {
				_ = a.saveCurrentPlanWithVisibility("step verification command")
			}
		}

		result, err := bashTool.Execute(verifyCtx, args)
		if err != nil {
			return "", "", false, fmt.Sprintf("verify command failed: %s (%v)", cmd, err)
		}

		detail := strings.TrimSpace(result.Content)
		if runes := []rune(detail); len(runes) > 240 {
			detail = string(runes[:240]) + "..."
		}
		if !result.Success {
			failure := strings.TrimSpace(result.Error)
			if failure == "" {
				failure = detail
			}
			if failure == "" {
				failure = "command returned non-success status"
			}
			return "", "", false, fmt.Sprintf("verify command failed: %s (%s)", cmd, failure)
		}

		if detail == "" {
			lines = append(lines, fmt.Sprintf("PASS `%s`", cmd))
		} else {
			lines = append(lines, fmt.Sprintf("PASS `%s`: %s", cmd, detail))
		}
	}

	summary := fmt.Sprintf("verify_commands_passed=%d/%d", len(required), len(required))
	return summary, "Verification results:\n- " + strings.Join(lines, "\n- "), true, ""
}

func (a *App) buildStepCompletionEvidence(p *plan.Plan, step *plan.Step, output string) ([]string, string, bool, string) {
	if p == nil || step == nil {
		return nil, "", false, "missing step context"
	}

	ledger := p.GetRunLedgerSnapshot()
	entry := ledger[step.ID]

	evidence := make([]string, 0, 14)
	hasToolCalls := false
	hasTools := false
	hasFiles := false
	hasCommands := false
	verificationByCommand := false
	if entry != nil {
		if entry.ToolCalls > 0 {
			evidence = append(evidence, fmt.Sprintf("tool_calls=%d", entry.ToolCalls))
			hasToolCalls = true
		}
		if len(entry.Tools) > 0 {
			evidence = append(evidence, "tools="+joinLimited(entry.Tools, 4))
			hasTools = true
		}
		if len(entry.FilesTouched) > 0 {
			evidence = append(evidence, "files="+joinLimited(entry.FilesTouched, 4))
			hasFiles = true
		}
		if len(entry.Commands) > 0 {
			evidence = append(evidence, "commands="+joinLimited(entry.Commands, 3))
			hasCommands = true
			verificationByCommand = donegate.CommandsContainVerificationSignals(entry.Commands)
		}
	}

	output = strings.TrimSpace(output)
	hasOutput := false
	if output != "" {
		compact := output
		if runes := []rune(compact); len(runes) > 260 {
			compact = string(runes[:260]) + "..."
		}
		evidence = append(evidence, "output="+compact)
		hasOutput = true
	}

	hasOperationalProof := hasToolCalls || hasTools || hasFiles || hasCommands
	artifactHints := collectExpectedArtifactHints(step)
	explicitArtifactPaths := normalizeExpectedArtifactPaths(step.ExpectedArtifactPaths)
	if a != nil && a.config != nil && a.config.Plan.RequireExpectedArtifactPaths {
		if stepLikelyMutatesFiles(step, entry) && len(explicitArtifactPaths) == 0 {
			return nil, "", false, fmt.Sprintf(
				"step %d is missing expected_artifact_paths while mutating files (strict mode)",
				step.ID,
			)
		}
	}
	artifactHintsProof, matchedArtifactHints, missingArtifactHints := artifactHintsCovered(artifactHints, entry, output)
	hasArtifactProof := hasFiles || hasCommands || hasOutput
	if len(artifactHints) > 0 {
		hasArtifactProof = hasArtifactProof && artifactHintsProof
	}
	requiresArtifactProof := strings.TrimSpace(step.ExpectedArtifact) != ""
	requiredVerifyCommands := normalizeVerifyCommands(step.VerifyCommands)
	verifyCommandsSatisfied := verifyCommandsCovered(requiredVerifyCommands, entry)
	requiresVerificationProof := len(requiredVerifyCommands) > 0 || criteriaRequireVerificationSignals(step.SuccessCriteria, step.Description)
	verificationByOutput := donegate.OutputContainsVerificationSignals(output)
	hasVerificationProof := verificationByCommand || verificationByOutput || verifyCommandsSatisfied

	evidenceClasses := 0
	if hasOperationalProof {
		evidenceClasses++
	}
	if hasOutput {
		evidenceClasses++
	}
	if hasVerificationProof {
		evidenceClasses++
	}

	requiredClasses := 1
	if requiresArtifactProof || len(step.SuccessCriteria) > 0 {
		requiredClasses = 2
	}

	if requiresArtifactProof && len(artifactHints) > 0 && !artifactHintsProof {
		return nil, "", false, fmt.Sprintf(
			"step %d missing artifact proof for expected paths: %s",
			step.ID,
			joinLimited(missingArtifactHints, 4),
		)
	}
	if requiresArtifactProof && !hasArtifactProof {
		return nil, "", false, fmt.Sprintf("step %d lacks proof for expected artifact", step.ID)
	}
	if len(requiredVerifyCommands) > 0 && !verifyCommandsSatisfied {
		return nil, "", false, fmt.Sprintf("step %d missing proof that required verify_commands were executed", step.ID)
	}
	if requiresVerificationProof && !hasVerificationProof {
		return nil, "", false, fmt.Sprintf("step %d lacks verification proof for success criteria", step.ID)
	}
	if evidenceClasses < requiredClasses {
		return nil, "", false, fmt.Sprintf(
			"step %d needs stronger proof (%d/%d evidence classes: operational/output/verification)",
			step.ID, evidenceClasses, requiredClasses,
		)
	}

	if len(evidence) == 0 {
		return nil, "", false, "cannot complete step without proof (no diff/command/output evidence)"
	}

	proofJSON := buildStepCompletionProofJSON(
		step,
		hasArtifactProof,
		hasVerificationProof,
		hasOperationalProof,
		hasOutput,
		evidenceClasses,
		requiredClasses,
		len(requiredVerifyCommands),
		verifyCommandsSatisfied,
		len(artifactHints),
		artifactHintsProof,
		matchedArtifactHints,
		missingArtifactHints,
		entry,
	)
	if proofJSON != "" {
		evidence = append(evidence, "proof_json="+proofJSON)
	}

	evidence = append(evidence, fmt.Sprintf("contract.artifact_proof=%t", hasArtifactProof))
	evidence = append(evidence, fmt.Sprintf("contract.artifact_hints=%d", len(artifactHints)))
	evidence = append(evidence, fmt.Sprintf("contract.artifact_hints_proof=%t", artifactHintsProof))
	if len(matchedArtifactHints) > 0 {
		evidence = append(evidence, "artifact_hints_matched="+joinLimited(matchedArtifactHints, 6))
	}
	evidence = append(evidence, fmt.Sprintf("contract.verification_proof=%t", hasVerificationProof))
	evidence = append(evidence, fmt.Sprintf("contract.verify_commands=%d", len(requiredVerifyCommands)))
	evidence = append(evidence, fmt.Sprintf("contract.verify_commands_proof=%t", verifyCommandsSatisfied))
	evidence = append(evidence, fmt.Sprintf("contract.evidence_classes=%d/%d", evidenceClasses, requiredClasses))

	note := fmt.Sprintf(
		"Contract verification: artifact_proof=%t, artifact_hints=%d(proof=%t), verification_proof=%t, verify_commands=%d(proof=%t), evidence_classes=%d/%d; success_criteria=%d; expected_artifact=%s; evidence_items=%d.",
		hasArtifactProof,
		len(artifactHints),
		artifactHintsProof,
		hasVerificationProof,
		len(requiredVerifyCommands),
		verifyCommandsSatisfied,
		evidenceClasses,
		requiredClasses,
		len(step.SuccessCriteria),
		strings.TrimSpace(step.ExpectedArtifact),
		len(evidence),
	)
	return evidence, note, true, ""
}

func joinLimited(items []string, limit int) string {
	if len(items) == 0 {
		return ""
	}
	if limit <= 0 || len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:limit], ", ") + fmt.Sprintf(", ...(+%d)", len(items)-limit)
}

func criteriaRequireVerificationSignals(criteria []string, description string) bool {
	keywords := []string{
		"verify", "verification", "test", "lint", "build", "compile",
		"check", "typecheck", "vet", "pass", "validate", "validated",
		"проверь", "провер", "тест", "линт", "сборк", "валидац",
	}

	texts := make([]string, 0, len(criteria)+1)
	for _, c := range criteria {
		c = strings.TrimSpace(strings.ToLower(c))
		if c != "" {
			texts = append(texts, c)
		}
	}
	desc := strings.TrimSpace(strings.ToLower(description))
	if desc != "" {
		texts = append(texts, desc)
	}

	for _, text := range texts {
		for _, kw := range keywords {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

func (a *App) validateVerifyCommandSafety(ctx context.Context, command string, projectProfile donegate.Profile) (bool, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return false, "empty command"
	}

	lower := strings.ToLower(command)

	denyContains := normalizePolicyMarkers(defaultVerifyPolicyDenyContains())
	allowContains := make([]string, 0)
	requireIntent := true
	if cfg := a.snapshotConfig(); cfg != nil {
		policy := cfg.Plan.VerifyPolicy
		if policy.Enabled {
			requireIntent = policy.RequireVerificationIntent
			allowContains = append(allowContains, normalizePolicyMarkers(policy.AllowContains)...)
			denyContains = append(denyContains, normalizePolicyMarkers(policy.DenyContains)...)
			for _, profileKey := range a.resolveVerifyPolicyProfiles(lower, projectProfile) {
				profileCfg, ok := policy.Profiles[profileKey]
				if !ok {
					continue
				}
				allowContains = append(allowContains, normalizePolicyMarkers(profileCfg.AllowContains)...)
				denyContains = append(denyContains, normalizePolicyMarkers(profileCfg.DenyContains)...)
			}
		} else {
			requireIntent = false
		}
	}
	allowContains = dedupePolicyMarkers(allowContains)
	denyContains = dedupePolicyMarkers(denyContains)
	if commandContainsAny(lower, denyContains) {
		return false, "contains disallowed mutating operation"
	}
	// Track whether the command passed an explicit allowlist so the intent
	// check below can be skipped — the user's allowlist is more authoritative
	// than the generic "looks like verification" heuristic.
	passedExplicitAllowList := len(allowContains) > 0 && commandContainsAny(lower, allowContains)
	if len(allowContains) > 0 && !passedExplicitAllowList {
		return false, "command does not match allowlist markers for active verify policy"
	}

	// Allow /dev/null redirection, deny file-writing redirects in verify stage.
	if strings.Contains(lower, ">>") {
		return false, "append redirection is not allowed in verify commands"
	}
	redirectionCheck := lower
	for _, allowed := range []string{"2>&1", "1>/dev/null", "2>/dev/null", ">/dev/null"} {
		redirectionCheck = strings.ReplaceAll(redirectionCheck, allowed, "")
	}
	if strings.Contains(redirectionCheck, ">") {
		return false, "output redirection to files is not allowed in verify commands"
	}

	validator := tools.NewDefaultSafetyValidator()
	check, err := validator.ValidateSafety(ctx, "bash", map[string]any{"command": command})
	if err != nil {
		return false, err.Error()
	}
	if check != nil {
		if !check.IsValid {
			return false, strings.Join(check.Errors, "; ")
		}
		for _, warning := range check.Warnings {
			if strings.Contains(strings.ToLower(warning), "destructive operation detected") {
				return false, warning
			}
		}
	}

	// Skip the generic intent heuristic when the command passed an explicit
	// user-defined allowlist — the allowlist is a stronger signal than pattern
	// matching, and double-blocking an explicitly allowed command is confusing.
	if requireIntent && !passedExplicitAllowList && !looksLikeVerificationCommand(lower) {
		return false, "command does not match verification intent"
	}

	return true, ""
}

func defaultVerifyPolicyDenyContains() []string {
	return []string{
		"rm -", "rm -rf", " mv ", " cp ", " chmod ", " chown ", "sudo ",
		"mkfs", " dd ", "git reset", "git clean", "git checkout --",
		"git commit", "git push", "git stash", "npm install", "npm i ",
		"pnpm add", "yarn add", "pip install", "go get ", "cargo add ",
		"brew install", "apt install", "apk add", "dnf install", "pacman -s",
		"curl |", "wget |",
	}
}

func normalizePolicyMarkers(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func dedupePolicyMarkers(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func commandContainsAny(lowerCommand string, markers []string) bool {
	lowerCommand = strings.ToLower(lowerCommand)
	guarded := " " + lowerCommand
	for _, marker := range markers {
		marker = strings.TrimSpace(strings.ToLower(marker))
		if marker == "" {
			continue
		}
		if strings.Contains(guarded, marker) {
			return true
		}
	}
	return false
}

func stepLikelyMutatesFiles(step *plan.Step, entry *plan.RunLedgerEntry) bool {
	if entry == nil {
		return false
	}

	if slices.ContainsFunc(entry.Tools, isMutatingToolName) {
		return true
	}

	requiredVerify := normalizeVerifyCommands(nil)
	if step != nil {
		requiredVerify = normalizeVerifyCommands(step.VerifyCommands)
	}
	for _, cmd := range entry.Commands {
		if isCommandCoveredByVerify(cmd, requiredVerify) {
			continue
		}
		if commandLooksMutating(cmd) {
			return true
		}
	}
	return false
}

func isMutatingToolName(name string) bool {
	switch strings.TrimSpace(strings.ToLower(name)) {
	case "write", "edit", "batch", "move", "delete", "mkdir":
		return true
	default:
		return false
	}
}

func isCommandCoveredByVerify(command string, required []string) bool {
	if len(required) == 0 {
		return false
	}
	fingerprint := normalizeCommandFingerprint(command)
	if fingerprint == "" {
		return false
	}
	for _, verifyCmd := range required {
		v := normalizeCommandFingerprint(verifyCmd)
		if v == "" {
			continue
		}
		if fingerprint == v || strings.Contains(fingerprint, v) || strings.Contains(v, fingerprint) {
			return true
		}
	}
	return false
}

func commandLooksMutating(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	if looksLikeVerificationCommand(lower) {
		return false
	}
	markers := []string{
		"rm -", "mv ", "cp ", "chmod ", "chown ", "truncate ", "touch ",
		"tee ", "sed -i", "perl -pi", "cat >", "echo >", ">>", "apply_patch",
		"git add", "git rm", "git mv", "git commit", "git checkout --",
		"npm install", "npm i ", "pnpm add", "yarn add", "pip install",
		"go get ", "cargo add ", "mkdir ", "rmdir ",
	}
	return commandContainsAny(lower, markers)
}

func (a *App) resolveVerifyPolicyProfiles(lowerCommand string, profile donegate.Profile) []string {
	keys := []string{"default"}
	seen := map[string]bool{"default": true}
	appendKey := func(key string) {
		key = strings.TrimSpace(strings.ToLower(key))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		keys = append(keys, key)
	}

	present := a.projectVerifyStacks(profile)
	if len(present) == 1 {
		appendKey(present[0])
		return keys
	}

	stackKeywords := map[string][]string{
		"go":     {"go test", "go vet", "go build"},
		"node":   {"npm ", "pnpm ", "yarn ", "bun ", "eslint", "vitest", "jest", "tsc"},
		"python": {"pytest", "ruff", "mypy", "python -m"},
		"rust":   {"cargo "},
		"java":   {"mvn ", "gradle", "./gradlew"},
		"cmake":  {"cmake", "ctest"},
		"bazel":  {"bazel "},
		"make":   {"make "},
		"php":    {"composer", "phpunit", "phpstan"},
	}

	for _, stack := range present {
		keywords := stackKeywords[stack]
		for _, kw := range keywords {
			if strings.Contains(lowerCommand, kw) {
				appendKey(stack)
				break
			}
		}
	}

	return keys
}

func (a *App) projectVerifyStacks(profile donegate.Profile) []string {
	stacks := make([]string, 0, 9)
	if len(profile.GoModules) > 0 {
		stacks = append(stacks, "go")
	}
	if len(profile.NodeProjects) > 0 {
		stacks = append(stacks, "node")
	}
	if len(profile.PythonRoots) > 0 {
		stacks = append(stacks, "python")
	}
	if len(profile.RustModules) > 0 {
		stacks = append(stacks, "rust")
	}
	if len(profile.JavaProjects) > 0 {
		stacks = append(stacks, "java")
	}
	if len(profile.CMakeProjects) > 0 {
		stacks = append(stacks, "cmake")
	}
	if len(profile.BazelRoots) > 0 {
		stacks = append(stacks, "bazel")
	}
	if len(profile.MakeProjects) > 0 {
		stacks = append(stacks, "make")
	}
	if len(profile.PHPProjects) > 0 {
		stacks = append(stacks, "php")
	}
	return stacks
}

func looksLikeVerificationCommand(lowerCommand string) bool {
	if strings.TrimSpace(lowerCommand) == "" {
		return false
	}
	signals := []string{
		" test", "go test", "pytest", "cargo test", "npm test", "pnpm test", "yarn test", "bun test",
		"lint", "typecheck", "check", "verify", "vet", "build", "compile", "validate",
		"git diff --check", "git status", "git rev-parse", "mvn test", "gradle test", "composer validate",
		"bazel test", "cmake --build", "make test", "phpstan", "ruff", "mypy",
	}
	padded := " " + lowerCommand
	for _, signal := range signals {
		if strings.Contains(padded, signal) {
			return true
		}
	}
	return false
}

func normalizeVerifyCommands(commands []string) []string {
	if len(commands) == 0 {
		return nil
	}
	out := make([]string, 0, len(commands))
	seen := make(map[string]bool, len(commands))
	for _, cmd := range commands {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		fingerprint := normalizeCommandFingerprint(cmd)
		if fingerprint == "" || seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		out = append(out, cmd)
	}
	return out
}

func normalizeExpectedArtifactPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		fingerprint := normalizePathHintForMatch(path)
		if fingerprint == "" || seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		out = append(out, path)
	}
	return out
}

func normalizeCommandFingerprint(cmd string) string {
	cmd = strings.TrimSpace(strings.ToLower(cmd))
	if cmd == "" {
		return ""
	}
	return strings.Join(strings.Fields(cmd), " ")
}

func verifyCommandsCovered(required []string, entry *plan.RunLedgerEntry) bool {
	if len(required) == 0 {
		return true
	}
	if entry == nil || len(entry.Commands) == 0 {
		return false
	}

	executed := make([]string, 0, len(entry.Commands))
	for _, cmd := range entry.Commands {
		fingerprint := normalizeCommandFingerprint(cmd)
		if fingerprint == "" {
			continue
		}
		executed = append(executed, fingerprint)
	}
	if len(executed) == 0 {
		return false
	}

	for _, requiredCmd := range required {
		requiredFingerprint := normalizeCommandFingerprint(requiredCmd)
		if requiredFingerprint == "" {
			continue
		}
		found := false
		for _, executedCmd := range executed {
			if executedCmd == requiredFingerprint ||
				strings.Contains(executedCmd, requiredFingerprint) ||
				strings.Contains(requiredFingerprint, executedCmd) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func extractArtifactPathHints(expectedArtifact string) []string {
	expectedArtifact = strings.TrimSpace(expectedArtifact)
	if expectedArtifact == "" {
		return nil
	}

	tokens := strings.FieldsFunc(expectedArtifact, func(r rune) bool {
		switch r {
		case ' ', '\n', '\t', ',', ';', '|':
			return true
		default:
			return false
		}
	})

	hints := make([]string, 0, len(tokens))
	seen := make(map[string]bool)
	for _, token := range tokens {
		candidate := normalizePathToken(token)
		if !looksLikeArtifactPathHint(candidate) {
			continue
		}
		normalized := normalizePathHintForMatch(candidate)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		hints = append(hints, candidate)
	}
	return hints
}

func collectExpectedArtifactHints(step *plan.Step) []string {
	if step == nil {
		return nil
	}
	hints := make([]string, 0, len(step.ExpectedArtifactPaths)+4)
	seen := make(map[string]bool)
	appendHint := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		fingerprint := normalizePathHintForMatch(raw)
		if fingerprint == "" || seen[fingerprint] {
			return
		}
		seen[fingerprint] = true
		hints = append(hints, raw)
	}

	for _, p := range normalizeExpectedArtifactPaths(step.ExpectedArtifactPaths) {
		appendHint(p)
	}
	for _, inferred := range extractArtifactPathHints(step.ExpectedArtifact) {
		appendHint(inferred)
	}
	return hints
}

func looksLikeArtifactPathHint(token string) bool {
	if token == "" {
		return false
	}
	if strings.HasPrefix(token, "/") || strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") {
		return true
	}
	if strings.Contains(token, "/") || strings.Contains(token, "\\") {
		return true
	}
	ext := filepath.Ext(token)
	if ext != "" && len(ext) <= 8 && len(token) > len(ext) {
		return true
	}
	return false
}

func normalizePathHintForMatch(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.Trim(value, "\"'`")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.TrimPrefix(value, "./")
	value = strings.TrimPrefix(value, "/")
	value = strings.Trim(value, "/")
	return value
}

func artifactHintsCovered(hints []string, entry *plan.RunLedgerEntry, output string) (bool, []string, []string) {
	if len(hints) == 0 {
		return true, nil, nil
	}

	candidates := make([]string, 0, 16)
	addCandidate := func(raw string) {
		normalized := normalizePathHintForMatch(raw)
		if normalized == "" {
			return
		}
		candidates = append(candidates, normalized)
	}

	if entry != nil {
		for _, file := range entry.FilesTouched {
			addCandidate(file)
		}
		for _, cmd := range entry.Commands {
			for _, pathHint := range extractCommandPathHints(cmd) {
				addCandidate(pathHint)
			}
		}
	}
	for _, outputHint := range extractArtifactPathHints(output) {
		addCandidate(outputHint)
	}

	matched := make([]string, 0, len(hints))
	missing := make([]string, 0, len(hints))
	for _, hint := range hints {
		hintNorm := normalizePathHintForMatch(hint)
		if hintNorm == "" {
			continue
		}
		found := false
		for _, candidate := range candidates {
			if candidate == hintNorm ||
				strings.HasSuffix(candidate, "/"+hintNorm) ||
				strings.Contains(candidate, hintNorm) ||
				strings.Contains(hintNorm, candidate) {
				found = true
				break
			}
		}
		if found {
			matched = append(matched, hint)
		} else {
			missing = append(missing, hint)
		}
	}
	return len(missing) == 0, matched, missing
}

func buildStepContractJSON(step *plan.Step, totalSteps int) string {
	if step == nil {
		return ""
	}

	payload := map[string]any{
		"step_id":                 step.ID,
		"total_steps":             totalSteps,
		"title":                   strings.TrimSpace(step.Title),
		"description":             strings.TrimSpace(step.Description),
		"inputs":                  step.Inputs,
		"expected_artifact":       strings.TrimSpace(step.ExpectedArtifact),
		"expected_artifact_paths": normalizeExpectedArtifactPaths(step.ExpectedArtifactPaths),
		"success_criteria":        step.SuccessCriteria,
		"verify_commands":         normalizeVerifyCommands(step.VerifyCommands),
		"rollback":                strings.TrimSpace(step.Rollback),
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	return string(data)
}

func buildStepCompletionProofJSON(
	step *plan.Step,
	hasArtifactProof, hasVerificationProof, hasOperationalProof, hasOutput bool,
	evidenceClasses, requiredClasses int,
	verifyCommandsCount int,
	verifyCommandsProof bool,
	artifactHintsCount int,
	artifactHintsProof bool,
	artifactHintsMatched []string,
	artifactHintsMissing []string,
	entry *plan.RunLedgerEntry,
) string {
	if step == nil {
		return ""
	}

	proof := map[string]any{
		"step_id":                 step.ID,
		"expected_artifact":       strings.TrimSpace(step.ExpectedArtifact),
		"expected_artifact_paths": normalizeExpectedArtifactPaths(step.ExpectedArtifactPaths),
		"success_criteria_count":  len(step.SuccessCriteria),
		"artifact_proof":          hasArtifactProof,
		"verification_proof":      hasVerificationProof,
		"operational_proof":       hasOperationalProof,
		"output_proof":            hasOutput,
		"evidence_classes":        evidenceClasses,
		"required_classes":        requiredClasses,
		"verify_commands_count":   verifyCommandsCount,
		"verify_commands_proof":   verifyCommandsProof,
		"artifact_hints_count":    artifactHintsCount,
		"artifact_hints_proof":    artifactHintsProof,
	}
	if len(artifactHintsMatched) > 0 {
		proof["artifact_hints_matched"] = artifactHintsMatched
	}
	if len(artifactHintsMissing) > 0 {
		proof["artifact_hints_missing"] = artifactHintsMissing
	}

	if entry != nil {
		proof["tool_calls"] = entry.ToolCalls
		proof["tools"] = entry.Tools
		proof["files_touched"] = entry.FilesTouched
		proof["commands"] = entry.Commands
	}

	data, err := json.Marshal(proof)
	if err != nil {
		return ""
	}
	return string(data)
}

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
	fmt.Fprintf(&sb, "**Plan:** %s\n", ctx.PlanTitle)
	if ctx.PlanDescription != "" {
		fmt.Fprintf(&sb, "**Goal:** %s\n", ctx.PlanDescription)
	}
	if ctx.PlanRequest != "" && len(ctx.PlanRequest) < 500 {
		fmt.Fprintf(&sb, "**Original Request:** %s\n", ctx.PlanRequest)
	}
	fmt.Fprintf(&sb, "**Progress:** Step %d of %d (%d completed)\n\n",
		ctx.Step.ID, ctx.TotalSteps, ctx.CompletedCount)

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
	fmt.Fprintf(&sb, "## Current Step %d: %s\n", ctx.Step.ID, ctx.Step.Title)
	if ctx.Step.Description != "" {
		sb.WriteString(ctx.Step.Description)
		sb.WriteString("\n")
	}
	sb.WriteString("\n### Step Contract\n")
	if len(ctx.Step.Inputs) > 0 {
		sb.WriteString("- Inputs:\n")
		for _, in := range ctx.Step.Inputs {
			fmt.Fprintf(&sb, "  - %s\n", in)
		}
	}
	if ctx.Step.ExpectedArtifact != "" {
		fmt.Fprintf(&sb, "- Expected artifact: %s\n", ctx.Step.ExpectedArtifact)
	}
	if len(ctx.Step.ExpectedArtifactPaths) > 0 {
		sb.WriteString("- Expected artifact paths:\n")
		for _, path := range ctx.Step.ExpectedArtifactPaths {
			fmt.Fprintf(&sb, "  - %s\n", path)
		}
	}
	if len(ctx.Step.SuccessCriteria) > 0 {
		sb.WriteString("- Success criteria:\n")
		for _, c := range ctx.Step.SuccessCriteria {
			fmt.Fprintf(&sb, "  - %s\n", c)
		}
	}
	if len(ctx.Step.VerifyCommands) > 0 {
		sb.WriteString("- Verify commands:\n")
		for _, cmd := range ctx.Step.VerifyCommands {
			fmt.Fprintf(&sb, "  - %s\n", cmd)
		}
	}
	if ctx.Step.Rollback != "" {
		fmt.Fprintf(&sb, "- Rollback: %s\n", ctx.Step.Rollback)
	}
	if contractJSON := buildStepContractJSON(ctx.Step, ctx.TotalSteps); contractJSON != "" {
		sb.WriteString("\n### StepContractJSON\n")
		sb.WriteString("```json\n")
		sb.WriteString(contractJSON)
		sb.WriteString("\n```\n")
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
	sb.WriteString("- Provide explicit evidence (files/commands/output) for completion\n")
	sb.WriteString("- Verify commands are mandatory and must pass before completion\n")
	sb.WriteString("- Report any issues or deviations from the plan\n")

	return sb.String()
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
				if runes := []rune(text); len(runes) > 500 {
					text = string(runes[:500]) + "..."
				}
				fmt.Fprintf(&sb, "**%s**: %s\n\n", role, text)
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
	for line := range strings.SplitSeq(text, "\n") {
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
	for line := range strings.SplitSeq(text, "\n") {
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
