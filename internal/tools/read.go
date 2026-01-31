package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gooner/internal/logging"
	"gooner/internal/security"
	"gooner/internal/tools/readers"

	"google.golang.org/genai"
)

const (
	// DefaultChunkSize is the default number of lines to read per chunk.
	DefaultChunkSize = 1000
	// LargeFileSizeMB is the threshold for considering a file "large".
	LargeFileSizeMB = 10
)

// ChunkedReader provides efficient reading of large files in chunks.
type ChunkedReader struct {
	filePath    string
	totalLines  int
	currentLine int
	chunkSize   int
	file        *os.File
	scanner     *bufio.Scanner
	initialized bool
}

// NewChunkedReader creates a new chunked reader for large files.
func NewChunkedReader(path string, chunkSize int) (*ChunkedReader, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	// First, count total lines
	totalLines, err := countLines(path)
	if err != nil {
		return nil, fmt.Errorf("failed to count lines: %w", err)
	}

	return &ChunkedReader{
		filePath:   path,
		totalLines: totalLines,
		chunkSize:  chunkSize,
	}, nil
}

// countLines counts the total number of lines in a file efficiently.
func countLines(filePath string) (int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	buf := make([]byte, 32*1024)
	count := 0
	lineSep := []byte{'\n'}

	for {
		c, err := file.Read(buf)
		count += countBytes(buf[:c], lineSep[0])

		if err == io.EOF {
			// Add 1 if file doesn't end with newline
			if c > 0 && buf[c-1] != '\n' {
				count++
			}
			break
		}
		if err != nil {
			return 0, err
		}
	}

	return count, nil
}

// countBytes counts occurrences of a byte in a buffer.
func countBytes(buf []byte, b byte) int {
	count := 0
	for _, c := range buf {
		if c == b {
			count++
		}
	}
	return count
}

// TotalLines returns the total number of lines in the file.
func (r *ChunkedReader) TotalLines() int {
	return r.totalLines
}

// SeekToLine positions the reader at the specified line number (1-indexed).
func (r *ChunkedReader) SeekToLine(lineNum int) error {
	if lineNum < 1 {
		lineNum = 1
	}

	// Open new file first to ensure we can read it
	file, err := os.Open(r.filePath)
	if err != nil {
		return err
	}

	// Close existing file if open (after successful open of new file)
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			logging.Warn("error closing file", "path", r.filePath, "error", err)
		}
	}

	r.file = file

	// Skip lines until we reach the target
	r.scanner = bufio.NewScanner(file)
	r.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for i := 1; i < lineNum && r.scanner.Scan(); i++ {
		// Just skip
	}

	r.currentLine = lineNum
	r.initialized = true
	return nil
}

// NextChunk reads the next chunk of lines.
// Returns the lines, starting line number, whether there are more lines, and any error.
func (r *ChunkedReader) NextChunk() (lines []string, startLine int, hasMore bool, err error) {
	if !r.initialized {
		if err := r.SeekToLine(1); err != nil {
			return nil, 0, false, err
		}
	}

	startLine = r.currentLine
	lines = make([]string, 0, r.chunkSize)

	for i := 0; i < r.chunkSize && r.scanner.Scan(); i++ {
		lines = append(lines, r.scanner.Text())
		r.currentLine++
	}

	if err := r.scanner.Err(); err != nil {
		return lines, startLine, false, err
	}

	hasMore = r.currentLine <= r.totalLines
	return lines, startLine, hasMore, nil
}

