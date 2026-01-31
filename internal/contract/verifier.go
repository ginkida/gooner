package contract

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Dangerous patterns that could indicate malicious commands
var dangerousPatterns = []string{
	// Network exfiltration
	"curl ", "wget ", "nc ", "netcat ",
	// Credential access
	"/etc/passwd", "/etc/shadow", "~/.ssh", ".aws/credentials",
	"$HOME/.ssh", "${HOME}/.ssh",
	// System modification
	"rm -rf /", "rm -rf /*", "mkfs", "dd if=",
	// Process/service manipulation
	"systemctl", "service ", "init ",
	// Privilege escalation
	"sudo ", "su -", "chmod 777", "chown root",
	// Environment variable exfiltration
	"printenv", "export ", "$AWS_", "$GITHUB_TOKEN",
	// Reverse shells
	"/dev/tcp/", "/dev/udp/", "bash -i",
	// Encoded payloads
	"base64 -d", "base64 --decode",
}

// Allowed command prefixes for verification (whitelist approach)
var allowedCommandPrefixes = []string{
	// Testing commands
	"go test", "npm test", "yarn test", "pytest", "cargo test",
	"make test", "make check",
	// Build verification
	"go build", "npm run", "yarn ", "cargo build", "make ",
	// Linting/formatting
	"go fmt", "go vet", "golint", "eslint", "prettier",
	"cargo fmt", "cargo clippy", "rustfmt",
	// File checks
	"test -f", "test -d", "test -e", "[ -f", "[ -d", "[ -e",
	"ls ", "cat ", "head ", "tail ", "wc ", "grep ",
	"find ", "stat ",
	// Git checks
	"git status", "git diff", "git log", "git show",
	// Exit code tests
	"exit ", "true", "false",
	// Echo for simple checks
	"echo ",
}

// Verifier runs shell-based verification against contracts.
type Verifier struct {
	workDir          string
	timeout          time.Duration
	allowUnsafe      bool // If true, skip safety checks (for trusted contexts only)
	allowedPrefixes  []string
	blockedPatterns  []string
}

// VerifierOption configures the verifier.
type VerifierOption func(*Verifier)

// WithAllowUnsafe disables command safety checks (use with caution).
func WithAllowUnsafe(allow bool) VerifierOption {
	return func(v *Verifier) {
		v.allowUnsafe = allow
	}
}

// WithAllowedPrefixes adds additional allowed command prefixes.
func WithAllowedPrefixes(prefixes []string) VerifierOption {
	return func(v *Verifier) {
		v.allowedPrefixes = append(v.allowedPrefixes, prefixes...)
	}
}

// WithBlockedPatterns adds additional blocked patterns.
func WithBlockedPatterns(patterns []string) VerifierOption {
	return func(v *Verifier) {
		v.blockedPatterns = append(v.blockedPatterns, patterns...)
	}
}

