package commands

import (
	"context"
	"strings"
	"testing"
)

// TestUndoGroupFormIsDiscoverable guards the visibility of a capability that
// is otherwise invisible: /undo all and /redo all reach machinery that had
// been written and never called, and nothing in the UI ever hinted that one
// request is more than one change. A feature nobody can find is barely
// shipped, so the three surfaces a user actually reads — the Usage block, the
// worked examples under it, and the palette's argument hint — must all name
// the group form. This drives the real /help path rather than asserting on the
// string constants, so a change to how help is assembled is caught too.
func TestUndoGroupFormIsDiscoverable(t *testing.T) {
	h := NewHandler()
	app := &fakeAppForMCP{}

	for _, tc := range []struct{ name, wantExample string }{
		{"undo", "/undo all"},
		{"redo", "/redo all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&HelpCommand{handler: h}).Execute(context.Background(), []string{tc.name}, app)
			if err != nil {
				t.Fatalf("/help %s: %v", tc.name, err)
			}
			usage, examples, split := strings.Cut(out, "Examples:")
			if !split {
				t.Fatalf("/help %s has no Examples block:\n%s", tc.name, out)
			}
			if !strings.Contains(usage, tc.wantExample) {
				t.Errorf("Usage block omits %q:\n%s", tc.wantExample, usage)
			}
			if !strings.Contains(examples, tc.wantExample) {
				t.Errorf("Examples block omits %q — the part users actually scan:\n%s", tc.wantExample, examples)
			}

			cmd, ok := h.GetCommand(tc.name)
			if !ok {
				t.Fatalf("%s is not registered", tc.name)
			}
			mp, ok := cmd.(MetadataProvider)
			if !ok {
				t.Fatalf("%s exposes no metadata, so the palette cannot hint its arguments", tc.name)
			}
			if hint := mp.GetMetadata().ArgHint; !strings.Contains(hint, "all") {
				t.Errorf("palette ArgHint omits the group form: %q", hint)
			}
		})
	}
}
