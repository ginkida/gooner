package config

import "time"

// Config represents the main application configuration.
type Config struct {
	API           APIConfig           `yaml:"api"`
	Model         ModelConfig         `yaml:"model"`
	Tools         ToolsConfig         `yaml:"tools"`
	UI            UIConfig            `yaml:"ui"`
	Context       ContextConfig       `yaml:"context"`
	Permission    PermissionConfig    `yaml:"permission"`
	Plan          PlanConfig          `yaml:"plan"`
	Hooks         HooksConfig         `yaml:"hooks"`
	Web           WebConfig           `yaml:"web"`
	Session       SessionConfig       `yaml:"session"`
	Memory        MemoryConfig        `yaml:"memory"`
	Logging       LoggingConfig       `yaml:"logging"`
	Audit         AuditConfig         `yaml:"audit"`
	RateLimit     RateLimitConfig     `yaml:"rate_limit"`
	Cache   CacheConfig   `yaml:"cache"`
	Watcher WatcherConfig `yaml:"watcher"`
	DiffPreview   DiffPreviewConfig   `yaml:"diff_preview"`
	Semantic      SemanticConfig      `yaml:"semantic"`
	Contract      ContractConfig      `yaml:"contract"`
	MCP           MCPConfig           `yaml:"mcp"`
	Update        UpdateConfig        `yaml:"update"`

	// Runtime version information
	Version string `yaml:"-"`
}

// APIConfig holds API-related settings.
type APIConfig struct {
	// Legacy field - for backwards compatibility
	APIKey string `yaml:"api_key,omitempty"`

	// Separate keys for each provider
	GeminiKey string `yaml:"gemini_key,omitempty"`
	GLMKey    string `yaml:"glm_key,omitempty"`
	OllamaKey string `yaml:"ollama_key,omitempty"` // Optional, for remote Ollama servers with auth

	// Ollama server URL (default: http://localhost:11434)
	OllamaBaseURL string `yaml:"ollama_base_url,omitempty"`

	// Active provider: gemini, glm, ollama (default: gemini)
	ActiveProvider string `yaml:"active_provider"`

	// Backend: gemini, glm, ollama, auto (default: gemini) - legacy, use ActiveProvider
	Backend string `yaml:"backend,omitempty"`

	// Retry configuration for API calls
	Retry RetryConfig `yaml:"retry"`
}

// GetActiveKey returns the API key for the active provider.
func (c *APIConfig) GetActiveKey() string {
	provider := c.ActiveProvider
	if provider == "" {
		provider = c.Backend
	}
	if provider == "" {
		provider = "gemini"
	}

	switch provider {
	case "glm":
		if c.GLMKey != "" {
			return c.GLMKey
		}
	case "gemini":
		if c.GeminiKey != "" {
			return c.GeminiKey
		}
	case "ollama":
		// Ollama key is optional (local server doesn't need it)
		return c.OllamaKey
	}

	// Fallback to legacy APIKey field
	return c.APIKey
}

// GetActiveProvider returns the active provider name.
func (c *APIConfig) GetActiveProvider() string {
	if c.ActiveProvider != "" {
		return c.ActiveProvider
	}
	if c.Backend != "" {
		return c.Backend
	}
	return "gemini"
}

// HasProvider checks if a provider has an API key configured.
// Note: Ollama doesn't require an API key for local servers.
func (c *APIConfig) HasProvider(provider string) bool {
	switch provider {
	case "gemini":
		return c.GeminiKey != "" || (c.APIKey != "" && c.GetActiveProvider() == "gemini")
	case "glm":
		return c.GLMKey != "" || (c.APIKey != "" && c.GetActiveProvider() == "glm")
	case "ollama":
		// Ollama is always "available" since it doesn't require an API key
		return true
	}
	return false
}

// SetProviderKey sets the API key for a specific provider.
func (c *APIConfig) SetProviderKey(provider, key string) {
	switch provider {
	case "gemini":
		c.GeminiKey = key
	case "glm":
		c.GLMKey = key
	case "ollama":
		c.OllamaKey = key
	}
}

// RetryConfig holds retry settings for API calls.
type RetryConfig struct {
	MaxRetries  int           `yaml:"max_retries"`  // Maximum number of retry attempts (default: 3)
	RetryDelay  time.Duration `yaml:"retry_delay"`  // Initial delay between retries (default: 1s)
	HTTPTimeout time.Duration `yaml:"http_timeout"` // HTTP request timeout (default: 120s)
}

