package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// EnhancedError provides rich error information with context and suggestions.
type EnhancedError struct {
	OriginalError error
	Category      ErrorCategory
	Context       string   // What operation was being performed
	Suggestions   []string // Actionable suggestions
	RetryInfo     *RetryInfo
	RelatedFiles  []string // Files involved in the error
	Documentation string   // Link or reference to documentation
}

// ErrorCategory categorizes errors for appropriate handling and display.
type ErrorCategory string

const (
	ErrorCategoryNetwork    ErrorCategory = "network"
	ErrorCategoryPermission ErrorCategory = "permission"
	ErrorCategorySyntax     ErrorCategory = "syntax"
	ErrorCategoryFile       ErrorCategory = "file"
	ErrorCategoryTimeout    ErrorCategory = "timeout"
	ErrorCategoryAuth       ErrorCategory = "auth"
	ErrorCategoryConfig     ErrorCategory = "config"
	ErrorCategoryRateLimit  ErrorCategory = "rate_limit"
	ErrorCategoryAPI        ErrorCategory = "api"
	ErrorCategoryGit        ErrorCategory = "git"
	ErrorCategoryUnknown    ErrorCategory = "unknown"
)

// RetryInfo contains information about retry attempts.
type RetryInfo struct {
	AttemptNumber int
	MaxAttempts   int
	NextRetryIn   time.Duration
	CanRetry      bool
	RetryReason   string
}

// ErrorDisplayModel renders enhanced error information.
type ErrorDisplayModel struct {
	error    *EnhancedError
	width    int
	expanded bool
}

// NewErrorDisplayModel creates a new error display model.
func NewErrorDisplayModel(err *EnhancedError) ErrorDisplayModel {
	return ErrorDisplayModel{error: err, width: 80}
}

// SetWidth sets the display width.
func (m *ErrorDisplayModel) SetWidth(width int) {
	m.width = width
}

// SetExpanded sets whether to show full details.
func (m *ErrorDisplayModel) SetExpanded(expanded bool) {
	m.expanded = expanded
}

// View renders the enhanced error display.
func (m ErrorDisplayModel) View() string {
	if m.error == nil {
		return ""
	}

	var sb strings.Builder

	// Header with category icon and title
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FF6B6B")).
		Background(lipgloss.Color("#2D1F1F")).
		Padding(0, 1)

	categoryIcon := m.getCategoryIcon()
	categoryTitle := m.getCategoryTitle()
	sb.WriteString(headerStyle.Render(fmt.Sprintf("%s %s", categoryIcon, categoryTitle)))
	sb.WriteString("\n\n")

	// Error message
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FECACA"))
	sb.WriteString(errorStyle.Render(m.error.OriginalError.Error()))
	sb.WriteString("\n")

	// Context (what was being done)
	if m.error.Context != "" {
		contextStyle := lipgloss.NewStyle().Foreground(ColorDim).Italic(true)
		sb.WriteString(contextStyle.Render("While: "+m.error.Context) + "\n")
	}

	// Retry information
	if m.error.RetryInfo != nil {
		sb.WriteString("\n")
		sb.WriteString(m.renderRetryInfo())
	}

	// Suggestions
	if len(m.error.Suggestions) > 0 {
		sb.WriteString("\n")
		suggestHeaderStyle := lipgloss.NewStyle().Foreground(ColorSuccess).Bold(true)
		sb.WriteString(suggestHeaderStyle.Render("Suggestions:") + "\n")

		suggestStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#A7F3D0"))
		for _, sug := range m.error.Suggestions {
			sb.WriteString("   " + suggestStyle.Render("• "+sug) + "\n")
		}
	}

	// Related files (expanded view)
	if m.expanded && len(m.error.RelatedFiles) > 0 {
		sb.WriteString("\n")
		fileHeaderStyle := lipgloss.NewStyle().Foreground(ColorMuted).Bold(true)
		sb.WriteString(fileHeaderStyle.Render("Related files:") + "\n")

		fileStyle := lipgloss.NewStyle().Foreground(ColorDim)
		for _, file := range m.error.RelatedFiles {
			sb.WriteString("   " + fileStyle.Render(file) + "\n")
		}
	}

	// Documentation link
	if m.error.Documentation != "" {
		sb.WriteString("\n")
		docStyle := lipgloss.NewStyle().Foreground(ColorInfo).Italic(true)
		sb.WriteString(docStyle.Render("See: "+m.error.Documentation) + "\n")
	}

	// Wrap in a box
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#FF6B6B")).
		Padding(1).
		MaxWidth(m.width)

	return boxStyle.Render(sb.String())
}

