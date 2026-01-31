package git

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

// pattern represents a single gitignore pattern.
type pattern struct {
	pattern  string
	negation bool // starts with !
	dirOnly  bool // ends with /
	anchored bool // contains / in middle
	baseDir  string
}

// GitIgnore parses and matches gitignore patterns.
type GitIgnore struct {
	workDir      string
	patterns     []pattern
	mu           sync.RWMutex
	loaded       bool
	resultCache  map[string]bool // path -> isIgnored cache
	cacheOrder   []string        // for LRU eviction
	maxCacheSize int
}

// NewGitIgnore creates a new GitIgnore instance.
func NewGitIgnore(workDir string) *GitIgnore {
	return &GitIgnore{
		workDir:      workDir,
		patterns:     make([]pattern, 0),
		resultCache:  make(map[string]bool),
		cacheOrder:   make([]string, 0),
		maxCacheSize: 1000,
	}
}

// Load parses .gitignore files recursively.
func (g *GitIgnore) Load() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.patterns = make([]pattern, 0)
	g.loaded = true

	// Load root .gitignore
	rootGitignore := filepath.Join(g.workDir, ".gitignore")
	if err := g.loadFile(rootGitignore, g.workDir); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Load global .gitignore from git config (optional, skip if not found)
	globalGitignore := g.getGlobalGitignore()
	if globalGitignore != "" {
		if err := g.loadFile(globalGitignore, g.workDir); err != nil && !os.IsNotExist(err) {
			// Ignore errors for global gitignore
		}
	}

	// Walk directories to find nested .gitignore files
	err := filepath.Walk(g.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Skip .git directory
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}

		if !info.IsDir() && info.Name() == ".gitignore" && path != rootGitignore {
			baseDir := filepath.Dir(path)
			if err := g.loadFile(path, baseDir); err != nil && !os.IsNotExist(err) {
				// Continue even on error
			}
		}
		return nil
	})

	// Always add .git directory to ignores
	g.patterns = append(g.patterns, pattern{
		pattern:  ".git",
		negation: false,
		dirOnly:  true,
		anchored: false,
		baseDir:  g.workDir,
	})

	return err
}

// loadFile parses a single .gitignore file.
func (g *GitIgnore) loadFile(path, baseDir string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		p := g.parseLine(line, baseDir)
		if p != nil {
			g.patterns = append(g.patterns, *p)
		}
	}

	return scanner.Err()
}

// parseLine parses a single gitignore line.
func (g *GitIgnore) parseLine(line, baseDir string) *pattern {
	// Skip empty lines and comments
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}

	p := &pattern{
		baseDir: baseDir,
	}

	// Check for negation
	if strings.HasPrefix(line, "!") {
		p.negation = true
		line = line[1:]
	}

	// Check for directory-only pattern
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	// Check if pattern is anchored (contains / that is not trailing)
	if strings.Contains(line, "/") {
		p.anchored = true
	}

	// Handle leading slash (explicit root anchor)
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = line[1:]
	}

	p.pattern = line
	return p
}

// IsIgnored checks if a path should be ignored.
func (g *GitIgnore) IsIgnored(path string) bool {
	// Check cache first with read lock
	g.mu.RLock()
	if !g.loaded {
		g.mu.RUnlock()
		return false
	}
	if result, ok := g.resultCache[path]; ok {
		g.mu.RUnlock()
		return result
	}
	g.mu.RUnlock()

	// Calculate and cache the result
	result := g.calculateIsIgnored(path)
	g.cacheResult(path, result)
	return result
}

// calculateIsIgnored performs the actual gitignore check without caching.
func (g *GitIgnore) calculateIsIgnored(path string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Make path relative to workDir
	relPath, err := filepath.Rel(g.workDir, path)
	if err != nil {
		relPath = path
	}

	// Normalize to forward slashes for matching
	relPath = filepath.ToSlash(relPath)

	// Check if path is a directory
	info, err := os.Stat(path)
	isDir := err == nil && info.IsDir()

	// Apply patterns in order (last matching pattern wins)
	ignored := false
	for _, p := range g.patterns {
		if g.matchPattern(p, relPath, isDir) {
			ignored = !p.negation
		}
	}

	return ignored
}

// cacheResult adds a result to the cache with LRU eviction.
func (g *GitIgnore) cacheResult(path string, result bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Check if already in cache
	if _, ok := g.resultCache[path]; ok {
		return
	}

	// Evict oldest if at capacity
	if len(g.resultCache) >= g.maxCacheSize {
		if len(g.cacheOrder) > 0 {
			oldest := g.cacheOrder[0]
			delete(g.resultCache, oldest)
			g.cacheOrder = g.cacheOrder[1:]
		}
	}

	g.resultCache[path] = result
	g.cacheOrder = append(g.cacheOrder, path)
}

// InvalidateCache clears the result cache.
func (g *GitIgnore) InvalidateCache() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resultCache = make(map[string]bool)
	g.cacheOrder = nil
}

// matchPattern checks if a path matches a gitignore pattern.
func (g *GitIgnore) matchPattern(p pattern, relPath string, isDir bool) bool {
	// Directory-only patterns don't match files
	if p.dirOnly && !isDir {
		return false
	}

	// Make pattern relative to its base directory
	patternPath := p.pattern
	if p.baseDir != g.workDir {
		baseDirRel, err := filepath.Rel(g.workDir, p.baseDir)
		if err == nil {
			patternPath = filepath.ToSlash(filepath.Join(baseDirRel, p.pattern))
		}
	}

	// For anchored patterns, match from the start
	if p.anchored {
		return g.globMatch(patternPath, relPath) || g.globMatch(patternPath+"/**", relPath)
	}

	// For non-anchored patterns, match anywhere in the path
	// Try matching the full path
	if g.globMatch("**/"+patternPath, relPath) || g.globMatch("**/"+patternPath+"/**", relPath) {
		return true
	}

	// Try matching just the filename
	baseName := filepath.Base(relPath)
	if g.globMatch(patternPath, baseName) {
		return true
	}

	return false
}

// globMatch performs glob matching with doublestar.
func (g *GitIgnore) globMatch(pattern, path string) bool {
	matched, err := doublestar.Match(pattern, path)
	if err != nil {
		return false
	}
	return matched
}

// getGlobalGitignore returns the path to global gitignore file.
func (g *GitIgnore) getGlobalGitignore() string {
	// Check XDG config
	xdgConfig := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			xdgConfig = filepath.Join(home, ".config")
		}
	}
	if xdgConfig != "" {
		globalPath := filepath.Join(xdgConfig, "git", "ignore")
		if _, err := os.Stat(globalPath); err == nil {
			return globalPath
		}
	}

	// Check home directory
	home, err := os.UserHomeDir()
	if err == nil {
		globalPath := filepath.Join(home, ".gitignore_global")
		if _, err := os.Stat(globalPath); err == nil {
			return globalPath
		}
	}

	return ""
}

// Reload forces a reload of all gitignore files.
func (g *GitIgnore) Reload() error {
	g.InvalidateCache()
	return g.Load()
}

// AddPattern adds a pattern programmatically.
func (g *GitIgnore) AddPattern(pat string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	p := g.parseLine(pat, g.workDir)
	if p != nil {
		g.patterns = append(g.patterns, *p)
	}
}

// IsLoaded returns whether gitignore has been loaded.
func (g *GitIgnore) IsLoaded() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.loaded
}
