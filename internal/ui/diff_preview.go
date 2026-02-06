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

	// Configurable context lines (default 3)
	contextLines int

	// Ignore whitespace-only changes
	ignoreWhitespace bool

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
		viewport:     vp,
		styles:       styles,
		decision:     DiffPending,
		contextLines: 3,
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

	m.refreshDiffView()
}

// refreshDiffView regenerates and re-renders the diff with current settings.
func (m *DiffPreviewModel) refreshDiffView() {
	m.diff = m.generateDiff(m.oldContent, m.newContent)
	m.viewport.SetContent(m.highlightDiff(m.diff))
	m.viewport.GotoTop()
}

// SetDecisionCallback sets the callback for when user makes a decision.
func (m *DiffPreviewModel) SetDecisionCallback(callback func(DiffDecision)) {
	m.onDecision = callback
}

// diffLine represents a single line in the diff with its type.
type diffLine struct {
	Type diffmatchpatch.Operation
	Text string
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

	// Convert diffs to individual lines with their types
	var allLines []diffLine
	for _, d := range diffs {
		lines := strings.Split(d.Text, "\n")
		for i, line := range lines {
			if i == len(lines)-1 && line == "" {
				continue
			}
			allLines = append(allLines, diffLine{Type: d.Type, Text: line})
		}
	}

	// Build hunks with context lines
	contextN := m.contextLines
	if contextN < 0 {
		contextN = 0
	}

	// Find ranges of changed lines (non-Equal)
	type changeRange struct {
		start, end int // indices into allLines
	}
	var changes []changeRange
	i := 0
	for i < len(allLines) {
		if allLines[i].Type != diffmatchpatch.DiffEqual {
			start := i
			for i < len(allLines) && allLines[i].Type != diffmatchpatch.DiffEqual {
				i++
			}
			changes = append(changes, changeRange{start, i})
		} else {
			i++
		}
	}

	if len(changes) == 0 {
		return result.String()
	}

	// Merge change ranges that are close together (separated by <= 2*contextN equal lines)
	type hunkRange struct {
		start, end int // indices into allLines, including context
	}
	var hunks []hunkRange

	for _, ch := range changes {
		hStart := ch.start - contextN
		if hStart < 0 {
			hStart = 0
		}
		hEnd := ch.end + contextN
		if hEnd > len(allLines) {
			hEnd = len(allLines)
		}

		if len(hunks) > 0 && hStart <= hunks[len(hunks)-1].end {
			// Merge with previous hunk
			hunks[len(hunks)-1].end = hEnd
		} else {
			hunks = append(hunks, hunkRange{hStart, hEnd})
		}
	}

	// Compute old/new line numbers for each position in allLines
	oldLineNums := make([]int, len(allLines)+1)
	newLineNums := make([]int, len(allLines)+1)
	oldLine := 1
	newLine := 1
	for idx, dl := range allLines {
		oldLineNums[idx] = oldLine
		newLineNums[idx] = newLine
		switch dl.Type {
		case diffmatchpatch.DiffEqual:
			oldLine++
			newLine++
		case diffmatchpatch.DiffDelete:
			oldLine++
		case diffmatchpatch.DiffInsert:
			newLine++
		}
	}
	oldLineNums[len(allLines)] = oldLine
	newLineNums[len(allLines)] = newLine

	// Render each hunk
	for _, hunk := range hunks {
		// Calculate hunk header line counts
		oldStart := oldLineNums[hunk.start]
		newStart := newLineNums[hunk.start]
		oldCount := 0
		newCount := 0
		for idx := hunk.start; idx < hunk.end; idx++ {
			switch allLines[idx].Type {
			case diffmatchpatch.DiffEqual:
				oldCount++
				newCount++
			case diffmatchpatch.DiffDelete:
				oldCount++
			case diffmatchpatch.DiffInsert:
				newCount++
			}
		}

		// Check if this hunk is whitespace-only when ignoreWhitespace is enabled
		if m.ignoreWhitespace && isWhitespaceOnlyHunk(allLines[hunk.start:hunk.end]) {
			continue
		}

		// Find the nearest function/class/def line above the hunk start
		funcName := findNearestFuncName(allLines, hunk.start)

		// Write hunk header
		header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldCount, newStart, newCount)
		if funcName != "" {
			header += " " + funcName
		}
		result.WriteString(header + "\n")

		// Write hunk lines
		for idx := hunk.start; idx < hunk.end; idx++ {
			dl := allLines[idx]
			switch dl.Type {
			case diffmatchpatch.DiffEqual:
				result.WriteString(fmt.Sprintf(" %s\n", dl.Text))
			case diffmatchpatch.DiffDelete:
				result.WriteString(fmt.Sprintf("-%s\n", dl.Text))
			case diffmatchpatch.DiffInsert:
				result.WriteString(fmt.Sprintf("+%s\n", dl.Text))
			}
		}
	}

	return result.String()
}

// findNearestFuncName searches backward from the given position to find the nearest
// function, class, or def declaration in the context (equal) lines.
func findNearestFuncName(lines []diffLine, startIdx int) string {
	for i := startIdx - 1; i >= 0; i-- {
		text := lines[i].Text
		// Only consider equal (context) lines for function detection
		if lines[i].Type != diffmatchpatch.DiffEqual {
			continue
		}
		trimmed := strings.TrimSpace(text)
		for _, prefix := range []string{"func ", "class ", "def "} {
			if strings.HasPrefix(trimmed, prefix) {
				// Return the full signature up to the opening brace or end of line
				sig := trimmed
				if braceIdx := strings.Index(sig, "{"); braceIdx > 0 {
					sig = strings.TrimSpace(sig[:braceIdx])
				}
				return sig
			}
		}
	}
	return ""
}

