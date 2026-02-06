package ui

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
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

	// Table buffering state: accumulate consecutive table lines before rendering
	inTable   bool
	tableRows []string

	// Inline code style (computed once from styles)
	inlineCodeStyle lipgloss.Style
}

// codeBlockStartRegex matches code block start: ```lang or ```lang:filename
var codeBlockStartRegex = regexp.MustCompile("^```(\\w*)(?::(.+))?$")

// inlineCodeRegex matches single backtick-wrapped inline code: `code`
// It avoids matching double backticks (which start code blocks).
var inlineCodeRegex = regexp.MustCompile("`([^`]+)`")

// unorderedListRegex matches unordered list items with leading whitespace: "  - item" or "  * item"
var unorderedListRegex = regexp.MustCompile(`^(\s*)([-*])\s+(.+)$`)

// orderedListRegex matches ordered list items with leading whitespace: "  1. item"
var orderedListRegex = regexp.MustCompile(`^(\s*)(\d+)\.\s+(.+)$`)

// tableRowRegex matches a markdown table row containing pipe separators.
var tableRowRegex = regexp.MustCompile(`^\s*\|.*\|\s*$`)

// tableSeparatorRegex matches a markdown table separator row: | --- | --- |
var tableSeparatorRegex = regexp.MustCompile(`^\s*\|[\s\-:|]+\|\s*$`)

// Markdown inline formatting regexes
var (
	headingRegex       = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	blockquoteRegex    = regexp.MustCompile(`^>\s?(.*)$`)
	horizontalRuleRegex = regexp.MustCompile(`^(---+|\*\*\*+|___+)\s*$`)
	boldRegex          = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRegex        = regexp.MustCompile(`(?:^|[^*])\*([^*]+?)\*(?:[^*]|$)`)
	linkRegex          = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	strikethroughRegex = regexp.MustCompile(`~~(.+?)~~`)
)

