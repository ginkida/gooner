package tools

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/security"
	"gokin/internal/undo"
)

const editContextMaxChars = 5000

// EditTool performs search/replace operations in files.
type EditTool struct {
	undoManager   *undo.Manager
	diffHandler   DiffHandler
	diffEnabled   bool
	workDir       string
	pathValidator *security.PathValidator
}

// NewEditTool creates a new EditTool instance.
func NewEditTool(workDir string) *EditTool {
	t := &EditTool{
		workDir: workDir,
	}
	if workDir != "" {
		t.pathValidator = security.NewPathValidator([]string{workDir}, false)
	}
	return t
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *EditTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetDiffHandler sets the diff handler for preview approval.
func (t *EditTool) SetDiffHandler(handler DiffHandler) {
	t.diffHandler = handler
}

// SetDiffEnabled enables or disables diff preview.
func (t *EditTool) SetDiffEnabled(enabled bool) {
	t.diffEnabled = enabled
}

// SetWorkDir sets the working directory and initializes path validator.
func (t *EditTool) SetWorkDir(workDir string) {
	t.workDir = workDir
	t.pathValidator = security.NewPathValidator([]string{workDir}, false)
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *EditTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *EditTool) Name() string {
	return "edit"
}

func (t *EditTool) Description() string {
	return "Performs string replacement in a file. Supports three modes: (1) old_string/new_string for exact match replacement, (2) regex=true for regex replacement, (3) line_start/line_end/new_string for line-based replacement."
}

func (t *EditTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The absolute path to the file to edit",
				},
				"old_string": {
					Type:        genai.TypeString,
					Description: "The text to find and replace",
				},
				"new_string": {
					Type:        genai.TypeString,
					Description: "The text to replace with (must be different from old_string)",
				},
				"replace_all": {
					Type:        genai.TypeBoolean,
					Description: "If true, replace all occurrences. If false (default), old_string must be unique.",
				},
				"regex": {
					Type:        genai.TypeBoolean,
					Description: "If true, treat old_string as a regular expression pattern.",
				},
				"line_start": {
					Type:        genai.TypeInteger,
					Description: "Start line (1-indexed). Alternative to old_string: replaces lines line_start..line_end with new_string.",
				},
				"line_end": {
					Type:        genai.TypeInteger,
					Description: "End line (1-indexed, inclusive). Used with line_start.",
				},
				"edits": {
					Type:        genai.TypeArray,
					Description: "Array of {old_string, new_string} pairs for multiple edits in one call. Each edit is applied sequentially to the result of the previous one.",
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"old_string": {
								Type:        genai.TypeString,
								Description: "The text to find",
							},
							"new_string": {
								Type:        genai.TypeString,
								Description: "The text to replace with",
							},
						},
						Required: []string{"old_string", "new_string"},
					},
				},
			},
			Required: []string{"file_path"},
		},
	}
}

func (t *EditTool) Validate(args map[string]any) error {
	filePath, ok := GetString(args, "file_path")
	if !ok || filePath == "" {
		return NewValidationError("file_path", "is required")
	}
	_ = filePath

	// Multi-edit mode: edits array takes precedence
	if edits, ok := args["edits"].([]any); ok && len(edits) > 0 {
		for i, e := range edits {
			editMap, ok := e.(map[string]any)
			if !ok {
				return NewValidationError("edits", fmt.Sprintf("edit[%d] is not an object", i))
			}
			oldStr, _ := editMap["old_string"].(string)
			newStr, _ := editMap["new_string"].(string)
			if oldStr == "" {
				return NewValidationError("edits", fmt.Sprintf("edit[%d].old_string is required", i))
			}
			if oldStr == newStr {
				return NewValidationError("edits", fmt.Sprintf("edit[%d]: new_string must differ from old_string", i))
			}
		}
		return nil
	}

	// Line-based edit mode
	if lineStart, hasStart := GetInt(args, "line_start"); hasStart && lineStart > 0 {
		if _, hasEnd := GetInt(args, "line_end"); !hasEnd {
			return NewValidationError("line_end", "required when line_start is provided")
		}
		if _, ok := GetString(args, "new_string"); !ok {
			return NewValidationError("new_string", "required for line-based editing")
		}
		return nil
	}

	// Single edit mode
	oldStr, ok := GetString(args, "old_string")
	if !ok || oldStr == "" {
		return NewValidationError("old_string", "is required (or provide edits array or line_start/line_end)")
	}

	newStr, ok := GetString(args, "new_string")
	if !ok {
		return NewValidationError("new_string", "is required")
	}

	if oldStr == newStr {
		return NewValidationError("new_string", "must be different from old_string")
	}

	return nil
}

