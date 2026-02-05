package context

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gokin/internal/client"
	"gokin/internal/config"

	"google.golang.org/genai"
)

// DefaultModelLimits provides default token limits for known models.
// Keys are used both for exact match and as substrings for fuzzy matching.
var DefaultModelLimits = map[string]TokenLimits{
	"gemini-2.0-flash": {
		MaxInputTokens:  1048576, // 1M tokens
		MaxOutputTokens: 8192,
	},
	"gemini-2.5-flash": {
		MaxInputTokens:  1048576, // 1M tokens
		MaxOutputTokens: 8192,
	},
	"gemini-2.5-pro": {
		MaxInputTokens:  1048576, // 1M tokens
		MaxOutputTokens: 8192,
	},
	"gemini-3-flash": {
		MaxInputTokens:  1048576, // 1M tokens
		MaxOutputTokens: 65536,
	},
	"gemini-3-pro": {
		MaxInputTokens:  1048576, // 1M tokens
		MaxOutputTokens: 65536,
	},
	"glm-4.7": {
		MaxInputTokens:  128000,
		MaxOutputTokens: 131072,
	},
}

// ModelPricing defines the cost per 1M tokens in USD.
type ModelPricing struct {
	InputCostPer1M  float64
	OutputCostPer1M float64
}

// DefaultPricing provides cost estimation for known models.
var DefaultPricing = map[string]ModelPricing{
	"gemini-1.5-flash": {InputCostPer1M: 0.075, OutputCostPer1M: 0.30},
	"gemini-1.5-pro":   {InputCostPer1M: 3.50, OutputCostPer1M: 10.50},
	"gemini-2.0-flash": {InputCostPer1M: 0.10, OutputCostPer1M: 0.40},
	"gemini-flash":     {InputCostPer1M: 0.10, OutputCostPer1M: 0.40},
	"gemini-pro":       {InputCostPer1M: 3.50, OutputCostPer1M: 10.50},
	"gemini-3-flash":   {InputCostPer1M: 0.50, OutputCostPer1M: 3.00},
	"gemini-3-pro":     {InputCostPer1M: 2.00, OutputCostPer1M: 12.00},
	"glm-4":            {InputCostPer1M: 1.00, OutputCostPer1M: 1.00}, // Placeholder
}

// TokenLimits defines token limits for a model.
type TokenLimits struct {
	MaxInputTokens   int
	MaxOutputTokens  int
	WarningThreshold float64 // 0.8 = 80%
}

// TokenUsage represents current token usage statistics.
type TokenUsage struct {
	InputTokens  int
	MaxTokens    int
	PercentUsed  float64
	NearLimit    bool
	ExceedsLimit bool
	IsEstimate   bool // True when token count is an estimate (API call failed)
}

// cacheEntry holds a cached token count with its key.
type cacheEntry struct {
	key    string
	tokens int
}

// TokenCounter handles token counting for context management.
type TokenCounter struct {
	client   client.Client
	model    string
	limits   TokenLimits
	mu       sync.RWMutex
	cache    map[string]*list.Element // content hash -> list element
	lruList  *list.List               // LRU list (front = most recent)
	maxCache int
}

// NewTokenCounter creates a new token counter.
func NewTokenCounter(c client.Client, model string, cfg *config.ContextConfig) *TokenCounter {
	limits := getModelLimits(model)

	// Apply config overrides
	if cfg != nil {
		if cfg.MaxInputTokens > 0 {
			limits.MaxInputTokens = cfg.MaxInputTokens
		}
		if cfg.WarningThreshold > 0 {
			limits.WarningThreshold = cfg.WarningThreshold
		}
	}

	// Default warning threshold if not set
	if limits.WarningThreshold == 0 {
		limits.WarningThreshold = 0.8
	}

	return &TokenCounter{
		client:   c,
		model:    model,
		limits:   limits,
		cache:    make(map[string]*list.Element),
		lruList:  list.New(),
		maxCache: 1000,
	}
}

// SetClient updates the underlying client.
func (t *TokenCounter) SetClient(c client.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.client = c
	// Also update model limits as model might have changed
	t.model = c.GetModel()
	t.limits = getModelLimits(t.model)
}

// getModelLimits returns limits for a model, with fallback defaults.
// Uses exact match first, then fuzzy matching by checking if the model
// name contains a known base name (e.g. "gemini-2.5-flash-preview" matches "gemini-2.5-flash").
func getModelLimits(model string) TokenLimits {
	// Exact match first
	if limits, ok := DefaultModelLimits[model]; ok {
		return limits
	}
	// Fuzzy match: check if model name contains a known base name
	modelLower := strings.ToLower(model)
	for key, limits := range DefaultModelLimits {
		if strings.Contains(modelLower, key) {
			return limits
		}
	}
	// Default limits for unknown models
	return TokenLimits{
		MaxInputTokens:   128000,
		MaxOutputTokens:  8192,
		WarningThreshold: 0.8,
	}
}

