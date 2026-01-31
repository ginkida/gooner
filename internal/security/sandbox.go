package security

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// SandboxConfig holds sandbox configuration
type SandboxConfig struct {
	// Enabled determines if sandboxing is active
	Enabled bool
	// RootDir is the root directory for chroot (empty = use current workDir)
	RootDir string
	// EnableSeccomp enables seccomp-bpf syscall filtering (Linux only)
	EnableSeccomp bool
	// ReadOnly makes the sandbox filesystem read-only
	ReadOnly bool
}

// DefaultSandboxConfig returns the default sandbox configuration
func DefaultSandboxConfig() SandboxConfig {
	return SandboxConfig{
		Enabled:       true,
		EnableSeccomp: false, // Disabled by default (requires libseccomp)
		ReadOnly:      false,
	}
}

// SandboxResult represents the result of a sandboxed command execution
type SandboxResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Error    error
}

// SandboxedCommand represents a command that will be executed in a sandbox
type SandboxedCommand struct {
	cmd    *exec.Cmd
	config SandboxConfig
}

// NewSandboxedCommand creates a new sandboxed command
// Note: Full chroot and seccomp require Linux and specific permissions
// This implementation provides basic isolation with safety checks
func NewSandboxedCommand(ctx context.Context, workDir string, command string, config SandboxConfig) (*SandboxedCommand, error) {
	// Validate workDir before doing anything
	if workDir == "" {
		return nil, fmt.Errorf("workDir cannot be empty")
	}

	// Resolve absolute path to prevent directory traversal
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workDir: %w", err)
	}

	// Check if directory exists
	if _, err := os.Stat(absWorkDir); err != nil {
		return nil, fmt.Errorf("workDir does not exist: %s", absWorkDir)
	}

	// Create command with context
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = absWorkDir

	// Set safe environment variables (limited set)
	cmd.Env = safeEnvironment(absWorkDir)

	sandboxed := &SandboxedCommand{
		cmd:    cmd,
		config: config,
	}

	// Apply sandboxing if enabled
	if config.Enabled {
		if err := sandboxed.applySandbox(absWorkDir); err != nil {
			return nil, fmt.Errorf("failed to apply sandbox: %w", err)
		}
	}

	return sandboxed, nil
}

// safeEnvironment returns a sanitized environment with safe defaults
func safeEnvironment(workDir string) []string {
	// Safe environment variables whitelist
	safeVars := map[string]string{
		"PATH":        "/usr/local/bin:/usr/bin:/bin",
		"HOME":        workDir,
		"USER":        os.Getenv("USER"),
		"TERM":        "xterm",
		"LANG":        "en_US.UTF-8",
		"LC_ALL":      "en_US.UTF-8",
		"PWD":         workDir,
		"TMPDIR":      filepath.Join(workDir, "tmp"),
		"SHELL":       "/bin/bash",
		"GOPATH":      os.Getenv("GOPATH"),
		"GOROOT":      os.Getenv("GOROOT"),
		"GOPROXY":     os.Getenv("GOPROXY"),
		"NODE_PATH":   os.Getenv("NODE_PATH"),
		"PYTHONPATH":  os.Getenv("PYTHONPATH"),
		"VIRTUAL_ENV": os.Getenv("VIRTUAL_ENV"),
		"EDITOR":      os.Getenv("EDITOR"),
		"VISUAL":      os.Getenv("VISUAL"),
	}

	// Build environment array
	env := make([]string, 0, len(safeVars))
	for k, v := range safeVars {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}

	return env
}

