package commands

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SemanticReindexCommand forces a rebuild of the semantic index.
type SemanticReindexCommand struct{}

func (c *SemanticReindexCommand) Name() string { return "semantic-reindex" }
func (c *SemanticReindexCommand) Description() string {
	return "Force rebuild of semantic search index"
}
func (c *SemanticReindexCommand) Usage() string { return "/semantic-reindex" }
func (c *SemanticReindexCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySemanticSearch,
		Icon:     "reindex",
		Priority: 10,
	}
}

func (c *SemanticReindexCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	// Get semantic indexer from app
	indexer, err := app.GetSemanticIndexer()
	if err != nil {
		return fmt.Sprintf("❌ Semantic search not available: %v", err), nil
	}
	if indexer == nil {
		return "❌ Semantic search is not enabled in config", nil
	}

	var sb strings.Builder

	// Header
	sb.WriteString("🔄 Rebuilding Semantic Index\n")
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n\n")

	// Clear existing index
	sb.WriteString("Step 1: Clearing existing index...\n")
	if err := indexer.Clear(); err != nil {
		return fmt.Sprintf("❌ Failed to clear index: %v", err), nil
	}
	sb.WriteString("  ✓ Cleared\n\n")

	// Rebuild index
	sb.WriteString("Step 2: Rebuilding index...\n")
	startTime := time.Now()

	if err := indexer.Build(); err != nil {
		return fmt.Sprintf("❌ Failed to build index: %v", err), nil
	}

	duration := time.Since(startTime)

	// Get stats
	stats := indexer.GetStats()

	sb.WriteString(fmt.Sprintf("  ✓ Indexed in %s\n\n", formatDuration(duration)))

	// Results
	sb.WriteString("📊 Results\n")
	sb.WriteString(fmt.Sprintf("  Files Indexed:   %d\n", stats.FilesIndexed))
	sb.WriteString(fmt.Sprintf("  Total Chunks:    %d\n", stats.TotalChunks))
	sb.WriteString(fmt.Sprintf("  Cache Size:      %s\n", formatBytes(int64(stats.CacheSizeBytes))))
	sb.WriteString(fmt.Sprintf("  Index Size:      %s\n\n", formatBytes(int64(stats.IndexSizeBytes))))

	// Footer
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n")
	sb.WriteString("✅ Semantic index rebuilt successfully!\n")

	return sb.String(), nil
}
