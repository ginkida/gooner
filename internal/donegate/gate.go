// Package donegate implements the project-aware completion gate —
// build/vet/test checks targeted at touched paths, git ground-truth
// discovery, and claim verification — shared by the foreground App gate
// and sub-agent gates.
package donegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gokin/internal/tools"
)

const (
	doneGateOutputLimit          = 1600
	DefaultCheckTimeout          = 3 * time.Minute
	doneGateValidateCheckTimeout = 30 * time.Second // Fast checks (validate, lint) don't need 3 min
	doneGateMaxScanDepth         = 4
	doneGateMaxModulesPerStack   = 8
	doneGateMonorepoModuleLimit  = 4
	doneGateMaxRunTestTargets    = 2
	doneGateGitProbeTimeout      = 2 * time.Second
)

// requireCmd prefixes a toolchain check with a shell existence probe: if the
// binary is absent, the check prints an explanatory line and exits 0 (a skip),
// so a repo whose toolchain isn't installed on this host never gets a
// permanently-failing check that blocks every mutating turn after the auto-fix
// budget. Matches the composer-probe idiom already used inline below; inside a
// subshell wrap (newBashCheckWithDir) the exit 0 exits the subshell → the whole
// check succeeds.
func requireCmd(bin, command string) string {
	return "command -v " + bin + " >/dev/null 2>&1 || { echo '" + bin + " not found — skipping'; exit 0; }; " + command
}

type Check struct {
	Name     string
	Evidence string
	Run      func(context.Context) (tools.ToolResult, error)
}

type Result struct {
	Name        string
	DisplayName string
	Success     bool
	Content     string
	Error       string
}

type doneGateTestTarget struct {
	Path      string
	Framework string
}

type Policy struct {
	Enabled         bool
	Mode            string
	FailClosed      bool
	CheckTimeout    time.Duration
	AutoFixAttempts int
}

type NodeProject struct {
	Dir     string
	Runner  string
	Scripts map[string]string
}

type JavaProject struct {
	Dir    string
	Runner string // maven | gradle_wrapper | gradle
}

type PHPProject struct {
	Dir     string
	Scripts map[string]string
}

type Profile struct {
	WorkDir           string
	GoModules         []string
	RustModules       []string
	NodeProjects      []NodeProject
	JavaProjects      []JavaProject
	CMakeProjects     []string
	BazelRoots        []string
	MakeProjects      []string
	PHPProjects       []PHPProject
	PythonRoots       []string
	PythonTestsLikely bool
	TouchedPaths      []string
	Monorepo          bool
}

// ToolGetter is the minimal registry surface BuildChecks needs.
// Satisfied by *tools.Registry.
type ToolGetter interface {
	Get(name string) (tools.Tool, bool)
}

