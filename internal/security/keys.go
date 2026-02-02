package security

import (
	"fmt"
	"os"
	"strings"
)

// KeySource represents where an API key was loaded from
type KeySource string

const (
	// KeySourceEnvironment indicates the key was loaded from environment variables
	KeySourceEnvironment KeySource = "environment"
	// KeySourceConfig indicates the key was loaded from config file
	KeySourceConfig KeySource = "config"
	// KeySourceKeyring indicates the key was loaded from system keyring
	KeySourceKeyring KeySource = "keyring"
	// KeySourceNotSet indicates no key was found
	KeySourceNotSet KeySource = "not_set"
)

// LoadedKey represents a loaded API key with metadata
type LoadedKey struct {
	Value  string    // The actual API key
	Source KeySource // Where the key was loaded from
}

// String returns a safe string representation (hides the key value)
func (k *LoadedKey) String() string {
	if k == nil || k.Value == "" {
		return "LoadedKey{Source: not_set, Value: \"\"}"
	}
	// Show only first 8 characters for debugging
	prefix := ""
	if len(k.Value) > 8 {
		prefix = k.Value[:8]
	} else {
		prefix = k.Value
	}
	return fmt.Sprintf("LoadedKey{Source: %s, Value: %s...}", k.Source, prefix)
}

// IsSet returns true if the key has a value
func (k *LoadedKey) IsSet() bool {
	return k != nil && k.Value != ""
}

// GetAPIKey loads an API key from multiple sources in priority order:
// 1. Environment variables (highest priority)
// 2. Config file value (fallback)
// 3. System keyring (optional, future enhancement)
//
// Priority ensures that environment variables override config files,
// allowing secure deployment without storing keys in configs.
//
// Parameters:
//   - envVarNames: List of environment variable names to check (in priority order)
//   - configValue: Fallback value from config file
//   - keyringService: Optional service name for keyring lookup (empty string = skip keyring)
//
// Returns:
//   - LoadedKey: The loaded key with source information
func GetAPIKey(envVarNames []string, configValue string, keyringService string) *LoadedKey {
	// Priority 1: Environment variables (highest priority)
	for _, envVar := range envVarNames {
		if value := os.Getenv(envVar); value != "" {
			return &LoadedKey{
				Value:  value,
				Source: KeySourceEnvironment,
			}
		}
	}

	// Priority 2: Config file (fallback)
	if configValue != "" {
		return &LoadedKey{
			Value:  configValue,
			Source: KeySourceConfig,
		}
	}

	// Priority 3: System keyring (reserved for future use)
	// Keyring support is not implemented to avoid external dependencies.
	// Use environment variables for secure key storage instead.
	_ = keyringService

	// No key found
	return &LoadedKey{
		Value:  "",
		Source: KeySourceNotSet,
	}
}

// GetGeminiKey loads the Gemini API key from environment or config
//
// Environment variables checked (in priority order):
//   - GOKIN_GEMINI_KEY (recommended, explicit)
//   - GEMINI_API_KEY (generic, for compatibility)
//   - GOOGLE_API_KEY (generic Google API key)
//
// Config fallback:
//   - api.gemini_key (new field)
//   - api.api_key (legacy field, for backward compatibility)
func GetGeminiKey(configGeminiKey, configLegacyKey string) *LoadedKey {
	envVars := []string{
		"GOKIN_GEMINI_KEY", // Preferred, explicit
		"GEMINI_API_KEY",   // Generic Gemini
		"GOOGLE_API_KEY",   // Generic Google
	}

	// Try new config field first, then legacy
	configValue := configGeminiKey
	if configValue == "" {
		configValue = configLegacyKey
	}

	return GetAPIKey(envVars, configValue, "")
}

// GetGLMKey loads the GLM (GLM-4.7) API key from environment or config
//
// Environment variables checked (in priority order):
//   - GOKIN_GLM_KEY (recommended, explicit)
//   - GLM_API_KEY (generic)
//   - ANTHROPIC_API_KEY (for Anthropic-compatible APIs)
//
// Config fallback:
//   - api.glm_key (new field)
//   - api.anthropic_api_key (legacy field, for backward compatibility)
func GetGLMKey(configGLMKey, configLegacyKey string) *LoadedKey {
	envVars := []string{
		"GOKIN_GLM_KEY",     // Preferred, explicit
		"GLM_API_KEY",       // Generic GLM
		"ANTHROPIC_API_KEY", // Anthropic-compatible
	}

	// Try new config field first, then legacy
	configValue := configGLMKey
	if configValue == "" {
		configValue = configLegacyKey
	}

	return GetAPIKey(envVars, configValue, "")
}

// GetOllamaKey loads the optional Ollama API key from environment or config.
// Note: Ollama doesn't require an API key for local servers,
// but remote Ollama servers may require authentication.
//
// Environment variables checked (in priority order):
//   - GOKIN_OLLAMA_KEY (recommended, explicit)
//   - OLLAMA_API_KEY (generic)
//
// Config fallback:
//   - api.ollama_key
func GetOllamaKey(configOllamaKey string) *LoadedKey {
	envVars := []string{
		"GOKIN_OLLAMA_KEY", // Preferred, explicit
		"OLLAMA_API_KEY",   // Generic Ollama
	}

	return GetAPIKey(envVars, configOllamaKey, "")
}

// MaskKey masks an API key for safe logging/display
// Shows first 4 and last 4 characters with asterisks in between
//
// Example: "sk-1234567890abcdef" -> "sk-1****cdef"
func MaskKey(key string) string {
	if key == "" {
		return "(not set)"
	}

	if len(key) <= 8 {
		// Key too short, just show asterisks
		return strings.Repeat("*", len(key))
	}

	// Show first 4 and last 4 characters
	prefix := key[:4]
	suffix := key[len(key)-4:]
	middle := strings.Repeat("*", len(key)-8)

	return prefix + middle + suffix
}

// ValidateKeyFormat performs basic validation on API key format
// This is a sanity check, not comprehensive validation
//
// Returns an error if the key format is obviously invalid
func ValidateKeyFormat(key string) error {
	if key == "" {
		return fmt.Errorf("API key cannot be empty")
	}

	// Check minimum length (most API keys are at least 20 characters)
	if len(key) < 10 {
		return fmt.Errorf("API key too short (expected at least 10 characters, got %d)", len(key))
	}

	// Check for common placeholder values
	lowerKey := strings.ToLower(key)
	placeholderValues := []string{
		"your-api-key",
		"your_api_key",
		"api_key",
		"sk-xxxx",
		"<insert-key>",
	}

	for _, placeholder := range placeholderValues {
		if strings.Contains(lowerKey, placeholder) {
			return fmt.Errorf("API key appears to be a placeholder: %s", placeholder)
		}
	}

	return nil
}
