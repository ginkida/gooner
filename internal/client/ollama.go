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
	client      *api.Client
	config      OllamaConfig
	tools       []*genai.Tool
	rateLimiter RateLimiter
	mu          sync.RWMutex
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
	// Convert Gemini format to Ollama format
	messages := c.convertHistoryToMessages(history, message)

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

	// Convert and add tools if available
	c.mu.RLock()
	if len(c.tools) > 0 {
		req.Tools = c.convertToolsToOllama()
	}
	c.mu.RUnlock()

	return c.streamChat(ctx, req)
}

// SendFunctionResponse sends function call results back to the model.
func (c *OllamaClient) SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error) {
	// Convert history with function results
	messages := c.convertHistoryWithResults(history, results)

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

	c.mu.RLock()
	if len(c.tools) > 0 {
		req.Tools = c.convertToolsToOllama()
	}
	c.mu.RUnlock()

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

// convertHistoryToMessages converts Gemini history to Ollama messages format.
func (c *OllamaClient) convertHistoryToMessages(history []*genai.Content, newMessage string) []api.Message {
	messages := make([]api.Message, 0, len(history)+1)

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

// wrapOllamaError wraps Ollama errors with user-friendly messages.
func (c *OllamaClient) wrapOllamaError(err error) error {
	if err == nil {
		return nil
	}

	errStr := err.Error()

	// Connection refused - Ollama not running
	if strings.Contains(errStr, "connection refused") {
		return fmt.Errorf("Ollama server not running. Start it with: ollama serve\nOriginal error: %w", err)
	}

	// Check for model not found
	var statusErr *api.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode == 404 {
			return fmt.Errorf("model '%s' not found. Download it with: ollama pull %s\nOriginal error: %w",
				c.config.Model, c.config.Model, err)
		}
	}

	// Model pull required
	if strings.Contains(errStr, "model") && strings.Contains(errStr, "not found") {
		return fmt.Errorf("model '%s' not found. Download it with: ollama pull %s\nOriginal error: %w",
			c.config.Model, c.config.Model, err)
	}

	return err
}
