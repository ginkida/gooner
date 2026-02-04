package tools

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"gokin/internal/memory"
)

// MemorizeTool allows the agent to save project-specific knowledge.
type MemorizeTool struct {
	learning *memory.ProjectLearning
}

// NewMemorizeTool creates a new MemorizeTool instance.
func NewMemorizeTool(learning *memory.ProjectLearning) *MemorizeTool {
	return &MemorizeTool{
		learning: learning,
	}
}

// SetLearning sets the learning store for the tool.
func (t *MemorizeTool) SetLearning(learning *memory.ProjectLearning) {
	t.learning = learning
}

func (t *MemorizeTool) Name() string {
	return "memorize"
}

func (t *MemorizeTool) Description() string {
	return "Saves project-specific knowledge, facts, or preferences to persistent memory. This information will be available in future sessions to help the agent work more effectively."
}

func (t *MemorizeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"type": {
					Type:        genai.TypeString,
					Description: "Type of information: 'fact', 'preference', 'convention', 'pattern'",
					Enum:        []string{"fact", "preference", "convention", "pattern"},
				},
				"key": {
					Type:        genai.TypeString,
					Description: "A short, descriptive key or name for the knowledge (e.g., 'test_command', 'logging_library')",
				},
				"content": {
					Type:        genai.TypeString,
					Description: "The actual fact or preference to remember",
				},
			},
			Required: []string{"type", "key", "content"},
		},
	}
}

func (t *MemorizeTool) Validate(args map[string]any) error {
	infoType, _ := GetString(args, "type")
	key, _ := GetString(args, "key")
	content, _ := GetString(args, "content")

	if infoType == "" {
		return NewValidationError("type", "is required")
	}
	if key == "" {
		return NewValidationError("key", "is required")
	}
	if content == "" {
		return NewValidationError("content", "is required")
	}

	return nil
}

func (t *MemorizeTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.learning == nil {
		return NewErrorResult("project learning store not initialized"), nil
	}

	infoType, _ := GetString(args, "type")
	key, _ := GetString(args, "key")
	content, _ := GetString(args, "content")

	switch infoType {
	case "preference":
		t.learning.SetPreference(key, content)
	case "fact", "convention":
		// Store as a preference for now or extend ProjectLearning
		t.learning.SetPreference(fmt.Sprintf("%s:%s", infoType, key), content)
	case "pattern":
		t.learning.LearnPattern(key, content, nil, nil)
	default:
		return NewErrorResult(fmt.Sprintf("unknown information type: %s", infoType)), nil
	}

	// Flush immediately to ensure persistence
	if err := t.learning.Flush(); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to save memory: %s", err)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Successfully memorized %s: %s", infoType, key)), nil
}
