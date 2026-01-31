package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// DiffDecision represents user's decision on a diff preview.
type DiffDecision int

const (
	DiffPending DiffDecision = iota
	DiffApply
	DiffReject
	DiffEdit
)

// DiffPreviewModel is the UI component for displaying diff previews.
type DiffPreviewModel struct {
	viewport   viewport.Model
	diff       string
	filePath   string
	oldContent string
	newContent string
	decision   DiffDecision
	toolName   string
	isNewFile  bool
	styles     *Styles
	width      int
	height     int

	// Callback when user makes a decision
	onDecision func(decision DiffDecision)
}

// DiffPreviewRequestMsg is sent to request a diff preview.
type DiffPreviewRequestMsg struct {
	FilePath   string
	OldContent string
	NewContent string
	ToolName   string
	IsNewFile  bool
}

// DiffPreviewResponseMsg is sent when user makes a decision.
type DiffPreviewResponseMsg struct {
	Decision   DiffDecision
	FilePath   string
	NewContent string
}

// NewDiffPreviewModel creates a new diff preview model.
func NewDiffPreviewModel(styles *Styles) DiffPreviewModel {
	vp := viewport.New(80, 20)
	vp.MouseWheelEnabled = true

	return DiffPreviewModel{
		viewport: vp,
		styles:   styles,
		decision: DiffPending,
	}
}

// SetSize sets the size of the diff preview.
func (m *DiffPreviewModel) SetSize(width, height int) {
	if width < 10 {
		width = 80
	}
	if height < 10 {
		height = 24
	}
	m.width = width
	m.height = height
	m.viewport.Width = width - 4
	m.viewport.Height = height - 10 // Reserve space for header and footer
}

// SetContent sets the diff content to display.
func (m *DiffPreviewModel) SetContent(filePath, oldContent, newContent, toolName string, isNewFile bool) {
	m.filePath = filePath
	m.oldContent = oldContent
	m.newContent = newContent
	m.toolName = toolName
	m.isNewFile = isNewFile
	m.decision = DiffPending

	// Generate diff
	m.diff = m.generateDiff(oldContent, newContent)
	m.viewport.SetContent(m.highlightDiff(m.diff))
	m.viewport.GotoTop()
}

// SetDecisionCallback sets the callback for when user makes a decision.
func (m *DiffPreviewModel) SetDecisionCallback(callback func(DiffDecision)) {
	m.onDecision = callback
}

// generateDiff creates a unified diff between old and new content.
func (m *DiffPreviewModel) generateDiff(oldContent, newContent string) string {
	dmp := diffmatchpatch.New()

	var result strings.Builder

	// Header
	result.WriteString(fmt.Sprintf("--- %s\n", m.filePath))
	result.WriteString(fmt.Sprintf("+++ %s\n", m.filePath))

	// Generate line-based diff
	diffs := dmp.DiffMain(oldContent, newContent, true)
	diffs = dmp.DiffCleanupSemantic(diffs)

	// Convert to unified diff format
	oldLineNum := 1
	newLineNum := 1

	for _, d := range diffs {
		lines := strings.Split(d.Text, "\n")
		for i, line := range lines {
			// Skip empty trailing element from split
			if i == len(lines)-1 && line == "" {
				continue
			}

			switch d.Type {
			case diffmatchpatch.DiffEqual:
				result.WriteString(fmt.Sprintf(" %s\n", line))
				oldLineNum++
				newLineNum++
			case diffmatchpatch.DiffDelete:
				result.WriteString(fmt.Sprintf("-%s\n", line))
				oldLineNum++
			case diffmatchpatch.DiffInsert:
				result.WriteString(fmt.Sprintf("+%s\n", line))
				newLineNum++
			}
		}
	}

	// Stats
	added := 0
	removed := 0
	for _, d := range diffs {
		if d.Type == diffmatchpatch.DiffInsert {
			added += strings.Count(d.Text, "\n") + 1
		} else if d.Type == diffmatchpatch.DiffDelete {
			removed += strings.Count(d.Text, "\n") + 1
		}
	}

	return result.String()
}

// highlightDiff applies syntax highlighting to the diff.
func (m *DiffPreviewModel) highlightDiff(diff string) string {
	addedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Bold(true)
	removedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Bold(true)
	headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#06B6D4")).Bold(true)
	hunkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#A78BFA"))
	contextStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))

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

