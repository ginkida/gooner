package commands

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/atotto/clipboard"
)

// CopyCommand copies text to the system clipboard.
// Works on macOS, Linux (X11/Wayland), Windows, and WSL.
type CopyCommand struct{}

func (c *CopyCommand) Name() string        { return "copy" }
func (c *CopyCommand) Description() string { return "Copy text or last response to clipboard" }
func (c *CopyCommand) Usage() string       { return "/copy [--last|--all|--ascii] [<text>]" }
func (c *CopyCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryInteractive,
		Icon:     "copy",
		Priority: 0,
		HasArgs:  true,
		ArgHint:  "[--last|<text>]",
	}
}

func (c *CopyCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	// Check clipboard availability
	if !isClipboardAvailable() {
		return "Clipboard not available. Install xclip, xsel, or wl-copy on Linux.", nil
	}

	var text string
	normalizeASCII := false

	if len(args) == 0 {
		return "Usage:\n  /copy <text>       - Copy specified text\n  /copy --last       - Copy last AI response\n  /copy --all        - Copy full conversation\n  /copy --ascii ...  - Normalize Unicode to ASCII for compatibility", nil
	}

	// Check for --ascii flag
	filteredArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--ascii" || arg == "-A" {
			normalizeASCII = true
		} else {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	args = filteredArgs

	if len(args) == 0 {
		return "No text specified. Use --last or --all, or provide text.", nil
	}

	// Handle special flags
	switch args[0] {
	case "--last", "-l":
		// Copy last AI response
		session := app.GetSession()
		if session == nil {
			return "No session available", nil
		}
		history := session.GetHistory()
		// Find last model response
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == "model" {
				for _, part := range history[i].Parts {
					if part.Text != "" {
						text = part.Text
						break
					}
				}
				break
			}
		}
		if text == "" {
			return "No AI response to copy", nil
		}

	case "--all", "-a":
		// Copy full conversation
		session := app.GetSession()
		if session == nil {
			return "No session available", nil
		}
		history := session.GetHistory()
		var sb strings.Builder
		for _, content := range history {
			role := "User"
			if content.Role == "model" {
				role = "Assistant"
			}
			for _, part := range content.Parts {
				if part.Text != "" {
					sb.WriteString(fmt.Sprintf("## %s\n\n%s\n\n", role, part.Text))
				}
			}
		}
		text = sb.String()
		if text == "" {
			return "No conversation to copy", nil
		}

	default:
		// Copy provided text
		text = strings.Join(args, " ")
	}

	// Normalize Unicode to ASCII if requested
	if normalizeASCII {
		text = normalizeUnicodeToASCII(text)
	}

	if err := copyToClipboard(text); err != nil {
		return fmt.Sprintf("Failed to copy: %v", err), nil
	}

	suffix := ""
	if normalizeASCII {
		suffix = " (ASCII normalized)"
	}
	return fmt.Sprintf("Copied to clipboard (%d chars)%s", len(text), suffix), nil
}

// PasteCommand gets text from the system clipboard.
type PasteCommand struct{}

func (c *PasteCommand) Name() string        { return "paste" }
func (c *PasteCommand) Description() string { return "Get text from clipboard" }
func (c *PasteCommand) Usage() string       { return "/paste" }
func (c *PasteCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryInteractive,
		Icon:     "paste",
		Priority: 10,
	}
}

func (c *PasteCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if !isClipboardAvailable() {
		return "Clipboard not available. Install xclip, xsel, or wl-copy on Linux.", nil
	}

	text, err := pasteFromClipboard()
	if err != nil {
		return fmt.Sprintf("Failed to paste: %v", err), nil
	}

	if text == "" {
		return "(clipboard is empty)", nil
	}

	return text, nil
}

// QuickLookCommand opens a file using macOS Quick Look.
type QuickLookCommand struct{}

func (c *QuickLookCommand) Name() string        { return "ql" }
func (c *QuickLookCommand) Description() string { return "Preview a file using macOS Quick Look" }
func (c *QuickLookCommand) Usage() string       { return "/ql <file>" }
func (c *QuickLookCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryMacOS,
		Icon:     "preview",
		Priority: 20,
		Platform: "darwin",
		HasArgs:  true,
		ArgHint:  "<file>",
	}
}

