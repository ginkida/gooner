package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"
)

// AgentProgress represents progress information from an agent.
type AgentProgress struct {
	AgentID       string
	CurrentStep   int
	TotalSteps    int
	CurrentAction string
	Elapsed       time.Duration
	ToolsUsed     []string
}

// AgentRunner interface for spawning and managing agents.
// This is implemented by agent.Runner to avoid import cycles.
type AgentRunner interface {
	Spawn(ctx context.Context, agentType string, prompt string, maxTurns int, model string) (string, error)
	SpawnAsync(ctx context.Context, agentType string, prompt string, maxTurns int, model string) string
	SpawnAsyncWithStreaming(ctx context.Context, agentType string, prompt string, maxTurns int, model string, onText func(string), onProgress func(id string, progress *AgentProgress)) string
	Resume(ctx context.Context, agentID string, prompt string) (string, error)
	ResumeAsync(ctx context.Context, agentID string, prompt string) (string, error)
	GetResult(agentID string) (AgentResult, bool)
}

// AgentResult represents the result from an agent execution.
type AgentResult struct {
	AgentID   string
	Type      string
	Status    string
	Output    string
	Error     string
	Duration  time.Duration
	Completed bool
}

// TaskTool spawns subagents to handle complex tasks.
type TaskTool struct {
	runner AgentRunner
}

// NewTaskTool creates a new TaskTool instance.
func NewTaskTool() *TaskTool {
	return &TaskTool{}
}

// SetRunner sets the agent runner for spawning subagents.
func (t *TaskTool) SetRunner(runner AgentRunner) {
	t.runner = runner
}

func (t *TaskTool) Name() string {
	return "task"
}

func (t *TaskTool) Description() string {
	return `Spawns a specialized subagent to handle complex tasks autonomously.
Agent types:
- explore: Codebase exploration (read, glob, grep, tree, list_dir)
- bash: Command execution (bash, read, glob)
- general: All tools available
- plan: Implementation planning (read-only exploration + planning tools)
- claude-code-guide: Answer questions about Claude Code CLI (documentation/search focused)

Use for multi-step tasks, parallel exploration, or isolated command execution.`
}

func (t *TaskTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"prompt": {
					Type:        genai.TypeString,
					Description: "The task for the subagent to perform",
				},
				"subagent_type": {
					Type:        genai.TypeString,
					Description: "Type of agent: 'explore', 'bash', 'general', 'plan', or 'claude-code-guide'",
					Enum:        []string{"explore", "bash", "general", "plan", "claude-code-guide"},
				},
				"description": {
					Type:        genai.TypeString,
					Description: "A short description of the task (3-5 words)",
				},
				"run_in_background": {
					Type:        genai.TypeBoolean,
					Description: "If true, run the agent in the background and return immediately",
				},
				"max_turns": {
					Type:        genai.TypeInteger,
					Description: "Maximum number of agentic turns (API round-trips) before stopping. Default: 30.",
				},
				"model": {
					Type:        genai.TypeString,
					Description: "Model to use: 'flash' (fast), 'pro' (balanced). If not specified, inherits from parent.",
					Enum:        []string{"flash", "pro"},
				},
				"resume": {
					Type:        genai.TypeString,
					Description: "Agent ID to resume from previous execution. If provided, continues from saved state.",
				},
			},
			Required: []string{"prompt"},
		},
	}
}

func (t *TaskTool) Validate(args map[string]any) error {
	prompt, ok := GetString(args, "prompt")
	if !ok || prompt == "" {
		return NewValidationError("prompt", "is required")
	}

	// If resuming, we don't need subagent_type
	resume, _ := GetString(args, "resume")
	if resume != "" {
		return nil
	}

	agentType, ok := GetString(args, "subagent_type")
	if !ok || agentType == "" {
		return NewValidationError("subagent_type", "is required when not resuming")
	}

	switch agentType {
	case "explore", "bash", "general", "plan", "claude-code-guide":
		// Valid types
	default:
		return NewValidationError("subagent_type", "must be 'explore', 'bash', 'general', 'plan', or 'claude-code-guide'")
	}

	return nil
}

func (t *TaskTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.runner == nil {
		return NewErrorResult("task runner not initialized"), nil
	}

	prompt, _ := GetString(args, "prompt")
	agentType, _ := GetString(args, "subagent_type")
	description := GetStringDefault(args, "description", "")
	runInBackground := GetBoolDefault(args, "run_in_background", false)
	maxTurns := GetIntDefault(args, "max_turns", 30)
	model := GetStringDefault(args, "model", "")
	resume := GetStringDefault(args, "resume", "")

	// If resuming an existing agent
	if resume != "" {
		if runInBackground {
			return t.executeResumeBackground(ctx, resume, prompt, description)
		}
		return t.executeResumeForeground(ctx, resume, prompt, description)
	}

	if runInBackground {
		return t.executeBackground(ctx, agentType, prompt, description, maxTurns, model)
	}

	return t.executeForeground(ctx, agentType, prompt, description, maxTurns, model)
}

