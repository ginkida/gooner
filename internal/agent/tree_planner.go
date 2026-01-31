package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gooner/internal/client"
	"gooner/internal/logging"
)

// SearchAlgorithm defines the algorithm used for tree search.
type SearchAlgorithm string

const (
	SearchAlgorithmBeam  SearchAlgorithm = "beam"
	SearchAlgorithmMCTS  SearchAlgorithm = "mcts"
	SearchAlgorithmAStar SearchAlgorithm = "astar"
)

// LLM plan validation constants
const (
	maxPlanActions  = 10            // Maximum steps in a plan
	maxPromptLength = 500           // Maximum characters in step prompt
	llmPlanTimeout  = 30 * time.Second // Timeout for LLM calls
)

// TreePlannerConfig holds configuration for the tree planner.
type TreePlannerConfig struct {
	Algorithm       SearchAlgorithm `json:"algorithm"`
	BeamWidth       int             `json:"beam_width"`
	MCTSIterations  int             `json:"mcts_iterations"`
	MinSuccessProb  float64         `json:"min_success_prob"`
	MaxTreeDepth    int             `json:"max_tree_depth"`
	MaxTreeNodes    int             `json:"max_tree_nodes"`
	ReplanOnFailure bool            `json:"replan_on_failure"`
	MaxReplans      int             `json:"max_replans"`

	// Scoring weights (should sum to 1.0)
	SuccessProbWeight float64 `json:"success_prob_weight"`
	CostWeight        float64 `json:"cost_weight"`
	ProgressWeight    float64 `json:"progress_weight"`

	// MCTS exploration constant
	ExplorationC float64 `json:"exploration_c"`
}

// DefaultTreePlannerConfig returns the default tree planner configuration.
func DefaultTreePlannerConfig() *TreePlannerConfig {
	return &TreePlannerConfig{
		Algorithm:         SearchAlgorithmBeam,
		BeamWidth:         5,
		MCTSIterations:    100,
		MinSuccessProb:    0.1,
		MaxTreeDepth:      10,
		MaxTreeNodes:      1000,
		ReplanOnFailure:   true,
		MaxReplans:        3,
		SuccessProbWeight: 0.4,
		CostWeight:        0.3,
		ProgressWeight:    0.3,
		ExplorationC:      1.414, // sqrt(2) - standard UCB1 constant
	}
}

// ReplanContext provides information for replanning after a failure.
type ReplanContext struct {
	FailedNode    *PlanNode
	Error         string
	Reflection    *Reflection
	AttemptNumber int
}

// TreePlanner builds and manages plan trees for agent execution.
type TreePlanner struct {
	config      *TreePlannerConfig
	strategyOpt *StrategyOptimizer
	reflector   *Reflector
	client      client.Client

	trees map[string]*PlanTree
	mu    sync.RWMutex

	// Callbacks for plan events
	onNodeStart    func(tree *PlanTree, node *PlanNode)
	onNodeComplete func(tree *PlanTree, node *PlanNode, success bool)
	onReplan       func(tree *PlanTree, ctx *ReplanContext)
}

// NewTreePlanner creates a new tree planner.
func NewTreePlanner(config *TreePlannerConfig, strategyOpt *StrategyOptimizer, reflector *Reflector, c client.Client) *TreePlanner {
	if config == nil {
		config = DefaultTreePlannerConfig()
	}

	return &TreePlanner{
		config:      config,
		strategyOpt: strategyOpt,
		reflector:   reflector,
		client:      c,
		trees:       make(map[string]*PlanTree),
	}
}

// SetCallbacks sets event callbacks for plan execution.
func (tp *TreePlanner) SetCallbacks(
	onNodeStart func(tree *PlanTree, node *PlanNode),
	onNodeComplete func(tree *PlanTree, node *PlanNode, success bool),
	onReplan func(tree *PlanTree, ctx *ReplanContext),
) {
	tp.onNodeStart = onNodeStart
	tp.onNodeComplete = onNodeComplete
	tp.onReplan = onReplan
}

