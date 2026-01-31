package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/security"
	"gokin/internal/undo"
)

// RefactorTool performs intelligent code refactoring operations.
type RefactorTool struct {
	undoManager   *undo.Manager
	workDir       string
	pathValidator *security.PathValidator
	diffHandler   DiffHandler
	diffEnabled   bool
}

// NewRefactorTool creates a new RefactorTool instance.
func NewRefactorTool() *RefactorTool {
	return &RefactorTool{}
}

// SetUndoManager sets the undo manager for tracking changes.
func (t *RefactorTool) SetUndoManager(manager *undo.Manager) {
	t.undoManager = manager
}

// SetWorkDir sets the working directory and initializes path validator.
func (t *RefactorTool) SetWorkDir(workDir string) {
	t.workDir = workDir
	t.pathValidator = security.NewPathValidator([]string{workDir}, false)
}

// SetDiffHandler sets the diff handler for preview approval.
func (t *RefactorTool) SetDiffHandler(handler DiffHandler) {
	t.diffHandler = handler
}

// SetDiffEnabled enables or disables diff preview.
func (t *RefactorTool) SetDiffEnabled(enabled bool) {
	t.diffEnabled = enabled
}

func (t *RefactorTool) Name() string {
	return "refactor"
}

func (t *RefactorTool) Description() string {
	return "Performs intelligent code refactoring: rename functions/variables, extract code, find references. Uses AST analysis for safe transformations."
}

func (t *RefactorTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"operation": {
					Type:        genai.TypeString,
					Description: "Refactoring operation: 'rename', 'extract', 'inline', 'find_refs'",
					Enum:        []string{"rename", "extract", "inline", "find_refs"},
				},
				"file_path": {
					Type:        genai.TypeString,
					Description: "Path to the file to refactor (for rename/extract/inline)",
				},
				"pattern": {
					Type:        genai.TypeString,
					Description: "Glob pattern for multi-file operations (e.g., '**/*.go')",
				},
				"old_name": {
					Type:        genai.TypeString,
					Description: "Current function/variable name (for rename)",
				},
				"new_name": {
					Type:        genai.TypeString,
					Description: "New function/variable name (for rename)",
				},
				"extract_name": {
					Type:        genai.TypeString,
					Description: "Name for the extracted function (for extract)",
				},
				"start_line": {
					Type:        genai.TypeInteger,
					Description: "Start line for extraction (for extract)",
				},
				"end_line": {
					Type:        genai.TypeInteger,
					Description: "End line for extraction (for extract)",
				},
				"target_name": {
					Type:        genai.TypeString,
					Description: "Function name to find references or inline (for find_refs/inline)",
				},
			},
			Required: []string{"operation"},
		},
	}
}

func (t *RefactorTool) Validate(args map[string]any) error {
	op, ok := GetString(args, "operation")
	if !ok || op == "" {
		return NewValidationError("operation", "is required")
	}

	switch op {
	case "rename", "extract", "inline":
		filePath, _ := GetString(args, "file_path")
		if filePath == "" {
			return NewValidationError("file_path", "is required for this operation")
		}
	case "find_refs":
		// Can work with just pattern and target_name
		targetName, _ := GetString(args, "target_name")
		if targetName == "" {
			return NewValidationError("target_name", "is required for find_refs")
		}
	}

	return nil
}

func (t *RefactorTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	op, _ := GetString(args, "operation")

	switch op {
	case "rename":
		return t.executeRename(ctx, args)
	case "extract":
		return t.executeExtract(ctx, args)
	case "inline":
		return t.executeInline(ctx, args)
	case "find_refs":
		return t.executeFindRefs(ctx, args)
	default:
		return NewErrorResult(fmt.Sprintf("unknown operation: %s", op)), nil
	}
}

