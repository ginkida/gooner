package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/logging"
	"gokin/internal/repl"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

// HelpCommand shows help for commands.
type HelpCommand struct {
	handler *Handler
}

func (c *HelpCommand) Name() string        { return "help" }
func (c *HelpCommand) Description() string { return "Show help for commands" }
func (c *HelpCommand) Usage() string       { return "/help [command]" }
func (c *HelpCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryGettingStarted,
		Icon:     "help",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "[command]",
	}
}

func (c *HelpCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if len(args) > 0 {
		// Show help for specific command
		cmd, exists := c.handler.GetCommand(args[0])
		if !exists {
			// Match the typo-correction UX of regular Execute (commands.go:277).
			// Without this, /help typo just said "Unknown command" without
			// pointing to the likely intended command — inconsistent with
			// how unknown commands are handled at the runtime path.
			if closest := c.handler.findClosestCommand(args[0]); closest != "" {
				return fmt.Sprintf("%sUnknown command: /%s%s. Did you mean %s/%s%s?\nUse /help to see all commands.",
					colorRed, args[0], colorReset, colorGreen, closest, colorReset), nil
			}
			return fmt.Sprintf("%sUnknown command: /%s%s\nUse /help to see all commands.", colorRed, args[0], colorReset), nil
		}
		result := fmt.Sprintf("%s/%s%s — %s\n\n%sUsage:%s %s",
			colorGreen, cmd.Name(), colorReset, cmd.Description(),
			colorCyan, colorReset, cmd.Usage())

		if example := getCommandExample(cmd.Name()); example != "" {
			result += fmt.Sprintf("\n\n%sExamples:%s\n%s", colorCyan, colorReset, example)
		}
		if related := getRelatedCommands(cmd.Name()); related != "" {
			result += fmt.Sprintf("\n%sSee also:%s %s", colorCyan, colorReset, related)
		}

		return result, nil
	}

	var sb strings.Builder

	// Essential Commands — the most useful commands at a glance
	fmt.Fprintf(&sb, "\n%s─── Essential Commands ───%s\n\n", colorYellow, colorReset)

	essentials := []struct {
		name string
		desc string
	}{
		{"help", "Show help (this page) or /help <cmd> for details"},
		{"quickstart", "Guided examples and safety modes"},
		{"model", "Switch AI model"},
		{"clear", "Clear conversation history"},
		{"save", "Save current session"},
		{"commit", "Create a git commit with AI-generated message"},
		{"plan", "Toggle planning mode"},
		{"doctor", "Check environment and configuration"},
	}

	for _, e := range essentials {
		fmt.Fprintf(&sb, "  %s/%-10s%s %s\n", colorGreen, e.name, colorReset, e.desc)
	}

	// All Commands grouped by 6 categories
	fmt.Fprintf(&sb, "\n%s─── All Commands ───%s\n", colorYellow, colorReset)

	categories := []struct {
		name     string
		commands []string
	}{
		{"Getting Started", []string{"help", "quickstart", "shortcuts"}},
		{"Session", []string{"model", "thinking", "clear", "compact", "save", "resume", "sessions", "stats", "tasks", "audit", "cost", "instructions", "memory", "loop", "undo", "redo"}},
		{"Auth & Setup", []string{"login", "logout", "keys", "provider", "status", "doctor", "config", "set", "settings", "update", "restart", "whats-new", "changelog"}},
		{"Git", []string{"init", "commit", "pr", "diff", "log", "branches", "grep", "blame", "show"}},
		{"Planning", []string{"plan", "resume-plan", "checkpoints", "health", "policy", "ledger", "plan-proof", "journal", "recovery", "observability", "insights", "memory-governance", "tree-stats"}},
		{"Tools", []string{"browse", "open", "pwd", "mcp", "skill", "copy", "paste", "clear-todos", "ql", "permissions", "sandbox", "timeout", "theme", "hooks", "add-dir", "remove-dir", "debug-dump",
			"register-agent-type", "list-agent-types", "unregister-agent-type"}},
	}

	// Build a map for quick lookup
	cmds := c.handler.ListCommands()
	cmdMap := make(map[string]Command)
	for _, cmd := range cmds {
		cmdMap[cmd.Name()] = cmd
	}

	for _, cat := range categories {
		var catCmds []Command
		for _, name := range cat.commands {
			if cmd, ok := cmdMap[name]; ok {
				catCmds = append(catCmds, cmd)
				delete(cmdMap, name)
			}
		}
		if len(catCmds) == 0 {
			continue
		}

		fmt.Fprintf(&sb, "\n  %s%s%s\n", colorBold, cat.name, colorReset)
		for _, cmd := range catCmds {
			fmt.Fprintf(&sb, "    %s/%-22s%s %s%s%s\n", colorGreen, cmd.Name(), colorReset, colorCyan, cmd.Description(), colorReset)
		}
	}

	// Show any uncategorized commands
	if len(cmdMap) > 0 {
		fmt.Fprintf(&sb, "\n  %sOther%s\n", colorBold, colorReset)
		var remaining []Command
		for _, cmd := range cmdMap {
			remaining = append(remaining, cmd)
		}
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].Name() < remaining[j].Name()
		})
		for _, cmd := range remaining {
			fmt.Fprintf(&sb, "    %s/%-22s%s %s%s%s\n", colorGreen, cmd.Name(), colorReset, colorCyan, cmd.Description(), colorReset)
		}
	}

	// Keyboard Shortcuts — must stay in sync with internal/ui/shortcuts.go
	// (DefaultShortcuts) and the bindings handled in internal/ui/tui.go's
	// handleGlobalKeys. The shortcuts overlay (`?`) is the authoritative
	// list; this is a short subset for the most-used bindings.
	fmt.Fprintf(&sb, "\n%s─── Keyboard Shortcuts ───%s\n\n", colorYellow, colorReset)
	shortcuts := []struct {
		key  string
		desc string
	}{
		{"Ctrl+P", "Command palette"},
		{"Ctrl+S", "Open settings"},
		{"Ctrl+K", "Open model selector"},
		{"Ctrl+E", "Toggle last output (expand appends; compact keeps existing scrollback)"},
		{"Shift+Tab", "Cycle mode: Normal → Plan → YOLO → Normal"},
		{"Ctrl+H", "Context Observatory (technical health)"},
		{"Ctrl+T", "Toggle task list"},
		{"Ctrl+O", "Toggle live activity detail"},
		{"Ctrl+U / Ctrl+D", "Scroll half page up / down (empty input)"},
		{"Alt+C", "Copy last response"},
		{"Ctrl+G", "Toggle select mode (freeze + native selection)"},
		{"Ctrl+L", "Clear screen"},
		{"Ctrl+R", "Search input history"},
		{"Ctrl+C", "Cancel once, quit on second press"},
		{"Esc", "Cancel current operation"},
		{"?", "Show all keyboard shortcuts (filterable; empty input)"},
	}
	for _, s := range shortcuts {
		fmt.Fprintf(&sb, "  %s%-16s%s %s\n", colorGreen, s.key, colorReset, s.desc)
	}

	fmt.Fprintf(&sb, "\nTip: Use %sCtrl+P%s for the full command palette, or %s?%s for the filterable shortcuts overlay.\n",
		colorGreen, colorReset, colorGreen, colorReset)
	fmt.Fprintf(&sb, "\n%sSession modes:%s Normal asks before write/edit/bash; Plan explores read-only and proposes a plan; YOLO disables prompts and sandbox. The active mode remains visible in the status bar.\n",
		colorYellow, colorReset)

	return sb.String(), nil
}

// ClearCommand clears the conversation history.
type ClearCommand struct{}

func (c *ClearCommand) Name() string        { return "clear" }
func (c *ClearCommand) Description() string { return "Clear conversation history" }
func (c *ClearCommand) Usage() string       { return "/clear [--force]" }
func (c *ClearCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "clear",
		Priority: 10,
		HasArgs:  true,
		ArgHint:  "[--force]",
	}
}

func (c *ClearCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	force := false
	for _, arg := range args {
		if arg != "--force" {
			return "Invalid argument. Use /clear or /clear --force.", nil
		}
		force = true
	}

	// Count messages before clearing
	msgCount := 0
	if session := app.GetSession(); session != nil {
		msgCount = len(session.GetHistory())
	}
	todoTool := app.GetTodoTool()
	todoCount := 0
	if todoTool != nil {
		todoCount = len(todoTool.GetItems())
	}

	// Save current plan before clearing (so it can be resumed with /resume-plan)
	planSaved := false
	planSaveWarning := ""
	if pm := app.GetPlanManager(); pm != nil {
		if currentPlan := pm.GetCurrentPlan(); currentPlan != nil && !currentPlan.IsComplete() {
			var saveErr error
			if pm.GetPlanStore() == nil {
				saveErr = errors.New("plan storage is unavailable")
			} else {
				saveErr = pm.SaveCurrentPlan()
			}
			if saveErr != nil {
				logging.Warn("failed to save plan before clear", "error", saveErr)
				if !force {
					return fmt.Sprintf("Clear cancelled: active plan could not be saved (%s). No conversation data was cleared. Retry, or use /clear --force to discard recovery.",
						singleLineDisplayText(saveErr.Error(), 160)), nil
				}
				planSaveWarning = fmt.Sprintf(" Active plan was not saved: %s.", singleLineDisplayText(saveErr.Error(), 160))
			} else {
				planSaved = true
			}
		}
	}

	if err := clearConversationChecked(app); err != nil {
		return "", fmt.Errorf("clear conversation safely: %w", err)
	}
	app.RefreshTokenCount()
	// Also clear todos
	if todoTool != nil {
		todoTool.ClearItems()
	}

	msg := fmt.Sprintf("Started a fresh conversation: removed %d messages and %d todos.", msgCount, todoCount)
	if planSaved {
		msg += " Active plan saved for /resume-plan."
	}
	msg += planSaveWarning
	return msg, nil
}

