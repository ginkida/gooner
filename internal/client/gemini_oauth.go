package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gokin/internal/auth"
	"gokin/internal/config"
	"gokin/internal/logging"
	"gokin/internal/ratelimit"

	"google.golang.org/genai"
)

// GeminiOAuthClient implements Client interface using OAuth tokens
// and the Code Assist API (cloudcode-pa.googleapis.com)
type GeminiOAuthClient struct {
	httpClient   *http.Client
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	email        string
	projectID    string
	config       *config.Config
	model        string
	tools        []*genai.Tool
	rateLimiter  *ratelimit.Limiter
	maxRetries   int
	retryDelay   time.Duration
	genConfig    *genai.GenerateContentConfig

	statusCallback    StatusCallback
	systemInstruction string

	mu sync.RWMutex
}

// NewGeminiOAuthClient creates a new client using OAuth tokens
func NewGeminiOAuthClient(ctx context.Context, cfg *config.Config) (*GeminiOAuthClient, error) {
	oauth := cfg.API.GeminiOAuth
	if oauth == nil {
		return nil, fmt.Errorf("no OAuth token configured")
	}

	maxRetries := cfg.API.Retry.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	retryDelay := cfg.API.Retry.RetryDelay
	if retryDelay == 0 {
		retryDelay = 1 * time.Second
	}

	httpTimeout := cfg.API.Retry.HTTPTimeout
	if httpTimeout == 0 {
		httpTimeout = 120 * time.Second
	}

	client := &GeminiOAuthClient{
		httpClient: &http.Client{
			Timeout: httpTimeout,
		},
		accessToken:  oauth.AccessToken,
		refreshToken: oauth.RefreshToken,
		expiresAt:    time.Unix(oauth.ExpiresAt, 0),
		email:        oauth.Email,
		projectID:    oauth.ProjectID,
		config:       cfg,
		model:        cfg.Model.Name,
		maxRetries:   maxRetries,
		retryDelay:   retryDelay,
		genConfig: &genai.GenerateContentConfig{
			Temperature:     Ptr(cfg.Model.Temperature),
			MaxOutputTokens: cfg.Model.MaxOutputTokens,
		},
	}

	// Ensure token is valid
	if err := client.ensureValidToken(ctx); err != nil {
		return nil, fmt.Errorf("failed to validate OAuth token: %w", err)
	}

	// Load project ID if not cached
	if client.projectID == "" {
		if err := client.loadProjectID(ctx); err != nil {
			return nil, fmt.Errorf("failed to load project ID: %w", err)
		}
	}

	logging.Debug("created Gemini OAuth client",
		"email", client.email,
		"model", client.model,
		"projectID", client.projectID,
		"expires", client.expiresAt.Format(time.RFC3339))

	return client, nil
}

// loadProjectID loads the project ID from Code Assist API
func (c *GeminiOAuthClient) loadProjectID(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	url := auth.GeminiCodeAssistAPI + "/v1internal:loadCodeAssist"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range auth.CodeAssistHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to load code assist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("loadCodeAssist failed (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse loadCodeAssist response: %w", err)
	}

	if result.CloudaicompanionProject == "" {
		return fmt.Errorf("no project ID returned from Code Assist API")
	}

	c.projectID = result.CloudaicompanionProject

	// Save to config
	c.config.API.GeminiOAuth.ProjectID = c.projectID
	if err := c.config.Save(); err != nil {
		logging.Warn("failed to save project ID to config", "error", err)
	}

	logging.Debug("loaded Code Assist project ID", "projectID", c.projectID)
	return nil
}

// SetSystemInstruction sets the system-level instruction for the model.
func (c *GeminiOAuthClient) SetSystemInstruction(instruction string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.systemInstruction = instruction
}

// SetTools sets the tools available for function calling
func (c *GeminiOAuthClient) SetTools(tools []*genai.Tool) {
	c.tools = tools
}

// SetRateLimiter sets the rate limiter for API calls
func (c *GeminiOAuthClient) SetRateLimiter(limiter interface{}) {
	if rl, ok := limiter.(*ratelimit.Limiter); ok {
		c.rateLimiter = rl
	}
}

// SetStatusCallback sets the callback for status updates
func (c *GeminiOAuthClient) SetStatusCallback(cb StatusCallback) {
	c.statusCallback = cb
}

// SendMessage sends a user message and returns a streaming response
func (c *GeminiOAuthClient) SendMessage(ctx context.Context, message string) (*StreamingResponse, error) {
	return c.SendMessageWithHistory(ctx, nil, message)
}