// ViewCompact renders a compact single-line error display.
func (m ErrorDisplayModel) ViewCompact() string {
	if m.error == nil {
		return ""
	}

	icon := m.getCategoryIcon()
	errorStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	msgStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FECACA"))
	hintStyle := lipgloss.NewStyle().Foreground(ColorWarning)

	errMsg := m.error.OriginalError.Error()
	if len(errMsg) > 60 {
		errMsg = errMsg[:57] + "..."
	}

	result := errorStyle.Render(icon+" Error: ") + msgStyle.Render(errMsg)

	// Add first suggestion as hint
	if len(m.error.Suggestions) > 0 {
		hint := m.error.Suggestions[0]
		if len(hint) > 40 {
			hint = hint[:37] + "..."
		}
		result += " " + hintStyle.Render("→ "+hint)
	}

	return result
}

func (m ErrorDisplayModel) getCategoryIcon() string {
	icons := map[ErrorCategory]string{
		ErrorCategoryNetwork:    "NET",
		ErrorCategoryPermission: "PERM",
		ErrorCategorySyntax:     "SYN",
		ErrorCategoryFile:       "FILE",
		ErrorCategoryTimeout:    "TIME",
		ErrorCategoryAuth:       "AUTH",
		ErrorCategoryConfig:     "CFG",
		ErrorCategoryRateLimit:  "RATE",
		ErrorCategoryAPI:        "API",
		ErrorCategoryGit:        "GIT",
		ErrorCategoryUnknown:    "ERR",
	}
	if icon, ok := icons[m.error.Category]; ok {
		return icon
	}
	return "ERR"
}

func (m ErrorDisplayModel) getCategoryTitle() string {
	titles := map[ErrorCategory]string{
		ErrorCategoryNetwork:    "Network Error",
		ErrorCategoryPermission: "Permission Denied",
		ErrorCategorySyntax:     "Syntax Error",
		ErrorCategoryFile:       "File Error",
		ErrorCategoryTimeout:    "Timeout",
		ErrorCategoryAuth:       "Authentication Failed",
		ErrorCategoryConfig:     "Configuration Error",
		ErrorCategoryRateLimit:  "Rate Limited",
		ErrorCategoryAPI:        "API Error",
		ErrorCategoryGit:        "Git Error",
		ErrorCategoryUnknown:    "Error",
	}
	if title, ok := titles[m.error.Category]; ok {
		return title
	}
	return "Error"
}

func (m ErrorDisplayModel) renderRetryInfo() string {
	ri := m.error.RetryInfo
	retryStyle := lipgloss.NewStyle().Foreground(ColorWarning)
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var sb strings.Builder

	if ri.CanRetry {
		progress := fmt.Sprintf("Attempt %d/%d", ri.AttemptNumber, ri.MaxAttempts)
		sb.WriteString(retryStyle.Render(progress))

		if ri.NextRetryIn > 0 {
			sb.WriteString(dimStyle.Render(fmt.Sprintf(" • Next retry in %s", ri.NextRetryIn.Round(time.Second))))
		}

		if ri.RetryReason != "" {
			sb.WriteString("\n" + dimStyle.Render("Reason: "+ri.RetryReason))
		}
	} else {
		sb.WriteString(retryStyle.Render(fmt.Sprintf("All %d attempts failed", ri.MaxAttempts)))
		if ri.RetryReason != "" {
			sb.WriteString(dimStyle.Render(" - " + ri.RetryReason))
		}
	}

	return sb.String()
}