func BuildChecks(reg ToolGetter, workDir string, userMessage string, toolsUsed []string, profile Profile, policy Policy) []Check {
	var checks []Check
	seen := make(map[string]bool)

	if verifyTarget, supported := doneGateVerifyCodeTarget(workDir, profile); supported {
		verifyTool, ok := reg.Get("verify_code")
		if ok {
			checks = append(checks, Check{
				Name:     "verify_code",
				Evidence: doneGateCheckEvidence("verify_code", ""),
				Run: func(ctx context.Context) (tools.ToolResult, error) {
					return verifyTool.Execute(ctx, map[string]any{"path": verifyTarget})
				},
			})
			seen["verify_code"] = true
		}
	}

	// Stack-specific checks.
	if bashTool, ok := reg.Get("bash"); ok {
		// Targeted `go vet`: if the turn touched Go files, run vet only
		// in their packages. Otherwise fall back to whole-module `./...`.
		// Scoping keeps feedback fast for small edits (1-3 packages
		// instead of every package in a monorepo) without losing the
		// broader-sweep safety net when we can't identify a target.
		goVetTargets := doneGateGoVetTargets(profile, doneGateMaxRunTestTargets)
		if len(goVetTargets) > 0 {
			// targetDir is either `.` (module root) or a subdirectory
			// relative to the module, so `go vet .` runs in that dir.
			// Pass the dir as the bash cwd via newBashCheckWithDir.
			for _, tgt := range goVetTargets {
				rel := relPathOrDot(workDir, tgt.Path)
				name := "go_vet@" + rel
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
					bashTool,
					name,
					tgt.Path,
					"go vet .",
				))
			}
		} else {
			for _, moduleDir := range profile.GoModules {
				name := "go_vet@" + relPathOrDot(workDir, moduleDir)
				command := "go vet ./..."
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
					bashTool,
					name,
					moduleDir,
					command,
				))
			}
		}
		for _, moduleDir := range profile.RustModules {
			// Skip untouched Rust crates — narrows the check to what
			// the turn actually changed in multi-crate workspaces.
			if !moduleHasTouchedFile(moduleDir, workDir, profile.TouchedPaths) {
				continue
			}
			name := "cargo_check@" + relPathOrDot(workDir, moduleDir)
			command := "cargo check --all-targets"
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				name,
				moduleDir,
				command,
			))
		}
		for _, project := range profile.JavaProjects {
			rel := relPathOrDot(workDir, project.Dir)
			name := "java_verify@" + rel
			command := ""
			switch project.Runner {
			case "maven":
				command = requireCmd("mvn", "mvn -q -DskipTests verify")
			case "gradle_wrapper":
				// The marker WAS the ./gradlew wrapper, so it exists by
				// construction — no toolchain probe needed.
				command = "./gradlew -q build -x test"
			default:
				command = requireCmd("gradle", "gradle -q build -x test")
			}
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				name,
				project.Dir,
				command,
			))
		}
		for _, moduleDir := range profile.CMakeProjects {
			name := "cmake_configure@" + relPathOrDot(workDir, moduleDir)
			inner := "cmake -S . -B .gokin-donegate-build"
			if policy.Mode == "strict" {
				inner += " && cmake --build .gokin-donegate-build"
			}
			inner += "; ret=$?; rm -rf .gokin-donegate-build; exit $ret"
			command := requireCmd("cmake", inner)
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				name,
				moduleDir,
				command,
			))
		}
		for _, moduleDir := range profile.BazelRoots {
			// Skip bazel roots untouched this turn (mirrors the Rust/PHP
			// per-module narrowing) so a stray marker in an unedited part of
			// the tree can't force a build.
			if !moduleHasTouchedFile(moduleDir, workDir, profile.TouchedPaths) {
				continue
			}
			name := "bazel_nobuild@" + relPathOrDot(workDir, moduleDir)
			command := requireCmd("bazel", "bazel build --nobuild //...")
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				name,
				moduleDir,
				command,
			))
		}
		for _, moduleDir := range profile.MakeProjects {
			name := "make_dry_run@" + relPathOrDot(workDir, moduleDir)
			command := requireCmd("make", "make -n")
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				name,
				moduleDir,
				command,
			))
		}
		for _, project := range profile.PHPProjects {
			// Laravel / Symfony monorepos routinely have multiple
			// composer projects; skip the ones untouched this turn.
			if !moduleHasTouchedFile(project.Dir, workDir, profile.TouchedPaths) {
				continue
			}
			rel := relPathOrDot(workDir, project.Dir)
			if scriptName, _, ok := pickComposerScript(project.Scripts, "lint"); ok {
				name := "php_lint@" + rel
				command := "command -v composer >/dev/null 2>&1 || { echo 'composer not found — skipping'; exit 0; }; composer run --no-interaction --quiet " + shellQuote(scriptName)
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithTimeout(
					bashTool,
					name,
					project.Dir,
					command,
					DefaultCheckTimeout,
				))
			}
			if scriptName, _, ok := pickComposerScript(project.Scripts, "static"); ok {
				name := "php_static@" + rel
				command := "command -v composer >/dev/null 2>&1 || { echo 'composer not found — skipping'; exit 0; }; composer run --no-interaction --quiet " + shellQuote(scriptName)
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithTimeout(
					bashTool,
					name,
					project.Dir,
					command,
					DefaultCheckTimeout,
				))
			}
			if policy.Mode == "strict" || len(project.Scripts) == 0 {
				name := "composer_validate@" + rel
				// Probe composer existence first — skip if not installed (exit 127 is "not found").
				// Use short timeout: validate is a fast local operation, no network needed.
				command := "command -v composer >/dev/null 2>&1 || { echo 'composer not found in PATH — skipping'; exit 0; }; composer validate --no-interaction --strict"
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithTimeout(
					bashTool,
					name,
					project.Dir,
					command,
					doneGateValidateCheckTimeout,
				))
			}
		}
		for _, project := range profile.NodeProjects {
			// Skip untouched Node workspaces — a yarn/pnpm monorepo
			// with 10 packages shouldn't re-lint all of them when the
			// turn touched only one.
			if !moduleHasTouchedFile(project.Dir, workDir, profile.TouchedPaths) {
				continue
			}
			if scriptName, _, ok := pickNodeScript(project.Scripts, "lint"); ok {
				name := "node_lint@" + relPathOrDot(workDir, project.Dir)
				command := nodeRunScriptCommand(project.Runner, scriptName)
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
					bashTool,
					name,
					project.Dir,
					command,
				))
			}
			if scriptName, _, ok := pickNodeScript(project.Scripts, "typecheck"); ok {
				name := "node_typecheck@" + relPathOrDot(workDir, project.Dir)
				command := nodeRunScriptCommand(project.Runner, scriptName)
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
					bashTool,
					name,
					project.Dir,
					command,
				))
			}
			if scriptName, scriptBody, ok := pickNodeScript(project.Scripts, "test"); ok && !isNoopNodeTestScript(scriptBody) {
				name := "node_test@" + relPathOrDot(workDir, project.Dir)
				command := nodeRunScriptCommand(project.Runner, scriptName)
				checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
					bashTool,
					name,
					project.Dir,
					command,
				))
			}

			if policy.Mode == "strict" {
				if scriptName, _, ok := pickNodeScript(project.Scripts, "build"); ok {
					name := "node_build@" + relPathOrDot(workDir, project.Dir)
					command := nodeRunScriptCommand(project.Runner, scriptName)
					checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
						bashTool,
						name,
						project.Dir,
						command,
					))
				}
			}
		}

		for _, pythonRoot := range profile.PythonRoots {
			// Skip Python roots with no touched files — compileall is
			// cheap but not instant on large packages; narrowing keeps
			// done-gate fast when the turn only modified one service
			// in a multi-package repo.
			if !moduleHasTouchedFile(pythonRoot, workDir, profile.TouchedPaths) {
				continue
			}
			name := "python_compile@" + relPathOrDot(workDir, pythonRoot)
			command := "python3 -m compileall -q ."
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				name,
				pythonRoot,
				command,
			))
		}

		// Generic repository health checks (stack-agnostic). Anchored to the
		// repo being VERIFIED (workDir) — un-anchored they ran in whatever
		// cwd the model or a previous check left behind.
		checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
			bashTool,
			"git_diff_check",
			workDir,
			"if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then git diff --check; else true; fi",
		))
		if policy.Mode == "strict" {
			checks = appendUniqueDoneGateCheck(checks, seen, newBashCheckWithDir(
				bashTool,
				"git_unmerged_paths",
				workDir,
				"if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then if [ -n \"$(git ls-files -u)\" ]; then echo 'unmerged paths detected'; git ls-files -u; exit 1; fi; fi",
			))
		}
	}

	testTargets := doneGateRunTestsTargets(profile)
	if len(testTargets) > 0 && shouldRunDoneGateToolTests(userMessage, toolsUsed, profile) {
		if testsTool, ok := reg.Get("run_tests"); ok {
			for _, target := range testTargets {
				name := "run_tests@" + target.Framework + "@" + relPathOrDot(workDir, target.Path)
				checks = appendUniqueDoneGateCheck(checks, seen, Check{
					Name:     name,
					Evidence: doneGateCheckEvidence(name, ""),
					Run: func(ctx context.Context) (tools.ToolResult, error) {
						return testsTool.Execute(ctx, map[string]any{
							"path":      target.Path,
							"framework": target.Framework,
						})
					},
				})
			}
		}
	}

	return checks
}

type doneGateVerifyTarget struct {
	dir       string
	supported bool
}

