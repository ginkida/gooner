package agent

import (
	"context"
	"strings"
	"time"

	"gokin/internal/logging"
)

// DelegationStrategy determines when and how an agent should delegate to sub-agents.
type DelegationStrategy struct {
	messenger          *AgentMessenger
	agentType          AgentType
	turnCount          int
	stuckThreshold     int
	lastProgress       string
	sameProgressCount  int
	currentDepth       int                 // Current delegation depth
	strategyOpt        *StrategyOptimizer  // For historical success rate lookup
	delegationMetrics  *DelegationMetrics  // For adaptive delegation rules
	currentContextType string              // Current task context type for metrics
	failedRules        map[string]time.Time // Rule suppression cache: rule_key -> failure_time
	activeAgents       int                  // Number of currently active delegated agents
}

// DelegationDecision represents a decision to delegate work to another agent.
type DelegationDecision struct {
	ShouldDelegate bool
	TargetType     string
	Reason         string
	Query          string
}

// DelegationRule defines a condition and action for delegation.
type DelegationRule struct {
	FromType   AgentType
	Condition  func(ctx *DelegationContext) bool
	TargetType string
	BuildQuery func(ctx *DelegationContext) string
	Reason     string
}

// MaxDelegationDepth is the maximum allowed delegation depth to prevent infinite recursion.
const MaxDelegationDepth = 5

// DelegationContext provides context for delegation decisions.
type DelegationContext struct {
	AgentType       AgentType
	CurrentTurn     int
	MaxTurns        int
	LastToolName    string
	LastToolError   string
	LastToolArgs    map[string]any
	ReflectionInfo  *Reflection
	StuckCount      int
	DelegationDepth int // Current depth of delegation chain
}

// NewDelegationStrategy creates a new delegation strategy for an agent.
func NewDelegationStrategy(agentType AgentType, messenger *AgentMessenger) *DelegationStrategy {
	return &DelegationStrategy{
		messenger:      messenger,
		agentType:      agentType,
		stuckThreshold: 5,
		failedRules:    make(map[string]time.Time),
	}
}

// SetStrategyOptimizer sets the strategy optimizer for historical success rate lookup.
func (d *DelegationStrategy) SetStrategyOptimizer(opt *StrategyOptimizer) {
	d.strategyOpt = opt
}

// SetDelegationMetrics sets the delegation metrics for adaptive rules.
func (d *DelegationStrategy) SetDelegationMetrics(dm *DelegationMetrics) {
	d.delegationMetrics = dm
}

// SetContextType sets the current task context type for metrics tracking.
func (d *DelegationStrategy) SetContextType(contextType string) {
	d.currentContextType = contextType
}

// RecordDelegationResult records the outcome of a delegation for learning.
func (d *DelegationStrategy) RecordDelegationResult(targetType string, success bool, duration time.Duration, errorType string) {
	if d.delegationMetrics == nil {
		return
	}

	contextType := d.currentContextType
	if contextType == "" {
		contextType = "general"
	}

	d.delegationMetrics.RecordExecution(
		string(d.agentType),
		targetType,
		contextType,
		success,
		duration,
		errorType,
	)
}

// SetMessenger sets the messenger for delegation.
func (d *DelegationStrategy) SetMessenger(m *AgentMessenger) {
	d.messenger = m
}

// SuppressRule temporarily suppresses a delegation rule after failure.
func (d *DelegationStrategy) SuppressRule(targetType string, duration time.Duration) {
	if d.failedRules == nil {
		d.failedRules = make(map[string]time.Time)
	}
	key := string(d.agentType) + ":" + targetType
	d.failedRules[key] = time.Now().Add(duration)
}

// isRuleSuppressed checks if a delegation rule is currently suppressed.
func (d *DelegationStrategy) isRuleSuppressed(targetType string) bool {
	if d.failedRules == nil {
		return false
	}
	key := string(d.agentType) + ":" + targetType
	expiry, ok := d.failedRules[key]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(d.failedRules, key)
		return false
	}
	return true
}

