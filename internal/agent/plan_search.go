package agent

import (
	"math"
	"math/rand"
	"sort"
)

// SelectBestPath selects the best path through the plan tree using the configured algorithm.
func (tp *TreePlanner) SelectBestPath(tree *PlanTree) []*PlanNode {
	if tree == nil || tree.Root == nil {
		return nil
	}

	switch tp.config.Algorithm {
	case SearchAlgorithmMCTS:
		return tp.mctsSelectBestPath(tree)
	case SearchAlgorithmAStar:
		return tp.astarSearch(tree)
	case SearchAlgorithmBeam:
		fallthrough
	default:
		return tp.beamSearch(tree)
	}
}

// beamSearch performs beam search to find the best path.
// It maintains a fixed-width beam of the best candidates at each level.
func (tp *TreePlanner) beamSearch(tree *PlanTree) []*PlanNode {
	if tree.Root == nil {
		return nil
	}

	beam := []*PlanNode{tree.Root}
	var bestPath []*PlanNode

	for len(beam) > 0 {
		candidates := make([]*PlanNode, 0)

		for _, node := range beam {
			// Check if this node completes the goal
			if tp.isGoalNode(node, tree.Goal) {
				path := tree.ReconstructPath(node.ID)
				if bestPath == nil || pathScore(path) > pathScore(bestPath) {
					bestPath = path
				}
				continue
			}

			// Add children as candidates
			for _, child := range node.Children {
				if child.Status != PlanNodePruned {
					candidates = append(candidates, child)
				}
			}
		}

		if len(candidates) == 0 {
			break
		}

		// Sort by score (highest first)
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Score > candidates[j].Score
		})

		// Keep only top beam_width candidates
		if len(candidates) > tp.config.BeamWidth {
			candidates = candidates[:tp.config.BeamWidth]
		}

		beam = candidates
	}

	// If no goal was found, return the highest-scoring path
	if bestPath == nil && tree.Root != nil {
		bestPath = tp.findHighestScoringPath(tree.Root)
	}

	return bestPath
}

// findHighestScoringPath finds the path to the highest-scoring leaf.
func (tp *TreePlanner) findHighestScoringPath(root *PlanNode) []*PlanNode {
	var bestLeaf *PlanNode
	var bestScore float64 = -1

	var findBest func(n *PlanNode, pathScore float64)
	findBest = func(n *PlanNode, cumulativeScore float64) {
		newScore := cumulativeScore + n.Score

		if n.IsLeaf() || len(n.Children) == 0 {
			if newScore > bestScore {
				bestScore = newScore
				bestLeaf = n
			}
			return
		}

		for _, child := range n.Children {
			if child.Status != PlanNodePruned {
				findBest(child, newScore)
			}
		}
	}

	findBest(root, 0)

	if bestLeaf == nil {
		return []*PlanNode{root}
	}

	// Reconstruct path from root to best leaf
	var path []*PlanNode
	current := bestLeaf
	for current != nil {
		path = append([]*PlanNode{current}, path...)
		if current.ParentID == "" {
			break
		}
		// Find parent
		var parent *PlanNode
		var findParent func(n *PlanNode) bool
		findParent = func(n *PlanNode) bool {
			if n.ID == current.ParentID {
				parent = n
				return true
			}
			for _, child := range n.Children {
				if findParent(child) {
					return true
				}
			}
			return false
		}
		findParent(root)
		current = parent
	}

	return path
}

// mctsSelectBestPath uses MCTS statistics to select the best path.
func (tp *TreePlanner) mctsSelectBestPath(tree *PlanTree) []*PlanNode {
	if tree.Root == nil {
		return nil
	}

	var path []*PlanNode
	current := tree.Root

	for current != nil {
		path = append(path, current)

		if len(current.Children) == 0 {
			break
		}

		// Select child with highest visit count (exploitation)
		var bestChild *PlanNode
		bestVisits := -1

		for _, child := range current.Children {
			if child.Status == PlanNodePruned {
				continue
			}
			if child.VisitCount > bestVisits {
				bestVisits = child.VisitCount
				bestChild = child
			}
		}

		if bestChild == nil {
			break
		}
		current = bestChild
	}

	return path
}

