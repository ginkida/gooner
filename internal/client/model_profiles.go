package client

import "strings"

// ModelProfile contains metadata about an Ollama model family.
type ModelProfile struct {
	Family        string // e.g., "llama", "qwen", "mistral", "phi", "codellama"
	ContextWindow int    // approximate context window size
	SupportsTools bool   // native tool calling support
	IsCoding      bool   // optimized for code generation
	IsSmall       bool   // under 13B parameters (needs simpler prompts)
}

// knownModelProfiles maps model name prefixes to their profiles.
var knownModelProfiles = map[string]ModelProfile{
	// Llama family
	"llama3.2":  {Family: "llama", ContextWindow: 128000, SupportsTools: true, IsSmall: true},
	"llama3.1":  {Family: "llama", ContextWindow: 128000, SupportsTools: true},
	"llama3":    {Family: "llama", ContextWindow: 8192, SupportsTools: true},
	"llama2":    {Family: "llama", ContextWindow: 4096, SupportsTools: false},

	// Qwen family
	"qwen2.5-coder": {Family: "qwen", ContextWindow: 32768, SupportsTools: true, IsCoding: true},
	"qwen2.5":       {Family: "qwen", ContextWindow: 32768, SupportsTools: true},
	"qwen2":         {Family: "qwen", ContextWindow: 32768, SupportsTools: true},
	"qwen":          {Family: "qwen", ContextWindow: 8192, SupportsTools: false},

	// Mistral family
	"mistral-nemo":  {Family: "mistral", ContextWindow: 128000, SupportsTools: true},
	"mistral":       {Family: "mistral", ContextWindow: 32768, SupportsTools: true},
	"mixtral":       {Family: "mistral", ContextWindow: 32768, SupportsTools: true},

	// Phi family (Microsoft)
	"phi4":  {Family: "phi", ContextWindow: 16384, SupportsTools: true, IsSmall: true},
	"phi3":  {Family: "phi", ContextWindow: 4096, SupportsTools: false, IsSmall: true},

	// Code-specialized
	"codellama":      {Family: "codellama", ContextWindow: 16384, SupportsTools: false, IsCoding: true},
	"starcoder2":     {Family: "starcoder", ContextWindow: 16384, SupportsTools: false, IsCoding: true},
	"deepseek-coder": {Family: "deepseek", ContextWindow: 16384, SupportsTools: false, IsCoding: true},
	"codegemma":      {Family: "gemma", ContextWindow: 8192, SupportsTools: false, IsCoding: true},

	// Gemma family
	"gemma2": {Family: "gemma", ContextWindow: 8192, SupportsTools: false, IsSmall: true},
	"gemma":  {Family: "gemma", ContextWindow: 8192, SupportsTools: false, IsSmall: true},

	// Command R family
	"command-r-plus": {Family: "command-r", ContextWindow: 128000, SupportsTools: true},
	"command-r":      {Family: "command-r", ContextWindow: 128000, SupportsTools: true},
}

// GetModelProfile returns the profile for a given model name.
// Uses longest-prefix matching to find the best profile.
func GetModelProfile(modelName string) ModelProfile {
	modelName = strings.ToLower(modelName)

	// Strip tag like ":latest", ":7b", ":70b-instruct"
	baseName := modelName
	if idx := strings.Index(modelName, ":"); idx > 0 {
		baseName = modelName[:idx]
	}

	// Try exact match first
	if profile, ok := knownModelProfiles[baseName]; ok {
		// Check size tag for IsSmall
		profile.IsSmall = profile.IsSmall || isSmallByTag(modelName)
		return profile
	}

	// Try longest prefix match
	bestMatch := ""
	for prefix := range knownModelProfiles {
		if strings.HasPrefix(baseName, prefix) && len(prefix) > len(bestMatch) {
			bestMatch = prefix
		}
	}
	if bestMatch != "" {
		profile := knownModelProfiles[bestMatch]
		profile.IsSmall = profile.IsSmall || isSmallByTag(modelName)
		return profile
	}

	// Unknown model — conservative defaults
	return ModelProfile{
		Family:        "unknown",
		ContextWindow: 4096,
		SupportsTools: false,
		IsSmall:       true,
	}
}

// isSmallByTag checks if the model tag indicates a small model (<13B).
func isSmallByTag(modelName string) bool {
	lower := strings.ToLower(modelName)
	smallTags := []string{":1b", ":3b", ":7b", ":8b", ":9b", ":11b", ":12b",
		"-1b", "-3b", "-7b", "-8b", "-9b", "-11b", "-12b"}
	for _, tag := range smallTags {
		if strings.Contains(lower, tag) {
			return true
		}
	}
	return false
}

// ModelPromptEnhancement returns a model-specific system prompt enhancement for Ollama models.
func ModelPromptEnhancement(modelName string) string {
	profile := GetModelProfile(modelName)

	var sb strings.Builder

	// Small models: simplified instructions
	if profile.IsSmall {
		sb.WriteString("\n\n**Important:** Keep responses concise and focused. ")
		sb.WriteString("Use tools when needed. Prefer short, precise answers over verbose explanations.")
	}

	// Coding models: code-focused instructions
	if profile.IsCoding {
		sb.WriteString("\n\n**Coding focus:** Prioritize code output over prose. ")
		sb.WriteString("Show code changes directly. Use read/edit tools for file modifications.")
	}

	// Family-specific tweaks
	switch profile.Family {
	case "llama":
		if profile.IsSmall {
			sb.WriteString("\nWhen using tools, always explain what you're doing briefly before and after.")
		}
	case "phi":
		sb.WriteString("\nStructure responses clearly with steps when performing multi-step tasks.")
	case "gemma":
		sb.WriteString("\nAlways provide a brief summary after completing tool operations.")
	}

	return sb.String()
}
