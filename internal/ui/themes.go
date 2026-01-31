package ui

import (
	"github.com/charmbracelet/lipgloss"
)

// ThemeType represents different UI themes.
type ThemeType string

const (
	ThemeDark  ThemeType = "dark"  // Default dark theme (soft purple/cyan)
	ThemeMacOS ThemeType = "macos" // Apple-inspired theme
)

// ThemeColorScheme defines the color palette for a theme.
type ThemeColorScheme struct {
	Name       string
	Primary    lipgloss.Color
	Secondary  lipgloss.Color
	Success    lipgloss.Color
	Warning    lipgloss.Color
	Error      lipgloss.Color
	Muted      lipgloss.Color
	Text       lipgloss.Color
	Background lipgloss.Color
	Border     lipgloss.Color
	Highlight  lipgloss.Color
	Accent     lipgloss.Color
	Info       lipgloss.Color
}

// predefinedThemes returns all available theme color schemes.
func predefinedThemes() map[ThemeType]ThemeColorScheme {
	return map[ThemeType]ThemeColorScheme{
		ThemeDark: {
			Name:       "Dark (Default)",
			Primary:    lipgloss.Color("#A78BFA"), // Soft Purple
			Secondary:  lipgloss.Color("#22D3EE"), // Bright Cyan
			Success:    lipgloss.Color("#34D399"), // Soft Green
			Warning:    lipgloss.Color("#FBBF24"), // Warm Amber
			Error:      lipgloss.Color("#F87171"), // Soft Red
			Muted:      lipgloss.Color("#9CA3AF"), // Neutral Gray
			Text:       lipgloss.Color("#F1F5F9"), // Soft White
			Background: lipgloss.Color("#0F172A"), // Deep Navy
			Border:     lipgloss.Color("#1E293B"), // Subtle Slate
			Highlight:  lipgloss.Color("#E9D5FF"), // Soft Purple
			Accent:     lipgloss.Color("#F472B6"), // Pink Accent
			Info:       lipgloss.Color("#2DD4BF"), // Teal
		},
		ThemeMacOS: {
			Name:       "Apple (MacOS)",
			Primary:    lipgloss.Color("#007AFF"), // SF Blue
			Secondary:  lipgloss.Color("#5856D6"), // SF Purple
			Success:    lipgloss.Color("#34C759"), // SF Green
			Warning:    lipgloss.Color("#FF9500"), // SF Orange
			Error:      lipgloss.Color("#FF3B30"), // SF Red
			Muted:      lipgloss.Color("#8E8E93"), // SF Gray
			Text:       lipgloss.Color("#FFFFFF"), // White
			Background: lipgloss.Color("#1C1C1E"), // Dark Mode Gray
			Border:     lipgloss.Color("#3A3A3C"), // Separator Gray
			Highlight:  lipgloss.Color("#64D2FF"), // SF Sky
			Accent:     lipgloss.Color("#FF2D55"), // SF Pink
			Info:       lipgloss.Color("#00C7BE"), // SF Mint
		},
	}
}

// GetTheme returns the color scheme for a given theme type.
func GetTheme(themeType ThemeType) ThemeColorScheme {
	themes := predefinedThemes()
	if theme, ok := themes[themeType]; ok {
		return theme
	}
	return themes[ThemeDark] // Default to dark theme
}

// ApplyTheme applies a theme to the Styles struct.
func (s *Styles) ApplyTheme(theme ThemeType) {
	colors := GetTheme(theme)

	// Update all color constants
	ColorPrimary = colors.Primary
	ColorSecondary = colors.Secondary
	ColorSuccess = colors.Success
	ColorWarning = colors.Warning
	ColorError = colors.Error
	ColorMuted = colors.Muted
	ColorText = colors.Text
	ColorBg = colors.Background
	ColorBorder = colors.Border
	ColorHighlight = colors.Highlight
	ColorAccent = colors.Accent
	ColorInfo = colors.Info

	// Rebuild styles with new colors
	s.rebuildStyles()
}

// rebuildStyles rebuilds all styles with the current color constants.
func (s *Styles) rebuildStyles() {
	*s = *DefaultStyles()
}

// GetAvailableThemes returns a list of all available theme names and their IDs.
func GetAvailableThemes() []struct {
	ID   ThemeType
	Name string
} {
	themes := predefinedThemes()
	var result []struct {
		ID   ThemeType
		Name string
	}

	for id, theme := range themes {
		result = append(result, struct {
			ID   ThemeType
			Name string
		}{
			ID:   id,
			Name: theme.Name,
		})
	}

	return result
}
