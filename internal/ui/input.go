package ui

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gokin/internal/config"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// defaultProviderNames returns all provider names from the registry.
func defaultProviderNames() []string { return config.ProviderNames() }

// defaultKeyProviderNames returns provider names that require API keys.
func defaultKeyProviderNames() []string { return config.KeyProviderNames() }

// defaultAllProviderNames returns all provider names plus "all".
func defaultAllProviderNames() []string { return config.AllProviderNames() }

const (
	maxHistorySize             = 100
	maxComposerLines           = 6
	maxInputHistoryEntryBytes  = 1 << 20
	maxInputHistoryRecordBytes = 4*maxInputHistoryEntryBytes + 32
	maxInputHistoryFileBytes   = 16 << 20
	maxInputHistoryRecords     = 10000
	historyFile                = "input_history"
)

// ArgInfo describes a command argument for autocomplete hints.
type ArgInfo struct {
	Name     string   // Argument name (e.g., "message", "filename")
	Required bool     // True if argument is required
	Type     string   // "string", "path", "option", "number"
	Options  []string // Default options for "option" type
	// OptionsByPrevious overrides Options when the preceding argument matches
	// a key. This keeps subcommand-style chains honest (for example `/set
	// preset <safe|balanced|fast>` instead of suggesting on/off for a preset).
	OptionsByPrevious map[string][]string
}

// CommandInfo contains information about a slash command for autocomplete.
type CommandInfo struct {
	Name        string
	Description string
	Category    string
	Args        []ArgInfo // Command arguments
	Usage       string    // Usage pattern (e.g., "/copy <n> [filename]")
}

// SuggestionType represents the type of autocomplete suggestion.
type SuggestionType int

const (
	SuggestionCommand SuggestionType = iota
	SuggestionArgument
	SuggestionFile
	SuggestionAtFile // @path file reference (the buffer keeps the leading @)
)

// atFileWord returns the trailing @-reference word being typed (e.g. "@src/ma"),
// for autocomplete. It is the LAST whitespace-delimited token when that token
// starts with a single '@' and the value has no trailing space (the word is
// still being typed). ok=false otherwise. Lets "look at @main" autocomplete the
// @main token mid-sentence, not just a whole-value "@main".
func atFileWord(value string) (string, bool) {
	token, ok := trailingInputToken(value)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(token.text, "@") && strings.Count(token.text, "@") == 1 {
		return token.text, true
	}
	return "", false
}

type inputToken struct {
	start int
	text  string
	quote rune
}

// pasteSpan is the internal identity of one paste chip. The textarea stores
// only the user-visible label; this out-of-band span is what makes an inserted
// chip expandable. Literal chip-shaped text therefore remains literal.
type pasteSpan struct {
	id    int
	start int // byte offset in textarea.Value()
	end   int // byte offset immediately after the visible label
}

func trailingInputToken(value string) (inputToken, bool) {
	var token inputToken
	var b strings.Builder
	var quote rune
	tokenStarted := false
	escaped := false

	for i, r := range value {
		if escaped {
			if r == quote || r == '\\' {
				b.WriteRune(r)
			} else {
				b.WriteRune('\\')
				b.WriteRune(r)
			}
			tokenStarted = true
			escaped = false
			continue
		}

		if quote != 0 {
			switch r {
			case '\\':
				escaped = true
			case quote:
				quote = 0
				tokenStarted = true
			default:
				b.WriteRune(r)
				tokenStarted = true
			}
			continue
		}

		if unicode.IsSpace(r) {
			b.Reset()
			token = inputToken{}
			tokenStarted = false
			continue
		}

		if !tokenStarted {
			tokenStarted = true
			token.start = i
		}

		if (r == '"' || r == '\'') && (b.Len() == 0 || b.String() == "@") {
			// Quote mode opens only at token start, with one exception: @file
			// references may start as @"path with spaces". A mid-word apostrophe
			// stays literal. Without this, typing "don't forget @mai" merged the
			// whole rest of the line into one quoted token, so autocomplete died
			// for the remainder of the message after any English contraction.
			if token.quote == 0 {
				token.quote = r
			}
			quote = r
			continue
		}

		b.WriteRune(r)
	}

	if escaped {
		b.WriteRune('\\')
	}
	if !tokenStarted {
		return inputToken{}, false
	}
	token.text = b.String()
	return token, true
}

// InputModel represents the input component.
type InputModel struct {
	textarea       textarea.Model
	styles         *Styles
	viewportHeight int      // terminal rows available to the main composer; 0 = unconstrained
	history        []string // Command history
	historyIndex   int      // Current position in history (-1 = new input)
	savedInput     string   // Saved current input when browsing history
	workDir        string   // Working directory for file suggestions

	// Autocomplete
	commands        []CommandInfo
	suggestions     []CommandInfo
	suggestionType  SuggestionType
	argSuggestions  []string
	suggestionArg   string
	fileSuggestions []string
	suggestionIndex int
	// argNavigated is true once the user explicitly moved the highlight
	// (↑/↓) inside an ARGUMENT dropdown. Only then does Enter accept the
	// highlighted option — a bare Enter after typing must stay "run as
	// typed" so a highlighted destructive flag (--force) is never inserted
	// by an ordinary submit keystroke (v0.100.106).
	argNavigated     bool
	showSuggestions  bool
	suggestionNotice string

	// Alias map: user-typed short name → canonical command. Wired from
	// commands.Handler at startup so /p resolves to plan in autocomplete
	// even though "p" isn't in the commands slice. Written once at startup,
	// treated as read-only afterward — no locking needed.
	commandAliases map[string]string

	// Recent command names in most-recently-used order (≤ 5 entries). Small
	// additive score bump so frequently used commands rank first on ties.
	//
	// NOTE on synchronization: InputModel is embedded by value in ui.Model,
	// which Bubble Tea passes by value through Init/Update/View. This
	// prevents using sync.Mutex or atomic primitives (they carry noCopy and
	// would trip vet across 20+ call sites). RecordRecentCommand writes
	// from the app goroutine; updateSuggestions reads from the Bubble Tea
	// event loop. In practice slice-header assignment is word-sized on all
	// supported architectures, so the worst case of a race is one keystroke
	// seeing a stale ranking — not corruption. This matches the existing
	// pattern for Model setters (AddSystemMessage, SetPermissionsEnabled,
	// etc.) which also mutate Model state from app goroutines without
	// locks.
	recentCommands []string

	// Ghost text (inline completion hint)
	ghostText    string // Suggested completion shown in dim color
	ghostEnabled bool   // Whether ghost text is enabled

	// Argument hints
	showArgHints   bool         // Show argument hints after command
	currentCommand *CommandInfo // Current command for arg hints

	// History search (Ctrl+R)
	historySearchMode   bool
	historySearchQuery  string
	historySearchResult string
	historySearchIndex  int

	// Placeholder context
	activeTask string
	// placeholderTipIndex rotates placeholderTips on each Reset (every send or
	// Ctrl+U clear) so the empty composer surfaces one discovery tip at a time
	// instead of a static string. Plain int — copies with the by-value Model
	// like the rest of InputModel's scalar state.
	placeholderTipIndex int
	placeholderTipsOn   bool

	// Paste collapsing (Claude-Code-style): a large bracketed paste is replaced
	// in the textarea with a compact "[Pasted text #N +M lines]" chip; the real
	// content is stored here, keyed by N, and expanded back on submit
	// (ExpandedValue). Keeps the input readable instead of dumping hundreds of
	// lines. Cleared on Reset. The map is reference-shared across the by-value
	// Model copies (intended — chips accumulate across keystrokes until Reset).
	pastes          map[int]string
	pasteSpans      []pasteSpan
	savedPasteSpans []pasteSpan
	pasteSeq        int
}

// placeholderTips rotate through the empty composer's placeholder, advancing
// one step per Reset. Index 0 must stay the classic "Message or /command" —
// first-run familiarity keys on it; later entries surface one binding each,
// worded to match the welcome panel and shortcuts overlay (hints.go keeps the
// canonical binding list; TestGeneralHintsMatchCurrentBindings guards drift).
var placeholderTips = []string{
	"Message or /command",
	"Message · / commands · @ files",
	"Message · Ctrl+K model · ? shortcuts",
	"Message · Shift+Tab cycles modes",
	"Message · Ctrl+P all actions",
	"Message · Ctrl+E last tool output",
}

// NewInputModel creates a new input model.
func NewInputModel(styles *Styles, workDir string) InputModel {
	ta := textarea.New()
	ta.Placeholder = placeholderTips[0]
	ta.Focus()
	ta.CharLimit = 10000
	ta.ShowLineNumbers = false
	ta.SetHeight(1)

	// Violet ›-prompt prefix per the Gokin Classic mockup — pulls the
	// eye to where typing happens and gives the input area a typographic
	// anchor that survives across themes via ColorPrimary.
	ta.Prompt = "› "
	// Violet glyph as the typographic anchor, but un-bold — with the quiet input
	// border the prompt no longer needs to shout to mark where typing happens.
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(ColorPrimary)
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(ColorMuted)

	return InputModel{
		textarea:           ta,
		styles:             styles,
		workDir:            workDir,
		history:            make([]string, 0, maxHistorySize),
		historyIndex:       -1,
		savedInput:         "",
		commands:           DefaultCommands(),
		suggestions:        nil,
		suggestionType:     SuggestionCommand,
		argSuggestions:     nil,
		fileSuggestions:    nil,
		suggestionIndex:    0,
		showSuggestions:    false,
		ghostText:          "",
		ghostEnabled:       true, // Enable ghost text by default
		historySearchIndex: -1,
		placeholderTipsOn:  true,
	}
}

