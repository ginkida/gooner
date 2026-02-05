package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"gokin/internal/client"
	"gokin/internal/logging"
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
	maxPlanActions  = 10  // Maximum steps in a plan
	maxPromptLength = 500 // Maximum characters in step prompt
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
	PlanningTimeout time.Duration   `json:"planning_timeout"`

	// Scoring weights (should sum to 1.0)
	SuccessProbWeight float64 `json:"success_prob_weight"`
	CostWeight        float64 `json:"cost_weight"`
	ProgressWeight    float64 `json:"progress_weight"`

	// MCTS exploration constant
	ExplorationC float64 `json:"exploration_c"`

	// Dynamic LLM expansion
	UseLLMExpansion bool `json:"use_llm_expansion"`
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
		PlanningTimeout:   60 * time.Second,
		SuccessProbWeight: 0.4,
		CostWeight:        0.3,
		ProgressWeight:    0.3,
		ExplorationC:      1.414, // sqrt(2) - standard UCB1 constant
		UseLLMExpansion:   true,
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

	trees      map[string]*PlanTree
	lastTreeID string
	mu         sync.RWMutex

	// Callbacks for plan events
	onNodeStart    func(tree *PlanTree, node *PlanNode)
	onNodeComplete func(tree *PlanTree, node *PlanNode, success bool)
	onReplan       func(tree *PlanTree, ctx *ReplanContext)
	onProgress     func(action *PlannedAction)
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
	onProgress func(action *PlannedAction),
) {
	tp.onNodeStart = onNodeStart
	tp.onNodeComplete = onNodeComplete
	tp.onReplan = onReplan
	tp.onProgress = onProgress
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
				Type:      ActionDelegate,
				AgentType: AgentTypeGeneral,
				Prompt:    prompt,
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
	tp.lastTreeID = tree.ID
	tp.mu.Unlock()

	logging.Debug("plan tree built",
		"tree_id", tree.ID,
		"total_nodes", tree.TotalNodes,
		"best_path_length", len(tree.BestPath))

	return tree, nil
}

