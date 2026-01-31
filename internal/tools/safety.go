package tools

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SafetyLevel represents the safety classification of a tool
type SafetyLevel string

const (
	SafetyLevelSafe      SafetyLevel = "safe"      // Read-only operations
	SafetyLevelCaution   SafetyLevel = "caution"   // Writes to files, modifies state
	SafetyLevelDangerous SafetyLevel = "dangerous" // Executes commands, deletes data
	SafetyLevelCritical  SafetyLevel = "critical"  // System-level changes
)

// ToolMetadata provides extended information about tools for safety and UX
type ToolMetadata struct {
	Name        string
	SafetyLevel SafetyLevel
	Category    string   // e.g., "file", "system", "network", "ai"
	RiskFactors []string // e.g., ["data-loss", "execution", "network"]

	// Human-readable descriptions
	Impact        string   // What this tool does
	Example       string   // Example usage
	BestPractices []string // Usage recommendations

	// Constraints
	MaxExecTime          time.Duration // Maximum execution time
	RequiresConfirmation bool          // Whether to always ask user
	AllowRetry           bool          // Whether to retry on failure
}

// ExecutionSummary provides a human-readable summary of tool execution
type ExecutionSummary struct {
	ToolName         string
	DisplayName      string
	Action           string // e.g., "Read file", "Execute command"
	Target           string // e.g., "/path/to/file.txt"
	ExpectedTime     time.Duration
	RiskLevel        SafetyLevel
	UserVisible      bool // Whether to show to user
	RequiresApproval bool
}

// String returns a user-friendly string representation
func (s *ExecutionSummary) String() string {
	if s.Target != "" {
		return fmt.Sprintf("%s %s", s.Action, s.Target)
	}
	return s.Action
}

// PreFlightCheck validates execution before running the tool
type PreFlightCheck struct {
	IsValid      bool
	Warnings     []string
	Errors       []string
	Requirements []string // e.g., "file must exist", "must be in git repo"
	Suggestions  []string // e.g., "consider using read tool first"
}

// SafetyValidator provides pre-execution safety checks
type SafetyValidator interface {
	// ValidateSafety checks if execution is safe before running
	ValidateSafety(ctx context.Context, toolName string, args map[string]any) (*PreFlightCheck, error)

	// GetSummary returns a human-readable execution summary
	GetSummary(toolName string, args map[string]any) *ExecutionSummary

	// GetMetadata returns tool metadata
	GetMetadata(toolName string) (*ToolMetadata, bool)
}

// DefaultSafetyValidator provides standard safety validations
type DefaultSafetyValidator struct {
	metadata map[string]*ToolMetadata
}

// NewDefaultSafetyValidator creates a new safety validator
func NewDefaultSafetyValidator() *DefaultSafetyValidator {
	v := &DefaultSafetyValidator{
		metadata: make(map[string]*ToolMetadata),
	}
	v.initMetadata()
	return v
}

