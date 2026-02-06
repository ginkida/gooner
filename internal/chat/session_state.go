package chat

import (
	"encoding/json"
	"time"

	"google.golang.org/genai"
)

// SessionState represents the serializable state of a session.
type SessionState struct {
	ID                string              `json:"id"`
	StartTime         time.Time           `json:"start_time"`
	LastActive        time.Time           `json:"last_active"`
	WorkDir           string              `json:"work_dir,omitempty"`
	History           []SerializedContent `json:"history"`
	TokenCounts       []int               `json:"token_counts,omitempty"`
	TotalTokens       int                 `json:"total_tokens"`
	Version           int64               `json:"version"`
	Summary           string              `json:"summary,omitempty"`
	Scratchpad        string              `json:"scratchpad,omitempty"`
	SystemInstruction string              `json:"system_instruction,omitempty"`
}

// SerializedContent represents a serializable conversation content.
type SerializedContent struct {
	Role  string           `json:"role"`
	Parts []SerializedPart `json:"parts"`
}

// SerializedPart represents a serializable content part.
type SerializedPart struct {
	Type             string          `json:"type"` // "text", "function_call", "function_response"
	Text             string          `json:"text,omitempty"`
	FunctionCall     *SerializedFunc `json:"function_call,omitempty"`
	FunctionResp     *SerializedFunc `json:"function_response,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature []byte          `json:"thought_signature,omitempty"`
}

// SerializedFunc represents a serializable function call or response.
type SerializedFunc struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Args     map[string]any `json:"args,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

// SessionInfo provides summary info about a saved session.
type SessionInfo struct {
	ID           string    `json:"id"`
	StartTime    time.Time `json:"start_time"`
	LastActive   time.Time `json:"last_active"`
	Summary      string    `json:"summary"`
	MessageCount int       `json:"message_count"`
	WorkDir      string    `json:"work_dir,omitempty"`
}

// SerializeContent converts a genai.Content to SerializedContent.
func SerializeContent(content *genai.Content) SerializedContent {
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
	sp := SerializedPart{
		Thought:          part.Thought,
		ThoughtSignature: part.ThoughtSignature,
	}

	if part.FunctionCall != nil {
		sp.Type = "function_call"
		sp.FunctionCall = &SerializedFunc{
			ID:   part.FunctionCall.ID,
			Name: part.FunctionCall.Name,
			Args: part.FunctionCall.Args,
		}
		return sp
	}

	if part.FunctionResponse != nil {
		sp.Type = "function_response"
		sp.FunctionResp = &SerializedFunc{
			ID:       part.FunctionResponse.ID,
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

// DeserializeContent converts a SerializedContent back to genai.Content.
func DeserializeContent(sc SerializedContent) (*genai.Content, error) {
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
	var part *genai.Part

	switch sp.Type {
	case "text":
		text := sp.Text
		if text == "" {
			text = " " // Avoid empty text parts
		}
		part = genai.NewPartFromText(text)
	case "function_call":
		if sp.FunctionCall == nil {
			part = genai.NewPartFromText(" ")
		} else {
			part = &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   sp.FunctionCall.ID,
					Name: sp.FunctionCall.Name,
					Args: sp.FunctionCall.Args,
				},
			}
		}
	case "function_response":
		if sp.FunctionResp == nil {
			part = genai.NewPartFromText(" ")
		} else {
			part = genai.NewPartFromFunctionResponse(sp.FunctionResp.Name, sp.FunctionResp.Response)
			part.FunctionResponse.ID = sp.FunctionResp.ID
		}
	default:
		text := sp.Text
		if text == "" {
			text = " " // Avoid empty text parts
		}
		part = genai.NewPartFromText(text)
	}

	// Restore Thought and ThoughtSignature for Gemini 3 compatibility
	part.Thought = sp.Thought
	part.ThoughtSignature = sp.ThoughtSignature

	return part, nil
}

// MarshalJSON implements json.Marshaler for SessionState.
func (s *SessionState) MarshalJSON() ([]byte, error) {
	type Alias SessionState
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(s),
	})
}

// UnmarshalJSON implements json.Unmarshaler for SessionState.
func (s *SessionState) UnmarshalJSON(data []byte) error {
	type Alias SessionState
	aux := &struct {
		*Alias
	}{
		Alias: (*Alias)(s),
	}
	return json.Unmarshal(data, aux)
}

// GenerateSummary creates a brief summary of the session based on messages.
func (s *SessionState) GenerateSummary() string {
	if len(s.History) == 0 {
		return ""
	}

	// Find first user message after system prompt
	for i, content := range s.History {
		if i < 2 { // Skip system prompt and initial response
			continue
		}
		if content.Role == "user" && len(content.Parts) > 0 {
			text := ""
			for _, part := range content.Parts {
				if part.Type == "text" && part.Text != "" {
					text = part.Text
					break
				}
			}
			if len(text) > 100 {
				return text[:97] + "..."
			}
			return text
		}
	}

	return ""
}
