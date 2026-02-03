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

// EditTool performs search/replace operations in files.
type EditTool struct {
	undoManager   *undo.Manager
	diffHandler   DiffHandler
	diffEnabled   bool
	workDir       string
	pathValidator *security.PathValidator
}

// NewEditTool creates a new EditTool instance.
func NewEditTool() *EditTool {
	return &EditTool{}
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
	return "Performs string replacement in a file. The old_string must be unique in the file unless replace_all is true. Use regex=true to treat old_string as a regular expression."
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
			},
			Required: []string{"file_path", "old_string", "new_string"},
		},
	}
}

func (t *EditTool) Validate(args map[string]any) error {
	filePath, ok := GetString(args, "file_path")
	if !ok || filePath == "" {
		return NewValidationError("file_path", "is required")
	}

	oldStr, ok := GetString(args, "old_string")
	if !ok || oldStr == "" {
		return NewValidationError("old_string", "is required")
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
			return NewErrorResult(fmt.Sprintf("regex pattern not found in file: %s", filePath)), nil
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
			}
		}
	} else {
		// Literal mode (existing behavior)
		count = strings.Count(content, oldStr)

		if count == 0 {
			return NewErrorResult(fmt.Sprintf("old_string not found in file: %s", filePath)), nil
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
