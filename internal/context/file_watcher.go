package context

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileWatcher watches a file or directory for changes.
type FileWatcher struct {
	path       string
	debounceMs int
	callback   func(string)
	cancel     context.CancelFunc

	mu      sync.Mutex
	lastMod time.Time
	timer   *time.Timer
}

// NewFileWatcher creates a new file watcher.
func NewFileWatcher(ctx context.Context, path string, debounceMs int, callback func(string)) (*FileWatcher, error) {
	if debounceMs <= 0 {
		debounceMs = 500 // Default 500ms debounce
	}

	fw := &FileWatcher{
		path:       path,
		debounceMs: debounceMs,
		callback:   callback,
	}

	ctx, cancel := context.WithCancel(ctx)
	fw.cancel = cancel

	// Get initial mod time
	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if info != nil {
		fw.lastMod = info.ModTime()
	}

	// Start watching goroutine
	go fw.watch(ctx)

	return fw, nil
}

// watch periodically checks for file changes.
func (fw *FileWatcher) watch(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fw.checkChanges()
		}
	}
}

// checkChanges checks if the file has been modified.
func (fw *FileWatcher) checkChanges() {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	info, err := os.Stat(fw.path)
	if err != nil {
		// File might not exist yet, that's ok
		return
	}

	modTime := info.ModTime()
	if !modTime.Equal(fw.lastMod) && modTime.Sub(fw.lastMod) > 100*time.Millisecond {
		// File changed
		fw.lastMod = modTime

		// Debounce
		if fw.timer != nil {
			fw.timer.Stop()
		}

		fw.timer = time.AfterFunc(time.Duration(fw.debounceMs)*time.Millisecond, func() {
			fw.callback(fw.path)
		})
	}
}

// Close stops the file watcher.
func (fw *FileWatcher) Close() {
	if fw.cancel != nil {
		fw.cancel()
	}
	if fw.timer != nil {
		fw.timer.Stop()
	}
}

// WatchInstructionFiles watches all possible instruction file locations.
func WatchInstructionFiles(ctx context.Context, workDir string, debounceMs int, callback func()) error {
	// Watch the directory containing instruction files
	watchDir := workDir
	watcher, err := NewFileWatcher(ctx, watchDir, debounceMs, func(changedPath string) {
		// Check if any instruction file exists and changed
		for _, filename := range instructionFiles {
			path := filepath.Join(workDir, filename)
			if changedPath == path || changedPath == workDir {
				callback()
				return
			}
		}
	})

	if err != nil {
		return err
	}

	// Keep watcher alive until context is done
	go func() {
		<-ctx.Done()
		watcher.Close()
	}()

	return nil
}
