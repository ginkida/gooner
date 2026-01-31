package tools

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"gooner/internal/audit"
	"gooner/internal/client"
	"gooner/internal/hooks"
	"gooner/internal/logging"
	"gooner/internal/permission"
	"gooner/internal/robustness"
	"gooner/internal/security"

	"google.golang.org/genai"
)

// ResultCompactor interface for compacting tool results.
type ResultCompactor interface {
	CompactForType(toolName string, result ToolResult) ToolResult
}

// FallbackConfig holds configurable fallback text for when AI doesn't respond after tool use.
type FallbackConfig struct {
	ToolResponseText  string // Text shown when tools were used but no response
	EmptyResponseText string // Text shown when no response at all
}

// DefaultFallbackConfig returns the default fallback configuration.
func DefaultFallbackConfig() FallbackConfig {
	return FallbackConfig{
		ToolResponseText:  "I've completed the requested operations. Based on the tools I used, here's what I found:\n\nPlease let me know if you'd like me to analyze this further or if you have other questions.",
		EmptyResponseText: "I'm here to help with software development tasks. What would you like me to do?",
	}
}

// SmartFallbackConfig contains tool-specific fallback messages.
var SmartFallbackConfig = map[string]struct {
	Success string // Message when tool succeeds but model doesn't respond
	Empty   string // Message when tool returns empty results
	Error   string // Message template for errors
}{
	"read": {
		Success: "I've read the file. Here's a summary of what I found:\n\n[The file was read successfully. Key observations should be provided above.]\n\nWould you like me to explain any specific part?",
		Empty:   "The file appears to be empty or could not be read. Would you like me to:\n1. Check if the file exists\n2. Try reading a different file\n3. Search for similar files",
		Error:   "I encountered an issue reading the file: %s\n\nSuggestions:\n- Check if the file path is correct\n- Verify file permissions\n- Try reading a different file",
	},
	"grep": {
		Success: "Here's what I found from searching:\n\n[Search results should be analyzed above.]\n\nWould you like me to read any of these files in detail?",
		Empty:   "No matches found for this pattern. This could mean:\n1. The pattern doesn't exist in this codebase\n2. Try a different search pattern\n3. The content might be in different files\n\nWould you like me to try a different search?",
		Error:   "Search encountered an issue: %s\n\nSuggestions:\n- Check regex syntax\n- Try a simpler pattern\n- Narrow down the search path",
	},
	"glob": {
		Success: "I found these files. Here's an overview:\n\n[File list should be categorized above.]\n\nWhich files would you like me to examine?",
		Empty:   "No files match this pattern. Possible reasons:\n1. Pattern syntax might be incorrect\n2. Files might have different extensions\n3. Directory might not exist\n\nTry patterns like:\n- `**/*.go` for Go files\n- `**/*.ts` for TypeScript\n- `**/config.*` for config files",
		Error:   "File search failed: %s\n\nCheck:\n- Pattern syntax (use ** for recursive)\n- Directory path exists\n- Permissions on directories",
	},
	"bash": {
		Success: "Command executed. Here's what happened:\n\n[Command output should be explained above.]\n\nIs there anything else you'd like me to run?",
		Empty:   "The command completed successfully but produced no output. This is often normal for:\n- File operations (cp, mv, mkdir)\n- Configuration changes\n- Silent success modes\n\nThe operation was likely successful.",
		Error:   "Command failed: %s\n\nSuggestions:\n- Check command syntax\n- Verify paths and permissions\n- Look at the error message for clues",
	},
	"write": {
		Success: "File written successfully. The changes include:\n\n[File contents should be summarized above.]\n\nWould you like me to verify the file or make additional changes?",
		Empty:   "File operation completed.",
		Error:   "Failed to write file: %s\n\nCheck:\n- Directory exists\n- Write permissions\n- Disk space",
	},
	"edit": {
		Success: "File edited. Here's what changed:\n\n[Changes should be detailed above.]\n\nWould you like me to verify the changes or make additional modifications?",
		Empty:   "No changes were needed - the content already matches.",
		Error:   "Edit failed: %s\n\nThis usually means:\n- The search text wasn't found\n- File content has changed\n- Try reading the file first to see current content",
	},
}