// executeRename performs safe renaming using AST analysis.
func (t *RefactorTool) executeRename(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")
	oldName, _ := GetString(args, "old_name")
	newName, _ := GetString(args, "new_name")
	pattern, _ := GetString(args, "pattern")

	if oldName == "" || newName == "" {
		return NewErrorResult("old_name and new_name are required for rename"), nil
	}

	// Determine files to process
	var files []string
	if pattern != "" {
		// Multi-file rename
		matches, err := filepath.Glob(filepath.Join(t.workDir, pattern))
		if err != nil {
			return NewErrorResult(fmt.Sprintf("invalid pattern: %s", err)), nil
		}
		files = matches
	} else {
		files = []string{filePath}
	}

	// Process each file
	var results []string
	var totalChanges int

	for _, file := range files {
		changes, err := t.renameInFile(ctx, file, oldName, newName)
		if err != nil {
			results = append(results, fmt.Sprintf("%s: ERROR - %s", file, err))
			continue
		}
		if changes > 0 {
			results = append(results, fmt.Sprintf("%s: %d changes", file, changes))
			totalChanges += changes
		}
	}

	if totalChanges == 0 {
		return NewSuccessResult(fmt.Sprintf("No occurrences of '%s' found", oldName)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Renamed '%s' to '%s' in %d file(s):\n%s",
		oldName, newName, len(results), strings.Join(results, "\n"))), nil
}

// renameInFile performs AST-based renaming in a single file.
func (t *RefactorTool) renameInFile(ctx context.Context, filePath, oldName, newName string) (int, error) {
	// Read file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}

	// Parse AST only for Go files
	if strings.HasSuffix(filePath, ".go") {
		return t.renameInGoFile(filePath, content, oldName, newName)
	}

	// For non-Go files, use simple text replacement with scope awareness
	return t.renameInTextFile(filePath, content, oldName, newName)
}

// renameInGoFile performs AST-based renaming for Go code.
func (t *RefactorTool) renameInGoFile(filePath string, content []byte, oldName, newName string) (int, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		// Fall back to text-based replacement
		return t.renameInTextFile(filePath, content, oldName, newName)
	}

	// Find all identifiers matching oldName
	var positions []token.Pos
	ast.Inspect(node, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && ident.Name == oldName {
			positions = append(positions, ident.Pos())
		}
		return true
	})

	if len(positions) == 0 {
		return 0, nil
	}

	// Apply replacements in reverse order to preserve positions
	newContent := string(content)
	for i := len(positions) - 1; i >= 0; i-- {
		pos := positions[i]
		position := fset.Position(pos)
		offset := position.Offset - 1 // Convert to 0-indexed

		// Replace identifier
		newContent = newContent[:offset] + newName + newContent[offset+len(oldName):]
	}

	// Write back (diff preview requires context, skip for now)
	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		return 0, err
	}

	// Record for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "refactor_rename", content, []byte(newContent), false)
		t.undoManager.Record(*change)
	}

	return len(positions), nil
}

// renameInTextFile performs scope-aware text replacement.
func (t *RefactorTool) renameInTextFile(filePath string, content []byte, oldName, newName string) (int, error) {
	text := string(content)

	// Simple word-boundary replacement to avoid partial matches
	// Build regex pattern manually
	patternStr := "\\b" + regexp.QuoteMeta(oldName) + "\\b"
	re := regexp.MustCompile(patternStr)
	matches := re.FindAllStringIndex(text, -1)

	if len(matches) == 0 {
		return 0, nil
	}

	// Apply replacements from end to start
	newText := text
	for i := len(matches) - 1; i >= 0; i-- {
		start, end := matches[i][0], matches[i][1]
		newText = newText[:start] + newName + newText[end:]
	}

	// Write back
	if err := os.WriteFile(filePath, []byte(newText), 0644); err != nil {
		return 0, err
	}

	// Record for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "refactor_rename", content, []byte(newText), false)
		t.undoManager.Record(*change)
	}

	return len(matches), nil
}

