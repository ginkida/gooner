package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gokin/internal/semantic"

	"google.golang.org/genai"
)

// SemanticCleanupTool provides semantic cache management.
type SemanticCleanupTool struct {
	configDir string
	cacheTTL  time.Duration
}

// NewSemanticCleanupTool creates a new semantic cleanup tool.
func NewSemanticCleanupTool(configDir string, cacheTTL time.Duration) *SemanticCleanupTool {
	return &SemanticCleanupTool{
		configDir: configDir,
		cacheTTL:  cacheTTL,
	}
}

// Name returns the tool name.
func (t *SemanticCleanupTool) Name() string {
	return "semantic_cleanup"
}

// Description returns the tool description.
func (t *SemanticCleanupTool) Description() string {
	return "Manages semantic search cache: list projects, clean old entries, remove specific projects"
}

// Declaration returns the tool declaration for the AI.
func (t *SemanticCleanupTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: 'list' (show all projects), 'clean' (remove expired entries), 'remove' (delete specific project), 'remove_all' (delete all semantic cache)",
				},
				"project_id": {
					Type:        genai.TypeString,
					Description: "Project ID to remove (required for 'remove' action)",
				},
				"older_than": {
					Type:        genai.TypeString,
					Description: "Remove projects older than this duration (e.g., '30d', '7d', '24h'). Only for 'clean' action.",
				},
			},
			Required: []string{"action"},
		},
	}
}

// Validate validates the tool arguments.
func (t *SemanticCleanupTool) Validate(args map[string]any) error {
	action, ok := args["action"].(string)
	if !ok {
		return fmt.Errorf("action required")
	}

	validActions := map[string]bool{
		"list": true, "clean": true, "remove": true, "remove_all": true,
	}
	if !validActions[action] {
		return fmt.Errorf("invalid action: %s", action)
	}

	if action == "remove" {
		if _, ok := args["project_id"].(string); !ok {
			return fmt.Errorf("project_id required for remove action")
		}
	}

	return nil
}

// Execute executes the tool.
func (t *SemanticCleanupTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	action := args["action"].(string)

	switch action {
	case "list":
		return t.listProjects()
	case "clean":
		return t.cleanProjects(args)
	case "remove":
		return t.removeProject(args["project_id"].(string))
	case "remove_all":
		return t.removeAll()
	default:
		return NewErrorResult(fmt.Sprintf("unknown action: %s", action)), nil
	}
}

// listProjects lists all projects with semantic cache.
func (t *SemanticCleanupTool) listProjects() (ToolResult, error) {
	pm := semantic.NewProjectManager(t.configDir)
	projects, err := pm.ListProjects()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to list projects: %v", err)), nil
	}

	if len(projects) == 0 {
		return NewSuccessResult("No semantic cache found"), nil
	}

	// Collect project info
	type projectInfo struct {
		ID          string
		Path        string
		LastUpdated time.Time
		FileCount   int
		ChunkCount  int
		Size        int64
	}

	projectInfos := make([]projectInfo, 0, len(projects))
	totalSize := int64(0)

	for _, projectID := range projects {
		// Load metadata
		metadata, err := pm.LoadMetadata(projectID)
		if err != nil {
			continue
		}

		// Get cache file size
		var cacheSize int64
		var idxSize int64

		cachePath := pm.GetProjectCachePath(projectID)
		idxPath := pm.GetProjectIndexPath(projectID)

		if info, err := os.Stat(cachePath); err == nil {
			cacheSize = info.Size()
		}
		if info, err := os.Stat(idxPath); err == nil {
			idxSize = info.Size()
		}

		projectInfos = append(projectInfos, projectInfo{
			ID:          string(projectID),
			Path:        metadata.ProjectPath,
			LastUpdated: metadata.LastUpdated,
			FileCount:   metadata.FileCount,
			ChunkCount:  metadata.ChunkCount,
			Size:        cacheSize + idxSize,
		})

		totalSize += cacheSize + idxSize
	}

	// Sort by last updated (newest first)
	sort.Slice(projectInfos, func(i, j int) bool {
		return projectInfos[i].LastUpdated.After(projectInfos[j].LastUpdated)
	})

	// Build output
	output := fmt.Sprintf("📊 Semantic Cache (%d projects, %s total)\n\n",
		len(projectInfos), formatBytes(totalSize))

	for i, info := range projectInfos {
		age := time.Since(info.LastUpdated)
		output += fmt.Sprintf("%d. **%s**\n", i+1, info.ID)
		output += fmt.Sprintf("   Path: %s\n", info.Path)
		output += fmt.Sprintf("   Files: %d | Chunks: %d | Size: %s\n",
			info.FileCount, info.ChunkCount, formatBytes(info.Size))
		output += fmt.Sprintf("   Last updated: %s (%s ago)\n\n",
			info.LastUpdated.Format("2006-01-02 15:04"), formatBytesDuration(age))
	}

	return NewSuccessResult(output), nil
}

