package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gokin/internal/config"
)

// The audit log is enabled by default and writes on every tool call, and
// nothing in the app can read it: no command lists it, and its query API has
// never had a production caller. A subsystem that costs disk on every session
// and cannot be found is worse than one that does not exist — the user pays
// for it and never sees it. The line in /stats is the whole interface.
func TestFormatAuditLogLocation_NamesTheFilesWhenTheyExist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	auditDir := filepath.Join(dir, "gokin", "audit")
	if err := os.MkdirAll(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session-a.json", "session-b.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(auditDir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	cfg.Audit.RetentionDays = 30

	out := formatAuditLogLocation(cfg)

	if out == "" {
		t.Fatal("wrote audit files and reported nothing; the user cannot find what they are paying for")
	}
	if !strings.Contains(out, auditDir) {
		t.Errorf("must name the directory, got: %q", out)
	}
	if !strings.Contains(out, "2") {
		t.Errorf("must count the session files and only those, got: %q", out)
	}
	if !strings.Contains(out, "30") {
		t.Errorf("must say how long they are kept, got: %q", out)
	}
}

// Silence is right in the two cases where a line would be noise or a lie.
func TestFormatAuditLogLocation_SilentWhenThereIsNothingToPointAt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	enabled := &config.Config{}
	enabled.Audit.Enabled = true
	if got := formatAuditLogLocation(enabled); got != "" {
		t.Errorf("no audit directory exists yet; a location line would point at nothing: %q", got)
	}

	if err := os.MkdirAll(filepath.Join(dir, "gokin", "audit"), 0700); err != nil {
		t.Fatal(err)
	}
	off := &config.Config{}
	off.Audit.Enabled = false
	if got := formatAuditLogLocation(off); got != "" {
		t.Errorf("audit is off; reporting a log would misdescribe what is running: %q", got)
	}
}

// The formatter and the command are two ends of the same promise, and testing
// only the ends leaves the wire between them free to be cut — removing the
// call site left every other test in this file green. This exercises /stats
// itself.
func TestStatsCommand_ShowsWhereTheAuditLogLives(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	auditDir := filepath.Join(dir, "gokin", "audit")
	if err := os.MkdirAll(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auditDir, "session-a.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Audit.Enabled = true
	cfg.Audit.RetentionDays = 30
	app := newAuthApp(cfg)

	out, err := (&StatsCommand{}).Execute(context.Background(), nil, app)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, auditDir) {
		t.Fatalf("/stats must point at the audit files, since nothing else in the app can:\n%s", out)
	}
}
