package ui

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// ansiCSIPattern matches Control Sequence Introducer escapes — SGR (colour
// resets, foregrounds, backgrounds) and friends. Used by isVisuallyBlank
// to recognise lines that carry only formatting escapes and would render
// as blank rows in the terminal.
var ansiCSIPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// safeTerminalDisplayText removes terminal control sequences from externally
// supplied multi-line content while preserving ordinary newlines and tabs.
// Metadata that must stay on one visual row should use safeKeyEntryText.
func safeTerminalDisplayText(text string) string {
	text = ansi.Strip(text)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r':
			return -1
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// safeInlineDisplayText makes external metadata safe for a single terminal
// row without rewriting meaningful horizontal spacing inside commands or
// filenames. Line separators become spaces; ANSI and other controls disappear.
func safeInlineDisplayText(text string) string {
	text = ansi.Strip(text)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\v', '\f', '\u0085', '\u2028', '\u2029':
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// verticalMouseWheelDirection recognizes the ordinary vertical wheel gesture
// shared by selectable list overlays. Release events and Shift+wheel are left
// to viewport.Model so horizontal scrolling keeps its native behavior.
func verticalMouseWheelDirection(msg tea.MouseMsg) int {
	if msg.Action != tea.MouseActionPress || msg.Shift {
		return 0
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return -1
	case tea.MouseButtonWheelDown:
		return 1
	default:
		return 0
	}
}

// visibleTerminalControlText makes terminal control bytes reviewable without
// executing them. Unlike safeTerminalDisplayText it deliberately preserves
// their meaning as Unicode control pictures, which is essential on approval
// surfaces where silently hiding proposed bytes would be misleading.
func visibleTerminalControlText(text string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case 0x7f:
			return '␡'
		}
		if r >= 0 && r < 0x20 {
			return rune(0x2400) + r
		}
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, text)
}

// safeToolOutputDisplayText removes terminal styling/envelopes from ordinary
// command output, then makes any remaining raw controls reviewable. Unlike diff
// approval surfaces, routine output should not show harmless SGR bytes as text.
func safeToolOutputDisplayText(text string) string {
	return expandDisplayTabs(visibleTerminalControlText(ansi.Strip(text)), 4)
}

