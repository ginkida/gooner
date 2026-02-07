package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"gokin/internal/config"
	"gokin/internal/logging"
	"gokin/internal/ratelimit"
	"gokin/internal/security"

	"google.golang.org/genai"
)

// GeminiClient wraps the Google Gemini API.
type GeminiClient struct {
	client            *genai.Client
	model             string
	config            *genai.GenerateContentConfig
	tools             []*genai.Tool
	rateLimiter       *ratelimit.Limiter
	maxRetries        int            // Maximum number of retry attempts (default: 3)
	retryDelay        time.Duration  // Initial delay between retries (default: 1s)
	statusCallback    StatusCallback // Optional callback for status updates
	systemInstruction string         // System-level instruction passed via API parameter
	thinkingBudget    int32          // Thinking budget (0 = disabled)
}

// NewGeminiClient creates a new Gemini API client (returns Client interface).
func NewGeminiClient(ctx context.Context, cfg *config.Config) (Client, error) {
	// Load API key from environment or config (try GeminiKey first, then legacy APIKey)
	loadedKey := security.GetGeminiKey(cfg.API.GeminiKey, cfg.API.APIKey)

	if !loadedKey.IsSet() {
		return nil, fmt.Errorf("Gemini API key required.\n\nGet your free API key at: https://aistudio.google.com/apikey\n\nThen set it with: /login gemini <your-api-key>")
	}

	// Log key source for debugging (without exposing the key)
	logging.Debug("loaded Gemini API key",
		"source", loadedKey.Source,
		"model", cfg.Model.Name)

	// Validate key format
	if err := security.ValidateKeyFormat(loadedKey.Value); err != nil {
		return nil, fmt.Errorf("invalid Gemini API key: %w", err)
	}

	clientConfig := &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  loadedKey.Value,
	}

	client, err := genai.NewClient(ctx, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	genConfig := &genai.GenerateContentConfig{
		Temperature:     Ptr(cfg.Model.Temperature),
		MaxOutputTokens: cfg.Model.MaxOutputTokens,
	}

	// Use retry config from config, with defaults
	maxRetries := cfg.API.Retry.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3 // Default: 3 retries
	}
	retryDelay := cfg.API.Retry.RetryDelay
	if retryDelay == 0 {
		retryDelay = 1 * time.Second // Default: 1 second initial delay
	}

	return &GeminiClient{
		client:     client,
		model:      cfg.Model.Name,
		config:     genConfig,
		maxRetries: maxRetries,
		retryDelay: retryDelay,
	}, nil
}

// SetSystemInstruction sets the system-level instruction for the model.
func (c *GeminiClient) SetSystemInstruction(instruction string) {
	c.systemInstruction = instruction
}

// SetThinkingBudget configures the thinking/reasoning budget.
func (c *GeminiClient) SetThinkingBudget(budget int32) {
	c.thinkingBudget = budget
}

// SetTools sets the tools available for function calling.
func (c *GeminiClient) SetTools(tools []*genai.Tool) {
	c.tools = tools
}

// SetRateLimiter sets the rate limiter for API calls.
func (c *GeminiClient) SetRateLimiter(limiter interface{}) {
	if rl, ok := limiter.(*ratelimit.Limiter); ok {
		c.rateLimiter = rl
	}
}

// SetStatusCallback sets the callback for status updates during operations.
func (c *GeminiClient) SetStatusCallback(cb StatusCallback) {
	c.statusCallback = cb
}

// SendMessage sends a user message and returns a streaming response.
func (c *GeminiClient) SendMessage(ctx context.Context, message string) (*StreamingResponse, error) {
	return c.SendMessageWithHistory(ctx, nil, message)
}

// SendMessageWithHistory sends a message with conversation history.
func (c *GeminiClient) SendMessageWithHistory(ctx context.Context, history []*genai.Content, message string) (*StreamingResponse, error) {
	contents := make([]*genai.Content, len(history)+1)
	copy(contents, history)
	contents[len(contents)-1] = genai.NewContentFromText(message, genai.RoleUser)

	return c.generateContentStream(ctx, contents)
}