// GetSmartFallback returns an appropriate fallback message based on tool and result.
func GetSmartFallback(toolName string, result ToolResult, toolsUsed []string) string {
	// Get tool-specific config
	config, hasConfig := SmartFallbackConfig[toolName]

	// Build context about what tools were used
	var toolContext string
	if len(toolsUsed) > 0 {
		toolContext = fmt.Sprintf("\n\n**Tools used:** %s", strings.Join(toolsUsed, ", "))
	}

	// Handle case where result is empty/default (tools were called but result not tracked properly)
	// This is a graceful fallback - don't show error messages for missing data
	isEmptyResult := result.Content == "" && result.Error == "" && !result.Success

	// Handle error cases - but only if there's an actual error message
	if !result.Success && result.Error != "" {
		if hasConfig {
			return fmt.Sprintf(config.Error, result.Error) + toolContext
		}
		return fmt.Sprintf("Operation encountered an issue: %s\n\nWould you like me to try a different approach?", result.Error) + toolContext
	}

	// Handle empty/default result - provide helpful generic message
	if isEmptyResult {
		if hasConfig {
			return config.Success + toolContext
		}
		return "I've completed the requested operations. The results should be shown above.\n\nIs there anything specific you'd like me to explain or analyze further?" + toolContext
	}

	// Handle empty content results (tool ran but returned nothing)
	if result.Content == "" || result.Content == "(empty file)" || result.Content == "(no output)" || result.Content == "No matches found." {
		if hasConfig {
			return config.Empty + toolContext
		}
		return "The operation completed but returned no results. This may be expected depending on the context." + toolContext
	}

	// Handle success with content
	if hasConfig {
		return config.Success + toolContext
	}

	return "I've completed the operation. The results are shown above. Would you like me to analyze them further?" + toolContext
}

// Executor handles the function calling loop with enhanced safety and user awareness.
type Executor struct {
	registry    *Registry
	client      client.Client
	timeout     time.Duration
	handler     *ExecutionHandler
	compactor   ResultCompactor
	permissions *permission.Manager
	hooks       *hooks.Manager
	auditLogger *audit.Logger
	sessionID   string
	fallback    FallbackConfig
	historyMu   sync.Mutex // Protects history modifications in executeLoop

	// Enhanced safety features
	safetyValidator  SafetyValidator
	preFlightChecks  bool                      // Enable pre-flight safety checks
	userNotified     bool                      // Track if user was notified about tool execution
	executionContext map[string]*ExecutionInfo // Track active executions
	notificationMgr  *NotificationManager      // User notifications
	redactor         *security.SecretRedactor  // Redact secrets from output

	// Token usage from last response (from API usage metadata)
	lastInputTokens  int
	lastOutputTokens int

	// Circuit breakers for tools
	toolBreakers map[string]*robustness.CircuitBreaker
}

// ExecutionInfo holds information about an active tool execution
type ExecutionInfo struct {
	StartTime      time.Time
	ToolName       string
	Args           map[string]any
	SafetyLevel    SafetyLevel
	Summary        *ExecutionSummary
	PreFlightCheck *PreFlightCheck
	ParentTool     string // For nested tool calls
}

// ExecutionHandler provides callbacks for execution events.
type ExecutionHandler struct {
	// OnText is called when text is streamed from the model.
	OnText func(text string)

	// OnToolStart is called when a tool begins execution.
	OnToolStart func(name string, args map[string]any)

	// OnToolEnd is called when a tool finishes execution.
	OnToolEnd func(name string, result ToolResult)

	// OnToolProgress is called periodically during long-running tool execution.
	// This helps keep the UI alive and prevents timeout during long operations.
	OnToolProgress func(name string, elapsed time.Duration)

	// OnToolValidating is called before tool execution to show safety checks.
	OnToolValidating func(name string, check *PreFlightCheck)

	// OnToolApproved is called when user approves a tool execution.
	OnToolApproved func(name string, summary *ExecutionSummary)

	// OnToolDenied is called when user denies a tool execution.
	OnToolDenied func(name string, reason string)

	// OnError is called when an error occurs.
	OnError func(err error)

	// OnWarning is called when a warning is issued.
	OnWarning func(warning string)
}

