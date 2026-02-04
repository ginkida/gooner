package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gokin/internal/client"
	"gokin/internal/config"
	"gokin/internal/logging"
	"gokin/internal/memory"
	"gokin/internal/permission"
	"gokin/internal/tools"
)

// Constants for resource management
const (
	// MaxAgentResults is the maximum number of agent results to keep in memory
	MaxAgentResults = 100
	// MaxCompletedAgents is the maximum number of completed agents to keep
	MaxCompletedAgents = 50
)

// ActivityReporter is an interface for reporting agent activity.
type ActivityReporter interface {
	ReportActivity()
}

// Runner manages the execution of multiple agents.
type Runner struct {
	client       client.Client
	baseRegistry tools.ToolRegistry
	workDir      string
	agents       map[string]*Agent
	results      map[string]*AgentResult
	store        *AgentStore
	permissions  *permission.Manager

	// Activity reporting
	activityReporter ActivityReporter

	// Inter-agent communication
	messengerFactory func(agentID string) *AgentMessenger

	ctxCfg *config.ContextConfig

	// Error learning (Phase 3)
	errorStore *memory.ErrorStore

	// Phase 5: Agent system improvements
	typeRegistry      *AgentTypeRegistry
	strategyOptimizer *StrategyOptimizer
	metaAgent         *MetaAgent

	// Phase 6: Tree Planner
	treePlanner            *TreePlanner
	planningModeEnabled    bool // Global planning mode flag
	requireApprovalEnabled bool // Global require approval flag

	// Callback for context compaction when plan is approved
	onPlanApproved func(planSummary string)

	// Callbacks for background task tracking (UI updates)
	onAgentStart    func(id, agentType, description string)
	onAgentComplete func(id string, result *AgentResult)

	// Phase 2: Progress tracking callback
	onAgentProgress func(id string, progress *AgentProgress)

	// Scratchpad state
	onScratchpadUpdate func(content string)
	sharedScratchpad   string

	// Phase 2: Shared memory for inter-agent communication
	sharedMemory *SharedMemory

	// User input callback
	onInput func(prompt string) (string, error)

	// Phase 2: Example store for few-shot learning
	exampleStore ExampleStoreInterface

	// Phase 2: Prompt optimizer
	promptOptimizer *PromptOptimizer

	// Sub-agent activity callback for UI updates
	onSubAgentActivity func(agentID, agentType, toolName string, args map[string]any, status string)

	mu sync.RWMutex
}

// SetPermissions sets the permission manager for agents.
func (r *Runner) SetPermissions(mgr *permission.Manager) {
	r.permissions = mgr
}

// SetActivityReporter sets the activity reporter.
func (r *Runner) SetActivityReporter(reporter ActivityReporter) {
	r.mu.Lock()
	r.activityReporter = reporter
	r.mu.Unlock()
}

// SetContextConfig sets the context configuration for agents.
func (r *Runner) SetContextConfig(cfg *config.ContextConfig) {
	r.mu.Lock()
	r.ctxCfg = cfg
	r.mu.Unlock()
}

// SetErrorStore sets the error store for learning from errors.
func (r *Runner) SetErrorStore(store *memory.ErrorStore) {
	r.mu.Lock()
	r.errorStore = store
	r.mu.Unlock()
}

// GetErrorStore returns the error store.
func (r *Runner) GetErrorStore() *memory.ErrorStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.errorStore
}

// SetTypeRegistry sets the agent type registry for dynamic types.
func (r *Runner) SetTypeRegistry(registry *AgentTypeRegistry) {
	r.mu.Lock()
	r.typeRegistry = registry
	r.mu.Unlock()
}

// GetTypeRegistry returns the agent type registry.
func (r *Runner) GetTypeRegistry() *AgentTypeRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.typeRegistry
}

// SetStrategyOptimizer sets the strategy optimizer for learning from outcomes.
func (r *Runner) SetStrategyOptimizer(optimizer *StrategyOptimizer) {
	r.mu.Lock()
	r.strategyOptimizer = optimizer
	r.mu.Unlock()
}

// GetStrategyOptimizer returns the strategy optimizer.
func (r *Runner) GetStrategyOptimizer() *StrategyOptimizer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.strategyOptimizer
}

// SetMetaAgent sets the meta-agent for monitoring and optimization.
func (r *Runner) SetMetaAgent(meta *MetaAgent) {
	r.mu.Lock()
	r.metaAgent = meta
	r.mu.Unlock()
}

