package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gokin/internal/client"
	"gokin/internal/config"
	ctxmgr "gokin/internal/context"
	"gokin/internal/logging"
	"gokin/internal/permission"
	"gokin/internal/tools"

	"google.golang.org/genai"
)

// Agent represents an isolated executor for subtasks.
type Agent struct {
	ID           string
	Type         AgentType
	Model        string
	client       client.Client
	registry     *tools.Registry
	baseRegistry *tools.Registry
	messenger    tools.Messenger
	permissions  *permission.Manager
	timeout      time.Duration
	history      []*genai.Content
	status       AgentStatus
	startTime    time.Time
	endTime      time.Time
	maxTurns     int

	// === IMPROVEMENT 4: Progress tracking ===
	currentStep      int
	totalSteps       int
	stepDescription  string
	progressMu       sync.Mutex
	progressCallback func(progress *AgentProgress)

	// Mental loop detection tracking
	callHistory    map[string]int // Map of tool_name:arguments -> count
	callHistoryMu  sync.Mutex     // Protects callHistory map
	lastThought    string         // Store the last reasoning/thought
	loopIntervened bool           // Flag to indicate if loop intervention occurred

	// Project context injection for sub-agents
	projectContext string            // Injected project guidelines/instructions
	onText         func(text string) // Streaming callback for real-time output
	onTextMu       sync.Mutex        // Protects onText from interleaving
	onInput        func(prompt string) (string, error)

	// Plan approval callback for context compaction
	onPlanApproved func(planSummary string) // Called when plan is built, allows context clearing

	// Scratchpad update callback
	onScratchpadUpdate func(content string)

	// Context management
	ctxCfg       *config.ContextConfig
	tokenCounter *ctxmgr.TokenCounter
	summarizer   *ctxmgr.Summarizer
	compactor    *ctxmgr.ResultCompactor

	// Self-reflection for error recovery
	reflector *Reflector

	// Autonomous delegation strategy
	delegation *DelegationStrategy

	// Tree planning (Phase 6)
	treePlanner     *TreePlanner
	activePlan      *PlanTree
	planningMode    bool
	requireApproval bool
	planGoal        *PlanGoal

	// Phase 2: Shared memory for inter-agent communication
	sharedMemory *SharedMemory

	// Phase 2: Tools used tracking for progress
	toolsUsed []string
	toolsMu   sync.Mutex

	// Agent Scratchpad (Phase 7)
	Scratchpad string

	// Tool activity callback for UI updates
	onToolActivity func(agentID, toolName string, args map[string]any, status string)
}

// NewAgent creates a new agent with the specified type and filtered tools.
func NewAgent(agentType AgentType, c client.Client, baseRegistry *tools.Registry, workDir string, maxTurns int, model string, permManager *permission.Manager, ctxCfg *config.ContextConfig) *Agent {
	id := generateAgentID()

	// Create filtered registry based on agent type
	filteredRegistry := createFilteredRegistry(agentType, baseRegistry)

	if maxTurns <= 0 {
		maxTurns = 30 // default
	}

	// Use a different model if specified
	agentClient := c
	if model != "" {
		modelName := mapModelName(model)
		if modelName != "" {
			agentClient = c.WithModel(modelName)
		}
	}

	agent := &Agent{
		ID:           id,
		Type:         agentType,
		Model:        model,
		client:       agentClient,
		registry:     filteredRegistry,
		baseRegistry: baseRegistry,
		permissions:  permManager,
		timeout:      2 * time.Minute,
		history:      make([]*genai.Content, 0),
		status:       AgentStatusPending,
		maxTurns:     maxTurns,
		callHistory:  make(map[string]int),
		ctxCfg:       ctxCfg,
	}

	// Wire up RequestTool tool if it exists in the registry
	if rt, ok := agent.registry.Get("request_tool"); ok {
		if rtt, ok := rt.(*tools.RequestToolTool); ok {
			rtt.SetRequester(agent)
		}
	}

	// Initialize context management tools if config provided
	if ctxCfg != nil {
		agent.tokenCounter = ctxmgr.NewTokenCounter(agent.client, agent.Model, ctxCfg)
		agent.summarizer = ctxmgr.NewSummarizer(agent.client)
		agent.compactor = ctxmgr.NewResultCompactor(ctxCfg.ToolResultMaxChars)
	}

	// Initialize self-reflection capability
	agent.reflector = NewReflector()

	// Wire up scratchpad if it exists
	if t, ok := agent.registry.Get("update_scratchpad"); ok {
		if ust, ok := t.(*tools.UpdateScratchpadTool); ok {
			ust.SetUpdater(func(content string) {
				agent.Scratchpad = content
				if agent.onScratchpadUpdate != nil {
					agent.onScratchpadUpdate(content)
				}
			})
		}
	}

	// Initialize delegation strategy (messenger set later)
	agent.delegation = NewDelegationStrategy(agentType, nil)

	return agent
}

// NewAgentWithDynamicType creates a new agent with a dynamic type configuration.
func NewAgentWithDynamicType(dynType *DynamicAgentType, c client.Client, baseRegistry *tools.Registry, workDir string, maxTurns int, model string, permManager *permission.Manager, ctxCfg *config.ContextConfig) *Agent {
	id := generateAgentID()

	// Create filtered registry based on dynamic type's allowed tools
	filteredRegistry := createFilteredRegistryFromList(dynType.AllowedTools, baseRegistry)

	if maxTurns <= 0 {
		maxTurns = 30
	}

	agentClient := c
	if model != "" {
		modelName := mapModelName(model)
		if modelName != "" {
			agentClient = c.WithModel(modelName)
		}
	}

	agent := &Agent{
		ID:           id,
		Type:         AgentType(dynType.Name), // Use dynamic type name
		Model:        model,
		client:       agentClient,
		registry:     filteredRegistry,
		baseRegistry: baseRegistry,
		permissions:  permManager,
		timeout:      2 * time.Minute,
		history:      make([]*genai.Content, 0),
		status:       AgentStatusPending,
		maxTurns:     maxTurns,
		callHistory:  make(map[string]int),
		ctxCfg:       ctxCfg,
		// Store custom prompt for dynamic type
		projectContext: dynType.SystemPrompt,
	}

	// Wire up RequestTool tool if it exists
	if rt, ok := agent.registry.Get("request_tool"); ok {
		if rtt, ok := rt.(*tools.RequestToolTool); ok {
			rtt.SetRequester(agent)
		}
	}

	// Initialize context management
	if ctxCfg != nil {
		agent.tokenCounter = ctxmgr.NewTokenCounter(agent.client, agent.Model, ctxCfg)
		agent.summarizer = ctxmgr.NewSummarizer(agent.client)
		agent.compactor = ctxmgr.NewResultCompactor(ctxCfg.ToolResultMaxChars)
	}

	agent.reflector = NewReflector()
	agent.delegation = NewDelegationStrategy(AgentTypeGeneral, nil)

	return agent
}

