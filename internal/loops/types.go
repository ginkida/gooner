// Package loops implements the /loop autonomous workflow system.
//
// A "loop" is a recurring task description that the agent executes on a
// schedule (interval-based or self-paced). State persists to disk so loops
// survive restarts. Each iteration:
//
//  1. Loads accumulated history from the loop's state file.
//  2. Builds a prompt from the task description + recent iteration summaries.
//  3. Submits the prompt through the normal request pipeline.
//  4. Captures the agent's summary back into the loop state.
//  5. Optionally writes a structured summary to project MEMORY.md.
//
// The user controls loops via the /loop command:
//
//	/loop                       List active loops
//	/loop <task>                Start self-paced loop
//	/loop <interval> <task>     Start interval loop (e.g. /loop 30m <task>)
//	/loop status <id>           Show loop details + recent iterations
//	/loop stop <id>             Stop a running loop
//	/loop pause <id>            Pause (won't fire until /loop resume)
//	/loop resume <id>           Re-arm a paused loop
//	/loop now <id>              Fire iteration immediately
//	/loop remove <id>           Delete loop state file
//
// Conflicts: if the user is interacting (typing, agent processing) when a
// loop is due to fire, the runner queues the trigger and waits until the
// session goes idle. Loops never preempt foreground work.
//
// Persistence: loops live as ~/.gokin/loops/<id>.json. Auto-resume on
// startup picks up wherever the prior process left off.
package loops

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Status enumerates the lifecycle states of a loop.
type Status string

const (
	// StatusRunning: scheduler will fire the next iteration when due.
	StatusRunning Status = "running"
	// StatusPaused: present in state, but scheduler skips it. User
	// re-arms via /loop resume.
	StatusPaused Status = "paused"
	// StatusStopped: terminal state — scheduler won't fire it again.
	// User can /loop remove to delete the state file, or leave it for
	// historical reference.
	StatusStopped Status = "stopped"
	// StatusCompleted: max_iterations reached, terminal. Same handling
	// as Stopped from scheduler's perspective; distinct so /loop status
	// can show it as success.
	StatusCompleted Status = "completed"
)

// Mode picks between fixed-interval and self-paced scheduling.
type Mode string

const (
	// ModeInterval: scheduler fires every IntervalSeconds regardless
	// of what the iteration says. Cron-like predictability.
	ModeInterval Mode = "interval"
	// ModeSelfPaced: scheduler fires after MinDelaySeconds (default
	// 5 minutes) so we don't hot-loop. The agent itself can include
	// "next check in X" hints that the runner respects.
	ModeSelfPaced Mode = "self-paced"
)