func expandDisplayTabs(text string, tabSize int) string {
	if tabSize <= 0 || !strings.Contains(text, "\t") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		graphemes := uniseg.NewGraphemes(line)
		var b strings.Builder
		column := 0
		for graphemes.Next() {
			cluster := graphemes.Str()
			if cluster == "\t" {
				spaces := tabSize - column%tabSize
				b.WriteString(strings.Repeat(" ", spaces))
				column += spaces
				continue
			}
			b.WriteString(cluster)
			column += lipgloss.Width(cluster)
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

// truncateLeftForWidth preserves the identifying tail of paths and other
// suffix-significant labels while fitting an exact terminal-cell budget.
func truncateLeftForWidth(text string, width int) string {
	if width <= 0 {
		return ""
	}
	current := lipgloss.Width(text)
	if current <= width {
		return text
	}
	if width == 1 {
		return "…"
	}
	// Build the tail using the same width implementation that renders it.
	// This avoids x/ansi vs Lip Gloss disagreements for mixed emoji/Cyrillic.
	graphemes := uniseg.NewGraphemes(ansi.Strip(text))
	var clusters []string
	for graphemes.Next() {
		clusters = append(clusters, graphemes.Str())
	}
	budget := width - lipgloss.Width("…")
	used := 0
	start := len(clusters)
	for i := len(clusters) - 1; i >= 0; i-- {
		clusterWidth := lipgloss.Width(clusters[i])
		if used+clusterWidth > budget {
			break
		}
		used += clusterWidth
		start = i
	}
	return "…" + strings.Join(clusters[start:], "")
}

// truncateMiddleForWidth keeps both the distinguishing prefix and the
// extension/suffix of a single label. It measures Unicode grapheme clusters
// with Lip Gloss so emoji cannot make a supposedly one-row filename wrap.
func truncateMiddleForWidth(text string, width int) string {
	if width <= 0 {
		return ""
	}
	text = ansi.Strip(text)
	if lipgloss.Width(text) <= width {
		return text
	}
	if width == 1 {
		return "…"
	}
	graphemes := uniseg.NewGraphemes(text)
	var clusters []string
	for graphemes.Next() {
		clusters = append(clusters, graphemes.Str())
	}
	budget := width - lipgloss.Width("…")
	leftBudget := budget / 2
	leftEnd, leftUsed := 0, 0
	for leftEnd < len(clusters) {
		clusterWidth := lipgloss.Width(clusters[leftEnd])
		if leftUsed+clusterWidth > leftBudget {
			break
		}
		leftUsed += clusterWidth
		leftEnd++
	}
	rightBudget := budget - leftUsed
	rightStart, rightUsed := len(clusters), 0
	for rightStart > leftEnd {
		clusterWidth := lipgloss.Width(clusters[rightStart-1])
		if rightUsed+clusterWidth > rightBudget {
			break
		}
		rightUsed += clusterWidth
		rightStart--
	}
	return strings.Join(clusters[:leftEnd], "") + "…" + strings.Join(clusters[rightStart:], "")
}

// isVisuallyBlank reports whether a line renders as empty after the
// terminal strips ANSI escapes. Lines like "\x1b[0m" — emitted by syntax
// highlighters around source blank lines — pass `s == ""` but visually
// occupy a blank row inside a card body; this catches them.
func isVisuallyBlank(s string) bool {
	return strings.TrimSpace(ansiCSIPattern.ReplaceAllString(s, "")) == ""
}

// getTextSelectionHint returns a platform/terminal-specific hint for text selection.
func getTextSelectionHint() string {
	term := os.Getenv("TERM_PROGRAM")
	termEnv := os.Getenv("TERM")

	switch {
	case runtime.GOOS == "darwin" && term == "iTerm.app":
		return "Hold ⌥+Drag to select text"
	case runtime.GOOS == "darwin" && term == "Apple_Terminal":
		return "Hold Fn+Drag to select text"
	case term == "WezTerm":
		return "Hold Shift+Drag to select text"
	case strings.Contains(termEnv, "kitty") || term == "kitty":
		return "Hold Shift+Drag to select text"
	case term == "Alacritty" || strings.Contains(termEnv, "alacritty"):
		return "Hold Shift+Drag to select text"
	case runtime.GOOS == "darwin":
		return "Hold ⌥+Drag (iTerm) or Fn+Drag (Terminal) to select"
	default:
		return "Hold Shift+Drag to select text"
	}
}

// copyViaOSC52 copies text to clipboard via OSC 52 escape sequence.
func copyViaOSC52(text string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Fprintf(os.Stderr, "\033]52;c;%s\a", encoded)
}

type clipboardCopyOutcome struct {
	SystemError error
	OSC52Sent   bool
}

// copyTextToClipboard uses both supported transports and reports what can
// actually be confirmed. OSC52 is fire-and-forget (terminals do not ack it),
// while the system clipboard returns an error. UI feedback must distinguish
// "copied" from "fallback request sent" instead of claiming success blindly.
func copyTextToClipboard(text string) clipboardCopyOutcome {
	return copyTextToClipboardUsing(text, clipboard.WriteAll, copyViaOSC52)
}

func copyTextToClipboardUsing(text string, writeSystem func(string) error, sendOSC52 func(string)) clipboardCopyOutcome {
	outcome := clipboardCopyOutcome{}
	if sendOSC52 != nil {
		sendOSC52(text)
		outcome.OSC52Sent = true
	}
	if writeSystem != nil {
		outcome.SystemError = writeSystem(text)
	} else {
		outcome.SystemError = fmt.Errorf("system clipboard unavailable")
	}
	return outcome
}

func showClipboardCopyFeedback(manager *ToastManager, successMessage string, outcome clipboardCopyOutcome) {
	if manager == nil {
		return
	}
	if outcome.SystemError == nil {
		manager.ShowSuccess(successMessage)
		return
	}
	if outcome.OSC52Sent {
		manager.ShowWarning("Copy request sent via terminal · system clipboard unavailable")
		return
	}
	manager.ShowError("Could not copy · system clipboard unavailable")
}

// getMacOSBattery removed as battery monitoring is disabled.

// SetBadgeCmd returns a tea.Cmd that sets the iTerm2 badge.
// Uses stderr to avoid corrupting Bubble Tea's stdout rendering.
func SetBadgeCmd(badge string) tea.Cmd {
	return func() tea.Msg {
		if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
			encoded := base64.StdEncoding.EncodeToString([]byte(badge))
			fmt.Fprintf(os.Stderr, "\033]1337;SetBadgeFormat=%s\a", encoded)
		}
		return nil
	}
}

// ClearBadgeCmd returns a tea.Cmd that clears the iTerm2 badge.
// Uses stderr to avoid corrupting Bubble Tea's stdout rendering.
func ClearBadgeCmd() tea.Cmd {
	return func() tea.Msg {
		if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
			fmt.Fprintf(os.Stderr, "\033]1337;SetBadgeFormat=\a")
		}
		return nil
	}
}

const terminalTitleMaxWidth = 160