// BuildTree constructs a plan tree for the given prompt and goal.
func (tp *TreePlanner) BuildTree(ctx context.Context, prompt string, goal *PlanGoal) (*PlanTree, error) {
	if goal == nil {
		goal = &PlanGoal{
			Description: prompt,
			MaxDepth:    tp.config.MaxTreeDepth,
			MaxNodes:    tp.config.MaxTreeNodes,
			Timeout:     5 * time.Minute,
		}
	}

	tree := NewPlanTree(goal)

	// Generate initial action candidates
	actions, err := tp.generateActions(ctx, prompt, goal)
	if err != nil {
		logging.Warn("failed to generate initial actions", "error", err)
		// Fall back to a single general action
		actions = []*PlannedAction{
			{
				Type:   ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt: prompt,
			},
		}
	}

	// Add initial actions as children of root
	for _, action := range actions {
		node := tree.AddNode(tree.Root.ID, action)
		if node != nil {
			node.SuccessProb = tp.EstimateSuccessProbability(action)
			node.Score = tp.ScoreNode(node)
		}
	}

	// Find the best path using the configured algorithm
	tree.BestPath = tp.SelectBestPath(tree)

	tp.mu.Lock()
	tp.trees[tree.ID] = tree
	tp.mu.Unlock()

	logging.Debug("plan tree built",
		"tree_id", tree.ID,
		"total_nodes", tree.TotalNodes,
		"best_path_length", len(tree.BestPath))

	return tree, nil
}

// generateActions generates candidate actions for a prompt.
// Uses pattern matching for common task types, with LLM fallback for complex tasks.
func (tp *TreePlanner) generateActions(ctx context.Context, prompt string, goal *PlanGoal) ([]*PlannedAction, error) {
	actions := []*PlannedAction{}

	// Try LLM-based generation first if client is available
	if tp.client != nil {
		llmActions, err := tp.generateActionsWithLLM(ctx, prompt, goal)
		if err == nil && len(llmActions) > 0 {
			return llmActions, nil
		}
		// Fall back to pattern matching on error
		logging.Debug("LLM action generation failed, using pattern matching", "error", err)
	}

	// Pattern-based generation (fallback)
	promptLower := prompt

	// Implementation tasks (EN + RU)
	if containsAny(promptLower, []string{
		"implement", "create", "add", "build", "develop", "make", "write",
		"реализ", "создай", "добав", "напиши", "сделай", "разработ",
	}) {
		actions = append(actions,
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeExplore,
				Prompt:    "Explore the codebase to understand the current structure and patterns relevant to: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypePlan,
				Prompt:    "Create a detailed implementation plan for: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    "Execute the implementation: " + prompt,
			},
			&PlannedAction{
				Type:   ActionVerify,
				Prompt: "Verify the implementation is complete and correct",
			},
		)
	} else if containsAny(promptLower, []string{
		"fix", "bug", "error", "issue", "debug", "repair", "resolve",
		"исправ", "баг", "ошибк", "почин", "отлад",
	}) {
		// Bug fixing tasks
		actions = append(actions,
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeExplore,
				Prompt:    "Investigate the issue and locate the root cause: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    "Apply the fix: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeBash,
				Prompt:    "Run tests to verify the fix",
			},
		)
	} else if containsAny(promptLower, []string{
		"refactor", "clean", "improve", "optimize", "restructure",
		"рефактор", "очист", "улучш", "оптимиз",
	}) {
		// Refactoring tasks
		actions = append(actions,
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeExplore,
				Prompt:    "Analyze the current implementation: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypePlan,
				Prompt:    "Plan the refactoring approach: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    "Execute the refactoring: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeBash,
				Prompt:    "Run tests to ensure no regressions",
			},
		)
	} else if containsAny(promptLower, []string{"test", "тест"}) {
		actions = append(actions,
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeExplore,
				Prompt:    "Find relevant test files and understand testing patterns: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    "Write or update tests: " + prompt,
			},
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeBash,
				Prompt:    "Run the tests and verify they pass",
			},
		)
	} else {
		// Generic pattern for unrecognized tasks
		actions = append(actions,
			&PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    prompt,
			},
		)
	}

	return actions, nil
}

