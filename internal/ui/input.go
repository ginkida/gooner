package ui

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	maxHistorySize = 100
	historyFile    = "input_history"
)

// ArgInfo describes a command argument for autocomplete hints.
type ArgInfo struct {
	Name     string   // Argument name (e.g., "message", "filename")
	Required bool     // True if argument is required
	Type     string   // "string", "path", "option", "number"
	Options  []string // Available options for "option" type
}

// CommandInfo contains information about a slash command for autocomplete.
type CommandInfo struct {
	Name        string
	Description string
	Category    string
	Args        []ArgInfo // Command arguments
	Usage       string    // Usage pattern (e.g., "/copy <n> [filename]")
}

// InputModel represents the input component.
type InputModel struct {
	textarea     textarea.Model
	styles       *Styles
	history      []string // Command history
	historyIndex int      // Current position in history (-1 = new input)
	savedInput   string   // Saved current input when browsing history

	// Autocomplete
	commands        []CommandInfo
	suggestions     []CommandInfo
	suggestionIndex int
	showSuggestions bool

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
}

// NewInputModel creates a new input model.
func NewInputModel(styles *Styles) InputModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message or /command... (Tab to complete)"
	ta.Focus()
	ta.CharLimit = 10000
	ta.ShowLineNumbers = false
	ta.SetHeight(1)

	return InputModel{
		textarea:           ta,
		styles:             styles,
		history:            make([]string, 0, maxHistorySize),
		historyIndex:       -1,
		savedInput:         "",
		commands:           defaultCommands(),
		suggestions:        nil,
		suggestionIndex:    0,
		showSuggestions:    false,
		ghostText:          "",
		ghostEnabled:       true, // Enable ghost text by default
		historySearchIndex: -1,
	}
}

// defaultCommands returns the default list of slash commands.
func defaultCommands() []CommandInfo {
	return []CommandInfo{
		// Session commands
		{Name: "help", Description: "Show help for commands", Category: "Session"},
		{Name: "clear", Description: "Clear conversation history", Category: "Session"},
		{Name: "compact", Description: "Force context compaction", Category: "Session"},
		{
			Name:        "save",
			Description: "Save current session",
			Category:    "Session",
			Args:        []ArgInfo{{Name: "name", Required: false, Type: "string"}},
			Usage:       "/save [name]",
		},
		{
			Name:        "resume",
			Description: "Resume a saved session",
			Category:    "Session",
			Args:        []ArgInfo{{Name: "session", Required: true, Type: "string"}},
			Usage:       "/resume <session>",
		},
		{Name: "sessions", Description: "List saved sessions", Category: "Session"},

		// History commands
		{Name: "undo", Description: "Undo the last file change", Category: "History"},
		{
			Name:        "checkpoint",
			Description: "Create a checkpoint",
			Category:    "History",
			Args:        []ArgInfo{{Name: "name", Required: false, Type: "string"}},
			Usage:       "/checkpoint [name]",
		},
		{Name: "checkpoints", Description: "List all checkpoints", Category: "History"},
		{
			Name:        "restore",
			Description: "Restore a checkpoint",
			Category:    "History",
			Args:        []ArgInfo{{Name: "checkpoint", Required: true, Type: "string"}},
			Usage:       "/restore <checkpoint>",
		},

		// Git commands
		{Name: "init", Description: "Initialize GOKIN.md", Category: "Git"},
		{
			Name:        "commit",
			Description: "Commit changes",
			Category:    "Git",
			Args: []ArgInfo{
				{Name: "message", Required: true, Type: "string"},
				{Name: "--amend", Required: false, Type: "option"},
			},
			Usage: "/commit <message> [--amend]",
		},

		// Auth commands
		{Name: "login", Description: "Login with API key or OAuth", Category: "Auth"},
		{Name: "logout", Description: "Clear stored credentials", Category: "Auth"},
		{Name: "auth-status", Description: "Show authentication status", Category: "Auth"},

		// Utility commands
		{Name: "doctor", Description: "Check environment", Category: "Utility"},
		{Name: "config", Description: "Show configuration", Category: "Utility"},
		{Name: "cost", Description: "Show token usage", Category: "Utility"},
		{
			Name:        "copy",
			Description: "Copy code block to clipboard",
			Category:    "Utility",
			Args: []ArgInfo{
				{Name: "n", Required: false, Type: "number"},
				{Name: "filename", Required: false, Type: "path"},
			},
			Usage: "/copy [n] [filename]",
		},
		{
			Name:        "browse",
			Description: "Open file browser",
			Category:    "Utility",
			Args:        []ArgInfo{{Name: "path", Required: false, Type: "path"}},
			Usage:       "/browse [path]",
		},
	}
}

// Init initializes the input model.
func (m InputModel) Init() tea.Cmd {
	return textarea.Blink
}

