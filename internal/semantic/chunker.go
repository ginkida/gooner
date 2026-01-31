package semantic

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
)

// Chunker handles splitting file content into logical chunks.
type Chunker interface {
	Chunk(filePath, content string) []ChunkInfo
}

// StructuralChunker splits code based on its structure (functions, methods, types).
type StructuralChunker struct {
	baseChunkSize int
	overlap       int
}

// NewStructuralChunker creates a new structural chunker.
func NewStructuralChunker(baseChunkSize, overlap int) *StructuralChunker {
	return &StructuralChunker{
		baseChunkSize: baseChunkSize,
		overlap:       overlap,
	}
}

// Chunk splits the content into chunks based on language-specific structure.
func (c *StructuralChunker) Chunk(filePath, content string) []ChunkInfo {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".go":
		return c.chunkGo(filePath, content)
	case ".py":
		return c.chunkPython(filePath, content)
	case ".js", ".jsx", ".ts", ".tsx":
		return c.chunkJS(filePath, content)
	case ".java":
		return c.chunkJava(filePath, content)
	default:
		// Fallback to heuristic or sliding window
		return c.chunkHeuristic(filePath, content)
	}
}

// chunkPython uses regex to split Python code into classes and functions.
func (c *StructuralChunker) chunkPython(filePath, content string) []ChunkInfo {
	// Regex for Python top-level class and function definitions
	structRegex := regexp.MustCompile(`^(class|def)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	return c.chunkWithRegex(filePath, content, structRegex, "python")
}

// chunkJS uses regex to split JS/TS code into classes, functions, and exports.
func (c *StructuralChunker) chunkJS(filePath, content string) []ChunkInfo {
	// Regex for JS/TS structures
	structRegex := regexp.MustCompile(`^(class|function|export\s+(class|function|const|var|let|async\s+<ctrl42>))\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	return c.chunkWithRegex(filePath, content, structRegex, "javascript")
}

// chunkJava uses regex to split Java code into classes and methods.
func (c *StructuralChunker) chunkJava(filePath, content string) []ChunkInfo {
	// Regex for Java top-level declarations
	structRegex := regexp.MustCompile(`^(public|private|protected|static|\s+)*(class|interface|enum|@interface)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	return c.chunkWithRegex(filePath, content, structRegex, "java")
}

// chunkWithRegex is a generic regex-based chunker for multiple languages.
func (c *StructuralChunker) chunkWithRegex(filePath, content string, structRegex *regexp.Regexp, lang string) []ChunkInfo {
	lines := strings.Split(content, "\n")
	var chunks []ChunkInfo
	var currentStart = -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Only match at the start of the line (no indentation for top-level stuff typically,
		// though Python functions can be indented if they are methods. We'll stick to non-indented for now
		// or handle indentation as a separate improvement).
		if structRegex.MatchString(line) {
			if currentStart != -1 {
				c.addChunk(filePath, lines, currentStart, i, &chunks)
			}
			currentStart = i
		}

		// Force split if too large
		if currentStart != -1 && i-currentStart >= c.baseChunkSize*2 {
			c.addChunk(filePath, lines, currentStart, i, &chunks)
			currentStart = i
		}
	}

	if currentStart != -1 {
		c.addChunk(filePath, lines, currentStart, len(lines), &chunks)
	}

	if len(chunks) == 0 {
		return c.chunkSlidingWindow(filePath, content)
	}

	return chunks
}

// chunkGo uses the Go AST to split code into functions and types.
func (c *StructuralChunker) chunkGo(filePath, content string) []ChunkInfo {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		// Fallback if parsing fails
		return c.chunkSlidingWindow(filePath, content)
	}

	var chunks []ChunkInfo
	lines := strings.Split(content, "\n")

	// Process top-level declarations
	for _, decl := range f.Decls {
		var start, end token.Pos
		var chunkType string

		switch d := decl.(type) {
		case *ast.FuncDecl:
			start, end = d.Pos(), d.End()
			chunkType = "function"
		case *ast.GenDecl:
			start, end = d.Pos(), d.End()
			chunkType = "declaration"
		default:
			continue
		}

		startPos := fset.Position(start)
		endPos := fset.Position(end)

		// Get the lines for this declaration
		if startPos.Line > 0 && endPos.Line >= startPos.Line && endPos.Line <= len(lines) {
			chunkContent := strings.Join(lines[startPos.Line-1:endPos.Line], "\n")
			if strings.TrimSpace(chunkContent) == "" {
				continue
			}

			chunks = append(chunks, ChunkInfo{
				FilePath:  filePath,
				LineStart: startPos.Line,
				LineEnd:   endPos.Line,
				Content:   fmt.Sprintf("// Type: %s\n%s", chunkType, chunkContent),
			})
		}
	}

	// If no structural chunks found, fallback
	if len(chunks) == 0 {
		return c.chunkSlidingWindow(filePath, content)
	}

	return chunks
}

// chunkHeuristic uses regexes to find potential structure in other languages.
func (c *StructuralChunker) chunkHeuristic(filePath, content string) []ChunkInfo {
	lines := strings.Split(content, "\n")
	
	// Regexes for common language structures (start of line)
	// (e.g. func, def, class, interface, type)
	structRegex := regexp.MustCompile(`^(func|def|class|interface|type|struct|enum|namespace|module|async\s+func|export\s+(class|func))\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	
	var chunks []ChunkInfo
	var currentStart = -1

	for i, line := range lines {
		if structRegex.MatchString(strings.TrimSpace(line)) {
			// If we were already in a chunk, close it
			if currentStart != -1 {
				c.addChunk(filePath, lines, currentStart, i, &chunks)
			}
			currentStart = i
		}
		
		// If chunk gets too big, force split it
		if currentStart != -1 && i-currentStart >= c.baseChunkSize*2 {
			c.addChunk(filePath, lines, currentStart, i, &chunks)
			currentStart = i
		}
	}

	// Add the last chunk
	if currentStart != -1 {
		c.addChunk(filePath, lines, currentStart, len(lines), &chunks)
	}

	// If no heuristic chunks found, fallback to sliding window
	if len(chunks) == 0 {
		return c.chunkSlidingWindow(filePath, content)
	}

	return chunks
}

func (c *StructuralChunker) addChunk(filePath string, lines []string, start, end int, chunks *[]ChunkInfo) {
	chunkContent := strings.Join(lines[start:end], "\n")
	if strings.TrimSpace(chunkContent) == "" {
		return
	}
	
	*chunks = append(*chunks, ChunkInfo{
		FilePath:  filePath,
		LineStart: start + 1,
		LineEnd:   end,
		Content:   chunkContent,
	})
}

// chunkSlidingWindow is the original fallback approach.
func (c *StructuralChunker) chunkSlidingWindow(filePath, content string) []ChunkInfo {
	lines := strings.Split(content, "\n")
	var chunks []ChunkInfo

	for start := 0; start < len(lines); start += (c.baseChunkSize - c.overlap) {
		end := start + c.baseChunkSize
		if end > len(lines) {
			end = len(lines)
		}

		chunkContent := strings.Join(lines[start:end], "\n")
		if strings.TrimSpace(chunkContent) == "" {
			continue
		}

		chunks = append(chunks, ChunkInfo{
			FilePath:  filePath,
			LineStart: start + 1,
			LineEnd:   end,
			Content:   chunkContent,
		})

		if end >= len(lines) {
			break
		}
	}

	return chunks
}
