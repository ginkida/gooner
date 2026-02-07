package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gokin/internal/logging"

	"github.com/ollama/ollama/api"
	"google.golang.org/genai"
)

// OllamaConfig holds configuration for Ollama API client.
type OllamaConfig struct {
	BaseURL     string        // Default: "http://localhost:11434"
	APIKey      string        // Optional, for remote Ollama servers with auth
	Model       string        // e.g., "llama3.2", "qwen2.5-coder"
	Temperature float32       // Temperature for generation
	MaxTokens   int32         // Max output tokens
	HTTPTimeout time.Duration // HTTP request timeout (default: 120s)
	// Retry configuration
	MaxRetries int           // Maximum retry attempts (default: 3)
	RetryDelay time.Duration // Initial delay between retries (default: 1s)
}

// OllamaClient implements Client interface for Ollama API.
type OllamaClient struct {
	client            *api.Client
	config            OllamaConfig
	tools             []*genai.Tool
	rateLimiter       RateLimiter
	statusCallback    StatusCallback
	systemInstruction string
	mu                sync.RWMutex
}

// authTransport adds Authorization header to HTTP requests.
type authTransport struct {
	base   http.RoundTripper
	apiKey string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid modifying the original
	reqClone := req.Clone(req.Context())
	reqClone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return t.base.RoundTrip(reqClone)
}

// NewOllamaClient creates a new Ollama API client.
func NewOllamaClient(config OllamaConfig) (*OllamaClient, error) {
	// Validate model name
	if config.Model == "" {
		return nil, fmt.Errorf("model name is required")
	}

	// Set defaults
	if config.BaseURL == "" {
		config.BaseURL = "http://localhost:11434"
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = 8192
	}
	if config.HTTPTimeout == 0 {
		config.HTTPTimeout = 120 * time.Second
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = 1 * time.Second
	}

	// Parse base URL
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid BaseURL: %w", err)
	}

	// Warn if using unencrypted HTTP to a non-localhost host
	if baseURL.Scheme == "http" {
		host := baseURL.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			logging.Warn("Ollama connection uses unencrypted HTTP to remote host",
				"host", host,
				"recommendation", "use HTTPS for remote Ollama servers")
		}
	}

	// Create HTTP client with timeout and optional auth
	var httpClient *http.Client
	if config.APIKey != "" {
		// Add Authorization header for remote Ollama servers with auth
		httpClient = &http.Client{
			Timeout: config.HTTPTimeout,
			Transport: &authTransport{
				base:   http.DefaultTransport,
				apiKey: config.APIKey,
			},
		}
	} else {
		httpClient = &http.Client{
			Timeout: config.HTTPTimeout,
		}
	}

	// Create Ollama client
	ollamaClient := api.NewClient(baseURL, httpClient)

	return &OllamaClient{
		client: ollamaClient,
		config: config,
		tools:  make([]*genai.Tool, 0),
	}, nil
}

// SendMessage sends a message and returns a streaming response.
func (c *OllamaClient) SendMessage(ctx context.Context, message string) (*StreamingResponse, error) {
	return c.SendMessageWithHistory(ctx, nil, message)
}

// SendMessageWithHistory sends a message with conversation history.
func (c *OllamaClient) SendMessageWithHistory(ctx context.Context, history []*genai.Content, message string) (*StreamingResponse, error) {
	var messages []api.Message

	// For fallback models, convert FunctionCall/FunctionResponse parts to text
	if c.NeedsToolCallFallback() {
		messages = c.convertHistoryForFallback(history, nil)
		if message != "" {
			messages = append(messages, api.Message{Role: "user", Content: message})
		}
	} else {
		messages = c.convertHistoryToMessages(history, message)
	}

	// Build request
	req := &api.ChatRequest{
		Model:    c.config.Model,
		Messages: messages,
		Stream:   Ptr(true),
		Options: map[string]interface{}{
			"num_predict": c.config.MaxTokens,
		},
	}

	// Set temperature if specified
	if c.config.Temperature > 0 {
		req.Options["temperature"] = c.config.Temperature
	}

	// Only include native tools for models that support them
	if !c.NeedsToolCallFallback() {
		c.mu.RLock()
		if len(c.tools) > 0 {
			req.Tools = c.convertToolsToOllama()
		}
		c.mu.RUnlock()
	}

	return c.streamChat(ctx, req)
}

