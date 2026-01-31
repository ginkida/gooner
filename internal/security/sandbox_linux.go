//go:build linux

package security

import (
	"syscall"
)

// applySandbox applies sandbox restrictions to the command (Linux-specific)
func (sc *SandboxedCommand) applySandbox(workDir string) error {
	// Set process attributes for basic isolation
	sc.cmd.SysProcAttr = &syscall.SysProcAttr{
		// Create new process group
		Setpgid: true,
		// Set clone flags for basic isolation
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS,
	}

	return nil
}
