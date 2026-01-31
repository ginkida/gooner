package semantic

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// CodeGraph represents the dependency graph of a codebase.
type CodeGraph struct {
	nodes map[string]*GraphNode
	edges map[string][]*GraphEdge
	mu    sync.RWMutex
}

// GraphNode represents a file or function in the code graph.
type GraphNode struct {
	ID       string
	Type     string // "file", "function", "class", "variable"
	Path     string
	Name     string
	Language string
	Metadata map[string]string
}

// GraphEdge represents a relationship between nodes.
type GraphEdge struct {
	From     string
	To       string
	Type     string // "imports", "calls", "defines", "uses"
	Weight   int
	Metadata map[string]string
}

// NewCodeGraph creates a new code graph.
func NewCodeGraph() *CodeGraph {
	return &CodeGraph{
		nodes: make(map[string]*GraphNode),
		edges: make(map[string][]*GraphEdge),
	}
}

// AddNode adds a node to the graph.
func (g *CodeGraph) AddNode(node *GraphNode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[node.ID] = node
}

// AddEdge adds an edge to the graph.
func (g *CodeGraph) AddEdge(edge *GraphEdge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.edges[edge.From] = append(g.edges[edge.From], edge)
}

// GetDependencies returns all dependencies of a node.
func (g *CodeGraph) GetDependencies(nodeID string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var deps []string
	if edges, ok := g.edges[nodeID]; ok {
		for _, edge := range edges {
			deps = append(deps, edge.To)
		}
	}
	return deps
}

// GetDependents returns all nodes that depend on this node.
func (g *CodeGraph) GetDependents(nodeID string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var dependents []string
	for from, edges := range g.edges {
		for _, edge := range edges {
			if edge.To == nodeID {
				dependents = append(dependents, from)
			}
		}
	}
	return dependents
}

// GetAllNodes returns all nodes in the graph.
func (g *CodeGraph) GetAllNodes() []*GraphNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := make([]*GraphNode, 0, len(g.nodes))
	for _, node := range g.nodes {
		nodes = append(nodes, node)
	}
	return nodes
}

// FindCircularDeps detects circular dependencies.
func (g *CodeGraph) FindCircularDeps() [][]string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	var cycles [][]string

	var visit func(string, []string)
	visit = func(nodeID string, path []string) {
		visited[nodeID] = true
		recStack[nodeID] = true
		path = append(path, nodeID)

		for _, edge := range g.edges[nodeID] {
			if !visited[edge.To] {
				visit(edge.To, path)
			} else if recStack[edge.To] {
				// Found a cycle
				cycleStart := -1
				for i, p := range path {
					if p == edge.To {
						cycleStart = i
						break
					}
				}
				if cycleStart >= 0 {
					cycle := append([]string{}, path[cycleStart:]...)
					cycles = append(cycles, cycle)
				}
			}
		}

		recStack[nodeID] = false
		path = path[:len(path)-1]
	}

	for nodeID := range g.nodes {
		if !visited[nodeID] {
			visit(nodeID, []string{})
		}
	}

	return cycles
}

// PatternMatcher searches for code patterns.
type PatternMatcher struct {
	workDir string
}

// NewPatternMatcher creates a new pattern matcher.
func NewPatternMatcher(workDir string) *PatternMatcher {
	return &PatternMatcher{workDir: workDir}
}

// PatternResult represents a pattern match result.
type PatternResult struct {
	FilePath    string
	Line        int
	Match       string
	Context     string
	PatternName string
	Confidence  float64
}

// FindSingletons finds all singleton pattern implementations.
func (p *PatternMatcher) FindSingletons(ctx context.Context) ([]PatternResult, error) {
	patterns := []struct {
		name    string
		pattern string
		level   string
	}{
		{"Go sync.Once", `sync\.Once`, "info"},
		{"Go private instance", `var \w+\s+\*?\w+\s*=\s*&\w+\{}`, "info"},
		{"Python singleton decorator", `@singleton`, "info"},
		{"Java singleton", `private static.*\w+\s+instance`, "info"},
		{"JS module.exports", `module\.exports\s*=\s*{\s*getInstance`, "info"},
	}

	var results []PatternResult
	for _, lang := range []string{".go", ".py", ".java", ".js"} {
		matches, err := p.findPatternByExt(ctx, lang, patterns)
		if err != nil {
			continue
		}
		results = append(results, matches...)
	}

	return results, nil
}

