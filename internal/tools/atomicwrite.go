package tools

import (
	"os"

	"gooner/internal/fileutil"
)

// AtomicWrite writes data to a file atomically using a tmp file + rename pattern.
// This is a convenience wrapper around fileutil.AtomicWrite.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	return fileutil.AtomicWrite(path, data, perm)
}

// AtomicWriteString is a convenience wrapper for AtomicWrite that accepts a string.
func AtomicWriteString(path string, content string, perm os.FileMode) error {
	return fileutil.AtomicWriteString(path, content, perm)
}
