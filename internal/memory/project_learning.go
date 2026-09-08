package memory

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gokin/internal/logging"
	"gopkg.in/yaml.v3"
)

var (
	projectLearningMu    sync.Mutex
	projectLearningCache = make(map[string]*ProjectLearning)
)

// projectLearningSaveDebounceInterval is a variable so tests can drive the
// debounced-save path deterministically without waiting for the production
// two-second window.
var projectLearningSaveDebounceInterval = 2 * time.Second

// projectLearningSaveIOHookForTest is invoked after the debounced save enters
// its write phase, before the first AtomicWrite. Tests use it to widen the
// Timer.Stop/in-flight-write race window.
var projectLearningSaveIOHookForTest func()

// ProjectLearning manages project-specific learned patterns and preferences.
// Data is stored in .gokin/learning.yaml within the project directory.
type ProjectLearning struct {
	path         string
	markdownPath string
	data         *ProjectData
	mu           sync.RWMutex
	dirty        bool
	saveFunc     func()

	// ioMu serializes the disk-write phase of the debounced save against Flush.
	ioMu sync.Mutex

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
	gokinDir := filepath.Join(projectRoot, ".gokin")
	path := filepath.Join(gokinDir, "learning.yaml")
	markdownPath := filepath.Join(gokinDir, "project-memory.md")
	if err := ensureProjectLearningStorage(gokinDir, path, markdownPath); err != nil {
		return nil, err
	}

	pl := &ProjectLearning{
		path:         path,
		markdownPath: markdownPath,
		data: &ProjectData{
			Preferences: make(map[string]string),
		},
	}

	// Load existing data
	if err := pl.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		var storageErr *projectLearningStorageError
		if errors.As(err, &storageErr) {
			return nil, err
		}
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
		pl.timer = time.AfterFunc(projectLearningSaveDebounceInterval, func() {
			defer func() {
				if r := recover(); r != nil {
					logging.Error("project learning save timer panicked", "panic", r)
				}
			}()
			pl.mu.Lock()
			if !pl.dirty {
				pl.mu.Unlock()
				return
			}
			data, markdown, err := pl.snapshotLocked()
			if err != nil {
				pl.mu.Unlock()
				return
			}
			pl.dirty = false
			pl.mu.Unlock()

			// Write outside lock — disk I/O no longer blocks readers/writers.
			pl.ioMu.Lock()
			defer pl.ioMu.Unlock()
			if projectLearningSaveIOHookForTest != nil {
				projectLearningSaveIOHookForTest()
			}
			if err := writeProjectLearningFile(pl.path, data); err != nil {
				logging.Warn("failed to save project learning", "path", pl.path, "error", err)
				pl.mu.Lock()
				pl.dirty = true
				pl.mu.Unlock()
				return
			}
			if err := writeProjectLearningFile(pl.markdownPath, []byte(markdown)); err != nil {
				logging.Warn("failed to save project memory markdown", "path", pl.markdownPath, "error", err)
				pl.mu.Lock()
				pl.dirty = true
				pl.mu.Unlock()
			}
		})
	}

	return pl, nil
}

// GetSharedProjectLearning returns a shared ProjectLearning instance for a
// project root to avoid concurrent writers managing the same file independently.
func GetSharedProjectLearning(projectRoot string) (*ProjectLearning, error) {
	cacheKey := normalizeProjectRoot(projectRoot)

	projectLearningMu.Lock()
	if cached, ok := projectLearningCache[cacheKey]; ok {
		projectLearningMu.Unlock()
		return cached, nil
	}

	pl, err := NewProjectLearning(cacheKey)
	if err != nil {
		projectLearningMu.Unlock()
		return nil, err
	}

	projectLearningCache[cacheKey] = pl
	projectLearningMu.Unlock()
	return pl, nil
}

func normalizeProjectRoot(projectRoot string) string {
	root := projectRoot
	if abs, err := filepath.Abs(projectRoot); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return filepath.Clean(root)
}

// load reads data from the YAML file.
func (pl *ProjectLearning) load() error {
	data, err := readProjectLearningFile(pl.path)
	if err != nil {
		return err
	}

	var loaded ProjectData
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		return err
	}

	sanitizeProjectData(&loaded)
	pl.data = &loaded

	return nil
}

// save writes data to the YAML file.
func (pl *ProjectLearning) save() error {
	data, markdown, err := pl.snapshotLocked()
	if err != nil {
		return err
	}

	if err := writeProjectLearningFile(pl.path, data); err != nil {
		return err
	}
	return writeProjectLearningFile(pl.markdownPath, []byte(markdown))
}

func (pl *ProjectLearning) snapshotLocked() ([]byte, string, error) {
	pl.data.LastUpdated = time.Now()

	data, err := yaml.Marshal(pl.data)
	if err != nil {
		return nil, "", err
	}

	return data, pl.renderMarkdownLocked(), nil
}

// LearnCommand records a command execution with success/failure tracking.
func (pl *ProjectLearning) LearnCommand(cmd, desc string, success bool, durationMs float64) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if cmd == "" {
		return
	}

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
	if len(pl.data.Commands) > maxProjectLearningCommands {
		pl.data.Commands = sanitizeLearnedCommands(pl.data.Commands)
	}

	pl.dirty = true
	pl.saveFunc()
}