// SendFunctionResponse sends function call results back to the model.
func (c *OllamaClient) SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error) {
	var messages []api.Message

	// For models without native tool support, convert tool results to user messages
	// instead of tool role messages (which these models don't understand)
	if c.NeedsToolCallFallback() {
		messages = c.convertHistoryForFallback(history, results)
	} else {
		messages = c.convertHistoryWithResults(history, results)
	}

	req := &api.ChatRequest{
		Model:    c.config.Model,
		Messages: messages,
		Stream:   Ptr(true),
		Options: map[string]interface{}{
			"num_predict": c.config.MaxTokens,
		},
	}

	if c.config.Temperature > 0 {
		req.Options["temperature"] = c.config.Temperature
	}

	// Only include native tools for models that support them
	if !c.NeedsToolCallFallback() {
		c.mu.RLock()
		if len(c.tools) > 0 {
			req.Tools = c.convertToolsToOllama()
		}
		c.mu.RUnlock()
	}

	return c.streamChat(ctx, req)
}

// streamChat performs a streaming chat request with retry logic.
func (c *OllamaClient) streamChat(ctx context.Context, req *api.ChatRequest) (*StreamingResponse, error) {
	// Acquire rate limiter if configured
	var estimatedTokens int64 = 500 // Rough estimate for Ollama requests
	c.mu.RLock()
	rateLimiter := c.rateLimiter
	c.mu.RUnlock()

	if rateLimiter != nil {
		if err := rateLimiter.AcquireWithContext(ctx, estimatedTokens); err != nil {
			return nil, fmt.Errorf("rate limit: %w", err)
		}
	}

	var lastErr error
	maxDelay := 30 * time.Second

	// Retry loop
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := calculateBackoffWithJitter(c.config.RetryDelay, attempt-1, maxDelay)
			logging.Info("retrying Ollama request", "attempt", attempt, "delay", delay)

			// Notify UI about retry
			c.mu.RLock()
			cb := c.statusCallback
			c.mu.RUnlock()
			if cb != nil {
				reason := "API error"
				if lastErr != nil {
					reason = lastErr.Error()
					// Shorten common error patterns
					if strings.Contains(reason, "connection refused") {
						reason = "Ollama not running"
					} else if strings.Contains(reason, "timeout") {
						reason = "timeout"
					} else if len(reason) > 50 {
						reason = reason[:47] + "..."
					}
				}
				cb.OnRetry(attempt, c.config.MaxRetries, delay, reason)
			}

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		response, err := c.doStreamChat(ctx, req)
		if err == nil {
			return response, nil
		}

		lastErr = err

		// Check if error is retryable
		if !c.isRetryableError(err) {
			// Return tokens on permanent failure
			if rateLimiter != nil {
				rateLimiter.ReturnTokens(1, estimatedTokens)
			}
			return nil, c.wrapOllamaError(err)
		}

		logging.Warn("Ollama request failed, will retry", "attempt", attempt, "error", err)
	}

	// Return tokens on exhausted retries
	if rateLimiter != nil {
		rateLimiter.ReturnTokens(1, estimatedTokens)
	}
	return nil, fmt.Errorf("max retries (%d) exceeded: %w", c.config.MaxRetries, c.wrapOllamaError(lastErr))
}

