package agent

import (
	"regexp"
	"strings"

	"gokin/internal/memory"
)

// Reflector analyzes tool execution failures and suggests recovery strategies.
type Reflector struct {
	patterns   []ErrorPattern
	errorStore *memory.ErrorStore // Persistent error learning
}

// ErrorPattern matches an error and provides a recommendation.
type ErrorPattern struct {
	Pattern     *regexp.Regexp
	Category    string
	Suggestion  string
	ShouldRetry bool
	Alternative string // Alternative tool or approach to suggest
}

// Reflection contains analysis of a tool failure.
type Reflection struct {
	ToolName       string
	Error          string
	Category       string
	Suggestion     string
	ShouldRetry    bool
	Alternative    string
	Intervention   string // Message to inject into agent history
	LearnedContext string // Context from previously learned errors
	LearnedEntryID string // ID of the learned entry used (for feedback)
}

// NewReflector creates a new reflector with default error patterns.
func NewReflector() *Reflector {
	return &Reflector{
		patterns: defaultErrorPatterns(),
	}
}

// SetErrorStore sets the persistent error store for learning.
func (r *Reflector) SetErrorStore(store *memory.ErrorStore) {
	r.errorStore = store
}

// GetErrorStore returns the error store.
func (r *Reflector) GetErrorStore() *memory.ErrorStore {
	return r.errorStore
}

// Analyze examines a tool error and returns recovery recommendations.
func (r *Reflector) Analyze(toolName string, args map[string]any, errorMsg string) *Reflection {
	reflection := &Reflection{
		ToolName: toolName,
		Error:    errorMsg,
	}

	lowerError := strings.ToLower(errorMsg)

	// Check learned errors first (Phase 3)
	if r.errorStore != nil {
		learnedContext := r.errorStore.GetErrorContext(errorMsg)
		if learnedContext != "" {
			reflection.LearnedContext = learnedContext

			// Get the top matching entry for feedback tracking
			matches := r.errorStore.GetLearnedErrors(errorMsg)
			if len(matches) > 0 {
				reflection.LearnedEntryID = matches[0].ID
			}
		}
	}

	// Match against known patterns
	for _, pattern := range r.patterns {
		if pattern.Pattern.MatchString(lowerError) {
			reflection.Category = pattern.Category
			reflection.Suggestion = pattern.Suggestion
			reflection.ShouldRetry = pattern.ShouldRetry
			reflection.Alternative = pattern.Alternative

			// Build intervention message based on context
			reflection.Intervention = r.buildIntervention(toolName, args, pattern, errorMsg)

			// Append learned context if available
			if reflection.LearnedContext != "" {
				reflection.Intervention += "\n" + reflection.LearnedContext
			}

			return reflection
		}
	}

	// Generic reflection for unmatched errors
	reflection.Category = "unknown"
	reflection.Suggestion = "Try a different approach or break down the task"
	reflection.Intervention = buildGenericIntervention(toolName, errorMsg)

	// Append learned context if available
	if reflection.LearnedContext != "" {
		reflection.Intervention += "\n" + reflection.LearnedContext
	}

	return reflection
}

// LearnFromError stores a new error pattern with its solution.
func (r *Reflector) LearnFromError(errorType, pattern, solution string, tags []string) error {
	if r.errorStore == nil {
		return nil
	}
	return r.errorStore.LearnError(errorType, pattern, solution, tags)
}

// RecordSolutionSuccess records that a learned solution was successful.
func (r *Reflector) RecordSolutionSuccess(entryID string) error {
	if r.errorStore == nil || entryID == "" {
		return nil
	}
	return r.errorStore.RecordSuccess(entryID)
}

// RecordSolutionFailure records that a learned solution did not work.
func (r *Reflector) RecordSolutionFailure(entryID string) error {
	if r.errorStore == nil || entryID == "" {
		return nil
	}
	return r.errorStore.RecordFailure(entryID)
}

