package semantic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ProjectManager manages project-specific semantic data.
type ProjectManager struct {
	configDir string
	mu        sync.RWMutex
}

// NewProjectManager creates a new project manager.
func NewProjectManager(configDir string) *ProjectManager {
	return &ProjectManager{
		configDir: configDir,
	}
}

// GetProjectID generates a unique ID for a project directory.
// Uses SHA256 hash of the absolute path (first 8 bytes = 16 hex chars).
func (pm *ProjectManager) GetProjectID(projectDir string) ProjectID {
	// Normalize path
	projectDir = filepath.Clean(projectDir)

	// Make absolute
	absPath, err := filepath.Abs(projectDir)
	if err != nil {
		absPath = projectDir
	}

	// Generate hash using ContentHash (SHA256, first 8 bytes)
	return ProjectID(ContentHash(absPath))
}

// GetProjectCacheDir returns the cache directory for a specific project.
func (pm *ProjectManager) GetProjectCacheDir(projectID ProjectID) string {
	return filepath.Join(pm.configDir, "semantic_cache", string(projectID))
}

// GetProjectCachePath returns the embeddings cache file path for a specific project.
func (pm *ProjectManager) GetProjectCachePath(projectID ProjectID) string {
	return filepath.Join(pm.GetProjectCacheDir(projectID), "embeddings.gob")
}

// GetProjectIndexPath returns the index file path for a specific project.
func (pm *ProjectManager) GetProjectIndexPath(projectID ProjectID) string {
	return filepath.Join(pm.GetProjectCacheDir(projectID), "index.json")
}

// GetProjectMetadataPath returns the metadata file path for a specific project.
func (pm *ProjectManager) GetProjectMetadataPath(projectID ProjectID) string {
	return filepath.Join(pm.GetProjectCacheDir(projectID), "metadata.json")
}

// ListProjects returns all project IDs that have cached data.
func (pm *ProjectManager) ListProjects() ([]ProjectID, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	cacheDir := filepath.Join(pm.configDir, "semantic_cache")

	// Check if directory exists
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		return []ProjectID{}, nil
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, err
	}

	projects := make([]ProjectID, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		projectID := ProjectID(entry.Name())
		projects = append(projects, projectID)
	}

	return projects, nil
}

// DeleteProject removes all cached data for a specific project.
func (pm *ProjectManager) DeleteProject(projectID ProjectID) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	cacheDir := pm.GetProjectCacheDir(projectID)

	// Remove entire project cache directory
	return os.RemoveAll(cacheDir)
}

// GetTotalCacheSize returns the total size of all semantic cache files.
func (pm *ProjectManager) GetTotalCacheSize() (int64, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	cacheDir := filepath.Join(pm.configDir, "semantic_cache")

	// Check if directory exists
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		return 0, nil
	}

	var totalSize int64

	err := filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	return totalSize, err
}

// GetProjectDirSize returns the size of a specific project directory.
func (pm *ProjectManager) GetProjectDirSize(projectDir string) (int64, error) {
	var totalSize int64

	err := filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})

	return totalSize, err
}

// ProjectMetadata holds metadata about a project's semantic cache.
type ProjectMetadata struct {
	ProjectID    ProjectID `json:"project_id"`
	ProjectPath  string    `json:"project_path"`
	LastUpdated  time.Time `json:"last_updated"`
	FileCount    int       `json:"file_count"`
	ChunkCount   int       `json:"chunk_count"`
	CacheSize    int64     `json:"cache_size"`
	IndexSize    int64     `json:"index_size"`
	ModelVersion string    `json:"model_version"`
}

// LoadMetadata loads metadata for a project.
func (pm *ProjectManager) LoadMetadata(projectID ProjectID) (*ProjectMetadata, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	path := pm.GetProjectMetadataPath(projectID)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var metadata ProjectMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

// SaveMetadata saves metadata for a project.
func (pm *ProjectManager) SaveMetadata(metadata *ProjectMetadata) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path := pm.GetProjectMetadataPath(metadata.ProjectID)

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// ProjectID is a unique identifier for a project (16-char hex string).
type ProjectID string
