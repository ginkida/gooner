package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gokin/internal/logging"
)

// StrategyMetrics tracks performance metrics for a strategy.
type StrategyMetrics struct {
	StrategyName string        `json:"strategy_name"`
	SuccessCount int           `json:"success_count"`
	FailureCount int           `json:"failure_count"`
	TotalTime    time.Duration `json:"total_time"`
	AvgDuration  time.Duration `json:"avg_duration"`
	LastUsed     time.Time     `json:"last_used"`
	TaskTypes    map[string]int `json:"task_types"` // TaskType -> count
}

// SuccessRate returns the success rate as a percentage.
func (sm *StrategyMetrics) SuccessRate() float64 {
	total := sm.SuccessCount + sm.FailureCount
	if total == 0 {
		return 0.5 // Unknown, return neutral
	}
	return float64(sm.SuccessCount) / float64(total)
}

// StrategyOptimizer analyzes and optimizes agent strategies based on outcomes.
type StrategyOptimizer struct {
	metrics   map[string]*StrategyMetrics // strategy name -> metrics
	configDir string
	mu        sync.RWMutex
}

// NewStrategyOptimizer creates a new strategy optimizer.
func NewStrategyOptimizer(configDir string) *StrategyOptimizer {
	so := &StrategyOptimizer{
		metrics:   make(map[string]*StrategyMetrics),
		configDir: configDir,
	}

	// Load existing metrics
	if err := so.load(); err != nil {
		logging.Debug("failed to load strategy metrics", "error", err)
	}

	return so
}

// storagePath returns the path to the metrics file.
func (so *StrategyOptimizer) storagePath() string {
	return filepath.Join(so.configDir, "memory", "strategy_metrics.json")
}

// load loads metrics from disk.
func (so *StrategyOptimizer) load() error {
	data, err := os.ReadFile(so.storagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var metrics map[string]*StrategyMetrics
	if err := json.Unmarshal(data, &metrics); err != nil {
		return err
	}

	so.metrics = metrics
	return nil
}

// save persists metrics to disk.
func (so *StrategyOptimizer) save() error {
	dir := filepath.Dir(so.storagePath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(so.metrics, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(so.storagePath(), data, 0644)
}

// RecordExecution records the outcome of a strategy execution.
func (so *StrategyOptimizer) RecordExecution(strategyName string, taskType string, success bool, duration time.Duration) {
	so.mu.Lock()
	defer so.mu.Unlock()

	metrics, ok := so.metrics[strategyName]
	if !ok {
		metrics = &StrategyMetrics{
			StrategyName: strategyName,
			TaskTypes:    make(map[string]int),
		}
		so.metrics[strategyName] = metrics
	}

	if success {
		metrics.SuccessCount++
	} else {
		metrics.FailureCount++
	}

	metrics.TotalTime += duration
	total := metrics.SuccessCount + metrics.FailureCount
	metrics.AvgDuration = metrics.TotalTime / time.Duration(total)
	metrics.LastUsed = time.Now()
	metrics.TaskTypes[taskType]++

	// Save asynchronously
	go func() {
		if err := so.save(); err != nil {
			logging.Debug("failed to save strategy metrics", "error", err)
		}
	}()
}

// GetSuccessRate returns the success rate for a strategy.
func (so *StrategyOptimizer) GetSuccessRate(strategyName string) float64 {
	so.mu.RLock()
	defer so.mu.RUnlock()

	metrics, ok := so.metrics[strategyName]
	if !ok {
		return 0.5 // Unknown strategy, return neutral
	}

	return metrics.SuccessRate()
}

// GetMetrics returns the metrics for a strategy.
func (so *StrategyOptimizer) GetMetrics(strategyName string) (*StrategyMetrics, bool) {
	so.mu.RLock()
	defer so.mu.RUnlock()

	metrics, ok := so.metrics[strategyName]
	return metrics, ok
}

// RecommendStrategy recommends the best strategy for a task type.
func (so *StrategyOptimizer) RecommendStrategy(taskType string) string {
	so.mu.RLock()
	defer so.mu.RUnlock()

	type strategyScore struct {
		name  string
		score float64
	}

	var scores []strategyScore

	for name, metrics := range so.metrics {
		// Calculate a score based on:
		// 1. Success rate (most important)
		// 2. Experience with this task type
		// 3. Recency of use

		baseScore := metrics.SuccessRate()

		// Boost score if this strategy has been used for this task type
		taskTypeCount := metrics.TaskTypes[taskType]
		if taskTypeCount > 0 {
			// More experience = higher confidence in the score
			experienceBoost := float64(taskTypeCount) / float64(metrics.SuccessCount+metrics.FailureCount)
			baseScore += experienceBoost * 0.2 // Up to 20% boost
		}

		// Small penalty for strategies not used recently
		daysSinceUse := time.Since(metrics.LastUsed).Hours() / 24
		if daysSinceUse > 30 {
			baseScore *= 0.9 // 10% penalty for old strategies
		}

		scores = append(scores, strategyScore{name: name, score: baseScore})
	}

	if len(scores) == 0 {
		return "general" // Default fallback
	}

	// Sort by score (highest first)
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	return scores[0].name
}

// GetAllMetrics returns all strategy metrics.
func (so *StrategyOptimizer) GetAllMetrics() map[string]*StrategyMetrics {
	so.mu.RLock()
	defer so.mu.RUnlock()

	// Return a copy to avoid data races
	copy := make(map[string]*StrategyMetrics)
	for k, v := range so.metrics {
		copy[k] = v
	}
	return copy
}

// GetTopStrategies returns the top N strategies by success rate.
func (so *StrategyOptimizer) GetTopStrategies(n int) []*StrategyMetrics {
	so.mu.RLock()
	defer so.mu.RUnlock()

	metrics := make([]*StrategyMetrics, 0, len(so.metrics))
	for _, m := range so.metrics {
		metrics = append(metrics, m)
	}

	sort.Slice(metrics, func(i, j int) bool {
		return metrics[i].SuccessRate() > metrics[j].SuccessRate()
	})

	if n > len(metrics) {
		n = len(metrics)
	}

	return metrics[:n]
}

// Clear removes all metrics.
func (so *StrategyOptimizer) Clear() error {
	so.mu.Lock()
	defer so.mu.Unlock()

	so.metrics = make(map[string]*StrategyMetrics)
	return so.save()
}
