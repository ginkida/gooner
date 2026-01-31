package ui

import (
	"fmt"
	"regexp"
	"strings"

	"gokin/internal/highlight"
)

// RenderedBlock represents a rendered piece of content.
type RenderedBlock struct {
	Content  string
	IsCode   bool
	Language string
	Filename string
}

// MarkdownStreamParser parses streaming markdown and detects code blocks.
type MarkdownStreamParser struct {
	buffer       strings.Builder
	highlighter  *highlight.Highlighter
	inCodeBlock  bool
	codeLanguage string
	codeFilename string
	codeContent  strings.Builder
	styles       *Styles
}

// codeBlockStartRegex matches code block start: ```lang or ```lang:filename
var codeBlockStartRegex = regexp.MustCompile("^```(\\w*)(?::(.+))?$")

// NewMarkdownStreamParser creates a new streaming markdown parser.
func NewMarkdownStreamParser(styles *Styles) *MarkdownStreamParser {
	return &MarkdownStreamParser{
		highlighter: highlight.New("monokai"),
		styles:      styles,
	}
}

// Feed processes a chunk of text and returns any completed blocks.
func (p *MarkdownStreamParser) Feed(chunk string) []RenderedBlock {
	var blocks []RenderedBlock

	// Add chunk to buffer
	p.buffer.WriteString(chunk)
	content := p.buffer.String()

	// Handle empty content
	if content == "" {
		return blocks
	}

	// Process line by line, keeping incomplete lines in buffer
	lines := strings.Split(content, "\n")
	var processedLen int

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		isLastLine := i == len(lines)-1

		// Keep incomplete last line in buffer (no newline at end)
		if isLastLine && !strings.HasSuffix(content, "\n") {
			break
		}

		// Skip empty trailing line from split (artifact of trailing newline)
		if isLastLine && line == "" && strings.HasSuffix(content, "\n") {
			break
		}

		processedLen += len(line) + 1 // +1 for newline

		if p.inCodeBlock {
			// Check for code block end
			if strings.TrimSpace(line) == "```" {
				// Flush code block
				codeContent := p.codeContent.String()
				if len(codeContent) > 0 && codeContent[len(codeContent)-1] == '\n' {
					codeContent = codeContent[:len(codeContent)-1] // Remove trailing newline
				}

				blocks = append(blocks, RenderedBlock{
					Content:  codeContent,
					IsCode:   true,
					Language: p.codeLanguage,
					Filename: p.codeFilename,
				})

				p.inCodeBlock = false
				p.codeLanguage = ""
				p.codeFilename = ""
				p.codeContent.Reset()
			} else {
				p.codeContent.WriteString(line)
				p.codeContent.WriteString("\n")
			}
		} else {
			// Check for code block start
			trimmed := strings.TrimSpace(line)
			if matches := codeBlockStartRegex.FindStringSubmatch(trimmed); matches != nil {
				p.inCodeBlock = true
				p.codeLanguage = matches[1]
				if len(matches) > 2 {
					p.codeFilename = matches[2]
				}
				p.codeContent.Reset()
			} else {
				// Regular text
				blocks = append(blocks, RenderedBlock{
					Content: line + "\n",
					IsCode:  false,
				})
			}
		}
	}

	// Update buffer with remaining content
	if processedLen > 0 {
		p.buffer.Reset()
		if processedLen < len(content) {
			p.buffer.WriteString(content[processedLen:])
		}
	}

	return blocks
}

// Flush returns any remaining content in the buffer.
func (p *MarkdownStreamParser) Flush() []RenderedBlock {
	var blocks []RenderedBlock

	// If we're in a code block, flush it (incomplete but better than nothing)
	if p.inCodeBlock {
		codeContent := p.codeContent.String()
		if len(codeContent) > 0 {
			blocks = append(blocks, RenderedBlock{
				Content:  codeContent,
				IsCode:   true,
				Language: p.codeLanguage,
				Filename: p.codeFilename,
			})
		}
		p.inCodeBlock = false
		p.codeLanguage = ""
		p.codeFilename = ""
		p.codeContent.Reset()
	}

	// Flush remaining buffer
	remaining := p.buffer.String()
	if remaining != "" {
		blocks = append(blocks, RenderedBlock{
			Content: remaining,
			IsCode:  false,
		})
		p.buffer.Reset()
	}

	return blocks
}

