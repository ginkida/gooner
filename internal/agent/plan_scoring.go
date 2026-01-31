package agent

// EstimateSuccessProbability estimates the probability of success for an action.
// It uses historical data from StrategyOptimizer when available.
func (tp *TreePlanner) EstimateSuccessProbability(action *PlannedAction) float64 {
	if action == nil {
		return 0.5
	}

	// Try to get historical success rate from strategy optimizer
	if tp.strategyOpt != nil {
		key := buildStrategyKey(action)
		rate := tp.strategyOpt.GetSuccessRate(key)
		if rate > 0 && rate < 1 {
			return rate
		}
	}

	// Fall back to heuristic estimates based on action type
	return estimateActionProbability(action)
}

// buildStrategyKey creates a unique key for strategy lookup.
func buildStrategyKey(action *PlannedAction) string {
	if action == nil {
		return "unknown"
	}

	switch action.Type {
	case ActionToolCall:
		if action.ToolName != "" {
			return "tool:" + action.ToolName
		}
		return "tool:unknown"
	case ActionDelegate:
		return "delegate:" + string(action.AgentType)
	case ActionDecompose:
		return "decompose"
	case ActionVerify:
		return "verify"
	default:
		return string(action.Type)
	}
}

// estimateActionProbability provides heuristic probability estimates.
func estimateActionProbability(action *PlannedAction) float64 {
	switch action.Type {
	case ActionToolCall:
		return estimateToolProbability(action.ToolName)
	case ActionDelegate:
		return estimateAgentProbability(action.AgentType)
	case ActionDecompose:
		return 0.8 // Decomposition usually succeeds
	case ActionVerify:
		return 0.7 // Verification depends on prior steps
	default:
		return 0.5 // Unknown action type
	}
}

// estimateToolProbability provides heuristic estimates for tool success.
func estimateToolProbability(toolName string) float64 {
	switch toolName {
	case "read", "glob", "grep":
		return 0.9 // Read-only operations are very reliable
	case "bash":
		return 0.7 // Bash commands can fail for various reasons
	case "write", "edit":
		return 0.8 // Write operations usually succeed
	case "web_search", "web_fetch":
		return 0.6 // Network operations are less reliable
	default:
		return 0.7 // Default tool reliability
	}
}

// estimateAgentProbability provides heuristic estimates for agent success.
func estimateAgentProbability(agentType AgentType) float64 {
	switch agentType {
	case AgentTypeExplore:
		return 0.85 // Exploration is generally reliable
	case AgentTypeBash:
		return 0.7 // Command execution varies
	case AgentTypeGeneral:
		return 0.75 // General agents handle diverse tasks
	case AgentTypePlan:
		return 0.9 // Planning is usually successful
	case AgentTypeGuide:
		return 0.85 // Documentation lookup is reliable
	default:
		return 0.7 // Unknown agent type
	}
}

// ScoreNode calculates the composite score for a node.
// The score is a weighted combination of success probability, cost, and progress.
func (tp *TreePlanner) ScoreNode(node *PlanNode) float64 {
	if node == nil {
		return 0
	}

	cfg := tp.config

	// Ensure weights sum to 1.0
	totalWeight := cfg.SuccessProbWeight + cfg.CostWeight + cfg.ProgressWeight
	if totalWeight == 0 {
		totalWeight = 1.0
	}

	// Normalize weights
	successWeight := cfg.SuccessProbWeight / totalWeight
	costWeight := cfg.CostWeight / totalWeight
	progressWeight := cfg.ProgressWeight / totalWeight

	// Calculate weighted score
	// Success probability component (higher is better)
	successComponent := node.SuccessProb * successWeight

	// Cost component (lower cost is better, so we use 1 - cost)
	costComponent := (1.0 - node.CostEstimate) * costWeight

	// Progress component (higher progress is better)
	progressComponent := node.GoalProgress * progressWeight

	score := successComponent + costComponent + progressComponent

	// Apply depth penalty (prefer shorter paths)
	depthPenalty := 0.02 * float64(node.Depth)
	score -= depthPenalty

	// Clamp to [0, 1]
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	return score
}

// EstimateCost estimates the cost of executing an action.
// Cost is normalized to [0, 1] where 0 is free and 1 is very expensive.
func (tp *TreePlanner) EstimateCost(action *PlannedAction) float64 {
	if action == nil {
		return 0.5
	}

	switch action.Type {
	case ActionToolCall:
		return estimateToolCost(action.ToolName)
	case ActionDelegate:
		return estimateAgentCost(action.AgentType)
	case ActionDecompose:
		return 0.3 // Moderate cost for decomposition
	case ActionVerify:
		return 0.2 // Verification is typically cheap
	default:
		return 0.5
	}
}

