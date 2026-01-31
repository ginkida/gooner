package highlight

import (
	"bytes"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/lipgloss"
)

// Highlighter provides syntax highlighting for code and diffs.
type Highlighter struct {
	style     string
	formatter chroma.Formatter
}

// New creates a new Highlighter with the specified style.
// Supported styles: "monokai", "dracula", "github-dark", "native".
func New(style string) *Highlighter {
	if style == "" {
		style = "monokai"
	}

	return &Highlighter{
		style:     style,
		formatter: formatters.Get("terminal256"),
	}
}

// Highlight applies syntax highlighting to code based on language.
func (h *Highlighter) Highlight(code, lang string) string {
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	style := styles.Get(h.style)
	if style == nil {
		style = styles.Fallback
	}

	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return code
	}

	var buf bytes.Buffer
	if err := h.formatter.Format(&buf, style, iterator); err != nil {
		return code
	}

	return buf.String()
}

// HighlightWithLineNumbers highlights code with line numbers.
func (h *Highlighter) HighlightWithLineNumbers(code, lang string, startLine int) string {
	highlighted := h.Highlight(code, lang)
	lines := strings.Split(highlighted, "\n")

	lineNumStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))

	var result strings.Builder
	for i, line := range lines {
		lineNum := startLine + i
		result.WriteString(lineNumStyle.Render(padLeft(lineNum, 4)))
		result.WriteString(" │ ")
		result.WriteString(line)
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}

// HighlightDiff applies syntax highlighting to unified diff output.
func (h *Highlighter) HighlightDiff(diff string) string {
	// Define diff-specific colors
	addedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Bold(true)   // Green
	removedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Bold(true) // Red
	headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#06B6D4")).Bold(true)  // Cyan
	hunkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#A78BFA"))               // Purple
	contextStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))            // Gray

	lines := strings.Split(diff, "\n")
	var result strings.Builder

	for i, line := range lines {
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
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}

	return result.String()
}

// HighlightInlineDiff highlights added/removed spans within lines.
func (h *Highlighter) HighlightInlineDiff(oldLine, newLine string) (string, string) {
	// Guard clause for empty inputs
	if oldLine == "" && newLine == "" {
		return "", ""
	}
	if oldLine == "" {
		addedStyle := lipgloss.NewStyle().Background(lipgloss.Color("#064E3B")).Foreground(lipgloss.Color("#6EE7B7"))
		return "", addedStyle.Render(newLine)
	}
	if newLine == "" {
		removedStyle := lipgloss.NewStyle().Background(lipgloss.Color("#7F1D1D")).Foreground(lipgloss.Color("#FCA5A5"))
		return removedStyle.Render(oldLine), ""
	}

	addedStyle := lipgloss.NewStyle().Background(lipgloss.Color("#064E3B")).Foreground(lipgloss.Color("#6EE7B7"))
	removedStyle := lipgloss.NewStyle().Background(lipgloss.Color("#7F1D1D")).Foreground(lipgloss.Color("#FCA5A5"))

	// Find common prefix and suffix
	prefix := commonPrefix(oldLine, newLine)
	suffix := commonSuffix(oldLine[len(prefix):], newLine[len(prefix):])

	oldMiddle := oldLine[len(prefix) : len(oldLine)-len(suffix)]
	newMiddle := newLine[len(prefix) : len(newLine)-len(suffix)]

	highlightedOld := prefix + removedStyle.Render(oldMiddle) + suffix
	highlightedNew := prefix + addedStyle.Render(newMiddle) + suffix

	return highlightedOld, highlightedNew
}

// DetectLanguage detects the programming language from a filename.
func (h *Highlighter) DetectLanguage(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))

	// Common mappings
	langMap := map[string]string{
		".go":         "go",
		".py":         "python",
		".js":         "javascript",
		".ts":         "typescript",
		".tsx":        "tsx",
		".jsx":        "jsx",
		".rs":         "rust",
		".rb":         "ruby",
		".java":       "java",
		".c":          "c",
		".cpp":        "cpp",
		".h":          "c",
		".hpp":        "cpp",
		".cs":         "csharp",
		".php":        "php",
		".swift":      "swift",
		".kt":         "kotlin",
		".scala":      "scala",
		".sh":         "bash",
		".bash":       "bash",
		".zsh":        "bash",
		".fish":       "fish",
		".sql":        "sql",
		".html":       "html",
		".css":        "css",
		".scss":       "scss",
		".sass":       "sass",
		".less":       "less",
		".json":       "json",
		".yaml":       "yaml",
		".yml":        "yaml",
		".toml":       "toml",
		".xml":        "xml",
		".md":         "markdown",
		".markdown":   "markdown",
		".lua":        "lua",
		".r":          "r",
		".m":          "matlab",
		".pl":         "perl",
		".ex":         "elixir",
		".exs":        "elixir",
		".erl":        "erlang",
		".hs":         "haskell",
		".clj":        "clojure",
		".vim":        "vim",
		".dockerfile": "docker",
		".tf":         "terraform",
		".hcl":        "hcl",
		".proto":      "protobuf",
		".graphql":    "graphql",
		".gql":        "graphql",
	}

	if lang, ok := langMap[ext]; ok {
		return lang
	}

	// Check by filename
	base := strings.ToLower(filepath.Base(filename))
	filenameMap := map[string]string{
		"dockerfile":     "docker",
		"makefile":       "makefile",
		"cmakelists.txt": "cmake",
		"gemfile":        "ruby",
		"rakefile":       "ruby",
		".gitignore":     "gitignore",
		".env":           "ini",
		"go.mod":         "gomod",
		"go.sum":         "text",
		"cargo.toml":     "toml",
		"package.json":   "json",
		"tsconfig.json":  "json",
	}

	if lang, ok := filenameMap[base]; ok {
		return lang
	}

	// Try chroma's built-in detection
	lexer := lexers.Match(filename)
	if lexer != nil {
		return lexer.Config().Name
	}

	return "text"
}

// Helper functions

func padLeft(num, width int) string {
	s := strings.Repeat(" ", width)
	numStr := itoa(num)
	if len(numStr) >= width {
		return numStr
	}
	return s[:width-len(numStr)] + numStr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func commonPrefix(a, b string) string {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}

	for i := 0; i < minLen; i++ {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:minLen]
}

func commonSuffix(a, b string) string {
	// Guard clause for empty strings
	if len(a) == 0 || len(b) == 0 {
		return ""
	}

	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}

	for i := 0; i < minLen; i++ {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			if i == 0 {
				return ""
			}
			return a[len(a)-i:]
		}
	}
	return a[len(a)-minLen:]
}
