package router

import (
	"context"
	"time"

	"gokin/internal/agent"
	"gokin/internal/client"
	"gokin/internal/logging"
	"gokin/internal/tools"

	"google.golang.org/genai"
)

// ExampleStoreInterface defines the interface for example stores.
// This is implemented by memory.ExampleStore to avoid import cycles.
type ExampleStoreInterface interface {
	GetSimilarExamples(prompt string, limit int) []ExampleSummary
	GetExamplesForContext(taskType, prompt string, limit int) string
	LearnFromSuccess(taskType, prompt, agentType, output string, duration time.Duration, tokens int) error
}

// ExampleSummary contains a summary of a task example.
type ExampleSummary struct {
	ID          string
	TaskType    string
	InputPrompt string
	AgentType   string
	Duration    time.Duration
	Score       float64
}

// SmartRouterConfig holds configuration for the smart router.
type SmartRouterConfig struct {
	*RouterConfig

	// Adaptive selection settings
	AdaptiveEnabled bool
	MinDataPoints   int // Minimum executions before using learned data
	ExampleLimit    int // Maximum examples to include in context
}

// DefaultSmartRouterConfig returns the default smart router configuration.
func DefaultSmartRouterConfig() *SmartRouterConfig {
	return &SmartRouterConfig{
		RouterConfig: &RouterConfig{
			Enabled:            true,
			DecomposeThreshold: 4,
			ParallelThreshold:  7,
		},
		AdaptiveEnabled: true,
		MinDataPoints:   5,
		ExampleLimit:    3,
	}
}

// SmartRouter extends Router with adaptive agent selection and few-shot learning.
type SmartRouter struct {
	*Router

	// Strategy optimizer for learning from outcomes
	optimizer *agent.StrategyOptimizer

	// Example store for few-shot learning
	exampleStore ExampleStoreInterface

	// Configuration
	adaptiveEnabled bool
	minDataPoints   int
	exampleLimit    int
}

// NewSmartRouter creates a new smart router with adaptive capabilities.
func NewSmartRouter(cfg *SmartRouterConfig, executor *tools.Executor, agentRunner AgentRunner, c client.Client, workDir string) *SmartRouter {
	if cfg == nil {
		cfg = DefaultSmartRouterConfig()
	}

	baseRouter := NewRouter(cfg.RouterConfig, executor, agentRunner, c, workDir)

	return &SmartRouter{
		Router:          baseRouter,
		adaptiveEnabled: cfg.AdaptiveEnabled,
		minDataPoints:   cfg.MinDataPoints,
		exampleLimit:    cfg.ExampleLimit,
	}
}

// SetStrategyOptimizer sets the strategy optimizer for learning.
func (sr *SmartRouter) SetStrategyOptimizer(optimizer *agent.StrategyOptimizer) {
	sr.optimizer = optimizer
}

// SetExampleStore sets the example store for few-shot learning.
func (sr *SmartRouter) SetExampleStore(store ExampleStoreInterface) {
	sr.exampleStore = store
}

// Route determines the best execution strategy with adaptive learning.
func (sr *SmartRouter) Route(message string) *RoutingDecision {
	// Get base routing decision
	decision := sr.Router.Route(message)

	// Add learned examples if available
	if sr.exampleStore != nil && sr.exampleLimit > 0 {
		examples := sr.exampleStore.GetSimilarExamples(message, sr.exampleLimit)
		if len(examples) > 0 {
			decision.LearnedExamples = make([]LearnedExample, len(examples))
			for i, ex := range examples {
				decision.LearnedExamples[i] = LearnedExample{
					ID:        ex.ID,
					TaskType:  ex.TaskType,
					Prompt:    ex.InputPrompt,
					AgentType: ex.AgentType,
					Score:     ex.Score,
				}
			}

			logging.Debug("found similar examples for routing",
				"count", len(examples),
				"message", message[:min(50, len(message))])
		}
	}

	// Override agent selection if we have enough data
	if decision.Handler == HandlerSubAgent && sr.adaptiveEnabled && sr.optimizer != nil {
		taskType := string(decision.Analysis.Type)
		metrics, hasData := sr.optimizer.GetMetrics(taskType)

		if hasData && totalCount(metrics) >= sr.minDataPoints {
			recommended := sr.optimizer.RecommendStrategy(taskType)
			if recommended != decision.SubAgentType {
				logging.Debug("adaptive override",
					"original", decision.SubAgentType,
					"recommended", recommended,
					"task_type", taskType,
					"data_points", totalCount(metrics))

				decision.SubAgentType = recommended
				decision.Reasoning = decision.Reasoning + " (adaptive: " + recommended + ")"
			}
		}
	}

	return decision
}