// createFilteredRegistryFromList creates a registry with only the specified tools.
func createFilteredRegistryFromList(allowedTools []string, baseRegistry *tools.Registry) *tools.Registry {
	if len(allowedTools) == 0 {
		return baseRegistry // All tools allowed
	}

	filtered := tools.NewRegistry()
	allowedMap := make(map[string]bool)
	for _, name := range allowedTools {
		allowedMap[name] = true
	}

	for _, tool := range baseRegistry.List() {
		if allowedMap[tool.Name()] {
			_ = filtered.Register(tool)
		}
	}

	return filtered
}

// SetProjectContext injects project guidelines for sub-agent system prompts.
func (a *Agent) SetProjectContext(ctx string) {
	a.projectContext = ctx
}

// SetOnText sets the streaming callback for real-time output.
func (a *Agent) SetOnText(onText func(string)) {
	a.onText = onText
}

// SetOnScratchpadUpdate sets the callback for scratchpad updates.
func (a *Agent) SetOnScratchpadUpdate(fn func(string)) {
	a.onScratchpadUpdate = fn
}

// SetOnToolActivity sets the callback for tool activity reporting.
func (a *Agent) SetOnToolActivity(fn func(agentID, toolName string, args map[string]any, status string)) {
	a.onToolActivity = fn
}

// SetOnInput sets the callback for requesting user input.
func (a *Agent) SetOnInput(onInput func(string) (string, error)) {
	a.onInput = onInput
}

// SetOnPlanApproved sets a callback for when a plan is built and ready.
// The callback receives a plan summary and should clear/compact context.
func (a *Agent) SetOnPlanApproved(callback func(planSummary string)) {
	a.onPlanApproved = callback
}

// SetMessenger sets the messenger for inter-agent communication.
func (a *Agent) SetMessenger(m tools.Messenger) {
	a.messenger = m

	// Wire up AskAgentTool if it exists in the registry
	if askTool, ok := a.registry.Get("ask_agent"); ok {
		if aat, ok := askTool.(*tools.AskAgentTool); ok {
			aat.SetMessenger(m)
		}
	}

	// Wire up delegation strategy with messenger
	if a.delegation != nil {
		if am, ok := m.(*AgentMessenger); ok {
			a.delegation.SetMessenger(am)
		}
	}
}

// SetTreePlanner sets the tree planner for planned execution mode.
func (a *Agent) SetTreePlanner(tp *TreePlanner) {
	a.treePlanner = tp

	if tp != nil {
		tp.SetCallbacks(
			func(tree *PlanTree, node *PlanNode) {
				a.IncrementStep("Executing step: " + node.Action.Prompt)
				if a.onText != nil {
					a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
				}
			},
			func(tree *PlanTree, node *PlanNode, success bool) {
				if a.onText != nil {
					a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
				}
			},
			func(tree *PlanTree, ctx *ReplanContext) {
				if a.onText != nil {
					a.safeOnText(fmt.Sprintf("\n[Replanning: %s]\n", ctx.Error))
					a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
				}
			},
			func(action *PlannedAction) {
				// Record planning progress
				if a.onText != nil {
					a.safeOnText(fmt.Sprintf("  • %s: %s\n", action.AgentType, action.Prompt))
				}
				a.SetProgress(0, 0, "Planning: "+action.Prompt)
			},
		)
	}
}

// SetSharedMemory sets the shared memory instance for inter-agent communication.
func (a *Agent) SetSharedMemory(sm *SharedMemory) {
	a.sharedMemory = sm
}

// GetSharedMemory returns the shared memory instance.
func (a *Agent) GetSharedMemory() *SharedMemory {
	return a.sharedMemory
}

// AddToolUsed tracks a tool that was used during execution.
func (a *Agent) AddToolUsed(toolName string) {
	a.toolsMu.Lock()
	a.toolsUsed = append(a.toolsUsed, toolName)
	a.toolsMu.Unlock()
}

// GetToolsUsed returns the list of tools used during execution.
func (a *Agent) GetToolsUsed() []string {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	result := make([]string, len(a.toolsUsed))
	copy(result, a.toolsUsed)
	return result
}

// SetPlanGoal sets the goal for the plan.
func (a *Agent) SetPlanGoal(goal *PlanGoal) {
	a.planGoal = goal
}

// SetRequireApproval sets whether plan approval is required.
func (a *Agent) SetRequireApproval(required bool) {
	a.requireApproval = required
}

// EnablePlanningMode enables tree-based planning for agent execution.
func (a *Agent) EnablePlanningMode(goal *PlanGoal) {
	a.planningMode = true
	a.planGoal = goal
}

// DisablePlanningMode disables tree-based planning.
func (a *Agent) DisablePlanningMode() {
	a.planningMode = false
	a.planGoal = nil
	a.activePlan = nil
}

// GetActivePlan returns the currently active plan tree.
func (a *Agent) GetActivePlan() *PlanTree {
	return a.activePlan
}

// IsPlanningMode returns whether the agent is in planning mode.
func (a *Agent) IsPlanningMode() bool {
	return a.planningMode
}

// mapModelName maps user-friendly model names to actual Gemini model names.
func mapModelName(name string) string {
	switch strings.ToLower(name) {
	case "flash", "haiku":
		return "gemini-3-flash-preview"
	case "pro", "sonnet":
		return "gemini-3-pro-preview"
	case "ultra", "opus":
		return "gemini-3-pro-preview" // Use pro for ultra/opus for now
	default:
		return name // Return as is if already a full model name
	}
}

// generateAgentID creates a unique identifier for an agent.
func generateAgentID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// createFilteredRegistry creates a registry with only allowed tools for the agent type.
func createFilteredRegistry(agentType AgentType, baseRegistry *tools.Registry) *tools.Registry {
	allowedTools := agentType.AllowedTools()

	// If nil, all tools are allowed (general type)
	if allowedTools == nil {
		return baseRegistry
	}

	// Create new registry with filtered tools
	filtered := tools.NewRegistry()
	allowedMap := make(map[string]bool)
	for _, name := range allowedTools {
		allowedMap[name] = true
	}

	for _, tool := range baseRegistry.List() {
		if allowedMap[tool.Name()] {
			_ = filtered.Register(tool)
		}
	}

	return filtered
}

