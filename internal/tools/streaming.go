package tools

import "context"

// streamingCallbackKey is the context key for streaming callbacks.
type streamingCallbackKey struct{}

// StreamingCallback is a function that receives streaming text output.
type StreamingCallback func(text string)

// ContextWithStreamingCallback returns a new context with the streaming callback attached.
// This allows tool execution to stream output back to the caller in real-time.
func ContextWithStreamingCallback(ctx context.Context, onText StreamingCallback) context.Context {
	return context.WithValue(ctx, streamingCallbackKey{}, onText)
}

// GetStreamingCallback retrieves the streaming callback from the context, if present.
// Returns nil if no callback was attached.
func GetStreamingCallback(ctx context.Context) StreamingCallback {
	if cb, ok := ctx.Value(streamingCallbackKey{}).(StreamingCallback); ok {
		return cb
	}
	return nil
}

// progressCallbackKey is the context key for progress callbacks.
type progressCallbackKey struct{}

// ProgressCallback is a function that receives progress updates.
type ProgressCallback func(progress float64, currentStep string)

// ContextWithProgressCallback returns a new context with the progress callback attached.
func ContextWithProgressCallback(ctx context.Context, onProgress ProgressCallback) context.Context {
	return context.WithValue(ctx, progressCallbackKey{}, onProgress)
}

// GetProgressCallback retrieves the progress callback from the context, if present.
func GetProgressCallback(ctx context.Context) ProgressCallback {
	if cb, ok := ctx.Value(progressCallbackKey{}).(ProgressCallback); ok {
		return cb
	}
	return nil
}

// toolsUsedKey is the context key for tracking tools used.
type toolsUsedKey struct{}

// ToolsUsedTracker tracks which tools have been used during execution.
type ToolsUsedTracker struct {
	tools []string
}

// Add adds a tool to the tracker.
func (t *ToolsUsedTracker) Add(toolName string) {
	t.tools = append(t.tools, toolName)
}

// List returns all tools used.
func (t *ToolsUsedTracker) List() []string {
	return t.tools
}

// ContextWithToolsTracker returns a new context with a tools used tracker attached.
func ContextWithToolsTracker(ctx context.Context, tracker *ToolsUsedTracker) context.Context {
	return context.WithValue(ctx, toolsUsedKey{}, tracker)
}

// GetToolsTracker retrieves the tools used tracker from the context, if present.
func GetToolsTracker(ctx context.Context) *ToolsUsedTracker {
	if tracker, ok := ctx.Value(toolsUsedKey{}).(*ToolsUsedTracker); ok {
		return tracker
	}
	return nil
}
