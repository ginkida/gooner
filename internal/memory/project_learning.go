package memory

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ProjectLearning manages project-specific learned patterns and preferences.
// Data is stored in .gokin/learning.yaml within the project directory.
type ProjectLearning struct {
	path     string
	data     *ProjectData
	mu       sync.RWMutex
	dirty    bool
	saveFunc func()

	// Timer mutex for debounced save
	timerMu sync.Mutex
	timer   *time.Timer
}

// ProjectData contains all learned project-specific data.
type ProjectData struct {
	Patterns    []LearnedPattern  `yaml:"patterns,omitempty"`
	Preferences map[string]string `yaml:"preferences,omitempty"`
	Commands    []LearnedCommand  `yaml:"commands,omitempty"`
	FileTypes   []LearnedFileType `yaml:"file_types,omitempty"`
	LastUpdated time.Time         `yaml:"last_updated"`
}

// LearnedPattern represents a learned code pattern.
type LearnedPattern struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Examples    []string  `yaml:"examples,omitempty"`
	UsageCount  int       `yaml:"usage_count"`
	LastUsed    time.Time `yaml:"last_used"`
	Tags        []string  `yaml:"tags,omitempty"`
}

// LearnedCommand represents a learned command with success tracking.
type LearnedCommand struct {
	Command     string    `yaml:"command"`
	Description string    `yaml:"description,omitempty"`
	UsageCount  int       `yaml:"usage_count"`
	LastUsed    time.Time `yaml:"last_used"`
	SuccessRate float64   `yaml:"success_rate"`
	AvgDuration float64   `yaml:"avg_duration_ms,omitempty"` // Average duration in milliseconds
}

// LearnedFileType tracks patterns for specific file types.
type LearnedFileType struct {
	Extension   string   `yaml:"extension"`
	Conventions []string `yaml:"conventions,omitempty"`
	UsageCount  int      `yaml:"usage_count"`
}

// NewProjectLearning creates a new project learning store.
func NewProjectLearning(projectRoot string) (*ProjectLearning, error) {
	// Create .gokin directory if it doesn't exist
	gokinDir := filepath.Join(projectRoot, ".gokin")
	if err := os.MkdirAll(gokinDir, 0755); err != nil {
		return nil, err
	}

	path := filepath.Join(gokinDir, "learning.yaml")

	pl := &ProjectLearning{
		path: path,
		data: &ProjectData{
			Preferences: make(map[string]string),
		},
	}

	// Load existing data
	if err := pl.load(); err != nil && !os.IsNotExist(err) {
		// Non-fatal: start fresh if load fails
		pl.data = &ProjectData{
			Preferences: make(map[string]string),
		}
	}

	// Setup debounced save with proper synchronization
	pl.saveFunc = func() {
		pl.timerMu.Lock()
		defer pl.timerMu.Unlock()

		if pl.timer != nil {
			pl.timer.Stop()
		}
		pl.timer = time.AfterFunc(2*time.Second, func() {
			pl.mu.Lock()
			if !pl.dirty {
				pl.mu.Unlock()
				return
			}
			err := pl.save()
			if err == nil {
				pl.dirty = false
			}
			pl.mu.Unlock()
		})
	}

	return pl, nil
}

// load reads data from the YAML file.
func (pl *ProjectLearning) load() error {
	data, err := os.ReadFile(pl.path)
	if err != nil {
		return err
	}

	var loaded ProjectData
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		return err
	}

	pl.data = &loaded
	if pl.data.Preferences == nil {
		pl.data.Preferences = make(map[string]string)
	}

	return nil
}

// save writes data to the YAML file.
func (pl *ProjectLearning) save() error {
	pl.data.LastUpdated = time.Now()

	data, err := yaml.Marshal(pl.data)
	if err != nil {
		return err
	}

	return os.WriteFile(pl.path, data, 0644)
}