// ExpandNode generates child nodes for the given node.
func (tp *TreePlanner) ExpandNode(ctx context.Context, tree *PlanTree, node *PlanNode, goal *PlanGoal) ([]*PlanNode, error) {
	if node.Depth >= tp.config.MaxTreeDepth {
		return nil, fmt.Errorf("max depth reached")
	}

	if tree.TotalNodes >= tp.config.MaxTreeNodes {
		return nil, fmt.Errorf("max nodes reached")
	}

	// Generate candidate actions based on node context
	var actions []*PlannedAction

	switch node.Status {
	case PlanNodeFailed:
		// Generate recovery actions
		actions = tp.generateRecoveryActions(node)
	case PlanNodeSucceeded:
		// Generate follow-up actions
		actions = tp.generateFollowUpActions(ctx, node, goal)
	default:
		// Generate alternative approaches
		actions = tp.generateAlternativeActions(node, goal)
	}

	var newNodes []*PlanNode
	for _, action := range actions {
		child := tree.AddNode(node.ID, action)
		if child != nil {
			child.SuccessProb = tp.EstimateSuccessProbability(action)
			child.Score = tp.ScoreNode(child)
			newNodes = append(newNodes, child)
		}
	}

	tree.ExpandedNodes++

	return newNodes, nil
}

// generateRecoveryActions creates actions to recover from a failure.
func (tp *TreePlanner) generateRecoveryActions(failedNode *PlanNode) []*PlannedAction {
	var actions []*PlannedAction

	if failedNode.Action == nil {
		return actions
	}

	// If we have reflection data, use it
	if tp.reflector != nil && failedNode.Error != "" {
		reflection := tp.reflector.Analyze(
			failedNode.Action.ToolName,
			failedNode.Action.ToolArgs,
			failedNode.Error,
		)

		if reflection.Alternative != "" {
			actions = append(actions, &PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentType(reflection.Alternative),
				Prompt:    fmt.Sprintf("Alternative approach after failure: %s", failedNode.Action.Prompt),
			})
		}

		if reflection.ShouldRetry {
			// Retry with modified approach
			actions = append(actions, &PlannedAction{
				Type:      failedNode.Action.Type,
				AgentType: failedNode.Action.AgentType,
				ToolName:  failedNode.Action.ToolName,
				ToolArgs:  failedNode.Action.ToolArgs,
				Prompt:    fmt.Sprintf("Retry with modifications: %s\nPrevious error: %s", failedNode.Action.Prompt, failedNode.Error),
			})
		}
	}

	// Always add a general fallback
	actions = append(actions, &PlannedAction{
		Type:      ActionDelegate,
		AgentType: AgentTypeGeneral,
		Prompt:    fmt.Sprintf("Find an alternative way to complete: %s\nPrevious approach failed with: %s", failedNode.Action.Prompt, failedNode.Error),
	})

	return actions
}

// generateFollowUpActions creates actions to continue after success.
func (tp *TreePlanner) generateFollowUpActions(ctx context.Context, node *PlanNode, goal *PlanGoal) []*PlannedAction {
	var actions []*PlannedAction

	// Check if goal is achieved
	if node.GoalProgress >= 1.0 {
		// Add verification step
		actions = append(actions, &PlannedAction{
			Type:   ActionVerify,
			Prompt: "Verify that all success criteria are met: " + goal.Description,
		})
		return actions
	}

	// Continue with next logical step based on current progress
	if node.Action != nil && node.Action.Type == ActionDelegate {
		switch node.Action.AgentType {
		case AgentTypeExplore:
			// After exploration, plan
			actions = append(actions, &PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypePlan,
				Prompt:    fmt.Sprintf("Based on exploration results, plan the next steps for: %s", goal.Description),
			})
		case AgentTypePlan:
			// After planning, execute
			actions = append(actions, &PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    "Execute the plan",
			})
		case AgentTypeGeneral:
			// After execution, verify
			actions = append(actions, &PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentTypeBash,
				Prompt:    "Run tests to verify the changes",
			})
		case AgentTypeBash:
			// After testing, verify completion
			actions = append(actions, &PlannedAction{
				Type:   ActionVerify,
				Prompt: "Verify all success criteria are met",
			})
		}
	}

	return actions
}

// generateAlternativeActions creates alternative approaches for the same goal.
func (tp *TreePlanner) generateAlternativeActions(node *PlanNode, goal *PlanGoal) []*PlannedAction {
	var actions []*PlannedAction

	if node.Action == nil {
		return actions
	}

	// Generate alternatives based on current action type
	switch node.Action.Type {
	case ActionDelegate:
		// Try different agent types
		alternatives := []AgentType{AgentTypeGeneral, AgentTypeExplore, AgentTypePlan, AgentTypeBash}
		for _, at := range alternatives {
			if at != node.Action.AgentType {
				actions = append(actions, &PlannedAction{
					Type:      ActionDelegate,
					AgentType: at,
					Prompt:    node.Action.Prompt,
				})
			}
		}
	case ActionToolCall:
		// Suggest using delegation instead
		actions = append(actions, &PlannedAction{
			Type:      ActionDelegate,
			AgentType: AgentTypeGeneral,
			Prompt:    fmt.Sprintf("Complete this task that requires tool %s: %s", node.Action.ToolName, node.Action.Prompt),
		})
	}

	return actions
}

