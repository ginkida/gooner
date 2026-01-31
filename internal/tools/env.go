package tools

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"google.golang.org/genai"
)

// EnvTool provides access to environment variables.
type EnvTool struct{}

// NewEnvTool creates a new EnvTool instance.
func NewEnvTool() *EnvTool {
	return &EnvTool{}
}

func (t *EnvTool) Name() string {
	return "env"
}

func (t *EnvTool) Description() string {
	return "Gets environment variables. Can retrieve a specific variable or list all."
}

func (t *EnvTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"name": {
					Type:        genai.TypeString,
					Description: "Name of the environment variable to retrieve. If empty, lists all variables.",
				},
				"prefix": {
					Type:        genai.TypeString,
					Description: "Filter variables by prefix (e.g., 'PATH', 'GO')",
				},
				"mask_secrets": {
					Type:        genai.TypeBoolean,
					Description: "Mask values of variables that might contain secrets (default: true)",
				},
			},
		},
	}
}

func (t *EnvTool) Validate(args map[string]any) error {
	return nil
}

func (t *EnvTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	name := GetStringDefault(args, "name", "")
	prefix := GetStringDefault(args, "prefix", "")
	maskSecrets := GetBoolDefault(args, "mask_secrets", true)

	// Get specific variable
	if name != "" {
		value := os.Getenv(name)
		if value == "" {
			// Check if it exists but is empty vs doesn't exist
			_, exists := os.LookupEnv(name)
			if !exists {
				return NewErrorResult(fmt.Sprintf("environment variable not found: %s", name)), nil
			}
			return NewSuccessResult(fmt.Sprintf("%s=(empty)", name)), nil
		}

		if maskSecrets && isSensitiveVar(name) {
			value = maskValue(value)
		}

		return NewSuccessResult(fmt.Sprintf("%s=%s", name, value)), nil
	}

	// List all variables
	envVars := os.Environ()
	sort.Strings(envVars)

	var builder strings.Builder
	count := 0

	for _, env := range envVars {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}

		varName := parts[0]
		varValue := parts[1]

		// Apply prefix filter
		if prefix != "" && !strings.HasPrefix(strings.ToUpper(varName), strings.ToUpper(prefix)) {
			continue
		}

		// Mask sensitive values
		if maskSecrets && isSensitiveVar(varName) {
			varValue = maskValue(varValue)
		}

		// Truncate very long values
		if len(varValue) > 100 {
			varValue = varValue[:100] + "..."
		}

		builder.WriteString(fmt.Sprintf("%s=%s\n", varName, varValue))
		count++
	}

	if count == 0 {
		if prefix != "" {
			return NewSuccessResult(fmt.Sprintf("No environment variables found with prefix '%s'", prefix)), nil
		}
		return NewSuccessResult("No environment variables found"), nil
	}

	result := builder.String()
	if prefix != "" {
		result = fmt.Sprintf("Environment variables with prefix '%s':\n\n%s", prefix, result)
	} else {
		result = fmt.Sprintf("Environment variables (%d total):\n\n%s", count, result)
	}

	return NewSuccessResult(result), nil
}

// isSensitiveVar checks if an environment variable might contain sensitive data.
func isSensitiveVar(name string) bool {
	sensitivePatterns := []string{
		"PASSWORD",
		"SECRET",
		"TOKEN",
		"API_KEY",
		"APIKEY",
		"API-KEY",
		"PRIVATE",
		"CREDENTIAL",
		"AUTH",
		"KEY",
		"CERT",
		"SSH",
		"ACCESS",
	}

	upperName := strings.ToUpper(name)
	for _, pattern := range sensitivePatterns {
		if strings.Contains(upperName, pattern) {
			return true
		}
	}

	return false
}

// maskValue masks a sensitive value, showing only first and last characters.
func maskValue(value string) string {
	if len(value) <= 4 {
		return "****"
	}
	return value[:2] + "****" + value[len(value)-2:]
}