// estimateToolCost provides heuristic cost estimates for tools.
func estimateToolCost(toolName string) float64 {
	switch toolName {
	case "read":
		return 0.1 // Very cheap
	case "glob", "grep":
		return 0.2 // Cheap
	case "edit", "write":
		return 0.3 // Moderate
	case "bash":
		return 0.4 // Moderate to expensive
	case "web_search", "web_fetch":
		return 0.6 // Expensive (network + API)
	case "semantic_search":
		return 0.5 // Moderate (embeddings)
	default:
		return 0.3
	}
}

// estimateAgentCost provides heuristic cost estimates for agents.
func estimateAgentCost(agentType AgentType) float64 {
	switch agentType {
	case AgentTypeExplore:
		return 0.4 // Moderate (multiple reads)
	case AgentTypeBash:
		return 0.3 // Relatively cheap
	case AgentTypeGeneral:
		return 0.6 // Expensive (full capabilities)
	case AgentTypePlan:
		return 0.5 // Moderate (analysis)
	case AgentTypeGuide:
		return 0.3 // Relatively cheap
	default:
		return 0.5
	}
}

// EstimateProgress estimates the goal progress after completing an action.
func (tp *TreePlanner) EstimateProgress(action *PlannedAction, goal *PlanGoal, currentProgress float64) float64 {
	if action == nil || goal == nil {
		return currentProgress
	}

	// Base progress increment depends on action type
	var increment float64

	switch action.Type {
	case ActionDecompose:
		increment = 0.1 // Decomposition is a small step
	case ActionDelegate:
		increment = estimateAgentProgressContribution(action.AgentType)
	case ActionToolCall:
		increment = estimateToolProgressContribution(action.ToolName)
	case ActionVerify:
		// Verification completes the remaining progress if successful
		increment = 1.0 - currentProgress
	default:
		increment = 0.1
	}

	// Cap at 1.0
	newProgress := currentProgress + increment
	if newProgress > 1.0 {
		newProgress = 1.0
	}

	return newProgress
}

// estimateAgentProgressContribution estimates progress contribution by agent type.
func estimateAgentProgressContribution(agentType AgentType) float64 {
	switch agentType {
	case AgentTypeExplore:
		return 0.15 // Exploration is early-stage
	case AgentTypePlan:
		return 0.15 // Planning is early-stage
	case AgentTypeGeneral:
		return 0.4 // General execution makes significant progress
	case AgentTypeBash:
		return 0.2 // Testing/verification
	case AgentTypeGuide:
		return 0.1 // Documentation lookup
	default:
		return 0.15
	}
}

// estimateToolProgressContribution estimates progress contribution by tool.
func estimateToolProgressContribution(toolName string) float64 {
	switch toolName {
	case "read", "glob", "grep":
		return 0.05 // Information gathering
	case "write", "edit":
		return 0.25 // Code modifications
	case "bash":
		return 0.15 // Command execution
	default:
		return 0.1
	}
}

// RecalculateScores recalculates scores for all nodes in a tree.
func (tp *TreePlanner) RecalculateScores(tree *PlanTree) {
	if tree == nil || tree.Root == nil {
		return
	}

	var recalc func(n *PlanNode)
	recalc = func(n *PlanNode) {
		n.SuccessProb = tp.EstimateSuccessProbability(n.Action)
		n.CostEstimate = tp.EstimateCost(n.Action)
		n.Score = tp.ScoreNode(n)

		for _, child := range n.Children {
			recalc(child)
		}
	}

	recalc(tree.Root)
}

// RankNodes returns nodes sorted by score (highest first).
func (tp *TreePlanner) RankNodes(nodes []*PlanNode) []*PlanNode {
	ranked := make([]*PlanNode, len(nodes))
	copy(ranked, nodes)

	for i := 0; i < len(ranked)-1; i++ {
		for j := i + 1; j < len(ranked); j++ {
			if ranked[j].Score > ranked[i].Score {
				ranked[i], ranked[j] = ranked[j], ranked[i]
			}
		}
	}

	return ranked
}

// FilterByMinProbability filters nodes by minimum success probability.
func (tp *TreePlanner) FilterByMinProbability(nodes []*PlanNode) []*PlanNode {
	var filtered []*PlanNode
	minProb := tp.config.MinSuccessProb

	for _, node := range nodes {
		if node.SuccessProb >= minProb {
			filtered = append(filtered, node)
		}
	}

	return filtered
}