// doStreamChat performs a single streaming chat request.
func (c *OllamaClient) doStreamChat(ctx context.Context, req *api.ChatRequest) (*StreamingResponse, error) {
	chunks := make(chan ResponseChunk, 10)
	done := make(chan struct{})

	// Capture status callback for goroutine
	c.mu.RLock()
	statusCb := c.statusCallback
	c.mu.RUnlock()

	go func() {
		defer close(chunks)
		defer close(done)

		var inputTokens, outputTokens int

		err := c.client.Chat(ctx, req, func(resp api.ChatResponse) error {
			chunk := ResponseChunk{}

			// Extract text content
			if resp.Message.Content != "" {
				chunk.Text = resp.Message.Content
			}

			// Convert tool calls
			for i, tc := range resp.Message.ToolCalls {
				fc := c.convertOllamaToolCallToGenai(tc, i)
				chunk.FunctionCalls = append(chunk.FunctionCalls, fc)
			}

			// Handle completion
			if resp.Done {
				chunk.Done = true
				chunk.FinishReason = genai.FinishReasonStop

				// Extract token counts from final response
				if resp.PromptEvalCount > 0 {
					inputTokens = resp.PromptEvalCount
				}
				if resp.EvalCount > 0 {
					outputTokens = resp.EvalCount
				}

				chunk.InputTokens = inputTokens
				chunk.OutputTokens = outputTokens
			}

			// Send chunk
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return ctx.Err()
			}

			return nil
		})

		if err != nil {
			// Notify about recoverable errors
			if statusCb != nil && c.isRetryableError(err) {
				statusCb.OnError(err, true)
			}
			// Send error through chunks channel - this is the standard pattern
			select {
			case chunks <- ResponseChunk{Error: c.wrapOllamaError(err), Done: true}:
			case <-ctx.Done():
				// Context cancelled, just exit
			}
		}
	}()

	return &StreamingResponse{
		Chunks: chunks,
		Done:   done,
	}, nil
}

// SetSystemInstruction sets the system-level instruction for the model.
func (c *OllamaClient) SetSystemInstruction(instruction string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.systemInstruction = instruction
}

// SetThinkingBudget is a no-op for Ollama (not supported).
func (c *OllamaClient) SetThinkingBudget(budget int32) {}

// SetTools sets the tools available for function calling.
func (c *OllamaClient) SetTools(tools []*genai.Tool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools = tools
}

// SetRateLimiter sets the rate limiter for API calls.
func (c *OllamaClient) SetRateLimiter(limiter interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rl, ok := limiter.(RateLimiter); ok {
		c.rateLimiter = rl
	}
}

// SetStatusCallback sets the callback for status updates during operations.
func (c *OllamaClient) SetStatusCallback(cb StatusCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statusCallback = cb
}

// CountTokens estimates tokens for the given contents.
// Ollama returns token counts in the response, but for pre-counting we estimate.
func (c *OllamaClient) CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error) {
	// Estimate tokens by character count
	totalChars := 0
	for _, content := range contents {
		totalChars += 4 * 4 // role overhead (~4 tokens)
		for _, part := range content.Parts {
			if part.Text != "" {
				totalChars += len(part.Text)
			}
			if part.FunctionCall != nil {
				totalChars += len(part.FunctionCall.Name) + 40
				if argsJSON, err := json.Marshal(part.FunctionCall.Args); err == nil {
					totalChars += len(argsJSON)
				}
			}
			if part.FunctionResponse != nil {
				totalChars += len(part.FunctionResponse.Name) + 40
				if respJSON, err := json.Marshal(part.FunctionResponse.Response); err == nil {
					totalChars += len(respJSON)
				}
			}
		}
	}

	// Approximate 4 characters per token (varies by model)
	estimatedTokens := int32(totalChars / 4)
	return &genai.CountTokensResponse{
		TotalTokens: estimatedTokens,
	}, nil
}

// GetModel returns the model name.
func (c *OllamaClient) GetModel() string {
	return c.config.Model
}

// SetModel changes the model for this client.
func (c *OllamaClient) SetModel(modelName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.config.Model = modelName
}

// WithModel returns a new client configured for the specified model.
func (c *OllamaClient) WithModel(modelName string) Client {
	newConfig := c.config
	newConfig.Model = modelName
	newClient, err := NewOllamaClient(newConfig)
	if err != nil {
		logging.Error("failed to create Ollama client with new model", "model", modelName, "error", err)
		return c
	}
	c.mu.RLock()
	newClient.SetTools(c.tools)
	c.mu.RUnlock()
	return newClient
}

// GetRawClient returns the underlying Ollama client.
func (c *OllamaClient) GetRawClient() interface{} {
	return c.client
}