// writeTerminalTitle keeps runtime metadata inside one OSC-0 envelope. Model
// names and working directories may come from config files, providers, or the
// filesystem, so writing either directly would let BEL/ST terminate the title
// and turn the rest into a second terminal control sequence.
func writeTerminalTitle(w io.Writer, title string) {
	title = truncateForWidth(safeKeyEntryText(title), terminalTitleMaxWidth)
	if title == "" {
		title = "Gokin"
	}
	fmt.Fprintf(w, "\033]0;%s\a", title)
}

// setTerminalTitle sets the terminal window title via OSC-0 escape sequence.
// Visible in terminal tabs, tmux, iTerm2, etc.
func setTerminalTitle(title string) {
	writeTerminalTitle(os.Stderr, title)
}

func (m *Model) refreshTerminalTitle() {
	modelName := safeKeyEntryText(m.currentModel)
	if modelName == "" {
		modelName = "no model"
	}
	dir := shortenPath(safeKeyEntryText(prettyPath(m.workDir)), 30)
	setTerminalTitle(fmt.Sprintf("Gokin · %s · %s", modelName, dir))
}

// Welcome displays a minimalist welcome message using lipgloss borders.
//
// What lands here matters disproportionately: it's the first thing the
// user reads after typing `gokin`. We surface three things on this
// screen and nothing else: WHO they're talking to (model + path),
// WHAT mode they're in (so they're not surprised by plan-mode
// approval prompts), and HOW to do common things (Ctrl+P / Shift+Tab /
// slash). Quick-start suggestions sit just outside the box so users
// who already know the app can skim past them.
func (m *Model) Welcome() {
	m.ShowFirstLaunchWelcome(nil)
}

// welcomeModeBadge renders the current session-mode indicator for the
// welcome banner. Lives separately from the status-bar version because
// the banner has more vertical real-estate and benefits from a short
// human-readable description right next to the badge — first-time users
// shouldn't have to learn what "plan mode" means by hitting it
// experimentally.
func (m *Model) welcomeModeBadge() string {
	planStyle := lipgloss.NewStyle().Foreground(ColorInfo).Bold(true)
	yoloStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	normalStyle := lipgloss.NewStyle().Foreground(ColorDim)
	hintStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	switch {
	case m.planningModeEnabled:
		return planStyle.Render("✦ plan mode") + " " +
			hintStyle.Render("(read-only · proposes plan before acting)")
	case !m.permissionsEnabled && !m.sandboxEnabled:
		return yoloStyle.Render("⚠ YOLO mode") + " " +
			hintStyle.Render("(no prompts · agent runs everything)")
	case !m.permissionsEnabled:
		return yoloStyle.Render("⚠ YOLO mode") + " " +
			hintStyle.Render("(no prompts · bash remains sandboxed)")
	case !m.sandboxEnabled:
		return yoloStyle.Render("⚠ sandbox off") + " " +
			hintStyle.Render("(prompts remain · approved bash runs unrestricted)")
	default:
		return normalStyle.Render("● normal mode") + " " +
			hintStyle.Render("(asks before write/edit/bash)")
	}
}

// AddSystemMessage adds a system message to the output.
func (m *Model) AddSystemMessage(msg string) {
	iconStyle := lipgloss.NewStyle().Foreground(ColorDim)
	// Quiet dim notice (was bold saturated teal) — a transient "switched model"
	// note should recede, not out-shout the model's prose.
	textStyle := lipgloss.NewStyle().
		Foreground(ColorMuted).
		MarginBottom(1)
	m.output.AppendLine(iconStyle.Render("  ▸ ") + textStyle.Render(msg))
}

