package agent

import (
	"time"
)

// AgentType defines the type of agent and its capabilities.
type AgentType string

const (
	// AgentTypeExplore is for exploring and searching codebases.
	// Tools: read, glob, grep, tree, list_dir
	AgentTypeExplore AgentType = "explore"

	// AgentTypeBash is for executing shell commands.
	// Tools: bash, read, glob
	AgentTypeBash AgentType = "bash"

	// AgentTypeGeneral is a general-purpose agent with access to all tools.
	AgentTypeGeneral AgentType = "general"

	// AgentTypePlan is for designing implementation strategies.
	// Tools: read-only exploration + planning tools
	AgentTypePlan AgentType = "plan"

	// AgentTypeGuide is for answering questions about Claude Code.
	// Tools: documentation/search focused
	AgentTypeGuide AgentType = "claude-code-guide"
)

// AllowedTools returns the list of tools allowed for this agent type.
func (t AgentType) AllowedTools() []string {
	switch t {
	case AgentTypeExplore:
		return []string{
			"read", "glob", "grep", "tree", "list_dir",
			"tools_list", "request_tool", "ask_agent",
		}
	case AgentTypeBash:
		return []string{"bash", "read", "glob", "tools_list", "request_tool", "ask_agent"}
	case AgentTypeGeneral:
		return nil // nil means all tools allowed
	case AgentTypePlan:
		// Read-only exploration + planning tools
		return []string{
			"read", "glob", "grep", "tree", "list_dir", "diff",
			"todo", "web_fetch", "web_search", "ask_user", "env",
			"tools_list", "request_tool", "ask_agent",
		}
	case AgentTypeGuide:
		// Documentation/search focused
		return []string{"glob", "grep", "read", "web_fetch", "web_search", "tools_list", "request_tool", "ask_agent"}
	default:
		return []string{}
	}
}

// String returns the string representation of the agent type.
func (t AgentType) String() string {
	return string(t)
}

// ParseAgentType parses a string into an AgentType.
func ParseAgentType(s string) AgentType {
	switch s {
	case "explore":
		return AgentTypeExplore
	case "bash":
		return AgentTypeBash
	case "general":
		return AgentTypeGeneral
	case "plan":
		return AgentTypePlan
	case "claude-code-guide":
		return AgentTypeGuide
	default:
		return AgentTypeGeneral
	}
}

// AgentStatus represents the current status of an agent.
type AgentStatus string

const (
	AgentStatusPending   AgentStatus = "pending"
	AgentStatusRunning   AgentStatus = "running"
	AgentStatusCompleted AgentStatus = "completed"
	AgentStatusFailed    AgentStatus = "failed"
	AgentStatusCancelled AgentStatus = "cancelled"
)

// AgentResult contains the result of an agent's execution.
type AgentResult struct {
	AgentID   string        `json:"agent_id"`
	Type      AgentType     `json:"type"`
	Status    AgentStatus   `json:"status"`
	Output    string        `json:"output"`
	Error     string        `json:"error,omitempty"`
	Duration  time.Duration `json:"duration"`
	Completed bool          `json:"completed"`
}

// AgentTask represents a task to be executed by an agent.
type AgentTask struct {
	Prompt      string    `json:"prompt"`
	Type        AgentType `json:"type"`
	Background  bool      `json:"background"`
	Description string    `json:"description,omitempty"`
	MaxTurns    int       `json:"max_turns,omitempty"`
	Model       string    `json:"model,omitempty"`
}

// IsSuccess returns true if the agent completed successfully.
func (r *AgentResult) IsSuccess() bool {
	return r.Status == AgentStatusCompleted && r.Error == ""
}