// Loop is the persisted state for a single recurring task.
type Loop struct {
	// WorkDir is the workspace this loop belongs to. Loops are persisted in
	// the GLOBAL config dir, so without an owner ANY gokin process adopted
	// every loop on disk and fired it against ITS OWN repository — an
	// unattended agent editing, building and committing in the wrong project,
	// on the user's quota (v0.100.111). Empty means a loop written before
	// this field existed; the scheduler adopts it on first sight rather than
	// silently killing a running loop.
	WorkDir string `json:"work_dir,omitempty"`

	// ID is the stable identifier — used in filenames and /loop subcommand
	// args. Generated as a short hex string when the loop is created.
	ID string `json:"id"`

	// Task is the user-supplied description of what each iteration should
	// accomplish. Injected verbatim as the first part of every iteration
	// prompt; the rest is the accumulated summary context.
	Task string `json:"task"`

	// Mode + IntervalSeconds determine when iterations fire.
	Mode            Mode  `json:"mode"`
	IntervalSeconds int64 `json:"interval_seconds,omitempty"`
	MinDelaySeconds int64 `json:"min_delay_seconds,omitempty"` // self-paced floor; default 300
	MaxIterations   int   `json:"max_iterations,omitempty"`    // 0 = unlimited
	MaxTotalTokens  int64 `json:"max_total_tokens,omitempty"`  // 0 = unlimited; auto-pause when TotalTokensIn+Out reaches it
	UpdateMemory    bool  `json:"update_memory"`               // auto-write summaries to MEMORY.md

	// Status drives the scheduler. See Status constants for semantics.
	Status Status `json:"status"`

	// Timestamps for ordering and scheduler decisions.
	CreatedAt time.Time `json:"created_at"`
	LastRunAt time.Time `json:"last_run_at,omitempty"`
	NextRunAt time.Time `json:"next_run_at,omitempty"`
	StoppedAt time.Time `json:"stopped_at,omitempty"`

	// Iterations is the append-only log of what each iteration did.
	// Capped at MaxHistorySize entries — older entries drop off the
	// front so the file doesn't grow unbounded over a multi-day loop.
	Iterations []Iteration `json:"iterations,omitempty"`

	// IterationCount is the lifetime count, including dropped-off entries.
	IterationCount int `json:"iteration_count"`

	// SuccessCount and FailureCount are lifetime counters of completed
	// iterations partitioned by outcome. Used by /loop list to
	// distinguish a loop making real progress from one silently
	// failing every cycle. Pre-existing loop state files written
	// before these fields were added load with zeros — they'll
	// re-populate correctly from the next iteration onward.
	SuccessCount int `json:"success_count,omitempty"`
	FailureCount int `json:"failure_count,omitempty"`

	// TotalTokensIn / TotalTokensOut are LIFETIME provider-input / generated-output
	// token totals across every iteration (not just the visible, trimmed
	// Iterations slice). Surfaced by /loop status so a user who walked away from
	// a background loop can see what it has spent — a runaway unattended loop
	// can quietly burn a provider's quota. int64 so a multi-day loop can't
	// overflow. Zero on pre-existing state files; repopulate from the next
	// iteration onward.
	TotalTokensIn  int64 `json:"total_tokens_in,omitempty"`
	TotalTokensOut int64 `json:"total_tokens_out,omitempty"`

	// ConsecutiveFailures resets to 0 on every successful iteration
	// and increments on every TASK failure (not transient infra failures —
	// see ConsecutiveTransientFailures). When it crosses
	// ConsecutiveFailureLimit, the loop is auto-paused so a runaway
	// failure cycle doesn't burn LLM credits indefinitely. The user
	// gets a warning and can /loop resume after fixing the underlying
	// issue.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`

	// ConsecutiveTransientFailures counts iterations that failed for a
	// TRANSIENT infrastructure reason (provider overload/rate-limit, network)
	// rather than a genuine task failure. Tracked separately from
	// ConsecutiveFailures so a busy/unreachable provider is WAITED OUT (the
	// loop reschedules with exponential backoff) instead of tripping the
	// task-failure auto-pause breaker — a provider hiccup must not look like
	// "this loop is broken". Reset to 0 on any success or any non-transient
	// (task) failure. A very long run of these (TransientFailureLimit) still
	// auto-pauses as a backstop, so a permanently-dead provider doesn't leave
	// a zombie loop poking forever.
	ConsecutiveTransientFailures int `json:"consecutive_transient_failures,omitempty"`

	// ConsecutiveNoProgress counts consecutive SUCCESSFUL (OK) iterations that
	// made NO code/repo change (it.MadeChanges == false) on an ACTION task —
	// "succeeding but spinning". Distinct from the failure breakers (those track
	// OK==false); a no-op success would otherwise reset every streak and look
	// healthy while burning quota forever on a finished task. Reset on a real
	// change (MadeChanges). Accrued ONLY for action tasks — a monitor/watch loop
	// (IsMonitorTask) is SUPPOSED to be no-change, so it never accrues this. At
	// NoProgressWarnThreshold the agent is warned via the iteration prompt; at
	// NoProgressLimit the loop auto-pauses (AutoPauseNoProgress). Cleared on Resume.
	ConsecutiveNoProgress int `json:"consecutive_no_progress,omitempty"`

	// AutoPaused is true when the loop was paused by a breaker (failures /
	// provider-unavailable / token-budget) rather than by an explicit
	// /loop pause. Surfaced to the UI so the warning toast can mention WHY the
	// pause happened. Cleared on Resume.
	AutoPaused bool `json:"auto_paused,omitempty"`

	// AutoPauseReason records WHICH breaker actually fired (one of the
	// AutoPause* constants). Set at the pause site so the UI reports the true
	// cause instead of RECONSTRUCTING it from loop state — reconstruction is
	// ambiguous when several conditions hold on the same iteration (e.g. a
	// failing task that also crosses its token budget paused for FAILURES, but
	// a spend-only check would mislabel it "budget"). Cleared on Resume. Empty
	// on legacy state files / manual pause → the UI shows a generic label.
	AutoPauseReason string `json:"auto_pause_reason,omitempty"`
}

