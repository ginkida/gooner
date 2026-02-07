package client

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gokin/internal/logging"

	"google.golang.org/genai"
)

// ToolCallFromText represents a tool call parsed from text output.
type ToolCallFromText struct {
	Tool string         `json:"tool"`
	Name string         `json:"name"` // alias for "tool"
	Args map[string]any `json:"args"`
}

// ParseToolCallsFromText attempts to extract tool calls from model text output.
// This is used as a fallback when models don't support native function calling.
// Supports multiple formats:
//   - {"tool": "name", "args": {...}}
//   - {"name": "tool_name", "args": {...}}
//   - ```json\n{"tool": "name", "args": {...}}\n```
//   - Multiple tool calls in sequence
func ParseToolCallsFromText(text string) []*genai.FunctionCall {
	if text == "" {
		return nil
	}

	var calls []*genai.FunctionCall

	// Try extracting from JSON code blocks first
	calls = extractFromCodeBlocks(text)
	if len(calls) > 0 {
		return calls
	}

	// Try extracting bare JSON objects
	calls = extractFromBareJSON(text)
	if len(calls) > 0 {
		return calls
	}

	return nil
}

// extractFromCodeBlocks extracts tool calls from ```json ... ``` blocks.
var codeBlockPattern = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(\\{.*?\\})\\s*\\n?```")

func extractFromCodeBlocks(text string) []*genai.FunctionCall {
	matches := codeBlockPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	var calls []*genai.FunctionCall
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		fc := parseToolCallJSON(match[1])
		if fc != nil {
			calls = append(calls, fc)
		}
	}
	return calls
}

// extractFromBareJSON extracts tool calls from bare JSON objects in text.
func extractFromBareJSON(text string) []*genai.FunctionCall {
	var calls []*genai.FunctionCall

	// Find all JSON-like objects in the text
	objects := findJSONObjects(text)
	for _, obj := range objects {
		fc := parseToolCallJSON(obj)
		if fc != nil {
			calls = append(calls, fc)
		}
	}
	return calls
}

// findJSONObjects extracts JSON objects from text by matching braces.
func findJSONObjects(text string) []string {
	var objects []string
	i := 0
	for i < len(text) {
		if text[i] == '{' {
			// Find matching closing brace
			depth := 0
			inString := false
			escaped := false
			j := i
			for j < len(text) {
				ch := text[j]
				if escaped {
					escaped = false
					j++
					continue
				}
				if ch == '\\' && inString {
					escaped = true
					j++
					continue
				}
				if ch == '"' {
					inString = !inString
				}
				if !inString {
					if ch == '{' {
						depth++
					} else if ch == '}' {
						depth--
						if depth == 0 {
							candidate := text[i : j+1]
							// Quick sanity check: must contain "tool" or "name" key
							if strings.Contains(candidate, `"tool"`) || strings.Contains(candidate, `"name"`) {
								objects = append(objects, candidate)
							}
							break
						}
					}
				}
				j++
			}
			if depth != 0 {
				// Unmatched brace, skip
				i++
				continue
			}
			i = j + 1
		} else {
			i++
		}
	}
	return objects
}

// parseToolCallJSON parses a single JSON object as a tool call.
func parseToolCallJSON(jsonStr string) *genai.FunctionCall {
	jsonStr = strings.TrimSpace(jsonStr)

	var tc ToolCallFromText
	if err := json.Unmarshal([]byte(jsonStr), &tc); err != nil {
		return nil
	}

	// Determine tool name from "tool" or "name" field
	toolName := tc.Tool
	if toolName == "" {
		toolName = tc.Name
	}
	if toolName == "" {
		return nil
	}

	// Ensure args is not nil
	if tc.Args == nil {
		tc.Args = make(map[string]any)
	}

	logging.Debug("parsed tool call from text", "tool", toolName, "args_count", len(tc.Args))

	return &genai.FunctionCall{
		ID:   fmt.Sprintf("text_call_%s", toolName),
		Name: toolName,
		Args: tc.Args,
	}
}

// ToolCallFallbackPrompt returns the system prompt addition that instructs models
// to output tool calls in a parseable JSON format.
func ToolCallFallbackPrompt(toolDeclarations []*genai.FunctionDeclaration) string {
	if len(toolDeclarations) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Tool Calling Instructions\n\n")
	sb.WriteString("You have access to tools. To call a tool, output a JSON object in a code block:\n\n")
	sb.WriteString("```json\n{\"tool\": \"tool_name\", \"args\": {\"param1\": \"value1\"}}\n```\n\n")
	sb.WriteString("IMPORTANT RULES:\n")
	sb.WriteString("- Output ONLY the JSON block when calling a tool, no other text before or after\n")
	sb.WriteString("- Wait for the tool result before continuing\n")
	sb.WriteString("- Use exact parameter names as defined below\n")
	sb.WriteString("- You can call only ONE tool at a time\n\n")
	sb.WriteString("Available tools:\n\n")

	for _, decl := range toolDeclarations {
		fmt.Fprintf(&sb, "### %s\n", decl.Name)
		fmt.Fprintf(&sb, "%s\n", decl.Description)

		if decl.Parameters != nil && len(decl.Parameters.Properties) > 0 {
			sb.WriteString("Parameters:\n")
			required := make(map[string]bool)
			for _, r := range decl.Parameters.Required {
				required[r] = true
			}
			for name, prop := range decl.Parameters.Properties {
				reqMark := ""
				if required[name] {
					reqMark = " (required)"
				}
				fmt.Fprintf(&sb, "- `%s`%s: %s\n", name, reqMark, prop.Description)
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
