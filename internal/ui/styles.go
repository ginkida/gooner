package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Colors for the UI theme - Muted Professional Palette
var (
	ColorPrimary   = lipgloss.Color("#A78BFA") // Soft Purple (Lavender 400)
	ColorSecondary = lipgloss.Color("#22D3EE") // Bright Cyan (Cyan 400)
	ColorSuccess   = lipgloss.Color("#059669") // Emerald 600 (muted green)
	ColorWarning   = lipgloss.Color("#D97706") // Amber 600 (muted amber)
	ColorError     = lipgloss.Color("#DC2626") // Red 600 (muted red)
	ColorMuted     = lipgloss.Color("#9CA3AF") // Neutral Gray (Gray 400)
	ColorText      = lipgloss.Color("#F1F5F9") // Soft White (Slate 100)
	ColorBg        = lipgloss.Color("#0F172A") // Deep Navy (Slate 900)

	// Extended semantic colors
	ColorBorder    = lipgloss.Color("#1E293B") // Subtle Slate Border
	ColorHighlight = lipgloss.Color("#E9D5FF") // Soft Purple (Purple 200)
	ColorDim       = lipgloss.Color("#6B7280") // Gray 500 (slightly lighter)
	ColorAccent    = lipgloss.Color("#F472B6") // Pink Accent (Pink 400)
	ColorRunning   = lipgloss.Color("#60A5FA") // Sky Blue (Blue 400)
	ColorInfo      = lipgloss.Color("#2DD4BF") // Teal Info (Teal 400)

	// Modal semantic colors
	ColorContext  = lipgloss.Color("#CBD5E1") // Slate 300
	ColorQuestion = ColorSecondary            // Cyan
	ColorPlan     = ColorInfo                 // Teal

	// Gradient colors (used sparingly)
	ColorGradient1 = lipgloss.Color("#C084FC") // Purple 500
	ColorGradient2 = lipgloss.Color("#818CF8") // Indigo 500
	ColorGradient3 = lipgloss.Color("#38BDF8") // Sky 500
	ColorGolden    = lipgloss.Color("#FCD34D") // Amber 300
	ColorRose      = lipgloss.Color("#FB7185") // Rose 400
	ColorMint      = lipgloss.Color("#6EE7B7") // Emerald 300
)

// MessageIcons provides consistent icons for different message types
var MessageIcons = map[string]string{
	"success": "✓",
	"error":   "✗",
	"warning": "⚠",
	"info":    "ℹ",
	"hint":    "💡",
	"loading": "◐",
	"done":    "✨",
	"pending": "○",
	"active":  "●",
	"skip":    "↷",
}

// ToolIcons provides contextual icons for different tool calls - Enhanced with more expressive icons
var ToolIcons = map[string]string{
	"read":           "📄",
	"write":          "✨",
	"edit":           "🔧",
	"bash":           "💻",
	"glob":           "📁",
	"grep":           "🔍",
	"todo":           "📋",
	"diff":           "📊",
	"tree":           "🌲",
	"web_fetch":      "🌍",
	"web_search":     "🔎",
	"list_files":     "📂",
	"file_search":    "🔍",
	"code_search":    "💡",
	"ask_question":   "🤔",
	"git_log":        "📜",
	"git_diff":       "🔄",
	"git_blame":      "👤",
	"commit":         "✅",
	"undo":           "↩️",
	"redo":           "↪️",
	"memory":         "🧠",
	"pattern_search": "🎯",
	"refactor":       "⚗️",
	"code_graph":     "🕸️",
	"batch":          "📦",
	"task":           "🎬",
	"test":           "🧪",
	"build":          "🔨",
	"default":        "⚙️",
}

// GetToolIcon returns the icon for a given tool name.
func GetToolIcon(toolName string) string {
	// Normalize tool name (lowercase, underscores)
	normalized := strings.ToLower(strings.ReplaceAll(toolName, "-", "_"))

	if icon, ok := ToolIcons[normalized]; ok {
		return icon
	}
	return ToolIcons["default"]
}