// NewMarkdownStreamParser creates a new streaming markdown parser.
func NewMarkdownStreamParser(styles *Styles) *MarkdownStreamParser {
	return &MarkdownStreamParser{
		highlighter: highlight.New("monokai"),
		styles:      styles,
		inlineCodeStyle: lipgloss.NewStyle().
			Background(lipgloss.Color("#2D2D2D")).
			Foreground(lipgloss.Color("#E06C75")).
			Padding(0, 1),
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
				// Flush any buffered table before entering code block
				if p.inTable {
					blocks = append(blocks, p.flushTable()...)
				}
				p.inCodeBlock = true
				p.codeLanguage = matches[1]
				if len(matches) > 2 {
					p.codeFilename = matches[2]
				}
				p.codeContent.Reset()
			} else if tableRowRegex.MatchString(line) {
				// Accumulate table rows
				p.inTable = true
				p.tableRows = append(p.tableRows, line)
			} else {
				// Non-table line: flush any buffered table first
				if p.inTable {
					blocks = append(blocks, p.flushTable()...)
				}

				// Render full markdown line (headings, blockquotes, hr, lists, inline)
				rendered := p.renderMarkdownLine(line)
				blocks = append(blocks, RenderedBlock{
					Content: rendered + "\n",
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

	// Flush any buffered table
	if p.inTable {
		blocks = append(blocks, p.flushTable()...)
	}

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
			Content: p.renderInlineCode(remaining),
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
	p.inTable = false
	p.tableRows = nil
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

// renderCodeBlockWithBorder renders a code block with clean thin borders.
// Top: ─── go ─── (language label centered in thin line)
// Bottom: ──────── (plain thin line)
func (p *MarkdownStreamParser) renderCodeBlockWithBorder(filename, lang, content string, width int) string {
	var result strings.Builder

	contentWidth := width - 4
	if contentWidth < 40 {
		contentWidth = 40
	}

	lines := strings.Split(content, "\n")

	// Determine label for the header
	label := lang
	if filename != "" {
		label = filename
		if lang != "" {
			label = filename + " · " + lang
		}
	}

	// Top border: ─── label ───
	if label != "" {
		labelLen := len(label) + 2 // spaces around label
		sideLen := (contentWidth - labelLen) / 2
		if sideLen < 3 {
			sideLen = 3
		}
		leftDash := strings.Repeat("─", sideLen)
		rightDash := strings.Repeat("─", contentWidth-sideLen-labelLen)
		if len(rightDash) < 0 {
			rightDash = "───"
		}
		result.WriteString(p.styles.Dim.Render(leftDash+" ") + p.styles.CodeBlockHeader.Render(label) + p.styles.Dim.Render(" "+rightDash))
	} else {
		result.WriteString(p.styles.Dim.Render(strings.Repeat("─", contentWidth)))
	}
	result.WriteString("\n")

	// Content — 2-space indent, no side borders, no line numbers
	for _, line := range lines {
		result.WriteString("  ")
		result.WriteString(line)
		result.WriteString("\n")
	}

	// Bottom border: thin line
	result.WriteString(p.styles.Dim.Render(strings.Repeat("─", contentWidth)))
	result.WriteString("\n")

	return result.String()
}

// renderInlineCode detects backtick-wrapped `code` segments within a text line
// and applies a distinct background color + monospace style using lipgloss.
// Double/triple backticks are not matched (those are code block fences).
func (p *MarkdownStreamParser) renderInlineCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}

	var result strings.Builder
	remaining := line

	for {
		loc := inlineCodeRegex.FindStringIndex(remaining)
		if loc == nil {
			result.WriteString(remaining)
			break
		}

		// Write text before the match
		result.WriteString(remaining[:loc[0]])

		// Extract the matched segment and the inner code text
		matched := remaining[loc[0]:loc[1]]
		// Strip the surrounding backticks to get inner text
		inner := matched[1 : len(matched)-1]

		// Apply inline code style
		result.WriteString(p.inlineCodeStyle.Render(inner))

		// Advance past the match
		remaining = remaining[loc[1]:]
	}

	return result.String()
}

// renderListItem detects unordered (-, *) and ordered (1.) list items,
// calculates nesting depth from leading whitespace, and renders with
// proper indentation and bullet/number styling.
// Returns the rendered string and true if the line is a list item, or ("", false) otherwise.
func (p *MarkdownStreamParser) renderListItem(line string) (string, bool) {
	bulletStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true)
	numberStyle := lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)

	// Check for unordered list: "  - item" or "  * item"
	if matches := unorderedListRegex.FindStringSubmatch(line); matches != nil {
		indent := matches[1]
		content := matches[3]
		depth := listDepth(indent)

		// Build indentation: 2 spaces per depth level
		prefix := strings.Repeat("  ", depth)

		// Use different bullet characters per depth level
		bullets := []string{"●", "○", "■", "□", "◆", "◇"}
		bullet := bullets[depth%len(bullets)]

		rendered := prefix + bulletStyle.Render(bullet) + " " + p.renderInlineCode(content)
		return rendered, true
	}

	// Check for ordered list: "  1. item"
	if matches := orderedListRegex.FindStringSubmatch(line); matches != nil {
		indent := matches[1]
		num := matches[2]
		content := matches[3]
		depth := listDepth(indent)

		// Build indentation: 2 spaces per depth level
		prefix := strings.Repeat("  ", depth)

		rendered := prefix + numberStyle.Render(num+".") + " " + p.renderInlineCode(content)
		return rendered, true
	}

	return "", false
}

