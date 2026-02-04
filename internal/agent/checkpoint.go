package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// AgentCheckpoint represents a complete agent state for resumption.
type AgentCheckpoint struct {
	// Core state
	AgentState *AgentState `json:"agent_state"`

	// Extended state
	SharedMemorySnapshot map[string]*SharedEntry `json:"shared_memory,omitempty"`
	PlanTreeSnapshot     *SerializedPlanTree     `json:"plan_tree,omitempty"`
	ReflectorState       *ReflectorSnapshot      `json:"reflector,omitempty"`
	ScratchpadContent    string                  `json:"scratchpad,omitempty"`

	// Metadata
	Timestamp     time.Time `json:"timestamp"`
	CheckpointID  string    `json:"checkpoint_id"`
	TriggerReason string    `json:"trigger_reason"` // "auto", "manual", "error"
	TurnNumber    int       `json:"turn_number"`
}

// SerializedPlanTree represents a serializable plan tree.
type SerializedPlanTree struct {
	RootID      string                      `json:"root_id"`
	Nodes       map[string]*SerializedPNode `json:"nodes"`
	CurrentPath []string                    `json:"current_path"`
	TotalNodes  int                         `json:"total_nodes"`
	Goal        string                      `json:"goal,omitempty"`
}

// SerializedPNode represents a serializable plan node.
type SerializedPNode struct {
	ID         string            `json:"id"`
	Action     *SerializedAction `json:"action"`
	Status     string            `json:"status"`
	Children   []string          `json:"children"`
	Result     string            `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
	Confidence float64           `json:"confidence"`
}

// SerializedAction represents a serializable planned action.
type SerializedAction struct {
	Type      string         `json:"type"`
	AgentType string         `json:"agent_type"`
	Prompt    string         `json:"prompt"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolArgs  map[string]any `json:"tool_args,omitempty"`
}

// ReflectorSnapshot captures reflection history for persistence.
type ReflectorSnapshot struct {
	RecentErrors []string `json:"recent_errors"`
	LearnedFixes []string `json:"learned_fixes"`
}

// SaveCheckpoint creates and persists a checkpoint.
func (a *Agent) SaveCheckpoint(reason string) (*AgentCheckpoint, error) {
	cp := &AgentCheckpoint{
		AgentState:        a.GetState(),
		Timestamp:         time.Now(),
		CheckpointID:      fmt.Sprintf("%s-%d", a.ID, time.Now().UnixNano()),
		TriggerReason:     reason,
		TurnNumber:        a.GetTurnCount(),
		ScratchpadContent: a.Scratchpad,
	}

	// Capture shared memory if available
	if a.sharedMemory != nil {
		cp.SharedMemorySnapshot = a.exportSharedMemory()
	}

	// Capture plan tree if active
	if a.activePlan != nil {
		cp.PlanTreeSnapshot = serializePlanTree(a.activePlan)
	}

	// Capture reflector state if available
	if a.reflector != nil {
		cp.ReflectorState = a.reflector.Snapshot()
	}

	// Persist to store if available
	if a.store != nil {
		if err := a.store.SaveCheckpoint(cp); err != nil {
			return cp, fmt.Errorf("failed to persist checkpoint: %w", err)
		}
	}

	return cp, nil
}

// RestoreFromCheckpoint restores agent state from checkpoint.
func (a *Agent) RestoreFromCheckpoint(cp *AgentCheckpoint) error {
	// Restore core history and state
	if err := a.RestoreHistory(cp.AgentState); err != nil {
		return fmt.Errorf("failed to restore history: %w", err)
	}

	// Restore shared memory
	if cp.SharedMemorySnapshot != nil && a.sharedMemory != nil {
		a.importSharedMemory(cp.SharedMemorySnapshot)
	}

	// Restore plan tree
	if cp.PlanTreeSnapshot != nil {
		a.activePlan = deserializePlanTree(cp.PlanTreeSnapshot)
		if a.activePlan != nil {
			a.planningMode = true
		}
	}

	// Restore reflector state
	if cp.ReflectorState != nil && a.reflector != nil {
		a.reflector.Restore(cp.ReflectorState)
	}

	// Restore scratchpad
	a.Scratchpad = cp.ScratchpadContent

	return nil
}

// exportSharedMemory exports shared memory entries for checkpointing.
func (a *Agent) exportSharedMemory() map[string]*SharedEntry {
	entries := a.sharedMemory.ReadAll()
	result := make(map[string]*SharedEntry, len(entries))
	for _, entry := range entries {
		result[entry.Key] = entry
	}
	return result
}

// importSharedMemory imports shared memory entries from checkpoint.
func (a *Agent) importSharedMemory(snapshot map[string]*SharedEntry) {
	for key, entry := range snapshot {
		// Skip expired entries
		if entry.IsExpired() {
			continue
		}
		a.sharedMemory.WriteWithTTL(key, entry.Value, entry.Type, entry.Source, entry.TTL)
	}
}