// DefaultCommands returns the default list of slash commands shown in
// autocomplete suggestions. Exported so cross-package guard tests in
// internal/commands can verify every registered command appears here
// (see TestEveryRegisteredCommandIsInAutocomplete).
func DefaultCommands() []CommandInfo {
	return []CommandInfo{
		// Getting Started
		{Name: "help", Description: "Show help for commands", Category: "Getting Started",
			Args: []ArgInfo{{Name: "command", Required: false, Type: "string"}}, Usage: "/help [command]"},
		{Name: "quickstart", Description: "Quick start guide with examples", Category: "Getting Started"},
		{Name: "shortcuts", Description: "Show keyboard shortcuts", Category: "Getting Started"},

		// Session
		{Name: "model", Description: "Switch AI model", Category: "Session",
			// Tab-complete options reflect the v0.65+ supported set —
			// gemini/claude/gpt entries pre-v0.78.30 were stale references
			// to providers removed during the v0.65 trim.
			Args:  []ArgInfo{{Name: "model", Required: false, Type: "option", Options: []string{"list", "glm-5.2", "glm-5.1", "deepseek-v4-pro", "deepseek-v4-flash", "k3", "kimi-for-coding", "MiniMax-M2.7", "ollama"}}},
			Usage: "/model [name]"},
		{Name: "clear", Description: "Clear conversation history", Category: "Session",
			Args: []ArgInfo{{Name: "force", Required: false, Type: "option", Options: []string{"--force"}}}, Usage: "/clear [--force]"},
		{Name: "compact", Description: "Force context compaction/summarization", Category: "Session"},
		{Name: "save", Description: "Save current session", Category: "Session",
			Args: []ArgInfo{
				{Name: "name", Required: false, Type: "string"},
				{Name: "force", Required: false, Type: "option", Options: []string{"--force"}},
			}, Usage: "/save [name] [--force]"},
		{Name: "resume", Description: "Resume a saved session", Category: "Session",
			Args: []ArgInfo{
				{Name: "session", Required: false, Type: "string"},
				{Name: "force", Required: false, Type: "option", Options: []string{"--force"}},
			}, Usage: "/resume [session_id] [--force]"},
		{Name: "sessions", Description: "List saved sessions", Category: "Session",
			Args: []ArgInfo{{Name: "scope", Required: false, Type: "option", Options: []string{"--all"}}}, Usage: "/sessions [--all]"},
		{Name: "stats", Description: "Show detailed session statistics", Category: "Session"},
		{Name: "tasks", Description: "List background agent + shell tasks, view results, or stop a running one", Category: "Session",
			Args: []ArgInfo{{Name: "id", Required: false, Type: "string"}}, Usage: "/tasks [id]"},
		{Name: "audit", Description: "Multi-agent find→verify audit of the current changes (or a given scope) for real bugs", Category: "Session",
			Args: []ArgInfo{{Name: "path", Required: false, Type: "string"}}, Usage: "/audit [path or description]"},
		{Name: "cost", Description: "Show token usage and cost", Category: "Session"},
		{Name: "memory", Description: "Show stored memories", Category: "Session"},
		{Name: "loop", Description: "Run a recurring task in the background", Category: "Session",
			Args: []ArgInfo{
				{Name: "action_or_task", Required: false, Type: "option", Options: []string{"list", "status", "output", "pause", "resume", "stop", "now", "remove", "--max-tokens"}},
				{Name: "task_or_id", Required: false, Type: "string"},
				{Name: "token_budget", Required: false, Type: "string"},
			}, Usage: "/loop [<interval>] <task> [--max-tokens <N>] | list|status|output|pause|resume|stop|now|remove [id]"},
		// /undo and /redo are fundamental enough to be in Session autocomplete.
		// Were missing pre-v0.78.14 — caught by TestEveryRegisteredCommandIsInAutocomplete.
		{Name: "undo", Description: "Undo last file change(s)", Category: "Session",
			Args:  []ArgInfo{{Name: "n_or_all_or_list", Required: false, Type: "string"}},
			Usage: "/undo [N|all|list]"},
		{Name: "redo", Description: "Redo last undone change(s)", Category: "Session",
			Args:  []ArgInfo{{Name: "n_or_all", Required: false, Type: "string"}},
			Usage: "/redo [N|all]"},
		{Name: "instructions", Description: "Show loaded project instructions (GOKIN.md/CLAUDE.md)", Category: "Session",
			Args:  []ArgInfo{{Name: "source", Required: false, Type: "option", Options: []string{"--source"}}},
			Usage: "/instructions [--source]"},

		// Auth & Setup
		{Name: "login", Description: "Set API key for a provider", Category: "Auth",
			Args: []ArgInfo{{Name: "provider", Required: false, Type: "option", Options: defaultKeyProviderNames()},
				{Name: "key", Required: false, Type: "string"}}, Usage: "/login [provider] [key]"},
		{Name: "logout", Description: "Remove API key", Category: "Auth",
			Args: []ArgInfo{
				{Name: "provider", Required: false, Type: "option", Options: defaultAllProviderNames()},
				{Name: "force", Required: false, Type: "option", Options: []string{"--force"}},
			}, Usage: "/logout [provider|all] [--force]"},
		{Name: "provider", Description: "Switch AI provider", Category: "Auth",
			Args:  []ArgInfo{{Name: "provider", Required: false, Type: "option", Options: defaultProviderNames()}},
			Usage: "/provider [name]"},
		{Name: "status", Description: "Show configuration status", Category: "Auth"},
		{Name: "doctor", Description: "Check environment and configuration", Category: "Auth"},
		{Name: "config", Description: "Show current configuration", Category: "Auth"},
		{Name: "set", Description: "View or change a setting (live)", Category: "Auth",
			Args: []ArgInfo{{Name: "key|preset", Required: false, Type: "option",
				Options: []string{"permissions", "sandbox", "diff", "autocompact", "memory", "globalmemory", "sessionmemory", "plan", "donegate", "thinking", "session", "watcher", "searchcache", "tokens", "compactui", "reducedmotion", "toolcalls", "markdown", "hints", "bell", "nativealerts", "glmsearch", "preset"}},
				{Name: "value", Required: false, Type: "option", Options: []string{"on", "off"},
					OptionsByPrevious: map[string][]string{"preset": {"safe", "balanced", "fast"}}}},
			Usage: "/set [<key> <on|off> | preset <safe|balanced|fast>]"},
		{Name: "settings", Description: "Open the interactive settings screen", Category: "Auth"},
		{Name: "update", Description: "Check for and install application updates", Category: "Auth",
			Args:  []ArgInfo{{Name: "action", Required: false, Type: "option", Options: []string{"install", "backups", "rollback"}}},
			Usage: "/update [install|backups|rollback [backup-id]]"},
		// v0.74–v0.76 release feedback set. These were registered in
		// commands.go but missing from autocomplete — surfaced by
		// TestEveryRegisteredCommandIsInAutocomplete in v0.78.14.
		{Name: "restart", Description: "Re-exec gokin (apply updates without manual quit)", Category: "Auth"},
		{Name: "whats-new", Description: "Show release notes for the current (or specified) version", Category: "Auth",
			Args: []ArgInfo{{Name: "tag", Required: false, Type: "string"}}, Usage: "/whats-new [tag]"},
		{Name: "changelog", Description: "List recent releases (compact)", Category: "Auth",
			Args: []ArgInfo{{Name: "count", Required: false, Type: "number"}}, Usage: "/changelog [count]"},

		// Git
		{Name: "init", Description: "Initialize GOKIN.md for this project", Category: "Git"},
		{Name: "commit", Description: "Create a git commit", Category: "Git",
			Args: []ArgInfo{
				{Name: "message", Required: false, Type: "string"},
				{Name: "mode", Required: false, Type: "option", Options: []string{"-m"}},
			}, Usage: "/commit [message] | /commit -m <message>"},
		{Name: "pr", Description: "Create a pull request", Category: "Git",
			Args: []ArgInfo{{Name: "options", Required: false, Type: "option",
				Options: []string{"--title", "--draft", "--base"}}},
			Usage: "/pr [--title title] [--draft] [--base branch]"},
		// v0.77.x git-inspect family + v0.78.12 /blame. These were registered
		// in commands.go but missing from autocomplete — added here so the
		// suggestion list matches the actual available commands.
		{Name: "diff", Description: "Show pending git changes", Category: "Git",
			Args: []ArgInfo{
				{Name: "mode", Required: false, Type: "option", Options: []string{"--stat", "--staged", "--cached"}},
				{Name: "file", Required: false, Type: "path"},
			},
			Usage: "/diff [--staged|--stat|<file>]"},
		{Name: "log", Description: "Show recent git commits", Category: "Git",
			Args:  []ArgInfo{{Name: "count_or_file", Required: false, Type: "string"}},
			Usage: "/log [count] [file]"},
		{Name: "branches", Description: "List local branches with last commit", Category: "Git",
			Args:  []ArgInfo{{Name: "scope", Required: false, Type: "option", Options: []string{"--all"}}},
			Usage: "/branches [--all]"},
		{Name: "grep", Description: "Search the working tree for a pattern", Category: "Git",
			Args:  []ArgInfo{{Name: "pattern", Required: true, Type: "string"}, {Name: "path", Required: false, Type: "path"}},
			Usage: "/grep <pattern> [path]"},
		{Name: "blame", Description: "Show line-by-line git blame for a file", Category: "Git",
			Args:  []ArgInfo{{Name: "file", Required: true, Type: "path"}, {Name: "range", Required: false, Type: "string"}},
			Usage: "/blame <file> [N|N-M|N M]"},
		{Name: "show", Description: "Show a specific commit (metadata + diff)", Category: "Git",
			Args:  []ArgInfo{{Name: "ref", Required: false, Type: "string"}, {Name: "file", Required: false, Type: "path"}},
			Usage: "/show [ref] [file]"},

		// Planning
		{Name: "plan", Description: "Toggle planning mode for complex multi-step tasks", Category: "Planning",
			Args:  []ArgInfo{{Name: "action", Required: false, Type: "option", Options: []string{"status"}}},
			Usage: "/plan [status]"},
		{Name: "resume-plan", Description: "Resume a paused/failed plan execution", Category: "Planning",
			Args:  []ArgInfo{{Name: "list|plan_id", Required: false, Type: "option", Options: []string{"list"}}},
			Usage: "/resume-plan [list|<plan_id>]"},
		{Name: "health", Description: "Show runtime health and provider reliability", Category: "Planning"},
		{Name: "policy", Description: "Show policy engine and circuit breaker state", Category: "Planning"},
		{Name: "ledger", Description: "Show run ledger for current plan", Category: "Planning"},
		{Name: "plan-proof", Description: "Show contract/evidence proof for a plan step", Category: "Planning",
			Args: []ArgInfo{{Name: "step_id", Required: false, Type: "number"}}, Usage: "/plan-proof [step_id]"},
		{Name: "journal", Description: "Show recent execution journal events", Category: "Planning"},
		{Name: "recovery", Description: "Show latest recovery snapshot", Category: "Planning"},
		{Name: "observability", Description: "Show unified observability dashboard", Category: "Planning"},
		{Name: "memory-governance", Description: "Show session memory governance status", Category: "Planning"},
		{Name: "tree-stats", Description: "Show tree planner statistics", Category: "Planning"},

		// Tools
		{Name: "pwd", Description: "Show current working directory", Category: "Tools"},
		{Name: "browse", Description: "Open interactive file browser", Category: "Tools",
			Args: []ArgInfo{{Name: "path", Required: false, Type: "path"}}, Usage: "/browse [path]"},
		{Name: "open", Description: "Open a file in your editor", Category: "Tools",
			Args: []ArgInfo{{Name: "file", Required: true, Type: "path"}}, Usage: "/open <file>"},
		{Name: "copy", Description: "Copy text, the last response, or the conversation", Category: "Tools",
			Args: []ArgInfo{
				{Name: "source", Required: false, Type: "option", Options: []string{"--last", "--all", "--ascii"}},
				{Name: "text", Required: false, Type: "string"},
			},
			Usage: "/copy [--last|--all|--ascii] [<text>]"},
		{Name: "paste", Description: "Get text from clipboard", Category: "Tools"},
		{Name: "clear-todos", Description: "Clear all todo items", Category: "Tools",
			Args:  []ArgInfo{{Name: "force", Required: false, Type: "option", Options: []string{"--force"}}},
			Usage: "/clear-todos [--force]"},
		{Name: "ql", Description: "Preview a file using macOS Quick Look", Category: "Tools",
			Args: []ArgInfo{{Name: "path", Required: true, Type: "path"}}, Usage: "/ql <path>"},
		{Name: "hooks", Description: "List configured agent hooks and their sources", Category: "Tools"},
		{Name: "add-dir", Description: "Grant the agent access to a directory outside the workspace", Category: "Tools",
			Args: []ArgInfo{{Name: "persist", Required: false, Type: "option", Options: []string{"--persist"}},
				{Name: "path", Required: true, Type: "path"}},
			Usage: "/add-dir [--persist] <path>"},
		{Name: "remove-dir", Description: "Revoke a session directory grant added with /add-dir", Category: "Tools",
			Args: []ArgInfo{{Name: "path", Required: true, Type: "path"}}, Usage: "/remove-dir <path>"},
		{Name: "permissions", Description: "View or configure risky-action permission prompts", Category: "Tools",
			Args:  []ArgInfo{{Name: "mode", Required: false, Type: "option", Options: []string{"on", "off"}}},
			Usage: "/permissions [on|off]"},
		{Name: "sandbox", Description: "View or configure bash sandbox containment", Category: "Tools",
			Args:  []ArgInfo{{Name: "mode", Required: false, Type: "option", Options: []string{"on", "off"}}},
			Usage: "/sandbox [on|off]"},
		{Name: "thinking", Description: "Configure adaptive/forced reasoning and its token budget", Category: "Tools",
			Args:  []ArgInfo{{Name: "mode", Required: false, Type: "option", Options: []string{"auto", "on", "off"}}},
			Usage: "/thinking [auto|on|off|<budget>]"},
		{Name: "timeout", Description: "View or change the model round timeout", Category: "Tools",
			Args:  []ArgInfo{{Name: "duration", Required: false, Type: "option", Options: []string{"14m", "20m", "30m", "default"}}},
			Usage: "/timeout [<duration>|default]"},
		{Name: "theme", Description: "Show the active UI theme", Category: "Tools",
			Usage: "/theme"},
		{Name: "register-agent-type", Description: "Register a custom agent type with specific tools", Category: "Tools",
			Args: []ArgInfo{
				{Name: "name", Required: true, Type: "string"},
				{Name: "description", Required: true, Type: "string"},
				{Name: "option", Required: false, Type: "option", Options: []string{"--tools", "--prompt"}},
				{Name: "value", Required: false, Type: "string"},
			}, Usage: `/register-agent-type <name> "<description>" [--tools t1,t2] [--prompt "text"]`},
		{Name: "list-agent-types", Description: "List all registered agent types (built-in and custom)", Category: "Tools"},
		{Name: "unregister-agent-type", Description: "Remove a custom agent type", Category: "Tools",
			Args: []ArgInfo{{Name: "name", Required: true, Type: "string"}}, Usage: "/unregister-agent-type <name>"},
		{Name: "mcp", Description: "Manage MCP (Model Context Protocol) servers", Category: "Tools",
			Args: []ArgInfo{
				{Name: "subcommand", Required: false, Type: "option",
					Options: []string{"list", "status", "enable", "disable", "add", "remove", "pause", "resume", "edit", "refresh", "preset", "setup", "help"}},
				{Name: "preset", Required: false, Type: "option",
					// Offered only after `preset` (server names for pause/
					// resume/edit are runtime state — no static options).
					OptionsByPrevious: map[string][]string{
						"preset": {"github", "filesystem", "sqlite", "brave-search", "puppeteer", "memory", "fetch", "sequential-thinking", "time", "postgres"},
					}},
			},
			Usage: "/mcp [list|status|enable|disable|add|remove|pause|resume|edit|refresh|preset|setup|help]"},
		{Name: "skill", Description: "List or run reusable project/user workflows", Category: "Tools",
			Args: []ArgInfo{
				{Name: "name", Required: false, Type: "string"},
				{Name: "arguments", Required: false, Type: "string"},
			},
			Usage: "/skill [name] [arguments...]"},
	}
}

// Init initializes the input model.
func (m InputModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m InputModel) onFirstComposerVisualLine() bool {
	return m.textarea.Line() == 0 && m.textarea.LineInfo().RowOffset == 0
}

func (m InputModel) onLastComposerVisualLine() bool {
	lineInfo := m.textarea.LineInfo()
	return m.textarea.Line() == m.textarea.LineCount()-1 && lineInfo.RowOffset+1 >= lineInfo.Height
}

func clonePasteSpans(spans []pasteSpan) []pasteSpan {
	return append([]pasteSpan(nil), spans...)
}

func (m *InputModel) clearCompletionState() {
	m.showSuggestions = false
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.fileSuggestions = nil
	m.suggestionIndex = 0
	m.ghostText = ""
	m.suggestionNotice = ""
	m.showArgHints = false
	m.currentCommand = nil
}

