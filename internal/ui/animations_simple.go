package ui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// SpinnerType represents different spinner animation contexts.
type SpinnerType string

const (
	SpinnerThinking SpinnerType = "thinking" // AI thinking/processing
	SpinnerNetwork  SpinnerType = "network"  // Network operations
	SpinnerFile     SpinnerType = "file"     // File operations
	SpinnerSearch   SpinnerType = "search"   // Search operations
	SpinnerBuild    SpinnerType = "build"    // Build/compile operations
	SpinnerDefault  SpinnerType = "default"  // Default spinner
)

// Spinners provides different animation frames for different contexts.
var Spinners = map[SpinnerType][]string{
	SpinnerThinking: {".", "..", "...", ""},                              // Thinking dots
	SpinnerNetwork:  {"◜", "◠", "◝", "◞", "◡", "◟"},                      // Rotating arc
	SpinnerFile:     {"◐", "◓", "◑", "◒"},                                // Quarter circles
	SpinnerSearch:   {"◴", "◷", "◶", "◵"},                                // Clock-like
	SpinnerBuild:    {"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"},          // Building blocks
	SpinnerDefault:  {"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}, // Braille dots
}

// GetSpinnerFrame returns the current frame for a spinner type.
func GetSpinnerFrame(spinnerType SpinnerType) string {
	frames, ok := Spinners[spinnerType]
	if !ok {
		frames = Spinners[SpinnerDefault]
	}
	idx := int(time.Now().UnixMilli()/100) % len(frames)
	return frames[idx]
}

// GetSpinnerForTool returns the appropriate spinner type for a tool.
func GetSpinnerForTool(toolName string) SpinnerType {
	switch toolName {
	case "web_fetch", "web_search":
		return SpinnerNetwork
	case "read", "write", "edit", "glob":
		return SpinnerFile
	case "grep", "tree":
		return SpinnerSearch
	case "bash":
		return SpinnerBuild
	default:
		return SpinnerDefault
	}
}

// Simple animation functions for UX enhancement

// LoadingIndicator returns a loading indicator with animation
func LoadingIndicator(message string) string {
	frame := GetSpinnerFrame(SpinnerDefault)

	style := lipgloss.NewStyle().
		Foreground(ColorInfo).
		Bold(true)

	return style.Render(frame + " " + message + "...")
}

// LoadingIndicatorWithType returns a loading indicator with context-specific animation.
func LoadingIndicatorWithType(message string, spinnerType SpinnerType) string {
	frame := GetSpinnerFrame(spinnerType)

	style := lipgloss.NewStyle().
		Foreground(ColorInfo).
		Bold(true)

	return style.Render(frame + " " + message + "...")
}

// ProcessingIndicator shows processing with elapsed time
func ProcessingIndicator(message string, elapsed time.Duration) string {
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	idx := int(time.Now().Unix()/100) % len(spinners)

	var timeStr string
	if elapsed < time.Minute {
		timeStr = fmt.Sprintf("%.0fs", elapsed.Seconds())
	} else {
		timeStr = fmt.Sprintf("%.1fm", elapsed.Minutes())
	}

	timeStyle := lipgloss.NewStyle().
		Foreground(ColorMuted).
		Italic(true)

	return lipgloss.NewStyle().
		Foreground(ColorInfo).
		Bold(true).
		Render(spinners[idx]+" "+message) + " " + timeStyle.Render("("+timeStr+")")
}

// SuccessAnimation returns a success message
func SuccessAnimation(message string) string {
	return lipgloss.NewStyle().
		Foreground(ColorSuccess).
		Bold(true).
		Render("✨ " + message)
}

// ErrorAnimation returns an error message
func ErrorAnimation(message string) string {
	return lipgloss.NewStyle().
		Foreground(ColorError).
		Bold(true).
		Render("✕ " + message)
}

// WarningAnimation returns a warning message
func WarningAnimation(message string) string {
	return lipgloss.NewStyle().
		Foreground(ColorWarning).
		Bold(true).
		Render("⚠ " + message)
}

// InfoAnimation returns an info message
func InfoAnimation(message string) string {
	return lipgloss.NewStyle().
		Foreground(ColorInfo).
		Bold(true).
		Render("ℹ " + message)
}