func (t *EditTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")

	// Check for multi-edit mode
	if edits, ok := args["edits"].([]any); ok && len(edits) > 0 {
		return t.executeMultiEdit(ctx, filePath, edits)
	}

	// Check for line-based edit mode
	if lineStart, hasStart := GetInt(args, "line_start"); hasStart && lineStart > 0 {
		lineEnd := GetIntDefault(args, "line_end", lineStart)
		newStr, _ := GetString(args, "new_string")
		return t.executeLineEdit(ctx, filePath, lineStart, lineEnd, newStr)
	}

	oldStr, _ := GetString(args, "old_string")
	newStr, _ := GetString(args, "new_string")
	replaceAll := GetBoolDefault(args, "replace_all", false)

	// Validate path (mandatory for security)
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}

	validPath, err := t.pathValidator.ValidateFile(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
	}
	filePath = validPath

	// Read existing file
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return NewErrorResult(fmt.Sprintf("error reading file: %s", err)), nil
	}

	// Detect binary files by checking for null bytes in the first 512 bytes
	checkLen := len(data)
	if checkLen > 512 {
		checkLen = 512
	}
	for _, b := range data[:checkLen] {
		if b == 0 {
			return NewErrorResult(fmt.Sprintf("cannot edit binary file: %s", filePath)), nil
		}
	}

	content := string(data)
	oldContent := data // Save for undo
	useRegex := GetBoolDefault(args, "regex", false)

	var newContent string
	var count int

	if useRegex {
		// Regex mode
		re, err := regexp.Compile(oldStr)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("invalid regex pattern: %s", err)), nil
		}

		// Count matches
		matches := re.FindAllStringIndex(content, -1)
		count = len(matches)

		if count == 0 {
			errMsg := fmt.Sprintf("regex pattern not found in file: %s", filePath)
			fileCtx := extractFileContext(content, editContextMaxChars)
			return NewErrorResultWithContext(errMsg, fileCtx), nil
		}

		if count > 1 && !replaceAll {
			// Find line numbers of matches for a more helpful error
			lines := strings.Split(content, "\n")
			var lineNums []string
			pos := 0
			for i, line := range lines {
				lineEnd := pos + len(line)
				for _, match := range matches {
					if match[0] >= pos && match[0] < lineEnd {
						lineNums = append(lineNums, fmt.Sprintf("%d", i+1))
						break
					}
				}
				pos = lineEnd + 1 // +1 for newline
			}
			lineInfo := ""
			if len(lineNums) > 0 {
				lineInfo = fmt.Sprintf(" (lines: %s)", strings.Join(lineNums, ", "))
			}
			return NewErrorResult(fmt.Sprintf("regex pattern matches %d times in %s%s. Set replace_all=true to replace all.", count, filePath, lineInfo)), nil
		}

		// Perform regex replacement
		if replaceAll {
			newContent = re.ReplaceAllString(content, newStr)
		} else {
			// Replace first match only
			loc := re.FindStringIndex(content)
			if loc != nil {
				newContent = content[:loc[0]] + re.ReplaceAllString(content[loc[0]:loc[1]], newStr) + content[loc[1]:]
			} else {
				newContent = content // Safety fallback: no match, no change
			}
		}
	} else {
		// Literal mode (existing behavior)
		count = strings.Count(content, oldStr)

		if count == 0 {
			errMsg := fmt.Sprintf("old_string not found in file: %s", filePath)
			if actual, line := findFuzzyMatch(content, oldStr); actual != "" {
				errMsg += fmt.Sprintf("\n\nFuzzy match at line %d (whitespace differs). Actual text:\n```\n%s\n```\nUse this exact text as old_string.", line, actual)
			}
			fileCtx := extractFileContext(content, editContextMaxChars)
			return NewErrorResultWithContext(errMsg, fileCtx), nil
		}

		if count > 1 && !replaceAll {
			// Find line numbers of occurrences for a more helpful error
			lines := strings.Split(content, "\n")
			var lineNums []string
			for i, line := range lines {
				if strings.Contains(line, oldStr) {
					lineNums = append(lineNums, fmt.Sprintf("%d", i+1))
				}
			}
			lineInfo := ""
			if len(lineNums) > 0 {
				lineInfo = fmt.Sprintf(" (lines: %s)", strings.Join(lineNums, ", "))
			}
			return NewErrorResult(fmt.Sprintf("old_string appears %d times in %s%s. Provide more surrounding context to make it unique, or set replace_all=true.", count, filePath, lineInfo)), nil
		}

		// Perform replacement
		if replaceAll {
			newContent = strings.ReplaceAll(content, oldStr, newStr)
		} else {
			newContent = strings.Replace(content, oldStr, newStr, 1)
		}
	}

	// Show diff preview and wait for approval if enabled
	// Skip diff approval when running in delegated plan execution (context flag)
	if t.diffEnabled && t.diffHandler != nil && !ShouldSkipDiff(ctx) {
		approved, err := t.diffHandler.PromptDiff(ctx, filePath, content, newContent, "edit", false)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("diff preview error: %s", err)), nil
		}
		if !approved {
			return NewErrorResult("changes rejected by user"), nil
		}
	}

	// Write back atomically to prevent data corruption on interruption
	newContentBytes := []byte(newContent)
	if err := AtomicWrite(filePath, newContentBytes, 0644); err != nil {
		return NewErrorResult(fmt.Sprintf("error writing file: %s", err)), nil
	}

	// Record change for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "edit", oldContent, newContentBytes, false)
		t.undoManager.Record(*change)
	}

	var status string
	if replaceAll {
		status = fmt.Sprintf("Replaced %d occurrence(s) in %s", count, filePath)
	} else {
		status = fmt.Sprintf("Replaced 1 occurrence in %s", filePath)
	}

	return NewSuccessResult(status), nil
}