// LearnPattern records a code pattern.
// LearnPattern records a pattern and reports whether it survived. At the limit
// the store re-sorts by name and keeps the first N, so a newly added pattern
// can be the one dropped — while dirty is already set, which made the write
// look successful downstream. Same failure as SetPreference, through a
// different door.
func (pl *ProjectLearning) LearnPattern(name, description string, examples []string, tags []string) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if name == "" {
		return false
	}

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
			Examples:    uniqueStringTail(examples, maxProjectLearningExamples),
			Tags:        uniqueStringHead(tags, maxProjectLearningTags),
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
		if ex != "" && !existingExamples[ex] {
			pattern.Examples = append(pattern.Examples, ex)
			existingExamples[ex] = true
		}
	}

	// Limit examples
	if len(pattern.Examples) > maxProjectLearningExamples {
		pattern.Examples = uniqueStringTail(pattern.Examples, maxProjectLearningExamples)
	}
	if len(pl.data.Patterns) > maxProjectLearningPatterns {
		pl.data.Patterns = sanitizeLearnedPatterns(pl.data.Patterns)
	}

	pl.dirty = true
	pl.saveFunc()
	return patternPresent(pl.data.Patterns, name)
}

func patternPresent(patterns []LearnedPattern, name string) bool {
	for i := range patterns {
		if patterns[i].Name == name {
			return true
		}
	}
	return false
}

// LearnFileType records conventions for a file type.
func (pl *ProjectLearning) LearnFileType(ext string, conventions []string) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if ext == "" {
		return
	}

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
		if c != "" && !existing[c] {
			fileType.Conventions = append(fileType.Conventions, c)
			existing[c] = true
		}
	}
	fileType.Conventions = uniqueStringTail(fileType.Conventions, maxProjectLearningConventions)
	if len(pl.data.FileTypes) > maxProjectLearningFileTypes {
		pl.data.FileTypes = sanitizeLearnedFileTypes(pl.data.FileTypes)
	}

	pl.dirty = true
	pl.saveFunc()
}

// SetPreference sets a project preference.
// SetPreference stores an entry and reports whether it survived. A refusal at
// the limit used to be a Warn and a bare return, which the caller could not
// see: memorize then reported "Memorized fact: X (already current — nothing to
// write)", asserting both that the fact was stored and that it had been there
// all along. The model reads that as success, so the fact is lost and every
// later one is lost the same way — a memory that is permanently write-only and
// says nothing about it.
func (pl *ProjectLearning) SetPreference(key, value string) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if key == "" {
		return false
	}
	if _, exists := pl.data.Preferences[key]; !exists && len(pl.data.Preferences) >= maxProjectLearningPreferences {
		logging.Warn("project learning preference limit reached", "limit", maxProjectLearningPreferences)
		return false
	}

	pl.data.Preferences[key] = value
	pl.dirty = true
	pl.saveFunc()
	return true
}

// RemoveEntry deletes a memorized entry by key — a bare preference, an entry
// stored under the "fact:"/"convention:" prefix (how memorize namespaces
// those types), or a learned pattern by name. Returns whether anything was
// removed. This is the CORRECTION half of the memory discipline: without it
// a wrong or stale memorized fact pollutes every future session's prompt
// forever (the store is loaded into context each session).
func (pl *ProjectLearning) RemoveEntry(key string) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	removed := false
	for _, k := range []string{key, "fact:" + key, "convention:" + key} {
		if _, ok := pl.data.Preferences[k]; ok {
			delete(pl.data.Preferences, k)
			removed = true
		}
	}
	for i := range pl.data.Patterns {
		if pl.data.Patterns[i].Name == key {
			pl.data.Patterns = append(pl.data.Patterns[:i], pl.data.Patterns[i+1:]...)
			removed = true
			break
		}
	}
	if removed {
		pl.dirty = true
		pl.saveFunc()
	}
	return removed
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
	maps.Copy(result, pl.data.Preferences)
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
// maxPromptEntriesPerSection bounds how many Preferences/Facts/Conventions
// entries FormatForPrompt renders per section (alphabetical; the overflow is
// disclosed with a count). Patterns (5) and commands (3) were always capped —
// these sections weren't, and they sit in EVERY session's system prompt.
const maxPromptEntriesPerSection = 20

