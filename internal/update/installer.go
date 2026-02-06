package update

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Installer handles the installation of updates.
type Installer struct {
	config         *Config
	rollbackMgr    *RollbackManager
	currentBinary  string
	progressReport ProgressCallback
}

// NewInstaller creates a new installer.
func NewInstaller(config *Config, backupDir string) (*Installer, error) {
	currentBinary, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get current executable: %w", err)
	}

	// Resolve symlinks to get the real path
	currentBinary, err = filepath.EvalSymlinks(currentBinary)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve executable path: %w", err)
	}

	rollbackMgr := NewRollbackManager(backupDir, config.MaxBackups)

	return &Installer{
		config:        config,
		rollbackMgr:   rollbackMgr,
		currentBinary: currentBinary,
	}, nil
}

// SetProgressCallback sets the progress callback.
func (i *Installer) SetProgressCallback(callback ProgressCallback) {
	i.progressReport = callback
}

// Install installs the new binary, creating a backup of the current one.
func (i *Installer) Install(ctx context.Context, newBinaryPath string, version string) error {
	i.reportProgress(StatusInstalling, "Creating backup...")

	// Create backup first
	backupInfo, err := i.rollbackMgr.CreateBackup(i.currentBinary, version)
	if err != nil {
		return fmt.Errorf("%w: failed to create backup: %w", ErrInstallFailed, err)
	}

	i.reportProgress(StatusInstalling, "Verifying new binary...")

	// Verify the new binary is executable
	if err := i.verifyBinary(newBinaryPath); err != nil {
		return fmt.Errorf("%w: %w", ErrCorruptBinary, err)
	}

	i.reportProgress(StatusInstalling, "Installing update...")

	// Perform the installation
	if err := i.replaceBinary(newBinaryPath); err != nil {
		// Try to rollback
		i.reportProgress(StatusInstalling, "Installation failed, rolling back...")
		if rollbackErr := i.rollbackMgr.Rollback(backupInfo.ID); rollbackErr != nil {
			return fmt.Errorf("%w: install failed (%v) and rollback failed (%v)",
				ErrInstallFailed, err, rollbackErr)
		}
		return fmt.Errorf("%w (rolled back successfully): %w", ErrInstallFailed, err)
	}

	// Mark backup as successful
	backupInfo.InstalledAt = time.Now()
	if err := i.rollbackMgr.SaveBackupInfo(backupInfo); err != nil {
		// Non-fatal, just log
		fmt.Fprintf(os.Stderr, "warning: failed to save backup info: %v\n", err)
	}

	// Clean up old backups
	if err := i.rollbackMgr.CleanupOldBackups(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to cleanup old backups: %v\n", err)
	}

	i.reportProgress(StatusComplete, "Update installed successfully")
	return nil
}

// verifyBinary verifies that the binary is valid and executable.
func (i *Installer) verifyBinary(binaryPath string) error {
	// Check file exists
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("binary not found: %w", err)
	}

	// Check it's a regular file
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}

	// Check file size is reasonable (at least 1MB for Go binary)
	if info.Size() < 1024*1024 {
		return fmt.Errorf("binary too small (%d bytes), possibly corrupt", info.Size())
	}

	// On Unix, check if executable
	if runtime.GOOS != "windows" {
		if info.Mode()&0111 == 0 {
			// Try to make it executable
			if err := os.Chmod(binaryPath, 0755); err != nil {
				return fmt.Errorf("cannot make binary executable: %w", err)
			}
		}
	}

	return nil
}

// replaceBinary replaces the current binary with the new one.
func (i *Installer) replaceBinary(newBinaryPath string) error {
	// On Windows, we can't replace a running binary directly
	// We need to rename it first, then copy the new one
	if runtime.GOOS == "windows" {
		return i.replaceWindows(newBinaryPath)
	}

	return i.replaceUnix(newBinaryPath)
}

// replaceUnix replaces the binary on Unix systems.
func (i *Installer) replaceUnix(newBinaryPath string) error {
	// Get info about current binary
	info, err := os.Stat(i.currentBinary)
	if err != nil {
		return err
	}

	// Create temp file in same directory (for atomic rename)
	dir := filepath.Dir(i.currentBinary)
	tmpFile, err := os.CreateTemp(dir, ".gokin-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Copy new binary to temp file
	newFile, err := os.Open(newBinaryPath)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to open new binary: %w", err)
	}

	_, err = io.Copy(tmpFile, newFile)
	newFile.Close()
	tmpFile.Close()

	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to copy new binary: %w", err)
	}

	// Set permissions
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, i.currentBinary); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}

// replaceWindows replaces the binary on Windows.
func (i *Installer) replaceWindows(newBinaryPath string) error {
	// On Windows, rename the old binary first
	oldPath := i.currentBinary + ".old"

	// Remove any existing .old file
	os.Remove(oldPath)

	// Rename current to .old
	if err := os.Rename(i.currentBinary, oldPath); err != nil {
		return fmt.Errorf("failed to rename old binary: %w", err)
	}

	// Copy new binary
	if err := copyFile(newBinaryPath, i.currentBinary); err != nil {
		// Try to restore
		os.Rename(oldPath, i.currentBinary)
		return fmt.Errorf("failed to copy new binary: %w", err)
	}

	// Remove old binary (may fail if still in use, that's OK)
	os.Remove(oldPath)

	return nil
}

// GetCurrentBinaryPath returns the path to the current binary.
func (i *Installer) GetCurrentBinaryPath() string {
	return i.currentBinary
}

// GetRollbackManager returns the rollback manager.
func (i *Installer) GetRollbackManager() *RollbackManager {
	return i.rollbackMgr
}

// reportProgress reports progress if a callback is set.
func (i *Installer) reportProgress(status UpdateStatus, message string) {
	if i.progressReport != nil {
		i.progressReport(&UpdateProgress{
			Status:  status,
			Message: message,
		})
	}
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}