// CompactCommand forces context compaction.
type CompactCommand struct{}

func (c *CompactCommand) Name() string        { return "compact" }
func (c *CompactCommand) Description() string { return "Force context compaction/summarization" }
func (c *CompactCommand) Usage() string       { return "/compact" }
func (c *CompactCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category:         CategorySession,
		Icon:             "compress",
		Priority:         20,
		LongRunning:      true,
		LongRunningLabel: "Compacting context — this calls the model to summarize history...",
	}
}

func (c *CompactCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cm := app.GetContextManager()
	if cm == nil {
		return "Context manager not available.", nil
	}

	// Capture token usage before compaction
	usageBefore := cm.GetTokenUsage()
	tokensBefore := 0
	if usageBefore != nil {
		tokensBefore = usageBefore.InputTokens
	}

	err := cm.ForceSummarize(ctx)
	app.RefreshTokenCount()
	usageAfter := cm.GetTokenUsage()
	tokensAfter := 0
	pctAfter := 0.0
	if usageAfter != nil {
		tokensAfter = usageAfter.InputTokens
		pctAfter = usageAfter.PercentUsed
	}
	return formatCompactionResult(err, tokensBefore, tokensAfter, pctAfter), nil
}

// formatCompactionResult renders the user-facing string for /compact, with
// distinct messages for each sentinel error and an honest accounting of
// the no-op cases that earlier versions reported as silent success.
func formatCompactionResult(err error, tokensBefore, tokensAfter int, pctAfter float64) string {
	if err != nil {
		switch {
		case errors.Is(err, appcontext.ErrSummarizerUnavailable):
			return "No-op: summarizer is not configured for this provider. /compact has no effect — context will only shrink via /clear."
		case errors.Is(err, appcontext.ErrHistoryTooShort):
			return "No-op: conversation is too short to compact. Add more messages and try again, or use /clear to start fresh."
		case errors.Is(err, appcontext.ErrNothingToSummarize):
			return "No-op: every message is pinned or already summarized — nothing left to compact."
		case errors.Is(err, appcontext.ErrSummarizationInProgress):
			return "No-op: context summarization is already running. Wait for it to finish, then retry /compact if needed."
		default:
			return fmt.Sprintf("Compaction failed: %v", err)
		}
	}

	if tokensBefore <= 0 {
		// Token counter wasn't populated yet (fresh session). The summarizer
		// ran without error, but we have nothing to compare against.
		return "Context compacted successfully."
	}

	saved := tokensBefore - tokensAfter
	if saved <= 0 {
		// Summarizer ran but produced a result no smaller than the
		// original — rare (model emitted a verbose summary), but worth
		// reporting honestly instead of claiming "compacted".
		return fmt.Sprintf("Compaction ran but freed no tokens (was %dk, now %dk). Try /clear if context still feels heavy.",
			tokensBefore/1000, tokensAfter/1000)
	}

	pct := int(pctAfter * 100)
	return fmt.Sprintf("Context compacted: %dk → %dk tokens (freed %dk, now %d%% full)",
		tokensBefore/1000, tokensAfter/1000, saved/1000, pct)
}

// SaveCommand saves the current session.
type SaveCommand struct{}

const maxSessionIDRunes = chat.MaxSessionIDRunes

// validSessionID keeps session names safe to show as copyable command arguments
// and prevents a custom /save or /resume value from escaping the sessions
// directory. Generated IDs already satisfy this contract; this primarily
// protects user-provided checkpoint names and corrupt hand-edited metadata.
func validSessionID(id string) bool {
	return chat.ValidateSessionID(id) == nil
}

func parseOptionalTargetAndFlag(args []string, flag, usage string) (string, bool, string) {
	target := ""
	flagSet := false
	for _, arg := range args {
		switch {
		case arg == flag:
			if flagSet {
				return "", false, fmt.Sprintf("Duplicate option %s. Usage: %s", flag, usage)
			}
			flagSet = true
		case strings.HasPrefix(arg, "-"):
			return "", false, fmt.Sprintf("Unknown option %q. Usage: %s", singleLineDisplayText(arg, 80), usage)
		case target != "":
			return "", false, fmt.Sprintf("Unexpected argument %q; only one target is accepted. Usage: %s", singleLineDisplayText(arg, 80), usage)
		default:
			target = arg
		}
	}
	return target, flagSet, ""
}

func parseOnlyFlag(args []string, flag, usage string) (bool, string) {
	flagSet := false
	for _, arg := range args {
		if arg != flag {
			return false, fmt.Sprintf("Unexpected argument %q. Usage: %s", singleLineDisplayText(arg, 80), usage)
		}
		if flagSet {
			return false, fmt.Sprintf("Duplicate option %s. Usage: %s", flag, usage)
		}
		flagSet = true
	}
	return flagSet, ""
}

// singleLineDisplayText removes terminal control sequences and folds all
// whitespace so persisted summaries and paths cannot forge extra output rows.
func singleLineDisplayText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if maxRunes > 0 {
		runes := []rune(value)
		if len(runes) > maxRunes {
			if maxRunes <= 3 {
				return string(runes[:maxRunes])
			}
			value = string(runes[:maxRunes-3]) + "..."
		}
	}
	return value
}

func formatRecentSessions(sessions []chat.SessionInfo, workDir string) (string, int) {
	var sb strings.Builder
	sb.WriteString("Recent sessions (use /resume <id>):\n\n")

	shown := 0
	firstShownID := ""
	for _, info := range sessions {
		if workDir != "" && info.WorkDir != "" &&
			filepath.Clean(info.WorkDir) != filepath.Clean(workDir) {
			continue
		}
		if !validSessionID(info.ID) {
			continue
		}

		summary := singleLineDisplayText(info.Summary, 60)
		if summary == "" {
			summary = "(no summary)"
		}
		age := formatTimeAgo(info.LastActive)
		fmt.Fprintf(&sb, "  %s%s%s  %d msgs, %s — %s\n",
			colorGreen, info.ID, colorReset, info.MessageCount, age, summary)
		if firstShownID == "" {
			firstShownID = info.ID
		}
		shown++
		if shown >= 5 {
			break
		}
	}

	if shown > 0 {
		fmt.Fprintf(&sb, "\nExample: /resume %s", firstShownID)
	}
	return sb.String(), shown
}

func (c *SaveCommand) Name() string        { return "save" }
func (c *SaveCommand) Description() string { return "Save current session" }
func (c *SaveCommand) Usage() string       { return "/save [name] [--force]" }
func (c *SaveCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "save",
		Priority: 30,
		HasArgs:  true,
		ArgHint:  "[name] [--force]",
	}
}

func (c *SaveCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	customName, force, argErr := parseOptionalTargetAndFlag(args, "--force", c.Usage())
	if argErr != "" {
		return argErr, nil
	}
	if force && customName == "" {
		return "Option --force requires a session name: /save <name> --force. Nothing was saved.", nil
	}

	hm, err := app.GetHistoryManager()
	if err != nil {
		return fmt.Sprintf("Failed to get history manager: %v", err), nil
	}

	session := app.GetSession()
	if session == nil {
		return "No active session.", nil
	}

	originalID := session.GetID()
	if customName == "" || customName == originalID {
		if err := hm.SaveFull(session); err != nil {
			return fmt.Sprintf("Failed to save session: %v", err), nil
		}
		savedID := originalID
		return fmt.Sprintf("Session saved as: %s (%d messages)\nTo restore: /resume %s",
			savedID, len(session.GetHistory()), savedID), nil
	}

	if !validSessionID(customName) {
		return "Invalid session name. Use 1–120 characters (max 240 bytes), no whitespace/control or <>:\"/\\|?*; do not start with '-' or use a reserved device name.", nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("Save cancelled: %v", err), nil
	}

	// A named save creates a snapshot under a different persisted identity. It
	// must own that target's writer lease for the existence check and write;
	// otherwise /save name --force can overwrite a session currently active in
	// another process. Never temporarily mutate the live Session ID: autosave
	// may run concurrently and would write the wrong file.
	targetLease, err := chat.AcquireSessionWriterLease(customName)
	if err != nil {
		if errors.Is(err, chat.ErrSessionWriterLeaseBusy) {
			return fmt.Sprintf("Session '%s' is open in another Gokin process. Nothing was overwritten.", customName), nil
		}
		return fmt.Sprintf("Failed to protect target session '%s': %v", customName, err), nil
	}
	defer targetLease.Release()

	if !force {
		if _, loadErr := hm.LoadFull(customName); loadErr == nil {
			return fmt.Sprintf("Session '%s' already exists.\nUse /save %s --force to overwrite, or pick a different name.",
				customName, customName), nil
		} else if !os.IsNotExist(loadErr) {
			return fmt.Sprintf("Cannot safely inspect existing session '%s': %v. Nothing was overwritten.", customName, loadErr), nil
		}
	}

	state := session.GetState()
	state.ID = customName
	// A named save is a clone, not a writer-ownership transfer. Copying a
	// scheduled retry into both identities would let the same interrupted
	// mutation resume twice; claimed entries would also carry a mismatched
	// SessionID. Durable recoveries remain exclusively with the active source.
	state.PendingRecoveries = nil
	snapshot := chat.NewSession()
	if err := snapshot.RestoreFromState(state); err != nil {
		return fmt.Sprintf("Failed to prepare session snapshot: %v", err), nil
	}
	if err := hm.SaveFull(snapshot); err != nil {
		return fmt.Sprintf("Failed to save session: %v", err), nil
	}

	msgCount := len(session.GetHistory())
	return fmt.Sprintf("Session saved as: %s (%d messages)\nTo restore: /resume %s", customName, msgCount, customName), nil
}

