package config

// ModelPreset defines a model preset configuration.
type ModelPreset struct {
	Provider        string
	Name            string
	Temperature     float32
	MaxOutputTokens int32
}

// ModelPresets contains predefined model configurations.
var ModelPresets = map[string]ModelPreset{
	"coding": {
		Provider:        "glm",
		Name:            "glm-4.7",
		Temperature:     0.7,
		MaxOutputTokens: 131072,
	},
	"fast": {
		Provider:        "gemini",
		Name:            "gemini-3-flash-preview",
		Temperature:     1.0,
		MaxOutputTokens: 8192,
	},
	"balanced": {
		Provider:        "gemini",
		Name:            "gemini-3-flash-preview",
		Temperature:     1.0,
		MaxOutputTokens: 8192,
	},
	"creative": {
		Provider:        "gemini",
		Name:            "gemini-3-pro-preview",
		Temperature:     1.0,
		MaxOutputTokens: 8192,
	},
	"gemini-flash": {
		Provider:        "gemini",
		Name:            "gemini-3-flash-preview",
		Temperature:     1.0,
		MaxOutputTokens: 8192,
	},
	"gemini-pro": {
		Provider:        "gemini",
		Name:            "gemini-3-pro-preview",
		Temperature:     1.0,
		MaxOutputTokens: 8192,
	},
	"glm": {
		Provider:        "glm",
		Name:            "glm-4.7",
		Temperature:     0.7,
		MaxOutputTokens: 131072,
	},
}

// ApplyPreset applies a model preset to the ModelConfig.
// Returns true if preset was applied successfully, false if preset not found.
func (m *ModelConfig) ApplyPreset(preset string) bool {
	p, ok := ModelPresets[preset]
	if !ok {
		return false
	}

	m.Provider = p.Provider
	m.Name = p.Name
	m.Temperature = p.Temperature
	m.MaxOutputTokens = p.MaxOutputTokens
	return true
}

// IsValidPreset checks if a preset name is valid.
func IsValidPreset(preset string) bool {
	_, ok := ModelPresets[preset]
	return ok
}

// ListPresets returns all available preset names.
func ListPresets() []string {
	presets := make([]string, 0, len(ModelPresets))
	for name := range ModelPresets {
		presets = append(presets, name)
	}
	return presets
}
