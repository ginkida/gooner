package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/logging"
	"gokin/internal/security"
	"gokin/internal/undo"
)

const maxCopyDepth = 50

// CopyTool copies files or directories.
type CopyTool struct {
	workDir       string
	undoManager   *undo.Manager
	pathValidator *security.PathValidator
}

// NewCopyTool creates a new CopyTool instance.
func NewCopyTool(workDir string) *CopyTool {
	return &CopyTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *CopyTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *CopyTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *CopyTool) Name() string {
	return "copy"
}

func (t *CopyTool) Description() string {
	return "Copies a file or directory to a new location."
}

func (t *CopyTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"source": {
					Type:        genai.TypeString,
					Description: "The path to the source file or directory",
				},
				"destination": {
					Type:        genai.TypeString,
					Description: "The path to the destination",
				},
				"recursive": {
					Type:        genai.TypeBoolean,
					Description: "If true (default), copy directories recursively",
				},
			},
			Required: []string{"source", "destination"},
		},
	}
}

func (t *CopyTool) Validate(args map[string]any) error {
	source, ok := GetString(args, "source")
	if !ok || source == "" {
		return NewValidationError("source", "is required")
	}

	dest, ok := GetString(args, "destination")
	if !ok || dest == "" {
		return NewValidationError("destination", "is required")
	}

	return nil
}

func (t *CopyTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	source, _ := GetString(args, "source")
	dest, _ := GetString(args, "destination")
	recursive := GetBoolDefault(args, "recursive", true)

	// Validate paths
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}

	validSource, err := t.pathValidator.Validate(source)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("source path validation failed: %s", err)), nil
	}
	source = validSource

	validDest, err := t.pathValidator.Validate(dest)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("destination path validation failed: %s", err)), nil
	}
	dest = validDest

	if err := security.IsBlockedWritePath(dest); err != nil {
		return NewErrorResult(err.Error()), nil
	}

	// Check source exists
	srcInfo, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("source not found: %s", source)), nil
		}
		return NewErrorResult(fmt.Sprintf("error accessing source: %s", err)), nil
	}

	// Check if source and destination are the same
	if source == dest {
		return NewErrorResult("source and destination are the same"), nil
	}

	// Refuse to clobber an existing destination (mirrors move.go). copy used to
	// O_TRUNC over it and record the undo as a "new file" (WasNew=true), so /undo
	// would then DELETE the destination — with the original content captured
	// nowhere (OldContent=nil), it was unrecoverable. Safe default: never
	// overwrite; the user can pick a new dest or delete first.
	if _, statErr := os.Stat(dest); statErr == nil {
		return NewErrorResult(fmt.Sprintf("destination already exists: %s (refusing to overwrite)", dest)), nil
	}

	// Prevent copying a directory into a subdirectory of itself
	if srcInfo.IsDir() {
		absSrc, _ := filepath.Abs(source)
		absDst, _ := filepath.Abs(dest)
		if strings.HasPrefix(absDst, absSrc+string(filepath.Separator)) {
			return NewErrorResult("cannot copy directory into itself"), nil
		}
	}

	var copiedPaths []string

	if srcInfo.IsDir() {
		if !recursive {
			return NewErrorResult("source is a directory but recursive=false"), nil
		}
		copiedPaths, err = t.copyDir(source, dest)
	} else {
		err = t.copyFile(source, dest)
		if err == nil {
			copiedPaths = []string{dest}
		}
	}

	if err != nil {
		return NewErrorResult(fmt.Sprintf("copy failed: %s", err)), nil
	}

	// Record for undo (we'll track the created destination for deletion on undo)
	unsnapshotted := 0
	if t.undoManager != nil && len(copiedPaths) > 0 {
		// For undo, we record as a "new file" creation so undo will delete it
		for _, p := range copiedPaths {
			info, err := os.Stat(p)
			if err == nil && !info.IsDir() {
				// copyFile STREAMS through io.Copy, so the copy itself never
				// holds the file. This read exists only so the memory-resident
				// undo stack can restore it, and that stack is bounded by a
				// change COUNT, not by bytes — reading without a ceiling would
				// let a streamed multi-gigabyte copy become an equally large
				// allocation held for the rest of the session. Declining is the
				// mildest trade in this family: an unsnapshotted copy leaves an
				// extra file behind, where an unsnapshotted delete loses one.
				if undoSnapshotTooLarge(info.Size()) {
					unsnapshotted++
					logging.Warn("copy: undo unavailable, file exceeds the undo snapshot limit",
						"path", p, "size", info.Size(), "limit", maxUndoSnapshotBytes)
					continue
				}
				content, _ := os.ReadFile(p)
				change := undo.NewFileChange(p, "copy", nil, content, true)
				change.Mode = info.Mode().Perm()
				t.undoManager.Record(*change)
			}
		}
	}

	if srcInfo.IsDir() {
		return NewSuccessResultWithData(
			fmt.Sprintf("Copied directory %s to %s (%d files)%s",
				source, dest, len(copiedPaths), undoSnapshotSkippedSuffix(unsnapshotted)),
			map[string]any{
				"changed":           true,
				"workspace_changed": true,
				"written_paths":     []string{dest},
			},
		), nil
	}
	return NewSuccessResultWithData(
		fmt.Sprintf("Copied %s to %s%s", source, dest, undoSnapshotSkippedSuffix(unsnapshotted)),
		map[string]any{"changed": true, "written_paths": copiedPaths},
	), nil
}