// renderTodos renders the current todo items.
func (m Model) renderTodos() string {
	if len(m.todoItems) == 0 {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	if width < 4 {
		return truncateForWidth("Tasks", width)
	}
	maxPanelHeight := todosPanelHeightBudget(m.height)
	if maxPanelHeight > 0 && maxPanelHeight < 4 {
		return truncateForWidth("Tasks · Ctrl+T hide", width)
	}
	horizontalPadding := 1
	if width < 6 {
		horizontalPadding = 0
	}
	contentWidth := max(width-2-horizontalPadding*2, 1)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorGradient2).
		Padding(0, horizontalPadding)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorAccent)

	itemStyle := lipgloss.NewStyle().Foreground(ColorText)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	items := make([]todoRenderItem, 0, len(m.todoItems))
	var activeCount, pendingCount, doneCount int
	for _, raw := range m.todoItems {
		item := parseTodoRenderItem(raw)
		items = append(items, item)
		switch item.status {
		case todoStatusActive:
			activeCount++
		case todoStatusPending:
			pendingCount++
		case todoStatusDone:
			doneCount++
		}
	}

	spinChar := "●"
	if !m.reducedMotion {
		spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		idx := int(time.Now().UnixNano()/100000000) % len(spinner)
		spinChar = spinner[idx]
	}

	showFooter := true
	rowBudget := todosPanelFullRender
	if maxPanelHeight > 0 {
		if maxPanelHeight >= 5 {
			rowBudget = min(rowBudget, max(maxPanelHeight-4, 1))
		} else {
			showFooter = false
			rowBudget = 1
		}
	}
	visible := selectTodoRows(items, rowBudget)
	hiddenDone := 0
	hiddenLive := 0
	for i, item := range items {
		if visible[i] {
			continue
		}
		if item.status == todoStatusDone {
			hiddenDone++
		} else {
			hiddenLive++
		}
	}

	var builder strings.Builder
	title := fmt.Sprintf("Tasks · %d/%d done", doneCount, len(items))
	if activeCount > 0 {
		title += fmt.Sprintf(" · %d active", activeCount)
	}
	if !showFooter {
		title = "Tasks · Ctrl+T hide"
		if hidden := hiddenDone + hiddenLive; hidden > 0 {
			title += fmt.Sprintf(" · %d folded", hidden)
		}
	}
	builder.WriteString(titleStyle.Render(truncateForWidth(title, contentWidth)))
	builder.WriteString("\n")

	for index, item := range items {
		if !visible[index] {
			continue
		}
		var styledItem string
		switch item.status {
		case todoStatusPending:
			styledItem = m.styles.TodoPending.Render("○ " + item.text)
		case todoStatusActive:
			styledItem = m.styles.TodoActive.Render(spinChar + " " + item.text)
		case todoStatusDone:
			styledItem = m.styles.TodoDone.Render("✓ " + item.text)
		default:
			styledItem = itemStyle.Render("• " + item.text)
		}

		builder.WriteString(fitPanelContent(styledItem, contentWidth))
		builder.WriteString("\n")
	}

	if showFooter {
		// Keep the inverse action first so it survives narrow-width truncation.
		footerParts := []string{"Ctrl+T hide"}
		if hiddenDone > 0 {
			footerParts = append(footerParts, fmt.Sprintf("✓ %d completed", hiddenDone))
		}
		if hiddenLive > 0 {
			footerParts = append(footerParts, fmt.Sprintf("… %d more", hiddenLive))
		}
		if pendingCount == 0 && activeCount == 0 {
			footerParts = append(footerParts, "All complete")
		}
		footer := strings.Join(footerParts, " · ")
		builder.WriteString(dimStyle.Render(truncateForWidth(footer, contentWidth)))
	}

	content := fitPanelContent(strings.TrimSuffix(builder.String(), "\n"), contentWidth)
	return boxStyle.Width(width - 2).Render(content)
}

type todoRenderItem struct {
	status string
	text   string
}

const (
	todoStatusPending = "pending"
	todoStatusActive  = "active"
	todoStatusDone    = "done"
	todoStatusOther   = "other"
)

func parseTodoRenderItem(raw string) todoRenderItem {
	text := normalizeTimelineText(raw)
	if strings.HasPrefix(text, "- ") {
		text = strings.TrimSpace(text[2:])
	}
	item := todoRenderItem{status: todoStatusOther, text: text}
	for prefix, status := range map[string]string{
		"[ ]": todoStatusPending,
		"[/]": todoStatusActive,
		"[x]": todoStatusDone,
		"[X]": todoStatusDone,
	} {
		if strings.HasPrefix(text, prefix) {
			item.status = status
			item.text = strings.TrimSpace(text[len(prefix):])
			break
		}
	}
	if item.text == "" {
		item.text = "Untitled task"
	}
	return item
}

func selectTodoRows(items []todoRenderItem, budget int) map[int]bool {
	budget = max(budget, 1)
	selected := make(map[int]bool, min(len(items), budget))
	if len(items) <= budget {
		for i := range items {
			selected[i] = true
		}
		return selected
	}
	// Operational priority wins, while the final render retains source order.
	for _, status := range []string{todoStatusActive, todoStatusPending, todoStatusOther} {
		for i, item := range items {
			if len(selected) >= budget {
				return selected
			}
			if item.status == status {
				selected[i] = true
			}
		}
	}
	return selected
}

// todosPanelFullRender is the largest todo list that still renders every
// item, including completed ones. Above it, ✓ items collapse to one
// "✓ N completed" row — the pending/active rows are the signal.
const todosPanelFullRender = 8

