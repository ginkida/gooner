package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// File-based slash commands: a markdown file at
// <project>/.gokin/commands/<name>.md or ~/.config/gokin/commands/<name>.md
// becomes /<name>. The file body is a PROMPT TEMPLATE (never a shell
// command): invoking the command expands $ARGUMENTS / $1..$9 and submits the
// result to the model as a regular user message via the __prompt: marker
// (same marker mechanism as __browse:).
//
// Conflict rules: built-in commands always win (a file command shadowing a
// builtin is skipped with a warning); project commands win over global ones.

// PromptMarker is the executeCommandCtx marker that submits a command's
// result to the model as a prompt instead of displaying it. Exported so the
// app-side dispatch cuts the SAME constant (no cross-package drift).
const PromptMarker = "__prompt:"

// fileCommandMaxArgs is the highest positional placeholder ($1..$9).
const fileCommandMaxArgs = 9

type fileCommandFrontmatter struct {
	Description  string `yaml:"description"`
	ArgumentHint string `yaml:"argument-hint"`
}

// FileCommand is a user-defined command loaded from a markdown file.
type FileCommand struct {
	name        string
	description string
	argHint     string
	template    string
	source      string // "project" | "global"
	path        string
}

func (c *FileCommand) Name() string { return c.name }

func (c *FileCommand) Description() string {
	if c.description != "" {
		return c.description + " (" + c.source + " command)"
	}
	return "Custom " + c.source + " command"
}

func (c *FileCommand) Usage() string {
	if c.argHint != "" {
		return "/" + c.name + " " + c.argHint
	}
	return "/" + c.name
}

func (c *FileCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryTools,
		Icon:     "command",
		Priority: 90,
		HasArgs:  true,
		ArgHint:  c.argHint,
	}
}

// Source reports where the command was defined ("project" or "global").
func (c *FileCommand) Source() string { return c.source }

// Execute expands the template and hands the prompt to the model.
func (c *FileCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	expanded := expandFileCommandTemplate(c.template, args)
	if strings.TrimSpace(expanded) == "" {
		return "", fmt.Errorf("/%s expanded to an empty prompt", c.name)
	}
	return PromptMarker + expanded, nil
}

// expandFileCommandTemplate substitutes $ARGUMENTS (all args, space-joined)
// and $1..$9 (individual args; missing args expand to ""). Substitution is
// plain text into a model prompt — never shell — so no escaping is needed.
func expandFileCommandTemplate(template string, args []string) string {
	out := strings.ReplaceAll(template, "$ARGUMENTS", strings.Join(args, " "))
	// Replace high indices first so $12 isn't clobbered by $1 ($10+ is out of
	// contract, but don't corrupt it into "<arg1>0").
	for i := fileCommandMaxArgs; i >= 1; i-- {
		val := ""
		if i <= len(args) {
			val = args[i-1]
		}
		out = strings.ReplaceAll(out, "$"+strconv.Itoa(i), val)
	}
	return strings.TrimSpace(out)
}

// maxFileCommandBytes bounds one command file. The body becomes a MODEL PROMPT,
// and every .md in the commands directory is read at boot, so an oversized file
// costs both startup memory and — if invoked — most of a context window, with
// nothing on screen explaining where the tokens went. Real prompt templates are
// a few kilobytes; this is generous enough that only a document misfiled into
// the directory trips it. Refusing loudly beats truncating: half a template is
// a prompt that says something its author never wrote.
const maxFileCommandBytes = 256 << 10

// parseFileCommand reads one command file: optional YAML frontmatter between
// `---` fences, body = prompt template.
func parseFileCommand(path, source string) (*FileCommand, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxFileCommandBytes {
		return nil, fmt.Errorf("file is %d bytes, over the %d-byte limit for a command template",
			info.Size(), maxFileCommandBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Lowercase at the REGISTRATION boundary: Parse lowercases the typed
	// command name before lookup (slash commands are case-insensitive), so a
	// file named Deploy.md must register as "deploy" or it becomes
	// unreachable — /Deploy AND /deploy would both miss the "Deploy" map key
	// and the whole line would silently go to the model as chat. Alias
	// targets are lowercased on load for the same reason (aliases.go).
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(path), ".md"))
	if name == "" {
		return nil, fmt.Errorf("empty command name")
	}

	body := string(data)
	var fm fileCommandFrontmatter
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		idx := strings.Index(rest, "\n---")
		if idx < 0 {
			return nil, fmt.Errorf("unterminated frontmatter (missing closing ---)")
		}
		if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
			return nil, fmt.Errorf("frontmatter: %w", err)
		}
		body = rest[idx+len("\n---"):]
		body = strings.TrimPrefix(body, "\n")
	}

	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("empty prompt body")
	}

	return &FileCommand{
		name:        name,
		description: strings.TrimSpace(fm.Description),
		argHint:     strings.TrimSpace(fm.ArgumentHint),
		template:    body,
		source:      source,
		path:        path,
	}, nil
}

// LoadFileCommands scans the project and global command directories and
// registers the results on the handler. Returns the loaded commands and
// human-readable warnings (broken files, shadowed names). Missing
// directories are fine. MUST be called once during startup, before the
// handler is shared across goroutines — same contract as LoadAliasesFromFile.
// Panics if called after SealBootPhase.
func (h *Handler) LoadFileCommands(projectDir, globalDir string) (loaded []*FileCommand, warnings []string) {
	if h.bootDone {
		panic("commands: LoadFileCommands called after boot phase sealed")
	}
	seen := map[string]bool{}
	for _, src := range []struct {
		dir    string
		source string
	}{
		{projectDir, "project"}, // project wins — registered first
		{globalDir, "global"},
	} {
		if src.dir == "" {
			continue
		}
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("read %s commands dir: %v", src.source, err))
			}
			continue
		}
		// Deterministic load order within a directory.
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)

		for _, fname := range names {
			path := filepath.Join(src.dir, fname)
			cmd, err := parseFileCommand(path, src.source)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s command %s: %v", src.source, path, err))
				continue
			}
			if _, isBuiltin := h.commands[cmd.name]; isBuiltin && !seen[cmd.name] {
				warnings = append(warnings, fmt.Sprintf("%s command %q shadows a built-in command — ignored (%s)", src.source, cmd.name, path))
				continue
			}
			if seen[cmd.name] {
				// Project already claimed this name; global loses silently-ish.
				warnings = append(warnings, fmt.Sprintf("global command %q is shadowed by the project command of the same name", cmd.name))
				continue
			}
			h.commands[cmd.name] = cmd
			seen[cmd.name] = true
			loaded = append(loaded, cmd)
		}
	}
	return loaded, warnings
}
