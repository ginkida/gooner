package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"
)

// RunTestsTool runs project tests and parses results.
type RunTestsTool struct {
	workDir string
}

// NewRunTestsTool creates a new RunTestsTool instance.
func NewRunTestsTool(workDir string) *RunTestsTool {
	return &RunTestsTool{workDir: workDir}
}

func (t *RunTestsTool) Name() string { return "run_tests" }

func (t *RunTestsTool) Description() string {
	return "Runs project tests with automatic framework detection (Go, Python, Node, Rust). Parses output, reports failures with context, and provides coverage summary."
}

func (t *RunTestsTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "Path to run tests in (default: working directory). Can be a specific file or package.",
				},
				"filter": {
					Type:        genai.TypeString,
					Description: "Test name filter/pattern (e.g., 'TestMyFunc' for Go, '-k test_name' for pytest)",
				},
				"verbose": {
					Type:        genai.TypeBoolean,
					Description: "Show verbose test output (default: false)",
				},
				"coverage": {
					Type:        genai.TypeBoolean,
					Description: "Run with coverage reporting (default: false)",
				},
				"framework": {
					Type:        genai.TypeString,
					Description: "Force specific framework: 'go', 'pytest', 'jest', 'cargo', 'auto' (default: auto-detect)",
					Enum:        []string{"auto", "go", "pytest", "jest", "cargo"},
				},
			},
		},
	}
}

func (t *RunTestsTool) Validate(args map[string]any) error {
	return nil
}

func (t *RunTestsTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	testPath := GetStringDefault(args, "path", "")
	filter := GetStringDefault(args, "filter", "")
	verbose := GetBoolDefault(args, "verbose", false)
	coverage := GetBoolDefault(args, "coverage", false)
	framework := GetStringDefault(args, "framework", "auto")

	workDir := t.workDir
	if testPath != "" {
		if filepath.IsAbs(testPath) {
			workDir = testPath
		} else {
			workDir = filepath.Join(t.workDir, testPath)
		}
	}

	// Auto-detect framework
	if framework == "auto" {
		framework = detectTestFramework(workDir)
		if framework == "" {
			return NewErrorResult("could not detect test framework. Specify 'framework' parameter."), nil
		}
	}

	// Build command
	cmdName, cmdArgs := buildTestCommand(framework, workDir, filter, verbose, coverage)

	// Execute with timeout
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(testCtx, cmdName, cmdArgs...)
	cmd.Dir = workDir

	start := time.Now()
	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	outStr := string(output)

	// Parse results based on framework
	result := parseTestResults(framework, outStr, err, duration)

	return NewSuccessResult(result), nil
}

// detectTestFramework auto-detects the test framework from project files.
func detectTestFramework(dir string) string {
	checks := []struct {
		file      string
		framework string
	}{
		{"go.mod", "go"},
		{"Cargo.toml", "cargo"},
		{"package.json", "jest"},
		{"pytest.ini", "pytest"},
		{"setup.py", "pytest"},
		{"pyproject.toml", "pytest"},
		{"requirements.txt", "pytest"},
	}

	for _, check := range checks {
		if _, err := os.Stat(filepath.Join(dir, check.file)); err == nil {
			return check.framework
		}
	}

	// Walk up to find project root
	parent := filepath.Dir(dir)
	if parent != dir {
		return detectTestFramework(parent)
	}

	return ""
}

// buildTestCommand creates the test command for the given framework.
func buildTestCommand(framework, _ string, filter string, verbose, coverage bool) (string, []string) {
	switch framework {
	case "go":
		args := []string{"test"}
		if verbose {
			args = append(args, "-v")
		}
		if coverage {
			args = append(args, "-coverprofile=coverage.out")
		}
		args = append(args, "-json")
		if filter != "" {
			args = append(args, "-run", filter)
		}
		args = append(args, "./...")
		return "go", args

	case "pytest":
		args := []string{"-m", "pytest"}
		if verbose {
			args = append(args, "-v")
		}
		if coverage {
			args = append(args, "--cov", "--cov-report=term-missing")
		}
		if filter != "" {
			args = append(args, "-k", filter)
		}
		args = append(args, "--tb=short", "--no-header", "-q")
		return "python3", args

	case "jest":
		args := []string{"test"}
		if verbose {
			args = append(args, "--verbose")
		}
		if coverage {
			args = append(args, "--coverage")
		}
		if filter != "" {
			args = append(args, "--testNamePattern", filter)
		}
		args = append(args, "--forceExit", "--no-color")
		// Use npx if npm test not available
		return "npx", append([]string{"jest"}, args[1:]...)

	case "cargo":
		args := []string{"test"}
		if !verbose {
			args = append(args, "--quiet")
		}
		if filter != "" {
			args = append(args, filter)
		}
		args = append(args, "--", "--format", "json")
		return "cargo", args

	default:
		return "echo", []string{"unknown framework: " + framework}
	}
}

