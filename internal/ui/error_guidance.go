package ui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ErrorGuidance provides actionable suggestions for common errors.
type ErrorGuidance struct {
	Pattern     *regexp.Regexp // Compiled regex to match error
	Title       string         // User-friendly title
	Suggestions []string       // What user can try
	Command     string         // Relevant command hint (optional)
}

// errorGuidancePatterns contains known error patterns with guidance.
var errorGuidancePatterns = []ErrorGuidance{
	{
		Pattern:     regexp.MustCompile(`(?i)(deadline exceeded|timeout|context deadline)`),
		Title:       "Request Timed Out",
		Suggestions: []string{"Check your network connection", "Try a simpler request", "The API server may be overloaded - wait and retry"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(rate limit|429|too many requests)`),
		Title:       "Rate Limit Reached",
		Suggestions: []string{"Wait a moment before trying again", "Reduce request frequency", "Consider upgrading your API plan"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(connection refused|no such host|network unreachable|dial tcp)`),
		Title:       "Connection Failed",
		Suggestions: []string{"Check your internet connection", "Verify API endpoint in config", "Check if firewall is blocking the connection"},
		Command:     "/config show api",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(unauthorized|401|invalid.*key|api.*key.*invalid)`),
		Title:       "Authentication Failed",
		Suggestions: []string{"Check your API key is correct", "Regenerate your API key at aistudio.google.com", "Run setup wizard again"},
		Command:     "/auth",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(forbidden|403|permission denied)`),
		Title:       "Access Denied",
		Suggestions: []string{"Check API key permissions", "Verify you have access to this model", "Contact API provider for access"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(quota|limit exceeded|resource exhausted)`),
		Title:       "Quota Exceeded",
		Suggestions: []string{"You've reached your usage limit", "Wait for quota reset or upgrade plan", "Use a more efficient model"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(model.*not.*found|invalid.*model|unknown.*model)`),
		Title:       "Model Not Found",
		Suggestions: []string{"Check the model name is correct", "List available models with /model", "The model may have been deprecated"},
		Command:     "/model list",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(context.*too.*long|token.*limit|max.*tokens)`),
		Title:       "Context Too Long",
		Suggestions: []string{"Clear conversation history with /clear", "Use /compact to summarize context", "Break your request into smaller parts"},
		Command:     "/clear",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(file.*not.*found|no such file|ENOENT)`),
		Title:       "File Not Found",
		Suggestions: []string{"Check the file path is correct", "Use /pwd to see current directory", "Search for the file with glob pattern"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(permission.*denied|EACCES|cannot.*write)`),
		Title:       "File Permission Error",
		Suggestions: []string{"Check file permissions", "You may need elevated privileges", "Try a different location"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(command.*not.*found|executable.*not.*found)`),
		Title:       "Command Not Found",
		Suggestions: []string{"Check the command is installed", "Verify it's in your PATH", "Install the required tool"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(git.*not.*found|not.*git.*repository)`),
		Title:       "Git Error",
		Suggestions: []string{"Initialize a git repository with 'git init'", "Check you're in the right directory", "Install git if not available"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(json.*parse|invalid.*json|unexpected.*token)`),
		Title:       "Invalid Response Format",
		Suggestions: []string{"The API returned an unexpected response", "Try the request again", "Report this issue if it persists"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(content.*policy|safety|blocked|harmful)`),
		Title:       "Content Policy",
		Suggestions: []string{"Your request was flagged by content filters", "Rephrase your request", "Review content guidelines"},
		Command:     "",
	},
	// Go-specific errors
	{
		Pattern:     regexp.MustCompile(`(?i)(undefined:|cannot refer to unexported|imported and not used|declared and not used)`),
		Title:       "Go Compilation Error",
		Suggestions: []string{"Check for typos in variable/function names", "Ensure all imports are used", "Remove unused variables or use _ placeholder"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(cannot use .* as .* in|incompatible type|type mismatch)`),
		Title:       "Go Type Error",
		Suggestions: []string{"Check type assertions and conversions", "Verify function signatures match expected types", "Use explicit type conversion if needed"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(go\.mod.*requires|module .* found .* but does not contain|no required module provides)`),
		Title:       "Go Module Error",
		Suggestions: []string{"Run 'go mod tidy' to fix dependencies", "Check go.mod for correct module path", "Run 'go get <package>' to add missing dependency"},
		Command:     "",
	},
	// Python-specific errors
	{
		Pattern:     regexp.MustCompile(`(?i)(ModuleNotFoundError|ImportError|No module named)`),
		Title:       "Python Import Error",
		Suggestions: []string{"Install the missing package with pip", "Check virtual environment is activated", "Verify the module name is spelled correctly"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(IndentationError|TabError|unexpected indent)`),
		Title:       "Python Indentation Error",
		Suggestions: []string{"Check for mixed tabs and spaces", "Use consistent indentation (4 spaces recommended)", "Run your editor's auto-format"},
		Command:     "",
	},
	// Node.js-specific errors
	{
		Pattern:     regexp.MustCompile(`(?i)(Cannot find module|MODULE_NOT_FOUND|ERR_MODULE_NOT_FOUND)`),
		Title:       "Node Module Not Found",
		Suggestions: []string{"Run 'npm install' to install dependencies", "Check package.json for the module", "Verify the import path is correct"},
		Command:     "",
	},
	{
		Pattern:     regexp.MustCompile(`(?i)(ENOSPC|no space left|disk quota exceeded)`),
		Title:       "Disk Space Error",
		Suggestions: []string{"Free up disk space", "Clean build caches and node_modules", "Check disk usage with 'df -h'"},
		Command:     "",
	},
	// Docker errors
	{
		Pattern:     regexp.MustCompile(`(?i)(Cannot connect to the Docker daemon|docker.*not running)`),
		Title:       "Docker Not Running",
		Suggestions: []string{"Start Docker Desktop or Docker daemon", "Check Docker service status", "Verify Docker installation"},
		Command:     "",
	},
	// Memory errors
	{
		Pattern:     regexp.MustCompile(`(?i)(out of memory|OOM|heap.*exhausted|allocation failed)`),
		Title:       "Out of Memory",
		Suggestions: []string{"Reduce batch size or data volume", "Increase memory limits", "Check for memory leaks in your code"},
		Command:     "",
	},
	// Port/address errors
	{
		Pattern:     regexp.MustCompile(`(?i)(address already in use|EADDRINUSE|port.*already|bind.*failed)`),
		Title:       "Port Already In Use",
		Suggestions: []string{"Another process is using this port", "Find the process: lsof -i :<port>", "Use a different port or kill the blocking process"},
		Command:     "",
	},
}