// Reset clears the parser state.
func (p *MarkdownStreamParser) Reset() {
	p.buffer.Reset()
	p.inCodeBlock = false
	p.codeLanguage = ""
	p.codeFilename = ""
	p.codeContent.Reset()
}

// RenderCodeBlock renders a code block with syntax highlighting and optional header.
func (p *MarkdownStreamParser) RenderCodeBlock(block RenderedBlock, width int) string {
	if !block.IsCode {
		return block.Content
	}

	var result strings.Builder

	// Detect language from filename if not specified
	lang := block.Language
	if lang == "" && block.Filename != "" {
		lang = p.highlighter.DetectLanguage(block.Filename)
	}

	// Apply syntax highlighting
	highlighted := block.Content
	if lang != "" {
		highlighted = p.highlighter.Highlight(block.Content, lang)
	}

	// Render the code block with border
	result.WriteString(p.renderCodeBlockWithBorder(block.Filename, lang, highlighted, width))

	return result.String()
}

// renderCodeBlockWithBorder renders a code block with rounded corners and line numbers.
func (p *MarkdownStreamParser) renderCodeBlockWithBorder(filename, lang, content string, width int) string {
	var result strings.Builder

	// Calculate width
	contentWidth := width - 4
	if contentWidth < 40 {
		contentWidth = 40
	}

	lines := strings.Split(content, "\n")
	lineCount := len(lines)

	// Calculate line number width
	lineNumWidth := len(fmt.Sprintf("%d", lineCount))
	if lineNumWidth < 2 {
		lineNumWidth = 2
	}

	// Header components
	filenameText := ""
	langText := ""
	if filename != "" {
		filenameText = filename
	}
	if lang != "" {
		langText = lang
	}

	// Top border with filename on left and language on right (rounded corners)
	topLeft := "╭─"
	topRight := "─╮"

	// Build header line
	var headerLine strings.Builder
	headerLine.WriteString(p.styles.Dim.Render(topLeft))

	if filenameText != "" {
		headerLine.WriteString(p.styles.Accent.Render(" " + filenameText + " "))
		usedWidth := len(filenameText) + 4
		remainingWidth := contentWidth - usedWidth

		if langText != "" && remainingWidth > len(langText)+4 {
			// Add separator and language on right
			sepWidth := remainingWidth - len(langText) - 3
			if sepWidth < 0 {
				sepWidth = 0
			}
			headerLine.WriteString(p.styles.Dim.Render(strings.Repeat("─", sepWidth)))
			headerLine.WriteString(p.styles.CodeBlockHeader.Render(" " + langText + " "))
		} else {
			headerLine.WriteString(p.styles.Dim.Render(strings.Repeat("─", remainingWidth-2)))
		}
	} else if langText != "" {
		headerLine.WriteString(p.styles.CodeBlockHeader.Render(" " + langText + " "))
		remainingWidth := contentWidth - len(langText) - 4
		if remainingWidth < 0 {
			remainingWidth = 0
		}
		headerLine.WriteString(p.styles.Dim.Render(strings.Repeat("─", remainingWidth)))
	} else {
		headerLine.WriteString(p.styles.Dim.Render(strings.Repeat("─", contentWidth)))
	}

	headerLine.WriteString(p.styles.Dim.Render(topRight))
	result.WriteString(headerLine.String())
	result.WriteString("\n")

	// Content with side borders and line numbers
	lineNumStyle := p.styles.Dim
	for i, line := range lines {
		lineNum := fmt.Sprintf("%*d", lineNumWidth, i+1)
		result.WriteString(p.styles.Dim.Render("│ "))
		result.WriteString(lineNumStyle.Render(lineNum + " │ "))
		result.WriteString(line)

		// Pad to width if needed
		lineWidth := visibleWidth(line)
		usedWidth := lineNumWidth + 5 + lineWidth
		if usedWidth < contentWidth {
			result.WriteString(strings.Repeat(" ", contentWidth-usedWidth))
		}
		result.WriteString(p.styles.Dim.Render(" │"))
		result.WriteString("\n")
	}

	// Bottom border (rounded corners)
	result.WriteString(p.styles.Dim.Render("╰" + strings.Repeat("─", contentWidth+2) + "╯"))
	result.WriteString("\n")

	return result.String()
}

// visibleWidth calculates the visible width of a string (ignoring ANSI codes).
func visibleWidth(s string) int {
	// Simple approximation - strip ANSI codes
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	clean := ansiRegex.ReplaceAllString(s, "")
	return len(clean)
}
