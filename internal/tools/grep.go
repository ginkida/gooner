package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"google.golang.org/genai"

	"gokin/internal/cache"
	"gokin/internal/git"
	"gokin/internal/security"
)

// GrepPredictorInterface defines the interface for context predictors used by grep.
type GrepPredictorInterface interface {
	RecordAccess(path, accessType, fromFile string)
}

// GrepTool searches for patterns in files.
type GrepTool struct {
	workDir       string
	gitIgnore     *git.GitIgnore
	cache         *cache.SearchCache
	pathValidator *security.PathValidator
	predictor     GrepPredictorInterface
}

// NewGrepTool creates a new GrepTool instance.
func NewGrepTool(workDir string) *GrepTool {
	gitIgnore := git.NewGitIgnore(workDir)
	_ = gitIgnore.Load() // Ignore error - gitignore is optional

	return &GrepTool{
		workDir:       workDir,
		gitIgnore:     gitIgnore,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetGitIgnore sets the gitignore instance for the tool.
func (t *GrepTool) SetGitIgnore(gi *git.GitIgnore) {
	t.gitIgnore = gi
}

// SetCache sets the search cache for the tool.
func (t *GrepTool) SetCache(c *cache.SearchCache) {
	t.cache = c
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *GrepTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

// SetPredictor sets the context predictor for access pattern learning.
func (t *GrepTool) SetPredictor(p GrepPredictorInterface) {
	t.predictor = p
}

func (t *GrepTool) Name() string {
	return "grep"
}

func (t *GrepTool) Description() string {
	return `Searches for a regex pattern in files. Returns matching lines with file paths and line numbers.

PARAMETERS:
- pattern (required): Regex pattern to search for (e.g., "func.*Error", "TODO:", "import.*react")
- path (optional): File or directory to search in (default: current directory)
- glob (optional): Filter files by pattern (e.g., "*.go", "**/*.ts", "src/**/*.js")
- case_insensitive (optional): If true, ignore case (default: false)
- context_lines (optional): Number of lines to show before/after matches (default: 0)

REGEX TIPS:
- Literal search: "functionName" - finds exact text
- Wildcards: "handle.*Error" - matches handleError, handleUserError, etc.
- Word boundary: "\bfunc\b" - matches "func" but not "function"
- Alternatives: "(error|Error|ERROR)" - matches any case

LIMITATIONS:
- Maximum 500 matches returned
- Files >10MB are skipped
- Binary files are skipped
- Gitignored files are excluded
- Regex with 5+ second compile time will timeout

COMMON PATTERNS:
- Find function: "func\s+FunctionName"
- Find imports: "import.*package"
- Find TODOs: "TODO:|FIXME:|HACK:"
- Find errors: "error|Error|panic"

AFTER SEARCHING - YOU MUST:
1. Summarize: "Found X matches in Y files"
2. Group results by category/file
3. Highlight most relevant matches
4. If no results, explain why and suggest alternatives`
}

func (t *GrepTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern": {
					Type:        genai.TypeString,
					Description: "The regex pattern to search for",
				},
				"path": {
					Type:        genai.TypeString,
					Description: "File or directory to search in. Defaults to current directory.",
				},
				"glob": {
					Type:        genai.TypeString,
					Description: "Glob pattern to filter files (e.g., '*.go', '**/*.ts')",
				},
				"case_insensitive": {
					Type:        genai.TypeBoolean,
					Description: "If true, search is case-insensitive",
				},
				"context_lines": {
					Type:        genai.TypeInteger,
					Description: "Number of context lines to show before and after matches",
				},
				"invert": {
					Type:        genai.TypeBoolean,
					Description: "If true, show lines that do NOT match the pattern (like grep -v)",
				},
				"count_only": {
					Type:        genai.TypeBoolean,
					Description: "If true, return only the count of matches per file instead of matching lines",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

func (t *GrepTool) Validate(args map[string]any) error {
	pattern, ok := GetString(args, "pattern")
	if !ok || pattern == "" {
		return NewValidationError("pattern", "is required")
	}

	// Validate regex
	_, err := regexp.Compile(pattern)
	if err != nil {
		return NewValidationError("pattern", fmt.Sprintf("invalid regex: %s", err))
	}

	return nil
}

func (t *GrepTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	pattern, _ := GetString(args, "pattern")
	searchPath := GetStringDefault(args, "path", t.workDir)
	globPattern := GetStringDefault(args, "glob", "")
	caseInsensitive := GetBoolDefault(args, "case_insensitive", false)
	contextLines := GetIntDefault(args, "context_lines", 0)
	invertMatch := GetBoolDefault(args, "invert", false)
	countOnly := GetBoolDefault(args, "count_only", false)

	// Make path absolute first (relative to workDir)
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(t.workDir, searchPath)
	}

	// Validate path if validator is configured
	// Validation happens after making absolute to ensure proper path resolution
	if t.pathValidator != nil {
		validPath, err := t.pathValidator.Validate(searchPath)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
		}
		searchPath = validPath
	}

	// Check cache first
	var cacheKey string
	if t.cache != nil {
		cacheKey = cache.GrepKey(pattern, searchPath, globPattern, caseInsensitive, contextLines)
		if cached, ok := t.cache.GetGrep(cacheKey); ok {
			// Return cached results
			content := cache.FormatCachedGrep(cached, t.workDir)
			if len(cached.Matches) == 0 {
				return NewSuccessResult("No matches found. (cached)"), nil
			}
			summary := fmt.Sprintf("Found %d match(es) in %d file(s) (cached):\n\n", len(cached.Matches), cached.FileCount)
			return NewSuccessResult(summary + content), nil
		}
	}

	// Compile regex with timeout protection
	regexPattern := pattern
	if caseInsensitive {
		regexPattern = "(?i)" + pattern
	}

	var re *regexp.Regexp
	var compileErr error
	compileDone := make(chan struct{})

	go func() {
		defer close(compileDone)
		re, compileErr = regexp.Compile(regexPattern)
	}()

	select {
	case <-compileDone:
		if compileErr != nil {
			return NewErrorResult(fmt.Sprintf("invalid regex: %s", compileErr)), nil
		}
	case <-time.After(5 * time.Second):
		// Goroutine will leak but this is acceptable for rare pathological patterns
		return NewErrorResult("regex compilation timeout: pattern too complex"), nil
	case <-ctx.Done():
		return NewErrorResult("cancelled"), ctx.Err()
	}

	// Safety check: ensure regex was compiled successfully before use
	if re == nil {
		return NewErrorResult("regex compilation failed unexpectedly"), nil
	}

	// Get files to search
	files, err := t.getFiles(searchPath, globPattern)
	if err != nil {
		return NewErrorResult(err.Error()), nil
	}

	// Search files
	const maxMatches = 500
	var fileMatches []fileMatch
	if invertMatch {
		fileMatches = t.invertMatches(ctx, files, re)
	} else {
		fileMatches = t.searchParallel(ctx, files, re, contextLines)
	}

	// Count-only mode
	if countOnly {
		var results strings.Builder
		totalCount := 0
		fileCount := 0
		for _, fm := range fileMatches {
			relPath, _ := filepath.Rel(t.workDir, fm.path)
			if relPath == "" {
				relPath = fm.path
			}
			count := len(fm.matches)
			if count > 0 {
				results.WriteString(fmt.Sprintf("%s: %d\n", relPath, count))
				totalCount += count
				fileCount++
			}
		}
		if totalCount == 0 {
			return NewSuccessResult("No matches found."), nil
		}
		summary := fmt.Sprintf("Total: %d match(es) in %d file(s):\n\n", totalCount, fileCount)
		return NewSuccessResult(summary + results.String()), nil
	}

	// Build results and cache data
	var results strings.Builder
	var cacheMatches []cache.GrepMatch
	matchCount := 0
	fileCount := 0

	for _, fm := range fileMatches {
		if matchCount >= maxMatches {
			break
		}

		fileCount++
		relPath, _ := filepath.Rel(t.workDir, fm.path)
		if relPath == "" {
			relPath = fm.path
		}

		// Record access pattern for predictive loading
		if t.predictor != nil {
			t.predictor.RecordAccess(fm.path, "grep", "")
		}

		for _, match := range fm.matches {
			if matchCount >= maxMatches {
				break
			}
			results.WriteString(fmt.Sprintf("%s:%d: %s\n", relPath, match.lineNum, match.line))
			cacheMatches = append(cacheMatches, cache.GrepMatch{
				FilePath: fm.path,
				LineNum:  match.lineNum,
				Line:     match.line,
			})
			matchCount++
		}
	}

	// Cache the results
	if t.cache != nil && cacheKey != "" {
		t.cache.SetGrep(cacheKey, cache.GrepResult{
			Matches:   cacheMatches,
			FileCount: fileCount,
		})
	}

	if matchCount == 0 {
		return NewSuccessResult("No matches found."), nil
	}

	label := "Found"
	if invertMatch {
		label = "Found (inverted)"
	}
	summary := fmt.Sprintf("%s %d match(es) in %d file(s):\n\n", label, matchCount, fileCount)
	if matchCount >= maxMatches {
		summary = fmt.Sprintf("%s %d+ match(es) in %d file(s) (capped at %d — refine pattern for complete results):\n\n", label, matchCount, fileCount, maxMatches)
	}
	return NewSuccessResult(summary + results.String()), nil
}

// invertMatches returns lines that do NOT match the regex for each file.
func (t *GrepTool) invertMatches(ctx context.Context, files []string, re *regexp.Regexp) []fileMatch {
	var results []fileMatch

	for _, file := range files {
		select {
		case <-ctx.Done():
			return results
		default:
		}

		f, err := os.Open(file)
		if err != nil {
			continue
		}

		var matches []grepMatch
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if !re.MatchString(line) {
				if len(line) > 500 {
					line = line[:500] + "..."
				}
				matches = append(matches, grepMatch{lineNum: lineNum, line: line})
			}
		}
		f.Close()

		if len(matches) > 0 {
			results = append(results, fileMatch{path: file, matches: matches})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].path < results[j].path
	})

	return results
}

