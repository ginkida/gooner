package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"gooner/internal/logging"
	"gooner/internal/security"
	"gooner/internal/tasks"

	"google.golang.org/genai"
)

// SafeEnvVars is the whitelist of environment variables passed to bash commands.
// This prevents leaking sensitive environment variables like API keys.
var SafeEnvVars = []string{
	"PATH",
	"HOME",
	"USER",
	"SHELL",
	"TERM",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TMPDIR",
	"TMP",
	"TEMP",
	"EDITOR",
	"VISUAL",
	"PAGER",
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_CACHE_HOME",
	"XDG_RUNTIME_DIR",
	// Go-specific
	"GOPATH",
	"GOROOT",
	"GOPROXY",
	"GOPRIVATE",
	"GOFLAGS",
	// Node/npm
	"NODE_PATH",
	"NPM_CONFIG_PREFIX",
	// Python
	"PYTHONPATH",
	"VIRTUAL_ENV",
	// Git
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
	"GIT_COMMITTER_NAME",
	"GIT_COMMITTER_EMAIL",
}

const (
	// DefaultBashTimeout is the default timeout for bash commands
	DefaultBashTimeout = 30 * time.Second
	// ProgressInterval is the interval for sending progress updates during long-running commands
	ProgressInterval = 5 * time.Second
)

// BashTool executes bash commands.
type BashTool struct {
	workDir        string
	taskManager    *tasks.Manager
	timeout        time.Duration // Explicit timeout for commands
	sandboxEnabled bool          // Enable sandboxing for bash commands
}

// NewBashTool creates a new BashTool instance.
func NewBashTool(workDir string) *BashTool {
	return &BashTool{
		workDir:        workDir,
		timeout:        DefaultBashTimeout, // Set default timeout
		sandboxEnabled: false,              // Sandbox disabled by default (requires root)
	}
}

// buildSafeEnv creates a sanitized environment for command execution.
// Only whitelisted environment variables are passed through.
func buildSafeEnv() []string {
	env := make([]string, 0, len(SafeEnvVars))
	for _, key := range SafeEnvVars {
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}
	// Always set a safe PATH if not already set
	hasPath := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
			break
		}
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	// Set TERM for proper terminal handling
	hasTerm := false
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			hasTerm = true
			break
		}
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// SetTimeout sets the timeout for bash commands.
func (t *BashTool) SetTimeout(timeout time.Duration) {
	t.timeout = timeout
}

// SetTaskManager sets the task manager for background execution.
func (t *BashTool) SetTaskManager(manager *tasks.Manager) {
	t.taskManager = manager
}

// SetSandboxEnabled enables or disables sandbox mode.
// When enabled, commands run in a Linux namespace sandbox (requires root).
// When disabled, commands run directly without isolation.
func (t *BashTool) SetSandboxEnabled(enabled bool) {
	t.sandboxEnabled = enabled
}

func (t *BashTool) Name() string {
	return "bash"
}

func (t *BashTool) Description() string {
	return `Executes a bash command and returns the output. Use for system operations, git commands, running tests, etc.

PARAMETERS:
- command (required): The bash command to execute
- description (optional): Brief description of what the command does
- run_in_background (optional): If true, run in background and return task ID

TIMEOUT:
- Default: 30 seconds
- Long commands: Use run_in_background=true
- Check background tasks: Use task_output tool with task_id

BLOCKED COMMANDS (safety):
- rm -rf /
- mkfs
- Fork bombs
- Direct device writes

COMMON USE CASES:
- Build: "go build ./...", "npm run build"
- Test: "go test ./...", "pytest", "npm test"
- Git: "git status", "git diff", "git log --oneline -10"
- Install: "go mod tidy", "npm install"
- Run: "go run cmd/main.go", "node app.js"

OUTPUT:
- stdout and stderr are captured
- Output >30000 chars is truncated
- Exit codes are reported on failure

AFTER RUNNING - YOU MUST:
1. Explain what the command did
2. Summarize the output (don't just dump it)
3. Highlight errors or warnings
4. Suggest fixes if command failed
5. Recommend next steps`
}

func (t *BashTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"command": {
					Type:        genai.TypeString,
					Description: "The bash command to execute",
				},
				"description": {
					Type:        genai.TypeString,
					Description: "A brief description of what the command does",
				},
				"run_in_background": {
					Type:        genai.TypeBoolean,
					Description: "If true, run the command in background and return task ID immediately",
				},
			},
			Required: []string{"command"},
		},
	}
}

func (t *BashTool) Validate(args map[string]any) error {
	command, ok := GetString(args, "command")
	if !ok || command == "" {
		return NewValidationError("command", "is required")
	}

	// Use unified command validator for comprehensive security checks
	result := security.ValidateCommand(command)
	if !result.Valid {
		return NewValidationError("command", fmt.Sprintf("blocked: %s", result.Reason))
	}

	return nil
}

func (t *BashTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	command, _ := GetString(args, "command")

	// Check if should run in background
	runInBackground, _ := args["run_in_background"].(bool)

	if runInBackground {
		return t.executeBackground(ctx, command)
	}

	return t.executeForeground(ctx, command)
}

// executeBackground starts a command in background and returns task ID.
func (t *BashTool) executeBackground(ctx context.Context, command string) (ToolResult, error) {
	if t.taskManager == nil {
		return NewErrorResult("background tasks not configured"), nil
	}

	taskID, err := t.taskManager.Start(ctx, command)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to start background task: %s", err)), nil
	}

	return NewSuccessResultWithData(
		fmt.Sprintf("Started background task: %s\nUse task_output tool with task_id=\"%s\" to check status and get output.", taskID, taskID),
		map[string]any{
			"task_id":    taskID,
			"background": true,
		},
	), nil
}

