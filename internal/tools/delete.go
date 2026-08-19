package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/genai"

	"gokin/internal/logging"
	"gokin/internal/security"
	"gokin/internal/undo"
)

// DeleteTool deletes files or directories.
type DeleteTool struct {
	workDir       string
	undoManager   *undo.Manager
	pathValidator *security.PathValidator
}

// NewDeleteTool creates a new DeleteTool instance.
func NewDeleteTool(workDir string) *DeleteTool {
	return &DeleteTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *DeleteTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *DeleteTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *DeleteTool) Name() string {
	return "delete"
}

func (t *DeleteTool) Description() string {
	return "Deletes a file or directory. Use recursive=true to delete non-empty directories."
}

func (t *DeleteTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path to the file or directory to delete",
				},
				"recursive": {
					Type:        genai.TypeBoolean,
					Description: "If true, delete directories recursively. Required for non-empty directories.",
				},
				"force": {
					Type:        genai.TypeBoolean,
					Description: "If true, ignore errors for non-existent files.",
				},
			},
			Required: []string{"path"},
		},
	}
}

func (t *DeleteTool) Validate(args map[string]any) error {
	path, ok := GetString(args, "path")
	if !ok || path == "" {
		return NewValidationError("path", "is required")
	}

	return nil
}

func (t *DeleteTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	path, _ := GetString(args, "path")
	recursive := GetBoolDefault(args, "recursive", false)
	force := GetBoolDefault(args, "force", false)

	// Validate path
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}

	validPath, err := t.pathValidator.Validate(path)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
	}
	path = validPath

	if err := security.IsBlockedWritePath(path); err != nil {
		return NewErrorResult(err.Error()), nil
	}

	// Check if path exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if force {
				return NewSuccessResultWithData(
					fmt.Sprintf("File not found (ignored): %s", path),
					map[string]any{"changed": false, "written_paths": []string{}},
				), nil
			}
			return NewErrorResult(fmt.Sprintf("file not found: %s", path)), nil
		}
		return NewErrorResult(fmt.Sprintf("error accessing file: %s", err)), nil
	}

	// Safety check: prevent deleting work directory itself
	absPath, _ := filepath.Abs(path)
	absWorkDir, _ := filepath.Abs(t.workDir)
	if absPath == absWorkDir {
		return NewErrorResult("cannot delete working directory"), nil
	}

	// For directories, require recursive flag
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("error reading directory: %s", err)), nil
		}
		if len(entries) > 0 && !recursive {
			return NewErrorResult(fmt.Sprintf("directory not empty: %s. Use recursive=true to delete.", path)), nil
		}
	}

	// Save content for undo (only for files). If read fails (permissions, I/O),
	// we degrade gracefully: undo simply won't be recorded for this file. Log so
	// the user has a clue if /undo later seems to "skip" the deletion.
	var oldContent []byte
	snapshotSkipped := false
	if !info.IsDir() && t.undoManager != nil {
		// The pre-read exists ONLY to make the delete undoable, and the undo
		// stack is memory-resident and bounded by a change COUNT, not by bytes.
		// Reading an arbitrarily large file here would pin it in RAM for the
		// rest of the session — a streamed 4GB delete must not become a 4GB
		// allocation. Decline the snapshot and say so instead.
		if undoSnapshotTooLarge(info.Size()) {
			snapshotSkipped = true
			logging.Warn("delete: undo unavailable, file exceeds the undo snapshot limit",
				"path", path, "size", info.Size(), "limit", maxUndoSnapshotBytes)
		} else {
			var readErr error
			oldContent, readErr = os.ReadFile(path)
			if readErr != nil {
				logging.Warn("delete: undo unavailable, pre-delete read failed",
					"path", path, "error", readErr)
			}
		}
	}

	// Perform deletion
	if info.IsDir() {
		if recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
	} else {
		err = os.Remove(path)
	}

	if err != nil {
		if force && os.IsNotExist(err) {
			return NewSuccessResultWithData(
				fmt.Sprintf("File not found (ignored): %s", path),
				map[string]any{"changed": false, "written_paths": []string{}},
			), nil
		}
		return NewErrorResult(fmt.Sprintf("delete failed: %s", err)), nil
	}

	// Record for undo (only for files, not directories, and only when we
	// successfully read the content — nil means the pre-read failed).
	if t.undoManager != nil && !info.IsDir() && oldContent != nil {
		// For undo, we store the deleted content so it can be restored
		change := undo.NewFileChange(path, "delete", oldContent, nil, false)
		change.Mode = info.Mode().Perm()
		t.undoManager.Record(*change)
	}

	if info.IsDir() {
		// Directory deletes are NOT undoable — contents are not snapshotted. Say so
		// explicitly so the user doesn't trust /undo to bring the tree back.
		return NewSuccessResultWithData(
			fmt.Sprintf("Deleted directory: %s (not undoable — directory contents were not snapshotted)", path),
			map[string]any{
				"changed":           true,
				"workspace_changed": true,
				"written_paths":     []string{path},
			},
		), nil
	}
	deleted := fmt.Sprintf("Deleted file: %s", path)
	if snapshotSkipped {
		// Same honesty rule as the directory branch above: never let the user
		// believe /undo can bring back something that was never snapshotted.
		deleted += " (" + undoSnapshotSkippedNote(info.Size()) + ")"
	}
	return NewSuccessResultWithData(
		deleted,
		map[string]any{"changed": true, "written_paths": []string{path}},
	), nil
}
