package context

import (
	"context"
	"sync"
	"time"

	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/logging"

	"google.golang.org/genai"
)

// ContextManager orchestrates context management including token counting,
// auto-summarization, and optimization.
type ContextManager struct {
	session      *chat.Session
	tokenCounter *TokenCounter
	summarizer   *Summarizer
	config       *config.ContextConfig
	client       client.Client

	mu            sync.RWMutex
	currentTokens int
	lastUsage     *TokenUsage
	updateVersion uint64 // Monotonically increasing version to prevent stale watcher updates

	// New components
	metrics            *ContextMetrics
	summaryCache       *SummaryCache
	messageScorer      *MessageScorer
	summaryStrategy    SummaryStrategy
	responseCompressor *ResponseCompressor
}

// NewContextManager creates a new context manager.
func NewContextManager(
	session *chat.Session,
	c client.Client,
	cfg *config.ContextConfig,
) *ContextManager {
	tokenCounter := NewTokenCounter(c, c.GetModel(), cfg)

	var summarizer *Summarizer
	if cfg != nil && cfg.EnableAutoSummary {
		summarizer = NewSummarizer(c)
	}

	// Initialize new components
	metrics := NewContextMetrics()
	summaryCache := NewSummaryCache(100, 30*time.Minute)
	messageScorer := NewMessageScorer()
	summaryStrategy := DefaultSummaryStrategy()

	// Configure response compressor
	maxToolChars := 10000
	if cfg != nil && cfg.ToolResultMaxChars > 0 {
		maxToolChars = cfg.ToolResultMaxChars
	}
	responseCompressor := NewResponseCompressor(maxToolChars)

	// Customize strategy from config
	if cfg != nil {
		if cfg.SummarizationRatio > 0 {
			summaryStrategy.TargetRatio = cfg.SummarizationRatio
		}
		if cfg.MaxInputTokens > 0 {
			summaryStrategy.MaxHistorySize = cfg.MaxInputTokens / 2000 // Rough estimate
		}
	}

	return &ContextManager{
		session:            session,
		tokenCounter:       tokenCounter,
		summarizer:         summarizer,
		config:             cfg,
		client:             c,
		metrics:            metrics,
		summaryCache:       summaryCache,
		messageScorer:      messageScorer,
		summaryStrategy:    summaryStrategy,
		responseCompressor: responseCompressor,
	}
}

// SetClient updates the underlying client for token counting and summarization.
func (m *ContextManager) SetClient(c client.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.client = c
	m.tokenCounter.SetClient(c)
	if m.summarizer != nil {
		m.summarizer.SetClient(c)
	}
}

// SetConfig updates the context configuration.
func (m *ContextManager) SetConfig(cfg *config.ContextConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg
	if m.summarizer != nil {
		m.summarizer.SetConfig(cfg)
	}
}

// StartSessionWatcher starts monitoring session changes for auto-updating token counts.
func (m *ContextManager) StartSessionWatcher() {
	m.session.SetChangeHandler(m.onSessionChange)
}

// onSessionChange is called when session history changes.
func (m *ContextManager) onSessionChange(event chat.ChangeEvent) {
	// Invalidate token cache when history changes
	// This ensures we recalculate token counts instead of using stale cached values
	m.tokenCounter.InvalidateCache()

	// Update token count asynchronously, but guard against overwriting
	// a more recent update from the main flow (UpdateTokenCount).
	go func() {
		m.mu.RLock()
		versionBefore := m.updateVersion
		m.mu.RUnlock()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		history := m.session.GetHistory()
		tokens, err := m.tokenCounter.CountContents(ctx, history)
		isEstimate := false
		if err != nil {
			tokens = EstimateContentsTokens(history)
			isEstimate = true
			logging.Debug("failed to count tokens on session change, using estimate", "error", err)
		}

		m.mu.Lock()
		// Only apply if no newer update happened while we were counting
		if m.updateVersion == versionBefore {
			m.currentTokens = tokens
			usage := m.tokenCounter.GetUsage(tokens)
			usage.IsEstimate = isEstimate
			m.lastUsage = &usage
		}
		m.mu.Unlock()
	}()
}

// PrepareForRequest prepares the context before sending a request.
// It counts tokens and triggers optimization if needed.
func (m *ContextManager) PrepareForRequest(ctx context.Context) error {
	startTime := time.Now()

	history := m.session.GetHistory()

	// Compress large function responses
	history = m.responseCompressor.CompressContents(history)

	// Count tokens
	tokens, err := m.tokenCounter.CountContents(ctx, history)
	isEstimate := false
	if err != nil {
		// Fall back to estimation if API call fails
		tokens = EstimateContentsTokens(history)
		isEstimate = true
		m.metrics.RecordEstimation()
	} else {
		m.metrics.RecordAPICount()
	}

	m.mu.Lock()
	m.updateVersion++ // Invalidate any in-flight watcher updates
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	usage.IsEstimate = isEstimate
	m.lastUsage = &usage
	m.mu.Unlock()

	// Record metrics
	m.metrics.RecordPrepare(time.Since(startTime), tokens)

	// Optimize if near limit
	if usage.NearLimit && m.summarizer != nil && m.config.EnableAutoSummary {
		if err := m.OptimizeContext(ctx); err != nil {
			// Log error but don't fail the request
			// The request might still succeed with the current context
			logging.Warn("context optimization failed", "error", err)
		}
	}

	return nil
}