// FindAntiPatterns detects common anti-patterns.
func (p *PatternMatcher) FindAntiPatterns(ctx context.Context) ([]PatternResult, error) {
	antiPatterns := []struct {
		name    string
		pattern string
		level   string // "warning", "error", "info"
	}{
		{"God function", `^func \w+\([^)]*\) \{[\s\S]{200,}`, "warning"},
		{"Deep nesting", `[\t ]{20,}`, "warning"},
		{"Magic numbers", `\b\d{3,}\b`, "info"},
		{"TODO/FIXME", `(?i)(TODO|FIXME|HACK|XXX)`, "info"},
		{"Empty catch", `catch\s*\([^)]*\)\s*\{\s*\}`, "error"},
		{"Swallowed error", `err\s*!=\s*nil\s*\{\s*return\s*\}`, "warning"},
	}

	var results []PatternResult
	err := filepath.Walk(p.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		// Check context
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_ = strings.ToLower(filepath.Ext(path)) // Check extension but don't use
		if !isCodeFile(path) {
			return nil
		}

		matches, err := p.findPatternsInFile(path, antiPatterns)
		if err != nil {
			return nil // Skip files with errors
		}
		results = append(results, matches...)

		return nil
	})

	return results, err
}

// FindPattern finds a custom pattern in the codebase.
func (p *PatternMatcher) FindPattern(ctx context.Context, pattern, name string) ([]PatternResult, error) {
	specs := []struct {
		name    string
		pattern string
		level   string
	}{
		{name, pattern, "info"},
	}

	var results []PatternResult
	err := filepath.Walk(p.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !isCodeFile(path) {
			return nil
		}

		matches, err := p.findPatternsInFile(path, specs)
		if err != nil {
			return nil
		}
		results = append(results, matches...)

		return nil
	})

	return results, err
}

// findPatternsInFile searches for patterns in a single file.
func (p *PatternMatcher) findPatternsInFile(filePath string, patterns []struct {
	name    string
	pattern string
	level   string
}) ([]PatternResult, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var results []PatternResult
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		for _, pat := range patterns {
			re, err := regexp.Compile(pat.pattern)
			if err != nil {
				continue // Skip invalid patterns
			}

			if re.MatchString(line) {
				results = append(results, PatternResult{
					FilePath:    filePath,
					Line:        lineNum,
					Match:       strings.TrimSpace(line),
					Context:     p.getContext(file, lineNum, 2),
					PatternName: pat.name,
					Confidence:  0.8, // Base confidence
				})
			}
		}
	}

	return results, scanner.Err()
}

// getContext returns surrounding lines for context.
func (p *PatternMatcher) getContext(file *os.File, lineNum, contextLines int) string {
	// This is a simplified version - in production, you'd want to cache file content
	return fmt.Sprintf("(context around line %d)", lineNum)
}

// findPatternByExt finds patterns in files with a specific extension.
func (p *PatternMatcher) findPatternByExt(ctx context.Context, ext string, patterns []struct {
	name    string
	pattern string
	level   string
}) ([]PatternResult, error) {
	var results []PatternResult

	err := filepath.Walk(p.workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if filepath.Ext(path) != ext {
			return nil
		}

		matches, err := p.findPatternsInFile(path, patterns)
		if err != nil {
			return nil
		}
		results = append(results, matches...)

		return nil
	})

	return results, err
}

// BuildDependencyGraph builds a code dependency graph.
func BuildDependencyGraph(workDir string) (*CodeGraph, error) {
	graph := NewCodeGraph()

	err := filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		if !isCodeFile(path) {
			return nil
		}

		// Add file node
		relPath, _ := filepath.Rel(workDir, path)
		nodeID := fmt.Sprintf("file:%s", relPath)
		lang := DetectLanguage(path)

		graph.AddNode(&GraphNode{
			ID:       nodeID,
			Type:     "file",
			Path:     path,
			Name:     filepath.Base(path),
			Language: lang,
		})

		// Extract imports/dependencies
		deps := extractDependencies(path, lang)
		for _, dep := range deps {
			depNodeID := fmt.Sprintf("dep:%s", dep)
			graph.AddNode(&GraphNode{
				ID:       depNodeID,
				Type:     "dependency",
				Name:     dep,
				Language: lang,
			})
			graph.AddEdge(&GraphEdge{
				From: nodeID,
				To:   depNodeID,
				Type: "imports",
			})
		}

		return nil
	})

	return graph, err
}

// extractDependencies extracts import/require dependencies from a file.
func extractDependencies(filePath, lang string) []string {
	file, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer file.Close()

	var deps []string
	scanner := bufio.NewScanner(file)

	var importPattern *regexp.Regexp
	switch lang {
	case "go":
		importPattern = regexp.MustCompile(`import\s+(?:"(.+?)"|'(.+?)')`)
	case "python":
		importPattern = regexp.MustCompile(`(?:import|from)\s+(\w+)`)
	case "javascript", "typescript":
		importPattern = regexp.MustCompile(`import.*from\s+['"](.+?)['"]`)
	case "java":
		importPattern = regexp.MustCompile(`import\s+([\w.]+);`)
	}

	if importPattern == nil {
		return nil
	}

	for scanner.Scan() {
		matches := importPattern.FindStringSubmatch(scanner.Text())
		if len(matches) > 1 {
			dep := matches[1]
			if dep != "" {
				deps = append(deps, dep)
			}
		}
	}

	return deps
}

// DetectLanguage detects the programming language from file extension.
func DetectLanguage(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc":
		return "cpp"
	case ".rs":
		return "rust"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	default:
		return "unknown"
	}
}
