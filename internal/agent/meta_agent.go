package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gokin/internal/logging"
)

// AgentMonitor tracks the state of a running agent.
type AgentMonitor struct {
	AgentID       string
	AgentType     AgentType
	StartTime     time.Time
	LastActivity  time.Time
	CurrentTool   string
	TurnCount     int
	StuckCount    int
	Intervened    bool
	InterventionMsg string
}

// MetaAgentConfig holds configuration for the meta agent.
type MetaAgentConfig struct {
	Enabled           bool
	CheckInterval     time.Duration // How often to check agent health
	StuckThreshold    time.Duration // Duration before considering an agent stuck
	MaxInterventions  int           // Max interventions before giving up
}

// DefaultMetaAgentConfig returns the default configuration.
func DefaultMetaAgentConfig() *MetaAgentConfig {
	return &MetaAgentConfig{
		Enabled:          true,
		CheckInterval:    10 * time.Second,
		StuckThreshold:   2 * time.Minute,
		MaxInterventions: 3,
	}
}

// MetaAgent monitors and optimizes agent execution.
type MetaAgent struct {
	runner       *Runner
	coordinator  *Coordinator
	strategyOpt  *StrategyOptimizer
	typeRegistry *AgentTypeRegistry

	config       *MetaAgentConfig
	activeAgents map[string]*AgentMonitor
	mu           sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc

	// Callbacks for external notifications
	onIntervention func(agentID string, reason string, action string)
	onStuckAgent   func(agentID string, duration time.Duration)
}