// mctsSelect selects a node for expansion using UCB1.
func (tp *TreePlanner) mctsSelect(root *PlanNode) *PlanNode {
	current := root

	for !current.IsLeaf() && !current.IsTerminal() {
		// Find child with highest UCB1 score
		var bestChild *PlanNode
		bestUCB := math.Inf(-1)

		for _, child := range current.Children {
			if child.Status == PlanNodePruned {
				continue
			}

			ucb := tp.ucb1Score(child, current)
			if ucb > bestUCB {
				bestUCB = ucb
				bestChild = child
			}
		}

		if bestChild == nil {
			break
		}
		current = bestChild
	}

	return current
}

// ucb1Score calculates the UCB1 score for MCTS node selection.
// UCB1 balances exploitation (nodes with high rewards) and exploration (less visited nodes).
func (tp *TreePlanner) ucb1Score(node, parent *PlanNode) float64 {
	if node.VisitCount == 0 {
		return math.Inf(1) // Prioritize unvisited nodes
	}

	exploitation := node.AverageReward()
	exploration := tp.config.ExplorationC * math.Sqrt(math.Log(float64(parent.VisitCount))/float64(node.VisitCount))

	return exploitation + exploration
}

// mctsExpand expands a node by adding children.
func (tp *TreePlanner) mctsExpand(tree *PlanTree, node *PlanNode) *PlanNode {
	if node.IsTerminal() {
		return node
	}

	// If node has no children, we can't expand further without context
	if len(node.Children) == 0 {
		return node
	}

	// Return a random unvisited child
	for _, child := range node.Children {
		if child.VisitCount == 0 && child.Status != PlanNodePruned {
			return child
		}
	}

	// All children visited, return the node itself
	return node
}

// mctsSimulate runs an enhanced simulation using strategy metrics and rollout.
func (tp *TreePlanner) mctsSimulate(node *PlanNode) float64 {
	if node == nil {
		return 0.5
	}

	// 1. Base score through StrategyOptimizer
	var baseScore float64
	if node.Action != nil {
		baseScore = tp.EstimateSuccessProbability(node.Action)

		// Contextual bonuses by action type
		switch node.Action.Type {
		case ActionDelegate:
			switch node.Action.AgentType {
			case AgentTypeExplore:
				if node.Depth <= 2 {
					baseScore += 0.05 // Exploration is effective early
				}
			case AgentTypePlan:
				if node.Depth <= 1 {
					baseScore += 0.05 // Planning is better at start
				}
			case AgentTypeBash:
				if node.GoalProgress >= 0.7 {
					baseScore += 0.05 // Tests are important near end
				}
			}
		case ActionVerify:
			if node.GoalProgress >= 0.8 {
				baseScore = 0.9 // High success chance for verification
			}
		}
	} else {
		baseScore = node.SuccessProb
	}

	// 2. Exponential depth discount
	depthDiscount := math.Pow(0.95, float64(node.Depth))

	// 3. Bonus for goal progress
	progressBonus := node.GoalProgress * 0.3

	// 4. Stochastic component for exploration
	randomFactor := 0.05 * (rand.Float64() - 0.5) // ±2.5%

	// 5. Exploration bonus for rarely visited nodes
	explorationBonus := 0.0
	if node.VisitCount < 5 {
		explorationBonus = 0.1 * (1.0 - float64(node.VisitCount)/5.0)
	}

	// Final score
	score := baseScore*depthDiscount + progressBonus + randomFactor + explorationBonus

	// Clamp to [0, 1]
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	return score
}

// mctsRollout performs a lightweight simulation from node with lookahead.
func (tp *TreePlanner) mctsRollout(node *PlanNode, maxDepth int) float64 {
	if node == nil || maxDepth <= 0 {
		return tp.mctsSimulate(node)
	}

	// If no children - evaluate current node
	if len(node.Children) == 0 {
		return tp.mctsSimulate(node)
	}

	// Epsilon-greedy: 80% best, 20% random
	var selected *PlanNode
	if rand.Float64() < 0.8 {
		// Select best child by score
		selected = node.Children[0]
		for _, child := range node.Children {
			if child.Score > selected.Score {
				selected = child
			}
		}
	} else {
		// Random selection
		selected = node.Children[rand.Intn(len(node.Children))]
	}

	// Recursive rollout
	return tp.mctsRollout(selected, maxDepth-1)
}