// GetNextAction returns the next action to execute from the plan.
func (tp *TreePlanner) GetNextAction(tree *PlanTree) (*PlannedAction, error) {
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("invalid plan tree")
	}

	// Find the first ready node in the best path
	for _, node := range tree.BestPath {
		if node.Status == PlanNodePending && tree.arePrerequisitesMet(node) {
			node.Status = PlanNodeExecuting
			node.UpdatedAt = time.Now()
			tree.CurrentNode = node

			if tp.onNodeStart != nil {
				tp.onNodeStart(tree, node)
			}

			return node.Action, nil
		}
	}

	// No ready nodes in best path, check all ready nodes
	readyNodes := tree.GetReadyNodes()
	if len(readyNodes) > 0 {
		// Pick the highest scored ready node
		var bestNode *PlanNode
		for _, node := range readyNodes {
			if bestNode == nil || node.Score > bestNode.Score {
				bestNode = node
			}
		}

		bestNode.Status = PlanNodeExecuting
		bestNode.UpdatedAt = time.Now()
		tree.CurrentNode = bestNode

		if tp.onNodeStart != nil {
			tp.onNodeStart(tree, bestNode)
		}

		return bestNode.Action, nil
	}

	return nil, fmt.Errorf("no ready actions in plan")
}

// RecordResult records the result of executing an action.
func (tp *TreePlanner) RecordResult(tree *PlanTree, nodeID string, result *AgentResult) error {
	node, ok := tree.GetNode(nodeID)
	if !ok {
		return fmt.Errorf("node not found: %s", nodeID)
	}

	node.Result = result
	node.UpdatedAt = time.Now()

	if result.IsSuccess() {
		node.Status = PlanNodeSucceeded
		// Estimate goal progress based on position in plan
		if len(tree.BestPath) > 0 {
			for i, pathNode := range tree.BestPath {
				if pathNode.ID == nodeID {
					node.GoalProgress = float64(i+1) / float64(len(tree.BestPath))
					break
				}
			}
		}
	} else {
		node.Status = PlanNodeFailed
		node.Error = result.Error
	}

	// Update MCTS statistics
	reward := 0.0
	if result.IsSuccess() {
		reward = 1.0
	}
	tp.backpropagateReward(tree, node, reward)

	if tp.onNodeComplete != nil {
		tp.onNodeComplete(tree, node, result.IsSuccess())
	}

	tree.UpdatedAt = time.Now()

	return nil
}

// backpropagateReward updates MCTS statistics up the tree.
func (tp *TreePlanner) backpropagateReward(tree *PlanTree, node *PlanNode, reward float64) {
	current := node
	for current != nil {
		current.VisitCount++
		current.TotalReward += reward
		current.UpdatedAt = time.Now()

		if current.ParentID == "" {
			break
		}
		parent, ok := tree.GetNode(current.ParentID)
		if !ok {
			break
		}
		current = parent
	}
}

// ShouldReplan determines if replanning is needed after a failure.
func (tp *TreePlanner) ShouldReplan(tree *PlanTree, result *AgentResult) bool {
	if !tp.config.ReplanOnFailure {
		return false
	}

	if tree.ReplanCount >= tp.config.MaxReplans {
		return false
	}

	// Check if there are alternative paths available
	if tree.CurrentNode != nil {
		parent, ok := tree.GetParent(tree.CurrentNode.ID)
		if ok && len(parent.Children) > 1 {
			// There are siblings to try
			return true
		}
	}

	// Always allow replanning for failures within limit
	return !result.IsSuccess()
}

