package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gokin/internal/security"
	"gokin/internal/tasks"

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
	// StreamingFlushInterval is the interval for flushing partial output during foreground execution
	StreamingFlushInterval = 100 * time.Millisecond
)

// BashSession maintains persistent state across bash command invocations.
// It tracks the working directory and environment variables so that
// sequential commands behave as if they run in the same shell session.
type BashSession struct {
	workDir string            // persistent working directory
	env     map[string]string // environment variables set during session
	mu      sync.Mutex        // for thread safety
}

// NewBashSession creates a new BashSession with the given initial working directory.
func NewBashSession(workDir string) *BashSession {
	return &BashSession{
		workDir: workDir,
		env:     make(map[string]string),
	}
}

// WorkDir returns the current working directory of the session.
func (s *BashSession) WorkDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workDir
}

// SetWorkDir updates the working directory of the session.
func (s *BashSession) SetWorkDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workDir = dir
}

// SetEnv sets an environment variable in the session.
func (s *BashSession) SetEnv(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.env[key] = value
}

// Env returns a copy of the session environment variables.
func (s *BashSession) Env() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]string, len(s.env))
	for k, v := range s.env {
		cp[k] = v
	}
	return cp
}

// BashTool executes bash commands.
type BashTool struct {
	workDir          string
	session          *BashSession
	taskManager      *tasks.Manager
	timeout          time.Duration // Explicit timeout for commands
	sandboxEnabled   bool          // Enable sandboxing for bash commands
	unrestrictedMode bool          // Skip command validation when both sandbox and permissions are off
}

