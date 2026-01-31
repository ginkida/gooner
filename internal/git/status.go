package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileStatus represents the git status of a file.
type FileStatus string

const (
	StatusUntracked FileStatus = "?"
	StatusModified  FileStatus = "M"
	StatusAdded     FileStatus = "A"
	StatusDeleted   FileStatus = "D"
	StatusRenamed   FileStatus = "R"
	StatusCopied    FileStatus = "C"
	StatusIgnored   FileStatus = "!"
	StatusUnknown   FileStatus = " "
)

// StatusEntry represents a file's git status.
type StatusEntry struct {
	Path   string
	Status FileStatus
}

// StatusCache caches git status information.
type StatusCache struct {
	workDir   string
	entries   map[string]FileStatus
	lastCheck time.Time
	cacheTTL  time.Duration
	mu        sync.RWMutex
}

// NewStatusCache creates a new StatusCache.
func NewStatusCache(workDir string) *StatusCache {
	return &StatusCache{
		workDir:  workDir,
		entries:  make(map[string]FileStatus),
		cacheTTL: 5 * time.Second,
	}
}

// Refresh updates the git status cache.
func (s *StatusCache) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Run git status
	cmd := exec.Command("git", "status", "--porcelain", "-uall")
	cmd.Dir = s.workDir
	output, err := cmd.Output()
	if err != nil {
		// Not a git repo or git not installed
		return err
	}

	// Clear old entries
	s.entries = make(map[string]FileStatus)

	// Parse output
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if len(line) < 3 {
			continue
		}

		status := FileStatus(string(line[1]))
		if status == " " {
			status = FileStatus(string(line[0]))
		}
		path := strings.TrimSpace(line[3:])

		// Handle renamed files (format: "R  old -> new")
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			if len(parts) == 2 {
				path = parts[1]
			}
		}

		s.entries[path] = status
	}

	s.lastCheck = time.Now()
	return nil
}

// GetStatus returns the git status of a file.
func (s *StatusCache) GetStatus(path string) FileStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Make path relative to workDir
	relPath, err := filepath.Rel(s.workDir, path)
	if err != nil {
		relPath = path
	}
	relPath = filepath.ToSlash(relPath)

	status, ok := s.entries[relPath]
	if !ok {
		return StatusUnknown
	}
	return status
}

// IsModified checks if a file has uncommitted changes.
func (s *StatusCache) IsModified(path string) bool {
	status := s.GetStatus(path)
	return status == StatusModified || status == StatusAdded || status == StatusDeleted
}

// IsUntracked checks if a file is untracked.
func (s *StatusCache) IsUntracked(path string) bool {
	return s.GetStatus(path) == StatusUntracked
}

// NeedsRefresh checks if the cache should be refreshed.
func (s *StatusCache) NeedsRefresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.lastCheck) > s.cacheTTL
}

// EnsureFresh refreshes the cache if needed.
func (s *StatusCache) EnsureFresh() error {
	if s.NeedsRefresh() {
		return s.Refresh()
	}
	return nil
}

// IsGitRepo checks if the working directory is a git repository.
func IsGitRepo(workDir string) bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = workDir
	err := cmd.Run()
	return err == nil
}
