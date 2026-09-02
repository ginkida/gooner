package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gokin/internal/config"
)

// repoWithCommit builds a real repository with one committed file, because the
// claim under test is about what git does and does not report.
func repoWithCommit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func diffOutput(t *testing.T, dir string, args []string) string {
	t.Helper()
	app := newAuthApp(&config.Config{})
	app.fakeAppForMCP.workDir = dir
	out, err := (&DiffCommand{}).Execute(context.Background(), args, app)
	if err != nil {
		t.Fatalf("Execute(%v): %v", args, err)
	}
	return out
}

// `git diff` never reports untracked files, so an empty diff does not mean the
// tree is clean. Telling someone their tree is clean while the file the agent
// just wrote sits beside them is a false statement about their repository, and
// it is the kind that ends the search: they conclude nothing was written.
func TestDiff_UntrackedFileIsNotACleanTree(t *testing.T) {
	dir := repoWithCommit(t)
	if err := os.WriteFile(filepath.Join(dir, "brand_new.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := diffOutput(t, dir, nil)

	if strings.Contains(out, "Working tree is clean") {
		t.Fatalf("called the tree clean while an untracked file sat in it: %q", out)
	}
	if !strings.Contains(out, "brand_new.go") {
		t.Fatalf("output must name the file git is not tracking: %q", out)
	}
}

// The message must still be available when it is true, or the fix would have
// traded a false claim for no claim.
func TestDiff_GenuinelyCleanTreeStillSaysSo(t *testing.T) {
	out := diffOutput(t, repoWithCommit(t), nil)

	if !strings.Contains(out, "Working tree is clean") {
		t.Fatalf("a tree with nothing in it must still be reported as clean: %q", out)
	}
}

// Deliberate exemption: an untracked file is by definition not staged, so
// "nothing staged" stays true, and its hint already points at `git add`.
func TestDiff_StagedViewIgnoresUntrackedFiles(t *testing.T) {
	dir := repoWithCommit(t)
	if err := os.WriteFile(filepath.Join(dir, "brand_new.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := diffOutput(t, dir, []string{"--staged"})

	if !strings.Contains(out, "Nothing staged") {
		t.Fatalf("the staged view answers about the index, not the working tree: %q", out)
	}
}

// A named file that git has never seen produces an empty diff for a reason the
// user cannot guess from "No changes in brand_new.go".
func TestDiff_NamedUntrackedFileExplainsTheEmptyDiff(t *testing.T) {
	dir := repoWithCommit(t)
	if err := os.WriteFile(filepath.Join(dir, "brand_new.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := diffOutput(t, dir, []string{"brand_new.go"})

	if strings.Contains(out, "No changes in") {
		t.Fatalf("a file git never saw has not \"no changes\": %q", out)
	}
	if !strings.Contains(out, "not tracking") {
		t.Fatalf("output must explain why the diff is empty: %q", out)
	}
}