func textareaCursorByteOffset(model textarea.Model) int {
	value := model.Value()
	lines := strings.Split(value, "\n")
	row := min(max(model.Line(), 0), len(lines)-1)
	lineInfo := model.LineInfo()
	column := max(lineInfo.StartColumn+lineInfo.ColumnOffset, 0)
	lineRunes := []rune(lines[row])
	column = min(column, len(lineRunes))

	offset := 0
	for i := 0; i < row; i++ {
		offset += len(lines[i]) + 1
	}
	return offset + len(string(lineRunes[:column]))
}

func (m *InputModel) reconcilePasteEdit(before, after string, oldStart, oldEnd int) {
	if len(m.pasteSpans) == 0 || before == after {
		return
	}
	oldStart = min(max(oldStart, 0), len(before))
	oldEnd = min(max(oldEnd, oldStart), len(before))
	delta := len(after) - len(before)
	next := make([]pasteSpan, 0, len(m.pasteSpans))
	for _, span := range m.pasteSpans {
		content, exists := m.pastes[span.id]
		label := pasteDisplayLabel(span.id, content)
		if !exists || span.start < 0 || span.end > len(before) || span.start >= span.end || before[span.start:span.end] != label {
			continue
		}
		switch {
		case oldEnd <= span.start:
			span.start += delta
			span.end += delta
			next = append(next, span)
		case oldStart >= span.end:
			next = append(next, span)
		default:
			// Any edit inside a chip turns the remaining text into ordinary
			// literal text. It must never expand to hidden content later.
		}
	}
	m.pasteSpans = next
}

func (m *InputModel) reconcilePasteChange(before, after string, prefixLimit int) {
	if before == after {
		return
	}
	prefixLimit = min(max(prefixLimit, 0), min(len(before), len(after)))
	start := 0
	for start < prefixLimit && before[start] == after[start] {
		start++
	}
	oldEnd, newEnd := len(before), len(after)
	for oldEnd > start && newEnd > start && before[oldEnd-1] == after[newEnd-1] {
		oldEnd--
		newEnd--
	}
	m.reconcilePasteEdit(before, after, start, oldEnd)
}

func (m *InputModel) updateTextarea(msg tea.Msg) tea.Cmd {
	before := m.textarea.Value()
	beforeCursor := textareaCursorByteOffset(m.textarea)
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	after := m.textarea.Value()
	afterCursor := textareaCursorByteOffset(m.textarea)
	m.reconcilePasteChange(before, after, min(beforeCursor, afterCursor))
	return cmd
}

func graphemeRangeAtCursor(value string, cursor int, forward bool) (int, int, bool) {
	if cursor < 0 || cursor > len(value) {
		return 0, 0, false
	}
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		start, end := graphemes.Positions()
		if (forward && cursor >= start && cursor < end) || (!forward && cursor > start && cursor <= end) {
			return start, end, true
		}
	}
	return 0, 0, false
}

func (m *InputModel) deleteComposerGrapheme(forward bool) (tea.Cmd, bool) {
	before := m.textarea.Value()
	cursor := textareaCursorByteOffset(m.textarea)
	clusterStart, clusterEnd, ok := graphemeRangeAtCursor(before, cursor, forward)
	if !ok {
		return nil, false
	}

	forwardCount := utf8.RuneCountInString(before[cursor:clusterEnd])
	backwardCount := utf8.RuneCountInString(before[clusterStart:cursor])
	cmds := make([]tea.Cmd, 0, forwardCount+backwardCount)
	// Delete the part after an inside-cluster caret first, then the part before
	// it. Running the textarea's native edits preserves its viewport and cursor
	// blink bookkeeping while still treating the full EGC as one editing unit.
	for range forwardCount {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(tea.KeyMsg{Type: tea.KeyDelete})
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	for range backwardCount {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	after := m.textarea.Value()
	m.reconcilePasteEdit(before, after, clusterStart, clusterEnd)
	return tea.Batch(cmds...), true
}

func (m InputModel) graphemeDeleteDirection(msg tea.KeyMsg) (forward, matched bool) {
	switch {
	case key.Matches(msg, m.textarea.KeyMap.DeleteCharacterBackward):
		return false, true
	case key.Matches(msg, m.textarea.KeyMap.DeleteCharacterForward):
		return true, true
	default:
		return false, false
	}
}

func (m *InputModel) refreshCompletionStateForValue() {
	value := m.textarea.Value()
	if word, ok := atFileWord(value); ok {
		m.suggestionType = SuggestionAtFile
		m.updateAtFileSuggestions(word)
		m.showArgHints = false
		m.currentCommand = nil
	} else if strings.HasPrefix(value, "/") && !strings.Contains(value, " ") {
		m.suggestionType = SuggestionCommand
		m.updateSuggestions(value)
		m.showArgHints = false
		m.currentCommand = nil
	} else if m.updateArgumentSuggestions(value) {
		// Structured option completion takes precedence over path heuristics.
	} else if m.shouldSuggestFiles(value) {
		m.suggestionType = SuggestionFile
		m.updateFileSuggestions(value)
	} else {
		m.showSuggestions = false
		m.suggestions = nil
		m.argSuggestions = nil
		m.suggestionArg = ""
		m.fileSuggestions = nil
		m.ghostText = ""
		m.suggestionNotice = ""
		if !strings.HasPrefix(value, "/") {
			m.showArgHints = false
			m.currentCommand = nil
		}
	}
}

// Update handles input events.
func (m InputModel) Update(msg tea.Msg) (InputModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle history search mode
		if m.historySearchMode {
			return m.handleHistorySearch(msg)
		}

		// Collapse a LARGE bracketed paste into a compact "[Pasted text #N …]"
		// chip so the input doesn't fill with hundreds of lines; the real
		// content is restored on submit (ExpandedValue). Small pastes fall
		// through to the normal textarea insert below.
		if msg.Paste && isLargePaste(string(msg.Runes)) {
			return m.collapsePaste(string(msg.Runes)), nil
		}
		if forward, matched := m.graphemeDeleteDirection(msg); matched {
			cmd, deleted := m.deleteComposerGrapheme(forward)
			if deleted {
				m.syncTextareaHeight()
				m.refreshCompletionStateForValue()
				return m, cmd
			}
		}

		switch msg.Type {
		case tea.KeyCtrlR:
			// Enter history search mode
			if m.historyIndex == -1 {
				// Keep the live draft behind accepted history navigation. After
				// choosing a match, Down can walk forward and return to it.
				m.savedInput = m.textarea.Value()
				m.savedPasteSpans = clonePasteSpans(m.pasteSpans)
			}
			m.historySearchMode = true
			m.historySearchQuery = ""
			m.historySearchResult = ""
			m.historySearchIndex = len(m.history)
			m.syncTextareaHeight()
			if !m.historySearchSurfaceReadable() {
				m.leaveHistorySearch()
				m.syncTextareaHeight()
				return m, nil
			}
			m.showSuggestions = false
			m.suggestions = nil
			m.argSuggestions = nil
			m.suggestionArg = ""
			m.fileSuggestions = nil
			m.ghostText = ""
			m.suggestionNotice = ""
			m.showArgHints = false
			m.currentCommand = nil
			return m, nil

		case tea.KeyTab:
			// Accept ghost text if visible (and no dropdown)
			if m.ghostText != "" && !m.showSuggestions {
				m.textarea.SetValue(m.textarea.Value() + m.ghostText)
				m.textarea.CursorEnd()
				m.syncTextareaHeight()
				m.ghostText = ""
				return m, nil
			}

			// Accept suggestion from dropdown
			if m.showSuggestions {
				if !m.suggestionActionsReadable() {
					m.clearCompletionState()
					return m, nil
				}
				if m.suggestionType == SuggestionCommand && len(m.suggestions) > 0 {
					selected := m.suggestions[m.suggestionIndex]
					m.textarea.SetValue("/" + selected.Name + " ")
					m.textarea.CursorEnd()
					m.syncTextareaHeight()
					m.showSuggestions = false
					m.suggestions = nil
					m.ghostText = ""
					// Show arg hints if command has arguments
					if len(selected.Args) > 0 {
						m.showArgHints = true
						m.currentCommand = &selected
						m.updateArgumentSuggestions(m.textarea.Value())
					}
					return m, nil
				} else if m.suggestionType == SuggestionArgument && len(m.argSuggestions) > 0 {
					m.acceptArgumentSuggestion(m.argSuggestions[m.suggestionIndex])
					return m, nil
				} else if (m.suggestionType == SuggestionFile || m.suggestionType == SuggestionAtFile) && len(m.fileSuggestions) > 0 {
					selected := m.fileSuggestions[m.suggestionIndex]
					m.acceptFileSuggestion(selected)
					return m, nil
				}
			}

			// Try to trigger suggestions
			value := m.textarea.Value()
			if word, ok := atFileWord(value); ok {
				m.suggestionType = SuggestionAtFile
				m.updateAtFileSuggestions(word)
			} else if strings.HasPrefix(value, "/") && !strings.Contains(value, " ") {
				m.updateSuggestions(value)
				if len(m.suggestions) == 1 {
					// Auto-complete if only one match
					selected := m.suggestions[0]
					m.textarea.SetValue("/" + selected.Name + " ")
					m.textarea.CursorEnd()
					m.syncTextareaHeight()
					m.showSuggestions = false
					m.suggestions = nil
					m.ghostText = ""
					// Show arg hints
					if len(selected.Args) > 0 {
						m.showArgHints = true
						m.currentCommand = &selected
						m.updateArgumentSuggestions(m.textarea.Value())
					}
				}
			} else if m.updateArgumentSuggestions(value) {
				// Argument options are accepted by Tab only. Enter remains submit.
			} else if m.shouldSuggestFiles(value) {
				m.updateFileSuggestions(value)
			}
			return m, nil

		case tea.KeyEnter:
			if m.showSuggestions && !m.suggestionActionsReadable() {
				// A cropped dropdown must not own Enter: the parent model can
				// submit the draft as typed, while direct InputModel use consumes
				// the key without inserting a textarea newline.
				m.clearCompletionState()
				return m, nil
			}
			if m.showSuggestions && m.suggestionType == SuggestionArgument {
				if m.argNavigated && len(m.argSuggestions) > 0 {
					// The user explicitly moved the highlight — Enter accepts
					// the selected option, exactly like Tab (v0.100.106).
					m.acceptArgumentSuggestion(m.argSuggestions[m.suggestionIndex])
					return m, nil
				}
				// No explicit navigation: never let Enter insert an option
				// (especially --force) or a textarea newline implicitly —
				// submit-as-typed stays with the parent model.
				m.showSuggestions = false
				m.argSuggestions = nil
				m.suggestionArg = ""
				return m, nil
			}
			// Handle autocomplete on Enter
			if m.showSuggestions {
				if m.suggestionType == SuggestionCommand && len(m.suggestions) > 0 {
					selected := m.suggestions[m.suggestionIndex]
					m.textarea.SetValue("/" + selected.Name + " ")
					m.textarea.CursorEnd()
					m.syncTextareaHeight()
					m.showSuggestions = false
					m.suggestions = nil
					m.ghostText = ""
					// Show arg hints
					if len(selected.Args) > 0 {
						m.showArgHints = true
						m.currentCommand = &selected
						m.updateArgumentSuggestions(m.textarea.Value())
					}
					return m, nil
				} else if (m.suggestionType == SuggestionFile || m.suggestionType == SuggestionAtFile) && len(m.fileSuggestions) > 0 {
					selected := m.fileSuggestions[m.suggestionIndex]
					m.acceptFileSuggestion(selected)
					return m, nil
				}
			}

		case tea.KeyUp:
			// Navigate suggestions or history
			if m.showSuggestions {
				if !m.suggestionActionsReadable() {
					m.clearCompletionState()
					return m, nil
				}
				count := 0
				if m.suggestionType == SuggestionCommand {
					count = len(m.suggestions)
				} else if m.suggestionType == SuggestionArgument {
					count = len(m.argSuggestions)
				} else {
					count = len(m.fileSuggestions)
				}
				if count > 0 && m.suggestionIndex > 0 {
					m.suggestionIndex--
				}
				if m.suggestionType == SuggestionArgument {
					m.argNavigated = true
				}
				return m, nil
			}
			if !m.onFirstComposerVisualLine() {
				break
			}
			// Navigate to older history (rest of logic continues...)
			// Navigate to older history
			if len(m.history) > 0 {
				if m.historyIndex == -1 {
					m.savedInput = m.textarea.Value()
					m.savedPasteSpans = clonePasteSpans(m.pasteSpans)
					m.historyIndex = len(m.history) - 1
				} else if m.historyIndex > 0 {
					m.historyIndex--
				}
				// Bounds check: historyIndex may exceed len if history was externally trimmed
				if m.historyIndex >= 0 && m.historyIndex < len(m.history) {
					m.setRecalledValue(m.history[m.historyIndex])
				} else {
					m.historyIndex = -1
				}
			}
			return m, nil

		case tea.KeyDown:
			// Navigate suggestions or history
			if m.showSuggestions {
				if !m.suggestionActionsReadable() {
					m.clearCompletionState()
					return m, nil
				}
				count := 0
				if m.suggestionType == SuggestionCommand {
					count = len(m.suggestions)
				} else if m.suggestionType == SuggestionArgument {
					count = len(m.argSuggestions)
				} else {
					count = len(m.fileSuggestions)
				}
				if count > 0 && m.suggestionIndex < count-1 {
					m.suggestionIndex++
				}
				if m.suggestionType == SuggestionArgument {
					m.argNavigated = true
				}
				return m, nil
			}
			if !m.onLastComposerVisualLine() {
				break
			}
			// Navigate to newer history
			if m.historyIndex >= 0 {
				if m.historyIndex < len(m.history)-1 {
					m.historyIndex++
					if m.historyIndex < len(m.history) {
						m.setRecalledValue(m.history[m.historyIndex])
					} else {
						m.historyIndex = -1
						m.restoreSavedInput()
					}
				} else {
					m.historyIndex = -1
					m.restoreSavedInput()
				}
			}
			return m, nil

		case tea.KeyEscape:
			// Cancel suggestions and ghost text
			if m.showSuggestions || m.ghostText != "" || m.showArgHints || m.suggestionNotice != "" {
				m.showSuggestions = false
				m.suggestions = nil
				m.argSuggestions = nil
				m.suggestionArg = ""
				m.fileSuggestions = nil
				m.ghostText = ""
				m.suggestionNotice = ""
				m.showArgHints = false
				m.currentCommand = nil
				return m, nil
			}

		case tea.KeyShiftTab:
			// Ignore Shift+Tab - it's handled by the parent TUI for planning mode toggle
			// Don't pass to textarea to avoid unexpected behavior
			return m, nil
		}

		// Update textarea and check for suggestion updates.
		beforeValue := m.textarea.Value()
		cmd := m.updateTextarea(msg)
		m.syncTextareaHeight()
		if m.textarea.Value() != beforeValue {
			m.refreshCompletionStateForValue()
		}

		return m, cmd
	}

	cmd := m.updateTextarea(msg)
	m.syncTextareaHeight()
	return m, cmd
}

// updateDecisionEditor is the plain multiline editor path used by question and
// plan-feedback modals. Those fields have no command suggestions or history;
// routing through the main composer would consume Up/Down for an empty history
// and make earlier lines unreachable. It also keeps pasted answers literal
// instead of collapsing them into composer-only paste chips.
func (m InputModel) updateDecisionEditor(msg tea.Msg) (InputModel, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		if forward, matched := m.graphemeDeleteDirection(keyMsg); matched {
			cmd, deleted := m.deleteComposerGrapheme(forward)
			if deleted {
				m.syncTextareaHeight()
				return m, cmd
			}
		}
	}
	cmd := m.updateTextarea(msg)
	m.syncTextareaHeight()
	return m, cmd
}

