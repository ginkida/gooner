package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const (
	// Maximum entries to display in the activity feed
	maxActivityEntries = 10
	// Maximum recent log messages
	maxRecentLog = 5
)

// SubAgentState tracks the state of a sub-agent.
type SubAgentState struct {
	AgentID     string
	AgentType   string
	Description string
	CurrentTool string
	ToolArgs    map[string]any
	StartTime   time.Time
}

// ActivityFeedPanel displays real-time activity from tools and sub-agents.
type ActivityFeedPanel struct {
	visible       bool
	entries       []ActivityFeedEntry
	recentLog     []string
	activeEntries map[string]int // ID -> index in entries
	frame         int            // For spinner animation
	styles        *Styles
	mu            sync.RWMutex

	// Sub-agent tracking
	subAgentActivities map[string]*SubAgentState
}

// NewActivityFeedPanel creates a new activity feed panel.
func NewActivityFeedPanel(styles *Styles) *ActivityFeedPanel {
	return &ActivityFeedPanel{
		visible:            true, // Visible by default
		entries:            make([]ActivityFeedEntry, 0, maxActivityEntries),
		recentLog:          make([]string, 0, maxRecentLog),
		activeEntries:      make(map[string]int),
		styles:             styles,
		subAgentActivities: make(map[string]*SubAgentState),
	}
}

// AddEntry adds a new activity entry.
func (p *ActivityFeedPanel) AddEntry(entry ActivityFeedEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Update existing entry if it exists
	if idx, ok := p.activeEntries[entry.ID]; ok && idx < len(p.entries) {
		p.entries[idx] = entry
		return
	}

	// Add new entry
	if len(p.entries) >= maxActivityEntries {
		// Remove oldest entry
		oldID := p.entries[0].ID
		delete(p.activeEntries, oldID)
		p.entries = p.entries[1:]
		// Update indices
		for id, idx := range p.activeEntries {
			p.activeEntries[id] = idx - 1
		}
	}

	p.activeEntries[entry.ID] = len(p.entries)
	p.entries = append(p.entries, entry)
}

// CompleteEntry marks an entry as completed.
func (p *ActivityFeedPanel) CompleteEntry(id string, success bool, summary string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx, ok := p.activeEntries[id]
	if !ok || idx >= len(p.entries) {
		return
	}

	entry := &p.entries[idx]
	entry.Duration = time.Since(entry.StartTime)
	if success {
		entry.Status = ActivityCompleted
	} else {
		entry.Status = ActivityFailed
	}

	// Add to recent log
	logMsg := p.formatLogMessage(entry, summary)
	p.addRecentLog(logMsg)
}

// StartSubAgent starts tracking a sub-agent.
func (p *ActivityFeedPanel) StartSubAgent(agentID, agentType, description string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.subAgentActivities[agentID] = &SubAgentState{
		AgentID:     agentID,
		AgentType:   agentType,
		Description: description,
		StartTime:   time.Now(),
	}

	// Add as activity entry
	entry := ActivityFeedEntry{
		ID:          "agent-" + agentID,
		Type:        ActivityTypeAgent,
		Name:        agentType,
		Description: description,
		Status:      ActivityRunning,
		StartTime:   time.Now(),
		AgentID:     agentID,
	}

	if len(p.entries) >= maxActivityEntries {
		oldID := p.entries[0].ID
		delete(p.activeEntries, oldID)
		p.entries = p.entries[1:]
		for id, idx := range p.activeEntries {
			p.activeEntries[id] = idx - 1
		}
	}

	p.activeEntries[entry.ID] = len(p.entries)
	p.entries = append(p.entries, entry)
}

// UpdateSubAgentTool updates the current tool for a sub-agent.
func (p *ActivityFeedPanel) UpdateSubAgentTool(agentID, toolName string, args map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, ok := p.subAgentActivities[agentID]
	if !ok {
		return
	}

	state.CurrentTool = toolName
	state.ToolArgs = args

	// Update the entry description
	entryID := "agent-" + agentID
	if idx, ok := p.activeEntries[entryID]; ok && idx < len(p.entries) {
		desc := state.Description
		if toolName != "" {
			toolInfo := formatToolActivity(toolName, args)
			desc = fmt.Sprintf("%s -> %s", state.AgentType, toolInfo)
		}
		p.entries[idx].Description = desc
	}
}

