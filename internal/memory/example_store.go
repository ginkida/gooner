package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gooner/internal/logging"
)

// TaskExample represents a successful task execution that can be used for few-shot learning.
type TaskExample struct {
	ID           string            `json:"id"`
	TaskType     string            `json:"task_type"`     // explore, refactor, implement, etc.
	InputPrompt  string            `json:"input_prompt"`  // The original user request
	AgentType    string            `json:"agent_type"`    // The agent type that handled it
	ToolsUsed    []string          `json:"tools_used"`    // List of tools used
	ToolSequence []ToolCallExample `json:"tool_sequence"` // Detailed tool call sequence
	FinalOutput  string            `json:"final_output"`  // The final response
	Duration     time.Duration     `json:"duration"`      // How long it took
	TokensUsed   int               `json:"tokens_used"`   // Tokens consumed
	SuccessScore float64           `json:"success_score"` // 0-1 based on feedback
	Tags         []string          `json:"tags"`          // Keywords for matching
	Created      time.Time         `json:"created"`
	UseCount     int               `json:"use_count"` // How many times used as example
}

// ToolCallExample represents a single tool call in a sequence.
type ToolCallExample struct {
	ToolName string         `json:"tool_name"`
	Args     map[string]any `json:"args"`
	Success  bool           `json:"success"`
	Output   string         `json:"output"` // Truncated output
}

// ExampleStore manages task examples for few-shot learning.
type ExampleStore struct {
	configDir string
	examples  map[string]*TaskExample
	byType    map[string][]string // TaskType -> list of example IDs
	mu        sync.RWMutex
}

// NewExampleStore creates a new example store.
func NewExampleStore(configDir string) (*ExampleStore, error) {
	es := &ExampleStore{
		configDir: configDir,
		examples:  make(map[string]*TaskExample),
		byType:    make(map[string][]string),
	}

	if err := es.load(); err != nil {
		logging.Debug("failed to load example store", "error", err)
		// Not a fatal error - start with empty store
	}

	return es, nil
}

// storagePath returns the path to the examples file.
func (es *ExampleStore) storagePath() string {
	return filepath.Join(es.configDir, "memory", "examples.json")
}

// load loads examples from disk.
func (es *ExampleStore) load() error {
	data, err := os.ReadFile(es.storagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var examples map[string]*TaskExample
	if err := json.Unmarshal(data, &examples); err != nil {
		return err
	}

	es.examples = examples

	// Rebuild type index
	es.byType = make(map[string][]string)
	for id, ex := range es.examples {
		es.byType[ex.TaskType] = append(es.byType[ex.TaskType], id)
	}

	return nil
}

// save persists examples to disk.
func (es *ExampleStore) save() error {
	dir := filepath.Dir(es.storagePath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(es.examples, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(es.storagePath(), data, 0644)
}

// generateExampleID creates a unique ID for an example.
func generateExampleID() string {
	return time.Now().Format("20060102150405") + "-" + randomExampleSuffix()
}

func randomExampleSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 6)
	for i := range result {
		result[i] = chars[time.Now().UnixNano()%int64(len(chars))]
		time.Sleep(time.Nanosecond) // Ensure uniqueness
	}
	return string(result)
}

// LearnFromSuccess records a successful task execution for future learning.
func (es *ExampleStore) LearnFromSuccess(taskType, prompt, agentType, output string, duration time.Duration, tokens int) error {
	return es.LearnFromSuccessWithTools(taskType, prompt, agentType, output, nil, duration, tokens)
}

// LearnFromSuccessWithTools records a successful task with tool sequence.
func (es *ExampleStore) LearnFromSuccessWithTools(taskType, prompt, agentType, output string, toolSeq []ToolCallExample, duration time.Duration, tokens int) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	// Generate tags from prompt
	tags := extractTags(prompt)

	// Extract tools used from sequence
	var toolsUsed []string
	toolSet := make(map[string]bool)
	for _, tc := range toolSeq {
		if !toolSet[tc.ToolName] {
			toolsUsed = append(toolsUsed, tc.ToolName)
			toolSet[tc.ToolName] = true
		}
	}

	// Truncate output for storage
	truncatedOutput := output
	if len(truncatedOutput) > 2000 {
		truncatedOutput = truncatedOutput[:2000] + "...[truncated]"
	}

	example := &TaskExample{
		ID:           generateExampleID(),
		TaskType:     taskType,
		InputPrompt:  prompt,
		AgentType:    agentType,
		ToolsUsed:    toolsUsed,
		ToolSequence: toolSeq,
		FinalOutput:  truncatedOutput,
		Duration:     duration,
		TokensUsed:   tokens,
		SuccessScore: 1.0, // Starts at 100%
		Tags:         tags,
		Created:      time.Now(),
		UseCount:     0,
	}

	es.examples[example.ID] = example
	es.byType[taskType] = append(es.byType[taskType], example.ID)

	// Limit examples per type to prevent unbounded growth
	es.pruneOldExamples(taskType, 50) // Keep top 50 per type

	// Save asynchronously
	go func() {
		if err := es.save(); err != nil {
			logging.Debug("failed to save example store", "error", err)
		}
	}()

	return nil
}