// AutoPause* are the recorded reasons a loop was auto-paused (Loop.AutoPauseReason).
const (
	AutoPauseConsecutiveFailures = "consecutive_failures"
	AutoPauseProviderUnavailable = "provider_unavailable"
	AutoPauseTokenBudget         = "token_budget"
	AutoPauseNoProgress          = "no_progress"
)

// Iteration is the result of one execution of the loop's task.
type Iteration struct {
	N           int           `json:"n"` // 1-based iteration number
	StartedAt   time.Time     `json:"started_at"`
	Duration    time.Duration `json:"duration"`               // wall-clock for this iteration
	Summary     string        `json:"summary"`                // 1-3 sentence summary of what happened
	OK          bool          `json:"ok"`                     // false = iteration errored or model returned nothing useful
	Transient   bool          `json:"transient,omitempty"`    // true = failed for a transient infra reason (overload/rate-limit/network), not a task failure
	TimedOut    bool          `json:"timed_out,omitempty"`    // true = the iteration's own time budget fired mid-work (transient for the breaker, but a CUTOFF for the next prompt)
	MadeChanges bool          `json:"made_changes,omitempty"` // true = ran ≥1 code/repo-mutating tool this iteration (the churn signal)
	TokensIn    int           `json:"tokens_in,omitempty"`
	TokensOut   int           `json:"tokens_out,omitempty"`
	NextHint    string        `json:"next_hint,omitempty"` // e.g. "wait 1h" — self-paced runner respects

	// Handoff is the agent's own working state at the end of this iteration
	// (the trailing "HANDOFF:" block it was coached to leave): what's done,
	// what's in progress, the exact next step, blockers. Injected VERBATIM
	// into the next iteration's prompt so multi-iteration work continues as
	// ONE project instead of N independent attempts. Bounded at capture time
	// (MaxHandoffRunes) — it's model output that re-enters a prompt.
	Handoff string `json:"handoff,omitempty"`

	// FilesTouched lists the files this iteration successfully mutated
	// (workDir-relative, deduped, sorted; capped at
	// MaxFilesTouchedPerIteration). Gives the NEXT iteration concrete anchors
	// ("#3 edited internal/foo/bar.go") instead of re-grepping for its own
	// prior work, and makes the markdown log's file history real.
	FilesTouched []string `json:"files_touched,omitempty"`
}

// MaxHandoffRunes bounds a captured HANDOFF block. It is model output that
// gets re-injected into the next iteration's prompt — the
// bounded-model-input class (see CLAUDE.md, v0.85.15): without a cap a
// runaway block would bloat every future prompt and the state file.
const MaxHandoffRunes = 1500

// MaxFilesTouchedPerIteration caps the per-iteration touched-files list. An
// iteration that touched more files than this keeps the first N (sorted) —
// the anchor value is in the common few-files case, not exhaustiveness.
const MaxFilesTouchedPerIteration = 12

// MaxHistorySize caps the per-loop iterations slice. Set well above the
// number a typical user will care to scroll through but small enough that
// a runaway loop doesn't bloat the state file.
const MaxHistorySize = 50

