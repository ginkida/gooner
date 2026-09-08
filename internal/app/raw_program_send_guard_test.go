package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every UI message must travel through safeSendToProgram, which reads the
// program reference under the lock, nil-checks it, and recovers from the panic
// that a send to a closed channel raises during shutdown. A raw Program.Send
// has none of that, and the failure it produces is a crash on the way out —
// the least debuggable moment there is.
//
// Two sites are allowed to make that call, and the rule saying so has lived in
// prose. Prose did not hold: v0.85.10 caught pattern_detector.go adding a third
// after the original 44 call sites were migrated. This is the same rule with a
// test under it.
//
// The exemptions are checked in both directions. A stale entry — a sanctioned
// site that no longer sends — fails too, because an allowance nobody needs is
// how the next real one gets waved through.
var sanctionedRawProgramSend = map[string]string{
	"app.go":                  "sendProgramMessage is the one place the send is made; safeSendToProgram delegates to it",
	"ui_event_broadcaster.go": "the broadcaster's own self-recovering send, which owns its recovery",
}

func TestNoRawProgramSendOutsideTheTwoSanctionedSites(t *testing.T) {
	// A call on any receiver, so a rename of the variable cannot slip past.
	call := regexp.MustCompile(`\b\w+\.Send\(`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// A mention in a comment is not a call; pattern_detector.go carries
			// one explaining why it does not make it.
			if strings.HasPrefix(trimmed, "//") || !call.MatchString(trimmed) {
				continue
			}
			if strings.Contains(trimmed, "SendMessage") || strings.Contains(trimmed, "safeSendToProgram") {
				continue
			}
			if _, ok := sanctionedRawProgramSend[name]; ok {
				found[name] = true
				continue
			}
			t.Errorf("%s:%d makes a raw Program.Send — route it through safeSendToProgram:\n  %s",
				name, i+1, trimmed)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source; the guard would pass on an empty read")
	}
	for name, why := range sanctionedRawProgramSend {
		if !found[name] {
			t.Errorf("%s is exempt (%s) but no longer makes the call; drop the exemption "+
				"rather than leaving a door open for the next one", name, why)
		}
	}
}