// SendFunctionResponse sends function call results back to the model.
func (c *GeminiClient) SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error) {
	// Create function response content
	var parts []*genai.Part
	for _, result := range results {
		part := genai.NewPartFromFunctionResponse(result.Name, result.Response)
		part.FunctionResponse.ID = result.ID
		parts = append(parts, part)
	}

	// Ensure we have at least one part
	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(" "))
	}

	funcContent := &genai.Content{
		Role:  genai.RoleUser,
		Parts: parts,
	}

	contents := make([]*genai.Content, len(history)+1)
	copy(contents, history)
	contents[len(contents)-1] = funcContent

	return c.generateContentStream(ctx, contents)
}

// sanitizeContents validates and fixes all Contents before sending to API.
// This ensures that each Part has exactly one of: Text, FunctionCall, or FunctionResponse.
func sanitizeContents(contents []*genai.Content) []*genai.Content {
	var result []*genai.Content

	for _, content := range contents {
		if content == nil {
			continue
		}

		var validParts []*genai.Part
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			// Part is valid if it has FunctionCall, FunctionResponse, non-empty Text, or InlineData (images)
			if part.FunctionCall != nil || part.FunctionResponse != nil || part.Text != "" || part.InlineData != nil {
				validParts = append(validParts, part)
			}
		}

		// Content must have at least one part
		if len(validParts) == 0 {
			validParts = []*genai.Part{genai.NewPartFromText(" ")}
		}

		result = append(result, &genai.Content{
			Role:  content.Role,
			Parts: validParts,
		})
	}

	// Must have at least one content
	if len(result) == 0 {
		result = []*genai.Content{{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{genai.NewPartFromText(" ")},
		}}
	}

	return result
}

// isRetryableError returns true if the error should trigger a retry.
func (c *GeminiClient) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Check for retryable HTTP status codes in error message
	// 429 = rate limit, 500/502/503/504 = server errors
	retryableCodes := []string{"429", "500", "502", "503", "504"}
	for _, code := range retryableCodes {
		if strings.Contains(errStr, code) {
			return true
		}
	}

	// Check for network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	// Check for common network error patterns
	networkPatterns := []string{
		"connection refused",
		"connection reset",
		"no such host",
		"timeout",
		"temporary failure",
		"UNAVAILABLE",
		"RESOURCE_EXHAUSTED",
	}
	for _, pattern := range networkPatterns {
		if strings.Contains(strings.ToLower(errStr), strings.ToLower(pattern)) {
			return true
		}
	}

	return false
}

// generateContentStream handles the streaming content generation with retry logic.
func (c *GeminiClient) generateContentStream(ctx context.Context, contents []*genai.Content) (*StreamingResponse, error) {
	// Sanitize contents before sending to API
	contents = sanitizeContents(contents)

	var lastErr error

	// Retry loop
	maxDelay := 30 * time.Second
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			delay := CalculateBackoff(c.retryDelay, attempt-1, maxDelay)
			logging.Info("retrying Gemini request", "attempt", attempt, "delay", delay)

			// Notify UI about retry
			if c.statusCallback != nil {
				reason := "API error"
				if lastErr != nil {
					reason = lastErr.Error()
					// Shorten common error patterns
					if strings.Contains(reason, "429") {
						reason = "rate limit"
					} else if strings.Contains(reason, "connection") {
						reason = "connection error"
					} else if strings.Contains(reason, "timeout") {
						reason = "timeout"
					} else if len(reason) > 50 {
						reason = reason[:47] + "..."
					}
				}
				c.statusCallback.OnRetry(attempt, c.maxRetries, delay, reason)
			}

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		response, err := c.doGenerateContentStream(ctx, contents)
		if err == nil {
			return response, nil
		}

		lastErr = err

		// Check if error is retryable
		if !c.isRetryableError(err) {
			return nil, err
		}

		logging.Warn("Gemini request failed, will retry", "attempt", attempt, "error", err)
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", c.maxRetries, lastErr)
}