// Replan adjusts the plan after a failure.
func (tp *TreePlanner) Replan(ctx context.Context, tree *PlanTree, rctx *ReplanContext) error {
	if rctx.AttemptNumber >= tp.config.MaxReplans {
		return fmt.Errorf("max replans exceeded (%d)", tp.config.MaxReplans)
	}

	tree.ReplanCount++
	tree.UpdatedAt = time.Now()

	logging.Info("replanning",
		"tree_id", tree.ID,
		"attempt", rctx.AttemptNumber+1,
		"failed_node", rctx.FailedNode.ID)

	// 1. Prune the failed subtree
	if rctx.FailedNode != nil {
		tree.PruneSubtree(rctx.FailedNode.ID)
	}

	// 2. Update scores based on reflection
	if rctx.Reflection != nil && rctx.FailedNode != nil {
		tp.updateScoresAfterFailure(tree, rctx.FailedNode, rctx.Reflection)
	}

	// 3. Expand from parent of failed node
	parent, ok := tree.GetParent(rctx.FailedNode.ID)
	if !ok {
		// Failed at root, need to regenerate entire tree
		return fmt.Errorf("cannot replan from root failure")
	}

	newChildren, err := tp.ExpandNode(ctx, tree, parent, tree.Goal)
	if err != nil {
		logging.Warn("failed to expand alternatives", "error", err)
	}

	// 4. Boost alternatives suggested by reflection
	if rctx.Reflection != nil && rctx.Reflection.Alternative != "" {
		for _, child := range newChildren {
			if child.Action != nil && child.Action.ToolName == rctx.Reflection.Alternative {
				child.Score *= 1.2
			}
			if child.Action != nil && string(child.Action.AgentType) == rctx.Reflection.Alternative {
				child.Score *= 1.2
			}
		}
	}

	// 5. Recalculate best path
	tree.BestPath = tp.SelectBestPath(tree)

	if tp.onReplan != nil {
		tp.onReplan(tree, rctx)
	}

	return nil
}

// updateScoresAfterFailure adjusts node scores based on failure.
func (tp *TreePlanner) updateScoresAfterFailure(tree *PlanTree, failedNode *PlanNode, reflection *Reflection) {
	// Reduce confidence in similar actions
	var updateSimilar func(n *PlanNode)
	updateSimilar = func(n *PlanNode) {
		if n.Action != nil && failedNode.Action != nil {
			// Same action type and target
			if n.Action.Type == failedNode.Action.Type &&
				n.Action.AgentType == failedNode.Action.AgentType {
				n.SuccessProb *= 0.8 // 20% reduction
				n.Score = tp.ScoreNode(n)
			}
		}
		for _, child := range n.Children {
			updateSimilar(child)
		}
	}

	if tree.Root != nil {
		updateSimilar(tree.Root)
	}
}

// GetTree retrieves a tree by ID.
func (tp *TreePlanner) GetTree(treeID string) (*PlanTree, bool) {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	tree, ok := tp.trees[treeID]
	return tree, ok
}

// DeleteTree removes a tree from the planner.
func (tp *TreePlanner) DeleteTree(treeID string) {
	tp.mu.Lock()
	delete(tp.trees, treeID)
	tp.mu.Unlock()
}

// GetStats returns statistics about the planner's trees.
func (tp *TreePlanner) GetStats() map[string]any {
	tp.mu.RLock()
	defer tp.mu.RUnlock()

	totalNodes := 0
	totalReplans := 0
	for _, tree := range tp.trees {
		totalNodes += tree.TotalNodes
		totalReplans += tree.ReplanCount
	}

	return map[string]any{
		"active_trees":  len(tp.trees),
		"total_nodes":   totalNodes,
		"total_replans": totalReplans,
		"algorithm":     tp.config.Algorithm,
	}
}

// containsAny checks if the string contains any of the substrings.
func containsAny(s string, substrings []string) bool {
	for _, sub := range substrings {
		if contains(s, sub) {
			return true
		}
	}
	return false
}

