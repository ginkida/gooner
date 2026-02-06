package ui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// CommandType defines what kind of command this is
type CommandType int

const (
	CommandTypeSlash  CommandType = iota // Executes a slash command
	CommandTypeAction                    // Executes a direct action
)

// PaletteCategoryInfo contains display information for a category (local copy to avoid import cycle).
type PaletteCategoryInfo struct {
	ID       string
	Name     string
	Icon     string
	Priority int
}

// PaletteCommandData is the interface that command data must implement.
type PaletteCommandData interface {
	GetName() string
	GetDescription() string
	GetUsage() string
	GetCategoryName() string
	GetCategoryIcon() string
	GetCategoryPriority() int
	GetIcon() string
	GetArgHint() string
	IsEnabled() bool
	GetReason() string
	GetPriority() int
}

// PaletteProvider is an interface for fetching palette commands.
// Uses any slice to avoid import cycle issues - caller must return []PaletteCommandData.
type PaletteProvider interface {
	GetPaletteCommandsForUI() []any
}

// EnhancedPaletteCommand represents a command in the enhanced command palette.
type EnhancedPaletteCommand struct {
	Name        string
	Description string
	Usage       string
	Shortcut    string // /command format
	Category    PaletteCategoryInfo
	Icon        string
	ArgHint     string
	Enabled     bool
	Reason      string // Why disabled
	Priority    int    // For sorting
	IsRecent    bool   // In recently used
	Type        CommandType
	Action      func() // Direct action (for CommandTypeAction)
}

// CommandPalette provides quick access to commands via Ctrl+P.
type CommandPalette struct {
	visible         bool
	query           string
	commands        []EnhancedPaletteCommand
	filtered        []EnhancedPaletteCommand
	selected        int
	styles          *Styles
	maxHeight       int
	scroll          int
	width           int
	height          int
	history         *CommandHistory
	showPreview     bool
	previewCmd      *EnhancedPaletteCommand
	paletteProvider PaletteProvider
}

// NewCommandPalette creates a new command palette.
func NewCommandPalette(styles *Styles) *CommandPalette {
	return &CommandPalette{
		visible:   false,
		query:     "",
		commands:  nil,
		filtered:  nil,
		selected:  0,
		styles:    styles,
		maxHeight: 20,
		scroll:    0,
		history:   NewCommandHistory(),
	}
}

// SetPaletteProvider sets the provider for fetching commands.
func (p *CommandPalette) SetPaletteProvider(provider PaletteProvider) {
	p.paletteProvider = provider
}

// RefreshCommands refreshes the command list from the provider.
func (p *CommandPalette) RefreshCommands() {
	if p.paletteProvider == nil {
		return
	}

	paletteCmds := p.paletteProvider.GetPaletteCommandsForUI()
	recentCmds := p.history.GetRecentCommands(5)
	recentSet := make(map[string]bool)
	for _, c := range recentCmds {
		recentSet[c] = true
	}

	p.commands = make([]EnhancedPaletteCommand, 0, len(paletteCmds))
	for _, item := range paletteCmds {
		pc, ok := item.(PaletteCommandData)
		if !ok {
			continue
		}
		p.commands = append(p.commands, EnhancedPaletteCommand{
			Name:        pc.GetName(),
			Description: pc.GetDescription(),
			Usage:       pc.GetUsage(),
			Shortcut:    "/" + pc.GetName(),
			Category: PaletteCategoryInfo{
				Name:     pc.GetCategoryName(),
				Icon:     pc.GetCategoryIcon(),
				Priority: pc.GetCategoryPriority(),
			},
			Icon:     pc.GetIcon(),
			ArgHint:  pc.GetArgHint(),
			Enabled:  pc.IsEnabled(),
			Reason:   pc.GetReason(),
			Priority: pc.GetPriority(),
			IsRecent: recentSet[pc.GetName()],
			Type:     CommandTypeSlash,
		})
	}

	p.sortCommands()
	p.filterCommands(p.query)
}

// sortCommands sorts commands by: recent first (by real timestamp), then by category priority, then by priority within category.
func (p *CommandPalette) sortCommands() {
	sort.SliceStable(p.commands, func(i, j int) bool {
		// Recent commands first
		if p.commands[i].IsRecent && !p.commands[j].IsRecent {
			return true
		}
		if !p.commands[i].IsRecent && p.commands[j].IsRecent {
			return false
		}

		// For recent commands, sort by actual timestamp (most recent first)
		if p.commands[i].IsRecent && p.commands[j].IsRecent {
			ti := p.history.GetTimestamp(p.commands[i].Name)
			tj := p.history.GetTimestamp(p.commands[j].Name)
			return ti.After(tj)
		}

		// Then by priority (lower = higher priority)
		return p.commands[i].Priority < p.commands[j].Priority
	})
}

