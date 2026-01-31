package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gokin/internal/logging"
	"gokin/internal/security"

	"google.golang.org/genai"
)

// HTTPError represents an HTTP error with status code.
type HTTPError struct {
	StatusCode int
	Message    string
	Err        error
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP error: %d", e.StatusCode)
}

func (e *HTTPError) Unwrap() error {
	return e.Err
}

// AnthropicConfig holds configuration for Anthropic-compatible API (GLM-4.7).
type AnthropicConfig struct {
	APIKey        string
	BaseURL       string // Default: "https://api.anthropic.com" for Anthropic, custom for GLM-4.7
	Model         string
	MaxTokens     int32
	Temperature   float32
	StreamEnabled bool
	// Retry configuration
	MaxRetries  int           // Maximum number of retry attempts
	RetryDelay  time.Duration // Initial delay between retries
	HTTPTimeout time.Duration // HTTP request timeout
	// Extended Thinking
	EnableThinking bool  // Enable extended thinking mode
	ThinkingBudget int32 // Max tokens for thinking (0 = disabled)
}

// AnthropicClient implements Client interface for Anthropic-compatible APIs (including GLM-4.7).
type AnthropicClient struct {
	config      AnthropicConfig
	httpClient  *http.Client
	tools       []*genai.Tool
	rateLimiter RateLimiter
	mu          sync.RWMutex
}

// RateLimiter interface for rate limiting (optional).
type RateLimiter interface {
	AcquireWithContext(ctx context.Context, tokens int64) error
	ReturnTokens(requests int, tokens int64)
}

// NewAnthropicClient creates a new Anthropic-compatible client.
func NewAnthropicClient(config AnthropicConfig) (*AnthropicClient, error) {
	// Validate required fields
	if config.APIKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	// Validate BaseURL if provided
	if config.BaseURL != "" {
		if !strings.HasPrefix(config.BaseURL, "http://") && !strings.HasPrefix(config.BaseURL, "https://") {
			return nil, fmt.Errorf("invalid BaseURL: must start with http:// or https://")
		}
	}

	// Validate model name
	if config.Model == "" {
		return nil, fmt.Errorf("model name is required")
	}

	// Set defaults
	if config.BaseURL == "" {
		config.BaseURL = "https://api.anthropic.com"
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = 4096
	}
	if config.MaxTokens < 1 {
		return nil, fmt.Errorf("MaxTokens must be positive, got: %d", config.MaxTokens)
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3 // Default to 3 retries
	}
	if config.MaxRetries < 0 {
		return nil, fmt.Errorf("MaxRetries cannot be negative, got: %d", config.MaxRetries)
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = 1 * time.Second // Default 1 second delay
	}
	if config.HTTPTimeout == 0 {
		config.HTTPTimeout = 120 * time.Second // Default 2 minute timeout
	}
	if config.HTTPTimeout < time.Second {
		return nil, fmt.Errorf("HTTPTimeout too short: %v (minimum: 1s)", config.HTTPTimeout)
	}

		// Create secure HTTP client with TLS 1.2+ enforcement
		tlsConfig := security.DefaultTLSConfig()
		httpClient, err := security.CreateSecureHTTPClient(tlsConfig, config.HTTPTimeout)
		if err != nil {
			return nil, fmt.Errorf("failed to create secure HTTP client: %w", err)
		}

		return &AnthropicClient{
			config:     config,
			httpClient: httpClient,
			tools:      make([]*genai.Tool, 0),
		}, nil
}

// SendMessage sends a message and returns a streaming response.
func (c *AnthropicClient) SendMessage(ctx context.Context, message string) (*StreamingResponse, error) {
	return c.SendMessageWithHistory(ctx, nil, message)
}