func (t *TaskTool) executeForeground(ctx context.Context, agentType, prompt, description string, maxTurns int, model string) (ToolResult, error) {
	agentID, err := t.runner.Spawn(ctx, agentType, prompt, maxTurns, model)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("Agent failed: %s", err)), nil
	}

	result, ok := t.runner.GetResult(agentID)
	if !ok {
		return NewErrorResult("Failed to get agent result"), nil
	}

	var output strings.Builder

	// Header
	if description != "" {
		output.WriteString(fmt.Sprintf("## Task: %s\n\n", description))
	}
	output.WriteString(fmt.Sprintf("Agent ID: %s\n", result.AgentID))
	output.WriteString(fmt.Sprintf("Type: %s\n", result.Type))
	output.WriteString(fmt.Sprintf("Status: %s\n", result.Status))
	output.WriteString(fmt.Sprintf("Duration: %s\n\n", result.Duration.Round(time.Millisecond)))

	// Result
	if result.Error != "" {
		output.WriteString(fmt.Sprintf("**Error:** %s\n\n", result.Error))
	}

	if result.Output != "" {
		output.WriteString("### Output:\n")
		output.WriteString(result.Output)
	}

	return NewSuccessResultWithData(output.String(), map[string]any{
		"agent_id": result.AgentID,
		"type":     result.Type,
		"status":   result.Status,
		"duration": result.Duration.String(),
	}), nil
}

func (t *TaskTool) executeBackground(ctx context.Context, agentType, prompt, description string, maxTurns int, model string) (ToolResult, error) {
	// Check for streaming callback in context
	onText := GetStreamingCallback(ctx)
	onProgress := GetProgressCallback(ctx)

	var agentID string
	if onText != nil || onProgress != nil {
		// Use streaming-enabled spawn
		var progressAdapter func(id string, progress *AgentProgress)
		if onProgress != nil {
			progressAdapter = func(id string, progress *AgentProgress) {
				if progress != nil {
					pct := float64(progress.CurrentStep) / float64(max(progress.TotalSteps, 1))
					onProgress(pct, progress.CurrentAction)
				}
			}
		}
		agentID = t.runner.SpawnAsyncWithStreaming(ctx, agentType, prompt, maxTurns, model, onText, progressAdapter)
	} else {
		// Use standard async spawn
		agentID = t.runner.SpawnAsync(ctx, agentType, prompt, maxTurns, model)
	}

	var output strings.Builder
	output.WriteString("Agent spawned in background.\n\n")
	output.WriteString(fmt.Sprintf("Agent ID: %s\n", agentID))
	output.WriteString(fmt.Sprintf("Type: %s\n", agentType))
	if description != "" {
		output.WriteString(fmt.Sprintf("Task: %s\n", description))
	}
	if onText != nil {
		output.WriteString("Streaming: enabled\n")
	}
	output.WriteString("\nUse task_output with this agent_id to check status and results.")

	return NewSuccessResultWithData(output.String(), map[string]any{
		"agent_id":   agentID,
		"type":       agentType,
		"background": true,
		"streaming":  onText != nil,
	}), nil
}

func (t *TaskTool) executeResumeForeground(ctx context.Context, agentID, prompt, description string) (ToolResult, error) {
	resumedID, err := t.runner.Resume(ctx, agentID, prompt)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("Failed to resume agent: %s", err)), nil
	}

	result, ok := t.runner.GetResult(resumedID)
	if !ok {
		return NewErrorResult("Failed to get agent result"), nil
	}

	var output strings.Builder

	// Header
	output.WriteString("## Agent Resumed\n\n")
	if description != "" {
		output.WriteString(fmt.Sprintf("Task: %s\n", description))
	}
	output.WriteString(fmt.Sprintf("Agent ID: %s\n", result.AgentID))
	output.WriteString(fmt.Sprintf("Type: %s\n", result.Type))
	output.WriteString(fmt.Sprintf("Status: %s\n", result.Status))
	output.WriteString(fmt.Sprintf("Duration: %s\n\n", result.Duration.Round(time.Millisecond)))

	// Result
	if result.Error != "" {
		output.WriteString(fmt.Sprintf("**Error:** %s\n\n", result.Error))
	}

	if result.Output != "" {
		output.WriteString("### Output:\n")
		output.WriteString(result.Output)
	}

	return NewSuccessResultWithData(output.String(), map[string]any{
		"agent_id": result.AgentID,
		"type":     result.Type,
		"status":   result.Status,
		"duration": result.Duration.String(),
		"resumed":  true,
	}), nil
}

func (t *TaskTool) executeResumeBackground(ctx context.Context, agentID, prompt, description string) (ToolResult, error) {
	resumedID, err := t.runner.ResumeAsync(ctx, agentID, prompt)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("Failed to resume agent: %s", err)), nil
	}

	var output strings.Builder
	output.WriteString("Agent resumed in background.\n\n")
	output.WriteString(fmt.Sprintf("Agent ID: %s\n", resumedID))
	if description != "" {
		output.WriteString(fmt.Sprintf("Task: %s\n", description))
	}
	output.WriteString("\nUse task_output with this agent_id to check status and results.")

	return NewSuccessResultWithData(output.String(), map[string]any{
		"agent_id":   resumedID,
		"background": true,
		"resumed":    true,
	}), nil
}