// NewExecutor creates a new tool executor.
func NewExecutor(registry *Registry, c client.Client, timeout time.Duration) *Executor {
	return &Executor{
		registry:         registry,
		client:           c,
		timeout:          timeout,
		handler:          &ExecutionHandler{},
		fallback:         DefaultFallbackConfig(),
		safetyValidator:  NewDefaultSafetyValidator(),
		preFlightChecks:  true,
		executionContext: make(map[string]*ExecutionInfo),
		notificationMgr:  NewNotificationManager(),
		toolBreakers:     make(map[string]*robustness.CircuitBreaker),
		redactor:         security.NewSecretRedactor(),
	}
}

// SetClient updates the underlying client.
func (e *Executor) SetClient(c client.Client) {
	e.client = c
}

// SetSafetyValidator sets the safety validator.
func (e *Executor) SetSafetyValidator(validator SafetyValidator) {
	e.safetyValidator = validator
}

// EnablePreFlightChecks enables or disables pre-flight safety checks.
func (e *Executor) EnablePreFlightChecks(enabled bool) {
	e.preFlightChecks = enabled
}

// SetFallbackConfig sets the fallback configuration for tool responses.
func (e *Executor) SetFallbackConfig(config FallbackConfig) {
	e.fallback = config
}

// SetHandler sets the execution event handler.
func (e *Executor) SetHandler(handler *ExecutionHandler) {
	if handler != nil {
		e.handler = handler
	}
}

// SetCompactor sets the result compactor for tool output optimization.
func (e *Executor) SetCompactor(compactor ResultCompactor) {
	e.compactor = compactor
}

// SetPermissions sets the permission manager for tool execution.
func (e *Executor) SetPermissions(mgr *permission.Manager) {
	e.permissions = mgr
}

// SetHooks sets the hooks manager for tool execution.
func (e *Executor) SetHooks(mgr *hooks.Manager) {
	e.hooks = mgr
}

// SetAuditLogger sets the audit logger for tool execution.
func (e *Executor) SetAuditLogger(logger *audit.Logger) {
	e.auditLogger = logger
}

// SetSessionID sets the session ID for audit logging.
func (e *Executor) SetSessionID(id string) {
	e.sessionID = id
}

// GetNotificationManager returns the notification manager.
func (e *Executor) GetNotificationManager() *NotificationManager {
	return e.notificationMgr
}

// GetLastTokenUsage returns the token usage from the last API response.
// Returns (inputTokens, outputTokens). Values are 0 if metadata was unavailable.
func (e *Executor) GetLastTokenUsage() (int, int) {
	return e.lastInputTokens, e.lastOutputTokens
}

// Execute processes a user message through the function calling loop.
func (e *Executor) Execute(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	// Add user message to history
	userContent := genai.NewContentFromText(message, genai.RoleUser)
	history = append(history, userContent)

	return e.executeLoop(ctx, history)
}

