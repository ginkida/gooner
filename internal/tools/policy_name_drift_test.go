package tools

import (
	"sort"
	"testing"
)

// A tool name written into a policy set must name a tool that exists. A
// misspelled or obsolete entry is invisible: it can never match, so nothing
// fails, and the list keeps reading as if it covered something it does not.
// `atomicwrite` sat in six of these sets — including the discuss-mode gate and
// the permission table — for a tool that has never been registered, which is
// exactly what the failure mode looks like when a real tool is entered under
// the wrong name and silently escapes its gate.
func TestPolicyToolSetsNameOnlyRegisteredTools(t *testing.T) {
	known := map[string]bool{}
	for _, tool := range DefaultRegistry(t.TempDir()).List() {
		known[tool.Name()] = true
	}
	// Union with the lazy registry: it is the model-facing set, and a tool
	// present in only one of the two must not read as a phantom.
	for _, name := range DefaultLazyRegistry(t.TempDir()).Names() {
		known[name] = true
	}
	if len(known) == 0 {
		t.Fatal("registry produced no tool names")
	}

	sets := map[string]map[string]bool{
		"implementationTools":     implementationTools,
		"deltaCheckToolSet":       deltaCheckToolSet,
		"fileModifyingTools":      fileModifyingTools,
		"parallelSafeTools":       parallelSafeTools,
		"sequentialReadOnlyTools": sequentialReadOnlyTools,
		"planModeReadOnlyTools":   planModeReadOnlyTools,
		"idempotentStateTools":    idempotentStateTools,
	}

	var phantom []string
	for setName, set := range sets {
		for tool := range set {
			if !known[tool] {
				phantom = append(phantom, setName+": "+tool)
			}
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Fatalf("policy sets name tools that are not registered: %v", phantom)
	}
}
