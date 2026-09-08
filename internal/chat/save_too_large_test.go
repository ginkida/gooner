package chat

import (
	"errors"
	"strings"
	"testing"
)

// A conversation that outgrows the persistence limit is the one save failure
// the user can resolve, and it is permanent: every later autosave fails the
// same way, while compaction runs on the context limit rather than on file
// size. The UI can only say so if the cause survives as a typed error, and a
// sentinel that is declared but never actually returned by the real Save path
// would leave the message dead while looking implemented.
func TestSaveFull_OversizedHistoryIsTypedAsTooLarge(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	hm, err := NewHistoryManager()
	if err != nil {
		t.Fatal(err)
	}

	session := NewSession()
	session.SetID("oversized-session")
	session.AddUserMessage(strings.Repeat("x", int(maxSessionFileBytes)+1))

	err = hm.SaveFull(session)
	if err == nil {
		t.Fatal("a history over the persistence limit must not report success")
	}
	if !errors.Is(err, ErrSessionStateTooLarge) {
		t.Fatalf("the cause must survive as a typed error so the UI can name it, got: %v", err)
	}
}

// The limit must not fire on an ordinary conversation, or every session would
// be told to compact itself.
func TestSaveFull_OrdinaryHistorySavesCleanly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	hm, err := NewHistoryManager()
	if err != nil {
		t.Fatal(err)
	}

	session := NewSession()
	session.SetID("ordinary-session")
	for range 20 {
		session.AddUserMessage(strings.Repeat("a normal turn of conversation. ", 200))
	}

	if err := hm.SaveFull(session); err != nil {
		t.Fatalf("an ordinary conversation must save: %v", err)
	}
}