// isWhitespaceOnlyHunk checks if a hunk contains only whitespace changes.
func isWhitespaceOnlyHunk(lines []diffLine) bool {
	hasChange := false
	for _, dl := range lines {
		if dl.Type == diffmatchpatch.DiffEqual {
			continue
		}
		hasChange = true
		trimmed := strings.TrimSpace(dl.Text)
		if trimmed != "" {
			return false
		}
	}
	return hasChange
}

// highlightDiff applies syntax highlighting to the diff with inline word-level highlighting.
func (m *DiffPreviewModel) highlightDiff(diff string) string {
	addedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Bold(true)
	removedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Bold(true)
	addedWordStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ECFDF5")).Background(lipgloss.Color("#059669")).Bold(true)
	removedWordStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FEF2F2")).Background(lipgloss.Color("#DC2626")).Bold(true)
	headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#06B6D4")).Bold(true)
	hunkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#A78BFA"))
	contextStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))

	lines := strings.Split(diff, "\n")
	var result strings.Builder

	// Pre-scan to pair consecutive removed/added line blocks for word-level diff
	paired := make(map[int]int) // maps removed line index -> added line index and vice versa

	i := 0
	for i < len(lines) {
		// Find a block of removed lines followed by added lines
		if strings.HasPrefix(lines[i], "-") && !strings.HasPrefix(lines[i], "---") {
			removeStart := i
			for i < len(lines) && strings.HasPrefix(lines[i], "-") && !strings.HasPrefix(lines[i], "---") {
				i++
			}
			removeEnd := i

			addStart := i
			for i < len(lines) && strings.HasPrefix(lines[i], "+") && !strings.HasPrefix(lines[i], "+++") {
				i++
			}
			addEnd := i

			// Pair up removed and added lines 1-to-1
			removeCount := removeEnd - removeStart
			addCount := addEnd - addStart
			pairCount := removeCount
			if addCount < pairCount {
				pairCount = addCount
			}
			for p := 0; p < pairCount; p++ {
				paired[removeStart+p] = addStart + p
				paired[addStart+p] = removeStart + p
			}
		} else {
			i++
		}
	}

	for idx, line := range lines {
		var styledLine string

		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			styledLine = headerStyle.Render(line)
		case strings.HasPrefix(line, "@@"):
			styledLine = hunkStyle.Render(line)
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			if partnerIdx, ok := paired[idx]; ok && strings.HasPrefix(lines[partnerIdx], "+") {
				// Word-level highlight for paired removed line
				oldText := line[1:] // strip the "-" prefix
				newText := lines[partnerIdx][1:]
				styledLine = removedStyle.Render("-") + highlightWordDiffs(oldText, newText, removedStyle, removedWordStyle)
			} else {
				styledLine = removedStyle.Render(line)
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if partnerIdx, ok := paired[idx]; ok && strings.HasPrefix(lines[partnerIdx], "-") {
				// Word-level highlight for paired added line
				oldText := lines[partnerIdx][1:]
				newText := line[1:]
				styledLine = addedStyle.Render("+") + highlightWordDiffs(newText, oldText, addedStyle, addedWordStyle)
			} else {
				styledLine = addedStyle.Render(line)
			}
		default:
			styledLine = contextStyle.Render(line)
		}

		result.WriteString(styledLine)
		result.WriteString("\n")
	}

	return result.String()
}

// highlightWordDiffs highlights the words in 'text' that differ from 'other'.
// baseStyle is applied to unchanged portions; emphStyle is applied to changed words.
// 'text' is the line we are rendering; 'other' is the counterpart line for comparison.
func highlightWordDiffs(text, other string, baseStyle, emphStyle lipgloss.Style) string {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(other, text, false)
	diffs = dmp.DiffCleanupSemantic(diffs)

	var result strings.Builder
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			result.WriteString(baseStyle.Render(d.Text))
		case diffmatchpatch.DiffInsert:
			// This text is unique to 'text' (the line we are rendering)
			result.WriteString(emphStyle.Render(d.Text))
		case diffmatchpatch.DiffDelete:
			// This text is unique to 'other' — skip it for this line's rendering
		}
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

		case "=": // "+" key (shift not needed on most keyboards, use "=" as alias for "+")
			m.contextLines++
			if m.contextLines > 20 {
				m.contextLines = 20
			}
			m.refreshDiffView()
			return m, nil

		case "-":
			m.contextLines--
			if m.contextLines < 0 {
				m.contextLines = 0
			}
			m.refreshDiffView()
			return m, nil

		case "I":
			m.ignoreWhitespace = !m.ignoreWhitespace
			m.refreshDiffView()
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

	// Diff statistics and settings
	addCount, removeCount := m.countChanges()
	addedStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
	removedStyle := lipgloss.NewStyle().Foreground(ColorError)
	settingsStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	stats := fmt.Sprintf("%s, %s",
		addedStyle.Render(fmt.Sprintf("+%d", addCount)),
		removedStyle.Render(fmt.Sprintf("-%d", removeCount)))
	wsLabel := "off"
	if m.ignoreWhitespace {
		wsLabel = "on"
	}
	settings := settingsStyle.Render(fmt.Sprintf("  context: %d | ignore-ws: %s", m.contextLines, wsLabel))
	builder.WriteString(markerStyle.Render("     ") + stats + settings)
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

	builder.WriteString(hintStyle.Render("j/k: Scroll | g/G: Top/Bottom | Ctrl+D/U: Half page | +/-: Context | I: Ignore whitespace"))
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