// SendMessageWithHistory sends a message with conversation history.
func (c *AnthropicClient) SendMessageWithHistory(ctx context.Context, history []*genai.Content, message string) (*StreamingResponse, error) {
	// Convert Gemini format to Anthropic format
	messages := c.convertHistoryToMessages(history, message)

	// Build request
	requestBody := map[string]interface{}{
		"model":      c.config.Model,
		"max_tokens": c.config.MaxTokens,
		"messages":   messages,
		"stream":     true,
	}

	// Extended Thinking support
	if c.config.EnableThinking && c.config.ThinkingBudget > 0 {
		requestBody["thinking"] = map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": c.config.ThinkingBudget,
		}
		// Extended thinking requires temperature=1 (Anthropic requirement)
		requestBody["temperature"] = 1.0
	} else if c.config.Temperature > 0 {
		requestBody["temperature"] = c.config.Temperature
	}

	if len(c.tools) > 0 {
		// Convert tools to Anthropic format
		tools := c.convertToolsToAnthropic()
		if len(tools) > 0 {
			requestBody["tools"] = tools
		}
	}

	return c.streamRequest(ctx, requestBody)
}

// SendFunctionResponse sends function call results back to the model.
func (c *AnthropicClient) SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error) {
	// Append function results to history and continue
	messages := c.convertHistoryWithResults(history, results)

	requestBody := map[string]interface{}{
		"model":      c.config.Model,
		"max_tokens": c.config.MaxTokens,
		"messages":   messages,
		"stream":     true,
	}

	if len(c.tools) > 0 {
		requestBody["tools"] = c.convertToolsToAnthropic()
	}

	return c.streamRequest(ctx, requestBody)
}

// SetTools sets the tools available for function calling.
func (c *AnthropicClient) SetTools(tools []*genai.Tool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools = tools
}

// SetRateLimiter sets the rate limiter for API calls.
func (c *AnthropicClient) SetRateLimiter(limiter interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rl, ok := limiter.(RateLimiter); ok {
		c.rateLimiter = rl
	}
}

