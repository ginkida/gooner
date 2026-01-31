package tools

import (
	"context"
	"fmt"
	"strings"

	"gokin/internal/memory"

	"google.golang.org/genai"
)

// MemoryTool provides persistent memory storage between sessions.
type MemoryTool struct {
	store *memory.Store
}

// NewMemoryTool creates a new memory tool.
func NewMemoryTool() *MemoryTool {
	return &MemoryTool{}
}

// SetStore sets the memory store.
func (t *MemoryTool) SetStore(store *memory.Store) {
	t.store = store
}

func (t *MemoryTool) Name() string {
	return "memory"
}

func (t *MemoryTool) Description() string {
	return "Persistent memory storage for remembering and recalling information across sessions"
}

func (t *MemoryTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: 'remember' (save), 'recall' (retrieve), 'forget' (delete), 'list' (show all)",
					Enum:        []string{"remember", "recall", "forget", "list"},
				},
				"content": {
					Type:        genai.TypeString,
					Description: "Content to remember (required for 'remember' action)",
				},
				"key": {
					Type:        genai.TypeString,
					Description: "Optional key to identify the memory. Makes it easier to recall or update later",
				},
				"query": {
					Type:        genai.TypeString,
					Description: "Search query for 'recall' action (searches in content and key)",
				},
				"tags": {
					Type:        genai.TypeArray,
					Items:       &genai.Schema{Type: genai.TypeString},
					Description: "Tags for organizing memories. Use for filtering in 'recall'",
				},
				"scope": {
					Type:        genai.TypeString,
					Description: "Scope of the memory: 'session' (current session only), 'project' (this repository only), 'global' (all projects). Default: 'project'",
					Enum:        []string{"session", "project", "global"},
				},
				"id": {
					Type:        genai.TypeString,
					Description: "Memory ID for 'forget' action",
				},
				"project_only": {
					Type:        genai.TypeBoolean,
					Description: "If true, only show/search memories for current project (default: false for 'recall' and 'list')",
				},
			},
			Required: []string{"action"},
		},
	}
}

func (t *MemoryTool) Validate(args map[string]any) error {
	action, ok := GetString(args, "action")
	if !ok {
		return NewValidationError("action", "action is required")
	}

	switch action {
	case "remember":
		if _, ok := GetString(args, "content"); !ok {
			return NewValidationError("content", "content is required for 'remember' action")
		}
	case "forget":
		// Need either id or key
		_, hasID := GetString(args, "id")
		_, hasKey := GetString(args, "key")
		if !hasID && !hasKey {
			return NewValidationError("id", "either 'id' or 'key' is required for 'forget' action")
		}
	case "recall", "list":
		// No required parameters
	default:
		return NewValidationError("action", "invalid action: "+action)
	}

	return nil
}

func (t *MemoryTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.store == nil {
		return NewErrorResult("memory store not configured"), nil
	}

	action, _ := GetString(args, "action")

	switch action {
	case "remember":
		return t.remember(args)
	case "recall":
		return t.recall(args)
	case "forget":
		return t.forget(args)
	case "list":
		return t.list(args)
	default:
		return NewErrorResult("invalid action: " + action), nil
	}
}

func (t *MemoryTool) remember(args map[string]any) (ToolResult, error) {
	content, _ := GetString(args, "content")
	key, _ := GetString(args, "key")
	tagsRaw, _ := args["tags"]

	// Parse scope
	scopeName, _ := GetString(args, "scope")
	memType := memory.MemoryProject
	switch scopeName {
	case "session":
		memType = memory.MemorySession
	case "global":
		memType = memory.MemoryGlobal
	}

	// Create entry
	entry := memory.NewEntry(content, memType)
	if key != "" {
		entry.WithKey(key)
	}

	// Parse tags
	if tagsRaw != nil {
		if tagsSlice, ok := tagsRaw.([]any); ok {
			tags := make([]string, 0, len(tagsSlice))
			for _, t := range tagsSlice {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
			entry.WithTags(tags)
		}
	}

	// Save entry
	if err := t.store.Add(entry); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to save memory: %s", err)), nil
	}

	var msg string
	if key != "" {
		msg = fmt.Sprintf("Remembered: %q with key %q", truncate(content, 100), key)
	} else {
		msg = fmt.Sprintf("Remembered: %q (id: %s)", truncate(content, 100), entry.ID)
	}

	return NewSuccessResultWithData(msg, map[string]any{
		"id":      entry.ID,
		"key":     entry.Key,
		"content": entry.Content,
		"type":    entry.Type,
		"tags":    entry.Tags,
	}), nil
}

