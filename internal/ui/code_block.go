package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/lipgloss"
)

// CodeBlock represents a code block in the output.
type CodeBlock struct {
	ID           int
	Language     string
	Filename     string
	Content      string
	ViewportLine int // Line number in viewport for navigation
}

// CodeBlockRegistry manages code blocks for navigation and actions.
type CodeBlockRegistry struct {
	blocks           []CodeBlock
	selectedIndex    int
	styles           *Styles
	nextID           int
	showCopyFeedback bool
	copyFeedbackTime int64 // Unix timestamp when copy feedback was shown
}

// NewCodeBlockRegistry creates a new code block registry.
func NewCodeBlockRegistry(styles *Styles) *CodeBlockRegistry {
	return &CodeBlockRegistry{
		blocks:        make([]CodeBlock, 0),
		selectedIndex: -1,
		styles:        styles,
	}
}

// AddBlock adds a new code block to the registry.
func (r *CodeBlockRegistry) AddBlock(language, filename, content string, viewportLine int) int {
	id := r.nextID
	r.nextID++

	r.blocks = append(r.blocks, CodeBlock{
		ID:           id,
		Language:     language,
		Filename:     filename,
		Content:      content,
		ViewportLine: viewportLine,
	})

	// Auto-select first block
	if r.selectedIndex < 0 {
		r.selectedIndex = 0
	}

	return id
}

// SelectNext selects the next code block.
func (r *CodeBlockRegistry) SelectNext() bool {
	if len(r.blocks) == 0 {
		return false
	}
	if r.selectedIndex < len(r.blocks)-1 {
		r.selectedIndex++
		return true
	}
	return false
}

// SelectPrev selects the previous code block.
func (r *CodeBlockRegistry) SelectPrev() bool {
	if len(r.blocks) == 0 {
		return false
	}
	if r.selectedIndex > 0 {
		r.selectedIndex--
		return true
	}
	return false
}

// GetSelected returns the currently selected code block.
func (r *CodeBlockRegistry) GetSelected() *CodeBlock {
	if r.selectedIndex < 0 || r.selectedIndex >= len(r.blocks) {
		return nil
	}
	return &r.blocks[r.selectedIndex]
}

// GetSelectedIndex returns the current selection index.
func (r *CodeBlockRegistry) GetSelectedIndex() int {
	return r.selectedIndex
}

// Count returns the number of code blocks.
func (r *CodeBlockRegistry) Count() int {
	return len(r.blocks)
}

// Clear clears all code blocks.
func (r *CodeBlockRegistry) Clear() {
	r.blocks = r.blocks[:0]
	r.selectedIndex = -1
	r.nextID = 0
}

// CopySelected copies the selected code block content to clipboard.
func (r *CodeBlockRegistry) CopySelected() error {
	block := r.GetSelected()
	if block == nil {
		return fmt.Errorf("no code block selected")
	}
	err := clipboard.WriteAll(block.Content)
	if err == nil {
		r.ShowCopyFeedback()
	}
	return err
}

// ShowCopyFeedback triggers the copy feedback indicator.
func (r *CodeBlockRegistry) ShowCopyFeedback() {
	r.showCopyFeedback = true
	r.copyFeedbackTime = time.Now().Unix()
}

// IsCopyFeedbackVisible returns true if copy feedback should be shown.
func (r *CodeBlockRegistry) IsCopyFeedbackVisible() bool {
	if !r.showCopyFeedback {
		return false
	}
	// Show for 2 seconds
	if time.Now().Unix()-r.copyFeedbackTime > 2 {
		r.showCopyFeedback = false
		return false
	}
	return true
}

// RenderCopyFeedback returns the copy feedback indicator.
func (r *CodeBlockRegistry) RenderCopyFeedback() string {
	if !r.IsCopyFeedbackVisible() {
		return ""
	}
	feedbackStyle := lipgloss.NewStyle().
		Foreground(ColorSuccess).
		Bold(true)
	return feedbackStyle.Render(" Copied!")
}

// GetAll returns all code blocks.
func (r *CodeBlockRegistry) GetAll() []CodeBlock {
	return r.blocks
}

