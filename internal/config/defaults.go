package config

import "time"

// Default configuration values.
// These constants centralize all hardcoded values to enable easy configuration.
const (
	// Token and content limits
	DefaultMaxTokens          = 8192
	DefaultMaxChars           = 10000
	DefaultToolResultMaxChars = 30000
	DefaultMaxFetchContent    = 50000
	DefaultDiffTruncation     = 50000

	// Cache settings
	DefaultCacheSize    = 1000
	DefaultCacheTTL     = 5 * time.Minute
	DefaultLRUCacheSize = 1000

	// File system limits
	DefaultMaxWatches     = 1000
	DefaultMaxGlobResults = 1000
	DefaultChunkSize      = 1000

	// Audit settings
	DefaultAuditMaxEntries = 10000

	// Retry settings
	DefaultMaxRetries  = 3
	DefaultRetryDelay  = 1 * time.Second
	DefaultHTTPTimeout = 120 * time.Second

	// Timeout settings
	DefaultToolTimeout         = 30 * time.Second
	DefaultBashTimeout         = 30 * time.Second
	DefaultGracefulShutdown    = 10 * time.Second
	DefaultForcedShutdown      = 15 * time.Second
	DefaultPermissionTimeout   = 5 * time.Minute
	DefaultQuestionTimeout     = 5 * time.Minute
	DefaultPlanApprovalTimeout = 10 * time.Minute
	DefaultDiffDecisionTimeout = 5 * time.Minute

	// Coordinator settings
	DefaultMaxConcurrentAgents = 5
	DefaultAgentTimeout        = 30 * time.Minute
	DefaultDecomposeThreshold  = 5
	DefaultParallelThreshold   = 8

	// Context management
	DefaultContextWarningThreshold   = 0.8
	DefaultContextSummarizationRatio = 0.5

	// Session settings
	DefaultMaxSessionHistory = 100

	// Memory settings
	DefaultMaxMemoryEntries = 100

	// Rate limiting
	DefaultRequestsPerMinute = 60
	DefaultTokensPerMinute   = 100000

	// UI update intervals
	DefaultGraphUpdateInterval    = 500 * time.Millisecond
	DefaultParallelUpdateInterval = 300 * time.Millisecond
	DefaultQueueUpdateInterval    = 500 * time.Millisecond

	// Progress intervals
	DefaultProgressInterval = 5 * time.Second
	DefaultCleanupInterval  = 5 * time.Minute
	DefaultTaskCleanupAge   = 30 * time.Minute
)
