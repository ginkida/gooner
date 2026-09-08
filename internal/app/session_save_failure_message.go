package app

import (
	"errors"

	"gokin/internal/chat"
)

// sessionSaveFailureMessage names the cause when the code knows it.
//
// The default text guesses — "check disk space / permissions" — which is the
// right guess for an external failure and the wrong one for the single cause
// the user can actually resolve. A history too large to persist is not
// transient: every later autosave fails the same way, and compaction is driven
// by the context limit rather than by file size, so on a million-token model
// the session can stay unsaveable for hours. Telling that user to check their
// disk sends them looking in the wrong place for something they could fix in
// one command.
func sessionSaveFailureMessage(err error) string {
	if errors.Is(err, chat.ErrSessionStateTooLarge) {
		return "Session autosave is failing: the conversation is too large to save. " +
			"Run /compact to shrink it (or /clear to start fresh) — until then this " +
			"history will not persist."
	}
	return "Session autosave is failing — history may not persist (check disk space / permissions)"
}
