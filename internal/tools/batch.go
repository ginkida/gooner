package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"google.golang.org/genai"

	"gooner/internal/undo"
)

// BatchTool performs batch operations on multiple files.
type BatchTool struct {
	undoManager *undo.Manager
	workDir     string
}

// NewBatchTool creates a new BatchTool instance.
func NewBatchTool(workDir string) *BatchTool {
	return &BatchTool{
		workDir: workDir,
	}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *BatchTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

func (t *BatchTool) Name() string {
	return "batch"
}

func (t *BatchTool) Description() string {
	return "Performs batch operations on multiple files matching a pattern. Supports replace, rename, and delete operations."
}

func (t *BatchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"operation": {
					Type:        genai.TypeString,
					Description: "The operation to perform: 'replace', 'rename', 'delete'",
					Enum:        []string{"replace", "rename", "delete"},
				},
				"pattern": {
					Type:        genai.TypeString,
					Description: "Glob pattern to match files (e.g., '**/*.go', 'src/*.ts')",
				},
				"files": {
					Type:        genai.TypeArray,
					Description: "Explicit list of file paths (alternative to pattern)",
					Items:       &genai.Schema{Type: genai.TypeString},
				},
				"search": {
					Type:        genai.TypeString,
					Description: "Text to search for (required for replace operation)",
				},
				"replacement": {
					Type:        genai.TypeString,
					Description: "Replacement text (required for replace operation)",
				},
				"rename_from": {
					Type:        genai.TypeString,
					Description: "Pattern to match in filenames for rename (e.g., '.old')",
				},
				"rename_to": {
					Type:        genai.TypeString,
					Description: "Replacement for filenames (e.g., '.new')",
				},
				"dry_run": {
					Type:        genai.TypeBoolean,
					Description: "Preview changes without applying them (default: false)",
				},
				"parallel": {
					Type:        genai.TypeBoolean,
					Description: "Execute operations in parallel (default: true)",
				},
			},
			Required: []string{"operation"},
		},
	}
}

func (t *BatchTool) Validate(args map[string]any) error {
	op, ok := GetString(args, "operation")
	if !ok || op == "" {
		return NewValidationError("operation", "is required")
	}

	// Check for pattern or files
	pattern, hasPattern := GetString(args, "pattern")
	files, _ := args["files"].([]interface{})
	hasFiles := len(files) > 0

	if !hasPattern && !hasFiles {
		return NewValidationError("pattern or files", "one of pattern or files is required")
	}

	// Operation-specific validation
	switch op {
	case "replace":
		search, _ := GetString(args, "search")
		if search == "" {
			return NewValidationError("search", "is required for replace operation")
		}
		_, ok := GetString(args, "replacement")
		if !ok {
			return NewValidationError("replacement", "is required for replace operation")
		}

	case "rename":
		from, _ := GetString(args, "rename_from")
		to, _ := GetString(args, "rename_to")
		if from == "" || to == "" {
			return NewValidationError("rename_from/rename_to", "both are required for rename operation")
		}

	case "delete":
		// No additional validation needed

	default:
		return NewValidationError("operation", fmt.Sprintf("unknown operation: %s", op))
	}

	_ = pattern // Used for glob matching
	return nil
}

func (t *BatchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	op, _ := GetString(args, "operation")
	pattern, _ := GetString(args, "pattern")
	dryRun := GetBoolDefault(args, "dry_run", false)
	parallel := GetBoolDefault(args, "parallel", true)

	// Collect target files
	var files []string
	var err error

	if pattern != "" {
		files, err = t.matchFiles(pattern)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("pattern error: %s", err)), nil
		}
	}

	// Add explicit files
	if fileList, ok := args["files"].([]interface{}); ok {
		for _, f := range fileList {
			if path, ok := f.(string); ok {
				files = append(files, path)
			}
		}
	}

	if len(files) == 0 {
		return NewErrorResult("no files matched the pattern or list"), nil
	}

	// Execute operation
	var result BatchResult
	switch op {
	case "replace":
		search, _ := GetString(args, "search")
		replacement, _ := GetString(args, "replacement")
		result = t.executeReplace(ctx, files, search, replacement, dryRun, parallel)

	case "rename":
		from, _ := GetString(args, "rename_from")
		to, _ := GetString(args, "rename_to")
		result = t.executeRename(ctx, files, from, to, dryRun, parallel)

	case "delete":
		result = t.executeDelete(ctx, files, dryRun, parallel)

	default:
		return NewErrorResult(fmt.Sprintf("unknown operation: %s", op)), nil
	}

	// Format result
	return t.formatResult(op, result, dryRun), nil
}

