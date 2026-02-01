package mcp

import "time"

// JSON-RPC 2.0 types

// JSONRPCMessage represents a JSON-RPC 2.0 message (request, response, or notification).
type JSONRPCMessage struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`      // Can be string, int, or nil for notifications
	Method  string `json:"method,omitempty"`  // For requests/notifications
	Params  any    `json:"params,omitempty"`  // For requests/notifications
	Result  any    `json:"result,omitempty"`  // For successful responses
	Error   *Error `json:"error,omitempty"`   // For error responses
}

// IsRequest returns true if the message is a request (has ID and method).
func (m *JSONRPCMessage) IsRequest() bool {
	return m.ID != nil && m.Method != ""
}

// IsNotification returns true if the message is a notification (has method but no ID).
func (m *JSONRPCMessage) IsNotification() bool {
	return m.ID == nil && m.Method != ""
}

// IsResponse returns true if the message is a response (has ID but no method).
func (m *JSONRPCMessage) IsResponse() bool {
	return m.ID != nil && m.Method == ""
}

// Error represents a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return e.Message
}

// Standard JSON-RPC error codes
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
)

// MCP Protocol types

// ServerInfo contains information about the MCP server.
type ServerInfo struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Capabilities *ServerCapability `json:"capabilities,omitempty"`
}

// ServerCapability describes server capabilities.
type ServerCapability struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
}

// ToolsCapability describes tool capabilities.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"` // Server supports tools/list_changed notifications
}

// ResourcesCapability describes resource capabilities.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`   // Server supports resource subscriptions
	ListChanged bool `json:"listChanged,omitempty"` // Server supports resources/list_changed notifications
}

// PromptsCapability describes prompt capabilities.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"` // Server supports prompts/list_changed notifications
}

// ClientInfo contains information about the MCP client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams are the parameters for the initialize request.
type InitializeParams struct {
	ProtocolVersion string      `json:"protocolVersion"`
	ClientInfo      *ClientInfo `json:"clientInfo"`
	Capabilities    any         `json:"capabilities,omitempty"`
}

// InitializeResult is the result of the initialize request.
type InitializeResult struct {
	ProtocolVersion string      `json:"protocolVersion"`
	ServerInfo      *ServerInfo `json:"serverInfo"`
	Capabilities    any         `json:"capabilities,omitempty"`
}

// ToolInfo describes an MCP tool.
type ToolInfo struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema *JSONSchema `json:"inputSchema,omitempty"`
}

// JSONSchema represents a JSON Schema object.
type JSONSchema struct {
	Type        string                 `json:"type,omitempty"`
	Description string                 `json:"description,omitempty"`
	Properties  map[string]*JSONSchema `json:"properties,omitempty"`
	Required    []string               `json:"required,omitempty"`
	Items       *JSONSchema            `json:"items,omitempty"`
	Enum        []string               `json:"enum,omitempty"`
	Default     any                    `json:"default,omitempty"`
	Minimum     *float64               `json:"minimum,omitempty"`
	Maximum     *float64               `json:"maximum,omitempty"`
	MinLength   *int                   `json:"minLength,omitempty"`
	MaxLength   *int                   `json:"maxLength,omitempty"`
	Pattern     string                 `json:"pattern,omitempty"`
}

// ListToolsResult is the result of the tools/list request.
type ListToolsResult struct {
	Tools []*ToolInfo `json:"tools"`
}

// CallToolParams are the parameters for the tools/call request.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// CallToolResult is the result of the tools/call request.
type CallToolResult struct {
	Content []*ContentBlock `json:"content"`
	IsError bool            `json:"isError,omitempty"`
}

// ContentBlock represents a content block in tool results.
type ContentBlock struct {
	Type     string `json:"type"`               // "text", "image", "resource"
	Text     string `json:"text,omitempty"`     // For text content
	MIMEType string `json:"mimeType,omitempty"` // For image/resource content
	Data     string `json:"data,omitempty"`     // Base64 encoded data for images
	URI      string `json:"uri,omitempty"`      // For resource references
}

// Resource represents an MCP resource.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// ListResourcesResult is the result of the resources/list request.
type ListResourcesResult struct {
	Resources []*Resource `json:"resources"`
}

// ReadResourceParams are the parameters for the resources/read request.
type ReadResourceParams struct {
	URI string `json:"uri"`
}

// ReadResourceResult is the result of the resources/read request.
type ReadResourceResult struct {
	Contents []*ResourceContent `json:"contents"`
}

// ResourceContent represents the content of a resource.
type ResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"` // Base64 encoded binary data
}

// Prompt represents an MCP prompt template.
type Prompt struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Arguments   []*PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument describes an argument for a prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ListPromptsResult is the result of the prompts/list request.
type ListPromptsResult struct {
	Prompts []*Prompt `json:"prompts"`
}

// GetPromptParams are the parameters for the prompts/get request.
type GetPromptParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// GetPromptResult is the result of the prompts/get request.
type GetPromptResult struct {
	Description string           `json:"description,omitempty"`
	Messages    []*PromptMessage `json:"messages"`
}

// PromptMessage represents a message in a prompt result.
type PromptMessage struct {
	Role    string          `json:"role"` // "user" or "assistant"
	Content *ContentBlock   `json:"content"`
}

// ServerConfig holds configuration for an MCP server connection.
type ServerConfig struct {
	Name        string            `yaml:"name" json:"name"`                   // Unique identifier
	Transport   string            `yaml:"transport" json:"transport"`         // "stdio" or "http"

	// STDIO transport
	Command     string            `yaml:"command,omitempty" json:"command,omitempty"`       // e.g., "npx"
	Args        []string          `yaml:"args,omitempty" json:"args,omitempty"`             // e.g., ["-y", "@mcp/server-github"]
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`               // Additional env vars

	// HTTP transport
	URL         string            `yaml:"url,omitempty" json:"url,omitempty"`               // Server URL
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`       // Custom headers

	// Connection settings
	AutoConnect bool              `yaml:"auto_connect" json:"autoConnect"`                   // Connect on startup
	Timeout     time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`       // Request timeout
	MaxRetries  int               `yaml:"max_retries,omitempty" json:"maxRetries,omitempty"` // Retry count
	RetryDelay  time.Duration     `yaml:"retry_delay,omitempty" json:"retryDelay,omitempty"` // Between retries

	// Tool settings
	ToolPrefix  string            `yaml:"tool_prefix,omitempty" json:"toolPrefix,omitempty"` // Prefix for tool names
}

// MCP protocol version
const ProtocolVersion = "2024-11-05"

// MCP method names
const (
	MethodInitialize     = "initialize"
	MethodInitialized    = "notifications/initialized"
	MethodToolsList      = "tools/list"
	MethodToolsCall      = "tools/call"
	MethodResourcesList  = "resources/list"
	MethodResourcesRead  = "resources/read"
	MethodPromptsList    = "prompts/list"
	MethodPromptsGet     = "prompts/get"
	MethodPing           = "ping"
)
