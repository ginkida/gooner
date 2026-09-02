package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The other half of the shortcuts promise: a Ctrl chord on the card must be a
// Ctrl chord some handler actually looks at.
//
// This guard covers Ctrl chords and nothing else, deliberately. The remaining
// entries resist a source scan for reasons worth writing down rather than
// papering over: single-letter keys (y, n, A, R, E, j, k) appear in Go source
// everywhere, so finding one proves nothing, and Alt+Enter is matched by the
// `msg.Alt` flag inside the KeyEnter case, so no "alt+enter" literal exists to
// find. Claiming to check those would be worse than not checking them — a
// green test nobody can trust is how an invented binding survives review. All
// 45 entries were verified by hand when this guard was written; what is
// automated here is the subset where automation is honest.
func TestAdvertisedCtrlChordsHaveHandlers(t *testing.T) {
	source := readPackageSource(t)

	checked := 0
	for _, category := range DefaultShortcuts() {
		for _, sc := range category.Shortcuts {
			key := strings.Join(sc.Keys, " ")
			func() {
				chord, ok := ctrlChord(key)
				if !ok {
					return
				}
				checked++
				// Bubble Tea accepts either form, and this package uses both.
				literal := "\"ctrl+" + chord + "\""
				constant := "KeyCtrl" + strings.ToUpper(chord)
				if !strings.Contains(source, literal) && !strings.Contains(source, constant) {
					t.Errorf("%q is advertised under %q (%s) but no handler mentions %s or %s",
						key, category.Name, sc.Description, literal, constant)
				}
			}()
		}
	}
	if checked < 10 {
		t.Fatalf("only %d Ctrl chords found in the overlay; the guard has stopped looking at the thing it guards", checked)
	}
}

// ctrlChord turns a display form — the Keys slice joined, e.g. {"Ctrl","b"} —
// into its single letter.
func ctrlChord(key string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(key), "Ctrl")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "+"))
	if len(rest) != 1 {
		return "", false
	}
	return strings.ToLower(rest), true
}

func readPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		t.Fatal("read no package source; the guard would pass on an empty read")
	}
	return b.String()
}