// RenderBlockHeader renders the header for a code block with actions.
func (r *CodeBlockRegistry) RenderBlockHeader(block CodeBlock, width int, isSelected bool) string {
	var result strings.Builder

	// Header content
	headerText := ""
	if block.Filename != "" {
		headerText = block.Filename
	} else if block.Language != "" {
		headerText = block.Language
	}

	// Calculate widths
	contentWidth := width - 4
	if contentWidth < 40 {
		contentWidth = 40
	}

	// Build header
	topLeft := "┌─"
	topRight := "─┐"

	// Style based on selection
	var headerStyle, borderStyle lipgloss.Style
	if isSelected {
		headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSecondary)
		borderStyle = lipgloss.NewStyle().Foreground(ColorSecondary)
	} else {
		headerStyle = r.styles.Accent
		borderStyle = r.styles.Dim
	}

	// Build the header line
	result.WriteString(borderStyle.Render(topLeft))

	if headerText != "" {
		result.WriteString(headerStyle.Render(" " + headerText + " "))
	}

	// Calculate remaining space
	headerLen := 0
	if headerText != "" {
		headerLen = len(headerText) + 2
	}

	fillerLen := contentWidth - headerLen - 2
	if fillerLen < 1 {
		fillerLen = 1
	}

	result.WriteString(borderStyle.Render(strings.Repeat("─", fillerLen)))
	result.WriteString(borderStyle.Render(topRight))

	return result.String()
}

// RenderSelectionIndicator renders an indicator showing current selection.
func (r *CodeBlockRegistry) RenderSelectionIndicator() string {
	if len(r.blocks) == 0 {
		return ""
	}

	return fmt.Sprintf("Code block %d/%d", r.selectedIndex+1, len(r.blocks))
}

// RenderBlockWithLineNumbers renders a code block with line numbers.
func (r *CodeBlockRegistry) RenderBlockWithLineNumbers(block CodeBlock, width int, isSelected bool) string {
	lines := strings.Split(block.Content, "\n")
	maxLineNum := len(lines)
	lineNumWidth := len(fmt.Sprintf("%d", maxLineNum))

	var result strings.Builder

	// Header with language badge
	result.WriteString(r.RenderBlockHeader(block, width, isSelected))
	result.WriteString("\n")

	// Line number style
	lineNumStyle := lipgloss.NewStyle().Foreground(ColorDim)
	separatorStyle := lipgloss.NewStyle().Foreground(ColorBorder)
	codeStyle := lipgloss.NewStyle().Foreground(ColorText)

	// Render each line with line number
	for i, line := range lines {
		lineNum := fmt.Sprintf("%*d", lineNumWidth, i+1)
		result.WriteString(lineNumStyle.Render(lineNum))
		result.WriteString(separatorStyle.Render(" │ "))
		result.WriteString(codeStyle.Render(line))
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}

	// Footer with actions (if selected)
	if isSelected {
		result.WriteString("\n")
		result.WriteString(r.RenderBlockFooter(block, width))
	}

	return result.String()
}

// RenderBlockFooter renders the footer with actions for a code block.
func (r *CodeBlockRegistry) RenderBlockFooter(block CodeBlock, width int) string {
	var result strings.Builder

	borderStyle := lipgloss.NewStyle().Foreground(ColorBorder)

	// Bottom border
	contentWidth := width - 4
	if contentWidth < 40 {
		contentWidth = 40
	}

	result.WriteString(borderStyle.Render("└"))

	// Copy feedback (only shown briefly after copy)
	if r.IsCopyFeedbackVisible() {
		feedback := lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true).Render(" ✓ Copied! ")
		result.WriteString(borderStyle.Render(strings.Repeat("─", 2)))
		result.WriteString(feedback)
		remainingWidth := contentWidth - 3 - lipgloss.Width(feedback)
		if remainingWidth > 0 {
			result.WriteString(borderStyle.Render(strings.Repeat("─", remainingWidth)))
		}
	} else {
		result.WriteString(borderStyle.Render(strings.Repeat("─", contentWidth-1)))
	}

	result.WriteString(borderStyle.Render("┘"))

	return result.String()
}

// RenderLanguageBadge renders a badge showing the language.
func (r *CodeBlockRegistry) RenderLanguageBadge(language string) string {
	if language == "" {
		return ""
	}

	badgeStyle := lipgloss.NewStyle().
		Background(ColorDim).
		Foreground(ColorText).
		Padding(0, 1).
		Bold(true)

	return badgeStyle.Render(language)
}