// OptimizeContext optimizes the context by summarizing old messages.
func (m *ContextManager) OptimizeContext(ctx context.Context) error {
	startTime := time.Now()

	history := m.session.GetHistory()
	if len(history) <= m.summaryStrategy.MinMessagesForSummary {
		// Not enough messages to summarize
		return nil
	}

	// Create summary plan using strategy
	plan := CreateSummaryPlan(history, m.summaryStrategy, m.messageScorer)

	if len(plan.ToSummarize) == 0 {
		// Nothing to summarize
		return nil
	}

	// Check cache first
	messageHash := HashMessages(plan.ToSummarize)
	cachedSummary, found := m.summaryCache.Get(messageHash)

	var summary *genai.Content
	var fromCache bool

	if found {
		// Use cached summary
		summary = cachedSummary.Summary
		fromCache = true
		logging.Debug("using cached summary", "tokens", cachedSummary.TokenCount)
	} else {
		// Generate new summary
		startSummary := time.Now()
		var err error
		summary, err = m.summarizer.Summarize(ctx, plan.ToSummarize)
		if err != nil {
			return err
		}

		// Count summary tokens
		summaryTokens, err := m.tokenCounter.CountContents(ctx, []*genai.Content{summary})
		if err != nil {
			summaryTokens = EstimateContentsTokens([]*genai.Content{summary})
		}

		// Cache the summary
		originalTokens := EstimateContentsTokens(plan.ToSummarize)
		m.summaryCache.Put(messageHash, summary, summaryTokens, 0, 0)

		// Record metrics
		m.metrics.RecordSummary(time.Since(startSummary), summaryTokens, originalTokens, false)
		fromCache = false
	}

	// Apply the summary plan
	newHistory := ApplySummaryPlan(plan, summary)
	m.session.SetHistory(newHistory)

	// Recount tokens
	tokens, err := m.tokenCounter.CountContents(ctx, newHistory)
	if err != nil {
		tokens = EstimateContentsTokens(newHistory)
	}

	m.mu.Lock()
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	m.lastUsage = &usage
	m.mu.Unlock()

	// Record optimization metrics
	oldTokens := EstimateContentsTokens(history)
	m.metrics.RecordOptimize(time.Since(startTime), oldTokens, tokens)

	logging.Info("context optimized",
		"before", oldTokens,
		"after", tokens,
		"saved", oldTokens-tokens,
		"from_cache", fromCache)

	return nil
}

// GetTokenUsage returns the current token usage.
func (m *ContextManager) GetTokenUsage() *TokenUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.lastUsage == nil {
		return &TokenUsage{}
	}
	return m.lastUsage
}

// GetCurrentTokens returns the current token count.
func (m *ContextManager) GetCurrentTokens() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentTokens
}

// UpdateTokenCount updates the token count after a request.
func (m *ContextManager) UpdateTokenCount(ctx context.Context) error {
	history := m.session.GetHistory()

	tokens, err := m.tokenCounter.CountContents(ctx, history)
	isEstimate := false
	if err != nil {
		tokens = EstimateContentsTokens(history)
		isEstimate = true
	}

	m.mu.Lock()
	m.updateVersion++ // Invalidate any in-flight watcher updates
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	usage.IsEstimate = isEstimate
	m.lastUsage = &usage
	m.mu.Unlock()

	return nil
}

// NeedsSummarization checks if the context needs summarization.
func (m *ContextManager) NeedsSummarization() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.lastUsage == nil {
		return false
	}
	return m.lastUsage.NearLimit
}

// GetTokenCounter returns the underlying token counter.
func (m *ContextManager) GetTokenCounter() *TokenCounter {
	return m.tokenCounter
}

// GetMetrics returns the context metrics.
func (m *ContextManager) GetMetrics() *ContextMetrics {
	return m.metrics
}

// GetCacheStats returns summary cache statistics.
func (m *ContextManager) GetCacheStats() CacheStats {
	return m.summaryCache.GetStats()
}

// SetSummaryStrategy sets a new summarization strategy.
func (m *ContextManager) SetSummaryStrategy(strategy SummaryStrategy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaryStrategy = strategy
}

// GetSummaryStrategy returns the current summarization strategy.
func (m *ContextManager) GetSummaryStrategy() SummaryStrategy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.summaryStrategy
}

// ForceSummarize forces context summarization regardless of token count.
func (m *ContextManager) ForceSummarize(ctx context.Context) error {
	if m.summarizer == nil {
		return nil
	}
	return m.OptimizeContext(ctx)
}