// ModelConfig holds model-related settings.
type ModelConfig struct {
	// New fields for simplified configuration
	Preset   string `yaml:"preset"`   // Model preset: coding, fast, balanced, creative
	Provider string `yaml:"provider"` // Model provider: gemini, glm, auto (default: auto)

	// Existing fields (manual configuration)
	Name            string  `yaml:"name"`
	Temperature     float32 `yaml:"temperature"`
	MaxOutputTokens int32   `yaml:"max_output_tokens"`
	// Custom API endpoint override (optional)
	// If set, overrides the default BaseURL for the model
	CustomBaseURL string `yaml:"custom_base_url"`

	// Extended Thinking (Anthropic API feature)
	EnableThinking bool  `yaml:"enable_thinking"` // Enable extended thinking mode
	ThinkingBudget int32 `yaml:"thinking_budget"` // Max tokens for thinking (0 = disabled)
}

// ToolsConfig holds tool-related settings.
type ToolsConfig struct {
	Timeout     time.Duration `yaml:"timeout"`
	Bash        BashConfig    `yaml:"bash"`
	AllowedDirs []string      `yaml:"allowed_dirs"` // Additional allowed directories (besides workDir)
}

// BashConfig holds bash tool settings.
type BashConfig struct {
	Sandbox         bool     `yaml:"sandbox"`
	BlockedCommands []string `yaml:"blocked_commands"`
}

// UIConfig holds UI-related settings.
type UIConfig struct {
	StreamOutput      bool   `yaml:"stream_output"`
	MarkdownRendering bool   `yaml:"markdown_rendering"`
	ShowToolCalls     bool   `yaml:"show_tool_calls"`
	ShowTokenUsage    bool   `yaml:"show_token_usage"`
	MouseMode         string `yaml:"mouse_mode"`    // "enabled" (default) or "disabled"
	Theme             string `yaml:"theme"`         // Theme name: dark, light, sepia, cyber, forest, ocean, monokai, dracula, high_contrast
	ShowWelcome       bool   `yaml:"show_welcome"`  // Show welcome message on first launch
	HintsEnabled      bool   `yaml:"hints_enabled"` // Show contextual hints for features
}

// ContextConfig holds context management settings.
type ContextConfig struct {
	MaxInputTokens     int     `yaml:"max_input_tokens"`      // 0 = use model default
	WarningThreshold   float64 `yaml:"warning_threshold"`     // 0.8 = warn at 80%
	SummarizationRatio float64 `yaml:"summarization_ratio"`   // 0.5 = summarize to 50%
	ToolResultMaxChars int     `yaml:"tool_result_max_chars"` // Max chars for tool results
	EnableAutoSummary  bool    `yaml:"enable_auto_summary"`   // Enable auto-summarization
}

// PermissionConfig holds permission system settings.
type PermissionConfig struct {
	Enabled       bool              `yaml:"enabled"`        // Enable/disable permission system
	DefaultPolicy string            `yaml:"default_policy"` // Default policy: "allow", "ask", "deny"
	Rules         map[string]string `yaml:"rules"`          // Per-tool rules
}

// PlanConfig holds plan mode settings.
type PlanConfig struct {
	Enabled            bool          `yaml:"enabled"`               // Enable/disable plan mode
	RequireApproval    bool          `yaml:"require_approval"`      // Require user approval before execution
	AutoDetect         bool          `yaml:"auto_detect"`           // Auto-trigger planning for complex tasks
	ClearContext       bool          `yaml:"clear_context"`         // Clear context before plan execution
	DelegateSteps      bool          `yaml:"delegate_steps"`        // Run each step in isolated sub-agent
	AbortOnStepFailure bool          `yaml:"abort_on_step_failure"` // Stop plan on step failure
	PlanningTimeout    time.Duration `yaml:"planning_timeout"`      // Timeout for LLM plan generation
	UseLLMExpansion    bool          `yaml:"use_llm_expansion"`     // Use LLM for dynamic plan expansion
}

// HooksConfig holds hooks system settings.
type HooksConfig struct {
	Enabled bool         `yaml:"enabled"` // Enable/disable hooks
	Hooks   []HookConfig `yaml:"hooks"`   // List of configured hooks
}