// doneGateVerifyCodeTarget returns the project directory that verify_code can
// actually verify. VerifyCodeTool only supports Go, Rust, Node, and Python; the
// done-gate also knows about Java, PHP, CMake, Bazel, and Make projects, so
// invoking verify_code unconditionally made those projects fail with "Could not
// detect project type".
//
// Touched paths narrow the choice in mixed workspaces. Project roots are matched
// most-specific-first so, for example, a Java service nested under a root Go
// module does not accidentally trigger a repository-wide Go build when only the
// Java service changed.
func doneGateVerifyCodeTarget(workDir string, profile Profile) (string, bool) {
	root := strings.TrimSpace(workDir)
	if root == "" {
		root = strings.TrimSpace(profile.WorkDir)
	}
	if root != "" {
		root = filepath.Clean(root)
	}

	targets := make([]doneGateVerifyTarget, 0,
		len(profile.GoModules)+len(profile.RustModules)+len(profile.NodeProjects)+len(profile.PythonRoots)+
			len(profile.JavaProjects)+len(profile.PHPProjects)+len(profile.CMakeProjects)+len(profile.BazelRoots)+len(profile.MakeProjects))
	seen := make(map[string]bool)
	addTarget := func(dir string, supported bool) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		if !filepath.IsAbs(dir) && root != "" {
			dir = filepath.Join(root, dir)
		}
		dir = filepath.Clean(dir)
		key := fmt.Sprintf("%t|%s", supported, dir)
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, doneGateVerifyTarget{dir: dir, supported: supported})
	}

	for _, dir := range profile.GoModules {
		addTarget(dir, true)
	}
	for _, dir := range profile.RustModules {
		addTarget(dir, true)
	}
	for _, project := range profile.NodeProjects {
		addTarget(project.Dir, true)
	}
	for _, dir := range profile.PythonRoots {
		addTarget(dir, true)
	}
	for _, project := range profile.JavaProjects {
		addTarget(project.Dir, false)
	}
	for _, project := range profile.PHPProjects {
		addTarget(project.Dir, false)
	}
	for _, dir := range profile.CMakeProjects {
		addTarget(dir, false)
	}
	for _, dir := range profile.BazelRoots {
		addTarget(dir, false)
	}
	for _, dir := range profile.MakeProjects {
		addTarget(dir, false)
	}

	if len(profile.TouchedPaths) == 0 {
		for _, target := range targets {
			if target.supported && root != "" && target.dir == root {
				return target.dir, true
			}
		}
		for _, target := range targets {
			if target.supported {
				return target.dir, true
			}
		}
		return "", false
	}

	for _, touched := range profile.TouchedPaths {
		absTouched := strings.TrimSpace(touched)
		if absTouched == "" {
			continue
		}
		if !filepath.IsAbs(absTouched) && root != "" {
			absTouched = filepath.Join(root, filepath.FromSlash(absTouched))
		}
		absTouched = filepath.Clean(absTouched)

		bestSupported, bestUnsupported := "", ""
		bestSupportedDepth, bestUnsupportedDepth := -1, -1
		for _, target := range targets {
			if !pathWithinDir(target.dir, absTouched) {
				continue
			}
			depth := doneGatePathSpecificity(target.dir)
			if target.supported {
				if depth > bestSupportedDepth {
					bestSupported, bestSupportedDepth = target.dir, depth
				}
			} else if depth > bestUnsupportedDepth {
				bestUnsupported, bestUnsupportedDepth = target.dir, depth
			}
		}

		supportedHint, hintKnown := doneGateTouchedPathVerifySupport(absTouched)
		if hintKnown && !supportedHint {
			continue
		}
		if bestSupported != "" && bestSupportedDepth > bestUnsupportedDepth {
			return bestSupported, true
		}
		// Native sources and headers are contextual: they may belong to cgo,
		// Rust FFI, a Node native addon, or a Python extension even though
		// verify_code cannot detect a standalone C/C++ project. When a supported
		// project and a generic Make/CMake marker share the same root, prefer the
		// supported project. A more-specific unsupported root still wins above.
		if bestSupported != "" && bestSupportedDepth == bestUnsupportedDepth &&
			((hintKnown && supportedHint) || doneGateTouchedPathIsNative(absTouched)) {
			return bestSupported, true
		}
		if hintKnown && supportedHint && bestSupported == "" && bestUnsupported == "" {
			return filepath.Dir(absTouched), true
		}
	}

	return "", false
}

func doneGatePathSpecificity(path string) int {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	path = strings.TrimPrefix(path, volume)
	path = strings.Trim(path, string(filepath.Separator))
	if path == "" {
		return 0
	}
	return strings.Count(path, string(filepath.Separator)) + 1
}

// doneGateTouchedPathVerifySupport recognizes files that unambiguously belong
// to a stack VerifyCodeTool either supports or deliberately does not support.
// Unknown files (README, YAML, etc.) inherit the nearest project root.
func doneGateTouchedPathVerifySupport(path string) (supported, known bool) {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "go.mod", "go.sum", "cargo.toml", "package.json", "package-lock.json", "npm-shrinkwrap.json",
		"pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb", "requirements.txt", "pyproject.toml", "setup.py":
		return true, true
	case "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradlew",
		"composer.json", "cmakelists.txt", "build", "build.bazel", "workspace",
		"workspace.bazel", "module.bazel":
		return false, true
	}

	switch strings.ToLower(filepath.Ext(base)) {
	case ".go", ".rs", ".js", ".jsx", ".ts", ".tsx", ".py":
		return true, true
	case ".java", ".kt", ".kts", ".php", ".bzl":
		return false, true
	default:
		return false, false
	}
}

func doneGateTouchedPathIsNative(path string) bool {
	switch strings.ToLower(filepath.Ext(filepath.Base(path))) {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx":
		return true
	default:
		return false
	}
}

func appendUniqueDoneGateCheck(checks []Check, seen map[string]bool, check Check) []Check {
	if check.Name == "" {
		return checks
	}
	if seen[check.Name] {
		return checks
	}
	seen[check.Name] = true
	return append(checks, check)
}

func newBashCheck(bashTool tools.Tool, name, command string) Check {
	return Check{
		Name:     name,
		Evidence: doneGateCheckEvidence(name, command),
		Run: func(ctx context.Context) (tools.ToolResult, error) {
			return bashTool.Execute(ctx, map[string]any{
				"command":     command,
				"description": "done-gate check: " + name,
			})
		},
	}
}

func newBashCheckWithDir(bashTool tools.Tool, name, dir, command string) Check {
	if strings.TrimSpace(dir) == "" || dir == "." {
		return newBashCheck(bashTool, name, command)
	}

	wrapped := subshellCd(dir, command)
	return newBashCheck(bashTool, name, wrapped)
}

// subshellCd wraps a check command so its cd is confined to a subshell — gate
// checks run through the model's LIVE shared BashTool, whose pwd probe commits
// the final cwd as the persistent session cwd, so a bare `cd subdir && check`
// permanently moved the model's cwd into the checked subpackage (even on
// failure). `cd dir || exit 1` (NOT `cd dir && cmd`) so a cd FAILURE fails the
// check loudly instead of falling through into a requireCmd `|| { exit 0; }`
// skip branch (which would silently PASS a check whose dir vanished).
func subshellCd(dir, command string) string {
	return "( cd " + shellQuote(dir) + " || exit 1; " + command + " )"
}

// newBashCheckWithTimeout creates a done-gate check with a custom timeout.
// Used for fast checks (validate, lint) that shouldn't block for 3 minutes.
func newBashCheckWithTimeout(bashTool tools.Tool, name, dir, command string, timeout time.Duration) Check {
	if strings.TrimSpace(dir) == "" || dir == "." {
		dir = ""
	}
	wrapped := command
	if dir != "" {
		// Subshell for the same reason as newBashCheckWithDir: never move
		// the shared bash session's persistent cwd.
		wrapped = subshellCd(dir, command)
	}
	return Check{
		Name:     name,
		Evidence: doneGateCheckEvidence(name, wrapped),
		Run: func(ctx context.Context) (tools.ToolResult, error) {
			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return bashTool.Execute(checkCtx, map[string]any{
				"command":     wrapped,
				"description": "done-gate check: " + name,
			})
		},
	}
}

func RunChecks(ctx context.Context, checks []Check, checkTimeout time.Duration, onProgress func(current, total int, name string), onEvidence func(evidence string)) []Result {
	results := make([]Result, 0, len(checks))
	for i, check := range checks {
		if onProgress != nil {
			onProgress(i+1, len(checks), doneGateCheckDisplayName(check))
		}
		checkCtx := ctx
		cancel := func() {}
		if checkTimeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, checkTimeout)
		}
		result, err := check.Run(checkCtx)
		timedOut := errors.Is(checkCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			errMsg := strings.TrimSpace(err.Error())
			if timedOut {
				errMsg = fmt.Sprintf("check timed out after %v", checkTimeout)
			}
			results = append(results, Result{
				Name:        check.Name,
				DisplayName: doneGateCheckDisplayName(check),
				Success:     false,
				Error:       errMsg,
			})
			continue
		}
		content := strings.TrimSpace(result.Content)
		errContent := strings.TrimSpace(result.Error)
		if timedOut {
			results = append(results, Result{
				Name:        check.Name,
				DisplayName: doneGateCheckDisplayName(check),
				Success:     false,
				Content:     content,
				Error:       fmt.Sprintf("check timed out after %v", checkTimeout),
			})
			continue
		}
		results = append(results, Result{
			Name:        check.Name,
			DisplayName: doneGateCheckDisplayName(check),
			Success:     result.Success,
			Content:     content,
			Error:       errContent,
		})
		if result.Success {
			if onEvidence != nil {
				onEvidence(check.Evidence)
			}
		}
	}
	return results
}