// ResumeCommand resumes a saved session.
type ResumeCommand struct{}

// sessionSwitcher is intentionally narrower than AppInterface. Runtimes that
// support /resume must provide an atomic writer-lease handoff; falling back to
// mutating Session directly would allow two processes to write one identity.
type sessionSwitcher interface {
	SwitchSession(context.Context, *chat.SessionState, bool) (*chat.SessionState, error)
}

func (c *ResumeCommand) Name() string        { return "resume" }
func (c *ResumeCommand) Description() string { return "Resume a saved session" }
func (c *ResumeCommand) Usage() string       { return "/resume <session_id> [--force]" }
func (c *ResumeCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "resume",
		Priority: 40,
		HasArgs:  true,
		ArgHint:  "[id] [--force]",
	}
}

func (c *ResumeCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	sessionID := ""
	force := false
	if len(args) > 0 {
		var argErr string
		sessionID, force, argErr = parseOptionalTargetAndFlag(args, "--force", c.Usage())
		if argErr != "" {
			return argErr, nil
		}
		if sessionID == "" {
			return "Missing session ID. Use /resume (no args) to list sessions, or /resume <session_id> [--force].", nil
		}
	}

	hm, err := app.GetHistoryManager()
	if err != nil {
		return fmt.Sprintf("Failed to get history manager: %v", err), nil
	}

	// No args: show recent sessions for this project to pick from
	if len(args) == 0 {
		sessions, err := hm.ListSessions()
		if err != nil || len(sessions) == 0 {
			return "No saved sessions. Use /save to save the current session first.", nil
		}

		workDir := app.GetWorkDir()
		output, shown := formatRecentSessions(sessions, workDir)
		if shown == 0 {
			return "No sessions for current project. Use /sessions --all to see all.", nil
		}
		return output, nil
	}

	if !validSessionID(sessionID) {
		return "Invalid session ID. Run /resume (no args) and copy an ID from the list.", nil
	}

	state, err := hm.LoadFull(sessionID)
	if err != nil {
		// Distinguish "not found" from real load failures (corrupt JSON,
		// schema drift, IO error). Without this, the user sees the raw
		// fs error path "open /Users/.../sessions/abc.json: no such file
		// or directory" — confusing because it leaks the storage layout.
		if os.IsNotExist(err) {
			return fmt.Sprintf("Session '%s' not found. Run /resume (no args) to list available sessions, or /sessions for the full list.", sessionID), nil
		}
		return fmt.Sprintf("Failed to load session '%s': %v", sessionID, err), nil
	}

	// Warn if session is from a different project
	currentDir := app.GetWorkDir()
	if !force && state.WorkDir != "" && currentDir != "" &&
		filepath.Clean(state.WorkDir) != filepath.Clean(currentDir) {
		return fmt.Sprintf("Session '%s' was created in %s (current: %s).\nUse /resume %s --force to load anyway.",
			sessionID, singleLineDisplayText(state.WorkDir, 120), singleLineDisplayText(currentDir, 120), sessionID), nil
	}

	// Cross-provider guard, mirroring the automatic startup auto-resume path
	// (app.go Run()): a session tagged with a different provider than the
	// current active one will silently drop assistant turns on the next
	// request (unsigned thinking-signature replay across providers — see
	// CLAUDE.md's Cross-provider history rules), or worse, 400. The auto-
	// resume path refuses this; /resume itself never checked it — this
	// closes that gap. Empty state.Provider = legacy session predating the
	// provider tag; treat as compatible. --force also bypasses this (same
	// escape hatch as the workdir-mismatch check above).
	currentProvider := runtimeProviderForConfig(app.GetConfig())
	if !force && state.Provider != "" && currentProvider != "" && state.Provider != currentProvider {
		return fmt.Sprintf("Session '%s' was created on provider %s (current: %s) — history formats are incompatible and would silently drop turns.\nUse /provider %s first, or /resume %s --force to load anyway.",
			sessionID, state.Provider, currentProvider, state.Provider, sessionID), nil
	}

	switcher, ok := app.(sessionSwitcher)
	if !ok {
		return "Session switching is unavailable in this runtime; the current session was left unchanged.", nil
	}
	state, err = switcher.SwitchSession(ctx, state, force)
	if err != nil {
		if errors.Is(err, chat.ErrSessionWriterLeaseBusy) {
			return fmt.Sprintf("Session '%s' is already open in another Gokin process. The current session was left unchanged.", sessionID), nil
		}
		return fmt.Sprintf("Failed to resume session '%s': %v. The current session was left unchanged.", sessionID, err), nil
	}
	if state == nil {
		return fmt.Sprintf("Failed to resume session '%s': the runtime returned no state. The current session was left unchanged.", sessionID), nil
	}

	session := app.GetSession()
	if session == nil {
		return "Session switch completed, but no active session is available.", nil
	}

	// Compare on-disk count vs what actually loaded. RestoreFromState skips
	// malformed entries (FunctionCall/FunctionResponse pair mismatches,
	// nil parts, etc.) silently — without this check, a session that
	// dropped 3 of 50 messages would still be reported as 50 loaded, and
	// the next /save would overwrite the original on disk with the
	// truncated state.
	onDisk := len(state.History)
	loaded := len(session.GetHistory())

	var msg string
	switch {
	case onDisk == 0:
		msg = fmt.Sprintf("Session '%s' restored, but the saved state had no messages.", sessionID)
	case loaded < onDisk:
		msg = fmt.Sprintf("Session '%s' restored: %d of %d messages loaded (%d skipped — malformed or pair-mismatched entries; see logs).",
			sessionID, loaded, onDisk, onDisk-loaded)
	default:
		msg = fmt.Sprintf("Session '%s' restored. %d messages loaded.", sessionID, loaded)
	}
	if state.Summary != "" {
		summary := singleLineDisplayText(state.Summary, 100)
		if summary != "" {
			msg += fmt.Sprintf("\nLast topic: %s", summary)
		}
	}
	return msg, nil
}

// SessionsCommand lists saved sessions.
type SessionsCommand struct{}

func (c *SessionsCommand) Name() string        { return "sessions" }
func (c *SessionsCommand) Description() string { return "List saved sessions" }
func (c *SessionsCommand) Usage() string       { return "/sessions [--all]" }
func (c *SessionsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "list",
		Priority: 50,
		HasArgs:  true,
		ArgHint:  "[--all]",
	}
}

func (c *SessionsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	showAll, argErr := parseOnlyFlag(args, "--all", c.Usage())
	if argErr != "" {
		return argErr, nil
	}

	hm, err := app.GetHistoryManager()
	if err != nil {
		return fmt.Sprintf("Failed to get history manager: %v", err), nil
	}

	sessions, err := hm.ListSessions()
	if err != nil {
		return fmt.Sprintf("Failed to list sessions: %v", err), nil
	}

	if len(sessions) == 0 {
		return "No saved sessions found.", nil
	}

	workDir := app.GetWorkDir()
	var sb strings.Builder
	shown := 0

	if showAll {
		sb.WriteString("All saved sessions:\n")
	} else {
		sb.WriteString("Saved sessions (current project):\n")
	}

	for _, info := range sessions {
		if !showAll && workDir != "" {
			sessionDir := filepath.Clean(info.WorkDir)
			if info.WorkDir == "" || sessionDir != filepath.Clean(workDir) {
				continue
			}
		}
		if !validSessionID(info.ID) {
			continue
		}

		summary := singleLineDisplayText(info.Summary, 80)
		if summary == "" {
			summary = "(no summary)"
		}

		dirLabel := ""
		if showAll && info.WorkDir != "" {
			dirLabel = fmt.Sprintf(" [%s]", singleLineDisplayText(filepath.Base(info.WorkDir), 60))
		}

		age := formatTimeAgo(info.LastActive)
		fmt.Fprintf(&sb, "  %s (%d messages, %s) — %s%s\n", info.ID, info.MessageCount, age, summary, dirLabel)
		shown++
	}

	if shown == 0 {
		if showAll {
			return "No saved sessions found.", nil
		}
		return "No sessions for current project.\nUse /sessions --all to see sessions from all projects.", nil
	}

	// Hint how to resume — without this, /sessions output looked like a
	// passive list. New users had to figure out from context that the
	// IDs go to /resume.
	sb.WriteString("\nResume: /resume <id>")
	if !showAll {
		sb.WriteString("\nUse /sessions --all to see sessions from all projects.")
	}

	return sb.String(), nil
}