// searchParallel searches files concurrently using a worker pool.
func (t *GrepTool) searchParallel(ctx context.Context, files []string, re *regexp.Regexp, contextLines int) []fileMatch {
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]fileMatch, 0)

	// Limit concurrency to 10 workers
	semaphore := make(chan struct{}, 10)

searchLoop:
	for _, file := range files {
		select {
		case <-ctx.Done():
			break searchLoop
		default:
		}

		wg.Add(1)
		semaphore <- struct{}{}

		go func(f string) {
			defer wg.Done()
			defer func() { <-semaphore }()

			matches := t.searchFile(f, re, contextLines)
			if len(matches) > 0 {
				mu.Lock()
				results = append(results, fileMatch{path: f, matches: matches})
				mu.Unlock()
			}
		}(file)
	}

	wg.Wait()

	// Sort results by file path for consistent output
	sort.Slice(results, func(i, j int) bool {
		return results[i].path < results[j].path
	})

	return results
}

type grepMatch struct {
	lineNum int
	line    string
}

// fileMatch holds all matches for a single file.
type fileMatch struct {
	path    string
	matches []grepMatch
}

func (t *GrepTool) getFiles(searchPath, globPattern string) ([]string, error) {
	info, err := os.Stat(searchPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("path not found: %s", searchPath)
		}
		return nil, fmt.Errorf("error accessing path: %w", err)
	}

	// If it's a file, return just that file
	if !info.IsDir() {
		return []string{searchPath}, nil
	}

	// Build glob pattern
	if globPattern == "" {
		globPattern = "**/*"
	}
	fullPattern := filepath.Join(searchPath, globPattern)

	// Find files
	matches, err := doublestar.FilepathGlob(fullPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern: %w", err)
	}

	// Filter to only files (not directories)
	var files []string
	for _, match := range matches {
		info, err := os.Stat(match)
		if err == nil && !info.IsDir() {
			// Skip binary files and very large files
			if info.Size() < 10*1024*1024 && !isBinaryFile(match) {
				// Filter by gitignore
				if t.gitIgnore != nil && t.gitIgnore.IsIgnored(match) {
					continue
				}
				files = append(files, match)
			}
		}
	}

	return files, nil
}