// Init initializes the diff preview model.
func (m DiffPreviewModel) Init() tea.Cmd {
	return nil
}

// Update handles input events for the diff preview.
func (m DiffPreviewModel) Update(msg tea.Msg) (DiffPreviewModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y":
			m.decision = DiffApply
			if m.onDecision != nil {
				m.onDecision(DiffApply)
			}
			return m, func() tea.Msg {
				return DiffPreviewResponseMsg{
					Decision:   DiffApply,
					FilePath:   m.filePath,
					NewContent: m.newContent,
				}
			}

		case "n", "N", "esc":
			m.decision = DiffReject
			if m.onDecision != nil {
				m.onDecision(DiffReject)
			}
			return m, func() tea.Msg {
				return DiffPreviewResponseMsg{
					Decision:   DiffReject,
					FilePath:   m.filePath,
					NewContent: m.newContent,
				}
			}

		case "j", "down":
			m.viewport, cmd = m.viewport.Update(tea.KeyMsg{Type: tea.KeyDown})
			return m, cmd

		case "k", "up":
			m.viewport, cmd = m.viewport.Update(tea.KeyMsg{Type: tea.KeyUp})
			return m, cmd

		case "g":
			m.viewport.GotoTop()
			return m, nil

		case "G":
			m.viewport.GotoBottom()
			return m, nil

		case "ctrl+d":
			m.viewport.HalfViewDown()
			return m, nil

		case "ctrl+u":
			m.viewport.HalfViewUp()
			return m, nil
		}

	case tea.MouseMsg:
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	}

	return m, nil
}

// View renders the diff preview (Claude Code style — no bordered boxes).
func (m DiffPreviewModel) View() string {
	var builder strings.Builder

	markerStyle := lipgloss.NewStyle().Foreground(ColorDim)
	nameStyle := lipgloss.NewStyle().Foreground(ColorText).Bold(true)
	fileStyle := lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)
	toolStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	// Header: ● Diff Preview
	bulletStyle := lipgloss.NewStyle().Foreground(ColorPrimary).Bold(true)
	builder.WriteString(bulletStyle.Render("● ") + nameStyle.Render("Diff Preview"))
	builder.WriteString("\n")

	// File info with ⎿ marker
	var fileLabel string
	if m.isNewFile {
		fileLabel = "New file: "
	} else {
		fileLabel = "Modified: "
	}
	builder.WriteString(markerStyle.Render("  ⎿  ") + toolStyle.Render(m.toolName+" → ") + fileStyle.Render(fileLabel+m.filePath))
	builder.WriteString("\n")

	// Diff statistics
	addCount, removeCount := m.countChanges()
	addedStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
	removedStyle := lipgloss.NewStyle().Foreground(ColorError)
	stats := fmt.Sprintf("%s, %s",
		addedStyle.Render(fmt.Sprintf("+%d", addCount)),
		removedStyle.Render(fmt.Sprintf("-%d", removeCount)))
	builder.WriteString(markerStyle.Render("     ") + stats)
	builder.WriteString("\n\n")

	// Diff viewport without border
	builder.WriteString(m.viewport.View())
	builder.WriteString("\n\n")

	// Footer with actions
	m.renderActions(&builder)

	return builder.String()
}

// countChanges counts added and removed lines.
func (m DiffPreviewModel) countChanges() (added, removed int) {
	lines := strings.Split(m.diff, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removed++
		}
	}
	return
}

// renderActions renders the action buttons.
func (m DiffPreviewModel) renderActions(builder *strings.Builder) {
	// Styled buttons
	applyStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(ColorSuccess).
		Padding(0, 2)

	rejectStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(ColorError).
		Padding(0, 2)

	hintStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	builder.WriteString(applyStyle.Render("y Apply"))
	builder.WriteString("  ")
	builder.WriteString(rejectStyle.Render("n Reject"))
	builder.WriteString("\n\n")

	builder.WriteString(hintStyle.Render("j/k: Scroll | g/G: Top/Bottom | Ctrl+D/U: Half page"))
}

// GetDecision returns the current decision.
func (m DiffPreviewModel) GetDecision() DiffDecision {
	return m.decision
}

