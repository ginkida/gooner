package commands

import (
	"strings"
	"testing"

	"gokin/internal/ui"
)

// The shortcuts overlay is a promise made to the user's fingers, and part of it
// is made in slash commands. A card that offers /cost when no such command is
// registered fails silently — the user types it, gets "unknown command", and
// learns not to trust the card. gokin has shipped this exact class before: the
// welcome panel advertised "@ to pin a file" while nothing implemented it, and
// /reasoning sat in autocomplete after being removed. The rule against it lives
// in a checklist, which is what let those two through.
//
// This test runs in the commands package because commands imports ui and not
// the other way round; the chord half of the same promise is pinned in
// internal/ui, where the handlers are.
func TestAdvertisedSlashCommandsAreRegistered(t *testing.T) {
	h := NewHandler()
	checked := 0
	for _, category := range ui.DefaultShortcuts() {
		for _, sc := range category.Shortcuts {
			for _, key := range sc.Keys {
				name, isCommand := strings.CutPrefix(strings.TrimSpace(key), "/")
				if !isCommand || name == "" {
					continue
				}
				checked++
				if _, _, ok := h.Parse("/" + name); !ok {
					t.Errorf("%q is advertised under %q (%s) but no such command is registered",
						key, category.Name, sc.Description)
				}
			}
		}
	}
	// A sweep that finds nothing has proven nothing: if the overlay stops
	// listing commands entirely, this test must say so rather than pass.
	if checked == 0 {
		t.Fatal("no slash commands found in the shortcuts overlay — the guard is no longer looking at anything")
	}
}