// SetActiveAgents updates the count of currently active delegated agents.
func (d *DelegationStrategy) SetActiveAgents(count int) {
	d.activeAgents = count
}

// AdaptiveMaxTurns returns the maximum turns for a delegated agent,
// reducing turns for deeper delegation chains.
func (d *DelegationStrategy) AdaptiveMaxTurns(baseTurns int) int {
	adapted := baseTurns - (d.currentDepth * 3)
	if adapted < 5 {
		adapted = 5
	}
	return adapted
}

// Evaluate checks if delegation should occur based on current state.
// Uses StrategyOptimizer to prefer agents with higher historical success rates.
func (d *DelegationStrategy) Evaluate(ctx *DelegationContext) *DelegationDecision {
	// Check delegation depth limit to prevent infinite recursion
	if ctx.DelegationDepth >= MaxDelegationDepth {
		logging.Debug("delegation depth limit reached",
			"depth", ctx.DelegationDepth,
			"max", MaxDelegationDepth)
		return &DelegationDecision{ShouldDelegate: false}
	}

	// Collect all matching rules
	var matchingDecisions []*DelegationDecision
	for _, rule := range defaultDelegationRules() {
		// Check if rule applies to this agent type
		if rule.FromType != "" && rule.FromType != ctx.AgentType {
			continue
		}

		// Check condition
		if rule.Condition(ctx) {
			matchingDecisions = append(matchingDecisions, &DelegationDecision{
				ShouldDelegate: true,
				TargetType:     rule.TargetType,
				Reason:         rule.Reason,
				Query:          rule.BuildQuery(ctx),
			})
		}
	}

	// Filter out suppressed rules
	var activeDecisions []*DelegationDecision
	for _, dec := range matchingDecisions {
		if !d.isRuleSuppressed(dec.TargetType) {
			activeDecisions = append(activeDecisions, dec)
		}
	}

	if len(activeDecisions) == 0 {
		return &DelegationDecision{ShouldDelegate: false}
	}

	matchingDecisions = activeDecisions

	// If only one match, return it
	if len(matchingDecisions) == 1 {
		return matchingDecisions[0]
	}

	// Multiple matches - use StrategyOptimizer to pick the best one
	if d.strategyOpt != nil {
		return d.selectBestDelegation(matchingDecisions)
	}

	// Fall back to first match
	return matchingDecisions[0]
}

// selectBestDelegation chooses the delegation target with the highest historical success rate.
// Uses both StrategyOptimizer and DelegationMetrics for comprehensive scoring.
func (d *DelegationStrategy) selectBestDelegation(decisions []*DelegationDecision) *DelegationDecision {
	if len(decisions) == 0 {
		return &DelegationDecision{ShouldDelegate: false}
	}

	bestDecision := decisions[0]
	bestScore := d.calculateDelegationScore(bestDecision.TargetType)

	for _, decision := range decisions[1:] {
		score := d.calculateDelegationScore(decision.TargetType)
		if score > bestScore {
			bestScore = score
			bestDecision = decision
		}
	}

	logging.Debug("selected delegation target by combined score",
		"target", bestDecision.TargetType,
		"score", bestScore)

	return bestDecision
}

// calculateDelegationScore calculates a combined score for a delegation target.
func (d *DelegationStrategy) calculateDelegationScore(targetType string) float64 {
	baseRate := d.getAgentSuccessRate(targetType)

	// If we have delegation metrics, enhance the score
	if d.delegationMetrics != nil {
		contextType := d.currentContextType
		if contextType == "" {
			contextType = "general"
		}

		// Get historical success rate from delegation metrics
		historicalRate := d.delegationMetrics.GetSuccessRate(
			string(d.agentType),
			targetType,
			contextType,
		)

		// Get rule weight
		weight := d.delegationMetrics.GetRuleWeight(
			string(d.agentType),
			targetType,
			contextType,
		)

		// Get trend
		trend := d.delegationMetrics.GetRecentTrend(
			string(d.agentType),
			targetType,
			contextType,
		)

		// Combined score: weighted average of base rate and historical rate, plus trend bonus
		combinedRate := (baseRate*0.4 + historicalRate*0.6) * weight
		trendBonus := trend * 0.1

		score := combinedRate + trendBonus

		// Apply load factor adjustment
		loadFactor := float64(d.activeAgents) / 5.0 // Normalize to 0-1
		if loadFactor > 1.0 {
			loadFactor = 1.0
		}
		score = score * (1.0 - loadFactor*0.3)

		return score
	}

	return baseRate
}