// CompleteSubAgent marks a sub-agent as completed.
func (p *ActivityFeedPanel) CompleteSubAgent(agentID string, success bool, summary string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entryID := "agent-" + agentID
	idx, ok := p.activeEntries[entryID]
	if !ok || idx >= len(p.entries) {
		delete(p.subAgentActivities, agentID)
		return
	}

	state := p.subAgentActivities[agentID]
	entry := &p.entries[idx]
	if state != nil {
		entry.Duration = time.Since(state.StartTime)
	} else {
		entry.Duration = time.Since(entry.StartTime)
	}

	if success {
		entry.Status = ActivityCompleted
	} else {
		entry.Status = ActivityFailed
	}

	// Add to recent log
	logMsg := p.formatLogMessage(entry, summary)
	p.addRecentLog(logMsg)

	delete(p.subAgentActivities, agentID)
}

// Tick advances the spinner animation.
func (p *ActivityFeedPanel) Tick() {
	p.mu.Lock()
	p.frame++
	p.mu.Unlock()
}

// Toggle toggles the panel visibility.
func (p *ActivityFeedPanel) Toggle() {
	p.mu.Lock()
	p.visible = !p.visible
	p.mu.Unlock()
}

// IsVisible returns whether the panel is visible.
func (p *ActivityFeedPanel) IsVisible() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.visible
}

// HasActiveEntries returns whether there are any running entries.
func (p *ActivityFeedPanel) HasActiveEntries() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, entry := range p.entries {
		if entry.Status == ActivityRunning || entry.Status == ActivityPending {
			return true
		}
	}
	return len(p.subAgentActivities) > 0
}

// View renders the activity feed panel.
func (p *ActivityFeedPanel) View(width int) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.visible {
		return ""
	}

	// Only show if there are entries
	if len(p.entries) == 0 && len(p.recentLog) == 0 {
		return ""
	}

	var builder strings.Builder

	// Styles
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)

	headerStyle := lipgloss.NewStyle().
		Foreground(ColorAccent).
		Bold(true)

	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	successStyle := lipgloss.NewStyle().Foreground(ColorSuccess)
	errorStyle := lipgloss.NewStyle().Foreground(ColorError)
	toolStyle := lipgloss.NewStyle().Foreground(ColorGradient1).Bold(true)
	agentStyle := lipgloss.NewStyle().Foreground(ColorGradient2).Bold(true)
	timeStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	// Header
	builder.WriteString(headerStyle.Render("Activity"))
	builder.WriteString("\n")

	// Spinner frames
	spinnerFrames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	// Render active entries (most recent first)
	for i := len(p.entries) - 1; i >= 0 && i >= len(p.entries)-5; i-- {
		entry := p.entries[i]
		var line strings.Builder

		// Status icon
		switch entry.Status {
		case ActivityRunning, ActivityPending:
			spinner := spinnerFrames[p.frame%len(spinnerFrames)]
			line.WriteString(toolStyle.Render(spinner))
		case ActivityCompleted:
			line.WriteString(successStyle.Render("✓"))
		case ActivityFailed:
			line.WriteString(errorStyle.Render("✗"))
		}
		line.WriteString(" ")

		// Name (tool or agent)
		if entry.Type == ActivityTypeAgent {
			line.WriteString(agentStyle.Render(fmt.Sprintf("[%s]", entry.Name)))
		} else {
			line.WriteString(toolStyle.Render(entry.Name))
		}
		line.WriteString(" ")

		// Description (truncated to fit)
		maxDescLen := width - 30
		if maxDescLen < 20 {
			maxDescLen = 20
		}
		desc := entry.Description
		if len(desc) > maxDescLen {
			desc = desc[:maxDescLen-3] + "..."
		}
		line.WriteString(dimStyle.Render(desc))

		// Duration (right-aligned)
		var duration string
		if entry.Status == ActivityRunning || entry.Status == ActivityPending {
			duration = formatDuration(time.Since(entry.StartTime))
		} else {
			duration = formatDuration(entry.Duration)
		}
		// Pad to right
		padding := width - lipgloss.Width(line.String()) - len(duration) - 6
		if padding > 0 {
			line.WriteString(strings.Repeat(" ", padding))
		}
		line.WriteString(" ")
		line.WriteString(timeStyle.Render(duration))

		builder.WriteString(line.String())
		builder.WriteString("\n")
	}

	// Separator if we have recent log
	if len(p.recentLog) > 0 && len(p.entries) > 0 {
		builder.WriteString(dimStyle.Render(strings.Repeat("─", width-4)))
		builder.WriteString("\n")
	}

	// Recent log (last few messages)
	for _, msg := range p.recentLog {
		builder.WriteString(dimStyle.Render("  › " + msg))
		builder.WriteString("\n")
	}

	// Apply border
	content := strings.TrimSuffix(builder.String(), "\n")
	return borderStyle.Width(width - 2).Render(content)
}