// NeedsToolCallFallback returns true if this client should use text-based
// tool call parsing as a fallback (for models without native function calling).
func (c *OllamaClient) NeedsToolCallFallback() bool {
	profile := GetModelProfile(c.config.Model)
	return !profile.SupportsTools
}

// Close closes the client connection.
func (c *OllamaClient) Close() error {
	// Ollama client doesn't require explicit close
	return nil
}

// ListModels returns a list of available models from the Ollama server.
func (c *OllamaClient) ListModels(ctx context.Context) ([]string, error) {
	resp, err := c.client.List(ctx)
	if err != nil {
		return nil, c.wrapOllamaError(err)
	}

	models := make([]string, 0, len(resp.Models))
	for _, model := range resp.Models {
		models = append(models, model.Name)
	}
	return models, nil
}

// PullProgress contains information about model download progress.
type PullProgress struct {
	Status    string  // "pulling", "verifying", "done"
	Digest    string  // Layer being downloaded
	Total     int64   // Total size in bytes
	Completed int64   // Downloaded bytes
	Percent   float64 // Completion percentage (0-100)
}

// PullModel downloads a model from Ollama Hub with progress reporting.
func (c *OllamaClient) PullModel(ctx context.Context, modelName string, progressFn func(PullProgress)) error {
	req := &api.PullRequest{
		Model: modelName,
	}

	return c.client.Pull(ctx, req, func(resp api.ProgressResponse) error {
		if progressFn != nil {
			var percent float64
			if resp.Total > 0 {
				percent = float64(resp.Completed) / float64(resp.Total) * 100
			}
			progressFn(PullProgress{
				Status:    resp.Status,
				Digest:    resp.Digest,
				Total:     resp.Total,
				Completed: resp.Completed,
				Percent:   percent,
			})
		}
		return nil
	})
}

// Healthcheck verifies that the Ollama server is accessible.
func (c *OllamaClient) Healthcheck(ctx context.Context) error {
	// Ollama SDK doesn't have an explicit ping, use List as healthcheck
	_, err := c.client.List(ctx)
	if err != nil {
		return c.wrapOllamaError(err)
	}
	return nil
}

// IsModelAvailable checks if a model is installed locally.
func (c *OllamaClient) IsModelAvailable(ctx context.Context, modelName string) (bool, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return false, err
	}

	for _, m := range models {
		// Check exact match or with :latest tag
		if m == modelName || m == modelName+":latest" ||
			strings.HasPrefix(m, modelName+":") {
			return true, nil
		}
	}
	return false, nil
}

// convertHistoryToMessages converts Gemini history to Ollama messages format.
func (c *OllamaClient) convertHistoryToMessages(history []*genai.Content, newMessage string) []api.Message {
	messages := make([]api.Message, 0, len(history)+2)

	// Prepend system instruction if set
	c.mu.RLock()
	sysInstruction := c.systemInstruction
	c.mu.RUnlock()
	if sysInstruction != "" {
		messages = append(messages, api.Message{Role: "system", Content: sysInstruction})
	}

	for _, content := range history {
		msg := c.convertContentToMessage(content)
		if msg.Role != "" {
			messages = append(messages, msg)
		}
	}

	// Add new user message
	if newMessage != "" {
		messages = append(messages, api.Message{
			Role:    "user",
			Content: newMessage,
		})
	}

	return messages
}

// convertContentToMessage converts a single genai.Content to api.Message.
func (c *OllamaClient) convertContentToMessage(content *genai.Content) api.Message {
	msg := api.Message{}

	// Map role
	switch content.Role {
	case genai.RoleUser:
		msg.Role = "user"
	case genai.RoleModel:
		msg.Role = "assistant"
	default:
		msg.Role = string(content.Role)
	}

	// Extract content
	var textParts []string
	var toolCalls []api.ToolCall

	for _, part := range content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			toolCalls = append(toolCalls, c.convertGenaiToolCallToOllama(part.FunctionCall))
		}
	}

	msg.Content = strings.Join(textParts, "\n")
	msg.ToolCalls = toolCalls

	return msg
}