func CountResults(results []Result) (passed, failed int) {
	for _, r := range results {
		if r.Success {
			passed++
		} else {
			failed++
		}
	}
	return passed, failed
}

func FormatFailedSummary(results []Result, limit int) string {
	if limit <= 0 {
		return ""
	}
	var items []string
	remaining := 0
	for _, r := range results {
		if r.Success {
			continue
		}
		if len(items) >= limit {
			remaining++
			continue
		}
		name := ResultDisplayName(r)
		if detail := compactDoneGateFailureDetail(r); detail != "" {
			items = append(items, fmt.Sprintf("%s (%s)", name, detail))
		} else {
			items = append(items, name)
		}
	}
	if len(items) == 0 {
		return ""
	}
	if remaining > 0 {
		items = append(items, fmt.Sprintf("+%d more", remaining))
	}
	return strings.Join(items, "; ")
}

func Passed(results []Result) bool {
	for _, r := range results {
		if !r.Success {
			return false
		}
	}
	return true
}

func ShouldEnforce(userMessage string, toolsUsed []string) bool {
	if len(toolsUsed) == 0 {
		return false
	}

	sideEffecting := map[string]bool{
		"write": true, "edit": true, "move": true, "copy": true, "delete": true,
		"mkdir": true, "refactor": true, "batch": true,
	}

	for _, name := range toolsUsed {
		if sideEffecting[name] {
			return true
		}
		if name == "bash" && looksLikeCodingTask(userMessage) {
			return true
		}
	}
	return false
}

