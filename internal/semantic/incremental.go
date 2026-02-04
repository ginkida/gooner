package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gokin/internal/logging"
)

// FileState represents the state of a file for incremental indexing.
type FileState struct {
	Path       string    `json:"path"`
	ModTime    time.Time `json:"mod_time"`
	Size       int64     `json:"size"`
	Hash       string    `json:"hash"`
	LastIndexed time.Time `json:"last_indexed"`
}

// IndexingStats holds statistics about an indexing operation.
type IndexingStats struct {
	TotalFiles     int           `json:"total_files"`
	NewFiles       int           `json:"new_files"`
	ModifiedFiles  int           `json:"modified_files"`
	DeletedFiles   int           `json:"deleted_files"`
	UnchangedFiles int           `json:"unchanged_files"`
	ChunksIndexed  int           `json:"chunks_indexed"`
	Duration       time.Duration `json:"duration"`
	EmbedBatches   int           `json:"embed_batches"`
	Errors         int           `json:"errors"`
}

// IncrementalIndexer extends EnhancedIndexer with incremental indexing.
// It tracks file states and only re-indexes changed files.
type IncrementalIndexer struct {
	*EnhancedIndexer
	fileStates map[string]FileState
	batchSize  int // For EmbedBatch
	workers    int // For parallel indexing
	stateMu    sync.RWMutex
}

// NewIncrementalIndexer creates a new incremental indexer.
func NewIncrementalIndexer(base *EnhancedIndexer, batchSize, workers int) *IncrementalIndexer {
	if batchSize <= 0 {
		batchSize = 20 // Default batch size for EmbedBatch
	}
	if workers <= 0 {
		workers = 4 // Default parallel workers
	}

	return &IncrementalIndexer{
		EnhancedIndexer: base,
		fileStates:      make(map[string]FileState),
		batchSize:       batchSize,
		workers:         workers,
	}
}

// GetChangedFiles returns lists of new, modified, and deleted files.
func (i *IncrementalIndexer) GetChangedFiles(ctx context.Context, dir string) (newFiles, modifiedFiles, deletedFiles []string, err error) {
	i.stateMu.RLock()
	oldStates := make(map[string]FileState, len(i.fileStates))
	for k, v := range i.fileStates {
		oldStates[k] = v
	}
	i.stateMu.RUnlock()

	// Track seen files
	seenFiles := make(map[string]bool)

	// Walk directory
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if info.IsDir() {
			if isSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		if !isCodeFile(path) {
			return nil
		}

		seenFiles[path] = true

		oldState, exists := oldStates[path]
		if !exists {
			newFiles = append(newFiles, path)
			return nil
		}

		// Check if modified
		if info.ModTime().After(oldState.ModTime) || info.Size() != oldState.Size {
			modifiedFiles = append(modifiedFiles, path)
		}

		return nil
	})

	if err != nil {
		return nil, nil, nil, err
	}

	// Find deleted files
	for path := range oldStates {
		if !seenFiles[path] {
			deletedFiles = append(deletedFiles, path)
		}
	}

	return newFiles, modifiedFiles, deletedFiles, nil
}