// GetToolIconColor returns the semantic color for a given tool name.
func GetToolIconColor(toolName string) lipgloss.Color {
	// Normalize tool name
	normalized := strings.ToLower(strings.ReplaceAll(toolName, "-", "_"))

	// Tool-specific semantic colors
	colors := map[string]lipgloss.Color{
		"read":           ColorPrimary,   // Purple - file operations
		"write":          ColorSuccess,   // Green - creation
		"edit":           ColorWarning,   // Amber - modification
		"bash":           ColorRunning,   // Blue - execution
		"glob":           ColorSecondary, // Cyan - search
		"grep":           ColorInfo,      // Teal - pattern search
		"todo":           ColorAccent,    // Pink - tasks
		"diff":           ColorPrimary,   // Purple - comparison
		"tree":           ColorSuccess,   // Green - structure
		"web_fetch":      ColorGradient2, // Indigo - network
		"web_search":     ColorGradient2, // Indigo - network
		"git_log":        ColorWarning,   // Amber - history
		"git_diff":       ColorPrimary,   // Purple - changes
		"git_blame":      ColorSecondary, // Cyan - attribution
		"commit":         ColorSuccess,   // Green - save
		"memory":         ColorGradient1, // Purple - storage
		"pattern_search": ColorInfo,      // Teal - patterns
		"refactor":       ColorWarning,   // Amber - transformation
		"code_graph":     ColorGradient3, // Sky - visualization
		"batch":          ColorAccent,    // Pink - bulk
		"test":           ColorRunning,   // Blue - testing
		"build":          ColorWarning,   // Amber - compilation
	}

	if color, ok := colors[normalized]; ok {
		return color
	}
	return ColorMuted // Default gray
}

// Styles contains all UI styles.
type Styles struct {
	App           lipgloss.Style
	Header        lipgloss.Style
	UserPrompt    lipgloss.Style
	AssistantText lipgloss.Style
	ToolCall      lipgloss.Style
	ToolResult    lipgloss.Style
	Error         lipgloss.Style
	Warning       lipgloss.Style
	Spinner       lipgloss.Style
	StatusBar     lipgloss.Style
	Input         lipgloss.Style
	Viewport      lipgloss.Style
	TodoItem      lipgloss.Style
	TodoPending   lipgloss.Style
	TodoActive    lipgloss.Style
	TodoDone      lipgloss.Style

	// Box styles for structured output
	InfoBox    lipgloss.Style
	WarningBox lipgloss.Style
	SuccessBox lipgloss.Style
	ErrorBox   lipgloss.Style

	// Additional styles
	Dim       lipgloss.Style
	Highlight lipgloss.Style
	Accent    lipgloss.Style

	// Modal styles (unified across question prompt, plan approval, model selector)
	ModalTitle    lipgloss.Style // Bold title with ColorQuestion/ColorPlan
	ModalSelected lipgloss.Style // Selected option (bold + ColorSecondary)
	ModalNormal   lipgloss.Style // Normal option (ColorMuted)
	ModalMuted    lipgloss.Style // Dimmed text (ColorDim + italic)
	ModalDefault  lipgloss.Style // Default indicator (ColorContext + italic)

	// Code block styles
	CodeBlockBorder   lipgloss.Style // Border for code blocks
	CodeBlockHeader   lipgloss.Style // Header with filename/language
	CodeBlockSelected lipgloss.Style // Selected code block border
	CodeBlockActions  lipgloss.Style // Action hints in header

	// Tool execution block styles
	ToolBlock     lipgloss.Style // Container for entire tool execution
	ToolHeader    lipgloss.Style // Header with icon + name
	ToolContent   lipgloss.Style // Tool output content
	ToolSeparator lipgloss.Style // Separator line between tools
	ToolSpinner   lipgloss.Style // Animated spinner for active tools
	ToolTiming    lipgloss.Style // Timing information

	// Segmented Status Bar styles
	StatusSegment      lipgloss.Style
	StatusSegmentBold  lipgloss.Style
	StatusSeparator    lipgloss.Style
	StatusSectionName  lipgloss.Style
	StatusSectionValue lipgloss.Style

	// Card styles for message grouping
	UserCard      lipgloss.Style
	AssistantCard lipgloss.Style
}

