package watcher

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"gokin/internal/git"
)

// Watcher monitors file system changes in a directory.
type Watcher struct {
	fsWatcher    *fsnotify.Watcher
	workDir      string
	ignorer      *git.GitIgnore
	debounceMs   int
	maxWatches   int
	onFileChange FileChangeHandler
	pending      map[string]time.Time
	mu           sync.Mutex
	done         chan struct{}
	running      bool
	stopOnce     sync.Once
}

// NewWatcher creates a new file watcher for the specified directory.
func NewWatcher(workDir string, ignorer *git.GitIgnore, cfg Config) (*Watcher, error) {
	if !cfg.Enabled {
		return &Watcher{running: false}, nil
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	debounceMs := cfg.DebounceMs
	if debounceMs <= 0 {
		debounceMs = 500
	}

	maxWatches := cfg.MaxWatches
	if maxWatches <= 0 {
		maxWatches = 1000
	}

	return &Watcher{
		fsWatcher:  fsWatcher,
		workDir:    workDir,
		ignorer:    ignorer,
		debounceMs: debounceMs,
		maxWatches: maxWatches,
		pending:    make(map[string]time.Time),
		done:       make(chan struct{}),
	}, nil
}

// SetOnFileChange sets the callback for file change events.
func (w *Watcher) SetOnFileChange(handler FileChangeHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onFileChange = handler
}

// Start begins watching for file changes.
func (w *Watcher) Start() error {
	if w.fsWatcher == nil {
		return nil // Watcher is disabled
	}

	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return nil
	}
	w.running = true
	w.mu.Unlock()

	// Add directories to watch
	if err := w.addDirectories(); err != nil {
		return err
	}

	// Start event processing
	go w.processEvents()

	// Start debounce processor
	go w.processDebounce()

	return nil
}

// Stop stops watching for file changes.
func (w *Watcher) Stop() error {
	if w.fsWatcher == nil {
		return nil
	}

	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return nil
	}
	w.running = false
	w.mu.Unlock()

	w.stopOnce.Do(func() {
		close(w.done)
	})
	return w.fsWatcher.Close()
}

// addDirectories adds directories to the watcher up to maxWatches.
func (w *Watcher) addDirectories() error {
	watchCount := 0

	err := filepath.Walk(w.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible paths
		}

		if watchCount >= w.maxWatches {
			return filepath.SkipDir
		}

		// Skip if ignored
		if w.ignorer != nil && w.ignorer.IsIgnored(path) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Only watch directories
		if info.IsDir() {
			// Skip common directories that don't need watching
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				name == ".idea" || name == ".vscode" || name == "__pycache__" ||
				name == "target" || name == "build" || name == "dist" {
				return filepath.SkipDir
			}

			if err := w.fsWatcher.Add(path); err != nil {
				return nil // Don't fail on individual directory errors
			}
			watchCount++
		}

		return nil
	})

	return err
}

// processEvents processes raw fsnotify events.
func (w *Watcher) processEvents() {
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case _, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			// Log error but continue watching
		}
	}
}

// handleEvent handles a single fsnotify event.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	path := event.Name

	// Skip if ignored
	if w.ignorer != nil && w.ignorer.IsIgnored(path) {
		return
	}

	// Skip temporary files
	base := filepath.Base(path)
	if len(base) > 0 && (base[0] == '.' || base[0] == '#' || base[len(base)-1] == '~') {
		return
	}

	// If a new directory was created, add it to the watch list
	if event.Op&fsnotify.Create != 0 {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			// Skip common directories that don't need watching
			name := info.Name()
			if name != ".git" && name != "node_modules" && name != "vendor" &&
				name != ".idea" && name != ".vscode" && name != "__pycache__" &&
				name != "target" && name != "build" && name != "dist" {
				// Check if we're under the max watches limit (protected by mutex to avoid race)
				w.mu.Lock()
				watchCount := len(w.fsWatcher.WatchList())
				if watchCount < w.maxWatches {
					_ = w.fsWatcher.Add(path)
				}
				w.mu.Unlock()
			}
		}
	}

	// Add to pending with current time (for debouncing)
	w.mu.Lock()
	w.pending[path] = time.Now()
	w.mu.Unlock()
}

// processDebounce processes debounced events.
func (w *Watcher) processDebounce() {
	ticker := time.NewTicker(time.Duration(w.debounceMs/2) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.flushPending()
		}
	}
}

// flushPending sends events for paths that have been stable.
func (w *Watcher) flushPending() {
	w.mu.Lock()
	handler := w.onFileChange
	if handler == nil || len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}

	now := time.Now()
	debounce := time.Duration(w.debounceMs) * time.Millisecond
	toSend := make([]string, 0)

	for path, eventTime := range w.pending {
		if now.Sub(eventTime) >= debounce {
			toSend = append(toSend, path)
			delete(w.pending, path)
		}
	}
	w.mu.Unlock()

	// Send events outside of lock
	for _, path := range toSend {
		op := w.detectOperation(path)
		handler(path, op)
	}
}

// detectOperation determines the type of operation for a path.
func (w *Watcher) detectOperation(path string) Operation {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return OpDelete
	}
	// We can't easily distinguish between create and modify without tracking state
	// Default to modify as it's more common
	return OpModify
}

// IsRunning returns whether the watcher is running.
func (w *Watcher) IsRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// WatchedPaths returns the number of watched paths.
func (w *Watcher) WatchedPaths() int {
	if w.fsWatcher == nil {
		return 0
	}
	return len(w.fsWatcher.WatchList())
}