// HookConfig represents a single hook configuration.
type HookConfig struct {
	Name     string `yaml:"name"`      // Human-readable name
	Type     string `yaml:"type"`      // Hook type: pre_tool, post_tool, on_error, on_start, on_exit
	ToolName string `yaml:"tool_name"` // Tool to trigger on (empty = all)
	Command  string `yaml:"command"`   // Shell command to execute
	Enabled  bool   `yaml:"enabled"`   // Whether hook is active
}

// WebConfig holds web tool settings.
type WebConfig struct {
	SearchProvider string `yaml:"search_provider"` // Search provider: "serpapi", "google"
	SearchAPIKey   string `yaml:"search_api_key"`  // API key for search provider
	GoogleCX       string `yaml:"google_cx"`       // Google Custom Search Engine ID
}

// SessionConfig holds session persistence settings.
type SessionConfig struct {
	Enabled      bool          `yaml:"enabled"`       // Enable session persistence
	SaveInterval time.Duration `yaml:"save_interval"` // Auto-save interval (default: 2m)
	AutoLoad     bool          `yaml:"auto_load"`     // Auto-load last session on startup
}

// MemoryConfig holds memory system settings.
type MemoryConfig struct {
	Enabled    bool `yaml:"enabled"`     // Enable/disable memory system
	MaxEntries int  `yaml:"max_entries"` // Maximum number of memory entries
	AutoInject bool `yaml:"auto_inject"` // Auto-inject memories into system prompt
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level string `yaml:"level"` // Logging level: debug, info, warn, error
}

// AuditConfig holds audit log settings.
type AuditConfig struct {
	Enabled       bool `yaml:"enabled"`        // Enable/disable audit logging
	MaxEntries    int  `yaml:"max_entries"`    // Maximum entries per session
	MaxResultLen  int  `yaml:"max_result_len"` // Maximum result length to store
	RetentionDays int  `yaml:"retention_days"` // Days to retain audit logs
}

// RateLimitConfig holds rate limiting settings.
type RateLimitConfig struct {
	Enabled           bool  `yaml:"enabled"`             // Enable/disable rate limiting
	RequestsPerMinute int   `yaml:"requests_per_minute"` // Max requests per minute
	TokensPerMinute   int64 `yaml:"tokens_per_minute"`   // Max tokens per minute
	BurstSize         int   `yaml:"burst_size"`          // Burst size for rate limiting
}

// CacheConfig holds search cache settings.
type CacheConfig struct {
	Enabled  bool          `yaml:"enabled"`  // Enable/disable caching
	Capacity int           `yaml:"capacity"` // Maximum cache entries
	TTL      time.Duration `yaml:"ttl"`      // Time to live for cache entries
}

// WatcherConfig holds file watcher settings.
type WatcherConfig struct {
	Enabled    bool `yaml:"enabled"`     // Enable/disable file watching
	DebounceMs int  `yaml:"debounce_ms"` // Debounce time in milliseconds
	MaxWatches int  `yaml:"max_watches"` // Maximum number of watched paths
}

// DiffPreviewConfig holds diff preview settings.
type DiffPreviewConfig struct {
	Enabled bool `yaml:"enabled"` // Enable/disable diff preview for write/edit operations
}

// SemanticConfig holds semantic search settings.
type SemanticConfig struct {
	Enabled         bool          `yaml:"enabled"`          // Enable/disable semantic search
	Model           string        `yaml:"model"`            // Embedding model (e.g., text-embedding-004)
	IndexOnStart    bool          `yaml:"index_on_start"`   // Index workspace on startup
	MaxFileSize     int64         `yaml:"max_file_size"`    // Max file size to index (bytes)
	CacheTTL        time.Duration `yaml:"cache_ttl"`        // Cache TTL for embeddings
	TopK            int           `yaml:"top_k"`            // Default number of results
	ChunkSize       int           `yaml:"chunk_size"`       // Chunk size (characters)
	ChunkOverlap    int           `yaml:"chunk_overlap"`    // Overlap between chunks
	AutoCleanup     bool          `yaml:"auto_cleanup"`     // Auto-cleanup old projects
	IndexPatterns   []string      `yaml:"index_patterns"`   // File patterns to index
	ExcludePatterns []string      `yaml:"exclude_patterns"` // Patterns to exclude
}

