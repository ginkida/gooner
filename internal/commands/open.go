package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
)

// processTracker tracks spawned editor processes for cleanup on exit.
var (
	spawnedPIDs   []int
	spawnedPIDsMu sync.Mutex
)

// trackPID adds a process ID to the tracking list.
func trackPID(pid int) {
	spawnedPIDsMu.Lock()
	defer spawnedPIDsMu.Unlock()
	spawnedPIDs = append(spawnedPIDs, pid)
}

// CleanupSpawnedProcesses terminates all tracked editor processes.
// Call this during graceful shutdown.
func CleanupSpawnedProcesses() {
	spawnedPIDsMu.Lock()
	defer spawnedPIDsMu.Unlock()
	for _, pid := range spawnedPIDs {
		if proc, err := os.FindProcess(pid); err == nil {
			// Send SIGTERM for graceful shutdown, not SIGKILL
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
	spawnedPIDs = nil
}

// OpenCommand opens a file in the system's default editor.
type OpenCommand struct{}

func (c *OpenCommand) Name() string        { return "open" }
func (c *OpenCommand) Description() string { return "Open a file in your editor" }
func (c *OpenCommand) Usage() string       { return "/open <file>" }
func (c *OpenCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryInteractive,
		Icon:     "edit",
		Priority: 20,
		HasArgs:  true,
		ArgHint:  "<file>",
	}
}

func (c *OpenCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if len(args) == 0 {
		return "Usage: /open <file>\n\nOpens the specified file in your default editor ($EDITOR or vi).\n\nExamples:\n  /open main.go\n  /open internal/app/app.go", nil
	}

	filePath := args[0]

	// Get the working directory
	workDir := app.GetWorkDir()

	// Resolve the file path
	absPath := filePath
	if !filepath.IsAbs(filePath) {
		absPath = filepath.Join(workDir, filePath)
	}

	// Check if file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return fmt.Sprintf("Error: File not found: %s", absPath), nil
	}

	// Determine the editor to use
	editor := os.Getenv("EDITOR")
	if editor == "" {
		// Default editors by OS
		switch runtime.GOOS {
		case "windows":
			editor = "notepad"
		case "darwin":
			editor = "open"
		default:
			editor = "vi"
		}
	}

	// For macOS, use "open -t" to open in default text editor
	if runtime.GOOS == "darwin" && editor == "open" {
		editor = "open -t"
	}

	// Execute the editor command
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "start", "", absPath)
	} else {
		// Split editor command for unix-like systems
		cmd = exec.Command("sh", "-c", fmt.Sprintf("%s %s", editor, absPath))
	}

	// Detach from parent process so the editor continues running
	if runtime.GOOS != "windows" {
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
	}

	// Start the command (don't wait)
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("Error opening file: %v\n\nEditor: %s\nFile: %s", err, editor, absPath), nil
	}

	// Track the spawned process for cleanup
	if cmd.Process != nil {
		trackPID(cmd.Process.Pid)

		// Release the process so it can be reaped by init when we exit.
		// This prevents zombie processes.
		if err := cmd.Process.Release(); err != nil {
			// Non-fatal: editor still opened, just might leave a zombie
			_ = err
		}
	}

	return fmt.Sprintf("Opening %s in %s...", filePath, editor), nil
}
