package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"
)

// SharedMemoryInterface defines the interface for shared memory operations.
// This is implemented by agent.SharedMemory to avoid import cycles.
type SharedMemoryInterface interface {
	Write(key string, value any, entryType string, sourceAgent string)
	WriteWithTTL(key string, value any, entryType string, sourceAgent string, ttl time.Duration)
	Read(key string) (SharedMemoryEntry, bool)
	ReadByType(entryType string) []SharedMemoryEntry
	ReadAll() []SharedMemoryEntry
	Delete(key string) bool
	GetForContext(agentID string, maxEntries int) string
}

// SharedMemoryEntry represents an entry from shared memory.
type SharedMemoryEntry struct {
	Key       string
	Value     any
	Type      string
	Source    string
	Timestamp time.Time
	Version   int
}

// SharedMemoryTool allows agents to read and write shared memory.
type SharedMemoryTool struct {
	memory  SharedMemoryInterface
	agentID string // The ID of the agent using this tool
}

// NewSharedMemoryTool creates a new SharedMemoryTool.
func NewSharedMemoryTool() *SharedMemoryTool {
	return &SharedMemoryTool{}
}

// SetMemory sets the shared memory interface.
func (t *SharedMemoryTool) SetMemory(memory SharedMemoryInterface) {
	t.memory = memory
}

// SetAgentID sets the agent ID for this tool instance.
func (t *SharedMemoryTool) SetAgentID(agentID string) {
	t.agentID = agentID
}

func (t *SharedMemoryTool) Name() string {
	return "shared_memory"
}

func (t *SharedMemoryTool) Description() string {
	return `Read and write shared memory to communicate with other agents.

Use this to:
- Share discovered facts about the codebase
- Store insights from analysis
- Record file states after modifications
- Share decisions that other agents should know about

Entry types:
- fact: A verified piece of information (e.g., "main entry point is cmd/main.go")
- insight: An analysis conclusion (e.g., "authentication uses JWT tokens")
- file_state: State of a file after modification (e.g., "user.go has new validation")
- decision: A decision that was made (e.g., "using singleton pattern for config")

Actions:
- write: Store information in shared memory
- read: Get a specific entry by key
- list: List all entries or filter by type`
}

func (t *SharedMemoryTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "The action to perform: 'read', 'write', or 'list'",
					Enum:        []string{"read", "write", "list"},
				},
				"key": {
					Type:        genai.TypeString,
					Description: "The key for the memory entry (required for read and write)",
				},
				"value": {
					Type:        genai.TypeString,
					Description: "The value to store (required for write)",
				},
				"type": {
					Type:        genai.TypeString,
					Description: "The type of entry: 'fact', 'insight', 'file_state', or 'decision'",
					Enum:        []string{"fact", "insight", "file_state", "decision"},
				},
				"ttl_minutes": {
					Type:        genai.TypeInteger,
					Description: "Time-to-live in minutes (0 = never expires, default)",
				},
				"filter_type": {
					Type:        genai.TypeString,
					Description: "Filter entries by type when listing (optional)",
					Enum:        []string{"fact", "insight", "file_state", "decision"},
				},
			},
			Required: []string{"action"},
		},
	}
}

func (t *SharedMemoryTool) Validate(args map[string]any) error {
	action, ok := GetString(args, "action")
	if !ok || action == "" {
		return NewValidationError("action", "is required")
	}

	switch action {
	case "read":
		if key, _ := GetString(args, "key"); key == "" {
			return NewValidationError("key", "is required for read action")
		}
	case "write":
		if key, _ := GetString(args, "key"); key == "" {
			return NewValidationError("key", "is required for write action")
		}
		if value, _ := GetString(args, "value"); value == "" {
			return NewValidationError("value", "is required for write action")
		}
		entryType, _ := GetString(args, "type")
		if entryType == "" {
			return NewValidationError("type", "is required for write action")
		}
	case "list":
		// No additional validation needed
	default:
		return NewValidationError("action", "must be 'read', 'write', or 'list'")
	}

	return nil
}

func (t *SharedMemoryTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if t.memory == nil {
		return NewErrorResult("shared memory not initialized"), nil
	}

	action, _ := GetString(args, "action")

	switch action {
	case "read":
		return t.executeRead(args)
	case "write":
		return t.executeWrite(args)
	case "list":
		return t.executeList(args)
	default:
		return NewErrorResult(fmt.Sprintf("unknown action: %s", action)), nil
	}
}

