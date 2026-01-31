package agent

import (
	"encoding/json"
	"time"

	"google.golang.org/genai"
)

// AgentState represents the serializable state of an agent.
type AgentState struct {
	ID         string              `json:"id"`
	Type       AgentType           `json:"type"`
	Model      string              `json:"model,omitempty"`
	Status     AgentStatus         `json:"status"`
	History    []SerializedContent `json:"history"`
	StartTime  time.Time           `json:"start_time"`
	EndTime    time.Time           `json:"end_time,omitempty"`
	MaxTurns   int                 `json:"max_turns"`
	TurnCount  int                 `json:"turn_count"`
	LastPrompt string              `json:"last_prompt,omitempty"`
}

// SerializedContent represents a serializable conversation content.
type SerializedContent struct {
	Role  string           `json:"role"`
	Parts []SerializedPart `json:"parts"`
}

// SerializedPart represents a serializable content part.
type SerializedPart struct {
	Type         string          `json:"type"` // "text", "function_call", "function_response"
	Text         string          `json:"text,omitempty"`
	FunctionCall *SerializedFunc `json:"function_call,omitempty"`
	FunctionResp *SerializedFunc `json:"function_response,omitempty"`
}

// SerializedFunc represents a serializable function call or response.
type SerializedFunc struct {
	Name     string         `json:"name"`
	Args     map[string]any `json:"args,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

// GetState returns the current state of the agent for serialization.
func (a *Agent) GetState() *AgentState {
	history := make([]SerializedContent, len(a.history))
	for i, content := range a.history {
		history[i] = serializeContent(content)
	}

	return &AgentState{
		ID:        a.ID,
		Type:      a.Type,
		Model:     a.Model,
		Status:    a.status,
		History:   history,
		StartTime: a.startTime,
		EndTime:   a.endTime,
		MaxTurns:  a.maxTurns,
		TurnCount: len(a.history) / 2, // Approximate turn count
	}
}

// RestoreHistory restores the conversation history from a saved state.
func (a *Agent) RestoreHistory(state *AgentState) error {
	history := make([]*genai.Content, len(state.History))
	for i, sc := range state.History {
		content, err := deserializeContent(sc)
		if err != nil {
			return err
		}
		history[i] = content
	}

	a.history = history
	a.status = state.Status
	a.startTime = state.StartTime
	a.endTime = state.EndTime
	return nil
}

// GetTurnCount returns the current turn count.
func (a *Agent) GetTurnCount() int {
	return len(a.history) / 2
}

// serializeContent converts a genai.Content to SerializedContent.
func serializeContent(content *genai.Content) SerializedContent {
	parts := make([]SerializedPart, len(content.Parts))
	for i, part := range content.Parts {
		parts[i] = serializePart(part)
	}

	return SerializedContent{
		Role:  string(content.Role),
		Parts: parts,
	}
}

// serializePart converts a genai.Part to SerializedPart.
func serializePart(part *genai.Part) SerializedPart {
	sp := SerializedPart{}

	if part.FunctionCall != nil {
		sp.Type = "function_call"
		sp.FunctionCall = &SerializedFunc{
			Name: part.FunctionCall.Name,
			Args: part.FunctionCall.Args,
		}
		return sp
	}

	if part.FunctionResponse != nil {
		sp.Type = "function_response"
		sp.FunctionResp = &SerializedFunc{
			Name:     part.FunctionResponse.Name,
			Response: part.FunctionResponse.Response,
		}
		return sp
	}

	// Default to text (even if empty, use space to avoid API errors)
	sp.Type = "text"
	if part.Text != "" {
		sp.Text = part.Text
	} else {
		sp.Text = " "
	}
	return sp
}

// deserializeContent converts a SerializedContent back to genai.Content.
func deserializeContent(sc SerializedContent) (*genai.Content, error) {
	parts := make([]*genai.Part, len(sc.Parts))
	for i, sp := range sc.Parts {
		part, err := deserializePart(sp)
		if err != nil {
			return nil, err
		}
		parts[i] = part
	}

	return &genai.Content{
		Role:  sc.Role,
		Parts: parts,
	}, nil
}

// deserializePart converts a SerializedPart back to genai.Part.
func deserializePart(sp SerializedPart) (*genai.Part, error) {
	switch sp.Type {
	case "text":
		text := sp.Text
		if text == "" {
			text = " " // Avoid empty text parts
		}
		return genai.NewPartFromText(text), nil
	case "function_call":
		if sp.FunctionCall == nil {
			return genai.NewPartFromText(" "), nil
		}
		return &genai.Part{
			FunctionCall: &genai.FunctionCall{
				Name: sp.FunctionCall.Name,
				Args: sp.FunctionCall.Args,
			},
		}, nil
	case "function_response":
		if sp.FunctionResp == nil {
			return genai.NewPartFromText(" "), nil
		}
		return genai.NewPartFromFunctionResponse(sp.FunctionResp.Name, sp.FunctionResp.Response), nil
	default:
		text := sp.Text
		if text == "" {
			text = " " // Avoid empty text parts
		}
		return genai.NewPartFromText(text), nil
	}
}

// MarshalJSON implements json.Marshaler for AgentState.
func (s *AgentState) MarshalJSON() ([]byte, error) {
	type Alias AgentState
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(s),
	})
}

// UnmarshalJSON implements json.Unmarshaler for AgentState.
func (s *AgentState) UnmarshalJSON(data []byte) error {
	type Alias AgentState
	aux := &struct {
		*Alias
	}{
		Alias: (*Alias)(s),
	}
	return json.Unmarshal(data, aux)
}
