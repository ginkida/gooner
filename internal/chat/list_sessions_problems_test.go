package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ListSessions skips two kinds of file, and both used to vanish without a
// trace: one that cannot be parsed, and one whose embedded id names a
// different session than its filename — the redirect the filename/id check
// exists to refuse. Skipping is right in both cases; silence is not, because
// the caller then reports an empty list as "no saved sessions".
func TestListSessionsWithProblems_NamesEverySkippedFile(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	dir := filepath.Join(data, "gokin", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	good := SessionState{ID: "good", StartTime: time.Now(), LastActive: time.Now()}
	body, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"good.json":      body,
		"truncated.json": []byte(`{"id":"truncated",`),
		"redirect.json":  []byte(`{"id":"good"}`),
		"notes.txt":      []byte("not a session"),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}

	hm, err := NewHistoryManager()
	if err != nil {
		t.Fatal(err)
	}
	sessions, problems, err := hm.ListSessionsWithProblems()
	if err != nil {
		t.Fatalf("ListSessionsWithProblems: %v", err)
	}

	if len(sessions) != 1 || sessions[0].ID != "good" {
		ids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		t.Fatalf("the readable session must survive its damaged neighbours, got %v", ids)
	}
	for _, want := range []string{"truncated.json", "redirect.json"} {
		found := false
		for _, p := range problems {
			if strings.Contains(p.Error(), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s was skipped without saying so; problems=%v", want, problems)
		}
	}
	for _, p := range problems {
		if strings.Contains(p.Error(), "notes.txt") {
			t.Errorf("a non-.json file is not a session and must not be reported: %v", p)
		}
	}

	// The single-return form stays a drop-in for its existing callers.
	plain, err := hm.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(plain) != len(sessions) {
		t.Fatalf("ListSessions returned %d sessions, ListSessionsWithProblems %d", len(plain), len(sessions))
	}
}
