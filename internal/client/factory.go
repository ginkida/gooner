package client

import (
	"context"
	"fmt"
	"strings"

	"gokin/internal/config"
	"gokin/internal/logging"
	"gokin/internal/security"
)

// globalPool is the shared client connection pool.
var globalPool *ClientPool

// GetPool returns the global client connection pool, creating it if necessary.
func GetPool(cfg *config.Config) *ClientPool {
	if globalPool == nil {
		maxSize := cfg.Model.MaxPoolSize
		if maxSize <= 0 {
			maxSize = DefaultMaxPoolSize
		}
		globalPool = NewClientPool(maxSize)
	}
	return globalPool
}

// ClosePool closes the global client connection pool.
func ClosePool() {
	if globalPool != nil {
		globalPool.Close()
		globalPool = nil
	}
}

// NewClient creates a client based on the configuration and model provider.
// This is the main entry point for client creation.
// If FallbackProviders are configured, returns a FallbackClient wrapping
// clients for the primary provider and each fallback provider.
// Uses the connection pool to reuse existing clients when possible.
func NewClient(ctx context.Context, cfg *config.Config, modelID string) (Client, error) {
	// Migrate configuration to new format
	config.MigrateConfig(cfg)

	// Normalize configuration
	if err := config.NormalizeConfig(cfg); err != nil {
		return nil, err
	}

	// If modelID is not specified, use default from config
	if modelID == "" {
		modelID = cfg.Model.Name
	}

	logging.Debug("creating client",
		"provider", cfg.Model.Provider,
		"modelID", modelID,
		"preset", cfg.Model.Preset)

	// Determine the primary provider
	provider := cfg.Model.Provider
	if provider == "" {
		provider = cfg.API.Backend
	}

	// If fallback providers are configured, build a FallbackClient
	if len(cfg.Model.FallbackProviders) > 0 {
		return newFallbackClientFromConfig(ctx, cfg, provider, modelID)
	}

	// Single client creation with pool support
	return getOrCreateClient(ctx, cfg, provider, modelID)
}

// newFallbackClientFromConfig creates a FallbackClient with the primary provider
// and each configured fallback provider.
func newFallbackClientFromConfig(ctx context.Context, cfg *config.Config, primaryProvider, modelID string) (Client, error) {
	var clients []Client

	// Create the primary client
	primary, err := getOrCreateClient(ctx, cfg, primaryProvider, modelID)
	if err != nil {
		logging.Warn("failed to create primary client, trying fallbacks",
			"provider", primaryProvider,
			"error", err.Error())
	} else {
		clients = append(clients, primary)
	}

	// Create fallback clients
	for _, fbProvider := range cfg.Model.FallbackProviders {
		fbProvider = strings.TrimSpace(fbProvider)
		if fbProvider == "" || fbProvider == primaryProvider {
			continue
		}

		fbClient, fbErr := getOrCreateClient(ctx, cfg, fbProvider, modelID)
		if fbErr != nil {
			logging.Warn("failed to create fallback client",
				"provider", fbProvider,
				"error", fbErr.Error())
			continue
		}
		clients = append(clients, fbClient)
	}

	if len(clients) == 0 {
		return nil, fmt.Errorf("failed to create any client: primary provider %q and all fallback providers failed", primaryProvider)
	}

	return NewFallbackClient(clients)
}

// getOrCreateClient retrieves a client from the pool or creates a new one.
func getOrCreateClient(ctx context.Context, cfg *config.Config, provider, modelID string) (Client, error) {
	pool := GetPool(cfg)

	// Check pool first
	if c, ok := pool.Get(provider, modelID); ok {
		return c, nil
	}

	// Create new client
	c, err := createClientForProvider(ctx, cfg, provider, modelID)
	if err != nil {
		return nil, err
	}

	// Store in pool for reuse
	pool.Put(provider, modelID, c)

	return c, nil
}

// createClientForProvider creates a new client for the given provider.
func createClientForProvider(ctx context.Context, cfg *config.Config, provider, modelID string) (Client, error) {
	switch provider {
	case "glm":
		return newGLMClient(cfg, modelID)
	case "deepseek":
		return newDeepSeekClient(cfg, modelID)
	case "gemini":
		// Check OAuth first
		if cfg.API.HasOAuthToken("gemini") {
			logging.Debug("using Gemini OAuth client", "email", cfg.API.GeminiOAuth.Email)
			return NewGeminiOAuthClient(ctx, cfg)
		}
		return NewGeminiClient(ctx, cfg)
	case "anthropic":
		return newAnthropicClientForModelID(cfg, modelID)
	case "ollama":
		return newOllamaClient(cfg, modelID)
	default:
		// Fallback to auto-detection from model name
		return autoDetectClient(ctx, cfg, modelID)
	}
}