func (t *MemoryTool) recall(args map[string]any) (ToolResult, error) {
	key, _ := GetString(args, "key")
	query, _ := GetString(args, "query")
	tagsRaw, _ := args["tags"]
	projectOnly := GetBoolDefault(args, "project_only", false)

	// If key is specified, try exact match first
	if key != "" {
		if entry, ok := t.store.Get(key); ok {
			return NewSuccessResultWithData(
				fmt.Sprintf("Found memory with key %q: %s", key, entry.Content),
				map[string]any{
					"id":      entry.ID,
					"key":     entry.Key,
					"content": entry.Content,
					"tags":    entry.Tags,
					"found":   true,
				},
			), nil
		}
	}

	// Build search query
	searchQuery := memory.SearchQuery{
		Key:         key,
		Query:       query,
		ProjectOnly: projectOnly,
		Limit:       20, // Reasonable default
	}

	// Parse tags
	if tagsRaw != nil {
		if tagsSlice, ok := tagsRaw.([]any); ok {
			for _, t := range tagsSlice {
				if s, ok := t.(string); ok {
					searchQuery.Tags = append(searchQuery.Tags, s)
				}
			}
		}
	}

	results := t.store.Search(searchQuery)

	if len(results) == 0 {
		return NewSuccessResultWithData("No memories found", map[string]any{
			"found": false,
			"count": 0,
		}), nil
	}

	// Build results string by type
	byType := make(map[memory.MemoryType][]*memory.Entry)
	for _, entry := range results {
		byType[entry.Type] = append(byType[entry.Type], entry)
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Found %d memories:\n\n", len(results)))

	types := []struct {
		t     memory.MemoryType
		label string
	}{
		{memory.MemorySession, "Session"},
		{memory.MemoryProject, "Project"},
		{memory.MemoryGlobal, "Global"},
	}

	resultData := make([]map[string]any, 0, len(results))
	for _, tc := range types {
		if items, ok := byType[tc.t]; ok && len(items) > 0 {
			builder.WriteString(fmt.Sprintf("[%s Memories]\n", tc.label))
			for _, entry := range items {
				if entry.Key != "" {
					builder.WriteString(fmt.Sprintf("- [%s] %s\n", entry.Key, entry.Content))
				} else {
					builder.WriteString(fmt.Sprintf("- %s\n", entry.Content))
				}
				resultData = append(resultData, map[string]any{
					"id":      entry.ID,
					"key":     entry.Key,
					"content": entry.Content,
					"type":    entry.Type,
					"tags":    entry.Tags,
				})
			}
			builder.WriteString("\n")
		}
	}

	return NewSuccessResultWithData(builder.String(), map[string]any{
		"found":   true,
		"count":   len(results),
		"results": resultData,
	}), nil
}

func (t *MemoryTool) forget(args map[string]any) (ToolResult, error) {
	id, _ := GetString(args, "id")
	key, _ := GetString(args, "key")

	target := id
	if target == "" {
		target = key
	}

	if t.store.Remove(target) {
		return NewSuccessResult(fmt.Sprintf("Forgot memory: %s", target)), nil
	}

	return NewErrorResult(fmt.Sprintf("Memory not found: %s", target)), nil
}

func (t *MemoryTool) list(args map[string]any) (ToolResult, error) {
	projectOnly := GetBoolDefault(args, "project_only", false)

	entries := t.store.List(projectOnly)

	if len(entries) == 0 {
		scope := "global"
		if projectOnly {
			scope = "project"
		}
		return NewSuccessResultWithData(
			fmt.Sprintf("No memories stored (%s scope)", scope),
			map[string]any{"count": 0},
		), nil
	}

	// Build results string by type
	byType := make(map[memory.MemoryType][]*memory.Entry)
	for _, entry := range entries {
		byType[entry.Type] = append(byType[entry.Type], entry)
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Stored memories (%d total):\n\n", len(entries)))

	types := []struct {
		t     memory.MemoryType
		label string
	}{
		{memory.MemorySession, "Session"},
		{memory.MemoryProject, "Project"},
		{memory.MemoryGlobal, "Global"},
	}

	resultData := make([]map[string]any, 0, len(entries))
	for _, tc := range types {
		if items, ok := byType[tc.t]; ok && len(items) > 0 {
			builder.WriteString(fmt.Sprintf("[%s Memories]\n", tc.label))
			for _, entry := range items {
				if entry.Key != "" {
					builder.WriteString(fmt.Sprintf("- [%s] %s (id: %s)\n", entry.Key, truncate(entry.Content, 60), entry.ID))
				} else {
					builder.WriteString(fmt.Sprintf("- %s (id: %s)\n", truncate(entry.Content, 60), entry.ID))
				}
				resultData = append(resultData, map[string]any{
					"id":      entry.ID,
					"key":     entry.Key,
					"content": entry.Content,
					"type":    entry.Type,
					"tags":    entry.Tags,
				})
			}
			builder.WriteString("\n")
		}
	}

	return NewSuccessResultWithData(builder.String(), map[string]any{
		"count":   len(entries),
		"entries": resultData,
	}), nil
}

// truncate truncates a string to the specified length.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
