package context

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gokin/internal/logging"
)

// FileAccessPattern tracks how files are accessed during agent execution.
type FileAccessPattern struct {
	Path       string    `json:"path"`
	AccessedAt time.Time `json:"accessed_at"`
	AccessType string    `json:"access_type"` // read, write, grep, edit
	FromFile   string    `json:"from_file"`   // File that led to this access (for co-access tracking)
}

// CoAccessEntry tracks files that are frequently accessed together.
type CoAccessEntry struct {
	FileA     string  `json:"file_a"`
	FileB     string  `json:"file_b"`
	Count     int     `json:"count"`
	Strength  float64 `json:"strength"` // 0.0 to 1.0
	LastSeen  time.Time `json:"last_seen"`
}

// ContextPredictor predicts which files might be needed next based on access patterns.
type ContextPredictor struct {
	// Access history for pattern detection
	accessHistory []FileAccessPattern

	// Co-access relationships: "fileA|fileB" -> CoAccessEntry
	coAccess map[string]*CoAccessEntry

	// File type relationships: "type_from|type_to" -> count
	typeRelations map[string]int

	// Import/include tracking for code files
	importGraph map[string][]string // file -> imported files

	// Working directory for relative path resolution
	workDir string

	mu sync.RWMutex
}

// NewContextPredictor creates a new context predictor.
func NewContextPredictor(workDir string) *ContextPredictor {
	return &ContextPredictor{
		accessHistory: make([]FileAccessPattern, 0),
		coAccess:      make(map[string]*CoAccessEntry),
		typeRelations: make(map[string]int),
		importGraph:   make(map[string][]string),
		workDir:       workDir,
	}
}

// RecordAccess records a file access for pattern learning.
func (cp *ContextPredictor) RecordAccess(path, accessType, fromFile string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	pattern := FileAccessPattern{
		Path:       path,
		AccessedAt: time.Now(),
		AccessType: accessType,
		FromFile:   fromFile,
	}

	cp.accessHistory = append(cp.accessHistory, pattern)

	// Trim history to last 1000 accesses
	if len(cp.accessHistory) > 1000 {
		cp.accessHistory = cp.accessHistory[len(cp.accessHistory)-1000:]
	}

	// Update co-access relationships
	if fromFile != "" && fromFile != path {
		cp.updateCoAccess(fromFile, path)
	}

	// Update type relations
	if fromFile != "" {
		fromExt := filepath.Ext(fromFile)
		toExt := filepath.Ext(path)
		if fromExt != "" && toExt != "" {
			key := fromExt + "|" + toExt
			cp.typeRelations[key]++
		}
	}
}

// updateCoAccess updates the co-access relationship between two files.
func (cp *ContextPredictor) updateCoAccess(fileA, fileB string) {
	// Normalize order for consistent key
	if fileA > fileB {
		fileA, fileB = fileB, fileA
	}
	key := fileA + "|" + fileB

	entry, ok := cp.coAccess[key]
	if !ok {
		entry = &CoAccessEntry{
			FileA: fileA,
			FileB: fileB,
		}
		cp.coAccess[key] = entry
	}

	entry.Count++
	entry.LastSeen = time.Now()

	// Calculate strength based on count and recency
	// More recent and more frequent = stronger
	entry.Strength = calculateStrength(entry.Count, entry.LastSeen)
}

// calculateStrength calculates the strength of a co-access relationship.
func calculateStrength(count int, lastSeen time.Time) float64 {
	// Base strength from count (logarithmic to prevent runaway values)
	baseStrength := 0.3 + 0.7*(1-1/float64(count+1))

	// Decay based on time since last seen (30-day half-life)
	daysSince := time.Since(lastSeen).Hours() / 24
	decay := 1.0
	if daysSince > 0 {
		decay = 1.0 / (1.0 + daysSince/30.0)
	}

	return baseStrength * decay
}

// PredictFiles predicts which files might be needed based on current context.
func (cp *ContextPredictor) PredictFiles(currentFile string, limit int) []PredictedFile {
	cp.mu.RLock()
	defer cp.mu.RUnlock()

	predictions := make(map[string]float64)

	// 1. Co-accessed files
	for key, entry := range cp.coAccess {
		if strings.Contains(key, currentFile) {
			otherFile := entry.FileA
			if otherFile == currentFile {
				otherFile = entry.FileB
			}
			predictions[otherFile] += entry.Strength * 0.5
		}
	}

	// 2. Files of related types
	currentExt := filepath.Ext(currentFile)
	if currentExt != "" {
		for key, count := range cp.typeRelations {
			parts := strings.Split(key, "|")
			if len(parts) == 2 && parts[0] == currentExt {
				// Find recent files with the related extension
				for _, access := range cp.accessHistory {
					if filepath.Ext(access.Path) == parts[1] {
						predictions[access.Path] += float64(count) * 0.01
					}
				}
			}
		}
	}

	// 3. Import graph relationships
	if imports, ok := cp.importGraph[currentFile]; ok {
		for _, imp := range imports {
			predictions[imp] += 0.4
		}
	}

	// 4. Same-directory files (weaker signal)
	currentDir := filepath.Dir(currentFile)
	for _, access := range cp.accessHistory {
		if filepath.Dir(access.Path) == currentDir && access.Path != currentFile {
			predictions[access.Path] += 0.1
		}
	}

	// Convert to sorted list
	var result []PredictedFile
	for path, score := range predictions {
		// Check if file exists
		if _, err := os.Stat(path); err == nil {
			result = append(result, PredictedFile{
				Path:       path,
				Confidence: normalizeScore(score),
				Reason:     getPredictionReason(path, currentFile, cp),
			})
		}
	}

	// Sort by confidence descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].Confidence > result[j].Confidence
	})

	// Limit results
	if len(result) > limit {
		result = result[:limit]
	}

	return result
}