// InitCommand initializes GOKIN.md for the project.
type InitCommand struct{}

func (c *InitCommand) Name() string        { return "init" }
func (c *InitCommand) Description() string { return "Initialize GOKIN.md for this project" }
func (c *InitCommand) Usage() string       { return "/init" }
func (c *InitCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryGit,
		Icon:     "init",
		Priority: 0,
	}
}

func (c *InitCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	workDir := app.GetWorkDir()
	gokinPath := filepath.Join(workDir, "GOKIN.md")

	// Atomic create-or-fail via O_EXCL. The older two-step Stat-then-Write
	// had a TOCTOU gap where a concurrent process (or a follow-up /init
	// double-click) could create GOKIN.md between the existence check and
	// the write, causing silent overwrite of the just-created file.
	template := c.detectTemplate(workDir)
	f, err := os.OpenFile(gokinPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if os.IsExist(err) {
			// Match v0.80.5 /compact pattern: "No-op:" prefix so visually
			// (and for users who skim) this is distinct from the
			// "Created GOKIN.md..." success message below. Same toast
			// styling but different first word — enough signal that
			// nothing was written.
			return "No-op: GOKIN.md already exists. Edit it manually or delete to reinitialize.", nil
		}
		return fmt.Sprintf("Failed to create GOKIN.md: %v", err), nil
	}
	if _, werr := f.WriteString(template); werr != nil {
		_ = f.Close()
		_ = os.Remove(gokinPath)
		return fmt.Sprintf("Failed to write GOKIN.md: %v", werr), nil
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Sprintf("Failed to close GOKIN.md: %v", cerr), nil
	}

	return "Created GOKIN.md with project-specific template. Edit it to refine instructions.", nil
}

func (c *InitCommand) detectTemplate(workDir string) string {
	// Detect Go
	if _, err := os.Stat(filepath.Join(workDir, "go.mod")); err == nil {
		return `# Project Instructions

## Build & Test
` + "```" + `bash
go build ./...
go vet ./...
go test -race ./...
` + "```" + `

## Architecture
<!-- Key packages and their responsibilities -->

## Coding Guidelines
- Follow standard Go conventions (gofmt, go vet)
- Handle errors explicitly; don't use panic in library code
- Write table-driven tests
`
	}

	// Detect Node.js
	if _, err := os.Stat(filepath.Join(workDir, "package.json")); err == nil {
		return `# Project Instructions

## Build & Test
` + "```" + `bash
npm install
npm test
npm run build
` + "```" + `

## Architecture
<!-- Key directories and their purpose -->

## Coding Guidelines
- Use TypeScript where possible
- Run linter before committing
`
	}

	// Detect Python
	for _, f := range []string{"pyproject.toml", "setup.py", "requirements.txt"} {
		if _, err := os.Stat(filepath.Join(workDir, f)); err == nil {
			return `# Project Instructions

## Build & Test
` + "```" + `bash
pip install -e .
pytest
` + "```" + `

## Architecture
<!-- Key modules and their purpose -->

## Coding Guidelines
- Follow PEP 8
- Use type hints
- Write docstrings for public functions
`
		}
	}

	// Detect Rust
	if _, err := os.Stat(filepath.Join(workDir, "Cargo.toml")); err == nil {
		return `# Project Instructions

## Build & Test
` + "```" + `bash
cargo build
cargo test
cargo clippy
` + "```" + `

## Architecture
<!-- Key crates and modules -->

## Coding Guidelines
- Run clippy and fix all warnings
- Use Result<T, E> for error handling
`
	}

	// Generic fallback
	return `# Project Instructions

## Project Overview
<!-- Describe your project -->

## Build & Test
<!-- How to build and test -->

## Architecture
<!-- Key files and their purpose -->

## Coding Guidelines
<!-- Project-specific standards -->
`
}

// DoctorCommand checks environment and configuration.
type DoctorCommand struct{}

func (c *DoctorCommand) Name() string        { return "doctor" }
func (c *DoctorCommand) Description() string { return "Check environment and configuration" }
func (c *DoctorCommand) Usage() string       { return "/doctor" }
func (c *DoctorCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthSetup,
		Icon:     "doctor",
		Priority: 0,
	}
}

func (c *DoctorCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	var hybridAvailability *repl.Availability
	cfg := app.GetConfig()
	mode := "auto"
	if cfg != nil {
		if configured := strings.ToLower(strings.TrimSpace(cfg.Engine.Mode)); configured != "" {
			mode = configured
		}
	}
	if reporter, ok := app.(RuntimeEngineModeReporter); ok {
		if running := strings.ToLower(strings.TrimSpace(reporter.GetRuntimeEngineMode())); running != "" {
			mode = running
		}
	}
	replCapabilityDisabled := false
	if reporter, ok := app.(RuntimeREPLCapabilityReporter); ok {
		replCapabilityDisabled = !reporter.RuntimeREPLCapabilityEnabled()
	}
	if (mode == "auto" || mode == "hybrid") && !replCapabilityDisabled {
		availability := repl.Detect(ctx, app.GetWorkDir())
		hybridAvailability = &availability
	}
	return RenderDoctor(DoctorOptions{
		Version:             app.GetVersion(),
		Config:              cfg,
		WorkDir:             app.GetWorkDir(),
		RuntimeEngineMode:   mode,
		RuntimeREPLDisabled: replCapabilityDisabled,
		HybridAvailability:  hybridAvailability,
	}), nil
}

// DoctorOptions contains the inputs needed by diagnostics without requiring a
// fully initialized App. The top-level `gokin doctor` command uses this path so
// configuration failures can be diagnosed before a provider client or TUI
// exists; /doctor uses the same renderer after startup.
type DoctorOptions struct {
	Version        string
	Config         *config.Config
	WorkDir        string
	ConfigPath     string
	ExecutablePath string
	CLI            bool
	// RuntimeEngineMode is populated by the in-process /doctor command. The
	// standalone CLI doctor has no running App and therefore uses Config only.
	RuntimeEngineMode   string
	RuntimeREPLDisabled bool
	HybridAvailability  *repl.Availability
}

