package ui

import (
	"fmt"
	"strings"
	"time"

	"gokin/internal/format"

	"github.com/charmbracelet/lipgloss"
)

// Visual design of the live activity indicator.
//
// Design target: the card adds what the status bar can't show — the specific
// action happening right now — and nothing else. Provider, model, state
// (WRITING / WORKING / IDLE), the context-usage bar, and the "esc Interrupt"
// key hint are all permanently visible in the bottom status bar. Repeating
// them here just steals vertical space.
//
// Current shape (big Live Activity panel closed):
//
//	▎ Writing: - `tui.go` — Model struct fields
//	▎ ↳ Editing tui_status_bar.go → applied (63ms)
//	▎ → Plan step 3/8
//
// The colored left-edge bar (▎) IS the state indicator — its hue matches
// the status bar's state chip, so state reads from colour, not repeated
// text. When the big Live Activity panel is open (Ctrl+O), the card
// collapses to just the current-action line; Recent/Next are already in
// the panel below.
//
// Earlier iterations had a bordered "chip" (● WRITING), repeated meta
// (kimi · kimi-for-coding), and an "Esc cancel" footer — every one of
// those duplicated something the status bar already shows. Removed.
func (m Model) shouldShowLiveActivityCard() bool {
	if m.isModalState() {
		return false
	}
	if m.state == StateProcessing || m.state == StateStreaming {
		return true
	}
	if len(m.backgroundTasks) > 0 {
		return true
	}
	return m.activityFeed != nil && m.activityFeed.HasActiveEntries()
}

// spinnerFramesCard is the braille-dot animation used as an inline leading
// indicator on the first line of the card when the agent is actively
// working. We pick the frame from a monotonic wall-clock source so the
// animation is consistent across every re-render within the same tick.
var spinnerFramesCard = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// renderLiveActivityCard renders the compact "now" card. feedRendered says
// whether the big activity feed panel is ACTUALLY drawn this frame (not just
// IsVisible — the panel is suppressed while the agent tree renders); when the
// feed is on screen the card collapses to the current-action line.
func (m Model) renderLiveActivityCard(feedRendered bool) string {
	if !m.shouldShowLiveActivityCard() {
		return ""
	}
	if m.width <= 0 {
		return ""
	}

	width := m.width - 2
	if width < 1 {
		width = 1
	}

	accent := m.liveActivityAccentColor()
	barStyle := lipgloss.NewStyle().Foreground(accent)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	valueStyle := lipgloss.NewStyle().Foreground(ColorText)

	snapshot := ActivityFeedSnapshot{}
	if m.activityFeed != nil {
		snapshot = m.activityFeed.Snapshot(4, 2)
	}

	// When the big Live Activity panel is actually on screen, collapse to
	// just the current-action line — Recent/Next live in the panel below.
	feedOpen := feedRendered
	rowBudget := liveActivityCardRowBudget(m.height)

	// Line 1: the current action — no state badge, no meta. State comes
	// from the accent color on the left bar; state word + provider + model
	// are already permanent fixtures in the bottom status bar.
	current := m.liveActivityCurrentLine(snapshot)
	if current == "" {
		// Nothing specific to show and the card would otherwise be empty —
		// better to render nothing than a bare accent bar with no content.
		return ""
	}

	// Pick the leading glyph for the first line: animated braille spinner
	// while the agent is working (Processing/Streaming), static bar (▎)
	// otherwise. The spinner replaces — not complements — the bar, so the
	// user sees "⠋ Writing: …" on one line instead of a separate spinner
	// line below the card. Same colour scheme (state accent) either way.
	firstGlyph := "▎"
	if m.state == StateProcessing || m.state == StateStreaming {
		if m.reducedMotion {
			firstGlyph = "●"
		} else {
			elapsed := time.Duration(0)
			if !m.streamStartTime.IsZero() {
				elapsed = time.Since(m.streamStartTime)
			}
			frameIdx := int(elapsed.Milliseconds()/80) % len(spinnerFramesCard)
			firstGlyph = spinnerFramesCard[frameIdx]
		}
	}
	// A glyph plus the normal separating space needs three cells once the
	// truncation disclosure is included. Keep degenerate terminals bounded
	// while still exposing that work is active; the status row carries the
	// textual state at these widths.
	if m.width == 1 {
		return barStyle.Render(firstGlyph)
	}
	if m.width == 2 {
		return barStyle.Render(firstGlyph) + dimStyle.Render("…")
	}

	// MINIMAL mode (default, Claude-Code style): ONE dim unobtrusive line —
	// spinner + current action + elapsed — nothing else. Ctrl+O (works during
	// Processing/Streaming too) expands to the full card below. The dim style
	// keeps the line readable but visually quiet next to streamed output.
	if !m.liveDetailExpanded {
		minimalStyle := lipgloss.NewStyle().Foreground(ColorMuted)
		return barStyle.Render(firstGlyph) + " " +
			minimalStyle.Render(truncateSafeForWidth(current, max(width-2, 1)))
	}

	// Prefix every line with a coloured leading glyph. First line uses the
	// animated spinner (when active); subsequent lines keep the static bar
	// so the animation doesn't become visually noisy.
	var out strings.Builder
	out.WriteString(barStyle.Render(firstGlyph) + " ")
	out.WriteString(valueStyle.Render(truncateSafeForWidth(current, max(width-2, 1))))
	if feedOpen || rowBudget <= 1 {
		return out.String()
	}
	rowsUsed := 1
	appendTodo := func(todo string) {
		out.WriteByte('\n')
		out.WriteString(barStyle.Render("▎") + " " +
			valueStyle.Render(truncateSafeForWidth(todo, max(width-2, 1))))
		rowsUsed++
	}
	appendNext := func(next string) {
		out.WriteByte('\n')
		out.WriteString(barStyle.Render("▎") + " " +
			dimStyle.Render("→ ") +
			valueStyle.Render(truncateSafeForWidth(next, max(width-4, 1))))
		rowsUsed++
	}

	// A stalled/idle warning is more urgent than plan orientation on a short
	// terminal. Render it immediately after the current action; ordinary next
	// context remains below the Todo row on roomier screens.
	next := m.liveActivityNextLine(snapshot)
	if m.streamIdleMsg != "" && next != "" && rowsUsed < rowBudget {
		appendNext(next)
		next = ""
	}

	// Todo progress: when the agent is working through a plan (todo list),
	// surface the currently-active step (or next pending) so users see
	// "we're on step 3 of 7" instead of having to guess from tool calls.
	// The line lives below the current-action row because it's a durable
	// orientation signal, not a transient one.
	if todo := m.liveActivityTodoLine(); todo != "" && rowsUsed < rowBudget {
		appendTodo(todo)
	}

	if rowsUsed < rowBudget {
		// No "recent completed" echo here — the finished tool is already a row
		// in scrollback right above the card, so repeating it ("↳ Running: … ->
		// completed") was pure duplication. Only surface genuinely NEW signal
		// (retries, rate limits, plan progression, parallel work); generic status
		// echoes are dropped inside liveActivityNextLine.
		if next != "" {
			appendNext(next)
		}
	}

	return out.String()
}