// Update handles input events.
func (m InputModel) Update(msg tea.Msg) (InputModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle history search mode
		if m.historySearchMode {
			return m.handleHistorySearch(msg)
		}

		switch msg.Type {
		case tea.KeyCtrlR:
			// Enter history search mode
			m.historySearchMode = true
			m.historySearchQuery = ""
			m.historySearchResult = ""
			m.historySearchIndex = len(m.history)
			return m, nil

		case tea.KeyTab:
			// Accept ghost text if visible (and no dropdown)
			if m.ghostText != "" && !m.showSuggestions {
				m.textarea.SetValue(m.textarea.Value() + m.ghostText)
				m.textarea.CursorEnd()
				m.ghostText = ""
				// Check if we should show arg hints
				value := m.textarea.Value()
				if strings.HasPrefix(value, "/") && strings.HasSuffix(value, " ") {
					cmdName := strings.TrimPrefix(strings.TrimSuffix(value, " "), "/")
					for _, cmd := range m.commands {
						if strings.EqualFold(cmd.Name, cmdName) && len(cmd.Args) > 0 {
							m.showArgHints = true
							m.currentCommand = &cmd
							break
						}
					}
				}
				return m, nil
			}

			// Accept suggestion from dropdown
			if m.showSuggestions && len(m.suggestions) > 0 {
				selected := m.suggestions[m.suggestionIndex]
				m.textarea.SetValue("/" + selected.Name + " ")
				m.textarea.CursorEnd()
				m.showSuggestions = false
				m.suggestions = nil
				m.ghostText = ""
				// Show arg hints if command has arguments
				if len(selected.Args) > 0 {
					m.showArgHints = true
					m.currentCommand = &selected
				}
				return m, nil
			}

			// Try to trigger suggestions
			value := m.textarea.Value()
			if strings.HasPrefix(value, "/") && !strings.Contains(value, " ") {
				m.updateSuggestions(value)
				if len(m.suggestions) == 1 {
					// Auto-complete if only one match
					selected := m.suggestions[0]
					m.textarea.SetValue("/" + selected.Name + " ")
					m.textarea.CursorEnd()
					m.showSuggestions = false
					m.suggestions = nil
					m.ghostText = ""
					// Show arg hints
					if len(selected.Args) > 0 {
						m.showArgHints = true
						m.currentCommand = &selected
					}
				}
			}
			return m, nil

		case tea.KeyEnter:
			// Handle autocomplete on Enter
			if m.showSuggestions && len(m.suggestions) > 0 {
				// Accept current suggestion
				selected := m.suggestions[m.suggestionIndex]
				m.textarea.SetValue("/" + selected.Name + " ")
				m.textarea.CursorEnd()
				m.showSuggestions = false
				m.suggestions = nil
				m.ghostText = ""
				// Show arg hints
				if len(selected.Args) > 0 {
					m.showArgHints = true
					m.currentCommand = &selected
				}
				return m, nil
			}

		case tea.KeyUp:
			// Navigate suggestions or history
			if m.showSuggestions && len(m.suggestions) > 0 {
				if m.suggestionIndex > 0 {
					m.suggestionIndex--
				}
				return m, nil
			}
			// Navigate to older history
			if len(m.history) > 0 {
				if m.historyIndex == -1 {
					m.savedInput = m.textarea.Value()
					m.historyIndex = len(m.history) - 1
				} else if m.historyIndex > 0 {
					m.historyIndex--
				}
				m.textarea.SetValue(m.history[m.historyIndex])
				m.textarea.CursorEnd()
			}
			return m, nil

		case tea.KeyDown:
			// Navigate suggestions or history
			if m.showSuggestions && len(m.suggestions) > 0 {
				if m.suggestionIndex < len(m.suggestions)-1 {
					m.suggestionIndex++
				}
				return m, nil
			}
			// Navigate to newer history
			if m.historyIndex >= 0 {
				if m.historyIndex < len(m.history)-1 {
					m.historyIndex++
					m.textarea.SetValue(m.history[m.historyIndex])
				} else {
					m.historyIndex = -1
					m.textarea.SetValue(m.savedInput)
				}
				m.textarea.CursorEnd()
			}
			return m, nil

		case tea.KeyEscape:
			// Cancel suggestions and ghost text
			if m.showSuggestions || m.ghostText != "" || m.showArgHints {
				m.showSuggestions = false
				m.suggestions = nil
				m.ghostText = ""
				m.showArgHints = false
				m.currentCommand = nil
				return m, nil
			}
		}

		// Update textarea and check for suggestion updates
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)

		// Update suggestions based on input
		value := m.textarea.Value()
		if strings.HasPrefix(value, "/") && !strings.Contains(value, " ") {
			m.updateSuggestions(value)
			// Clear arg hints when typing command
			m.showArgHints = false
			m.currentCommand = nil
		} else {
			m.showSuggestions = false
			m.suggestions = nil
			m.ghostText = ""
			// Clear arg hints when not in command context
			if !strings.HasPrefix(value, "/") {
				m.showArgHints = false
				m.currentCommand = nil
			}
		}

		return m, cmd
	}

	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// handleHistorySearch handles key events in history search mode.