// DefaultStyles returns the default UI styles.
func DefaultStyles() *Styles {
	return &Styles{
		App: lipgloss.NewStyle(),

		Header: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			MarginBottom(1),

		UserPrompt: lipgloss.NewStyle().
			Foreground(ColorSecondary).
			Bold(true),

		AssistantText: lipgloss.NewStyle().
			Foreground(ColorText),

		ToolCall: lipgloss.NewStyle().
			Foreground(ColorWarning).
			Italic(true),

		ToolResult: lipgloss.NewStyle().
			Foreground(ColorMuted).
			MarginLeft(2),

		Error: lipgloss.NewStyle().
			Foreground(ColorError).
			Bold(true),

		Warning: lipgloss.NewStyle().
			Foreground(ColorWarning).
			Bold(true),

		Spinner: lipgloss.NewStyle().
			Foreground(ColorPrimary),

		StatusBar: lipgloss.NewStyle().
			Foreground(ColorMuted).
			Background(ColorBg).
			Padding(0, 1),

		Input: lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(ColorPrimary).
			Padding(0, 1),

		Viewport: lipgloss.NewStyle().
			MarginBottom(1),

		TodoItem: lipgloss.NewStyle().
			MarginLeft(2),

		TodoPending: lipgloss.NewStyle().
			Foreground(ColorMuted),

		TodoActive: lipgloss.NewStyle().
			Foreground(ColorWarning).
			Bold(true),

		TodoDone: lipgloss.NewStyle().
			Foreground(ColorSuccess),

		// Box styles for structured output
		InfoBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorInfo).
			Padding(0, 1).
			MarginTop(1).
			MarginBottom(1),

		WarningBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorWarning).
			Padding(0, 1).
			MarginTop(1).
			MarginBottom(1),

		SuccessBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorSuccess).
			Padding(0, 1).
			MarginTop(1).
			MarginBottom(1),

		ErrorBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorError).
			Padding(0, 1).
			MarginTop(1).
			MarginBottom(1),

		// Additional styles
		Dim: lipgloss.NewStyle().
			Foreground(ColorDim),

		Highlight: lipgloss.NewStyle().
			Foreground(ColorHighlight),

		Accent: lipgloss.NewStyle().
			Foreground(ColorAccent),

		// Modal styles (unified across prompts)
		ModalTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSecondary).
			Padding(0, 1),

		ModalSelected: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorSecondary),

		ModalNormal: lipgloss.NewStyle().
			Foreground(ColorMuted),

		ModalMuted: lipgloss.NewStyle().
			Foreground(ColorDim).
			Italic(true),

		ModalDefault: lipgloss.NewStyle().
			Foreground(ColorContext).
			Italic(true),

		// Code block styles
		CodeBlockBorder: lipgloss.NewStyle().
			Foreground(ColorBorder),

		CodeBlockHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent),

		CodeBlockSelected: lipgloss.NewStyle().
			Foreground(ColorSecondary).
			Bold(true),

		CodeBlockActions: lipgloss.NewStyle().
			Foreground(ColorInfo),

		// Tool execution block styles
		ToolBlock: lipgloss.NewStyle().
			MarginTop(1).
			MarginBottom(1).
			Padding(0, 1),

		ToolHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			MarginBottom(1),

		ToolContent: lipgloss.NewStyle().
			Foreground(ColorText).
			PaddingLeft(2),

		ToolSeparator: lipgloss.NewStyle().
			Foreground(ColorDim).
			MarginTop(1).
			MarginBottom(1),

		ToolSpinner: lipgloss.NewStyle().
			Foreground(ColorInfo).
			Bold(true),

		ToolTiming: lipgloss.NewStyle().
			Foreground(ColorMuted).
			Italic(true),

		// Segmented Status Bar (Powerline inspired)
		StatusSegment: lipgloss.NewStyle().
			Background(ColorBg).
			Padding(0, 1),

		StatusSegmentBold: lipgloss.NewStyle().
			Background(ColorBg).
			Bold(true).
			Padding(0, 1),

		StatusSeparator: lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(lipgloss.Color("#1E293B")), // Lighter slate for separator background

		StatusSectionName: lipgloss.NewStyle().
			Foreground(ColorDim).
			Bold(true),

		StatusSectionValue: lipgloss.NewStyle().
			Foreground(ColorText),

		// Card styles (Glassmorphism-lite)
		UserCard: lipgloss.NewStyle().
			Border(lipgloss.ThickBorder(), false, false, false, true).
			BorderForeground(ColorSecondary).
			PaddingLeft(2).
			MarginTop(1).
			MarginBottom(1),

		AssistantCard: lipgloss.NewStyle().
			Border(lipgloss.ThickBorder(), false, false, false, true).
			BorderForeground(ColorPrimary).
			PaddingLeft(2).
			MarginTop(1).
			MarginBottom(1),
	}
}