func liveActivityCardRowBudget(terminalHeight int) int {
	if terminalHeight <= 0 {
		return 3
	}
	return min(max(terminalHeight-5, 1), 3)
}

func (m Model) liveActivityAccentColor() lipgloss.Color {
	switch {
	case m.retryAttempt > 0 || (!m.rateLimitWaitUntil.IsZero() && time.Now().Before(m.rateLimitWaitUntil)):
		return ColorWarning
	case m.streamIdleMsg != "":
		return ColorInfo
	case m.currentTool != "":
		return GetToolIconColor(m.currentTool)
	case m.responseToolFailures > 0:
		return ColorWarning
	case m.state == StateStreaming:
		return ColorSuccess
	default:
		return ColorGradient2
	}
}

// withElapsed appends the elapsed wall-clock once past the "barely started"
// threshold, so the working line answers "is it hung, or working?" the way CC's
// `(12s)` does. Below 3s it returns s unchanged (no jittery early clock).
func (m Model) withElapsed(s string) string {
	s = safeKeyEntryText(s)
	if m.streamStartTime.IsZero() {
		return s
	}
	if elapsed := time.Since(m.streamStartTime); elapsed >= 3*time.Second {
		return s + " · " + format.Duration(elapsed)
	}
	return s
}

func (m Model) liveActivityCurrentLine(snapshot ActivityFeedSnapshot) string {
	if m.currentTool != "" {
		title := displayToolName(safeKeyEntryText(m.currentTool))
		if title == "" {
			title = "Tool"
		}
		if active := len(m.activeToolCalls); active > 1 {
			title = fmt.Sprintf("%s · %d in flight", title, active)
		}
		if info := safeKeyEntryText(m.currentToolInfo); info != "" {
			title += " — " + info
		}
		if !m.toolStartTime.IsZero() {
			title += " · " + format.Duration(max(time.Since(m.toolStartTime), 0))
		}
		return title
	}

	if m.state == StateStreaming {
		// What the agent is DOING beats an echo of what it's typing. The
		// line prefers STABLE, semantic signals and only falls back to the
		// raw stream snippet (which changes every chunk and often reads as
		// a mid-list fragment) when nothing better exists:
		//
		//  1. reasoning phase — thinking chunks never enter
		//     currentResponseBuf, so the old code showed "Writing: <stale
		//     last text line>" (mid-turn) or a bare "Writing response"
		//     (turn start) while the model reasoned silently for minutes;
		//  2. the agent's own declared activity (in-progress todo, via
		//     ActivityLabelMsg) — "Implementing backup/restore";
		//  3. the section heading the answer is currently under —
		//     "Writing: Root cause analysis";
		//  4. the raw snippet (start of the current line, v0.100.57).
		thinking := m.output.IsThinkingActive()
		if act := safeKeyEntryText(m.currentActivity); act != "" {
			if thinking {
				return m.withElapsed(act + " · thinking")
			}
			return m.withElapsed(act)
		}
		if thinking {
			return m.withElapsed("Thinking")
		}
		if h := lastStreamHeading(m.currentResponseBuf.String()); h != "" {
			return m.withElapsed("Writing: " + truncateSafeForWidth(h, max(m.width-20, 24)))
		}
		// Clean the snippet: strip leading bullets/dashes/stars from the
		// model's markdown so "Writing: - `tui.go`" doesn't read as a
		// double-dash. Cleaning may reduce a pure-bullet snippet to "" — in
		// that case we fall through to the generic "Writing response" label
		// rather than render "Writing: " with a dangling space.
		cleaned := ""
		if raw := m.lastStreamSnippet(); raw != "" {
			cleaned = cleanStreamSnippet(raw)
		}
		switch {
		case cleaned != "":
			return m.withElapsed("Writing: " + cleaned)
		case m.responseToolCount > 0:
			return m.withElapsed("Writing response · " + formatToolRunSummary(m.responseToolCount, m.responseToolFailures, true))
		default:
			return m.withElapsed("Writing response")
		}
	}

	activity := safeKeyEntryText(m.currentActivity)
	label := safeKeyEntryText(m.processingLabel)
	if activity != "" && isGenericLiveProcessingLabel(label) {
		return m.withElapsed(activity)
	}
	if label != "" {
		// Append elapsed wall-clock once past the "barely started" threshold —
		// answers "has it really been thinking for a while?" without waiting for
		// a tool call to land.
		return m.withElapsed(label)
	}

	// The agent's specific activity ("Implementing backup/restore", set via
	// ActivityLabelMsg) — surfaced HERE (the card is the active renderer during
	// processing) instead of only in the now-suppressed inline spinner block, so
	// the working line reads the real task rather than a generic "Thinking".
	if activity != "" {
		return m.withElapsed(activity)
	}

	if len(m.backgroundTasks) > 0 {
		return m.formatBackgroundTaskStatus(len(m.backgroundTasks))
	}

	if snapshot.RunningAgents > 0 || snapshot.RunningTools > 0 {
		return fmt.Sprintf("%d agents · %d tools", snapshot.RunningAgents, snapshot.RunningTools)
	}

	return ""
}