// todosPanelHeightBudget reserves four terminal rows for the surrounding
// composer/status chrome and caps a task list at twelve rows. A zero height
// preserves the standalone rendering contract.
func todosPanelHeightBudget(terminalHeight int) int {
	if terminalHeight <= 0 {
		return 0
	}
	return min(terminalHeight, min(max(terminalHeight-4, 4), 12))
}

// renderScratchpad renders the agent scratchpad.
func (m Model) renderScratchpad() string {
	text := strings.TrimSpace(m.scratchpad)
	if text == "" {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	if width < 4 {
		return truncateForWidth("Notes", width)
	}
	maxPanelHeight := scratchpadPanelHeightBudget(m.height)
	if maxPanelHeight > 0 && maxPanelHeight < 4 {
		return truncateForWidth("Notes · latest", width)
	}
	horizontalPadding := 1
	if width < 6 {
		horizontalPadding = 0
	}
	contentWidth := max(width-2-horizontalPadding*2, 1)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorAccent).
		Padding(0, horizontalPadding)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorPrimary)

	contentStyle := lipgloss.NewStyle().
		Foreground(ColorText)

	// Render only the TAIL of the scratchpad — it's append-style working
	// notes, so the latest lines carry the signal. Unbounded render let a
	// chatty agent grow the box to half the screen.
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = normalizeTimelineText(lines[i])
		if lines[i] == "" {
			lines[i] = "·"
		}
	}
	lineBudget := scratchpadMaxRenderLines
	showFoldMarker := true
	if maxPanelHeight > 0 {
		availableRows := max(maxPanelHeight-3, 1) // border + title
		lineBudget = min(lineBudget, availableRows)
		if len(lines) > lineBudget {
			if availableRows >= 2 {
				lineBudget = min(lineBudget, availableRows-1)
			} else {
				showFoldMarker = false
			}
		}
	}
	hidden := max(len(lines)-lineBudget, 0)
	if hidden > 0 {
		lines = lines[hidden:]
	}

	var builder strings.Builder
	title := "Scratchpad · latest notes"
	if hidden > 0 && !showFoldMarker {
		title = fmt.Sprintf("Notes · %d earlier", hidden)
	}
	builder.WriteString(titleStyle.Render(truncateForWidth(title, contentWidth)))
	builder.WriteString("\n")

	if hidden > 0 && showFoldMarker {
		dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
		builder.WriteString(dimStyle.Render(fmt.Sprintf("… %d earlier line(s)", hidden)))
		builder.WriteString("\n")
	}
	for i, line := range lines {
		if i > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(contentStyle.Render(truncateForWidth(line, contentWidth)))
	}

	content := fitPanelContent(builder.String(), contentWidth)
	return boxStyle.Width(width - 2).Render(content)
}

// scratchpadMaxRenderLines caps the scratchpad box height (content lines,
// excluding the title and the "… earlier" marker).
const scratchpadMaxRenderLines = 6

// scratchpadPanelHeightBudget reserves five rows for surrounding live/composer
// chrome and caps the note panel at its existing ten-row maximum.
func scratchpadPanelHeightBudget(terminalHeight int) int {
	if terminalHeight <= 0 {
		return 0
	}
	return min(terminalHeight, min(max(terminalHeight-5, 4), scratchpadMaxRenderLines+4))
}

// getCommandHint returns a hint for the current command input.
func (m Model) getCommandHint(input string) string {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return ""
	}
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if cmd == "" {
		return ""
	}

	// Most copy comes from the same catalog that powers autocomplete. Overrides
	// are reserved for behavior that genuinely needs more context than the
	// canonical one-sentence description (especially independent safety axes).
	overrides := map[string]string{
		"clear":       "Start fresh; saves an active plan for /resume-plan",
		"logout":      "Remove one provider key; 'all' previews until --force",
		"permissions": "View or configure risky-action prompts; sandbox is separate",
		"sandbox":     "View or configure bash containment; permission prompts are separate",
		"thinking":    "Configure adaptive, forced, or disabled reasoning and its token budget",
		"theme":       "Show the active Graphite UI theme",
	}
	if hint, ok := overrides[cmd]; ok {
		return hint
	}
	for _, command := range DefaultCommands() {
		if command.Name == cmd {
			return command.Description
		}
	}
	return ""
}

func (m Model) inlineCommandHint() string {
	if m.state != StateInput || m.input.showSuggestions || m.input.showArgHints {
		return ""
	}
	input := m.input.Value()
	if !strings.HasPrefix(input, "/") || len(input) <= 1 {
		return ""
	}
	hint := m.getCommandHint(input)
	if hint == "" {
		return ""
	}
	return truncateForWidth("  "+safeKeyEntryText(hint), max(m.width, 1))
}