// DefaultMinDelaySeconds is the floor between self-paced iterations.
// Prevents hot-loop runaway if the model keeps requesting "check again
// soon" — at minimum we wait this long.
const DefaultMinDelaySeconds = 300 // 5 minutes

// MaxSelfPacedDelaySeconds caps a model-suggested self-pacing delay. Without it
// an absurd "next:" hint (e.g. "next 99999h") parsed from the iteration output
// would push NextRunAt years out and silently disable the loop — the
// bounded-duration-from-model-input class (see CLAUDE.md, v0.85.15). 24h is
// generous for any autonomous cadence; a user-configured MinDelaySeconds above
// this still takes precedence.
const MaxSelfPacedDelaySeconds = 24 * 60 * 60 // 24 hours

// ConsecutiveFailureLimit caps the failures-in-a-row before auto-pause.
// Picked to absorb transient flakes (network blips, temporary 503s)
// without letting a genuinely broken loop run for hours: at the
// default 5-min self-paced cadence, 5 failures cost ~25 minutes of
// LLM time before the user is alerted; faster cadences burn through
// it sooner. Users can /loop resume after fixing root cause.
const ConsecutiveFailureLimit = 5

// TransientFailureLimit is the BACKSTOP for transient (infra) failures —
// much higher than ConsecutiveFailureLimit because a provider being
// overloaded/unreachable is not the loop's fault and should be waited out, not
// quickly given up on. With the capped exponential backoff below, this many
// consecutive transient failures spans roughly half a day of a provider being
// down — at which point auto-pausing and alerting the user is the right call
// (something is genuinely wrong) rather than poking a dead endpoint forever.
const TransientFailureLimit = 30

// TransientFailureWarnThreshold is a MID-STREAK heads-up: a loop quietly backing
// off through provider trouble would otherwise stay silent until the far-away
// TransientFailureLimit backstop (≈half a day). Crossing this many consecutive
// transient failures means the provider has been struggling for a while, so the
// UI emits ONE warning toast (fired exactly when the streak == this value) so
// the user can intervene early (switch provider, check the network) instead of
// discovering it hours later. Well below the backstop; only relevant once Fix
// #2 made iteration timeouts transient (deadline-heavy loops build these faster).
const TransientFailureWarnThreshold = 10

// NoProgressWarnThreshold / NoProgressLimit bound the churn breaker (ACTION
// tasks only): after this many consecutive OK-but-no-change iterations the agent
// is WARNED via its iteration prompt (decide: emit done., change approach, or
// slow the cadence); at the limit the loop auto-pauses (AutoPauseNoProgress) so
// a finished/stuck loop can't burn a quota-limited provider unsupervised. Warn
// is low (catch it early); the pause limit is generous so a slow-but-real task
// (occasional no-op iterations between real changes — those reset the streak)
// is never wrongly paused. Monitor/watch loops (IsMonitorTask) are exempt — for
// them no-change is the expected steady state, not churn.
const NoProgressWarnThreshold = 3
const NoProgressLimit = 10