// getAgentSuccessRate returns the historical success rate for an agent type.
func (d *DelegationStrategy) getAgentSuccessRate(agentType string) float64 {
	if d.strategyOpt == nil {
		return 0.5 // Default neutral
	}

	// Look up by delegation strategy key
	key := "delegate:" + agentType
	rate := d.strategyOpt.GetSuccessRate(key)
	if rate > 0 && rate < 1 {
		return rate
	}

	// Also try agent type directly
	rate = d.strategyOpt.GetSuccessRate(agentType)
	if rate > 0 && rate < 1 {
		return rate
	}

	return 0.5 // Default neutral
}

// TrackProgress tracks progress to detect stuck agents.
func (d *DelegationStrategy) TrackProgress(progress string) {
	d.turnCount++

	if progress == d.lastProgress {
		d.sameProgressCount++
	} else {
		d.sameProgressCount = 0
		d.lastProgress = progress
	}
}

// IsStuck returns true if the agent appears to be stuck.
func (d *DelegationStrategy) IsStuck() bool {
	return d.sameProgressCount >= d.stuckThreshold
}

// GetStuckCount returns how many turns the agent has been stuck.
func (d *DelegationStrategy) GetStuckCount() int {
	return d.sameProgressCount
}

// SetDepth sets the current delegation depth.
func (d *DelegationStrategy) SetDepth(depth int) {
	d.currentDepth = depth
}

// GetDepth returns the current delegation depth.
func (d *DelegationStrategy) GetDepth() int {
	return d.currentDepth
}

// ExecuteDelegation sends a delegation request to another agent.
func (d *DelegationStrategy) ExecuteDelegation(ctx context.Context, decision *DelegationDecision) (string, error) {
	if d.messenger == nil {
		return "", nil
	}

	logging.Info("delegating to sub-agent",
		"from_type", d.agentType,
		"to_type", decision.TargetType,
		"reason", decision.Reason,
		"depth", d.currentDepth)

	// Send delegation request with depth tracking
	msgID, err := d.messenger.SendMessage("delegate", decision.TargetType, decision.Query, map[string]any{
		"reason":           decision.Reason,
		"max_turns":        15,
		"delegation_depth": d.currentDepth,
	})
	if err != nil {
		return "", err
	}

	// Wait for response with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	return d.messenger.ReceiveResponse(timeoutCtx, msgID)
}