// GetErrorGuidance returns guidance for an error message, or nil if no match.
func GetErrorGuidance(errMsg string) *ErrorGuidance {
	for _, g := range errorGuidancePatterns {
		if g.Pattern.MatchString(errMsg) {
			return &g
		}
	}
	return nil
}

// FormatErrorWithGuidance formats an error with helpful guidance.
func FormatErrorWithGuidance(styles *Styles, errMsg string) string {
	guidance := GetErrorGuidance(errMsg)

	errorStyle := lipgloss.NewStyle().Foreground(ColorError).Bold(true)
	msgStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FECACA"))
	titleStyle := lipgloss.NewStyle().Foreground(ColorWarning).Bold(true)
	suggestionStyle := lipgloss.NewStyle().Foreground(ColorMuted)
	cmdStyle := lipgloss.NewStyle().Foreground(ColorSecondary)
	markerStyle := lipgloss.NewStyle().Foreground(ColorDim)

	var result strings.Builder

	// Error message
	result.WriteString(errorStyle.Render("✗ Error: ") + msgStyle.Render(truncateError(errMsg, 100)))

	if guidance != nil {
		// Title
		result.WriteString("\n")
		result.WriteString(markerStyle.Render("  ⎿  ") + titleStyle.Render(guidance.Title))

		// Suggestions
		for _, suggestion := range guidance.Suggestions {
			result.WriteString("\n")
			result.WriteString(markerStyle.Render("     • ") + suggestionStyle.Render(suggestion))
		}

		// Command hint
		if guidance.Command != "" {
			result.WriteString("\n")
			result.WriteString(markerStyle.Render("     ") + cmdStyle.Render("Try: "+guidance.Command))
		}
	}

	return result.String()
}

// truncateError truncates an error message to a maximum length.
func truncateError(msg string, maxLen int) string {
	// Remove newlines for single-line display
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.TrimSpace(msg)

	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
}

// GetCompactHint returns a short actionable hint for an error.
// Returns empty string if no matching guidance is found.
func GetCompactHint(errMsg string) string {
	guidance := GetErrorGuidance(errMsg)
	if guidance == nil || len(guidance.Suggestions) == 0 {
		return ""
	}
	// Return first suggestion, shortened if needed
	hint := guidance.Suggestions[0]
	if len(hint) > 40 {
		hint = hint[:37] + "..."
	}
	return hint
}