// executeMultiEdit applies multiple edits to a single file sequentially.
// Each edit operates on the result of the previous one.
func (t *EditTool) executeMultiEdit(ctx context.Context, filePath string, edits []any) (ToolResult, error) {
	// Validate path
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}
	validPath, err := t.pathValidator.ValidateFile(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
	}
	filePath = validPath

	// Read file
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return NewErrorResult(fmt.Sprintf("error reading file: %s", err)), nil
	}

	content := string(data)
	oldContent := data
	totalReplacements := 0

	// Apply each edit sequentially
	for i, e := range edits {
		editMap, ok := e.(map[string]any)
		if !ok {
			return NewErrorResult(fmt.Sprintf("edit[%d] is not an object", i)), nil
		}

		oldStr, ok1 := editMap["old_string"].(string)
		newStr, ok2 := editMap["new_string"].(string)
		if !ok1 || oldStr == "" {
			return NewErrorResult(fmt.Sprintf("edit[%d]: old_string is required and must be a non-empty string", i)), nil
		}
		if !ok2 {
			newStr = "" // Allow deletion (replace with nothing)
		}

		count := strings.Count(content, oldStr)
		if count == 0 {
			errMsg := fmt.Sprintf("edit[%d]: old_string not found in file after previous edits", i)
			if actual, line := findFuzzyMatch(content, oldStr); actual != "" {
				errMsg += fmt.Sprintf("\n\nFuzzy match at line %d. Actual text:\n```\n%s\n```", line, actual)
			}
			fileCtx := extractFileContext(content, editContextMaxChars)
			return NewErrorResultWithContext(errMsg, fileCtx), nil
		}

		content = strings.Replace(content, oldStr, newStr, 1)
		totalReplacements++
	}

	// Show combined diff preview
	if t.diffEnabled && t.diffHandler != nil && !ShouldSkipDiff(ctx) {
		approved, err := t.diffHandler.PromptDiff(ctx, filePath, string(oldContent), content, "edit", false)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("diff preview error: %s", err)), nil
		}
		if !approved {
			return NewErrorResult("changes rejected by user"), nil
		}
	}

	// Write atomically
	newContentBytes := []byte(content)
	if err := AtomicWrite(filePath, newContentBytes, 0644); err != nil {
		return NewErrorResult(fmt.Sprintf("error writing file: %s", err)), nil
	}

	// Record single undo for all edits
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "edit", oldContent, newContentBytes, false)
		t.undoManager.Record(*change)
	}

	return NewSuccessResult(fmt.Sprintf("Applied %d edit(s) to %s", totalReplacements, filePath)), nil
}