// PredictedFile represents a file that might be needed.
type PredictedFile struct {
	Path       string  `json:"path"`
	Confidence float64 `json:"confidence"` // 0.0 to 1.0
	Reason     string  `json:"reason"`
}

// normalizeScore converts a raw score to 0.0-1.0 confidence.
func normalizeScore(score float64) float64 {
	if score <= 0 {
		return 0
	}
	// Use sigmoid-like function to normalize
	return score / (score + 1)
}

// getPredictionReason explains why a file was predicted.
func getPredictionReason(predictedFile, currentFile string, cp *ContextPredictor) string {
	// Check co-access
	fileA, fileB := currentFile, predictedFile
	if fileA > fileB {
		fileA, fileB = fileB, fileA
	}
	key := fileA + "|" + fileB
	if entry, ok := cp.coAccess[key]; ok && entry.Count > 2 {
		return "frequently accessed together"
	}

	// Check imports
	if imports, ok := cp.importGraph[currentFile]; ok {
		for _, imp := range imports {
			if imp == predictedFile {
				return "imported by current file"
			}
		}
	}

	// Check same directory
	if filepath.Dir(predictedFile) == filepath.Dir(currentFile) {
		return "same directory"
	}

	// Check related types
	currentExt := filepath.Ext(currentFile)
	predictedExt := filepath.Ext(predictedFile)
	if currentExt != predictedExt {
		return "related file type"
	}

	return "pattern match"
}

// LearnImports parses a file to learn its import relationships.
func (cp *ContextPredictor) LearnImports(filePath string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}

	ext := filepath.Ext(filePath)
	var imports []string

	switch ext {
	case ".go":
		imports = parseGoImports(string(content), filepath.Dir(filePath))
	case ".ts", ".tsx", ".js", ".jsx":
		imports = parseJSImports(string(content), filepath.Dir(filePath))
	case ".py":
		imports = parsePythonImports(string(content), filepath.Dir(filePath))
	}

	if len(imports) > 0 {
		cp.mu.Lock()
		cp.importGraph[filePath] = imports
		cp.mu.Unlock()

		logging.Debug("learned imports", "file", filePath, "imports", len(imports))
	}
}

// parseGoImports extracts Go import paths.
func parseGoImports(content, baseDir string) []string {
	var imports []string

	// Simple regex for Go imports
	importRe := regexp.MustCompile(`import\s+(?:\(\s*([\s\S]*?)\s*\)|"([^"]+)")`)
	matches := importRe.FindAllStringSubmatch(content, -1)

	for _, match := range matches {
		if match[1] != "" {
			// Multi-line import block
			lineRe := regexp.MustCompile(`"([^"]+)"`)
			lineMatches := lineRe.FindAllStringSubmatch(match[1], -1)
			for _, lm := range lineMatches {
				imports = append(imports, lm[1])
			}
		} else if match[2] != "" {
			imports = append(imports, match[2])
		}
	}

	return imports
}

// parseJSImports extracts JavaScript/TypeScript import paths.
func parseJSImports(content, baseDir string) []string {
	var imports []string

	// Match import statements
	importRe := regexp.MustCompile(`import\s+(?:.*?\s+from\s+)?['"]([^'"]+)['"]`)
	matches := importRe.FindAllStringSubmatch(content, -1)

	for _, match := range matches {
		importPath := match[1]
		// Convert relative imports to absolute paths
		if strings.HasPrefix(importPath, "./") || strings.HasPrefix(importPath, "../") {
			absPath := filepath.Join(baseDir, importPath)
			// Try with common extensions
			for _, ext := range []string{"", ".ts", ".tsx", ".js", ".jsx"} {
				if _, err := os.Stat(absPath + ext); err == nil {
					imports = append(imports, absPath+ext)
					break
				}
			}
		}
	}

	return imports
}

// parsePythonImports extracts Python import paths.
func parsePythonImports(content, baseDir string) []string {
	var imports []string

	// Match from X import Y and import X
	importRe := regexp.MustCompile(`(?:from\s+(\S+)\s+import|import\s+(\S+))`)
	matches := importRe.FindAllStringSubmatch(content, -1)

	for _, match := range matches {
		module := match[1]
		if module == "" {
			module = match[2]
		}
		// Convert relative imports
		if strings.HasPrefix(module, ".") {
			relPath := strings.ReplaceAll(module, ".", string(filepath.Separator))
			absPath := filepath.Join(baseDir, relPath+".py")
			if _, err := os.Stat(absPath); err == nil {
				imports = append(imports, absPath)
			}
		}
	}

	return imports
}

// GetStats returns statistics about the predictor.
func (cp *ContextPredictor) GetStats() map[string]any {
	cp.mu.RLock()
	defer cp.mu.RUnlock()

	return map[string]any{
		"access_history_size": len(cp.accessHistory),
		"co_access_entries":   len(cp.coAccess),
		"type_relations":      len(cp.typeRelations),
		"import_graph_files":  len(cp.importGraph),
	}
}

// Clear removes all learned patterns.
func (cp *ContextPredictor) Clear() {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	cp.accessHistory = make([]FileAccessPattern, 0)
	cp.coAccess = make(map[string]*CoAccessEntry)
	cp.typeRelations = make(map[string]int)
	cp.importGraph = make(map[string][]string)
}
