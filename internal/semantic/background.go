package semantic

import (
	"context"
	"sync"
	"time"

	"gokin/internal/logging"
	"gokin/internal/watcher"
)

// BackgroundIndexerConfig holds configuration for background indexing.
type BackgroundIndexerConfig struct {
	// Interval between automatic re-indexing checks
	Interval time.Duration
	// DebounceInterval for file change events
	DebounceInterval time.Duration
	// MaxPendingFiles limits how many files can queue before force processing
	MaxPendingFiles int
	// BatchSize for embedding
	BatchSize int
	// Workers for parallel indexing
	Workers int
}

// DefaultBackgroundIndexerConfig returns default configuration.
func DefaultBackgroundIndexerConfig() *BackgroundIndexerConfig {
	return &BackgroundIndexerConfig{
		Interval:         5 * time.Minute,
		DebounceInterval: 2 * time.Second,
		MaxPendingFiles:  100,
		BatchSize:        20,
		Workers:          4,
	}
}

// BackgroundIndexer handles automatic re-indexing in the background.
type BackgroundIndexer struct {
	indexer      *IncrementalIndexer
	watcher      *watcher.Watcher
	config       *BackgroundIndexerConfig
	workDir      string

	pendingFiles map[string]time.Time
	pendingMu    sync.Mutex

	ctx        context.Context
	cancel     context.CancelFunc
	running    bool
	runningMu  sync.Mutex

	// Callbacks
	onIndexStart    func()
	onIndexComplete func(stats *IndexingStats)
	onError         func(error)
}

