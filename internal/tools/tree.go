package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/genai"

	"gokin/internal/security"
)

// TreeTool displays directory structure as a tree.
type TreeTool struct {
	workDir       string
	pathValidator *security.PathValidator
}

// NewTreeTool creates a new TreeTool instance.
func NewTreeTool(workDir string) *TreeTool {
	return &TreeTool{
		workDir:       workDir,
		pathValidator: security.NewPathValidator([]string{workDir}, false),
	}
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *TreeTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *TreeTool) Name() string {
	return "tree"
}

func (t *TreeTool) Description() string {
	return "Displays the directory structure as a tree. Useful for understanding project layout."
}

func (t *TreeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The directory path to display (default: current directory)",
				},
				"depth": {
					Type:        genai.TypeInteger,
					Description: "Maximum depth to traverse (default: 3)",
				},
				"pattern": {
					Type:        genai.TypeString,
					Description: "Glob pattern to filter files (e.g., '*.go')",
				},
				"show_hidden": {
					Type:        genai.TypeBoolean,
					Description: "Show hidden files and directories (default: false)",
				},
				"dirs_only": {
					Type:        genai.TypeBoolean,
					Description: "Show only directories (default: false)",
				},
				"ascii": {
					Type:        genai.TypeBoolean,
					Description: "Use ASCII characters instead of Unicode for better copy-paste compatibility (default: false)",
				},
			},
		},
	}
}

func (t *TreeTool) Validate(args map[string]any) error {
	depth, hasDepth := GetInt(args, "depth")
	if hasDepth && depth < 1 {
		return NewValidationError("depth", "must be at least 1")
	}
	return nil
}

// Tree drawing characters
type treeChars struct {
	Branch    string // ├── or |--
	LastBranch string // └── or `--
	Vertical  string // │   or |
	Space     string // spaces
}

var unicodeChars = treeChars{
	Branch:     "├── ",
	LastBranch: "└── ",
	Vertical:   "│   ",
	Space:      "    ",
}

var asciiChars = treeChars{
	Branch:     "|-- ",
	LastBranch: "`-- ",
	Vertical:   "|   ",
	Space:      "    ",
}

func (t *TreeTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	path := GetStringDefault(args, "path", t.workDir)
	depth := GetIntDefault(args, "depth", 3)
	pattern := GetStringDefault(args, "pattern", "")
	showHidden := GetBoolDefault(args, "show_hidden", false)
	dirsOnly := GetBoolDefault(args, "dirs_only", false)
	useASCII := GetBoolDefault(args, "ascii", false)

	// Resolve path
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.workDir, path)
	}

	// Validate path if validator is configured
	if t.pathValidator != nil {
		validPath, err := t.pathValidator.ValidateDir(path)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
		}
		path = validPath
	}

	// Verify directory exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("directory not found: %s", path)), nil
		}
		return NewErrorResult(fmt.Sprintf("error accessing directory: %s", err)), nil
	}

	if !info.IsDir() {
		return NewErrorResult(fmt.Sprintf("%s is not a directory", path)), nil
	}

	// Select character set
	chars := unicodeChars
	if useASCII {
		chars = asciiChars
	}

	// Build tree
	var builder strings.Builder
	builder.WriteString(path + "\n")

	stats := &treeStats{}
	err = t.buildTree(ctx, &builder, path, "", depth, pattern, showHidden, dirsOnly, stats, chars)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error building tree: %s", err)), nil
	}

	builder.WriteString(fmt.Sprintf("\n%d directories, %d files", stats.dirs, stats.files))

	return NewSuccessResult(builder.String()), nil
}

type treeStats struct {
	dirs  int
	files int
}

func (t *TreeTool) buildTree(ctx context.Context, builder *strings.Builder, path, prefix string, depth int, pattern string, showHidden, dirsOnly bool, stats *treeStats, chars treeChars) error {
	if depth <= 0 {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}

	// Filter and sort entries
	var filtered []os.DirEntry
	for _, entry := range entries {
		name := entry.Name()

		// Skip hidden files unless requested
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}

		// Skip files if dirs_only
		if dirsOnly && !entry.IsDir() {
			continue
		}

		// Apply pattern filter
		if pattern != "" && !entry.IsDir() {
			matched, _ := filepath.Match(pattern, name)
			if !matched {
				continue
			}
		}

		filtered = append(filtered, entry)
	}

	// Sort: directories first, then alphabetically
	sort.Slice(filtered, func(i, j int) bool {
		di := filtered[i].IsDir()
		dj := filtered[j].IsDir()
		if di != dj {
			return di
		}
		return filtered[i].Name() < filtered[j].Name()
	})

	for i, entry := range filtered {
		isLast := i == len(filtered)-1
		connector := chars.Branch
		childPrefix := prefix + chars.Vertical

		if isLast {
			connector = chars.LastBranch
			childPrefix = prefix + chars.Space
		}

		name := entry.Name()
		if entry.IsDir() {
			name += "/"
			stats.dirs++
		} else {
			stats.files++
		}

		builder.WriteString(prefix + connector + name + "\n")

		// Recurse into directories
		if entry.IsDir() {
			childPath := filepath.Join(path, entry.Name())
			err := t.buildTree(ctx, builder, childPath, childPrefix, depth-1, pattern, showHidden, dirsOnly, stats, chars)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
