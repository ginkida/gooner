package tools

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// QuestionHandler is called to ask the user a question.
// It returns the user's answer (selected option or custom text).
type QuestionHandler func(ctx context.Context, question string, options []string, defaultOpt string) (string, error)

// AskUserTool allows the AI to ask the user questions.
type AskUserTool struct {
	handler QuestionHandler
}

// NewAskUserTool creates a new ask user tool.
func NewAskUserTool() *AskUserTool {
	return &AskUserTool{}
}

// SetHandler sets the question handler.
func (t *AskUserTool) SetHandler(handler QuestionHandler) {
	t.handler = handler
}

func (t *AskUserTool) Name() string {
	return "ask_user"
}

func (t *AskUserTool) Description() string {
	return "Ask the user a question to get clarification or make a decision"
}

func (t *AskUserTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"question": {
					Type:        genai.TypeString,
					Description: "The question to ask the user",
				},
				"options": {
					Type:        genai.TypeArray,
					Description: "Optional list of options for the user to choose from",
					Items: &genai.Schema{
						Type: genai.TypeString,
					},
				},
				"default": {
					Type:        genai.TypeString,
					Description: "Optional default option (must be one of the options if provided)",
				},
			},
			Required: []string{"question"},
		},
	}
}

func (t *AskUserTool) Validate(args map[string]any) error {
	question, ok := GetString(args, "question")
	if !ok || question == "" {
		return NewValidationError("question", "question is required")
	}

	return nil
}

func (t *AskUserTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	question, _ := GetString(args, "question")
	defaultOpt := GetStringDefault(args, "default", "")

	// Parse options
	var options []string
	if optionsRaw, ok := args["options"]; ok {
		if optList, ok := optionsRaw.([]any); ok {
			for _, opt := range optList {
				if optStr, ok := opt.(string); ok {
					options = append(options, optStr)
				}
			}
		}
	}

	// If no handler is set, return an error
	if t.handler == nil {
		return NewErrorResult("question handler not configured"), nil
	}

	// Ask the user
	answer, err := t.handler(ctx, question, options, defaultOpt)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to get user answer: %s", err)), nil
	}

	// Format the response
	var response string
	if len(options) > 0 {
		response = fmt.Sprintf("User selected: %s", answer)
	} else {
		response = fmt.Sprintf("User answered: %s", answer)
	}

	return NewSuccessResultWithData(response, map[string]any{
		"answer":   answer,
		"question": question,
		"options":  options,
	}), nil
}

// GetStringSlice extracts a string slice from args.
func GetStringSlice(args map[string]any, key string) ([]string, bool) {
	val, ok := args[key]
	if !ok {
		return nil, false
	}

	switch v := val.(type) {
	case []string:
		return v, true
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result, len(result) > 0
	}
	return nil, false
}

// FormatQuestion formats a question with options for display.
func FormatQuestion(question string, options []string, defaultOpt string) string {
	var builder strings.Builder
	builder.WriteString(question)

	if len(options) > 0 {
		builder.WriteString("\n\nOptions:")
		for i, opt := range options {
			prefix := fmt.Sprintf("\n  %d. ", i+1)
			if opt == defaultOpt {
				builder.WriteString(prefix + opt + " (default)")
			} else {
				builder.WriteString(prefix + opt)
			}
		}
	}

	return builder.String()
}
