package commands

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gokin/internal/semantic"
)

type SemanticCleanupCommand struct{}

func (c *SemanticCleanupCommand) Name() string        { return "semantic-cleanup" }
func (c *SemanticCleanupCommand) Description() string { return "Clean up old semantic search caches" }
func (c *SemanticCleanupCommand) Usage() string       { return "/semantic-cleanup [list|clean] [days]" }
func (c *SemanticCleanupCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySemanticSearch,
		Icon:     "cleanup",
		Priority: 20,
		HasArgs:  true,
		ArgHint:  "list|clean [days]",
	}
}

func (c *SemanticCleanupCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	// Get semantic indexer from app
	indexer, err := app.GetSemanticIndexer()
	if err != nil {
		return fmt.Sprintf("❌ Semantic search not available: %v", err), nil
	}
	if indexer == nil {
		return "❌ Semantic search is not enabled in config", nil
	}

	var sb strings.Builder

	// Parse arguments
	action := "list"
	olderThanDays := 30

	if len(args) > 0 {
		action = args[0]
	}

	if len(args) > 1 && action == "clean" {
		if _, err := fmt.Sscanf(args[1], "%d", &olderThanDays); err != nil {
			return fmt.Sprintf("❌ Invalid days value: %s", args[1]), nil
		}
	}

	// Header
	sb.WriteString("🧹 Semantic Cache Cleanup\n")
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n\n")

	switch action {
	case "list":
		return c.listProjects(sb, indexer, app)

	case "clean":
		return c.cleanOldProjects(sb, indexer, olderThanDays, app)

	default:
		return fmt.Sprintf("❌ Unknown action: %s\n\nUsage: /semantic-cleanup [list|clean] [days]", action), nil
	}
}

func (c *SemanticCleanupCommand) listProjects(sb strings.Builder, indexer *semantic.EnhancedIndexer, app AppInterface) (string, error) {
	sb.WriteString("📋 Cached Projects\n\n")

	// Get project manager from indexer
	projectMgr := indexer.GetProjectManager()

	// Get all projects from cache directory
	projects, err := projectMgr.ListCachedProjects()
	if err != nil {
		return fmt.Sprintf("❌ Failed to list projects: %v", err), nil
	}

	if len(projects) == 0 {
		sb.WriteString("No cached projects found.\n")
		return sb.String(), nil
	}

	// Current project
	currentProjectID := indexer.GetProjectID()
	workDir := app.GetWorkDir()

	cacheDir := projectMgr.GetProjectCacheDir(semantic.ProjectID(currentProjectID))
	sb.WriteString(fmt.Sprintf("Cache Directory: %s\n\n", filepath.Dir(cacheDir)))
	sb.WriteString(fmt.Sprintf("Current Project:\n"))
	sb.WriteString(fmt.Sprintf("  Path:     %s\n", workDir))
	sb.WriteString(fmt.Sprintf("  ID:       %s\n\n", currentProjectID))

	// List all projects
	sb.WriteString(fmt.Sprintf("Cached Projects (%d):\n\n", len(projects)))

	for i, proj := range projects {
		prefix := "  "
		if proj.ProjectID == currentProjectID {
			prefix = "→ "
		}

		sb.WriteString(fmt.Sprintf("%s%d. %s\n", prefix, i+1, proj.ProjectID[:8]))
		sb.WriteString(fmt.Sprintf("     Path: %s\n", proj.Path))
		sb.WriteString(fmt.Sprintf("     Age:  %s\n", formatTime(proj.LastModified)))
		sb.WriteString(fmt.Sprintf("     Size: %s\n", formatBytes(proj.SizeBytes)))

		if proj.ProjectID == currentProjectID {
			sb.WriteString("     (current project)\n")
		}

		sb.WriteString("\n")
	}

	// Footer
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n")
	sb.WriteString("💡 Tips:\n")
	sb.WriteString("  Use /semantic-cleanup clean 30 to remove projects older than 30 days\n")
	sb.WriteString("  Use /semantic-cleanup clean 0 to remove all except current")

	return sb.String(), nil
}

func (c *SemanticCleanupCommand) cleanOldProjects(sb strings.Builder, indexer *semantic.EnhancedIndexer, olderThanDays int, app AppInterface) (string, error) {
	currentProjectID := indexer.GetProjectID()

	sb.WriteString(fmt.Sprintf("Action: Clean projects older than %d days\n\n", olderThanDays))

	if olderThanDays == 0 {
		sb.WriteString("⚠️  WARNING: This will remove ALL cached projects except the current one!\n")
		sb.WriteString("Current project will be preserved.\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Projects not accessed in %d+ days will be removed.\n\n", olderThanDays))
	}

	// Get all projects
	projects, err := indexer.ListCachedProjects()
	if err != nil {
		return fmt.Sprintf("❌ Failed to list projects: %v", err), nil
	}

	// Filter projects to remove
	var toRemove []semantic.CachedProjectInfo
	for _, proj := range projects {
		// Never remove current project
		if proj.ProjectID == currentProjectID {
			continue
		}

		// Check age
		if olderThanDays == 0 || isOlderThanDays(proj.LastModified, olderThanDays) {
			toRemove = append(toRemove, proj)
		}
	}

	if len(toRemove) == 0 {
		sb.WriteString("✅ No projects to clean.\n")
		return sb.String(), nil
	}

	sb.WriteString(fmt.Sprintf("Found %d project(s) to remove:\n\n", len(toRemove)))

	for i, proj := range toRemove {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, proj.ProjectID[:8]))
		sb.WriteString(fmt.Sprintf("   Path: %s\n", proj.Path))
		sb.WriteString(fmt.Sprintf("   Age:  %s\n\n", formatTime(proj.LastModified)))
	}

	// Remove projects
	sb.WriteString("Removing...\n\n")

	removed := 0
	for _, proj := range toRemove {
		if err := indexer.RemoveProject(proj.ProjectID); err != nil {
			sb.WriteString(fmt.Sprintf("❌ Failed to remove %s: %v\n", proj.ProjectID[:8], err))
		} else {
			sb.WriteString(fmt.Sprintf("✓ Removed %s (%s)\n", proj.ProjectID[:8], filepath.Base(proj.Path)))
			removed++
		}
	}

	sb.WriteString(fmt.Sprintf("\nRemoved %d/%d projects\n", removed, len(toRemove)))

	// Get updated stats
	stats := indexer.GetStats()
	sb.WriteString(fmt.Sprintf("\nRemaining: %d chunks (%s)\n", stats.TotalChunks, formatBytes(int64(stats.CacheSizeBytes))))

	return sb.String(), nil
}

// isOlderThanDays checks if a timestamp is older than the specified number of days.
func isOlderThanDays(t int64, days int) bool {
	ageSeconds := int64(days * 86400)
	currentTime := time.Now().Unix()
	return (currentTime - t) > ageSeconds
}
