package tools

import (
	"context"

	"google.golang.org/genai"
)

// Tool defines the interface for all tools.
type Tool interface {
	// Name returns the unique name of the tool.
	Name() string

	// Description returns a human-readable description.
	Description() string

	// Declaration returns the Gemini function declaration for this tool.
	Declaration() *genai.FunctionDeclaration

	// Execute runs the tool with the given arguments.
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)

	// Validate validates the arguments before execution.
	Validate(args map[string]any) error
}

// ToolRequester is an interface for components that can add tools dynamically.
// Implemented by agent.Agent.
type ToolRequester interface {
	RequestTool(name string) error
}

// Messenger is an interface for inter-agent communication.
type Messenger interface {
	SendMessage(msgType string, toRole string, content string, data map[string]any) (string, error)
	ReceiveResponse(ctx context.Context, messageID string) (string, error)
}

// ToolResult represents the result of a tool execution.
type ToolResult struct {
	// Content is the main result content (usually text).
	Content string

	// Data contains structured data if applicable.
	Data any

	// Error contains an error message if the tool failed.
	Error string

	// Success indicates if the tool executed successfully.
	Success bool

	// Execution metadata for user awareness
	ExecutionSummary *ExecutionSummary // Summary of what was done
	SafetyLevel      SafetyLevel       // Risk level of the operation
	Duration         string            // Human-readable duration
}

// NewSuccessResult creates a successful tool result.
func NewSuccessResult(content string) ToolResult {
	return ToolResult{
		Content: content,
		Success: true,
	}
}

// NewSuccessResultWithData creates a successful tool result with additional data.
func NewSuccessResultWithData(content string, data any) ToolResult {
	return ToolResult{
		Content: content,
		Data:    data,
		Success: true,
	}
}

// NewErrorResult creates a failed tool result.
func NewErrorResult(errMsg string) ToolResult {
	return ToolResult{
		Error:   errMsg,
		Success: false,
	}
}

// ToMap converts the result to a map for Gemini function response.
func (r ToolResult) ToMap() map[string]any {
	result := make(map[string]any)

	if r.Success {
		result["success"] = true
		if r.Content != "" {
			result["content"] = r.Content
		}
		if r.Data != nil {
			result["data"] = r.Data
		}
	} else {
		result["success"] = false
		result["error"] = r.Error
	}

	return result
}

// ValidationError represents a tool argument validation error.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// NewValidationError creates a new validation error.
func NewValidationError(field, message string) ValidationError {
	return ValidationError{Field: field, Message: message}
}

// GetString extracts a string argument from the args map.
func GetString(args map[string]any, key string) (string, bool) {
	val, ok := args[key]
	if !ok {
		return "", false
	}
	str, ok := val.(string)
	return str, ok
}

// GetStringDefault extracts a string argument with a default value.
func GetStringDefault(args map[string]any, key, defaultVal string) string {
	if val, ok := GetString(args, key); ok {
		return val
	}
	return defaultVal
}

// GetInt extracts an integer argument from the args map.
func GetInt(args map[string]any, key string) (int, bool) {
	val, ok := args[key]
	if !ok {
		return 0, false
	}
	// Gemini may return numbers as float64
	switch v := val.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// GetIntDefault extracts an integer argument with a default value.
func GetIntDefault(args map[string]any, key string, defaultVal int) int {
	if val, ok := GetInt(args, key); ok {
		return val
	}
	return defaultVal
}

// GetBool extracts a boolean argument from the args map.
func GetBool(args map[string]any, key string) (bool, bool) {
	val, ok := args[key]
	if !ok {
		return false, false
	}
	b, ok := val.(bool)
	return b, ok
}

// GetBoolDefault extracts a boolean argument with a default value.
func GetBoolDefault(args map[string]any, key string, defaultVal bool) bool {
	if val, ok := GetBool(args, key); ok {
		return val
	}
	return defaultVal
}

// DiffHandler is the interface for prompting user approval of file changes.
type DiffHandler interface {
	// PromptDiff displays a diff preview and waits for user approval.
	// Returns true if user approved, false if rejected.
	PromptDiff(ctx context.Context, filePath, oldContent, newContent, toolName string, isNewFile bool) (bool, error)
}

// skipDiffKey is a context key to signal that diff approval should be skipped.
// Used during delegated plan execution where the plan itself was already approved.
type skipDiffKeyType struct{}

// ContextWithSkipDiff returns a context that signals tools to skip diff approval prompts.
func ContextWithSkipDiff(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipDiffKeyType{}, true)
}

// ShouldSkipDiff checks whether diff approval should be skipped for this context.
func ShouldSkipDiff(ctx context.Context) bool {
	v, _ := ctx.Value(skipDiffKeyType{}).(bool)
	return v
}

// StreamingToolResult represents a tool result that streams its output.
type StreamingToolResult struct {
	Chunks <-chan string    // Chunks of output
	Done   <-chan struct{}  // Signals completion
	Error  <-chan error     // Error channel
}

// StreamingTool is an optional interface for tools with large outputs.
// Tools implementing this interface can stream results incrementally
// instead of returning all content at once.
type StreamingTool interface {
	Tool
	// ExecuteStreaming runs the tool with streaming output.
	ExecuteStreaming(ctx context.Context, args map[string]any) (*StreamingToolResult, error)
	// SupportsStreaming returns true if the tool supports streaming.
	SupportsStreaming() bool
}

// CollectStreamingResult collects all chunks from a streaming result.
// Returns the complete content and any error that occurred.
func CollectStreamingResult(sr *StreamingToolResult) (string, error) {
	if sr == nil {
		return "", nil
	}

	var content string
	for {
		select {
		case chunk, ok := <-sr.Chunks:
			if !ok {
				// Chunks channel closed, check for error
				select {
				case err := <-sr.Error:
					if err != nil {
						return content, err
					}
				default:
				}
				return content, nil
			}
			content += chunk
		case err := <-sr.Error:
			if err != nil {
				return content, err
			}
		case <-sr.Done:
			// Drain remaining chunks
			for chunk := range sr.Chunks {
				content += chunk
			}
			return content, nil
		}
	}
}

// NewStreamingToolResult creates a new streaming result with the given buffer sizes.
func NewStreamingToolResult(chunkBuffer int) (*StreamingToolResult, chan<- string, chan<- error, func()) {
	chunks := make(chan string, chunkBuffer)
	done := make(chan struct{})
	errChan := make(chan error, 1)

	complete := func() {
		close(chunks)
		close(done)
	}

	return &StreamingToolResult{
		Chunks: chunks,
		Done:   done,
		Error:  errChan,
	}, chunks, errChan, complete
}
