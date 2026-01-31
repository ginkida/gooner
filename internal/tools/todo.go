package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/genai"
)

// TodoItem represents a single todo item.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form"`
}

// TodoTool manages a list of tasks.
type TodoTool struct {
	items []TodoItem
	mu    sync.RWMutex
}

// NewTodoTool creates a new TodoTool instance.
func NewTodoTool() *TodoTool {
	return &TodoTool{
		items: make([]TodoItem, 0),
	}
}

func (t *TodoTool) Name() string {
	return "todo"
}

func (t *TodoTool) Description() string {
	return "Manages a task list. Use to track progress on multi-step tasks."
}

func (t *TodoTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"todos": {
					Type:        genai.TypeArray,
					Description: "The complete updated list of todos",
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"content": {
								Type:        genai.TypeString,
								Description: "The task description (imperative form)",
							},
							"status": {
								Type:        genai.TypeString,
								Description: "Task status: pending, in_progress, or completed",
								Enum:        []string{"pending", "in_progress", "completed"},
							},
							"active_form": {
								Type:        genai.TypeString,
								Description: "The task in present continuous form (e.g., 'Running tests')",
							},
						},
						Required: []string{"content", "status", "active_form"},
					},
				},
			},
			Required: []string{"todos"},
		},
	}
}

func (t *TodoTool) Validate(args map[string]any) error {
	todosRaw, ok := args["todos"]
	if !ok {
		return NewValidationError("todos", "is required")
	}

	// Validate it's an array
	_, ok = todosRaw.([]any)
	if !ok {
		return NewValidationError("todos", "must be an array")
	}

	return nil
}

func (t *TodoTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	todosRaw, ok := args["todos"].([]any)
	if !ok {
		return NewErrorResult("todos must be an array"), nil
	}

	// Parse todos
	var newItems []TodoItem
	for i, itemRaw := range todosRaw {
		itemMap, ok := itemRaw.(map[string]any)
		if !ok {
			return NewErrorResult(fmt.Sprintf("todo[%d]: invalid format", i)), nil
		}

		content, _ := itemMap["content"].(string)
		status, _ := itemMap["status"].(string)
		activeForm, _ := itemMap["active_form"].(string)

		if content == "" {
			return NewErrorResult(fmt.Sprintf("todo[%d]: content is required", i)), nil
		}
		if status == "" {
			status = "pending"
		}
		if activeForm == "" {
			activeForm = content
		}

		// Validate status
		if status != "pending" && status != "in_progress" && status != "completed" {
			return NewErrorResult(fmt.Sprintf("todo[%d]: invalid status '%s'", i, status)), nil
		}

		newItems = append(newItems, TodoItem{
			Content:    content,
			Status:     status,
			ActiveForm: activeForm,
		})
	}

	// Update items
	t.mu.Lock()
	t.items = newItems
	t.mu.Unlock()

	// Generate summary
	summary := t.generateSummary()

	return NewSuccessResultWithData(summary, newItems), nil
}

// GetItems returns the current todo items.
func (t *TodoTool) GetItems() []TodoItem {
	t.mu.RLock()
	defer t.mu.RUnlock()

	items := make([]TodoItem, len(t.items))
	copy(items, t.items)
	return items
}

// GetCurrentTask returns the current in-progress task.
func (t *TodoTool) GetCurrentTask() *TodoItem {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, item := range t.items {
		if item.Status == "in_progress" {
			return &item
		}
	}
	return nil
}

func (t *TodoTool) generateSummary() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(t.items) == 0 {
		return "Todo list cleared."
	}

	var pending, inProgress, completed int
	for _, item := range t.items {
		switch item.Status {
		case "pending":
			pending++
		case "in_progress":
			inProgress++
		case "completed":
			completed++
		}
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Todo list updated: %d pending, %d in progress, %d completed\n\n",
		pending, inProgress, completed))

	for i, item := range t.items {
		var icon string
		switch item.Status {
		case "pending":
			icon = "[ ]"
		case "in_progress":
			icon = "[>]"
		case "completed":
			icon = "[✓]"
		}
		builder.WriteString(fmt.Sprintf("%d. %s %s\n", i+1, icon, item.Content))
	}

	return builder.String()
}

// ToJSON serializes the todo list to JSON.
func (t *TodoTool) ToJSON() (string, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	data, err := json.Marshal(t.items)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FromJSON deserializes the todo list from JSON.
func (t *TodoTool) FromJSON(data string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return json.Unmarshal([]byte(data), &t.items)
}

// RestoreItems restores todo items from an external source.
func (t *TodoTool) RestoreItems(items []TodoItem) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.items = make([]TodoItem, len(items))
	copy(t.items, items)
}

// ClearItems clears all todo items.
func (t *TodoTool) ClearItems() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.items = make([]TodoItem, 0)
}
