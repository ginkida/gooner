package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ShortcutsOverlay model for displaying keyboard shortcuts.
type ShortcutsOverlay struct {
	visible     bool
	styles      *Styles
	categories  []ShortcutCategory
	scrollIndex int
	searchQuery string
	filtered    []ShortcutCategory
}

// ShortcutCategory represents a category of shortcuts.
type ShortcutCategory struct {
	Name      string
	Shortcuts []Shortcut
}

// Shortcut represents a single keyboard shortcut.
type Shortcut struct {
	Keys        []string
	Description string
}

// DefaultShortcuts returns the default keyboard shortcuts.
func DefaultShortcuts() []ShortcutCategory {
	return []ShortcutCategory{
		{
			Name: "Navigation",
			Shortcuts: []Shortcut{
				{Keys: []string{"↑", "↓"}, Description: "Navigate suggestions or history"},
				{Keys: []string{"Ctrl", "b"}, Description: "Scroll up"},
				{Keys: []string{"Ctrl", "f"}, Description: "Scroll down"},
				{Keys: []string{"Ctrl", "u"}, Description: "Scroll half page up"},
				{Keys: []string{"Ctrl", "d"}, Description: "Scroll half page down"},
			},
		},
		{
			Name: "Input",
			Shortcuts: []Shortcut{
				{Keys: []string{"Enter"}, Description: "Send message"},
				{Keys: []string{"Tab"}, Description: "Autocomplete command"},
				{Keys: []string{"Ctrl", "R"}, Description: "Search history"},
				{Keys: []string{"Esc"}, Description: "Cancel/close modal"},
				{Keys: []string{"Ctrl", "C"}, Description: "Quit application"},
			},
		},
		{
			Name: "Command Center",
			Shortcuts: []Shortcut{
				{Keys: []string{"Ctrl", "p"}, Description: "Command Palette (All Actions)"},
				{Keys: []string{"y"}, Description: "Copy last response"},
				{Keys: []string{"Y"}, Description: "Copy conversation history"},
			},
		},
		{
			Name: "Code Actions",
			Shortcuts: []Shortcut{
				{Keys: []string{"Tab"}, Description: "Apply code block"},
				{Keys: []string{"Enter"}, Description: "Accept diff"},
				{Keys: []string{"Ctrl", "E"}, Description: "Edit code"},
			},
		},
		{
			Name: "History",
			Shortcuts: []Shortcut{
				{Keys: []string{"/undo"}, Description: "Undo last change"},
				{Keys: []string{"/redo"}, Description: "Redo undone change"},
				{Keys: []string{"/checkpoint"}, Description: "Create checkpoint"},
				{Keys: []string{"/restore"}, Description: "Restore checkpoint"},
			},
		},
		{
			Name: "Session Management",
			Shortcuts: []Shortcut{
				{Keys: []string{"/clear"}, Description: "Clear conversation"},
				{Keys: []string{"/save"}, Description: "Save session"},
				{Keys: []string{"/resume"}, Description: "Resume session"},
				{Keys: []string{"/cost"}, Description: "Show token usage"},
			},
		},
	}
}

// NewShortcutsOverlay creates a new shortcuts overlay.
func NewShortcutsOverlay(styles *Styles) *ShortcutsOverlay {
	return &ShortcutsOverlay{
		visible:     false,
		styles:      styles,
		categories:  DefaultShortcuts(),
		scrollIndex: 0,
		searchQuery: "",
		filtered:    nil, // nil means show all
	}
}

// Show displays the shortcuts overlay.
func (m *ShortcutsOverlay) Show() {
	m.visible = true
	m.scrollIndex = 0
}

// Hide hides the shortcuts overlay.
func (m *ShortcutsOverlay) Hide() {
	m.visible = false
}

// IsVisible returns whether the overlay is visible.
func (m *ShortcutsOverlay) IsVisible() bool {
	return m.visible
}

// Toggle toggles the visibility of the overlay.
func (m *ShortcutsOverlay) Toggle() {
	m.visible = !m.visible
	if m.visible {
		m.scrollIndex = 0
	}
}

