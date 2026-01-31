package fileutil

import (
	"os"
	"path/filepath"
)

// AtomicWrite writes data to a file atomically using a tmp file + rename pattern.
// This prevents data corruption if the process is interrupted during write.
// The file is written to a temporary file in the same directory, synced to disk,
// then renamed atomically to the target path.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	// Create temporary file in the same directory (required for atomic rename)
	tmp, err := os.CreateTemp(dir, ".gokin-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	// Track success to determine cleanup behavior
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	// Write data to temporary file
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}

	// Sync to disk to ensure data is persisted before rename
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	// Set permissions on temporary file
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}

	// Atomic rename - this is the key operation that makes the write atomic
	// On POSIX systems, rename is atomic when source and destination are on the same filesystem
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	success = true
	return nil
}

// AtomicWriteString is a convenience wrapper for AtomicWrite that accepts a string.
func AtomicWriteString(path string, content string, perm os.FileMode) error {
	return AtomicWrite(path, []byte(content), perm)
}
