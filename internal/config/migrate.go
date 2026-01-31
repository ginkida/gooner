package config

import (
	"fmt"
	"strings"
)

// MigrateConfig migrates old configuration format to new format.
func MigrateConfig(cfg *Config) {
	migrated := false

	// Auto-detect provider if not set
	if cfg.Model.Provider == "" || cfg.Model.Provider == "auto" {
		if cfg.Model.Preset != "" {
			cfg.Model.ApplyPreset(cfg.Model.Preset)
		} else {
			cfg.Model.Provider = DetectProvider(cfg.Model.Name)
		}
		migrated = true
	}

	// Apply preset if specified
	if cfg.Model.Preset != "" {
		if cfg.Model.ApplyPreset(cfg.Model.Preset) {
			migrated = true
		}
	}

	// Normalize backend to match provider
	if cfg.API.Backend == "auto" && cfg.Model.Provider != "" {
		cfg.API.Backend = cfg.Model.Provider
	}

	if migrated {
		fmt.Println("✓ Configuration migrated to new format")
	}
}

// DetectProvider determines the provider from model name.
func DetectProvider(modelName string) string {
	if modelName == "" {
		return "gemini" // default
	}

	lower := strings.ToLower(modelName)

	switch {
	case strings.HasPrefix(lower, "gemini") || strings.HasPrefix(lower, "models/"):
		return "gemini"
	case strings.HasPrefix(lower, "glm"):
		return "glm"
	case strings.HasPrefix(lower, "claude"):
		return "anthropic"
	default:
		return "gemini" // default
	}
}

// NormalizeConfig ensures configuration is consistent and valid.
func NormalizeConfig(cfg *Config) error {
	// Ensure provider is set
	if cfg.Model.Provider == "" {
		cfg.Model.Provider = DetectProvider(cfg.Model.Name)
	}

	// Ensure backend is set (legacy field, for compatibility)
	if cfg.API.Backend == "" || cfg.API.Backend == "auto" {
		cfg.API.Backend = cfg.Model.Provider
	}

	// Sync ActiveProvider with Backend if not set
	if cfg.API.ActiveProvider == "" {
		cfg.API.ActiveProvider = cfg.API.Backend
	}

	// Validate that we have an API key for the active provider
	// Keys are loaded from environment or config when client is created
	// So we don't validate here - let the client creation handle missing keys

	// Apply preset if specified
	if cfg.Model.Preset != "" {
		if !cfg.Model.ApplyPreset(cfg.Model.Preset) {
			return fmt.Errorf("unknown preset: %s (available: %s)",
				cfg.Model.Preset,
				strings.Join(ListPresets(), ", "))
		}
	}

	return nil
}

// GetEffectiveAPIKey returns the effective API key for the active provider.
func (c *Config) GetEffectiveAPIKey() (string, error) {
	key := c.API.GetActiveKey()
	if key != "" {
		return key, nil
	}
	return "", fmt.Errorf("API key not configured for provider: %s", c.API.GetActiveProvider())
}