// GetMetaAgent returns the meta-agent.
func (r *Runner) GetMetaAgent() *MetaAgent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.metaAgent
}

// SetTreePlanner sets the tree planner for planned execution.
func (r *Runner) SetTreePlanner(tp *TreePlanner) {
	r.mu.Lock()
	r.treePlanner = tp
	r.mu.Unlock()
}

// GetTreePlanner returns the tree planner.
func (r *Runner) GetTreePlanner() *TreePlanner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.treePlanner
}

// IsPlanningModeEnabled returns the global planning mode enabled flag.
func (r *Runner) IsPlanningModeEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.planningModeEnabled
}

// SetPlanningModeEnabled sets the global planning mode enabled flag.
func (r *Runner) SetPlanningModeEnabled(enabled bool) {
	r.mu.Lock()
	r.planningModeEnabled = enabled
	r.mu.Unlock()
}

// IsRequireApprovalEnabled returns the global require approval flag.
func (r *Runner) IsRequireApprovalEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.requireApprovalEnabled
}

// SetRequireApprovalEnabled sets the global require approval flag.
func (r *Runner) SetRequireApprovalEnabled(enabled bool) {
	r.mu.Lock()
	r.requireApprovalEnabled = enabled
	r.mu.Unlock()
}

// SetOnPlanApproved sets the callback for when a plan is approved.
// This callback is used to compact context and inject plan summary.
func (r *Runner) SetOnPlanApproved(callback func(planSummary string)) {
	r.mu.Lock()
	r.onPlanApproved = callback
	r.mu.Unlock()
}

// SetOnAgentStart sets the callback for when a background agent starts.
func (r *Runner) SetOnAgentStart(callback func(id, agentType, description string)) {
	r.mu.Lock()
	r.onAgentStart = callback
	r.mu.Unlock()
}

// SetOnAgentComplete sets the callback for when a background agent completes.
func (r *Runner) SetOnAgentComplete(callback func(id string, result *AgentResult)) {
	r.mu.Lock()
	r.onAgentComplete = callback
	r.mu.Unlock()
}

// SetOnAgentProgress sets the callback for agent progress updates.
func (r *Runner) SetOnAgentProgress(callback func(id string, progress *AgentProgress)) {
	r.mu.Lock()
	r.onAgentProgress = callback
	r.mu.Unlock()
}

// SetOnScratchpadUpdate sets the callback for agent scratchpad updates.
// The callback is called asynchronously to avoid deadlock.
func (r *Runner) SetOnScratchpadUpdate(fn func(string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onScratchpadUpdate = func(content string) {
		// Update shared scratchpad atomically
		r.mu.Lock()
		r.sharedScratchpad = content
		r.mu.Unlock()
		// Call user callback outside the lock to prevent deadlock
		if fn != nil {
			fn(content)
		}
	}
}

// SetSharedScratchpad sets the shared scratchpad content.
func (r *Runner) SetSharedScratchpad(content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sharedScratchpad = content
}

// SetSharedMemory sets the shared memory for inter-agent communication.
func (r *Runner) SetSharedMemory(sm *SharedMemory) {
	r.mu.Lock()
	r.sharedMemory = sm
	r.mu.Unlock()
}

// GetSharedMemory returns the shared memory instance.
func (r *Runner) GetSharedMemory() *SharedMemory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sharedMemory
}

// ExampleStoreInterface defines the interface for example stores.
type ExampleStoreInterface interface {
	LearnFromSuccess(taskType, prompt, agentType, output string, duration time.Duration, tokens int) error
	GetSimilarExamples(prompt string, limit int) []TaskExampleSummary
	GetExamplesForContext(taskType, prompt string, limit int) string
}

// TaskExampleSummary contains a summary of a task example.
type TaskExampleSummary struct {
	ID          string
	TaskType    string
	InputPrompt string
	AgentType   string
	Duration    time.Duration
	Score       float64
}

// SetExampleStore sets the example store for few-shot learning.
func (r *Runner) SetExampleStore(store ExampleStoreInterface) {
	r.mu.Lock()
	r.exampleStore = store
	r.mu.Unlock()
}

// GetExampleStore returns the example store.
func (r *Runner) GetExampleStore() ExampleStoreInterface {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.exampleStore
}

