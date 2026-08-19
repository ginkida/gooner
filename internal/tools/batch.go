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

	"gokin/internal/logging"
	"gokin/internal/security"
	"gokin/internal/undo"
)

// BatchProgressCallback is called to report progress during batch operations.
type BatchProgressCallback func(processed, total int, currentFile string, success bool)

// BatchTool performs batch operations on multiple files.
type BatchTool struct {
	undoManager      *undo.Manager
	workDir          string
	progressCallback BatchProgressCallback
	failureThreshold float64 // Stop if failure rate exceeds this (0.0 to 1.0, 0 = disabled)
	pathValidator    *security.PathValidator
}

// NewBatchTool creates a new BatchTool instance.
func NewBatchTool(workDir string) *BatchTool {
	return &BatchTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetAllowedDirs sets additional allowed directories for path validation,
// mirroring edit/write/delete/refactor. Without this, batch would write/delete
// arbitrary model-supplied paths outside the workspace and bypass .git
// protection.
func (t *BatchTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

// validateWritePath enforces the workspace boundary + .git protection for every
// model-supplied path the batch tool mutates (replace/rename/delete) — the same
// chokepoint edit/write/delete/refactor use. Returns the validated path.
func (t *BatchTool) validateWritePath(path string) (string, error) {
	if t.pathValidator == nil {
		return "", fmt.Errorf("security error: path validator not initialized")
	}
	valid, err := t.pathValidator.Validate(path)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %s", err)
	}
	if err := security.IsBlockedWritePath(valid); err != nil {
		return "", err
	}
	return valid, nil
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *BatchTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetProgressCallback sets the progress callback for real-time updates.
func (t *BatchTool) SetProgressCallback(callback BatchProgressCallback) {
	t.progressCallback = callback
}

// SetFailureThreshold sets the failure threshold (0.0 to 1.0).
// When the failure rate exceeds this threshold, the batch operation stops.
// Set to 0 to disable (default).
func (t *BatchTool) SetFailureThreshold(threshold float64) {
	if threshold < 0 {
		threshold = 0
	}
	if threshold > 1 {
		threshold = 1
	}
	t.failureThreshold = threshold
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
				"failure_threshold": {
					Type:        genai.TypeNumber,
					Description: "Stop if failure rate exceeds this value (0.0 to 1.0, default: 0 = disabled)",
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
	files, _ := args["files"].([]any)
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
	if fileList, ok := args["files"].([]any); ok {
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
	Succeeded []string
	Failed    map[string]string // path -> error
	Skipped   []string
	// RawPaths are the actual filesystem paths that CHANGED (for rename,
	// both old and new). Succeeded holds display strings ("old -> new" for
	// rename), which aren't real paths — RawPaths feeds written_paths so the
	// done-gate and read-dedup see the true set. Populated on real writes
	// only (never dry-run).
	RawPaths []string
	// Unsnapshotted lists files that were deleted but whose content was too
	// large to hold in the memory-resident undo stack. They are NOT undoable
	// and the summary says so — a batch the user believes is reversible but
	// partly is not is worse than one that discloses the gap.
	Unsnapshotted []string
	TotalFiles    int
	Description   string
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
		// executeParallel only fills Succeeded; mirror the sequential branch so
		// written_paths is populated on the parallel path too (Succeeded holds
		// real file paths for replace).
		if !dryRun {
			result.RawPaths = append(result.RawPaths, result.Succeeded...)
		}
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
				if !dryRun {
					result.RawPaths = append(result.RawPaths, path)
				}
			}
		}
	}

	return result
}

// replaceInFile performs search/replace in a single file.
func (t *BatchTool) replaceInFile(path, search, replacement string, dryRun bool) error {
	validPath, err := t.validateWritePath(path)
	if err != nil {
		return err
	}
	path = validPath

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

	perm := os.FileMode(0644)
	if info, stErr := os.Stat(path); stErr == nil {
		perm = info.Mode().Perm()
	}
	if err := AtomicWrite(path, newData, perm); err != nil {
		return err
	}

	// Record for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(path, "batch_replace", data, newData, false)
		change.Mode = perm
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

	// Security: validate rename_to doesn't contain path traversal sequences
	if strings.Contains(to, "..") || strings.Contains(to, "/") || strings.Contains(to, string(filepath.Separator)) {
		result.Failed["validation"] = "rename_to cannot contain path separators or '..'"
		return result
	}

	for _, path := range files {
		select {
		case <-ctx.Done():
			result.Failed[path] = "cancelled"
			continue
		default:
		}

		validPath, vErr := t.validateWritePath(path)
		if vErr != nil {
			result.Failed[path] = vErr.Error()
			continue
		}
		path = validPath

		dir := filepath.Dir(path)
		base := filepath.Base(path)

		if !strings.Contains(base, from) {
			result.Skipped = append(result.Skipped, path)
			continue
		}

		newBase := strings.ReplaceAll(base, from, to)
		newPath := filepath.Join(dir, newBase)

		// Security: verify the new path stays in the same directory
		newPath = filepath.Clean(newPath)
		if filepath.Dir(newPath) != filepath.Clean(dir) {
			result.Failed[path] = "path traversal detected: new path escapes original directory"
			continue
		}

		// Validate the destination too (.git protection on the new name).
		if _, vErr := t.validateWritePath(newPath); vErr != nil {
			result.Failed[path] = vErr.Error()
			continue
		}

		if dryRun {
			result.Succeeded = append(result.Succeeded, fmt.Sprintf("%s -> %s", path, newPath))
			continue
		}

		if err := os.Rename(path, newPath); err != nil {
			result.Failed[path] = err.Error()
		} else {
			result.Succeeded = append(result.Succeeded, fmt.Sprintf("%s -> %s", path, newPath))
			result.RawPaths = append(result.RawPaths, path, newPath)

			// Record for undo. Tag as "move" so undo/redo use revertMove/applyMove
			// (os.Rename back), NOT the generic AtomicWrite path — which would write
			// the destination-path STRING as the source file's content and orphan
			// the renamed file. Mirrors move.go's reversible encoding: FilePath = the
			// current location (newPath), OldContent = the original source path.
			if t.undoManager != nil {
				change := undo.NewFileChange(newPath, "move", []byte(path), nil, false)
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

		validPath, vErr := t.validateWritePath(path)
		if vErr != nil {
			result.Failed[path] = vErr.Error()
			continue
		}
		path = validPath

		// Read content for undo before deletion. Read errors degrade silently
		// (undo just isn't recorded for this file) so a single unreadable file
		// can't abort a batch — but log so the user has a clue why /undo "skips".
		var oldContent []byte
		var oldMode os.FileMode
		oversized := false
		if !dryRun && t.undoManager != nil {
			info, stErr := os.Stat(path)
			if stErr == nil {
				oldMode = info.Mode().Perm()
			}
			// The pre-read exists only to make the deletion undoable, and a
			// batch multiplies it across every matched file — an unbounded read
			// here can pin a whole directory of large files in memory at once.
			if stErr == nil && undoSnapshotTooLarge(info.Size()) {
				oversized = true
				logging.Warn("batch_delete: undo unavailable, file exceeds the undo snapshot limit",
					"path", path, "size", info.Size(), "limit", maxUndoSnapshotBytes)
			} else {
				var readErr error
				oldContent, readErr = os.ReadFile(path)
				if readErr != nil {
					logging.Warn("batch_delete: undo unavailable, pre-delete read failed",
						"path", path, "error", readErr)
				}
			}
		}

		if dryRun {
			result.Succeeded = append(result.Succeeded, path)
			continue
		}

		if err := os.Remove(path); err != nil {
			result.Failed[path] = err.Error()
		} else {
			result.Succeeded = append(result.Succeeded, path)
			result.RawPaths = append(result.RawPaths, path)
			if oversized {
				result.Unsnapshotted = append(result.Unsnapshotted, path)
			}

			// Record for undo (nil means pre-read failed; []byte{} is a valid empty file).
			if t.undoManager != nil && oldContent != nil {
				change := undo.NewFileChange(path, "batch_delete", oldContent, nil, false)
				change.Mode = oldMode
				t.undoManager.Record(*change)
			}
		}
	}

	return result
}

// executeParallel runs operations in parallel with progress callbacks and failure threshold.
func (t *BatchTool) executeParallel(ctx context.Context, files []string, operation func(string) error) BatchResult {
	result := BatchResult{
		TotalFiles: len(files),
		Failed:     make(map[string]string),
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var processedCount int
	var failureCount int
	var shouldStop bool

	// Limit concurrency
	semaphore := make(chan struct{}, 10)

	for _, path := range files {
		// Check if we should stop due to failure threshold
		mu.Lock()
		if shouldStop {
			result.Failed[path] = "stopped due to failure threshold"
			mu.Unlock()
			continue
		}
		mu.Unlock()

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
			defer func() {
				if rec := recover(); rec != nil {
					logging.Error("batch operation panic",
						"path", p,
						"panic", rec,
						"stack", logging.PanicStack())
					mu.Lock()
					result.Failed[p] = fmt.Sprintf("internal panic: %v", rec)
					mu.Unlock()
				}
			}()

			// Check if context is already cancelled or should stop
			mu.Lock()
			if shouldStop {
				result.Failed[p] = "stopped due to failure threshold"
				mu.Unlock()
				return
			}
			mu.Unlock()

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
			processedCount++
			success := true

			if err != nil {
				success = false
				if strings.Contains(err.Error(), "not found") {
					result.Skipped = append(result.Skipped, p)
				} else {
					result.Failed[p] = err.Error()
					failureCount++

					// Check failure threshold
					if t.failureThreshold > 0 && processedCount >= 3 {
						currentFailureRate := float64(failureCount) / float64(processedCount)
						if currentFailureRate > t.failureThreshold {
							shouldStop = true
							result.Failed["_threshold"] = fmt.Sprintf("failure rate %.1f%% exceeded threshold %.1f%%",
								currentFailureRate*100, t.failureThreshold*100)
						}
					}
				}
			} else {
				result.Succeeded = append(result.Succeeded, p)
			}

			// Call progress callback
			if t.progressCallback != nil {
				t.progressCallback(processedCount, len(files), p, success)
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

	fmt.Fprintf(&sb, "%sBatch %s: %s\n\n", prefix, op, result.Description)

	// Summary
	fmt.Fprintf(&sb, "Total: %d files\n", result.TotalFiles)
	fmt.Fprintf(&sb, "Succeeded: %d\n", len(result.Succeeded))
	if len(result.Skipped) > 0 {
		fmt.Fprintf(&sb, "Skipped: %d\n", len(result.Skipped))
	}
	if len(result.Failed) > 0 {
		fmt.Fprintf(&sb, "Failed: %d\n", len(result.Failed))
	}
	if len(result.Unsnapshotted) > 0 {
		fmt.Fprintf(&sb, "Not undoable: %d (too large to snapshot for undo)\n", len(result.Unsnapshotted))
	}

	// Details for small result sets
	if len(result.Succeeded) > 0 && len(result.Succeeded) <= 10 {
		sb.WriteString("\nSucceeded:\n")
		for _, path := range result.Succeeded {
			fmt.Fprintf(&sb, "  ✓ %s\n", filepath.Base(path))
		}
	}

	if len(result.Failed) > 0 {
		sb.WriteString("\nFailed:\n")
		for path, err := range result.Failed {
			fmt.Fprintf(&sb, "  ✗ %s: %s\n", filepath.Base(path), err)
		}
	}

	if len(result.Failed) > 0 {
		if len(result.Succeeded) == 0 {
			// Nothing succeeded — every file that wasn't skipped errored, so
			// this is a total failure, not a partial success. The "success"
			// class of tool must never report Success:true on an all-error
			// path (the same honesty fix already applied to refactor's
			// executeRename) — a model trusting the top-level success flag
			// would believe a total-failure batch delete/rename/replace
			// actually changed something.
			return NewErrorResult(sb.String())
		}
		data := map[string]any{
			"succeeded": len(result.Succeeded),
			"failed":    len(result.Failed),
			"skipped":   len(result.Skipped),
			"total":     result.TotalFiles,
		}
		// Declare the ACTUALLY-changed files so the done-gate records them as
		// touched (batch's targets are a "files" list, not a scalar arg) and
		// the executor invalidates read-dedup/caches. See "Adding a New Tool"
		// step 14. Dry runs change nothing, so RawPaths is empty then.
		if len(result.RawPaths) > 0 {
			data["written_paths"] = uniqueStrings(result.RawPaths)
		}
		return NewSuccessResultWithData(sb.String(), data)
	}

	if len(result.RawPaths) > 0 {
		return NewSuccessResultWithData(sb.String(), map[string]any{
			"written_paths": uniqueStrings(result.RawPaths),
		})
	}
	return NewSuccessResult(sb.String())
}

// uniqueStrings returns the input with duplicates removed, order preserved.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