// copyFile copies a single file. Rejects symlinks to prevent symlink attacks.
func (t *CopyTool) copyFile(src, dst string) error {
	// Check source is not a symlink (use Lstat to detect symlinks)
	srcLstat, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if srcLstat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to copy symlink: %s", filepath.Base(src))
	}

	// Create destination directory if needed
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

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

	_, err = io.Copy(dstFile, srcFile)
	if closeErr := dstFile.Close(); closeErr != nil && err == nil {
		// Close can fail when buffered data fails to flush (e.g. disk full
		// after io.Copy returns). Surface that as the copy error.
		err = closeErr
	}
	// OpenFile's mode arg is masked by umask, so explicitly restore the source's
	// permission bits (chmod is NOT umask-masked) — copied scripts/binaries keep
	// their exec/group/other bits (the mode-preservation invariant).
	if err == nil {
		if chmodErr := os.Chmod(dst, srcInfo.Mode().Perm()); chmodErr != nil {
			err = chmodErr
		}
	}
	return err
}

// copyDir copies a directory recursively with depth limit and symlink protection.
func (t *CopyTool) copyDir(src, dst string) ([]string, error) {
	return t.copyDirRecursive(src, dst, 0)
}

func (t *CopyTool) copyDirRecursive(src, dst string, depth int) ([]string, error) {
	if depth > maxCopyDepth {
		return nil, fmt.Errorf("maximum directory depth (%d) exceeded", maxCopyDepth)
	}

	var copiedPaths []string

	// Use Lstat to detect symlinks at directory level
	srcLstat, err := os.Lstat(src)
	if err != nil {
		return nil, err
	}
	if srcLstat.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to copy symlink directory: %s", filepath.Base(src))
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// Check each entry with Lstat to detect symlinks
		entryInfo, err := os.Lstat(srcPath)
		if err != nil {
			return copiedPaths, err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			// Skip symlinks silently
			continue
		}

		if entry.IsDir() {
			subPaths, err := t.copyDirRecursive(srcPath, dstPath, depth+1)
			if err != nil {
				return copiedPaths, err
			}
			copiedPaths = append(copiedPaths, subPaths...)
		} else {
			if err := t.copyFile(srcPath, dstPath); err != nil {
				return copiedPaths, err
			}
			copiedPaths = append(copiedPaths, dstPath)
		}
	}

	return copiedPaths, nil
}
