package semantic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gooner/internal/git"
)

// ChunkInfo represents a chunk of code with its embedding.
type ChunkInfo struct {
	FilePath  string
	LineStart int
	LineEnd   int
	Content   string
	Embedding []float32
}

// Indexer manages file indexing for semantic search.
type Indexer struct {
	embedder    *Embedder
	workDir     string
	cache       *EmbeddingCache
	gitIgnore   *git.GitIgnore
	maxFileSize int64
	chunker     Chunker
	chunks      map[string][]ChunkInfo // filePath -> chunks
	mu          sync.RWMutex
}

// NewIndexer creates a new indexer.
func NewIndexer(embedder *Embedder, workDir string, cache *EmbeddingCache, maxFileSize int64) *Indexer {
	gitIgnore := git.NewGitIgnore(workDir)
	_ = gitIgnore.Load() // Ignore error - gitignore is optional

	return &Indexer{
		embedder:    embedder,
		workDir:     workDir,
		cache:       cache,
		gitIgnore:   gitIgnore,
		maxFileSize: maxFileSize,
		chunker:     NewStructuralChunker(50, 10),
		chunks:      make(map[string][]ChunkInfo),
	}
}

// IndexFile indexes a single file.
func (i *Indexer) IndexFile(ctx context.Context, filePath string) error {
	// Check if file should be ignored
	relPath, err := filepath.Rel(i.workDir, filePath)
	if err != nil {
		relPath = filePath
	}

	if i.gitIgnore.IsIgnored(relPath) {
		return nil
	}

	// Check file size
	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if info.Size() > i.maxFileSize {
		return nil // Skip large files
	}

	// Skip binary files and non-code files
	if !isCodeFile(filePath) {
		return nil
	}

	// Read file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	// Split into chunks using structural chunker
	chunks := i.chunker.Chunk(filePath, string(content))
	if len(chunks) == 0 {
		return nil
	}

	// Generate embeddings for each chunk
	indexedChunks := make([]ChunkInfo, 0, len(chunks))
	for _, chunk := range chunks {
		// Check cache first
		cacheKey := fmt.Sprintf("%s:%d", filePath, chunk.LineStart)
		contentHash := ContentHash(chunk.Content)

		if embedding, ok := i.cache.Get(cacheKey, contentHash); ok {
			chunk.Embedding = embedding
			indexedChunks = append(indexedChunks, chunk)
			continue
		}

		// Generate embedding
		embedding, err := i.embedder.Embed(ctx, chunk.Content)
		if err != nil {
			continue // Skip chunk on error
		}

		chunk.Embedding = embedding
		indexedChunks = append(indexedChunks, chunk)

		// Cache the embedding
		i.cache.Set(cacheKey, embedding, contentHash)
	}

	// Store indexed chunks
	i.mu.Lock()
	i.chunks[filePath] = indexedChunks
	i.mu.Unlock()

	return nil
}

// IndexDirectory indexes all files in a directory recursively.
func (i *Indexer) IndexDirectory(ctx context.Context, dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Check context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if info.IsDir() {
			// Skip hidden directories and common non-code directories
			name := info.Name()
			if strings.HasPrefix(name, ".") || isSkipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}

		return i.IndexFile(ctx, path)
	})
}

// Search performs semantic search across indexed files.
func (i *Indexer) Search(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	// Generate query embedding
	queryEmbedding, err := i.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to embed query: %w", err)
	}

	// Search all chunks
	i.mu.RLock()
	var results SearchResults
	for _, chunks := range i.chunks {
		for _, chunk := range chunks {
			if chunk.Embedding == nil {
				continue
			}
			score := CosineSimilarity(queryEmbedding, chunk.Embedding)
			results = append(results, SearchResult{
				FilePath:  chunk.FilePath,
				Score:     score,
				Content:   chunk.Content,
				LineStart: chunk.LineStart,
				LineEnd:   chunk.LineEnd,
			})
		}
	}
	i.mu.RUnlock()

	// Sort by score and take top K
	sort.Sort(results)
	if len(results) > topK {
		results = results[:topK]
	}

	return results, nil
}

// GetIndexedFileCount returns the number of indexed files.
func (i *Indexer) GetIndexedFileCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.chunks)
}

// SaveCache saves the embedding cache to disk.
func (i *Indexer) SaveCache() error {
	if i.cache != nil {
		return i.cache.Save()
	}
	return nil
}



// isCodeFile checks if a file is likely a code file based on extension.
func isCodeFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	codeExts := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
		".java": true, ".c": true, ".cpp": true, ".h": true, ".hpp": true,
		".rs": true, ".rb": true, ".php": true, ".swift": true, ".kt": true,
		".scala": true, ".cs": true, ".fs": true, ".hs": true, ".ml": true,
		".lua": true, ".r": true, ".sh": true, ".bash": true, ".zsh": true,
		".sql": true, ".html": true, ".css": true, ".scss": true, ".less": true,
		".json": true, ".yaml": true, ".yml": true, ".toml": true, ".xml": true,
		".md": true, ".txt": true, ".rst": true,
	}
	return codeExts[ext]
}

// isSkipDir checks if a directory should be skipped.
func isSkipDir(name string) bool {
	skipDirs := map[string]bool{
		"node_modules": true, "vendor": true, "target": true, "build": true,
		"dist": true, "out": true, "__pycache__": true, ".git": true,
		".idea": true, ".vscode": true, "bin": true, "obj": true,
	}
	return skipDirs[name]
}