func RenderDoctor(options DoctorOptions) string {
	var sb strings.Builder
	// Header style matches /stats and /tree-stats (lowercase muted label,
	// no banner, no emoji). The double-border `╔═╗ 🔍 ╚═╝` box was the
	// last "Slack-tier informal" header in the app — v0.82.5 stripped
	// every other header emoji and ASCII art for the same reason.
	fmt.Fprintf(&sb, "\n%sSystem Diagnostics%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "%s──────────────────%s\n", colorCyan, colorReset)

	if v := options.Version; v != "" {
		fmt.Fprintf(&sb, "  Version: %s%s%s\n", colorGreen, v, colorReset)
	}

	fmt.Fprintf(&sb, "\n%s─── Authentication ───%s\n", colorCyan, colorReset)

	cfg := options.Config
	issues := []string{}
	solutions := []string{}

	// Check API key via provider registry. Use GetActiveProvider() so
	// the fallback matches what the rest of the system sees as default
	// (was hardcoded to "gemini" — a removed provider — pre-v0.78.29).
	backend := "glm"
	if cfg != nil {
		backend = cfg.API.GetActiveProvider()
	}
	fmt.Fprintf(&sb, "  Backend: %s%s%s\n", colorGreen, backend, colorReset)

	hasKey := cfg != nil && cfg.API.HasProvider(backend)
	if !hasKey && os.Getenv("GOKIN_API_KEY") != "" {
		hasKey = true
	}

	if hasKey {
		provider := config.GetProvider(backend)
		if provider != nil && provider.KeyOptional && cfg != nil && cfg.API.GetActiveKey() == "" {
			fmt.Fprintf(&sb, "  Status: %s✓ Authentication not required%s\n", colorGreen, colorReset)
		} else {
			fmt.Fprintf(&sb, "  Status: %s✓ API key configured%s\n", colorGreen, colorReset)
		}
	} else {
		fmt.Fprintf(&sb, "  Status: %s✗ API key not configured%s\n", colorRed, colorReset)
		issues = append(issues, "API key not found")
		// Default fallback was "GEMINI_API_KEY" — leftover from v0.65 when
		// Gemini was removed but this hint wasn't updated. Now defaults to
		// the GLM env var, which is the project's recommended primary
		// provider (per README and v0.71.0 model recommendations).
		envHint := "GOKIN_GLM_KEY"
		if p := config.GetProvider(backend); p != nil && len(p.EnvVars) > 0 {
			envHint = p.EnvVars[0]
		}
		if options.CLI {
			solutions = append(solutions, fmt.Sprintf("Run gokin --setup or set %s", envHint))
		} else {
			solutions = append(solutions, fmt.Sprintf("Use /login <provider> <api_key> or set %s", envHint))
		}
	}

	fmt.Fprintf(&sb, "\n%s─── Runtime Limits ───%s\n", colorCyan, colorReset)
	modelRoundTimeout := effectiveModelRoundTimeout(cfg)
	if modelRoundTimeout < config.DefaultModelRoundTimeout {
		fmt.Fprintf(&sb, "  %s⚠%s Model round timeout: %s (recommended: %s or longer)\n",
			colorYellow, colorReset, modelRoundTimeout, config.DefaultModelRoundTimeout)
		issues = append(issues, fmt.Sprintf("Model round timeout is only %s", modelRoundTimeout))
		if options.CLI {
			solutions = append(solutions, fmt.Sprintf(
				"Set tools.model_round_timeout to %s or longer in the config file",
				config.DefaultModelRoundTimeout))
		} else {
			solutions = append(solutions, fmt.Sprintf(
				"Run /timeout %s (or edit tools.model_round_timeout in the config file)",
				config.DefaultModelRoundTimeout))
		}
	} else {
		fmt.Fprintf(&sb, "  %s✓%s Model round timeout: %s\n",
			colorGreen, colorReset, modelRoundTimeout)
	}
	providerTimeouts := client.EffectiveProviderTimeouts(cfg, backend)
	fmt.Fprintf(&sb, "  Provider watchdogs (%s): first headers %s · stream idle %s\n",
		backend,
		providerTimeouts.ResponseHeaderTimeout,
		formatOptionalTimeout(providerTimeouts.StreamIdleTimeout))
	normalAgentTimeout := config.DefaultAgentTimeout
	if minimum := modelRoundTimeout + config.DefaultAgentTimeoutHeadroom; minimum > normalAgentTimeout {
		normalAgentTimeout = minimum
	}
	modelWatchdog := config.ModelWatchdogTimeout(modelRoundTimeout)
	coordinateFloor := config.DefaultThoroughAgentTimeout
	if normalAgentTimeout > coordinateFloor {
		coordinateFloor = normalAgentTimeout
	}
	fmt.Fprintf(&sb, "  Orchestration: foreground idle %s · meta-agent stuck %s · normal agent %s · coordinate floor %s (DAG-aware)\n",
		modelWatchdog, modelWatchdog, normalAgentTimeout, coordinateFloor)
	fmt.Fprintf(&sb, "  Auxiliary LLM: compaction %s · session memory %s · semantic scoring %s · error reflection %s (inherit model round)\n",
		modelRoundTimeout, modelRoundTimeout, modelRoundTimeout, modelRoundTimeout)

	planningTimeout := modelRoundTimeout
	planningSource := "inherits model"
	stepTimeout := normalAgentTimeout
	stepSource := "dynamic"
	if cfg != nil {
		if cfg.Plan.PlanningTimeout > 0 {
			planningTimeout = cfg.Plan.PlanningTimeout
			planningSource = "explicit"
		}
		if cfg.Plan.DefaultStepTimeout > 0 {
			stepTimeout = cfg.Plan.DefaultStepTimeout
			stepSource = "explicit"
		}
	}
	fmt.Fprintf(&sb, "  Plan: generation %s (%s) · step %s (%s) · stuck watchdog %s\n",
		planningTimeout, planningSource, stepTimeout, stepSource, modelWatchdog)
	if planningTimeout < modelRoundTimeout {
		issues = append(issues, fmt.Sprintf(
			"Plan generation timeout %s is shorter than model round %s",
			planningTimeout, modelRoundTimeout))
		solutions = append(solutions,
			"Set plan.planning_timeout to 0s to inherit tools.model_round_timeout")
	}
	if stepTimeout < modelRoundTimeout {
		issues = append(issues, fmt.Sprintf(
			"Plan step timeout %s is shorter than model round %s",
			stepTimeout, modelRoundTimeout))
		solutions = append(solutions,
			"Set plan.default_step_timeout to 0s for the dynamic agent budget")
	}

	configuredEngineMode := "auto"
	if cfg != nil && strings.TrimSpace(cfg.Engine.Mode) != "" {
		configuredEngineMode = strings.ToLower(strings.TrimSpace(cfg.Engine.Mode))
	}
	engineMode := strings.ToLower(strings.TrimSpace(options.RuntimeEngineMode))
	if engineMode == "" {
		engineMode = configuredEngineMode
	}
	if engineMode == configuredEngineMode {
		fmt.Fprintf(&sb, "  Engine mode: %s\n", engineMode)
	} else {
		fmt.Fprintf(&sb, "  Engine mode: %s (running) · %s configured for next launch; restart required\n",
			engineMode, configuredEngineMode)
	}
	if engineMode == "tools" {
		fmt.Fprintf(&sb, "  %s○%s Stateful hybrid disabled; using structured tools\n", colorYellow, colorReset)
	} else if options.RuntimeREPLDisabled {
		fmt.Fprintf(&sb, "  %s○%s Stateful REPL disabled by invocation policy; secure runtime not started\n", colorYellow, colorReset)
	} else if availability := options.HybridAvailability; availability != nil {
		if availability.Available {
			fmt.Fprintf(&sb, "  %s✓%s Stateful hybrid available (%s)\n", colorGreen, colorReset, availability.Backend)
		} else if engineMode == "hybrid" {
			fmt.Fprintf(&sb, "  %s✗%s Required secure REPL unavailable: %s\n", colorRed, colorReset, availability.Reason)
			issues = append(issues, "Required secure hybrid runtime is unavailable")
			solutions = append(solutions, "Install/enable python3 and the platform sandbox, or set engine.mode to auto/tools")
		} else {
			fmt.Fprintf(&sb, "  %s○%s Auto fallback to structured tools: %s\n", colorYellow, colorReset, availability.Reason)
		}
	}

	fmt.Fprintf(&sb, "\n%s─── Environment ───%s\n", colorCyan, colorReset)

	// Config file
	configPath := options.ConfigPath
	if configPath == "" {
		configPath = config.GetConfigPath()
	}
	if _, err := os.Stat(configPath); err == nil {
		fmt.Fprintf(&sb, "  %s✓%s Config: %s\n", colorGreen, colorReset, prettyHomePath(configPath))
	} else {
		fmt.Fprintf(&sb, "  %s○%s Config not found (using defaults)\n", colorYellow, colorReset)
	}

	// Git
	if _, err := exec.LookPath("git"); err == nil {
		fmt.Fprintf(&sb, "  %s✓%s git installed\n", colorGreen, colorReset)
	} else {
		fmt.Fprintf(&sb, "  %s✗%s git not installed\n", colorRed, colorReset)
		issues = append(issues, "Git not installed")
		solutions = append(solutions, "Install git: apt install git / brew install git")
	}

	// GitHub CLI
	if _, err := exec.LookPath("gh"); err == nil {
		fmt.Fprintf(&sb, "  %s✓%s gh (GitHub CLI) installed\n", colorGreen, colorReset)
	} else {
		fmt.Fprintf(&sb, "  %s○%s gh (GitHub CLI) not installed (optional for /pr)\n", colorYellow, colorReset)
	}

	// Git repo check
	workDir := options.WorkDir
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		fmt.Fprintf(&sb, "  %s✓%s Working directory is a git repository\n", colorGreen, colorReset)
	} else {
		fmt.Fprintf(&sb, "  %s○%s Not a git repository (git tools will be limited)\n", colorYellow, colorReset)
	}

	// Developer checkout delivery check. A fixed worktree is not enough when
	// the shell still resolves an older installed executable—the exact state in
	// which timeout fixes appeared green in tests but users kept running the old
	// behavior. Only recognized Gokin source roots participate.
	if checkout, ok := findDoctorCheckout(workDir); ok && strings.TrimSpace(options.Version) != "" {
		sourceVersion := checkout.Version
		executablePath := strings.TrimSpace(options.ExecutablePath)
		if executablePath == "" {
			executablePath, _ = os.Executable()
		}
		if comparison, comparable := compareDoctorVersionCore(options.Version, sourceVersion); comparable {
			switch {
			case comparison < 0:
				fmt.Fprintf(&sb, "  %s⚠%s Active binary %s is older than checkout %s\n",
					colorYellow, colorReset, options.Version, sourceVersion)
				issues = append(issues, fmt.Sprintf(
					"Active binary %s is older than Gokin checkout %s", options.Version, sourceVersion))
				solution := "Rebuild and reinstall Gokin from this checkout"
				if executablePath != "" {
					solution = fmt.Sprintf("Rebuild this checkout and replace %s", prettyHomePath(executablePath))
				}
				solutions = append(solutions, solution)
			case comparison == 0:
				if newerInput, stale := checkoutBuildInputNewerThan(checkout.Root, executablePath); stale {
					fmt.Fprintf(&sb, "  %s⚠%s Active binary %s predates checkout change %s\n",
						colorYellow, colorReset, options.Version, newerInput)
					issues = append(issues, fmt.Sprintf(
						"Active binary %s predates same-version checkout changes", options.Version))
					solution := "Rebuild and reinstall Gokin from this checkout"
					if executablePath != "" {
						solution = fmt.Sprintf("Rebuild this checkout and replace %s", prettyHomePath(executablePath))
					}
					solutions = append(solutions, solution)
				} else {
					fmt.Fprintf(&sb, "  %s✓%s Active binary matches checkout version %s\n",
						colorGreen, colorReset, sourceVersion)
				}
			default:
				fmt.Fprintf(&sb, "  %s✓%s Active binary %s is newer than checkout %s\n",
					colorGreen, colorReset, options.Version, sourceVersion)
			}
		} else {
			fmt.Fprintf(&sb, "  %s○%s Checkout version %s (runtime label %s is not comparable)\n",
				colorYellow, colorReset, sourceVersion, options.Version)
		}
	}

	// Project instruction file (GOKIN.md/CLAUDE.md and other supported paths)
	foundInstruction := ""
	for _, filename := range appcontext.InstructionFileNames() {
		path := filepath.Join(workDir, filename)
		if _, err := os.Stat(path); err == nil {
			foundInstruction = filename
			break
		}
	}
	if foundInstruction != "" {
		fmt.Fprintf(&sb, "  %s✓%s Instruction file found: %s\n", colorGreen, colorReset, foundInstruction)
	} else {
		fmt.Fprintf(&sb, "  %s○%s No project instruction file found (GOKIN.md/CLAUDE.md)\n", colorYellow, colorReset)
	}

	// Data directories
	dataDir, _ := getDataDir()
	fmt.Fprintf(&sb, "\n%s─── Directories ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Data: %s\n", prettyHomePath(dataDir))

	// Summary
	fmt.Fprintf(&sb, "\n%s─── Summary ───%s\n", colorCyan, colorReset)

	if len(issues) == 0 {
		fmt.Fprintf(&sb, "  %s✓ All systems working properly!%s\n", colorGreen, colorReset)
	} else {
		fmt.Fprintf(&sb, "  %s⚠ Issues detected:%s\n", colorYellow, colorReset)
		for i, issue := range issues {
			fmt.Fprintf(&sb, "    %d. %s\n", i+1, issue)
		}

		fmt.Fprintf(&sb, "\n%sSolutions:%s\n", colorGreen, colorReset)
		for i, solution := range solutions {
			fmt.Fprintf(&sb, "    %d. %s\n", i+1, solution)
		}
	}

	// Surface the fix-issues palette only when issues exist. Pre-v0.84.7
	// it always rendered, including on a clean-bill-of-health success
	// path — reading as "we just told you everything's fine, here are
	// commands to fix it anyway". Also dropped the stale `/test` ref
	// (no such command in the registry); replaced with `/status` which
	// is the actual command for "show me my settings".
	if len(issues) > 0 {
		fmt.Fprintf(&sb, "\n%sCommands to fix issues:%s\n", colorCyan, colorReset)
		if options.CLI {
			fmt.Fprintf(&sb, "  %sgokin --setup%s      Set up authentication\n", colorGreen, colorReset)
			fmt.Fprintf(&sb, "  %sgokin%s              Open Gokin, then use /status or /init\n", colorGreen, colorReset)
		} else {
			fmt.Fprintf(&sb, "  %s/login%s    Set up authentication\n", colorGreen, colorReset)
			fmt.Fprintf(&sb, "  %s/status%s   Show current configuration\n", colorGreen, colorReset)
			fmt.Fprintf(&sb, "  %s/init%s     Create GOKIN.md template\n", colorGreen, colorReset)
		}
	}

	return sb.String()
}