// contains checks if s contains substr (case-insensitive).
func contains(s, substr string) bool {
	sLower := []rune(s)
	subLower := []rune(substr)

	for i := 0; i <= len(sLower)-len(subLower); i++ {
		match := true
		for j := 0; j < len(subLower); j++ {
			if toLower(sLower[i+j]) != toLower(subLower[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// toLower converts a rune to lowercase.
func toLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	// Handle Cyrillic uppercase
	if r >= 'А' && r <= 'Я' {
		return r + 32
	}
	return r
}

// generateActionsWithLLM uses the LLM to intelligently generate a plan.
func (tp *TreePlanner) generateActionsWithLLM(ctx context.Context, prompt string, goal *PlanGoal) ([]*PlannedAction, error) {
	if tp.client == nil {
		return nil, fmt.Errorf("no client available")
	}

	// Timeout for LLM call
	llmCtx, cancel := context.WithTimeout(ctx, llmPlanTimeout)
	defer cancel()

	// Build planning prompt
	planningPrompt := fmt.Sprintf(`You are a task planning assistant. Analyze the following task and create a step-by-step execution plan.

TASK: %s

Create a plan with 3-5 steps. For each step, specify:
1. The type of agent to use: "explore" (for reading/searching code), "plan" (for planning), "general" (for writing code), "bash" (for running commands)
2. A clear prompt for that step

Respond in this exact format (one step per line):
STEP: <agent_type> | <prompt for this step>

Example:
STEP: explore | Search for authentication-related files and understand the current implementation
STEP: plan | Design the new authentication flow based on the codebase patterns
STEP: general | Implement the planned changes to the authentication system
STEP: bash | Run tests to verify the implementation works correctly`, prompt)

	// Call LLM
	stream, err := tp.client.SendMessage(llmCtx, planningPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}

	resp, err := stream.Collect()
	if err != nil {
		return nil, fmt.Errorf("failed to collect response: %w", err)
	}

	// Clean markdown code blocks from response
	responseText := stripMarkdownBlocks(resp.Text)

	// Parse with validation
	actions := tp.parsePlanResponseValidated(responseText, prompt, goal)
	if len(actions) == 0 {
		return nil, fmt.Errorf("no valid actions parsed from LLM response")
	}

	return actions, nil
}

// parsePlanResponse parses the LLM response into planned actions.
func (tp *TreePlanner) parsePlanResponse(response string, originalPrompt string) []*PlannedAction {
	var actions []*PlannedAction

	lines := splitLines(response)
	for _, line := range lines {
		line = trimSpace(line)
		if !hasPrefix(line, "STEP:") {
			continue
		}

		// Parse: STEP: <agent_type> | <prompt>
		content := trimSpace(line[5:]) // Remove "STEP:"
		parts := splitByPipe(content)
		if len(parts) < 2 {
			continue
		}

		agentTypeStr := trimSpace(parts[0])
		stepPrompt := trimSpace(parts[1])

		agentType := ParseAgentType(agentTypeStr)
		if agentType == "" {
			agentType = AgentTypeGeneral
		}

		actions = append(actions, &PlannedAction{
			Type:      ActionDelegate,
			AgentType: agentType,
			Prompt:    stepPrompt,
		})
	}

	// Add verification step if not present
	if len(actions) > 0 {
		lastAction := actions[len(actions)-1]
		if lastAction.AgentType != AgentTypeBash && !containsAny(lastAction.Prompt, []string{"test", "verify", "тест", "проверь"}) {
			actions = append(actions, &PlannedAction{
				Type:   ActionVerify,
				Prompt: "Verify the implementation is complete and working correctly",
			})
		}
	}

	return actions
}

// stripMarkdownBlocks removes markdown code block markers from text.
func stripMarkdownBlocks(text string) string {
	result := text
	for {
		start := strings.Index(result, "```")
		if start == -1 {
			break
		}
		end := strings.Index(result[start+3:], "```")
		if end == -1 {
			break
		}
		// Extract content between markers
		blockContent := result[start+3 : start+3+end]
		// Remove first line if it's a language identifier (json, yaml, etc)
		if newline := strings.Index(blockContent, "\n"); newline != -1 {
			firstLine := strings.TrimSpace(blockContent[:newline])
			if len(firstLine) < 15 && !strings.Contains(firstLine, " ") {
				blockContent = blockContent[newline+1:]
			}
		}
		result = result[:start] + blockContent + result[start+3+end+3:]
	}
	return result
}

// parsePlanResponseValidated parses LLM response with validation and deduplication.
func (tp *TreePlanner) parsePlanResponseValidated(response string, originalPrompt string, goal *PlanGoal) []*PlannedAction {
	var actions []*PlannedAction
	seen := make(map[string]bool) // For deduplication

	lines := splitLines(response)
	for _, line := range lines {
		line = trimSpace(line)
		if !hasPrefix(line, "STEP:") {
			continue
		}

		// Limit on number of steps
		maxActions := maxPlanActions
		if goal != nil && goal.MaxDepth > 0 && goal.MaxDepth < maxActions {
			maxActions = goal.MaxDepth
		}
		if len(actions) >= maxActions {
			logging.Debug("plan action limit reached", "max", maxActions)
			break
		}

		content := trimSpace(line[5:])
		parts := splitByPipe(content)
		if len(parts) < 2 {
			logging.Debug("skipping malformed STEP line", "line", line)
			continue
		}

		agentTypeStr := trimSpace(parts[0])
		agentTypeStr = strings.Trim(agentTypeStr, "\"'") // Remove quotes
		stepPrompt := trimSpace(parts[1])

		// Validation: empty prompt
		if stepPrompt == "" {
			logging.Debug("skipping STEP with empty prompt", "line", line)
			continue
		}

		// Validation: truncate long prompts
		if len(stepPrompt) > maxPromptLength {
			stepPrompt = stepPrompt[:maxPromptLength] + "..."
			logging.Debug("truncated long prompt", "original_len", len(parts[1]))
		}

		// Validation: deduplication
		key := agentTypeStr + "|" + stepPrompt
		if seen[key] {
			logging.Debug("skipping duplicate STEP", "key", key)
			continue
		}
		seen[key] = true

		agentType := ParseAgentType(agentTypeStr)
		if agentType == "" {
			logging.Debug("unknown agent type, defaulting to general", "type", agentTypeStr)
			agentType = AgentTypeGeneral
		}

		actions = append(actions, &PlannedAction{
			Type:      ActionDelegate,
			AgentType: agentType,
			Prompt:    stepPrompt,
		})
	}

	// Add verification step if not present
	if len(actions) > 0 {
		lastAction := actions[len(actions)-1]
		if lastAction.AgentType != AgentTypeBash && !containsAny(lastAction.Prompt, []string{"test", "verify", "тест", "проверь"}) {
			actions = append(actions, &PlannedAction{
				Type:   ActionVerify,
				Prompt: "Verify the implementation is complete and working correctly",
			})
		}
	}

	return actions
}

// Helper functions for string parsing without importing strings package heavily
func splitLines(s string) []string {
	var lines []string
	var line []rune
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, string(line))
			line = nil
		} else {
			line = append(line, r)
		}
	}
	if len(line) > 0 {
		lines = append(lines, string(line))
	}
	return lines
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func hasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if toLower(rune(s[i])) != toLower(rune(prefix[i])) {
			return false
		}
	}
	return true
}

