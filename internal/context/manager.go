package context

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gokin/internal/chat"
	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/logging"

	"google.golang.org/genai"
)

// tokenSnapshot records token usage at a point in time for trend prediction.
type tokenSnapshot struct {
	tokens    int
	timestamp time.Time
}

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
	keyFiles           map[string]bool // Files critical to the session, always preserved
	tokenHistory       []tokenSnapshot  // Token usage history for trend prediction
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
		keyFiles:           make(map[string]bool),
		tokenHistory:       make([]tokenSnapshot, 0, 20),
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
	m.tokenCounter.InvalidateCache()

	// Track key files from new messages
	if event.NewCount > event.OldCount {
		m.trackKeyFiles(event)
	}

	// Update token count asynchronously
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Error("panic in session change handler", "error", r)
			}
		}()

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
		if m.updateVersion == versionBefore {
			m.currentTokens = tokens
			usage := m.tokenCounter.GetUsage(tokens)
			usage.IsEstimate = isEstimate
			m.lastUsage = &usage
		}
		m.mu.Unlock()

		// Auto-compact if threshold exceeded
		m.tryAutoCompact(ctx, tokens)
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
	m.tokenHistory = append(m.tokenHistory, tokenSnapshot{tokens: tokens, timestamp: time.Now()})
	// Keep only last 20 snapshots
	if len(m.tokenHistory) > 20 {
		m.tokenHistory = m.tokenHistory[len(m.tokenHistory)-20:]
	}
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

	// Predictive: check if next request will exceed limit based on trend
	if !usage.NearLimit && m.summarizer != nil && m.config.EnableAutoSummary {
		if predicted := m.predictTokensAfterRequest(); predicted > 0 {
			predUsage := m.tokenCounter.GetUsage(predicted)
			if predUsage.PercentUsed > 0.85 {
				logging.Info("predictive summarization triggered",
					"current_tokens", tokens,
					"predicted_tokens", predicted,
					"predicted_pct", predUsage.PercentUsed)
				if err := m.OptimizeContext(ctx); err != nil {
					logging.Warn("predictive summarization failed", "error", err)
				}
			}
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

// predictTokensAfterRequest estimates token count after the next request
// based on the trend of recent token snapshots.
func (m *ContextManager) predictTokensAfterRequest() int {
	m.mu.RLock()
	history := make([]tokenSnapshot, len(m.tokenHistory))
	copy(history, m.tokenHistory)
	m.mu.RUnlock()

	if len(history) < 3 {
		return 0 // Not enough data to predict
	}

	// Calculate average token growth per request
	totalGrowth := 0
	count := 0
	for i := 1; i < len(history); i++ {
		growth := history[i].tokens - history[i-1].tokens
		if growth > 0 {
			totalGrowth += growth
			count++
		}
	}

	if count == 0 {
		return 0
	}

	avgGrowth := totalGrowth / count
	current := history[len(history)-1].tokens

	// Predict: current + average growth for next request
	return current + avgGrowth
}

// ForceSummarize forces context summarization regardless of token count.
func (m *ContextManager) ForceSummarize(ctx context.Context) error {
	if m.summarizer == nil {
		return nil
	}
	return m.OptimizeContext(ctx)
}

// trackKeyFiles extracts file paths from session changes to track critical files.
func (m *ContextManager) trackKeyFiles(event chat.ChangeEvent) {
	history := m.session.GetHistory()
	if len(history) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Scan newly added messages (from OldCount to NewCount)
	start := event.OldCount
	if start < 0 {
		start = 0
	}
	if start >= len(history) {
		return
	}

	for i := start; i < len(history); i++ {
		content := history[i]
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				if path, ok := part.FunctionCall.Args["path"].(string); ok {
					m.keyFiles[path] = true
				}
				if path, ok := part.FunctionCall.Args["file_path"].(string); ok {
					m.keyFiles[path] = true
				}
			}
		}
	}
}

// tryAutoCompact triggers incremental compaction when tokens exceed the configured threshold.
func (m *ContextManager) tryAutoCompact(ctx context.Context, currentTokens int) {
	if m.summarizer == nil || m.config == nil || !m.config.EnableAutoSummary {
		return
	}

	threshold := m.config.AutoCompactThreshold
	if threshold <= 0 {
		threshold = 0.75 // default
	}

	usage := m.tokenCounter.GetUsage(currentTokens)
	if usage.PercentUsed < threshold {
		return
	}

	logging.Info("auto-compaction triggered",
		"tokens", currentTokens,
		"percentage", usage.PercentUsed,
		"threshold", threshold)

	if err := m.IncrementalCompact(ctx); err != nil {
		logging.Warn("auto-compaction failed", "error", err)
	}
}

// IncrementalCompact performs incremental compaction: summarizes oldest messages first,
// preserves recent messages and key file references.
func (m *ContextManager) IncrementalCompact(ctx context.Context) error {
	history := m.session.GetHistory()

	// Preserve last 50 messages in full fidelity
	preserveCount := 50
	if preserveCount > len(history) {
		preserveCount = len(history)
	}

	if len(history) <= preserveCount {
		return nil // Nothing old enough to compact
	}

	// Split: old messages to summarize, recent to preserve
	oldMessages := history[:len(history)-preserveCount]
	recentMessages := history[len(history)-preserveCount:]

	if len(oldMessages) < m.summaryStrategy.MinMessagesForSummary {
		return nil
	}

	// Summarize old messages
	summary, err := m.summarizer.Summarize(ctx, oldMessages)
	if err != nil {
		return fmt.Errorf("incremental compaction failed: %w", err)
	}

	// Build new history: summary + recent
	newHistory := make([]*genai.Content, 0, 1+len(recentMessages))
	newHistory = append(newHistory, summary)
	newHistory = append(newHistory, recentMessages...)

	m.session.SetHistory(newHistory)

	// Update token count
	tokens, err := m.tokenCounter.CountContents(ctx, newHistory)
	if err != nil {
		tokens = EstimateContentsTokens(newHistory)
	}

	m.mu.Lock()
	m.currentTokens = tokens
	usage := m.tokenCounter.GetUsage(tokens)
	m.lastUsage = &usage
	m.mu.Unlock()

	oldTokens := EstimateContentsTokens(history)
	logging.Info("incremental compaction done",
		"before", oldTokens,
		"after", tokens,
		"messages_summarized", len(oldMessages),
		"messages_preserved", len(recentMessages))

	return nil
}

// GetKeyFiles returns the set of files tracked as critical to the session.
func (m *ContextManager) GetKeyFiles() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	files := make([]string, 0, len(m.keyFiles))
	for f := range m.keyFiles {
		files = append(files, f)
	}
	return files
}