// CountTokens counts tokens for the given contents.
func (c *AnthropicClient) CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error) {
	// Anthropic doesn't have a dedicated count endpoint.
	// Estimate by counting characters across all part types (text, function calls, function responses).
	totalChars := 0
	for _, content := range contents {
		totalChars += 4 * 4 // role overhead (~4 tokens)
		for _, part := range content.Parts {
			if part.Text != "" {
				totalChars += len(part.Text)
			}
			if part.FunctionCall != nil {
				totalChars += len(part.FunctionCall.Name) + 40 // name + structural overhead
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

	// Adjust multiplier based on model type
	// GLM models (GLM-4, GLM-4.7) are more efficient than standard tokenizers
	multiplier := 4.0 // Default for most models
	if strings.HasPrefix(strings.ToLower(c.config.Model), "glm") {
		multiplier = 3.5 // GLM tokenizes more efficiently (more chars per token)
	}

	estimatedTokens := int32(float64(totalChars) / multiplier)
	return &genai.CountTokensResponse{
		TotalTokens: estimatedTokens,
	}, nil
}

// GetModel returns the model name.
func (c *AnthropicClient) GetModel() string {
	return c.config.Model
}

// SetModel changes the model for this client.
func (c *AnthropicClient) SetModel(modelName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.config.Model = modelName
}

// WithModel returns a new client configured for the specified model.
func (c *AnthropicClient) WithModel(modelName string) Client {
	newConfig := c.config
	newConfig.Model = modelName
	newClient, err := NewAnthropicClient(newConfig)
	if err != nil {
		logging.Error("failed to create client with new model", "model", modelName, "error", err)
		return c // Return original client on error
	}
	newClient.SetTools(c.tools)
	return newClient
}

// Close closes the client connection and releases resources.
func (c *AnthropicClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close idle connections to release resources
	if c.httpClient != nil {
		if transport, ok := c.httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	return nil
}

// GetRawClient returns the underlying HTTP client for direct API access.
func (c *AnthropicClient) GetRawClient() interface{} {
	return c.httpClient
}

// toolCallAccumulator tracks tool call and thinking state during streaming.
type toolCallAccumulator struct {
	currentToolID    string
	currentToolName  string
	currentToolInput strings.Builder
	completedCalls   []*genai.FunctionCall
	// Thinking block tracking
	currentBlockType string          // "thinking", "text", or "tool_use"
	thinkingBuilder  strings.Builder // Accumulates thinking content
}

// isRetryableError returns true if the error should trigger a retry.
func (c *AnthropicClient) isRetryableError(err error, statusCode int) bool {
	// HTTP status codes that are retryable (5xx server errors and 429 rate limit)
	switch statusCode {
	case 429, 500, 502, 503, 504:
		return true
	}

	// Only retry on specific network-related errors, not all errors
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "timeout") ||
			strings.Contains(errStr, "connection refused") ||
			strings.Contains(errStr, "no such host") ||
			strings.Contains(errStr, "connection reset") ||
			strings.Contains(errStr, "EOF") {
			return true
		}
	}
	return false
}

// streamRequest performs a streaming request to the Anthropic API with retry logic.
func (c *AnthropicClient) streamRequest(ctx context.Context, requestBody map[string]interface{}) (*StreamingResponse, error) {
	var lastErr error
	var lastStatusCode int

	// Retry loop
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			delay := c.config.RetryDelay * time.Duration(1<<uint(attempt-1))
			logging.Info("retrying request", "attempt", attempt, "delay", delay, "last_status", lastStatusCode)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		response, err := c.doStreamRequest(ctx, requestBody)
		if err == nil {
			return response, nil
		}

		// Store error for potential retry
		lastErr = err

		// Extract status code if available
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			lastStatusCode = httpErr.StatusCode
		}

		// Check if error is retryable
		if !c.isRetryableError(err, lastStatusCode) {
			return nil, err
		}

		logging.Warn("request failed, will retry", "attempt", attempt, "error", err, "status", lastStatusCode)
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", c.config.MaxRetries, lastErr)
}

// doStreamRequest performs a single streaming request attempt.
func (c *AnthropicClient) doStreamRequest(ctx context.Context, requestBody map[string]interface{}) (*StreamingResponse, error) {
	// Marshal request body
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Log request body for debugging (truncated for large messages)
	reqStr := string(jsonData)
	if len(reqStr) > 2000 {
		logging.Debug("API request body (truncated)", "body", reqStr[:2000]+"...")
	} else {
		logging.Debug("API request body", "body", reqStr)
	}

	// Create HTTP request
	// Handle different URL patterns for different providers
	var url string
	if c.config.BaseURL == "https://api.anthropic.com" || c.config.BaseURL == "" {
		// Standard Anthropic API
		url = "https://api.anthropic.com/v1/messages"
	} else if strings.Contains(c.config.BaseURL, "api.z.ai") {
		// GLM-4.7 / Z.AI API - use Anthropic-compatible endpoint
		if strings.HasSuffix(c.config.BaseURL, "/anthropic") {
			// BaseURL is https://api.z.ai/api/anthropic - add /v1/messages for Anthropic format
			url = c.config.BaseURL + "/v1/messages"
		} else {
			// Add full anthropic path
			url = strings.TrimSuffix(c.config.BaseURL, "/") + "/anthropic/v1/messages"
		}
	} else {
		// Other custom endpoints - assume Anthropic-compatible
		url = strings.TrimSuffix(c.config.BaseURL, "/") + "/v1/messages"
	}

	logging.Info("anthropic API request", "url", url, "model", c.config.Model)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.config.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	// Z.AI also accepts Authorization header
	if strings.Contains(c.config.BaseURL, "api.z.ai") {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	// Perform request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			logging.Error("failed to read error response", "error", err)
			body = []byte("(failed to read response body)")
		}
		resp.Body.Close()
		logging.Warn("anthropic API error", "status", resp.StatusCode, "body", string(body))
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("API error (status %d): %s", resp.StatusCode, string(body)),
		}
	}

	// Create streaming response
	chunks := make(chan ResponseChunk, 10)
	done := make(chan struct{})

	// Process stream in goroutine
	go func() {
		defer close(chunks)
		defer close(done)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		accumulator := &toolCallAccumulator{
			completedCalls: make([]*genai.FunctionCall, 0),
		}

		eventCount := 0
	scanLoop:
		for {
			// Check for context cancellation BEFORE scanning next line
			select {
			case <-ctx.Done():
				logging.Debug("context cancelled, stopping stream processing")
				chunks <- ResponseChunk{Error: ctx.Err(), Done: true}
				return
			default:
			}

			// Scan with timeout check
			if !scanner.Scan() {
				break scanLoop
			}
			line := scanner.Text()

			// Log raw SSE lines for debugging
			if line != "" {
				logging.Debug("SSE raw line", "line", line)
			}

			// SSE format: "data: {...}" or "data:{...}" (handle both)
			var data string
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			} else if strings.HasPrefix(line, "data:") {
				data = strings.TrimPrefix(line, "data:")
			} else {
				continue
			}
			eventCount++
			// Log FULL data for error events to see the complete error message
			if strings.Contains(data, "error") {
				logging.Error("SSE ERROR event received", "full_data", data)
			} else {
				logging.Debug("SSE data event", "count", eventCount, "data_preview", truncateString(data, 200))
			}

			// Skip "[DONE]" marker
			if data == "[DONE]" {
				// Send any accumulated tool calls before marking done
				if len(accumulator.completedCalls) > 0 {
					chunks <- ResponseChunk{
						FunctionCalls: accumulator.completedCalls,
						Done:          true,
					}
				} else {
					select {
					case chunks <- ResponseChunk{Done: true}:
					case <-ctx.Done():
						return
					}
				}
				return
			}

			// Parse JSON
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				// Log invalid JSON for debugging
				preview := data
				if len(preview) > 100 {
					preview = preview[:100] + "..."
				}
				logging.Warn("failed to parse SSE event", "error", err, "data", preview)
				continue
			}

			// Handle Z.AI/GLM error format: {"error":{"code":"...", "message":"..."}}
			if errObj, ok := event["error"].(map[string]interface{}); ok {
				errCode, _ := errObj["code"].(string)
				errMsg, _ := errObj["message"].(string)
				logging.Error("Z.AI API error", "code", errCode, "message", errMsg)
				chunks <- ResponseChunk{
					Error: fmt.Errorf("API error (%s): %s", errCode, errMsg),
					Done:  true,
				}
				return
			}

			// Process event
			chunk := c.processStreamEvent(event, accumulator)
			if chunk.Text != "" || chunk.Done || len(chunk.FunctionCalls) > 0 {
				// Check context before sending chunk
				select {
				case chunks <- chunk:
				case <-ctx.Done():
					logging.Debug("context cancelled during chunk send")
					return
				}
			}

			if chunk.Done {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			logging.Warn("SSE scanner error", "error", err)
			select {
			case chunks <- ResponseChunk{Error: err, Done: true}:
			case <-ctx.Done():
				logging.Debug("context cancelled, dropping scanner error", "error", err)
			}
		}
	}()

	return &StreamingResponse{
		Chunks: chunks,
		Done:   done,
	}, nil
}