// executeLoop runs the function calling loop until a final text response is received.
func (e *Executor) executeLoop(ctx context.Context, history []*genai.Content) ([]*genai.Content, string, error) {
	// === IMPROVEMENT 3: Dynamic max iterations based on context complexity ===
	maxIterations := e.calculateMaxIterations(history)

	var finalText string
	var toolsUsed []string        // Track which tools were used for smart fallback
	var lastToolResult ToolResult // Track the last tool result for context

	for i := 0; i < maxIterations; i++ {
		// Get response from model
		resp, err := e.getModelResponse(ctx, history)
		if err != nil {
			// Error will be returned and displayed by UI - no need to call OnError here
			return history, "", fmt.Errorf("model response error: %w", err)
		}

		// Add model response to history (with mutex protection)
		modelContent := &genai.Content{
			Role:  genai.RoleModel,
			Parts: e.buildResponseParts(resp),
		}

		e.historyMu.Lock()
		history = append(history, modelContent)
		e.historyMu.Unlock()

		// Handle chained function calls in an inner loop
		for len(resp.FunctionCalls) > 0 {
			// Track tools being called
			for _, fc := range resp.FunctionCalls {
				toolsUsed = append(toolsUsed, fc.Name)
			}

			results, err := e.executeTools(ctx, resp.FunctionCalls)
			if err != nil {
				return history, "", fmt.Errorf("tool execution error: %w", err)
			}

			// Track last result for smart fallback
			if len(results) > 0 {
				lastResult := results[len(results)-1]
				respMap := lastResult.Response // Already map[string]any
				content, _ := respMap["content"].(string)
				success, _ := respMap["success"].(bool)
				errMsg, _ := respMap["error"].(string)
				lastToolResult = ToolResult{Content: content, Success: success, Error: errMsg}
			}

			// Send function responses back to model
			stream, err := e.client.SendFunctionResponse(ctx, history, results)
			if err != nil {
				return history, "", fmt.Errorf("function response error: %w", err)
			}

			// Collect the response
			resp, err = e.collectStreamWithHandler(ctx, stream)
			if err != nil {
				return history, "", err
			}

			// CRITICAL: Add function results to history for chained tool calls
			funcResultParts := make([]*genai.Part, len(results))
			for j, result := range results {
				funcResultParts[j] = genai.NewPartFromFunctionResponse(result.Name, result.Response)
				funcResultParts[j].FunctionResponse.ID = result.ID
			}
			funcResultContent := &genai.Content{
				Role:  genai.RoleUser,
				Parts: funcResultParts,
			}

			e.historyMu.Lock()
			history = append(history, funcResultContent)
			e.historyMu.Unlock()

			// Add model response to history (with mutex protection)
			modelContent = &genai.Content{
				Role:  genai.RoleModel,
				Parts: e.buildResponseParts(resp),
			}

			e.historyMu.Lock()
			history = append(history, modelContent)
			e.historyMu.Unlock()
		}

		// No more function calls, we have the final response
		finalText = resp.Text

		// Capture token usage metadata from API response
		if resp.InputTokens > 0 || resp.OutputTokens > 0 {
			e.lastInputTokens = resp.InputTokens
			e.lastOutputTokens = resp.OutputTokens
		}

		if finalText != "" {
			break
		}
	}

	// CRITICAL: Ensure we always have a response
	if finalText == "" {
		// Check if tools were used in THIS request (not all history)
		// toolsUsed is populated during execution, so it only contains current turn's tools
		if len(toolsUsed) > 0 {
			// Use smart fallback based on the last tool used
			lastToolName := toolsUsed[len(toolsUsed)-1]
			finalText = GetSmartFallback(lastToolName, lastToolResult, toolsUsed)
			if e.handler != nil && e.handler.OnText != nil {
				e.handler.OnText(finalText)
			}
		} else {
			// No tools used in this request - simple conversation
			finalText = e.fallback.EmptyResponseText
			if e.handler != nil && e.handler.OnText != nil {
				e.handler.OnText(finalText)
			}
		}
	}

	return history, finalText, nil
}

// calculateMaxIterations determines the optimal iteration limit based on context complexity.
// More complex tasks with longer history get more iterations.
func (e *Executor) calculateMaxIterations(history []*genai.Content) int {
	baseLimit := 50

	// Count conversation turns (pairs of user/model messages)
	turnCount := len(history) / 2

	// Estimate complexity based on history length
	switch {
	case turnCount > 30:
		// Very long conversation - allow many iterations
		return baseLimit + 100
	case turnCount > 20:
		// Long conversation - significantly more iterations
		return baseLimit + 50
	case turnCount > 10:
		// Medium conversation - moderately more iterations
		return baseLimit + 25
	default:
		// Short conversation - base limit
		return baseLimit
	}
}

