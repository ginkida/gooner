package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Result represents the result of running a hook.
type Result struct {
	Hook    *Hook
	Output  string
	Error   error
	Elapsed time.Duration
}

// Handler is called when a hook produces output or errors.
type Handler func(hook *Hook, output string, err error)

// Manager manages and executes hooks.
type Manager struct {
	enabled bool
	hooks   []*Hook
	workDir string
	timeout time.Duration
	handler Handler

	mu sync.RWMutex
}

// NewManager creates a new hooks manager.
func NewManager(enabled bool, workDir string) *Manager {
	return &Manager{
		enabled: enabled,
		hooks:   make([]*Hook, 0),
		workDir: workDir,
		timeout: 30 * time.Second,
	}
}

// SetEnabled enables or disables the hooks system.
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enabled
}

// IsEnabled returns whether hooks are enabled.
func (m *Manager) IsEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled
}

// SetTimeout sets the execution timeout for hooks.
func (m *Manager) SetTimeout(timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timeout = timeout
}

// SetHandler sets the output handler.
func (m *Manager) SetHandler(handler Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = handler
}

// AddHook adds a hook to the manager.
func (m *Manager) AddHook(hook *Hook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = append(m.hooks, hook)
}

// AddHooks adds multiple hooks to the manager.
func (m *Manager) AddHooks(hooks []*Hook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = append(m.hooks, hooks...)
}

// ClearHooks removes all hooks.
func (m *Manager) ClearHooks() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = make([]*Hook, 0)
}

// GetHooks returns a copy of all hooks.
func (m *Manager) GetHooks() []*Hook {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Hook, len(m.hooks))
	copy(result, m.hooks)
	return result
}

// Run executes all matching hooks for the given type and context.
func (m *Manager) Run(ctx context.Context, hookType Type, hctx *Context) []Result {
	m.mu.RLock()
	if !m.enabled {
		m.mu.RUnlock()
		return nil
	}
	hooks := m.hooks
	timeout := m.timeout
	handler := m.handler
	m.mu.RUnlock()

	if hctx.WorkDir == "" {
		hctx.WorkDir = m.workDir
	}

	var results []Result

	for _, hook := range hooks {
		if !hook.Matches(hookType, hctx.ToolName) {
			continue
		}

		result := m.executeHook(ctx, hook, hctx, timeout)
		results = append(results, result)

		if handler != nil {
			handler(hook, result.Output, result.Error)
		}
	}

	return results
}

// RunPreTool runs pre-tool hooks.
func (m *Manager) RunPreTool(ctx context.Context, toolName string, args map[string]any) []Result {
	hctx := NewContext(toolName, args, m.workDir)
	return m.Run(ctx, PreTool, hctx)
}

// RunPostTool runs post-tool hooks.
func (m *Manager) RunPostTool(ctx context.Context, toolName string, args map[string]any, result string) []Result {
	hctx := NewContext(toolName, args, m.workDir)
	hctx.SetResult(result)
	return m.Run(ctx, PostTool, hctx)
}

// RunOnError runs on-error hooks.
func (m *Manager) RunOnError(ctx context.Context, toolName string, args map[string]any, err string) []Result {
	hctx := NewContext(toolName, args, m.workDir)
	hctx.SetError(err)
	return m.Run(ctx, OnError, hctx)
}

// RunOnStart runs on-start hooks.
func (m *Manager) RunOnStart(ctx context.Context) []Result {
	hctx := &Context{WorkDir: m.workDir, Extra: make(map[string]string)}
	return m.Run(ctx, OnStart, hctx)
}

// RunOnExit runs on-exit hooks.
func (m *Manager) RunOnExit(ctx context.Context) []Result {
	hctx := &Context{WorkDir: m.workDir, Extra: make(map[string]string)}
	return m.Run(ctx, OnExit, hctx)
}

// killHookProcess attempts graceful shutdown with SIGTERM, then SIGKILL after grace period.
func killHookProcess(cmd *exec.Cmd, gracePeriod time.Duration) {
	if cmd.Process == nil {
		return
	}

	// Try SIGTERM first for graceful shutdown
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// SIGTERM failed, try SIGKILL immediately
		cmd.Process.Kill()
		return
	}

	// Wait briefly for graceful shutdown
	time.Sleep(gracePeriod)

	// Escalate to SIGKILL if process is still running
	cmd.Process.Kill()
}

// executeHook executes a single hook.
func (m *Manager) executeHook(ctx context.Context, hook *Hook, hctx *Context, timeout time.Duration) Result {
	start := time.Now()

	// Expand variables in command
	command := hctx.ExpandCommand(hook.Command)

	// Create context with timeout that respects parent cancellation
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Execute command
	cmd := exec.CommandContext(execCtx, "sh", "-c", command)
	cmd.Dir = hctx.WorkDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Start command asynchronously for better cancellation handling
	if err := cmd.Start(); err != nil {
		return Result{
			Hook:    hook,
			Error:   fmt.Errorf("failed to start hook '%s': %w", hook.Name, err),
			Elapsed: time.Since(start),
		}
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

	// Track if we were cancelled
	cancelled := false

	select {
	case <-cmdDone:
		// Command completed normally
	case <-execCtx.Done():
		// Context cancelled or timeout - kill process with graceful shutdown
		cancelled = true
		killHookProcess(cmd, 2*time.Second)
		// Wait for the Wait() goroutine to complete to avoid goroutine leak
		wg.Wait()
	}

	elapsed := time.Since(start)

	// Handle cancellation case
	if cancelled {
		return Result{
			Hook:    hook,
			Error:   fmt.Errorf("hook '%s' cancelled: %v", hook.Name, execCtx.Err()),
			Elapsed: elapsed,
		}
	}

	// Safely read the error
	cmdErrMu.Lock()
	finalErr := cmdErr
	cmdErrMu.Unlock()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if finalErr != nil {
		finalErr = fmt.Errorf("hook '%s' failed: %w (output: %s)", hook.Name, finalErr, output)
	}

	return Result{
		Hook:    hook,
		Output:  output,
		Error:   finalErr,
		Elapsed: elapsed,
	}
}

// HasHooksFor checks if there are any hooks for the given type and tool.
func (m *Manager) HasHooksFor(hookType Type, toolName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.enabled {
		return false
	}

	for _, hook := range m.hooks {
		if hook.Matches(hookType, toolName) {
			return true
		}
	}
	return false
}