// IndexChanged performs incremental indexing, only processing changed files.
func (i *IncrementalIndexer) IndexChanged(ctx context.Context, dir string) (*IndexingStats, error) {
	startTime := time.Now()
	stats := &IndexingStats{}

	// Get changed files
	newFiles, modifiedFiles, deletedFiles, err := i.GetChangedFiles(ctx, dir)
	if err != nil {
		return nil, err
	}

	stats.NewFiles = len(newFiles)
	stats.ModifiedFiles = len(modifiedFiles)
	stats.DeletedFiles = len(deletedFiles)

	// Remove deleted files from index
	i.stateMu.Lock()
	for _, path := range deletedFiles {
		delete(i.fileStates, path)
		delete(i.chunks, path)
	}
	i.stateMu.Unlock()

	// Combine files to index
	filesToIndex := make([]string, 0, len(newFiles)+len(modifiedFiles))
	filesToIndex = append(filesToIndex, newFiles...)
	filesToIndex = append(filesToIndex, modifiedFiles...)

	if len(filesToIndex) == 0 {
		// Count unchanged files
		i.stateMu.RLock()
		stats.UnchangedFiles = len(i.fileStates)
		stats.TotalFiles = stats.UnchangedFiles
		i.stateMu.RUnlock()
		stats.Duration = time.Since(startTime)
		return stats, nil
	}

	// Index files in parallel with batched embedding
	chunksIndexed, embedBatches, errors := i.indexFilesParallel(ctx, filesToIndex)
	stats.ChunksIndexed = chunksIndexed
	stats.EmbedBatches = embedBatches
	stats.Errors = errors

	// Update file states
	i.updateFileStates(filesToIndex)

	// Count total
	i.stateMu.RLock()
	stats.TotalFiles = len(i.fileStates)
	i.stateMu.RUnlock()

	stats.UnchangedFiles = stats.TotalFiles - stats.NewFiles - stats.ModifiedFiles
	stats.Duration = time.Since(startTime)

	// Save index after incremental update
	if err := i.SaveIndex(); err != nil {
		logging.Warn("failed to save index after incremental update", "error", err)
	}

	return stats, nil
}

// indexFilesParallel indexes files in parallel using worker goroutines.
func (i *IncrementalIndexer) indexFilesParallel(ctx context.Context, files []string) (chunksIndexed, embedBatches, errors int) {
	if len(files) == 0 {
		return 0, 0, 0
	}

	// Channel for files to process
	filesChan := make(chan string, len(files))
	for _, f := range files {
		filesChan <- f
	}
	close(filesChan)

	// Results channel
	type result struct {
		path      string
		chunks    []ChunkInfo
		embedBatch int
		err       error
	}
	resultsChan := make(chan result, len(files))

	// Start workers
	var wg sync.WaitGroup
	for w := 0; w < i.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range filesChan {
				select {
				case <-ctx.Done():
					return
				default:
				}

				chunks, batches, err := i.indexFileWithBatch(ctx, path)
				resultsChan <- result{
					path:      path,
					chunks:    chunks,
					embedBatch: batches,
					err:       err,
				}
			}
		}()
	}

	// Wait for workers and close results
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Collect results
	for res := range resultsChan {
		if res.err != nil {
			errors++
			logging.Debug("error indexing file", "path", res.path, "error", res.err)
			continue
		}
		if len(res.chunks) > 0 {
			i.mu.Lock()
			i.chunks[res.path] = res.chunks
			i.mu.Unlock()
			chunksIndexed += len(res.chunks)
			embedBatches += res.embedBatch
		}
	}

	return chunksIndexed, embedBatches, errors
}