// SetOnInput sets the callback for requesting user input.
func (r *Runner) SetOnInput(callback func(string) (string, error)) {
	r.mu.Lock()
	r.onInput = callback
	r.mu.Unlock()
}

// SetPromptOptimizer sets the prompt optimizer.
func (r *Runner) SetPromptOptimizer(optimizer *PromptOptimizer) {
	r.mu.Lock()
	r.promptOptimizer = optimizer
	r.mu.Unlock()
}

// SetOnSubAgentActivity sets the callback for sub-agent activity reporting.
func (r *Runner) SetOnSubAgentActivity(fn func(agentID, agentType, toolName string, args map[string]any, status string)) {
	r.mu.Lock()
	r.onSubAgentActivity = fn
	r.mu.Unlock()
}

// GetPromptOptimizer returns the prompt optimizer.
func (r *Runner) GetPromptOptimizer() *PromptOptimizer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.promptOptimizer
}

// reportActivity reports activity if configured.
func (r *Runner) reportActivity() {
	r.mu.RLock()
	reporter := r.activityReporter
	r.mu.RUnlock()

	if reporter != nil {
		reporter.ReportActivity()
	}
}

// NewRunner creates a new agent runner.
func NewRunner(c client.Client, registry tools.ToolRegistry, workDir string) *Runner {
	r := &Runner{
		client:       c,
		baseRegistry: registry,
		workDir:      workDir,
		agents:       make(map[string]*Agent),
		results:      make(map[string]*AgentResult),
	}
	// Set up messenger factory
	r.messengerFactory = func(agentID string) *AgentMessenger {
		return NewAgentMessenger(r, agentID)
	}
	return r
}

// GetClient returns the underlying client.
func (r *Runner) GetClient() client.Client {
	return r.client
}

// SetClient updates the underlying client.
func (r *Runner) SetClient(c client.Client) {
	r.client = c
}

// cleanupOldResults removes old completed agents and results to prevent unbounded growth.
// Should be called periodically or when capacity is reached.
func (r *Runner) cleanupOldResults() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Clean up completed agents if we exceed the limit
	if len(r.agents) > MaxCompletedAgents {
		var completedIDs []string
		for id, agent := range r.agents {
			if agent.GetStatus() == AgentStatusCompleted ||
				agent.GetStatus() == AgentStatusFailed ||
				agent.GetStatus() == AgentStatusCancelled {
				completedIDs = append(completedIDs, id)
			}
		}
		// Remove oldest completed agents (keep MaxCompletedAgents/2)
		removeCount := len(completedIDs) - MaxCompletedAgents/2
		if removeCount > 0 && removeCount <= len(completedIDs) {
			for i := 0; i < removeCount; i++ {
				delete(r.agents, completedIDs[i])
			}
			logging.Debug("cleaned up old agents", "removed", removeCount)
		}
	}

	// Clean up old results if we exceed the limit
	if len(r.results) > MaxAgentResults {
		// Keep only MaxAgentResults/2 results
		var oldestIDs []string
		for id, result := range r.results {
			if result.Completed {
				oldestIDs = append(oldestIDs, id)
			}
		}
		removeCount := len(oldestIDs) - MaxAgentResults/2
		if removeCount > 0 && removeCount <= len(oldestIDs) {
			for i := 0; i < removeCount; i++ {
				delete(r.results, oldestIDs[i])
			}
			logging.Debug("cleaned up old results", "removed", removeCount)
		}
	}
}