func splitByPipe(s string) []string {
	var parts []string
	var part []rune
	for _, r := range s {
		if r == '|' {
			parts = append(parts, string(part))
			part = nil
		} else {
			part = append(part, r)
		}
	}
	if len(part) > 0 {
		parts = append(parts, string(part))
	}
	return parts
}

// GeneratePlanSummary creates a concise summary of the plan for context injection.
// This is used when compacting context after plan approval.
func (tp *TreePlanner) GeneratePlanSummary(tree *PlanTree) string {
	if tree == nil || tree.Root == nil {
		return ""
	}

	var sb stringBuilder
	sb.WriteString("[Approved Plan Summary]\n")
	sb.WriteString("Goal: ")
	if tree.Goal != nil {
		sb.WriteString(tree.Goal.Description)
	} else {
		sb.WriteString("Task execution")
	}
	sb.WriteString("\n\n")

	sb.WriteString("Planned Steps:\n")
	for i, node := range tree.BestPath {
		if node.Action == nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("%d. ", i+1))
		switch node.Action.Type {
		case ActionToolCall:
			sb.WriteString(fmt.Sprintf("[Tool: %s] %s", node.Action.ToolName, node.Action.Prompt))
		case ActionDelegate:
			sb.WriteString(fmt.Sprintf("[Agent: %s] %s", node.Action.AgentType, node.Action.Prompt))
		case ActionVerify:
			sb.WriteString(fmt.Sprintf("[Verify] %s", node.Action.Prompt))
		case ActionDecompose:
			sb.WriteString(fmt.Sprintf("[Decompose] %s", node.Action.Prompt))
		default:
			sb.WriteString(node.Action.Prompt)
		}
		sb.WriteString(fmt.Sprintf(" (confidence: %.0f%%)", node.Score*100))
		sb.WriteString("\n")
	}

	sb.WriteString("\n[End of Plan Summary]")
	return sb.String()
}

// stringBuilder is a simple string builder for plan summaries.
type stringBuilder struct {
	data []byte
}

func (sb *stringBuilder) WriteString(s string) {
	sb.data = append(sb.data, s...)
}

func (sb *stringBuilder) String() string {
	return string(sb.data)
}
