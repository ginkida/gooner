package semantic

import (
	"fmt"
	"os"
	"path/filepath"
)

// ListCachedProjects lists all projects in the cache directory.
func (pm *ProjectManager) ListCachedProjects() ([]CachedProjectInfo, error) {
	cacheDir := filepath.Join(pm.configDir, "semantic_cache")

	// Read cache directory
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []CachedProjectInfo{}, nil
		}
		return nil, err
	}

	var projects []CachedProjectInfo

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Skip non-.gob files
		if filepath.Ext(entry.Name()) != ".gob" {
			continue
		}

		// Extract project ID from filename (remove .gob extension)
		projectID := ProjectID(entry.Name()[:len(entry.Name())-4])

		// Load metadata
		metadata, err := pm.LoadMetadata(projectID)
		if err != nil {
			continue
		}

		// Get project size
		projectDir := pm.GetProjectCacheDir(projectID)
		size, _ := pm.GetProjectDirSize(projectDir)

		projects = append(projects, CachedProjectInfo{
			ProjectID:    string(projectID),
			Path:         metadata.ProjectPath,
			LastModified: metadata.LastUpdated.Unix(),
			SizeBytes:    size,
			FileCount:    metadata.FileCount,
			ChunkCount:   metadata.ChunkCount,
		})
	}

	return projects, nil
}

// RemoveProject removes a project from the cache.
func (pm *ProjectManager) RemoveProject(projectID ProjectID) error {
	projectDir := pm.GetProjectCacheDir(projectID)

	// Check if directory exists
	if _, err := os.Stat(projectDir); os.IsNotExist(err) {
		return fmt.Errorf("project not found: %s", projectID)
	}

	// Remove directory
	return os.RemoveAll(projectDir)
}

// CachedProjectInfo holds information about a cached project.
type CachedProjectInfo struct {
	ProjectID    string `json:"project_id"`
	Path         string `json:"path"`
	LastModified int64  `json:"last_modified"`
	SizeBytes    int64  `json:"size_bytes"`
	FileCount    int    `json:"file_count"`
	ChunkCount   int    `json:"chunk_count"`
}