// generateActions generates candidate actions for a prompt.
// Uses pattern matching for common task types, with LLM fallback for complex tasks.
// Leverages StrategyOptimizer to prefer historically successful strategies.
func (tp *TreePlanner) generateActions(ctx context.Context, prompt string, goal *PlanGoal) ([]*PlannedAction, error) {
	actions := []*PlannedAction{}

	// Try LLM-based generation first if client is available
	if tp.client != nil {
		llmActions, err := tp.generateActionsWithLLM(ctx, prompt, goal)
		if err == nil && len(llmActions) > 0 {
			// Reorder actions based on historical success rates
			llmActions = tp.reorderByHistoricalSuccess(llmActions)
			return llmActions, nil
		}
		// Fall back to pattern matching on error
		logging.Debug("LLM action generation failed, using pattern matching", "error", err)
	}

	// Check if StrategyOptimizer has a recommendation for this task type
	if tp.strategyOpt != nil {
		taskType := tp.classifyTaskType(prompt)
		if taskType != "" {
			recommendedStrategy := tp.strategyOpt.RecommendStrategy(taskType)
			if recommendedStrategy != "" && recommendedStrategy != "general" {
				// Strategy optimizer has a recommendation - use it as the primary action
				logging.Debug("strategy optimizer recommendation",
					"task_type", taskType,
					"recommended", recommendedStrategy)

				// Add the recommended strategy action first
				recommendedAction := tp.buildActionFromStrategy(recommendedStrategy, prompt)
				if recommendedAction != nil {
					actions = append(actions, recommendedAction)
				}
			}
		}
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
		if tp.config.UseLLMExpansion && tp.client != nil {
			actions = tp.generateRecoveryActionsWithLLM(ctx, node, goal)
		} else {
			actions = tp.generateRecoveryActions(node)
		}
	case PlanNodeSucceeded:
		// Generate follow-up actions
		if tp.config.UseLLMExpansion && tp.client != nil {
			actions = tp.generateFollowUpActionsWithLLM(ctx, node, goal)
		} else {
			actions = tp.generateFollowUpActions(ctx, node, goal)
		}
	default:
		// Generate alternative approaches
		if tp.config.UseLLMExpansion && tp.client != nil {
			actions = tp.generateAlternativeActionsWithLLM(ctx, node, goal)
		} else {
			actions = tp.generateAlternativeActions(node, goal)
		}
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
		reflection := tp.reflector.Reflect(
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
		// Select node based on configured algorithm
		bestNode := tp.selectNodeByAlgorithm(readyNodes, tree)

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

// selectNodeByAlgorithm selects the best node based on the configured algorithm.
func (tp *TreePlanner) selectNodeByAlgorithm(nodes []*PlanNode, tree *PlanTree) *PlanNode {
	if len(nodes) == 0 {
		return nil
	}
	if len(nodes) == 1 {
		return nodes[0]
	}

	switch tp.config.Algorithm {
	case SearchAlgorithmMCTS:
		return tp.selectNodeMCTS(nodes, tree)
	case SearchAlgorithmAStar:
		return tp.selectNodeAStar(nodes, tree)
	case SearchAlgorithmBeam:
		fallthrough
	default:
		return tp.selectNodeBeam(nodes)
	}
}

// selectNodeBeam selects the highest-scored node (beam search style).
func (tp *TreePlanner) selectNodeBeam(nodes []*PlanNode) *PlanNode {
	var bestNode *PlanNode
	for _, node := range nodes {
		if bestNode == nil || node.Score > bestNode.Score {
			bestNode = node
		}
	}
	return bestNode
}

// selectNodeMCTS selects a node using UCB1 formula (exploration vs exploitation).
func (tp *TreePlanner) selectNodeMCTS(nodes []*PlanNode, tree *PlanTree) *PlanNode {
	// Calculate total visits for parent context
	totalVisits := 0
	for _, node := range nodes {
		totalVisits += node.VisitCount + 1 // +1 to avoid division by zero
	}

	var bestNode *PlanNode
	bestUCB := -1.0

	for _, node := range nodes {
		// UCB1 = exploitation + exploration
		// exploitation = average reward
		// exploration = C * sqrt(ln(total) / visits)
		visits := float64(node.VisitCount + 1)
		exploitation := 0.0
		if node.VisitCount > 0 {
			exploitation = node.TotalReward / float64(node.VisitCount)
		} else {
			exploitation = node.Score // Use initial score for unvisited nodes
		}

		exploration := tp.config.ExplorationC * math.Sqrt(math.Log(float64(totalVisits))/visits)
		ucb := exploitation + exploration

		if ucb > bestUCB {
			bestUCB = ucb
			bestNode = node
		}
	}

	return bestNode
}

// selectNodeAStar selects a node using A* heuristic (f = g + h).
func (tp *TreePlanner) selectNodeAStar(nodes []*PlanNode, tree *PlanTree) *PlanNode {
	var bestNode *PlanNode
	bestF := -1.0

	for _, node := range nodes {
		// g = cost so far (depth in tree, normalized)
		g := 1.0 - (float64(node.Depth) / float64(tp.config.MaxTreeDepth))

		// h = heuristic estimate (use score as estimate of remaining value)
		h := node.Score

		// f = g + h (we want to maximize, so higher is better)
		f := g*tp.config.CostWeight + h*tp.config.SuccessProbWeight

		if f > bestF {
			bestF = f
			bestNode = node
		}
	}

	return bestNode
}

// GetReadyActions returns all currently ready independent actions.
func (tp *TreePlanner) GetReadyActions(tree *PlanTree) ([]*PlannedAction, error) {
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("invalid plan tree")
	}

	tree.mu.Lock()
	defer tree.mu.Unlock()

	readyNodes := tree.GetReadyNodes()
	if len(readyNodes) == 0 {
		return nil, nil // No ready actions available
	}

	var actions []*PlannedAction
	for _, node := range readyNodes {
		node.Status = PlanNodeExecuting
		node.UpdatedAt = time.Now()

		// For parallel execution, the concept of a single "current" node is less applicable,
		// but we update it to the last retrieved node for backward compatibility.
		tree.CurrentNode = node

		if tp.onNodeStart != nil {
			tp.onNodeStart(tree, node)
		}

		actions = append(actions, node.Action)
	}

	return actions, nil
}

// RecordResult records the result of executing an action.
func (tp *TreePlanner) RecordResult(tree *PlanTree, nodeID string, result *AgentResult) error {
	tree.mu.Lock()
	defer tree.mu.Unlock()

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

// generateFollowUpActionsWithLLM uses LLM to suggest next steps after success.
func (tp *TreePlanner) generateFollowUpActionsWithLLM(ctx context.Context, node *PlanNode, goal *PlanGoal) []*PlannedAction {
	prompt := fmt.Sprintf(`Given the overall goal: "%s"
We have successfully completed the previous step: "%s" (Agent: %s)
Output of previous step: %s

Suggest 2-3 logical next steps to continue toward the goal.
Respond in the same language as the goal description.
Respond in the STEP format:
STEP: <explore|plan|general|bash|decompose> | <prompt>`, goal.Description, node.Action.Prompt, node.Action.AgentType, node.Result.Output)

	actions, err := tp.generateActionsWithLLM(ctx, prompt, goal)
	if err != nil {
		logging.Warn("LLM follow-up generation failed, falling back", "error", err)
		return tp.generateFollowUpActions(ctx, node, goal)
	}
	return actions
}

// generateAlternativeActionsWithLLM uses LLM to suggest alternative ways.
func (tp *TreePlanner) generateAlternativeActionsWithLLM(ctx context.Context, node *PlanNode, goal *PlanGoal) []*PlannedAction {
	prompt := fmt.Sprintf(`The overall goal is: "%s"
We are considering this approach: "%s" (Agent: %s)

Suggest 2 alternative approaches or different next steps to achieve the same goal.
Respond in the same language as the goal description.
Respond in the STEP format:
STEP: <explore|plan|general|bash|decompose> | <prompt>`, goal.Description, node.Action.Prompt, node.Action.AgentType)

	actions, err := tp.generateActionsWithLLM(ctx, prompt, goal)
	if err != nil {
		logging.Warn("LLM alternative generation failed, falling back", "error", err)
		return tp.generateAlternativeActions(node, goal)
	}
	return actions
}

// generateRecoveryActionsWithLLM uses LLM to suggest recovery steps after failure.
func (tp *TreePlanner) generateRecoveryActionsWithLLM(ctx context.Context, node *PlanNode, goal *PlanGoal) []*PlannedAction {
	errorMsg := node.Error
	if node.Result != nil && node.Result.Error != "" {
		errorMsg = node.Result.Error
	}

	prompt := fmt.Sprintf(`The overall goal is: "%s"
The step: "%s" (Agent: %s) FAILED with error: %s

Analyze the error and suggest 2-3 recovery steps or alternative approaches to overcome this failure.
Respond in the same language as the goal description.
Respond in the STEP format:
STEP: <explore|plan|general|bash|decompose> | <prompt>`, goal.Description, node.Action.Prompt, node.Action.AgentType, errorMsg)

	actions, err := tp.generateActionsWithLLM(ctx, prompt, goal)
	if err != nil {
		logging.Warn("LLM recovery generation failed, falling back", "error", err)
		return tp.generateRecoveryActions(node)
	}
	return actions
}

// ExpandMilestone expands a decompose node into a sub-plan.
func (tp *TreePlanner) ExpandMilestone(ctx context.Context, tree *PlanTree, node *PlanNode) error {
	if node.Action == nil || node.Action.Type != ActionDecompose {
		return fmt.Errorf("node is not a decompose node")
	}

	logging.Info("expanding milestone", "node_id", node.ID, "prompt", node.Action.Prompt)

	// Call LLM to generate sub-actions
	goal := &PlanGoal{
		Description: node.Action.Prompt,
	}
	actions, err := tp.generateActionsWithLLM(ctx, node.Action.Prompt, goal)
	if err != nil {
		return fmt.Errorf("failed to generate sub-actions: %w", err)
	}

	// Add actions as children of the milestone node
	for _, action := range actions {
		tree.AddNode(node.ID, action)
	}

	// Recalculate best path to include new children
	tree.BestPath = tp.SelectBestPath(tree)

	return nil
}

// Replan adjusts the plan after a failure.
func (tp *TreePlanner) Replan(ctx context.Context, tree *PlanTree, rctx *ReplanContext) error {
	if rctx.AttemptNumber >= tp.config.MaxReplans {
		return fmt.Errorf("max replans exceeded (%d)", tp.config.MaxReplans)
	}

	// FailedNode is required for replanning
	if rctx.FailedNode == nil {
		return fmt.Errorf("cannot replan: FailedNode is nil")
	}

	tree.ReplanCount++
	tree.UpdatedAt = time.Now()

	logging.Info("replanning",
		"tree_id", tree.ID,
		"attempt", rctx.AttemptNumber+1,
		"failed_node", rctx.FailedNode.ID)

	// 1. Prune the failed subtree
	tree.PruneSubtree(rctx.FailedNode.ID)

	// 2. Update scores based on reflection
	if rctx.Reflection != nil {
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

// GetActiveTree returns the most recently built or accessed tree.
func (tp *TreePlanner) GetActiveTree() *PlanTree {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	if tp.lastTreeID == "" {
		return nil
	}
	return tp.trees[tp.lastTreeID]
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

// reorderByHistoricalSuccess reorders actions based on historical success rates from StrategyOptimizer.
func (tp *TreePlanner) reorderByHistoricalSuccess(actions []*PlannedAction) []*PlannedAction {
	if tp.strategyOpt == nil || len(actions) == 0 {
		return actions
	}

	// Create a scored list
	type scoredAction struct {
		action *PlannedAction
		score  float64
	}

	scored := make([]scoredAction, len(actions))
	for i, action := range actions {
		key := buildStrategyKey(action)
		rate := tp.strategyOpt.GetSuccessRate(key)
		// Default score of 0.5 for unknown strategies
		if rate <= 0 || rate >= 1 {
			rate = 0.5
		}
		scored[i] = scoredAction{action: action, score: rate}
	}

	// Simple bubble sort by score (descending)
	for i := 0; i < len(scored)-1; i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[i].score {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}

	// Extract reordered actions
	result := make([]*PlannedAction, len(actions))
	for i, sa := range scored {
		result[i] = sa.action
	}

	return result
}

// classifyTaskType determines the task type from a prompt for strategy lookup.
func (tp *TreePlanner) classifyTaskType(prompt string) string {
	promptLower := strings.ToLower(prompt)

	if containsAny(promptLower, []string{"implement", "create", "add", "build", "write", "реализ", "созда", "добав"}) {
		return "implementation"
	}
	if containsAny(promptLower, []string{"fix", "bug", "error", "debug", "исправ", "баг", "ошибк"}) {
		return "bugfix"
	}
	if containsAny(promptLower, []string{"refactor", "clean", "improve", "optimize", "рефактор", "улучш", "оптимиз"}) {
		return "refactoring"
	}
	if containsAny(promptLower, []string{"test", "тест"}) {
		return "testing"
	}
	if containsAny(promptLower, []string{"explain", "what", "how", "why", "where", "объясн", "что", "как", "почему", "где"}) {
		return "exploration"
	}
	if containsAny(promptLower, []string{"document", "doc", "readme", "документ"}) {
		return "documentation"
	}

	return ""
}

// buildActionFromStrategy creates a PlannedAction from a strategy name.
func (tp *TreePlanner) buildActionFromStrategy(strategy string, prompt string) *PlannedAction {
	switch {
	case strings.HasPrefix(strategy, "delegate:"):
		agentType := AgentType(strings.TrimPrefix(strategy, "delegate:"))
		return &PlannedAction{
			Type:      ActionDelegate,
			AgentType: agentType,
			Prompt:    prompt,
		}
	case strings.HasPrefix(strategy, "tool:"):
		toolName := strings.TrimPrefix(strategy, "tool:")
		return &PlannedAction{
			Type:     ActionToolCall,
			ToolName: toolName,
			Prompt:   prompt,
		}
	case strategy == "decompose":
		return &PlannedAction{
			Type:   ActionDecompose,
			Prompt: prompt,
		}
	case strategy == "verify":
		return &PlannedAction{
			Type:   ActionVerify,
			Prompt: prompt,
		}
	default:
		// Try to match as agent type
		switch AgentType(strategy) {
		case AgentTypeExplore, AgentTypeBash, AgentTypeGeneral, AgentTypePlan, AgentTypeGuide:
			return &PlannedAction{
				Type:      ActionDelegate,
				AgentType: AgentType(strategy),
				Prompt:    prompt,
			}
		}
	}

	return nil
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
	timeout := tp.config.PlanningTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	llmCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build planning prompt
	planningPrompt := fmt.Sprintf(`You are a task planning assistant. Analyze the following task and create a step-by-step execution plan.

TASK: %s

Create a plan with 3-5 steps. For each step, specify:
1. The type of agent to use: "explore" (for reading/searching code), "plan" (for planning), "general" (for writing code), "bash" (for running commands), "decompose" (for broad subtasks that need further planning)
2. A clear prompt for that step

Respond in this exact format (one step per line):
STEP: <agent_type> | <prompt for this step>

Respond in the same language as the task description.

Example:
STEP: explore | Search for authentication-related files and understand the current implementation
STEP: plan | Design the new authentication flow based on the codebase patterns
STEP: decompose | Implement the core JWT authentication logic
STEP: bash | Run tests to verify the implementation works correctly`, prompt)

	// Call LLM
	stream, err := tp.client.SendMessage(llmCtx, planningPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}

	var actions []*PlannedAction
	var fullText strings.Builder
	var incompleteLine string

	for {
		select {
		case <-llmCtx.Done():
			if len(actions) > 0 {
				return actions, nil // Return partial plan on timeout
			}
			return nil, llmCtx.Err()
		case chunk, ok := <-stream.Chunks:
			if !ok {
				goto Done
			}
			if chunk.Error != nil {
				return nil, chunk.Error
			}

			chunkText := chunk.Text
			fullText.WriteString(chunkText)

			// Simple line-based streaming parser
			textToParse := incompleteLine + chunkText
			lines := strings.Split(textToParse, "\n")

			// Keep the last (potentially incomplete) line
			if !strings.HasSuffix(textToParse, "\n") {
				incompleteLine = lines[len(lines)-1]
				lines = lines[:len(lines)-1]
			} else {
				incompleteLine = ""
			}

			for _, line := range lines {
				parsed := tp.parsePlanResponseValidated(line, prompt, goal)
				for _, action := range parsed {
					// Check for duplicates
					isDup := false
					for _, existing := range actions {
						if existing.Prompt == action.Prompt && existing.AgentType == action.AgentType {
							isDup = true
							break
						}
					}
					if !isDup {
						actions = append(actions, action)
						if tp.onProgress != nil {
							tp.onProgress(action)
						}
					}
				}
			}
		}
	}

Done:
	// Process remaining text
	remaining := strings.TrimSpace(incompleteLine)
	if remaining != "" {
		parsed := tp.parsePlanResponseValidated(remaining, prompt, goal)
		for _, action := range parsed {
			isDup := false
			for _, existing := range actions {
				if existing.Prompt == action.Prompt && existing.AgentType == action.AgentType {
					isDup = true
					break
				}
			}
			if !isDup {
				actions = append(actions, action)
				if tp.onProgress != nil {
					tp.onProgress(action)
				}
			}
		}
	}

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
		actionType := ActionDelegate
		if agentTypeStr == "decompose" {
			actionType = ActionDecompose
			agentType = AgentTypePlan // Use plan agent for decomposition
		}

		if agentType == "" && actionType != ActionDecompose {
			logging.Debug("unknown agent type, defaulting to general", "type", agentTypeStr)
			agentType = AgentTypeGeneral
		}

		actions = append(actions, &PlannedAction{
			Type:      actionType,
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

// GenerateVisualTree returns an ASCII representation of the plan tree.
func (tp *TreePlanner) GenerateVisualTree(tree *PlanTree) string {
	if tree == nil || tree.Root == nil {
		return "No active plan tree"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ROOT: %s\n", tree.Goal.Description))

	var renderNode func(node *PlanNode, prefix string, isLast bool)
	renderNode = func(node *PlanNode, prefix string, isLast bool) {
		if node == tree.Root {
			for i, child := range node.Children {
				renderNode(child, "", i == len(node.Children)-1)
			}
			return
		}

		// Choose branch symbol
		branch := "├── "
		if isLast {
			branch = "└── "
		}

		// Node info
		status := " "
		switch node.Status {
		case PlanNodeSucceeded:
			status = "✅"
		case PlanNodeFailed:
			status = "❌"
		case PlanNodeExecuting:
			status = "⏳"
		case PlanNodePruned:
			status = "✂️"
		}

		actionDesc := "Unknown Action"
		if node.Action != nil {
			switch node.Action.Type {
			case ActionToolCall:
				actionDesc = fmt.Sprintf("[Tool: %s] %s", node.Action.ToolName, node.Action.Prompt)
			case ActionDelegate:
				actionDesc = fmt.Sprintf("[%s] %s", node.Action.AgentType, node.Action.Prompt)
			case ActionVerify:
				actionDesc = fmt.Sprintf("[Verify] %s", node.Action.Prompt)
			case ActionDecompose:
				actionDesc = fmt.Sprintf("[Decompose] %s", node.Action.Prompt)
			default:
				actionDesc = node.Action.Prompt
			}
		}

		sb.WriteString(fmt.Sprintf("%s%s%s %s (score: %.2f)\n", prefix, branch, status, actionDesc, node.Score))

		// New prefix for children
		newPrefix := prefix
		if isLast {
			newPrefix += "    "
		} else {
			newPrefix += "│   "
		}

		// Render children
		for i, child := range node.Children {
			renderNode(child, newPrefix, i == len(node.Children)-1)
		}
	}

	renderNode(tree.Root, "", true)

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
