package readers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// NotebookCell represents a cell in a Jupyter notebook.
type NotebookCell struct {
	CellType       string   `json:"cell_type"`
	Source         any      `json:"source"` // Can be string or []string
	Outputs        []Output `json:"outputs,omitempty"`
	ExecutionCount *int     `json:"execution_count,omitempty"`
}

// Output represents the output of a notebook cell.
type Output struct {
	OutputType string   `json:"output_type"`
	Text       any      `json:"text,omitempty"` // Can be string or []string
	Data       DataMIME `json:"data,omitempty"`
	Name       string   `json:"name,omitempty"`
	EName      string   `json:"ename,omitempty"`
	EValue     string   `json:"evalue,omitempty"`
}

// DataMIME represents MIME-typed data in notebook outputs.
type DataMIME struct {
	TextPlain any `json:"text/plain,omitempty"`
	TextHTML  any `json:"text/html,omitempty"`
	ImagePNG  any `json:"image/png,omitempty"`
}

// NotebookFile represents a Jupyter notebook file.
type NotebookFile struct {
	Cells    []NotebookCell `json:"cells"`
	Metadata map[string]any `json:"metadata"`
	NBFormat int            `json:"nbformat"`
}

// NotebookReader reads Jupyter notebook files.
type NotebookReader struct{}

// NewNotebookReader creates a new NotebookReader.
func NewNotebookReader() *NotebookReader {
	return &NotebookReader{}
}

// Read reads a Jupyter notebook file and returns formatted content.
func (r *NotebookReader) Read(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read notebook: %w", err)
	}

	var nb NotebookFile
	if err := json.Unmarshal(data, &nb); err != nil {
		return "", fmt.Errorf("failed to parse notebook JSON: %w", err)
	}

	return r.formatNotebook(&nb), nil
}

// formatNotebook formats a notebook into readable text.
func (r *NotebookReader) formatNotebook(nb *NotebookFile) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# Jupyter Notebook (nbformat %d)\n\n", nb.NBFormat))

	for i, cell := range nb.Cells {
		cellNum := i + 1
		source := r.extractSource(cell.Source)

		switch cell.CellType {
		case "code":
			sb.WriteString(fmt.Sprintf("## Cell %d [code]", cellNum))
			if cell.ExecutionCount != nil {
				sb.WriteString(fmt.Sprintf(" In[%d]", *cell.ExecutionCount))
			}
			sb.WriteString("\n```python\n")
			sb.WriteString(source)
			if !strings.HasSuffix(source, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n")

			// Format outputs
			if len(cell.Outputs) > 0 {
				sb.WriteString("\n**Output:**\n")
				for _, output := range cell.Outputs {
					sb.WriteString(r.formatOutput(output))
				}
			}

		case "markdown":
			sb.WriteString(fmt.Sprintf("## Cell %d [markdown]\n", cellNum))
			sb.WriteString(source)
			if !strings.HasSuffix(source, "\n") {
				sb.WriteString("\n")
			}

		case "raw":
			sb.WriteString(fmt.Sprintf("## Cell %d [raw]\n", cellNum))
			sb.WriteString("```\n")
			sb.WriteString(source)
			if !strings.HasSuffix(source, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n")

		default:
			sb.WriteString(fmt.Sprintf("## Cell %d [%s]\n", cellNum, cell.CellType))
			sb.WriteString(source)
		}

		sb.WriteString("\n")
	}

	return sb.String()
}

// extractSource extracts source as a string from various formats.
func (r *NotebookReader) extractSource(source any) string {
	switch v := source.(type) {
	case string:
		return v
	case []any:
		var lines []string
		for _, line := range v {
			if s, ok := line.(string); ok {
				lines = append(lines, s)
			}
		}
		return strings.Join(lines, "")
	case []string:
		return strings.Join(v, "")
	default:
		return fmt.Sprintf("%v", source)
	}
}

// formatOutput formats a cell output.
func (r *NotebookReader) formatOutput(output Output) string {
	var sb strings.Builder

	switch output.OutputType {
	case "stream":
		text := r.extractSource(output.Text)
		if output.Name == "stderr" {
			sb.WriteString("```stderr\n")
		} else {
			sb.WriteString("```\n")
		}
		sb.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")

	case "execute_result", "display_data":
		if output.Data.TextPlain != nil {
			text := r.extractSource(output.Data.TextPlain)
			sb.WriteString("```\n")
			sb.WriteString(text)
			if !strings.HasSuffix(text, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n")
		}
		if output.Data.ImagePNG != nil {
			sb.WriteString("[Image output: PNG]\n")
		}

	case "error":
		sb.WriteString(fmt.Sprintf("**Error:** %s: %s\n", output.EName, output.EValue))
	}

	return sb.String()
}