func (t *GrepTool) searchFile(filePath string, re *regexp.Regexp, contextLines int) []grepMatch {
	file, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer file.Close()

	// Read all lines for context support
	var allLines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	var matches []grepMatch
	matchedLines := make(map[int]bool) // Track which lines are already included

	for lineNum, line := range allLines {
		if re.MatchString(line) {
			// Add context lines before
			start := lineNum - contextLines
			if start < 0 {
				start = 0
			}

			// Add context lines after
			end := lineNum + contextLines
			if end >= len(allLines) {
				end = len(allLines) - 1
			}

			// Add all lines in range (including match itself)
			for i := start; i <= end; i++ {
				if matchedLines[i] {
					continue // Skip already added lines
				}
				matchedLines[i] = true

				contextLine := allLines[i]
				// Truncate long lines
				if len(contextLine) > 500 {
					contextLine = contextLine[:500] + "..."
				}
				matches = append(matches, grepMatch{
					lineNum: i + 1, // 1-indexed
					line:    contextLine,
				})
			}
		}
	}

	return matches
}

// isBinaryFile checks if a file is likely binary based on extension.
func isBinaryFile(path string) bool {
	binaryExts := map[string]bool{
		".exe": true, ".dll": true, ".so": true, ".dylib": true,
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
		".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".rar": true,
		".mp3": true, ".mp4": true, ".avi": true, ".mov": true,
		".bin": true, ".dat": true, ".db": true, ".sqlite": true,
		".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	}

	ext := strings.ToLower(filepath.Ext(path))
	return binaryExts[ext]
}

// SupportsStreaming returns true as grep supports streaming output.
func (t *GrepTool) SupportsStreaming() bool {
	return true
}

// ExecuteStreaming runs grep with streaming output for large result sets.
func (t *GrepTool) ExecuteStreaming(ctx context.Context, args map[string]any) (*StreamingToolResult, error) {
	pattern, _ := GetString(args, "pattern")
	searchPath := GetStringDefault(args, "path", t.workDir)
	globPattern := GetStringDefault(args, "glob", "")
	caseInsensitive := GetBoolDefault(args, "case_insensitive", false)
	contextLines := GetIntDefault(args, "context_lines", 0)

	// Make path absolute
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(t.workDir, searchPath)
	}

	// Validate path
	if t.pathValidator != nil {
		validPath, err := t.pathValidator.Validate(searchPath)
		if err != nil {
			return nil, fmt.Errorf("path validation failed: %w", err)
		}
		searchPath = validPath
	}

	// Compile regex
	regexPattern := pattern
	if caseInsensitive {
		regexPattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}

	// Get files to search
	files, err := t.getFiles(searchPath, globPattern)
	if err != nil {
		return nil, err
	}

	// Create streaming result
	result, chunks, errChan, complete := NewStreamingToolResult(100)

	go func() {
		defer complete()

		const maxMatches = 500
		matchCount := 0
		fileCount := 0

		// Send header
		chunks <- fmt.Sprintf("Searching %d files for pattern: %s\n\n", len(files), pattern)

		for _, file := range files {
			select {
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			default:
			}

			if matchCount >= maxMatches {
				chunks <- fmt.Sprintf("\n... (reached %d match limit)\n", maxMatches)
				break
			}

			matches := t.searchFile(file, re, contextLines)
			if len(matches) == 0 {
				continue
			}

			fileCount++
			relPath, _ := filepath.Rel(t.workDir, file)
			if relPath == "" {
				relPath = file
			}

			for _, match := range matches {
				if matchCount >= maxMatches {
					break
				}
				chunks <- fmt.Sprintf("%s:%d: %s\n", relPath, match.lineNum, match.line)
				matchCount++
			}
		}

		// Send summary
		if matchCount == 0 {
			chunks <- "No matches found."
		} else {
			chunks <- fmt.Sprintf("\nFound %d match(es) in %d file(s).\n", matchCount, fileCount)
		}
	}()

	return result, nil
}