// extractTags extracts keywords from a prompt for matching.
func extractTags(prompt string) []string {
	// Simple keyword extraction - split and filter
	words := strings.Fields(strings.ToLower(prompt))
	tagSet := make(map[string]bool)

	// Common stop words to ignore
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"to": true, "for": true, "and": true, "or": true, "in": true,
		"of": true, "that": true, "this": true, "it": true, "with": true,
		"on": true, "be": true, "as": true, "by": true, "at": true,
		"from": true, "can": true, "how": true, "what": true, "where": true,
		"i": true, "you": true, "we": true, "they": true, "my": true,
		"please": true, "help": true, "me": true,
	}

	for _, word := range words {
		// Clean word
		word = strings.Trim(word, ".,!?;:'\"()[]{}*")
		if len(word) < 3 {
			continue
		}
		if stopWords[word] {
			continue
		}
		tagSet[word] = true
	}

	var tags []string
	for tag := range tagSet {
		tags = append(tags, tag)
	}

	return tags
}

// pruneOldExamples removes old, low-performing examples.
func (es *ExampleStore) pruneOldExamples(taskType string, maxCount int) {
	ids := es.byType[taskType]
	if len(ids) <= maxCount {
		return
	}

	// Sort by score (desc), then by created (desc)
	sort.Slice(ids, func(i, j int) bool {
		ei := es.examples[ids[i]]
		ej := es.examples[ids[j]]
		if ei.SuccessScore != ej.SuccessScore {
			return ei.SuccessScore > ej.SuccessScore
		}
		return ei.Created.After(ej.Created)
	})

	// Remove excess examples
	for _, id := range ids[maxCount:] {
		delete(es.examples, id)
	}
	es.byType[taskType] = ids[:maxCount]
}

// GetSimilarExamples finds examples similar to the given prompt.
func (es *ExampleStore) GetSimilarExamples(prompt string, limit int) []TaskExampleSummary {
	es.mu.RLock()
	defer es.mu.RUnlock()

	promptTags := extractTags(prompt)
	if len(promptTags) == 0 {
		return nil
	}

	type scored struct {
		example *TaskExample
		score   float64
	}

	var scoredExamples []scored

	for _, ex := range es.examples {
		// Calculate similarity score based on tag overlap
		overlap := 0
		for _, tag := range promptTags {
			for _, exTag := range ex.Tags {
				if tag == exTag || strings.Contains(exTag, tag) || strings.Contains(tag, exTag) {
					overlap++
					break
				}
			}
		}

		if overlap == 0 {
			continue
		}

		// Score = (overlap / promptTags) * successScore
		score := (float64(overlap) / float64(len(promptTags))) * ex.SuccessScore

		scoredExamples = append(scoredExamples, scored{
			example: ex,
			score:   score,
		})
	}

	// Sort by score
	sort.Slice(scoredExamples, func(i, j int) bool {
		return scoredExamples[i].score > scoredExamples[j].score
	})

	// Take top N
	if limit > len(scoredExamples) {
		limit = len(scoredExamples)
	}

	results := make([]TaskExampleSummary, limit)
	for i := 0; i < limit; i++ {
		ex := scoredExamples[i].example
		results[i] = TaskExampleSummary{
			ID:          ex.ID,
			TaskType:    ex.TaskType,
			InputPrompt: ex.InputPrompt,
			AgentType:   ex.AgentType,
			Duration:    ex.Duration,
			Score:       scoredExamples[i].score,
		}
	}

	return results
}