// handleHistorySearch handles key events in history search mode.
func (m InputModel) handleHistorySearch(msg tea.KeyMsg) (InputModel, tea.Cmd) {
	if key.Matches(msg, m.textarea.KeyMap.DeleteCharacterBackward) {
		// Remove one user-perceived character. A skin-tone emoji, combining
		// sequence, or ZWJ family is one editing unit even though it contains
		// multiple runes. Match the textarea keymap so Backspace and its Ctrl+H
		// alias retain identical semantics when InputModel is used directly.
		if len(m.historySearchQuery) > 0 {
			m.historySearchQuery = removeLastGrapheme(m.historySearchQuery)
			m.historySearchIndex = len(m.history)
			m.searchHistory(false)
		}
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		// Accept search result
		if m.historySearchResult != "" {
			m.setRecalledValue(m.historySearchResult)
			m.historyIndex = m.historySearchIndex
		}
		m.historySearchMode = false
		m.historySearchQuery = ""
		m.historySearchResult = ""
		m.historySearchIndex = -1
		m.syncTextareaHeight()
		return m, nil

	case tea.KeyEscape, tea.KeyCtrlC:
		// Cancel search
		m.historySearchMode = false
		m.historySearchQuery = ""
		m.historySearchResult = ""
		m.historySearchIndex = -1
		m.syncTextareaHeight()
		return m, nil

	case tea.KeyCtrlR:
		// Search for next match (older)
		if m.historySearchIndex > 0 {
			previousResult := m.historySearchResult
			previousIndex := m.historySearchIndex
			m.searchHistory(true)
			if m.historySearchResult == "" && previousResult != "" {
				m.historySearchResult = previousResult
				m.historySearchIndex = previousIndex
			}
		}
		return m, nil

	case tea.KeySpace:
		// Bubble Tea reports a physical space as KeySpace, not KeyRunes. Keep
		// multi-word reverse search usable with real terminal key events.
		m.historySearchQuery += " "
		m.historySearchIndex = len(m.history)
		m.searchHistory(false)
		return m, nil

	default:
		// Add character to search query
		if msg.Type == tea.KeyRunes {
			m.historySearchQuery += sanitizeHistorySearchText(string(msg.Runes))
			m.historySearchIndex = len(m.history)
			m.searchHistory(false)
		}
		return m, nil
	}
}

func (m *InputModel) leaveHistorySearch() {
	m.historySearchMode = false
	m.historySearchQuery = ""
	m.historySearchResult = ""
	m.historySearchIndex = -1
}

func sanitizeHistorySearchText(value string) string {
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			if unicode.IsSpace(r) {
				return ' '
			}
			return -1
		}
		return r
	}, value)
}

func removeLastGrapheme(value string) string {
	lastStart := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		lastStart, _ = graphemes.Positions()
	}
	return value[:lastStart]
}

// searchHistory searches history for the current query. allowEmpty is used by
// an explicit repeated Ctrl+R, where an empty query means "older entry" just
// like conventional reverse-search; editing the query back to empty restores
// the neutral placeholder instead.
func (m *InputModel) searchHistory(allowEmpty bool) {
	if m.historySearchQuery == "" && !allowEmpty {
		m.historySearchResult = ""
		return
	}

	query := strings.ToLower(m.historySearchQuery)
	for i := m.historySearchIndex - 1; i >= 0; i-- {
		if query == "" || strings.Contains(strings.ToLower(m.history[i]), query) {
			m.historySearchResult = m.history[i]
			m.historySearchIndex = i
			return
		}
	}
	// No match found
	m.historySearchResult = ""
}

func (m InputModel) historySearchCanSearchOlder() bool {
	if m.historySearchIndex <= 0 {
		return false
	}
	query := strings.ToLower(m.historySearchQuery)
	for i := m.historySearchIndex - 1; i >= 0; i-- {
		if query == "" || strings.Contains(strings.ToLower(m.history[i]), query) {
			return true
		}
	}
	return false
}

func (m *InputModel) setRecalledValue(value string) {
	// History deliberately stores the expanded message so recalling and
	// resubmitting a paste sends the original content. Re-collapse large
	// entries for display, though: expanding them into the textarea defeats
	// the compact-paste UX and can fill the screen. Do not Reset here — the
	// current draft may itself contain chips whose stored content must survive
	// until Down returns to it.
	m.clearCompletionState()
	m.pasteSpans = nil
	if isLargePaste(value) {
		m.textarea.SetValue("")
		m.textarea.CursorEnd()
		*m = m.collapsePaste(value)
		m.textarea.SetHeight(1)
		return
	}
	m.textarea.SetValue(value)
	m.textarea.CursorEnd()
	m.syncTextareaHeight()
}

func (m *InputModel) restoreSavedInput() {
	m.textarea.SetValue(m.savedInput)
	m.textarea.CursorEnd()
	m.pasteSpans = clonePasteSpans(m.savedPasteSpans)
	m.clearCompletionState()
	m.syncTextareaHeight()
}

// fuzzyScore calculates a fuzzy match score for a query against a string.
// Returns 0 if no match, higher score for better matches.
func fuzzyScore(str, query string) int {
	str = strings.ToLower(str)
	query = strings.ToLower(query)

	if query == "" {
		return 0
	}

	score := 0
	qi := 0
	lastMatchIdx := -1
	consecutiveBonus := 0

	for i := 0; i < len(str) && qi < len(query); i++ {
		if str[i] == query[qi] {
			qi++
			score += 10 // Base score for each match

			// Bonus for consecutive matches
			if lastMatchIdx == i-1 {
				consecutiveBonus++
				score += consecutiveBonus * 5
			} else {
				consecutiveBonus = 0
			}

			// Bonus for match at start
			if i == 0 {
				score += 20
			}

			// Bonus for match after separator (like camelCase or word boundary)
			if i > 0 && (str[i-1] == '-' || str[i-1] == '_' || str[i-1] == ' ') {
				score += 15
			}

			lastMatchIdx = i
		}
	}

	// All query characters must be matched
	if qi == len(query) {
		return score
	}
	return 0
}

// scoredCommand holds a command with its match score.
type scoredCommand struct {
	cmd   CommandInfo
	score int
}

// updateSuggestions updates the autocomplete suggestions with fuzzy matching.
// Scoring priority (high → low):
//  1. Exact alias match (e.g. user typed "/p" — show "plan" first).
//  2. Exact prefix match on command name.
//  3. Substring match anywhere in the name (not just prefix).
//  4. Character-by-character fuzzy with consecutive-char bonus.
//
// Recent usage bumps all of the above by a small additive, so frequently
// used commands bubble up when multiple candidates tie.
func (m *InputModel) updateSuggestions(input string) {
	prefix := strings.TrimPrefix(input, "/")
	prefix = strings.ToLower(prefix)

	// Resolve alias up-front: if user typed something that matches an alias
	// exactly, surface the target command as a strong signal.
	aliasTarget := ""
	if target, ok := m.commandAliases[prefix]; ok {
		aliasTarget = strings.ToLower(target)
	}

	var scored []scoredCommand

	for _, cmd := range m.commands {
		cmdLower := strings.ToLower(cmd.Name)

		if aliasTarget != "" && cmdLower == aliasTarget {
			scored = append(scored, scoredCommand{cmd: cmd, score: 2000})
			continue
		}

		// Exact prefix match gets highest priority
		if strings.HasPrefix(cmdLower, prefix) {
			scored = append(scored, scoredCommand{
				cmd:   cmd,
				score: 1000 + (100 - len(cmd.Name)), // Shorter names rank higher
			})
			continue
		}

		// Substring match anywhere (e.g. "mit" finds "commit"). Lower than
		// prefix but higher than fuzzy so "mit" ranks commit above abstract
		// character-by-character matches.
		if prefix != "" && strings.Contains(cmdLower, prefix) {
			scored = append(scored, scoredCommand{
				cmd:   cmd,
				score: 500 + (100 - len(cmd.Name)),
			})
			continue
		}

		// Fuzzy match
		if score := fuzzyScore(cmd.Name, prefix); score > 0 {
			scored = append(scored, scoredCommand{cmd: cmd, score: score})
		}
	}

	// Recent-usage bonus — small additive so frequently used commands win
	// ties but don't override semantic matches. Most-recent = highest bonus.
	for i, name := range m.recentCommands {
		for j := range scored {
			if scored[j].cmd.Name == name {
				scored[j].score += 30 - i*3
			}
		}
	}

	// Sort by score (descending), then by name (ascending)
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].cmd.Name < scored[j].cmd.Name
	})

	// Extract commands
	m.suggestions = make([]CommandInfo, 0, len(scored))
	for _, sc := range scored {
		m.suggestions = append(m.suggestions, sc.cmd)
	}

	m.suggestionIndex = 0
	m.showSuggestions = len(m.suggestions) > 0
	m.suggestionType = SuggestionCommand
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.fileSuggestions = nil
	m.suggestionNotice = ""
	if !m.showSuggestions && prefix != "" {
		m.suggestionNotice = fmt.Sprintf("No commands match %q", "/"+prefix)
	}

	// Update ghost text with top suggestion
	m.updateGhostText(prefix)
}

// updateArgumentSuggestions opens structured option completion for an exact
// slash command. It deliberately returns false for free-form arguments: a
// missing option match is not an error and must never block normal typing.
func (m *InputModel) updateArgumentSuggestions(value string) bool {
	command, argIndex, prefix, ok := m.commandArgumentContext(value)
	m.argSuggestions = nil
	m.suggestionArg = ""
	if !ok {
		return false
	}

	var options []string
	if argIndex >= 0 && argIndex < len(command.Args) {
		options = append(options, argumentOptionsForContext(command.Args[argIndex], argIndex, value)...)
		m.suggestionArg = command.Args[argIndex].Name
	}
	// Flags may legally appear after positional values for several commands.
	// When the user starts a flag, offer every declared flag regardless of the
	// nominal positional index instead of making metadata order a hidden rule.
	if strings.HasPrefix(prefix, "-") {
		for _, arg := range command.Args {
			for _, option := range arg.Options {
				if strings.HasPrefix(option, "-") {
					options = append(options, option)
				}
			}
		}
	}

	seen := make(map[string]bool, len(options))
	prefixLower := strings.ToLower(prefix)
	for _, option := range options {
		if seen[option] || !strings.HasPrefix(strings.ToLower(option), prefixLower) {
			continue
		}
		seen[option] = true
		m.argSuggestions = append(m.argSuggestions, option)
	}
	if len(m.argSuggestions) == 0 {
		m.suggestionArg = ""
		return false
	}

	commandCopy := cloneCommandInfo(command)
	m.currentCommand = &commandCopy
	m.showArgHints = true
	m.suggestions = nil
	m.fileSuggestions = nil
	m.suggestionNotice = ""
	m.ghostText = ""
	m.suggestionType = SuggestionArgument
	m.suggestionIndex = 0
	m.argNavigated = false
	m.showSuggestions = true
	return true
}

