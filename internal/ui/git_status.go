package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// GitAction represents user actions on git status.
type GitAction int

const (
	GitActionNone GitAction = iota
	GitActionStage
	GitActionUnstage
	GitActionStageAll
	GitActionUnstageAll
	GitActionDiff
	GitActionCommit
	GitActionReset
	GitActionClose
)

// GitFileStatus represents the status of a file in git.
type GitFileStatus int

const (
	GitFileUntracked GitFileStatus = iota
	GitFileModified
	GitFileStaged
	GitFileDeleted
	GitFileRenamed
	GitFileConflict
)

// GitFileEntry represents a file in git status.
type GitFileEntry struct {
	FilePath  string
	Status    GitFileStatus
	IsStaged  bool
	OldPath   string // For renames
	DiffStats string // e.g., "+10 -5"
}

// GitStatusRequestMsg is sent to display git status.
type GitStatusRequestMsg struct {
	Entries     []GitFileEntry
	Branch      string
	Upstream    string
	AheadBehind string // e.g., "2 ahead, 1 behind"
}

// GitStatusActionMsg is sent when user performs an action.
type GitStatusActionMsg struct {
	Action  GitAction
	Files   []string
	Message string
}

// GitStatusModel is the UI for interactive git status.
type GitStatusModel struct {
	entries         []GitFileEntry
	selectedIndex   int
	selectedIndices map[int]bool // For multi-select
	viewport        viewport.Model
	diffViewport    viewport.Model
	showDiff        bool
	currentDiff     string
	branch          string
	upstream        string
	aheadBehind     string
	styles          *Styles
	width           int
	height          int

	// Callback for actions
	onAction func(action GitAction, files []string, message string)
}

// NewGitStatusModel creates a new git status model.
func NewGitStatusModel(styles *Styles) GitStatusModel {
	vp := viewport.New(60, 15)
	vp.MouseWheelEnabled = true

	diffVp := viewport.New(60, 15)
	diffVp.MouseWheelEnabled = true

	return GitStatusModel{
		viewport:        vp,
		diffViewport:    diffVp,
		styles:          styles,
		selectedIndices: make(map[int]bool),
	}
}

// SetSize sets the size of the git status view.
func (m *GitStatusModel) SetSize(width, height int) {
	if width < 10 {
		width = 80
	}
	if height < 10 {
		height = 24
	}
	m.width = width
	m.height = height

	if m.showDiff {
		halfWidth := (width - 4) / 2
		m.viewport.Width = halfWidth
		m.viewport.Height = height - 10
		m.diffViewport.Width = halfWidth
		m.diffViewport.Height = height - 10
	} else {
		m.viewport.Width = width - 4
		m.viewport.Height = height - 10
	}
}

// SetStatus sets the git status to display.
func (m *GitStatusModel) SetStatus(entries []GitFileEntry, branch, upstream, aheadBehind string) {
	m.entries = entries
	m.branch = branch
	m.upstream = upstream
	m.aheadBehind = aheadBehind
	m.selectedIndex = 0
	m.selectedIndices = make(map[int]bool)
	m.updateViewport()
}

// SetActionCallback sets the callback for user actions.
func (m *GitStatusModel) SetActionCallback(callback func(GitAction, []string, string)) {
	m.onAction = callback
}

// updateViewport updates the viewport content.
func (m *GitStatusModel) updateViewport() {
	var content strings.Builder

	// Staged section
	staged := m.filterByStaged(true)
	if len(staged) > 0 {
		headerStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSuccess)
		content.WriteString(headerStyle.Render("Staged Changes"))
		content.WriteString("\n")

		for _, idx := range staged {
			line := m.formatEntryLine(idx)
			content.WriteString(line)
			content.WriteString("\n")
		}
		content.WriteString("\n")
	}

	// Unstaged section
	unstaged := m.filterByStaged(false)
	if len(unstaged) > 0 {
		headerStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorWarning)
		content.WriteString(headerStyle.Render("Unstaged Changes"))
		content.WriteString("\n")

		for _, idx := range unstaged {
			line := m.formatEntryLine(idx)
			content.WriteString(line)
			content.WriteString("\n")
		}
	}

	m.viewport.SetContent(content.String())
}

// filterByStaged returns indices of entries filtered by staged status.
func (m *GitStatusModel) filterByStaged(staged bool) []int {
	var result []int
	for i, entry := range m.entries {
		if entry.IsStaged == staged {
			result = append(result, i)
		}
	}
	return result
}