// Show displays the command palette.
func (p *CommandPalette) Show() {
	p.visible = true
	p.query = ""
	p.selected = 0
	p.scroll = 0
	p.showPreview = false
	p.previewCmd = nil
	p.RefreshCommands()
}

// Hide hides the command palette.
func (p *CommandPalette) Hide() {
	p.visible = false
	p.query = ""
	p.selected = 0
	p.scroll = 0
	p.showPreview = false
	p.previewCmd = nil
}

// Toggle toggles the visibility.
func (p *CommandPalette) Toggle() {
	if p.visible {
		p.Hide()
	} else {
		p.Show()
	}
}

// IsVisible returns whether the palette is visible.
func (p *CommandPalette) IsVisible() bool {
	return p.visible
}

// SetQuery sets the search query and filters commands.
func (p *CommandPalette) SetQuery(query string) {
	p.query = query
	p.filterCommands(query)
	p.selected = 0
	p.scroll = 0
}

// AppendQuery appends a character to the query.
func (p *CommandPalette) AppendQuery(char string) {
	p.SetQuery(p.query + char)
}

// BackspaceQuery removes the last character from the query.
func (p *CommandPalette) BackspaceQuery() {
	if len(p.query) > 0 {
		p.SetQuery(p.query[:len(p.query)-1])
	}
}

// GetQuery returns the current query.
func (p *CommandPalette) GetQuery() string {
	return p.query
}

// filterCommands filters commands based on the query with fuzzy matching.
func (p *CommandPalette) filterCommands(query string) {
	if query == "" {
		p.filtered = p.commands
		return
	}

	query = strings.ToLower(query)
	var matches []EnhancedPaletteCommand

	for _, cmd := range p.commands {
		name := strings.ToLower(cmd.Name)
		desc := strings.ToLower(cmd.Description)
		shortcut := strings.ToLower(cmd.Shortcut)
		category := strings.ToLower(cmd.Category.Name)

		// Check for substring match
		if strings.Contains(name, query) ||
			strings.Contains(desc, query) ||
			strings.Contains(shortcut, query) ||
			strings.Contains(category, query) {
			matches = append(matches, cmd)
			continue
		}

		// Check for fuzzy match (all query chars in order)
		if fuzzyMatch(name, query) {
			matches = append(matches, cmd)
		}
	}

	p.filtered = matches
}

// fuzzyMatch checks if all characters in query appear in str in order.
func fuzzyMatch(str, query string) bool {
	qi := 0
	for _, c := range str {
		if qi < len(query) && byte(c) == query[qi] {
			qi++
		}
	}
	return qi == len(query)
}

// SelectNext moves selection to the next item.
func (p *CommandPalette) SelectNext() {
	if len(p.filtered) > 0 && p.selected < len(p.filtered)-1 {
		p.selected++
		p.adjustScroll()
	}
}

// SelectPrev moves selection to the previous item.
func (p *CommandPalette) SelectPrev() {
	if p.selected > 0 {
		p.selected--
		p.adjustScroll()
	}
}

// adjustScroll ensures the selected item is visible.
func (p *CommandPalette) adjustScroll() {
	visibleItems := p.maxHeight - 8 // Account for header, input, footer, borders
	if visibleItems < 3 {
		visibleItems = 3
	}

	if p.selected < p.scroll {
		p.scroll = p.selected
	} else if p.selected >= p.scroll+visibleItems {
		p.scroll = p.selected - visibleItems + 1
	}
}

// TogglePreview toggles the preview panel.
func (p *CommandPalette) TogglePreview() {
	if len(p.filtered) == 0 || p.selected >= len(p.filtered) {
		return
	}
	p.showPreview = !p.showPreview
	if p.showPreview {
		p.previewCmd = &p.filtered[p.selected]
	} else {
		p.previewCmd = nil
	}
}

// GetSelected returns the currently selected command, or nil if none.
func (p *CommandPalette) GetSelected() *EnhancedPaletteCommand {
	if len(p.filtered) == 0 || p.selected >= len(p.filtered) {
		return nil
	}
	return &p.filtered[p.selected]
}

// Execute executes the selected command and returns it.
// If the query starts with "/" and matches a command name exactly, that command
// is executed directly without requiring list navigation.
func (p *CommandPalette) Execute() *EnhancedPaletteCommand {
	// Direct slash command execution: if query starts with "/" and matches a command name exactly
	if strings.HasPrefix(p.query, "/") {
		queryName := p.query[1:] // strip leading "/"
		for i := range p.commands {
			if strings.EqualFold(p.commands[i].Name, queryName) && p.commands[i].Enabled {
				p.history.RecordUsage(p.commands[i].Name)
				cmd := p.commands[i]
				if cmd.Type == CommandTypeAction && cmd.Action != nil {
					cmd.Action()
				}
				p.Hide()
				return &cmd
			}
		}
	}

	cmd := p.GetSelected()
	if cmd == nil {
		p.Hide()
		return nil
	}

	// Don't execute disabled commands
	if !cmd.Enabled {
		return nil
	}

	// Record usage
	p.history.RecordUsage(cmd.Name)

	switch cmd.Type {
	case CommandTypeAction:
		if cmd.Action != nil {
			cmd.Action()
		}
	}

	p.Hide()
	return cmd
}

