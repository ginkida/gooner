package ui

import (
	"fmt"
	"strings"
)

// generateToolResultSummary creates compact summaries based on tool type and content.
func generateToolResultSummary(toolName, content, fallbackSummary string) string {
	switch toolName {
	case "read":
		if len(content) > 0 {
			lineCount := strings.Count(content, "\n") + 1
			return fmt.Sprintf("%d lines", lineCount)
		}
	case "glob":
		if strings.Contains(content, "(no matches)") {
			return "no matches"
		}
		if len(content) > 0 {
			fileCount := strings.Count(content, "\n")
			if fileCount == 0 && content != "" {
				fileCount = 1
			}
			return fmt.Sprintf("%d files", fileCount)
		}
		return "no matches"
	case "grep":
		if len(content) > 0 {
			matchCount := strings.Count(content, "\n")
			if matchCount == 0 && content != "" {
				matchCount = 1
			}
			return fmt.Sprintf("%d matches", matchCount)
		}
		return "no matches"
	case "bash":
		if len(content) > 0 {
			lineCount := strings.Count(content, "\n")
			if lineCount > 0 {
				return fmt.Sprintf("%d lines", lineCount)
			}
			return "1 line"
		}
		return ""
	case "edit":
		return "updated"
	case "write":
		return "written"
	case "tree", "list_dir":
		if strings.Contains(content, "(empty)") {
			return "empty"
		}
		if len(content) > 0 {
			items := strings.Count(content, "\n")
			return fmt.Sprintf("%d items", items)
		}
	case "web_fetch":
		return "fetched"
	case "web_search":
		if len(content) > 0 {
			resultCount := strings.Count(content, "http")
			if resultCount > 0 {
				return fmt.Sprintf("%d results", resultCount)
			}
		}
		return "done"
	case "ask_user":
		return "answered"
	}

	return fallbackSummary
}