// ScrollUp scrolls the shortcuts list up.
func (m *ShortcutsOverlay) ScrollUp() {
	if m.scrollIndex > 0 {
		m.scrollIndex--
	}
}

// ScrollDown scrolls the shortcuts list down.
func (m *ShortcutsOverlay) ScrollDown() {
	categories := m.getFilteredCategories()
	maxScroll := len(categories) - 5 // Show 5 categories at a time
	if m.scrollIndex < maxScroll {
		m.scrollIndex++
	}
}

// SetSearch sets the search query for filtering shortcuts.
func (m *ShortcutsOverlay) SetSearch(query string) {
	m.searchQuery = query
	m.scrollIndex = 0 // Reset scroll when searching
}

// ClearSearch clears the search query.
func (m *ShortcutsOverlay) ClearSearch() {
	m.searchQuery = ""
	m.scrollIndex = 0
}

// GetSearch returns the current search query.
func (m *ShortcutsOverlay) GetSearch() string {
	return m.searchQuery
}

// getFilteredCategories returns categories filtered by search query.
func (m *ShortcutsOverlay) getFilteredCategories() []ShortcutCategory {
	if m.searchQuery == "" {
		return m.categories
	}

	query := strings.ToLower(m.searchQuery)
	var filtered []ShortcutCategory

	for _, cat := range m.categories {
		var matchingShortcuts []Shortcut

		// Check if category name matches
		categoryMatches := strings.Contains(strings.ToLower(cat.Name), query)

		// Filter shortcuts within category
		for _, shortcut := range cat.Shortcuts {
			// Check if description matches
			descMatches := strings.Contains(strings.ToLower(shortcut.Description), query)

			// Check if any key matches
			keyMatches := false
			for _, key := range shortcut.Keys {
				if strings.Contains(strings.ToLower(key), query) {
					keyMatches = true
					break
				}
			}

			if categoryMatches || descMatches || keyMatches {
				matchingShortcuts = append(matchingShortcuts, shortcut)
			}
		}

		// Add category if it has matching shortcuts
		if len(matchingShortcuts) > 0 {
			filtered = append(filtered, ShortcutCategory{
				Name:      cat.Name,
				Shortcuts: matchingShortcuts,
			})
		}
	}

	return filtered
}

// View renders the shortcuts overlay.
func (m *ShortcutsOverlay) View(width, height int) string {
	if !m.visible {
		return ""
	}

	// Get categories to display (filtered or all)
	categories := m.getFilteredCategories()

	// Helper function
	minInt := func(a, b int) int {
		if a < b {
			return a
		}
		return b
	}

	// Overlay dimensions
	overlayWidth := minInt(80, width-4)
	overlayHeight := minInt(25, height-4)

	// Container style
	containerStyle := lipgloss.NewStyle().
		Width(overlayWidth).
		Height(overlayHeight).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorSecondary).
		Background(ColorBg).
		Padding(1, 2)

	// Title style
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary).
		MarginBottom(1)

	// Search prompt style
	searchStyle := lipgloss.NewStyle().
		Foreground(ColorAccent).
		Italic(true)

	// Category style
	categoryStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorAccent).
		MarginTop(1).
		MarginBottom(1)

	// Key style
	keyStyle := lipgloss.NewStyle().
		Foreground(ColorText).
		Background(ColorDim).
		Bold(true).
		Padding(0, 1).
		MarginRight(1)

	// Description style
	descStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	// Footer style
	footerStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true).
		MarginTop(1)

	var content strings.Builder

	// Title
	content.WriteString(titleStyle.Render("⌨️  Keyboard Shortcuts"))
	content.WriteString("\n")

	// Search prompt (if filtering)
	if m.searchQuery != "" {
		content.WriteString(searchStyle.Render(fmt.Sprintf("🔍 Filter: %s", m.searchQuery)))
		content.WriteString("\n")
		if len(categories) == 0 {
			content.WriteString("\nNo matching shortcuts found.")
			content.WriteString("\n")
			content.WriteString(footerStyle.Render("Type to filter • Esc to clear"))
			return containerStyle.Render(content.String())
		}
	} else {
		content.WriteString(searchStyle.Render("Type to filter shortcuts..."))
		content.WriteString("\n")
	}

	// Determine visible categories
	visibleCategories := categories
	if len(categories) > 5 {
		start := m.scrollIndex
		end := min(start+5, len(categories))
		visibleCategories = categories[start:end]
	}

	// Render categories
	for _, cat := range visibleCategories {
		content.WriteString(categoryStyle.Render("● " + cat.Name))
		content.WriteString("\n")

		for _, shortcut := range cat.Shortcuts {
			// Render keys
			var keysPart strings.Builder
			for i, key := range shortcut.Keys {
				if i > 0 {
					keysPart.WriteString(" + ")
				}
				keysPart.WriteString(keyStyle.Render(key))
			}

			// Render full line
			line := keysPart.String() + " " + descStyle.Render(shortcut.Description)
			content.WriteString(line)
			content.WriteString("\n")
		}
	}

	// Footer
	content.WriteString("\n")
	if m.searchQuery != "" {
		content.WriteString(footerStyle.Render("Esc to clear filter • ↑/↓ to scroll"))
	} else {
		content.WriteString(footerStyle.Render("Type to filter • Esc to close • ↑/↓ to scroll"))
	}

	// Wrap in container
	return containerStyle.Render(content.String())
}