func (s *Styles) FormatUserMessage(msg string) string {
	promptStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true)
	contentStyle := lipgloss.NewStyle().Foreground(ColorText)

	lines := strings.Split(msg, "\n")
	var result strings.Builder
	for i, line := range lines {
		if i == 0 {
			result.WriteString(promptStyle.Render("❯ ") + contentStyle.Render(line))
		} else {
			result.WriteString("\n" + contentStyle.Render(line))
		}
	}
	return s.UserCard.Render(result.String())
}

// FormatAssistantMessage formats an assistant message.
func (s *Styles) FormatAssistantMessage(msg string) string {
	return s.AssistantCard.Render(s.AssistantText.Render(msg))
}

// FormatAssistantStreaming formats streaming assistant text (without header).
// Used during streaming to avoid repeating the header.
func (s *Styles) FormatAssistantStreaming(msg string) string {
	return s.AssistantText.Render(msg)
}

// FormatToolCall formats a tool call notification.
func (s *Styles) FormatToolCall(name string) string {
	icon := GetToolIcon(name)
	return s.ToolCall.Render(icon + " " + name)
}

// FormatToolCallWithArgs formats a tool call with brief argument summary.
func (s *Styles) FormatToolCallWithArgs(name string, args map[string]any) string {
	icon := GetToolIcon(name)
	summary := formatArgsSummary(args)
	if summary != "" {
		return s.ToolCall.Render(icon + " " + name + " " + summary)
	}
	return s.ToolCall.Render(icon + " " + name)
}

// FormatToolExecuting formats a tool that is currently executing.
func (s *Styles) FormatToolExecuting(name string, args map[string]any) string {
	icon := GetToolIcon(name)
	summary := formatArgsSummary(args)

	// Animated spinner for executing tools
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	idx := int(time.Now().Unix()/100) % len(spinner)

	exeStyle := lipgloss.NewStyle().
		Foreground(ColorInfo).
		Bold(true)

	if summary != "" {
		return exeStyle.Render(spinner[idx] + " " + icon + " " + name + " " + summary + "...")
	}
	return exeStyle.Render(spinner[idx] + " " + icon + " " + name + "...")
}

// FormatToolExecutingBlock formats a tool execution as a compact line.
// Format: ◦ ToolName(args)
func (s *Styles) FormatToolExecutingBlock(name string, args map[string]any) string {
	bulletStyle := lipgloss.NewStyle().Foreground(ColorDim)
	nameStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	argsStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder
	result.WriteString(bulletStyle.Render("◦ "))
	result.WriteString(nameStyle.Render(capitalizeToolName(name)))

	argsStr := buildClaudeCodeArgs(name, args)
	if argsStr != "" {
		result.WriteString(argsStyle.Render("(" + argsStr + ")"))
	}

	return result.String()
}

// FormatToolSuccess formats a successful tool result.
func (s *Styles) FormatToolSuccess(name string, duration time.Duration) string {
	icon := GetToolIcon(name)
	successStyle := lipgloss.NewStyle().
		Foreground(ColorSuccess)

	durationStr := ""
	if duration < time.Second {
		durationStr = fmt.Sprintf("%.0fms", float64(duration.Milliseconds()))
	} else if duration < time.Minute {
		durationStr = fmt.Sprintf("%.1fs", duration.Seconds())
	}

	if durationStr != "" {
		return successStyle.Render("✓ " + icon + " " + name + " (" + durationStr + ")")
	}
	return successStyle.Render("✓ " + icon + " " + name)
}