// BatchResult holds the results of a batch operation.
type BatchResult struct {
	Succeeded   []string
	Failed      map[string]string // path -> error
	Skipped     []string
	TotalFiles  int
	Description string
}

// matchFiles matches files using glob pattern.
func (t *BatchTool) matchFiles(pattern string) ([]string, error) {
	// Handle relative patterns
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(t.workDir, pattern)
	}

	matches, err := doublestar.FilepathGlob(pattern)
	if err != nil {
		return nil, err
	}

	// Filter out directories
	var files []string
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			files = append(files, match)
		}
	}

	return files, nil
}

// executeReplace performs search/replace on multiple files.
func (t *BatchTool) executeReplace(ctx context.Context, files []string, search, replacement string, dryRun, parallel bool) BatchResult {
	result := BatchResult{
		TotalFiles:  len(files),
		Failed:      make(map[string]string),
		Description: fmt.Sprintf("replace '%s' with '%s'", search, replacement),
	}

	if parallel && len(files) > 1 {
		result = t.executeParallel(ctx, files, func(path string) error {
			return t.replaceInFile(path, search, replacement, dryRun)
		})
		result.Description = fmt.Sprintf("replace '%s' with '%s'", search, replacement)
	} else {
		for _, path := range files {
			select {
			case <-ctx.Done():
				result.Failed[path] = "cancelled"
				continue
			default:
			}

			err := t.replaceInFile(path, search, replacement, dryRun)
			if err != nil {
				if strings.Contains(err.Error(), "not found") {
					result.Skipped = append(result.Skipped, path)
				} else {
					result.Failed[path] = err.Error()
				}
			} else {
				result.Succeeded = append(result.Succeeded, path)
			}
		}
	}

	return result
}

// replaceInFile performs search/replace in a single file.
func (t *BatchTool) replaceInFile(path, search, replacement string, dryRun bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)
	if !strings.Contains(content, search) {
		return fmt.Errorf("search string not found")
	}

	if dryRun {
		return nil
	}

	newContent := strings.ReplaceAll(content, search, replacement)
	newData := []byte(newContent)

	if err := os.WriteFile(path, newData, 0644); err != nil {
		return err
	}

	// Record for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(path, "batch_replace", data, newData, false)
		t.undoManager.Record(*change)
	}

	return nil
}

// executeRename renames multiple files.
func (t *BatchTool) executeRename(ctx context.Context, files []string, from, to string, dryRun, parallel bool) BatchResult {
	result := BatchResult{
		TotalFiles:  len(files),
		Failed:      make(map[string]string),
		Description: fmt.Sprintf("rename '%s' to '%s'", from, to),
	}

	for _, path := range files {
		select {
		case <-ctx.Done():
			result.Failed[path] = "cancelled"
			continue
		default:
		}

		dir := filepath.Dir(path)
		base := filepath.Base(path)

		if !strings.Contains(base, from) {
			result.Skipped = append(result.Skipped, path)
			continue
		}

		newBase := strings.ReplaceAll(base, from, to)
		newPath := filepath.Join(dir, newBase)

		if dryRun {
			result.Succeeded = append(result.Succeeded, fmt.Sprintf("%s -> %s", path, newPath))
			continue
		}

		if err := os.Rename(path, newPath); err != nil {
			result.Failed[path] = err.Error()
		} else {
			result.Succeeded = append(result.Succeeded, fmt.Sprintf("%s -> %s", path, newPath))

			// Record for undo (file rename)
			if t.undoManager != nil {
				change := undo.NewFileChange(path, "batch_rename", []byte(newPath), nil, false)
				t.undoManager.Record(*change)
			}
		}
	}

	return result
}