func argumentOptionsForContext(arg ArgInfo, argIndex int, value string) []string {
	options := arg.Options
	if argIndex <= 0 || len(arg.OptionsByPrevious) == 0 {
		return options
	}
	fields := splitInputFields(value)
	// fields[0] is the slash command; the previous argument for argIndex N is
	// therefore fields[N] whenever it has already been entered.
	if argIndex >= len(fields) {
		return options
	}
	previous := strings.ToLower(strings.TrimSpace(fields[argIndex]))
	for key, contextual := range arg.OptionsByPrevious {
		if strings.EqualFold(strings.TrimSpace(key), previous) {
			return contextual
		}
	}
	return options
}

func (m InputModel) commandArgumentContext(value string) (CommandInfo, int, string, bool) {
	if !strings.HasPrefix(value, "/") || strings.IndexFunc(value, unicode.IsSpace) < 0 {
		return CommandInfo{}, 0, "", false
	}
	fields := splitInputFields(value)
	if len(fields) == 0 {
		return CommandInfo{}, 0, "", false
	}
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	if target, ok := m.commandAliases[name]; ok {
		name = strings.ToLower(target)
	}
	var command CommandInfo
	found := false
	for _, candidate := range m.commands {
		if strings.EqualFold(candidate.Name, name) {
			command = candidate
			found = true
			break
		}
	}
	if !found || len(command.Args) == 0 {
		return CommandInfo{}, 0, "", false
	}

	argIndex := len(fields) - 1
	prefix := ""
	if token, hasToken := trailingInputToken(value); hasToken && token.start > len(fields[0]) {
		argIndex--
		prefix = token.text
	}
	return command, argIndex, prefix, true
}

// splitInputFields mirrors slash-command token semantics closely enough to
// determine the current argument index while preserving spaces inside quotes.
func splitInputFields(value string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	tokenStarted := false
	escaped := false
	for _, r := range value {
		if escaped {
			b.WriteRune(r)
			tokenStarted = true
			escaped = false
			continue
		}
		if quote != 0 {
			switch r {
			case '\\':
				escaped = true
			case quote:
				quote = 0
				tokenStarted = true
			default:
				b.WriteRune(r)
				tokenStarted = true
			}
			continue
		}
		switch {
		case (r == '"' || r == '\'') && b.Len() == 0:
			quote = r
			tokenStarted = true
		case unicode.IsSpace(r):
			if tokenStarted {
				fields = append(fields, b.String())
				b.Reset()
				tokenStarted = false
			}
		default:
			b.WriteRune(r)
			tokenStarted = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if tokenStarted {
		fields = append(fields, b.String())
	}
	return fields
}

func (m *InputModel) acceptArgumentSuggestion(option string) {
	value := m.textarea.Value()
	if token, ok := trailingInputToken(value); ok && token.start > strings.IndexFunc(value, unicode.IsSpace) {
		value = value[:token.start] + option + " "
	} else {
		value += option + " "
	}
	m.textarea.SetValue(value)
	m.textarea.CursorEnd()
	m.syncTextareaHeight()
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.showSuggestions = false
	m.suggestionIndex = 0
	m.ghostText = ""
	m.suggestionNotice = ""
	// Chain completion for commands such as `/set <key> <on|off>`.
	m.updateArgumentSuggestions(value)
}

func (m *InputModel) updateGhostText(prefix string) {
	if !m.ghostEnabled {
		m.ghostText = ""
		return
	}

	if m.suggestionType == SuggestionCommand && len(m.suggestions) > 0 {
		topCmd := m.suggestions[0]
		cmdLower := strings.ToLower(topCmd.Name)
		prefixLower := strings.ToLower(prefix)

		// Only show ghost text for prefix matches (not fuzzy matches)
		if strings.HasPrefix(cmdLower, prefixLower) && len(topCmd.Name) > len(prefix) {
			m.ghostText = topCmd.Name[len(prefix):]
		} else {
			m.ghostText = ""
		}
	} else if m.suggestionType == SuggestionFile && len(m.fileSuggestions) > 0 {
		top := m.fileSuggestions[0]
		rel, err := filepath.Rel(m.workDir, top)
		if err == nil {
			token, ok := trailingInputToken(prefix)
			if ok {
				lastWord := token.text
				if strings.HasPrefix(rel, lastWord) && len(rel) > len(lastWord) {
					m.ghostText = rel[len(lastWord):]
				} else {
					m.ghostText = ""
				}
			}
		}
	} else {
		m.ghostText = ""
	}
}

// shouldSuggestFiles returns true if the input looks like a path or command argument.
func (m InputModel) shouldSuggestFiles(input string) bool {
	if input == "" {
		return false
	}

	if command, argIndex, prefix, ok := m.commandArgumentContext(input); ok && argIndex >= 0 && argIndex < len(command.Args) {
		arg := command.Args[argIndex]
		if arg.Type == "path" {
			return true
		}
		// Optional flags do not consume a positional slot when omitted. If the
		// typed value is not a flag and the following argument is a path, treat
		// it as that path (e.g. /add-dir <path> without --persist).
		if arg.Type == "option" && !strings.HasPrefix(prefix, "-") && argIndex+1 < len(command.Args) && command.Args[argIndex+1].Type == "path" {
			return true
		}
	}

	// Suggest if current word looks like a path (contains /, ., or is part of a known path).
	token, ok := trailingInputToken(input)
	if !ok {
		return false
	}
	lastWord := token.text
	return len(lastWord) >= 2 && (strings.Contains(lastWord, "/") || strings.Contains(lastWord, "."))
}

// updateFileSuggestions finds file matches for the current input.
// updateAtFileSuggestions globs file matches for an @-reference word (e.g.
// "@src/ma"), mirroring updateFileSuggestions but on the @-stripped prefix. The
// matches are stored as absolute paths; acceptFileSuggestion re-adds the @.
func (m *InputModel) updateAtFileSuggestions(word string) {
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.suggestionType = SuggestionAtFile
	m.suggestionNotice = ""
	if !m.fileSuggestionsReady() {
		m.showSuggestions = false
		m.fileSuggestions = nil
		m.ghostText = ""
		return
	}
	prefix := strings.TrimPrefix(word, "@")

	matches, _ := filepath.Glob(filepath.Join(m.workDir, prefix+"*"))
	if len(matches) == 0 && !filepath.IsAbs(prefix) {
		matches = m.searchFilesFuzzy(prefix)
	}

	m.fileSuggestions = matches
	m.suggestionIndex = 0
	m.showSuggestions = len(m.fileSuggestions) > 0
	if !m.showSuggestions {
		m.suggestionNotice = fmt.Sprintf("No files match %q", word)
	}

	if len(m.fileSuggestions) > 0 {
		rel, err := filepath.Rel(m.workDir, m.fileSuggestions[0])
		if err == nil && strings.HasPrefix(rel, prefix) && len(rel) > len(prefix) {
			m.ghostText = rel[len(prefix):]
		} else {
			m.ghostText = ""
		}
	} else {
		m.ghostText = ""
	}
}

func (m *InputModel) updateFileSuggestions(input string) {
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.suggestionType = SuggestionFile
	m.suggestionNotice = ""
	if !m.fileSuggestionsReady() {
		m.showSuggestions = false
		m.fileSuggestions = nil
		m.ghostText = ""
		return
	}

	token, ok := trailingInputToken(input)
	if !ok {
		last, _ := utf8.DecodeLastRuneInString(input)
		if !unicode.IsSpace(last) {
			m.showSuggestions = false
			m.fileSuggestions = nil
			m.ghostText = ""
			return
		}
		// A just-entered path argument should reveal the top-level choices
		// immediately instead of requiring an arbitrary first character.
		token = inputToken{start: len(input)}
	}
	lastWord := token.text

	// Simple glob matching for now
	pattern := lastWord + "*"
	if !filepath.IsAbs(lastWord) {
		pattern = filepath.Join(m.workDir, pattern)
	}
	matches, _ := filepath.Glob(pattern)

	// Fallback: search in current dir if not absolute
	if len(matches) == 0 && !filepath.IsAbs(lastWord) {
		matches = m.searchFilesFuzzy(lastWord)
	}

	m.fileSuggestions = matches
	m.suggestionIndex = 0
	m.showSuggestions = len(m.fileSuggestions) > 0
	if !m.showSuggestions {
		m.suggestionNotice = fmt.Sprintf("No files match %q", lastWord)
	}

	// Update ghost text for files
	if len(m.fileSuggestions) > 0 {
		top := m.fileSuggestions[0]
		rel, err := filepath.Rel(m.workDir, top)
		if err == nil {
			if strings.HasPrefix(rel, lastWord) && len(rel) > len(lastWord) {
				m.ghostText = rel[len(lastWord):]
			} else {
				m.ghostText = ""
			}
		}
	} else {
		m.ghostText = ""
	}
}

func (m *InputModel) fileSuggestionsReady() bool {
	if strings.TrimSpace(m.workDir) == "" {
		m.suggestionNotice = "File suggestions unavailable · no working directory"
		return false
	}
	info, err := os.Stat(m.workDir)
	if err != nil || !info.IsDir() {
		m.suggestionNotice = "File suggestions unavailable · check working directory"
		return false
	}
	return true
}

// searchFilesFuzzy performs a shallow search for files matching a query.
func (m InputModel) searchFilesFuzzy(query string) []string {
	var results []string
	maxResults := 10

	filepath.Walk(m.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || len(results) >= maxResults {
			return filepath.SkipDir
		}
		// Skip hidden dirs
		if info.IsDir() && strings.HasPrefix(info.Name(), ".") && info.Name() != "." {
			return filepath.SkipDir
		}

		rel, _ := filepath.Rel(m.workDir, path)
		if fuzzyScore(rel, query) > 0 {
			results = append(results, path)
		}
		return nil
	})

	return results
}

// View renders the input component.
func (m InputModel) View() string {
	var result strings.Builder
	auxiliaryRows := m.auxiliaryRowBudget()

	// Show history search mode
	if m.historySearchMode {
		if search := m.renderHistorySearch(auxiliaryRows); search != "" {
			result.WriteString(search)
			result.WriteString("\n")
		}
	}

	// Show autocomplete suggestions. Each suggestion kind owns a separate slice
	// so command metadata, argument options, and filesystem paths cannot be
	// mistaken for one another during rendering or acceptance.
	if m.showSuggestions && (len(m.suggestions) > 0 || len(m.argSuggestions) > 0 || len(m.fileSuggestions) > 0) {
		suggestionBox := m.renderSuggestionsWithin(auxiliaryRows)
		if suggestionBox != "" {
			result.WriteString(suggestionBox)
			result.WriteString("\n")
		}
	} else if m.suggestionNotice != "" {
		if notice := m.renderSuggestionNoticeWithin(auxiliaryRows); notice != "" {
			result.WriteString(notice)
			result.WriteString("\n")
		}
	}

	// Show argument hints after command
	if m.showArgHints && m.currentCommand != nil && len(m.currentCommand.Args) > 0 {
		argHints := m.renderArgHints()
		result.WriteString(argHints)
		result.WriteString("\n")
	}

	// Input field with ghost text
	inputView := m.textarea.View()
	if m.ghostText != "" && m.ghostEnabled && !m.showSuggestions {
		// Render ghost text inline (after input)
		ghostStyle := lipgloss.NewStyle().Foreground(ColorDim)
		inputView = inputView + ghostStyle.Render(m.ghostText)
	}
	result.WriteString(m.styles.Input.Render(inputView))

	return result.String()
}

func (m InputModel) renderHistorySearch(maxRows int) string {
	if maxRows == 0 {
		return ""
	}
	searchStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true)
	queryStyle := lipgloss.NewStyle().Foreground(ColorText)
	resultStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	width := max(m.textarea.Width(), 1)

	query := m.historySearchQuery
	if query == "" {
		query = "Type to search previous messages"
	}
	header := searchStyle.Render(truncateForWidth("History search · "+query, width))
	main := "Start typing to filter"
	mainStyle := queryStyle
	if m.historySearchResult != "" {
		main = "Match · " + safeKeyEntryText(m.historySearchResult)
		mainStyle = resultStyle
	} else if m.historySearchQuery != "" {
		main = "No matching history"
		mainStyle = resultStyle
	}
	main = truncateForWidth(main, width)

	var footerVariants []string
	switch {
	case m.historySearchResult != "":
		footer := "Esc cancel · Enter use"
		if m.historySearchCanSearchOlder() {
			footer += " · Ctrl+R older"
			footerVariants = append(footerVariants, footer, "Esc · Enter · Ctrl+R")
		} else {
			footerVariants = append(footerVariants, footer)
		}
		footerVariants = append(footerVariants, "Esc · Enter")
	case m.historySearchQuery != "":
		footerVariants = []string{"Esc cancel · Keep typing", "Esc · Type"}
	default:
		footer := "Esc cancel"
		if len(m.history) > 0 {
			footer += " · Ctrl+R newest"
		}
		footer += " · Type to filter"
		footerVariants = []string{footer, "Esc · Type"}
	}
	footerVariants = append(footerVariants, "Esc", "⎋")
	footer := completionFooter(width, footerVariants...)

	if maxRows == 1 {
		return resultStyle.Render(truncateMiddleForWidth(ansi.Strip(mainStyle.Render(main))+" · "+footer, width))
	}
	if maxRows == 2 {
		return mainStyle.Render(main) + "\n" + resultStyle.Render(footer)
	}
	return strings.Join([]string{header, mainStyle.Render(main), resultStyle.Render(footer)}, "\n")
}