// LearnCommand records a command execution with success/failure tracking.
func (pl *ProjectLearning) LearnCommand(cmd, desc string, success bool, durationMs float64) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	// Find or create command entry
	var command *LearnedCommand
	for i := range pl.data.Commands {
		if pl.data.Commands[i].Command == cmd {
			command = &pl.data.Commands[i]
			break
		}
	}

	if command == nil {
		pl.data.Commands = append(pl.data.Commands, LearnedCommand{
			Command:     cmd,
			Description: desc,
			SuccessRate: 1.0,
		})
		command = &pl.data.Commands[len(pl.data.Commands)-1]
	}

	// Update statistics
	command.UsageCount++
	command.LastUsed = time.Now()

	// Update description if provided and empty
	if command.Description == "" && desc != "" {
		command.Description = desc
	}

	// Update success rate using exponential moving average
	alpha := 0.3 // Weight for new observations
	if success {
		command.SuccessRate = alpha + (1-alpha)*command.SuccessRate
	} else {
		command.SuccessRate = (1 - alpha) * command.SuccessRate
	}

	// Update average duration
	if durationMs > 0 {
		if command.AvgDuration == 0 {
			command.AvgDuration = durationMs
		} else {
			command.AvgDuration = alpha*durationMs + (1-alpha)*command.AvgDuration
		}
	}

	pl.dirty = true
	pl.saveFunc()
}

// LearnPattern records a code pattern.
func (pl *ProjectLearning) LearnPattern(name, description string, examples []string, tags []string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	// Find or create pattern
	var pattern *LearnedPattern
	for i := range pl.data.Patterns {
		if pl.data.Patterns[i].Name == name {
			pattern = &pl.data.Patterns[i]
			break
		}
	}

	if pattern == nil {
		pl.data.Patterns = append(pl.data.Patterns, LearnedPattern{
			Name:        name,
			Description: description,
			Examples:    examples,
			Tags:        tags,
		})
		pattern = &pl.data.Patterns[len(pl.data.Patterns)-1]
	}

	// Update
	pattern.UsageCount++
	pattern.LastUsed = time.Now()

	// Add new examples (deduplicated)
	existingExamples := make(map[string]bool)
	for _, ex := range pattern.Examples {
		existingExamples[ex] = true
	}
	for _, ex := range examples {
		if !existingExamples[ex] {
			pattern.Examples = append(pattern.Examples, ex)
		}
	}

	// Limit examples
	if len(pattern.Examples) > 5 {
		pattern.Examples = pattern.Examples[len(pattern.Examples)-5:]
	}

	pl.dirty = true
	pl.saveFunc()
}

// LearnFileType records conventions for a file type.
func (pl *ProjectLearning) LearnFileType(ext string, conventions []string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	var fileType *LearnedFileType
	for i := range pl.data.FileTypes {
		if pl.data.FileTypes[i].Extension == ext {
			fileType = &pl.data.FileTypes[i]
			break
		}
	}

	if fileType == nil {
		pl.data.FileTypes = append(pl.data.FileTypes, LearnedFileType{
			Extension: ext,
		})
		fileType = &pl.data.FileTypes[len(pl.data.FileTypes)-1]
	}

	fileType.UsageCount++

	// Add conventions (deduplicated)
	existing := make(map[string]bool)
	for _, c := range fileType.Conventions {
		existing[c] = true
	}
	for _, c := range conventions {
		if !existing[c] {
			fileType.Conventions = append(fileType.Conventions, c)
		}
	}

	pl.dirty = true
	pl.saveFunc()
}

// SetPreference sets a project preference.
func (pl *ProjectLearning) SetPreference(key, value string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	pl.data.Preferences[key] = value
	pl.dirty = true
	pl.saveFunc()
}

// GetPreference returns a project preference.
func (pl *ProjectLearning) GetPreference(key string) string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return pl.data.Preferences[key]
}

// GetPreferences returns all preferences.
func (pl *ProjectLearning) GetPreferences() map[string]string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	result := make(map[string]string, len(pl.data.Preferences))
	for k, v := range pl.data.Preferences {
		result[k] = v
	}
	return result
}

// GetFrequentCommands returns the most frequently used commands.
func (pl *ProjectLearning) GetFrequentCommands(limit int) []LearnedCommand {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	// Sort by usage count
	commands := make([]LearnedCommand, len(pl.data.Commands))
	copy(commands, pl.data.Commands)

	sort.Slice(commands, func(i, j int) bool {
		return commands[i].UsageCount > commands[j].UsageCount
	})

	if limit > 0 && len(commands) > limit {
		return commands[:limit]
	}
	return commands
}