// serializePlanTree converts a PlanTree to serializable form.
func serializePlanTree(tree *PlanTree) *SerializedPlanTree {
	if tree == nil || tree.Root == nil {
		return nil
	}

	st := &SerializedPlanTree{
		RootID:     tree.Root.ID,
		Nodes:      make(map[string]*SerializedPNode),
		TotalNodes: tree.TotalNodes,
	}

	// Serialize current path
	for _, node := range tree.BestPath {
		st.CurrentPath = append(st.CurrentPath, node.ID)
	}

	// Serialize all nodes
	serializeNode(tree.Root, st.Nodes)

	return st
}

// serializeNode recursively serializes a plan node and its children.
func serializeNode(node *PlanNode, nodes map[string]*SerializedPNode) {
	if node == nil {
		return
	}

	sn := &SerializedPNode{
		ID:         node.ID,
		Status:     string(node.Status),
		Confidence: node.Score, // Use Score field as Confidence
	}

	if node.Action != nil {
		sn.Action = &SerializedAction{
			Type:      string(node.Action.Type),
			AgentType: string(node.Action.AgentType),
			Prompt:    node.Action.Prompt,
			ToolName:  node.Action.ToolName,
			ToolArgs:  node.Action.ToolArgs,
		}
	}

	if node.Result != nil {
		sn.Result = node.Result.Output
		sn.Error = node.Result.Error
	}

	for _, child := range node.Children {
		sn.Children = append(sn.Children, child.ID)
		serializeNode(child, nodes)
	}

	nodes[node.ID] = sn
}

// deserializePlanTree reconstructs a PlanTree from serialized form.
func deserializePlanTree(st *SerializedPlanTree) *PlanTree {
	if st == nil || st.RootID == "" {
		return nil
	}

	tree := &PlanTree{
		TotalNodes: st.TotalNodes,
		nodeIndex:  make(map[string]*PlanNode),
	}

	// Reconstruct nodes
	for id, sn := range st.Nodes {
		node := &PlanNode{
			ID:     id,
			Status: PlanNodeStatus(sn.Status),
			Score:  sn.Confidence,
		}

		if sn.Action != nil {
			node.Action = &PlannedAction{
				Type:      ActionType(sn.Action.Type),
				AgentType: AgentType(sn.Action.AgentType),
				Prompt:    sn.Action.Prompt,
				ToolName:  sn.Action.ToolName,
				ToolArgs:  sn.Action.ToolArgs,
				NodeID:    id,
			}
		}

		if sn.Result != "" || sn.Error != "" {
			node.Result = &AgentResult{
				Output: sn.Result,
				Error:  sn.Error,
			}
		}

		tree.nodeIndex[id] = node
	}

	// Reconstruct parent-child relationships
	for id, sn := range st.Nodes {
		node := tree.nodeIndex[id]
		for _, childID := range sn.Children {
			if child, ok := tree.nodeIndex[childID]; ok {
				node.Children = append(node.Children, child)
				child.ParentID = id
			}
		}
	}

	// Set root
	tree.Root = tree.nodeIndex[st.RootID]

	// Reconstruct best path
	for _, nodeID := range st.CurrentPath {
		if node, ok := tree.nodeIndex[nodeID]; ok {
			tree.BestPath = append(tree.BestPath, node)
		}
	}

	return tree
}

// Snapshot returns a snapshot of the reflector's state.
// Note: The reflector uses immutable patterns, so we only save the error store state reference.
func (r *Reflector) Snapshot() *ReflectorSnapshot {
	if r == nil {
		return nil
	}

	// Reflector patterns are static, no state to snapshot beyond the error store
	// which is persisted separately
	return &ReflectorSnapshot{
		RecentErrors: make([]string, 0),
		LearnedFixes: make([]string, 0),
	}
}

// Restore restores the reflector's state from a snapshot.
// Note: Currently a no-op as reflector patterns are static and error store
// is persisted separately.
func (r *Reflector) Restore(snapshot *ReflectorSnapshot) {
	// No-op: patterns are static, error store has its own persistence
}

// MarshalJSON implements json.Marshaler for AgentCheckpoint.
func (cp *AgentCheckpoint) MarshalJSON() ([]byte, error) {
	type Alias AgentCheckpoint
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(cp),
	})
}

// UnmarshalJSON implements json.Unmarshaler for AgentCheckpoint.
func (cp *AgentCheckpoint) UnmarshalJSON(data []byte) error {
	type Alias AgentCheckpoint
	aux := &struct {
		*Alias
	}{
		Alias: (*Alias)(cp),
	}
	return json.Unmarshal(data, aux)
}