// CountContents counts tokens for a list of contents using the API.
func (t *TokenCounter) CountContents(ctx context.Context, contents []*genai.Content) (int, error) {
	// Try cache first
	hash := t.hashContents(contents)
	if count, ok := t.getFromCache(hash); ok {
		return count, nil
	}

	// Count via API
	resp, err := t.client.CountTokens(ctx, contents)
	if err != nil {
		return 0, err
	}

	count := int(resp.TotalTokens)

	// Cache the result
	t.addToCache(hash, count)

	return count, nil
}

// getFromCache retrieves a value from cache and moves it to front (LRU).
func (t *TokenCounter) getFromCache(key string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if elem, ok := t.cache[key]; ok {
		// Move to front (most recently used)
		t.lruList.MoveToFront(elem)
		return elem.Value.(*cacheEntry).tokens, true
	}
	return 0, false
}

// addToCache adds a value to cache with LRU eviction.
func (t *TokenCounter) addToCache(key string, tokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Check if already in cache
	if elem, ok := t.cache[key]; ok {
		t.lruList.MoveToFront(elem)
		elem.Value.(*cacheEntry).tokens = tokens
		return
	}

	// Evict oldest if at capacity
	if t.lruList.Len() >= t.maxCache {
		oldest := t.lruList.Back()
		if oldest != nil {
			delete(t.cache, oldest.Value.(*cacheEntry).key)
			t.lruList.Remove(oldest)
		}
	}

	// Add new entry
	entry := &cacheEntry{key: key, tokens: tokens}
	elem := t.lruList.PushFront(entry)
	t.cache[key] = elem
}

// GetUsage returns current token usage statistics.
func (t *TokenCounter) GetUsage(tokenCount int) TokenUsage {
	percentUsed := float64(tokenCount) / float64(t.limits.MaxInputTokens)

	return TokenUsage{
		InputTokens:  tokenCount,
		MaxTokens:    t.limits.MaxInputTokens,
		PercentUsed:  percentUsed,
		NearLimit:    percentUsed >= t.limits.WarningThreshold,
		ExceedsLimit: tokenCount >= t.limits.MaxInputTokens,
	}
}

// CalculateCost estimates the USD cost for the given token usage.
func (t *TokenCounter) CalculateCost(inputTokens, outputTokens int) float64 {
	pricing := getPricing(t.model)
	inputCost := (float64(inputTokens) / 1000000.0) * pricing.InputCostPer1M
	outputCost := (float64(outputTokens) / 1000000.0) * pricing.OutputCostPer1M
	return inputCost + outputCost
}

// getPricing returns pricing for a model, with fallback defaults.
func getPricing(model string) ModelPricing {
	modelLower := strings.ToLower(model)
	for key, pricing := range DefaultPricing {
		if strings.Contains(modelLower, key) {
			return pricing
		}
	}
	// Default to Flash-like pricing for unknown models
	return DefaultPricing["gemini-1.5-flash"]
}

// FormatCost returns a human-readable string for USD cost.
func FormatCost(cost float64) string {
	if cost == 0 {
		return "$0.00"
	}
	if cost < 0.0001 {
		return "< $0.0001"
	}
	return fmt.Sprintf("$%.4f", cost)
}

// GetLimits returns the current token limits.
func (t *TokenCounter) GetLimits() TokenLimits {
	return t.limits
}

// InvalidateCache clears all cached token counts.
// Should be called when history changes to force recalculation.
func (t *TokenCounter) InvalidateCache() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Clear all cache entries
	t.cache = make(map[string]*list.Element)
	t.lruList.Init()
}

