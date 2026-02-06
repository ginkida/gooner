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

// View renders the enhanced error display — compact, no box.
// Format:
//
//	✗ API error: rate limit exceeded
//	  ↳ Wait 30s and retry, or check your API quota
func (m ErrorDisplayModel) View() string {
	if m.error == nil {
		return ""
	}

	errorStyle := lipgloss.NewStyle().Foreground(ColorRose)
	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	markerStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var sb strings.Builder

	// Main error line: ✗ Category: message
	categoryTitle := m.getCategoryTitle()
	sb.WriteString(errorStyle.Render("✗ "+categoryTitle+": ") + hintStyle.Render(m.error.OriginalError.Error()))
	sb.WriteString("\n")

	// First suggestion as indented hint
	if len(m.error.Suggestions) > 0 {
		sb.WriteString(markerStyle.Render("  ↳ ") + hintStyle.Render(m.error.Suggestions[0]))
		sb.WriteString("\n")
	}

	// Retry info (compact, inline)
	if m.error.RetryInfo != nil && m.error.RetryInfo.CanRetry {
		ri := m.error.RetryInfo
		retryStr := fmt.Sprintf("  ↳ Retry %d/%d", ri.AttemptNumber, ri.MaxAttempts)
		if ri.NextRetryIn > 0 {
			retryStr += fmt.Sprintf(", next in %s", ri.NextRetryIn.Round(time.Second))
		}
		sb.WriteString(hintStyle.Render(retryStr))
		sb.WriteString("\n")
	}

	// Expanded view: more suggestions, related files, docs
	if m.expanded {
		for i := 1; i < len(m.error.Suggestions); i++ {
			sb.WriteString(markerStyle.Render("  ↳ ") + hintStyle.Render(m.error.Suggestions[i]))
			sb.WriteString("\n")
		}

		if len(m.error.RelatedFiles) > 0 {
			for _, file := range m.error.RelatedFiles {
				sb.WriteString(hintStyle.Render("    " + file))
				sb.WriteString("\n")
			}
		}

		if m.error.Documentation != "" {
			sb.WriteString(hintStyle.Render("  See: " + m.error.Documentation))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// ViewCompact renders a compact single-line error display.
func (m ErrorDisplayModel) ViewCompact() string {
	if m.error == nil {
		return ""
	}

	errorStyle := lipgloss.NewStyle().Foreground(ColorRose)
	msgStyle := lipgloss.NewStyle().Foreground(ColorDim)

	errMsg := m.error.OriginalError.Error()
	if len(errMsg) > 60 {
		errMsg = errMsg[:57] + "..."
	}

	result := errorStyle.Render("✗ ") + msgStyle.Render(errMsg)

	if len(m.error.Suggestions) > 0 {
		hint := m.error.Suggestions[0]
		if len(hint) > 40 {
			hint = hint[:37] + "..."
		}
		result += " " + msgStyle.Render("→ "+hint)
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