// buildIntervention creates a context-aware intervention message.
func (r *Reflector) buildIntervention(toolName string, args map[string]any, pattern ErrorPattern, errorMsg string) string {
	var sb strings.Builder

	sb.WriteString("Let me reflect on what went wrong.\n\n")
	sb.WriteString("**Error Analysis:**\n")
	sb.WriteString("- Tool: " + toolName + "\n")
	sb.WriteString("- Category: " + pattern.Category + "\n")
	sb.WriteString("- Error: " + errorMsg + "\n\n")
	sb.WriteString("**My Assessment:**\n")
	sb.WriteString(pattern.Suggestion + "\n\n")

	if pattern.Alternative != "" {
		sb.WriteString("**Alternative Approach:**\n")
		sb.WriteString("I should try using " + pattern.Alternative + " instead.\n\n")
	}

	if pattern.ShouldRetry {
		sb.WriteString("I'll retry with the suggested modifications.\n")
	} else {
		sb.WriteString("I need to take a different approach to achieve the goal.\n")
	}

	return sb.String()
}

// buildGenericIntervention creates a generic reflection message.
func buildGenericIntervention(toolName string, errorMsg string) string {
	var sb strings.Builder

	sb.WriteString("I encountered an unexpected error.\n\n")
	sb.WriteString("**Error Details:**\n")
	sb.WriteString("- Tool: " + toolName + "\n")
	sb.WriteString("- Error: " + errorMsg + "\n\n")
	sb.WriteString("**Next Steps:**\n")
	sb.WriteString("1. Check if my arguments are correct\n")
	sb.WriteString("2. Verify the target exists and is accessible\n")
	sb.WriteString("3. Consider an alternative approach\n")

	return sb.String()
}

