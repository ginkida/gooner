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

// PromptVariant represents a variation of a prompt with its performance metrics.
type PromptVariant struct {
	ID          string        `json:"id"`
	BasePrompt  string        `json:"base_prompt"`  // Category/template (e.g., "explore", "implement")
	Variation   string        `json:"variation"`    // The actual prompt text
	SuccessRate float64       `json:"success_rate"` // 0-1
	AvgTokens   int           `json:"avg_tokens"`
	AvgDuration time.Duration `json:"avg_duration"`
	UseCount    int           `json:"use_count"`
	SuccessCount int          `json:"success_count"`
	FailureCount int          `json:"failure_count"`
	LastUsed    time.Time     `json:"last_used"`
	Created     time.Time     `json:"created"`
}

// Score calculates a combined score for ranking variants.
func (pv *PromptVariant) Score() float64 {
	// Higher success rate = better
	// Lower tokens = better (cost efficiency)
	// Lower duration = better
	// More uses = higher confidence

	baseScore := pv.SuccessRate

	// Confidence boost based on use count (max 20% boost at 100+ uses)
	confidence := float64(pv.UseCount) / 100.0
	if confidence > 0.2 {
		confidence = 0.2
	}

	return baseScore + confidence
}

// PromptOptimizer tracks and optimizes prompt variants.
type PromptOptimizer struct {
	configDir string
	variants  map[string]*PromptVariant // variant ID -> variant
	byBase    map[string][]string       // base prompt -> list of variant IDs
	mu        sync.RWMutex
}

// NewPromptOptimizer creates a new prompt optimizer.
func NewPromptOptimizer(configDir string) *PromptOptimizer {
	po := &PromptOptimizer{
		configDir: configDir,
		variants:  make(map[string]*PromptVariant),
		byBase:    make(map[string][]string),
	}

	if err := po.load(); err != nil {
		logging.Debug("failed to load prompt optimizer", "error", err)
	}

	return po
}

// storagePath returns the path to the variants file.
func (po *PromptOptimizer) storagePath() string {
	return filepath.Join(po.configDir, "memory", "prompt_variants.json")
}

// load loads variants from disk.
func (po *PromptOptimizer) load() error {
	data, err := os.ReadFile(po.storagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var variants map[string]*PromptVariant
	if err := json.Unmarshal(data, &variants); err != nil {
		return err
	}

	po.variants = variants

	// Rebuild base index
	po.byBase = make(map[string][]string)
	for id, v := range po.variants {
		po.byBase[v.BasePrompt] = append(po.byBase[v.BasePrompt], id)
	}

	return nil
}

// save persists variants to disk.
func (po *PromptOptimizer) save() error {
	dir := filepath.Dir(po.storagePath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(po.variants, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(po.storagePath(), data, 0644)
}

// generateVariantID creates a unique ID for a variant.
func generateVariantID() string {
	return time.Now().Format("20060102150405.000")
}

// RecordExecution records the outcome of a prompt execution.
func (po *PromptOptimizer) RecordExecution(basePrompt, variation string, success bool, tokens int, duration time.Duration) {
	po.mu.Lock()
	defer po.mu.Unlock()

	// Find or create variant
	var variant *PromptVariant
	for _, v := range po.variants {
		if v.BasePrompt == basePrompt && v.Variation == variation {
			variant = v
			break
		}
	}

	if variant == nil {
		variant = &PromptVariant{
			ID:         generateVariantID(),
			BasePrompt: basePrompt,
			Variation:  variation,
			Created:    time.Now(),
		}
		po.variants[variant.ID] = variant
		po.byBase[basePrompt] = append(po.byBase[basePrompt], variant.ID)
	}

	// Update metrics
	variant.UseCount++
	variant.LastUsed = time.Now()

	if success {
		variant.SuccessCount++
	} else {
		variant.FailureCount++
	}

	total := variant.SuccessCount + variant.FailureCount
	variant.SuccessRate = float64(variant.SuccessCount) / float64(total)

	// Update averages
	if tokens > 0 {
		variant.AvgTokens = (variant.AvgTokens*(variant.UseCount-1) + tokens) / variant.UseCount
	}
	if duration > 0 {
		variant.AvgDuration = (variant.AvgDuration*time.Duration(variant.UseCount-1) + duration) / time.Duration(variant.UseCount)
	}

	// Save asynchronously
	go func() {
		if err := po.save(); err != nil {
			logging.Debug("failed to save prompt optimizer", "error", err)
		}
	}()
}

// GetBestVariant returns the best performing variant for a base prompt.
func (po *PromptOptimizer) GetBestVariant(basePrompt string) (*PromptVariant, bool) {
	po.mu.RLock()
	defer po.mu.RUnlock()

	ids, ok := po.byBase[basePrompt]
	if !ok || len(ids) == 0 {
		return nil, false
	}

	var best *PromptVariant
	bestScore := -1.0

	for _, id := range ids {
		v := po.variants[id]
		if v == nil {
			continue
		}
		score := v.Score()
		if score > bestScore {
			bestScore = score
			best = v
		}
	}

	return best, best != nil
}

// OptimizePrompt returns the best prompt variation for a given base prompt.
// If no good variation exists, returns the original prompt.
func (po *PromptOptimizer) OptimizePrompt(prompt, taskType string) string {
	best, ok := po.GetBestVariant(taskType)
	if !ok {
		return prompt
	}

	// Only use the variant if it has good confidence
	if best.UseCount < 3 {
		return prompt // Not enough data
	}

	if best.SuccessRate < 0.5 {
		return prompt // Not better than random
	}

	return best.Variation
}

// GetVariantsByBase returns all variants for a base prompt, sorted by score.
func (po *PromptOptimizer) GetVariantsByBase(basePrompt string) []*PromptVariant {
	po.mu.RLock()
	defer po.mu.RUnlock()

	ids, ok := po.byBase[basePrompt]
	if !ok {
		return nil
	}

	variants := make([]*PromptVariant, 0, len(ids))
	for _, id := range ids {
		if v := po.variants[id]; v != nil {
			variants = append(variants, v)
		}
	}

	sort.Slice(variants, func(i, j int) bool {
		return variants[i].Score() > variants[j].Score()
	})

	return variants
}

// GetTopVariants returns the top N variants across all base prompts.
func (po *PromptOptimizer) GetTopVariants(n int) []*PromptVariant {
	po.mu.RLock()
	defer po.mu.RUnlock()

	variants := make([]*PromptVariant, 0, len(po.variants))
	for _, v := range po.variants {
		variants = append(variants, v)
	}

	sort.Slice(variants, func(i, j int) bool {
		return variants[i].Score() > variants[j].Score()
	})

	if n > len(variants) {
		n = len(variants)
	}

	return variants[:n]
}

// GetStats returns statistics about the prompt optimizer.
func (po *PromptOptimizer) GetStats() PromptOptimizerStats {
	po.mu.RLock()
	defer po.mu.RUnlock()

	stats := PromptOptimizerStats{
		TotalVariants: len(po.variants),
		BasePrompts:   len(po.byBase),
		ByBase:        make(map[string]int),
	}

	for base, ids := range po.byBase {
		stats.ByBase[base] = len(ids)
	}

	return stats
}

// PromptOptimizerStats contains statistics about the prompt optimizer.
type PromptOptimizerStats struct {
	TotalVariants int
	BasePrompts   int
	ByBase        map[string]int
}

// Clear removes all variants.
func (po *PromptOptimizer) Clear() error {
	po.mu.Lock()
	defer po.mu.Unlock()

	po.variants = make(map[string]*PromptVariant)
	po.byBase = make(map[string][]string)
	return po.save()
}
