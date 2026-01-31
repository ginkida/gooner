package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SearchAction represents user actions on search results.
type SearchAction int

const (
	SearchActionNone SearchAction = iota
	SearchActionOpen
	SearchActionEdit
	SearchActionCopyPath
	SearchActionClose
)

// SearchResult represents a single search result.
type SearchResult struct {
	FilePath   string
	LineNumber int
	Content    string
	Context    []string // Lines before/after for context
	MatchCount int      // Number of matches in this file
}

// SearchResultsRequestMsg is sent to display search results.
type SearchResultsRequestMsg struct {
	Query   string
	Results []SearchResult
	Tool    string // "grep", "glob", etc.
}

// SearchResultsActionMsg is sent when user performs an action.
type SearchResultsActionMsg struct {
	Action     SearchAction
	FilePath   string
	LineNumber int
}

// SearchResultsModel is the UI for interactive search results.
type SearchResultsModel struct {
	results       []SearchResult
	selectedIndex int
	viewport      viewport.Model
	previewPane   viewport.Model
	query         string
	tool          string
	styles        *Styles
	width         int
	height        int
	showPreview   bool

	// Callback for actions
	onAction func(action SearchAction, filePath string, lineNum int)
}

// NewSearchResultsModel creates a new search results model.
func NewSearchResultsModel(styles *Styles) SearchResultsModel {
	vp := viewport.New(40, 15)
	vp.MouseWheelEnabled = true

	preview := viewport.New(40, 15)
	preview.MouseWheelEnabled = true

	return SearchResultsModel{
		viewport:    vp,
		previewPane: preview,
		styles:      styles,
		showPreview: false,
	}
}

// SetSize sets the size of the search results view.
func (m *SearchResultsModel) SetSize(width, height int) {
	if width < 10 {
		width = 80
	}
	if height < 10 {
		height = 24
	}
	m.width = width
	m.height = height

	if m.showPreview {
		// Split view: 50% results, 50% preview
		halfWidth := (width - 4) / 2
		m.viewport.Width = halfWidth
		m.viewport.Height = height - 8
		m.previewPane.Width = halfWidth
		m.previewPane.Height = height - 8
	} else {
		m.viewport.Width = width - 4
		m.viewport.Height = height - 8
	}
}

// SetResults sets the search results to display.
func (m *SearchResultsModel) SetResults(query, tool string, results []SearchResult) {
	m.query = query
	m.tool = tool
	m.results = results
	m.selectedIndex = 0
	m.updateViewport()
}

// SetActionCallback sets the callback for user actions.
func (m *SearchResultsModel) SetActionCallback(callback func(SearchAction, string, int)) {
	m.onAction = callback
}

// updateViewport updates the viewport content based on current selection.
func (m *SearchResultsModel) updateViewport() {
	var content strings.Builder

	for i, result := range m.results {
		line := m.formatResultLine(i, result)
		content.WriteString(line)
		content.WriteString("\n")
	}

	m.viewport.SetContent(content.String())
}

// formatResultLine formats a single search result for display.
func (m *SearchResultsModel) formatResultLine(index int, result SearchResult) string {
	isSelected := index == m.selectedIndex

	// Styles
	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary).
		Background(lipgloss.Color("#1F2937"))
	normalStyle := lipgloss.NewStyle().
		Foreground(ColorText)
	pathStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)
	lineNumStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)
	matchStyle := lipgloss.NewStyle().
		Foreground(ColorWarning)

	// Format path
	dir := filepath.Dir(result.FilePath)
	base := filepath.Base(result.FilePath)
	shortDir := shortenPath(dir, 30)

	var lineStr string
	if result.LineNumber > 0 {
		lineStr = lineNumStyle.Render(fmt.Sprintf(":%d", result.LineNumber))
	}

	var matchInfo string
	if result.MatchCount > 1 {
		matchInfo = matchStyle.Render(fmt.Sprintf(" (%d matches)", result.MatchCount))
	}

	prefix := "  "
	if isSelected {
		prefix = "> "
	}

	line := fmt.Sprintf("%s%s/%s%s%s",
		prefix,
		pathStyle.Render(shortDir),
		pathStyle.Render(base),
		lineStr,
		matchInfo,
	)

	// Add content preview on the same line if short
	if result.Content != "" && len(result.Content) < 50 {
		contentPreview := strings.TrimSpace(result.Content)
		line += "  " + lineNumStyle.Render(truncate(contentPreview, 40))
	}

	if isSelected {
		return selectedStyle.Render(line)
	}
	return normalStyle.Render(line)
}

// updatePreview updates the preview pane with context.
func (m *SearchResultsModel) updatePreview() {
	if len(m.results) == 0 || m.selectedIndex >= len(m.results) {
		m.previewPane.SetContent("")
		return
	}

	result := m.results[m.selectedIndex]

	var content strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight)
	content.WriteString(headerStyle.Render(filepath.Base(result.FilePath)))
	content.WriteString("\n\n")

	// Context lines
	lineNumStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	matchStyle := lipgloss.NewStyle().
		Foreground(ColorWarning).
		Bold(true)

	for i, ctx := range result.Context {
		lineNum := result.LineNumber - len(result.Context)/2 + i
		if lineNum < 1 {
			lineNum = 1
		}

		isMatchLine := i == len(result.Context)/2

		numStr := lineNumStyle.Render(fmt.Sprintf("%4d │ ", lineNum))
		if isMatchLine {
			content.WriteString(numStr)
			content.WriteString(matchStyle.Render(ctx))
		} else {
			content.WriteString(numStr)
			content.WriteString(ctx)
		}
		content.WriteString("\n")
	}

	m.previewPane.SetContent(content.String())
}