// renderMarkdownLine renders a single line of markdown with full formatting support:
// headings, blockquotes, horizontal rules, lists, and inline formatting (bold, italic, etc.)
func (p *MarkdownStreamParser) renderMarkdownLine(line string) string {
	trimmed := strings.TrimSpace(line)

	// Horizontal rules: ---, ***, ___
	if horizontalRuleRegex.MatchString(trimmed) {
		hrStyle := lipgloss.NewStyle().Foreground(ColorDim)
		return hrStyle.Render(strings.Repeat("─", 40))
	}

	// Headings: # H1, ## H2, etc.
	if matches := headingRegex.FindStringSubmatch(trimmed); matches != nil {
		level := len(matches[1])
		text := matches[2]
		headingStyle := lipgloss.NewStyle().Bold(true)
		// Color by heading level
		switch level {
		case 1:
			headingStyle = headingStyle.Foreground(ColorAccent)
		case 2:
			headingStyle = headingStyle.Foreground(ColorSecondary)
		case 3:
			headingStyle = headingStyle.Foreground(ColorInfo)
		default:
			headingStyle = headingStyle.Foreground(ColorText)
		}
		prefix := strings.Repeat("#", level) + " "
		return headingStyle.Render(prefix + p.renderInlineFormatting(text))
	}

	// Blockquotes: > text
	if matches := blockquoteRegex.FindStringSubmatch(line); matches != nil {
		quoteStyle := lipgloss.NewStyle().Foreground(ColorMuted)
		barStyle := lipgloss.NewStyle().Foreground(ColorDim)
		content := p.renderInlineFormatting(matches[1])
		return barStyle.Render("│ ") + quoteStyle.Render(content)
	}

	// List items
	if rendered, ok := p.renderListItem(line); ok {
		return rendered
	}

	// Regular text with full inline formatting
	return p.renderInlineFormatting(line)
}

// renderInlineFormatting applies bold, italic, strikethrough, links, and inline code.
func (p *MarkdownStreamParser) renderInlineFormatting(line string) string {
	if line == "" {
		return line
	}

	// Apply inline code first (so code content isn't processed for other formatting)
	line = p.renderInlineCode(line)

	// Bold: **text**
	boldStyle := lipgloss.NewStyle().Bold(true)
	line = boldRegex.ReplaceAllStringFunc(line, func(match string) string {
		inner := boldRegex.FindStringSubmatch(match)
		if len(inner) > 1 {
			return boldStyle.Render(inner[1])
		}
		return match
	})

	// Italic: *text* (but not **text**)
	italicStyle := lipgloss.NewStyle().Italic(true)
	line = italicRegex.ReplaceAllStringFunc(line, func(match string) string {
		inner := italicRegex.FindStringSubmatch(match)
		if len(inner) > 1 {
			// Reconstruct with surrounding context chars from lookaround
			prefix := ""
			suffix := ""
			if len(match) > 0 && match[0] != '*' {
				prefix = string(match[0])
			}
			if len(match) > 0 && match[len(match)-1] != '*' {
				suffix = string(match[len(match)-1])
			}
			return prefix + italicStyle.Render(inner[1]) + suffix
		}
		return match
	})

	// Strikethrough: ~~text~~
	strikeStyle := lipgloss.NewStyle().Strikethrough(true)
	line = strikethroughRegex.ReplaceAllStringFunc(line, func(match string) string {
		inner := strikethroughRegex.FindStringSubmatch(match)
		if len(inner) > 1 {
			return strikeStyle.Render(inner[1])
		}
		return match
	})

	// Links: [text](url)
	linkStyle := lipgloss.NewStyle().Foreground(ColorInfo).Underline(true)
	line = linkRegex.ReplaceAllStringFunc(line, func(match string) string {
		inner := linkRegex.FindStringSubmatch(match)
		if len(inner) > 2 {
			return inner[1] + " (" + linkStyle.Render(inner[2]) + ")"
		}
		return match
	})

	return line
}

// listDepth calculates the nesting depth from leading whitespace.
// Each 2 spaces (or 1 tab) equals one level of depth.
func listDepth(indent string) int {
	spaces := 0
	for _, ch := range indent {
		if ch == '\t' {
			spaces += 2
		} else {
			spaces++
		}
	}
	return spaces / 2
}

// flushTable renders all buffered table rows as a formatted table block
// and resets the table state. Returns rendered blocks.
func (p *MarkdownStreamParser) flushTable() []RenderedBlock {
	if len(p.tableRows) == 0 {
		p.inTable = false
		return nil
	}

	rows := p.tableRows
	p.tableRows = nil
	p.inTable = false

	rendered := p.renderTable(rows)
	return []RenderedBlock{
		{
			Content: rendered + "\n",
			IsCode:  false,
		},
	}
}