// Spawn creates and starts a new agent with the given task.
// agentType should be "explore", "bash", "general", "plan", "claude-code-guide", or "coordinator".
// Also supports dynamic types registered via AgentTypeRegistry.
func (r *Runner) Spawn(ctx context.Context, agentType string, prompt string, maxTurns int, model string) (string, error) {
	// Cleanup old completed agents and results to prevent unbounded growth
	r.cleanupOldResults()

	r.mu.RLock()
	ctxCfg := r.ctxCfg
	errorStore := r.errorStore
	typeRegistry := r.typeRegistry
	strategyOpt := r.strategyOptimizer
	metaAgent := r.metaAgent
	treePlanner := r.treePlanner
	planningMode := r.planningModeEnabled
	requireApproval := r.requireApprovalEnabled
	planApprovedCallback := r.onPlanApproved
	onInput := r.onInput
	r.mu.RUnlock()

	// Check for dynamic type first
	var agent *Agent
	if typeRegistry != nil {
		if dynType, ok := typeRegistry.GetDynamic(agentType); ok {
			// Create agent with dynamic type configuration
			agent = NewAgentWithDynamicType(dynType, r.client, r.baseRegistry, r.workDir, maxTurns, model, r.permissions, ctxCfg)
		}
	}

	// Fall back to built-in types
	if agent == nil {
		at := ParseAgentType(agentType)
		agent = NewAgent(at, r.client, r.baseRegistry, r.workDir, maxTurns, model, r.permissions, ctxCfg)
	}

	// Set input callback
	if onInput != nil {
		agent.SetOnInput(onInput)
	}

	// Set up messenger for inter-agent communication
	if r.messengerFactory != nil {
		messenger := r.messengerFactory(agent.ID)
		agent.SetMessenger(messenger)
	}

	// Wire error store for learning from errors
	if errorStore != nil && agent.reflector != nil {
		agent.reflector.SetErrorStore(errorStore)
	}

	// Wire tree planner	// Wire planning capabilities
	if treePlanner != nil {
		agent.SetTreePlanner(treePlanner)
		if planningMode {
			agent.EnablePlanningMode(nil)
		}
		agent.SetRequireApproval(requireApproval)
		// Wire plan approval callback for context compaction
		if planApprovedCallback != nil {
			agent.SetOnPlanApproved(planApprovedCallback)
		}
	}

	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.mu.Unlock()

	// Register with meta-agent for monitoring
	if metaAgent != nil {
		metaAgent.RegisterAgent(agent.ID, agent.Type)
	}

	// Report activity to coordinator
	r.reportActivity()

	// Run agent synchronously
	startTime := time.Now()
	result, err := agent.Run(ctx, prompt)
	duration := time.Since(startTime)

	// Report activity after completion
	r.reportActivity()

	// Unregister from meta-agent
	if metaAgent != nil {
		metaAgent.UnregisterAgent(agent.ID)
	}

	// Record outcome with strategy optimizer
	if strategyOpt != nil && result != nil {
		success := result.Status == AgentStatusCompleted && result.Error == ""
		strategyOpt.RecordExecution(agentType, "spawn", success, duration)
	}

	// Save agent state for potential resume
	r.saveAgentState(agent)

	r.mu.Lock()
	r.results[agent.ID] = result
	r.mu.Unlock()

	if err != nil {
		return agent.ID, err
	}

	return agent.ID, nil
}

// SpawnWithContext creates and runs a sub-agent with project context and streaming.
// Unlike Spawn, it returns the AgentResult directly for immediate use by the caller.
// When skipPermissions is true, the sub-agent will not ask for permission before
// executing tools (used for approved plan execution).
func (r *Runner) SpawnWithContext(
	ctx context.Context,
	agentType string,
	prompt string,
	maxTurns int,
	model string,
	projectContext string,
	onText func(string),
	skipPermissions bool,
) (string, *AgentResult, error) {
	at := ParseAgentType(agentType)
	r.mu.RLock()
	ctxCfg := r.ctxCfg
	errorStore := r.errorStore
	onInput := r.onInput
	r.mu.RUnlock()

	// Pass nil permissions for approved plan execution to avoid per-tool prompts
	var perms *permission.Manager
	if !skipPermissions {
		perms = r.permissions
	}
	agent := NewAgent(at, r.client, r.baseRegistry, r.workDir, maxTurns, model, perms, ctxCfg)

	// Set input callback
	if onInput != nil {
		agent.SetOnInput(onInput)
	}

	// Set up messenger for inter-agent communication
	if r.messengerFactory != nil {
		messenger := r.messengerFactory(agent.ID)
		agent.SetMessenger(messenger)
	}

	// Set input callback
	if onInput != nil {
		agent.SetOnInput(onInput)
	}

	// Wire error store for learning from errors
	if errorStore != nil && agent.reflector != nil {
		agent.reflector.SetErrorStore(errorStore)
	}

	// Inject project context and streaming callback
	agent.SetProjectContext(projectContext)
	agent.SetOnText(onText)

	// Wire sub-agent activity callback
	r.mu.RLock()
	onSubAgentActivity := r.onSubAgentActivity
	r.mu.RUnlock()
	if onSubAgentActivity != nil {
		agent.SetOnToolActivity(func(agentID, toolName string, args map[string]any, status string) {
			onSubAgentActivity(agentID, string(agent.Type), toolName, args, "tool_"+status)
		})
	}

	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.mu.Unlock()

	r.reportActivity()

	result, err := agent.Run(ctx, prompt)

	r.reportActivity()
	r.saveAgentState(agent)

	r.mu.Lock()
	r.results[agent.ID] = result
	r.mu.Unlock()

	return agent.ID, result, err
}