// SendMessageWithHistory sends a message with conversation history
func (c *GeminiOAuthClient) SendMessageWithHistory(ctx context.Context, history []*genai.Content, message string) (*StreamingResponse, error) {
	contents := make([]*genai.Content, len(history)+1)
	copy(contents, history)
	contents[len(contents)-1] = genai.NewContentFromText(message, genai.RoleUser)

	return c.generateContentStream(ctx, contents)
}

// SendFunctionResponse sends function call results back to the model
func (c *GeminiOAuthClient) SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error) {
	var parts []*genai.Part
	for _, result := range results {
		part := genai.NewPartFromFunctionResponse(result.Name, result.Response)
		part.FunctionResponse.ID = result.ID
		parts = append(parts, part)
	}

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

// CountTokens counts tokens for the given contents
func (c *GeminiOAuthClient) CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error) {
	// The Code Assist API doesn't have a separate count tokens endpoint
	// Return an estimate based on content length
	totalChars := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.Text != "" {
				totalChars += len(part.Text)
			}
		}
	}

	// Rough estimate: ~4 chars per token
	estimatedTokens := int32(totalChars / 4)
	return &genai.CountTokensResponse{
		TotalTokens: estimatedTokens,
	}, nil
}

// GetModel returns the model name
func (c *GeminiOAuthClient) GetModel() string {
	return c.model
}

// SetModel changes the model for this client
func (c *GeminiOAuthClient) SetModel(modelName string) {
	c.model = modelName
}

// WithModel returns a new client configured for the specified model
func (c *GeminiOAuthClient) WithModel(modelName string) Client {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return &GeminiOAuthClient{
		httpClient:        c.httpClient,
		accessToken:       c.accessToken,
		refreshToken:      c.refreshToken,
		expiresAt:         c.expiresAt,
		email:             c.email,
		projectID:         c.projectID,
		config:            c.config,
		model:             modelName,
		tools:             c.tools,
		rateLimiter:       c.rateLimiter,
		maxRetries:        c.maxRetries,
		retryDelay:        c.retryDelay,
		genConfig:         c.genConfig,
		statusCallback:    c.statusCallback,
		systemInstruction: c.systemInstruction,
	}
}

// GetRawClient returns nil since we use HTTP directly
func (c *GeminiOAuthClient) GetRawClient() interface{} {
	return nil
}

// Close closes the client connection
func (c *GeminiOAuthClient) Close() error {
	return nil
}

// ensureValidToken checks if the token is valid and refreshes if needed
func (c *GeminiOAuthClient) ensureValidToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if token is still valid (with 5 minute buffer)
	if time.Now().Before(c.expiresAt.Add(-auth.TokenRefreshBuffer)) {
		return nil
	}

	logging.Debug("OAuth token expired, refreshing",
		"expiresAt", c.expiresAt.Format(time.RFC3339))

	// Refresh the token
	manager := auth.NewGeminiOAuthManager()
	newToken, err := manager.RefreshToken(ctx, c.refreshToken)
	if err != nil {
		return fmt.Errorf("failed to refresh token: %w", err)
	}

	// Update client state
	c.accessToken = newToken.AccessToken
	c.expiresAt = newToken.ExpiresAt
	if newToken.RefreshToken != "" {
		c.refreshToken = newToken.RefreshToken
	}

	// Persist to config
	c.config.API.GeminiOAuth.AccessToken = newToken.AccessToken
	c.config.API.GeminiOAuth.ExpiresAt = newToken.ExpiresAt.Unix()
	if newToken.RefreshToken != "" {
		c.config.API.GeminiOAuth.RefreshToken = newToken.RefreshToken
	}

	if err := c.config.Save(); err != nil {
		logging.Warn("failed to save refreshed OAuth token", "error", err)
	}

	logging.Debug("OAuth token refreshed",
		"expiresAt", c.expiresAt.Format(time.RFC3339))

	return nil
}

