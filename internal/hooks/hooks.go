package hooks

import (
	"os"
	"strings"
)

// Type represents when a hook should be triggered.
type Type string

const (
	// PreTool runs before a tool executes.
	PreTool Type = "pre_tool"
	// PostTool runs after a tool executes successfully.
	PostTool Type = "post_tool"
	// OnError runs when a tool fails.
	OnError Type = "on_error"
	// OnStart runs when the application starts.
	OnStart Type = "on_start"
	// OnExit runs when the application exits.
	OnExit Type = "on_exit"
)

// Hook represents a configured hook.
type Hook struct {
	Name     string `yaml:"name"`      // Human-readable name
	Type     Type   `yaml:"type"`      // When to trigger
	ToolName string `yaml:"tool_name"` // Which tool triggers this (empty = all)
	Command  string `yaml:"command"`   // Shell command to execute
	Enabled  bool   `yaml:"enabled"`   // Whether hook is active
}

// Context provides data to hooks for variable substitution.
type Context struct {
	ToolName   string            // Name of the tool being executed
	ToolArgs   map[string]any    // Arguments passed to the tool
	ToolResult string            // Result from tool (post_tool only)
	ToolError  string            // Error message (on_error only)
	WorkDir    string            // Working directory
	Extra      map[string]string // Additional variables
}

// NewContext creates a new hook context.
func NewContext(toolName string, args map[string]any, workDir string) *Context {
	return &Context{
		ToolName: toolName,
		ToolArgs: args,
		WorkDir:  workDir,
		Extra:    make(map[string]string),
	}
}

// SetResult sets the tool result for post-tool hooks.
func (c *Context) SetResult(result string) {
	c.ToolResult = result
}

// SetError sets the error for on-error hooks.
func (c *Context) SetError(err string) {
	c.ToolError = err
}

// ExpandCommand expands variables in the hook command.
// Supported variables:
//   - ${TOOL_NAME} - name of the tool
//   - ${FILE_PATH} - file_path argument if present
//   - ${COMMAND} - command argument if present (bash)
//   - ${PATTERN} - pattern argument if present (glob/grep)
//   - ${WORK_DIR} - working directory
//   - ${RESULT} - tool result (post_tool only)
//   - ${ERROR} - error message (on_error only)
//   - Any environment variable
func (c *Context) ExpandCommand(command string) string {
	result := command

	// Built-in variables
	result = strings.ReplaceAll(result, "${TOOL_NAME}", c.ToolName)
	result = strings.ReplaceAll(result, "${WORK_DIR}", c.WorkDir)
	result = strings.ReplaceAll(result, "${RESULT}", c.ToolResult)
	result = strings.ReplaceAll(result, "${ERROR}", c.ToolError)

	// Tool arguments
	if filePath, ok := c.ToolArgs["file_path"].(string); ok {
		result = strings.ReplaceAll(result, "${FILE_PATH}", filePath)
	}
	if cmd, ok := c.ToolArgs["command"].(string); ok {
		result = strings.ReplaceAll(result, "${COMMAND}", cmd)
	}
	if pattern, ok := c.ToolArgs["pattern"].(string); ok {
		result = strings.ReplaceAll(result, "${PATTERN}", pattern)
	}
	if content, ok := c.ToolArgs["content"].(string); ok {
		// Truncate content if too long
		if len(content) > 100 {
			content = content[:100] + "..."
		}
		result = strings.ReplaceAll(result, "${CONTENT}", content)
	}

	// Extra variables
	for key, val := range c.Extra {
		result = strings.ReplaceAll(result, "${"+key+"}", val)
	}

	// Environment variables (fallback)
	result = os.Expand(result, func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return ""
	})

	return result
}

// Matches checks if the hook should trigger for the given context.
func (h *Hook) Matches(hookType Type, toolName string) bool {
	if !h.Enabled {
		return false
	}
	if h.Type != hookType {
		return false
	}
	// Empty tool_name means match all tools
	if h.ToolName == "" {
		return true
	}
	return h.ToolName == toolName
}