// ClassifyError analyzes an error and returns an EnhancedError with context.
func ClassifyError(err error, context string) *EnhancedError {
	if err == nil {
		return nil
	}

	enhanced := &EnhancedError{
		OriginalError: err,
		Context:       context,
		Category:      ErrorCategoryUnknown,
	}

	errStr := strings.ToLower(err.Error())

	// Classify based on error message patterns
	switch {
	case containsAny(errStr, "permission denied", "access denied", "eacces", "operation not permitted"):
		enhanced.Category = ErrorCategoryPermission
		enhanced.Suggestions = []string{
			"Check file/directory permissions",
			"Try running with elevated privileges if necessary",
			"Verify you have write access to the target location",
		}

	case containsAny(errStr, "connection refused", "no such host", "network unreachable", "dial tcp", "dns"):
		enhanced.Category = ErrorCategoryNetwork
		enhanced.Suggestions = []string{
			"Check your internet connection",
			"Verify the API endpoint is correct",
			"Check if a firewall is blocking the connection",
		}

	case containsAny(errStr, "timeout", "deadline exceeded", "context deadline"):
		enhanced.Category = ErrorCategoryTimeout
		enhanced.Suggestions = []string{
			"The operation took too long - try again",
			"Consider increasing the timeout in config",
			"Check if the server is responding slowly",
		}

	case containsAny(errStr, "no such file", "not found", "does not exist", "enoent"):
		enhanced.Category = ErrorCategoryFile
		enhanced.Suggestions = []string{
			"Verify the file path is correct",
			"Check if the file was moved or deleted",
			"Use glob patterns to search for the file",
		}

	case containsAny(errStr, "unauthorized", "401", "invalid.*key", "api.*key.*invalid"):
		enhanced.Category = ErrorCategoryAuth
		enhanced.Suggestions = []string{
			"Verify your API key is correct",
			"Check if the API key has expired",
			"Run 'gokin --setup' to reconfigure credentials",
		}
		enhanced.Documentation = "https://ai.google.dev/tutorials/setup"

	case containsAny(errStr, "rate limit", "429", "too many requests", "quota"):
		enhanced.Category = ErrorCategoryRateLimit
		enhanced.Suggestions = []string{
			"Wait a moment before trying again",
			"Reduce request frequency",
			"Consider upgrading your API plan",
		}

	case containsAny(errStr, "syntax", "parse", "unexpected token", "invalid json"):
		enhanced.Category = ErrorCategorySyntax
		enhanced.Suggestions = []string{
			"Check the syntax of your input",
			"Verify JSON/YAML formatting is correct",
			"Look for missing brackets or quotes",
		}

	case containsAny(errStr, "config", "configuration", "yaml", "invalid option"):
		enhanced.Category = ErrorCategoryConfig
		enhanced.Suggestions = []string{
			"Check your configuration file for errors",
			"Run 'gokin --setup' to reconfigure",
			"Verify all required fields are present",
		}
		enhanced.Documentation = "~/.config/gokin/config.yaml"

	case containsAny(errStr, "git", "not a git repository", "fatal:"):
		enhanced.Category = ErrorCategoryGit
		enhanced.Suggestions = []string{
			"Make sure you're in a git repository",
			"Initialize with 'git init' if needed",
			"Check if git is installed and in PATH",
		}

	case containsAny(errStr, "api", "500", "502", "503", "server error", "internal error"):
		enhanced.Category = ErrorCategoryAPI
		enhanced.Suggestions = []string{
			"The API server may be experiencing issues",
			"Try again in a few moments",
			"Check the API status page for outages",
		}
	}

	return enhanced
}

// ClassifyErrorWithRetry creates an enhanced error with retry information.
func ClassifyErrorWithRetry(err error, context string, attempt, maxAttempts int, nextRetry time.Duration) *EnhancedError {
	enhanced := ClassifyError(err, context)
	if enhanced == nil {
		return nil
	}

	enhanced.RetryInfo = &RetryInfo{
		AttemptNumber: attempt,
		MaxAttempts:   maxAttempts,
		NextRetryIn:   nextRetry,
		CanRetry:      attempt < maxAttempts,
	}

	// Add retry-specific reason based on category
	switch enhanced.Category {
	case ErrorCategoryNetwork:
		enhanced.RetryInfo.RetryReason = "Network issues are often transient"
	case ErrorCategoryTimeout:
		enhanced.RetryInfo.RetryReason = "Server may be temporarily overloaded"
	case ErrorCategoryRateLimit:
		enhanced.RetryInfo.RetryReason = "Waiting for rate limit reset"
	case ErrorCategoryAPI:
		enhanced.RetryInfo.RetryReason = "Server error - may be temporary"
	}

	return enhanced
}

// AddRelatedFiles adds file information to an enhanced error.
func (e *EnhancedError) AddRelatedFiles(files ...string) *EnhancedError {
	e.RelatedFiles = append(e.RelatedFiles, files...)
	return e
}

// AddSuggestion adds a suggestion to the error.
func (e *EnhancedError) AddSuggestion(suggestion string) *EnhancedError {
	e.Suggestions = append(e.Suggestions, suggestion)
	return e
}

// containsAny checks if s contains any of the substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// FormatEnhancedError formats an enhanced error for display using the given styles.
func FormatEnhancedError(styles *Styles, err *EnhancedError) string {
	model := NewErrorDisplayModel(err)
	model.SetWidth(80)
	return model.View()
}

// FormatEnhancedErrorCompact formats an enhanced error in compact form.
func FormatEnhancedErrorCompact(styles *Styles, err *EnhancedError) string {
	model := NewErrorDisplayModel(err)
	return model.ViewCompact()
}
