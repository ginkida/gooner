package agent

import (
	"fmt"
	"sync"
)

// DynamicAgentType represents a user-defined agent type.
type DynamicAgentType struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	AllowedTools []string `json:"allowed_tools"`
	SystemPrompt string   `json:"system_prompt"`
	Priority     int      `json:"priority"` // Higher = evaluated first
}

// AgentTypeRegistry manages both built-in and dynamic agent types.
type AgentTypeRegistry struct {
	builtin map[AgentType]bool
	dynamic map[string]*DynamicAgentType
	mu      sync.RWMutex
}

// NewAgentTypeRegistry creates a new registry with built-in types.
func NewAgentTypeRegistry() *AgentTypeRegistry {
	return &AgentTypeRegistry{
		builtin: map[AgentType]bool{
			AgentTypeExplore: true,
			AgentTypeBash:    true,
			AgentTypeGeneral: true,
			AgentTypePlan:    true,
			AgentTypeGuide:   true,
		},
		dynamic: make(map[string]*DynamicAgentType),
	}
}

// RegisterDynamic registers a new dynamic agent type.
func (r *AgentTypeRegistry) RegisterDynamic(name, description string, tools []string, prompt string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check for conflict with built-in types
	if r.builtin[AgentType(name)] {
		return fmt.Errorf("cannot override built-in agent type: %s", name)
	}

	r.dynamic[name] = &DynamicAgentType{
		Name:         name,
		Description:  description,
		AllowedTools: tools,
		SystemPrompt: prompt,
	}

	return nil
}

// UnregisterDynamic removes a dynamic agent type.
func (r *AgentTypeRegistry) UnregisterDynamic(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.dynamic[name]; !ok {
		return fmt.Errorf("dynamic type not found: %s", name)
	}

	delete(r.dynamic, name)
	return nil
}

// GetDynamic returns a dynamic agent type by name.
func (r *AgentTypeRegistry) GetDynamic(name string) (*DynamicAgentType, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	dt, ok := r.dynamic[name]
	return dt, ok
}

// IsBuiltin checks if a type is a built-in type.
func (r *AgentTypeRegistry) IsBuiltin(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.builtin[AgentType(name)]
}

// IsDynamic checks if a type is a dynamic type.
func (r *AgentTypeRegistry) IsDynamic(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.dynamic[name]
	return ok
}

// Exists checks if a type (built-in or dynamic) exists.
func (r *AgentTypeRegistry) Exists(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.builtin[AgentType(name)] || r.dynamic[name] != nil
}

// ListDynamic returns all dynamic agent types.
func (r *AgentTypeRegistry) ListDynamic() []*DynamicAgentType {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]*DynamicAgentType, 0, len(r.dynamic))
	for _, dt := range r.dynamic {
		types = append(types, dt)
	}
	return types
}

// ListAll returns all agent type names (both built-in and dynamic).
func (r *AgentTypeRegistry) ListAll() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.builtin)+len(r.dynamic))

	for t := range r.builtin {
		names = append(names, string(t))
	}
	for name := range r.dynamic {
		names = append(names, name)
	}

	return names
}

// GetToolsForType returns the allowed tools for a type.
func (r *AgentTypeRegistry) GetToolsForType(name string) []string {
	// Check dynamic first
	if dt, ok := r.GetDynamic(name); ok {
		return dt.AllowedTools
	}

	// Fall back to built-in
	return AgentType(name).AllowedTools()
}

// GetPromptForType returns the system prompt for a type.
func (r *AgentTypeRegistry) GetPromptForType(name string) string {
	if dt, ok := r.GetDynamic(name); ok {
		return dt.SystemPrompt
	}
	return "" // Built-in types use their own prompt builders
}

// GetDescriptionForType returns the description for a type.
func (r *AgentTypeRegistry) GetDescriptionForType(name string) string {
	if dt, ok := r.GetDynamic(name); ok {
		return dt.Description
	}

	// Built-in descriptions
	switch AgentType(name) {
	case AgentTypeExplore:
		return "Explore and analyze codebases"
	case AgentTypeBash:
		return "Execute shell commands"
	case AgentTypeGeneral:
		return "General-purpose agent with full tool access"
	case AgentTypePlan:
		return "Design implementation strategies"
	case AgentTypeGuide:
		return "Answer questions about the CLI"
	default:
		return ""
	}
}