// FormatToolSuccessBlock formats a successful tool result as a compact line (Claude Code style).
// Format: ✓ icon name • summary • duration
func (s *Styles) FormatToolSuccessBlock(name string, duration time.Duration, resultSummary string) string {
	icon := GetToolIcon(name)
	iconColor := GetToolIconColor(name)

	checkStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
	iconStyle := lipgloss.NewStyle().Foreground(iconColor)
	nameStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
	summaryStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder

	// ✓ icon name (colored icon)
	result.WriteString(checkStyle.Render("✓ "))
	result.WriteString(iconStyle.Render(icon))
	result.WriteString(" ")
	result.WriteString(nameStyle.Render(name))

	// • summary
	if resultSummary != "" {
		result.WriteString(dimStyle.Render(" • "))
		result.WriteString(summaryStyle.Render(resultSummary))
	}

	// • duration
	durationStr := ""
	durationColor := ColorDim
	if duration < time.Second {
		durationStr = fmt.Sprintf("%dms", duration.Milliseconds())
	} else if duration < time.Minute {
		secs := duration.Seconds()
		durationStr = fmt.Sprintf("%.1fs", secs)
		if secs > 5 {
			durationColor = ColorWarning
		}
	} else {
		durationStr = fmt.Sprintf("%.1fm", duration.Minutes())
		durationColor = ColorWarning
	}

	if durationStr != "" {
		timingStyle := lipgloss.NewStyle().Foreground(durationColor)
		result.WriteString(dimStyle.Render(" • "))
		result.WriteString(timingStyle.Render(durationStr))
	}

	return result.String()
}

// FormatToolError formats a failed tool result.
func (s *Styles) FormatToolError(name string, err error) string {
	icon := GetToolIcon(name)
	errorStyle := lipgloss.NewStyle().
		Foreground(ColorError).
		Bold(true)

	return errorStyle.Render("✗ " + icon + " " + name + ": " + err.Error())
}

// formatArgsSummary creates a brief summary of tool arguments.
func formatArgsSummary(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	// Priority keys to show first
	priorityKeys := []string{"path", "file_path", "command", "pattern", "content", "query"}

	var parts []string
	shown := make(map[string]bool)

	// Show priority keys first
	for _, key := range priorityKeys {
		if val, ok := args[key]; ok {
			str := formatArgValue(val)
			if str != "" {
				parts = append(parts, key+"="+str)
				shown[key] = true
				if len(parts) >= 2 {
					break
				}
			}
		}
	}

	// Add other keys if we have room
	for key, val := range args {
		if shown[key] {
			continue
		}
		if len(parts) >= 2 {
			break
		}
		str := formatArgValue(val)
		if str != "" {
			parts = append(parts, key+"="+str)
		}
	}

	if len(parts) == 0 {
		return ""
	}

	result := "(" + lipgloss.NewStyle().Foreground(ColorMuted).Render(
		joinStrings(parts, ", "),
	) + ")"
	return result
}

