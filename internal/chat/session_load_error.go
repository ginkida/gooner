package chat

import "errors"

// SessionLoadErrorKind is the stable machine-readable failure class returned
// by SessionManager.LoadSession.
type SessionLoadErrorKind string

const (
	SessionLoadKindInvalidID           SessionLoadErrorKind = "invalid_id"
	SessionLoadKindNotFound            SessionLoadErrorKind = "not_found"
	SessionLoadKindCorrupt             SessionLoadErrorKind = "corrupt"
	SessionLoadKindIdentityMismatch    SessionLoadErrorKind = "identity_mismatch"
	SessionLoadKindForeignProject      SessionLoadErrorKind = "foreign_project"
	SessionLoadKindPersistenceDisabled SessionLoadErrorKind = "persistence_disabled"
	SessionLoadKindStorage             SessionLoadErrorKind = "storage"
)

// SessionLoadError classifies a LoadSession failure without discarding its
// original chain. Error deliberately preserves the wrapped human-readable
// message, while Unwrap keeps errors.Is/errors.As (including os.ErrNotExist
// and json errors) working for existing callers.
type SessionLoadError struct {
	Kind      SessionLoadErrorKind `json:"kind"`
	SessionID string               `json:"session_id,omitempty"`
	Err       error                `json:"-"`
}

func (e *SessionLoadError) Error() string {
	if e == nil {
		return "session load failed"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "session load failed: " + string(e.Kind)
}

func (e *SessionLoadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AsSessionLoadError returns the first typed session-load classification in
// err's chain. It is a convenience wrapper around errors.As.
func AsSessionLoadError(err error) (*SessionLoadError, bool) {
	var loadErr *SessionLoadError
	if !errors.As(err, &loadErr) {
		return nil, false
	}
	return loadErr, true
}

// SessionLoadErrorKindOf extracts only the stable kind for callers that do
// not need the session identity or underlying error.
func SessionLoadErrorKindOf(err error) (SessionLoadErrorKind, bool) {
	loadErr, ok := AsSessionLoadError(err)
	if !ok {
		return "", false
	}
	return loadErr.Kind, true
}

func wrapSessionLoadError(kind SessionLoadErrorKind, sessionID string, err error) error {
	return &SessionLoadError{Kind: kind, SessionID: sessionID, Err: err}
}

var (
	errSessionStateCorrupt     = errors.New("session state is corrupt")
	errSessionIdentityMismatch = errors.New("session identity mismatch")
	errSessionFileTooLarge     = errors.New("session file is too large")
)

// ErrSessionStateTooLarge marks the one save failure the user can actually
// resolve. Disk and permission failures are external and usually transient;
// this one is self-inflicted, permanent until the history shrinks, and
// invisible — compaction is driven by the context limit, so on a
// million-token model a session can sit unsaveable for hours while every
// autosave silently fails. Exported so the UI can name the cause and point at
// the action that fixes it instead of guessing at disk space.
var ErrSessionStateTooLarge = errors.New("session state is too large to persist")
