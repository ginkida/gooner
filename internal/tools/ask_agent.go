package tools

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// AskAgentTool allows an agent to request help from another agent type.
type AskAgentTool struct {
	messenger Messenger
}

// NewAskAgentTool creates a new ask_agent tool.
func NewAskAgentTool() *AskAgentTool {
	return &AskAgentTool{}
}

// SetMessenger sets the messenger for this tool.
func (t *AskAgentTool) SetMessenger(m Messenger) {
	t.messenger = m
}

func (t *AskAgentTool) Name() string {
	return "ask_agent"
}

func (t *AskAgentTool) Description() string {
	return "Sends a request for help to another specialized agent. Use this when you encounter an error or need expertise outside your domain (e.g., BashAgent asking ExploreAgent for context on a compilation error)."
}

func (t *AskAgentTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"target_role": {
					Type:        genai.TypeString,
					Description: "The role of the agent to ask: 'explore', 'bash', 'general', or 'plan'",
					Enum:        []string{"explore", "bash", "general", "plan"},
				},
				"query": {
					Type:        genai.TypeString,
					Description: "The specific question or task for the other agent",
				},
				"request_type": {
					Type:        genai.TypeString,
					Description: "Type of request: 'help' (ask for assistance), 'execute' (delegate a task), 'data' (get specific data)",
					Enum:        []string{"help", "execute", "data"},
				},
				"response_format": {
					Type:        genai.TypeString,
					Description: "Expected response format: 'text' (free-form), 'json' (structured), 'list' (bullet points)",
					Enum:        []string{"text", "json", "list"},
				},
				"context_data": {
					Type:        genai.TypeObject,
					Description: "Additional context data to pass to the target agent",
				},
			},
			Required: []string{"target_role", "query"},
		},
	}
}

func (t *AskAgentTool) Validate(args map[string]any) error {
	role, ok := GetString(args, "target_role")
	if !ok || role == "" {
		return NewValidationError("target_role", "is required")
	}

	query, ok := GetString(args, "query")
	if !ok || query == "" {
		return NewValidationError("query", "is required")
	}

	return nil
}

func (t *AskAgentTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.messenger == nil {
		return NewErrorResult("messenger not initialized for this agent"), nil
	}

	role, _ := GetString(args, "target_role")
	query, _ := GetString(args, "query")
	requestType := GetStringDefault(args, "request_type", "help")
	responseFormat := GetStringDefault(args, "response_format", "text")
	contextData, _ := args["context_data"].(map[string]any)

	// Determine message type based on request type
	msgType := "help_request"
	switch requestType {
	case "execute":
		msgType = "delegate"
	case "data":
		msgType = "data_request"
	}

	// Build message data with additional context
	msgData := map[string]any{
		"request_type":    requestType,
		"response_format": responseFormat,
	}
	if contextData != nil {
		msgData["context"] = contextData
	}

	// Send request
	msgID, err := t.messenger.SendMessage(msgType, role, query, msgData)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to send message: %s", err)), nil
	}

	// Wait for response
	response, err := t.messenger.ReceiveResponse(ctx, msgID)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to receive response: %s", err)), nil
	}

	// Format response based on requested format
	var formattedResponse string
	switch responseFormat {
	case "json":
		formattedResponse = fmt.Sprintf("```json\n%s\n```", response)
	case "list":
		formattedResponse = response // Assume agent returned a list
	default:
		formattedResponse = response
	}

	return NewSuccessResultWithData(
		fmt.Sprintf("Response from %s agent (%s request):\n\n%s", role, requestType, formattedResponse),
		map[string]any{
			"target_role":     role,
			"request_type":    requestType,
			"response_format": responseFormat,
			"response":        response,
		},
	), nil
}
