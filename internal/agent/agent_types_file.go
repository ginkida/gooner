package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gokin/internal/tools"
)

// File-based custom agent types: a markdown file at
// <project>/.gokin/agents/<name>.md or <configDir>/agents/<name>.md becomes
// agent type <name>, usable by the task tool, the router, and Runner.Spawn.
// Frontmatter declares metadata, the body IS the agent's system prompt.
// Same conflict rules as file commands and hooks: built-in types always win
// (with a warning), project beats global.

type agentTypeFrontmatter struct {
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"`
	Priority    int      `yaml:"priority"`
}

// maxAgentTypeBytes bounds one agent-type file. The body becomes the agent's
// SYSTEM PROMPT, so unlike a command template — which costs a context window
// once, when invoked — this cost recurs on every single request the agent
// makes, for as long as the type exists. The field it lands in
// (Agent.projectContext) is bounded to maxSubAgentPromptChars when a delegated
// sub-agent fills it from BuildSubAgentPrompt; this producer was the one path
// into it with no bound at all. Real agent definitions are a few kilobytes;
// only a document misfiled into the agents directory trips this. Refusing
// loudly beats truncating: half a system prompt still instructs the model, and
// it instructs it to do something its author never wrote.
const maxAgentTypeBytes = 256 << 10

// parseAgentTypeFile reads one agent-type file: required YAML frontmatter
// with a non-empty description, body = system prompt.
func parseAgentTypeFile(path, source string) (*DynamicAgentType, []string, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxAgentTypeBytes {
		return nil, nil, fmt.Errorf("file is %d bytes, over the %d-byte limit for an agent system prompt",
			info.Size(), maxAgentTypeBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	if name == "" {
		return nil, nil, fmt.Errorf("empty agent type name")
	}

	body := string(data)
	rest, ok := strings.CutPrefix(body, "---\n")
	if !ok {
		return nil, nil, fmt.Errorf("frontmatter is required (start the file with --- and a description)")
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, nil, fmt.Errorf("unterminated frontmatter (missing closing ---)")
	}
	var fm agentTypeFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
		return nil, nil, fmt.Errorf("frontmatter: %w", err)
	}
	if strings.TrimSpace(fm.Description) == "" {
		return nil, nil, fmt.Errorf("description is required in frontmatter")
	}

	prompt := strings.TrimSpace(strings.TrimPrefix(rest[idx+len("\n---"):], "\n"))
	if prompt == "" {
		return nil, nil, fmt.Errorf("empty system prompt body")
	}

	// Unknown tool names are a WARNING, not an error: the type still loads
	// (unknown entries are simply never matched by the tool registry), but
	// the user must see the typo.
	var warnings []string
	known := tools.GetAllDeclarations()
	cleaned := make([]string, 0, len(fm.Tools))
	for _, t := range fm.Tools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := known[t]; !ok {
			warnings = append(warnings, fmt.Sprintf("agent type %q: unknown tool %q in tools list", name, t))
		}
		cleaned = append(cleaned, t)
	}

	return &DynamicAgentType{
		Name:         name,
		Description:  strings.TrimSpace(fm.Description),
		AllowedTools: cleaned,
		SystemPrompt: prompt,
		Priority:     fm.Priority,
		Source:       source,
	}, warnings, nil
}

// LoadAgentTypeFiles scans the project and global agent directories and
// registers the results. Returns loaded types and human-readable warnings
// (broken files, shadowed names, unknown tools). Missing directories are
// fine. Project types win over global; built-in types always win.
func LoadAgentTypeFiles(registry *AgentTypeRegistry, projectDir, globalDir string) (loaded []*DynamicAgentType, warnings []string) {
	if registry == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, src := range []struct {
		dir    string
		source string
	}{
		{projectDir, "project"}, // project wins — loaded first
		{globalDir, "global"},
	} {
		if src.dir == "" {
			continue
		}
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("read %s agents dir: %v", src.source, err))
			}
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)

		for _, fname := range names {
			path := filepath.Join(src.dir, fname)
			dt, fileWarnings, err := parseAgentTypeFile(path, src.source)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s agent type %s: %v", src.source, path, err))
				continue
			}
			warnings = append(warnings, fileWarnings...)
			if seen[dt.Name] {
				warnings = append(warnings, fmt.Sprintf("global agent type %q is shadowed by the project type of the same name", dt.Name))
				continue
			}
			if err := registry.RegisterDynamicType(dt); err != nil {
				// Built-in collision — the registry refuses; surface it.
				warnings = append(warnings, fmt.Sprintf("%s agent type %s: %v", src.source, path, err))
				continue
			}
			seen[dt.Name] = true
			loaded = append(loaded, dt)
		}
	}
	return loaded, warnings
}