// SpawnAsync creates and starts a new agent asynchronously.
// agentType should be "explore", "bash", "general", "plan", "claude-code-guide", or "coordinator".
func (r *Runner) SpawnAsync(ctx context.Context, agentType string, prompt string, maxTurns int, model string) string {
	at := ParseAgentType(agentType)
	r.mu.RLock()
	ctxCfg := r.ctxCfg
	errorStore := r.errorStore
	r.mu.RUnlock()
	agent := NewAgent(at, r.client, r.baseRegistry, r.workDir, maxTurns, model, r.permissions, ctxCfg)

	// Set up messenger for inter-agent communication
	if r.messengerFactory != nil {
		messenger := r.messengerFactory(agent.ID)
		agent.SetMessenger(messenger)
	}

	// Wire error store for learning from errors
	if errorStore != nil && agent.reflector != nil {
		agent.reflector.SetErrorStore(errorStore)
	}

	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.results[agent.ID] = &AgentResult{
		AgentID: agent.ID,
		Type:    at,
		Status:  AgentStatusPending,
	}
	onStart := r.onAgentStart
	onComplete := r.onAgentComplete
	r.mu.Unlock()

	// Report activity to coordinator
	r.reportActivity()

	// Notify UI about agent start
	if onStart != nil {
		onStart(agent.ID, agentType, prompt)
	}

	// Run agent asynchronously with proper cleanup
	go func() {
		// Ensure cleanup happens even on panic
		defer func() {
			if p := recover(); p != nil {
				r.mu.Lock()
				if result, ok := r.results[agent.ID]; ok {
					result.Error = fmt.Sprintf("agent panic: %v", p)
					result.Status = AgentStatusFailed
					result.Completed = true
				}
				r.mu.Unlock()
			}
		}()

		// Check if context is already cancelled
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.results[agent.ID] = &AgentResult{
				AgentID:   agent.ID,
				Type:      at,
				Status:    AgentStatusCancelled,
				Error:     ctx.Err().Error(),
				Completed: true,
			}
			r.mu.Unlock()
			return
		default:
		}

		result, err := agent.Run(ctx, prompt)

		// Ensure result is never nil
		if result == nil {
			result = &AgentResult{
				AgentID:   agent.ID,
				Type:      at,
				Status:    AgentStatusFailed,
				Error:     "nil result from agent",
				Completed: true,
			}
		}

		// Handle error by updating result status
		if err != nil {
			result.Error = err.Error()
			result.Status = AgentStatusFailed
		}

		// Save agent state for potential resume
		r.saveAgentState(agent)

		r.mu.Lock()
		r.results[agent.ID] = result
		r.mu.Unlock()

		// Notify UI about agent completion
		if onComplete != nil {
			onComplete(agent.ID, result)
		}
	}()

	return agent.ID
}