// generateContentStream handles the streaming content generation
func (c *GeminiOAuthClient) generateContentStream(ctx context.Context, contents []*genai.Content) (*StreamingResponse, error) {
	contents = sanitizeContents(contents)

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.retryDelay * time.Duration(1<<uint(attempt-1))
			logging.Info("retrying OAuth Gemini request", "attempt", attempt, "delay", delay)

			if c.statusCallback != nil {
				reason := "API error"
				if lastErr != nil {
					reason = lastErr.Error()
					if strings.Contains(reason, "429") {
						reason = "rate limit"
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
		if !c.isRetryableError(err) {
			return nil, err
		}

		logging.Warn("OAuth Gemini request failed, will retry", "attempt", attempt, "error", err)
	}

	return nil, fmt.Errorf("max retries (%d) exceeded: %w", c.maxRetries, lastErr)
}

// doGenerateContentStream performs a single streaming request
func (c *GeminiOAuthClient) doGenerateContentStream(ctx context.Context, contents []*genai.Content) (*StreamingResponse, error) {
	if err := c.ensureValidToken(ctx); err != nil {
		return nil, err
	}

	// Rate limiting
	var estimatedTokens int64
	if c.rateLimiter != nil {
		estimatedTokens = ratelimit.EstimateTokensFromContents(len(contents), 500)
		if err := c.rateLimiter.AcquireWithContext(ctx, estimatedTokens); err != nil {
			if c.statusCallback != nil {
				c.statusCallback.OnRateLimit(5 * time.Second)
			}
			return nil, fmt.Errorf("rate limit: %w", err)
		}
	}

	// Build request body in Code Assist format
	reqBody := c.buildRequest(contents)

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Use v1internal endpoint with streaming
	url := auth.GeminiCodeAssistAPI + "/v1internal:streamGenerateContent?alt=sse"

	c.mu.RLock()
	token := c.accessToken
	projectID := c.projectID
	c.mu.RUnlock()

	if projectID == "" {
		return nil, fmt.Errorf("no project ID available")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range auth.CodeAssistHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.rateLimiter != nil && estimatedTokens > 0 {
			c.rateLimiter.ReturnTokens(1, estimatedTokens)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if c.rateLimiter != nil && estimatedTokens > 0 {
			c.rateLimiter.ReturnTokens(1, estimatedTokens)
		}
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	// Create streaming response
	chunks := make(chan ResponseChunk, 10)
	done := make(chan struct{})

	go c.processSSEStream(ctx, resp.Body, chunks, done, estimatedTokens)

	return &StreamingResponse{
		Chunks: chunks,
		Done:   done,
	}, nil
}

// buildRequest builds the API request body in Code Assist format
func (c *GeminiOAuthClient) buildRequest(contents []*genai.Content) map[string]interface{} {
	// Convert contents to API format
	apiContents := make([]map[string]interface{}, 0, len(contents))
	for _, content := range contents {
		parts := make([]map[string]interface{}, 0, len(content.Parts))
		for _, part := range content.Parts {
			if part.Text != "" {
				parts = append(parts, map[string]interface{}{"text": part.Text})
			}
			if part.FunctionCall != nil {
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{
						"name": part.FunctionCall.Name,
						"args": part.FunctionCall.Args,
					},
					"thoughtSignature": "skip_thought_signature_validator",
				})
			}
			if part.FunctionResponse != nil {
				parts = append(parts, map[string]interface{}{
					"functionResponse": map[string]interface{}{
						"name":     part.FunctionResponse.Name,
						"response": part.FunctionResponse.Response,
					},
				})
			}
		}
		apiContents = append(apiContents, map[string]interface{}{
			"role":  string(content.Role),
			"parts": parts,
		})
	}

	// Inner request payload
	requestPayload := map[string]interface{}{
		"contents": apiContents,
	}

	// Add system instruction if set
	c.mu.RLock()
	sysInstruction := c.systemInstruction
	c.mu.RUnlock()
	if sysInstruction != "" {
		requestPayload["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": sysInstruction},
			},
		}
	}

	// Add generation config
	if c.genConfig != nil {
		genConfig := map[string]interface{}{}
		if c.genConfig.Temperature != nil {
			genConfig["temperature"] = *c.genConfig.Temperature
		}
		if c.genConfig.MaxOutputTokens > 0 {
			genConfig["maxOutputTokens"] = c.genConfig.MaxOutputTokens
		}
		if len(genConfig) > 0 {
			requestPayload["generationConfig"] = genConfig
		}
	}

	// Add tools
	if len(c.tools) > 0 {
		toolDefs := make([]map[string]interface{}, 0)
		for _, tool := range c.tools {
			if tool.FunctionDeclarations != nil {
				funcs := make([]map[string]interface{}, 0, len(tool.FunctionDeclarations))
				for _, fd := range tool.FunctionDeclarations {
					funcDef := map[string]interface{}{
						"name":        fd.Name,
						"description": fd.Description,
					}
					if fd.Parameters != nil {
						funcDef["parameters"] = fd.Parameters
					}
					funcs = append(funcs, funcDef)
				}
				toolDefs = append(toolDefs, map[string]interface{}{
					"functionDeclarations": funcs,
				})
			}
		}
		if len(toolDefs) > 0 {
			requestPayload["tools"] = toolDefs
		}
	}

	// Wrap in Code Assist format
	return map[string]interface{}{
		"project": c.projectID,
		"model":   c.model,
		"request": requestPayload,
	}
}