func shouldRunDoneGateToolTests(userMessage string, toolsUsed []string, profile Profile) bool {
	lower := strings.ToLower(userMessage)
	if strings.Contains(lower, "без тест") || strings.Contains(lower, "no tests") {
		return false
	}

	if len(doneGateRunTestsTargets(profile)) == 0 {
		return false
	}

	if slices.Contains(toolsUsed, "run_tests") {
		return true
	}

	if TouchedPathsRequireTests(profile.TouchedPaths) {
		return true
	}

	if len(profile.TouchedPaths) > 0 {
		return false
	}

	if len(profile.GoModules) > 0 || len(profile.RustModules) > 0 {
		return true
	}
	if len(profile.PythonRoots) > 0 && profile.PythonTestsLikely {
		return true
	}

	triggers := []string{
		"run tests", "go test", "pytest", "npm test", "cargo test",
		"запусти тест", "прогони тест", "прогон тест", "падение тест",
	}
	for _, t := range triggers {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

func IsMutationTool(toolName string) bool {
	switch toolName {
	case "write", "edit", "move", "copy", "delete", "mkdir", "batch", "refactor":
		return true
	default:
		return false
	}
}

func ExtractTouchedPaths(args map[string]any) []string {
	if len(args) == 0 {
		return nil
	}

	var paths []string
	for _, key := range []string{"file_path", "path", "source", "destination", "new_path"} {
		if p, ok := args[key].(string); ok && strings.TrimSpace(p) != "" {
			paths = append(paths, p)
		}
	}
	// The batch tool carries its targets as a "files" LIST — reading only the
	// scalar keys above recorded ZERO touched paths for batch edits, and any
	// single other recorded path (a README write) then suppressed the git
	// ground-truth fallback, so the batch-edited module's checks were skipped
	// entirely. JSON-sourced args give []any; accept []string defensively.
	switch files := args["files"].(type) {
	case []any:
		for _, f := range files {
			if p, ok := f.(string); ok && strings.TrimSpace(p) != "" {
				paths = append(paths, p)
			}
		}
	case []string:
		for _, p := range files {
			if strings.TrimSpace(p) != "" {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

func NormalizeTouchedPaths(workDir string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(workDir, p)
			if err != nil {
				continue
			}
			p = rel
		}
		p = filepath.Clean(p)
		if p == "." || strings.HasPrefix(p, "..") {
			continue
		}
		p = filepath.ToSlash(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TouchedPathsRequireTests(paths []string) bool {
	for _, raw := range paths {
		p := strings.ToLower(filepath.ToSlash(strings.TrimSpace(raw)))
		if p == "" {
			continue
		}
		base := strings.ToLower(filepath.Base(p))

		if strings.Contains(p, "/tests/") || strings.Contains(p, "/test/") || strings.HasSuffix(base, "_test.go") {
			return true
		}

		switch filepath.Ext(base) {
		case ".go", ".rs", ".py", ".js", ".jsx", ".ts", ".tsx":
			return true
		}

		switch base {
		case "go.mod", "go.sum", "cargo.toml", "package.json", "pnpm-lock.yaml", "yarn.lock",
			"requirements.txt", "pyproject.toml", "setup.py":
			return true
		}
	}
	return false
}

func DetectProfile(workDir string) Profile {
	return DetectProfileWithTouchedPaths(workDir, nil)
}

func DetectProfileWithTouchedPaths(workDir string, touchedOverride []string) Profile {
	markers := discoverDoneGateMarkers(workDir)
	touched := NormalizeTouchedPaths(workDir, touchedOverride)
	if len(touched) == 0 {
		touched = DiscoverTouchedPaths(workDir)
	}

	// Supported language modules are independent build boundaries (Go's ./...
	// explicitly stops at a nested go.mod). Keep them and prioritize the
	// most-specific touched root below instead of pruning them behind an
	// ancestor module. CMakeLists, Bazel BUILD files, Makefiles, and Java build
	// descriptors are commonly hierarchical pieces of one parent build; treating
	// each nested marker as a standalone root causes invalid child-only builds.
	goAll := limitAndSortDirs(markers.GoModules, 0)
	rustAll := limitAndSortDirs(markers.RustModules, 0)
	pythonAll := limitAndSortDirs(markers.PythonRoots, 0)
	nodeAll := limitAndSortDirs(markers.NodeProjects, 0)
	javaAll := pruneNestedDirs(limitAndSortDirs(markers.JavaProjects, 0))
	cmakeAll := pruneNestedDirs(limitAndSortDirs(markers.CMakeProjects, 0))
	bazelAll := pruneNestedDirs(limitAndSortDirs(markers.BazelRoots, 0))
	makeAll := pruneNestedDirs(limitAndSortDirs(markers.MakeProjects, 0))
	phpAll := limitAndSortDirs(markers.PHPProjects, 0)

	monorepo := isMonorepoWorkspace(workDir, goAll, rustAll, nodeAll, pythonAll, javaAll, cmakeAll, bazelAll, makeAll, phpAll)
	moduleLimit := doneGateMaxModulesPerStack
	if monorepo {
		moduleLimit = doneGateMonorepoModuleLimit
	}

	goDirs := prioritizeDirsByTouched(workDir, goAll, touched, moduleLimit)
	rustDirs := prioritizeDirsByTouched(workDir, rustAll, touched, moduleLimit)
	pythonDirs := prioritizeDirsByTouched(workDir, pythonAll, touched, moduleLimit)
	nodeDirs := prioritizeDirsByTouched(workDir, nodeAll, touched, moduleLimit)
	javaDirs := prioritizeDirsByTouched(workDir, javaAll, touched, moduleLimit)
	cmakeDirs := prioritizeDirsByTouched(workDir, cmakeAll, touched, moduleLimit)
	bazelDirs := prioritizeDirsByTouched(workDir, bazelAll, touched, moduleLimit)
	makeDirs := prioritizeDirsByTouched(workDir, makeAll, touched, moduleLimit)
	phpDirs := prioritizeDirsByTouched(workDir, phpAll, touched, moduleLimit)

	profile := Profile{
		WorkDir:       workDir,
		GoModules:     goDirs,
		RustModules:   rustDirs,
		CMakeProjects: cmakeDirs,
		BazelRoots:    bazelDirs,
		MakeProjects:  makeDirs,
		PythonRoots:   pythonDirs,
		TouchedPaths:  touched,
		Monorepo:      monorepo,
	}
	profile.NodeProjects = loadNodeProjects(workDir, nodeDirs)
	profile.JavaProjects = loadJavaProjects(workDir, javaDirs)
	profile.PHPProjects = loadPHPProjects(phpDirs)
	profile.PythonTestsLikely = detectPythonTestsLikely(profile.PythonRoots)
	return profile
}

type doneGateMarkers struct {
	GoModules     map[string]struct{}
	RustModules   map[string]struct{}
	NodeProjects  map[string]struct{}
	JavaProjects  map[string]struct{}
	CMakeProjects map[string]struct{}
	BazelRoots    map[string]struct{}
	MakeProjects  map[string]struct{}
	PHPProjects   map[string]struct{}
	PythonRoots   map[string]struct{}
}

func discoverDoneGateMarkers(workDir string) doneGateMarkers {
	markers := doneGateMarkers{
		GoModules:     make(map[string]struct{}),
		RustModules:   make(map[string]struct{}),
		NodeProjects:  make(map[string]struct{}),
		JavaProjects:  make(map[string]struct{}),
		CMakeProjects: make(map[string]struct{}),
		BazelRoots:    make(map[string]struct{}),
		MakeProjects:  make(map[string]struct{}),
		PHPProjects:   make(map[string]struct{}),
		PythonRoots:   make(map[string]struct{}),
	}

	_ = filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if path == workDir {
				return nil
			}
			if shouldSkipDoneGateDir(d.Name()) {
				return filepath.SkipDir
			}
			if pathDepth(workDir, path) > doneGateMaxScanDepth {
				return filepath.SkipDir
			}
			return nil
		}

		if pathDepth(workDir, path) > doneGateMaxScanDepth+1 {
			return nil
		}

		dir := filepath.Dir(path)
		switch d.Name() {
		case "go.mod":
			markers.GoModules[dir] = struct{}{}
		case "Cargo.toml":
			markers.RustModules[dir] = struct{}{}
		case "package.json":
			markers.NodeProjects[dir] = struct{}{}
		case "pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradlew":
			markers.JavaProjects[dir] = struct{}{}
		case "CMakeLists.txt":
			markers.CMakeProjects[dir] = struct{}{}
		case "BUILD.bazel", "WORKSPACE.bazel", "MODULE.bazel":
			// Unambiguous bazel names — always register.
			markers.BazelRoots[dir] = struct{}{}
		case "BUILD", "WORKSPACE":
			// Bare BUILD/WORKSPACE are common plain-text filenames (build
			// instructions, a workspace description). Only treat them as bazel
			// markers when the repo is actually a bazel workspace — i.e. a
			// workspace-level marker exists at the root. Otherwise a stray text
			// file forced a permanently-failing `bazel build` on every turn.
			if bazelWorkspaceAtRoot(workDir) {
				markers.BazelRoots[dir] = struct{}{}
			}
		case "Makefile", "makefile", "GNUmakefile":
			markers.MakeProjects[dir] = struct{}{}
		case "composer.json":
			markers.PHPProjects[dir] = struct{}{}
		case "pyproject.toml", "requirements.txt", "setup.py":
			markers.PythonRoots[dir] = struct{}{}
		}

		return nil
	})

	return markers
}

func shouldSkipDoneGateDir(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".git", "node_modules", "vendor", "dist", "build", "target", ".next", ".turbo",
		".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".idea", ".vscode", ".gokin-donegate-build":
		return true
	default:
		return false
	}
}

func pathDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

func limitAndSortDirs(set map[string]struct{}, limit int) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for dir := range set {
		out = append(out, filepath.Clean(dir))
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func prioritizeDirsByTouched(workDir string, dirs []string, touched []string, limit int) []string {
	if len(dirs) == 0 {
		return nil
	}
	if len(touched) == 0 {
		if limit > 0 && len(dirs) > limit {
			return dirs[:limit]
		}
		return dirs
	}

	normalizedTouched := make([]string, 0, len(touched))
	seenTouched := make(map[string]bool, len(touched))
	for _, p := range touched {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." || strings.HasPrefix(p, "..") {
			continue
		}
		p = filepath.ToSlash(p)
		if seenTouched[p] {
			continue
		}
		seenTouched[p] = true
		normalizedTouched = append(normalizedTouched, p)
	}
	if len(normalizedTouched) == 0 {
		if limit > 0 && len(dirs) > limit {
			return dirs[:limit]
		}
		return dirs
	}

	matched := make([]string, 0, len(dirs))
	rest := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		rel := relPathOrDot(workDir, dir)
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." {
			matched = append(matched, dir)
			continue
		}
		prefix := rel + "/"
		hit := false
		for _, changed := range normalizedTouched {
			if changed == rel || strings.HasPrefix(changed, prefix) {
				hit = true
				break
			}
		}
		if hit {
			matched = append(matched, dir)
		} else {
			rest = append(rest, dir)
		}
	}
	// An ancestor module also contains every touched path under a nested module.
	// Put the deepest boundary first so target selection and module caps cannot
	// silently choose an ancestor whose build excludes the changed child.
	sort.SliceStable(matched, func(i, j int) bool {
		leftDepth := doneGatePathSpecificity(matched[i])
		rightDepth := doneGatePathSpecificity(matched[j])
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return matched[i] < matched[j]
	})

	ordered := append(matched, rest...)
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

// bazelWorkspaceAtRoot reports whether workDir looks like a genuine bazel
// workspace (a workspace-level marker at the root), used to decide whether a
// bare BUILD/WORKSPACE file elsewhere in the tree is a real bazel marker or just
// a plain-text file that happens to share the name.
func bazelWorkspaceAtRoot(workDir string) bool {
	for _, marker := range []string{"WORKSPACE", "WORKSPACE.bazel", "MODULE.bazel"} {
		if fileExists(filepath.Join(workDir, marker)) {
			return true
		}
	}
	return false
}

func isMonorepoWorkspace(workDir string, stacks ...[]string) bool {
	workspaceMarkers := []string{
		"go.work",
		"pnpm-workspace.yaml",
		"turbo.json",
		"nx.json",
		"lerna.json",
		"WORKSPACE",
		"WORKSPACE.bazel",
	}
	for _, marker := range workspaceMarkers {
		if fileExists(filepath.Join(workDir, marker)) {
			return true
		}
	}

	stackKinds := 0
	total := 0
	for _, stack := range stacks {
		n := len(stack)
		if n > 1 {
			return true
		}
		if n > 0 {
			stackKinds++
			total += n
		}
	}
	return stackKinds >= 2 && total >= 3
}

func DiscoverTouchedPaths(workDir string) []string {
	if !isGitWorkTree(workDir) {
		return nil
	}

	commands := [][]string{
		{"git", "-C", workDir, "diff", "--name-only", "--relative"},
		{"git", "-C", workDir, "diff", "--cached", "--name-only", "--relative"},
		{"git", "-C", workDir, "ls-files", "--others", "--exclude-standard"},
	}

	seen := make(map[string]bool)
	var paths []string
	for _, cmdArgs := range commands {
		if len(cmdArgs) == 0 {
			continue
		}
		out, err := runCommandWithTimeout(doneGateGitProbeTimeout, cmdArgs[0], cmdArgs[1:]...)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		for line := range strings.SplitSeq(out, "\n") {
			p := strings.TrimSpace(line)
			if p == "" {
				continue
			}
			p = filepath.Clean(p)
			if p == "." || strings.HasPrefix(p, "..") {
				continue
			}
			p = filepath.ToSlash(p)
			if seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

func isGitWorkTree(workDir string) bool {
	out, err := runCommandWithTimeout(doneGateGitProbeTimeout, "git", "-C", workDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

func runCommandWithTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// Even local git probes can invoke helpers. Ensure a timeout cannot leave
	// descendants holding locks or inherited pipes after the completion gate
	// has already moved on.
	tools.KillProcessGroupOnCancel(cmd)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func pruneNestedDirs(dirs []string) []string {
	if len(dirs) <= 1 {
		return dirs
	}

	ordered := make([]string, len(dirs))
	copy(ordered, dirs)
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) == len(ordered[j]) {
			return ordered[i] < ordered[j]
		}
		return len(ordered[i]) < len(ordered[j])
	})

	var out []string
	for _, dir := range ordered {
		skip := false
		for _, existing := range out {
			if dir == existing || strings.HasPrefix(dir, existing+string(filepath.Separator)) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

func loadNodeProjects(workDir string, dirs []string) []NodeProject {
	if len(dirs) == 0 {
		return nil
	}
	projects := make([]NodeProject, 0, len(dirs))
	for _, dir := range dirs {
		scripts := readNodeScripts(filepath.Join(dir, "package.json"))
		projects = append(projects, NodeProject{
			Dir:     dir,
			Runner:  detectNodeRunner(workDir, dir),
			Scripts: scripts,
		})
	}
	// dirs already has deterministic touched-first ordering. Preserve it so a
	// nested package selected ahead of an ancestor is not silently demoted.
	return projects
}

func loadJavaProjects(workDir string, dirs []string) []JavaProject {
	if len(dirs) == 0 {
		return nil
	}
	projects := make([]JavaProject, 0, len(dirs))
	for _, dir := range dirs {
		runner := "gradle"
		switch {
		case fileExists(filepath.Join(dir, "pom.xml")):
			runner = "maven"
		case fileExists(filepath.Join(dir, "gradlew")):
			runner = "gradle_wrapper"
		case fileExists(filepath.Join(dir, "build.gradle")) || fileExists(filepath.Join(dir, "build.gradle.kts")):
			runner = "gradle"
		default:
			if fileExists(filepath.Join(workDir, "mvnw")) || fileExists(filepath.Join(workDir, "pom.xml")) {
				runner = "maven"
			}
		}
		projects = append(projects, JavaProject{
			Dir:    dir,
			Runner: runner,
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Dir < projects[j].Dir
	})
	return projects
}

func loadPHPProjects(dirs []string) []PHPProject {
	if len(dirs) == 0 {
		return nil
	}
	projects := make([]PHPProject, 0, len(dirs))
	for _, dir := range dirs {
		projects = append(projects, PHPProject{
			Dir:     dir,
			Scripts: readComposerScripts(filepath.Join(dir, "composer.json")),
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Dir < projects[j].Dir
	})
	return projects
}

func readNodeScripts(packageJSONPath string) map[string]string {
	scripts := make(map[string]string)
	data, err := os.ReadFile(packageJSONPath)
	if err != nil {
		return scripts
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return scripts
	}

	for name, script := range pkg.Scripts {
		name = strings.TrimSpace(strings.ToLower(name))
		script = strings.TrimSpace(script)
		if name == "" || script == "" {
			continue
		}
		scripts[name] = script
	}
	return scripts
}

func readComposerScripts(path string) map[string]string {
	scripts := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return scripts
	}

	var pkg struct {
		Scripts map[string]any `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return scripts
	}

	for name, raw := range pkg.Scripts {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		script, ok := raw.(string)
		if !ok {
			continue
		}
		script = strings.TrimSpace(script)
		if script == "" {
			continue
		}
		scripts[name] = script
	}

	return scripts
}

func detectNodeRunner(workDir, startDir string) string {
	for dir := startDir; ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "pnpm-lock.yaml")) {
			return "pnpm"
		}
		if fileExists(filepath.Join(dir, "yarn.lock")) {
			return "yarn"
		}
		if fileExists(filepath.Join(dir, "bun.lockb")) || fileExists(filepath.Join(dir, "bun.lock")) {
			return "bun"
		}
		if fileExists(filepath.Join(dir, "package-lock.json")) || fileExists(filepath.Join(dir, "npm-shrinkwrap.json")) {
			return "npm"
		}
		if dir == workDir {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "npm"
}

func pickNodeScript(scripts map[string]string, category string) (string, string, bool) {
	if len(scripts) == 0 {
		return "", "", false
	}

	aliases := nodeScriptAliases(category)
	for _, alias := range aliases {
		if script, ok := scripts[alias]; ok {
			script = strings.TrimSpace(script)
			if script == "" {
				continue
			}
			return alias, script, true
		}
	}
	return "", "", false
}

func nodeScriptAliases(category string) []string {
	switch category {
	case "lint":
		return []string{"lint", "lint:ci", "check:lint", "eslint", "check-lint"}
	case "typecheck":
		return []string{"typecheck", "check-types", "types", "tsc", "check:type", "type-check"}
	case "test":
		return []string{"test", "test:ci", "ci:test", "unit", "test:unit"}
	case "build":
		return []string{"build", "build:ci", "check", "compile"}
	default:
		return []string{category}
	}
}

func pickComposerScript(scripts map[string]string, category string) (string, string, bool) {
	if len(scripts) == 0 {
		return "", "", false
	}

	aliases := composerScriptAliases(category)
	for _, alias := range aliases {
		if script, ok := scripts[alias]; ok {
			script = strings.TrimSpace(script)
			if script == "" {
				continue
			}
			return alias, script, true
		}
	}
	return "", "", false
}

func composerScriptAliases(category string) []string {
	switch category {
	case "lint":
		return []string{"lint", "cs", "cs-check", "phpcs", "style"}
	case "static":
		return []string{"phpstan", "psalm", "analyse", "analyze", "static", "typecheck"}
	default:
		return []string{category}
	}
}

func nodeRunScriptCommand(runner, script string) string {
	switch strings.ToLower(strings.TrimSpace(runner)) {
	case "pnpm":
		return "pnpm -s run " + shellQuote(script)
	case "yarn":
		return "yarn -s run " + shellQuote(script)
	case "bun":
		return "bun run " + shellQuote(script)
	default:
		return "npm run -s " + shellQuote(script)
	}
}

func isNoopNodeTestScript(script string) bool {
	s := strings.ToLower(strings.TrimSpace(script))
	if s == "" {
		return true
	}
	noop := []string{
		"no test specified",
		"echo \"error: no test specified\"",
		"echo 'error: no test specified'",
		"echo \"no tests\"",
		"echo 'no tests'",
		"echo no tests",
		"exit 1",
	}
	for _, marker := range noop {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// fileExists checks if a file exists.
//
// Copied verbatim from internal/app/project_detector.go — the app package
// keeps its own copy for project detection; this one serves the done-gate
// profile discovery helpers above.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readFileHead reads the first maxChars runes from a file. Rune-aware so a
// multibyte char in a README or package.json description doesn't get split
// at the truncation boundary.
//
// Copied verbatim from internal/app/project_detector.go (see fileExists).
func readFileHead(path string, maxChars int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	runes := []rune(content)
	if len(runes) > maxChars {
		return string(runes[:maxChars])
	}
	return content
}

func detectPythonTestsLikely(pythonRoots []string) bool {
	for _, root := range pythonRoots {
		if fileExists(filepath.Join(root, "pytest.ini")) ||
			fileExists(filepath.Join(root, "conftest.py")) ||
			fileExists(filepath.Join(root, "tox.ini")) ||
			fileExists(filepath.Join(root, "noxfile.py")) ||
			dirExists(filepath.Join(root, "tests")) ||
			dirExists(filepath.Join(root, "test")) ||
			strings.Contains(readFileHead(filepath.Join(root, "pyproject.toml"), 6000), "[tool.pytest") {
			return true
		}
	}
	return false
}

func doneGateRunTestsTargets(profile Profile) []doneGateTestTarget {
	maxTargets := 1
	if profile.Monorepo {
		maxTargets = doneGateMaxRunTestTargets
	}

	targets := make([]doneGateTestTarget, 0, maxTargets)
	appendTarget := func(path string, framework string) {
		if len(targets) >= maxTargets || strings.TrimSpace(path) == "" {
			return
		}
		for _, existing := range targets {
			if existing.Path == path && existing.Framework == framework {
				return
			}
		}
		targets = append(targets, doneGateTestTarget{
			Path:      path,
			Framework: framework,
		})
	}
	appendTargets := func(paths []string, framework string) {
		for _, dir := range paths {
			if len(targets) >= maxTargets {
				return
			}
			appendTarget(dir, framework)
		}
	}

	for _, target := range doneGateGoTestTargets(profile, maxTargets) {
		appendTarget(target.Path, target.Framework)
	}
	appendTargets(profile.RustModules, "cargo")
	if profile.PythonTestsLikely {
		appendTargets(profile.PythonRoots, "pytest")
	}

	return targets
}

func doneGateGoTestTargets(profile Profile, maxTargets int) []doneGateTestTarget {
	if maxTargets <= 0 || len(profile.GoModules) == 0 {
		return nil
	}
	workDir := strings.TrimSpace(profile.WorkDir)
	if workDir == "" {
		return doneGateFrameworkTargets(profile.GoModules, "go", maxTargets)
	}

	var targets []doneGateTestTarget
	add := func(path string) {
		if len(targets) >= maxTargets || strings.TrimSpace(path) == "" {
			return
		}
		path = filepath.Clean(path)
		for _, existing := range targets {
			if existing.Path == path {
				return
			}
		}
		targets = append(targets, doneGateTestTarget{Path: path, Framework: "go"})
	}

	for _, touched := range profile.TouchedPaths {
		if !doneGateTouchedPathSuggestsGoTest(touched) {
			continue
		}
		absTouched := strings.TrimSpace(touched)
		if absTouched == "" {
			continue
		}
		if !filepath.IsAbs(absTouched) {
			absTouched = filepath.Join(workDir, filepath.FromSlash(absTouched))
		}
		absTouched = filepath.Clean(absTouched)

		for _, moduleDir := range profile.GoModules {
			moduleDir = filepath.Clean(moduleDir)
			if !pathWithinDir(moduleDir, absTouched) {
				continue
			}
			targetDir := filepath.Dir(absTouched)
			base := strings.ToLower(filepath.Base(absTouched))
			if base == "go.mod" || base == "go.sum" {
				targetDir = moduleDir
			}
			add(targetDir)
			break
		}
	}

	if len(targets) > 0 {
		return targets
	}
	return doneGateFrameworkTargets(profile.GoModules, "go", maxTargets)
}

func doneGateFrameworkTargets(paths []string, framework string, maxTargets int) []doneGateTestTarget {
	targets := make([]doneGateTestTarget, 0, maxTargets)
	for _, path := range paths {
		if len(targets) >= maxTargets {
			break
		}
		if strings.TrimSpace(path) == "" {
			continue
		}
		targets = append(targets, doneGateTestTarget{Path: path, Framework: framework})
	}
	return targets
}

func doneGateTouchedPathSuggestsGoTest(path string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(path)))
	return filepath.Ext(base) == ".go" || base == "go.mod" || base == "go.sum"
}

// moduleHasTouchedFile reports whether any path in touchedPaths lies
// inside moduleDir. Used by BuildChecks to skip modules that
// were untouched by this turn — avoids running `cargo check` /
// `composer lint` / `npm test` for unrelated modules on a monorepo.
//
// If touchedPaths is empty, returns true (safety fallback — empty
// touched list means "don't narrow"). Paths can be absolute or
// relative to workDir; the helper handles both.
func moduleHasTouchedFile(moduleDir, workDir string, touchedPaths []string) bool {
	if len(touchedPaths) == 0 {
		return true
	}
	moduleDir = filepath.Clean(moduleDir)
	for _, touched := range touchedPaths {
		abs := strings.TrimSpace(touched)
		if abs == "" {
			continue
		}
		if !filepath.IsAbs(abs) && workDir != "" {
			abs = filepath.Join(workDir, filepath.FromSlash(abs))
		}
		abs = filepath.Clean(abs)
		if pathWithinDir(moduleDir, abs) {
			return true
		}
	}
	return false
}

// doneGateGoVetTargets returns the directories that should receive
// `go vet .` based on the files touched this turn. Mirrors
// doneGateGoTestTargets — walks TouchedPaths, finds directory of each
// *.go file, dedupes, caps at maxTargets. Falls back to empty list
// (caller does module-wide vet) if nothing Go-like was touched.
func doneGateGoVetTargets(profile Profile, maxTargets int) []doneGateTestTarget {
	if maxTargets <= 0 || len(profile.GoModules) == 0 || len(profile.TouchedPaths) == 0 {
		return nil
	}
	workDir := strings.TrimSpace(profile.WorkDir)
	if workDir == "" {
		return nil
	}

	var targets []doneGateTestTarget
	seen := make(map[string]bool)
	for _, touched := range profile.TouchedPaths {
		if !doneGateTouchedPathSuggestsGoTest(touched) {
			continue
		}
		abs := strings.TrimSpace(touched)
		if abs == "" {
			continue
		}
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workDir, filepath.FromSlash(abs))
		}
		abs = filepath.Clean(abs)

		for _, moduleDir := range profile.GoModules {
			moduleDir = filepath.Clean(moduleDir)
			if !pathWithinDir(moduleDir, abs) {
				continue
			}
			// `go vet .` runs for the package in the current directory.
			// For go.mod / go.sum changes, vet the whole module.
			targetDir := filepath.Dir(abs)
			base := strings.ToLower(filepath.Base(abs))
			if base == "go.mod" || base == "go.sum" {
				targetDir = moduleDir
			}
			if seen[targetDir] {
				break
			}
			seen[targetDir] = true
			targets = append(targets, doneGateTestTarget{Path: targetDir, Framework: "go"})
			if len(targets) >= maxTargets {
				return targets
			}
			break
		}
	}
	return targets
}

func pathWithinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func relPathOrDot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return "."
	}
	return rel
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func doneGateCheckEvidence(name, command string) string {
	name = strings.TrimSpace(name)
	command = strings.TrimSpace(command)

	if name == "verify_code" {
		return "verify code"
	}
	if name == "git_diff_check" {
		return "git diff --check"
	}
	if name == "git_unmerged_paths" {
		return "git unmerged paths check"
	}
	if strings.HasPrefix(name, "run_tests@") {
		parts := strings.Split(name, "@")
		if len(parts) >= 3 {
			return strings.TrimSpace("run tests " + parts[1] + " " + parts[2])
		}
		return "run tests"
	}

	prefixes := []struct {
		prefix string
		label  string
	}{
		{"go_vet@", "go vet"},
		{"cargo_check@", "cargo check"},
		{"java_verify@", "java verify"},
		{"cmake_configure@", "cmake build"},
		{"bazel_nobuild@", "bazel build --nobuild"},
		{"make_dry_run@", "make dry-run"},
		{"php_lint@", "composer lint"},
		{"php_static@", "composer static"},
		{"composer_validate@", "composer validate"},
		{"node_lint@", "node lint"},
		{"node_typecheck@", "node typecheck"},
		{"node_test@", "node test"},
		{"node_build@", "node build"},
		{"python_compile@", "python compile"},
	}
	for _, p := range prefixes {
		if rel, ok := strings.CutPrefix(name, p.prefix); ok {
			rel = strings.TrimSpace(rel)
			if rel == "" || rel == "." {
				return p.label
			}
			return p.label + " " + rel
		}
	}

	return summarizeDoneGateCommand(command)
}

func doneGateCheckDisplayName(check Check) string {
	label := strings.TrimSpace(check.Evidence)
	if label == "" {
		label = strings.TrimSpace(check.Name)
	}
	if label == "" {
		return "verification"
	}
	label = strings.ReplaceAll(label, "_", " ")
	return label
}

func ResultDisplayName(result Result) string {
	label := strings.TrimSpace(result.DisplayName)
	if label == "" {
		label = strings.TrimSpace(result.Name)
	}
	if label == "" {
		return "verification"
	}
	return strings.ReplaceAll(label, "_", " ")
}

func summarizeDoneGateCommand(command string) string {
	command = strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
	if command == "" {
		return ""
	}
	runes := []rune(command)
	if len(runes) <= 90 {
		return command
	}
	return string(runes[:87]) + "..."
}

func looksLikeCodingTask(msg string) bool {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" {
		return false
	}
	// Change verbs only. Question words are deliberately absent: bash is also
	// how the agent investigates, and enforcing a build because someone asked
	// "why does this crash" would run verification during pure analysis — the
	// same reason IsImplementationTool excludes bash. The list is vocabulary,
	// which guesses at intent rather than reading what happened, and that is a
	// deliberate trade: the evidence-based alternatives ("the command was not
	// read-only", "git reports a dirty tree") both fire on turns that changed
	// nothing, and in a repository whose build is ALREADY broken that turns a
	// question into a blocked turn. Over-verifying costs time; blocking work
	// that never needed checking costs the work.
	keywords := []string{
		"implement", "fix", "refactor", "update", "change", "bug", "build", "lint", "compile",
		"add", "remove", "delete", "rename", "write", "create", "migrat", "generat",
		"format", "patch", "revert", "extract", "replace", "clean up", "make it",
		"доработ", "исправ", "рефактор", "обнов", "помен", "ошиб", "сборк", "линт", "код",
		"почин", "перепиш", "добав", "убер", "удал", "создай", "напиш", "переимен",
		"компилир", "генер", "формат", "миграц", "вынес", "замен", "почист", "внедр",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func BuildFixPrompt(userMessage string, failed []Result, attempt, max int) string {
	var sb strings.Builder
	sb.WriteString("The hard done-gate failed. Autonomously fix the code until checks pass.\n")
	fmt.Fprintf(&sb, "Auto-fix attempt %d/%d.\n\n", attempt, max)
	sb.WriteString("Original user task:\n")
	sb.WriteString(userMessage)
	sb.WriteString("\n\nFailed checks:\n")
	for _, r := range failed {
		fmt.Fprintf(&sb, "- %s failed.\n", ResultDisplayName(r))
		if r.Error != "" {
			sb.WriteString("  Error: ")
			sb.WriteString(truncateDoneGateText(r.Error))
			sb.WriteString("\n")
		}
		if r.Content != "" {
			sb.WriteString("  Output: ")
			sb.WriteString(truncateDoneGateText(r.Content))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\nRules:\n")
	sb.WriteString("- Apply minimal deterministic fixes.\n")
	sb.WriteString("- Do not ask the user for input.\n")
	sb.WriteString("- After fixing, stop with a short summary of changes.\n")
	return sb.String()
}

func truncateDoneGateText(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= doneGateOutputLimit {
		return s
	}
	return string(runes[:doneGateOutputLimit]) + "..."
}

func compactDoneGateFailureDetail(r Result) string {
	raw := strings.TrimSpace(r.Error)
	if raw == "" {
		raw = strings.TrimSpace(r.Content)
	}
	if raw == "" {
		return ""
	}

	raw = strings.Join(strings.Fields(raw), " ")
	if raw == "" {
		return ""
	}
	if runes := []rune(raw); len(runes) > 280 {
		return string(runes[:280]) + "..."
	}
	return raw
}