// formatEntryLine formats a single git entry for display.
func (m *GitStatusModel) formatEntryLine(index int) string {
	entry := m.entries[index]
	isSelected := index == m.selectedIndex
	isMultiSelected := false
	if m.selectedIndices != nil {
		isMultiSelected = m.selectedIndices[index]
	}

	// Styles
	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary).
		Background(lipgloss.Color("#1F2937"))
	normalStyle := lipgloss.NewStyle().
		Foreground(ColorText)
	pathStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)
	statsStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	// Status icon
	var statusIcon string
	var statusColor lipgloss.Color
	switch entry.Status {
	case GitFileUntracked:
		statusIcon = "?"
		statusColor = ColorMuted
	case GitFileModified:
		statusIcon = "M"
		statusColor = ColorWarning
	case GitFileStaged:
		statusIcon = "A"
		statusColor = ColorSuccess
	case GitFileDeleted:
		statusIcon = "D"
		statusColor = ColorError
	case GitFileRenamed:
		statusIcon = "R"
		statusColor = ColorInfo
	case GitFileConflict:
		statusIcon = "!"
		statusColor = ColorError
	}

	statusStyle := lipgloss.NewStyle().
		Foreground(statusColor).
		Bold(true)

	// Selection indicator
	prefix := "  "
	if isSelected {
		prefix = "> "
	}
	if isMultiSelected {
		prefix = "* "
	}

	// Path display
	path := filepath.Base(entry.FilePath)
	dir := filepath.Dir(entry.FilePath)
	if dir != "." {
		path = shortenPath(dir, 20) + "/" + path
	}

	line := fmt.Sprintf("%s%s %s",
		prefix,
		statusStyle.Render(statusIcon),
		pathStyle.Render(path),
	)

	if entry.DiffStats != "" {
		line += " " + statsStyle.Render(entry.DiffStats)
	}

	if entry.OldPath != "" {
		line += statsStyle.Render(fmt.Sprintf(" (from %s)", filepath.Base(entry.OldPath)))
	}

	if isSelected {
		return selectedStyle.Render(line)
	}
	return normalStyle.Render(line)
}

// setDiff sets the diff content for preview.
func (m *GitStatusModel) setDiff(diff string) {
	m.currentDiff = diff
	m.diffViewport.SetContent(m.highlightDiff(diff))
}

// highlightDiff applies syntax highlighting to diff.
func (m *GitStatusModel) highlightDiff(diff string) string {
	addedStyle := lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true)
	removedStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	headerStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true)
	hunkStyle := lipgloss.NewStyle().Foreground(ColorHighlight)
	contextStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	lines := strings.Split(diff, "\n")
	var result strings.Builder

	for _, line := range lines {
		var styledLine string

		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			styledLine = headerStyle.Render(line)
		case strings.HasPrefix(line, "@@"):
			styledLine = hunkStyle.Render(line)
		case strings.HasPrefix(line, "+"):
			styledLine = addedStyle.Render(line)
		case strings.HasPrefix(line, "-"):
			styledLine = removedStyle.Render(line)
		default:
			styledLine = contextStyle.Render(line)
		}

		result.WriteString(styledLine)
		result.WriteString("\n")
	}

	return result.String()
}

// Init initializes the git status model.
func (m GitStatusModel) Init() tea.Cmd {
	return nil
}