// prettyHomePath collapses the user's $HOME prefix in a path to "~".
// Mirror of the same idea in internal/ui/tui_status_bar.go's prettyPath,
// kept local to commands package to avoid a UI dependency. Returns the
// input unchanged when $HOME isn't set or the path doesn't start with it.
func prettyHomePath(p string) string {
	if p == "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// ShortcutsCommand displays keyboard shortcuts.
type ShortcutsCommand struct{}

func (c *ShortcutsCommand) Name() string        { return "shortcuts" }
func (c *ShortcutsCommand) Description() string { return "Show keyboard shortcuts" }
func (c *ShortcutsCommand) Usage() string       { return "/shortcuts" }
func (c *ShortcutsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryGettingStarted,
		Icon:     "shortcuts",
	}
}

func (c *ShortcutsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n%sKeyboard Shortcuts%s  (also: press ? in empty input for the filterable overlay)\n\n", colorCyan, colorReset)

	// Must stay in sync with internal/ui/shortcuts.go (DefaultShortcuts)
	// and the binding handlers in internal/ui/tui.go. The shortcuts
	// overlay (`?`) is authoritative; this is a flat text fallback for
	// users who prefer slash-command output.
	shortcuts := []struct {
		keys, desc string
	}{
		{"Enter", "Send message"},
		{"Ctrl+J / Alt+Enter", "Insert newline"},
		{"Tab", "Autocomplete command"},
		{"Ctrl+P", "Command palette"},
		{"Ctrl+S", "Open settings"},
		{"Ctrl+K", "Open model selector"},
		{"Ctrl+E / e", "Toggle last output (expand appends; compact keeps existing scrollback)"},
		{"E", "Set expanded/compact default for new tool outputs"},
		{"Ctrl+R", "Search input history"},
		{"Ctrl+C", "Cancel once, quit on second press"},
		{"Ctrl+L", "Clear screen"},
		{"Ctrl+B / Ctrl+F", "Scroll up / down"},
		{"Ctrl+U / Ctrl+D", "Scroll half page (empty input)"},
		{"Ctrl+G", "Toggle select mode (freeze + native selection)"},
		{"Ctrl+H", "Context Observatory (technical health)"},
		{"Ctrl+T", "Toggle task list"},
		{"Ctrl+O", "Toggle live activity detail"},
		{"Shift+Tab", "Cycle mode: Normal → Plan → YOLO → Normal"},
		{"Alt+C", "Copy last response"},
		{"?", "Filterable shortcuts overlay (empty input)"},
	}

	for _, s := range shortcuts {
		fmt.Fprintf(&sb, "  %s%-22s%s %s\n", colorGreen, s.keys, colorReset, s.desc)
	}

	return sb.String(), nil
}

// PwdCommand shows the current working directory.
type PwdCommand struct{}

func (c *PwdCommand) Name() string        { return "pwd" }
func (c *PwdCommand) Description() string { return "Show current working directory" }
func (c *PwdCommand) Usage() string       { return "/pwd" }
func (c *PwdCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryTools,
		Icon:     "folder",
	}
}

func (c *PwdCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	return app.GetWorkDir(), nil
}

// KeysCommand is an alias for ShortcutsCommand.
type KeysCommand struct{ ShortcutsCommand }

func (c *KeysCommand) Name() string        { return "keys" }
func (c *KeysCommand) Description() string { return "Show keyboard shortcuts (alias for /shortcuts)" }
func (c *KeysCommand) Usage() string       { return "/keys" }

// ConfigCommand shows current configuration.
type ConfigCommand struct{}

func (c *ConfigCommand) Name() string        { return "config" }
func (c *ConfigCommand) Description() string { return "Show current configuration" }
func (c *ConfigCommand) Usage() string       { return "/config" }
func (c *ConfigCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryAuthSetup,
		Icon:     "config",
		Priority: 10,
	}
}