// prettyPath returns path with the user's home directory collapsed to "~".
// Unconditional — unlike shortenPath this does *not* depend on length, so
// it's safe for the status bar where we always want `~/github/gokin` over
// the longer `/Users/ginkida/github/gokin`. Returns "." for empty input
// so callers don't have to guard.
func prettyPath(path string) string {
	if path == "" {
		return "."
	}
	home, _ := os.UserHomeDir()
	if home == "" || !strings.HasPrefix(path, home) {
		return path
	}
	// Must match either exactly (path == home) or have a separator after
	// so /Users/ginkidaX isn't rewritten as ~X.
	rest := path[len(home):]
	if rest == "" {
		return "~"
	}
	if rest[0] == '/' {
		return "~" + rest
	}
	return path
}

// shortenPath shortens a path to fit within maxLen terminal cells while
// preserving the filename. Grapheme clusters stay intact, so a skin-tone
// modifier or ZWJ sequence can never be detached by truncation.
func shortenPath(path string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if lipgloss.Width(path) <= maxLen {
		return path
	}

	// Try to show ~/... for home directory paths
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}

	if lipgloss.Width(path) <= maxLen {
		return path
	}

	// Three dots consume the whole budget at tiny widths and convey less than
	// the single-cell ellipsis used by the shared tail truncator.
	if maxLen <= 3 {
		return truncateLeftForWidth(path, maxLen)
	}

	// Smart truncation: preserve filename and show as much context as possible
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		// No slash, truncate from the left
		return "..." + displayCellSuffix(path, maxLen-3)
	}

	filename := path[lastSlash:] // includes leading /
	filenameWidth := lipgloss.Width(filename)

	// If filename alone fits, show directory prefix + ... + filename
	if filenameWidth < maxLen-6 { // 6 = width("...") + some prefix
		availableForDir := maxLen - filenameWidth - 3
		if availableForDir > 3 {
			return displayCellPrefix(path, availableForDir) + "..." + filename
		}
	}

	// Fallback: truncate from the left to always show filename
	return "..." + displayCellSuffix(path, maxLen-3)
}

func displayCellPrefix(text string, width int) string {
	if width <= 0 {
		return ""
	}
	graphemes := uniseg.NewGraphemes(text)
	var b strings.Builder
	used := 0
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := lipgloss.Width(cluster)
		if used+clusterWidth > width {
			break
		}
		b.WriteString(cluster)
		used += clusterWidth
	}
	return b.String()
}

func displayCellSuffix(text string, width int) string {
	if width <= 0 {
		return ""
	}
	graphemes := uniseg.NewGraphemes(text)
	var clusters []string
	for graphemes.Next() {
		clusters = append(clusters, graphemes.Str())
	}
	used := 0
	start := len(clusters)
	for i := len(clusters) - 1; i >= 0; i-- {
		clusterWidth := lipgloss.Width(clusters[i])
		if used+clusterWidth > width {
			break
		}
		used += clusterWidth
		start = i
	}
	return strings.Join(clusters[start:], "")
}

// extractToolInfo extracts displayable info from tool arguments.
// This is used as a fallback when the switch statement doesn't match.
func extractToolInfo(args map[string]any) string {
	if args == nil {
		return ""
	}

	// Priority keys - check in order of preference
	priorityKeys := []string{
		"file_path",
		"path",
		"directory_path",
		"source",
		"destination",
		"command",
		"pattern",
		"query",
		"url",
		"action",
		"operation",
		"name",
	}

	for _, key := range priorityKeys {
		if val, ok := args[key]; ok {
			switch v := val.(type) {
			case string:
				if v != "" {
					return shortenPath(v, 50)
				}
			}
		}
	}
	return ""
}