// ContractConfig holds contract-driven development settings.
type ContractConfig struct {
	Enabled         bool          `yaml:"enabled"`          // Enable/disable contract system
	RequireApproval bool          `yaml:"require_approval"` // Require user approval for contracts
	AutoDetect      bool          `yaml:"auto_detect"`      // AI auto-proposes contracts
	AutoVerify      bool          `yaml:"auto_verify"`      // Auto-verify after implementation
	VerifyTimeout   time.Duration `yaml:"verify_timeout"`   // Timeout for verification commands
	StorePath       string        `yaml:"store_path"`       // Path for contract storage
	InjectContext   bool          `yaml:"inject_context"`   // Inject active contract into context
}

// MCPConfig holds MCP (Model Context Protocol) settings.
type MCPConfig struct {
	Enabled bool              `yaml:"enabled"` // Enable/disable MCP support
	Servers []MCPServerConfig `yaml:"servers"` // MCP server configurations
}

// MCPServerConfig holds configuration for a single MCP server.
type MCPServerConfig struct {
	Name        string            `yaml:"name"`                  // Unique identifier
	Transport   string            `yaml:"transport"`             // "stdio" or "http"
	Command     string            `yaml:"command,omitempty"`     // For stdio: command to run
	Args        []string          `yaml:"args,omitempty"`        // For stdio: command arguments
	Env         map[string]string `yaml:"env,omitempty"`         // Additional env vars (supports ${VAR})
	URL         string            `yaml:"url,omitempty"`         // For http: server URL
	Headers     map[string]string `yaml:"headers,omitempty"`     // For http: custom headers
	AutoConnect bool              `yaml:"auto_connect"`          // Connect on startup
	Timeout     time.Duration     `yaml:"timeout,omitempty"`     // Request timeout
	MaxRetries  int               `yaml:"max_retries,omitempty"` // Retry count
	RetryDelay  time.Duration     `yaml:"retry_delay,omitempty"` // Between retries
	ToolPrefix  string            `yaml:"tool_prefix,omitempty"` // Prefix for tool names
}