// resetTimer safely resets a timer to a new duration.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// doGenerateContentStream performs a single streaming request attempt.
func (c *GeminiClient) doGenerateContentStream(ctx context.Context, contents []*genai.Content) (*StreamingResponse, error) {
	// Track tokens for potential return on error
	var estimatedTokens int64
	if c.rateLimiter != nil {
		estimatedTokens = ratelimit.EstimateTokensFromContents(len(contents), 500)
		if err := c.rateLimiter.AcquireWithContext(ctx, estimatedTokens); err != nil {
			// Notify about rate limit
			if c.statusCallback != nil {
				c.statusCallback.OnRateLimit(5 * time.Second) // Estimate wait time
			}
			return nil, fmt.Errorf("rate limit: %w", err)
		}
	}

	config := *c.config
	if c.systemInstruction != "" {
		config.SystemInstruction = genai.NewContentFromText(c.systemInstruction, genai.RoleUser)
	}
	if c.thinkingBudget > 0 {
		config.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingBudget:  Ptr(c.thinkingBudget),
		}
	}
	if len(c.tools) > 0 {
		config.Tools = c.tools
	}

	iter := c.client.Models.GenerateContentStream(ctx, c.model, contents, &config)

	chunks := make(chan ResponseChunk, 10)
	done := make(chan struct{})

	// Stream idle timeout and warning
	const streamIdleTimeout = 30 * time.Second
	const streamIdleWarning = 15 * time.Second

	// Capture rate limiter and estimated tokens for the goroutine
	rateLimiter := c.rateLimiter
	estimatedForGoroutine := estimatedTokens
	// Capture status callback for goroutine
	statusCb := c.statusCallback

	go func() {
		defer close(chunks)
		defer close(done)

		hasError := false

		// Create a channel to pull values from the iterator
		type iterResult struct {
			resp *genai.GenerateContentResponse
			err  error
		}
		iterCh := make(chan iterResult)

		// Start goroutine to pull from iterator
		go func() {
			defer close(iterCh)
			for resp, err := range iter {
				iterCh <- iterResult{resp, err}
			}
		}()

		// Timers for idle detection
		idleTimer := time.NewTimer(streamIdleTimeout)
		defer idleTimer.Stop()
		warningTimer := time.NewTimer(streamIdleWarning)
		defer warningTimer.Stop()
		lastWarningAt := time.Duration(0)

		// Process iterator results with context checking
	streamLoop:
		for {
		waitLoop:
			for {
				select {
				case <-ctx.Done():
					hasError = true
					// Non-blocking send - channel might be full or receiver gone
					select {
					case chunks <- ResponseChunk{Error: ctx.Err(), Done: true}:
					default:
					}
					return

				case <-warningTimer.C:
					// Stream idle warning - notify UI
					lastWarningAt += streamIdleWarning
					if statusCb != nil {
						statusCb.OnStreamIdle(lastWarningAt)
					}
					// Reset for next warning (every 10 seconds after first)
					warningTimer.Reset(10 * time.Second)
					continue waitLoop

				case <-idleTimer.C:
					// Stream idle timeout - fail the stream
					hasError = true
					logging.Warn("stream idle timeout exceeded", "timeout", streamIdleTimeout)
					chunks <- ResponseChunk{
						Error: fmt.Errorf("stream idle timeout: no data received for %v", streamIdleTimeout),
						Done:  true,
					}
					return

				case result, ok := <-iterCh:
					// Got data - notify resume if we had warned
					if lastWarningAt > 0 && statusCb != nil {
						statusCb.OnStreamResume()
					}
					lastWarningAt = 0

					// Reset timers
					resetTimer(idleTimer, streamIdleTimeout)
					resetTimer(warningTimer, streamIdleWarning)

					if !ok {
						// Iterator channel closed, stream complete
						break streamLoop
					}

					if result.err != nil {
						hasError = true
						// Notify about recoverable error if applicable
						if c.isRetryableError(result.err) && statusCb != nil {
							statusCb.OnError(result.err, true)
						}
						select {
						case chunks <- ResponseChunk{Error: result.err, Done: true}:
						case <-ctx.Done():
						}
						break streamLoop
					}

					// Check for end of stream
					if result.resp == nil {
						// Stream completed successfully
						break streamLoop
					}

					chunk := processResponse(result.resp)

					// Use select to prevent goroutine leak if receiver stops reading
					select {
					case chunks <- chunk:
					case <-ctx.Done():
						hasError = true // Treat context cancellation as error for token return
						// Non-blocking send - channel might be full or receiver gone
						select {
						case chunks <- ResponseChunk{Error: ctx.Err(), Done: true}:
						default:
						}
						return
					}

					if chunk.Done {
						break streamLoop
					}

					// Continue to next iteration of streamLoop
					break waitLoop
				}
			}
		}

		// Return tokens if streaming failed
		if hasError && rateLimiter != nil && estimatedForGoroutine > 0 {
			rateLimiter.ReturnTokens(1, estimatedForGoroutine)
		}
	}()

	return &StreamingResponse{
		Chunks: chunks,
		Done:   done,
	}, nil
}