func isGenericLiveProcessingLabel(label string) bool {
	switch strings.ToLower(safeKeyEntryText(label)) {
	case "", "analyzing", "connecting", "generating", "thinking", "working":
		return true
	default:
		return false
	}
}

// liveActivityTodoLine surfaces the agent's current plan position as a
// single row in the live activity card — the user can see "which step"
// without opening the full todo panel (Ctrl+T). Returns "" when no todos
// exist or when none are actively or pending (all done).
//
// Priority picks, in order:
//  1. An in-progress step marked `[/]` (the agent just started this).
//  2. The next pending step marked `[ ]`.
//
// Format: "☐ Step 3/7: Refactor rate limit display" — position is the
// 1-based index of the chosen step among *all* items, not only pending.
// That way "Step 3/7" stays anchored to the plan even as earlier steps
// complete.
func (m Model) liveActivityTodoLine() string {
	if len(m.todoItems) == 0 {
		return ""
	}

	parseTodo := func(raw string) (marker, text string) {
		s := normalizeTimelineText(raw)
		if strings.HasPrefix(s, "- ") {
			s = strings.TrimSpace(s[2:])
		}
		if len(s) >= 3 && s[0] == '[' && s[2] == ']' {
			return string(s[1]), strings.TrimSpace(s[3:])
		}
		return "", s
	}

	type parsedTodo struct {
		marker string
		text   string
	}
	items := make([]parsedTodo, 0, len(m.todoItems))
	for _, raw := range m.todoItems {
		marker, text := parseTodo(raw)
		switch marker {
		case "x", "X", "/", " ":
			if text != "" {
				items = append(items, parsedTodo{marker: marker, text: text})
			}
		}
	}
	if len(items) == 0 {
		return ""
	}

	total := len(items)
	pickIdx := -1
	pickMarker := ""
	pickText := ""

	// First pass: in-progress. Second pass: next pending. Done items
	// ([x]) and empty lines skipped.
	for i, item := range items {
		if item.marker == "/" {
			pickIdx = i
			pickMarker = item.marker
			pickText = item.text
			break
		}
	}
	if pickIdx < 0 {
		for i, item := range items {
			if item.marker == " " {
				pickIdx = i
				pickMarker = item.marker
				pickText = item.text
				break
			}
		}
	}
	if pickIdx < 0 {
		return ""
	}

	glyph := "☐"
	switch pickMarker {
	case "/":
		glyph = "◐" // partial / in-progress
	case " ":
		glyph = "☐" // empty box / pending
	}

	return fmt.Sprintf("%s Step %d/%d: %s", glyph, pickIdx+1, total, pickText)
}