// IsMonitorTask reports whether a loop's task is a long-running MONITOR/WATCH
// task (its steady state is "nothing changed") rather than an ACTION task that
// is supposed to make changes. Used to (a) exempt monitor loops from the churn
// breaker — for them no-change is correct, not spinning — and (b) tailor the
// done-coaching ("done. not expected; pace with next:"). Heuristic, EN+RU, and
// deliberately CONSERVATIVE toward action: a task is monitor only on a clear
// monitor verb, or recurring-check phrasing ("check … every") with NO action
// verb. When in doubt it's treated as ACTION, so the churn protection (the point
// of the feature) is never silently lost; a misclassified monitor loop merely
// gets warned/paused and the user resumes.
func IsMonitorTask(task string) bool {
	t := strings.ToLower(task)
	// Tier 1 — unambiguous monitor verbs → monitor regardless of anything else.
	for _, kw := range []string{
		"monitor", "watch", "poll", "uptime", "heartbeat",
		"keep an eye", "keep watching", "keep checking", "health check", "healthcheck",
		"следи", "монитор", "наблюда", "отслеживай", "опрашивай",
	} {
		if strings.Contains(t, kw) {
			return true
		}
	}
	// Tier 2 — a clear build/change verb → ACTION (the churn-protected default).
	// Checked BEFORE the weaker recurring-check signals so an action task that
	// merely mentions checking ("сделай X и проверяй результат", "check the tests
	// then fix") is NOT misread as monitor — the point of the feature must not be
	// silently lost (the v0.100.51 review caught `проверяй` escaping this gate).
	for _, kw := range []string{
		"fix", "implement", "refactor", "build", "create", "add ", "write ",
		"rename", "delete", "migrate", "cleanup", "реализ", "сделай", "почини",
		"исправь", "добавь", "напиши", "рефактор",
	} {
		if strings.Contains(t, kw) {
			return false
		}
	}
	// Tier 3 — no action verb: weaker recurring-check phrasing counts as monitor.
	// The Russian imperfective "проверяй" (keep checking) implies monitoring;
	// note "проверь" (check once) does NOT contain "проверя", so this is the
	// recurring form only. English bare "check" is too common in action tasks, so
	// it counts only with "every" ("check the deploy every 20m").
	if strings.Contains(t, "проверя") {
		return true
	}
	if strings.Contains(t, "every") && strings.Contains(t, "check") {
		return true
	}
	// The Russian mirror of the rule above, which existed only in English. Two
	// signals are required for the same reason: a bare look verb is as common in
	// action tasks as a bare "check", so periodicity has to be explicit. Without
	// this, an ordinary watch task ("раз в час смотри что нового") accrued
	// no-progress and auto-paused after ten iterations — for doing exactly the
	// job it was given, since making no changes is what watching looks like.
	if containsAnyOf(t, russianPeriodicMarkers) && containsAnyOf(t, russianInspectVerbs) {
		return true
	}
	return false
}

var (
	russianPeriodicMarkers = []string{"кажд", "раз в ", "периодическ", "регулярно", "по расписан"}
	russianInspectVerbs    = []string{"смотри", "гляди", "глянь", "смотреть", "провер"}
)

