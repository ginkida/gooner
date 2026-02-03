package ui

import (
	"time"
)

// State represents the current UI state.
type State int

const (
	StateInput State = iota
	StateProcessing
	StateStreaming
	StatePermissionPrompt
	StateQuestionPrompt
	StatePlanApproval
	StateModelSelector
	StateShortcutsOverlay
	StateCommandPalette
	StateDiffPreview
	StateSearchResults
	StateGitStatus
	StateFileBrowser
	StateBatchProgress
)

// StatusBarLayout determines the level of detail shown in the status bar based on terminal width.
type StatusBarLayout int

const (
	StatusBarLayoutFull    StatusBarLayout = iota // >= 120 chars: full information
	StatusBarLayoutMedium                         // 80-119 chars: shortened paths
	StatusBarLayoutCompact                        // 60-79 chars: key elements only
	StatusBarLayoutMinimal                        // < 60 chars: icons + token bar only
)

// ModelInfo contains information about an available model.
type ModelInfo struct {
	ID          string
	Name        string
	Description string
}

// PermissionDecision represents the user's permission choice.
type PermissionDecision int

const (
	PermissionAllow PermissionDecision = iota
	PermissionAllowSession
	PermissionDeny
	PermissionDenySession
)

// PlanApprovalDecision represents the user's decision on a plan.
type PlanApprovalDecision int

const (
	PlanApproved PlanApprovalDecision = iota
	PlanRejected
	PlanModifyRequested
)

// Message types for communication.
type (
	StreamTextMsg string
	ToolCallMsg   struct {
		Name string
		Args map[string]any
	}
	ToolResultMsg string
	// ToolProgressMsg is sent periodically during long-running tool execution.
	ToolProgressMsg struct {
		Name           string
		Elapsed        time.Duration
		Progress       float64 // 0.0-1.0, -1 for indeterminate
		CurrentStep    string  // "Copying files...", "Reading..."
		TotalBytes     int64
		ProcessedBytes int64
		Cancellable    bool
	}
	ResponseDoneMsg struct{}
	// ResponseMetadataMsg carries metadata about the completed response.
	ResponseMetadataMsg struct {
		Model        string
		InputTokens  int
		OutputTokens int
		Duration     time.Duration
		ToolsUsed    []string
	}
	ErrorMsg      error
	TodoUpdateMsg []string
	TokenUsageMsg struct {
		Tokens      int
		MaxTokens   int
		PercentUsed float64
		NearLimit   bool
		IsEstimate  bool // True when token count is an estimate (API call failed)
	}
	ProjectInfoMsg struct {
		ProjectType string
		ProjectName string
	}
	// PermissionRequestMsg requests user permission for a tool.
	PermissionRequestMsg struct {
		ToolName  string
		Args      map[string]any
		RiskLevel string
		Reason    string
	}
	// PermissionResponseMsg carries the user's permission decision.
	PermissionResponseMsg struct {
		Decision PermissionDecision
	}
	// QuestionRequestMsg requests user input for a question.
	QuestionRequestMsg struct {
		Question string
		Options  []string
		Default  string
	}
	// QuestionResponseMsg carries the user's answer.
	QuestionResponseMsg struct {
		Answer string
	}
	// PlanApprovalRequestMsg requests approval for a plan.
	PlanApprovalRequestMsg struct {
		Title       string
		Description string
		Steps       []PlanStepInfo
		// Contract fields (optional, shown when ContractName is non-empty)
		ContractName string
		Intent       string
		Boundaries   []string // Pre-formatted
		Invariants   []string
		Examples     []string
	}
	// PlanStepInfo contains info about a plan step for display.
	PlanStepInfo struct {
		ID          int
		Title       string
		Description string
	}
	// PlanProgressMsg updates plan execution progress.
	PlanProgressMsg struct {
		PlanID        string
		CurrentStepID int
		CurrentTitle  string
		TotalSteps    int
		Completed     int
		Progress      float64 // 0.0 to 1.0
		Status        string  // "in_progress", "completed", "failed"
	}
	// PlanCompleteMsg indicates a plan execution is complete.
	PlanCompleteMsg struct {
		PlanID   string
		Success  bool
		Duration time.Duration
	}
	// ConfigUpdateMsg signals that config has changed and UI should refresh.
	ConfigUpdateMsg struct {
		PermissionsEnabled bool
		SandboxEnabled     bool
	}
	// BackgroundTaskMsg signals a background task state change.
	BackgroundTaskMsg struct {
		ID          string // Task/agent ID
		Type        string // "agent" or "shell"
		Description string // Short description of the task
		Status      string // "running", "completed", "failed", "cancelled"

		// Phase 2: Progress tracking fields
		Progress      float64       // 0.0 to 1.0
		CurrentStep   int           // Current step number
		TotalSteps    int           // Total expected steps
		CurrentAction string        // "Reading file...", "Searching..."
		ToolsUsed     []string      // List of tools used so far
		TokensUsed    int           // Approximate tokens consumed
		Elapsed       time.Duration // Time elapsed since start
	}

	// BackgroundTaskProgressMsg provides periodic progress updates for background tasks.
	BackgroundTaskProgressMsg struct {
		ID            string        // Task/agent ID
		Progress      float64       // 0.0 to 1.0
		CurrentStep   int           // Current step number
		TotalSteps    int           // Total expected steps
		CurrentAction string        // Current action description
		ToolsUsed     []string      // Tools used so far
		Elapsed       time.Duration // Time elapsed
	}

	// SubAgentActivityMsg reports activity from sub-agents.
	SubAgentActivityMsg struct {
		AgentID   string
		AgentType string
		ToolName  string
		ToolArgs  map[string]any
		Status    string        // "start", "tool_start", "tool_end", "complete", "failed"
		Elapsed   time.Duration // Time elapsed since agent start
	}
)

// ActivityType distinguishes sources of activity.
type ActivityType int

const (
	ActivityTypeTool ActivityType = iota
	ActivityTypeAgent
	ActivityTypeSystem
)

// ActivityStatus tracks the lifecycle of an activity entry.
type ActivityStatus int

const (
	ActivityPending ActivityStatus = iota
	ActivityRunning
	ActivityCompleted
	ActivityFailed
)

// ActivityFeedEntry represents an entry in the activity feed.
type ActivityFeedEntry struct {
	ID          string
	Type        ActivityType
	Name        string        // Tool or agent name
	Description string        // "Reading /path/to/file.go"
	Status      ActivityStatus
	StartTime   time.Time
	Duration    time.Duration
	AgentID     string         // For sub-agent tool calls
	Details     map[string]any // Additional details
}

// StatusType indicates the type of status update.
type StatusType int

const (
	StatusRetry StatusType = iota
	StatusRateLimit
	StatusStreamIdle
	StatusStreamResume
	StatusRecoverableError
)

// StatusUpdateMsg carries status updates from clients to the UI.
// Used to show feedback during retry operations, rate limiting, and stream idle states.
type StatusUpdateMsg struct {
	Type    StatusType
	Message string
	Details map[string]any
}