// goTestEvent represents a single Go test JSON event.
type goTestEvent struct {
	Time    string  `json:"Time"`
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// parseTestResults parses test output and generates a structured report.
func parseTestResults(framework, output string, execErr error, duration time.Duration) string {
	switch framework {
	case "go":
		return parseGoTestResults(output, execErr, duration)
	default:
		return parseGenericTestResults(output, execErr, duration)
	}
}

// parseGoTestResults parses Go's JSON test output.
func parseGoTestResults(output string, execErr error, duration time.Duration) string {
	var (
		passed    int
		failed    int
		skipped   int
		failures  []string
		packages  = make(map[string]string) // package -> status
		failOutput = make(map[string][]string) // test -> output lines
	)

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		var event goTestEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Action {
		case "pass":
			if event.Test != "" {
				passed++
			} else {
				packages[event.Package] = "pass"
			}
		case "fail":
			if event.Test != "" {
				failed++
				key := event.Package + "/" + event.Test
				failures = append(failures, key)
			} else {
				packages[event.Package] = "fail"
			}
		case "skip":
			if event.Test != "" {
				skipped++
			}
		case "output":
			if event.Test != "" {
				key := event.Package + "/" + event.Test
				failOutput[key] = append(failOutput[key], strings.TrimRight(event.Output, "\n"))
			}
		}
	}

	// If JSON parsing failed, fall back to generic
	if passed == 0 && failed == 0 && skipped == 0 {
		return parseGenericTestResults(output, execErr, duration)
	}

	var result strings.Builder
	total := passed + failed + skipped

	// Status header
	if failed > 0 {
		result.WriteString(fmt.Sprintf("FAIL - %d/%d tests failed", failed, total))
	} else {
		result.WriteString(fmt.Sprintf("PASS - %d tests passed", passed))
	}
	if skipped > 0 {
		result.WriteString(fmt.Sprintf(", %d skipped", skipped))
	}
	result.WriteString(fmt.Sprintf(" (%.1fs)\n", duration.Seconds()))

	// Failed test details
	if len(failures) > 0 {
		result.WriteString("\nFailed tests:\n")
		for _, f := range failures {
			result.WriteString(fmt.Sprintf("  ✗ %s\n", f))
			if lines, ok := failOutput[f]; ok {
				// Show relevant output (last 10 lines)
				start := 0
				if len(lines) > 10 {
					start = len(lines) - 10
				}
				for _, l := range lines[start:] {
					l = strings.TrimSpace(l)
					if l != "" && !strings.HasPrefix(l, "=== RUN") && !strings.HasPrefix(l, "--- FAIL") {
						result.WriteString(fmt.Sprintf("    %s\n", l))
					}
				}
			}
		}
	}

	// Package summary
	var failedPkgs []string
	for pkg, status := range packages {
		if status == "fail" {
			failedPkgs = append(failedPkgs, pkg)
		}
	}
	if len(failedPkgs) > 0 {
		result.WriteString(fmt.Sprintf("\nFailed packages: %s\n", strings.Join(failedPkgs, ", ")))
	}

	return result.String()
}

// parseGenericTestResults handles non-JSON test output.
func parseGenericTestResults(output string, execErr error, duration time.Duration) string {
	var result strings.Builder

	if execErr != nil {
		result.WriteString(fmt.Sprintf("FAIL (%.1fs)\n\n", duration.Seconds()))
	} else {
		result.WriteString(fmt.Sprintf("PASS (%.1fs)\n\n", duration.Seconds()))
	}

	// Truncate long output
	if len(output) > 5000 {
		// Show first 2000 and last 2000 chars
		result.WriteString(output[:2000])
		result.WriteString("\n\n... (output truncated) ...\n\n")
		result.WriteString(output[len(output)-2000:])
	} else {
		result.WriteString(output)
	}

	return result.String()
}