// liveActivityNextLine is only rendered when it tells the user something
// they can't infer from the header. Generic "watching for next tool call"
// dropped — it was noise.
func (m Model) liveActivityNextLine(snapshot ActivityFeedSnapshot) string {
	// Provider recovery / rate-limit lines moved OUT — the status bar's engine
	// badge ("↻ retry N/M · cooldown" / "RATE LIMIT · resumes in …") is the
	// single home for provider-health state, so it isn't duplicated on screen.
	if idle := safeKeyEntryText(m.streamIdleMsg); idle != "" {
		return idle
	}

	if m.planProgressMode && m.planProgress != nil {
		title := safeKeyEntryText(m.planProgress.CurrentTitle)
		if title != "" {
			return title
		}
		return fmt.Sprintf("Plan step %d/%d", m.planProgress.CurrentStepID, m.planProgress.TotalSteps)
	}

	if snapshot.RunningTools > 1 {
		return fmt.Sprintf("%d tools running in parallel", snapshot.RunningTools)
	}
	if snapshot.RunningAgents > 0 {
		return fmt.Sprintf("%d background agents working", snapshot.RunningAgents)
	}
	return ""
}

// cleanStreamSnippet removes leading markdown list markers (-, *, •, numbered
// bullets) and whitespace from a streaming snippet so the "Writing: X"
// header prefix doesn't collide with the snippet's own leading punctuation.
// Inline formatting (backticks, inline code) is preserved — that's real
// content signal.
//
// Returns "" for pure-marker inputs (the model emitted just "- " with no
// content yet) so the caller can fall back to a generic label instead of
// rendering "Writing: -" or "Writing: " with a dangling suffix.
func cleanStreamSnippet(snippet string) string {
	s := strings.TrimSpace(snippet)
	// A bare bullet character with no trailing content — drop. Upstream
	// lastStreamSnippet TrimSpace'd away the space after the marker, so
	// we see "-" / "*" / "•" rather than "- ".
	switch s {
	case "", "-", "*", "•":
		return ""
	}
	// Strip one leading bullet marker if present.
	switch {
	case strings.HasPrefix(s, "- "):
		s = s[2:]
	case strings.HasPrefix(s, "* "):
		s = s[2:]
	case strings.HasPrefix(s, "• "):
		s = strings.TrimSpace(s[len("• "):])
	}
	// Strip a leading numbered bullet "1. ", "12. " etc.
	if idx := strings.Index(s, ". "); idx > 0 && idx <= 3 {
		if allDigits(s[:idx]) {
			s = s[idx+2:]
		}
	}
	return strings.TrimSpace(s)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// truncateSafeForWidth sanitizes control characters out of text and clips it to
// a DISPLAY WIDTH, not a rune count — it delegates to truncateForWidth, so a
// CJK or emoji run costs its real two columns. It was called truncateRunes with
// a maxRunes parameter, which said the opposite of what it does and made every
// audit of the width-versus-rune class stop here to re-derive that it is safe.
func truncateSafeForWidth(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	return truncateForWidth(safeKeyEntryText(text), maxWidth)
}