func containsAnyOf(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// transientBackoffBaseSeconds / transientBackoffCapSeconds bound the
// loop-level exponential backoff applied after a transient failure: the next
// iteration is pushed out base×2^(streak-1), capped. This throttles a fast
// loop from hammering an overloaded provider while staying cheap (one
// fast-failing probe per cap interval) and self-healing (the moment the
// provider recovers, the next probe succeeds and the streak resets). Slow
// loops are unaffected — their normal cadence already exceeds the backoff.
const (
	transientBackoffBaseSeconds = 60   // first transient failure → ~1m extra
	transientBackoffCapSeconds  = 1800 // cap at 30m between retries
)

// transientBackoffSeconds returns the extra delay (from LastRunAt) to impose
// before the next iteration after `streak` consecutive transient failures.
// Exponential from the base, capped — robust to a hostile streak value.
func transientBackoffSeconds(streak int) int64 {
	if streak < 1 {
		streak = 1
	}
	shift := streak - 1
	if shift > 20 { // 60 << 20 already dwarfs the cap; avoid int overflow
		shift = 20
	}
	delay := int64(transientBackoffBaseSeconds) << uint(shift)
	if delay <= 0 || delay > transientBackoffCapSeconds {
		delay = transientBackoffCapSeconds
	}
	return delay
}

// AppendIteration adds a result to the loop's history, dropping the
// oldest entry if we'd exceed MaxHistorySize. Updates IterationCount
// (lifetime), LastRunAt, SuccessCount/FailureCount,
// ConsecutiveFailures, and (for interval mode) the next run time. May
// auto-pause the loop when ConsecutiveFailures crosses the limit.
//
// Caller is responsible for persisting the loop after calling this.
func (l *Loop) AppendIteration(it Iteration) {
	l.IterationCount++
	// Lifetime token accounting — accrue regardless of outcome (a failed
	// iteration still spent tokens). Guard against negatives defensively.
	if it.TokensIn > 0 {
		l.TotalTokensIn += int64(it.TokensIn)
	}
	if it.TokensOut > 0 {
		l.TotalTokensOut += int64(it.TokensOut)
	}
	switch {
	case it.OK:
		l.SuccessCount++
		l.ConsecutiveFailures = 0
		l.ConsecutiveTransientFailures = 0
		// Churn accounting (ACTION tasks only): a no-op success means the loop is
		// spinning (task done, or stuck); a real change resets it. Monitor/watch
		// tasks are exempt — no-change is their normal steady state.
		if !IsMonitorTask(l.Task) {
			if it.MadeChanges {
				l.ConsecutiveNoProgress = 0
			} else {
				l.ConsecutiveNoProgress++
			}
		}
	case it.Transient:
		// Transient infra failure (provider overload/rate-limit, network).
		// It IS a failed iteration for stats, but it must NOT trip the
		// task-failure breaker — and it must not reset a building task-failure
		// streak either (a provider blip mid-streak is noise). The separate
		// transient streak drives loop-level backoff + the high backstop.
		l.FailureCount++
		l.ConsecutiveTransientFailures++
	default:
		// Genuine task failure. A real attempt reached the model and produced
		// a task-level result, so the transient streak is broken.
		l.FailureCount++
		l.ConsecutiveFailures++
		l.ConsecutiveTransientFailures = 0
	}
	l.Iterations = append(l.Iterations, it)
	if len(l.Iterations) > MaxHistorySize {
		// Drop oldest. Realloc fresh slice so the GC can free the old
		// backing array's head — for a 50-entry cap this is cheap.
		trimmed := make([]Iteration, MaxHistorySize)
		copy(trimmed, l.Iterations[len(l.Iterations)-MaxHistorySize:])
		l.Iterations = trimmed
	}
	l.LastRunAt = it.StartedAt.Add(it.Duration)

	// Compute NextRunAt based on mode.
	switch l.Mode {
	case ModeInterval:
		if l.IntervalSeconds > 0 {
			l.NextRunAt = l.LastRunAt.Add(time.Duration(l.IntervalSeconds) * time.Second)
		}
	case ModeSelfPaced:
		delay := l.MinDelaySeconds
		if delay <= 0 {
			delay = DefaultMinDelaySeconds
		}
		// If the iteration suggested a longer delay via NextHint, honor it —
		// but cap the model-supplied value so an absurd hint can't push the next
		// run years out and silently kill the loop. A user-set floor above the
		// cap still wins (capped hint can't drop below the floor).
		hinted := parseNextHintSeconds(it.NextHint)
		if hinted > MaxSelfPacedDelaySeconds {
			hinted = MaxSelfPacedDelaySeconds
		}
		if hinted > delay {
			delay = hinted
		}
		l.NextRunAt = l.LastRunAt.Add(time.Duration(delay) * time.Second)
	}

	// Transient-failure loop-level backoff: push the next run out by capped
	// exponential backoff so a busy/unreachable provider isn't hammered every
	// cadence. Take the LATER of the normal schedule and the backoff, so slow
	// loops (whose interval already exceeds the backoff) are unaffected while
	// fast / self-paced loops get throttled. The request-level patient retry
	// (agent/message_processor) rides out brief spikes WITHIN an iteration;
	// this is the second tier for spikes that outlast a single iteration.
	if it.Transient && l.ConsecutiveTransientFailures > 0 {
		backoff := time.Duration(transientBackoffSeconds(l.ConsecutiveTransientFailures)) * time.Second
		if backoffRun := l.LastRunAt.Add(backoff); backoffRun.After(l.NextRunAt) {
			l.NextRunAt = backoffRun
		}
	}

	// Cap on max iterations? Mark completed.
	if l.MaxIterations > 0 && l.IterationCount >= l.MaxIterations {
		l.Status = StatusCompleted
		l.StoppedAt = l.LastRunAt
		return
	}

	// Consecutive-failure circuit breaker. Auto-pause so the user sees
	// the issue before the next iteration burns more credits. Skip if
	// we just transitioned to Completed (max iterations reached); the
	// terminal state already prevents further runs and overriding it
	// to Paused would be wrong (a "completed" loop shouldn't suddenly
	// become resumable just because the last few iterations failed).
	// Breaker PRECEDENCE is defined by check order here: each is gated on
	// StatusRunning, so the FIRST to fire wins and records its reason; the rest
	// see Status==Paused and skip. The UI reads AutoPauseReason (don't
	// reconstruct from state — that mislabels overlapping conditions).
	if l.ConsecutiveFailures >= ConsecutiveFailureLimit && l.Status == StatusRunning {
		l.Status = StatusPaused
		l.AutoPaused = true
		l.AutoPauseReason = AutoPauseConsecutiveFailures
	}

	// Transient-failure backstop: a provider that has been overloaded /
	// unreachable for this many consecutive iterations (≈half a day with the
	// capped backoff) is treated as genuinely down — auto-pause and alert
	// rather than keep probing indefinitely. Far more lenient than the
	// task-failure breaker so ordinary spikes never reach it.
	if l.ConsecutiveTransientFailures >= TransientFailureLimit && l.Status == StatusRunning {
		l.Status = StatusPaused
		l.AutoPaused = true
		l.AutoPauseReason = AutoPauseProviderUnavailable
	}

	// Token-budget breaker: an unattended loop can quietly burn a quota-limited
	// provider's cap (GLM Coding-Plan → error 1308). When the user set a budget
	// and lifetime spend has reached it, auto-pause so spend can't run away
	// unsupervised. Reuses the AutoPaused breaker pattern; gated on
	// StatusRunning so the MaxIterations→Completed precedence (early return
	// above) is preserved. Resume re-arms it (the cap still binds, so resuming
	// over budget re-pauses next iteration — the user raises/clears the cap).
	if l.MaxTotalTokens > 0 && (l.TotalTokensIn+l.TotalTokensOut) >= l.MaxTotalTokens && l.Status == StatusRunning {
		l.Status = StatusPaused
		l.AutoPaused = true
		l.AutoPauseReason = AutoPauseTokenBudget
	}

	// Churn (no-progress) breaker — ACTION tasks only (ConsecutiveNoProgress
	// stays 0 for monitor tasks, so they never reach here). A run of OK-but-no-
	// change iterations means the loop is spinning (finished or stuck); auto-pause
	// so it can't burn a quota-limited provider unsupervised. The agent was warned
	// earlier (NoProgressWarnThreshold, via the iteration prompt). Lowest
	// precedence — gated on StatusRunning so the failure/transient/budget breakers
	// above win when several conditions hold.
	if l.ConsecutiveNoProgress >= NoProgressLimit && l.Status == StatusRunning {
		l.Status = StatusPaused
		l.AutoPaused = true
		l.AutoPauseReason = AutoPauseNoProgress
	}
}

// OverTokenBudget reports whether a token budget is set and lifetime spend has
// reached it. Used by the UI to attribute an auto-pause to the budget (vs a
// failure streak) and to label /loop status.
func (l *Loop) OverTokenBudget() bool {
	return l.MaxTotalTokens > 0 && (l.TotalTokensIn+l.TotalTokensOut) >= l.MaxTotalTokens
}

// IsActive returns true if the loop is in a state where the scheduler
// should consider firing it.
func (l *Loop) IsActive() bool {
	return l.Status == StatusRunning
}

// IsDue returns true if the loop is active AND its NextRunAt is in the
// past (or unset, meaning "fire immediately when first picked up").
func (l *Loop) IsDue(now time.Time) bool {
	if !l.IsActive() {
		return false
	}
	if l.NextRunAt.IsZero() {
		return true
	}
	return !now.Before(l.NextRunAt)
}

// Validate checks that a loop is internally consistent. Called by storage
// on load and by Manager.Add on create — surfaces malformed state files
// as clear errors instead of letting them silently misbehave at runtime.
func (l *Loop) Validate() error {
	if strings.TrimSpace(l.ID) == "" {
		return fmt.Errorf("loop: id is empty")
	}
	if !isSafeLoopID(l.ID) {
		return fmt.Errorf("loop %q: id contains unsafe characters", l.ID)
	}
	if strings.TrimSpace(l.Task) == "" {
		return fmt.Errorf("loop %s: task is empty", l.ID)
	}
	switch l.Mode {
	case ModeInterval:
		if l.IntervalSeconds <= 0 {
			return fmt.Errorf("loop %s: interval mode needs interval_seconds > 0", l.ID)
		}
	case ModeSelfPaced:
		// MinDelaySeconds may be 0 — the AppendIteration path falls back
		// to DefaultMinDelaySeconds.
	default:
		return fmt.Errorf("loop %s: unknown mode %q", l.ID, l.Mode)
	}
	switch l.Status {
	case StatusRunning, StatusPaused, StatusStopped, StatusCompleted:
		// OK
	default:
		return fmt.Errorf("loop %s: unknown status %q", l.ID, l.Status)
	}
	return nil
}

func isSafeLoopID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// parseNextHintSeconds parses common "next check" hints from the iteration
// summary. Recognizes formats like "wait 30m", "next 1h", "in 5m". Returns
// 0 when the hint is absent or unparseable — caller treats 0 as "use the
// default min delay".
func parseNextHintSeconds(hint string) int64 {
	hint = strings.TrimSpace(strings.ToLower(hint))
	if hint == "" {
		return 0
	}
	// Strip common prefix words.
	for _, prefix := range []string{"wait", "next", "in", "after", "every"} {
		hint = strings.TrimPrefix(hint, prefix)
	}
	hint = strings.TrimSpace(hint)
	d, err := time.ParseDuration(hint)
	if err != nil {
		return parseHumanDurationSeconds(hint)
	}
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}

// parseHumanDurationSeconds accepts the phrasing LLMs naturally produce in
// self-paced hints ("30 minutes", "1 hour", "2 days"). Keep it deliberately
// narrow so ordinary prose doesn't become a schedule by accident.
func parseHumanDurationSeconds(hint string) int64 {
	fields := strings.Fields(strings.Trim(hint, " ."))
	for len(fields) > 0 && isApproxWord(fields[0]) {
		fields = fields[1:]
	}

	if len(fields) != 2 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	var seconds int64
	switch strings.TrimSuffix(fields[1], "s") {
	case "sec", "second":
		seconds = 1
	case "min", "minute":
		seconds = 60
	case "hr", "hour":
		seconds = 60 * 60
	case "day":
		seconds = 24 * 60 * 60
	default:
		return 0
	}
	if n > math.MaxInt64/seconds {
		return math.MaxInt64
	}
	return n * seconds
}

func isApproxWord(s string) bool {
	switch s {
	case "about", "around", "approximately", "approx", "roughly":
		return true
	default:
		return false
	}
}

// Marshal returns the canonical JSON encoding for this loop. Used by
// Storage; exposed so tests and /loop status formatting can use it too.
func (l *Loop) Marshal() ([]byte, error) {
	return json.MarshalIndent(l, "", "  ")
}

// Unmarshal parses the canonical JSON encoding. Validates the result so
// corrupt files are rejected at the boundary, not after some downstream
// nil-deref.
func Unmarshal(data []byte) (*Loop, error) {
	var l Loop
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("loop: parse json: %w", err)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}