// historySearchSurfaceReadable keeps reverse-search from becoming an invisible
// keyboard mode in a cropped composer. Two rows keep both the current result
// and its recovery/actions visible; the compact footer fits from 11 columns.
func (m InputModel) historySearchSurfaceReadable() bool {
	rows := m.auxiliaryRowBudget()
	if rows < 0 {
		return true
	}
	return rows >= 2 && m.textarea.Width() >= lipgloss.Width("Esc · Enter")
}

// renderArgHints renders argument hints for the current command.
func (m InputModel) renderArgHints() string {
	if m.currentCommand == nil || len(m.currentCommand.Args) == 0 {
		return ""
	}

	var builder strings.Builder

	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	reqStyle := lipgloss.NewStyle().Foreground(ColorWarning) // Amber for required
	optStyle := lipgloss.NewStyle().Foreground(ColorMuted)   // Gray for optional

	builder.WriteString(hintStyle.Render("  Usage: "))
	if usage := strings.TrimSpace(m.currentCommand.Usage); usage != "" {
		// Usage is the only representation that can faithfully express flags,
		// alternatives, and mixed optional/required ordering. ArgInfo remains the
		// structured source for completion, while this is the human-readable hint.
		// Two compact lines keep safety flags discoverable on narrow terminals;
		// the former one-line truncate often hid the only destructive/recovery
		// option at the end of a command's usage.
		return renderUsageHint(usage, max(m.textarea.Width(), 1), hintStyle)
	}

	builder.WriteString(hintStyle.Render("/" + m.currentCommand.Name + " "))

	for i, arg := range m.currentCommand.Args {
		if i > 0 {
			builder.WriteString(" ")
		}

		if arg.Required {
			builder.WriteString(reqStyle.Render("<" + arg.Name + ">"))
		} else {
			builder.WriteString(optStyle.Render("[" + arg.Name + "]"))
		}
	}

	return ansi.Truncate(builder.String(), max(m.textarea.Width(), 1), "…")
}

func renderUsageHint(usage string, width int, style lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	words := strings.Fields(usage)
	if len(words) == 0 {
		return ""
	}

	firstPrefix := "  Usage: "
	continuationPrefix := "  ↳ "
	if width < lipgloss.Width(firstPrefix)+4 {
		firstPrefix = "U "
	}
	if width <= lipgloss.Width(continuationPrefix) {
		continuationPrefix = " "
	}

	const maxLines = 2
	lines := make([]string, 0, maxLines)
	consumed := 0
	for lineIndex := 0; lineIndex < maxLines && consumed < len(words); lineIndex++ {
		prefix := continuationPrefix
		if lineIndex == 0 {
			prefix = firstPrefix
		}
		budget := max(width-lipgloss.Width(prefix), 1)
		content, used := takeUsageWords(words[consumed:], budget)
		if used == 0 {
			break
		}
		consumed += used
		if lineIndex == maxLines-1 && consumed < len(words) {
			content = truncateForWidth(strings.TrimSpace(content)+" …", budget)
		}
		lines = append(lines, style.Render(prefix+content))
	}
	return strings.Join(lines, "\n")
}

func takeUsageWords(words []string, width int) (string, int) {
	if len(words) == 0 || width <= 0 {
		return "", 0
	}
	var line strings.Builder
	used := 0
	for _, word := range words {
		separator := ""
		if used > 0 {
			separator = " "
		}
		candidate := line.String() + separator + word
		if lipgloss.Width(candidate) > width {
			if used == 0 {
				return truncateForWidth(word, width), 1
			}
			break
		}
		line.WriteString(separator)
		line.WriteString(word)
		used++
	}
	return line.String(), used
}

func (m InputModel) renderSuggestionNotice() string {
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)
	noticeStyle := lipgloss.NewStyle().Foreground(ColorMuted).Italic(true)
	footerStyle := lipgloss.NewStyle().Foreground(ColorDim)
	lineBudget := max(m.textarea.Width()-2, 1)
	notice := truncateForWidth(safeKeyEntryText(m.suggestionNotice), lineBudget)
	footer := truncateForWidth("Esc dismiss · Enter send anyway", lineBudget)
	if isUnavailablePromptNotice(m.suggestionNotice) || isSubmitCooldownNotice(m.suggestionNotice) {
		footer = truncateForWidth("Esc dismiss · Draft preserved", lineBudget)
	}
	return boxStyle.Render(noticeStyle.Render(notice) + "\n" + footerStyle.Render(footer))
}

func (m InputModel) renderSuggestionNoticeWithin(maxRows int) string {
	full := m.renderSuggestionNotice()
	if maxRows < 0 || lipgloss.Height(full) <= maxRows {
		return full
	}
	if maxRows == 0 {
		return ""
	}
	width := max(m.textarea.Width(), 1)
	notice := truncateForWidth(safeKeyEntryText(m.suggestionNotice), width)
	if maxRows == 1 {
		return lipgloss.NewStyle().Foreground(ColorMuted).Italic(true).Render(notice)
	}
	footer := "Esc dismiss · Enter send anyway"
	if isUnavailablePromptNotice(m.suggestionNotice) || isSubmitCooldownNotice(m.suggestionNotice) {
		footer = "Esc dismiss · Draft preserved"
	}
	return lipgloss.NewStyle().Foreground(ColorMuted).Italic(true).Render(notice) + "\n" +
		lipgloss.NewStyle().Foreground(ColorDim).Render(truncateForWidth(footer, width))
}

// renderSuggestions renders the autocomplete suggestion box.
func (m InputModel) renderSuggestions() string {
	return m.renderSuggestionsWithin(-1)
}

func (m InputModel) renderSuggestionsWithin(maxRows int) string {
	full := m.renderSuggestionBox()
	var rendered string
	if maxRows < 0 || lipgloss.Height(full) <= maxRows {
		rendered = full
	} else if maxRows == 0 {
		return ""
	} else {
		rendered = m.renderCompactSuggestions(maxRows)
	}
	if maxRows >= 0 && !suggestionRenderingHasActions(rendered, m.suggestionType) {
		return m.renderPausedSuggestions(maxRows)
	}
	return rendered
}

func (m InputModel) renderPausedSuggestions(maxRows int) string {
	if maxRows <= 0 {
		return ""
	}
	width := max(m.textarea.Width(), 1)
	style := lipgloss.NewStyle().Foreground(ColorDim)
	if maxRows == 1 {
		return style.Render(truncateMiddleForWidth("Suggestions paused · resize · Esc", width))
	}
	return style.Render(truncateForWidth("Suggestions paused", width)) + "\n" +
		style.Render(completionFooter(width, "Resize to complete · Esc close", "Resize · Esc", "Esc"))
}

func (m InputModel) renderSuggestionBox() string {
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)

	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary)

	normalStyle := lipgloss.NewStyle().
		Foreground(ColorText)

	descStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	var lines []string
	maxShow := 6

	// Bound each suggestion line to the input width so a long description or
	// path can't overflow a narrow terminal — the box has no fixed width, it
	// grows to its widest line. textarea width ≈ terminal − 4; leave a small
	// margin for the box border+padding. ≤0 ⇒ width unset, skip (let it wrap).
	lineBudget := m.textarea.Width() - 2

	if m.suggestionType == SuggestionCommand {
		if len(m.suggestions) < maxShow {
			maxShow = len(m.suggestions)
		}

		// Determine visible range
		start := 0
		if m.suggestionIndex >= maxShow {
			start = m.suggestionIndex - maxShow + 1
		}
		end := min(start+maxShow, len(m.suggestions))
		if indicator := suggestionOverflowIndicator("↑", start, lineBudget); indicator != "" {
			lines = append(lines, descStyle.Render(indicator))
		}

		for i := start; i < end; i++ {
			cmd := m.suggestions[i]
			style := normalStyle
			prefix := "  "
			if i == m.suggestionIndex {
				style = selectedStyle
				prefix = "> "
			}

			line := prefix + style.Render("/"+cmd.Name)
			// Category badge intentionally omitted — the description
			// already conveys the command's intent, and a vertical
			// column of `[Category]` brackets running down 6 lines of
			// suggestions made the eye jump between the brackets and
			// the prose. One signal per line.
			if cmd.Description != "" {
				desc := cmd.Description
				if lineBudget > 0 {
					if b := lineBudget - lipgloss.Width(prefix+"/"+cmd.Name) - 1; b > 0 {
						desc = truncateForWidth(desc, b)
					} else {
						desc = "" // no room for a description on this width
					}
				}
				if desc != "" {
					line += " " + descStyle.Render(desc)
				}
			}
			lines = append(lines, line)
		}

		if indicator := suggestionOverflowIndicator("↓", len(m.suggestions)-end, lineBudget); indicator != "" {
			lines = append(lines, descStyle.Render(indicator))
		}

		if m.suggestionIndex >= 0 && m.suggestionIndex < len(m.suggestions) {
			selected := m.suggestions[m.suggestionIndex]
			footer := completionFooter(lineBudget,
				"Enter/Tab complete · Esc close",
				"Enter/Tab · Esc",
			)
			lines = append(lines, descStyle.Render(footer))
			if selected.Usage != "" {
				usage := "Usage: " + strings.Join(strings.Fields(selected.Usage), " ")
				if lineBudget > 0 {
					usage = truncateForWidth(usage, lineBudget)
				}
				lines = append(lines, descStyle.Render(usage))
			}
		}
	} else if m.suggestionType == SuggestionArgument {
		if len(m.argSuggestions) < maxShow {
			maxShow = len(m.argSuggestions)
		}
		start := 0
		if m.suggestionIndex >= maxShow {
			start = m.suggestionIndex - maxShow + 1
		}
		end := min(start+maxShow, len(m.argSuggestions))
		if indicator := suggestionOverflowIndicator("↑", start, lineBudget); indicator != "" {
			lines = append(lines, descStyle.Render(indicator))
		}
		for i := start; i < end; i++ {
			style := normalStyle
			prefix := "  "
			if i == m.suggestionIndex {
				style = selectedStyle
				prefix = "> "
			}
			option := safeKeyEntryText(m.argSuggestions[i])
			if lineBudget > 0 {
				option = truncateForWidth(option, max(lineBudget-lipgloss.Width(prefix), 1))
			}
			lines = append(lines, prefix+style.Render(option))
		}
		if indicator := suggestionOverflowIndicator("↓", len(m.argSuggestions)-end, lineBudget); indicator != "" {
			lines = append(lines, descStyle.Render(indicator))
		}
		footer := completionFooter(lineBudget,
			argFooterHint(m.argNavigated),
			"Tab add · Enter run · Esc",
			"Tab/Enter · Esc",
		)
		lines = append(lines, descStyle.Render(footer))
	} else {
		// File suggestions
		if len(m.fileSuggestions) < maxShow {
			maxShow = len(m.fileSuggestions)
		}

		start := 0
		if m.suggestionIndex >= maxShow {
			start = m.suggestionIndex - maxShow + 1
		}
		end := min(start+maxShow, len(m.fileSuggestions))
		if indicator := suggestionOverflowIndicator("↑", start, lineBudget); indicator != "" {
			lines = append(lines, descStyle.Render(indicator))
		}

		for i := start; i < end; i++ {
			path := m.fileSuggestions[i]
			rel, _ := filepath.Rel(m.workDir, path)
			style := normalStyle
			prefix := "  "
			if i == m.suggestionIndex {
				style = selectedStyle
				prefix = "> "
			}

			// Path-aware shorten (keeps the filename) so deep paths don't
			// overflow a narrow terminal.
			if lineBudget > 0 {
				if b := lineBudget - lipgloss.Width(prefix); b > 0 {
					rel = shortenPath(rel, b)
				}
			}
			line := prefix + style.Render(rel)
			lines = append(lines, line)
		}

		if indicator := suggestionOverflowIndicator("↓", len(m.fileSuggestions)-end, lineBudget); indicator != "" {
			lines = append(lines, descStyle.Render(indicator))
		}
		lines = append(lines, descStyle.Render(completionFooter(lineBudget,
			"Enter/Tab insert path · Esc close",
			"Enter/Tab add · Esc",
			"Enter/Tab · Esc",
		)))
	}

	return boxStyle.Render(strings.Join(lines, "\n"))
}