// renderResponseMetadata renders the response metadata as a compact dim footer.
// Format: 1.2k in · 3.4k out · 4.1s
func (m Model) renderResponseMetadata(meta ResponseMetadataMsg) string {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var parts []string

	// Input tokens
	if meta.InputTokens > 0 {
		var s string
		if meta.InputTokens >= 1000 {
			s = fmt.Sprintf("%.1fk in", float64(meta.InputTokens)/1000)
		} else {
			s = fmt.Sprintf("%d in", meta.InputTokens)
		}
		parts = append(parts, s)
	}

	// Output tokens
	if meta.OutputTokens > 0 {
		var s string
		if meta.OutputTokens >= 1000 {
			s = fmt.Sprintf("%.1fk out", float64(meta.OutputTokens)/1000)
		} else {
			s = fmt.Sprintf("%d out", meta.OutputTokens)
		}
		parts = append(parts, s)
	}

	// Cache read tokens (prompt caching)
	if meta.CacheReadInputTokens > 0 {
		var s string
		if meta.CacheReadInputTokens >= 1000 {
			s = fmt.Sprintf("%.1fk cached", float64(meta.CacheReadInputTokens)/1000)
		} else {
			s = fmt.Sprintf("%d cached", meta.CacheReadInputTokens)
		}
		parts = append(parts, s)
	}

	// Timing breakdown: total duration with tool/thinking split
	if meta.Duration > 0 {
		toolDur := m.responseToolDuration
		thinkingDur := meta.Duration - toolDur
		if thinkingDur < 0 {
			thinkingDur = 0
		}

		// Show breakdown when tools were used
		if m.responseToolCount > 0 && toolDur > 0 {
			parts = append(parts, fmt.Sprintf("thinking %s", formatCompactDuration(thinkingDur)))
			toolLabel := fmt.Sprintf("tools %s (%s)", formatCompactDuration(toolDur),
				formatToolRunSummary(m.responseToolCount, m.responseToolFailures, false))
			parts = append(parts, toolLabel)
		} else {
			parts = append(parts, formatCompactDuration(meta.Duration))
		}
	}

	// Per-message cost estimate
	if meta.Cost > 0 {
		parts = append(parts, formatResponseCost(meta.Cost))
	}

	// Normally the status bar already shows the selected model, so repeating it
	// in every footer is noise. Surface identity only when an opt-in fallback
	// actually served the response — crucial for attribution and billing.
	if meta.FallbackUsed && meta.Provider != "" {
		identity := meta.Provider
		if meta.Model != "" {
			identity += "/" + meta.Model
		}
		parts = append(parts, "via "+identity)
	}

	if len(parts) == 0 {
		return ""
	}

	// A quiet dim stats line — NO "─ … ─" framing. The dashes were the same
	// inter-turn ruler the v0.100.37 work removed; they re-appeared here on every
	// real turn (tokens are always present). The numbers stay (handy for cost),
	// just without the chrome around them.
	return dimStyle.Render(strings.Join(parts, "  ·  "))
}

