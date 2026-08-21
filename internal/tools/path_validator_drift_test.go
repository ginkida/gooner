package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Adding a tool is a fourteen-step checklist, and the security-relevant step —
// "every tool that takes a model-supplied path must validate it" — was until
// now enforced by reading that checklist. It has failed that way before:
// refactor carried a pathValidator field it never called, which is worse than
// carrying none, because the field makes the file look guarded. The per-tool
// escape tests that exist (execution_path_scope, path_containment,
// semantic_tools) each cover the tools that existed when they were written;
// none of them notices tool sixty landing without a validator.
//
// This is the drift guard for that step, in the same spirit as the policy-name
// drift tests: it does not re-check what the existing tests already prove, it
// only refuses to let a NEW filesystem-touching tool appear unguarded and
// unexplained.
var filesystemCallPattern = regexp.MustCompile(
	`os\.(Open|OpenFile|Create|ReadFile|WriteFile|Remove|RemoveAll|MkdirAll|Mkdir|Rename|Stat|Lstat)\b|AtomicWrite\(`)

// pathValidatorExempt lists the files that reach the filesystem WITHOUT a
// PathValidator, each with the reason it is safe. An entry is a claim about
// where the path comes from — if that stops being true, the entry is wrong.
var pathValidatorExempt = map[string]string{
	"bash.go": "creates only its own temp dir and stats cd targets; the command itself is gated by the " +
		"permission layer rather than by path validation",
	"task_output.go": "opens the path the task manager recorded for a task ID; the model supplies the ID, " +
		"never the path",
	"executor.go": "the dispatcher, not a tool: its post-success read uses paths a validated tool has just " +
		"written, behind a result.Success gate",
}

func TestToolsTouchingTheFilesystemValidateTheirPaths(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	var unguarded []string
	seenExempt := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		if !strings.Contains(text, ") Execute(ctx") {
			continue
		}
		if !filesystemCallPattern.MatchString(text) {
			continue
		}
		if strings.Contains(text, "pathValidator") {
			continue
		}
		if _, ok := pathValidatorExempt[name]; ok {
			seenExempt[name] = true
			continue
		}
		unguarded = append(unguarded, name)
	}

	if len(unguarded) > 0 {
		t.Errorf("these files run an Execute and touch the filesystem with no PathValidator:\n  %s\n\n"+
			"Give the tool a pathValidator and route every model-supplied path through it "+
			"(see read.go for the plain form, git_diff.go for validateGitPath). If the path cannot "+
			"come from the model, add the file to pathValidatorExempt WITH the reason.",
			strings.Join(unguarded, "\n  "))
	}

	// A stale exemption is its own hazard: it keeps vouching for a file that may
	// have been renamed, deleted, or since given a validator, and reads as
	// coverage while covering nothing.
	for name := range pathValidatorExempt {
		if !seenExempt[name] {
			t.Errorf("pathValidatorExempt still vouches for %q, which no longer matches an "+
				"unguarded filesystem-touching tool — drop the entry", name)
		}
	}
}
