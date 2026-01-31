package tasks

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
)

// SafeEnvVars is the whitelist of environment variables passed to task commands.
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
	"GOPATH",
	"GOROOT",
	"GOPROXY",
	"GOPRIVATE",
	"GOFLAGS",
	"NODE_PATH",
	"NPM_CONFIG_PREFIX",
	"PYTHONPATH",
	"VIRTUAL_ENV",
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
	"GIT_COMMITTER_NAME",
	"GIT_COMMITTER_EMAIL",
}

// buildSafeEnv creates a sanitized environment for command execution.
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

// Status represents the status of a background task.
type Status int

const (
	StatusPending Status = iota
	StatusRunning
	StatusCompleted
	StatusFailed
	StatusCancelled
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Task represents a background task.
type Task struct {
	ID        string
	Command   string
	Status    Status
	Output    bytes.Buffer
	Error     string
	ExitCode  int
	StartTime time.Time
	EndTime   time.Time
	WorkDir   string

	cmd        *exec.Cmd
	cancelFunc context.CancelFunc
	mu         sync.RWMutex
}

// NewTask creates a new background task.
func NewTask(id, command, workDir string) *Task {
	return &Task{
		ID:      id,
		Command: command,
		Status:  StatusPending,
		WorkDir: workDir,
	}
}

// Start starts the task execution.
func (t *Task) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.Status != StatusPending {
		t.mu.Unlock()
		return fmt.Errorf("task already started")
	}

	// Create cancellable context
	execCtx, cancel := context.WithCancel(ctx)
	t.cancelFunc = cancel

	// Create command
	t.cmd = exec.CommandContext(execCtx, "sh", "-c", t.Command)
	t.cmd.Dir = t.WorkDir
	t.cmd.Stdout = &t.Output
	t.cmd.Stderr = &t.Output

	// Use sanitized environment to prevent leaking sensitive env vars
	t.cmd.Env = buildSafeEnv()

	// Set up process group for proper cleanup of child processes
	t.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	t.Status = StatusRunning
	t.StartTime = time.Now()
	t.mu.Unlock()

	// Run in background
	go t.run()

	return nil
}

// run executes the command and updates status.
func (t *Task) run() {
	err := t.cmd.Run()

	t.mu.Lock()
	defer t.mu.Unlock()

	t.EndTime = time.Now()

	if err != nil {
		if t.Status == StatusCancelled {
			// Already cancelled, keep that status
			return
		}
		t.Status = StatusFailed
		t.Error = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.ExitCode = exitErr.ExitCode()
		} else {
			t.ExitCode = -1
		}
	} else {
		t.Status = StatusCompleted
		t.ExitCode = 0
	}
}

// Cancel cancels the task.
func (t *Task) Cancel() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.Status == StatusRunning && t.cancelFunc != nil {
		// Kill entire process group for proper cleanup
		if t.cmd != nil && t.cmd.Process != nil {
			// Kill process group (negative PID)
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
		t.cancelFunc()
		t.Status = StatusCancelled
		t.EndTime = time.Now()
	}
}

// GetStatus returns the current status.
func (t *Task) GetStatus() Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status
}

// GetOutput returns the current output.
func (t *Task) GetOutput() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Output.String()
}

// GetError returns the error message if failed.
func (t *Task) GetError() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Error
}

// IsRunning returns true if the task is still running.
func (t *Task) IsRunning() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status == StatusRunning
}

// IsComplete returns true if the task has finished (success, fail, or cancelled).
func (t *Task) IsComplete() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status == StatusCompleted || t.Status == StatusFailed || t.Status == StatusCancelled
}

// Duration returns the task duration.
func (t *Task) Duration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.StartTime.IsZero() {
		return 0
	}
	if t.EndTime.IsZero() {
		return time.Since(t.StartTime)
	}
	return t.EndTime.Sub(t.StartTime)
}

// Info returns a summary of the task.
type Info struct {
	ID        string
	Command   string
	Status    string
	Output    string
	Error     string
	ExitCode  int
	Duration  time.Duration
	StartTime time.Time
	EndTime   time.Time
}

// GetInfo returns task information.
func (t *Task) GetInfo() Info {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return Info{
		ID:        t.ID,
		Command:   t.Command,
		Status:    t.Status.String(),
		Output:    t.Output.String(),
		Error:     t.Error,
		ExitCode:  t.ExitCode,
		Duration:  t.Duration(),
		StartTime: t.StartTime,
		EndTime:   t.EndTime,
	}
}
