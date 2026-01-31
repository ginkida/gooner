package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// OutputModel represents the output/viewport component.
type OutputModel struct {
	viewport     viewport.Model
	styles       *Styles
	content      *strings.Builder
	renderer     *glamour.TermRenderer
	ready        bool
	width        int
	streamParser *MarkdownStreamParser
	codeBlocks   *CodeBlockRegistry
}

// NewOutputModel creates a new output model.
func NewOutputModel(styles *Styles) OutputModel {
	renderer, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(0), // Disable fixed word wrap, handled by viewport
	)

	return OutputModel{
		styles:       styles,
		renderer:     renderer,
		content:      &strings.Builder{},
		streamParser: NewMarkdownStreamParser(styles),
		codeBlocks:   NewCodeBlockRegistry(styles),
	}
}

// Init initializes the output model.
func (m OutputModel) Init() tea.Cmd {
	return nil
}

// Update handles output events.
func (m OutputModel) Update(msg tea.Msg) (OutputModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if !m.ready {
			// Calculate viewport height dynamically - use all available space except for input (3 lines) and status bar (2 lines)
			availableHeight := msg.Height - 5
			m.viewport = viewport.New(msg.Width, availableHeight)
			m.viewport.YPosition = 0
			m.viewport.MouseWheelEnabled = true
			m.ready = true
		} else {
			// Recalculate on resize
			availableHeight := msg.Height - 5
			m.viewport.Width = msg.Width
			m.viewport.Height = availableHeight
		}
	case tea.KeyMsg, tea.MouseMsg:
		// Forward key and mouse events to viewport for scrolling
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// View renders the output component.
func (m OutputModel) View() string {
	if !m.ready {
		return "Loading..."
	}
	return m.styles.Viewport.Render(m.viewport.View())
}

// AppendText appends text to the output.
func (m *OutputModel) AppendText(text string) {
	m.content.WriteString(text)
	m.updateViewport()
}

// AppendTextStream appends streaming text with markdown parsing and syntax highlighting.
func (m *OutputModel) AppendTextStream(text string) {
	if m.streamParser == nil {
		// Fallback to regular append
		m.AppendText(text)
		return
	}

	blocks := m.streamParser.Feed(text)
	for _, block := range blocks {
		if block.IsCode {
			// Register code block for later actions
			if m.codeBlocks != nil {
				lineNum := strings.Count(m.content.String(), "\n")
				m.codeBlocks.AddBlock(block.Language, block.Filename, block.Content, lineNum)
			}

			// Render with syntax highlighting and border
			rendered := m.streamParser.RenderCodeBlock(block, m.width)
			m.content.WriteString(rendered)
		} else {
			m.content.WriteString(block.Content)
		}
	}
	m.updateViewport()
}

// FlushStream flushes any remaining content in the stream parser.
func (m *OutputModel) FlushStream() {
	if m.streamParser == nil {
		return
	}

	blocks := m.streamParser.Flush()
	for _, block := range blocks {
		if block.IsCode {
			if m.codeBlocks != nil {
				lineNum := strings.Count(m.content.String(), "\n")
				m.codeBlocks.AddBlock(block.Language, block.Filename, block.Content, lineNum)
			}
			rendered := m.streamParser.RenderCodeBlock(block, m.width)
			m.content.WriteString(rendered)
		} else {
			m.content.WriteString(block.Content)
		}
	}
	m.updateViewport()
}

// ResetStream resets the stream parser state.
func (m *OutputModel) ResetStream() {
	if m.streamParser != nil {
		m.streamParser.Reset()
	}
}

// GetCodeBlocks returns the code block registry.
func (m *OutputModel) GetCodeBlocks() *CodeBlockRegistry {
	return m.codeBlocks
}

// AppendLine appends a line to the output.
func (m *OutputModel) AppendLine(text string) {
	m.content.WriteString(text)
	m.content.WriteString("\n")
	m.updateViewport()
}

// AppendMarkdown appends and renders markdown.
func (m *OutputModel) AppendMarkdown(text string) {
	if m.renderer != nil {
		rendered, err := m.renderer.Render(text)
		if err == nil {
			text = rendered
		}
	}
	m.content.WriteString(text)
	m.content.WriteString("\n")
	m.updateViewport()
}

// Clear clears the output.
func (m *OutputModel) Clear() {
	m.content.Reset()
	m.updateViewport()
}

// SetSize sets the viewport size.
func (m *OutputModel) SetSize(width, height int) {
	m.viewport.Width = width
	m.viewport.Height = height
	// Use more conservative padding to ensure text never hits the right edge
	m.width = width - 4
	m.ready = true
	m.updateViewport()
}

// ScrollToBottom scrolls to the bottom of the output.
func (m *OutputModel) ScrollToBottom() {
	m.viewport.GotoBottom()
}

func (m *OutputModel) updateViewport() {
	if m.ready {
		content := m.content.String()
		if m.width > 20 {
			content = wrapText(content, m.width)
		}
		m.viewport.SetContent(content)
		m.viewport.GotoBottom()
	}
}

// wrapText wraps text to the specified width using lipgloss for ANSI-aware wrapping.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}

	style := lipgloss.NewStyle().Width(width)

	var result strings.Builder
	lines := strings.Split(text, "\n")

	for i, line := range lines {
		if i > 0 {
			result.WriteString("\n")
		}
		if lipgloss.Width(line) > width {
			result.WriteString(style.Render(line))
		} else {
			result.WriteString(line)
		}
	}

	return result.String()
}

// Ready returns whether the viewport is ready.
func (m OutputModel) Ready() bool {
	return m.ready
}

// Content returns the current content.
func (m OutputModel) Content() string {
	return m.content.String()
}

// SetMouseEnabled enables or disables mouse wheel scrolling in the viewport.
func (m *OutputModel) SetMouseEnabled(enabled bool) {
	m.viewport.MouseWheelEnabled = enabled
}