func (c *ConfigCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Configuration not available.", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%sCurrent Configuration%s\n\n", colorCyan, colorReset)

	// API
	fmt.Fprintf(&sb, "%s─── API ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Provider: %s%s%s\n", colorGreen, cfg.API.GetActiveProvider(), colorReset)

	// Model
	fmt.Fprintf(&sb, "\n%s─── Model ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Name:        %s%s%s\n", colorGreen, cfg.Model.Name, colorReset)
	fmt.Fprintf(&sb, "  Temperature: %.1f\n", cfg.Model.Temperature)
	fmt.Fprintf(&sb, "  Max Output:  %d tokens\n", cfg.Model.MaxOutputTokens)

	// Runtime limits. A persisted short round cap otherwise looks like a
	// provider failure when it fires, so surface the effective value here.
	modelRoundTimeout := effectiveModelRoundTimeout(cfg)
	fmt.Fprintf(&sb, "\n%s─── Runtime Limits ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Model Round: %s\n", modelRoundTimeout)
	provider := cfg.API.GetActiveProvider()
	providerTimeouts := client.EffectiveProviderTimeouts(cfg, provider)
	fmt.Fprintf(&sb, "  Provider:    %s headers / %s stream idle (%s)\n",
		providerTimeouts.ResponseHeaderTimeout,
		formatOptionalTimeout(providerTimeouts.StreamIdleTimeout),
		provider)
	fmt.Fprintf(&sb, "  Tool:        %s\n", cfg.Tools.Timeout)
	fmt.Fprintf(&sb, "  Change:      /timeout <duration>\n")

	// UI
	fmt.Fprintf(&sb, "\n%s─── UI ───%s\n", colorCyan, colorReset)
	activeTheme := activeThemeID(app)
	fmt.Fprintf(&sb, "  Theme:  %s — %s\n", activeTheme, themeDescription(activeTheme))
	if configured := configuredThemeValue(app); configured != "" && configured != string(activeTheme) {
		fmt.Fprintf(&sb, "  Configured ui.theme %q is legacy/unsupported and is not applied\n", configured)
	}
	fmt.Fprintf(&sb, "  Tokens: %v  Markdown: %v  Bell: %v\n",
		cfg.UI.ShowTokenUsage, cfg.UI.MarkdownRendering, cfg.UI.Bell)

	// Context
	fmt.Fprintf(&sb, "\n%s─── Context ───%s\n", colorCyan, colorReset)
	maxInput := cfg.Context.MaxInputTokens
	if maxInput == 0 {
		sb.WriteString("  Max Input: (model default)\n")
	} else {
		fmt.Fprintf(&sb, "  Max Input: %d tokens\n", maxInput)
	}
	fmt.Fprintf(&sb, "  Auto-Compact: %v\n", cfg.Context.EnableAutoSummary)

	// Memory. Global access is called out separately because it is an explicit
	// cross-project privacy boundary, disabled by default.
	fmt.Fprintf(&sb, "\n%s─── Memory ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Enabled: %v  Auto-Inject: %v  Global (cross-project): %v\n",
		cfg.Memory.Enabled, cfg.Memory.AutoInject, cfg.Memory.AllowGlobal)
	fmt.Fprintf(&sb, "  Toggle global access: /set globalmemory on|off\n")

	// Plan
	fmt.Fprintf(&sb, "\n%s─── Plan ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Delegate: %v  Clear Context: %v\n",
		cfg.Plan.DelegateSteps, cfg.Plan.ClearContext)

	// Permissions
	fmt.Fprintf(&sb, "\n%s─── Permissions ───%s\n", colorCyan, colorReset)
	fmt.Fprintf(&sb, "  Enabled: %v  Policy: %s\n",
		cfg.Permission.Enabled, cfg.Permission.DefaultPolicy)

	// Config path
	configPath := config.GetConfigPath()
	fmt.Fprintf(&sb, "\n%sConfig file:%s %s\n", colorCyan, colorReset, configPath)
	fmt.Fprintf(&sb, "%sChange a setting:%s /set (lists toggles) · /set <key> on|off\n", colorCyan, colorReset)

	return sb.String(), nil
}

func formatOptionalTimeout(timeout time.Duration) string {
	if timeout <= 0 {
		return "disabled"
	}
	return timeout.String()
}

// PermissionsCommand toggles permission prompts.
type PermissionsCommand struct{}

func (c *PermissionsCommand) Name() string { return "permissions" }
func (c *PermissionsCommand) Description() string {
	return "View or configure risky-action permission prompts"
}
func (c *PermissionsCommand) Usage() string {
	return `/permissions      - Show status
/permissions on   - Enable prompts
/permissions off  - YOLO mode`
}
func (c *PermissionsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryTools,
		Icon:     "shield",
		Priority: 20,
		HasArgs:  true,
		ArgHint:  "on|off",
	}
}

func (c *PermissionsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Config not available", nil
	}

	// No args - show current status
	if len(args) == 0 {
		if cfg.Permission.Enabled {
			return "permissions: on — risky actions ask first; sandbox setting is separate", nil
		}
		return "permissions: off (YOLO) — risky actions auto-approve; sandbox setting is unchanged", nil
	}

	// Toggle based on argument
	switch strings.ToLower(args[0]) {
	case "on", "true", "1", "enable":
		cfg.Permission.Enabled = true
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "permissions: on — risky actions ask first; sandbox setting is unchanged", nil

	case "off", "false", "0", "disable":
		cfg.Permission.Enabled = false
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "permissions: off (YOLO) — risky actions auto-approve; sandbox setting is unchanged", nil

	default:
		return "/permissions on | off", nil
	}
}

// SandboxCommand toggles bash sandbox mode.
type SandboxCommand struct{}

func (c *SandboxCommand) Name() string { return "sandbox" }
func (c *SandboxCommand) Description() string {
	return "View or configure bash sandbox containment"
}
func (c *SandboxCommand) Usage() string {
	return `/sandbox      - Show status
/sandbox on   - Safe mode
/sandbox off  - Unrestricted`
}
func (c *SandboxCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryTools,
		Icon:     "sandbox",
		Priority: 30,
		HasArgs:  true,
		ArgHint:  "on|off",
		Advanced: true,
	}
}

func (c *SandboxCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Config not available", nil
	}

	// No args - show current status
	if len(args) == 0 {
		if cfg.Tools.Bash.Sandbox {
			return "sandbox: on — bash is contained; permission prompts are separate", nil
		}
		return "sandbox: off (!SANDBOX) — approved or auto-approved bash is unrestricted; permission prompts are unchanged", nil
	}

	// Toggle based on argument
	switch strings.ToLower(args[0]) {
	case "on", "true", "1", "enable":
		cfg.Tools.Bash.Sandbox = true
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "sandbox: on — bash is contained; permission prompts are unchanged", nil

	case "off", "false", "0", "disable":
		cfg.Tools.Bash.Sandbox = false
		if err := app.ApplyConfig(cfg); err != nil {
			return fmt.Sprintf("Failed: %v", err), nil
		}
		return "sandbox: off (!SANDBOX) — approved or auto-approved bash is unrestricted; permission prompts are unchanged", nil

	default:
		return "/sandbox on | off", nil
	}
}

// ClearTodosCommand clears all todo items.
type ClearTodosCommand struct{}

func (c *ClearTodosCommand) Name() string        { return "clear-todos" }
func (c *ClearTodosCommand) Description() string { return "Clear all todo items" }
func (c *ClearTodosCommand) Usage() string       { return "/clear-todos [--force]" }
func (c *ClearTodosCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryTools,
		Icon:     "clear",
		Priority: 10,
		HasArgs:  true,
		ArgHint:  "[--force]",
	}
}

func (c *ClearTodosCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	todoTool := app.GetTodoTool()
	if todoTool == nil {
		return "Todo tool not available.", nil
	}

	// Show count and require --force when there are non-trivial number of
	// items. Same protection family as v0.80.22 /save and v0.80.23 /logout
	// all — destructive ops on user state get an explicit confirmation.
	// Empty list / 0 items: just clear (no-op message).
	items := todoTool.GetItems()
	force := false
	for _, a := range args {
		if a == "--force" {
			force = true
		}
	}

	if len(items) == 0 {
		return "Todo list is already empty.", nil
	}

	if !force {
		// Count by status so the user sees what work would disappear
		// (in-progress + pending = unfinished work; completed = done).
		var inProgress, pending, completed int
		for _, item := range items {
			switch item.Status {
			case "in_progress":
				inProgress++
			case "completed":
				completed++
			default:
				pending++
			}
		}
		var detail strings.Builder
		fmt.Fprintf(&detail, "Todo list has %d item(s)", len(items))
		var parts []string
		if inProgress > 0 {
			parts = append(parts, fmt.Sprintf("%d in progress", inProgress))
		}
		if pending > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", pending))
		}
		if completed > 0 {
			parts = append(parts, fmt.Sprintf("%d completed", completed))
		}
		if len(parts) > 0 {
			fmt.Fprintf(&detail, " (%s)", strings.Join(parts, ", "))
		}
		detail.WriteString(".\n\nUse /clear-todos --force to confirm.")
		return detail.String(), nil
	}

	todoTool.ClearItems()
	return fmt.Sprintf("Todo list cleared (%d items removed).", len(items)), nil
}

// BrowseCommand opens an interactive file browser.
type BrowseCommand struct{}

func (c *BrowseCommand) Name() string        { return "browse" }
func (c *BrowseCommand) Description() string { return "Open interactive file browser" }
func (c *BrowseCommand) Usage() string       { return "/browse [path]" }
func (c *BrowseCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryTools,
		Icon:     "folder",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "[path]",
	}
}

func (c *BrowseCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	startPath := app.GetWorkDir()
	if len(args) > 0 {
		startPath = args[0]
		// Handle relative paths
		if !filepath.IsAbs(startPath) {
			startPath = filepath.Join(app.GetWorkDir(), startPath)
		}
	}

	// Verify path exists
	info, err := os.Stat(startPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	// If it's a file, use its directory
	if !info.IsDir() {
		startPath = filepath.Dir(startPath)
	}

	return "__browse:" + startPath, nil
}

// getDataDir returns the data directory for the application.
func getDataDir() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "gokin"), nil
}

