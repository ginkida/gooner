package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/security"
)

const (
	// maxListDirEntries limits directory listing to prevent API payload overflow.
	maxListDirEntries = 2000
)

// ListDirTool lists the contents of a directory.
type ListDirTool struct {
	baseDir       string
	pathValidator *security.PathValidator
}

// NewListDirTool creates a new ListDirTool instance.
func NewListDirTool(baseDir string) *ListDirTool {
	return &ListDirTool{
		baseDir:       baseDir,
		pathValidator: security.NewPathValidator([]string{baseDir}, false),
	}
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *ListDirTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.baseDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *ListDirTool) Name() string {
	return "list_dir"
}

func (t *ListDirTool) Description() string {
	return "Lists the contents of a directory, including files and subdirectories."
}

func (t *ListDirTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"directory_path": {
					Type:        genai.TypeString,
					Description: "The path to the directory to list (relative to the working directory or absolute). Defaults to current directory if empty or not provided.",
				},
			},
			Required: []string{}, // Not required - defaults to current directory
		},
	}
}

func (t *ListDirTool) Validate(args map[string]any) error {
	// Allow empty directory_path - will default to current directory
	return nil
}

func (t *ListDirTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	dirPath, _ := GetString(args, "directory_path")

	// Default to current directory if empty
	if dirPath == "" {
		dirPath = "."
	}

	// If path is relative, make it absolute based on baseDir
	absPath := dirPath
	if !filepath.IsAbs(dirPath) {
		absPath = filepath.Join(t.baseDir, dirPath)
	}

	// Validate path if validator is configured
	if t.pathValidator != nil {
		validPath, err := t.pathValidator.ValidateDir(absPath)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
		}
		absPath = validPath
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("directory not found: %s", dirPath)), nil
		}
		return NewErrorResult(fmt.Sprintf("error reading directory: %s", err)), nil
	}

	if len(entries) == 0 {
		return NewSuccessResult("(empty)"), nil
	}

	truncated := false
	if len(entries) > maxListDirEntries {
		truncated = true
		entries = entries[:maxListDirEntries]
	}

	var builder strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		builder.WriteString(name)
		builder.WriteByte('\n')
	}

	if truncated {
		builder.WriteString(fmt.Sprintf("\n... (output truncated: showing %d entries)", maxListDirEntries))
	}

	return NewSuccessResult(builder.String()), nil
}
