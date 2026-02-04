package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/genai"
)

// VerifyCodeTool automatically checks code correctness.
type VerifyCodeTool struct {
	workDir string
}

// NewVerifyCodeTool creates a new VerifyCodeTool instance.
func NewVerifyCodeTool(workDir string) *VerifyCodeTool {
	return &VerifyCodeTool{
		workDir: workDir,
	}
}

func (t *VerifyCodeTool) Name() string {
	return "verify_code"
}

func (t *VerifyCodeTool) Description() string {
	return `Automatically verifies code correctness in the project.
Detects project type (Go, Node.js, Python) and runs relevant checks like build or lint.
Use this after making changes to ensure no regressions or syntax errors were introduced.`
}

func (t *VerifyCodeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The directory to verify (defaults to project root)",
				},
			},
		},
	}
}

func (t *VerifyCodeTool) Validate(args map[string]any) error {
	return nil
}

func (t *VerifyCodeTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	path, _ := GetString(args, "path")
	if path == "" {
		path = t.workDir
	}

	// 1. Detect project type
	projectType := t.detectProjectType(path)
	if projectType == "" {
		return NewErrorResult("Could not detect project type for verification. Supported: Go, Node.js, Python."), nil
	}

	// 2. Run verification command
	var cmd *exec.Cmd
	var checkName string

	switch projectType {
	case "go":
		checkName = "go build ./..."
		cmd = exec.CommandContext(ctx, "go", "build", "./...")
	case "node":
		if t.fileExists(filepath.Join(path, "package.json")) {
			// Try build first, then lint
			checkName = "npm run build"
			cmd = exec.CommandContext(ctx, "npm", "run", "build")
		}
	case "python":
		if t.fileExists(filepath.Join(path, "requirements.txt")) || t.fileExists(filepath.Join(path, "pyproject.toml")) {
			// Check syntax at least
			checkName = "python -m py_compile **/*.py"
			cmd = exec.CommandContext(ctx, "bash", "-c", "python3 -m py_compile $(find . -name '*.py')")
		}
	}

	if cmd == nil {
		return NewErrorResult(fmt.Sprintf("No verification command found for project type: %s", projectType)), nil
	}

	cmd.Dir = path
	output, err := cmd.CombinedOutput()

	if err != nil {
		return ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Verification failed (%s):\n%s", checkName, string(output)),
			Content: string(output),
		}, nil
	}

	return NewSuccessResult(fmt.Sprintf("Verification successful (%s):\n%s", checkName, string(output))), nil
}

func (t *VerifyCodeTool) detectProjectType(path string) string {
	if t.fileExists(filepath.Join(path, "go.mod")) {
		return "go"
	}
	if t.fileExists(filepath.Join(path, "package.json")) {
		return "node"
	}
	if t.fileExists(filepath.Join(path, "requirements.txt")) || t.fileExists(filepath.Join(path, "pyproject.toml")) || t.fileExists(filepath.Join(path, "setup.py")) {
		return "python"
	}

	// Fallback to searching for extensions
	files, _ := os.ReadDir(path)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".go") {
			return "go"
		}
		if strings.HasSuffix(f.Name(), ".js") || strings.HasSuffix(f.Name(), ".ts") {
			return "node"
		}
		if strings.HasSuffix(f.Name(), ".py") {
			return "python"
		}
	}

	return ""
}

func (t *VerifyCodeTool) fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
