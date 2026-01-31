//go:build !linux

package security

import (
	"syscall"
)

// applySandbox applies basic process isolation for non-Linux platforms
func (sc *SandboxedCommand) applySandbox(workDir string) error {
	// For non-Linux systems, we provide basic process group isolation
	sc.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	return nil
}