// initMetadata initializes metadata for all known tools
func (v *DefaultSafetyValidator) initMetadata() {
	// Read tool - safe
	v.metadata["read"] = &ToolMetadata{
		Name:        "read",
		SafetyLevel: SafetyLevelSafe,
		Category:    "file",
		RiskFactors: []string{},
		Impact:      "Reads and displays file contents without modification",
		Example:     `read(file_path="main.go")`,
		BestPractices: []string{
			"Always read before editing to understand file contents",
			"Use limit/offset for large files",
		},
		MaxExecTime:          5 * time.Second,
		RequiresConfirmation: false,
		AllowRetry:           true,
	}

	// Write tool - caution
	v.metadata["write"] = &ToolMetadata{
		Name:        "write",
		SafetyLevel: SafetyLevelCaution,
		Category:    "file",
		RiskFactors: []string{"data-loss", "file-overwrite"},
		Impact:      "Creates or completely overwrites files with new content",
		Example:     `write(file_path="main.go", content="package main\n...")`,
		BestPractices: []string{
			"Read file first before overwriting",
			"Consider using edit tool for partial modifications",
			"Backup important files before writing",
		},
		MaxExecTime:          10 * time.Second,
		RequiresConfirmation: true,
		AllowRetry:           true,
	}

	// Edit tool - caution
	v.metadata["edit"] = &ToolMetadata{
		Name:        "edit",
		SafetyLevel: SafetyLevelCaution,
		Category:    "file",
		RiskFactors: []string{"data-loss", "file-modification"},
		Impact:      "Makes targeted changes to files by replacing specific text",
		Example:     `edit(file_path="main.go", old_string="func old()", new_string="func new()")`,
		BestPractices: []string{
			"Always read file first to confirm old_string exists",
			"Use unique old_string to avoid multiple replacements",
			"Consider using diff tool to verify changes",
		},
		MaxExecTime:          5 * time.Second,
		RequiresConfirmation: true,
		AllowRetry:           true,
	}

	// Bash tool - dangerous
	v.metadata["bash"] = &ToolMetadata{
		Name:        "bash",
		SafetyLevel: SafetyLevelDangerous,
		Category:    "system",
		RiskFactors: []string{"execution", "system-modification", "data-loss"},
		Impact:      "Executes arbitrary shell commands with full system access",
		Example:     `bash(command="go test ./...")`,
		BestPractices: []string{
			"Always include description parameter",
			"Use non-destructive commands when possible (e.g., use --dry-run flags)",
			"Test commands in isolated environment first",
			"Prefer specific tools over bash when available (e.g., git tools, glob)",
		},
		MaxExecTime:          30 * time.Second,
		RequiresConfirmation: true,
		AllowRetry:           false,
	}

	// Delete operations - dangerous
	v.metadata["batch"] = &ToolMetadata{
		Name:        "batch",
		SafetyLevel: SafetyLevelDangerous,
		Category:    "file",
		RiskFactors: []string{"data-loss", "mass-modification"},
		Impact:      "Performs bulk operations (replace, delete, rename) on multiple files",
		Example:     `batch(operation="delete", pattern="**/*.tmp")`,
		BestPractices: []string{
			"Always use dry_run=true first to preview changes",
			"Use specific patterns to avoid unexpected matches",
			"Test pattern with glob tool before batch operations",
		},
		MaxExecTime:          60 * time.Second,
		RequiresConfirmation: true,
		AllowRetry:           false,
	}

	// Undo tool - caution
	v.metadata["undo"] = &ToolMetadata{
		Name:        "undo",
		SafetyLevel: SafetyLevelCaution,
		Category:    "file",
		RiskFactors: []string{"state-reversion"},
		Impact:      "Reverts recent file changes",
		Example:     `undo(action="undo", count=1)`,
		BestPractices: []string{
			"Use 'undo list' first to see recent changes",
			"Undo is not a substitute for version control",
			"Changes cannot be undone after being undone",
		},
		MaxExecTime:          5 * time.Second,
		RequiresConfirmation: true,
		AllowRetry:           false,
	}

	// Network tools - caution
	v.metadata["web_fetch"] = &ToolMetadata{
		Name:        "web_fetch",
		SafetyLevel: SafetyLevelCaution,
		Category:    "network",
		RiskFactors: []string{"network", "external-content"},
		Impact:      "Fetches and displays content from web URLs",
		Example:     `web_fetch(url="https://example.com/docs")`,
		BestPractices: []string{
			"Verify URLs are trustworthy",
			"Use selector to extract specific content",
			"Be aware that content may change",
		},
		MaxExecTime:          30 * time.Second,
		RequiresConfirmation: false,
		AllowRetry:           true,
	}

	v.metadata["web_search"] = &ToolMetadata{
		Name:        "web_search",
		SafetyLevel: SafetyLevelSafe,
		Category:    "network",
		RiskFactors: []string{"network"},
		Impact:      "Searches the web and returns results",
		Example:     `web_search(query="golang best practices")`,
		BestPractices: []string{
			"Use specific queries for better results",
			"Results may vary by region and time",
		},
		MaxExecTime:          15 * time.Second,
		RequiresConfirmation: false,
		AllowRetry:           true,
	}

	// Git tools - caution
	for _, tool := range []string{"git_log", "git_diff", "git_blame"} {
		v.metadata[tool] = &ToolMetadata{
			Name:        tool,
			SafetyLevel: SafetyLevelSafe,
			Category:    "git",
			RiskFactors: []string{},
			Impact:      "Reads git history and information",
			Example:     fmt.Sprintf(`%s(file="main.go", count=10)`, tool),
			BestPractices: []string{
				"These tools are read-only and safe to use",
				"Use git_diff to see changes before committing",
			},
			MaxExecTime:          10 * time.Second,
			RequiresConfirmation: false,
			AllowRetry:           true,
		}
	}

	// AI tools - safe
	for _, tool := range []string{"task", "task_output", "ask_user", "memory"} {
		v.metadata[tool] = &ToolMetadata{
			Name:        tool,
			SafetyLevel: SafetyLevelSafe,
			Category:    "ai",
			RiskFactors: []string{},
			Impact:      "AI assistant and user interaction tools",
			BestPractices: []string{
				"These tools are safe and don't modify files",
			},
			MaxExecTime:          5 * time.Second,
			RequiresConfirmation: false,
			AllowRetry:           true,
		}
	}
}