func (c *QuickLookCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	if runtime.GOOS != "darwin" {
		return "The /ql command is only supported on macOS.", nil
	}

	if len(args) == 0 {
		return "Usage: /ql <file>", nil
	}

	filePath := args[0]
	cmd := exec.Command("qlmanage", "-p", filePath)
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("Failed to start Quick Look: %v", err), nil
	}

	return fmt.Sprintf("Opening Quick Look preview for %s", filePath), nil
}

// Clipboard helper functions

// normalizeUnicodeToASCII converts Unicode box-drawing and special characters to ASCII
// for better copy-paste compatibility across different systems and terminals.
func normalizeUnicodeToASCII(text string) string {
	replacements := map[string]string{
		// Box drawing characters
		"├": "|",
		"└": "`",
		"─": "-",
		"│": "|",
		"┌": "+",
		"┐": "+",
		"┘": "+",
		"┴": "+",
		"┬": "+",
		"┼": "+",
		"╭": "+",
		"╮": "+",
		"╯": "+",
		"╰": "+",
		// Bullets and symbols
		"•": "*",
		"►": ">",
		"◄": "<",
		"▸": ">",
		"▹": ">",
		"✓": "[x]",
		"✗": "[ ]",
		"★": "*",
		"☆": "*",
		"→": "->",
		"←": "<-",
		"↑": "^",
		"↓": "v",
		// Quotes
		"\u201c": `"`, // "
		"\u201d": `"`, // "
		"\u2018": "'", // '
		"\u2019": "'", // '
		// Dashes
		"—": "--",
		"–": "-",
		// Ellipsis
		"…": "...",
	}

	result := text
	for unicode, ascii := range replacements {
		result = strings.ReplaceAll(result, unicode, ascii)
	}
	return result
}

func isClipboardAvailable() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("pbcopy")
		return err == nil
	case "linux":
		// Check for WSL
		if isWSL() {
			_, err := exec.LookPath("clip.exe")
			return err == nil
		}
		// Check for X11/Wayland tools
		if _, err := exec.LookPath("xclip"); err == nil {
			return true
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			return true
		}
		if _, err := exec.LookPath("wl-copy"); err == nil {
			return true
		}
		return false
	case "windows":
		return true
	}
	return false
}

func isWSL() bool {
	if _, err := exec.LookPath("clip.exe"); err == nil {
		return true
	}
	data, err := exec.Command("uname", "-r").Output()
	if err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft") {
		return true
	}
	return false
}

func copyToClipboard(text string) error {
	// Try atotto/clipboard first
	if err := clipboard.WriteAll(text); err == nil {
		return nil
	}

	// Fallbacks
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	case "linux":
		if isWSL() {
			// Use PowerShell Set-Clipboard for proper UTF-8/Unicode support
			// clip.exe has encoding issues with non-ASCII characters
			// Read from stdin with proper encoding
			cmd := exec.Command("powershell.exe", "-command", "$input | Set-Clipboard")
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
		// Try xclip
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.Command("xclip", "-selection", "clipboard")
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
		// Try xsel
		if _, err := exec.LookPath("xsel"); err == nil {
			cmd := exec.Command("xsel", "--clipboard", "--input")
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
		// Try wl-copy
		cmd := exec.Command("wl-copy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard method available")
}

func pasteFromClipboard() (string, error) {
	// Try atotto/clipboard first
	if text, err := clipboard.ReadAll(); err == nil {
		return text, nil
	}

	// Fallbacks
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("pbpaste").Output()
		return string(out), err
	case "linux":
		if isWSL() {
			out, err := exec.Command("powershell.exe", "-command", "Get-Clipboard").Output()
			if err != nil {
				return "", err
			}
			return strings.TrimRight(string(out), "\r\n"), nil
		}
		// Try xclip
		if _, err := exec.LookPath("xclip"); err == nil {
			out, err := exec.Command("xclip", "-selection", "clipboard", "-o").Output()
			return string(out), err
		}
		// Try xsel
		if _, err := exec.LookPath("xsel"); err == nil {
			out, err := exec.Command("xsel", "--clipboard", "--output").Output()
			return string(out), err
		}
		// Try wl-paste
		out, err := exec.Command("wl-paste").Output()
		return string(out), err
	}
	return "", fmt.Errorf("no clipboard method available")
}
