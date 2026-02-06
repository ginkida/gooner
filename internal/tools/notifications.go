package tools

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// NotificationManager handles user notifications for tool execution
type NotificationManager struct {
	mu                  sync.RWMutex
	history             []Notification
	maxHistory          int
	onNotify            func(Notification)
	quietMode           bool
	verboseMode         bool
	nativeNotifications bool            // Send native macOS notifications (disabled by default)
	silentTools         map[string]bool // Tools that shouldn't notify
}

// Notification represents a user-facing notification
type Notification struct {
	ID        string
	Type      NotificationType
	ToolName  string
	Message   string
	Details   string
	Timestamp time.Time
	Summary   *ExecutionSummary
	Read      bool
}

// NotificationType represents the severity/type of notification
type NotificationType string

const (
	NotificationTypeInfo     NotificationType = "info"
	NotificationTypeSuccess  NotificationType = "success"
	NotificationTypeWarning  NotificationType = "warning"
	NotificationTypeError    NotificationType = "error"
	NotificationTypeProgress NotificationType = "progress"
)

// NewNotificationManager creates a new notification manager
func NewNotificationManager() *NotificationManager {
	return &NotificationManager{
		history:     make([]Notification, 0),
		maxHistory:  100,
		silentTools: make(map[string]bool),
	}
}

// SetOnNotify sets the callback function for notifications
func (nm *NotificationManager) SetOnNotify(fn func(Notification)) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.onNotify = fn
}

// EnableQuietMode suppresses non-error notifications
func (nm *NotificationManager) EnableQuietMode(enabled bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.quietMode = enabled
}

// EnableVerboseMode shows all notifications including progress
func (nm *NotificationManager) EnableVerboseMode(enabled bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.verboseMode = enabled
}

// EnableNativeNotifications enables/disables native macOS Notification Center alerts.
// Disabled by default to avoid intrusive notifications during work.
func (nm *NotificationManager) EnableNativeNotifications(enabled bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.nativeNotifications = enabled
}

// SetSilentTool configures a tool to not send notifications
func (nm *NotificationManager) SetSilentTool(toolName string, silent bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if silent {
		nm.silentTools[toolName] = true
	} else {
		delete(nm.silentTools, toolName)
	}
}

// Notify sends a notification to the user
func (nm *NotificationManager) Notify(typ NotificationType, toolName, message, details string, summary *ExecutionSummary) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// Check if tool is silent
	if nm.silentTools[toolName] {
		return
	}

	// Filter based on mode
	if nm.quietMode && typ != NotificationTypeError && typ != NotificationTypeWarning {
		return
	}
	if !nm.verboseMode && typ == NotificationTypeProgress {
		return
	}

	notif := Notification{
		ID:        fmt.Sprintf("%s-%d", toolName, time.Now().UnixNano()),
		Type:      typ,
		ToolName:  toolName,
		Message:   message,
		Details:   details,
		Timestamp: time.Now(),
		Summary:   summary,
		Read:      false,
	}

	// Add to history
	nm.history = append(nm.history, notif)
	if len(nm.history) > nm.maxHistory {
		nm.history = nm.history[1:]
	}

	// Call callback if set
	if nm.onNotify != nil {
		// Call in goroutine to avoid blocking
		go nm.onNotify(notif)
	}

	// Native macOS notifications (opt-in)
	if runtime.GOOS == "darwin" && nm.nativeNotifications {
		go nm.sendNativeNotification(notif)
	}
}

// sendNativeNotification sends a native macOS notification using AppleScript.
func (nm *NotificationManager) sendNativeNotification(n Notification) {
	// Only notify for critical/important events or long-running successes
	if n.Type != NotificationTypeError && n.Type != NotificationTypeWarning && n.Type != NotificationTypeSuccess {
		return
	}

	title := "Gokin"
	if n.ToolName != "" {
		title = fmt.Sprintf("Gokin: %s", n.ToolName)
	}

	// Escape message for AppleScript
	msg := strings.ReplaceAll(n.Message, "\"", "\\\"")
	msg = strings.ReplaceAll(msg, "'", "\\'")

	script := fmt.Sprintf("display notification \"%s\" with title \"%s\"", msg, title)
	if n.Type == NotificationTypeSuccess {
		script += " sound name \"Glass\""
	} else if n.Type == NotificationTypeError {
		script += " sound name \"Basso\""
	}

	_ = exec.Command("osascript", "-e", script).Run()
}