// processStreamEvent converts an Anthropic stream event to a ResponseChunk.
func (c *AnthropicClient) processStreamEvent(event map[string]interface{}, acc *toolCallAccumulator) ResponseChunk {
	chunk := ResponseChunk{}

	eventType, ok := event["type"].(string)
	if !ok {
		logging.Debug("SSE event missing type", "event", event)
		return chunk
	}

	// Debug: log all events for diagnostics
	logging.Debug("SSE event received", "type", eventType)

	switch eventType {
	case "content_block_start":
		// Check if this is a tool_use, thinking, or text content block
		if contentBlock, ok := event["content_block"].(map[string]interface{}); ok {
			blockType, _ := contentBlock["type"].(string)
			logging.Debug("content_block_start", "type", blockType)

			// Track current block type for delta processing
			acc.currentBlockType = blockType

			if blockType == "tool_use" {
				// Extract tool ID and name
				if id, ok := contentBlock["id"].(string); ok {
					acc.currentToolID = id
					logging.Debug("captured tool_use ID", "id", id)
				} else {
					logging.Warn("tool_use block missing ID")
				}
				if name, ok := contentBlock["name"].(string); ok {
					acc.currentToolName = name
				}
				acc.currentToolInput.Reset()
			} else if blockType == "thinking" {
				// Start of thinking block - reset accumulator
				acc.thinkingBuilder.Reset()
				logging.Debug("thinking block started")
			}
		}

	case "error":
		// Handle API error events
		if errData, ok := event["error"].(map[string]interface{}); ok {
			errType, _ := errData["type"].(string)
			errMsg, _ := errData["message"].(string)
			logging.Error("API error event", "type", errType, "message", errMsg)
			chunk.Error = fmt.Errorf("API error: %s - %s", errType, errMsg)
			chunk.Done = true
		}

	case "content_block_delta":
		logging.Debug("SSE content_block_delta event", "event", event)
		if delta, ok := event["delta"].(map[string]interface{}); ok {
			deltaType, _ := delta["type"].(string)
			logging.Debug("SSE delta content", "delta", delta, "type", deltaType)

			// Handle thinking delta (extended thinking mode)
			if deltaType == "thinking_delta" {
				if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
					acc.thinkingBuilder.WriteString(thinking)
					chunk.Thinking = thinking
					logging.Debug("SSE thinking delta received", "thinking_length", len(thinking))
				}
			}

			// Handle text delta (check both with and without type for compatibility)
			if deltaType == "text_delta" || (deltaType == "" && acc.currentBlockType == "text") {
				if text, ok := delta["text"].(string); ok && text != "" {
					chunk.Text = text
					logging.Debug("SSE text delta received", "text_length", len(text))
				}
			}

			// Handle tool input JSON delta
			if deltaType == "input_json_delta" {
				if partialJSON, ok := delta["partial_json"].(string); ok {
					acc.currentToolInput.WriteString(partialJSON)
				}
			}
		} else {
			logging.Debug("SSE content_block_delta missing delta", "event", event)
		}

	case "content_block_stop":
		// If we were accumulating a tool call, finalize it
		if acc.currentToolID != "" && acc.currentToolName != "" {
			inputJSON := acc.currentToolInput.String()
			var args map[string]interface{}
			if inputJSON != "" {
				if err := json.Unmarshal([]byte(inputJSON), &args); err != nil {
					logging.Error("tool args JSON unmarshal failed",
						"error", err,
						"tool", acc.currentToolName,
						"json", inputJSON)
					args = make(map[string]interface{})
				}
			} else {
				logging.Warn("tool call received with empty input JSON",
					"tool", acc.currentToolName,
					"tool_id", acc.currentToolID)
				args = make(map[string]interface{})
			}

			functionCall := &genai.FunctionCall{
				ID:   acc.currentToolID,
				Name: acc.currentToolName,
				Args: args,
			}
			acc.completedCalls = append(acc.completedCalls, functionCall)

			// Reset accumulator state
			acc.currentToolID = ""
			acc.currentToolName = ""
			acc.currentToolInput.Reset()
		}

		// Reset block type
		acc.currentBlockType = ""

	case "message_start":
		// Message starting - no action needed

	case "message_delta":
		// Message metadata (usage, stop_reason, etc.)
		if delta, ok := event["delta"].(map[string]interface{}); ok {
			if stopReason, ok := delta["stop_reason"].(string); ok {
				chunk.Done = true
				switch stopReason {
				case "end_turn":
					chunk.FinishReason = genai.FinishReasonStop
				case "max_tokens":
					chunk.FinishReason = genai.FinishReasonMaxTokens
				case "tool_use":
					// Include accumulated tool calls in the final chunk
					if len(acc.completedCalls) > 0 {
						chunk.FunctionCalls = acc.completedCalls
					}
					chunk.FinishReason = genai.FinishReasonStop
				}
			}
		}

	case "message_stop":
		chunk.Done = true
		// Include any accumulated tool calls
		if len(acc.completedCalls) > 0 {
			chunk.FunctionCalls = acc.completedCalls
		}
	}

	return chunk
}