// ValidateSafety performs pre-execution safety validation
func (v *DefaultSafetyValidator) ValidateSafety(ctx context.Context, toolName string, args map[string]any) (*PreFlightCheck, error) {
	check := &PreFlightCheck{
		IsValid:      true,
		Warnings:     []string{},
		Errors:       []string{},
		Requirements: []string{},
		Suggestions:  []string{},
	}

	meta, ok := v.metadata[toolName]
	if !ok {
		check.Warnings = append(check.Warnings, fmt.Sprintf("Unknown tool: %s - no safety metadata available", toolName))
		return check, nil
	}

	// Check for dangerous operations
	switch toolName {
	case "bash":
		cmd, _ := GetString(args, "command")
		check.validateBashCommand(cmd)

	case "write":
		path, _ := GetString(args, "file_path")
		check.validateWritePath(path)

	case "edit":
		path, _ := GetString(args, "file_path")
		check.validateEditPath(path, args)

	case "batch":
		op, _ := GetString(args, "operation")
		if op == "delete" {
			check.Errors = append(check.Errors, "delete operation requires explicit confirmation")
			check.IsValid = false
		}
		pattern, _ := GetString(args, "pattern")
		check.validateBatchPattern(pattern)
	}

	// Add recommendations based on metadata
	if len(meta.BestPractices) > 0 {
		check.Suggestions = append(check.Suggestions, meta.BestPractices...)
	}

	return check, nil
}

// validateBashCommand checks bash commands for safety issues
func (c *PreFlightCheck) validateBashCommand(cmd string) {
	cmdLower := strings.ToLower(cmd)

	// Check for dangerous patterns
	dangerousPatterns := []struct {
		pattern string
		reason  string
	}{
		{"rm -rf /", "Attempting to delete root filesystem"},
		{"> /dev/sd", "Attempting to write directly to disk"},
		{"mkfs", "Attempting to format filesystem"},
		{"chmod 000", "Removing all permissions"},
		{"dd if=/dev/zero", "Attempting to overwrite disk"},
		{"curl | bash", "Piping unknown content to shell"},
		{"wget | bash", "Piping unknown content to shell"},
	}

	for _, d := range dangerousPatterns {
		if strings.Contains(cmdLower, d.pattern) {
			c.Errors = append(c.Errors, fmt.Sprintf("Dangerous command detected: %s", d.reason))
			c.IsValid = false
		}
	}

	// Warn about destructive operations
	warnPatterns := []string{
		"rm -rf",
		"rm ",
		"del ",
		"drop database",
		"truncate",
	}

	for _, p := range warnPatterns {
		if strings.Contains(cmdLower, p) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("Destructive operation detected: %s", p))
		}
	}

	// Check for missing description
	if !strings.Contains(cmd, "description") && len(strings.Fields(cmd)) > 3 {
		c.Suggestions = append(c.Suggestions, "Add description parameter to document what this command does")
	}
}

// validateWritePath checks write operations for safety
func (c *PreFlightCheck) validateWritePath(path string) {
	if path == "" {
		c.Errors = append(c.Errors, "file_path is required")
		c.IsValid = false
		return
	}

	// Warn about system paths
	dangerousPaths := []string{
		"/etc/",
		"/usr/",
		"/bin/",
		"/sbin/",
		"/boot/",
		"/sys/",
		"/proc/",
	}

	for _, d := range dangerousPaths {
		if strings.HasPrefix(path, d) {
			c.Errors = append(c.Errors, fmt.Sprintf("Cannot write to system directory: %s", d))
			c.IsValid = false
		}
	}

	// Warn about overwriting important files
	importantExts := []string{".go", ".py", ".js", ".ts", ".java", ".cpp", ".h"}
	for _, ext := range importantExts {
		if strings.HasSuffix(path, ext) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("About to overwrite source file: %s - ensure you have read it first", path))
			break
		}
	}
}