// ContextualHelp provides contextual help based on current UI state.
type ContextualHelp struct {
	styles   *Styles
	hints    []string
	position int // 0 = top, 1 = bottom
}

// NewContextualHelp creates a new contextual help.
func NewContextualHelp(styles *Styles, position int) *ContextualHelp {
	return &ContextualHelp{
		styles:   styles,
		hints:    []string{},
		position: position,
	}
}

// SetHints sets the help hints.
func (h *ContextualHelp) SetHints(hints []string) {
	h.hints = hints
}

// AddHint adds a single hint.
func (h *ContextualHelp) AddHint(hint string) {
	h.hints = append(h.hints, hint)
}

// Clear clears all hints.
func (h *ContextualHelp) Clear() {
	h.hints = []string{}
}

// View renders the contextual help.
func (h *ContextualHelp) View(width int) string {
	if len(h.hints) == 0 {
		return ""
	}

	helpStyle := lipgloss.NewStyle().
		Foreground(ColorDim)

	// Join hints with separator
	hintText := strings.Join(h.hints, " • ")

	// Truncate if too long
	maxWidth := width - 4
	if len(hintText) > maxWidth {
		hintText = hintText[:maxWidth-3] + "..."
	}

	return helpStyle.Render(hintText)
}

// QuickAction represents a quick action button.
type QuickAction struct {
	Label    string
	Shortcut string
	Action   func()
}

// QuickActionsBar displays quick action buttons.
type QuickActionsBar struct {
	actions []QuickAction
	styles  *Styles
	visible bool
}

// NewQuickActionsBar creates a new quick actions bar.
func NewQuickActionsBar(styles *Styles) *QuickActionsBar {
	return &QuickActionsBar{
		actions: []QuickAction{},
		styles:  styles,
		visible: true,
	}
}

// SetActions sets the available actions.
func (b *QuickActionsBar) SetActions(actions []QuickAction) {
	b.actions = actions
}

// Show shows the bar.
func (b *QuickActionsBar) Show() {
	b.visible = true
}

// Hide hides the bar.
func (b *QuickActionsBar) Hide() {
	b.visible = false
}

// View renders the quick actions bar.
func (b *QuickActionsBar) View(width int) string {
	if !b.visible || len(b.actions) == 0 {
		return ""
	}

	containerStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		MarginTop(1).
		MarginBottom(1)

	var content strings.Builder

	for i, action := range b.actions {
		if i > 0 {
			content.WriteString("  ")
		}

		keyStyle := lipgloss.NewStyle().
			Foreground(ColorText).
			Background(ColorDim).
			Bold(true).
			Padding(0, 1)

		labelStyle := lipgloss.NewStyle().
			Foreground(ColorMuted)

		content.WriteString(keyStyle.Render(action.Shortcut))
		content.WriteString(" ")
		content.WriteString(labelStyle.Render(action.Label))
	}

	return containerStyle.Render(content.String())
}