// NewBashTool creates a new BashTool instance.
func NewBashTool(workDir string) *BashTool {
	return &BashTool{
		workDir:        workDir,
		session:        NewBashSession(workDir),
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

// SetUnrestrictedMode enables or disables unrestricted mode.
// When enabled (both sandbox and permissions are off), command validation is skipped.
func (t *BashTool) SetUnrestrictedMode(enabled bool) {
	t.unrestrictedMode = enabled
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

	// Skip command validation in unrestricted mode (sandbox=off + permissions=off)
	if t.unrestrictedMode {
		return nil
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

// buildSessionEnv creates a sanitized environment with session env vars injected.
func (t *BashTool) buildSessionEnv() []string {
	env := buildSafeEnv()

	// Inject session environment variables (override safe env if same key)
	sessionEnv := t.session.Env()
	for key, val := range sessionEnv {
		// Remove existing entry for this key if present
		found := false
		prefix := key + "="
		for i, e := range env {
			if strings.HasPrefix(e, prefix) {
				env[i] = key + "=" + val
				found = true
				break
			}
		}
		if !found {
			env = append(env, key+"="+val)
		}
	}

	return env
}

// updateSessionAfterCommand checks if the command changed the working directory
// (via cd) and updates the session accordingly.
func (t *BashTool) updateSessionAfterCommand(command string) {
	trimmed := strings.TrimSpace(command)

	// Handle bare "cd" (go to home directory)
	if trimmed == "cd" || trimmed == "cd~" || trimmed == "cd ~" {
		if home, err := os.UserHomeDir(); err == nil {
			t.session.SetWorkDir(home)
		}
		return
	}

	// Handle "cd -" — we don't track OLDPWD, so skip
	if trimmed == "cd -" {
		return
	}

	// Match commands starting with "cd " — extract the target path.
	// We handle simple cases: "cd <path>", "cd <path> && ...", "cd <path>; ..."
	// For compound commands we only update if cd is the last meaningful command,
	// but a simple heuristic is to check if it starts with "cd ".
	if !strings.HasPrefix(trimmed, "cd ") {
		return
	}

	// Extract the path argument from the cd command.
	// Stop at shell operators: &&, ||, ;, |, #
	rest := strings.TrimPrefix(trimmed, "cd ")
	rest = strings.TrimSpace(rest)

	// If the cd is followed by another command (&&, ;, ||, |), it's part of a
	// compound command — the final working directory depends on the full chain,
	// which we can't easily determine. Skip updating in that case.
	for _, sep := range []string{"&&", "||", ";", "|"} {
		if strings.Contains(rest, sep) {
			return
		}
	}

	// Strip surrounding quotes if present
	if (strings.HasPrefix(rest, "\"") && strings.HasSuffix(rest, "\"")) ||
		(strings.HasPrefix(rest, "'") && strings.HasSuffix(rest, "'")) {
		rest = rest[1 : len(rest)-1]
	}

	// Handle home directory expansion
	if strings.HasPrefix(rest, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			rest = home + rest[1:]
		}
	}

	if rest == "" {
		return
	}

	// Resolve relative paths against current session workDir
	currentDir := t.session.WorkDir()
	var target string
	if filepath.IsAbs(rest) {
		target = rest
	} else {
		target = filepath.Join(currentDir, rest)
	}

	// Clean the path
	target = filepath.Clean(target)

	// Only update if the directory actually exists
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		t.session.SetWorkDir(target)
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

	// Use session working directory
	workDir := t.session.WorkDir()

	// Fall back to standard execution (legacy behavior)
	cmd := exec.CommandContext(execCtx, "bash", "-c", command)
	cmd.Dir = workDir

	// Use sanitized environment with session env vars injected
	cmd.Env = t.buildSessionEnv()

	// Set up process group for proper cleanup of child processes
	setBashProcAttr(cmd)

	// Get progress callback for streaming output
	onProgress := GetProgressCallback(ctx)

	// Set up output capture with optional streaming
	var stdout, stderr bytes.Buffer
	if onProgress != nil {
		// Use pipes for streaming output
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			return NewErrorResult(fmt.Sprintf("failed to create stdout pipe: %s", err)), nil
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			return NewErrorResult(fmt.Sprintf("failed to create stderr pipe: %s", err)), nil
		}

		// Start command
		if err := cmd.Start(); err != nil {
			return NewErrorResult(fmt.Sprintf("failed to start command: %s", err)), nil
		}

		// Read stdout and stderr in goroutines
		var readerWg sync.WaitGroup
		var stdoutMu, stderrMu sync.Mutex

		readerWg.Add(2)
		go func() {
			defer readerWg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := stdoutPipe.Read(buf)
				if n > 0 {
					stdoutMu.Lock()
					stdout.Write(buf[:n])
					stdoutMu.Unlock()
				}
				if err != nil {
					break
				}
			}
		}()
		go func() {
			defer readerWg.Done()
			buf := make([]byte, 4096)
			for {
				n, err := stderrPipe.Read(buf)
				if n > 0 {
					stderrMu.Lock()
					stderr.Write(buf[:n])
					stderrMu.Unlock()
				}
				if err != nil {
					break
				}
			}
		}()

		// Periodically flush partial output to the progress callback
		streamStop := make(chan struct{})
		streamDone := make(chan struct{})
		go func() {
			defer close(streamDone)
			ticker := time.NewTicker(StreamingFlushInterval)
			defer ticker.Stop()
			lastSentLen := 0
			for {
				select {
				case <-ticker.C:
					stdoutMu.Lock()
					current := stdout.String()
					stdoutMu.Unlock()
					if len(current) > lastSentLen {
						partial := current[lastSentLen:]
						onProgress(0, partial)
						lastSentLen = len(current)
					}
				case <-streamStop:
					return
				}
			}
		}()

		// Wait for command completion
		var cmdErr error
		cmdDone := make(chan struct{})
		go func() {
			cmdErr = cmd.Wait()
			close(cmdDone)
		}()

		timedOut := false
		select {
		case <-cmdDone:
			// Command completed
		case <-execCtx.Done():
			timedOut = true
			killBashProcessGroup(cmd, 5*time.Second)
			<-cmdDone
		}

		// Wait for readers to drain
		readerWg.Wait()
		// Stop the streaming goroutine and wait for it to exit
		close(streamStop)
		<-streamDone

		if timedOut {
			return NewErrorResult(fmt.Sprintf(
				"command timed out after %v. For long-running commands, use run_in_background=true",
				t.timeout)), nil
		}

		// Update session after successful command
		if cmdErr == nil {
			t.updateSessionAfterCommand(command)
		}

		if cmdErr != nil {
			exitErr, ok := cmdErr.(*exec.ExitError)
			if ok {
				return ToolResult{
					Content: stdout.String(),
					Error:   fmt.Sprintf("command exited with code %d", exitErr.ExitCode()),
					Success: false,
				}, nil
			}
			return NewErrorResult(fmt.Sprintf("command failed: %s", cmdErr)), nil
		}

		return t.buildResult(stdout.String(), stderr.String()), nil
	}

	// Non-streaming path: capture output directly
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
		killBashProcessGroup(cmd, 5*time.Second)
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

	// Update session after successful command
	if finalErr == nil {
		t.updateSessionAfterCommand(command)
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

	return t.buildResult(stdout.String(), stderr.String()), nil
}

// buildResult constructs a ToolResult from stdout and stderr output.
func (t *BashTool) buildResult(stdoutStr, stderrStr string) ToolResult {
	var output strings.Builder

	if len(stdoutStr) > 0 {
		output.WriteString(stdoutStr)
	}

	if len(stderrStr) > 0 {
		if output.Len() > 0 {
			output.WriteString("\n")
		}
		output.WriteString("STDERR:\n")
		output.WriteString(stderrStr)
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

	return NewSuccessResult(result)
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