// Run runs the sandboxed command and returns the result
func (sc *SandboxedCommand) Run(timeout time.Duration) *SandboxResult {
	result := &SandboxResult{}

	// Set up timeout if specified
	if timeout > 0 {
		timer := time.AfterFunc(timeout, func() {
			if sc.cmd.Process != nil {
				sc.cmd.Process.Kill()
			}
		})
		defer timer.Stop()
	}

	// Capture stdout and stderr
	stdout, err := sc.cmd.StdoutPipe()
	if err != nil {
		result.Error = fmt.Errorf("failed to create stdout pipe: %w", err)
		return result
	}

	stderr, err := sc.cmd.StderrPipe()
	if err != nil {
		result.Error = fmt.Errorf("failed to create stderr pipe: %w", err)
		return result
	}

	// Start the command
	if err := sc.cmd.Start(); err != nil {
		result.Error = fmt.Errorf("failed to start command: %w", err)
		return result
	}

	// Read output
	result.Stdout, _ = readWithTimeout(stdout, timeout)
	result.Stderr, _ = readWithTimeout(stderr, timeout)

	// Wait for command to finish
	err = sc.cmd.Wait()

	// Get exit code
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			result.Error = nil // Exit code is in result.ExitCode
		} else {
			result.Error = err
		}
	}

	return result
}

// readWithTimeout reads from a pipe with a timeout.
// It reads all available data from the pipe until EOF or timeout.
func readWithTimeout(pipe interface{}, timeout time.Duration) ([]byte, error) {
	reader, ok := pipe.(io.Reader)
	if !ok {
		return nil, fmt.Errorf("pipe is not an io.Reader")
	}

	// Create a channel for the read result
	type readResult struct {
		data []byte
		err  error
	}
	resultChan := make(chan readResult, 1)

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Read in a goroutine
	go func() {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, reader)
		resultChan <- readResult{data: buf.Bytes(), err: err}
	}()

	// Wait for either completion or timeout
	select {
	case <-ctx.Done():
		// Timeout occurred - return what we have (empty)
		// Note: We can't cancel the io.Copy, but the process will be killed
		// by the parent, which will close the pipe and unblock the goroutine
		return nil, fmt.Errorf("read timeout after %v", timeout)
	case result := <-resultChan:
		return result.data, result.err
	}
}

// IsSandboxSupported checks if the current system supports sandboxing features
func IsSandboxSupported() (chroot, seccomp bool) {
	// Check if running on Linux
	return runtime.GOOS == "linux", runtime.GOOS == "linux"
}

// SetupChroot prepares a chroot environment (requires CAP_SYS_CHROOT)
// This is a placeholder for future implementation
func SetupChroot(rootDir string) error {
	// Chroot requires:
	// 1. CAP_SYS_CHROOT capability (run as root)
	// 2. A valid root filesystem with necessary binaries
	// 3. Proper setup of /dev, /proc, etc.

	// For now, we don't implement full chroot because:
	// 1. Requires root privileges (security risk)
	// 2. Complex to set up properly
	// 3. May break user workflows

	// Instead, we rely on:
	// - Working directory restriction
	// - Environment sanitization
	// - Process group isolation

	return fmt.Errorf("chroot not implemented (requires root privileges)")
}

// FilterSyscalls applies seccomp filter to restrict syscalls (requires libseccomp)
// This is a placeholder for future implementation
func FilterSyscalls() error {
	// Seccomp would be implemented here using libseccomp-golang
	// Blocked syscalls would include:
	// - ptrace (prevent debugging other processes)
	// - kexec_load (prevent loading new kernels)
	// - reboot (prevent system reboot)
	// - swapon/off (prevent modifying swap)
	// - mount/umount (prevent mounting filesystems)

	return fmt.Errorf("seccomp not implemented (requires libseccomp)")
}

// DangerousSyscalls returns a list of dangerous syscalls that should be blocked
// This is for documentation purposes and future seccomp implementation
func DangerousSyscalls() []string {
	return []string{
		"ptrace",        // Process tracing/debugging
		"kexec_load",    // Load a new kernel
		"reboot",        // Reboot system
		"swapon",        // Enable swap
		"swapoff",       // Disable swap
		"mount",         // Mount filesystem
		"umount",        // Unmount filesystem
		"pivot_root",    // Change root filesystem
		"chroot",        // Change root directory
		"init_module",   // Load kernel module
		"delete_module", // Remove kernel module
		"settimeofday",  // Set system time
		"stime",         // Set system time
		"clock_settime", // Set clock
	}
}

// IsLinux checks if the current OS is Linux
func IsLinux() bool {
	return runtime.GOOS == "linux"
}