// processSSEStream processes Server-Sent Events from the API
func (c *GeminiOAuthClient) processSSEStream(ctx context.Context, body io.ReadCloser, chunks chan<- ResponseChunk, done chan<- struct{}, estimatedTokens int64) {
	defer close(chunks)
	defer close(done)
	defer body.Close()

	hasError := false
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max line size

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			hasError = true
			chunks <- ResponseChunk{Error: ctx.Err(), Done: true}
			return
		default:
		}

		line := scanner.Text()

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Parse SSE data
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			// Check for end of stream
			if data == "[DONE]" {
				break
			}

			chunk, err := c.parseSSEData(data)
			if err != nil {
				logging.Debug("failed to parse SSE data", "error", err, "data", data)
				continue
			}

			select {
			case chunks <- chunk:
			case <-ctx.Done():
				hasError = true
				chunks <- ResponseChunk{Error: ctx.Err(), Done: true}
				return
			}

			if chunk.Done {
				break
			}
		}
	}

	if err := scanner.Err(); err != nil {
		hasError = true
		chunks <- ResponseChunk{Error: err, Done: true}
	}

	// Return tokens if error
	if hasError && c.rateLimiter != nil && estimatedTokens > 0 {
		c.rateLimiter.ReturnTokens(1, estimatedTokens)
	}
}

// parseSSEData parses a single SSE data chunk (Code Assist format)
func (c *GeminiOAuthClient) parseSSEData(data string) (ResponseChunk, error) {
	// Code Assist wraps response in { "response": { ... } }
	var wrapper struct {
		Response *struct {
			Candidates []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text         string `json:"text,omitempty"`
						FunctionCall *struct {
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
						} `json:"functionCall,omitempty"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason,omitempty"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata,omitempty"`
		} `json:"response"`
	}

	if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
		return ResponseChunk{}, err
	}

	chunk := ResponseChunk{}

	// Handle case where response is not wrapped
	resp := wrapper.Response
	if resp == nil {
		// Try parsing as direct response
		var direct struct {
			Candidates []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text         string `json:"text,omitempty"`
						FunctionCall *struct {
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
						} `json:"functionCall,omitempty"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason,omitempty"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &direct); err == nil && len(direct.Candidates) > 0 {
			// Process direct format
			if direct.UsageMetadata != nil {
				chunk.InputTokens = direct.UsageMetadata.PromptTokenCount
				chunk.OutputTokens = direct.UsageMetadata.CandidatesTokenCount
			}

			candidate := direct.Candidates[0]
			if candidate.FinishReason != "" {
				chunk.Done = true
				chunk.FinishReason = genai.FinishReason(candidate.FinishReason)
			}

			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					chunk.Text += part.Text
					chunk.Parts = append(chunk.Parts, genai.NewPartFromText(part.Text))
				}
				if part.FunctionCall != nil {
					fc := &genai.FunctionCall{
						Name: part.FunctionCall.Name,
						Args: part.FunctionCall.Args,
					}
					chunk.FunctionCalls = append(chunk.FunctionCalls, fc)
					chunk.Parts = append(chunk.Parts, &genai.Part{FunctionCall: fc})
				}
			}
			return chunk, nil
		}

		chunk.Done = true
		return chunk, nil
	}

	if resp.UsageMetadata != nil {
		chunk.InputTokens = resp.UsageMetadata.PromptTokenCount
		chunk.OutputTokens = resp.UsageMetadata.CandidatesTokenCount
	}

	if len(resp.Candidates) == 0 {
		chunk.Done = true
		return chunk, nil
	}

	candidate := resp.Candidates[0]
	if candidate.FinishReason != "" {
		chunk.Done = true
		chunk.FinishReason = genai.FinishReason(candidate.FinishReason)
	}

	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			chunk.Text += part.Text
			chunk.Parts = append(chunk.Parts, genai.NewPartFromText(part.Text))
		}
		if part.FunctionCall != nil {
			fc := &genai.FunctionCall{
				Name: part.FunctionCall.Name,
				Args: part.FunctionCall.Args,
			}
			chunk.FunctionCalls = append(chunk.FunctionCalls, fc)
			chunk.Parts = append(chunk.Parts, &genai.Part{FunctionCall: fc})
		}
	}

	return chunk, nil
}

// isRetryableError returns true if the error should trigger a retry
func (c *GeminiOAuthClient) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Check for retryable HTTP status codes
	retryableCodes := []string{"429", "500", "502", "503", "504"}
	for _, code := range retryableCodes {
		if strings.Contains(errStr, code) {
			return true
		}
	}

	// Check for network errors
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

// GetEmail returns the authenticated user's email
func (c *GeminiOAuthClient) GetEmail() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.email
}

// GetExpiresAt returns when the token expires
func (c *GeminiOAuthClient) GetExpiresAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expiresAt
}

// GetProjectID returns the Code Assist project ID
func (c *GeminiOAuthClient) GetProjectID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.projectID
}
