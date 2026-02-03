package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"google.golang.org/genai"

	"gokin/internal/security"
	"gokin/internal/undo"
)

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
	if t.undoManager != nil && len(copiedPaths) > 0 {
		// For undo, we record as a "new file" creation so undo will delete it
		for _, p := range copiedPaths {
			info, err := os.Stat(p)
			if err == nil && !info.IsDir() {
				content, _ := os.ReadFile(p)
				change := undo.NewFileChange(p, "copy", nil, content, true)
				t.undoManager.Record(*change)
			}
		}
	}

	if srcInfo.IsDir() {
		return NewSuccessResult(fmt.Sprintf("Copied directory %s to %s (%d files)", source, dest, len(copiedPaths))), nil
	}
	return NewSuccessResult(fmt.Sprintf("Copied %s to %s", source, dest)), nil
}

// copyFile copies a single file.
func (t *CopyTool) copyFile(src, dst string) error {
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
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// copyDir copies a directory recursively.
func (t *CopyTool) copyDir(src, dst string) ([]string, error) {
	var copiedPaths []string

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

		if entry.IsDir() {
			subPaths, err := t.copyDir(srcPath, dstPath)
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
