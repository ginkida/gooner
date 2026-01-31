package security

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PathValidator validates file paths to prevent directory traversal attacks.
type PathValidator struct {
	allowedDirs   []string
	allowSymlinks bool
}

// NewPathValidator creates a new path validator.
func NewPathValidator(allowedDirs []string, allowSymlinks bool) *PathValidator {
	// Normalize allowed directories
	normalized := make([]string, len(allowedDirs))
	for i, dir := range allowedDirs {
		normalized[i] = filepath.Clean(dir)
	}
	return &PathValidator{
		allowedDirs:   normalized,
		allowSymlinks: allowSymlinks,
	}
}

// Validate validates that a path is safe and within allowed directories.
// Uses filepath.EvalSymlinks for atomic symlink resolution to prevent TOCTOU races.
func (v *PathValidator) Validate(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}

	// Additional security checks - do these first before any file operations
	if strings.Contains(path, "\x00") {
		return "", fmt.Errorf("null byte in path")
	}

	// Clean the path
	cleanPath := filepath.Clean(path)

	// Convert to absolute path for validation
	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute path: %w", err)
	}

	// Use EvalSymlinks for atomic symlink resolution (prevents TOCTOU race)
	// This resolves all symlinks in the path atomically
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// Path doesn't exist yet - that's OK for new files, but we need to
		// check the parent directory to prevent symlink attacks
		if os.IsNotExist(err) {
			// Check parent directory instead
			parentDir := filepath.Dir(absPath)
			resolvedParent, parentErr := filepath.EvalSymlinks(parentDir)
			if parentErr != nil && !os.IsNotExist(parentErr) {
				return "", fmt.Errorf("failed to resolve parent path: %w", parentErr)
			}
			// Use resolved parent + filename for validation
			if resolvedParent != "" {
				resolvedPath = filepath.Join(resolvedParent, filepath.Base(absPath))
			} else {
				resolvedPath = absPath
			}
		} else {
			return "", fmt.Errorf("failed to resolve symlinks: %w", err)
		}
	}

	// Check for symlink if not allowed (additional check after resolution)
	if !v.allowSymlinks {
		if err := v.checkSymlink(resolvedPath); err != nil {
			return "", err
		}
	}

	// Check if resolved path is within allowed directories
	if !v.isAllowed(resolvedPath) {
		return "", fmt.Errorf("path '%s' is outside allowed directories", path)
	}

	return resolvedPath, nil
}

// ValidateFile validates a file path for read/write operations.
func (v *PathValidator) ValidateFile(path string) (string, error) {
	absPath, err := v.Validate(path)
	if err != nil {
		return "", err
	}

	// Check if parent directory exists
	dir := filepath.Dir(absPath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return "", fmt.Errorf("parent directory does not exist: %s", dir)
	}

	return absPath, nil
}

// ValidateDir validates a directory path.
func (v *PathValidator) ValidateDir(path string) (string, error) {
	absPath, err := v.Validate(path)
	if err != nil {
		return "", err
	}

	// Check if it's actually a directory
	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("cannot access path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", path)
	}

	return absPath, nil
}

// isAllowed checks if the path is within allowed directories.
func (v *PathValidator) isAllowed(absPath string) bool {
	// If no restrictions, allow all (use with caution)
	if len(v.allowedDirs) == 0 {
		return true
	}

	for _, allowedDir := range v.allowedDirs {
		if v.isPathWithin(absPath, allowedDir) {
			return true
		}
	}
	return false
}

// isPathWithin checks if target is within base directory.
// Handles cross-drive paths on Windows and other edge cases.
func (v *PathValidator) isPathWithin(target, base string) bool {
	// Handle Windows cross-drive paths
	// filepath.Rel returns an error when target and base are on different drives
	rel, err := filepath.Rel(base, target)
	if err != nil {
		// Different drives or other path resolution issues
		// Check if they share the same root
		return filepath.VolumeName(target) == filepath.VolumeName(base)
	}

	// If relative path starts with "..", target is outside base
	if strings.HasPrefix(rel, "..") {
		return false
	}

	// Double check: joined path must match exactly or be a subpath
	joined := filepath.Join(base, rel)
	// On Windows, paths are case-insensitive, so we need to compare accordingly
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(strings.ToLower(joined), strings.ToLower(base))
	}
	return strings.HasPrefix(joined, base)
}

// checkSymlink checks if any component of the path is a symlink.
func (v *PathValidator) checkSymlink(path string) error {
	// Check each path component
	// Handle cross-platform paths
	sep := string(filepath.Separator)
	components := strings.Split(filepath.Clean(path), sep)
	
	current := ""
	if filepath.IsAbs(path) {
		if runtime.GOOS == "windows" {
			current = filepath.VolumeName(path) + sep
		} else {
			current = sep
		}
	}

	for _, comp := range components {
		if comp == "" {
			continue
		}
		current = filepath.Join(current, comp)

		info, err := os.Lstat(current)
		if err != nil {
			// Path doesn't exist yet, that's ok for new files
			if os.IsNotExist(err) {
				continue
			}
			return err
		}

		// Check if it's a symlink
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks not allowed: %s", current)
		}
	}

	return nil
}

// SanitizeFilename sanitizes a filename by removing dangerous characters.
func SanitizeFilename(name string) string {
	// Remove null bytes and other dangerous characters
	dangerous := []string{"\x00", "..", "/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	sanitized := name
	for _, dangerousChar := range dangerous {
		sanitized = strings.ReplaceAll(sanitized, dangerousChar, "_")
	}
	return sanitized
}

// JoinPathSafe joins path components safely.
func JoinPathSafe(base, rel string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("base path cannot be empty")
	}

	cleanBase := filepath.Clean(base)
	cleanRel := filepath.Clean(rel)

	// Don't allow absolute paths in relative part
	if filepath.IsAbs(cleanRel) {
		return "", fmt.Errorf("relative path cannot be absolute")
	}

	joined := filepath.Join(cleanBase, cleanRel)

	// Verify the result is still within base
	if !strings.HasPrefix(joined, cleanBase) {
		return "", fmt.Errorf("path traversal attempt detected")
	}

	return joined, nil
}