// NewBackgroundIndexer creates a new background indexer.
func NewBackgroundIndexer(
	indexer *IncrementalIndexer,
	fileWatcher *watcher.Watcher,
	workDir string,
	config *BackgroundIndexerConfig,
) *BackgroundIndexer {
	if config == nil {
		config = DefaultBackgroundIndexerConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &BackgroundIndexer{
		indexer:      indexer,
		watcher:      fileWatcher,
		config:       config,
		workDir:      workDir,
		pendingFiles: make(map[string]time.Time),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// SetOnIndexStart sets callback for when indexing starts.
func (bi *BackgroundIndexer) SetOnIndexStart(fn func()) {
	bi.onIndexStart = fn
}

// SetOnIndexComplete sets callback for when indexing completes.
func (bi *BackgroundIndexer) SetOnIndexComplete(fn func(stats *IndexingStats)) {
	bi.onIndexComplete = fn
}

// SetOnError sets callback for errors.
func (bi *BackgroundIndexer) SetOnError(fn func(error)) {
	bi.onError = fn
}

// Start begins background indexing.
func (bi *BackgroundIndexer) Start() error {
	bi.runningMu.Lock()
	if bi.running {
		bi.runningMu.Unlock()
		return nil
	}
	bi.running = true
	bi.runningMu.Unlock()

	// Set up file watcher callback
	if bi.watcher != nil {
		bi.watcher.SetOnFileChange(bi.onFileChange)
	}

	// Start periodic indexing
	go bi.periodicIndexLoop()

	// Start debounce processor
	go bi.debounceLoop()

	logging.Debug("background indexer started",
		"interval", bi.config.Interval,
		"debounce", bi.config.DebounceInterval)

	return nil
}

// Stop stops background indexing.
func (bi *BackgroundIndexer) Stop() {
	bi.runningMu.Lock()
	if !bi.running {
		bi.runningMu.Unlock()
		return
	}
	bi.running = false
	bi.runningMu.Unlock()

	bi.cancel()

	// Save the index before stopping
	if bi.indexer != nil {
		if err := bi.indexer.SaveIndex(); err != nil {
			logging.Warn("failed to save index on stop", "error", err)
		}
	}

	logging.Debug("background indexer stopped")
}

// IsRunning returns whether the background indexer is running.
func (bi *BackgroundIndexer) IsRunning() bool {
	bi.runningMu.Lock()
	defer bi.runningMu.Unlock()
	return bi.running
}

// onFileChange handles file change events from watcher.
func (bi *BackgroundIndexer) onFileChange(path string, op watcher.Operation) {
	// Only handle code files
	if !isCodeFile(path) {
		return
	}

	bi.pendingMu.Lock()
	bi.pendingFiles[path] = time.Now()
	pendingCount := len(bi.pendingFiles)
	bi.pendingMu.Unlock()

	logging.Debug("file change detected",
		"path", path,
		"operation", op.String(),
		"pending", pendingCount)

	// If we have too many pending files, process immediately
	if pendingCount >= bi.config.MaxPendingFiles {
		go bi.processPendingFiles()
	}
}

// periodicIndexLoop runs periodic full index checks.
func (bi *BackgroundIndexer) periodicIndexLoop() {
	ticker := time.NewTicker(bi.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-bi.ctx.Done():
			return
		case <-ticker.C:
			bi.runIncrementalIndex()
		}
	}
}

// debounceLoop processes pending files after debounce interval.
func (bi *BackgroundIndexer) debounceLoop() {
	ticker := time.NewTicker(bi.config.DebounceInterval / 2)
	defer ticker.Stop()

	for {
		select {
		case <-bi.ctx.Done():
			return
		case <-ticker.C:
			bi.checkAndProcessPending()
		}
	}
}

// checkAndProcessPending checks if any files have been stable long enough.
func (bi *BackgroundIndexer) checkAndProcessPending() {
	bi.pendingMu.Lock()
	if len(bi.pendingFiles) == 0 {
		bi.pendingMu.Unlock()
		return
	}

	now := time.Now()
	stableFiles := make([]string, 0)

	for path, eventTime := range bi.pendingFiles {
		if now.Sub(eventTime) >= bi.config.DebounceInterval {
			stableFiles = append(stableFiles, path)
			delete(bi.pendingFiles, path)
		}
	}
	bi.pendingMu.Unlock()

	if len(stableFiles) > 0 {
		bi.indexFiles(stableFiles)
	}
}

// processPendingFiles processes all pending files immediately.
func (bi *BackgroundIndexer) processPendingFiles() {
	bi.pendingMu.Lock()
	files := make([]string, 0, len(bi.pendingFiles))
	for path := range bi.pendingFiles {
		files = append(files, path)
	}
	bi.pendingFiles = make(map[string]time.Time)
	bi.pendingMu.Unlock()

	if len(files) > 0 {
		bi.indexFiles(files)
	}
}

// indexFiles indexes a list of specific files.
func (bi *BackgroundIndexer) indexFiles(files []string) {
	if bi.indexer == nil || len(files) == 0 {
		return
	}

	logging.Debug("indexing changed files", "count", len(files))

	if bi.onIndexStart != nil {
		bi.onIndexStart()
	}

	startTime := time.Now()

	// Index each file individually
	chunksIndexed := 0
	errors := 0

	for _, path := range files {
		select {
		case <-bi.ctx.Done():
			return
		default:
		}

		if err := bi.indexer.IndexFile(bi.ctx, path); err != nil {
			errors++
			logging.Debug("failed to index file", "path", path, "error", err)
		} else {
			chunksIndexed++
		}
	}

	stats := &IndexingStats{
		TotalFiles:    len(files),
		ModifiedFiles: len(files),
		ChunksIndexed: chunksIndexed,
		Errors:        errors,
		Duration:      time.Since(startTime),
	}

	if bi.onIndexComplete != nil {
		bi.onIndexComplete(stats)
	}

	logging.Debug("file indexing complete",
		"files", len(files),
		"duration", stats.Duration)
}

// runIncrementalIndex runs a full incremental index.
func (bi *BackgroundIndexer) runIncrementalIndex() {
	if bi.indexer == nil {
		return
	}

	logging.Debug("starting periodic incremental index")

	if bi.onIndexStart != nil {
		bi.onIndexStart()
	}

	stats, err := bi.indexer.IndexChanged(bi.ctx, bi.workDir)
	if err != nil {
		if bi.onError != nil {
			bi.onError(err)
		}
		logging.Warn("incremental index failed", "error", err)
		return
	}

	if bi.onIndexComplete != nil {
		bi.onIndexComplete(stats)
	}

	logging.Debug("incremental index complete",
		"new", stats.NewFiles,
		"modified", stats.ModifiedFiles,
		"deleted", stats.DeletedFiles,
		"duration", stats.Duration)
}

// ForceReindex triggers an immediate full re-index.
func (bi *BackgroundIndexer) ForceReindex() (*IndexingStats, error) {
	if bi.indexer == nil {
		return nil, nil
	}

	logging.Debug("forcing full re-index")

	if bi.onIndexStart != nil {
		bi.onIndexStart()
	}

	stats, err := bi.indexer.ReindexAll(bi.ctx, bi.workDir)
	if err != nil {
		if bi.onError != nil {
			bi.onError(err)
		}
		return nil, err
	}

	if bi.onIndexComplete != nil {
		bi.onIndexComplete(stats)
	}

	return stats, nil
}

// GetPendingCount returns the number of pending files.
func (bi *BackgroundIndexer) GetPendingCount() int {
	bi.pendingMu.Lock()
	defer bi.pendingMu.Unlock()
	return len(bi.pendingFiles)
}

// TriggerIndexNow processes any pending files immediately.
func (bi *BackgroundIndexer) TriggerIndexNow() {
	go bi.processPendingFiles()
}