// getModelResponse gets an initial response from the model.
func (e *Executor) getModelResponse(ctx context.Context, history []*genai.Content) (*client.Response, error) {
	if len(history) == 0 {
		return nil, fmt.Errorf("cannot get model response: empty history")
	}

	var message string
	lastContent := history[len(history)-1]
	if lastContent.Role == genai.RoleUser {
		for _, part := range lastContent.Parts {
			if part.Text != "" {
				message = part.Text
				break
			}
		}
	}

	historyWithoutLast := history[:len(history)-1]

	stream, err := e.client.SendMessageWithHistory(ctx, historyWithoutLast, message)
	if err != nil {
		return nil, err
	}

	return e.collectStreamWithHandler(ctx, stream)
}

// collectStreamWithHandler collects streaming response while calling handlers.
func (e *Executor) collectStreamWithHandler(ctx context.Context, stream *client.StreamingResponse) (*client.Response, error) {
	// Note: Don't pass OnError here - the executor handles error display
	// to avoid duplicate error messages
	var onText func(string)
	if e.handler != nil {
		onText = e.handler.OnText
	}
	return client.ProcessStream(ctx, stream, &client.StreamHandler{
		OnText: onText,
	})
}

// executeTools executes a list of function calls with enhanced safety checks.
func (e *Executor) executeTools(ctx context.Context, calls []*genai.FunctionCall) ([]*genai.FunctionResponse, error) {
	results := make([]*genai.FunctionResponse, len(calls))

	// For single tool, execute directly
	if len(calls) == 1 {
		result := e.executeTool(ctx, calls[0])
		results[0] = &genai.FunctionResponse{
			ID:       calls[0].ID,
			Name:     calls[0].Name,
			Response: result.ToMap(),
		}
		return results, nil
	}

	// For multiple tools, execute in parallel
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, fc *genai.FunctionCall) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					stack := make([]byte, 4096)
					length := runtime.Stack(stack, false)
					logging.Error("tool execution panic",
						"tool", fc.Name,
						"panic", r,
						"stack", string(stack[:length]))

					mu.Lock()
					results[idx] = &genai.FunctionResponse{
						ID:       fc.ID,
						Name:     fc.Name,
						Response: NewErrorResult(fmt.Sprintf("panic: %v", r)).ToMap(),
					}
					mu.Unlock()
				}
			}()

			select {
			case <-ctx.Done():
				mu.Lock()
				results[idx] = &genai.FunctionResponse{
					ID:       fc.ID,
					Name:     fc.Name,
					Response: NewErrorResult("cancelled").ToMap(),
				}
				mu.Unlock()
				return
			default:
			}

			result := e.executeTool(ctx, fc)

			mu.Lock()
			results[idx] = &genai.FunctionResponse{
				ID:       fc.ID,
				Name:     fc.Name,
				Response: result.ToMap(),
			}
			mu.Unlock()
		}(i, call)
	}

	wg.Wait()
	return results, nil
}