// GetFilePath returns the file path being previewed.
func (m DiffPreviewModel) GetFilePath() string {
	return m.filePath
}

// GetNewContent returns the new content for the file.
func (m DiffPreviewModel) GetNewContent() string {
	return m.newContent
}

// DiffFile represents a file in a multi-file diff.
type DiffFile struct {
	FilePath   string
	OldContent string
	NewContent string
	IsNewFile  bool
	Diff       string
}

// MultiDiffPreviewModel is the UI component for displaying multi-file diff previews.
type MultiDiffPreviewModel struct {
	files        []DiffFile
	currentIndex int
	viewport     viewport.Model
	decisions    map[int]DiffDecision
	styles       *Styles
	width        int
	height       int
	focusOnList  bool // true if focus is on file list, false if on diff

	// Callback when user makes decisions
	onComplete func(decisions map[string]DiffDecision)
}

// MultiDiffPreviewRequestMsg is sent to request a multi-file diff preview.
type MultiDiffPreviewRequestMsg struct {
	Files []DiffFile
}

// MultiDiffPreviewResponseMsg is sent when user completes multi-file decisions.
type MultiDiffPreviewResponseMsg struct {
	Decisions map[string]DiffDecision
}

// NewMultiDiffPreviewModel creates a new multi-file diff preview model.
func NewMultiDiffPreviewModel(styles *Styles) MultiDiffPreviewModel {
	vp := viewport.New(80, 20)
	vp.MouseWheelEnabled = true

	return MultiDiffPreviewModel{
		viewport:    vp,
		styles:      styles,
		decisions:   make(map[int]DiffDecision),
		focusOnList: true,
	}
}

// SetSize sets the size of the multi-diff preview.
func (m *MultiDiffPreviewModel) SetSize(width, height int) {
	if width < 20 {
		width = 80
	}
	if height < 15 {
		height = 24
	}
	m.width = width
	m.height = height
	// Viewport takes 2/3 of width (right side)
	m.viewport.Width = (width * 2 / 3) - 4
	m.viewport.Height = height - 10
}

// SetFiles sets the files to display.
func (m *MultiDiffPreviewModel) SetFiles(files []DiffFile) {
	m.files = files
	m.currentIndex = 0
	m.decisions = make(map[int]DiffDecision)

	// Initialize all decisions to pending
	for i := range files {
		m.decisions[i] = DiffPending
	}

	// Generate diffs for all files
	for i := range m.files {
		m.files[i].Diff = m.generateDiff(i)
	}

	// Show first file's diff
	if len(m.files) > 0 {
		m.updateViewport()
	}
}

// SetCompleteCallback sets the callback for when user completes decisions.
func (m *MultiDiffPreviewModel) SetCompleteCallback(callback func(map[string]DiffDecision)) {
	m.onComplete = callback
}

// generateDiff creates a diff for the file at the given index.
func (m *MultiDiffPreviewModel) generateDiff(index int) string {
	if index < 0 || index >= len(m.files) {
		return ""
	}

	file := m.files[index]
	dmp := diffmatchpatch.New()

	var result strings.Builder
	result.WriteString(fmt.Sprintf("--- %s\n", file.FilePath))
	result.WriteString(fmt.Sprintf("+++ %s\n", file.FilePath))

	diffs := dmp.DiffMain(file.OldContent, file.NewContent, true)
	diffs = dmp.DiffCleanupSemantic(diffs)

	for _, d := range diffs {
		lines := strings.Split(d.Text, "\n")
		for i, line := range lines {
			if i == len(lines)-1 && line == "" {
				continue
			}
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				result.WriteString(fmt.Sprintf(" %s\n", line))
			case diffmatchpatch.DiffDelete:
				result.WriteString(fmt.Sprintf("-%s\n", line))
			case diffmatchpatch.DiffInsert:
				result.WriteString(fmt.Sprintf("+%s\n", line))
			}
		}
	}

	return result.String()
}