// Update handles input events for git status.
func (m GitStatusModel) Update(msg tea.Msg) (GitStatusModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if m.selectedIndex < len(m.entries)-1 {
				m.selectedIndex++
				m.updateViewport()
			}

		case "k", "up":
			if m.selectedIndex > 0 {
				m.selectedIndex--
				m.updateViewport()
			}

		case " ":
			// Toggle stage/unstage for current file
			if len(m.entries) > 0 && m.selectedIndex < len(m.entries) {
				entry := m.entries[m.selectedIndex]
				var action GitAction
				if entry.IsStaged {
					action = GitActionUnstage
				} else {
					action = GitActionStage
				}
				if m.onAction != nil {
					m.onAction(action, []string{entry.FilePath}, "")
				}
				return m, func() tea.Msg {
					return GitStatusActionMsg{
						Action: action,
						Files:  []string{entry.FilePath},
					}
				}
			}

		case "a":
			// Stage all
			var files []string
			for _, entry := range m.entries {
				if !entry.IsStaged {
					files = append(files, entry.FilePath)
				}
			}
			if len(files) > 0 {
				if m.onAction != nil {
					m.onAction(GitActionStageAll, files, "")
				}
				return m, func() tea.Msg {
					return GitStatusActionMsg{
						Action: GitActionStageAll,
						Files:  files,
					}
				}
			}

		case "u":
			// Unstage all
			var files []string
			for _, entry := range m.entries {
				if entry.IsStaged {
					files = append(files, entry.FilePath)
				}
			}
			if len(files) > 0 {
				if m.onAction != nil {
					m.onAction(GitActionUnstageAll, files, "")
				}
				return m, func() tea.Msg {
					return GitStatusActionMsg{
						Action: GitActionUnstageAll,
						Files:  files,
					}
				}
			}

		case "d":
			// Toggle diff view
			m.showDiff = !m.showDiff
			m.SetSize(m.width, m.height)
			if m.showDiff && len(m.entries) > 0 && m.selectedIndex < len(m.entries) {
				// Request diff for selected file
				entry := m.entries[m.selectedIndex]
				if m.onAction != nil {
					m.onAction(GitActionDiff, []string{entry.FilePath}, "")
				}
			}

		case "c":
			// Commit staged changes
			var stagedFiles []string
			for _, entry := range m.entries {
				if entry.IsStaged {
					stagedFiles = append(stagedFiles, entry.FilePath)
				}
			}
			if len(stagedFiles) > 0 {
				if m.onAction != nil {
					m.onAction(GitActionCommit, stagedFiles, "")
				}
				return m, func() tea.Msg {
					return GitStatusActionMsg{
						Action: GitActionCommit,
						Files:  stagedFiles,
					}
				}
			}

		case "r":
			// Reset file
			if len(m.entries) > 0 && m.selectedIndex < len(m.entries) {
				entry := m.entries[m.selectedIndex]
				if m.onAction != nil {
					m.onAction(GitActionReset, []string{entry.FilePath}, "")
				}
				return m, func() tea.Msg {
					return GitStatusActionMsg{
						Action: GitActionReset,
						Files:  []string{entry.FilePath},
					}
				}
			}

		case "q", "esc":
			if m.onAction != nil {
				m.onAction(GitActionClose, nil, "")
			}
			return m, func() tea.Msg {
				return GitStatusActionMsg{Action: GitActionClose}
			}

		case "tab":
			// Toggle multi-select for current item
			if len(m.entries) > 0 {
				if m.selectedIndices == nil {
					m.selectedIndices = make(map[int]bool)
				}
				if m.selectedIndices[m.selectedIndex] {
					delete(m.selectedIndices, m.selectedIndex)
				} else {
					m.selectedIndices[m.selectedIndex] = true
				}
				m.updateViewport()
			}
		}

	case tea.MouseMsg:
		if m.showDiff {
			m.diffViewport, cmd = m.diffViewport.Update(msg)
		} else {
			m.viewport, cmd = m.viewport.Update(msg)
		}
		return m, cmd

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	}

	return m, nil
}

// View renders the git status.
func (m GitStatusModel) View() string {
	var builder strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight).
		Padding(0, 1)

	builder.WriteString(headerStyle.Render("📊 Git Status"))
	builder.WriteString("\n\n")

	// Branch info
	branchStyle := lipgloss.NewStyle().
		Foreground(ColorSuccess).
		Bold(true)
	upstreamStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)
	aheadStyle := lipgloss.NewStyle().
		Foreground(ColorWarning)

	builder.WriteString(fmt.Sprintf("  Branch: %s", branchStyle.Render(m.branch)))
	if m.upstream != "" {
		builder.WriteString(fmt.Sprintf(" → %s", upstreamStyle.Render(m.upstream)))
	}
	if m.aheadBehind != "" {
		builder.WriteString(fmt.Sprintf(" (%s)", aheadStyle.Render(m.aheadBehind)))
	}
	builder.WriteString("\n\n")

	// File counts
	staged := len(m.filterByStaged(true))
	unstaged := len(m.filterByStaged(false))
	countStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	builder.WriteString(countStyle.Render(fmt.Sprintf("  Staged: %d  │  Unstaged: %d", staged, unstaged)))
	builder.WriteString("\n\n")

	// Content viewports
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)

	if m.showDiff {
		// Split view
		filesBox := borderStyle.Render(m.viewport.View())
		diffBox := borderStyle.Render(m.diffViewport.View())
		builder.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, filesBox, " ", diffBox))
	} else {
		builder.WriteString(borderStyle.Render(m.viewport.View()))
	}

	builder.WriteString("\n\n")

	// Footer with actions
	m.renderActions(&builder)

	return builder.String()
}

// renderActions renders the available actions.
func (m *GitStatusModel) renderActions(builder *strings.Builder) {
	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	keyStyle := lipgloss.NewStyle().
		Foreground(ColorSecondary).
		Bold(true)

	hints := []string{
		keyStyle.Render("Space") + " Stage/Unstage",
		keyStyle.Render("a") + " Stage all",
		keyStyle.Render("d") + " Diff",
		keyStyle.Render("c") + " Commit",
		keyStyle.Render("r") + " Reset",
		keyStyle.Render("q") + " Close",
	}

	builder.WriteString(hintStyle.Render(strings.Join(hints, "  │  ")))
}

// GetSelectedFiles returns the currently selected files.
func (m GitStatusModel) GetSelectedFiles() []string {
	if len(m.selectedIndices) > 0 {
		var files []string
		for idx := range m.selectedIndices {
			if idx < len(m.entries) {
				files = append(files, m.entries[idx].FilePath)
			}
		}
		return files
	}

	if m.selectedIndex < len(m.entries) {
		return []string{m.entries[m.selectedIndex].FilePath}
	}
	return nil
}
