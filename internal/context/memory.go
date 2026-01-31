package context

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gooner/internal/logging"
)

// ProjectMemory holds project-specific instructions loaded from files.
type ProjectMemory struct {
	workDir      string
	instructions string
	sourcePath   string // Path where instructions were found
	mu           sync.RWMutex

	// File watching
	watcher       *FileWatcher
	watcherCancel context.CancelFunc
	onReload      func() // Callback when instructions are reloaded
}

// instructionFiles is the ordered list of files to search for instructions.
var instructionFiles = []string{
	"GOONER.md",
	".gooner/rules.md",
	".gooner/instructions.md",
	".gooner/INSTRUCTIONS.md",
	".gooner.md",
	"rules.md",
}

// NewProjectMemory creates a new ProjectMemory instance.
func NewProjectMemory(workDir string) *ProjectMemory {
	return &ProjectMemory{
		workDir: workDir,
	}
}

// Load searches for and loads project instructions.
// It checks files in order: GOONER.md, .gooner/instructions.md, etc.
// Returns nil error even if no file is found (instructions are optional).
func (m *ProjectMemory) Load() error {
	for _, filename := range instructionFiles {
		path := filepath.Join(m.workDir, filename)
		content, err := os.ReadFile(path)
		if err == nil {
			m.instructions = strings.TrimSpace(string(content))
			m.sourcePath = path
			// Log successful load with file info
			logging.Info("✓ Loaded project instructions",
				"source", filename,
				"path", path,
				"size_bytes", len(content),
				"lines", strings.Count(m.instructions, "\n")+1)
			return nil
		}
		// Continue searching if file not found
		if !os.IsNotExist(err) {
			// Log other errors but continue
			logging.Warn("failed to read instructions file", "file", path, "error", err)
			continue
		}
	}

	// No instructions file found - this is not an error
	logging.Debug("no project instructions found (checked GOONER.md, .gooner/instructions.md, .gooner.md)")
	return nil
}

// GetInstructions returns the loaded instructions.
func (m *ProjectMemory) GetInstructions() string {
	return m.instructions
}

// GetSourcePath returns the path where instructions were loaded from.
func (m *ProjectMemory) GetSourcePath() string {
	return m.sourcePath
}

// HasInstructions returns true if instructions were loaded.
func (m *ProjectMemory) HasInstructions() bool {
	return m.instructions != ""
}

// Reload reloads instructions from disk.
func (m *ProjectMemory) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.instructions = ""
	m.sourcePath = ""
	return m.Load()
}

// StartWatching enables automatic reloading when instruction files change.
func (m *ProjectMemory) StartWatching(ctx context.Context, debounceMs int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.watcher != nil {
		return nil // Already watching
	}

	// Find the instruction file that exists
	var watchPath string
	for _, filename := range instructionFiles {
		path := filepath.Join(m.workDir, filename)
		if _, err := os.Stat(path); err == nil {
			watchPath = path
			break
		}
	}

	if watchPath == "" {
		// No file found yet, watch for creation of any instruction file
		watchPath = filepath.Join(m.workDir, ".gooner")
		os.MkdirAll(watchPath, 0755)
	}

	watcherCtx, cancel := context.WithCancel(ctx)
	m.watcherCancel = cancel

	var err error
	m.watcher, err = NewFileWatcher(watcherCtx, watchPath, debounceMs, func(path string) {
		logging.Info("instruction file changed, reloading", "path", path)
		if reloadErr := m.Reload(); reloadErr != nil {
			logging.Warn("failed to reload instructions", "error", reloadErr)
		} else {
			logging.Info("✓ Reloaded project instructions", "source", m.GetSourcePath())
			if m.onReload != nil {
				m.onReload()
			}
		}
	})

	if err != nil {
		cancel()
		return err
	}

	logging.Info("started watching instruction files", "path", watchPath)
	return nil
}

// StopWatching disables automatic reloading.
func (m *ProjectMemory) StopWatching() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.watcherCancel != nil {
		m.watcherCancel()
		m.watcherCancel = nil
	}
	m.watcher = nil
}

// OnReload sets a callback function to be called when instructions are reloaded.
func (m *ProjectMemory) OnReload(callback func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReload = callback
}