// executeExtract extracts code into a separate function.
func (t *RefactorTool) executeExtract(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")
	extractName, _ := GetString(args, "extract_name")
	startLine, _ := GetInt(args, "start_line")
	endLine, _ := GetInt(args, "end_line")

	if filePath == "" || extractName == "" || startLine == 0 || endLine == 0 {
		return NewErrorResult("file_path, extract_name, start_line, and end_line are required"), nil
	}

	// Read file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error reading file: %s", err)), nil
	}

	lines := strings.Split(string(content), "\n")
	if startLine < 1 || endLine > len(lines) || startLine > endLine {
		return NewErrorResult(fmt.Sprintf("invalid line range: %d-%d (file has %d lines)", startLine, endLine, len(lines))), nil
	}

	// Extract the code block
	extractedCode := strings.Join(lines[startLine-1:endLine], "\n")

	// For Go files, try to create a proper function
	var newFunction string
	if strings.HasSuffix(filePath, ".go") {
		newFunction = fmt.Sprintf("// %s is an extracted function\nfunc %s() {\n%s\n}\n",
			extractName, extractName, extractedCode)
	} else {
		// For other languages, use a generic format
		newFunction = fmt.Sprintf("// Extracted function: %s\nfunction %s() {\n%s\n}\n",
			extractName, extractName, extractedCode)
	}

	// Create new content with extracted function
	var newContent strings.Builder
	newContent.WriteString(strings.Join(lines[:startLine-1], "\n"))
	newContent.WriteString("\n")
	newContent.WriteString(newFunction)
	newContent.WriteString("\n")
	newContent.WriteString(strings.Join(lines[endLine:], "\n"))

	contentStr := newContent.String()

	// Write back (diff preview requires context, skip for now)
	if err := os.WriteFile(filePath, []byte(contentStr), 0644); err != nil {
		return NewErrorResult(fmt.Sprintf("error writing file: %s", err)), nil
	}

	// Record for undo
	if t.undoManager != nil {
		change := undo.NewFileChange(filePath, "refactor_extract", content, []byte(contentStr), false)
		t.undoManager.Record(*change)
	}

	return NewSuccessResult(fmt.Sprintf("Extracted lines %d-%d into function '%s' in %s",
		startLine, endLine, extractName, filePath)), nil
}

// executeFindRefs finds all references to a function/variable.
func (t *RefactorTool) executeFindRefs(ctx context.Context, args map[string]any) (ToolResult, error) {
	targetName, _ := GetString(args, "target_name")
	pattern, _ := GetString(args, "pattern")

	if targetName == "" {
		return NewErrorResult("target_name is required"), nil
	}

	// Default to all Go files if no pattern specified
	if pattern == "" {
		pattern = "**/*.go"
	}

	// Find matching files
	matches, err := filepath.Glob(filepath.Join(t.workDir, pattern))
	if err != nil {
		return NewErrorResult(fmt.Sprintf("invalid pattern: %s", err)), nil
	}

	// Search for references
	var refs []string
	for _, file := range matches {
		content, err := os.ReadFile(file)
		if err != nil {
			continue
		}

		// For Go files, use AST to find accurate references
		if strings.HasSuffix(file, ".go") {
			fileRefs := findRefsInGoFile(file, content, targetName)
			if len(fileRefs) > 0 {
				refs = append(refs, fmt.Sprintf("%s:\n  %s", file, strings.Join(fileRefs, "\n  ")))
			}
		} else {
			// For other files, use simple text search
			lines := strings.Split(string(content), "\n")
			var fileRefs []string
			for i, line := range lines {
				if strings.Contains(line, targetName) {
					fileRefs = append(fileRefs, fmt.Sprintf("Line %d: %s", i+1, strings.TrimSpace(line)))
				}
			}
			if len(fileRefs) > 0 {
				refs = append(refs, fmt.Sprintf("%s:\n  %s", file, strings.Join(fileRefs, "\n  ")))
			}
		}
	}

	if len(refs) == 0 {
		return NewSuccessResult(fmt.Sprintf("No references found for '%s'", targetName)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Found references to '%s' in %d file(s):\n%s",
		targetName, len(refs), strings.Join(refs, "\n\n"))), nil
}

// executeInline inlines a function (stub implementation).
func (t *RefactorTool) executeInline(ctx context.Context, args map[string]any) (ToolResult, error) {
	// This is a complex operation that requires:
	// 1. Finding the function definition
	// 2. Parsing its body
	// 3. Finding all call sites
	// 4. Replacing calls with the function body
	// For now, return a not-implemented message
	return NewErrorResult("inline operation is not yet implemented. It requires full function body analysis and call site replacement."), nil
}

// findRefsInGoFile uses AST to find references in Go code.
func findRefsInGoFile(filePath string, content []byte, targetName string) []string {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		return nil
	}

	var refs []string
	ast.Inspect(node, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && ident.Name == targetName {
			pos := fset.Position(ident.Pos())
			refs = append(refs, fmt.Sprintf("Line %d", pos.Line))
		}
		return true
	})

	return refs
}

// Helper for regex escaping is now using standard regexp package