func (pl *ProjectLearning) FormatForPrompt() string {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("## Project Learning\n\n")

	preferences, facts, conventions := splitProjectPreferences(pl.data.Preferences)

	writeSection := func(title string, entries map[string]string) {
		if len(entries) == 0 {
			return
		}
		sb.WriteString("### " + title + "\n")
		keys := sortedPreferenceKeys(entries)
		shown := keys
		// Bound the per-session prompt cost: patterns and commands were always
		// capped, but these sections were UNBOUNDED — with the agent now
		// coached to memorize proactively, an old project would grow every
		// future session's prompt without limit. Over-cap entries are
		// disclosed, never silently hidden.
		if len(shown) > maxPromptEntriesPerSection {
			shown = shown[:maxPromptEntriesPerSection]
		}
		for _, k := range shown {
			sb.WriteString("- **" + k + "**: " + entries[k] + "\n")
		}
		if hidden := len(keys) - len(shown); hidden > 0 {
			fmt.Fprintf(&sb, "- … and %d more (see `memory` action=list)\n", hidden)
		}
		sb.WriteString("\n")
	}

	writeSection("Preferences", preferences)
	writeSection("Facts", facts)
	writeSection("Conventions", conventions)

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

func splitProjectPreferences(preferences map[string]string) (map[string]string, map[string]string, map[string]string) {
	plain := make(map[string]string)
	facts := make(map[string]string)
	conventions := make(map[string]string)

	for key, value := range preferences {
		switch {
		case strings.HasPrefix(key, "fact:"):
			facts[strings.TrimPrefix(key, "fact:")] = value
		case strings.HasPrefix(key, "convention:"):
			conventions[strings.TrimPrefix(key, "convention:")] = value
		default:
			plain[key] = value
		}
	}

	return plain, facts, conventions
}

func sortedPreferenceKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (pl *ProjectLearning) renderMarkdownLocked() string {
	preferences, facts, conventions := splitProjectPreferences(pl.data.Preferences)

	var sb strings.Builder
	sb.WriteString("# Project Memory\n\n")
	sb.WriteString("_Agent-managed durable project knowledge for future sessions._\n\n")
	if !pl.data.LastUpdated.IsZero() {
		sb.WriteString("_Updated: " + pl.data.LastUpdated.Format(time.RFC3339) + "_\n\n")
	}

	if len(preferences) == 0 && len(facts) == 0 && len(conventions) == 0 &&
		len(pl.data.Patterns) == 0 && len(pl.data.Commands) == 0 {
		sb.WriteString("No project knowledge recorded yet.\n")
		return sb.String()
	}

	writeMapSection := func(title string, values map[string]string) {
		if len(values) == 0 {
			return
		}
		sb.WriteString("## " + title + "\n")
		for _, key := range sortedPreferenceKeys(values) {
			sb.WriteString("- **" + key + "**: " + values[key] + "\n")
		}
		sb.WriteString("\n")
	}

	writeMapSection("Preferences", preferences)
	writeMapSection("Facts", facts)
	writeMapSection("Conventions", conventions)

	if len(pl.data.Patterns) > 0 {
		patterns := append([]LearnedPattern(nil), pl.data.Patterns...)
		sort.Slice(patterns, func(i, j int) bool {
			if patterns[i].UsageCount != patterns[j].UsageCount {
				return patterns[i].UsageCount > patterns[j].UsageCount
			}
			return patterns[i].LastUsed.After(patterns[j].LastUsed)
		})

		sb.WriteString("## Learned Patterns\n")
		for _, pattern := range patterns {
			sb.WriteString("- **" + pattern.Name + "**: " + pattern.Description + "\n")
		}
		sb.WriteString("\n")
	}

	successfulCmds := pl.getSuccessfulCommandsInternal(0.8, 5)
	if len(successfulCmds) > 0 {
		sb.WriteString("## Reliable Commands\n")
		for _, cmd := range successfulCmds {
			description := cmd.Description
			if description == "" {
				description = cmd.Command
			}
			sb.WriteString("- `" + cmd.Command + "`: " + description + "\n")
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
	_, err := pl.FlushChanged()
	return err
}

// FlushChanged is Flush but reports whether it ACTUALLY wrote: changed=true only
// when there were pending changes and save() succeeded; changed=false on a no-op
// flush (nothing dirty). memorize uses this so it never claims "(updated <path>)"
// for a write that did not happen — the honesty defect behind the user incident.
func (pl *ProjectLearning) FlushChanged() (changed bool, err error) {
	// Cancel pending debounced save
	pl.timerMu.Lock()
	if pl.timer != nil {
		pl.timer.Stop()
		pl.timer = nil
	}
	pl.timerMu.Unlock()

	pl.ioMu.Lock()
	defer pl.ioMu.Unlock()

	pl.mu.Lock()
	defer pl.mu.Unlock()

	if !pl.dirty {
		return false, nil
	}

	if err := pl.save(); err != nil {
		return false, err
	}
	pl.dirty = false
	return true, nil
}

// Path returns the path to the learning file.
func (pl *ProjectLearning) Path() string {
	return pl.path
}

// MarkdownPath returns the path to the human-readable project memory markdown file.
func (pl *ProjectLearning) MarkdownPath() string {
	return pl.markdownPath
}

// HasContent reports whether ProjectLearning currently contains prompt-worthy
// durable knowledge. This mirrors the sections rendered by FormatForPrompt.
func (pl *ProjectLearning) HasContent() bool {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if len(pl.data.Preferences) > 0 || len(pl.data.Patterns) > 0 {
		return true
	}

	return len(pl.getSuccessfulCommandsInternal(0.8, 1)) > 0
}

// Exists returns true if the learning file exists.
func (pl *ProjectLearning) Exists() bool {
	info, err := os.Lstat(pl.path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}