// convertHistoryToMessages converts Gemini history to Anthropic messages format.
func (c *AnthropicClient) convertHistoryToMessages(history []*genai.Content, newMessage string) []map[string]interface{} {
	messages := make([]map[string]interface{}, 0)

	for _, content := range history {
		if content.Role == genai.RoleUser {
			messages = append(messages, c.buildUserMessage(content.Parts))
		} else if content.Role == genai.RoleModel {
			messages = append(messages, c.buildAssistantMessage(content.Parts))
		}
	}

	// Add new user message (ensure non-empty content)
	if newMessage == "" {
		newMessage = "Continue."
	}
	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": newMessage,
	})

	return messages
}

// convertHistoryWithResults converts history with function results to messages.
func (c *AnthropicClient) convertHistoryWithResults(history []*genai.Content, results []*genai.FunctionResponse) []map[string]interface{} {
	messages := make([]map[string]interface{}, 0)

	logging.Debug("convertHistoryWithResults", "history_len", len(history), "results_len", len(results))

	// Convert history
	for i, content := range history {
		// Detailed logging to debug ID mismatch
		var partDetails []string
		for j, part := range content.Parts {
			if part.FunctionCall != nil {
				partDetails = append(partDetails, fmt.Sprintf("part[%d]:FunctionCall(id=%s,name=%s)", j, part.FunctionCall.ID, part.FunctionCall.Name))
			} else if part.FunctionResponse != nil {
				partDetails = append(partDetails, fmt.Sprintf("part[%d]:FunctionResponse(id=%s,name=%s)", j, part.FunctionResponse.ID, part.FunctionResponse.Name))
			} else if part.Text != "" {
				partDetails = append(partDetails, fmt.Sprintf("part[%d]:Text(len=%d)", j, len(part.Text)))
			}
		}
		logging.Debug("converting history item", "index", i, "role", content.Role, "parts", partDetails)

		if content.Role == genai.RoleUser {
			messages = append(messages, c.buildUserMessage(content.Parts))
		} else if content.Role == genai.RoleModel {
			messages = append(messages, c.buildAssistantMessage(content.Parts))
		}
	}

	// Add function result as user message
	resultContents := make([]map[string]interface{}, 0)
	for _, result := range results {
		// Use result.ID for tool_use_id (required by Anthropic API)
		// Fall back to Name if ID is not set (for backwards compatibility)
		toolUseID := result.ID
		if toolUseID == "" {
			logging.Warn("tool result missing ID, using Name as fallback", "name", result.Name)
			toolUseID = result.Name
		}
		logging.Debug("adding tool result", "tool_use_id", toolUseID, "name", result.Name)

		// Extract content string from the response map
		// Anthropic expects content as a string, not a map
		var contentStr string
		resp := result.Response // result.Response is map[string]any
		if resp != nil {
			if content, ok := resp["content"].(string); ok {
				contentStr = content
			} else if data, ok := resp["data"]; ok {
				// If there's structured data, convert to JSON string
				if jsonBytes, err := json.Marshal(data); err == nil {
					contentStr = string(jsonBytes)
				}
			}
			// If there was an error, include it
			if errStr, ok := resp["error"].(string); ok && errStr != "" {
				contentStr = "Error: " + errStr
			}
		}

		if contentStr == "" {
			contentStr = "Operation completed"
		}

		resultContents = append(resultContents, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": toolUseID,
			"id":          toolUseID, // Z.AI compatibility: some backends expect 'id' field
			"content":     contentStr,
		})
	}

	messages = append(messages, map[string]interface{}{
		"role":    "user",
		"content": resultContents,
	})

	return messages
}

