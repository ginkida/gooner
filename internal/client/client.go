package client

import (
	"context"

	"google.golang.org/genai"
)

// ModelInfo contains information about an available model.
type ModelInfo struct {
	ID          string // Model identifier (e.g., "gemini-2.5-flash", "glm-4.7")
	Name        string // Human-readable name
	Description string // Short description
	Provider    string // Provider: "gemini" or "anthropic"
	BaseURL     string // Custom base URL for Anthropic-compatible APIs (e.g., GLM-4.7)
}

// AvailableModels is the list of supported models across all providers.
var AvailableModels = []ModelInfo{
	// Gemini models (API key)
	{
		ID:          "gemini-3-flash-preview",
		Name:        "Gemini 3 Flash",
		Description: "Fast & cheap: $0.50/$3 per 1M tokens",
		Provider:    "gemini",
	},
	{
		ID:          "gemini-3-pro-preview",
		Name:        "Gemini 3 Pro",
		Description: "Most capable: $2/$12 per 1M tokens",
		Provider:    "gemini",
	},
	// Gemini models (OAuth / Code Assist API)
	{
		ID:          "gemini-2.5-flash",
		Name:        "Gemini 2.5 Flash",
		Description: "Fast model (Code Assist)",
		Provider:    "gemini",
	},
	{
		ID:          "gemini-2.5-pro",
		Name:        "Gemini 2.5 Pro",
		Description: "Advanced model (Code Assist)",
		Provider:    "gemini",
	},
	// GLM model (via Anthropic-compatible API)
	{
		ID:          "glm-4.7",
		Name:        "GLM-4.7",
		Description: "Powerful coding assistant: 131K max output",
		Provider:    "glm",
		BaseURL:     "https://api.z.ai/api/anthropic",
	},
	// DeepSeek models (via Anthropic-compatible API)
	{
		ID:          "deepseek-chat",
		Name:        "DeepSeek Chat",
		Description: "Powerful coding assistant from DeepSeek",
		Provider:    "deepseek",
		BaseURL:     "https://api.deepseek.com/anthropic",
	},
	{
		ID:          "deepseek-reasoner",
		Name:        "DeepSeek Reasoner",
		Description: "Extended reasoning model (thinking)",
		Provider:    "deepseek",
		BaseURL:     "https://api.deepseek.com/anthropic",
	},
	// Ollama (local models - use exact name from 'ollama list')
	{
		ID:          "ollama",
		Name:        "Ollama (Local)",
		Description: "Local LLM. Use --model <name> from 'ollama list'",
		Provider:    "ollama",
	},
}

// GetModelsForProvider returns models filtered by provider.
func GetModelsForProvider(provider string) []ModelInfo {
	var models []ModelInfo
	for _, m := range AvailableModels {
		if m.Provider == provider {
			models = append(models, m)
		}
	}
	return models
}

// IsValidModel checks if a model ID is valid.
func IsValidModel(modelID string) bool {
	for _, m := range AvailableModels {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

// GetModelInfo returns information about a specific model.
func GetModelInfo(modelID string) (ModelInfo, bool) {
	for _, m := range AvailableModels {
		if m.ID == modelID {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// Client defines the interface for AI model interactions.
type Client interface {
	// SendMessage sends a message and returns a streaming response.
	SendMessage(ctx context.Context, message string) (*StreamingResponse, error)

	// SendMessageWithHistory sends a message with conversation history.
	SendMessageWithHistory(ctx context.Context, history []*genai.Content, message string) (*StreamingResponse, error)

	// SendFunctionResponse sends function call results back to the model.
	SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error)

	// SetTools sets the tools available for the model to use.
	SetTools(tools []*genai.Tool)

	// SetRateLimiter sets the rate limiter for API calls.
	SetRateLimiter(limiter interface{})

	// CountTokens counts tokens for the given contents.
	CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error)

	// GetModel returns the model name.
	GetModel() string

	// SetModel changes the model for this client.
	SetModel(modelName string)

	// WithModel returns a new client configured for the specified model.
	WithModel(modelName string) Client

	// GetRawClient returns the underlying client for direct API access.
	GetRawClient() interface{}

	// SetSystemInstruction sets the system-level instruction for the model.
	// This is passed via the API's native system instruction parameter
	// rather than being injected as a user message in the conversation history.
	SetSystemInstruction(instruction string)

	// Close closes the client connection.
	Close() error
}

// RateLimiter interface for rate limiting API calls (optional).
type RateLimiter interface {
	AcquireWithContext(ctx context.Context, tokens int64) error
	ReturnTokens(requests int, tokens int64)
}

// StreamingResponse represents a streaming response from the model.
type StreamingResponse struct {
	// Chunks is a channel that receives response chunks.
	Chunks <-chan ResponseChunk

	// Done is closed when the response is complete.
	Done <-chan struct{}
}

// ResponseChunk represents a single chunk in a streaming response.
type ResponseChunk struct {
	// Text contains any text content in this chunk.
	Text string

	// Thinking contains extended thinking content (Anthropic API).
	Thinking string

	// FunctionCalls contains any function calls in this chunk.
	FunctionCalls []*genai.FunctionCall

	// Parts contains the original parts from the response (with ThoughtSignature).
	Parts []*genai.Part

	// Error contains any error that occurred.
	Error error

	// Done indicates if this is the final chunk.
	Done bool

	// FinishReason indicates why the response finished.
	FinishReason genai.FinishReason

	// InputTokens from API usage metadata (if available).
	InputTokens int

	// OutputTokens from API usage metadata (if available).
	OutputTokens int
}

// Response represents a complete response from the model.
type Response struct {
	// Text is the accumulated text response.
	Text string

	// Thinking is the accumulated extended thinking content (Anthropic API).
	Thinking string

	// FunctionCalls contains all function calls from the response.
	FunctionCalls []*genai.FunctionCall

	// Parts contains all original parts from the response (with ThoughtSignature).
	Parts []*genai.Part

	// FinishReason indicates why the response finished.
	FinishReason genai.FinishReason

	// InputTokens from API usage metadata (prompt tokens, if available).
	InputTokens int

	// OutputTokens from API usage metadata (completion tokens, if available).
	OutputTokens int
}

// Collect collects all chunks from a streaming response into a single Response.
func (sr *StreamingResponse) Collect() (*Response, error) {
	resp := &Response{}

	for chunk := range sr.Chunks {
		if chunk.Error != nil {
			return nil, chunk.Error
		}

		resp.Text += chunk.Text
		resp.Thinking += chunk.Thinking
		resp.FunctionCalls = append(resp.FunctionCalls, chunk.FunctionCalls...)
		resp.Parts = append(resp.Parts, chunk.Parts...)

		if chunk.Done {
			resp.FinishReason = chunk.FinishReason
		}

		// Keep the latest non-zero usage metadata (typically from the final chunk)
		if chunk.InputTokens > 0 {
			resp.InputTokens = chunk.InputTokens
		}
		if chunk.OutputTokens > 0 {
			resp.OutputTokens += chunk.OutputTokens
		}
	}

	return resp, nil
}
