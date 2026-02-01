package mcp

import (
	"google.golang.org/genai"
)

// ConvertMCPSchemaToGemini converts an MCP JSON Schema to a Gemini Schema.
func ConvertMCPSchemaToGemini(mcpSchema *JSONSchema) *genai.Schema {
	if mcpSchema == nil {
		return nil
	}

	schema := &genai.Schema{
		Description: mcpSchema.Description,
	}

	switch mcpSchema.Type {
	case "string":
		schema.Type = genai.TypeString
		if len(mcpSchema.Enum) > 0 {
			schema.Enum = mcpSchema.Enum
		}
	case "number":
		schema.Type = genai.TypeNumber
	case "integer":
		schema.Type = genai.TypeInteger
	case "boolean":
		schema.Type = genai.TypeBoolean
	case "array":
		schema.Type = genai.TypeArray
		if mcpSchema.Items != nil {
			schema.Items = ConvertMCPSchemaToGemini(mcpSchema.Items)
		}
	case "object":
		schema.Type = genai.TypeObject
		if len(mcpSchema.Properties) > 0 {
			schema.Properties = make(map[string]*genai.Schema)
			for name, prop := range mcpSchema.Properties {
				schema.Properties[name] = ConvertMCPSchemaToGemini(prop)
			}
		}
		schema.Required = mcpSchema.Required
	default:
		// Default to string for unknown types
		schema.Type = genai.TypeString
	}

	return schema
}

// ConvertMCPToolToDeclaration converts an MCP ToolInfo to a Gemini FunctionDeclaration.
func ConvertMCPToolToDeclaration(tool *ToolInfo, prefix string) *genai.FunctionDeclaration {
	if tool == nil {
		return nil
	}

	name := tool.Name
	if prefix != "" {
		name = prefix + "_" + tool.Name
	}

	// Sanitize name for Gemini (only alphanumeric and underscores)
	name = sanitizeFunctionName(name)

	return &genai.FunctionDeclaration{
		Name:        name,
		Description: tool.Description,
		Parameters:  ConvertMCPSchemaToGemini(tool.InputSchema),
	}
}

// sanitizeFunctionName ensures the function name is valid for Gemini.
// Gemini function names must match: [a-zA-Z_][a-zA-Z0-9_]*
func sanitizeFunctionName(name string) string {
	if name == "" {
		return "unnamed_tool"
	}

	result := make([]byte, 0, len(name))
	for i, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' {
			result = append(result, byte(c))
		} else if c >= '0' && c <= '9' {
			if i == 0 {
				// Numbers can't be first character
				result = append(result, '_')
			}
			result = append(result, byte(c))
		} else if c == '-' || c == '.' || c == ' ' {
			// Convert common separators to underscores
			result = append(result, '_')
		}
		// Skip other characters
	}

	if len(result) == 0 {
		return "unnamed_tool"
	}

	return string(result)
}

// ConvertGeminiSchemaToMCP converts a Gemini Schema to an MCP JSON Schema.
// This is useful for round-trip conversions or when we need to send schemas back to MCP servers.
func ConvertGeminiSchemaToMCP(geminiSchema *genai.Schema) *JSONSchema {
	if geminiSchema == nil {
		return nil
	}

	schema := &JSONSchema{
		Description: geminiSchema.Description,
	}

	switch geminiSchema.Type {
	case genai.TypeString:
		schema.Type = "string"
		if len(geminiSchema.Enum) > 0 {
			schema.Enum = geminiSchema.Enum
		}
	case genai.TypeNumber:
		schema.Type = "number"
	case genai.TypeInteger:
		schema.Type = "integer"
	case genai.TypeBoolean:
		schema.Type = "boolean"
	case genai.TypeArray:
		schema.Type = "array"
		if geminiSchema.Items != nil {
			schema.Items = ConvertGeminiSchemaToMCP(geminiSchema.Items)
		}
	case genai.TypeObject:
		schema.Type = "object"
		if len(geminiSchema.Properties) > 0 {
			schema.Properties = make(map[string]*JSONSchema)
			for name, prop := range geminiSchema.Properties {
				schema.Properties[name] = ConvertGeminiSchemaToMCP(prop)
			}
		}
		schema.Required = geminiSchema.Required
	default:
		schema.Type = "string"
	}

	return schema
}