// highlightMultiDiff applies syntax highlighting to the diff.
func (m *MultiDiffPreviewModel) highlightMultiDiff(diff string) string {
	addedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Bold(true)
	removedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Bold(true)
	headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#06B6D4")).Bold(true)
	contextStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))

	lines := strings.Split(diff, "\n")
	var result strings.Builder

	for _, line := range lines {
		var styledLine string
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			styledLine = headerStyle.Render(line)
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

// updateViewport updates the viewport with the current file's diff.
func (m *MultiDiffPreviewModel) updateViewport() {
	if m.currentIndex >= 0 && m.currentIndex < len(m.files) {
		diff := m.files[m.currentIndex].Diff
		m.viewport.SetContent(m.highlightMultiDiff(diff))
		m.viewport.GotoTop()
	}
}

// Init initializes the multi-diff preview model.
func (m MultiDiffPreviewModel) Init() tea.Cmd {
	return nil
}

// Update handles input events for the multi-file diff preview.
func (m MultiDiffPreviewModel) Update(msg tea.Msg) (MultiDiffPreviewModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab":
			// Toggle focus between file list and diff
			m.focusOnList = !m.focusOnList
			return m, nil

		case "up", "k":
			if m.focusOnList {
				// Move up in file list
				if m.currentIndex > 0 {
					m.currentIndex--
					m.updateViewport()
				}
			} else {
				// Scroll diff up
				m.viewport, cmd = m.viewport.Update(tea.KeyMsg{Type: tea.KeyUp})
			}
			return m, cmd

		case "down", "j":
			if m.focusOnList {
				// Move down in file list
				if m.currentIndex < len(m.files)-1 {
					m.currentIndex++
					m.updateViewport()
				}
			} else {
				// Scroll diff down
				m.viewport, cmd = m.viewport.Update(tea.KeyMsg{Type: tea.KeyDown})
			}
			return m, cmd

		case "y":
			// Accept current file
			m.decisions[m.currentIndex] = DiffApply
			// Move to next pending file
			m.moveToNextPending()
			return m, nil

		case "n":
			// Reject current file
			m.decisions[m.currentIndex] = DiffReject
			// Move to next pending file
			m.moveToNextPending()
			return m, nil

		case "Y":
			// Accept all
			for i := range m.files {
				m.decisions[i] = DiffApply
			}
			return m, m.finish()

		case "N":
			// Reject all
			for i := range m.files {
				m.decisions[i] = DiffReject
			}
			return m, m.finish()

		case "enter":
			// Apply decisions and exit
			return m, m.finish()

		case "esc":
			// Cancel - reject all
			for i := range m.files {
				m.decisions[i] = DiffReject
			}
			return m, m.finish()

		case "g":
			if !m.focusOnList {
				m.viewport.GotoTop()
			}
			return m, nil

		case "G":
			if !m.focusOnList {
				m.viewport.GotoBottom()
			}
			return m, nil

		case "ctrl+d":
			if !m.focusOnList {
				m.viewport.HalfViewDown()
			}
			return m, nil

		case "ctrl+u":
			if !m.focusOnList {
				m.viewport.HalfViewUp()
			}
			return m, nil
		}

	case tea.MouseMsg:
		if !m.focusOnList {
			m.viewport, cmd = m.viewport.Update(msg)
		}
		return m, cmd

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil
	}

	return m, nil
}

// moveToNextPending moves to the next file with pending decision.
func (m *MultiDiffPreviewModel) moveToNextPending() {
	// First, try to find next pending after current
	for i := m.currentIndex + 1; i < len(m.files); i++ {
		if m.decisions[i] == DiffPending {
			m.currentIndex = i
			m.updateViewport()
			return
		}
	}
	// Then try from beginning
	for i := 0; i < m.currentIndex; i++ {
		if m.decisions[i] == DiffPending {
			m.currentIndex = i
			m.updateViewport()
			return
		}
	}
	// Stay on current if no pending found
}

// finish creates the completion message.
func (m *MultiDiffPreviewModel) finish() tea.Cmd {
	decisions := make(map[string]DiffDecision)
	for i, file := range m.files {
		decisions[file.FilePath] = m.decisions[i]
	}

	if m.onComplete != nil {
		m.onComplete(decisions)
	}

	return func() tea.Msg {
		return MultiDiffPreviewResponseMsg{
			Decisions: decisions,
		}
	}
}