// TaskExampleSummary contains a summary of a task example.
type TaskExampleSummary struct {
	ID          string
	TaskType    string
	InputPrompt string
	AgentType   string
	Duration    time.Duration
	Score       float64
}

// GetExamplesForContext returns formatted examples for injection into prompts.
func (es *ExampleStore) GetExamplesForContext(taskType, prompt string, limit int) string {
	similar := es.GetSimilarExamples(prompt, limit)
	if len(similar) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## Similar Past Tasks (for reference)\n\n")

	for i, summary := range similar {
		es.mu.RLock()
		ex, ok := es.examples[summary.ID]
		es.mu.RUnlock()

		if !ok {
			continue
		}

		sb.WriteString("### Example ")
		sb.WriteString(string(rune('A' + i)))
		sb.WriteString("\n")
		sb.WriteString("**Request:** ")
		sb.WriteString(truncateString(ex.InputPrompt, 200))
		sb.WriteString("\n")
		sb.WriteString("**Agent:** ")
		sb.WriteString(ex.AgentType)
		sb.WriteString("\n")
		if len(ex.ToolsUsed) > 0 {
			sb.WriteString("**Tools:** ")
			sb.WriteString(strings.Join(ex.ToolsUsed, ", "))
			sb.WriteString("\n")
		}
		sb.WriteString("**Outcome:** ")
		sb.WriteString(truncateString(ex.FinalOutput, 300))
		sb.WriteString("\n\n")

		// Mark as used
		es.mu.Lock()
		if e, ok := es.examples[summary.ID]; ok {
			e.UseCount++
		}
		es.mu.Unlock()
	}

	return sb.String()
}

// truncateString truncates a string to max length.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// RecordFeedback updates the success score based on user feedback.
func (es *ExampleStore) RecordFeedback(exampleID string, positive bool) {
	es.mu.Lock()
	defer es.mu.Unlock()

	ex, ok := es.examples[exampleID]
	if !ok {
		return
	}

	// Adjust score using exponential moving average
	adjustment := 0.1
	if positive {
		ex.SuccessScore = ex.SuccessScore + (1.0-ex.SuccessScore)*adjustment
	} else {
		ex.SuccessScore = ex.SuccessScore * (1.0 - adjustment)
	}

	// Save asynchronously
	go func() {
		if err := es.save(); err != nil {
			logging.Debug("failed to save example store", "error", err)
		}
	}()
}

// GetStats returns statistics about the example store.
func (es *ExampleStore) GetStats() ExampleStoreStats {
	es.mu.RLock()
	defer es.mu.RUnlock()

	stats := ExampleStoreStats{
		TotalExamples: len(es.examples),
		ByType:        make(map[string]int),
	}

	var totalScore float64
	for _, ex := range es.examples {
		stats.ByType[ex.TaskType]++
		totalScore += ex.SuccessScore
	}

	if stats.TotalExamples > 0 {
		stats.AvgSuccessScore = totalScore / float64(stats.TotalExamples)
	}

	return stats
}

// ExampleStoreStats contains statistics about the example store.
type ExampleStoreStats struct {
	TotalExamples   int
	ByType          map[string]int
	AvgSuccessScore float64
}

// Clear removes all examples.
func (es *ExampleStore) Clear() error {
	es.mu.Lock()
	defer es.mu.Unlock()

	es.examples = make(map[string]*TaskExample)
	es.byType = make(map[string][]string)
	return es.save()
}