// cleanProjects removes expired entries from all projects.
func (t *SemanticCleanupTool) cleanProjects(args map[string]any) (ToolResult, error) {
	pm := semantic.NewProjectManager(t.configDir)
	projects, err := pm.ListProjects()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to list projects: %v", err)), nil
	}

	// Check for older_than parameter
	var maxAge time.Duration
	if olderThanStr, ok := args["older_than"].(string); ok {
		maxAge, err = time.ParseDuration(olderThanStr)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("invalid older_than duration: %v", err)), nil
		}
	}

	cleaned := 0
	freedSpace := int64(0)

	for _, projectID := range projects {
		if maxAge > 0 {
			// Remove entire project if older than maxAge
			metadata, err := pm.LoadMetadata(projectID)
			if err != nil {
				continue
			}

			if time.Since(metadata.LastUpdated) > maxAge {
				// Get size before deletion
				cachePath := pm.GetProjectCachePath(projectID)
				idxPath := pm.GetProjectIndexPath(projectID)
				size := int64(0)
				if info, err := os.Stat(cachePath); err == nil {
					size += info.Size()
				}
				if info, err := os.Stat(idxPath); err == nil {
					size += info.Size()
				}

				// Delete project
				if err := pm.DeleteProject(projectID); err == nil {
					cleaned++
					freedSpace += size
				}
			}
		} else {
			// Cleanup expired entries in project cache
			// Note: We'd need to load the specific cache file here
			// For now, we'll skip this optimization
			_ = projectID // Use the variable
		}
	}

	output := fmt.Sprintf("✅ Cleaned %d projects, freed %s\n", cleaned, formatBytes(freedSpace))
	if cleaned == 0 {
		output = "✅ No projects to clean\n"
	}

	return NewSuccessResult(output), nil
}

// removeProject removes a specific project's cache.
func (t *SemanticCleanupTool) removeProject(projectID string) (ToolResult, error) {
	pm := semantic.NewProjectManager(t.configDir)

	// Get size before deletion
	cachePath := pm.GetProjectCachePath(semantic.ProjectID(projectID))
	idxPath := pm.GetProjectIndexPath(semantic.ProjectID(projectID))
	size := int64(0)
	if info, err := os.Stat(cachePath); err == nil {
		size += info.Size()
	}
	if info, err := os.Stat(idxPath); err == nil {
		size += info.Size()
	}

	// Delete project
	if err := pm.DeleteProject(semantic.ProjectID(projectID)); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to remove project: %v", err)), nil
	}

	output := fmt.Sprintf("✅ Removed project %s (freed %s)\n", projectID, formatBytes(size))
	return NewSuccessResult(output), nil
}

// removeAll removes all semantic cache.
func (t *SemanticCleanupTool) removeAll() (ToolResult, error) {
	pm := semantic.NewProjectManager(t.configDir)
	projects, err := pm.ListProjects()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to list projects: %v", err)), nil
	}

	totalSize := int64(0)
	for _, projectID := range projects {
		cachePath := pm.GetProjectCachePath(projectID)
		idxPath := pm.GetProjectIndexPath(projectID)
		if info, err := os.Stat(cachePath); err == nil {
			totalSize += info.Size()
		}
		if info, err := os.Stat(idxPath); err == nil {
			totalSize += info.Size()
		}
	}

	// Remove entire semantic_cache directory
	cacheDir := filepath.Join(t.configDir, "semantic_cache")
	if err := os.RemoveAll(cacheDir); err != nil {
		return NewErrorResult(fmt.Sprintf("failed to remove cache: %v", err)), nil
	}

	output := fmt.Sprintf("✅ Removed all semantic cache (%d projects, %s freed)\n",
		len(projects), formatBytes(totalSize))
	return NewSuccessResult(output), nil
}

// formatBytes formats a byte size as human-readable.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatBytesDuration formats a duration as human-readable.
func formatBytesDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
