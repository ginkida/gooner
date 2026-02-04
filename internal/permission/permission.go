package permission

import "fmt"

// Level represents the permission level for a tool.
type Level string

const (
	// LevelAllow allows the tool to execute without asking.
	LevelAllow Level = "allow"
	// LevelAsk prompts the user before executing.
	LevelAsk Level = "ask"
	// LevelDeny denies execution of the tool.
	LevelDeny Level = "deny"
)

// RiskLevel indicates how risky a tool operation is.
type RiskLevel int

const (
	// RiskLow for read-only operations (read, glob, grep).
	RiskLow RiskLevel = iota
	// RiskMedium for file modifications (write, edit).
	RiskMedium
	// RiskHigh for system operations (bash).
	RiskHigh
)

func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	default:
		return "unknown"
	}
}

// Request represents a permission request for a tool execution.
type Request struct {
	ToolName  string         // Name of the tool
	Args      map[string]any // Arguments passed to the tool
	RiskLevel RiskLevel      // Risk level of the operation
	Reason    string         // Human-readable reason for the request
}

// NewRequest creates a new permission request.
func NewRequest(toolName string, args map[string]any) *Request {
	return &Request{
		ToolName:  toolName,
		Args:      args,
		RiskLevel: GetToolRiskLevel(toolName),
		Reason:    buildReason(toolName, args),
	}
}

// Decision represents the user's decision on a permission request.
type Decision int

const (
	// DecisionPending means the user hasn't decided yet.
	DecisionPending Decision = iota
	// DecisionAllow allows this specific execution.
	DecisionAllow
	// DecisionAllowSession allows this tool for the session.
	DecisionAllowSession
	// DecisionDeny denies this specific execution.
	DecisionDeny
	// DecisionDenySession denies this tool for the session.
	DecisionDenySession
)

// Response represents the result of a permission check.
type Response struct {
	Allowed  bool
	Decision Decision
	Reason   string
}

// GetToolRiskLevel returns the risk level for a tool.
func GetToolRiskLevel(toolName string) RiskLevel {
	switch toolName {
	case "read", "glob", "grep", "tree", "diff", "env", "list_dir":
		return RiskLow
	case "write", "edit":
		return RiskMedium
	case "bash":
		return RiskHigh
	default:
		return RiskMedium
	}
}

// buildReason creates a human-readable reason for the permission request.
func buildReason(toolName string, args map[string]any) string {
	switch toolName {
	case "write":
		if path, ok := args["file_path"].(string); ok {
			return fmt.Sprintf("Write to file: %s", path)
		}
		return "Write to file"

	case "edit":
		if path, ok := args["file_path"].(string); ok {
			return fmt.Sprintf("Edit file: %s", path)
		}
		return "Edit file"

	case "bash":
		if cmd, ok := args["command"].(string); ok {
			if len(cmd) > 150 {
				cmd = cmd[:147] + "..."
			}
			return fmt.Sprintf("Execute command: %s", cmd)
		}
		return "Execute shell command"

	case "read":
		if path, ok := args["file_path"].(string); ok {
			return fmt.Sprintf("Read file: %s", path)
		}
		return "Read file"

	case "glob":
		if pattern, ok := args["pattern"].(string); ok {
			return fmt.Sprintf("Search files matching: %s", pattern)
		}
		return "Search files"

	case "grep":
		if pattern, ok := args["pattern"].(string); ok {
			return fmt.Sprintf("Search content: %s", pattern)
		}
		return "Search file contents"

	default:
		return fmt.Sprintf("Execute tool: %s", toolName)
	}
}