// buildUserMessage builds a user message from parts.
func (c *AnthropicClient) buildUserMessage(parts []*genai.Part) map[string]interface{} {
	content := make([]map[string]interface{}, 0)

	for _, part := range parts {
		if part.Text != "" {
			content = append(content, map[string]interface{}{
				"type": "text",
				"text": part.Text,
			})
		}
		// Handle FunctionResponse parts (tool_result)
		if part.FunctionResponse != nil {
			toolUseID := part.FunctionResponse.ID
			if toolUseID == "" {
				toolUseID = part.FunctionResponse.Name
				logging.Warn("FunctionResponse missing ID in buildUserMessage", "name", part.FunctionResponse.Name)
			}

			// Extract content string from the response map
			var contentStr string
			resp := part.FunctionResponse.Response
			if resp != nil {
				if c, ok := resp["content"].(string); ok {
					contentStr = c
				} else if data, ok := resp["data"]; ok {
					if jsonBytes, err := json.Marshal(data); err == nil {
						contentStr = string(jsonBytes)
					}
				}
				if errStr, ok := resp["error"].(string); ok && errStr != "" {
					contentStr = "Error: " + errStr
				}
			}
			if contentStr == "" {
				contentStr = "Operation completed"
			}

			logging.Debug("buildUserMessage tool_result", "tool_use_id", toolUseID, "name", part.FunctionResponse.Name)
			content = append(content, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"id":          toolUseID, // Z.AI compatibility: some backends expect 'id' field
				"content":     contentStr,
			})
		}
	}

	// Anthropic API requires non-empty content array
	if len(content) == 0 {
		logging.Warn("buildUserMessage: empty content, adding placeholder", "parts_count", len(parts))
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": "Continue.",
		})
	}

	return map[string]interface{}{
		"role":    "user",
		"content": content,
	}
}

