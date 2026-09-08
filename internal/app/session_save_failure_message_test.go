package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gokin/internal/chat"
)

// The toast is the only place this failure becomes visible, so what it names
// as the cause decides where the user looks. Sending someone to check disk
// space for a conversation that outgrew the save limit costs them the search
// and the history both.
func TestSessionSaveFailureMessage_NamesTheCauseItCanFix(t *testing.T) {
	err := fmt.Errorf("saving session: %w", chat.ErrSessionStateTooLarge)

	msg := sessionSaveFailureMessage(err)

	if strings.Contains(msg, "disk space") {
		t.Fatalf("sent the user to check their disk for a size problem they can fix: %q", msg)
	}
	if !strings.Contains(msg, "/compact") {
		t.Fatalf("must point at the action available now: %q", msg)
	}
}

// Every other failure is external, and guessing at disk or permissions is the
// right guess there — dropping that hint would trade one wrong answer for no
// answer.
func TestSessionSaveFailureMessage_KeepsTheHintForExternalFailures(t *testing.T) {
	msg := sessionSaveFailureMessage(errors.New("open sessions/x.json: permission denied"))

	if !strings.Contains(msg, "disk space") {
		t.Fatalf("an external failure still needs its hint: %q", msg)
	}
	if strings.Contains(msg, "/compact") {
		t.Fatalf("compacting cannot fix a permission error: %q", msg)
	}
}