// NotifyInfo sends an info notification
func (nm *NotificationManager) NotifyInfo(toolName, message string) {
	nm.Notify(NotificationTypeInfo, toolName, message, "", nil)
}

// NotifySuccess sends a success notification
func (nm *NotificationManager) NotifySuccess(toolName, message string, summary *ExecutionSummary, duration time.Duration) {
	details := fmt.Sprintf("Completed in %s", formatDuration(duration))
	nm.Notify(NotificationTypeSuccess, toolName, message, details, summary)
}

// NotifyWarning sends a warning notification
func (nm *NotificationManager) NotifyWarning(toolName, message string, details []string) {
	detailStr := ""
	if len(details) > 0 {
		detailStr = fmt.Sprintf("Warnings:\n• %s", joinStrings(details, "\n• "))
	}
	nm.Notify(NotificationTypeWarning, toolName, message, detailStr, nil)
}

// NotifyError sends an error notification
func (nm *NotificationManager) NotifyError(toolName, message, error string) {
	nm.Notify(NotificationTypeError, toolName, message, error, nil)
}

// NotifyProgress sends a progress notification
func (nm *NotificationManager) NotifyProgress(toolName string, elapsed time.Duration) {
	message := fmt.Sprintf("Running... (%s)", FormatDuration(elapsed))
	nm.Notify(NotificationTypeProgress, toolName, message, "", nil)
}

// NotifyValidation sends validation warnings
func (nm *NotificationManager) NotifyValidation(toolName string, check *PreFlightCheck) {
	if check == nil || len(check.Warnings) == 0 {
		return
	}
	nm.NotifyWarning(toolName, "Safety validation warnings", check.Warnings)
}

// NotifyDenied sends a permission denied notification
func (nm *NotificationManager) NotifyDenied(toolName, reason string) {
	nm.Notify(NotificationTypeError, toolName, "Permission denied", reason, nil)
}

// NotifyApproved sends a permission approved notification
func (nm *NotificationManager) NotifyApproved(toolName string, summary *ExecutionSummary) {
	message := fmt.Sprintf("Approved: %s", summary.DisplayName)
	nm.Notify(NotificationTypeInfo, toolName, message, "", summary)
}

// GetHistory returns notification history
func (nm *NotificationManager) GetHistory(limit int) []Notification {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	if limit <= 0 || limit > len(nm.history) {
		limit = len(nm.history)
	}

	// Return most recent first
	result := make([]Notification, limit)
	start := len(nm.history) - limit
	copy(result, nm.history[start:])
	return result
}

// GetUnreadCount returns the number of unread notifications
func (nm *NotificationManager) GetUnreadCount() int {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	count := 0
	for _, n := range nm.history {
		if !n.Read {
			count++
		}
	}
	return count
}

// MarkAsRead marks a notification as read
func (nm *NotificationManager) MarkAsRead(id string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	for i := range nm.history {
		if nm.history[i].ID == id {
			nm.history[i].Read = true
			break
		}
	}
}

// MarkAllAsRead marks all notifications as read
func (nm *NotificationManager) MarkAllAsRead() {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	for i := range nm.history {
		nm.history[i].Read = true
	}
}

// ClearHistory clears all notification history
func (nm *NotificationManager) ClearHistory() {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.history = make([]Notification, 0)
}

// GetStats returns notification statistics
func (nm *NotificationManager) GetStats() NotificationStats {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	stats := NotificationStats{
		Total:  len(nm.history),
		Unread: 0,
		ByType: make(map[NotificationType]int),
		ByTool: make(map[string]int),
	}

	for _, n := range nm.history {
		if !n.Read {
			stats.Unread++
		}
		stats.ByType[n.Type]++
		stats.ByTool[n.ToolName]++
	}

	return stats
}

// NotificationStats holds notification statistics
type NotificationStats struct {
	Total  int
	Unread int
	ByType map[NotificationType]int
	ByTool map[string]int
}

// FormatDuration converts duration to human-readable string
func FormatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "< 1ms"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Second).String()
}

func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}