// GetSuccessfulCommands returns commands with high success rate.
func (pl *ProjectLearning) GetSuccessfulCommands(minRate float64) []LearnedCommand {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	var result []LearnedCommand
	for _, cmd := range pl.data.Commands {
		if cmd.SuccessRate >= minRate && cmd.UsageCount >= 2 {
			result = append(result, cmd)
		}
	}
	return result
}

// GetPatternsByTag returns patterns with a specific tag.
func (pl *ProjectLearning) GetPatternsByTag(tag string) []LearnedPattern {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	var result []LearnedPattern
	tagLower := strings.ToLower(tag)
	for _, p := range pl.data.Patterns {
		for _, t := range p.Tags {
			if strings.ToLower(t) == tagLower {
				result = append(result, p)
				break
			}
		}
	}
	return result
}

// GetRecentPatterns returns recently used patterns.
func (pl *ProjectLearning) GetRecentPatterns(limit int) []LearnedPattern {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	patterns := make([]LearnedPattern, len(pl.data.Patterns))
	copy(patterns, pl.data.Patterns)

	sort.Slice(patterns, func(i, j int) bool {
		return patterns[i].LastUsed.After(patterns[j].LastUsed)
	})

	if limit > 0 && len(patterns) > limit {
		return patterns[:limit]
	}
	return patterns
}

// FormatForPrompt returns a formatted string for prompt injection.
func (pl *ProjectLearning) FormatForPrompt() string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("## Project Learning\n\n")

	// Preferences
	if len(pl.data.Preferences) > 0 {
		sb.WriteString("### Preferences\n")
		for k, v := range pl.data.Preferences {
			sb.WriteString("- **" + k + "**: " + v + "\n")
		}
		sb.WriteString("\n")
	}

	// Top patterns
	if len(pl.data.Patterns) > 0 {
		sb.WriteString("### Learned Patterns\n")
		count := 0
		for _, p := range pl.data.Patterns {
			if count >= 5 {
				break
			}
			sb.WriteString("- **" + p.Name + "**: " + p.Description + "\n")
			count++
		}
		sb.WriteString("\n")
	}

	// Successful commands
	successfulCmds := pl.getSuccessfulCommandsInternal(0.8, 3)
	if len(successfulCmds) > 0 {
		sb.WriteString("### Reliable Commands\n")
		for _, cmd := range successfulCmds {
			desc := cmd.Command
			if cmd.Description != "" {
				desc = cmd.Description
			}
			sb.WriteString("- `" + cmd.Command + "`: " + desc + "\n")
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// getSuccessfulCommandsInternal is the internal version without locking.
func (pl *ProjectLearning) getSuccessfulCommandsInternal(minRate float64, limit int) []LearnedCommand {
	var result []LearnedCommand
	for _, cmd := range pl.data.Commands {
		if cmd.SuccessRate >= minRate && cmd.UsageCount >= 2 {
			result = append(result, cmd)
		}
	}

	// Sort by usage count
	sort.Slice(result, func(i, j int) bool {
		return result[i].UsageCount > result[j].UsageCount
	})

	if limit > 0 && len(result) > limit {
		return result[:limit]
	}
	return result
}

// Flush cancels any pending debounced save and forces an immediate save.
func (pl *ProjectLearning) Flush() error {
	// Cancel pending debounced save
	pl.timerMu.Lock()
	if pl.timer != nil {
		pl.timer.Stop()
		pl.timer = nil
	}
	pl.timerMu.Unlock()

	pl.mu.Lock()
	defer pl.mu.Unlock()

	if !pl.dirty {
		return nil
	}

	err := pl.save()
	if err == nil {
		pl.dirty = false
	}
	return err
}

// Path returns the path to the learning file.
func (pl *ProjectLearning) Path() string {
	return pl.path
}

// Exists returns true if the learning file exists.
func (pl *ProjectLearning) Exists() bool {
	_, err := os.Stat(pl.path)
	return err == nil
}
