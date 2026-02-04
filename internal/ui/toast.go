package ui

import (
	"strings"
	"sync"
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
	mu        sync.Mutex
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
	m.mu.Lock()
	defer m.mu.Unlock()

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

// ShowErrorWithHint displays an error toast with optional actionable hint.
// If a matching error pattern is found, appends hint: "Error message → Hint"
func (m *ToastManager) ShowErrorWithHint(message string) {
	hint := GetCompactHint(message)
	if hint != "" {
		message = message + " → " + hint
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, toast := range m.toasts {
		if toast.ID == id {
			m.toasts = append(m.toasts[:i], m.toasts[i+1:]...)
			return
		}
	}
}

// Update removes expired toasts.
func (m *ToastManager) Update() {
	m.mu.Lock()
	defer m.mu.Unlock()

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
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.toasts)
}

// View renders all active toasts in the right upper corner.
func (m *ToastManager) View(width int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

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

// renderToast renders a single toast as compact single line.
// Format: ✓ Message (no borders, minimal space)
func (m *ToastManager) renderToast(toast Toast, width int) string {
	var icon string
	var iconColor lipgloss.Color

	switch toast.Type {
	case ToastSuccess:
		icon, iconColor = "✓", ColorSuccess
	case ToastError:
		icon, iconColor = "✗", ColorError
	case ToastWarning:
		icon, iconColor = "⚠", ColorWarning
	default: // ToastInfo
		icon, iconColor = "ℹ", ColorInfo
	}

	// Fade effect when nearing expiration
	elapsed := time.Since(toast.CreatedAt)
	remaining := toast.Duration - elapsed
	if remaining < 500*time.Millisecond {
		iconColor = ColorDim
	}

	iconStyle := lipgloss.NewStyle().Foreground(iconColor).Bold(true)
	msgStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	// Truncate message to fit width
	msg := toast.Message
	maxLen := width - 5
	if maxLen < 20 {
		maxLen = 20
	}
	if len(msg) > maxLen {
		msg = msg[:maxLen-1] + "…"
	}

	return iconStyle.Render(icon) + " " + msgStyle.Render(msg)
}

// Clear removes all toasts.
func (m *ToastManager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toasts = m.toasts[:0]
}
