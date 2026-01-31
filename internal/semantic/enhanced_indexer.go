package semantic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// IndexData represents the persistent index data.
type IndexData struct {
	Version     string               `json:"version"`
	ProjectPath string               `json:"project_path"`
	LastUpdated time.Time            `json:"last_updated"`
	FileCount   int                  `json:"file_count"`
	ChunkCount  int                  `json:"chunk_count"`
	Files       map[string]FileIndex `json:"files"`
}

// FileIndex represents indexed data for a single file.
type FileIndex struct {
	FilePath    string      `json:"file_path"`
	LastIndexed time.Time   `json:"last_indexed"`
	ModTime     time.Time   `json:"mod_time"`
	Size        int64       `json:"size"`
	ChunkCount  int         `json:"chunk_count"`
	Chunks      []ChunkMeta `json:"chunks"`
}

// ChunkMeta represents metadata for a chunk (without embedding).
type ChunkMeta struct {
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Hash      string `json:"hash"`
}

// EnhancedIndexer extends Indexer with persistent storage.
type EnhancedIndexer struct {
	*Indexer   // Embedded indexer
	configDir  string
	projectID  string
	indexPath  string
	projectMgr *ProjectManager
	mu         sync.RWMutex
}

// NewEnhancedIndexer creates a new enhanced indexer with persistence.
func NewEnhancedIndexer(embedder *Embedder, workDir string, cache *EmbeddingCache, maxFileSize int64, configDir string) *EnhancedIndexer {
	// Create base indexer
	baseIndexer := NewIndexer(embedder, workDir, cache, maxFileSize)

	// Get project ID from cache
	projectID := cache.GetProjectID()

	indexPath := filepath.Join(configDir, "semantic_cache", projectID, "index.json")

	// Create project manager
	projectMgr := NewProjectManager(configDir)

	return &EnhancedIndexer{
		Indexer:    baseIndexer,
		configDir:  configDir,
		projectID:  projectID,
		indexPath:  indexPath,
		projectMgr: projectMgr,
	}
}

// LoadIndex loads the index from disk if available and fresh.
func (ei *EnhancedIndexer) LoadIndex(maxAge time.Duration) (bool, error) {
	ei.mu.Lock()
	defer ei.mu.Unlock()

	// Check if index file exists
	info, err := os.Stat(ei.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // No index exists yet
		}
		return false, err
	}

	// Check if index is too old
	if time.Since(info.ModTime()) > maxAge {
		return false, nil // Index is stale
	}

	// Load index data
	data, err := os.ReadFile(ei.indexPath)
	if err != nil {
		return false, err
	}

	var indexData IndexData
	if err := json.Unmarshal(data, &indexData); err != nil {
		return false, err
	}

	// Verify project path matches
	if indexData.ProjectPath != ei.workDir {
		// Project moved - need to reindex
		return false, nil
	}

	// Load chunks from cache and restore index
	loaded := 0
	for filePath, fileIndex := range indexData.Files {
		// Check if file still exists and hasn't changed
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			continue // File removed
		}

		// Check if file was modified
		if !fileInfo.ModTime().Before(fileIndex.ModTime) {
			continue // File modified
		}

		// Restore chunks from cache
		chunks := make([]ChunkInfo, 0, len(fileIndex.Chunks))
		for _, chunkMeta := range fileIndex.Chunks {
			cacheKey := fmt.Sprintf("%s:%d", filePath, chunkMeta.LineStart)

			// Try to get embedding from cache
			if embedding, ok := ei.cache.Get(cacheKey, chunkMeta.Hash); ok {
				chunks = append(chunks, ChunkInfo{
					FilePath:  filePath,
					LineStart: chunkMeta.LineStart,
					LineEnd:   chunkMeta.LineEnd,
					Embedding: embedding,
					// Content will be loaded on-demand
				})
			}
		}

		if len(chunks) > 0 {
			ei.chunks[filePath] = chunks
			loaded++
		}
	}

	return loaded > 0, nil
}

