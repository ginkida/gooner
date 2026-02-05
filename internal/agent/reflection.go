package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gokin/internal/client"
	"gokin/internal/logging"
	"gokin/internal/memory"
)

// PredictorInterface defines the interface for file prediction.
type PredictorInterface interface {
	PredictFiles(currentFile string, limit int) []PredictedFile
}

// PredictedFile represents a file that might be needed.
type PredictedFile struct {
	Path       string  `json:"path"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// Reflector analyzes tool execution failures and suggests recovery strategies.
type Reflector struct {
	patterns               []ErrorPattern
	errorStore             *memory.ErrorStore // Persistent error learning
	client                 client.Client      // For LLM-based semantic analysis
	enableSemanticAnalysis bool               // Whether to use LLM for unmatched errors
	predictor              PredictorInterface // For file predictions on file_not_found errors
}

// ErrorPattern matches an error and provides a recommendation.
type ErrorPattern struct {
	Pattern            *regexp.Regexp
	Category           string
	Suggestion         string
	ShouldRetry        bool
	ShouldRetryWithFix bool   // Whether we can suggest a specific fix
	SuggestedFix       string // The command or step to fix the error
	Alternative        string // Alternative tool or approach to suggest
}

// Reflection contains analysis of a tool failure.
type Reflection struct {
	ToolName           string
	Error              string
	Category           string
	Suggestion         string
	ShouldRetry        bool
	ShouldRetryWithFix bool
	SuggestedFix       string
	Alternative        string
	Intervention       string // Message to inject into agent history
	LearnedContext     string // Context from previously learned errors
	LearnedEntryID     string // ID of the learned entry used (for feedback)
}

// NewReflector creates a new reflector with default error patterns.
func NewReflector() *Reflector {
	return &Reflector{
		patterns:               defaultErrorPatterns(),
		enableSemanticAnalysis: true,
	}
}

// SetClient sets the LLM client for semantic analysis.
func (r *Reflector) SetClient(c client.Client) {
	r.client = c
}

// SetSemanticAnalysis enables or disables LLM-based error analysis.
func (r *Reflector) SetSemanticAnalysis(enabled bool) {
	r.enableSemanticAnalysis = enabled
}

// SetErrorStore sets the persistent error store for learning.
func (r *Reflector) SetErrorStore(store *memory.ErrorStore) {
	r.errorStore = store
}

// GetErrorStore returns the error store.
func (r *Reflector) GetErrorStore() *memory.ErrorStore {
	return r.errorStore
}

// SetPredictor sets the file predictor for enhanced file_not_found recovery.
func (r *Reflector) SetPredictor(p PredictorInterface) {
	r.predictor = p
}

// Reflect examines a tool error and returns recovery recommendations.
func (r *Reflector) Reflect(toolName string, args map[string]any, errorMsg string) *Reflection {
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
			reflection.ShouldRetryWithFix = pattern.ShouldRetryWithFix
			reflection.SuggestedFix = pattern.SuggestedFix
			reflection.Alternative = pattern.Alternative

			// Build intervention message based on context
			reflection.Intervention = r.buildIntervention(toolName, args, pattern, errorMsg)

			// Enhance file_not_found errors with file predictions
			if pattern.Category == "file_not_found" && r.predictor != nil {
				if filePath := extractFilePathFromError(errorMsg, args); filePath != "" {
					predictions := r.predictor.PredictFiles(filePath, 3)
					if len(predictions) > 0 {
						var suggestions []string
						for _, p := range predictions {
							suggestions = append(suggestions, p.Path)
						}
						reflection.Intervention += fmt.Sprintf(
							"\n\n**Similar files that might be relevant:**\n- %s",
							strings.Join(suggestions, "\n- "))
					}
				}
			}

			// Append learned context if available
			if reflection.LearnedContext != "" {
				reflection.Intervention += "\n" + reflection.LearnedContext
			}

			return reflection
		}
	}

	// For unmatched errors, try semantic analysis with LLM
	if r.enableSemanticAnalysis && r.client != nil {
		semanticReflection := r.semanticAnalyze(toolName, args, errorMsg)
		if semanticReflection != nil && semanticReflection.Category != "unknown" {
			// Append learned context if available
			if reflection.LearnedContext != "" {
				semanticReflection.LearnedContext = reflection.LearnedContext
				semanticReflection.LearnedEntryID = reflection.LearnedEntryID
				semanticReflection.Intervention += "\n" + reflection.LearnedContext
			}
			return semanticReflection
		}
	}

	// Fall back to generic reflection for unmatched errors
	reflection.Category = "unknown"
	reflection.Suggestion = "Try a different approach or break down the task"
	reflection.Intervention = buildGenericIntervention(toolName, errorMsg)

	// Append learned context if available
	if reflection.LearnedContext != "" {
		reflection.Intervention += "\n" + reflection.LearnedContext
	}

	return reflection
}

// SemanticAnalysisResult represents the LLM's analysis of an error.
type SemanticAnalysisResult struct {
	Category    string `json:"category"`
	Suggestion  string `json:"suggestion"`
	ShouldRetry bool   `json:"should_retry"`
	Alternative string `json:"alternative"`
	RootCause   string `json:"root_cause"`
}

// semanticAnalyze uses LLM to analyze errors that don't match known patterns.
func (r *Reflector) semanticAnalyze(toolName string, args map[string]any, errorMsg string) *Reflection {
	if r.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build analysis prompt
	argsJSON, _ := json.Marshal(args)
	prompt := `You are an error analysis expert. Analyze this tool execution error and provide recovery recommendations.

Tool: ` + toolName + `
Arguments: ` + string(argsJSON) + `
Error: ` + errorMsg + `

Respond with a JSON object containing:
{
  "category": "one of: file_not_found, permission_denied, syntax_error, compilation_error, network_error, timeout, resource_error, invalid_args, configuration_error, dependency_error, logic_error, unknown",
  "suggestion": "specific actionable advice to fix this error",
  "should_retry": true/false if retrying might help,
  "alternative": "name of alternative tool or approach if applicable (e.g., 'glob', 'read', 'explore')",
  "root_cause": "brief explanation of the likely root cause"
}

Be concise and practical. Focus on actionable solutions.`

	resp, err := r.client.SendMessage(ctx, prompt)
	if err != nil {
		logging.Debug("semantic analysis failed", "error", err)
		return nil
	}

	// Collect the response
	var fullResponse strings.Builder
	for chunk := range resp.Chunks {
		if chunk.Error != nil {
			logging.Debug("semantic analysis stream error", "error", chunk.Error)
			break
		}
		if chunk.Text != "" {
			fullResponse.WriteString(chunk.Text)
		}
	}

	// Parse JSON from response
	responseText := fullResponse.String()
	result, err := r.parseSemanticResult(responseText)
	if err != nil {
		logging.Debug("failed to parse semantic analysis result", "error", err, "response", responseText)
		return nil
	}

	// Build reflection from result
	reflection := &Reflection{
		ToolName:    toolName,
		Error:       errorMsg,
		Category:    result.Category,
		Suggestion:  result.Suggestion,
		ShouldRetry: result.ShouldRetry,
		Alternative: result.Alternative,
	}

	// Build intervention message
	var sb strings.Builder
	sb.WriteString("**Semantic Error Analysis:**\n\n")
	sb.WriteString("**Root Cause:** " + result.RootCause + "\n\n")
	sb.WriteString("**Category:** " + result.Category + "\n\n")
	sb.WriteString("**Recommendation:** " + result.Suggestion + "\n\n")
	if result.Alternative != "" {
		sb.WriteString("**Alternative Approach:** Try using `" + result.Alternative + "` instead.\n\n")
	}
	if result.ShouldRetry {
		sb.WriteString("This error might be resolved by retrying with modifications.\n")
	} else {
		sb.WriteString("A different approach is recommended.\n")
	}

	reflection.Intervention = sb.String()

	// Learn from this analysis for future reference
	if r.errorStore != nil {
		tags := []string{toolName, result.Category}
		if result.Alternative != "" {
			tags = append(tags, "alt:"+result.Alternative)
		}
		_ = r.errorStore.LearnError(result.Category, errorMsg, result.Suggestion, tags)
	}

	return reflection
}

// parseSemanticResult extracts JSON from LLM response.
func (r *Reflector) parseSemanticResult(response string) (*SemanticAnalysisResult, error) {
	// Find JSON in response
	start := strings.Index(response, "{")
	end := strings.LastIndex(response, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("no JSON found in response")
	}

	jsonStr := response[start : end+1]

	var result SemanticAnalysisResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, err
	}

	return &result, nil
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

// extractFilePathFromError attempts to extract a file path from error message or args.
func extractFilePathFromError(errorMsg string, args map[string]any) string {
	// Try to get path from args first (most reliable)
	if args != nil {
		for _, key := range []string{"path", "file_path", "filepath", "file", "filename"} {
			if v, ok := args[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}

	// Try to extract from error message using regex
	// Match common path patterns
	pathPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?:file|path|directory)\s+['"]?([^\s'"]+?)['"]?\s+(?:not found|does not exist)`),
		regexp.MustCompile(`no such file or directory:\s*['"]?([^\s'"]+)`),
		regexp.MustCompile(`cannot find\s+['"]?([^\s'"]+)`),
	}

	for _, re := range pathPatterns {
		if matches := re.FindStringSubmatch(errorMsg); len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
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
			Pattern:            regexp.MustCompile(`command not found|executable.*not found|unknown command|not recognized`),
			Category:           "command_not_found",
			Suggestion:         "The command doesn't exist in PATH. Check spelling, install the required package, or use an absolute path.",
			ShouldRetry:        false,
			ShouldRetryWithFix: true,
			SuggestedFix:       "Check if the binary is installed or use full path.",
			Alternative:        "",
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