// RequestTool dynamically adds a tool from the base registry to the agent's active registry.
func (a *Agent) RequestTool(name string) error {
	if a.registry == a.baseRegistry {
		return nil // Already has all tools
	}

	tool, ok := a.baseRegistry.Get(name)
	if !ok {
		return fmt.Errorf("tool not found in system: %s", name)
	}

	return a.registry.Register(tool)
}

// SendMessage sends a message to another agent via the messenger.
func (a *Agent) SendMessage(msgType string, toRole string, content string, data map[string]any) (string, error) {
	if a.messenger == nil {
		return "", fmt.Errorf("messenger not initialized for this agent")
	}
	return a.messenger.SendMessage(msgType, toRole, content, data)
}

// ReceiveResponse waits for a response to a previously sent message.
func (a *Agent) ReceiveResponse(ctx context.Context, messageID string) (string, error) {
	if a.messenger == nil {
		return "", fmt.Errorf("messenger not initialized for this agent")
	}
	return a.messenger.ReceiveResponse(ctx, messageID)
}

// Run executes the agent with the given prompt and returns the result.
func (a *Agent) Run(ctx context.Context, prompt string) (*AgentResult, error) {
	a.status = AgentStatusRunning
	a.startTime = time.Now()

	// Initialize progress
	a.SetProgress(0, a.maxTurns, "Starting agent execution")

	result := &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusRunning,
		Completed: false,
	}

	// Build system prompt for the agent
	systemPrompt := a.buildSystemPrompt()

	// Initialize history with system context
	a.history = []*genai.Content{
		genai.NewContentFromText(systemPrompt, genai.RoleUser),
		genai.NewContentFromText("I understand. I'll help with the task using only my allowed tools.", genai.RoleModel),
	}

	// Execute the prompt through the function calling loop
	var finalOutput strings.Builder
	_, output, err := a.executeLoop(ctx, prompt, &finalOutput)
	if err != nil {
		a.status = AgentStatusFailed
		a.endTime = time.Now()
		result.Status = AgentStatusFailed
		result.Error = err.Error()
		result.Output = output // Preserve partial output on failure
		result.Duration = a.endTime.Sub(a.startTime)

		// Update progress with failure
		a.SetProgress(a.currentStep, a.totalSteps, "Failed: "+err.Error())

		return result, err
	}

	a.status = AgentStatusCompleted
	a.endTime = time.Now()

	result.Status = AgentStatusCompleted
	result.Output = output
	result.Duration = a.endTime.Sub(a.startTime)
	result.Completed = true

	// Update progress with completion
	a.SetProgress(a.totalSteps, a.totalSteps, "Completed")

	return result, nil
}

// buildSystemPrompt creates the system prompt based on agent type.
func (a *Agent) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString("You are a specialized sub-agent with limited tool access.\n")
	sb.WriteString(fmt.Sprintf("Agent Type: %s\n", a.Type))
	sb.WriteString("Available tools: ")

	toolNames := a.registry.Names()
	sb.WriteString(strings.Join(toolNames, ", "))
	sb.WriteString("\n\n")

	// Universal instructions for all agents
	sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
	sb.WriteString("                         MANDATORY RULES\n")
	sb.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
	sb.WriteString("1. ALWAYS use tools to complete your task - don't just say you can't\n")
	sb.WriteString("2. After using ANY tool, provide a CLEAR summary of what you found\n")
	sb.WriteString("3. NEVER respond with just 'OK' or 'Done' - always explain\n")
	sb.WriteString("4. Structure responses with markdown: headers, bullets, code blocks\n")
	sb.WriteString("5. Include specific file:line references when discussing code\n\n")

	switch a.Type {
	case AgentTypeExplore:
		sb.WriteString(a.buildExplorePrompt())
	case AgentTypeBash:
		sb.WriteString(a.buildBashPrompt())
	case AgentTypeGeneral:
		sb.WriteString(a.buildGeneralPrompt())
	case AgentTypePlan:
		sb.WriteString(a.buildPlanPrompt())
	case AgentTypeGuide:
		sb.WriteString(a.buildGuidePrompt())
	default:
		sb.WriteString("Complete the assigned task using available tools.\n")
	}

	// Inject project context if provided (for delegated sub-agents)
	if a.projectContext != "" {
		sb.WriteString("\n")
		sb.WriteString(a.projectContext)
		sb.WriteString("\n")
	}

	// Inject scratchpad if not empty
	if a.Scratchpad != "" {
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("                         YOUR SCRATCHPAD\n")
		sb.WriteString("═══════════════════════════════════════════════════════════════════════\n")
		sb.WriteString("This is your persistent memory. Use it to store facts, thoughts, or plans.\n\n")
		sb.WriteString(a.Scratchpad)
		sb.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
	}

	return sb.String()
}

func (a *Agent) buildExplorePrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         EXPLORE AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Explore and analyze the codebase to answer questions.

RECOMMENDED APPROACH:
1. glob - Find relevant files first
2. read - Read key files to understand structure
3. grep - Search for specific patterns/usages
4. Analyze and summarize findings

RESPONSE FORMAT:
## Summary
[Direct answer to the question in 1-2 sentences]

## Key Findings
- **Finding 1** (file.go:123): Description
- **Finding 2** (other.go:45): Description

## Code Examples
` + "```" + `go
// Relevant code snippet with explanation
` + "```" + `

## Architecture
[How components connect, data flow, dependencies]

## Recommendations
[What to look at next, potential issues, suggestions]

═══════════════════════════════════════════════════════════════════════

EXAMPLE - GOOD RESPONSE:
User: "How does authentication work?"

## Summary
Authentication uses JWT tokens validated by middleware in auth/middleware.go.

## Key Findings
- **Token validation** (auth/middleware.go:45): Validates JWT on every request
- **Token generation** (auth/service.go:78): Creates tokens with 24h expiry
- **User lookup** (auth/repo.go:32): Fetches user from database

## Code Examples
` + "```" + `go
// middleware.go:45-52
func ValidateToken(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        token := r.Header.Get("Authorization")
        claims, err := validateJWT(token)
        // ...
    })
}
` + "```" + `

## Architecture
` + "```" + `
Request → Middleware → Validate JWT → Handler
                ↓
         auth/service.go (token ops)
                ↓
         auth/repo.go (user data)
` + "```" + `

## Recommendations
- Consider adding token refresh mechanism
- Rate limiting should be added to login endpoint

═══════════════════════════════════════════════════════════════════════