// View renders the multi-file diff preview.
func (m MultiDiffPreviewModel) View() string {
	var builder strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight).
		Padding(0, 1)

	builder.WriteString(headerStyle.Render(fmt.Sprintf("📝 Multi-File Diff Preview (%d files)", len(m.files))))
	builder.WriteString("\n\n")

	// Calculate widths
	listWidth := m.width / 3
	if listWidth < 20 {
		listWidth = 20
	}

	// Render file list
	listStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Width(listWidth - 2)

	if m.focusOnList {
		listStyle = listStyle.BorderForeground(ColorHighlight)
	}

	var listContent strings.Builder
	listContent.WriteString(lipgloss.NewStyle().Bold(true).Render("Files"))
	listContent.WriteString("\n\n")

	for i, file := range m.files {
		var icon string
		switch m.decisions[i] {
		case DiffApply:
			icon = lipgloss.NewStyle().Foreground(ColorSuccess).Render("✓")
		case DiffReject:
			icon = lipgloss.NewStyle().Foreground(ColorError).Render("✗")
		default:
			icon = lipgloss.NewStyle().Foreground(ColorMuted).Render("?")
		}

		fileName := filepath.Base(file.FilePath)
		if len(fileName) > listWidth-8 {
			fileName = fileName[:listWidth-11] + "..."
		}

		lineStyle := lipgloss.NewStyle()
		if i == m.currentIndex {
			lineStyle = lineStyle.Background(lipgloss.Color("#374151")).Bold(true)
		}

		listContent.WriteString(lineStyle.Render(fmt.Sprintf(" %s %s", icon, fileName)))
		listContent.WriteString("\n")
	}

	// Render diff viewport
	viewportStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)

	if !m.focusOnList {
		viewportStyle = viewportStyle.BorderForeground(ColorHighlight)
	}

	// Current file info
	var fileInfo string
	if m.currentIndex >= 0 && m.currentIndex < len(m.files) {
		file := m.files[m.currentIndex]
		label := "Modified: "
		if file.IsNewFile {
			label = "New file: "
		}
		fileInfo = lipgloss.NewStyle().
			Foreground(ColorAccent).
			Bold(true).
			Render(label + file.FilePath)
	}

	// Layout: file list on left, diff on right
	leftPane := listStyle.Render(listContent.String())
	rightPane := viewportStyle.Render(m.viewport.View())

	builder.WriteString(fileInfo)
	builder.WriteString("\n\n")
	builder.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPane, " ", rightPane))
	builder.WriteString("\n\n")

	// Footer with actions
	m.renderMultiActions(&builder)

	return builder.String()
}

// renderMultiActions renders the action buttons for multi-file diff.
func (m MultiDiffPreviewModel) renderMultiActions(builder *strings.Builder) {
	applyStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(ColorSuccess).
		Padding(0, 1)

	rejectStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(ColorError).
		Padding(0, 1)

	allStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorText).
		Background(ColorBorder).
		Padding(0, 1)

	hintStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	// Count decisions
	accepted, rejected, pending := 0, 0, 0
	for _, d := range m.decisions {
		switch d {
		case DiffApply:
			accepted++
		case DiffReject:
			rejected++
		default:
			pending++
		}
	}

	// Status line
	statusStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	status := fmt.Sprintf("Accepted: %d | Rejected: %d | Pending: %d", accepted, rejected, pending)
	builder.WriteString(statusStyle.Render(status))
	builder.WriteString("\n\n")

	// Buttons
	builder.WriteString(applyStyle.Render("y Accept"))
	builder.WriteString("  ")
	builder.WriteString(rejectStyle.Render("n Reject"))
	builder.WriteString("  ")
	builder.WriteString(allStyle.Render("Y All"))
	builder.WriteString("  ")
	builder.WriteString(allStyle.Render("N None"))
	builder.WriteString("  ")
	builder.WriteString(applyStyle.Render("Enter Apply"))
	builder.WriteString("\n\n")

	builder.WriteString(hintStyle.Render("Tab: Switch focus | ↑/↓: Navigate | j/k: Scroll | Esc: Cancel"))
}

// GetDecisions returns the current decisions map.
func (m MultiDiffPreviewModel) GetDecisions() map[string]DiffDecision {
	decisions := make(map[string]DiffDecision)
	for i, file := range m.files {
		decisions[file.FilePath] = m.decisions[i]
	}
	return decisions
}