// defaultDelegationRules returns the built-in delegation rules.
func defaultDelegationRules() []DelegationRule {
	return []DelegationRule{
		// Rule: Explore agent needs bash command execution
		{
			FromType: AgentTypeExplore,
			Condition: func(ctx *DelegationContext) bool {
				// Explore can't execute bash commands - delegate to bash agent
				if ctx.LastToolName == "bash" {
					return true
				}
				// Check if reflection suggests using bash
				if ctx.ReflectionInfo != nil && ctx.ReflectionInfo.Alternative == "bash" {
					return true
				}
				return false
			},
			TargetType: "bash",
			BuildQuery: func(ctx *DelegationContext) string {
				if ctx.LastToolArgs != nil {
					if cmd, ok := ctx.LastToolArgs["command"].(string); ok {
						return "Execute this command and return the result: " + cmd
					}
				}
				return "Help execute a shell command"
			},
			Reason: "Explore agent cannot execute bash commands",
		},

		// Rule: Bash agent has compilation error - ask explore for context
		{
			FromType: AgentTypeBash,
			Condition: func(ctx *DelegationContext) bool {
				if ctx.ReflectionInfo == nil {
					return false
				}
				// Compilation errors benefit from code exploration
				return ctx.ReflectionInfo.Category == "compilation_error" ||
					ctx.ReflectionInfo.Alternative == "explore"
			},
			TargetType: "explore",
			BuildQuery: func(ctx *DelegationContext) string {
				var sb strings.Builder
				sb.WriteString("I'm getting a compilation error. Help me understand the context:\n\n")
				if ctx.LastToolError != "" {
					sb.WriteString("Error: " + ctx.LastToolError + "\n\n")
				}
				sb.WriteString("Please find the relevant code and explain what might be wrong.")
				return sb.String()
			},
			Reason: "Bash agent needs code context for compilation error",
		},

		// Rule: General agent stuck too long - ask plan for decomposition
		{
			FromType: AgentTypeGeneral,
			Condition: func(ctx *DelegationContext) bool {
				return ctx.StuckCount >= 5
			},
			TargetType: "plan",
			BuildQuery: func(ctx *DelegationContext) string {
				return "I'm stuck on this task. Please help me break it down into smaller steps:\n\n" +
					"Last action: " + ctx.LastToolName + "\n" +
					"I've been trying the same approach for " + string(rune(ctx.StuckCount+'0')) + " turns."
			},
			Reason: "General agent stuck - needs task decomposition",
		},

		// Rule: Plan agent needs actual code analysis - delegate to explore
		{
			FromType: AgentTypePlan,
			Condition: func(ctx *DelegationContext) bool {
				// If plan agent tried glob/grep and got no results, delegate to explore
				if ctx.LastToolError != "" &&
					(ctx.LastToolName == "glob" || ctx.LastToolName == "grep") {
					return true
				}
				return false
			},
			TargetType: "explore",
			BuildQuery: func(ctx *DelegationContext) string {
				var sb strings.Builder
				sb.WriteString("Help me find information for planning:\n\n")
				if ctx.LastToolArgs != nil {
					if pattern, ok := ctx.LastToolArgs["pattern"].(string); ok {
						sb.WriteString("I was looking for: " + pattern + "\n")
					}
				}
				sb.WriteString("Please do a thorough exploration and report what you find.")
				return sb.String()
			},
			Reason: "Plan agent needs deeper exploration",
		},

		// Rule: Any agent with file not found - ask explore for correct path
		{
			FromType: "", // Applies to all types
			Condition: func(ctx *DelegationContext) bool {
				if ctx.ReflectionInfo == nil {
					return false
				}
				return ctx.ReflectionInfo.Category == "file_not_found" &&
					ctx.ReflectionInfo.Alternative == "glob"
			},
			TargetType: "explore",
			BuildQuery: func(ctx *DelegationContext) string {
				var sb strings.Builder
				sb.WriteString("I couldn't find a file. Help me locate it:\n\n")
				if ctx.LastToolArgs != nil {
					if path, ok := ctx.LastToolArgs["path"].(string); ok {
						sb.WriteString("Path I tried: " + path + "\n")
					}
					if pattern, ok := ctx.LastToolArgs["pattern"].(string); ok {
						sb.WriteString("Pattern I tried: " + pattern + "\n")
					}
				}
				sb.WriteString("\nPlease search for similar files and tell me the correct path.")
				return sb.String()
			},
			Reason: "File not found - need explore agent to find correct path",
		},

		// Rule: Any agent stuck for too long - get help from general
		{
			FromType: "", // Applies to all types
			Condition: func(ctx *DelegationContext) bool {
				// Don't delegate from general to general
				if ctx.AgentType == AgentTypeGeneral {
					return false
				}
				return ctx.StuckCount >= 7
			},
			TargetType: "general",
			BuildQuery: func(ctx *DelegationContext) string {
				return "I'm a specialized agent that's stuck. Please help me complete this task with your broader capabilities."
			},
			Reason: "Specialized agent stuck - escalating to general agent",
		},
	}
}
