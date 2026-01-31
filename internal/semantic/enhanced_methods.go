package semantic

import (
	"context"
	"fmt"
)

// Clear clears all indexed data.
func (ei *EnhancedIndexer) Clear() error {
	ei.mu.Lock()
	defer ei.mu.Unlock()

	// Clear chunks
	ei.Indexer.chunks = make(map[string][]ChunkInfo)

	return nil
}

// Build rebuilds the index from scratch.
func (ei *EnhancedIndexer) Build() error {
	ctx := context.Background()

	// Clear existing data
	if err := ei.Clear(); err != nil {
		return fmt.Errorf("failed to clear index: %w", err)
	}

	// Index the work directory
	if err := ei.IndexDirectory(ctx, ei.workDir); err != nil {
		return fmt.Errorf("failed to index directory: %w", err)
	}

	// Save index
	if err := ei.SaveIndex(); err != nil {
		return fmt.Errorf("failed to save index: %w", err)
	}

	return nil
}