// SaveIndex saves the current index to disk.
func (ei *EnhancedIndexer) SaveIndex() error {
	ei.mu.Lock()
	defer ei.mu.Unlock()

	// Build index data
	indexData := IndexData{
		Version:     "1.0",
		ProjectPath: ei.workDir,
		LastUpdated: time.Now(),
		FileCount:   len(ei.chunks),
		ChunkCount:  0,
		Files:       make(map[string]FileIndex),
	}

	// Collect file metadata
	for filePath, chunks := range ei.chunks {
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			continue
		}

		chunkMetas := make([]ChunkMeta, 0, len(chunks))
		for _, chunk := range chunks {
			chunkMetas = append(chunkMetas, ChunkMeta{
				LineStart: chunk.LineStart,
				LineEnd:   chunk.LineEnd,
				Hash:      ContentHash(chunk.Content),
			})
			indexData.ChunkCount++
		}

		indexData.Files[filePath] = FileIndex{
			FilePath:    filePath,
			LastIndexed: time.Now(),
			ModTime:     fileInfo.ModTime(),
			Size:        fileInfo.Size(),
			ChunkCount:  len(chunks),
			Chunks:      chunkMetas,
		}
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(indexData, "", "  ")
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(ei.indexPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Write to temp file
	tmpPath := ei.indexPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	// Atomic rename
	return os.Rename(tmpPath, ei.indexPath)
}

// IndexDirectory indexes all files and persists the index.
func (ei *EnhancedIndexer) IndexDirectory(ctx context.Context, dir string) error {
	// Use embedded indexer
	err := ei.Indexer.IndexDirectory(ctx, dir)
	if err != nil {
		return err
	}

	// Persist index after indexing
	_ = ei.SaveIndex() // Ignore error - indexing succeeded

	return nil
}

// IndexFile indexes a single file and updates the persisted index.
func (ei *EnhancedIndexer) IndexFile(ctx context.Context, filePath string) error {
	// Use embedded indexer
	err := ei.Indexer.IndexFile(ctx, filePath)
	if err != nil {
		return err
	}

	// Persist index after indexing
	_ = ei.SaveIndex() // Ignore error - indexing succeeded

	return nil
}

// GetIndexStats returns statistics about the index.
func (ei *EnhancedIndexer) GetIndexStats() *IndexStats {
	ei.mu.RLock()
	defer ei.mu.RUnlock()

	stats := &IndexStats{
		ProjectPath:    ei.workDir,
		ProjectID:      ei.projectID,
		FileCount:      len(ei.chunks),
		ChunkCount:     0,
		FilesIndexed:   len(ei.chunks),
		CacheSizeBytes: 0,
		IndexSizeBytes: 0,
	}

	for _, chunks := range ei.chunks {
		stats.ChunkCount += len(chunks)
		stats.TotalChunks += len(chunks)
	}

	// Get cache size
	if ei.cache != nil {
		stats.CacheSize = ei.cache.GetSizeOnDisk()
		stats.CacheEntries = ei.cache.Size()
		stats.EmbeddingsCached = ei.cache.Size()
		stats.CacheSizeBytes = ei.cache.GetSizeOnDisk()
	}

	// Get index file size
	if info, err := os.Stat(ei.indexPath); err == nil {
		stats.IndexSize = info.Size()
		stats.IndexSizeBytes = info.Size()
		stats.LastUpdated = info.ModTime()
		stats.LastIndexTime = info.ModTime().Unix()
	}

	return stats
}

// IndexStats holds statistics about the index.
type IndexStats struct {
	ProjectPath  string    `json:"project_path"`
	ProjectID    string    `json:"project_id"`
	FileCount    int       `json:"file_count"`
	ChunkCount   int       `json:"chunk_count"`
	CacheSize    int64     `json:"cache_size"`
	CacheEntries int       `json:"cache_entries"`
	IndexSize    int64     `json:"index_size"`
	LastUpdated  time.Time `json:"last_updated"`

	// Additional fields for commands
	FilesIndexed     int   `json:"files_indexed"`
	TotalChunks      int   `json:"total_chunks"`
	CacheSizeBytes   int64 `json:"cache_size_bytes"`
	IndexSizeBytes   int64 `json:"index_size_bytes"`
	EmbeddingsCached int   `json:"embeddings_cached"`
	IndexLoads       int   `json:"index_loads"`
	LastIndexTime    int64 `json:"last_index_time"`
}

// LoadOrIndex loads the index if fresh, otherwise re-indexes.
func (ei *EnhancedIndexer) LoadOrIndex(ctx context.Context, forceReindex bool, maxAge time.Duration) error {
	// Try to load existing index
	if !forceReindex {
		loaded, err := ei.LoadIndex(maxAge)
		if err == nil && loaded {
			return nil // Index loaded successfully
		}
	}

	// Re-index the project
	return ei.IndexDirectory(ctx, ei.workDir)
}

// GetStats returns statistics about the index.
func (ei *EnhancedIndexer) GetStats() *IndexStats {
	return ei.GetIndexStats()
}

// GetProjectID returns the current project ID.
func (ei *EnhancedIndexer) GetProjectID() string {
	return ei.projectID
}

// GetProjectManager returns the project manager.
func (ei *EnhancedIndexer) GetProjectManager() *ProjectManager {
	return ei.projectMgr
}

// ListCachedProjects lists all cached projects.
func (ei *EnhancedIndexer) ListCachedProjects() ([]CachedProjectInfo, error) {
	return ei.projectMgr.ListCachedProjects()
}

// RemoveProject removes a project from the cache.
func (ei *EnhancedIndexer) RemoveProject(projectID string) error {
	return ei.projectMgr.RemoveProject(ProjectID(projectID))
}