// indexFileWithBatch indexes a single file using batched embedding.
func (i *IncrementalIndexer) indexFileWithBatch(ctx context.Context, filePath string) ([]ChunkInfo, int, error) {
	// Check file size
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, 0, err
	}
	if info.Size() > i.maxFileSize {
		return nil, 0, nil // Skip large files
	}

	// Read file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, 0, err
	}

	// Get chunks
	chunks := i.chunker.Chunk(filePath, string(content))
	if len(chunks) == 0 {
		return nil, 0, nil
	}

	// Separate cached and uncached chunks
	var cachedChunks []ChunkInfo
	var uncachedChunks []ChunkInfo
	var uncachedTexts []string
	var uncachedIndices []int

	for idx, chunk := range chunks {
		cacheKey := chunkCacheKey(filePath, chunk.LineStart)
		contentHash := ContentHash(chunk.Content)

		if embedding, ok := i.cache.Get(cacheKey, contentHash); ok {
			chunk.Embedding = embedding
			cachedChunks = append(cachedChunks, chunk)
		} else {
			uncachedChunks = append(uncachedChunks, chunk)
			uncachedTexts = append(uncachedTexts, chunk.Content)
			uncachedIndices = append(uncachedIndices, idx)
		}
	}

	// Batch embed uncached chunks
	embedBatches := 0
	if len(uncachedTexts) > 0 {
		// Process in batches
		for start := 0; start < len(uncachedTexts); start += i.batchSize {
			end := start + i.batchSize
			if end > len(uncachedTexts) {
				end = len(uncachedTexts)
			}

			select {
			case <-ctx.Done():
				return nil, embedBatches, ctx.Err()
			default:
			}

			batchTexts := uncachedTexts[start:end]
			embeddings, err := i.embedder.EmbedBatch(ctx, batchTexts)
			if err != nil {
				logging.Debug("batch embed failed", "error", err, "batch_size", len(batchTexts))
				continue
			}
			embedBatches++

			// Assign embeddings to chunks
			for j, embedding := range embeddings {
				chunkIdx := start + j
				if chunkIdx < len(uncachedChunks) {
					uncachedChunks[chunkIdx].Embedding = embedding

					// Cache the embedding
					cacheKey := chunkCacheKey(filePath, uncachedChunks[chunkIdx].LineStart)
					contentHash := ContentHash(uncachedChunks[chunkIdx].Content)
					i.cache.Set(cacheKey, embedding, contentHash)
				}
			}
		}
	}

	// Combine all chunks with embeddings
	result := make([]ChunkInfo, 0, len(cachedChunks)+len(uncachedChunks))
	result = append(result, cachedChunks...)
	for _, chunk := range uncachedChunks {
		if chunk.Embedding != nil {
			result = append(result, chunk)
		}
	}

	// Sort by line start
	sort.Slice(result, func(a, b int) bool {
		return result[a].LineStart < result[b].LineStart
	})

	return result, embedBatches, nil
}

// updateFileStates updates file states after indexing.
func (i *IncrementalIndexer) updateFileStates(files []string) {
	now := time.Now()
	i.stateMu.Lock()
	defer i.stateMu.Unlock()

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		// Compute hash for change detection
		content, err := os.ReadFile(path)
		hash := ""
		if err == nil {
			h := sha256.Sum256(content)
			hash = hex.EncodeToString(h[:8]) // First 8 bytes for compact hash
		}

		i.fileStates[path] = FileState{
			Path:        path,
			ModTime:     info.ModTime(),
			Size:        info.Size(),
			Hash:        hash,
			LastIndexed: now,
		}
	}
}

// GetFileStates returns a copy of current file states.
func (i *IncrementalIndexer) GetFileStates() map[string]FileState {
	i.stateMu.RLock()
	defer i.stateMu.RUnlock()

	states := make(map[string]FileState, len(i.fileStates))
	for k, v := range i.fileStates {
		states[k] = v
	}
	return states
}

// LoadFileStates loads file states from the index.
func (i *IncrementalIndexer) LoadFileStates() {
	i.mu.RLock()
	defer i.mu.RUnlock()

	i.stateMu.Lock()
	defer i.stateMu.Unlock()

	// Infer file states from indexed chunks
	for path := range i.chunks {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		i.fileStates[path] = FileState{
			Path:        path,
			ModTime:     info.ModTime(),
			Size:        info.Size(),
			LastIndexed: time.Now(),
		}
	}
}

// SetBatchSize sets the batch size for embedding.
func (i *IncrementalIndexer) SetBatchSize(size int) {
	if size > 0 {
		i.batchSize = size
	}
}

// SetWorkers sets the number of parallel workers.
func (i *IncrementalIndexer) SetWorkers(workers int) {
	if workers > 0 {
		i.workers = workers
	}
}

// chunkCacheKey generates a cache key for a chunk.
func chunkCacheKey(filePath string, lineStart int) string {
	return filePath + ":" + string(rune(lineStart))
}

// ReindexAll forces a full re-index, ignoring cached states.
func (i *IncrementalIndexer) ReindexAll(ctx context.Context, dir string) (*IndexingStats, error) {
	// Clear all states
	i.stateMu.Lock()
	i.fileStates = make(map[string]FileState)
	i.stateMu.Unlock()

	i.mu.Lock()
	i.chunks = make(map[string][]ChunkInfo)
	i.mu.Unlock()

	// Now index as if all files are new
	return i.IndexChanged(ctx, dir)
}