// autoDetectClient attempts to create a client by detecting the provider from the model name.
func autoDetectClient(ctx context.Context, cfg *config.Config, modelID string) (Client, error) {
	logging.Debug("unknown provider, auto-detecting from model name", "modelID", modelID)

	// Check GLM models first
	if strings.HasPrefix(modelID, "glm") {
		return newGLMClient(cfg, modelID)
	}

	// Check Claude models
	if strings.HasPrefix(modelID, "claude") {
		return newAnthropicClientForModelID(cfg, modelID)
	}

	// Check DeepSeek models (API, not Ollama)
	modelLower := strings.ToLower(modelID)
	if strings.HasPrefix(modelLower, "deepseek") {
		return newDeepSeekClient(cfg, modelID)
	}

	// Check common open-source model prefixes (typically run via Ollama)
	ollamaPrefixes := []string{
		"llama", "qwen", "codellama", "mistral", "phi", "gemma",
		"vicuna", "yi", "starcoder", "wizardcoder", "orca", "neural", "solar",
		"openchat", "zephyr", "dolphin", "nous", "tinyllama", "stablelm",
	}
	for _, prefix := range ollamaPrefixes {
		if strings.HasPrefix(modelLower, prefix) {
			return newOllamaClient(cfg, modelID)
		}
	}

	// Default to Gemini (check OAuth first)
	if cfg.API.HasOAuthToken("gemini") {
		logging.Debug("using Gemini OAuth client (default)", "email", cfg.API.GeminiOAuth.Email)
		return NewGeminiOAuthClient(ctx, cfg)
	}
	return NewGeminiClient(ctx, cfg)
}

// newGLMClient creates a GLM (GLM-4.7) client using Anthropic-compatible API.
func newGLMClient(cfg *config.Config, modelID string) (Client, error) {
	// Load API key from environment or config (try GLMKey first, then legacy APIKey)
	loadedKey := security.GetGLMKey(cfg.API.GLMKey, cfg.API.APIKey)

	if !loadedKey.IsSet() {
		return nil, fmt.Errorf("GLM API key required (set GOKIN_GLM_KEY environment variable or use /login glm <key>)")
	}

	// Log key source for debugging (without exposing the key)
	logging.Debug("loaded GLM API key",
		"source", loadedKey.Source,
		"model", modelID)

	// Validate key format
	if err := security.ValidateKeyFormat(loadedKey.Value); err != nil {
		return nil, fmt.Errorf("invalid GLM API key: %w", err)
	}

	// Use custom base URL if provided, otherwise use default GLM endpoint
	baseURL := cfg.Model.CustomBaseURL
	if baseURL == "" {
		baseURL = "https://api.z.ai/api/anthropic"
	}

	anthropicConfig := AnthropicConfig{
		APIKey:        loadedKey.Value,
		BaseURL:       baseURL,
		Model:         modelID,
		MaxTokens:     cfg.Model.MaxOutputTokens,
		Temperature:   cfg.Model.Temperature,
		StreamEnabled: true,
		// Retry configuration from config
		MaxRetries:  cfg.API.Retry.MaxRetries,
		RetryDelay:  cfg.API.Retry.RetryDelay,
		HTTPTimeout: cfg.API.Retry.HTTPTimeout,
	}

	return NewAnthropicClient(anthropicConfig)
}

// newAnthropicClientForModel creates an Anthropic-compatible client for a specific model (from ModelInfo).
func newAnthropicClientForModel(cfg *config.Config, modelInfo ModelInfo) (Client, error) {
	// Check for custom base URL override in config
	baseURL := modelInfo.BaseURL
	if cfg.Model.CustomBaseURL != "" && cfg.Model.Name == modelInfo.ID {
		baseURL = cfg.Model.CustomBaseURL
	}

	// If still no base URL, use default for provider
	if baseURL == "" {
		if strings.Contains(modelInfo.ID, "glm") {
			baseURL = "https://api.z.ai/api/anthropic"
		} else {
			baseURL = "https://api.anthropic.com"
		}
	}

	// Determine API key to use
	apiKey := cfg.API.APIKey

	if apiKey == "" {
		return nil, fmt.Errorf("API key required for Anthropic-compatible model %s", modelInfo.ID)
	}

	anthropicConfig := AnthropicConfig{
		APIKey:        apiKey,
		BaseURL:       baseURL,
		Model:         modelInfo.ID,
		MaxTokens:     cfg.Model.MaxOutputTokens,
		Temperature:   cfg.Model.Temperature,
		StreamEnabled: true,
		// Retry configuration from config
		MaxRetries:  cfg.API.Retry.MaxRetries,
		RetryDelay:  cfg.API.Retry.RetryDelay,
		HTTPTimeout: cfg.API.Retry.HTTPTimeout,
	}

	return NewAnthropicClient(anthropicConfig)
}