// convertHistoryForFallback converts history for models using text-based tool calling.
// FunctionCall parts in model messages become plain text, and tool results become user messages.
func (c *OllamaClient) convertHistoryForFallback(history []*genai.Content, results []*genai.FunctionResponse) []api.Message {
	messages := make([]api.Message, 0, len(history)+len(results)+1)

	// Prepend system instruction if set
	c.mu.RLock()
	sysInstruction := c.systemInstruction
	c.mu.RUnlock()
	if sysInstruction != "" {
		messages = append(messages, api.Message{Role: "system", Content: sysInstruction})
	}

	// Convert history, converting FunctionCall parts to text
	for _, content := range history {
		msg := api.Message{}

		switch content.Role {
		case genai.RoleUser:
			msg.Role = "user"
		case genai.RoleModel:
			msg.Role = "assistant"
		default:
			msg.Role = string(content.Role)
		}

		var textParts []string
		for _, part := range content.Parts {
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
			// Convert FunctionCall to text representation for fallback models
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				textParts = append(textParts, fmt.Sprintf(
					"```json\n{\"tool\": \"%s\", \"args\": %s}\n```",
					part.FunctionCall.Name, string(argsJSON)))
			}
			// Convert FunctionResponse to text
			if part.FunctionResponse != nil {
				var contentStr string
				if val, ok := part.FunctionResponse.Response["content"].(string); ok {
					contentStr = val
				} else {
					jsonBytes, _ := json.Marshal(part.FunctionResponse.Response)
					contentStr = string(jsonBytes)
				}
				textParts = append(textParts, fmt.Sprintf(
					"Tool result for %s:\n%s", part.FunctionResponse.Name, contentStr))
			}
		}

		msg.Content = strings.Join(textParts, "\n")
		if msg.Content != "" {
			messages = append(messages, msg)
		}
	}

	// Add new tool results as user messages
	for _, result := range results {
		var contentStr string
		if result.Response != nil {
			if val, ok := result.Response["content"].(string); ok {
				contentStr = val
			} else if data, ok := result.Response["data"]; ok {
				if jsonBytes, err := json.Marshal(data); err == nil {
					contentStr = string(jsonBytes)
				}
			}
			if errStr, ok := result.Response["error"].(string); ok && errStr != "" {
				contentStr = "Error: " + errStr
			}
		}
		if contentStr == "" {
			contentStr = "Operation completed"
		}

		messages = append(messages, api.Message{
			Role:    "user",
			Content: fmt.Sprintf("Tool result for %s:\n%s", result.Name, contentStr),
		})
	}

	return messages
}

// convertHistoryWithResults converts history with function results to messages.
func (c *OllamaClient) convertHistoryWithResults(history []*genai.Content, results []*genai.FunctionResponse) []api.Message {
	messages := make([]api.Message, 0, len(history)+len(results))

	// Convert history
	for _, content := range history {
		msg := c.convertContentToMessage(content)
		if msg.Role != "" {
			messages = append(messages, msg)
		}
	}

	// Add tool results as separate messages
	for _, result := range results {
		// Extract content from response
		var contentStr string
		if result.Response != nil {
			if val, ok := result.Response["content"].(string); ok {
				contentStr = val
			} else if data, ok := result.Response["data"]; ok {
				if jsonBytes, err := json.Marshal(data); err == nil {
					contentStr = string(jsonBytes)
				}
			}
			if errStr, ok := result.Response["error"].(string); ok && errStr != "" {
				contentStr = "Error: " + errStr
			}
		}
		if contentStr == "" {
			contentStr = "Operation completed"
		}

		messages = append(messages, api.Message{
			Role:       "tool",
			Content:    contentStr,
			ToolName:   result.Name,
			ToolCallID: result.ID,
		})
	}

	return messages
}