// killProcessGroup attempts graceful shutdown with SIGTERM, then SIGKILL after timeout.
func killProcessGroup(cmd *exec.Cmd, gracePeriod time.Duration) {
	if cmd.Process == nil {
		return
	}

	pid := cmd.Process.Pid

	// First, try graceful shutdown with SIGTERM
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// Process group kill failed, try individual process
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			logging.Debug("SIGTERM failed, trying SIGKILL", "error", err)
		}
	}

	// Wait briefly for graceful shutdown
	done := make(chan struct{})
	go func() {
		// The process should exit after receiving SIGTERM
		// This goroutine just signals when we should escalate to SIGKILL
		time.Sleep(gracePeriod)
		close(done)
	}()

	select {
	case <-done:
		// Grace period expired - escalate to SIGKILL
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			// Fallback to killing just the process
			if err := cmd.Process.Kill(); err != nil {
				logging.Warn("failed to kill process", "error", err)
			}
		}
	}
}

// executeForeground runs a command and waits for completion.
func (t *BashTool) executeForeground(ctx context.Context, command string) (ToolResult, error) {
	// Create context with explicit timeout to prevent indefinite hangs
	execCtx := ctx
	if t.timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	// Apply sandboxing if enabled
	if t.sandboxEnabled {
		// Use sandbox wrapper for command execution
		return t.executeSandboxed(execCtx, command)
	}

	// Fall back to standard execution (legacy behavior)
	cmd := exec.CommandContext(execCtx, "bash", "-c", command)
	cmd.Dir = t.workDir

	// Use sanitized environment to prevent leaking sensitive env vars
	cmd.Env = buildSafeEnv()

	// Set up process group for proper cleanup of child processes
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Start command
	err := cmd.Start()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to start command: %s", err)), nil
	}

	// Use WaitGroup to safely track command completion and avoid race condition
	// between context cancellation and command completion
	var wg sync.WaitGroup
	var cmdErr error
	var cmdErrMu sync.Mutex
	cmdDone := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		waitErr := cmd.Wait()
		cmdErrMu.Lock()
		cmdErr = waitErr
		cmdErrMu.Unlock()
		close(cmdDone)
	}()

	// Track if we timed out to provide proper error message
	timedOut := false

	select {
	case <-cmdDone:
		// Command completed normally - get the error result
	case <-execCtx.Done():
		// Context was cancelled or timed out
		timedOut = true
		// Kill the process group with graceful shutdown (5 second grace period)
		killProcessGroup(cmd, 5*time.Second)
		// Wait for the Wait() goroutine to complete to avoid goroutine leak
		wg.Wait()
	}

	// At this point, command has definitely finished (either completed or killed)
	// Safely read the error
	cmdErrMu.Lock()
	finalErr := cmdErr
	cmdErrMu.Unlock()

	// Handle timeout case
	if timedOut {
		return NewErrorResult(fmt.Sprintf(
			"command timed out after %v. For long-running commands, use run_in_background=true",
			t.timeout)), nil
	}

	// Handle command error
	if finalErr != nil {
		// Include exit code in error
		exitErr, ok := finalErr.(*exec.ExitError)
		if ok {
			return ToolResult{
				Content: stdout.String(),
				Error:   fmt.Sprintf("command exited with code %d", exitErr.ExitCode()),
				Success: false,
			}, nil
		}
		return NewErrorResult(fmt.Sprintf("command failed: %s", finalErr)), nil
	}

	// Build output
	var output strings.Builder

	if stdout.Len() > 0 {
		output.WriteString(stdout.String())
	}

	if stderr.Len() > 0 {
		if output.Len() > 0 {
			output.WriteString("\n")
		}
		output.WriteString("STDERR:\n")
		output.WriteString(stderr.String())
	}

	// Truncate if too long
	result := output.String()
	const maxLen = 30000
	if len(result) > maxLen {
		totalLen := len(result)
		result = result[:maxLen] + fmt.Sprintf("\n... (output truncated: showing %d of %d characters)", maxLen, totalLen)
	}

	if result == "" {
		result = "(no output)"
	}

	return NewSuccessResult(result), nil
}

// executeSandboxed executes the command with sandbox isolation
func (t *BashTool) executeSandboxed(ctx context.Context, command string) (ToolResult, error) {
	// Create sandbox configuration
	sandboxConfig := security.DefaultSandboxConfig()
	sandboxConfig.Enabled = true

	// Create sandboxed command
	sandboxed, err := security.NewSandboxedCommand(ctx, t.workDir, command, sandboxConfig)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create sandboxed command: %s", err)), nil
	}

	// Run the sandboxed command
	result := sandboxed.Run(t.timeout)

	// Handle errors
	if result.Error != nil {
		return NewErrorResult(fmt.Sprintf("sandboxed command failed: %s", result.Error)), nil
	}

	// Build output
	var output strings.Builder
	if len(result.Stdout) > 0 {
		output.Write(result.Stdout)
	}
	if len(result.Stderr) > 0 {
		if output.Len() > 0 {
			output.WriteString("\n")
		}
		output.WriteString("STDERR:\n")
		output.Write(result.Stderr)
	}

	// Check exit code
	if result.ExitCode != 0 {
		return ToolResult{
			Content: output.String(),
			Error:   fmt.Sprintf("command exited with code %d", result.ExitCode),
			Success: false,
		}, nil
	}

	return NewSuccessResult(output.String()), nil
}
