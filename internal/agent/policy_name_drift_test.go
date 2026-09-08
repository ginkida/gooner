package agent

import (
	"sort"
	"testing"

	"gokin/internal/tools"
)

// The tools package guards its own policy sets against naming a tool that does
// not exist; the agent package has four such sets and had none. It cost
// exactly what the class costs: "go_diagnostics" sat in
// broadLoopExplorationTools for three releases. It is a code-intelligence RPC
// method the managed provider calls on its server, never a registered tool, so
// the entry matched nothing — a reasoning-heavy sub-agent navigating with those
// tools got the base loop ceiling the entry was added to raise, and nothing
// failed to say so.
//
// The mirror image is worse and is what the guard is really for: a REAL tool
// entered under a slightly wrong name drops out of whatever gate its author
// meant to put it in, while the tool keeps working.
func TestAgentPolicySetsNameRealTools(t *testing.T) {
	known := map[string]bool{}
	for _, tl := range tools.DefaultRegistry(t.TempDir()).List() {
		known[tl.Name()] = true
	}
	// The lazy registry is the model-facing set; a tool in only one of the two
	// must not read as a phantom.
	for _, name := range tools.DefaultLazyRegistry(t.TempDir()).Names() {
		known[name] = true
	}
	if len(known) == 0 {
		t.Fatal("registry produced no tool names")
	}

	sets := map[string][]string{
		"foregroundOnlyTools":              keysOfBoolSet(foregroundOnlyTools),
		"broadLoopExplorationTools":        keysOfNameSet(broadLoopExplorationTools),
		"workspaceIsolationReadOnlyTools":  keysOfNameSet(workspaceIsolationReadOnlyTools),
		"workspaceIsolationApplyBackTools": keysOfNameSet(workspaceIsolationApplyBackTools),
	}

	var phantom []string
	for setName, names := range sets {
		if len(names) == 0 {
			t.Errorf("%s is empty; the guard would pass while covering nothing", setName)
		}
		for _, tool := range names {
			if !known[tool] {
				phantom = append(phantom, setName+": "+tool)
			}
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Fatalf("agent policy sets name tools that are not registered: %v", phantom)
	}
}

func keysOfBoolSet(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	return names
}

func keysOfNameSet(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	return names
}