// Close closes the chunked reader and releases resources.
func (r *ChunkedReader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// ReadTool reads files and returns their contents with line numbers.
type ReadTool struct {
	notebookReader *readers.NotebookReader
	imageReader    *readers.ImageReader
	pdfReader      *readers.PDFReader
	workDir        string
	pathValidator  *security.PathValidator
}

// NewReadTool creates a new ReadTool instance.
func NewReadTool() *ReadTool {
	return &ReadTool{
		notebookReader: readers.NewNotebookReader(),
		imageReader:    readers.NewImageReader(),
		pdfReader:      readers.NewPDFReader(),
	}
}

// SetWorkDir sets the working directory and initializes path validator.
func (t *ReadTool) SetWorkDir(workDir string) {
	t.workDir = workDir
	t.pathValidator = security.NewPathValidator([]string{workDir}, false)
}

// SetAllowedDirs sets additional allowed directories for path validation.
func (t *ReadTool) SetAllowedDirs(dirs []string) {
	allDirs := append([]string{t.workDir}, dirs...)
	t.pathValidator = security.NewPathValidator(allDirs, false)
}

func (t *ReadTool) Name() string {
	return "read"
}

func (t *ReadTool) Description() string {
	return `Reads a file from the filesystem and returns its contents with line numbers.

PARAMETERS:
- file_path (required): Absolute path to the file to read
- offset (optional): Line number to start reading from (1-indexed, default: 1)
- limit (optional): Maximum number of lines to read (default: 2000)

SUPPORTED FORMATS:
- Text files: Returns content with line numbers (cat -n style)
- PDF files (.pdf): Extracts and returns text content
- Images (.png, .jpg, .gif, etc.): Returns image metadata and can be analyzed
- Jupyter notebooks (.ipynb): Returns all cells with outputs

LIMITATIONS:
- Lines longer than 2000 characters are truncated
- Large files (>10MB) are read in chunks; use offset to continue reading
- Maximum 2000 lines per request (use offset for more)

USAGE TIPS:
- Always read files BEFORE editing them
- Use offset/limit for large files
- Check file exists before reading (use glob to find files first)

AFTER READING - YOU MUST:
1. Explain what the file contains
2. Highlight key sections with line numbers
3. Relate findings to the user's question`
}

func (t *ReadTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The absolute path to the file to read",
				},
				"offset": {
					Type:        genai.TypeInteger,
					Description: "The line number to start reading from (1-indexed). Optional.",
				},
				"limit": {
					Type:        genai.TypeInteger,
					Description: "The maximum number of lines to read. Optional, defaults to 2000.",
				},
			},
			Required: []string{"file_path"},
		},
	}
}

func (t *ReadTool) Validate(args map[string]any) error {
	filePath, ok := GetString(args, "file_path")
	if !ok || filePath == "" {
		return NewValidationError("file_path", "is required")
	}
	return nil
}

func (t *ReadTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	filePath, _ := GetString(args, "file_path")

	// Validate path if validator is configured
	if t.pathValidator != nil {
		validPath, err := t.pathValidator.ValidateFile(filePath)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("path validation failed: %s", err)), nil
		}
		filePath = validPath
	}

	// Check if file exists
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return NewErrorResult(fmt.Sprintf("error accessing file: %s", err)), nil
	}

	if info.IsDir() {
		return NewErrorResult(fmt.Sprintf("%s is a directory, not a file", filePath)), nil
	}

	// Route based on file extension
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".pdf":
		return t.readPDF(filePath)
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg", ".ico", ".tiff", ".tif":
		return t.readImage(filePath)
	case ".ipynb":
		return t.readNotebook(filePath)
	default:
		// Check if file is large
		isLarge := info.Size() > LargeFileSizeMB*1024*1024
		if isLarge {
			return t.readLargeFile(ctx, filePath, args)
		}
		return t.readText(ctx, filePath, args)
	}
}