// renderCompactSuggestions keeps the selected completion and its real actions
// visible when a bordered dropdown would be cropped by a short terminal. The
// current row is always included; neighboring rows use only leftover space.
func (m InputModel) renderCompactSuggestions(maxRows int) string {
	if maxRows <= 0 {
		return ""
	}
	width := max(m.textarea.Width(), 1)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorSecondary)
	normalStyle := lipgloss.NewStyle().Foreground(ColorText)
	footerStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var labels []string
	selected := m.suggestionIndex
	footer := "Enter/Tab complete · Esc close"
	switch m.suggestionType {
	case SuggestionCommand:
		for _, command := range m.suggestions {
			labels = append(labels, "/"+safeKeyEntryText(command.Name))
		}
	case SuggestionArgument:
		for _, option := range m.argSuggestions {
			labels = append(labels, safeKeyEntryText(option))
		}
		footer = "Tab complete · Enter run · Esc close"
	default:
		for _, path := range m.fileSuggestions {
			rel, err := filepath.Rel(m.workDir, path)
			if err != nil {
				rel = path
			}
			labels = append(labels, safeKeyEntryText(rel))
		}
		footer = "Enter/Tab insert · Esc close"
	}
	if len(labels) == 0 {
		return ""
	}
	selected = min(max(selected, 0), len(labels)-1)
	if maxRows == 1 {
		// One spare row is still enough for a truthful completion surface when
		// the selected identity and the real keys fit together. Otherwise show
		// only recovery guidance; accepting a cropped, unidentified target is
		// worse than temporarily pausing completion.
		actions := "Enter/Tab · Esc"
		if m.suggestionType == SuggestionArgument {
			actions = "Tab · Enter run · Esc"
		}
		prefix := "> "
		suffix := " · " + actions
		labelBudget := width - lipgloss.Width(prefix) - lipgloss.Width(suffix)
		if labelBudget < 4 {
			return footerStyle.Render(truncateMiddleForWidth("Suggestions paused · resize · Esc", width))
		}
		label := truncateMiddleForWidth(labels[selected], labelBudget)
		return prefix + selectedStyle.Render(label) + footerStyle.Render(suffix)
	}

	rowCapacity := maxRows
	rowCapacity-- // keep one row for actions/recovery
	rowCapacity = min(max(rowCapacity, 1), len(labels))
	start := min(max(selected-rowCapacity+1, 0), len(labels)-rowCapacity)
	end := start + rowCapacity
	lines := make([]string, 0, maxRows)
	for i := start; i < end; i++ {
		prefix := "  "
		style := normalStyle
		if i == selected {
			prefix = "> "
			style = selectedStyle
		}
		position := ""
		if len(labels) > 1 {
			position = fmt.Sprintf(" %d/%d", i+1, len(labels))
		}
		labelBudget := max(width-lipgloss.Width(prefix)-lipgloss.Width(position), 1)
		label := truncateMiddleForWidth(labels[i], labelBudget)
		lines = append(lines, prefix+style.Render(label)+footerStyle.Render(position))
	}
	if maxRows > 1 {
		lines = append(lines, footerStyle.Render(truncateForWidth(footer, width)))
	}
	return strings.Join(lines, "\n")
}

// suggestionActionsReadable reports whether the constrained composer actually
// renders both the selected target and the keys that will operate on it. An
// unconstrained reusable InputModel retains its container-owned behavior.
func (m InputModel) suggestionActionsReadable() bool {
	rows := m.auxiliaryRowBudget()
	if rows < 0 {
		return true
	}
	if rows == 0 {
		return false
	}
	return suggestionRenderingHasActions(m.renderSuggestionsWithin(rows), m.suggestionType)
}

func suggestionRenderingHasActions(rendered string, suggestionType SuggestionType) bool {
	plain := ansi.Strip(rendered)
	selectedVisible := false
	actionsVisible := false
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "> ") {
			selectedVisible = true
		}
		if strings.Contains(line, "Tab") && strings.Contains(line, "Enter") && strings.Contains(line, "Esc") &&
			(suggestionType != SuggestionArgument ||
				strings.Contains(line, "run") || strings.Contains(line, "select")) {
			// The argument footer has TWO honest states (v0.100.107 regression
			// fix): "Enter run as typed" before navigation and "Enter select"
			// after ↑/↓. Requiring the literal "run" branded the navigated
			// footer unreadable, so the NEXT arrow key closed the dropdown on
			// wide terminals (narrow ones happened to pick a fallback variant
			// containing "run" and kept working — the field-report shape).
			actionsVisible = true
		}
	}
	return selectedVisible && actionsVisible
}

func completionFooter(width int, variants ...string) string {
	if len(variants) == 0 {
		return ""
	}
	if width <= 0 {
		return variants[0]
	}
	for _, variant := range variants {
		if lipgloss.Width(variant) <= width {
			return variant
		}
	}
	return truncateForWidth(variants[len(variants)-1], width)
}

func suggestionOverflowIndicator(direction string, hidden, width int) string {
	if hidden <= 0 {
		return ""
	}
	indicator := fmt.Sprintf("%s %d more", direction, hidden)
	if width > 0 {
		indicator = truncateForWidth(indicator, width)
	}
	return indicator
}

// Value returns the current input value.
func (m InputModel) Value() string {
	return strings.TrimSpace(m.textarea.Value())
}

// --- Paste collapsing (Claude-Code-style) ---

const (
	pasteCollapseMinLines = 4   // ≥ this many lines → collapse
	pasteCollapseMinChars = 400 // …or a single line ≥ this many runes
)

// isLargePaste reports whether a bracketed paste is big enough to collapse into
// a chip rather than insert verbatim.
func isLargePaste(text string) bool {
	if strings.Count(text, "\n") >= pasteCollapseMinLines-1 {
		return true
	}
	return len([]rune(text)) >= pasteCollapseMinChars
}

// pasteUnit returns the singular/plural unit word for a count.
func pasteUnit(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}

// pasteDisplayLabel is the compact, entirely user-visible label shown in the
// textarea. It is deliberately not an expansion token: pasteSpan carries the
// out-of-band identity, so typing this exact label remains literal text.
func pasteDisplayLabel(id int, text string) string {
	if nl := strings.Count(text, "\n"); nl > 0 {
		lines := nl + 1
		return fmt.Sprintf("[Pasted text #%d +%d %s]", id, lines, pasteUnit(lines, "line"))
	}
	chars := len([]rune(text))
	return fmt.Sprintf("[Pasted text #%d +%d %s]", id, chars, pasteUnit(chars, "char"))
}

// collapsePaste stores a large paste and inserts its compact chip at the cursor.
func (m InputModel) collapsePaste(text string) InputModel {
	if m.pastes == nil {
		m.pastes = make(map[int]string)
	}
	m.pasteSeq++
	id := m.pasteSeq
	m.pastes[id] = text
	label := pasteDisplayLabel(id, text)
	before := m.textarea.Value()
	cursor := textareaCursorByteOffset(m.textarea)
	m.textarea.InsertString(label)
	after := m.textarea.Value()
	m.reconcilePasteEdit(before, after, cursor, cursor)
	if cursor+len(label) <= len(after) && after[cursor:cursor+len(label)] == label {
		m.pasteSpans = append(m.pasteSpans, pasteSpan{id: id, start: cursor, end: cursor + len(label)})
	} else {
		delete(m.pastes, id)
	}
	// The chip never begins a /command or @file, and we skip the normal
	// post-insert suggestion refresh by returning early — so clear any open
	// suggestion/ghost/arg-hint state here, else a dropdown that was showing
	// when the paste landed stays stale (and the next Enter could accept it
	// instead of submitting).
	m.showSuggestions = false
	m.suggestionNotice = ""
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.fileSuggestions = nil
	m.ghostText = ""
	m.showArgHints = false
	m.currentCommand = nil
	m.syncTextareaHeight()
	return m
}

// ExpandedValue returns the input value with every paste chip replaced by its
// stored content — the form sent to the model. Value() stays collapsed (used for
// rendering, the scrollback echo, and history). A chip whose id has no stored
// content (e.g. the user typed a lookalike) is left as-is.
func (m InputModel) ExpandedValue() string {
	raw := m.textarea.Value()
	leftTrimmed := strings.TrimLeftFunc(raw, unicode.IsSpace)
	trimStart := len(raw) - len(leftTrimmed)
	trimmed := strings.TrimRightFunc(leftTrimmed, unicode.IsSpace)
	trimEnd := trimStart + len(trimmed)
	if len(m.pastes) == 0 || len(m.pasteSpans) == 0 || trimStart == trimEnd {
		return trimmed
	}

	spans := clonePasteSpans(m.pasteSpans)
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var expanded strings.Builder
	cursor := trimStart
	for _, span := range spans {
		content, ok := m.pastes[span.id]
		label := pasteDisplayLabel(span.id, content)
		if !ok || span.start < cursor || span.start < trimStart || span.end > trimEnd ||
			span.start >= span.end || raw[span.start:span.end] != label {
			continue
		}
		expanded.WriteString(raw[cursor:span.start])
		expanded.WriteString(content)
		cursor = span.end
	}
	expanded.WriteString(raw[cursor:trimEnd])
	return expanded.String()
}

// Reset clears the input and optionally saves to history.
func (m *InputModel) Reset() {
	m.textarea.Reset()
	m.syncTextareaHeight() // Shrink back to single line after send
	m.historyIndex = -1
	m.savedInput = ""
	m.pastes = nil // drop collapsed-paste content for the next compose
	m.pasteSpans = nil
	m.savedPasteSpans = nil
	m.pasteSeq = 0
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.fileSuggestions = nil
	m.suggestionIndex = 0
	m.showSuggestions = false
	m.ghostText = ""
	m.suggestionNotice = ""
	m.showArgHints = false
	m.currentCommand = nil
	m.historySearchMode = false
	m.historySearchQuery = ""
	m.historySearchResult = ""
	m.historySearchIndex = -1

	// Fresh compose → next discovery tip. Cheap: one modulo + a string-header
	// copy, and the textarea only renders the placeholder while empty anyway.
	if m.placeholderTipsOn {
		m.placeholderTipIndex = (m.placeholderTipIndex + 1) % len(placeholderTips)
	}
	m.updatePlaceholder()
}

// InsertNewline adds a newline at the cursor position and grows the input height.
// Used for multi-line input with Alt+Enter.
func (m *InputModel) InsertNewline() {
	before := m.textarea.Value()
	cursor := textareaCursorByteOffset(m.textarea)
	m.textarea.InsertString("\n")
	m.reconcilePasteEdit(before, m.textarea.Value(), cursor, cursor)
	m.syncTextareaHeight()
	// A newline ends the token that produced autocomplete. Keeping the old
	// dropdown alive lets the same Alt+Enter event accept its highlighted row
	// after inserting the newline, replacing or extending the user's draft.
	m.showSuggestions = false
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.fileSuggestions = nil
	m.suggestionIndex = 0
	m.ghostText = ""
	m.suggestionNotice = ""
	m.showArgHints = false
	m.currentCommand = nil
}

// InsertFileReferences appends file-browser selections to the composer as
// @references. Existing draft text is preserved, paths inside workDir become
// concise relative references, and names containing spaces are quoted using
// the same rules as autocomplete.
func (m *InputModel) InsertFileReferences(paths []string, workDir string) int {
	seen := make(map[string]bool, len(paths))
	references := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			continue
		}
		clean := filepath.Clean(path)
		if clean == "." || seen[clean] {
			continue
		}
		seen[clean] = true
		if workDir != "" && filepath.IsAbs(clean) {
			if rel, err := filepath.Rel(workDir, clean); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				clean = rel
			}
		}
		clean = filepath.ToSlash(clean)
		references = append(references, formatAcceptedFilePath(clean, inputToken{text: "@"}))
	}
	if len(references) == 0 {
		return 0
	}

	value := m.textarea.Value()
	if value != "" {
		last, _ := utf8.DecodeLastRuneInString(value)
		if !unicode.IsSpace(last) {
			value += " "
		}
	}
	value += strings.Join(references, " ") + " "
	m.textarea.SetValue(value)
	m.textarea.CursorEnd()
	m.syncTextareaHeight()
	m.showSuggestions = false
	m.suggestionNotice = ""
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.fileSuggestions = nil
	m.ghostText = ""
	m.showArgHints = false
	m.currentCommand = nil
	return len(references)
}

// RestoreDraft returns rejected user input to an empty composer without
// clobbering anything typed since submission. Large content is collapsed back
// into a paste chip so a queue overflow cannot suddenly fill the whole screen.
func (m *InputModel) RestoreDraft(value string) bool {
	value = sanitizeHistoryEntry(value)
	if value == "" || m.Value() != "" {
		return false
	}
	m.Reset()
	if isLargePaste(value) {
		*m = m.collapsePaste(value)
	} else {
		m.textarea.SetValue(value)
		m.textarea.CursorEnd()
		m.syncTextareaHeight()
	}
	return true
}

// Height returns the current input height in lines.
func (m InputModel) Height() int {
	return m.textarea.Height()
}

func (m *InputModel) syncTextareaHeight() {
	desired := min(strings.Count(m.textarea.Value(), "\n")+1, maxComposerLines)
	if m.historySearchMode {
		desired = 1
	}
	if m.viewportHeight > 0 {
		// Input border consumes two rows and the status bar owns the final row.
		// The textarea then scrolls internally, keeping the cursor visible rather
		// than letting the frame compositor cut off the top of the composer.
		desired = min(desired, max(m.viewportHeight-3, 1))
	}
	desired = max(desired, 1)
	if m.textarea.Height() != desired {
		m.textarea.SetHeight(desired)
	}
}

func (m InputModel) auxiliaryRowBudget() int {
	if m.viewportHeight <= 0 {
		return -1
	}
	inputRows := lipgloss.Height(m.styles.Input.Render(m.textarea.View()))
	return max(m.viewportHeight-inputRows-1, 0)
}

// AddToHistory adds a command to the history.
func (m *InputModel) AddToHistory(cmd string) {
	cmd = sanitizeHistoryEntry(cmd)
	if cmd == "" || len(cmd) > maxInputHistoryEntryBytes {
		return
	}

	// Don't add duplicates of the last entry
	if len(m.history) > 0 && m.history[len(m.history)-1] == cmd {
		return
	}

	m.history = append(m.history, cmd)

	m.history = boundInputHistory(m.history)
}

