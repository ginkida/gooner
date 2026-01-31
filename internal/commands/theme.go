package commands

import (
	"context"
	"fmt"
	"strings"

	"gokin/internal/ui"
)

// ThemeCommand switches the UI theme.
type ThemeCommand struct{}

func (c *ThemeCommand) Name() string        { return "theme" }
func (c *ThemeCommand) Description() string { return "Change UI color theme" }
func (c *ThemeCommand) Usage() string       { return "/theme [theme-name]" }
func (c *ThemeCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "theme",
		Priority: 40,
		HasArgs:  true,
		ArgHint:  "[theme]",
	}
}

func (c *ThemeCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	// Get theme setter interface
	themeSetter, ok := app.(ThemeSetter)
	if !ok {
		return "Theme switching not available in this context.", nil
	}

	currentTheme := themeSetter.GetTheme()

	// No args - show current theme and available themes
	if len(args) == 0 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Current theme: %s\n\n", currentTheme))

		availableThemes := ui.GetAvailableThemes()
		sb.WriteString("Available themes:\n")
		for _, themeInfo := range availableThemes {
			marker := "  "
			if string(themeInfo.ID) == currentTheme {
				marker = "> "
			}
			sb.WriteString(fmt.Sprintf("%s%-12s  %s\n", marker, string(themeInfo.ID), getThemeDescription(themeInfo.ID)))
		}
		sb.WriteString("\nUsage: /theme dark  or  /theme dracula")
		sb.WriteString("\n       /theme cyber --save  (save to config)")
		return sb.String(), nil
	}

	// Parse arguments
	newTheme := strings.ToLower(args[0])
	saveToConfig := false

	// Check for --save flag
	if len(args) > 1 && args[1] == "--save" {
		saveToConfig = true
	} else if len(args) > 1 && args[0] == "--save" {
		// Handle: /theme --save cyber
		if len(args) > 2 {
			newTheme = strings.ToLower(args[2])
		}
		saveToConfig = true
	}

	// Check if theme is valid
	availableThemes := ui.GetAvailableThemes()
	var matchedTheme ui.ThemeType
	found := false
	for _, themeInfo := range availableThemes {
		if strings.Contains(string(themeInfo.ID), newTheme) {
			matchedTheme = themeInfo.ID
			found = true
			if string(themeInfo.ID) == newTheme {
				// Exact match, stop searching
				break
			}
		}
	}

	if !found {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Unknown theme: %s\n\n", newTheme))
		sb.WriteString("Available themes:\n")
		for _, themeInfo := range availableThemes {
			sb.WriteString(fmt.Sprintf("  %-12s  %s\n", string(themeInfo.ID), getThemeDescription(themeInfo.ID)))
		}
		return sb.String(), nil
	}

	if string(matchedTheme) == currentTheme && !saveToConfig {
		return fmt.Sprintf("Already using %s theme", currentTheme), nil
	}

	// Apply theme
	themeSetter.SetTheme(matchedTheme)

	var result strings.Builder
	result.WriteString(fmt.Sprintf("✓ Theme changed to %s", matchedTheme))

	// Save to config if requested
	if saveToConfig {
		configSetter, ok := app.(ConfigSetter)
		if ok {
			if err := configSetter.SetConfigValue("ui.theme", string(matchedTheme)); err != nil {
				result.WriteString(fmt.Sprintf("\n⚠ Failed to save to config: %v", err))
			} else {
				result.WriteString(fmt.Sprintf("\n✓ Theme saved to config file"))
			}
		} else {
			result.WriteString("\n⚠ Config saving not available")
		}
	}

	return result.String(), nil
}

// getThemeDescription returns a human-readable description for a theme.
func getThemeDescription(theme ui.ThemeType) string {
	descriptions := map[ui.ThemeType]string{
		ui.ThemeDark: "Default soft purple-blue theme",
	}

	if desc, ok := descriptions[theme]; ok {
		return desc
	}
	return "Custom theme"
}

// ThemeSetter defines the interface for changing themes.
type ThemeSetter interface {
	GetTheme() string
	SetTheme(theme ui.ThemeType)
}

// ConfigSetter defines the interface for saving config changes.
type ConfigSetter interface {
	SetConfigValue(key, value string) error
}