// SetSize sets the available size for rendering.
func (p *CommandPalette) SetSize(width, height int) {
	p.width = width
	p.height = height
}


// View renders the command palette.
func (p *CommandPalette) View(width, height int) string {
	if !p.visible {
		return ""
	}

	// Use stored size or passed size
	if width == 0 {
		width = p.width
	}
	if height == 0 {
		height = p.height
	}

	// Palette dimensions
	paletteWidth := min(70, width-6)
	if paletteWidth < 45 {
		paletteWidth = 45
	}

	// Styles
	containerStyle := lipgloss.NewStyle().
		Width(paletteWidth).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorSecondary).
		Padding(0, 1)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary)

	subtitleStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true)

	inputBoxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorAccent).
		Padding(0, 1).
		Width(paletteWidth - 4)

	placeholderStyle := lipgloss.NewStyle().
		Foreground(ColorMuted).
		Italic(true)

	queryStyle := lipgloss.NewStyle().
		Foreground(ColorText).
		Bold(true)

	selectedBgStyle := lipgloss.NewStyle().
		Background(ColorSecondary).
		Foreground(ColorBg).
		Bold(true).
		Width(paletteWidth - 6).
		Padding(0, 1)

	normalStyle := lipgloss.NewStyle().
		Foreground(ColorText).
		Width(paletteWidth - 6).
		Padding(0, 1)

	disabledStyle := lipgloss.NewStyle().
		Foreground(ColorMuted).
		Width(paletteWidth - 6).
		Padding(0, 1)

	shortcutStyle := lipgloss.NewStyle().
		Foreground(ColorAccent).
		Bold(true)

	shortcutDisabledStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	descStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	argHintStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true)

	categoryStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Bold(true).
		MarginTop(1)

	footerStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true).
		Align(lipgloss.Center).
		Width(paletteWidth - 4)

	scrollStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true)

	recentBadgeStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)

	var content strings.Builder

	// Header
	content.WriteString(titleStyle.Render("Command Palette"))
	content.WriteString("  ")
	content.WriteString(subtitleStyle.Render("Ctrl+P"))
	content.WriteString("\n\n")

	// Search input
	var inputContent string
	if p.query == "" {
		inputContent = placeholderStyle.Render("Filter...")
	} else {
		inputContent = queryStyle.Render(p.query) + placeholderStyle.Render("_")
	}
	content.WriteString(inputBoxStyle.Render(inputContent))
	content.WriteString("\n")

	// Results count
	if p.query != "" {
		content.WriteString(descStyle.Render("  " + itoa(len(p.filtered)) + " results"))
		content.WriteString("\n")
	}

	// Commands list
	if len(p.filtered) == 0 {
		content.WriteString("\n")
		content.WriteString(descStyle.Render("  No matching commands"))
		content.WriteString("\n")
	} else {
		// Calculate visible range
		visibleItems := p.maxHeight - 8
		if visibleItems < 3 {
			visibleItems = 3
		}
		startIdx := p.scroll
		endIdx := min(startIdx+visibleItems, len(p.filtered))

		// Scroll indicator (top)
		if startIdx > 0 {
			content.WriteString(scrollStyle.Render("    ^ " + itoa(startIdx) + " more"))
			content.WriteString("\n")
		}

		// Track last category for headers
		var lastCategory string

		// Check if we're showing recent commands section
		showingRecent := false
		for i := startIdx; i < endIdx; i++ {
			if p.filtered[i].IsRecent && p.query == "" {
				showingRecent = true
				break
			}
		}

		if showingRecent && p.query == "" {
			content.WriteString(categoryStyle.Render("  Recently Used"))
			content.WriteString("\n")
		}

		for i := startIdx; i < endIdx; i++ {
			cmd := p.filtered[i]

			// Category header (only when not searching and not in recent section)
			if p.query == "" && !cmd.IsRecent {
				catName := cmd.Category.Name
				if catName != lastCategory {
					if lastCategory != "" || showingRecent {
						content.WriteString("\n")
					}
					content.WriteString(categoryStyle.Render("  " + catName))
					content.WriteString("\n")
					lastCategory = catName
				}
			}

			// Command line
			var line strings.Builder

			// Shortcut (left aligned, fixed width)
			shortcut := cmd.Shortcut
			if len(shortcut) > 15 {
				shortcut = shortcut[:15]
			}
			if cmd.Enabled {
				line.WriteString(shortcutStyle.Render(padRight(shortcut, 15)))
			} else {
				line.WriteString(shortcutDisabledStyle.Render(padRight(shortcut, 15)))
			}
			line.WriteString(" ")

			// Description (truncate to 60 chars max)
			desc := cmd.Description
			const maxDescLen = 60
			if len(desc) > maxDescLen {
				desc = desc[:maxDescLen] + "..."
			}
			maxDesc := paletteWidth - 25
			if cmd.ArgHint != "" {
				maxDesc -= len(cmd.ArgHint) + 3
			}
			if !cmd.Enabled && cmd.Reason != "" {
				maxDesc -= len(cmd.Reason) + 3
			}
			if maxDesc > 5 && len(desc) > maxDesc {
				desc = desc[:maxDesc-3] + "..."
			}
			line.WriteString(desc)

			// Arg hint or disabled reason
			if cmd.Enabled && cmd.ArgHint != "" {
				line.WriteString(" ")
				line.WriteString(argHintStyle.Render(cmd.ArgHint))
			} else if !cmd.Enabled && cmd.Reason != "" {
				line.WriteString(" ")
				line.WriteString(descStyle.Render("(" + cmd.Reason + ")"))
			}

			// Recent badge
			if cmd.IsRecent && p.query != "" {
				line.WriteString(" ")
				line.WriteString(recentBadgeStyle.Render("*"))
			}

			// Apply selection style
			lineStr := line.String()
			if i == p.selected {
				content.WriteString(selectedBgStyle.Render("> " + lineStr))
			} else if !cmd.Enabled {
				content.WriteString(disabledStyle.Render("  " + lineStr))
			} else {
				content.WriteString(normalStyle.Render("  " + lineStr))
			}
			content.WriteString("\n")
		}

		// Scroll indicator (bottom)
		if endIdx < len(p.filtered) {
			content.WriteString(scrollStyle.Render("    v " + itoa(len(p.filtered)-endIdx) + " more"))
			content.WriteString("\n")
		}
	}

	// Preview panel (if active)
	if p.showPreview && p.previewCmd != nil {
		content.WriteString("\n")
		content.WriteString(p.renderPreview(paletteWidth - 4))
	}

	// Footer
	content.WriteString("\n")
	footerText := "^/v Navigate  Enter Select  Tab Preview  Esc Close"
	content.WriteString(footerStyle.Render(footerText))

	return containerStyle.Render(content.String())
}