// NewVerifier creates a new contract verifier.
func NewVerifier(workDir string, timeout time.Duration, opts ...VerifierOption) *Verifier {
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	v := &Verifier{
		workDir:         workDir,
		timeout:         timeout,
		allowedPrefixes: append([]string{}, allowedCommandPrefixes...),
		blockedPatterns: append([]string{}, dangerousPatterns...),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// CommandSafetyError indicates a command failed safety validation.
type CommandSafetyError struct {
	Command string
	Reason  string
}

func (e *CommandSafetyError) Error() string {
	return fmt.Sprintf("unsafe command blocked: %s (reason: %s)", e.Command, e.Reason)
}

// validateCommand checks if a command is safe to execute.
// Returns nil if safe, error with reason if unsafe.
func (v *Verifier) validateCommand(cmd string) error {
	if v.allowUnsafe {
		return nil
	}

	cmd = strings.TrimSpace(cmd)
	cmdLower := strings.ToLower(cmd)

	// Check for dangerous patterns
	for _, pattern := range v.blockedPatterns {
		if strings.Contains(cmdLower, strings.ToLower(pattern)) {
			return &CommandSafetyError{
				Command: truncateCommand(cmd, 50),
				Reason:  fmt.Sprintf("contains blocked pattern: %s", pattern),
			}
		}
	}

	// Check for path traversal attempts
	if strings.Contains(cmd, "..") {
		// Allow .. only within the working directory context
		// But block absolute paths with ..
		if strings.Contains(cmd, "/../") || strings.HasPrefix(cmd, "../../../") {
			return &CommandSafetyError{
				Command: truncateCommand(cmd, 50),
				Reason:  "potential path traversal attack",
			}
		}
	}

	// Check for command chaining that could bypass restrictions
	// Allow && and || but check each part
	if strings.Contains(cmd, ";") {
		// Semicolon allows unconditional command chaining - more risky
		parts := strings.Split(cmd, ";")
		for _, part := range parts {
			if err := v.validateCommandPart(strings.TrimSpace(part)); err != nil {
				return err
			}
		}
		return nil
	}

	return v.validateCommandPart(cmd)
}

// validateCommandPart validates a single command (no chaining).
func (v *Verifier) validateCommandPart(cmd string) error {
	if cmd == "" {
		return nil
	}

	cmd = strings.TrimSpace(cmd)
	cmdLower := strings.ToLower(cmd)

	// Check against allowed prefixes (whitelist)
	for _, prefix := range v.allowedPrefixes {
		if strings.HasPrefix(cmdLower, strings.ToLower(prefix)) {
			return nil
		}
	}

	// Check for pipe chains - validate the first command
	if idx := strings.Index(cmd, "|"); idx > 0 {
		firstCmd := strings.TrimSpace(cmd[:idx])
		return v.validateCommandPart(firstCmd)
	}

	// Check for subshell/command substitution
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		return &CommandSafetyError{
			Command: truncateCommand(cmd, 50),
			Reason:  "command substitution not allowed",
		}
	}

	return &CommandSafetyError{
		Command: truncateCommand(cmd, 50),
		Reason:  "command not in allowed list",
	}
}

// truncateCommand shortens a command for error messages.
func truncateCommand(cmd string, maxLen int) string {
	if len(cmd) <= maxLen {
		return cmd
	}
	return cmd[:maxLen-3] + "..."
}

// Verify runs all examples and invariant checks for a contract.
func (v *Verifier) Verify(ctx context.Context, c *Contract) (*VerificationResult, error) {
	start := time.Now()

	// Validate contract first
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("contract validation failed: %w", err)
	}

	result := &VerificationResult{
		ContractID: c.ID,
		Passed:     true,
		VerifiedAt: start,
	}

	// Verify examples
	for _, ex := range c.Examples {
		if ex.Command == "" {
			continue
		}

		// Validate command safety
		if err := v.validateCommand(ex.Command); err != nil {
			result.ExampleResults = append(result.ExampleResults, ExampleResult{
				Name:   ex.Name,
				Passed: false,
				Error:  err.Error(),
			})
			result.Passed = false
			continue
		}

		exResult := v.verifyExample(ctx, &ex)
		result.ExampleResults = append(result.ExampleResults, exResult)
		if !exResult.Passed {
			result.Passed = false
		}
	}

	// Verify invariants
	for _, inv := range c.Invariants {
		if inv.Check == "" {
			continue
		}

		// Validate command safety
		if err := v.validateCommand(inv.Check); err != nil {
			result.InvariantResults = append(result.InvariantResults, InvariantResult{
				Name:   inv.Name,
				Passed: false,
				Error:  err.Error(),
			})
			result.Passed = false
			continue
		}

		invResult := v.verifyInvariant(ctx, &inv)
		result.InvariantResults = append(result.InvariantResults, invResult)
		if !invResult.Passed {
			result.Passed = false
		}
	}

	result.Duration = time.Since(start)
	result.Summary = v.buildSummary(result)

	return result, nil
}