func (m InputModel) handleHistorySearch(msg tea.KeyMsg) (InputModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		// Accept search result
		if m.historySearchResult != "" {
			m.textarea.SetValue(m.historySearchResult)
			m.textarea.CursorEnd()
		}
		m.historySearchMode = false
		m.historySearchQuery = ""
		m.historySearchResult = ""
		return m, nil

	case tea.KeyEscape, tea.KeyCtrlC:
		// Cancel search
		m.historySearchMode = false
		m.historySearchQuery = ""
		m.historySearchResult = ""
		return m, nil

	case tea.KeyCtrlR:
		// Search for next match (older)
		if m.historySearchIndex > 0 {
			m.historySearchIndex--
			m.searchHistory()
		}
		return m, nil

	case tea.KeyBackspace:
		// Remove last character from query
		if len(m.historySearchQuery) > 0 {
			m.historySearchQuery = m.historySearchQuery[:len(m.historySearchQuery)-1]
			m.historySearchIndex = len(m.history)
			m.searchHistory()
		}
		return m, nil

	default:
		// Add character to search query
		if msg.Type == tea.KeyRunes {
			m.historySearchQuery += string(msg.Runes)
			m.historySearchIndex = len(m.history)
			m.searchHistory()
		}
		return m, nil
	}
}