// readLargeFile reads a large file using chunked streaming.
func (t *ReadTool) readLargeFile(ctx context.Context, filePath string, args map[string]any) (ToolResult, error) {
	offset := GetIntDefault(args, "offset", 1)
	limit := GetIntDefault(args, "limit", DefaultChunkSize)

	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = DefaultChunkSize
	}

	// Create chunked reader
	reader, err := NewChunkedReader(filePath, limit)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error creating chunked reader: %s", err)), nil
	}
	defer reader.Close()

	// Seek to offset
	if err := reader.SeekToLine(offset); err != nil {
		return NewErrorResult(fmt.Sprintf("error seeking to line %d: %s", offset, err)), nil
	}

	// Read chunk
	lines, startLine, hasMore, err := reader.NextChunk()
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error reading chunk: %s", err)), nil
	}

	// Format output
	var builder strings.Builder
	maxLineLen := 2000

	for i, line := range lines {
		lineNum := startLine + i
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "..."
		}
		builder.WriteString(fmt.Sprintf("%6d\t%s\n", lineNum, line))
	}

	content := builder.String()
	if content == "" {
		content = "(empty file or reached end of file)"
	}

	// Add metadata about the large file
	metadata := map[string]any{
		"type":        "large_file",
		"total_lines": reader.TotalLines(),
		"start_line":  startLine,
		"lines_read":  len(lines),
		"has_more":    hasMore,
		"file_path":   filePath,
	}

	// Add hint if there's more content
	if hasMore {
		hint := fmt.Sprintf("\n[Large file: showing lines %d-%d of %d total. Use offset=%d to continue reading.]\n",
			startLine, startLine+len(lines)-1, reader.TotalLines(), startLine+len(lines))
		content = hint + content
	} else {
		hint := fmt.Sprintf("\n[Large file: showing lines %d-%d of %d total (end of file).]\n",
			startLine, startLine+len(lines)-1, reader.TotalLines())
		content = hint + content
	}

	return NewSuccessResultWithData(content, metadata), nil
}

// readPDF reads a PDF file and extracts text.
func (t *ReadTool) readPDF(filePath string) (ToolResult, error) {
	content, err := t.pdfReader.Read(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error reading PDF: %s", err)), nil
	}

	return NewSuccessResultWithData(content, map[string]any{
		"type":      "pdf",
		"file_path": filePath,
	}), nil
}

// readImage reads an image file and returns base64-encoded data.
func (t *ReadTool) readImage(filePath string) (ToolResult, error) {
	result, err := t.imageReader.Read(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error reading image: %s", err)), nil
	}

	// For display purposes, we return metadata
	// The actual image data is included in the structured data
	displayText := fmt.Sprintf("[Image: %s, %d bytes]\nMIME Type: %s\nFile: %s",
		result.MimeType, result.Size, result.MimeType, filePath)

	return NewSuccessResultWithData(displayText, map[string]any{
		"type":      "image",
		"mime_type": result.MimeType,
		"size":      result.Size,
		"file_path": filePath,
		"data":      result.Data, // Raw bytes for multimodal processing
	}), nil
}

// readNotebook reads a Jupyter notebook file.
func (t *ReadTool) readNotebook(filePath string) (ToolResult, error) {
	content, err := t.notebookReader.Read(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error reading notebook: %s", err)), nil
	}

	return NewSuccessResultWithData(content, map[string]any{
		"type":      "notebook",
		"file_path": filePath,
	}), nil
}

// readText reads a regular text file with line numbers.
func (t *ReadTool) readText(ctx context.Context, filePath string, args map[string]any) (ToolResult, error) {
	offset := GetIntDefault(args, "offset", 1)
	limit := GetIntDefault(args, "limit", 2000)

	// Ensure offset is at least 1
	if offset < 1 {
		offset = 1
	}

	// Open file
	file, err := os.Open(filePath)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("error opening file: %s", err)), nil
	}
	defer file.Close()

	// Read lines with line numbers
	var builder strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for long lines

	lineNum := 0
	linesRead := 0
	maxLineLen := 2000

	for scanner.Scan() {
		lineNum++

		// Skip lines before offset
		if lineNum < offset {
			continue
		}

		// Stop if we've read enough lines
		if linesRead >= limit {
			break
		}

		line := scanner.Text()

		// Truncate long lines
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "..."
		}

		// Format with line number (cat -n style)
		builder.WriteString(fmt.Sprintf("%6d\t%s\n", lineNum, line))
		linesRead++
	}

	if err := scanner.Err(); err != nil {
		return NewErrorResult(fmt.Sprintf("error reading file: %s", err)), nil
	}

	content := builder.String()
	if content == "" {
		if offset > 1 && lineNum > 0 {
			content = fmt.Sprintf("(offset %d is beyond end of file — file has %d lines)", offset, lineNum)
		} else {
			content = "(empty file)"
		}
	}

	return NewSuccessResult(content), nil
}