// newAnthropicClientForModelID creates an Anthropic-compatible client from a model ID string.
func newAnthropicClientForModelID(cfg *config.Config, modelID string) (Client, error) {
	// Create a synthetic ModelInfo from model ID
	modelInfo := ModelInfo{
		ID:   modelID,
		Name: modelID,
	}

	// Check if user has overridden the base URL in config
	if cfg.Model.CustomBaseURL != "" {
		modelInfo.BaseURL = cfg.Model.CustomBaseURL
		modelInfo.Provider = "anthropic"
	} else if strings.HasPrefix(modelID, "glm") {
		// Set default base URL for GLM models
		modelInfo.Provider = "anthropic"
		modelInfo.BaseURL = "https://api.z.ai/api/anthropic"
	} else {
		modelInfo.Provider = "anthropic"
		modelInfo.BaseURL = "https://api.anthropic.com"
	}

	return newAnthropicClientForModel(cfg, modelInfo)
}

// newDeepSeekClient creates a DeepSeek client using Anthropic-compatible API.
func newDeepSeekClient(cfg *config.Config, modelID string) (Client, error) {
	// Load API key from environment or config (try DeepSeekKey first, then legacy APIKey)
	loadedKey := security.GetDeepSeekKey(cfg.API.DeepSeekKey, cfg.API.APIKey)

	if !loadedKey.IsSet() {
		return nil, fmt.Errorf("DeepSeek API key required (set GOKIN_DEEPSEEK_KEY environment variable or use /login deepseek <key>)")
	}

	// Log key source for debugging (without exposing the key)
	logging.Debug("loaded DeepSeek API key",
		"source", loadedKey.Source,
		"model", modelID)

	// Validate key format
	if err := security.ValidateKeyFormat(loadedKey.Value); err != nil {
		return nil, fmt.Errorf("invalid DeepSeek API key: %w", err)
	}

	// Use custom base URL if provided, otherwise use default DeepSeek endpoint
	baseURL := cfg.Model.CustomBaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/anthropic"
	}

	anthropicConfig := AnthropicConfig{
		APIKey:        loadedKey.Value,
		BaseURL:       baseURL,
		Model:         modelID,
		MaxTokens:     cfg.Model.MaxOutputTokens,
		Temperature:   cfg.Model.Temperature,
		StreamEnabled: true,
		// Retry configuration from config
		MaxRetries:  cfg.API.Retry.MaxRetries,
		RetryDelay:  cfg.API.Retry.RetryDelay,
		HTTPTimeout: cfg.API.Retry.HTTPTimeout,
	}

	return NewAnthropicClient(anthropicConfig)
}

// newOllamaClient creates an Ollama client for local LLM inference.
func newOllamaClient(cfg *config.Config, modelID string) (Client, error) {
	// Load optional API key (for remote Ollama servers with auth)
	loadedKey := security.GetOllamaKey(cfg.API.OllamaKey)

	// Log key source for debugging (without exposing the key)
	if loadedKey.IsSet() {
		logging.Debug("loaded Ollama API key",
			"source", loadedKey.Source,
			"model", modelID)
	}

	// Use custom base URL if provided, otherwise use default
	baseURL := cfg.API.OllamaBaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	ollamaConfig := OllamaConfig{
		BaseURL:     baseURL,
		APIKey:      loadedKey.Value, // Optional
		Model:       modelID,
		Temperature: cfg.Model.Temperature,
		MaxTokens:   cfg.Model.MaxOutputTokens,
		HTTPTimeout: cfg.API.Retry.HTTPTimeout,
		MaxRetries:  cfg.API.Retry.MaxRetries,
		RetryDelay:  cfg.API.Retry.RetryDelay,
	}

	return NewOllamaClient(ollamaConfig)
}