// executeLineEdit replaces a range of lines in a file.
func (t *EditTool) executeLineEdit(ctx context.Context, filePath string, lineStart, lineEnd int, newStr string) (ToolResult, error) {
	// Validate path
	if t.pathValidator == nil {
		return NewErrorResult("security error: path validator not initialized"), nil
	}
	validPath, err := t.pathValidator.ValidateFile(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
	}
	filePath = validPath

	// Read file
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return NewErrorResult(fmt.Sprintf("error reading file: %s", err)), nil
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	// Validate range
	if lineStart < 1 {
		return NewErrorResult("line_start must be >= 1"), nil
	}
	if lineEnd < lineStart {
		return NewErrorResult(fmt.Sprintf("line_end (%d) must be >= line_start (%d)", lineEnd, lineStart)), nil
	}
	if lineStart > totalLines {
		errMsg := fmt.Sprintf("line_start (%d) exceeds file length (%d lines)", lineStart, totalLines)
		fileCtx := extractFileContext(content, editContextMaxChars)
		return NewErrorResultWithContext(errMsg, fileCtx), nil
	}

	// Clamp lineEnd to file length
	if lineEnd > totalLines {
		lineEnd = totalLines
	}

	// Build new content: lines before + new text + lines after
	var parts []string
	if lineStart > 1 {
		parts = append(parts, lines[:lineStart-1]...)
	}
	if newStr != "" {
		parts = append(parts, strings.Split(newStr, "\n")...)
	}
	if lineEnd < totalLines {
		parts = append(parts, lines[lineEnd:]...)
	}

	newContent := strings.Join(parts, "\n")

	// Show diff preview
	if t.diffEnabled && t.diffHandler != nil && !ShouldSkipDiff(ctx) {
		approved, err := t.diffHandler.PromptDiff(ctx, filePath, content, newContent, "edit", false)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("diff preview error: %s", err)), nil
		}
		if !approved {
			return NewErrorResult("changes rejected by user"), nil
		}
	}

	// Write atomically
	newContentBytes := []byte(newContent)
	if err := AtomicWrite(filePath, newContentBytes, 0644); err != nil {
		return NewErrorResult(fmt.Sprintf("error writing file: %s", err)), nil
	}

	// Record change for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "edit", data, newContentBytes, false)
		t.undoManager.Record(*change)
	}

	replacedCount := lineEnd - lineStart + 1
	return NewSuccessResult(fmt.Sprintf("Replaced lines %d-%d (%d lines) in %s", lineStart, lineEnd, replacedCount, filePath)), nil
}

// extractFileContext formats file content with line numbers for error context.
func extractFileContext(content string, maxChars int) string {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for i, line := range lines {
		s := fmt.Sprintf("%6d\t%s\n", i+1, line)
		if b.Len()+len(s) > maxChars {
			b.WriteString(fmt.Sprintf("... (showing %d of %d lines)", i, len(lines)))
			break
		}
		b.WriteString(s)
	}
	return b.String()
}

// findFuzzyMatch tries to find old_string in content after normalizing trailing whitespace.
// Returns the actual (unnormalized) text from the file and its starting line number.
// Returns ("", 0) if no unique normalized match is found.
func findFuzzyMatch(content, oldStr string) (string, int) {
	// Normalize both sides: trim trailing whitespace from each line
	normalizeLines := func(s string) string {
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimRight(line, " \t\r")
		}
		return strings.Join(lines, "\n")
	}

	normalizedOld := normalizeLines(oldStr)
	normalizedContent := normalizeLines(content)

	// If normalization doesn't change either string, whitespace isn't the issue
	if normalizedOld == oldStr && normalizedContent == content {
		return "", 0
	}

	// Count normalized matches
	count := strings.Count(normalizedContent, normalizedOld)
	if count != 1 {
		return "", 0
	}

	// Find position in normalized content
	normIdx := strings.Index(normalizedContent, normalizedOld)
	if normIdx < 0 {
		return "", 0
	}

	// Map normalized position back to original content.
	// The line number of the match start in normalized content
	// equals the line number in original content.
	normPrefix := normalizedContent[:normIdx]
	startLine := strings.Count(normPrefix, "\n")

	// Count how many lines the old_string spans
	oldLines := strings.Count(normalizedOld, "\n")
	endLine := startLine + oldLines

	// Extract the original lines
	contentLines := strings.Split(content, "\n")
	if startLine >= len(contentLines) {
		return "", 0
	}
	if endLine >= len(contentLines) {
		endLine = len(contentLines) - 1
	}

	actual := strings.Join(contentLines[startLine:endLine+1], "\n")
	return actual, startLine + 1 // 1-indexed line number
}
