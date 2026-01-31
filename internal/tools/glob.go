package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"google.golang.org/genai"

	"gooner/internal/cache"
	"gooner/internal/git"
	"gooner/internal/security"
)

// GlobTool finds files matching a glob pattern.
type GlobTool struct {
	workDir       string
	gitIgnore     *git.GitIgnore
	cache         *cache.SearchCache
	pathValidator *security.PathValidator
}

// NewGlobTool creates a new GlobTool instance.
func NewGlobTool(workDir string) *GlobTool {
	gitIgnore := git.NewGitIgnore(workDir)
	_ = gitIgnore.Load() // Ignore error - gitignore is optional

	return &GlobTool{
		workDir:       workDir,
		gitIgnore:     gitIgnore,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetGitIgnore sets the gitignore instance for the tool.
func (t *GlobTool) SetGitIgnore(gi *git.GitIgnore) {
	t.gitIgnore = gi
}

// SetCache sets the search cache for the tool.
func (t *GlobTool) SetCache(c *cache.SearchCache) {
	t.cache = c
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *GlobTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *GlobTool) Name() string {
	return "glob"
}

func (t *GlobTool) Description() string {
	return `Finds files matching a glob pattern. Returns file paths sorted by modification time (newest first).

PARAMETERS:
- pattern (required): Glob pattern to match files
- path (optional): Directory to search in (default: current working directory)

PATTERN SYNTAX:
- *: Matches any characters except /
- **: Matches any characters including / (recursive)
- ?: Matches single character
- [abc]: Matches any character in brackets
- {a,b}: Matches either a or b

COMMON PATTERNS:
- "**/*.go" - All Go files recursively
- "**/*.{ts,tsx}" - All TypeScript files
- "src/**/*" - All files in src directory
- "**/test*" - All files starting with "test"
- "**/*_test.go" - All Go test files
- "config.*" - All config files (any extension)
- "**/main.*" - All main files at any depth

LIMITATIONS:
- Maximum 1000 results returned
- Gitignored files are excluded
- Directories are not included (files only)
- Sorted by modification time (newest first)

AFTER FINDING FILES - YOU MUST:
1. Summarize what types of files were found
2. Group files by category (source, tests, config, etc.)
3. Highlight important/relevant files
4. Suggest which files to read next
5. If no results, suggest alternative patterns`
}

func (t *GlobTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern": {
					Type:        genai.TypeString,
					Description: "The glob pattern to match (e.g., '**/*.go', 'src/**/*.ts')",
				},
				"path": {
					Type:        genai.TypeString,
					Description: "The directory to search in. Defaults to current working directory.",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

func (t *GlobTool) Validate(args map[string]any) error {
	pattern, ok := GetString(args, "pattern")
	if !ok || pattern == "" {
		return NewValidationError("pattern", "is required")
	}
	return nil
}

func (t *GlobTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	pattern, _ := GetString(args, "pattern")
	searchPath := GetStringDefault(args, "path", t.workDir)

	// Make path absolute if relative
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(t.workDir, searchPath)
	}

	// Validate path if validator is configured
	if t.pathValidator != nil {
		validPath, err := t.pathValidator.ValidateDir(searchPath)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
		}
		searchPath = validPath
	}

	// Check cache first
	var cacheKey string
	if t.cache != nil {
		cacheKey = cache.GlobKey(pattern, searchPath)
		if cached, ok := t.cache.GetGlob(cacheKey); ok {
			// Return cached results
			if len(cached.Files) == 0 {
				return NewSuccessResult("(no matches)"), nil
			}
			var builder strings.Builder
			for _, f := range cached.Files {
				relPath, err := filepath.Rel(t.workDir, f)
				if err != nil {
					relPath = f
				}
				builder.WriteString(relPath)
				builder.WriteString("\n")
			}
			return NewSuccessResult(builder.String()), nil
		}
	}

	// Check if search path exists
	if _, err := os.Stat(searchPath); err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("path not found: %s", searchPath)), nil
		}
		return NewErrorResult(fmt.Sprintf("error accessing path: %s", err)), nil
	}

	// Build full pattern
	fullPattern := filepath.Join(searchPath, pattern)

	// Find matches
	matches, err := doublestar.FilepathGlob(fullPattern)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("invalid pattern: %s", err)), nil
	}

	// Filter out directories, gitignored files, and sort by modification time
	type fileInfo struct {
		path    string
		modTime int64
	}
	var files []fileInfo

	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			// Filter by gitignore
			if t.gitIgnore != nil && t.gitIgnore.IsIgnored(match) {
				continue
			}
			files = append(files, fileInfo{
				path:    match,
				modTime: info.ModTime().Unix(),
			})
		}
	}

	// Sort by modification time (newest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime > files[j].modTime
	})

	// Limit results
	const maxResults = 1000
	totalFound := len(files)
	if len(files) > maxResults {
		files = files[:maxResults]
	}

	// Cache the results
	if t.cache != nil && cacheKey != "" {
		filePaths := make([]string, len(files))
		for i, f := range files {
			filePaths[i] = f.path
		}
		t.cache.SetGlob(cacheKey, cache.GlobResult{
			Files: filePaths,
		})
	}

	// Build output
	if len(files) == 0 {
		return NewSuccessResult("(no matches)"), nil
	}

	var builder strings.Builder
	if totalFound > maxResults {
		builder.WriteString(fmt.Sprintf("(showing %d of %d+)\n", maxResults, totalFound))
	}
	for _, f := range files {
		// Make path relative if possible
		relPath, err := filepath.Rel(t.workDir, f.path)
		if err != nil {
			relPath = f.path
		}
		builder.WriteString(relPath)
		builder.WriteString("\n")
	}

	return NewSuccessResult(builder.String()), nil
}
