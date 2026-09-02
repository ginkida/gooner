package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agent-type files are read at boot from two directories the user controls.
// Two properties matter and neither is obvious from the happy path: one
// unreadable file must cost only itself, and a file too large to be an agent
// definition must be refused rather than turned into a system prompt that
// rides along on every request the agent makes.

func writeAgentTypeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadedNames(types []*DynamicAgentType) []string {
	names := make([]string, 0, len(types))
	for _, a := range types {
		names = append(names, a.Name)
	}
	return names
}

func TestLoadAgentTypeFiles_OneBadFileCostsOnlyItself(t *testing.T) {
	dir := writeAgentTypeFiles(t, map[string]string{
		"good.md":         "---\ndescription: a good agent\ntools: [read, grep]\n---\nYou are a helpful explorer.\n",
		"empty.md":        "",
		"only_fence.md":   "---\n",
		"unterminated.md": "---\ndescription: x\nno closing fence\n",
		"broken_yaml.md":  "---\ndescription: [unclosed\n---\nbody\n",
		"no_body.md":      "---\ndescription: x\n---\n",
		"binary.md":       "\x00\x01\xff",
		"bad_tools.md":    "---\ndescription: x\ntools: \"not-a-list\"\n---\nbody\n",
		"general.md":      "---\ndescription: shadows a builtin\n---\nbody\n",
		"notmarkdown.txt": "---\ndescription: wrong extension\n---\nbody\n",
	})

	loaded, warnings := LoadAgentTypeFiles(NewAgentTypeRegistry(), dir, "")

	if got := loadedNames(loaded); len(got) != 1 || got[0] != "good" {
		t.Fatalf("the valid agent type must survive its malformed neighbours, loaded=%v", got)
	}
	// Every malformed file must name itself, so the user can find and fix it.
	// A count alone would pass even if one warning were emitted nine times.
	for _, name := range []string{
		"empty.md", "only_fence.md", "unterminated.md", "broken_yaml.md",
		"no_body.md", "binary.md", "bad_tools.md", "general.md",
	} {
		found := false
		for _, w := range warnings {
			if strings.Contains(w, name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s was rejected without saying so; warnings=%v", name, warnings)
		}
	}
	for _, w := range warnings {
		if strings.Contains(w, "notmarkdown.txt") {
			t.Errorf("a non-.md file is not an agent type and must not be reported: %s", w)
		}
	}
}

func TestLoadAgentTypeFiles_OversizedPromptIsRefusedNotLoaded(t *testing.T) {
	dir := writeAgentTypeFiles(t, map[string]string{
		"small.md": "---\ndescription: normal\n---\nYou are helpful.\n",
		"huge.md": "---\ndescription: a document misfiled into the agents directory\n---\n" +
			strings.Repeat("x", maxAgentTypeBytes+1),
	})

	loaded, warnings := LoadAgentTypeFiles(NewAgentTypeRegistry(), dir, "")

	for _, a := range loaded {
		if a.Name == "huge" {
			t.Fatalf("an oversized file became a system prompt of %d bytes, "+
				"which every request of this agent would then carry", len(a.SystemPrompt))
		}
	}
	if got := loadedNames(loaded); len(got) != 1 || got[0] != "small" {
		t.Fatalf("refusing the oversized file must not disturb its neighbour, loaded=%v", got)
	}
	// Silence here is the failure mode being guarded: the user's file simply
	// would not work, with nothing on screen explaining why.
	said := false
	for _, w := range warnings {
		if strings.Contains(w, "huge.md") && strings.Contains(w, "limit") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the refusal must name the file and the limit; warnings=%v", warnings)
	}
}