// renderPreview renders the preview panel for the selected command.
func (p *CommandPalette) renderPreview(width int) string {
	if p.previewCmd == nil {
		return ""
	}

	cmd := p.previewCmd

	previewStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorDim).
		Padding(0, 1).
		Width(width)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorAccent)

	labelStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)

	var content strings.Builder

	// Title
	content.WriteString(titleStyle.Render(cmd.Shortcut + " - " + cmd.Description))
	content.WriteString("\n\n")

	// Usage
	content.WriteString(labelStyle.Render("Usage:"))
	content.WriteString("\n")
	usage := cmd.Usage
	if usage == "" {
		usage = cmd.Shortcut
	}
	// Split multi-line usage
	for _, line := range strings.Split(usage, "\n") {
		content.WriteString("  " + line + "\n")
	}

	// Category
	content.WriteString("\n")
	content.WriteString(labelStyle.Render("Category: "))
	content.WriteString(cmd.Category.Name)

	// Status
	if !cmd.Enabled {
		content.WriteString("\n")
		content.WriteString(labelStyle.Render("Status: "))
		content.WriteString("Disabled - " + cmd.Reason)
	}

	return previewStyle.Render(content.String())
}

// Legacy compatibility types and functions

// PaletteCommand is kept for backward compatibility.
type PaletteCommand = EnhancedPaletteCommand

// DefaultPaletteCommands returns empty list - commands now come from handler.
func DefaultPaletteCommands() []PaletteCommand {
	return nil
}

// SetCommands is a no-op for backward compatibility.
func (p *CommandPalette) SetCommands(commands []PaletteCommand) {
	// No-op - use RefreshCommands instead
}

// AddCommand is a no-op for backward compatibility.
func (p *CommandPalette) AddCommand(cmd PaletteCommand) {
	// No-op - use RefreshCommands instead
}

// Helper functions
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func padRight(s string, length int) string {
	if len(s) >= length {
		return s
	}
	return s + strings.Repeat(" ", length-len(s))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var result []byte
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		result = append([]byte{byte('0' + n%10)}, result...)
		n /= 10
	}
	if negative {
		result = append([]byte{'-'}, result...)
	}
	return string(result)
}
