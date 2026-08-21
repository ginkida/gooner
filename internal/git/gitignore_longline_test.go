package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNestedGitignoreSurvivesAnOverlongLine pins the reason loadFile reads with
// bufio.Reader instead of bufio.Scanner. Scanner stops at its 64KB line cap and
// reports ErrTooLong, and both nested-file callers used to swallow that error —
// so a single long line silently discarded every pattern AFTER it. A dropped
// ignore pattern does not fail loudly: it just makes files the user considers
// ignored start looking like project source, which is how a whole-repo scan
// ends up confidently reporting on a vendored tree.
func TestNestedGitignoreSurvivesAnOverlongLine(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	// One comment line past Scanner's 64KB cap, then a real rule after it.
	overlong := "#" + strings.Repeat("x", 70*1024)
	content := overlong + "\nsecret.txt\n"
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	g := NewGitIgnore(dir)
	if err := g.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !g.IsIgnored(filepath.Join(nested, "secret.txt")) {
		t.Error("the pattern after the overlong line was dropped — the rest of the file never loaded")
	}
}

// TestGitignoreRefusesPathologicalFileLoudly: the bound has to fail rather than
// truncate, because truncation is the same silent pattern loss by another name.
func TestGitignoreRefusesPathologicalFileLoudly(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("a.txt\n", (maxGitignoreBytes/6)+16)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(huge), 0644); err != nil {
		t.Fatal(err)
	}

	g := NewGitIgnore(dir)
	err := g.Load()
	if err == nil {
		t.Fatal("an oversized root .gitignore must fail loudly, not load partially")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should name the bound, got: %v", err)
	}
}

// TestGitignoreStillLoadsOrdinaryFiles is the companion: none of the above may
// cost the normal path anything.
func TestGitignoreStillLoadsOrdinaryFiles(t *testing.T) {
	dir := t.TempDir()
	content := "# comment\n\nbuild/\n*.log\n!keep.log\ntrailing-no-newline.txt"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	g := NewGitIgnore(dir)
	if err := g.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, ignored := range []string{"app.log", "trailing-no-newline.txt"} {
		if !g.IsIgnored(filepath.Join(dir, ignored)) {
			t.Errorf("%s should be ignored", ignored)
		}
	}
	if g.IsIgnored(filepath.Join(dir, "keep.log")) {
		t.Error("keep.log is re-included by a negation and must not be ignored")
	}
	if g.IsIgnored(filepath.Join(dir, "main.go")) {
		t.Error("main.go must not be ignored")
	}
}
