package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ToastType represents the type of toast notification.
type ToastType int

const (
	ToastInfo ToastType = iota
	ToastSuccess
	ToastWarning
	ToastError
)

// Toast represents a single toast notification.
type Toast struct {
	ID        int
	Type      ToastType
	Title     string
	Message   string
	Duration  time.Duration
	CreatedAt time.Time
	FadeOut   bool
}

// IsExpired returns true if the toast should be removed.
func (t *Toast) IsExpired() bool {
	return time.Since(t.CreatedAt) > t.Duration
}

// ToastManager manages toast notifications.
type ToastManager struct {
	toasts    []Toast
	maxToasts int
	styles    *Styles
	nextID    int
}

// NewToastManager creates a new toast manager.
func NewToastManager(styles *Styles) *ToastManager {
	return &ToastManager{
		toasts:    make([]Toast, 0),
		maxToasts: 3,
		styles:    styles,
		nextID:    1,
	}
}

// Show displays a new toast notification.
func (m *ToastManager) Show(toastType ToastType, title, message string, duration time.Duration) {
	toast := Toast{
		ID:        m.nextID,
		Type:      toastType,
		Title:     title,
		Message:   message,
		Duration:  duration,
		CreatedAt: time.Now(),
		FadeOut:   false,
	}
	m.nextID++

	// Add to the beginning (newest first)
	m.toasts = append([]Toast{toast}, m.toasts...)

	// Limit the number of toasts
	if len(m.toasts) > m.maxToasts {
		m.toasts = m.toasts[:m.maxToasts]
	}
}

// ShowSuccess displays a success toast.
func (m *ToastManager) ShowSuccess(message string) {
	m.Show(ToastSuccess, "Success", message, 3*time.Second)
}

// ShowError displays an error toast.
func (m *ToastManager) ShowError(message string) {
	m.Show(ToastError, "Error", message, 5*time.Second)
}

// ShowInfo displays an info toast.
func (m *ToastManager) ShowInfo(message string) {
	m.Show(ToastInfo, "Info", message, 3*time.Second)
}

// ShowWarning displays a warning toast.
func (m *ToastManager) ShowWarning(message string) {
	m.Show(ToastWarning, "Warning", message, 4*time.Second)
}

// Dismiss removes a toast by ID.
func (m *ToastManager) Dismiss(id int) {
	for i, toast := range m.toasts {
		if toast.ID == id {
			m.toasts = append(m.toasts[:i], m.toasts[i+1:]...)
			return
		}
	}
}

// Update removes expired toasts.
func (m *ToastManager) Update() {
	var active []Toast
	for _, toast := range m.toasts {
		if !toast.IsExpired() {
			active = append(active, toast)
		}
	}
	m.toasts = active
}

// Count returns the number of active toasts.
func (m *ToastManager) Count() int {
	return len(m.toasts)
}

// View renders all active toasts in the right upper corner.
func (m *ToastManager) View(width int) string {
	if len(m.toasts) == 0 {
		return ""
	}

	var lines []string

	for _, toast := range m.toasts {
		line := m.renderToast(toast, width)
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

// renderToast renders a single toast notification.
func (m *ToastManager) renderToast(toast Toast, width int) string {
	// Icon and color based on type
	var icon string
	var borderColor lipgloss.Color
	var titleColor lipgloss.Color

	switch toast.Type {
	case ToastSuccess:
		icon = "✓"
		borderColor = ColorSuccess
		titleColor = ColorSuccess
	case ToastError:
		icon = "✗"
		borderColor = ColorError
		titleColor = ColorError
	case ToastWarning:
		icon = "⚠"
		borderColor = ColorWarning
		titleColor = ColorWarning
	default: // ToastInfo
		icon = "ℹ"
		borderColor = ColorInfo
		titleColor = ColorInfo
	}

	// Calculate opacity based on remaining time
	elapsed := time.Since(toast.CreatedAt)
	remaining := toast.Duration - elapsed
	opacity := 1.0
	if remaining < 500*time.Millisecond {
		opacity = float64(remaining) / float64(500*time.Millisecond)
	}

	// Toast width (max 40 chars, min 20)
	toastWidth := 40
	if width < 60 {
		toastWidth = 25
	}

	// Styles
	containerStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(toastWidth)

	iconStyle := lipgloss.NewStyle().
		Foreground(titleColor).
		Bold(true)

	titleStyle := lipgloss.NewStyle().
		Foreground(titleColor).
		Bold(true)

	messageStyle := lipgloss.NewStyle().
		Foreground(ColorText)

	// Apply fade effect
	if opacity < 1.0 {
		containerStyle = containerStyle.Foreground(ColorDim)
		messageStyle = messageStyle.Foreground(ColorDim)
	}

	// Build content
	var content strings.Builder

	// Title line with icon
	content.WriteString(iconStyle.Render(icon))
	content.WriteString(" ")
	content.WriteString(titleStyle.Render(toast.Title))

	// Message (if present)
	if toast.Message != "" {
		content.WriteString("\n")
		// Truncate message if too long
		msg := toast.Message
		maxMsgLen := toastWidth - 4
		if len(msg) > maxMsgLen {
			msg = msg[:maxMsgLen-3] + "..."
		}
		content.WriteString(messageStyle.Render(msg))
	}

	return containerStyle.Render(content.String())
}

// Clear removes all toasts.
func (m *ToastManager) Clear() {
	m.toasts = m.toasts[:0]
}