// SpawnAsyncWithStreaming creates and starts a new agent asynchronously with streaming support.
// The onText callback receives real-time text output from the agent.
// The onProgress callback receives progress updates.
func (r *Runner) SpawnAsyncWithStreaming(
	ctx context.Context,
	agentType string,
	prompt string,
	maxTurns int,
	model string,
	onText func(string),
	onProgress func(id string, progress *AgentProgress),
) string {
	at := ParseAgentType(agentType)
	r.mu.RLock()
	ctxCfg := r.ctxCfg
	errorStore := r.errorStore
	sharedMem := r.sharedMemory
	exampleStore := r.exampleStore
	promptOpt := r.promptOptimizer
	r.mu.RUnlock()

	agent := NewAgent(at, r.client, r.baseRegistry, r.workDir, maxTurns, model, r.permissions, ctxCfg)

	// Set up streaming callback
	if onText != nil {
		agent.SetOnText(onText)
	}

	// Set up messenger for inter-agent communication
	if r.messengerFactory != nil {
		messenger := r.messengerFactory(agent.ID)
		agent.SetMessenger(messenger)
	}

	// Wire error store for learning from errors
	if errorStore != nil && agent.reflector != nil {
		agent.reflector.SetErrorStore(errorStore)
	}

	// Wire shared memory if available
	if sharedMem != nil {
		agent.SetSharedMemory(sharedMem)
	}

	// Wire sub-agent activity callback
	r.mu.RLock()
	onSubAgentActivity := r.onSubAgentActivity
	r.mu.RUnlock()
	if onSubAgentActivity != nil {
		agent.SetOnToolActivity(func(agentID, toolName string, args map[string]any, status string) {
			onSubAgentActivity(agentID, string(agent.Type), toolName, args, "tool_"+status)
		})
	}

	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.results[agent.ID] = &AgentResult{
		AgentID: agent.ID,
		Type:    at,
		Status:  AgentStatusPending,
	}
	onStart := r.onAgentStart
	onComplete := r.onAgentComplete
	onAgentProgress := r.onAgentProgress
	r.mu.Unlock()

	// Report activity to coordinator
	r.reportActivity()

	// Notify UI about agent start
	if onStart != nil {
		onStart(agent.ID, agentType, prompt)
	}

	// Run agent asynchronously with streaming and progress updates
	go func() {
		agentID := agent.ID

		// Ensure cleanup happens even on panic
		defer func() {
			if p := recover(); p != nil {
				r.mu.Lock()
				if result, ok := r.results[agentID]; ok {
					result.Error = fmt.Sprintf("agent panic: %v", p)
					result.Status = AgentStatusFailed
					result.Completed = true
				}
				r.mu.Unlock()
			}
		}()

		// Start progress ticker for periodic updates
		progressTicker := time.NewTicker(2 * time.Second)
		defer progressTicker.Stop()

		// Create a context with cancellation for the progress goroutine
		progressCtx, progressCancel := context.WithCancel(ctx)
		defer progressCancel()

		// Progress update goroutine
		go func() {
			for {
				select {
				case <-progressTicker.C:
					progress := agent.GetProgress()
					if onProgress != nil {
						onProgress(agentID, &progress)
					}
					if onAgentProgress != nil {
						onAgentProgress(agentID, &progress)
					}
				case <-progressCtx.Done():
					return
				}
			}
		}()

		// Set scratchpad update callback and initial content
		r.mu.RLock()
		agent.Scratchpad = r.sharedScratchpad
		callback := r.onScratchpadUpdate
		r.mu.RUnlock()

		if callback != nil {
			agent.SetOnScratchpadUpdate(callback)
		}

		// Check if context is already cancelled
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.results[agentID] = &AgentResult{
				AgentID:   agentID,
				Type:      at,
				Status:    AgentStatusCancelled,
				Error:     ctx.Err().Error(),
				Completed: true,
			}
			r.mu.Unlock()
			return
		default:
		}

		startTime := time.Now()
		result, err := agent.Run(ctx, prompt)
		duration := time.Since(startTime)

		// Ensure result is never nil
		if result == nil {
			result = &AgentResult{
				AgentID:   agentID,
				Type:      at,
				Status:    AgentStatusFailed,
				Error:     "nil result from agent",
				Completed: true,
			}
		}

		// Handle error by updating result status
		if err != nil {
			result.Error = err.Error()
			result.Status = AgentStatusFailed
		}

		// Record successful execution for learning
		if result.Status == AgentStatusCompleted && exampleStore != nil {
			// Learn from successful executions
			go func() {
				_ = exampleStore.LearnFromSuccess(
					agentType,
					prompt,
					agentType,
					result.Output,
					duration,
					0, // Token count not tracked at this level
				)
			}()
		}

		// Record with prompt optimizer
		if promptOpt != nil {
			success := result.Status == AgentStatusCompleted && result.Error == ""
			promptOpt.RecordExecution(agentType, prompt, success, 0, duration)
		}

		// Save agent state for potential resume
		r.saveAgentState(agent)

		r.mu.Lock()
		r.results[agentID] = result
		r.mu.Unlock()

		// Notify UI about agent completion
		if onComplete != nil {
			onComplete(agentID, result)
		}
	}()

	return agent.ID
}

