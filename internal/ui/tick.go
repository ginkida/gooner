package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TickMsg is sent periodically to trigger UI updates
type TickMsg time.Time

// TickCmd returns a command that sends TickMsg at intervals
func TickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}
