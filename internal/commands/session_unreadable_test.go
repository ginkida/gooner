package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gokin/internal/chat"
)

type fakeAppWithHistory struct {
	*fakeAppForMCP
	hm *chat.HistoryManager
}

func (f *fakeAppWithHistory) GetHistoryManager() (*chat.HistoryManager, error) { return f.hm, nil }

// sessionsDirWithCorruptFile points session storage at a temp directory whose
// only content is one unreadable session file.
func sessionsDirWithCorruptFile(t *testing.T) *chat.HistoryManager {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	dir := filepath.Join(data, "gokin", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "my-work.json"), []byte(`{"id":"my-work",`), 0600); err != nil {
		t.Fatal(err)
	}
	hm, err := chat.NewHistoryManager()
	if err != nil {
		t.Fatal(err)
	}
	return hm
}

// A session file that cannot be read is a conversation the user still has and
// cannot reach. Reporting "no saved sessions" in that situation is not a
// shortfall in detail — it is a false statement about their history, and it is
// the one that makes them stop looking. Both listings must say which file they
// could not read instead.
func TestSessionsCommand_UnreadableSessionIsNotReportedAsNone(t *testing.T) {
	app := &fakeAppWithHistory{fakeAppForMCP: &fakeAppForMCP{}, hm: sessionsDirWithCorruptFile(t)}

	out, err := (&SessionsCommand{}).Execute(context.Background(), nil, app)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "no saved sessions") {
		t.Fatalf("told the user they have no saved sessions while one sits unreadable on disk: %q", out)
	}
	if !strings.Contains(out, "my-work.json") {
		t.Fatalf("output must name the file the user needs to look at: %q", out)
	}
}

func TestResumeCommand_UnreadableSessionIsNotReportedAsNone(t *testing.T) {
	app := &fakeAppWithHistory{fakeAppForMCP: &fakeAppForMCP{}, hm: sessionsDirWithCorruptFile(t)}

	out, err := (&ResumeCommand{}).Execute(context.Background(), nil, app)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "no saved sessions") {
		t.Fatalf("told the user they have no saved sessions while one sits unreadable on disk: %q", out)
	}
	if !strings.Contains(out, "my-work.json") {
		t.Fatalf("output must name the file the user needs to look at: %q", out)
	}
}

func TestUnreadableSessionsNote_SilentWhenNothingWasSkipped(t *testing.T) {
	if got := unreadableSessionsNote(nil); got != "" {
		t.Fatalf("a clean directory must produce no note, got %q", got)
	}
}
