package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"google.golang.org/genai"

	"gokin/internal/security"
	"gokin/internal/undo"
)

// MkdirTool creates directories.
type MkdirTool struct {
	workDir       string
	undoManager   *undo.Manager
	pathValidator *security.PathValidator
}

// NewMkdirTool creates a new MkdirTool instance.
func NewMkdirTool(workDir string) *MkdirTool {
	return &MkdirTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *MkdirTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *MkdirTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *MkdirTool) Name() string {
	return "mkdir"
}

func (t *MkdirTool) Description() string {
	return "Creates a directory. By default creates parent directories as needed (like mkdir -p)."
}

func (t *MkdirTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the directory to create",
				},
				"parents": {
					Type:        genai.TypeBoolean,
					Description: "If true (default), create parent directories as needed",
				},
				"mode": {
					Type:        genai.TypeString,
					Description: "Directory permissions in octal format (default: 0755)",
				},
			},
			Required: []string{"path"},
		},
	}
}

func (t *MkdirTool) Validate(args map[string]any) error {
	path, ok := GetString(args, "path")
	if !ok || path == "" {
		return NewValidationError("path", "is required")
	}

	// Validate mode if provided
	if mode, ok := GetString(args, "mode"); ok && mode != "" {
		_, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return NewValidationError("mode", "must be a valid octal number (e.g., 0755)")
		}
	}

	return nil
}

func (t *MkdirTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	path, _ := GetString(args, "path")
	parents := GetBoolDefault(args, "parents", true)
	modeStr := GetStringDefault(args, "mode", "0755")

	// Validate path
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}

	validPath, err := t.pathValidator.Validate(path)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
	}
	path = validPath

	// Parse mode
	modeVal, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("invalid mode: %s", err)), nil
	}
	mode := os.FileMode(modeVal)

	// Check if path already exists
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return NewSuccessResult(fmt.Sprintf("Directory already exists: %s", path)), nil
		}
		return NewErrorResult(fmt.Sprintf("path exists but is not a directory: %s", path)), nil
	}

	// Create directory
	if parents {
		err = os.MkdirAll(path, mode)
	} else {
		err = os.Mkdir(path, mode)
	}

	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create directory: %s", err)), nil
	}

	// Record for undo
	// For directories, we can't use the standard file change mechanism
	// We record as a "new file" with empty content so undo will attempt to remove it
	if t.undoManager != nil {
		change := undo.NewFileChange(path, "mkdir", nil, nil, true)
		t.undoManager.Record(*change)
	}

	return NewSuccessResult(fmt.Sprintf("Created directory: %s", path)), nil
}
