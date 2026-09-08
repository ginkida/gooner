package tools

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"gokin/internal/memory"
)

// MemorizeTool allows the agent to save project-specific knowledge.
type MemorizeTool struct {
	learning *memory.ProjectLearning
}

// NewMemorizeTool creates a new MemorizeTool instance.
func NewMemorizeTool(learning *memory.ProjectLearning) *MemorizeTool {
	return &MemorizeTool{
		learning: learning,
	}
}

// SetLearning sets the learning store for the tool.
func (t *MemorizeTool) SetLearning(learning *memory.ProjectLearning) {
	t.learning = learning
}

// GetLearning returns the configured project learning store.
func (t *MemorizeTool) GetLearning() *memory.ProjectLearning {
	return t.learning
}

func (t *MemorizeTool) Name() string {
	return "memorize"
}

func (t *MemorizeTool) Description() string {
	return MemorizeToolDeclaration().Description
}

// Declaration delegates to the shared schema in declarations.go (single
// source — the two hand-maintained copies had drifted; see the CLAUDE.md
// new-tool rule).
func (t *MemorizeTool) Declaration() *genai.FunctionDeclaration {
	return MemorizeToolDeclaration()
}

func (t *MemorizeTool) Validate(args map[string]any) error {
	infoType, _ := GetString(args, "type")
	key, _ := GetString(args, "key")
	content, _ := GetString(args, "content")

	if infoType == "" {
		return NewValidationError("type", "is required")
	}
	if key == "" {
		return NewValidationError("key", "is required")
	}
	// forget removes an entry by key — no content to store.
	if content == "" && infoType != "forget" {
		return NewValidationError("content", "is required")
	}

	return nil
}

func (t *MemorizeTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.learning == nil {
		return NewErrorResult("project learning store not initialized"), nil
	}

	infoType, _ := GetString(args, "type")
	key, _ := GetString(args, "key")
	content, _ := GetString(args, "content")

	forgot := false
	stored := true
	switch infoType {
	case "preference":
		stored = t.learning.SetPreference(key, content)
	case "fact", "convention":
		// Store as a preference for now or extend ProjectLearning
		stored = t.learning.SetPreference(fmt.Sprintf("%s:%s", infoType, key), content)
	case "pattern":
		stored = t.learning.LearnPattern(key, content, nil, nil)
	case "forget":
		// The correction half of memory maintenance: remove an entry that
		// turned out wrong or stale so it stops polluting future sessions.
		if !t.learning.RemoveEntry(key) {
			return NewErrorResult(fmt.Sprintf("no memorized entry named %q to forget (check `memory` action=list for exact keys)", key)), nil
		}
		forgot = true
	default:
		return NewErrorResult(fmt.Sprintf("unknown information type: %s", infoType)), nil
	}

	// A full store drops the write, and the flush below would then report
	// "nothing to write" — which reads as "already saved" and loses the fact
	// silently, forever, for every later call too. Say so instead, and name
	// the action that frees room; the model can run it itself.
	if !stored {
		return NewErrorResult(fmt.Sprintf(
			"memory is full — %q was NOT saved. Remove entries that are no longer true "+
				"(`memorize` with type=forget and the key) and try again; `memory` action=list "+
				"shows what is stored.", key)), nil
	}

	// Flush immediately to ensure persistence. FlushChanged reports whether a
	// write ACTUALLY happened, so the success message never claims "(updated …)"
	// for a no-op flush — the honesty defect behind the field report.
	changed, err := t.learning.FlushChanged()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to save memory: %s", err)), nil
	}

	markdownPath := t.learning.MarkdownPath()
	var msg string
	if forgot {
		EmitMemoryNotify(ctx, "forgot", key)
		msg = fmt.Sprintf("Forgot memorized entry: %s", key)
	} else {
		EmitMemoryNotify(ctx, "memorized", fmt.Sprintf("%s: %s", infoType, key))
		msg = fmt.Sprintf("Memorized %s: %s", infoType, key)
	}
	if changed {
		if markdownPath != "" {
			msg += fmt.Sprintf(" (updated %s — also shown in `memory list` and loaded next session)", markdownPath)
		}
	} else {
		msg += " (already current — nothing to write)"
	}

	// Declare the files written so the executor invalidates the read-dedup cache
	// (the agent can re-read to VERIFY) and records them for post-compaction
	// hints — memorize writes via ProjectLearning, bypassing the write-tool path
	// the executor normally watches. Only when a write actually happened.
	if changed {
		var written []string
		for _, p := range []string{markdownPath, t.learning.Path()} {
			if p != "" {
				written = append(written, p)
			}
		}
		if len(written) > 0 {
			return NewSuccessResultWithData(msg, map[string]any{"written_paths": written}), nil
		}
	}
	return NewSuccessResult(msg), nil
}