// renderTable parses markdown table rows and renders them with aligned columns
// and box-drawing border characters.
func (p *MarkdownStreamParser) renderTable(rows []string) string {
	if len(rows) == 0 {
		return ""
	}

	// Parse all rows into cells
	type parsedRow struct {
		cells       []string
		isSeparator bool
	}

	var parsed []parsedRow
	for _, row := range rows {
		trimmed := strings.TrimSpace(row)
		isSep := tableSeparatorRegex.MatchString(row)

		// Split on | and trim the outer empty cells
		parts := strings.Split(trimmed, "|")
		var cells []string
		for i, part := range parts {
			// Skip empty leading/trailing parts from outer pipes
			if (i == 0 || i == len(parts)-1) && strings.TrimSpace(part) == "" {
				continue
			}
			cells = append(cells, strings.TrimSpace(part))
		}

		parsed = append(parsed, parsedRow{cells: cells, isSeparator: isSep})
	}

	if len(parsed) == 0 {
		return strings.Join(rows, "\n")
	}

	// Determine column count and max widths
	maxCols := 0
	for _, r := range parsed {
		if len(r.cells) > maxCols {
			maxCols = len(r.cells)
		}
	}
	if maxCols == 0 {
		return strings.Join(rows, "\n")
	}

	// Calculate the max visible width for each column
	colWidths := make([]int, maxCols)
	for _, r := range parsed {
		if r.isSeparator {
			continue
		}
		for j, cell := range r.cells {
			w := utf8.RuneCountInString(cell)
			if w > colWidths[j] {
				colWidths[j] = w
			}
		}
	}

	// Ensure minimum column width of 3
	for j := range colWidths {
		if colWidths[j] < 3 {
			colWidths[j] = 3
		}
	}

	borderStyle := p.styles.Dim
	headerStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true)

	var result strings.Builder

	// Top border: ╭───┬───┬───╮
	result.WriteString(borderStyle.Render("╭"))
	for j, w := range colWidths {
		result.WriteString(borderStyle.Render(strings.Repeat("─", w+2)))
		if j < maxCols-1 {
			result.WriteString(borderStyle.Render("┬"))
		}
	}
	result.WriteString(borderStyle.Render("╮"))
	result.WriteString("\n")

	// Render each row
	isHeader := true // First non-separator row is the header
	for _, r := range parsed {
		if r.isSeparator {
			// Separator row: ├───┼───┼───┤
			result.WriteString(borderStyle.Render("├"))
			for j, w := range colWidths {
				result.WriteString(borderStyle.Render(strings.Repeat("─", w+2)))
				if j < maxCols-1 {
					result.WriteString(borderStyle.Render("┼"))
				}
			}
			result.WriteString(borderStyle.Render("┤"))
			result.WriteString("\n")
			isHeader = false
			continue
		}

		// Data row: │ cell │ cell │ cell │
		result.WriteString(borderStyle.Render("│"))
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(r.cells) {
				cell = r.cells[j]
			}

			// Pad cell to column width
			cellWidth := utf8.RuneCountInString(cell)
			padding := colWidths[j] - cellWidth
			if padding < 0 {
				padding = 0
			}

			var styledCell string
			if isHeader {
				styledCell = headerStyle.Render(cell)
			} else {
				styledCell = p.renderInlineCode(cell)
			}

			result.WriteString(" ")
			result.WriteString(styledCell)
			result.WriteString(strings.Repeat(" ", padding))
			result.WriteString(" ")

			if j < maxCols-1 {
				result.WriteString(borderStyle.Render("│"))
			}
		}
		result.WriteString(borderStyle.Render("│"))
		result.WriteString("\n")
	}

	// Bottom border: ╰───┴───┴───╯
	result.WriteString(borderStyle.Render("╰"))
	for j, w := range colWidths {
		result.WriteString(borderStyle.Render(strings.Repeat("─", w+2)))
		if j < maxCols-1 {
			result.WriteString(borderStyle.Render("┴"))
		}
	}
	result.WriteString(borderStyle.Render("╯"))

	return result.String()
}

// ansiRegex matches ANSI escape codes for stripping in visible width calculation.
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visibleWidth calculates the visible width of a string (ignoring ANSI codes).
func visibleWidth(s string) int {
	clean := ansiRegex.ReplaceAllString(s, "")
	return len(clean)
}