// validateEditPath checks edit operations for safety
func (c *PreFlightCheck) validateEditPath(path string, args map[string]any) {
	if path == "" {
		c.Errors = append(c.Errors, "file_path is required")
		c.IsValid = false
	}

	oldString, hasOld := GetString(args, "old_string")
	_, hasNew := GetString(args, "new_string")

	if !hasOld || oldString == "" {
		c.Errors = append(c.Errors, "old_string is required for edit operations")
		c.IsValid = false
	}

	if !hasNew {
		c.Warnings = append(c.Warnings, "new_string not provided - this will delete the old_string")
	}

	if hasOld && len(oldString) < 5 && !strings.Contains(oldString, " ") {
		c.Warnings = append(c.Warnings, "old_string is very short - may match unexpectedly. Use more unique text.")
	}
}

// validateBatchPattern checks batch operation patterns for safety
func (c *PreFlightCheck) validateBatchPattern(pattern string) {
	if pattern == "" {
		c.Errors = append(c.Errors, "pattern is required for batch operations")
		c.IsValid = false
		return
	}

	// Check for overly broad patterns
	if pattern == "*" || pattern == "**/*" {
		c.Errors = append(c.Errors, "Pattern too broad - may affect entire project. Use more specific pattern.")
		c.IsValid = false
	}

	if strings.HasPrefix(pattern, "/") {
		c.Warnings = append(c.Warnings, "Absolute path pattern - ensure this is intended")
	}
}

// GetSummary returns a human-readable execution summary
func (v *DefaultSafetyValidator) GetSummary(toolName string, args map[string]any) *ExecutionSummary {
	meta, ok := v.metadata[toolName]
	if !ok {
		return &ExecutionSummary{
			ToolName:    toolName,
			DisplayName: toolName,
			Action:      "Execute",
			RiskLevel:   SafetyLevelCaution,
			UserVisible: true,
		}
	}

	summary := &ExecutionSummary{
		ToolName:         toolName,
		DisplayName:      strings.Title(strings.ReplaceAll(toolName, "_", " ")),
		ExpectedTime:     meta.MaxExecTime,
		RiskLevel:        meta.SafetyLevel,
		RequiresApproval: meta.RequiresConfirmation,
		UserVisible:      true,
	}

	// Generate specific summary based on tool
	switch toolName {
	case "read":
		path, _ := GetString(args, "file_path")
		summary.Action = "Read file"
		summary.Target = path
		summary.DisplayName = fmt.Sprintf("Read %s", shortenPath(path, 40))

	case "write":
		path, _ := GetString(args, "file_path")
		summary.Action = "Write to file"
		summary.Target = path
		summary.DisplayName = fmt.Sprintf("Write %s", shortenPath(path, 40))

	case "edit":
		path, _ := GetString(args, "file_path")
		summary.Action = "Edit file"
		summary.Target = path
		summary.DisplayName = fmt.Sprintf("Edit %s", shortenPath(path, 40))

	case "bash":
		cmd, _ := GetString(args, "command")
		summary.Action = "Execute command"
		summary.Target = cmd
		summary.DisplayName = fmt.Sprintf("Run: %s", truncateString(cmd, 50))

	case "batch":
		op, _ := GetString(args, "operation")
		pattern, _ := GetString(args, "pattern")
		summary.Action = fmt.Sprintf("Batch %s", op)
		summary.Target = pattern
		summary.DisplayName = fmt.Sprintf("Batch %s: %s", op, pattern)

	default:
		summary.Action = meta.Impact
	}

	return summary
}

// GetMetadata returns tool metadata
func (v *DefaultSafetyValidator) GetMetadata(toolName string) (*ToolMetadata, bool) {
	meta, ok := v.metadata[toolName]
	return meta, ok
}

// Helper functions
func shortenPath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	// Try to keep filename visible
	parts := strings.Split(path, "/")
	filename := parts[len(parts)-1]
	availableLen := maxLen - len(filename) - 4 // 4 for "..."
	if availableLen < 5 {
		return "..." + filename
	}
	return path[:availableLen] + "..." + filename
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
