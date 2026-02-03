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

// MoveTool moves/renames files or directories.
type MoveTool struct {
	workDir       string
	undoManager   *undo.Manager
	pathValidator *security.PathValidator
}

// NewMoveTool creates a new MoveTool instance.
func NewMoveTool(workDir string) *MoveTool {
	return &MoveTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *MoveTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *MoveTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *MoveTool) Name() string {
	return "move"
}

func (t *MoveTool) Description() string {
	return "Moves or renames a file or directory."
}

func (t *MoveTool) Declaration() *genai.FunctionDeclaration {
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
			},
			Required: []string{"source", "destination"},
		},
	}
}

func (t *MoveTool) Validate(args map[string]any) error {
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

func (t *MoveTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	source, _ := GetString(args, "source")
	dest, _ := GetString(args, "destination")

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

	// Check if destination exists
	if _, err := os.Stat(dest); err == nil {
		return NewErrorResult(fmt.Sprintf("destination already exists: %s", dest)), nil
	}

	// Create destination directory if needed
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create destination directory: %s", err)), nil
	}

	// For undo, we need to save file content before moving
	var oldContent []byte
	if !srcInfo.IsDir() {
		oldContent, _ = os.ReadFile(source)
	}

	// Perform the move
	if err := os.Rename(source, dest); err != nil {
		return NewErrorResult(fmt.Sprintf("move failed: %s", err)), nil
	}

	// Record for undo
	// We record a special "move" operation that stores both paths
	if t.undoManager != nil && !srcInfo.IsDir() {
		// For files, we can record content for restoration
		// Store the original path in the change for undo (move back)
		change := &undo.FileChange{
			FilePath:   dest,
			Tool:       "move",
			OldContent: []byte(source), // Store original path in OldContent
			NewContent: oldContent,     // Store file content in NewContent
			WasNew:     false,
		}
		t.undoManager.Record(*change)
	}

	if srcInfo.IsDir() {
		return NewSuccessResult(fmt.Sprintf("Moved directory %s to %s", source, dest)), nil
	}
	return NewSuccessResult(fmt.Sprintf("Moved %s to %s", source, dest)), nil
}