// formatLogMessage formats an entry for the recent log.
func (p *ActivityFeedPanel) formatLogMessage(entry *ActivityFeedEntry, summary string) string {
	if summary != "" {
		return summary
	}

	var msg string
	switch entry.Type {
	case ActivityTypeAgent:
		if entry.Status == ActivityCompleted {
			msg = fmt.Sprintf("Sub-agent [%s] completed", entry.Name)
		} else {
			msg = fmt.Sprintf("Sub-agent [%s] failed", entry.Name)
		}
	default:
		msg = entry.Description
	}

	if entry.Duration > 0 {
		msg += fmt.Sprintf(" (%s)", formatDuration(entry.Duration))
	}

	return msg
}

// addRecentLog adds a message to the recent log.
func (p *ActivityFeedPanel) addRecentLog(msg string) {
	if len(p.recentLog) >= maxRecentLog {
		p.recentLog = p.recentLog[1:]
	}
	p.recentLog = append(p.recentLog, msg)
}

// formatToolActivity generates a description for a tool call.
func formatToolActivity(toolName string, args map[string]any) string {
	switch toolName {
	case "read":
		if path, ok := args["file_path"].(string); ok {
			return fmt.Sprintf("Reading %s", shortenPath(path, 40))
		}
	case "write":
		if path, ok := args["file_path"].(string); ok {
			size := 0
			if content, ok := args["content"].(string); ok {
				size = len(content)
			}
			return fmt.Sprintf("Writing %d bytes to %s", size, shortenPath(path, 30))
		}
	case "edit":
		if path, ok := args["file_path"].(string); ok {
			return fmt.Sprintf("Editing %s", shortenPath(path, 40))
		}
	case "grep":
		if pattern, ok := args["pattern"].(string); ok {
			p := pattern
			if len(p) > 30 {
				p = p[:27] + "..."
			}
			return fmt.Sprintf("Searching '%s'", p)
		}
	case "glob":
		if pattern, ok := args["pattern"].(string); ok {
			return fmt.Sprintf("Finding files: %s", pattern)
		}
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			c := cmd
			if len(c) > 40 {
				c = c[:37] + "..."
			}
			c = strings.ReplaceAll(c, "\n", " ")
			return fmt.Sprintf("Running: %s", c)
		}
	case "web_fetch":
		if url, ok := args["url"].(string); ok {
			u := url
			if len(u) > 40 {
				u = u[:37] + "..."
			}
			return fmt.Sprintf("Fetching %s", u)
		}
	case "web_search":
		if query, ok := args["query"].(string); ok {
			return fmt.Sprintf("Searching: %s", query)
		}
	case "list_dir", "tree":
		if path, ok := args["directory_path"].(string); ok {
			return fmt.Sprintf("Listing %s", shortenPath(path, 40))
		}
		return "Listing directory"
	case "task":
		if desc, ok := args["description"].(string); ok {
			return desc
		}
		if prompt, ok := args["prompt"].(string); ok {
			p := prompt
			if len(p) > 40 {
				p = p[:37] + "..."
			}
			return p
		}
	}
	return toolName
}

// Clear removes all entries from the activity feed.
func (p *ActivityFeedPanel) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.entries = make([]ActivityFeedEntry, 0, maxActivityEntries)
	p.recentLog = make([]string, 0, maxRecentLog)
	p.activeEntries = make(map[string]int)
	p.subAgentActivities = make(map[string]*SubAgentState)
}