// convertToolsToOllama converts genai.Tool to Ollama api.Tool format.
func (c *OllamaClient) convertToolsToOllama() []api.Tool {
	tools := make([]api.Tool, 0)

	for _, tool := range c.tools {
		for _, decl := range tool.FunctionDeclarations {
			// Build parameters
			params := api.ToolFunctionParameters{
				Type:       "object",
				Properties: api.NewToolPropertiesMap(),
			}

			if decl.Parameters != nil {
				if len(decl.Parameters.Required) > 0 {
					params.Required = decl.Parameters.Required
				}

				for name, propSchema := range decl.Parameters.Properties {
					prop := api.ToolProperty{
						Description: propSchema.Description,
					}
					if propSchema.Type != "" {
						prop.Type = api.PropertyType{strings.ToLower(string(propSchema.Type))}
					}
					if len(propSchema.Enum) > 0 {
						enumVals := make([]any, len(propSchema.Enum))
						for i, v := range propSchema.Enum {
							enumVals[i] = v
						}
						prop.Enum = enumVals
					}
					params.Properties.Set(name, prop)
				}
			}

			ollamaTool := api.Tool{
				Type: "function",
				Function: api.ToolFunction{
					Name:        decl.Name,
					Description: decl.Description,
					Parameters:  params,
				},
			}

			tools = append(tools, ollamaTool)
		}
	}

	return tools
}

// convertOllamaToolCallToGenai converts Ollama tool call to genai.FunctionCall.
func (c *OllamaClient) convertOllamaToolCallToGenai(tc api.ToolCall, index int) *genai.FunctionCall {
	// Use Ollama's ID if available, otherwise generate from index
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("call_%d", index)
		if tc.Function.Index > 0 {
			id = fmt.Sprintf("call_%d", tc.Function.Index)
		}
	}
	return &genai.FunctionCall{
		ID:   id,
		Name: tc.Function.Name,
		Args: tc.Function.Arguments.ToMap(),
	}
}

// convertGenaiToolCallToOllama converts genai.FunctionCall to Ollama api.ToolCall.
func (c *OllamaClient) convertGenaiToolCallToOllama(fc *genai.FunctionCall) api.ToolCall {
	args := api.NewToolCallFunctionArguments()
	for k, v := range fc.Args {
		args.Set(k, v)
	}
	return api.ToolCall{
		ID: fc.ID,
		Function: api.ToolCallFunction{
			Name:      fc.Name,
			Arguments: args,
		},
	}
}

// isRetryableError returns true if the error should trigger a retry.
func (c *OllamaClient) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Connection errors are retryable
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "no such host") {
		return true
	}

	// Check for HTTP status errors
	var statusErr *api.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case 429, 500, 502, 503, 504:
			return true
		}
	}

	return false
}

// IsModelNotFoundError checks if the error indicates a missing model.
func IsModelNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()

	// Check wrapped error message patterns
	if strings.Contains(errStr, "is not installed") ||
		(strings.Contains(errStr, "model") && strings.Contains(errStr, "not found")) {
		return true
	}

	// Check status error
	var statusErr *api.StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
		return true
	}

	return false
}

// wrapOllamaError wraps Ollama errors with user-friendly messages.
func (c *OllamaClient) wrapOllamaError(err error) error {
	if err == nil {
		return nil
	}

	errStr := err.Error()

	// Connection refused - Ollama not running
	if strings.Contains(errStr, "connection refused") {
		return fmt.Errorf(`Ollama server is not running.

To fix this:
  1. Start Ollama: ollama serve
  2. Or check if it's running: ollama list

Original error: %w`, err)
	}

	// Timeout
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") {
		return fmt.Errorf(`Ollama request timed out.

Possible causes:
  • Model is loading into memory (first request is slow)
  • Model is too large for available RAM/VRAM
  • Server is overloaded

Try again or use a smaller model.

Original error: %w`, err)
	}

	// Check for model not found via status error
	var statusErr *api.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode == 404 {
			return fmt.Errorf(`Model '%s' is not installed.

To fix this:
  1. Pull the model: ollama pull %s
  2. Or list available models: ollama list

Original error: %w`, c.config.Model, c.config.Model, err)
		}
	}

	// Generic model not found
	if strings.Contains(errStr, "model") && strings.Contains(errStr, "not found") {
		return fmt.Errorf(`Model '%s' is not installed.

To fix this:
  1. Pull the model: ollama pull %s
  2. Or list available models: ollama list

Original error: %w`, c.config.Model, c.config.Model, err)
	}

	return err
}