// getCommandExample returns usage examples for a command.
func getCommandExample(name string) string {
	examples := map[string]string{
		"commit":   "  /commit              — AI generates commit message from staged changes\n  /commit fix typo     — commit with custom message",
		"model":    "  /model glm-5.2               — switch to GLM 5.2 (Z.AI Coding Plan default)\n  /model deepseek-v4-pro       — switch to DeepSeek V4 Pro\n  /model k3                    — switch to Kimi K3 (flagship, 1M ctx)\n  /model kimi-for-coding       — switch to Kimi K2.7\n  /model MiniMax-M2.7          — switch to MiniMax",
		"plan":     "  /plan                — toggle planning mode on/off\n  Then type a complex task and it will be broken into steps",
		"resume":   "  /resume abc123       — restore session abc123\n  /resume abc123 --force — restore even from different project",
		"save":     "  /save                — save current session for later /resume\n  /save mywork         — save with custom name (refuses to overwrite by default)\n  /save mywork --force — overwrite an existing 'mywork' checkpoint",
		"compact":  "  /compact             — summarize old messages to free context space",
		"clear":    "  /clear               — start fresh (saves active plan for /resume-plan)\n  /clear --force       — clear even if active-plan recovery cannot be saved",
		"theme":    "  /theme               — show the active UI theme (gokin ships one unified Graphite + violet theme)",
		"doctor":   "  /doctor              — check API key, git, config, and project setup",
		"login":    "  /login glm <key>          — set Z.AI / GLM key\n  /login deepseek <key>     — set DeepSeek key\n  /login kimi <key>         — set Kimi Coding Plan key\n  /login minimax <key>      — set MiniMax key",
		"undo":     "  /undo                — revert the last file change made by the AI\n  /undo all            — revert everything the last request changed, atomically\n  /undo 3              — revert the last three changes\n  /undo list           — preview what /undo would revert",
		"redo":     "  /redo                — re-apply the last undone change\n  /redo all            — re-apply the whole request /undo all reverted",
		"stats":    "  /stats               — show tokens, cost, cache hit rate, project info",
		"status":   "  /status              — show provider, model, API keys, workdir, version",
		"update":   "  /update              — check for new versions\n  /update install        — download and install latest\n  /update rollback       — revert to previous version",
		"provider": "  /provider glm         — switch to GLM (Z.AI)\n  /provider deepseek    — switch to DeepSeek\n  /provider kimi        — switch to Kimi\n  /provider minimax     — switch to MiniMax\n  /provider ollama      — switch to local Ollama",
		// v0.77.x git-inspect family + v0.78.12 /blame
		"diff":     "  /diff                — show working-tree diff\n  /diff --staged       — show staged-only diff\n  /diff --stat         — summary instead of patch\n  /diff main.go        — diff one file",
		"log":      "  /log                 — last 10 commits\n  /log 20              — last 20 commits\n  /log internal/app    — commits touching a path\n  /log 5 README.md     — last 5 touching README",
		"branches": "  /branches            — local branches sorted by activity\n  /branches --all      — include remote-tracking branches",
		"grep":     "  /grep TODO           — case-insensitive working-tree search\n  /grep -w foo         — whole-word match\n  /grep -C 2 panic     — 2 lines of context\n  /grep \"old func\" internal/  — scoped to a path",
		"blame":    "  /blame internal/app/app.go         — full-file authorship (capped 200 lines)\n  /blame internal/app/app.go 100-150 — line range\n  /blame README.md 1                 — single line",
		"show":     "  /show              — show HEAD (most recent commit)\n  /show abc123       — show a specific commit\n  /show HEAD~3       — three commits back\n  /show abc123 main.go — scope diff to one file",
		// v0.74–v0.76 release / upgrade feedback loop
		"whats-new": "  /whats-new           — release notes for the current version",
		"changelog": "  /changelog           — compact list of recent releases",
		"restart":   "  /restart             — re-exec into the latest installed binary (for self-update)",
		// v0.78.26 — fill out examples for the rest of the user-facing
		// complex commands. Trivial commands (/pwd /paste /ql /shortcuts
		// /sessions /logout) just have their Usage line and skip examples;
		// these benefit from concrete invocations.
		"pr":          "  /pr                          — show pending PR info\n  /pr --title \"Fix bug\"        — create PR with title\n  /pr --draft --title \"WIP\"    — create draft PR\n  /pr --base main --title \"…\"  — target a specific base",
		"mcp":         "  /mcp list             — show configured MCP servers\n  /mcp status           — connection health\n  /mcp add <name> <cmd> — register a server\n  /mcp remove <name>    — unregister\n  /mcp refresh <name>   — re-list tools",
		"memory":      "  /memory               — show all stored memories\n  Project memories live in .gokin/MEMORY.md",
		"sandbox":     "  /sandbox on           — gate bash on permission prompts\n  /sandbox off          — disable bash safety prompts (yolo)",
		"permissions": "  /permissions on       — prompt before write/edit/bash\n  /permissions off      — auto-approve (yolo mode)",
		"thinking":    "  /thinking auto        — reason only when the task is hard\n  /thinking on          — force reasoning every turn\n  /thinking off         — never reason\n  /thinking 16384       — force reasoning with a 16K-token budget",
		"timeout":     "  /timeout             — show the effective model round cap\n  /timeout 20m         — apply a 20-minute cap live\n  /timeout default     — restore the recommended default",
		"open":        "  /open main.go         — open file in $EDITOR (or vi)\n  /open internal/app/app.go",
		"resume-plan": "  /resume-plan          — restore the plan saved by the last /clear",
		"recovery":    "  /recovery             — show the recovery snapshot from the last unclean shutdown",
		"checkpoints": "  /checkpoints          — list session checkpoints (auto-saved every N messages)",
		"config":      "  /config               — print active config + which file it came from",
		"init":        "  /init                 — bootstrap GOKIN.md for this project",
		"sessions":    "  /sessions             — list saved sessions (most recent first)",
		"logout":      "  /logout               — clear API key for the current provider\n  /logout all           — preview what /logout all would wipe (dry-run)\n  /logout all --force   — actually clear all stored keys (irreversible)",
	}
	return examples[name]
}

// getRelatedCommands returns related commands for a command.
func getRelatedCommands(name string) string {
	related := map[string]string{
		"commit":      "/pr, /diff, /log, /save",
		"save":        "/resume, /sessions, /clear",
		"resume":      "/save, /sessions",
		"sessions":    "/save, /resume",
		"clear":       "/save, /compact, /resume-plan",
		"compact":     "/clear, /cost, /stats",
		"model":       "/provider, /config, /thinking",
		"plan":        "/resume-plan, /tree-stats",
		"resume-plan": "/plan",
		"login":       "/logout, /provider, /doctor",
		"doctor":      "/login, /config, /status",
		"config":      "/doctor, /model, /timeout, /theme",
		"theme":       "/config",
		"cost":        "/stats, /compact",
		"shortcuts":   "/help, /keys",
		"keys":        "/help, /shortcuts",
		"undo":        "/redo, /checkpoints",
		"redo":        "/undo",
		"stats":       "/cost, /compact, /status",
		"status":      "/doctor, /config, /stats",
		"update":      "/doctor, /status, /restart, /whats-new",
		"quickstart":  "/help, /doctor",
		"pr":          "/commit, /diff, /log",
		"permissions": "/sandbox, /config",
		"sandbox":     "/permissions",
		"provider":    "/model, /login, /status",
		"checkpoints": "/undo, /plan",
		"pwd":         "/status, /browse",
		"browse":      "/pwd, /open",
		"init":        "/doctor, /instructions",
		// v0.77.x git-inspect set: cross-link the family so users find adjacent tools
		"diff":      "/log, /branches, /grep, /blame, /commit",
		"log":       "/diff, /branches, /grep, /blame, /commit",
		"branches":  "/log, /diff, /pr",
		"grep":      "/log, /diff, /blame, /open",
		"blame":     "/log, /diff, /grep, /show, /commit",
		"show":      "/log, /diff, /blame, /commit",
		"whats-new": "/changelog, /update, /restart",
		"changelog": "/whats-new, /update",
		"restart":   "/update, /whats-new",
		// v0.78.26 — fill out see-also for the rest of user-facing commands.
		"mcp":               "/permissions, /status, /stats",
		"memory":            "/clear, /compact, /memory-governance",
		"thinking":          "/model, /provider, /timeout",
		"timeout":           "/doctor, /config, /thinking",
		"logout":            "/login, /provider, /status",
		"open":              "/grep, /blame, /diff, /browse",
		"recovery":          "/journal, /resume-plan, /clear",
		"journal":           "/recovery, /policy, /ledger",
		"policy":            "/journal, /ledger, /sandbox",
		"ledger":            "/journal, /policy, /plan-proof",
		"plan-proof":        "/journal, /ledger, /policy",
		"observability":     "/stats, /journal, /policy",
		"tree-stats":        "/plan, /stats, /resume-plan",
		"memory-governance": "/memory, /compact, /clear",
		"health":            "/stats, /policy, /observability",
	}
	return related[name]
}

// formatTimeAgo returns a human-readable relative time string.
func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

// runtimeProviderForConfig mirrors internal/app's function of the same name
// (app/provider_resolution.go) — duplicated rather than imported because
// internal/app already imports internal/commands, so the reverse import
// would cycle. Keep this in sync with the app package's version: both must
// agree on "what is the current provider" for the cross-provider resume
// guards (auto-resume in app.go, /resume here) to behave consistently.
func runtimeProviderForConfig(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if provider := normalizeProviderName(cfg.Model.Provider); provider != "" && provider != "auto" {
		return provider
	}
	if model := strings.TrimSpace(cfg.Model.Name); model != "" {
		if detected := normalizeProviderName(config.DetectKnownProviderFromModel(model)); detected != "" {
			return detected
		}
	}
	return normalizeProviderName(cfg.API.GetActiveProvider())
}

func normalizeProviderName(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