// buildAssistantMessage builds an assistant message from parts.
func (c *AnthropicClient) buildAssistantMessage(parts []*genai.Part) map[string]interface{} {
	content := make([]map[string]interface{}, 0)

	for _, part := range parts {
		if part.Text != "" {
			content = append(content, map[string]interface{}{
				"type": "text",
				"text": part.Text,
			})
		}
		if part.FunctionCall != nil {
			// Convert function call to Anthropic format
			// Use original ID from model response - this MUST match tool_use_id in tool_result
			toolID := part.FunctionCall.ID
			if toolID == "" {
				// Fallback only if ID is missing (shouldn't happen normally)
				logging.Warn("FunctionCall missing ID in buildAssistantMessage, generating new one", "name", part.FunctionCall.Name)
				toolID = part.FunctionCall.Name + "_" + randomID()
			}
			logging.Debug("buildAssistantMessage tool_use", "id", toolID, "name", part.FunctionCall.Name)
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    toolID,
				"name":  part.FunctionCall.Name,
				"input": part.FunctionCall.Args,
			})
		}
	}

	return map[string]interface{}{
		"role":    "assistant",
		"content": content,
	}
}

// convertSchemaToJSON converts a genai.Schema to Anthropic-compatible JSON Schema format.
// genai uses uppercase types ("STRING", "OBJECT") but Anthropic expects lowercase ("string", "object").
func convertSchemaToJSON(schema *genai.Schema) map[string]interface{} {
	if schema == nil {
		return nil
	}

	result := make(map[string]interface{})

	// Convert type to lowercase (genai uses "STRING", Anthropic expects "string")
	if schema.Type != "" {
		result["type"] = strings.ToLower(string(schema.Type))
	}

	if schema.Description != "" {
		result["description"] = schema.Description
	}

	if len(schema.Enum) > 0 {
		result["enum"] = schema.Enum
	}

	// Convert nested properties recursively
	if len(schema.Properties) > 0 {
		props := make(map[string]interface{})
		for name, propSchema := range schema.Properties {
			props[name] = convertSchemaToJSON(propSchema)
		}
		result["properties"] = props
	}

	if len(schema.Required) > 0 {
		result["required"] = schema.Required
	}

	// Convert array items schema
	if schema.Items != nil {
		result["items"] = convertSchemaToJSON(schema.Items)
	}

	return result
}

// convertToolsToAnthropic converts Gemini tools to Anthropic format.
func (c *AnthropicClient) convertToolsToAnthropic() []map[string]interface{} {
	tools := make([]map[string]interface{}, 0)

	for _, tool := range c.tools {
		for _, decl := range tool.FunctionDeclarations {
			// Convert parameters schema properly using recursive conversion
			inputSchema := convertSchemaToJSON(decl.Parameters)

			anthropicTool := map[string]interface{}{
				"name":         decl.Name,
				"description":  decl.Description,
				"input_schema": inputSchema,
			}
			tools = append(tools, anthropicTool)
		}
	}

	return tools
}

// randomID generates a unique ID for tool_use.
func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("toolu_%d", time.Now().UnixNano())
	}
	return "toolu_" + hex.EncodeToString(b)
}

// truncateString truncates a string to maxLen characters with ellipsis.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