// formatArgValue formats a single argument value for display.
func formatArgValue(val any) string {
	switch v := val.(type) {
	case string:
		// Don't truncate here - let the caller decide based on context
		// This allows file paths to be shown in full
		return "\"" + v + "\""
	case float64:
		return fmt.Sprintf("%.0f", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// formatArgValueWithLimit formats a value with a maximum length.
func formatArgValueWithLimit(val any, maxLen int) string {
	switch v := val.(type) {
	case string:
		if len(v) > maxLen {
			return "\"" + v[:maxLen-3] + "...\""
		}
		return "\"" + v + "\""
	case float64:
		return fmt.Sprintf("%.0f", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// joinStrings joins strings with a separator.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += sep + parts[i]
	}
	return result
}

// FormatError formats an error message with proper styling.
func (s *Styles) FormatError(err string) string {
	return s.FormatErrorWithSuggestion(err, "", "")
}

// FormatErrorWithSuggestion formats an error message with a suggestion - Claude Code style.
func (s *Styles) FormatErrorWithSuggestion(err, suggestion, code string) string {
	errorStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	msgStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FECACA"))
	suggestionStyle := lipgloss.NewStyle().Foreground(ColorWarning)
	codeStyle := lipgloss.NewStyle().Foreground(ColorDim)
	markerStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder
	result.WriteString(errorStyle.Render("✗ Error: ") + msgStyle.Render(err))

	if suggestion != "" {
		result.WriteString("\n" + markerStyle.Render("  ⎿  ") + suggestionStyle.Render(suggestion))
	}
	if code != "" {
		result.WriteString("\n" + markerStyle.Render("     ") + codeStyle.Render("("+code+")"))
	}
	return result.String()
}

// FormatSuccess formats a success message with proper styling - Enhanced with celebration.
func (s *Styles) FormatSuccess(msg string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSuccess).
		PaddingBottom(1)

	bodyStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#A7F3D0")). // Softer green (Emerald 200)
		PaddingLeft(2)

	header := headerStyle.Render("✨ " + "Success!")
	body := bodyStyle.Render(msg)

	return s.SuccessBox.Render(header + "\n" + body)
}

// FormatInfo formats an informational message - Enhanced with friendly styling.
func (s *Styles) FormatInfo(msg string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorInfo).
		PaddingBottom(1)

	bodyStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#A5F3FC")). // Softer cyan (Cyan 200)
		PaddingLeft(2)

	header := headerStyle.Render("ℹ️ " + "Info")
	body := bodyStyle.Render(msg)

	return s.InfoBox.Render(header + "\n" + body)
}

// FormatWarningBox formats a warning message in a box - Enhanced with friendly styling.
func (s *Styles) FormatWarningBox(msg string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorWarning).
		PaddingBottom(1)

	bodyStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FDE68A")). // Softer amber (Amber 200)
		PaddingLeft(2)

	header := headerStyle.Render("⚠️ " + "Heads up!")
	body := bodyStyle.Render(msg)

	return s.WarningBox.Render(header + "\n" + body)
}

// wrapErrorText wraps error text at word boundaries.
func wrapErrorText(text string, width int) string {
	if width <= 0 || len(text) <= width {
		return text
	}

	var result string
	var line string

	words := splitWords(text)
	for _, word := range words {
		// Handle newlines in the word
		if word == "\n" {
			result += line + "\n"
			line = ""
			continue
		}

		if len(line)+len(word)+1 > width && line != "" {
			result += line + "\n"
			line = word
		} else if line == "" {
			line = word
		} else {
			line += " " + word
		}
	}
	if line != "" {
		result += line
	}
	return result
}

// splitWords splits text into words, preserving newlines as separate tokens.
func splitWords(text string) []string {
	var words []string
	var current string

	for _, ch := range text {
		if ch == '\n' {
			if current != "" {
				words = append(words, current)
				current = ""
			}
			words = append(words, "\n")
		} else if ch == ' ' || ch == '\t' {
			if current != "" {
				words = append(words, current)
				current = ""
			}
		} else {
			current += string(ch)
		}
	}
	if current != "" {
		words = append(words, current)
	}
	return words
}

// extractToolDetails extracts detailed information from tool args.
func extractToolDetails(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	// Extract the most relevant detail based on arg types
	for _, key := range []string{"file_path", "path", "command", "pattern", "query", "url"} {
		if val, ok := args[key].(string); ok && val != "" {
			// Truncate if too long
			if len(val) > 60 {
				return val[:57] + "..."
			}
			return val
		}
	}

	return ""
}

// capitalizeToolName converts snake_case tool names to PascalCase.
// e.g., "web_fetch" → "WebFetch", "bash" → "Bash", "read" → "Read"
func capitalizeToolName(name string) string {
	if name == "" {
		return ""
	}
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// buildClaudeCodeArgs formats tool arguments for display in Claude Code style.
// Returns a string like "file_path=/path" or "command" for bash.
func buildClaudeCodeArgs(name string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	var result string
	isFilePath := false

	switch name {
	case "bash":
		if cmd, ok := args["command"]; ok {
			result = formatArgValue(cmd)
		}
	case "read", "write", "edit":
		if path, ok := args["file_path"]; ok {
			result = formatArgValue(path)
			isFilePath = true
		}
	case "grep":
		if pattern, ok := args["pattern"]; ok {
			result = "pattern=" + formatArgValue(pattern)
		}
		if path, ok := args["path"]; ok {
			result += " path=" + formatArgValue(path)
		}
	case "glob":
		if pattern, ok := args["pattern"]; ok {
			result = "pattern=" + formatArgValue(pattern)
		}
	case "web_fetch":
		if url, ok := args["url"]; ok {
			result = "url=" + formatArgValue(url)
		}
	case "web_search":
		if query, ok := args["query"]; ok {
			result = "query=" + formatArgValue(query)
		}
	default:
		// Use first priority arg
		for _, key := range []string{"file_path", "path", "command", "pattern", "query", "url"} {
			if val, ok := args[key]; ok {
				result = key + "=" + formatArgValue(val)
				if key == "file_path" || key == "path" {
					isFilePath = true
				}
				break
			}
		}
	}

	// Smart truncation: preserve filename for paths, truncate in middle
	maxLen := 120 // Increased from 80 for better visibility
	if len(result) > maxLen {
		if isFilePath {
			result = truncatePathSmart(result, maxLen)
		} else {
			result = result[:maxLen-3] + "..."
		}
	}

	return result
}

// truncatePathSmart truncates a file path while preserving the filename.
// For '"/home/user/projects/very/long/path/to/file.go"' with maxLen=50:
// Returns '"…/path/to/file.go"' preserving quotes and showing the filename
func truncatePathSmart(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}

	// Handle quoted paths
	hasQuotes := strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"")
	if hasQuotes {
		// Remove quotes for processing, will add back at the end
		path = path[1 : len(path)-1]
		maxLen -= 2 // Account for quotes
	}

	// Find the last path separator to get the filename
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		// No slash, just truncate normally
		result := path
		if len(result) > maxLen-3 {
			result = result[:maxLen-3] + "..."
		}
		if hasQuotes {
			return "\"" + result + "\""
		}
		return result
	}

	filename := path[lastSlash:] // includes the leading /
	dirPart := path[:lastSlash]

	var result string

	// If filename alone is too long, show as much as possible
	if len(filename) >= maxLen-3 {
		result = "..." + filename[len(filename)-(maxLen-3):]
	} else {
		// Calculate how much of the directory we can show
		availableForDir := maxLen - len(filename) - 3 // -3 for "..."

		if availableForDir <= 0 {
			result = "..." + filename
		} else if availableForDir >= 10 {
			// Show start of path + ... + filename
			result = dirPart[:availableForDir] + "..." + filename
		} else {
			// Just show ... + filename
			result = "..." + filename
		}
	}

	if hasQuotes {
		return "\"" + result + "\""
	}
	return result
}