EXAMPLE - BAD RESPONSE (NEVER DO THIS):
User: "How does authentication work?"
[reads files, says nothing or just "It uses JWT"]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildBashPrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         BASH AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Execute shell commands safely and explain results.

APPROACH:
1. Understand what command to run
2. Execute the command
3. Analyze the output
4. Explain results clearly

RESPONSE FORMAT:
## Command Executed
` + "```" + `bash
[The command you ran]
` + "```" + `

## Results Summary
[What the command did and what output means]

## Details
[Specific output analysis, errors, warnings]

## Next Steps
[What to do based on results]

═══════════════════════════════════════════════════════════════════════

EXAMPLE - GOOD RESPONSE:
User: "Run the tests"

## Command Executed
` + "```" + `bash
go test ./...
` + "```" + `

## Results Summary
**45 passed**, **2 failed**, **3.2s** total runtime

## Failed Tests

### TestUserCreate (user_test.go:34)
- **Expected**: status 201
- **Got**: status 400
- **Cause**: Missing required field 'email' in test fixture

### TestDBConnection (db_test.go:12)
- **Error**: connection timeout
- **Cause**: Test database not running

## Next Steps
1. Fix TestUserCreate: Add email field to fixture at line 30
2. Fix TestDBConnection: Run ` + "`docker-compose up -d`" + ` first
3. Re-run tests after fixes

═══════════════════════════════════════════════════════════════════════

EXAMPLE - BAD RESPONSE (NEVER DO THIS):
User: "Run the tests"
[runs test, shows raw output only]
or
"Tests completed." [no details]

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGeneralPrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         GENERAL AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Complete the assigned task using all available tools.

APPROACH:
1. Understand the task completely
2. Plan your approach (read before write)
3. Execute step by step
4. Verify your work
5. Summarize what was done

RESPONSE FORMAT:
## Task Summary
[What you were asked to do]

## Changes Made
- **file1.go**: [What changed and why]
- **file2.go**: [What changed and why]

## Verification
[How to verify the changes work]

## Summary
[Overall what was accomplished]

═══════════════════════════════════════════════════════════════════════

KEY RULES:
- ALWAYS read files before editing them
- Explain what you're changing and why
- Show before/after for significant changes
- Suggest how to verify the changes work

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildPlanPrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         PLAN AGENT (READ-ONLY)
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Design an implementation plan for the requested feature.
NOTE: You are READ-ONLY - you cannot modify files.

APPROACH:
1. Explore codebase to understand patterns
2. Identify files that need modification
3. Consider architectural trade-offs
4. Create detailed step-by-step plan

PLAN FORMAT:
## Overview
[Brief description of what will be implemented]

## Files to Modify
1. **path/to/file.go** - [What changes needed]
2. **path/to/other.go** - [What changes needed]

## Implementation Steps
### Step 1: [Title]
- [ ] Task 1.1
- [ ] Task 1.2

### Step 2: [Title]
- [ ] Task 2.1
- [ ] Task 2.2

## Testing Strategy
- Unit tests for [components]
- Integration tests for [flows]

## Risks & Considerations
- [Potential issue 1]: Mitigation
- [Potential issue 2]: Mitigation

═══════════════════════════════════════════════════════════════════════

KEY RULES:
- Be specific about file paths and line numbers
- Consider existing patterns in the codebase
- Break down into small, verifiable steps
- Identify potential risks upfront

═══════════════════════════════════════════════════════════════════════
`
}

func (a *Agent) buildGuidePrompt() string {
	return `═══════════════════════════════════════════════════════════════════════
                         GUIDE AGENT
═══════════════════════════════════════════════════════════════════════

YOUR MISSION: Answer questions about Gokin CLI and its features.

APPROACH:
1. Search documentation for accurate info
2. Provide clear explanations with examples
3. Include usage instructions
4. Help with troubleshooting

RESPONSE FORMAT:
## Answer
[Clear, direct answer to the question]

## Details
[In-depth explanation if needed]

## Examples
` + "```" + `bash
# Example usage
gokin [command] [options]
` + "```" + `

## Related Information
[Other relevant features or documentation]

═══════════════════════════════════════════════════════════════════════

KEY RULES:
- Be accurate - verify information before stating
- Include practical examples
- Mention relevant config options
- Link to related features