// UpdateConfig holds self-update settings.
type UpdateConfig struct {
	Enabled           bool          `yaml:"enabled"`            // Enable/disable auto-update system
	AutoCheck         bool          `yaml:"auto_check"`         // Check for updates on startup
	CheckInterval     time.Duration `yaml:"check_interval"`     // Interval between automatic checks
	AutoDownload      bool          `yaml:"auto_download"`      // Auto-download updates (not install)
	IncludePrerelease bool          `yaml:"include_prerelease"` // Include beta/rc versions
	Channel           string        `yaml:"channel"`            // Update channel: stable, beta, nightly
	GitHubRepo        string        `yaml:"github_repo"`        // GitHub repo for updates
	MaxBackups        int           `yaml:"max_backups"`        // Max backup versions to keep
	VerifyChecksum    bool          `yaml:"verify_checksum"`    // Verify downloaded file checksums
	NotifyOnly        bool          `yaml:"notify_only"`        // Only notify, don't prompt to install
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		API: APIConfig{
			Backend: "gemini",
			Retry: RetryConfig{
				MaxRetries:  3,
				RetryDelay:  1 * time.Second,
				HTTPTimeout: 120 * time.Second,
			},
		},
		Model: ModelConfig{
			Name:            "gemini-3-flash-preview", // Default - matches default backend "gemini"
			Temperature:     1.0,
			MaxOutputTokens: 8192,
			EnableThinking:  false, // Disabled by default
			ThinkingBudget:  0,     // 0 = disabled
		},
		Tools: ToolsConfig{
			Timeout: 2 * time.Minute,
			Bash: BashConfig{
				Sandbox:         true,
				BlockedCommands: []string{"rm -rf /", "mkfs"},
			},
		},
		UI: UIConfig{
			StreamOutput:      true,
			MarkdownRendering: true,
			ShowToolCalls:     true,
			ShowTokenUsage:    true,
			Theme:             "dark",
			ShowWelcome:       true,
			HintsEnabled:      true,
		},
		Context: ContextConfig{
			MaxInputTokens:     0,     // Use model default
			WarningThreshold:   0.8,   // Warn at 80%
			SummarizationRatio: 0.5,   // Summarize to 50%
			ToolResultMaxChars: 10000, // 10k chars max for tool results
			EnableAutoSummary:  true,  // Enable auto-summarization
		},
		Permission: PermissionConfig{
			Enabled:       true, // Enable permission system by default
			DefaultPolicy: "ask",
			Rules: map[string]string{
				"read":                 "allow",
				"glob":                 "allow",
				"grep":                 "allow",
				"tree":                 "allow",
				"diff":                 "allow",
				"env":                  "allow",
				"list_dir":             "allow",
				"todo":                 "allow",
				"task_output":          "allow",
				"web_fetch":            "allow",
				"web_search":           "allow",
				"ask_user":             "allow",
				"memory":               "allow",
				"kill_shell":           "allow",
				"enter_plan_mode":      "allow",
				"update_plan_progress": "allow",
				"get_plan_status":      "allow",
				"exit_plan_mode":       "allow",
				"contract_propose":     "allow",
				"contract_verify":      "allow",
				"contract_status":      "allow",
				"write":                "ask",
				"edit":                 "ask",
				"bash":                 "ask",
				"ssh":                  "ask", // SSH requires approval
			},
		},
		Plan: PlanConfig{
			Enabled:            true,  // Enabled by default
			RequireApproval:    true,  // Require approval when enabled
			AutoDetect:         true,  // Auto-trigger planning for complex tasks
			ClearContext:       true,  // Clear context before plan execution
			DelegateSteps:      true,  // Run each step in isolated sub-agent
			AbortOnStepFailure: false, // Continue by default on step failure
			PlanningTimeout:    60 * time.Second,
			UseLLMExpansion:    true,
		},
		Hooks: HooksConfig{
			Enabled: false, // Disabled by default
			Hooks:   []HookConfig{},
		},
		Web: WebConfig{
			SearchProvider: "serpapi", // Default to SerpAPI
		},
		Session: SessionConfig{
			Enabled:      true,            // Enabled by default
			SaveInterval: 2 * time.Minute, // Save every 2 minutes
			AutoLoad:     true,            // Auto-load on startup
		},
		Memory: MemoryConfig{
			Enabled:    true, // Enabled by default
			MaxEntries: 1000, // Max 1000 entries
			AutoInject: true, // Auto-inject into system prompt
		},
		Logging: LoggingConfig{
			Level: "warn", // Default to warn level (less verbose)
		},
		Audit: AuditConfig{
			Enabled:       true,  // Enabled by default
			MaxEntries:    10000, // Max 10k entries per session
			MaxResultLen:  1000,  // Truncate results to 1k chars
			RetentionDays: 30,    // Keep logs for 30 days
		},
		RateLimit: RateLimitConfig{
			Enabled:           true,    // Enabled by default
			RequestsPerMinute: 60,      // 60 requests/min
			TokensPerMinute:   1000000, // 1M tokens/min
			BurstSize:         10,      // Allow burst of 10 requests
		},
		Cache: CacheConfig{
			Enabled:  true,            // Enabled by default
			Capacity: 100,             // 100 entries
			TTL:      5 * time.Minute, // 5 minute TTL
		},
		Watcher: WatcherConfig{
			Enabled:    false, // Disabled by default (can be enabled if needed)
			DebounceMs: 500,   // 500ms debounce
			MaxWatches: 1000,  // Max 1000 watched paths
		},
		DiffPreview: DiffPreviewConfig{
			Enabled: true, // Enabled by default - show diff preview before write/edit
		},
		Semantic: SemanticConfig{
			Enabled:      false,                // Disabled by default (requires API calls)
			Model:        "text-embedding-004", // Default embedding model
			IndexOnStart: false,                // Don't index on startup by default
			MaxFileSize:  100 * 1024,           // 100KB max file size
			CacheTTL:     24 * time.Hour,       // Cache embeddings for 24 hours
			TopK:         10,                   // Return top 10 results
		},
		Contract: ContractConfig{
			Enabled:         true,
			RequireApproval: true,
			AutoDetect:      true,
			AutoVerify:      true,
			VerifyTimeout:   2 * time.Minute,
			StorePath:       ".gokin/contracts/",
			InjectContext:   true,
		},
		MCP: MCPConfig{
			Enabled: false, // Disabled by default
			Servers: []MCPServerConfig{},
		},
		Update: UpdateConfig{
			Enabled:           true,           // Enabled by default
			AutoCheck:         true,           // Check on startup
			CheckInterval:     24 * time.Hour, // Check once per day
			AutoDownload:      false,          // Require manual download
			IncludePrerelease: false,          // Only stable releases
			Channel:           "stable",       // Stable channel
			GitHubRepo:        "user/gokin",   // Should be updated to actual repo
			MaxBackups:        3,              // Keep 3 backups
			VerifyChecksum:    true,           // Always verify checksums
			NotifyOnly:        false,          // Allow prompting for install
		},
	}
}