// searchHistory searches history for the current query.
func (m *InputModel) searchHistory() {
	if m.historySearchQuery == "" {
		m.historySearchResult = ""
		return
	}

	query := strings.ToLower(m.historySearchQuery)
	for i := m.historySearchIndex - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(m.history[i]), query) {
			m.historySearchResult = m.history[i]
			m.historySearchIndex = i
			return
		}
	}
	// No match found
	m.historySearchResult = ""
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
func (m *InputModel) updateSuggestions(input string) {
	prefix := strings.TrimPrefix(input, "/")
	prefix = strings.ToLower(prefix)

	var scored []scoredCommand

	for _, cmd := range m.commands {
		cmdLower := strings.ToLower(cmd.Name)

		// Exact prefix match gets highest priority
		if strings.HasPrefix(cmdLower, prefix) {
			scored = append(scored, scoredCommand{
				cmd:   cmd,
				score: 1000 + (100 - len(cmd.Name)), // Shorter names rank higher
			})
			continue
		}

		// Fuzzy match
		if score := fuzzyScore(cmd.Name, prefix); score > 0 {
			scored = append(scored, scoredCommand{cmd: cmd, score: score})
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

	// Update ghost text with top suggestion
	m.updateGhostText(prefix)
}

// updateGhostText updates the ghost text based on current input and suggestions.
func (m *InputModel) updateGhostText(prefix string) {
	if !m.ghostEnabled || len(m.suggestions) == 0 {
		m.ghostText = ""
		return
	}

	topCmd := m.suggestions[0]
	cmdLower := strings.ToLower(topCmd.Name)
	prefixLower := strings.ToLower(prefix)

	// Only show ghost text for prefix matches (not fuzzy matches)
	if strings.HasPrefix(cmdLower, prefixLower) && len(topCmd.Name) > len(prefix) {
		m.ghostText = topCmd.Name[len(prefix):]
	} else {
		m.ghostText = ""
	}
}

// View renders the input component.
func (m InputModel) View() string {
	var result strings.Builder

	// Show history search mode
	if m.historySearchMode {
		searchStyle := lipgloss.NewStyle().
			Foreground(ColorSecondary).
			Bold(true)

		queryStyle := lipgloss.NewStyle().
			Foreground(ColorText)

		resultStyle := lipgloss.NewStyle().
			Foreground(ColorMuted)

		result.WriteString(searchStyle.Render("(reverse-i-search)"))
		result.WriteString(queryStyle.Render("`" + m.historySearchQuery + "'"))
		result.WriteString(": ")
		if m.historySearchResult != "" {
			result.WriteString(resultStyle.Render(m.historySearchResult))
		}
		result.WriteString("\n")
	}

	// Show autocomplete suggestions
	if m.showSuggestions && len(m.suggestions) > 0 {
		suggestionBox := m.renderSuggestions()
		result.WriteString(suggestionBox)
		result.WriteString("\n")
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

// renderArgHints renders argument hints for the current command.
func (m InputModel) renderArgHints() string {
	if m.currentCommand == nil || len(m.currentCommand.Args) == 0 {
		return ""
	}

	var builder strings.Builder

	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	reqStyle := lipgloss.NewStyle().Foreground(ColorWarning) // Amber for required
	optStyle := lipgloss.NewStyle().Foreground(ColorMuted)   // Gray for optional

	builder.WriteString(hintStyle.Render("  Usage: /"))
	builder.WriteString(hintStyle.Render(m.currentCommand.Name))
	builder.WriteString(" ")

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

	return builder.String()
}

// renderSuggestions renders the autocomplete suggestion box.
func (m InputModel) renderSuggestions() string {
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
	if len(m.suggestions) < maxShow {
		maxShow = len(m.suggestions)
	}

	// Determine visible range
	start := 0
	if m.suggestionIndex >= maxShow {
		start = m.suggestionIndex - maxShow + 1
	}
	end := start + maxShow
	if end > len(m.suggestions) {
		end = len(m.suggestions)
	}

	for i := start; i < end; i++ {
		cmd := m.suggestions[i]
		style := normalStyle
		prefix := "  "
		if i == m.suggestionIndex {
			style = selectedStyle
			prefix = "> "
		}

		line := prefix + style.Render("/"+cmd.Name) + " " + descStyle.Render(cmd.Description)
		lines = append(lines, line)
	}

	// Add scroll indicator if needed
	if len(m.suggestions) > maxShow {
		indicator := lipgloss.NewStyle().Foreground(ColorDim).Render(
			fmt.Sprintf(" ↑↓ %d commands", len(m.suggestions)),
		)
		lines = append(lines, indicator)
	}

	return boxStyle.Render(strings.Join(lines, "\n"))
}

// Value returns the current input value.
func (m InputModel) Value() string {
	return strings.TrimSpace(m.textarea.Value())
}

// Reset clears the input and optionally saves to history.
func (m *InputModel) Reset() {
	m.textarea.Reset()
	m.historyIndex = -1
	m.savedInput = ""
}

// AddToHistory adds a command to the history.
func (m *InputModel) AddToHistory(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}

	// Don't add duplicates of the last entry
	if len(m.history) > 0 && m.history[len(m.history)-1] == cmd {
		return
	}

	m.history = append(m.history, cmd)

	// Trim history if too large
	if len(m.history) > maxHistorySize {
		m.history = m.history[len(m.history)-maxHistorySize:]
	}
}

// SetWidth sets the input width.
func (m *InputModel) SetWidth(width int) {
	m.textarea.SetWidth(width - 4) // Account for border padding
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
	return m.history
}

// SetHistory sets the history from an external source.
func (m *InputModel) SetHistory(history []string) {
	m.history = history
	if len(m.history) > maxHistorySize {
		m.history = m.history[len(m.history)-maxHistorySize:]
	}
}

// LoadHistory loads command history from file.
func (m *InputModel) LoadHistory() error {
	histPath, err := getHistoryPath()
	if err != nil {
		return err
	}

	file, err := os.Open(histPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No history file yet, that's fine
		}
		return err
	}
	defer file.Close()

	var history []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			history = append(history, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Keep only the last maxHistorySize entries
	if len(history) > maxHistorySize {
		history = history[len(history)-maxHistorySize:]
	}

	m.history = history
	return nil
}

// SaveHistory saves command history to file.
func (m *InputModel) SaveHistory() error {
	histPath, err := getHistoryPath()
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(histPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	file, err := os.Create(histPath)
	if err != nil {
		return err
	}
	defer file.Close()

	for _, cmd := range m.history {
		if _, err := file.WriteString(cmd + "\n"); err != nil {
			return err
		}
	}

	return nil
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

// SetPlaceholder sets the placeholder text for the input.
func (m *InputModel) SetPlaceholder(placeholder string) {
	m.textarea.Placeholder = placeholder
}

// updatePlaceholder updates the placeholder text based on context.
func (m *InputModel) updatePlaceholder() {
	if m.activeTask != "" {
		m.textarea.Placeholder = "Continue: " + m.activeTask + " | /help for commands"
	} else {
		m.textarea.Placeholder = "Type a message or /command... (Tab to complete)"
	}
}

// SetCommands sets the available commands for autocomplete.
func (m *InputModel) SetCommands(commands []CommandInfo) {
	m.commands = commands
}

// AddCommand adds a single command to the autocomplete list.
func (m *InputModel) AddCommand(cmd CommandInfo) {
	// Check if command already exists
	for i, existing := range m.commands {
		if existing.Name == cmd.Name {
			m.commands[i] = cmd
			return
		}
	}
	m.commands = append(m.commands, cmd)
}

// IsHistorySearchMode returns whether history search mode is active.
func (m *InputModel) IsHistorySearchMode() bool {
	return m.historySearchMode
}

// ShowingSuggestions returns whether suggestions are being shown.
func (m *InputModel) ShowingSuggestions() bool {
	return m.showSuggestions
}