// mctsBackpropagate updates statistics from the simulated node up to the root.
func (tp *TreePlanner) mctsBackpropagate(tree *PlanTree, node *PlanNode, reward float64) {
	current := node
	for current != nil {
		current.VisitCount++
		current.TotalReward += reward

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

// RunMCTS runs the full MCTS algorithm for the specified number of iterations.
func (tp *TreePlanner) RunMCTS(tree *PlanTree) []*PlanNode {
	if tree.Root == nil {
		return nil
	}

	for i := 0; i < tp.config.MCTSIterations; i++ {
		// Selection
		selected := tp.mctsSelect(tree.Root)

		// Expansion
		if !selected.IsTerminal() {
			selected = tp.mctsExpand(tree, selected)
		}

		// Simulation with adaptive depth
		simDepth := 2
		if selected.GoalProgress < 0.5 {
			simDepth = 3 // Deeper for early nodes
		}

		var reward float64
		if len(selected.Children) > 0 {
			reward = tp.mctsRollout(selected, simDepth)
		} else {
			reward = tp.mctsSimulate(selected)
		}

		// Backpropagation
		tp.mctsBackpropagate(tree, selected, reward)
	}

	return tp.mctsSelectBestPath(tree)
}

// astarSearch performs A* search to find the optimal path.
func (tp *TreePlanner) astarSearch(tree *PlanTree) []*PlanNode {
	if tree.Root == nil {
		return nil
	}

	type astarNode struct {
		node     *PlanNode
		gScore   float64 // Cost from start to this node
		fScore   float64 // gScore + heuristic
		cameFrom *astarNode
	}

	// Open set (nodes to explore)
	openSet := []*astarNode{{
		node:   tree.Root,
		gScore: 0,
		fScore: tp.heuristic(tree.Root, tree.Goal),
	}}

	// Closed set (already explored nodes)
	closedSet := make(map[string]bool)

	var bestPath *astarNode

	for len(openSet) > 0 {
		// Find node with lowest fScore
		sort.Slice(openSet, func(i, j int) bool {
			return openSet[i].fScore < openSet[j].fScore
		})

		current := openSet[0]
		openSet = openSet[1:]

		if closedSet[current.node.ID] {
			continue
		}
		closedSet[current.node.ID] = true

		// Check if goal
		if tp.isGoalNode(current.node, tree.Goal) {
			bestPath = current
			break
		}

		// Explore children
		for _, child := range current.node.Children {
			if closedSet[child.ID] || child.Status == PlanNodePruned {
				continue
			}

			// Cost is inverse of score (lower score = higher cost)
			edgeCost := 1.0 - child.Score
			if edgeCost < 0.1 {
				edgeCost = 0.1
			}

			tentativeG := current.gScore + edgeCost

			childNode := &astarNode{
				node:     child,
				gScore:   tentativeG,
				fScore:   tentativeG + tp.heuristic(child, tree.Goal),
				cameFrom: current,
			}

			openSet = append(openSet, childNode)
		}
	}

	if bestPath == nil {
		// No goal found, return path to best node
		return tp.findHighestScoringPath(tree.Root)
	}

	// Reconstruct path
	var path []*PlanNode
	current := bestPath
	for current != nil {
		path = append([]*PlanNode{current.node}, path...)
		current = current.cameFrom
	}

	return path
}

// heuristic estimates the cost to reach the goal from a node.
func (tp *TreePlanner) heuristic(node *PlanNode, goal *PlanGoal) float64 {
	// Base heuristic: inverse of success probability
	h := 1.0 - node.SuccessProb

	// Penalize depth remaining
	if goal != nil && goal.MaxDepth > 0 {
		depthRemaining := float64(goal.MaxDepth - node.Depth)
		if depthRemaining > 0 {
			h += 0.1 * depthRemaining
		}
	}

	// Bonus for progress (less distance to goal)
	h -= node.GoalProgress * 0.5

	if h < 0 {
		h = 0
	}

	return h
}

// isGoalNode checks if a node satisfies the goal criteria.
func (tp *TreePlanner) isGoalNode(node *PlanNode, goal *PlanGoal) bool {
	if node == nil || goal == nil {
		return false
	}

	// Check if node represents verification step
	if node.Action != nil && node.Action.Type == ActionVerify {
		return node.Status == PlanNodeSucceeded
	}

	// Check goal progress
	if node.GoalProgress >= 1.0 {
		return true
	}

	// Check if node is a successful leaf with high progress
	if node.IsLeaf() && node.Status == PlanNodeSucceeded && node.GoalProgress >= 0.9 {
		return true
	}

	return false
}

// pathScore calculates the total score of a path.
func pathScore(path []*PlanNode) float64 {
	if len(path) == 0 {
		return 0
	}

	total := 0.0
	for _, node := range path {
		total += node.Score
	}

	// Normalize by path length (prefer shorter paths with same total)
	return total / float64(len(path))
}