func (t *SharedMemoryTool) executeRead(args map[string]any) (ToolResult, error) {
	key, _ := GetString(args, "key")

	entry, ok := t.memory.Read(key)
	if !ok {
		return NewSuccessResult(fmt.Sprintf("No entry found for key: %s", key)), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Shared Memory Entry: %s\n\n", key))
	sb.WriteString(fmt.Sprintf("**Type:** %s\n", entry.Type))
	sb.WriteString(fmt.Sprintf("**Source:** %s\n", entry.Source))
	sb.WriteString(fmt.Sprintf("**Version:** %d\n", entry.Version))
	sb.WriteString(fmt.Sprintf("**Timestamp:** %s\n\n", entry.Timestamp.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("**Value:**\n%v\n", entry.Value))

	return NewSuccessResultWithData(sb.String(), map[string]any{
		"key":       entry.Key,
		"value":     entry.Value,
		"type":      entry.Type,
		"source":    entry.Source,
		"version":   entry.Version,
		"timestamp": entry.Timestamp.Format(time.RFC3339),
	}), nil
}

func (t *SharedMemoryTool) executeWrite(args map[string]any) (ToolResult, error) {
	key, _ := GetString(args, "key")
	value, _ := GetString(args, "value")
	entryType, _ := GetString(args, "type")
	ttlMinutes := GetIntDefault(args, "ttl_minutes", 0)

	ttl := time.Duration(ttlMinutes) * time.Minute

	agentID := t.agentID
	if agentID == "" {
		agentID = "unknown"
	}

	if ttl > 0 {
		t.memory.WriteWithTTL(key, value, entryType, agentID, ttl)
	} else {
		t.memory.Write(key, value, entryType, agentID)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Entry Written to Shared Memory\n\n"))
	sb.WriteString(fmt.Sprintf("**Key:** %s\n", key))
	sb.WriteString(fmt.Sprintf("**Type:** %s\n", entryType))
	sb.WriteString(fmt.Sprintf("**Value:** %s\n", value))
	if ttlMinutes > 0 {
		sb.WriteString(fmt.Sprintf("**TTL:** %d minutes\n", ttlMinutes))
	}

	return NewSuccessResult(sb.String()), nil
}

func (t *SharedMemoryTool) executeList(args map[string]any) (ToolResult, error) {
	filterType, _ := GetString(args, "filter_type")

	var entries []SharedMemoryEntry
	if filterType != "" {
		entries = t.memory.ReadByType(filterType)
	} else {
		entries = t.memory.ReadAll()
	}

	if len(entries) == 0 {
		msg := "No entries in shared memory"
		if filterType != "" {
			msg = fmt.Sprintf("No entries of type '%s' in shared memory", filterType)
		}
		return NewSuccessResult(msg), nil
	}

	var sb strings.Builder
	sb.WriteString("## Shared Memory Entries\n\n")
	if filterType != "" {
		sb.WriteString(fmt.Sprintf("*Filtered by type: %s*\n\n", filterType))
	}

	for _, entry := range entries {
		sb.WriteString(fmt.Sprintf("### %s\n", entry.Key))
		sb.WriteString(fmt.Sprintf("- **Type:** %s\n", entry.Type))
		sb.WriteString(fmt.Sprintf("- **Source:** %s\n", entry.Source))
		sb.WriteString(fmt.Sprintf("- **Value:** %v\n\n", entry.Value))
	}

	sb.WriteString(fmt.Sprintf("---\n**Total entries:** %d\n", len(entries)))

	// Prepare structured data
	entriesData := make([]map[string]any, len(entries))
	for i, entry := range entries {
		entriesData[i] = map[string]any{
			"key":    entry.Key,
			"value":  entry.Value,
			"type":   entry.Type,
			"source": entry.Source,
		}
	}

	return NewSuccessResultWithData(sb.String(), map[string]any{
		"entries": entriesData,
		"count":   len(entries),
	}), nil
}

// SharedMemoryAdapter adapts agent.SharedMemory to SharedMemoryInterface.
// This allows the tool to work with the actual implementation without import cycles.
type SharedMemoryAdapter struct {
	memory any // Actually *agent.SharedMemory
}

// NewSharedMemoryAdapter creates an adapter for agent.SharedMemory.
func NewSharedMemoryAdapter(memory any) *SharedMemoryAdapter {
	return &SharedMemoryAdapter{memory: memory}
}

// The adapter methods use reflection/type assertion to call the actual methods.
// This is implemented in the wiring code in builder.go.

// Implement SharedMemoryInterface methods by delegating to the underlying memory.
// These are implemented via interface assertion in the builder when wiring.

func (a *SharedMemoryAdapter) Write(key string, value any, entryType string, sourceAgent string) {
	if sm, ok := a.memory.(interface {
		Write(string, any, string, string)
	}); ok {
		sm.Write(key, value, entryType, sourceAgent)
	}
}

func (a *SharedMemoryAdapter) WriteWithTTL(key string, value any, entryType string, sourceAgent string, ttl time.Duration) {
	if sm, ok := a.memory.(interface {
		WriteWithTTL(string, any, string, string, time.Duration)
	}); ok {
		sm.WriteWithTTL(key, value, entryType, sourceAgent, ttl)
	}
}

func (a *SharedMemoryAdapter) Read(key string) (SharedMemoryEntry, bool) {
	if sm, ok := a.memory.(interface {
		Read(string) (any, bool)
	}); ok {
		entry, found := sm.Read(key)
		if !found {
			return SharedMemoryEntry{}, false
		}
		// Convert to SharedMemoryEntry
		if e, ok := entry.(map[string]any); ok {
			return SharedMemoryEntry{
				Key:    key,
				Value:  e["value"],
				Type:   fmt.Sprintf("%v", e["type"]),
				Source: fmt.Sprintf("%v", e["source"]),
			}, true
		}
		// Handle direct struct conversion via JSON
		data, _ := json.Marshal(entry)
		var result SharedMemoryEntry
		_ = json.Unmarshal(data, &result)
		return result, true
	}
	return SharedMemoryEntry{}, false
}

func (a *SharedMemoryAdapter) ReadByType(entryType string) []SharedMemoryEntry {
	// Implemented via wiring
	return nil
}

func (a *SharedMemoryAdapter) ReadAll() []SharedMemoryEntry {
	// Implemented via wiring
	return nil
}

func (a *SharedMemoryAdapter) Delete(key string) bool {
	if sm, ok := a.memory.(interface {
		Delete(string) bool
	}); ok {
		return sm.Delete(key)
	}
	return false
}

func (a *SharedMemoryAdapter) GetForContext(agentID string, maxEntries int) string {
	if sm, ok := a.memory.(interface {
		GetForContext(string, int) string
	}); ok {
		return sm.GetForContext(agentID, maxEntries)
	}
	return ""
}