// defaultErrorPatterns returns the built-in error patterns and recommendations.
func defaultErrorPatterns() []ErrorPattern {
	return []ErrorPattern{
		// File not found errors
		{
			Pattern:     regexp.MustCompile(`(file|path|directory).*not (found|exist)|no such file|enoent`),
			Category:    "file_not_found",
			Suggestion:  "The file or directory doesn't exist. Use glob tool to search for similar files, or check the exact path spelling.",
			ShouldRetry: false,
			Alternative: "glob",
		},
		{
			Pattern:     regexp.MustCompile(`cannot find|could not find|unable to (find|locate)`),
			Category:    "file_not_found",
			Suggestion:  "The target wasn't found. Try using glob with a broader pattern to locate similar files.",
			ShouldRetry: false,
			Alternative: "glob",
		},

		// Permission errors
		{
			Pattern:     regexp.MustCompile(`permission denied|access denied|eacces|eperm|not permitted`),
			Category:    "permission_denied",
			Suggestion:  "Permission was denied. Check if the file is read-only, or if elevated privileges are needed. Consider an alternative path.",
			ShouldRetry: false,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`read.?only|cannot (write|modify)`),
			Category:    "permission_denied",
			Suggestion:  "The target is read-only. Check file permissions or consider working with a copy.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Command/executable errors
		{
			Pattern:     regexp.MustCompile(`command not found|executable.*not found|unknown command|not recognized`),
			Category:    "command_not_found",
			Suggestion:  "The command doesn't exist in PATH. Check spelling, install the required package, or use an absolute path.",
			ShouldRetry: false,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`no such (program|command|binary)`),
			Category:    "command_not_found",
			Suggestion:  "The program isn't installed. Consider installing it or using an alternative approach.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Timeout errors
		{
			Pattern:     regexp.MustCompile(`timeout|timed out|deadline exceeded|context deadline`),
			Category:    "timeout",
			Suggestion:  "The operation took too long. Try breaking the task into smaller parts, or increase the timeout if possible.",
			ShouldRetry: true,
			Alternative: "",
		},

		// Network errors
		{
			Pattern:     regexp.MustCompile(`connection refused|network unreachable|host not found|dns|econnrefused`),
			Category:    "network_error",
			Suggestion:  "Network connection failed. Check if the service is running, verify the URL/host, or try again later.",
			ShouldRetry: true,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`connection reset|broken pipe|econnreset`),
			Category:    "network_error",
			Suggestion:  "Connection was interrupted. This might be temporary - retry the request.",
			ShouldRetry: true,
			Alternative: "",
		},

		// Syntax/parsing errors
		{
			Pattern:     regexp.MustCompile(`syntax error|parse error|invalid (syntax|json|yaml|format)`),
			Category:    "syntax_error",
			Suggestion:  "There's a syntax or format error. Review the content for typos, missing brackets, or invalid characters.",
			ShouldRetry: false,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`unexpected (token|character|end|eof)`),
			Category:    "syntax_error",
			Suggestion:  "Parsing failed due to unexpected content. Check for unclosed quotes, brackets, or malformed data.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Compilation errors
		{
			Pattern:     regexp.MustCompile(`compilation (failed|error)|build (failed|error)|undefined:|cannot use`),
			Category:    "compilation_error",
			Suggestion:  "Code compilation failed. Read the error message carefully, check the referenced line, and fix the issue. Consider asking an explore agent for context.",
			ShouldRetry: false,
			Alternative: "explore",
		},
		{
			Pattern:     regexp.MustCompile(`undeclared|undefined reference|unknown (type|identifier|symbol)`),
			Category:    "compilation_error",
			Suggestion:  "Missing declaration or import. Check if the identifier exists and is imported/declared correctly.",
			ShouldRetry: false,
			Alternative: "explore",
		},

		// Test failures
		{
			Pattern:     regexp.MustCompile(`test(s)? failed|fail:|--- fail`),
			Category:    "test_failure",
			Suggestion:  "Tests are failing. Read the test output to understand what's expected vs actual, then fix the code or test.",
			ShouldRetry: false,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`assertion (failed|error)|expected .* got`),
			Category:    "test_failure",
			Suggestion:  "An assertion failed. Compare expected and actual values to understand the discrepancy.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Resource errors
		{
			Pattern:     regexp.MustCompile(`out of memory|memory limit|oom|enomem`),
			Category:    "resource_error",
			Suggestion:  "Ran out of memory. Process smaller chunks of data, optimize memory usage, or increase available memory.",
			ShouldRetry: false,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`disk (full|space)|no space left|enospc`),
			Category:    "resource_error",
			Suggestion:  "Disk is full. Free up space or write to a different location.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Git errors
		{
			Pattern:     regexp.MustCompile(`not a git repository|fatal: not a git`),
			Category:    "git_error",
			Suggestion:  "Not in a git repository. Make sure you're in the correct directory or initialize git first.",
			ShouldRetry: false,
			Alternative: "",
		},
		{
			Pattern:     regexp.MustCompile(`merge conflict|automatic merge failed`),
			Category:    "git_error",
			Suggestion:  "There's a merge conflict. Read the conflicting files, resolve the conflicts manually, then continue.",
			ShouldRetry: false,
			Alternative: "read",
		},
		{
			Pattern:     regexp.MustCompile(`detached head|not on any branch`),
			Category:    "git_error",
			Suggestion:  "In detached HEAD state. Create a branch to save changes, or checkout an existing branch.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Rate limiting
		{
			Pattern:     regexp.MustCompile(`rate limit|too many requests|429|throttl`),
			Category:    "rate_limit",
			Suggestion:  "Hit rate limits. Wait a moment before retrying, or reduce request frequency.",
			ShouldRetry: true,
			Alternative: "",
		},

		// Authentication errors
		{
			Pattern:     regexp.MustCompile(`unauthorized|authentication failed|401|invalid (token|credentials)`),
			Category:    "auth_error",
			Suggestion:  "Authentication failed. Check credentials, token expiration, or API key validity.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Already exists errors
		{
			Pattern:     regexp.MustCompile(`already exists|file exists|eexist|duplicate`),
			Category:    "already_exists",
			Suggestion:  "The target already exists. Use a different name, remove the existing one, or update it instead.",
			ShouldRetry: false,
			Alternative: "",
		},

		// Invalid arguments
		{
			Pattern:     regexp.MustCompile(`invalid argument|illegal argument|bad (parameter|argument|input)`),
			Category:    "invalid_args",
			Suggestion:  "Invalid arguments provided. Check the documentation for correct parameter format and values.",
			ShouldRetry: false,
			Alternative: "",
		},
	}
}

// AddPattern adds a custom error pattern to the reflector.
func (r *Reflector) AddPattern(pattern *regexp.Regexp, category, suggestion string, shouldRetry bool, alternative string) {
	r.patterns = append(r.patterns, ErrorPattern{
		Pattern:     pattern,
		Category:    category,
		Suggestion:  suggestion,
		ShouldRetry: shouldRetry,
		Alternative: alternative,
	})
}
