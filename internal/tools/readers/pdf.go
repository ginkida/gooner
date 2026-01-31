package readers

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// PDFReader reads PDF files and extracts text content.
type PDFReader struct{}

// NewPDFReader creates a new PDFReader.
func NewPDFReader() *PDFReader {
	return &PDFReader{}
}

// Read reads a PDF file and extracts text content.
// This is a basic PDF text extractor that handles simple PDFs.
// For complex PDFs with embedded fonts or scanned content, results may vary.
func (r *PDFReader) Read(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read PDF: %w", err)
	}

	return r.extractText(data)
}

// extractText extracts text from PDF data.
func (r *PDFReader) extractText(data []byte) (string, error) {
	// Check PDF header
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return "", fmt.Errorf("not a valid PDF file")
	}

	var sb strings.Builder
	sb.WriteString("# PDF Document\n\n")

	// Find all stream objects and extract text
	text := r.extractStreams(data)
	if text != "" {
		sb.WriteString(text)
	} else {
		// Fallback: try to extract any readable text
		text = r.extractPlainText(data)
		if text != "" {
			sb.WriteString(text)
		} else {
			sb.WriteString("[No extractable text content found in PDF]\n")
			sb.WriteString("[The PDF may contain scanned images or use unsupported encoding]\n")
		}
	}

	return sb.String(), nil
}

// extractStreams extracts text from PDF stream objects.
func (r *PDFReader) extractStreams(data []byte) string {
	var result strings.Builder

	// Find stream...endstream blocks
	streamStart := []byte("stream")
	streamEnd := []byte("endstream")

	pos := 0
	pageNum := 1

	for {
		start := bytes.Index(data[pos:], streamStart)
		if start == -1 {
			break
		}
		start += pos + len(streamStart)

		// Skip newline after "stream"
		if start < len(data) && (data[start] == '\r' || data[start] == '\n') {
			start++
		}
		if start < len(data) && data[start] == '\n' {
			start++
		}

		end := bytes.Index(data[start:], streamEnd)
		if end == -1 {
			break
		}
		end += start

		if end > start {
			streamData := data[start:end]

			// Try to decompress and extract text
			text := r.extractTextFromStream(streamData)
			if text != "" {
				if result.Len() > 0 {
					result.WriteString("\n")
				}
				result.WriteString(fmt.Sprintf("## Page %d\n\n", pageNum))
				result.WriteString(text)
				result.WriteString("\n")
				pageNum++
			}
		}

		pos = end + len(streamEnd)
	}

	return result.String()
}

// extractTextFromStream extracts text operators from a PDF stream.
func (r *PDFReader) extractTextFromStream(data []byte) string {
	// Try to find text operators (Tj, TJ, ', ")
	var result strings.Builder

	// Look for text between parentheses (simple strings)
	// and between < > (hex strings)
	text := r.extractTextOperators(string(data))
	if text != "" {
		result.WriteString(text)
	}

	return result.String()
}