// NewMetaAgent creates a new meta agent.
func NewMetaAgent(
	runner *Runner,
	coordinator *Coordinator,
	strategyOpt *StrategyOptimizer,
	typeRegistry *AgentTypeRegistry,
	config *MetaAgentConfig,
) *MetaAgent {
	if config == nil {
		config = DefaultMetaAgentConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &MetaAgent{
		runner:       runner,
		coordinator:  coordinator,
		strategyOpt:  strategyOpt,
		typeRegistry: typeRegistry,
		config:       config,
		activeAgents: make(map[string]*AgentMonitor),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Start begins the meta agent monitoring loop.
func (ma *MetaAgent) Start() {
	if !ma.config.Enabled {
		return
	}

	go ma.monitorLoop()
	logging.Info("meta agent started",
		"check_interval", ma.config.CheckInterval,
		"stuck_threshold", ma.config.StuckThreshold)
}

// Stop stops the meta agent.
func (ma *MetaAgent) Stop() {
	ma.cancel()
}

// RegisterAgent registers an agent for monitoring.
func (ma *MetaAgent) RegisterAgent(agentID string, agentType AgentType) {
	ma.mu.Lock()
	defer ma.mu.Unlock()

	ma.activeAgents[agentID] = &AgentMonitor{
		AgentID:      agentID,
		AgentType:    agentType,
		StartTime:    time.Now(),
		LastActivity: time.Now(),
	}

	logging.Debug("meta agent: registered agent for monitoring",
		"agent_id", agentID,
		"agent_type", agentType)
}

// UnregisterAgent removes an agent from monitoring.
func (ma *MetaAgent) UnregisterAgent(agentID string) {
	ma.mu.Lock()
	defer ma.mu.Unlock()

	delete(ma.activeAgents, agentID)
}

// UpdateActivity updates the last activity time for an agent.
func (ma *MetaAgent) UpdateActivity(agentID string, toolName string, turnCount int) {
	ma.mu.Lock()
	defer ma.mu.Unlock()

	if monitor, ok := ma.activeAgents[agentID]; ok {
		monitor.LastActivity = time.Now()
		monitor.CurrentTool = toolName
		monitor.TurnCount = turnCount
	}
}

// monitorLoop is the main monitoring loop.
func (ma *MetaAgent) monitorLoop() {
	ticker := time.NewTicker(ma.config.CheckInterval)
	defer ticker.Stop()

	// Recover from panics to prevent goroutine leak
	defer func() {
		if r := recover(); r != nil {
			logging.Error("meta agent monitor loop panicked", "panic", r)
		}
	}()

	for {
		select {
		case <-ma.ctx.Done():
			logging.Debug("meta agent monitor loop stopping due to context cancellation")
			return
		case <-ticker.C:
			// Check context again before doing work
			if ma.ctx.Err() != nil {
				return
			}
			ma.checkAgentHealth()
			ma.runOptimization()
		}
	}
}

// checkAgentHealth checks all active agents for stuck state.
func (ma *MetaAgent) checkAgentHealth() {
	ma.mu.Lock()
	defer ma.mu.Unlock()

	now := time.Now()

	for agentID, monitor := range ma.activeAgents {
		inactiveDuration := now.Sub(monitor.LastActivity)

		// Check if agent appears stuck
		if inactiveDuration > ma.config.StuckThreshold {
			monitor.StuckCount++

			// Notify callback
			if ma.onStuckAgent != nil {
				ma.onStuckAgent(agentID, inactiveDuration)
			}

			// Attempt intervention if not already intervened too many times
			if !monitor.Intervened && monitor.StuckCount <= ma.config.MaxInterventions {
				ma.handleStuckAgent(agentID, monitor)
			} else if monitor.StuckCount > ma.config.MaxInterventions {
				logging.Warn("meta agent: agent exceeded max interventions",
					"agent_id", agentID,
					"stuck_count", monitor.StuckCount)
			}
		} else {
			// Agent is active, reset stuck count
			monitor.StuckCount = 0
			monitor.Intervened = false
		}
	}
}

// handleStuckAgent handles an agent that appears to be stuck.
func (ma *MetaAgent) handleStuckAgent(agentID string, monitor *AgentMonitor) {
	logging.Info("meta agent: handling stuck agent",
		"agent_id", agentID,
		"agent_type", monitor.AgentType,
		"inactive_for", time.Since(monitor.LastActivity),
		"current_tool", monitor.CurrentTool,
		"turn_count", monitor.TurnCount)

	monitor.Intervened = true

	// Determine intervention strategy based on agent type and state
	var action string
	var reason string

	switch {
	case monitor.CurrentTool == "bash" && monitor.TurnCount > 10:
		action = "suggest_decompose"
		reason = "Agent stuck on bash execution - may need task decomposition"
		monitor.InterventionMsg = "The task seems complex. Consider breaking it into smaller steps."

	case monitor.CurrentTool == "" && monitor.TurnCount > 15:
		action = "suggest_reset"
		reason = "Agent appears to be in a decision loop - suggesting fresh approach"
		monitor.InterventionMsg = "Let me reconsider the approach from the beginning."

	case monitor.AgentType == AgentTypeExplore && monitor.TurnCount > 20:
		action = "suggest_summarize"
		reason = "Explore agent may have found enough information"
		monitor.InterventionMsg = "I have gathered enough information. Let me summarize what I found."

	default:
		action = "generic_nudge"
		reason = "Agent inactive for too long"
		monitor.InterventionMsg = "I should take action now rather than waiting."
	}

	// Log the intervention
	logging.Info("meta agent: intervening",
		"agent_id", agentID,
		"action", action,
		"reason", reason)

	// Notify callback
	if ma.onIntervention != nil {
		ma.onIntervention(agentID, reason, action)
	}

	// Note: Actual intervention would require injecting a message into the agent's
	// conversation history. This would be done through the Runner or Agent interface.
	// For now, we just log the intended intervention.
}

// runOptimization performs periodic optimization based on metrics.
func (ma *MetaAgent) runOptimization() {
	if ma.strategyOpt == nil {
		return
	}

	// Get all metrics
	metrics := ma.strategyOpt.GetAllMetrics()
	if len(metrics) < 3 {
		return // Not enough data for optimization
	}

	// Find underperforming strategies
	for name, m := range metrics {
		if m.SuccessRate() < 0.3 && m.SuccessCount+m.FailureCount >= 5 {
			logging.Info("meta agent: strategy underperforming",
				"strategy", name,
				"success_rate", fmt.Sprintf("%.1f%%", m.SuccessRate()*100),
				"total_executions", m.SuccessCount+m.FailureCount)

			// Could trigger retraining, parameter adjustment, or strategy replacement
		}
	}
}

// SetInterventionCallback sets the callback for intervention notifications.
func (ma *MetaAgent) SetInterventionCallback(callback func(agentID string, reason string, action string)) {
	ma.onIntervention = callback
}

// SetStuckAgentCallback sets the callback for stuck agent notifications.
func (ma *MetaAgent) SetStuckAgentCallback(callback func(agentID string, duration time.Duration)) {
	ma.onStuckAgent = callback
}

// GetActiveAgents returns information about all active agents.
func (ma *MetaAgent) GetActiveAgents() map[string]*AgentMonitor {
	ma.mu.RLock()
	defer ma.mu.RUnlock()

	// Return a copy
	copy := make(map[string]*AgentMonitor)
	for k, v := range ma.activeAgents {
		copy[k] = v
	}
	return copy
}

// GetAgentStatus returns the status of a specific agent.
func (ma *MetaAgent) GetAgentStatus(agentID string) (*AgentMonitor, bool) {
	ma.mu.RLock()
	defer ma.mu.RUnlock()

	monitor, ok := ma.activeAgents[agentID]
	return monitor, ok
}

// GetStats returns meta agent statistics.
func (ma *MetaAgent) GetStats() map[string]interface{} {
	ma.mu.RLock()
	defer ma.mu.RUnlock()

	totalInterventions := 0
	for _, monitor := range ma.activeAgents {
		if monitor.Intervened {
			totalInterventions++
		}
	}

	return map[string]interface{}{
		"active_agents":       len(ma.activeAgents),
		"total_interventions": totalInterventions,
		"check_interval":      ma.config.CheckInterval.String(),
		"stuck_threshold":     ma.config.StuckThreshold.String(),
	}
}