// executeTool executes a single tool call with enhanced safety and user awareness.
func (e *Executor) executeTool(ctx context.Context, call *genai.FunctionCall) ToolResult {
	// Step 0: Check circuit breaker
	breaker, ok := e.toolBreakers[call.Name]
	if !ok {
		// Initialize with default threshold (5 failures) and timeout (1 minute)
		breaker = robustness.NewCircuitBreaker(5, 1*time.Minute)
		e.toolBreakers[call.Name] = breaker
	}

	var result ToolResult
	err := breaker.Execute(ctx, func() error {
		res := e.doExecuteTool(ctx, call)
		result = res
		if !res.Success {
			return fmt.Errorf("tool execution failed: %s", res.Error)
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, robustness.ErrCircuitOpen) {
			return NewErrorResult(fmt.Sprintf("Circuit breaker for '%s' is open. Too many failures. Please wait before trying again.", call.Name))
		}
		// If it's not circuit open error, it's the wrapped tool error, which is already in 'result'
	}

	return result
}

// doExecuteTool handles the actual execution logic (previously body of executeTool).
func (e *Executor) doExecuteTool(ctx context.Context, call *genai.FunctionCall) ToolResult {
	// Step 1: Basic tool lookup and validation
	tool, ok := e.registry.Get(call.Name)
	if !ok {
		return NewErrorResult(fmt.Sprintf("unknown tool: %s", call.Name))
	}

	if err := tool.Validate(call.Args); err != nil {
		return NewErrorResult(fmt.Sprintf("validation error: %s", err))
	}

	// Step 2: Pre-flight safety checks
	var preFlight *PreFlightCheck
	if e.preFlightChecks && e.safetyValidator != nil {
		var err error
		preFlight, err = e.safetyValidator.ValidateSafety(ctx, call.Name, call.Args)
		if err != nil {
			logging.Warn("safety check failed", "tool", call.Name, "error", err)
		}

		// Notify user about validation
		if e.handler != nil && e.handler.OnToolValidating != nil {
			e.handler.OnToolValidating(call.Name, preFlight)
		}

		// Emit warnings to notification system
		if preFlight != nil && len(preFlight.Warnings) > 0 {
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyValidation(call.Name, preFlight)
			}
			if e.handler != nil && e.handler.OnWarning != nil {
				for _, warning := range preFlight.Warnings {
					e.handler.OnWarning(fmt.Sprintf("[%s] %s", call.Name, warning))
				}
			}
		}

		// Block execution if safety check failed
		if preFlight != nil && !preFlight.IsValid {
			reasons := strings.Join(preFlight.Errors, "; ")
			if e.handler != nil && e.handler.OnToolDenied != nil {
				e.handler.OnToolDenied(call.Name, reasons)
			}
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyDenied(call.Name, reasons)
			}
			return NewErrorResult(fmt.Sprintf("Safety check failed: %s", reasons))
		}
	}

	// Step 3: Get execution summary for user awareness
	var summary *ExecutionSummary
	if e.safetyValidator != nil {
		summary = e.safetyValidator.GetSummary(call.Name, call.Args)
	}

	// Step 4: Permission check
	if e.permissions != nil {
		resp, err := e.permissions.Check(ctx, call.Name, call.Args)
		if err != nil {
			if e.handler != nil && e.handler.OnToolDenied != nil {
				e.handler.OnToolDenied(call.Name, err.Error())
			}
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyDenied(call.Name, err.Error())
			}
			return NewErrorResult(fmt.Sprintf("permission error: %s", err))
		}
		if !resp.Allowed {
			reason := resp.Reason
			if reason == "" {
				reason = "permission denied"
			}
			if e.handler != nil && e.handler.OnToolDenied != nil {
				e.handler.OnToolDenied(call.Name, reason)
			}
			if e.notificationMgr != nil {
				e.notificationMgr.NotifyDenied(call.Name, reason)
			}
			return NewErrorResult(fmt.Sprintf("Permission denied: %s", reason))
		}
	}

	// Step 5: Notify user about approved execution
	if summary != nil && summary.UserVisible {
		if e.notificationMgr != nil {
			e.notificationMgr.NotifyApproved(call.Name, summary)
		}
		if e.handler != nil && e.handler.OnToolApproved != nil {
			e.handler.OnToolApproved(call.Name, summary)
		}
	}

	// Step 6: Run pre-tool hooks
	if e.hooks != nil {
		e.hooks.RunPreTool(ctx, call.Name, call.Args)
	}

	// Step 7: Create execution context
	execInfo := &ExecutionInfo{
		StartTime:      time.Now(),
		ToolName:       call.Name,
		Args:           call.Args,
		Summary:        summary,
		PreFlightCheck: preFlight,
	}
	if summary != nil {
		execInfo.SafetyLevel = summary.RiskLevel
	}

	// Notify start
	if e.handler != nil && e.handler.OnToolStart != nil {
		e.handler.OnToolStart(call.Name, call.Args)
	}

	// Step 8: Execute with timeout
	execCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	start := time.Now()

	// Start progress heartbeat for long-running operations
	done := make(chan struct{})
	if e.handler != nil && e.handler.OnToolProgress != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logging.Error("panic in tool progress goroutine", "tool", call.Name, "panic", r)
				}
			}()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					elapsed := time.Since(start)
					if e.handler != nil && e.handler.OnToolProgress != nil {
						e.handler.OnToolProgress(call.Name, elapsed)
					}
				case <-done:
					return
				case <-execCtx.Done():
					return
				}
			}
		}()
	}

	result, err := tool.Execute(execCtx, call.Args)
	close(done)
	duration := time.Since(start)

	if err != nil {
		logging.Error("tool execution failed",
			"tool", call.Name,
			"error", err,
			"duration", duration)
		result = NewErrorResult(err.Error())
	}

	// Enrich result with execution metadata
	if result.ExecutionSummary == nil && summary != nil {
		result.ExecutionSummary = summary
	}
	result.SafetyLevel = execInfo.SafetyLevel
	result.Duration = formatDuration(duration)

	// Step 9: Log to audit
	if e.auditLogger != nil {
		entry := audit.NewEntry(e.sessionID, call.Name, call.Args)
		entry.Complete(result.Content, result.Success, result.Error, duration)

		// Add safety context to audit log
		if preFlight != nil {
			entry.Args["safety_warnings"] = preFlight.Warnings
			entry.Args["safety_level"] = string(execInfo.SafetyLevel)
		}

		if err := e.auditLogger.Log(entry); err != nil {
			logging.Warn("failed to write audit log", "error", err, "tool", call.Name)
		}
	}

	// Step 10: Run post-tool or on-error hooks
	if e.hooks != nil {
		if result.Success {
			e.hooks.RunPostTool(ctx, call.Name, call.Args, result.Content)
		} else {
			e.hooks.RunOnError(ctx, call.Name, call.Args, result.Error)
		}
	}

	// Step 11: apply redaction
	if e.redactor != nil {
		result.Content = e.redactor.Redact(result.Content)
		result.Error = e.redactor.Redact(result.Error)
		// Also redact in response map if needed, but Content/Error are primary
	}

	// Step 12: Apply compaction if configured
	if e.compactor != nil && result.Success {
		result = e.compactor.CompactForType(call.Name, result)
	}

	// Step 13: Notify completion and send notifications
	if e.handler != nil && e.handler.OnToolEnd != nil {
		e.handler.OnToolEnd(call.Name, result)
	}

	if e.notificationMgr != nil {
		if result.Success {
			// Nil check for summary before calling String()
			var summaryStr string
			if summary != nil {
				summaryStr = summary.String()
			}
			e.notificationMgr.NotifySuccess(call.Name, summaryStr, summary, duration)
		} else {
			e.notificationMgr.NotifyError(call.Name, "Execution failed", result.Error)
		}
	}

	// Log execution metrics
	logging.Info("tool execution completed",
		"tool", call.Name,
		"success", result.Success,
		"duration", duration,
		"safety_level", execInfo.SafetyLevel)

	return result
}

// buildResponseParts returns Parts from a response.
func (e *Executor) buildResponseParts(resp *client.Response) []*genai.Part {
	if len(resp.Parts) > 0 {
		return resp.Parts
	}

	var parts []*genai.Part

	if resp.Text != "" {
		parts = append(parts, genai.NewPartFromText(resp.Text))
	}

	for _, fc := range resp.FunctionCalls {
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}

	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(" "))
	}

	return parts
}

// formatDuration converts duration to human-readable string
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "< 1ms"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Second).String()
}