// SpawnMultiple creates and starts multiple agents in parallel.
func (r *Runner) SpawnMultiple(ctx context.Context, tasks []AgentTask) ([]string, error) {
	ids := make([]string, len(tasks))
	results := make([]*AgentResult, len(tasks))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, t AgentTask) {
			defer wg.Done()

			r.mu.RLock()
			ctxCfg := r.ctxCfg
			r.mu.RUnlock()
			agent := NewAgent(t.Type, r.client, r.baseRegistry, r.workDir, t.MaxTurns, t.Model, r.permissions, ctxCfg)

			// Set up messenger for inter-agent communication
			if r.messengerFactory != nil {
				messenger := r.messengerFactory(agent.ID)
				agent.SetMessenger(messenger)
			}

			r.mu.Lock()
			r.agents[agent.ID] = agent
			r.mu.Unlock()

			result, err := agent.Run(ctx, t.Prompt)

			mu.Lock()
			ids[idx] = agent.ID
			results[idx] = result
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()

			r.mu.Lock()
			r.results[agent.ID] = result
			r.mu.Unlock()
		}(i, task)
	}

	wg.Wait()

	return ids, firstErr
}

// Wait waits for an agent to complete and returns its result.
// Uses a default 10-minute timeout. For context-aware waiting, use WaitWithContext.
func (r *Runner) Wait(agentID string) (*AgentResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return r.WaitWithContext(ctx, agentID)
}

// WaitWithContext waits for an agent to complete, respecting context cancellation.
func (r *Runner) WaitWithContext(ctx context.Context, agentID string) (*AgentResult, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			r.mu.RLock()
			result, ok := r.results[agentID]
			r.mu.RUnlock()

			if ok && result.Completed {
				return result, nil
			}
		}
	}
}

// WaitWithTimeout waits for an agent to complete with a specific timeout.
func (r *Runner) WaitWithTimeout(agentID string, timeout time.Duration) (*AgentResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.WaitWithContext(ctx, agentID)
}

