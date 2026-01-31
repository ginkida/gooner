package permission

// Rules holds the permission rules for tools.
type Rules struct {
	DefaultPolicy Level            // Default policy for unknown tools
	ToolPolicies  map[string]Level // Per-tool policies
}

// DefaultRules returns the default permission rules.
// Read-only tools are allowed, file modifications and bash require asking.
func DefaultRules() *Rules {
	return &Rules{
		DefaultPolicy: LevelAsk,
		ToolPolicies: map[string]Level{
			// Read-only tools - allow by default
			"read":     LevelAllow,
			"glob":     LevelAllow,
			"grep":     LevelAllow,
			"tree":     LevelAllow,
			"diff":     LevelAllow,
			"env":      LevelAllow,
			"list_dir": LevelAllow,
			"todo":     LevelAllow,

			// File modification tools - ask before executing
			"write": LevelAsk,
			"edit":  LevelAsk,

			// System tools - ask before executing
			"bash": LevelAsk,
		},
	}
}

// GetPolicy returns the permission level for a tool.
func (r *Rules) GetPolicy(toolName string) Level {
	if policy, ok := r.ToolPolicies[toolName]; ok {
		return policy
	}
	return r.DefaultPolicy
}

// SetPolicy sets the permission level for a tool.
func (r *Rules) SetPolicy(toolName string, level Level) {
	r.ToolPolicies[toolName] = level
}

// NewRulesFromConfig creates rules from a config map.
func NewRulesFromConfig(defaultPolicy string, toolPolicies map[string]string) *Rules {
	rules := &Rules{
		DefaultPolicy: parseLevel(defaultPolicy),
		ToolPolicies:  make(map[string]Level),
	}

	for tool, policy := range toolPolicies {
		rules.ToolPolicies[tool] = parseLevel(policy)
	}

	return rules
}

// parseLevel converts a string to a Level.
func parseLevel(s string) Level {
	switch s {
	case "allow":
		return LevelAllow
	case "deny":
		return LevelDeny
	default:
		return LevelAsk
	}
}
