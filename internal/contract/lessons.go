package contract

import (
	"fmt"
	"strings"
	"time"
)

// NewLesson creates a new lesson with a generated ID.
func NewLesson(content, category, contractID string, applicable []string) *Lesson {
	return &Lesson{
		ID:         fmt.Sprintf("lesson-%d", time.Now().UnixNano()),
		Content:    content,
		Category:   category,
		ContractID: contractID,
		Applicable: applicable,
	}
}

// FormatLessons renders a list of lessons as a human-readable string.
func FormatLessons(lessons []*Lesson) string {
	if len(lessons) == 0 {
		return "No lessons found."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Lessons (%d):\n\n", len(lessons)))

	byCategory := map[string][]*Lesson{}
	for _, l := range lessons {
		cat := l.Category
		if cat == "" {
			cat = "general"
		}
		byCategory[cat] = append(byCategory[cat], l)
	}

	categoryOrder := []string{"pitfall", "pattern", "optimization", "general"}
	categoryIcons := map[string]string{
		"pitfall":      "!",
		"pattern":      "*",
		"optimization": "+",
		"general":      "-",
	}

	for _, cat := range categoryOrder {
		items, ok := byCategory[cat]
		if !ok || len(items) == 0 {
			continue
		}

		icon := categoryIcons[cat]
		sb.WriteString(fmt.Sprintf("[%s] %s:\n", strings.ToUpper(cat), cat))
		for _, l := range items {
			sb.WriteString(fmt.Sprintf("  %s %s\n", icon, l.Content))
			if len(l.Applicable) > 0 {
				sb.WriteString(fmt.Sprintf("    Tags: %s\n", strings.Join(l.Applicable, ", ")))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
