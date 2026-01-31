package commands

import (
	"context"
	"fmt"
	"strings"
)

// SemanticStatsCommand shows semantic search statistics.
type SemanticStatsCommand struct{}

func (c *SemanticStatsCommand) Name() string        { return "semantic-stats" }
func (c *SemanticStatsCommand) Description() string { return "Show semantic search index statistics" }
func (c *SemanticStatsCommand) Usage() string       { return "/semantic-stats" }
func (c *SemanticStatsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySemanticSearch,
		Icon:     "stats",
		Priority: 0,
	}
}

func (c *SemanticStatsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
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
	sb.WriteString("🔍 Semantic Search Statistics\n")
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n\n")

	// Get index stats
	stats := indexer.GetStats()
	projectID := indexer.GetProjectID()
	workDir := app.GetWorkDir()

	// Project Info
	sb.WriteString("📁 Project\n")
	sb.WriteString(fmt.Sprintf("  Work Dir:        %s\n", workDir))
	sb.WriteString(fmt.Sprintf("  Project ID:      %s\n\n", projectID))

	// Index Stats
	sb.WriteString("📊 Index\n")
	sb.WriteString(fmt.Sprintf("  Files Indexed:   %d\n", stats.FilesIndexed))
	sb.WriteString(fmt.Sprintf("  Total Chunks:    %d\n", stats.TotalChunks))
	sb.WriteString(fmt.Sprintf("  Cache Size:      %s\n", formatBytes(int64(stats.CacheSizeBytes))))
	sb.WriteString(fmt.Sprintf("  Index Size:      %s\n\n", formatBytes(int64(stats.IndexSizeBytes))))

	// Cache Performance
	sb.WriteString("⚡ Cache Performance\n")
	sb.WriteString(fmt.Sprintf("  Embeddings:      %d cached\n", stats.EmbeddingsCached))
	sb.WriteString(fmt.Sprintf("  Index Loads:     %d\n", stats.IndexLoads))
	if stats.LastIndexTime > 0 {
		sb.WriteString(fmt.Sprintf("  Last Index Age:  %s\n\n", formatTime(stats.LastIndexTime)))
	} else {
		sb.WriteString("  Last Index Age:  never\n\n")
	}

	// Configuration
	cfg := app.GetConfig()
	sb.WriteString("⚙️  Configuration\n")
	sb.WriteString(fmt.Sprintf("  Enabled:         %v\n", cfg.Semantic.Enabled))
	sb.WriteString(fmt.Sprintf("  Chunk Size:      %d chars\n", cfg.Semantic.ChunkSize))
	sb.WriteString(fmt.Sprintf("  Max File Size:   %s\n", formatBytes(int64(cfg.Semantic.MaxFileSize))))
	sb.WriteString(fmt.Sprintf("  Cache TTL:       %s\n\n", cfg.Semantic.CacheTTL))

	// Patterns
	sb.WriteString("📋 Index Patterns\n")
	for _, pattern := range cfg.Semantic.IndexPatterns {
		sb.WriteString(fmt.Sprintf("  ✓ %s\n", pattern))
	}
	sb.WriteString("\n")

	// Exclusions
	if len(cfg.Semantic.ExcludePatterns) > 0 {
		sb.WriteString("🚫 Excluded Patterns\n")
		for _, pattern := range cfg.Semantic.ExcludePatterns {
			sb.WriteString(fmt.Sprintf("  ✗ %s\n", pattern))
		}
		sb.WriteString("\n")
	}

	// Footer
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n")
	sb.WriteString("💡 Tips:\n")
	sb.WriteString("  Use /semantic-reindex to rebuild the index\n")
	sb.WriteString("  Use /semantic-cleanup to remove old projects")

	return sb.String(), nil
}