// RenderToolSeparator renders a visual separator between tool executions.
func (s *Styles) RenderToolSeparator() string {
	separator := strings.Repeat("─", 80)
	return s.ToolSeparator.Render(separator)
}

// RenderMessageSeparator renders a subtle separator between messages.
func (s *Styles) RenderMessageSeparator() string {
	separatorStyle := lipgloss.NewStyle().
		Foreground(ColorDim)
	return separatorStyle.Render(strings.Repeat("─", 60))
}

// FormatThinkingIndicator formats the thinking/processing indicator with rotating spinner.
func (s *Styles) FormatThinkingIndicator() string {
	// Rotating spinner frames
	spinners := []string{"◐", "◓", "◑", "◒"}
	idx := int(time.Now().UnixMilli()/100) % len(spinners)

	spinnerStyle := lipgloss.NewStyle().
		Foreground(ColorGradient1).
		Bold(true)
	textStyle := lipgloss.NewStyle().
		Foreground(ColorInfo).
		Italic(true)

	return spinnerStyle.Render(spinners[idx]) + textStyle.Render(" Thinking...")
}

// FormatThinkingIndicatorWithTokens formats the thinking indicator with token usage.
func (s *Styles) FormatThinkingIndicatorWithTokens(usedTokens, maxTokens int) string {
	// Rotating spinner frames
	spinners := []string{"◐", "◓", "◑", "◒"}
	idx := int(time.Now().UnixMilli()/100) % len(spinners)

	spinnerStyle := lipgloss.NewStyle().
		Foreground(ColorGradient1).
		Bold(true)
	textStyle := lipgloss.NewStyle().
		Foreground(ColorInfo).
		Italic(true)
	dimStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	// Format token count
	var tokenStr string
	if usedTokens >= 1000 {
		tokenStr = fmt.Sprintf("%.1fk", float64(usedTokens)/1000)
	} else {
		tokenStr = fmt.Sprintf("%d", usedTokens)
	}
	var maxStr string
	if maxTokens >= 1000 {
		maxStr = fmt.Sprintf("%.0fk", float64(maxTokens)/1000)
	} else {
		maxStr = fmt.Sprintf("%d", maxTokens)
	}

	return spinnerStyle.Render(spinners[idx]) + textStyle.Render(" Thinking...") +
		dimStyle.Render("          [tokens: "+tokenStr+"/"+maxStr+"]")
}

