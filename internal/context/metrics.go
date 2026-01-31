package context

import (
	"sync"
	"time"
)

// ContextMetrics tracks efficiency and usage statistics for context management.
type ContextMetrics struct {
	mu sync.RWMutex

	// Request statistics
	TotalRequests     int64
	PreparedRequests  int64
	OptimizedRequests int64

	// Token statistics
	TotalTokensProcessed int64
	TotalTokensSaved     int64
	EstimationHits       int64
	EstimationMisses     int64

	// Summary statistics
	TotalSummaries     int64
	CacheHits          int64
	CacheMisses        int64
	SummaryTokensTotal int64
	SummaryTokensSaved int64

	// Timing statistics
	TotalPrepareTime  time.Duration
	TotalOptimizeTime time.Duration
	TotalSummaryTime  time.Duration

	// Timestamps
	StartTime      time.Time
	LastUpdateTime time.Time
}

// NewContextMetrics creates a new metrics collector.
func NewContextMetrics() *ContextMetrics {
	return &ContextMetrics{
		StartTime:      time.Now(),
		LastUpdateTime: time.Now(),
	}
}

// RecordPrepare records a context preparation event.
func (m *ContextMetrics) RecordPrepare(duration time.Duration, tokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TotalRequests++
	m.PreparedRequests++
	m.TotalTokensProcessed += int64(tokens)
	m.TotalPrepareTime += duration
	m.LastUpdateTime = time.Now()
}

// RecordOptimize records a context optimization event.
func (m *ContextMetrics) RecordOptimize(duration time.Duration, tokensBefore, tokensAfter int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.OptimizedRequests++
	m.TotalTokensSaved += int64(tokensBefore - tokensAfter)
	m.TotalOptimizeTime += duration
	m.LastUpdateTime = time.Now()
}

// RecordSummary records a summary generation event.
func (m *ContextMetrics) RecordSummary(duration time.Duration, summaryTokens, originalTokens int, fromCache bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TotalSummaries++
	m.SummaryTokensTotal += int64(summaryTokens)
	m.SummaryTokensSaved += int64(originalTokens - summaryTokens)
	m.TotalSummaryTime += duration

	if fromCache {
		m.CacheHits++
	} else {
		m.CacheMisses++
	}

	m.LastUpdateTime = time.Now()
}

// RecordEstimation records a token estimation event (fallback from API).
func (m *ContextMetrics) RecordEstimation() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.EstimationMisses++
	m.LastUpdateTime = time.Now()
}

// RecordAPICount records a successful API token count.
func (m *ContextMetrics) RecordAPICount() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.EstimationHits++
	m.LastUpdateTime = time.Now()
}

// GetSummary returns a summary of all metrics.
type MetricsSummary struct {
	Requests              int64
	Optimizations         int64
	Summaries             int64
	TokensProcessed       int64
	TokensSaved           int64
	AveragePrepareTime    time.Duration
	AverageOptimizeTime   time.Duration
	CacheHitRate          float64
	EstimationAccuracy    float64
	TotalTime             time.Duration
	TokensSavedPerSummary int64
}

// GetSummary calculates and returns metrics summary.
func (m *ContextMetrics) GetSummary() MetricsSummary {
	m.mu.RLock()
	defer m.mu.RUnlock()

	summary := MetricsSummary{
		Requests:        m.TotalRequests,
		Optimizations:   m.OptimizedRequests,
		Summaries:       m.TotalSummaries,
		TokensProcessed: m.TotalTokensProcessed,
		TokensSaved:     m.TotalTokensSaved,
		TotalTime:       time.Since(m.StartTime),
	}

	// Calculate averages
	if m.PreparedRequests > 0 {
		summary.AveragePrepareTime = m.TotalPrepareTime / time.Duration(m.PreparedRequests)
	}

	if m.OptimizedRequests > 0 {
		summary.AverageOptimizeTime = m.TotalOptimizeTime / time.Duration(m.OptimizedRequests)
	}

	// Calculate cache hit rate
	totalCacheOps := m.CacheHits + m.CacheMisses
	if totalCacheOps > 0 {
		summary.CacheHitRate = float64(m.CacheHits) / float64(totalCacheOps)
	}

	// Calculate estimation accuracy
	totalEstimations := m.EstimationHits + m.EstimationMisses
	if totalEstimations > 0 {
		summary.EstimationAccuracy = float64(m.EstimationHits) / float64(totalEstimations)
	}

	// Calculate tokens saved per summary
	if m.TotalSummaries > 0 {
		summary.TokensSavedPerSummary = m.SummaryTokensSaved / m.TotalSummaries
	}

	return summary
}

// GetCacheStats returns cache-specific statistics.
func (m *ContextMetrics) GetCacheStats() (hits, misses int64, hitRate float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	total := m.CacheHits + m.CacheMisses
	if total > 0 {
		hitRate = float64(m.CacheHits) / float64(total)
	}

	return m.CacheHits, m.CacheMisses, hitRate
}

// GetTokenStats returns token-related statistics.
func (m *ContextMetrics) GetTokenStats() (processed, saved int64, saveRatio float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.TotalTokensProcessed > 0 {
		saveRatio = float64(m.TotalTokensSaved) / float64(m.TotalTokensProcessed)
	}

	return m.TotalTokensProcessed, m.TotalTokensSaved, saveRatio
}

// Reset clears all metrics.
func (m *ContextMetrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.TotalRequests = 0
	m.PreparedRequests = 0
	m.OptimizedRequests = 0
	m.TotalTokensProcessed = 0
	m.TotalTokensSaved = 0
	m.EstimationHits = 0
	m.EstimationMisses = 0
	m.TotalSummaries = 0
	m.CacheHits = 0
	m.CacheMisses = 0
	m.SummaryTokensTotal = 0
	m.SummaryTokensSaved = 0
	m.TotalPrepareTime = 0
	m.TotalOptimizeTime = 0
	m.TotalSummaryTime = 0
	m.StartTime = time.Now()
	m.LastUpdateTime = time.Now()
}

// GetUptime returns the time since metrics collection started.
func (m *ContextMetrics) GetUptime() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return time.Since(m.StartTime)
}
