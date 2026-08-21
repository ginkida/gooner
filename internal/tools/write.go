package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/genai"

	"gokin/internal/security"
	"gokin/internal/undo"
)

// WriteTool writes content to files.
type WriteTool struct {
	workDir       string
	undoManager   *undo.Manager
	diffHandler   DiffHandler
	diffEnabled   bool
	pathValidator *security.PathValidator

	// readTracker + requireReadBeforeOverwrite mirror EditTool's read-before-edit
	// guard for the full-overwrite case: blindly overwriting an existing file the
	// model only grepped (never read) silently destroys whatever it didn't see.
	readTracker                *FileReadTracker
	requireReadBeforeOverwrite bool
}

// NewWriteTool creates a new WriteTool instance.
func NewWriteTool(workDir string) *WriteTool {
	return &WriteTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *WriteTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetDiffHandler sets the diff handler for preview approval.
func (t *WriteTool) SetDiffHandler(handler DiffHandler) {
	t.diffHandler = handler
}

// SetDiffEnabled enables or disables diff preview.
func (t *WriteTool) SetDiffEnabled(enabled bool) {
	t.diffEnabled = enabled
}

// DiffEnabled reports whether diff preview is currently enabled. Lets callers
// (and tests) observe the live state synced by updateUnrestrictedModeLocked.
func (t *WriteTool) DiffEnabled() bool {
	return t.diffEnabled
}

// SetReadTracker wires the shared file-read tracker so write can enforce
// read-before-overwrite (when enabled).
func (t *WriteTool) SetReadTracker(tracker *FileReadTracker) {
	t.readTracker = tracker
}

// SetRequireReadBeforeOverwrite toggles the read-before-overwrite guard. Only
// affects full overwrites of EXISTING files — new files and append=true are
// always allowed.
func (t *WriteTool) SetRequireReadBeforeOverwrite(require bool) {
	t.requireReadBeforeOverwrite = require
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *WriteTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *WriteTool) Name() string {
	return "write"
}

func (t *WriteTool) Description() string {
	return "Writes content to a file. Creates the file if it doesn't exist, or overwrites if it does. Set append=true to append instead of overwrite."
}

func (t *WriteTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The absolute path to the file to write",
				},
				"content": {
					Type:        genai.TypeString,
					Description: "The content to write to the file",
				},
				"append": {
					Type:        genai.TypeBoolean,
					Description: "If true, append content to the file instead of overwriting. Default: false.",
				},
			},
			Required: []string{"file_path", "content"},
		},
	}
}

func (t *WriteTool) Validate(args map[string]any) error {
	filePath, ok := GetString(args, "file_path")
	if !ok || filePath == "" {
		return NewValidationError("file_path", "is required")
	}

	content, ok := GetString(args, "content")
	if !ok {
		return NewValidationError("content", "is required")
	}
	if content == "" && !GetBoolDefault(args, "append", false) {
		return NewValidationError("content", "is empty — this would create a zero-byte file")
	}

	return nil
}

func (t *WriteTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")
	content, _ := GetString(args, "content")
	appendMode := GetBoolDefault(args, "append", false)

	// Validate path (mandatory for security)
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}

	validPath, err := t.pathValidator.Validate(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
	}
	filePath = validPath

	if err := security.IsBlockedWritePath(filePath); err != nil {
		return NewErrorResult(err.Error()), nil
	}

	// Create parent directories if they don't exist (0750: restrict group write, no others)
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return NewErrorResult(fmt.Sprintf("error creating directories: %s", err)), nil
	}

	// Check if file exists and read old content for undo
	var oldContent []byte
	_, existErr := os.Stat(filePath)
	isNew := os.IsNotExist(existErr)

	// Read-before-overwrite guard: a full overwrite of an existing file the model
	// hasn't read this session blindly discards everything it didn't see. New
	// files and append-mode are exempt (nothing is lost).
	if !isNew && !appendMode && t.requireReadBeforeOverwrite && t.readTracker != nil &&
		!t.readTracker.HasBeenRead(filePath) {
		return NewErrorResult(fmt.Sprintf(
			"read-before-overwrite: call the read tool on %s before overwriting it — a full write replaces the entire file and would discard anything you haven't seen. Read it first, then write (or use edit for a targeted change, or append=true to add without overwriting).",
			filePath,
		)), nil
	}

	if !isNew {
		var err error
		oldContent, err = os.ReadFile(filePath)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("error reading existing file: %s", err)), nil
		}
	}

	// Determine final content
	var finalContent string
	if appendMode && !isNew {
		finalContent = string(oldContent) + content
	} else {
		finalContent = content
	}

	// Show diff preview and wait for approval if enabled
	// Skip diff approval when running in delegated plan execution (context flag)
	if t.diffEnabled && t.diffHandler != nil && !ShouldSkipDiff(ctx) {
		approved, err := t.diffHandler.PromptDiff(ctx, filePath, string(oldContent), finalContent, "write", isNew)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("diff preview error: %s", err)), nil
		}
		if !approved {
			return NewErrorResult("changes rejected by user"), nil
		}
	}

	// Write file atomically to prevent data corruption on interruption
	newContent := []byte(finalContent)
	perm := os.FileMode(0644)
	if info, err := os.Stat(filePath); err == nil {
		perm = info.Mode().Perm()
	}
	if err := AtomicWrite(filePath, newContent, perm); err != nil {
		return NewErrorResult(fmt.Sprintf("error writing file: %s", err)), nil
	}

	// Record change for undo. Unlike delete and copy, the old content is read
	// for the operation itself — append concatenates it and the diff preview
	// shows it — so the read cannot be skipped; only the RECORD is refused, and
	// that is the half that would otherwise pin the bytes for the session.
	snapshotDeclined := false
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "write", oldContent, newContent, isNew)
		change.Mode = perm
		snapshotDeclined = !t.undoManager.Record(*change)
	}

	// Create status message
	var status string
	if appendMode && !isNew {
		status = fmt.Sprintf("Appended to %s (%d bytes added, %d bytes total)", filePath, len(content), len(finalContent))
	} else if isNew {
		status = fmt.Sprintf("Created new file: %s (%d bytes)", filePath, len(content))
	} else {
		status = fmt.Sprintf("Updated file: %s (%d bytes)", filePath, len(content))
	}

	if snapshotDeclined {
		status += " (" + undoSnapshotDeclinedNote() + ")"
	}

	// Emit FilePeek
	EmitFilePeek(ctx, filePath, "Writing", finalContent, "write")

	return NewSuccessResult(status), nil
}