func formatResponseCost(cost float64) string {
	if cost <= 0 {
		return ""
	}
	if cost < 0.0001 {
		return "< $0.0001"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func formatToolRunSummary(count, failures int, includeUsed bool) string {
	if count <= 0 {
		return ""
	}
	word := "tools"
	if count == 1 {
		word = "tool"
	}
	summary := fmt.Sprintf("%d %s", count, word)
	if includeUsed {
		summary += " used"
	}
	if failures > 0 {
		summary += fmt.Sprintf(" · %d failed", failures)
	}
	return summary
}

// formatCompactDuration formats a duration compactly: "242ms", "1.2s", "2.1m".
func formatCompactDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

// AppendOutput appends text to the output.
func (m *Model) AppendOutput(text string) {
	m.output.AppendText(text)
}

// AppendOutputLine appends a line to the output.
func (m *Model) AppendOutputLine(text string) {
	m.output.AppendLine(text)
}

// AppendMarkdown appends markdown to the output.
func (m *Model) AppendMarkdown(text string) {
	m.output.AppendMarkdown(text)
}

// LoadInputHistory loads input history from file.
func (m *Model) LoadInputHistory() error {
	return m.input.LoadHistory()
}

// SaveInputHistory saves input history to file.
func (m *Model) SaveInputHistory() error {
	return m.input.SaveHistory()
}

// ClearOutput clears the output viewport.
func (m *Model) ClearOutput() {
	m.output.Clear()
}

// ResetInput clears the input field.
func (m *Model) ResetInput() {
	m.input.Reset()
}

// StreamText sends a stream text message.
func StreamText(text string) tea.Cmd {
	return func() tea.Msg {
		return StreamTextMsg(text)
	}
}

// StreamThinking sends a stream thinking message.
func StreamThinking(text string) tea.Cmd {
	return func() tea.Msg {
		return StreamThinkingMsg(text)
	}
}

// ToolCall sends a tool call message.
func ToolCall(name string, args map[string]any) tea.Cmd {
	return func() tea.Msg {
		return ToolCallMsg{Name: name, Args: args}
	}
}

// ToolResult sends a tool result message.
func ToolResult(name string, result string) tea.Cmd {
	return func() tea.Msg {
		return ToolResultMsg{Name: name, Content: result}
	}
}

// ToolProgress sends a tool progress message (heartbeat for long operations).
func ToolProgress(name string, elapsed time.Duration) tea.Cmd {
	return func() tea.Msg {
		return ToolProgressMsg{Name: name, Elapsed: elapsed, Progress: -1}
	}
}

// ResponseDone signals that a response is complete.
func ResponseDone() tea.Cmd {
	return func() tea.Msg {
		return ResponseDoneMsg{}
	}
}

// SendError sends an error message.
func SendError(err error) tea.Cmd {
	return func() tea.Msg {
		return ErrorMsg(err)
	}
}

// UpdateTodos updates the todo list display.
func UpdateTodos(items []string) tea.Cmd {
	return func() tea.Msg {
		return TodoUpdateMsg(items)
	}
}

// PermissionRequest sends a permission request message.
func PermissionRequest(toolName string, args map[string]any, riskLevel, reason string) tea.Cmd {
	return func() tea.Msg {
		return PermissionRequestMsg{
			ToolName:  toolName,
			Args:      args,
			RiskLevel: riskLevel,
			Reason:    reason,
		}
	}
}

// QuestionRequest sends a question request message.
func QuestionRequest(question string, options []string, defaultOpt string) tea.Cmd {
	return func() tea.Msg {
		return QuestionRequestMsg{
			Question: question,
			Options:  options,
			Default:  defaultOpt,
		}
	}
}

// PlanApprovalRequest sends a plan approval request message.
func PlanApprovalRequest(title, description string, steps []PlanStepInfo) tea.Cmd {
	return func() tea.Msg {
		return PlanApprovalRequestMsg{
			Title:       title,
			Description: description,
			Steps:       steps,
		}
	}
}

// ResponseMetadata sends a response metadata message.
func ResponseMetadata(model string, inputTokens, outputTokens int, duration time.Duration, toolsUsed []string) tea.Cmd {
	return func() tea.Msg {
		return ResponseMetadataMsg{
			Model:        model,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Duration:     duration,
			ToolsUsed:    toolsUsed,
		}
	}
}

// lastStreamHeading returns the most recent markdown heading (outside code
// fences) in the streaming buffer — the SECTION the answer is currently
// writing, which reads as an activity ("Root cause analysis") where the raw
// current-line snippet reads as a fragment. Fence awareness matters: shell
// snippets inside ``` blocks are full of `# comment` lines that would
// otherwise masquerade as headings. Returns "" when no heading has streamed.
func lastStreamHeading(buf string) string {
	if buf == "" {
		return ""
	}
	inFence := false
	last := ""
	for _, raw := range strings.Split(buf, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if h := markdownHeadingText(line); h != "" {
			last = safeKeyEntryText(h)
		}
	}
	return last
}

// markdownHeadingText extracts the text of an ATX heading line ("## Title",
// optionally with closing hashes "## Title ##"). Returns "" for non-headings.
func markdownHeadingText(line string) string {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return ""
	}
	text := strings.TrimSpace(line[i+1:])
	// Strip an ATX closing sequence (trailing run of #'s preceded by a space).
	if j := strings.LastIndexByte(text, ' '); j >= 0 && strings.Trim(text[j+1:], "#") == "" {
		text = strings.TrimSpace(text[:j])
	}
	return text
}

// lastStreamSnippet returns the last non-empty line of the streaming buffer for
// the live "Writing: …" preview, keeping its START — the action/intent the model
// is expressing ("Я добавлю несуществующие тесты…"), which is what answers "what
// is it doing now". It does NOT front-truncate (keep the tail): that cut the
// leading verb mid-word ("…лю несуществующие тесты и исправлю типы"), so the line
// read as gibberish. Length is budgeted to the card width so the readable start
// AND the trailing "· <elapsed>" both survive the card's own width clip; trimmed
// at a word boundary with a trailing "…". A short line is returned as-is.
func (m Model) lastStreamSnippet() string {
	buf := m.currentResponseBuf.String()
	if buf == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(buf, "\n"), "\n")
	last := safeKeyEntryText(lines[len(lines)-1])
	if last == "" && len(lines) > 1 {
		last = safeKeyEntryText(lines[len(lines)-2])
	}
	if last == "" {
		return ""
	}
	// Reserve ~20 cols for the "Writing: " prefix + " · <elapsed>" suffix so the
	// card's truncateSafeForWidth(width-2) clip doesn't eat the elapsed. Floor so a
	// tiny/zero width (before the first WindowSizeMsg) still shows something.
	budget := max(m.width-20, 24)
	runes := []rune(last)
	if len(runes) <= budget {
		return last
	}
	cut := string(runes[:budget])
	if sp := strings.LastIndex(cut, " "); sp > budget/2 {
		cut = cut[:sp] // prefer a word boundary over a mid-word cut
	}
	return strings.TrimRight(cut, " ,.;:—-") + "…"
}