// WaitAll waits for multiple agents to complete.
func (r *Runner) WaitAll(agentIDs []string) ([]*AgentResult, error) {
	results := make([]*AgentResult, len(agentIDs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, id := range agentIDs {
		wg.Add(1)
		go func(idx int, agentID string) {
			defer wg.Done()

			result, err := r.Wait(agentID)

			mu.Lock()
			// Ensure result is never nil
			if result == nil {
				result = &AgentResult{
					AgentID:   agentID,
					Status:    AgentStatusFailed,
					Error:     fmt.Sprintf("wait failed: %v", err),
					Completed: true,
				}
			}
			results[idx] = result
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(i, id)
	}

	wg.Wait()
	return results, firstErr
}

// GetResult returns the result for an agent.
func (r *Runner) GetResult(agentID string) (*AgentResult, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result, ok := r.results[agentID]
	return result, ok
}

// GetAgent returns an agent by ID.
func (r *Runner) GetAgent(agentID string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agent, ok := r.agents[agentID]
	return agent, ok
}

// Cancel cancels an agent's execution.
func (r *Runner) Cancel(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	agent, ok := r.agents[agentID]
	if !ok {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	agent.Cancel()

	// Update result
	if result, ok := r.results[agentID]; ok {
		result.Status = AgentStatusCancelled
	}

	return nil
}

// ListAgents returns all agent IDs.
func (r *Runner) ListAgents() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	return ids
}

// ListRunning returns IDs of currently running agents.
func (r *Runner) ListRunning() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0)
	for id, agent := range r.agents {
		if agent.GetStatus() == AgentStatusRunning {
			ids = append(ids, id)
		}
	}
	return ids
}

// Cleanup removes completed agents older than the specified duration.
func (r *Runner) Cleanup(maxAge time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	cleaned := 0

	for id, agent := range r.agents {
		if agent.status == AgentStatusCompleted || agent.status == AgentStatusFailed {
			if !agent.endTime.IsZero() && agent.endTime.Before(cutoff) {
				delete(r.agents, id)
				delete(r.results, id)
				cleaned++
			}
		}
	}

	return cleaned
}

// SetStore sets the agent store for persistence.
func (r *Runner) SetStore(store *AgentStore) {
	r.store = store
}

// Resume resumes an agent from a saved state.
func (r *Runner) Resume(ctx context.Context, agentID string, prompt string) (string, error) {
	if r.store == nil {
		return "", fmt.Errorf("agent store not configured")
	}

	// Load state from store
	state, err := r.store.Load(agentID)
	if err != nil {
		return "", fmt.Errorf("failed to load agent state: %w", err)
	}

	// Create a new agent with the same configuration
	r.mu.RLock()
	ctxCfg := r.ctxCfg
	r.mu.RUnlock()
	agent := NewAgent(state.Type, r.client, r.baseRegistry, r.workDir, state.MaxTurns, state.Model, r.permissions, ctxCfg)
	// Override ID to match the resumed agent
	agent.ID = state.ID

	// Set up messenger for inter-agent communication
	if r.messengerFactory != nil {
		messenger := r.messengerFactory(agent.ID)
		agent.SetMessenger(messenger)
	}

	// Restore history
	if err := agent.RestoreHistory(state); err != nil {
		return "", fmt.Errorf("failed to restore agent history: %w", err)
	}

	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.mu.Unlock()

	// Run agent with the new prompt (continuing from previous context)
	result, err := agent.Run(ctx, prompt)

	// Save updated state
	if r.store != nil {
		if err := r.store.Save(agent); err != nil {
			logging.Warn("failed to save agent state", "agent_id", agent.ID, "error", err)
		}
	}

	r.mu.Lock()
	r.results[agent.ID] = result
	r.mu.Unlock()

	if err != nil {
		return agent.ID, err
	}

	return agent.ID, nil
}

// ResumeAsync resumes an agent asynchronously.
func (r *Runner) ResumeAsync(ctx context.Context, agentID string, prompt string) (string, error) {
	if r.store == nil {
		return "", fmt.Errorf("agent store not configured")
	}

	// Load state from store
	state, err := r.store.Load(agentID)
	if err != nil {
		return "", fmt.Errorf("failed to load agent state: %w", err)
	}

	// Create a new agent with the same configuration
	r.mu.RLock()
	ctxCfg := r.ctxCfg
	r.mu.RUnlock()
	agent := NewAgent(state.Type, r.client, r.baseRegistry, r.workDir, state.MaxTurns, state.Model, r.permissions, ctxCfg)
	agent.ID = state.ID

	// Set up messenger for inter-agent communication
	if r.messengerFactory != nil {
		messenger := r.messengerFactory(agent.ID)
		agent.SetMessenger(messenger)
	}

	// Restore history
	if err := agent.RestoreHistory(state); err != nil {
		return "", fmt.Errorf("failed to restore agent history: %w", err)
	}

	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.results[agent.ID] = &AgentResult{
		AgentID: agent.ID,
		Type:    state.Type,
		Status:  AgentStatusPending,
	}
	onStart := r.onAgentStart
	onComplete := r.onAgentComplete
	r.mu.Unlock()

	// Notify UI about agent start (resumed)
	if onStart != nil {
		onStart(agent.ID, string(state.Type), prompt)
	}

	// Run agent asynchronously with proper cleanup
	go func() {
		// Ensure cleanup happens even on panic
		defer func() {
			if p := recover(); p != nil {
				r.mu.Lock()
				if result, ok := r.results[agent.ID]; ok {
					result.Error = fmt.Sprintf("agent panic: %v", p)
					result.Status = AgentStatusFailed
					result.Completed = true
				}
				r.mu.Unlock()
			}
		}()

		// Check if context is already cancelled
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.results[agent.ID] = &AgentResult{
				AgentID:   agent.ID,
				Type:      state.Type,
				Status:    AgentStatusCancelled,
				Error:     ctx.Err().Error(),
				Completed: true,
			}
			r.mu.Unlock()
			return
		default:
		}

		result, err := agent.Run(ctx, prompt)

		// Ensure result is never nil
		if result == nil {
			result = &AgentResult{
				AgentID:   agent.ID,
				Type:      state.Type,
				Status:    AgentStatusFailed,
				Error:     "nil result from agent",
				Completed: true,
			}
		}

		// Handle error by updating result status
		if err != nil {
			result.Error = err.Error()
			result.Status = AgentStatusFailed
		}

		// Save updated state
		if r.store != nil {
			_ = r.store.Save(agent)
		}

		r.mu.Lock()
		r.results[agent.ID] = result
		r.mu.Unlock()

		// Notify UI about agent completion
		if onComplete != nil {
			onComplete(agent.ID, result)
		}
	}()

	return agent.ID, nil
}

// saveAgentState saves the agent state if store is configured.
func (r *Runner) saveAgentState(agent *Agent) {
	if r.store != nil {
		if err := r.store.Save(agent); err != nil {
			logging.Warn("failed to save agent state", "agent_id", agent.ID, "error", err)
		}
	}
}
