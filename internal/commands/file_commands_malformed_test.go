package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Command files are authored by hand and checked into repos, so the realistic
// corruption is not hostile input but an ordinary mistake: a missing closing
// fence, a typo'd list in the frontmatter, a file saved empty, a document
// misfiled into the directory. The promise that matters is that ONE bad file
// costs only itself — if a typo could take the whole custom-command set down,
// the failure would look like "my commands stopped working" with nothing
// pointing at the file responsible.
func TestOneMalformedCommandFileCostsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	write("good.md", "---\ndescription: a good one\n---\nDo the thing with $ARGUMENTS\n")
	write("empty.md", "")
	write("no_body.md", "---\ndescription: x\n---\n")
	write("only_fence.md", "---\n")
	write("unterminated.md", "---\ndescription: x\nBody without a closing fence\n")
	write("broken_yaml.md", "---\ndescription: [unclosed\n---\nbody\n")
	write("clear.md", "---\ndescription: shadows a builtin\n---\nbody\n")
	write("huge.md", "---\ndescription: big\n---\n"+strings.Repeat("x", maxFileCommandBytes+1))

	loaded, warnings := NewHandler().LoadFileCommands(dir, "")

	names := map[string]bool{}
	for _, c := range loaded {
		names[c.Name()] = true
	}
	if !names["good"] {
		t.Fatalf("the well-formed command did not survive its broken neighbours; loaded: %v", names)
	}
	for _, rejected := range []string{"empty", "no_body", "only_fence", "unterminated", "broken_yaml", "clear", "huge"} {
		if names[rejected] {
			t.Errorf("%q should not have registered", rejected)
		}
	}

	// Every rejection must name the file, or the user cannot find the one to fix.
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"empty.md", "no_body.md", "only_fence.md", "unterminated.md", "broken_yaml.md", "huge.md",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning names %s:\n%s", want, joined)
		}
	}
	// A builtin can never be shadowed, and the user is told why.
	if !strings.Contains(joined, "shadows a built-in") {
		t.Errorf("shadowing a builtin must be reported:\n%s", joined)
	}
	// The size refusal has to carry the number, since "too big" alone leaves the
	// author guessing what to cut to.
	if !strings.Contains(joined, "over the") || !strings.Contains(joined, "limit for a command template") {
		t.Errorf("the oversized file must be refused with its size and the limit:\n%s", joined)
	}
}