// hashContents creates a hash of contents for caching.
func (t *TokenCounter) hashContents(contents []*genai.Content) string {
	h := sha256.New()
	for _, content := range contents {
		h.Write([]byte(content.Role))
		for _, part := range content.Parts {
			if part.Text != "" {
				h.Write([]byte(part.Text))
			}
			if part.FunctionCall != nil {
				h.Write([]byte(part.FunctionCall.Name))
				if argsJSON, err := json.Marshal(part.FunctionCall.Args); err == nil {
					h.Write(argsJSON)
				}
			}
			if part.FunctionResponse != nil {
				h.Write([]byte(part.FunctionResponse.Name))
				if respJSON, err := json.Marshal(part.FunctionResponse.Response); err == nil {
					h.Write(respJSON)
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentType represents the type of content for token estimation.
type ContentType int

const (
	ContentTypeProse ContentType = iota
	ContentTypeCode
	ContentTypeJSON
	ContentTypeMixed
)

// EstimateTokens provides a rough estimate without API call.
// Uses a weighted combination of word-based and character-based estimation
// for better accuracy across different content types (prose, code, mixed).
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	// Detect content type and use appropriate estimation
	contentType := detectContentType(text)
	return estimateTokensForType(text, contentType)
}

// estimateTokensForType estimates tokens based on detected content type.
func estimateTokensForType(text string, contentType ContentType) int {
	chars := len(text)

	switch contentType {
	case ContentTypeCode:
		// Code has more tokens per character due to:
		// - camelCase and snake_case identifiers (split into multiple tokens)
		// - Short keywords (func, if, for, etc.)
		// - Operators and punctuation
		// Average: ~0.25 chars/token = 4 tokens/char... wait, that's inverted
		// Actually: ~3.2 chars per token for code
		return int(float64(chars) / 3.2)

	case ContentTypeJSON:
		// JSON is even more token-dense due to:
		// - Quotes, colons, commas, braces
		// - Keys are often tokenized separately
		// Average: ~3 chars per token
		return int(float64(chars) / 3.0)

	case ContentTypeProse:
		// Natural language text
		// Word-based estimation is better for prose
		words := len(strings.Fields(text))
		byWords := int(float64(words) * 1.3)

		// Character-based as fallback
		byChars := chars / 4

		// Weighted average favoring word-based
		return (byWords*3 + byChars) / 4

	default: // ContentTypeMixed
		// Use combined heuristic
		words := len(strings.Fields(text))
		byWords := int(float64(words) * 1.3)
		byChars := int(float64(chars) / 3.5) // Between code and prose

		return (byWords + byChars) / 2
	}
}

// detectContentType analyzes text to determine its type.
func detectContentType(text string) ContentType {
	if len(text) == 0 {
		return ContentTypeProse
	}

	// Quick heuristics based on content
	trimmed := strings.TrimSpace(text)

	// JSON detection
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		return ContentTypeJSON
	}

	// Count code indicators
	codeIndicators := 0
	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	for _, line := range lines {
		trimLine := strings.TrimSpace(line)

		// Go/C-style code indicators
		if strings.HasPrefix(trimLine, "func ") ||
			strings.HasPrefix(trimLine, "type ") ||
			strings.HasPrefix(trimLine, "package ") ||
			strings.HasPrefix(trimLine, "import ") ||
			strings.HasPrefix(trimLine, "//") ||
			strings.HasPrefix(trimLine, "/*") ||
			strings.HasPrefix(trimLine, "*/") ||
			strings.HasSuffix(trimLine, "{") ||
			strings.HasSuffix(trimLine, "}") ||
			strings.HasSuffix(trimLine, ";") ||
			strings.Contains(trimLine, " := ") ||
			strings.Contains(trimLine, "if err != nil") ||
			strings.Contains(trimLine, "return ") {
			codeIndicators++
		}

		// Python/JS/Other code indicators
		if strings.HasPrefix(trimLine, "def ") ||
			strings.HasPrefix(trimLine, "class ") ||
			strings.HasPrefix(trimLine, "function ") ||
			strings.HasPrefix(trimLine, "const ") ||
			strings.HasPrefix(trimLine, "let ") ||
			strings.HasPrefix(trimLine, "var ") ||
			strings.HasPrefix(trimLine, "#") ||
			strings.Contains(trimLine, " = ") {
			codeIndicators++
		}
	}

	// If more than 30% of lines look like code, treat as code
	if totalLines > 0 && float64(codeIndicators)/float64(totalLines) > 0.3 {
		return ContentTypeCode
	}

	// Check for camelCase/snake_case density (indicates code even without structure)
	camelCaseCount := 0
	words := strings.Fields(text)
	for _, word := range words {
		if containsCamelCase(word) || strings.Contains(word, "_") {
			camelCaseCount++
		}
	}

	if len(words) > 0 && float64(camelCaseCount)/float64(len(words)) > 0.2 {
		return ContentTypeMixed
	}

	return ContentTypeProse
}

// containsCamelCase checks if a word contains camelCase pattern.
func containsCamelCase(word string) bool {
	hasLower := false
	hasUpper := false
	for _, r := range word {
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
		// Transition from lower to upper indicates camelCase
		if hasLower && hasUpper {
			return true
		}
	}
	return false
}

// EstimateTokensWithType estimates tokens with explicit content type.
func EstimateTokensWithType(text string, contentType ContentType) int {
	if text == "" {
		return 0
	}
	return estimateTokensForType(text, contentType)
}

// EstimateContentsTokens estimates tokens for contents without API call.
func EstimateContentsTokens(contents []*genai.Content) int {
	total := 0
	for _, content := range contents {
		// Role overhead
		total += 4
		for _, part := range content.Parts {
			if part.Text != "" {
				total += EstimateTokens(part.Text)
			}
			if part.FunctionCall != nil {
				total += 10 + EstimateTokens(part.FunctionCall.Name)
				// Estimate args
				for k, v := range part.FunctionCall.Args {
					total += EstimateTokens(k)
					if str, ok := v.(string); ok {
						total += EstimateTokens(str)
					} else {
						total += 10 // estimate for non-string args
					}
				}
			}
			if part.FunctionResponse != nil {
				total += 10 + EstimateTokens(part.FunctionResponse.Name)
				// Estimate response map - Response is already map[string]any
				for k, v := range part.FunctionResponse.Response {
					total += EstimateTokens(k)
					if str, ok := v.(string); ok {
						total += EstimateTokens(str)
					} else {
						total += 10
					}
				}
			}
		}
	}
	return total
}