func sanitizeHistoryEntry(entry string) string {
	entry = ansi.Strip(entry)
	entry = strings.ReplaceAll(entry, "\r\n", "\n")
	entry = strings.Map(func(r rune) rune {
		switch r {
		case '\n':
			return r
		case '\r', '\t':
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, entry)
	return strings.TrimSpace(entry)
}

// SetWidth sets the input width.
func (m *InputModel) SetWidth(width int) {
	m.textarea.SetWidth(max(width-4, 1)) // Account for border padding, minimum 1
	if m.historySearchMode && !m.historySearchSurfaceReadable() {
		m.leaveHistorySearch()
		m.syncTextareaHeight()
	}
}

// SetWorkDir updates the workspace used by @file and path completion. The
// main TUI is constructed before Builder wires its real working directory, so
// keeping this only on Model leaves production autocomplete disconnected even
// though standalone InputModel tests work.
func (m *InputModel) SetWorkDir(workDir string) {
	if m.workDir == workDir {
		return
	}
	m.workDir = workDir
	// Any open filesystem suggestions belong to the previous workspace.
	if m.suggestionType == SuggestionFile || m.suggestionType == SuggestionAtFile {
		m.showSuggestions = false
		m.fileSuggestions = nil
		m.suggestionIndex = 0
		m.ghostText = ""
		m.suggestionNotice = ""
	}
}

// SetViewportHeight gives the main composer a vertical budget. Reusable prompt
// inputs leave it unset and retain their container-owned sizing behavior.
func (m *InputModel) SetViewportHeight(height int) {
	m.viewportHeight = max(height, 0)
	m.syncTextareaHeight()
	if m.historySearchMode && !m.historySearchSurfaceReadable() {
		m.leaveHistorySearch()
		m.syncTextareaHeight()
	}
}

// Focus focuses the input.
func (m *InputModel) Focus() tea.Cmd {
	return m.textarea.Focus()
}

// Blur unfocuses the input.
func (m *InputModel) Blur() {
	m.textarea.Blur()
}

// Focused returns whether the input is focused.
func (m InputModel) Focused() bool {
	return m.textarea.Focused()
}

// GetHistory returns the current history slice.
func (m *InputModel) GetHistory() []string {
	return append([]string(nil), m.history...)
}

// SetHistory sets the history from an external source.
func (m *InputModel) SetHistory(history []string) {
	m.history = make([]string, 0, min(len(history), maxHistorySize))
	for _, entry := range history {
		if entry = sanitizeHistoryEntry(entry); entry != "" && len(entry) <= maxInputHistoryEntryBytes {
			m.history = append(m.history, entry)
		}
	}
	m.history = boundInputHistory(m.history)
	m.historyIndex = -1
	m.savedInput = ""
	m.savedPasteSpans = nil
}

// boundInputHistory retains the newest contiguous suffix that can always be
// serialized within both the entry-count and durable file-size limits.
func boundInputHistory(history []string) []string {
	start := len(history)
	totalBytes := 0
	for start > 0 && len(history)-start < maxHistorySize {
		recordBytes := len("q:") + len(strconv.Quote(history[start-1])) + 1
		if recordBytes > maxInputHistoryRecordBytes || totalBytes > maxInputHistoryFileBytes-recordBytes {
			break
		}
		totalBytes += recordBytes
		start--
	}
	return append([]string(nil), history[start:]...)
}

// LoadHistory loads command history from file.
func (m *InputModel) LoadHistory() error {
	histPath, err := getHistoryPath()
	if err != nil {
		return err
	}

	data, err := readPrivateHistoryFile(histPath, maxInputHistoryFileBytes)
	if err != nil {
		// errors.Is, not os.IsNotExist: the history reader wraps its errors and
		// os.IsNotExist cannot unwrap them, so this "no history yet" branch was
		// dead and a fresh install logged a spurious failure.
		if errors.Is(err, os.ErrNotExist) {
			return nil // No history file yet, that's fine
		}
		return err
	}
	var history []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), maxInputHistoryRecordBytes)
	records := 0
	for scanner.Scan() {
		records++
		if records > maxInputHistoryRecords {
			return fmt.Errorf("input history exceeds %d-record limit", maxInputHistoryRecords)
		}
		line := scanner.Text()
		if encoded, ok := strings.CutPrefix(line, "q:"); ok {
			if unquoted, unquoteErr := strconv.Unquote(encoded); unquoteErr == nil {
				line = unquoted
			}
		}
		if entry := sanitizeHistoryEntry(line); len(entry) > maxInputHistoryEntryBytes {
			return fmt.Errorf("input history entry exceeds %d-byte limit", maxInputHistoryEntryBytes)
		} else if entry != "" {
			history = append(history, entry)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Keep only the last maxHistorySize entries
	if len(history) > maxHistorySize {
		history = history[len(history)-maxHistorySize:]
	}

	m.SetHistory(history)
	return nil
}

// SaveHistory saves command history to file.
func (m *InputModel) SaveHistory() error {
	histPath, err := getHistoryPath()
	if err != nil {
		return err
	}

	var data strings.Builder
	for _, cmd := range m.history {
		entry := sanitizeHistoryEntry(cmd)
		if len(entry) > maxInputHistoryEntryBytes {
			return fmt.Errorf("input history entry exceeds %d-byte limit", maxInputHistoryEntryBytes)
		}
		record := "q:" + strconv.Quote(entry) + "\n"
		if len(record) > maxInputHistoryRecordBytes {
			return fmt.Errorf("encoded input history entry exceeds %d-byte limit", maxInputHistoryRecordBytes)
		}
		if data.Len() > maxInputHistoryFileBytes-len(record) {
			return fmt.Errorf("input history exceeds %d-byte limit", maxInputHistoryFileBytes)
		}
		data.WriteString(record)
	}
	return writePrivateHistoryFile(histPath, []byte(data.String()), maxInputHistoryFileBytes)
}

// getHistoryPath returns the path to the history file.
func getHistoryPath() (string, error) {
	// Use XDG_DATA_HOME or fallback to ~/.local/share
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}

	return filepath.Join(dataDir, "gokin", historyFile), nil
}

// SetActiveTask sets the active task name for placeholder context.
func (m *InputModel) SetActiveTask(task string) {
	m.activeTask = task
	m.updatePlaceholder()
}

// SetCommandAliases wires the alias map from commands.Handler into the
// autocomplete ranker. Called once from App during wiring; thread-safe only
// when invoked before any goroutine dispatches UI events on this model.
func (m *InputModel) SetCommandAliases(aliases map[string]string) {
	if aliases == nil {
		m.commandAliases = nil
		return
	}
	out := make(map[string]string, len(aliases))
	for k, v := range aliases {
		out[strings.ToLower(k)] = strings.ToLower(v)
	}
	m.commandAliases = out
}

// RecordRecentCommand pushes `name` to the front of the recent-commands list
// (dedup, cap at 5). Called from the App after each successful command
// dispatch so autocomplete can favour commands the user actually runs.
//
// Builds the new slice in a fresh allocation rather than filtering in place —
// this is the one place where a concurrent reader could otherwise observe a
// mid-state corruption of the backing array. With a fresh slice the swap is a
// single slice-header assignment: readers see either the old or the new list.
func (m *InputModel) RecordRecentCommand(name string) {
	if name == "" {
		return
	}
	const maxRecent = 5
	next := make([]string, 0, maxRecent)
	next = append(next, name)
	for _, n := range m.recentCommands {
		if n == name || len(next) >= maxRecent {
			continue
		}
		next = append(next, n)
	}
	m.recentCommands = next
}

// SetPlaceholder sets the placeholder text for the input.
func (m *InputModel) SetPlaceholder(placeholder string) {
	m.textarea.Placeholder = placeholder
}

// SetHintsEnabled controls rotating discovery tips in the empty composer.
// Active-task context remains visible regardless of this preference.
func (m *InputModel) SetHintsEnabled(enabled bool) {
	m.placeholderTipsOn = enabled
	m.updatePlaceholder()
}

// updatePlaceholder updates the placeholder text based on context.
func (m *InputModel) updatePlaceholder() {
	if m.activeTask != "" {
		m.textarea.Placeholder = "Continue: " + m.activeTask
	} else if !m.placeholderTipsOn {
		m.textarea.Placeholder = placeholderTips[0]
	} else {
		m.textarea.Placeholder = placeholderTips[m.placeholderTipIndex%len(placeholderTips)]
	}
}

// SetCommands sets the available commands for autocomplete.
func (m *InputModel) SetCommands(commands []CommandInfo) {
	m.commands = make([]CommandInfo, len(commands))
	for i := range commands {
		m.commands[i] = cloneCommandInfo(commands[i])
	}
	m.suggestions = nil
	m.argSuggestions = nil
	m.suggestionArg = ""
	m.showSuggestions = false
	m.ghostText = ""
	m.suggestionNotice = ""
	if value := m.textarea.Value(); strings.HasPrefix(value, "/") && !strings.Contains(value, " ") {
		m.updateSuggestions(value)
	}
}

// AddCommand adds a single command to the autocomplete list.
func (m *InputModel) AddCommand(cmd CommandInfo) {
	cmd = cloneCommandInfo(cmd)
	// Check if command already exists
	for i, existing := range m.commands {
		if existing.Name == cmd.Name {
			m.commands[i] = cmd
			m.refreshCommandSuggestions()
			return
		}
	}
	m.commands = append(m.commands, cmd)
	m.refreshCommandSuggestions()
}

func (m *InputModel) refreshCommandSuggestions() {
	if value := m.textarea.Value(); strings.HasPrefix(value, "/") && !strings.Contains(value, " ") {
		m.updateSuggestions(value)
	}
}

func cloneCommandInfo(cmd CommandInfo) CommandInfo {
	clone := cmd
	clone.Args = make([]ArgInfo, len(cmd.Args))
	for i := range cmd.Args {
		clone.Args[i] = cmd.Args[i]
		clone.Args[i].Options = append([]string(nil), cmd.Args[i].Options...)
		if cmd.Args[i].OptionsByPrevious != nil {
			clone.Args[i].OptionsByPrevious = make(map[string][]string, len(cmd.Args[i].OptionsByPrevious))
			for key, options := range cmd.Args[i].OptionsByPrevious {
				clone.Args[i].OptionsByPrevious[key] = append([]string(nil), options...)
			}
		}
	}
	return clone
}

// IsHistorySearchMode returns whether history search mode is active.
func (m *InputModel) IsHistorySearchMode() bool {
	return m.historySearchMode
}

// ShowingSuggestions returns whether suggestions are being shown.
func (m *InputModel) ShowingSuggestions() bool {
	return m.showSuggestions
}

// SuggestionsBlockSubmit reports whether Enter belongs to the dropdown. Command
// and file completion accept Enter; argument options intentionally require Tab
// so a highlighted destructive flag such as --force is never inserted by an
// ordinary submit keystroke.
func (m *InputModel) SuggestionsBlockSubmit() bool {
	if !m.showSuggestions || !m.suggestionActionsReadable() {
		return false
	}
	if m.suggestionType == SuggestionArgument {
		// A bare Enter after typing stays submit-as-typed; once the user
		// navigated the highlight (↑/↓), Enter belongs to the dropdown and
		// accepts the selected option (v0.100.106).
		return m.argNavigated
	}
	return true
}

// acceptFileSuggestion replaces the last word with the selected file path.
func (m *InputModel) acceptFileSuggestion(path string) {
	value := m.textarea.Value()
	token, ok := trailingInputToken(value)
	if !ok {
		last, _ := utf8.DecodeLastRuneInString(value)
		if !unicode.IsSpace(last) {
			return
		}
		token = inputToken{start: len(value)}
	}

	// Suggestions are stored as absolute paths. Keep them absolute when the
	// user started an absolute path (important for /add-dir outside workDir),
	// otherwise insert the concise workspace-relative form.
	typedPath := strings.TrimPrefix(token.text, "@")
	if !filepath.IsAbs(typedPath) && filepath.IsAbs(path) {
		if rel, err := filepath.Rel(m.workDir, path); err == nil {
			path = rel
		}
	}

	replacement := formatAcceptedFilePath(path, token)
	m.textarea.SetValue(value[:token.start] + replacement + " ")
	m.textarea.CursorEnd()
	m.syncTextareaHeight()
	m.showSuggestions = false
	m.fileSuggestions = nil
	m.ghostText = ""
	m.suggestionNotice = ""
}

func formatAcceptedFilePath(path string, token inputToken) string {
	prefix := ""
	if strings.HasPrefix(token.text, "@") {
		prefix = "@"
		path = strings.TrimPrefix(path, "@")
	}

	if token.quote != 0 {
		return prefix + string(token.quote) + escapeQuotedInputPath(path, token.quote) + string(token.quote)
	}
	if strings.ContainsAny(path, " \t\r\n") {
		return prefix + `"` + escapeQuotedInputPath(path, '"') + `"`
	}
	return prefix + path
}

func escapeQuotedInputPath(path string, quote rune) string {
	var b strings.Builder
	for _, r := range path {
		if r == quote || r == '\\' {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// argFooterHint tells the truth about what Enter does in the argument
// dropdown: selection only after explicit ↑/↓ navigation.
func argFooterHint(navigated bool) string {
	if navigated {
		return "Enter select · Tab complete · Esc close"
	}
	return "Tab complete · ↑/↓ then Enter select · Enter run as typed · Esc close"
}