// extractTextOperators extracts text from PDF text operators.
func (r *PDFReader) extractTextOperators(content string) string {
	var result strings.Builder

	// Pattern for text in parentheses: (text) Tj or (text) '
	parenPattern := regexp.MustCompile(`\(([^)]*)\)\s*(?:Tj|TJ|'|")`)
	matches := parenPattern.FindAllStringSubmatch(content, -1)
	for _, match := range matches {
		if len(match) > 1 {
			text := r.decodeString(match[1])
			if text != "" {
				result.WriteString(text)
				result.WriteString(" ")
			}
		}
	}

	// Pattern for text arrays: [(text) -10 (more)] TJ
	arrayPattern := regexp.MustCompile(`\[([^\]]+)\]\s*TJ`)
	arrayMatches := arrayPattern.FindAllStringSubmatch(content, -1)
	for _, match := range arrayMatches {
		if len(match) > 1 {
			text := r.extractFromArray(match[1])
			if text != "" {
				result.WriteString(text)
				result.WriteString(" ")
			}
		}
	}

	// Pattern for hex strings: <hex> Tj
	hexPattern := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*(?:Tj|TJ|'|")`)
	hexMatches := hexPattern.FindAllStringSubmatch(content, -1)
	for _, match := range hexMatches {
		if len(match) > 1 {
			text := r.decodeHexString(match[1])
			if text != "" {
				result.WriteString(text)
				result.WriteString(" ")
			}
		}
	}

	return strings.TrimSpace(result.String())
}

// extractFromArray extracts text from a TJ array.
func (r *PDFReader) extractFromArray(content string) string {
	var result strings.Builder

	// Find all parenthesized strings in the array
	parenPattern := regexp.MustCompile(`\(([^)]*)\)`)
	matches := parenPattern.FindAllStringSubmatch(content, -1)
	for _, match := range matches {
		if len(match) > 1 {
			text := r.decodeString(match[1])
			result.WriteString(text)
		}
	}

	// Find all hex strings in the array
	hexPattern := regexp.MustCompile(`<([0-9A-Fa-f]+)>`)
	hexMatches := hexPattern.FindAllStringSubmatch(content, -1)
	for _, match := range hexMatches {
		if len(match) > 1 {
			text := r.decodeHexString(match[1])
			result.WriteString(text)
		}
	}

	return result.String()
}

// decodeString decodes a PDF string with escape sequences.
func (r *PDFReader) decodeString(s string) string {
	var result strings.Builder
	reader := strings.NewReader(s)

	for {
		ch, _, err := reader.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		if ch == '\\' {
			next, _, err := reader.ReadRune()
			if err != nil {
				result.WriteRune(ch)
				break
			}
			switch next {
			case 'n':
				result.WriteRune('\n')
			case 'r':
				result.WriteRune('\r')
			case 't':
				result.WriteRune('\t')
			case 'b':
				result.WriteRune('\b')
			case 'f':
				result.WriteRune('\f')
			case '(':
				result.WriteRune('(')
			case ')':
				result.WriteRune(')')
			case '\\':
				result.WriteRune('\\')
			default:
				// Octal escape
				if next >= '0' && next <= '7' {
					octal := string(next)
					for i := 0; i < 2; i++ {
						n, _, err := reader.ReadRune()
						if err != nil || n < '0' || n > '7' {
							if err == nil {
								reader.UnreadRune()
							}
							break
						}
						octal += string(n)
					}
					if val, err := strconv.ParseInt(octal, 8, 32); err == nil {
						result.WriteRune(rune(val))
					}
				} else {
					result.WriteRune(next)
				}
			}
		} else {
			result.WriteRune(ch)
		}
	}

	return result.String()
}

// decodeHexString decodes a PDF hex string.
func (r *PDFReader) decodeHexString(s string) string {
	var result strings.Builder

	// Remove whitespace
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")

	// Pad to even length
	if len(s)%2 != 0 {
		s += "0"
	}

	for i := 0; i < len(s); i += 2 {
		val, err := strconv.ParseInt(s[i:i+2], 16, 32)
		if err == nil && val >= 32 && val < 127 {
			result.WriteRune(rune(val))
		}
	}

	return result.String()
}

// extractPlainText extracts any readable ASCII text from the PDF.
func (r *PDFReader) extractPlainText(data []byte) string {
	var result strings.Builder
	var current strings.Builder
	minWordLen := 4

	for _, b := range data {
		// Printable ASCII range
		if b >= 32 && b < 127 {
			current.WriteByte(b)
		} else {
			word := current.String()
			if len(word) >= minWordLen && r.isReadableWord(word) {
				if result.Len() > 0 {
					result.WriteString(" ")
				}
				result.WriteString(word)
			}
			current.Reset()
		}
	}

	// Don't forget the last word
	word := current.String()
	if len(word) >= minWordLen && r.isReadableWord(word) {
		if result.Len() > 0 {
			result.WriteString(" ")
		}
		result.WriteString(word)
	}

	return result.String()
}

// isReadableWord checks if a string looks like readable text.
func (r *PDFReader) isReadableWord(s string) bool {
	// Filter out PDF keywords and hex-like strings
	if strings.HasPrefix(s, "obj") || strings.HasPrefix(s, "endobj") ||
		strings.HasPrefix(s, "stream") || strings.HasPrefix(s, "endstream") {
		return false
	}

	// Check for reasonable letter ratio
	letters := 0
	for _, ch := range s {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
			letters++
		}
	}

	return float64(letters)/float64(len(s)) > 0.5
}