// Init initializes the search results model.
func (m SearchResultsModel) Init() tea.Cmd {
	return nil
}

// Update handles input events for search results.
func (m SearchResultsModel) Update(msg tea.Msg) (SearchResultsModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if m.selectedIndex < len(m.results)-1 {
				m.selectedIndex++
				m.updateViewport()
				if m.showPreview {
					m.updatePreview()
				}
			}

		case "k", "up":
			if m.selectedIndex > 0 {
				m.selectedIndex--
				m.updateViewport()
				if m.showPreview {
					m.updatePreview()
				}
			}

		case "enter":
			if len(m.results) > 0 && m.selectedIndex < len(m.results) {
				result := m.results[m.selectedIndex]
				if m.onAction != nil {
					m.onAction(SearchActionOpen, result.FilePath, result.LineNumber)
				}
				return m, func() tea.Msg {
					return SearchResultsActionMsg{
						Action:     SearchActionOpen,
						FilePath:   result.FilePath,
						LineNumber: result.LineNumber,
					}
				}
			}

		case "e":
			if len(m.results) > 0 && m.selectedIndex < len(m.results) {
				result := m.results[m.selectedIndex]
				if m.onAction != nil {
					m.onAction(SearchActionEdit, result.FilePath, result.LineNumber)
				}
				return m, func() tea.Msg {
					return SearchResultsActionMsg{
						Action:     SearchActionEdit,
						FilePath:   result.FilePath,
						LineNumber: result.LineNumber,
					}
				}
			}

		case " ":
			// Toggle preview
			m.showPreview = !m.showPreview
			m.SetSize(m.width, m.height)
			if m.showPreview {
				m.updatePreview()
			}

		case "y":
			// Copy path
			if len(m.results) > 0 && m.selectedIndex < len(m.results) {
				result := m.results[m.selectedIndex]
				if m.onAction != nil {
					m.onAction(SearchActionCopyPath, result.FilePath, 0)
				}
				return m, func() tea.Msg {
					return SearchResultsActionMsg{
						Action:   SearchActionCopyPath,
						FilePath: result.FilePath,
					}
				}
			}

		case "q", "esc":
			if m.onAction != nil {
				m.onAction(SearchActionClose, "", 0)
			}
			return m, func() tea.Msg {
				return SearchResultsActionMsg{Action: SearchActionClose}
			}

		case "g":
			m.selectedIndex = 0
			m.updateViewport()
			if m.showPreview {
				m.updatePreview()
			}

		case "G":
			if len(m.results) > 0 {
				m.selectedIndex = len(m.results) - 1
				m.updateViewport()
				if m.showPreview {
					m.updatePreview()
				}
			}
		}

	case tea.MouseMsg:
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	}

	return m, nil
}

// View renders the search results.
func (m SearchResultsModel) View() string {
	var builder strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight).
		Padding(0, 1)

	builder.WriteString(headerStyle.Render("🔍 Search Results"))
	builder.WriteString("\n\n")

	// Query info
	queryStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)
	countStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	builder.WriteString(fmt.Sprintf("  %s: %s  %s\n\n",
		m.tool,
		queryStyle.Render(m.query),
		countStyle.Render(fmt.Sprintf("(%d results)", len(m.results))),
	))

	// Results viewport with border
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)

	if m.showPreview {
		// Split view
		resultBox := borderStyle.Render(m.viewport.View())
		previewBox := borderStyle.Render(m.previewPane.View())
		builder.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, resultBox, " ", previewBox))
	} else {
		builder.WriteString(borderStyle.Render(m.viewport.View()))
	}

	builder.WriteString("\n\n")

	// Footer with actions
	m.renderActions(&builder)

	return builder.String()
}

// renderActions renders the available actions.
func (m *SearchResultsModel) renderActions(builder *strings.Builder) {
	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	keyStyle := lipgloss.NewStyle().
		Foreground(ColorSecondary).
		Bold(true)

	hints := []string{
		keyStyle.Render("Enter") + " Open",
		keyStyle.Render("e") + " Edit",
		keyStyle.Render("Space") + " Preview",
		keyStyle.Render("y") + " Copy path",
		keyStyle.Render("q") + " Close",
	}

	builder.WriteString(hintStyle.Render(strings.Join(hints, "  │  ")))
	builder.WriteString("\n")
	builder.WriteString(hintStyle.Render("j/k: Navigate  │  g/G: Top/Bottom"))
}

// GetSelectedResult returns the currently selected result.
func (m SearchResultsModel) GetSelectedResult() (SearchResult, bool) {
	if len(m.results) == 0 || m.selectedIndex >= len(m.results) {
		return SearchResult{}, false
	}
	return m.results[m.selectedIndex], true
}

// truncate truncates a string to max length.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
