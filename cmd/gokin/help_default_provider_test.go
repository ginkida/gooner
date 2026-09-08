package main

import (
	"regexp"
	"strings"
	"testing"

	"gokin/internal/config"
)

// The first sentence of `gokin --help` names a default provider, and it named
// the wrong one: it still said Kimi after the default moved to GLM in v0.98.0.
// A stale line here is not cosmetic — it is the first thing a new user reads,
// and it tells them which key to go get. Nothing checked it, because prose in
// a help string is not code until something reads it.
func TestHelpNamesTheProviderThatIsActuallyTheDefault(t *testing.T) {
	long := rootLongHelp
	if long == "" {
		t.Fatal("root help is empty; the guard is looking at nothing")
	}

	// "Supports GLM\n(default), Kimi, ..." — the name may sit on the previous
	// line, so match across the break.
	claim := regexp.MustCompile(`(?s)Supports\s+(\S+)\s*\n?\s*\(default\)`)
	m := claim.FindStringSubmatch(long)
	if m == nil {
		t.Fatalf("help no longer names a default provider; drop this guard or restore the claim:\n%s", long)
	}

	want := config.DefaultConfig().API.Backend
	if got := strings.ToLower(strings.TrimSpace(m[1])); got != strings.ToLower(want) {
		t.Fatalf("help calls %q the default while a fresh config resolves to %q; "+
			"the user is told which key to obtain by this line", m[1], want)
	}
}