═══════════════════════════════════════════════════════════════════════
`
}

// executeLoop runs the function calling loop for the agent.
func (a *Agent) executeLoop(ctx context.Context, prompt string, output *strings.Builder) ([]*genai.Content, string, error) {
	// Add user prompt to history
	userContent := genai.NewContentFromText(prompt, genai.RoleUser)
	a.history = append(a.history, userContent)

	// Update progress
	a.SetProgress(1, a.maxTurns, "Processing request")

	// === Tree planning mode: Build plan tree if enabled ===
	if a.treePlanner != nil && a.planningMode {
		tree, err := a.treePlanner.BuildTree(ctx, prompt, a.planGoal)
		if err != nil {
			logging.Warn("failed to build plan tree, falling back to reactive mode", "error", err)
		} else {
			a.activePlan = tree
			if a.onText != nil {
				a.safeOnText(fmt.Sprintf("\n[Plan tree built: %d nodes, best path: %d steps]\n",
					tree.TotalNodes, len(tree.BestPath)))
			}

			// Notify plan approval callback for context compaction
			if a.onPlanApproved != nil {
				planSummary := a.treePlanner.GeneratePlanSummary(tree)
				a.onPlanApproved(planSummary)
			}

			// Set total steps to best path length
			a.totalSteps = len(tree.BestPath)
			a.currentStep = 0

			// === Interactive Plan Review ===
			if a.onInput != nil && a.requireApproval {
				if err := a.requestPlanApproval(ctx, tree); err != nil {
					return a.history, output.String(), err
				}
			} else if a.onText != nil {
				// Show plan tree even if approval not required
				a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
			}
		}
	}

	loopRecoveryTurns := 0
	replanAttempts := 0
	var i int
	for i = 0; i < a.maxTurns; i++ {
		select {
		case <-ctx.Done():
			return a.history, output.String(), ctx.Err()
		default:
		}

		// Check tokens and summarize if needed to prevent context overflow.
		// We do this BEFORE getting model response to ensure we have room.
		if a.tokenCounter != nil && a.summarizer != nil && a.ctxCfg != nil && a.ctxCfg.EnableAutoSummary {
			if err := a.checkAndSummarize(ctx); err != nil {
				logging.Warn("auto-summarization failed", "agent_id", a.ID, "error", err)
				if a.onText != nil {
					a.safeOnText("\n[Warning: context optimization failed — conversation may hit length limits]\n")
				}
			}
		}

		// Update progress at start of each turn
		if i > 0 && a.activePlan == nil {
			a.SetProgress(i+1, a.maxTurns, fmt.Sprintf("Turn %d: Executing tools", i+1))
		}

		// === Planned mode: Execute from plan tree ===
		if a.activePlan != nil {
			actions, err := a.treePlanner.GetReadyActions(a.activePlan)
			if err != nil {
				// No more actions in plan, check if completed
				a.safeOnText("\n[Plan completed or no more actions available]\n")
				a.activePlan = nil // Exit planned mode
			} else if len(actions) > 0 {
				type parallelResult struct {
					action *PlannedAction
					result *AgentResult
				}

				var wg sync.WaitGroup
				var resMu sync.Mutex
				results := make([]parallelResult, 0, len(actions))

				for _, act := range actions {
					wg.Add(1)
					go func(action *PlannedAction) {
						defer wg.Done()

						a.safeOnText(fmt.Sprintf("\n[Executing planned step: %s %s]\n",
							action.Type, action.AgentType))

						result := a.executePlannedAction(ctx, action)

						// Record result in tree (RecordResult is thread-safe)
						if err := a.treePlanner.RecordResult(a.activePlan, action.NodeID, result); err != nil {
							logging.Warn("failed to record plan result", "error", err)
						}

						resMu.Lock()
						results = append(results, parallelResult{action, result})
						resMu.Unlock()
					}(act)
				}
				wg.Wait()

				// Process results and collect failures
				var firstFailure *parallelResult
				for i := range results {
					res := &results[i]
					if res.result.Output != "" {
						output.WriteString(res.result.Output)
					}
					// Track first failure for potential replan
					if !res.result.IsSuccess() && firstFailure == nil {
						firstFailure = res
					}
				}

				// Handle failure with single replan attempt
				if firstFailure != nil {
					if a.treePlanner.ShouldReplan(a.activePlan, firstFailure.result) && replanAttempts < 3 {
						replanAttempts++

						// Build replan context with reflection
						var reflection *Reflection
						if a.reflector != nil && firstFailure.action.ToolName != "" {
							reflection = a.reflector.Analyze(firstFailure.action.ToolName, firstFailure.action.ToolArgs, firstFailure.result.Error)
						}

						// Find the node in the tree for replanning
						node, nodeFound := a.activePlan.GetNode(firstFailure.action.NodeID)
						if !nodeFound || node == nil {
							logging.Warn("failed node not found in tree, switching to reactive mode",
								"node_id", firstFailure.action.NodeID)
							a.activePlan = nil
							continue
						}

						replanCtx := &ReplanContext{
							FailedNode:    node,
							Error:         firstFailure.result.Error,
							Reflection:    reflection,
							AttemptNumber: replanAttempts,
						}

						a.safeOnText(fmt.Sprintf("\n[Replanning after failure of step \"%s\" (attempt %d)...]\n",
							firstFailure.action.Prompt, replanAttempts))

						if err := a.treePlanner.Replan(ctx, a.activePlan, replanCtx); err != nil {
							logging.Warn("replan failed", "error", err)
							a.activePlan = nil // Exit planned mode on replan failure
						}
					} else {
						// Max replans exceeded or should not replan
						a.safeOnText("\n[Plan failed, switching to reactive mode]\n")
						a.activePlan = nil
					}
				}
				continue
			}
		}

		// === Reactive mode: Get response from model ===
		resp, err := a.getModelResponse(ctx)
		if err != nil {
			return a.history, output.String(), fmt.Errorf("model response error: %w", err)
		}

		// Add model response to history
		modelContent := &genai.Content{
			Role:  genai.RoleModel,
			Parts: a.buildResponseParts(resp),
		}
		a.history = append(a.history, modelContent)

		// Accumulate text output
		if resp.Text != "" {
			output.WriteString(resp.Text)
			// Stream text to UI in real-time
			if a.onText != nil {
				a.safeOnText(resp.Text)
			}
		}

		// If there are function calls, execute them
		if len(resp.FunctionCalls) > 0 {
			// Track progress for delegation strategy
			if a.delegation != nil {
				toolsList := make([]string, 0, len(resp.FunctionCalls))
				for _, fc := range resp.FunctionCalls {
					toolsList = append(toolsList, fc.Name)
				}
				a.delegation.TrackProgress(strings.Join(toolsList, ","))
			}

			// Mental Loop Detection
			for _, fc := range resp.FunctionCalls {
				argsJSON, _ := json.Marshal(fc.Args)
				key := fmt.Sprintf("%s:%s", fc.Name, string(argsJSON))

				a.callHistoryMu.Lock()
				a.callHistory[key]++
				count := a.callHistory[key]
				intervened := a.loopIntervened
				a.callHistoryMu.Unlock()

				if count > 3 && !intervened {
					logging.Warn("mental loop detected", "tool", fc.Name, "count", count)
					a.callHistoryMu.Lock()
					a.loopIntervened = true
					a.callHistoryMu.Unlock()

					// Notify user
					if a.onText != nil {
						a.safeOnText(fmt.Sprintf("\n[Loop detected: %s called %d times with same args — intervening]\n", fc.Name, count))
					}

					// Inject intervention message
					intervention := fmt.Sprintf("Wait. I've noticed I'm calling %s with the same arguments repeatedly. I should stop and rethink my approach. Why isn't this working? What am I missing?", fc.Name)
					a.history = append(a.history, genai.NewContentFromText(intervention, genai.RoleUser))
					// Give bounded extra turns to recover (max 3)
					if loopRecoveryTurns < 3 {
						loopRecoveryTurns++
						a.maxTurns++
					}
					continue
				}
			}

			// Update progress to show tool execution
			toolsList := make([]string, 0, len(resp.FunctionCalls))
			for _, fc := range resp.FunctionCalls {
				toolsList = append(toolsList, fc.Name)
			}
			a.SetProgress(i+1, a.maxTurns, fmt.Sprintf("Executing tools: %v", toolsList))

			results := a.executeTools(ctx, resp.FunctionCalls)

			// Add function response to history
			funcParts := make([]*genai.Part, len(results))
			for j, result := range results {
				funcParts[j] = genai.NewPartFromFunctionResponse(result.Name, result.Response)
			}
			funcContent := &genai.Content{
				Role:  genai.RoleUser,
				Parts: funcParts,
			}
			a.history = append(a.history, funcContent)

			continue
		}

		// No more function calls, we're done
		break
	}

	// Notify user if the model produced no output
	if output.Len() == 0 {
		emptyMsg := "\n[Model returned an empty response — try rephrasing your request]\n"
		output.WriteString(emptyMsg)
		if a.onText != nil {
			a.safeOnText(emptyMsg)
		}
	}

	// Notify user if we hit the max turn limit
	if i >= a.maxTurns {
		if a.onText != nil {
			a.safeOnText("\n[Reached maximum turn limit — stopping]\n")
		}
	}

	return a.history, output.String(), nil
}

// checkAndSummarize monitors token usage and triggers summarization if thresholds are met.
func (a *Agent) checkAndSummarize(ctx context.Context) error {
	// 1. Get current token usage
	tokenCount, err := a.tokenCounter.CountContents(ctx, a.history)
	if err != nil {
		return fmt.Errorf("failed to count tokens: %w", err)
	}

	limits := a.tokenCounter.GetLimits()
	threshold := limits.WarningThreshold
	if threshold == 0 {
		threshold = 0.8
	}

	percentUsed := float64(tokenCount) / float64(limits.MaxInputTokens)

	// 2. If below threshold, do nothing
	if percentUsed < threshold {
		return nil
	}

	logging.Info("context threshold reached, compacting history",
		"agent_id", a.ID,
		"usage", fmt.Sprintf("%.1f%%", percentUsed*100),
		"tokens", tokenCount)

	// 3. Summarize history
	// We keep the system prompt (first 2 messages) and the last few turns
	if len(a.history) <= 6 {
		return nil // Not enough history to summarize effectively
	}

	// Keep first 2 (system context) and last 4 (recent turns)
	historyToSummarize := a.history[2 : len(a.history)-4]
	remainingHistory := a.history[len(a.history)-4:]

	summary, err := a.summarizer.Summarize(ctx, historyToSummarize)
	if err != nil {
		return fmt.Errorf("summarization failed: %w", err)
	}

	// 4. Reconstruct history: [System Context] + [Summary] + [Recent Turns]
	newHistory := make([]*genai.Content, 0, len(remainingHistory)+3)
	newHistory = append(newHistory, a.history[0], a.history[1])
	newHistory = append(newHistory, summary)
	newHistory = append(newHistory, remainingHistory...)

	a.history = newHistory

	logging.Info("context history compacted", "agent_id", a.ID, "new_message_count", len(a.history))

	return nil
}

// getModelResponse gets a response from the model.
func (a *Agent) getModelResponse(ctx context.Context) (*client.Response, error) {
	if len(a.history) == 0 {
		return nil, fmt.Errorf("empty history")
	}

	lastContent := a.history[len(a.history)-1]

	// Check if the last content contains function responses (tool results).
	// If so, use SendFunctionResponse instead of SendMessageWithHistory
	// to avoid sending an empty message string to APIs that reject it.
	if lastContent.Role == genai.RoleUser {
		var funcResponses []*genai.FunctionResponse
		for _, part := range lastContent.Parts {
			if part.FunctionResponse != nil {
				funcResponses = append(funcResponses, &genai.FunctionResponse{
					ID:       part.FunctionResponse.ID,
					Name:     part.FunctionResponse.Name,
					Response: part.FunctionResponse.Response,
				})
			}
		}

		if len(funcResponses) > 0 {
			// Route through SendFunctionResponse for proper API formatting
			historyWithoutLast := a.history[:len(a.history)-1]
			stream, err := a.client.SendFunctionResponse(ctx, historyWithoutLast, funcResponses)
			if err != nil {
				return nil, err
			}
			return stream.Collect()
		}
	}

	// Extract text message from last user content
	var message string
	if lastContent.Role == genai.RoleUser {
		for _, part := range lastContent.Parts {
			if part.Text != "" {
				message = part.Text
				break
			}
		}
	}

	// Safety: ensure message is not empty
	if message == "" {
		message = "Continue."
	}

	historyWithoutLast := a.history[:len(a.history)-1]
	stream, err := a.client.SendMessageWithHistory(ctx, historyWithoutLast, message)
	if err != nil {
		return nil, err
	}

	return stream.Collect()
}

// executeTools executes the function calls.
func (a *Agent) executeTools(ctx context.Context, calls []*genai.FunctionCall) []*genai.FunctionResponse {
	results := make([]*genai.FunctionResponse, len(calls))

	for i, call := range calls {
		result := a.executeTool(ctx, call)

		var reflection *Reflection

		// Apply self-reflection on errors to provide recovery suggestions
		if !result.Success && a.reflector != nil {
			reflection = a.reflector.Analyze(call.Name, call.Args, result.Content)
			if reflection.Intervention != "" {
				// Enrich the error result with reflection analysis
				result.Content = fmt.Sprintf("%s\n\n---\n**Self-Reflection:**\n%s",
					result.Content, reflection.Intervention)

				// Log reflection
				logging.Info("agent reflected on error",
					"agent_id", a.ID,
					"tool", call.Name,
					"category", reflection.Category,
					"should_retry", reflection.ShouldRetry)
			}
		}

		// Check for autonomous delegation opportunity
		if !result.Success && a.delegation != nil && a.delegation.messenger != nil {
			delCtx := &DelegationContext{
				AgentType:      a.Type,
				CurrentTurn:    a.currentStep,
				MaxTurns:       a.maxTurns,
				LastToolName:   call.Name,
				LastToolError:  result.Content,
				LastToolArgs:   call.Args,
				ReflectionInfo: reflection,
				StuckCount:     a.delegation.GetStuckCount(),
			}

			decision := a.delegation.Evaluate(delCtx)
			if decision.ShouldDelegate {
				// Execute delegation
				delegationResponse, err := a.delegation.ExecuteDelegation(ctx, decision)
				if err == nil && delegationResponse != "" {
					// Append delegation result to the tool response
					result.Content = fmt.Sprintf("%s\n\n---\n**Delegated to %s agent:**\n%s",
						result.Content, decision.TargetType, delegationResponse)
					result.Success = true // Mark as recovered

					logging.Info("delegation successful",
						"agent_id", a.ID,
						"delegated_to", decision.TargetType,
						"reason", decision.Reason)
				}
			}
		}

		// Compact result if it's too large before converting to map
		if a.compactor != nil {
			result = a.compactor.CompactForType(call.Name, result)
		}

		results[i] = &genai.FunctionResponse{
			Name:     call.Name,
			Response: result.ToMap(),
		}
	}

	return results
}

// executeTool executes a single tool call with enhanced safety and retry logic.
func (a *Agent) executeTool(ctx context.Context, call *genai.FunctionCall) tools.ToolResult {
	tool, ok := a.registry.Get(call.Name)
	if !ok {
		return tools.NewErrorResult(fmt.Sprintf("tool not available for this agent: %s", call.Name))
	}

	// Validate arguments
	if err := tool.Validate(call.Args); err != nil {
		return tools.NewErrorResult(fmt.Sprintf("validation error: %s", err))
	}

	// Check permissions before executing
	if a.permissions != nil {
		resp, err := a.permissions.Check(ctx, call.Name, call.Args)
		if err != nil {
			return tools.NewErrorResult(fmt.Sprintf("permission error: %s", err))
		}
		if !resp.Allowed {
			reason := resp.Reason
			if reason == "" {
				reason = "permission denied"
			}
			return tools.NewErrorResult(fmt.Sprintf("Permission denied: %s", reason))
		}
	}

	// Report tool start to UI
	if a.onToolActivity != nil {
		a.onToolActivity(a.ID, call.Name, call.Args, "start")
	}

	// === IMPROVEMENT 2: Retry mechanism with exponential backoff ===
	maxRetries := 3
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Execute the tool with parent context for cancellation support
		// The tool itself (e.g., bash) is responsible for implementing its own timeout
		result, err := tool.Execute(ctx, call.Args)
		if err == nil {
			// Report tool end to UI
			if a.onToolActivity != nil {
				a.onToolActivity(a.ID, call.Name, call.Args, "end")
			}
			// Success - return result
			return result
		}

		// Record error for potential retry
		lastErr = err

		// Check if this is a retryable error
		if !isRetryableError(err) || attempt == maxRetries-1 {
			// Not retryable or last attempt - return error immediately
			return tools.NewErrorResult(err.Error())
		}

		// Log retry attempt
		logging.Warn("tool execution failed, retrying",
			"tool", call.Name,
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err.Error())

		// Exponential backoff: 1s, 2s, 4s...
		backoffDuration := time.Duration(1<<uint(attempt)) * time.Second
		select {
		case <-time.After(backoffDuration):
			// Continue to next attempt
		case <-ctx.Done():
			// Context cancelled - stop retrying
			return tools.NewErrorResult("cancelled during retry backoff")
		}
	}

	// All retries exhausted
	return tools.NewErrorResult(fmt.Sprintf("failed after %d retries: %s", maxRetries, lastErr.Error()))
}

// isRetryableError determines if an error is worth retrying
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := strings.ToLower(err.Error())

	// Network/timeout errors are retryable
	retryablePatterns := []string{
		"timeout",
		"connection refused",
		"connection reset",
		"temporary failure",
		"rate limit",
		"deadline exceeded",
		"context deadline exceeded",
	}

	for _, pattern := range retryablePatterns {
		if strings.Contains(errMsg, pattern) {
			return true
		}
	}

	// By default, don't retry
	return false
}

// buildResponseParts creates Parts from a response.
// Returns at least one part to avoid empty Parts which causes API errors.
func (a *Agent) buildResponseParts(resp *client.Response) []*genai.Part {
	var parts []*genai.Part

	if resp.Text != "" {
		parts = append(parts, genai.NewPartFromText(resp.Text))
	}

	for _, fc := range resp.FunctionCalls {
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}

	// Ensure we never return empty parts - API requires at least one part
	if len(parts) == 0 {
		parts = append(parts, genai.NewPartFromText(" "))
	}

	return parts
}

// GetStatus returns the current agent status.
func (a *Agent) GetStatus() AgentStatus {
	return a.status
}

// Cancel cancels the agent's execution.
func (a *Agent) Cancel() {
	if a.status == AgentStatusRunning {
		a.status = AgentStatusCancelled
		a.endTime = time.Now()
	}
}

// safeOnText streams text to the UI in a thread-safe manner.
func (a *Agent) safeOnText(text string) {
	if a.onText == nil {
		return
	}
	a.onTextMu.Lock()
	defer a.onTextMu.Unlock()
	a.onText(text)
}

// executePlannedAction executes a single planned action and returns the result.
func (a *Agent) executePlannedAction(ctx context.Context, action *PlannedAction) *AgentResult {
	if action == nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "nil action",
		}
	}

	startTime := time.Now()

	switch action.Type {
	case ActionToolCall:
		return a.executeToolAction(ctx, action, startTime)
	case ActionDelegate:
		return a.executeDelegateAction(ctx, action, startTime)
	case ActionVerify:
		return a.executeVerifyAction(ctx, action, startTime)
	case ActionDecompose:
		return a.executeDecomposeAction(ctx, action, startTime)
	default:
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   fmt.Sprintf("unknown action type: %s", action.Type),
		}
	}
}

// executeDecomposeAction handles a decomposition milestone.
func (a *Agent) executeDecomposeAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	if a.activePlan == nil || a.treePlanner == nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "no active plan or tree planner",
		}
	}

	// Find the node in the active plan
	node, ok := a.activePlan.GetNode(action.NodeID)
	if !ok {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   "node not found in plan",
		}
	}

	if a.onText != nil {
		a.safeOnText(fmt.Sprintf("\n[Expanding milestone: %s]\n", action.Prompt))
	}

	// Expand the milestone into sub-tasks
	if err := a.treePlanner.ExpandMilestone(ctx, a.activePlan, node); err != nil {
		return &AgentResult{
			AgentID: a.ID,
			Type:    a.Type,
			Status:  AgentStatusFailed,
			Error:   fmt.Sprintf("decomposition failed: %v", err),
		}
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    fmt.Sprintf("Milestone expanded: %s", action.Prompt),
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeToolAction executes a tool call action.
func (a *Agent) executeToolAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	call := &genai.FunctionCall{
		Name: action.ToolName,
		Args: action.ToolArgs,
	}

	result := a.executeTool(ctx, call)

	status := AgentStatusCompleted
	errMsg := ""
	if !result.Success {
		status = AgentStatusFailed
		errMsg = result.Content
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    status,
		Output:    result.Content,
		Error:     errMsg,
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeDelegateAction delegates work to a sub-agent.
func (a *Agent) executeDelegateAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	if a.delegation == nil || a.delegation.messenger == nil {
		// No delegation support, execute directly with current agent
		return a.executeDirectly(ctx, action, startTime)
	}

	// Request delegation through messenger
	decision := &DelegationDecision{
		ShouldDelegate: true,
		TargetType:     string(action.AgentType),
		Reason:         "planned delegation",
		Query:          action.Prompt,
	}

	response, err := a.delegation.ExecuteDelegation(ctx, decision)
	if err != nil {
		return &AgentResult{
			AgentID:   a.ID,
			Type:      a.Type,
			Status:    AgentStatusFailed,
			Error:     fmt.Sprintf("delegation failed: %v", err),
			Duration:  time.Since(startTime),
			Completed: true,
		}
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    response,
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeDirectly executes an action without delegation.
func (a *Agent) executeDirectly(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	// For non-delegation actions, run the prompt through the model
	var output strings.Builder

	// Add the action prompt to history temporarily
	promptContent := genai.NewContentFromText(action.Prompt, genai.RoleUser)
	a.history = append(a.history, promptContent)

	// Get model response
	resp, err := a.getModelResponse(ctx)
	if err != nil {
		return &AgentResult{
			AgentID:   a.ID,
			Type:      a.Type,
			Status:    AgentStatusFailed,
			Error:     err.Error(),
			Duration:  time.Since(startTime),
			Completed: true,
		}
	}

	// Process response
	if resp.Text != "" {
		output.WriteString(resp.Text)
	}

	// Execute any function calls
	if len(resp.FunctionCalls) > 0 {
		results := a.executeTools(ctx, resp.FunctionCalls)
		for _, r := range results {
			if r.Response != nil {
				if content, ok := r.Response["content"].(string); ok {
					output.WriteString("\n")
					output.WriteString(content)
				}
			}
		}
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    output.String(),
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// executeVerifyAction runs verification checks.
func (a *Agent) executeVerifyAction(ctx context.Context, action *PlannedAction, startTime time.Time) *AgentResult {
	// Verification typically involves running tests or checking criteria
	var output strings.Builder

	// Use bash agent to run tests if available
	verifyPrompt := "Verify the implementation is complete. " + action.Prompt

	if a.delegation != nil && a.delegation.messenger != nil {
		decision := &DelegationDecision{
			ShouldDelegate: true,
			TargetType:     string(AgentTypeBash),
			Reason:         "verification",
			Query:          "Run tests to verify: " + verifyPrompt,
		}

		response, err := a.delegation.ExecuteDelegation(ctx, decision)
		if err != nil {
			return &AgentResult{
				AgentID:   a.ID,
				Type:      a.Type,
				Status:    AgentStatusFailed,
				Error:     fmt.Sprintf("verification failed: %v", err),
				Duration:  time.Since(startTime),
				Completed: true,
			}
		}

		output.WriteString(response)

		// Check for test failures in output
		if strings.Contains(strings.ToLower(response), "fail") ||
			strings.Contains(strings.ToLower(response), "error") {
			return &AgentResult{
				AgentID:   a.ID,
				Type:      a.Type,
				Status:    AgentStatusFailed,
				Output:    output.String(),
				Error:     "verification detected failures",
				Duration:  time.Since(startTime),
				Completed: true,
			}
		}
	} else {
		output.WriteString("Verification step (no test runner available)")
	}

	return &AgentResult{
		AgentID:   a.ID,
		Type:      a.Type,
		Status:    AgentStatusCompleted,
		Output:    output.String(),
		Duration:  time.Since(startTime),
		Completed: true,
	}
}

// requestPlanApproval handles the interactive review and editing of a plan.
func (a *Agent) requestPlanApproval(ctx context.Context, tree *PlanTree) error {
	if a.onInput == nil || a.onText == nil {
		return nil
	}

	for {
		// Show current plan
		a.safeOnText("\n" + a.treePlanner.GenerateVisualTree(tree) + "\n")
		a.safeOnText("Commands: [Enter] approve | e <n> <prompt> | d <n> | a [type] <prompt> | c cancel\n")
		a.safeOnText("Types: explore, plan, general, bash, decompose (default: general)\n")

		response, err := a.onInput("Plan approval > ")
		if err != nil {
			return err
		}

		response = strings.TrimSpace(response)
		if response == "" {
			// Approved
			a.safeOnText("[Plan approved]\n")
			return nil
		}

		parts := strings.Fields(response)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "c", "cancel", "abort":
			return fmt.Errorf("plan rejected by user")
		case "e", "edit":
			if len(parts) < 3 {
				a.safeOnText("Usage: e <num> <new prompt>\n")
				continue
			}
			var num int
			if _, err := fmt.Sscanf(parts[1], "%d", &num); err != nil {
				a.safeOnText("Invalid step number\n")
				continue
			}
			if num < 1 || num > len(tree.BestPath) {
				a.safeOnText("Step number out of range\n")
				continue
			}

			newPrompt := strings.Join(parts[2:], " ")
			tree.BestPath[num-1].Action.Prompt = newPrompt
			a.safeOnText(fmt.Sprintf("Step %d updated\n", num))

		case "d", "delete":
			if len(parts) < 2 {
				a.safeOnText("Usage: d <num>\n")
				continue
			}
			var num int
			if _, err := fmt.Sscanf(parts[1], "%d", &num); err != nil {
				a.safeOnText("Invalid step number\n")
				continue
			}
			if num < 1 || num > len(tree.BestPath) {
				a.safeOnText("Step number out of range\n")
				continue
			}

			// Remove node from best path
			tree.BestPath = append(tree.BestPath[:num-1], tree.BestPath[num:]...)
			a.safeOnText(fmt.Sprintf("Step %d deleted\n", num))

		case "a", "add":
			if len(parts) < 2 {
				a.safeOnText("Usage: a <prompt>\n")
				continue
			}
			prompt := strings.Join(parts[1:], " ")
			agentType := AgentTypeGeneral

			// Check if first word of prompt is a known type
			if len(parts) > 2 {
				potentialType := ParseAgentType(parts[1])
				if potentialType != "" || parts[1] == "decompose" {
					agentType = potentialType
					if parts[1] == "decompose" {
						agentType = AgentTypePlan // Use plan agent for decompose milestones
					}
					prompt = strings.Join(parts[2:], " ")
				}
			}

			// Add as child of root for now (end of plan)
			tree.AddNode(tree.Root.ID, &PlannedAction{
				Type:      ActionDelegate,
				AgentType: agentType,
				Prompt:    prompt,
			})
			if agentType == "" { // Was decompose
				node, _ := tree.GetNode(tree.Root.ID)
				if len(node.Children) > 0 {
					lastChild := node.Children[len(node.Children)-1]
					lastChild.Action.Type = ActionDecompose
				}
			}
			tree.BestPath = a.treePlanner.SelectBestPath(tree)
			a.safeOnText("[Step added]\n")

		default:
			a.safeOnText(fmt.Sprintf("Unknown command: %s\n", cmd))
		}
	}
}