// processResponse converts a Gemini response to a ResponseChunk.
func processResponse(resp *genai.GenerateContentResponse) ResponseChunk {
	chunk := ResponseChunk{}

	// Extract usage metadata if available
	if resp.UsageMetadata != nil {
		chunk.InputTokens = int(resp.UsageMetadata.PromptTokenCount)
		chunk.OutputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
	}

	if len(resp.Candidates) == 0 {
		chunk.Done = true
		return chunk
	}

	candidate := resp.Candidates[0]
	chunk.FinishReason = candidate.FinishReason

	if candidate.Content != nil {
		// Store original parts (with ThoughtSignature intact)
		chunk.Parts = candidate.Content.Parts

		for _, part := range candidate.Content.Parts {
			if part.Thought {
				chunk.Thinking += part.Text
				continue
			}
			if part.Text != "" {
				chunk.Text += part.Text
			}
			if part.FunctionCall != nil {
				chunk.FunctionCalls = append(chunk.FunctionCalls, part.FunctionCall)
			}
		}
	}

	// Check if this is the final chunk
	if candidate.FinishReason != "" {
		chunk.Done = true
	}

	return chunk
}

// Close closes the client connection.
func (c *GeminiClient) Close() error {
	// The genai client doesn't have an explicit close method
	return nil
}

// CountTokens counts tokens for the given contents with retry logic.
func (c *GeminiClient) CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error) {
	var lastErr error

	maxDelay := 30 * time.Second
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := CalculateBackoff(c.retryDelay, attempt-1, maxDelay)

			// Notify UI about retry
			if c.statusCallback != nil {
				reason := "token count failed"
				if lastErr != nil {
					reason = lastErr.Error()
					if len(reason) > 50 {
						reason = reason[:47] + "..."
					}
				}
				c.statusCallback.OnRetry(attempt, c.maxRetries, delay, reason)
			}

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		resp, err := c.client.Models.CountTokens(ctx, c.model, contents, nil)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if !c.isRetryableError(err) {
			return nil, err
		}

		logging.Warn("CountTokens failed, will retry", "attempt", attempt, "error", err)
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", c.maxRetries, lastErr)
}

// GetModel returns the model name.
func (c *GeminiClient) GetModel() string {
	return c.model
}

// SetModel changes the model for this client.
func (c *GeminiClient) SetModel(modelName string) {
	c.model = modelName
}

// WithModel returns a new client configured for the specified model.
func (c *GeminiClient) WithModel(modelName string) Client {
	return &GeminiClient{
		client:            c.client,
		model:             modelName,
		config:            c.config,
		tools:             c.tools,
		rateLimiter:       c.rateLimiter,
		maxRetries:        c.maxRetries,
		retryDelay:        c.retryDelay,
		statusCallback:    c.statusCallback,
		systemInstruction: c.systemInstruction,
		thinkingBudget:    c.thinkingBudget,
	}
}

// Ptr returns a pointer to the given value.
func Ptr[T any](v T) *T {
	return &v
}

// GetRawClient returns the underlying genai.Client for direct API access.
func (c *GeminiClient) GetRawClient() interface{} {
	return c.client
}
