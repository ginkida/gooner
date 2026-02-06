package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RollbackManager manages backups and rollbacks.
type RollbackManager struct {
	backupDir  string
	maxBackups int
}

// NewRollbackManager creates a new rollback manager.
func NewRollbackManager(backupDir string, maxBackups int) *RollbackManager {
	if maxBackups < 1 {
		maxBackups = 3
	}
	return &RollbackManager{
		backupDir:  backupDir,
		maxBackups: maxBackups,
	}
}

// CreateBackup creates a backup of the current binary.
func (rm *RollbackManager) CreateBackup(binaryPath string, version string) (*BackupInfo, error) {
	// Ensure backup directory exists
	if err := os.MkdirAll(rm.backupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Generate backup ID based on timestamp
	backupID := time.Now().Format("20060102-150405")
	backupName := fmt.Sprintf("gokin-%s-%s", version, backupID)
	backupPath := filepath.Join(rm.backupDir, backupName)

	// Copy current binary to backup
	if err := copyFile(binaryPath, backupPath); err != nil {
		return nil, fmt.Errorf("failed to copy binary: %w", err)
	}

	// Get file size
	stat, err := os.Stat(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat backup: %w", err)
	}

	// Compute checksum
	checksum, err := computeFileChecksum(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to compute checksum: %w", err)
	}

	// Create backup info
	info := &BackupInfo{
		ID:         backupID,
		Version:    version,
		Path:       backupPath,
		CreatedAt:  time.Now(),
		BinaryPath: binaryPath,
		Size:       stat.Size(),
		Checksum:   checksum,
	}

	// Save backup info
	if err := rm.SaveBackupInfo(info); err != nil {
		// Non-fatal, backup still works
		fmt.Fprintf(os.Stderr, "warning: failed to save backup info: %v\n", err)
	}

	return info, nil
}

// SaveBackupInfo saves backup information to a JSON file.
func (rm *RollbackManager) SaveBackupInfo(info *BackupInfo) error {
	infoPath := info.Path + ".json"
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(infoPath, data, 0644)
}

// LoadBackupInfo loads backup information from a JSON file.
func (rm *RollbackManager) LoadBackupInfo(backupPath string) (*BackupInfo, error) {
	infoPath := backupPath + ".json"
	data, err := os.ReadFile(infoPath)
	if err != nil {
		// If no info file, create basic info from path
		stat, statErr := os.Stat(backupPath)
		if statErr != nil {
			return nil, err
		}
		return &BackupInfo{
			ID:        filepath.Base(backupPath),
			Path:      backupPath,
			CreatedAt: stat.ModTime(),
		}, nil
	}

	var info BackupInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListBackups returns a list of available backups.
func (rm *RollbackManager) ListBackups() ([]*BackupInfo, error) {
	entries, err := os.ReadDir(rm.backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var backups []*BackupInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Skip .json files
		if filepath.Ext(entry.Name()) == ".json" {
			continue
		}

		backupPath := filepath.Join(rm.backupDir, entry.Name())
		info, err := rm.LoadBackupInfo(backupPath)
		if err != nil {
			continue
		}
		backups = append(backups, info)
	}

	// Sort by creation time (newest first)
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// GetLatestBackup returns the most recent backup.
func (rm *RollbackManager) GetLatestBackup() (*BackupInfo, error) {
	backups, err := rm.ListBackups()
	if err != nil {
		return nil, err
	}
	if len(backups) == 0 {
		return nil, ErrNoBackup
	}
	return backups[0], nil
}

// Rollback restores a backup by ID.
func (rm *RollbackManager) Rollback(backupID string) error {
	backups, err := rm.ListBackups()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRollbackFailed, err)
	}

	var backup *BackupInfo
	for _, b := range backups {
		if b.ID == backupID {
			backup = b
			break
		}
	}

	if backup == nil {
		return fmt.Errorf("%w: backup %q not found", ErrNoBackup, backupID)
	}

	return rm.RollbackToBackup(backup)
}

// RollbackToLatest restores the most recent backup.
func (rm *RollbackManager) RollbackToLatest() error {
	backup, err := rm.GetLatestBackup()
	if err != nil {
		return err
	}
	return rm.RollbackToBackup(backup)
}

// RollbackToBackup restores a specific backup.
func (rm *RollbackManager) RollbackToBackup(backup *BackupInfo) error {
	if backup == nil {
		return ErrNoBackup
	}

	// Verify backup file exists
	if _, err := os.Stat(backup.Path); err != nil {
		return fmt.Errorf("%w: backup file not found", ErrRollbackFailed)
	}

	// Get target path (original binary location)
	targetPath := backup.BinaryPath
	if targetPath == "" {
		// Try to determine from current executable
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("%w: cannot determine target path", ErrRollbackFailed)
		}
		targetPath, _ = filepath.EvalSymlinks(exe)
	}

	// Restore the backup
	if err := rm.restoreBackup(backup.Path, targetPath); err != nil {
		return fmt.Errorf("%w: %w", ErrRollbackFailed, err)
	}

	return nil
}

// restoreBackup copies a backup to the target location.
func (rm *RollbackManager) restoreBackup(backupPath, targetPath string) error {
	// Get backup file info
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		return err
	}

	// Create temp file in target directory
	dir := filepath.Dir(targetPath)
	tmpFile, err := os.CreateTemp(dir, ".gokin-rollback-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Copy backup to temp
	backupFile, err := os.Open(backupPath)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}

	_, err = io.Copy(tmpFile, backupFile)
	backupFile.Close()
	tmpFile.Close()

	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to copy backup: %w", err)
	}

	// Set permissions
	if err := os.Chmod(tmpPath, backupInfo.Mode()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to replace binary: %w", err)
	}

	return nil
}

// CleanupOldBackups removes old backups, keeping only maxBackups.
func (rm *RollbackManager) CleanupOldBackups() error {
	backups, err := rm.ListBackups()
	if err != nil {
		return err
	}

	// Keep newest maxBackups, remove the rest
	if len(backups) <= rm.maxBackups {
		return nil
	}

	for _, backup := range backups[rm.maxBackups:] {
		rm.DeleteBackup(backup)
	}

	return nil
}

// DeleteBackup removes a backup.
func (rm *RollbackManager) DeleteBackup(backup *BackupInfo) error {
	if backup == nil {
		return nil
	}

	// Remove backup file
	os.Remove(backup.Path)
	// Remove info file
	os.Remove(backup.Path + ".json")

	return nil
}

// GetBackupDir returns the backup directory path.
func (rm *RollbackManager) GetBackupDir() string {
	return rm.backupDir
}

// computeFileChecksum computes the SHA256 checksum of a file.
func computeFileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