// FormatPlanStepHeader formats a plan step header for delegated execution output.
func (s *Styles) FormatPlanStepHeader(stepID, totalSteps int, title string) string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorPrimary)
	progressStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)
	borderStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	progress := progressStyle.Render(fmt.Sprintf("[%d/%d]", stepID, totalSteps))
	header := headerStyle.Render(title)
	line := borderStyle.Render(strings.Repeat("─", 60))

	return line + "\n" + progress + " " + header + "\n"
}

// FormatPlanStepResult formats a plan step completion result.
func (s *Styles) FormatPlanStepResult(stepID int, success bool, summary string) string {
	if success {
		checkStyle := lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true)
		summaryStyle := lipgloss.NewStyle().Foreground(ColorText)
		result := checkStyle.Render(fmt.Sprintf("  Step %d done", stepID))
		if summary != "" {
			// Truncate summary for display
			if len(summary) > 120 {
				summary = summary[:117] + "..."
			}
			result += " " + summaryStyle.Render(summary)
		}
		return result
	}

	failStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	msgStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FECACA"))
	result := failStyle.Render(fmt.Sprintf("  Step %d failed", stepID))
	if summary != "" {
		result += " " + msgStyle.Render(summary)
	}
	return result
}

// FormatPlanBanner formats the plan execution start/end banner.
func (s *Styles) FormatPlanBanner(text string, isStart bool) string {
	color := ColorSuccess
	if isStart {
		color = ColorInfo
	}
	borderStyle := lipgloss.NewStyle().Foreground(color)
	textStyle := lipgloss.NewStyle().Foreground(color).Bold(true)

	line := strings.Repeat("─", 60)
	return borderStyle.Render(line) + "\n" + textStyle.Render(text) + "\n" + borderStyle.Render(line)
}

// FormatMessage formats a message with consistent styling based on type.
// msgType can be: success, error, warning, info, hint, loading
func (s *Styles) FormatMessage(msgType, title, body string) string {
	icon := MessageIcons[msgType]
	if icon == "" {
		icon = MessageIcons["info"]
	}

	var color lipgloss.Color
	var boxStyle lipgloss.Style

	switch msgType {
	case "success", "done":
		color = ColorSuccess
		boxStyle = s.SuccessBox
	case "error":
		color = ColorError
		boxStyle = s.ErrorBox
	case "warning":
		color = ColorWarning
		boxStyle = s.WarningBox
	case "hint":
		color = ColorAccent
		boxStyle = s.InfoBox.BorderForeground(ColorAccent)
	default: // info, loading
		color = ColorInfo
		boxStyle = s.InfoBox
	}

	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(color)

	bodyStyle := lipgloss.NewStyle().
		Foreground(ColorText).
		PaddingLeft(2)

	header := headerStyle.Render(icon + " " + title)
	if body == "" {
		return header
	}

	return boxStyle.Render(header + "\n" + bodyStyle.Render(body))
}

// FormatHint formats a contextual hint message.
func (s *Styles) FormatHint(text string) string {
	hintStyle := lipgloss.NewStyle().
		Foreground(ColorAccent).
		Italic(true)
	iconStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)

	return iconStyle.Render("💡 ") + hintStyle.Render(text)
}