// executeDelete deletes multiple files.
func (t *BatchTool) executeDelete(ctx context.Context, files []string, dryRun, parallel bool) BatchResult {
	result := BatchResult{
		TotalFiles:  len(files),
		Failed:      make(map[string]string),
		Description: "delete files",
	}

	for _, path := range files {
		select {
		case <-ctx.Done():
			result.Failed[path] = "cancelled"
			continue
		default:
		}

		// Read content for undo before deletion
		var oldContent []byte
		if !dryRun && t.undoManager != nil {
			oldContent, _ = os.ReadFile(path)
		}

		if dryRun {
			result.Succeeded = append(result.Succeeded, path)
			continue
		}

		if err := os.Remove(path); err != nil {
			result.Failed[path] = err.Error()
		} else {
			result.Succeeded = append(result.Succeeded, path)

			// Record for undo
			if t.undoManager != nil && oldContent != nil {
				change := undo.NewFileChange(path, "batch_delete", oldContent, nil, false)
				t.undoManager.Record(*change)
			}
		}
	}

	return result
}

// executeParallel runs operations in parallel.
func (t *BatchTool) executeParallel(ctx context.Context, files []string, operation func(string) error) BatchResult {
	result := BatchResult{
		TotalFiles: len(files),
		Failed:     make(map[string]string),
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	// Limit concurrency
	semaphore := make(chan struct{}, 10)

	for _, path := range files {
		select {
		case <-ctx.Done():
			mu.Lock()
			result.Failed[path] = "cancelled"
			mu.Unlock()
			continue
		default:
		}

		wg.Add(1)
		semaphore <- struct{}{} // Acquire

		go func(p string) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release

			// Check if context is already cancelled
			select {
			case <-ctx.Done():
				mu.Lock()
				result.Failed[p] = "cancelled"
				mu.Unlock()
				return
			default:
			}

			err := operation(p)
			mu.Lock()
			if err != nil {
				if strings.Contains(err.Error(), "not found") {
					result.Skipped = append(result.Skipped, p)
				} else {
					result.Failed[p] = err.Error()
				}
			} else {
				result.Succeeded = append(result.Succeeded, p)
			}
			mu.Unlock()
		}(path)
	}

	wg.Wait()
	return result
}

// formatResult formats the batch result for output.
func (t *BatchTool) formatResult(op string, result BatchResult, dryRun bool) ToolResult {
	var sb strings.Builder

	prefix := ""
	if dryRun {
		prefix = "[DRY RUN] "
	}

	sb.WriteString(fmt.Sprintf("%sBatch %s: %s\n\n", prefix, op, result.Description))

	// Summary
	sb.WriteString(fmt.Sprintf("Total: %d files\n", result.TotalFiles))
	sb.WriteString(fmt.Sprintf("Succeeded: %d\n", len(result.Succeeded)))
	if len(result.Skipped) > 0 {
		sb.WriteString(fmt.Sprintf("Skipped: %d\n", len(result.Skipped)))
	}
	if len(result.Failed) > 0 {
		sb.WriteString(fmt.Sprintf("Failed: %d\n", len(result.Failed)))
	}

	// Details for small result sets
	if len(result.Succeeded) > 0 && len(result.Succeeded) <= 10 {
		sb.WriteString("\nSucceeded:\n")
		for _, path := range result.Succeeded {
			sb.WriteString(fmt.Sprintf("  ✓ %s\n", filepath.Base(path)))
		}
	}

	if len(result.Failed) > 0 {
		sb.WriteString("\nFailed:\n")
		for path, err := range result.Failed {
			sb.WriteString(fmt.Sprintf("  ✗ %s: %s\n", filepath.Base(path), err))
		}
	}

	if len(result.Failed) > 0 {
		return NewSuccessResultWithData(sb.String(), map[string]any{
			"succeeded": len(result.Succeeded),
			"failed":    len(result.Failed),
			"skipped":   len(result.Skipped),
			"total":     result.TotalFiles,
		})
	}

	return NewSuccessResult(sb.String())
}
