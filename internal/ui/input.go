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

// CommandInfo contains information about a slash command for autocomplete.
type CommandInfo struct {
	Name        string
	Description string
	Category    string
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
		{Name: "save", Description: "Save current session", Category: "Session"},
		{Name: "resume", Description: "Resume a saved session", Category: "Session"},
		{Name: "sessions", Description: "List saved sessions", Category: "Session"},

		// History commands
		{Name: "undo", Description: "Undo the last file change", Category: "History"},
		{Name: "checkpoint", Description: "Create a checkpoint", Category: "History"},
		{Name: "checkpoints", Description: "List all checkpoints", Category: "History"},
		{Name: "restore", Description: "Restore a checkpoint", Category: "History"},

		// Git commands
		{Name: "init", Description: "Initialize GOONER.md", Category: "Git"},

		// Auth commands
		{Name: "login", Description: "Login with API key or OAuth", Category: "Auth"},
		{Name: "logout", Description: "Clear stored credentials", Category: "Auth"},
		{Name: "auth-status", Description: "Show authentication status", Category: "Auth"},

		// Utility commands
		{Name: "doctor", Description: "Check environment", Category: "Utility"},
		{Name: "config", Description: "Show configuration", Category: "Utility"},
		{Name: "cost", Description: "Show token usage", Category: "Utility"},
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

		case tea.KeyTab, tea.KeyEnter:
			// Handle autocomplete
			if m.showSuggestions && len(m.suggestions) > 0 {
				// Accept current suggestion
				selected := m.suggestions[m.suggestionIndex]
				m.textarea.SetValue("/" + selected.Name + " ")
				m.textarea.CursorEnd()
				m.showSuggestions = false
				m.suggestions = nil
				return m, nil
			}
			// If Tab but no suggestions, try to trigger suggestions
			if msg.Type == tea.KeyTab {
				value := m.textarea.Value()
				if strings.HasPrefix(value, "/") {
					m.updateSuggestions(value)
					if len(m.suggestions) == 1 {
						// Auto-complete if only one match
						m.textarea.SetValue("/" + m.suggestions[0].Name + " ")
						m.textarea.CursorEnd()
						m.showSuggestions = false
						m.suggestions = nil
					}
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
			// Cancel suggestions
			if m.showSuggestions {
				m.showSuggestions = false
				m.suggestions = nil
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
		} else {
			m.showSuggestions = false
			m.suggestions = nil
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

// updateSuggestions updates the autocomplete suggestions.
func (m *InputModel) updateSuggestions(input string) {
	prefix := strings.TrimPrefix(input, "/")
	prefix = strings.ToLower(prefix)

	var matches []CommandInfo
	for _, cmd := range m.commands {
		if strings.HasPrefix(strings.ToLower(cmd.Name), prefix) {
			matches = append(matches, cmd)
		}
	}

	// Sort by name
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Name < matches[j].Name
	})

	m.suggestions = matches
	m.suggestionIndex = 0
	m.showSuggestions = len(matches) > 0
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

	// Input field
	result.WriteString(m.styles.Input.Render(m.textarea.View()))

	return result.String()
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

	return filepath.Join(dataDir, "gooner", historyFile), nil
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