// Execute routes the task with learning from outcomes.
func (sr *SmartRouter) Execute(ctx context.Context, history []*genai.Content, message string) ([]*genai.Content, string, error) {
	startTime := time.Now()
	decision := sr.Route(message)

	// Execute using base router
	resultHistory, output, err := sr.Router.Execute(ctx, history, message)
	duration := time.Since(startTime)

	// Learn from the outcome
	success := err == nil && output != ""
	taskType := string(decision.Analysis.Type)
	agentType := decision.SubAgentType
	if agentType == "" {
		agentType = string(decision.Handler)
	}

	// Record with strategy optimizer
	if sr.optimizer != nil {
		sr.optimizer.RecordExecution(agentType, taskType, success, duration)
	}

	// Record successful executions for few-shot learning
	// Use a bounded goroutine with timeout to prevent hanging
	if success && sr.exampleStore != nil {
		go func() {
			// Timeout to prevent goroutine leak if store is slow
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			done := make(chan struct{})
			go func() {
				defer close(done)
				if err := sr.exampleStore.LearnFromSuccess(taskType, message, agentType, output, duration, 0); err != nil {
					logging.Debug("failed to learn from success", "error", err)
				}
			}()

			select {
			case <-done:
				// Completed successfully
			case <-ctx.Done():
				logging.Warn("learning from success timed out")
			}
		}()
	}

	return resultHistory, output, err
}

// RouteWithLearning returns a routing decision with learned examples included.
func (sr *SmartRouter) RouteWithLearning(message string) *RoutingDecision {
	return sr.Route(message)
}

// GetExamplesContext returns formatted examples for prompt injection.
func (sr *SmartRouter) GetExamplesContext(taskType, prompt string) string {
	if sr.exampleStore == nil {
		return ""
	}
	return sr.exampleStore.GetExamplesForContext(taskType, prompt, sr.exampleLimit)
}

// GetAdaptiveStats returns statistics about adaptive routing.
func (sr *SmartRouter) GetAdaptiveStats() AdaptiveStats {
	stats := AdaptiveStats{
		Enabled:       sr.adaptiveEnabled,
		MinDataPoints: sr.minDataPoints,
	}

	if sr.optimizer != nil {
		allMetrics := sr.optimizer.GetAllMetrics()
		stats.TotalStrategies = len(allMetrics)
		stats.TopStrategies = make([]StrategyStats, 0)

		for name, metrics := range allMetrics {
			stats.TopStrategies = append(stats.TopStrategies, StrategyStats{
				Name:        name,
				SuccessRate: metrics.SuccessRate(),
				UseCount:    totalCount(metrics),
			})
		}
	}

	return stats
}

// AdaptiveStats contains statistics about adaptive routing.
type AdaptiveStats struct {
	Enabled         bool
	MinDataPoints   int
	TotalStrategies int
	TopStrategies   []StrategyStats
}

// StrategyStats contains statistics about a strategy.
type StrategyStats struct {
	Name        string
	SuccessRate float64
	UseCount    int
}

// totalCount returns the total number of executions for a strategy.
func totalCount(m *agent.StrategyMetrics) int {
	return m.SuccessCount + m.FailureCount
}

// Extend RoutingDecision with learned examples
func init() {
	// The LearnedExamples field is added to RoutingDecision below
}

// min returns the smaller of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
