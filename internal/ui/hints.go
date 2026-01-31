package ui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// HintSystem manages contextual hints for the user.
type HintSystem struct {
	enabled      bool
	hintsShown   map[string]int
	styles       *Styles
	lastHint     string
	lastHintTime time.Time
}

// NewHintSystem creates a new hint system.
func NewHintSystem(styles *Styles) *HintSystem {
	return &HintSystem{
		enabled:    true,
		hintsShown: make(map[string]int),
		styles:     styles,
	}
}

// GetContextualHint returns a contextual hint based on the current state.
func (h *HintSystem) GetContextualHint(state State, currentTool string, sessionDuration time.Duration) string {
	if !h.enabled {
		return ""
	}

	// Only show hints every 60 seconds at most (less frequent)
	if time.Since(h.lastHintTime) < 60*time.Second {
		return ""
	}

	var hint string
	hintID := ""

	// Context-aware hints (simplified)
	switch {
	case sessionDuration < 2*time.Minute:
		hint = "Shift+Tab toggles planning mode for complex tasks"
		hintID = "first_message"

	case state == StateStreaming:
		hint = "Esc cancels response"
		hintID = "cancel_streaming"

	default:
		// Rotate through general hints (shorter)
		generalHints := []string{
			"Shift+Tab toggles planning mode",
			"Ctrl+P opens command palette",
			"/copy --last copies AI response",
		}

		idx := len(h.hintsShown) % len(generalHints)
		hint = generalHints[idx]
		hintID = fmt.Sprintf("general_%d", idx)
	}

	h.hintsShown[hintID]++

	// Only show the same hint once
	if h.hintsShown[hintID] > 1 {
		return ""
	}

	h.lastHint = hint
	h.lastHintTime = time.Now()
	return hint
}

// RenderHint renders a hint with minimal styling (single line).
func (h *HintSystem) RenderHint(hint string) string {
	if hint == "" {
		return ""
	}

	hintStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true)

	return hintStyle.Render("Tip: " + hint)
}

// ShouldShowHint checks if a hint should be shown based on frequency.
func (h *HintSystem) ShouldShowHint(hintID string, maxShows int) bool {
	count, exists := h.hintsShown[hintID]
	if !exists {
		return true
	}
	return count < maxShows
}

// MarkHintShown marks a hint as shown.
func (h *HintSystem) MarkHintShown(hintID string) {
	h.hintsShown[hintID]++
	h.lastHintTime = time.Now()
}

// Reset clears hint history.
func (h *HintSystem) Reset() {
	h.hintsShown = make(map[string]int)
}

// Disable turns off hints.
func (h *HintSystem) Disable() {
	h.enabled = false
}

// Enable turns on hints.
func (h *HintSystem) Enable() {
	h.enabled = true
}