// verifyExample runs a single example's command and checks output.
func (v *Verifier) verifyExample(ctx context.Context, ex *Example) ExampleResult {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	// Ensure workDir is absolute and clean
	absWorkDir, err := filepath.Abs(v.workDir)
	if err != nil {
		return ExampleResult{
			Name:   ex.Name,
			Passed: false,
			Error:  fmt.Sprintf("invalid work directory: %v", err),
		}
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", ex.Command)
	cmd.Dir = absWorkDir

	// Restrict environment to prevent leaking sensitive data
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + absWorkDir,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}

	output, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(output))

	if err != nil {
		// For exit_code match type, a non-zero exit is expected
		if ex.MatchType == "exit_code" {
			exitCode := fmt.Sprintf("%d", cmd.ProcessState.ExitCode())
			if exitCode == ex.ExpectedOutput {
				return ExampleResult{Name: ex.Name, Passed: true, Output: outputStr}
			}
			return ExampleResult{
				Name:   ex.Name,
				Passed: false,
				Output: outputStr,
				Error:  fmt.Sprintf("expected exit code %s, got %s", ex.ExpectedOutput, exitCode),
			}
		}
		return ExampleResult{
			Name:   ex.Name,
			Passed: false,
			Output: outputStr,
			Error:  err.Error(),
		}
	}

	// Check output against expected
	passed := v.matchOutput(outputStr, ex.ExpectedOutput, ex.MatchType)

	result := ExampleResult{
		Name:   ex.Name,
		Passed: passed,
		Output: outputStr,
	}
	if !passed {
		result.Error = fmt.Sprintf("output mismatch (match_type: %s)", ex.MatchType)
	}

	return result
}

// verifyInvariant runs an invariant check command.
func (v *Verifier) verifyInvariant(ctx context.Context, inv *Invariant) InvariantResult {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	// Ensure workDir is absolute and clean
	absWorkDir, err := filepath.Abs(v.workDir)
	if err != nil {
		return InvariantResult{
			Name:   inv.Name,
			Passed: false,
			Error:  fmt.Sprintf("invalid work directory: %v", err),
		}
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", inv.Check)
	cmd.Dir = absWorkDir

	// Restrict environment to prevent leaking sensitive data
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + absWorkDir,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return InvariantResult{
			Name:   inv.Name,
			Passed: false,
			Error:  fmt.Sprintf("%s: %s", err.Error(), strings.TrimSpace(string(output))),
		}
	}

	return InvariantResult{Name: inv.Name, Passed: true}
}

// matchOutput compares actual output against expected using the specified match type.
func (v *Verifier) matchOutput(actual, expected, matchType string) bool {
	if expected == "" {
		return true // No expected output means any output is fine
	}

	switch matchType {
	case "exact":
		return actual == expected
	case "contains":
		return strings.Contains(actual, expected)
	case "regex":
		matched, err := regexp.MatchString(expected, actual)
		return err == nil && matched
	case "exit_code":
		return actual == expected
	default:
		// Default to contains
		return strings.Contains(actual, expected)
	}
}

// buildSummary generates a human-readable summary of verification results.
func (v *Verifier) buildSummary(result *VerificationResult) string {
	passedExamples := 0
	totalExamples := len(result.ExampleResults)
	for _, er := range result.ExampleResults {
		if er.Passed {
			passedExamples++
		}
	}

	passedInvariants := 0
	totalInvariants := len(result.InvariantResults)
	for _, ir := range result.InvariantResults {
		if ir.Passed {
			passedInvariants++
		}
	}

	parts := []string{}
	if totalExamples > 0 {
		parts = append(parts, fmt.Sprintf("Examples: %d/%d passed", passedExamples, totalExamples))
	}
	if totalInvariants > 0 {
		parts = append(parts, fmt.Sprintf("Invariants: %d/%d passed", passedInvariants, totalInvariants))
	}
	parts = append(parts, fmt.Sprintf("Duration: %s", result.Duration.Round(time.Millisecond)))

	return strings.Join(parts, ", ")
}
