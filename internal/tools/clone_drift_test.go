package tools

import (
	"sort"
	"strings"
	"testing"
)

// A tool without a case in CloneToolForWorkDir falls to `default: return tool`,
// so every sub-agent gets the FOREGROUND instance. That silent default has
// produced the same bug four times — TodoTool, MemoryTool, MemorizeTool and
// ReplExecTool each shipped sharing state they should not have, and each was
// found only after the sharing turned live. The rule ("a tool that carries
// per-agent state or gets a per-agent Set* needs a clone case") has lived in a
// checklist, which is exactly what let it be missed four times.
//
// toolsSharedAcrossAgents is the set that is SUPPOSED to be shared, each with
// the reason. Sharing is often the correct answer — a per-clone background task
// manager was itself a bug — so the guard does not forbid it, it forbids
// sharing that nobody decided on.
var toolsSharedAcrossAgents = map[string]string{
	"enter_plan_mode":      "holds the app's single plan manager; there is one plan per session",
	"exit_plan_mode":       "holds the app's single plan manager; there is one plan per session",
	"get_plan_status":      "holds the app's single plan manager; there is one plan per session",
	"update_plan_progress": "holds the app's single plan manager; there is one plan per session",
	"undo_plan":            "holds the app's single plan manager and undo manager",
	"redo_plan":            "holds the app's single plan manager and undo manager",
	"task_output":          "reads the one background task manager; a per-clone manager was the v0.100.111 bug",
	"task_stop":            "stops tasks in the one background task manager, for the same reason",
	"kill_shell":           "kills tasks in the one background task manager, for the same reason",
	"ssh":                  "shares that same task manager so its background tasks stay visible to /tasks",
	"coordinate":           "holds the app-level coordinator factory",
	"mcp_admin":            "holds the app-level MCP control-plane callbacks",
	"loop_control":         "holds the app-level loop manager callbacks",
	"ask_user":             "holds the single UI prompt handler; there is one terminal to ask",
	"web_fetch":            "stateless; nothing to bind per agent",
	"web_search":           "stateless apart from provider/API-key config, which is applied app-wide",
	"env":                  "stateless; nothing to bind per agent",
	"harness": "holds a workspace-rooted store, and is excluded at the registry level from " +
		"CloneRegistryForWorkDirWithToolCeiling — the only clone path where workDir actually differs — " +
		"so the sharing never crosses a workspace",
}

func TestCloneGivesPerAgentToolsTheirOwnInstance(t *testing.T) {
	registry := DefaultRegistry(t.TempDir())

	var unexplained []string
	var renamed []string
	seen := map[string]bool{}

	for _, tool := range registry.List() {
		name := tool.Name()
		clone := CloneToolForWorkDir(tool, "/some/other/workspace")

		// A clone case that returns the wrong constructor swaps one tool for
		// another under the same registry key — the model would call `grep` and
		// reach `glob`. Cheap to check here, invisible everywhere else.
		if clone != nil && clone.Name() != name {
			renamed = append(renamed, name+" -> "+clone.Name())
		}

		if clone != tool {
			continue
		}
		reason, ok := toolsSharedAcrossAgents[name]
		if !ok || strings.TrimSpace(reason) == "" {
			unexplained = append(unexplained, name)
			continue
		}
		seen[name] = true
	}

	sort.Strings(unexplained)
	if len(unexplained) > 0 {
		t.Errorf("these tools hand every sub-agent the FOREGROUND instance with no decision recorded:\n  %s\n\n"+
			"If the tool carries per-agent state or takes a per-agent Set*, give it a case in "+
			"CloneToolForWorkDir returning a fresh instance. If sharing is right, add it to "+
			"toolsSharedAcrossAgents WITH the reason.",
			strings.Join(unexplained, "\n  "))
	}
	if len(renamed) > 0 {
		sort.Strings(renamed)
		t.Errorf("a clone case returned a different tool than it was given:\n  %s",
			strings.Join(renamed, "\n  "))
	}

	// A stale entry vouches for a tool that has since been given a clone case or
	// removed, and reads as a recorded decision while recording nothing.
	var stale []string
	for name := range toolsSharedAcrossAgents {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("toolsSharedAcrossAgents still vouches for tools that are no longer shared "+
			"(or no longer registered) — drop them:\n  %s", strings.Join(stale, "\n  "))
	}
}
