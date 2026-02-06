package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gokin/internal/highlight"
)

// FileBrowserAction represents user actions in the file browser.
type FileBrowserAction int

const (
	FileBrowserActionNone FileBrowserAction = iota
	FileBrowserActionOpen
	FileBrowserActionSelect
	FileBrowserActionClose
)

// FileEntry represents a file or directory entry.
type FileEntry struct {
	Name     string
	Path     string
	IsDir    bool
	Size     int64
	ModTime  string
	Mode     os.FileMode
	IsHidden bool
}

// FileBrowserRequestMsg is sent to open the file browser.
type FileBrowserRequestMsg struct {
	StartPath string
}

// FileBrowserActionMsg is sent when user performs an action.
type FileBrowserActionMsg struct {
	Action FileBrowserAction
	Path   string
	Files  []string // For multi-select
}

// FileBrowserModel is the UI for interactive file browsing.
type FileBrowserModel struct {
	currentDir    string
	entries       []FileEntry
	selectedIndex int
	selectedFiles map[string]bool // For multi-select
	filter        string
	showHidden    bool
	viewport      viewport.Model
	styles        *Styles
	width         int
	height        int

	// Search/filter state
	filterInput  string
	filterActive bool

	// Error message (displayed temporarily)
	errorMsg     string
	errorTimeout int // Countdown ticks to clear error

	// Preview panel
	previewEnabled    bool
	previewContent    string
	previewFilePath   string
	previewViewport   viewport.Model
	previewHighlighter *highlight.Highlighter
	previewMaxLines   int  // Max lines to preview (default: 100)
	previewLoadError  string

	// Split dimensions
	listWidth    int
	previewWidth int

	// Callback for actions
	onAction func(action FileBrowserAction, path string, files []string)
}

// NewFileBrowserModel creates a new file browser model.
func NewFileBrowserModel(styles *Styles) FileBrowserModel {
	vp := viewport.New(60, 20)
	vp.MouseWheelEnabled = true

	previewVp := viewport.New(60, 20)
	previewVp.MouseWheelEnabled = true

	return FileBrowserModel{
		viewport:           vp,
		styles:             styles,
		selectedFiles:      make(map[string]bool),
		showHidden:         false,
		previewEnabled:     false,
		previewViewport:    previewVp,
		previewHighlighter: highlight.New("monokai"),
		previewMaxLines:    100,
	}
}

// SetSize sets the size of the file browser.
func (m *FileBrowserModel) SetSize(width, height int) {
	if width < 10 {
		width = 80
	}
	if height < 10 {
		height = 24
	}
	m.width = width
	m.height = height

	// Calculate split widths
	if m.previewEnabled {
		m.listWidth = width * 40 / 100
		m.previewWidth = width - m.listWidth - 5
		m.viewport.Width = m.listWidth - 4
		m.previewViewport.Width = m.previewWidth - 4
	} else {
		m.viewport.Width = width - 4
	}
	m.viewport.Height = height - 10
	m.previewViewport.Height = height - 10
}

// SetPath sets the current directory path.
func (m *FileBrowserModel) SetPath(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		absPath = filepath.Dir(absPath)
	}

	m.currentDir = absPath
	m.selectedIndex = 0
	m.selectedFiles = make(map[string]bool)

	return m.loadEntries()
}

// loadEntries loads the directory entries.
func (m *FileBrowserModel) loadEntries() error {
	entries, err := os.ReadDir(m.currentDir)
	if err != nil {
		return err
	}

	m.entries = make([]FileEntry, 0, len(entries)+1)

	// Add parent directory if not at root
	if m.currentDir != "/" {
		m.entries = append(m.entries, FileEntry{
			Name:  "..",
			Path:  filepath.Dir(m.currentDir),
			IsDir: true,
		})
	}

	for _, entry := range entries {
		name := entry.Name()
		isHidden := strings.HasPrefix(name, ".")

		if !m.showHidden && isHidden {
			continue
		}

		// Apply filter
		if m.filter != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(m.filter)) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		fe := FileEntry{
			Name:     name,
			Path:     filepath.Join(m.currentDir, name),
			IsDir:    entry.IsDir(),
			Size:     info.Size(),
			ModTime:  info.ModTime().Format("Jan 02 15:04"),
			Mode:     info.Mode(),
			IsHidden: isHidden,
		}
		m.entries = append(m.entries, fe)
	}

	// Sort: directories first, then alphabetically
	sort.Slice(m.entries, func(i, j int) bool {
		if m.entries[i].Name == ".." {
			return true
		}
		if m.entries[j].Name == ".." {
			return false
		}
		if m.entries[i].IsDir != m.entries[j].IsDir {
			return m.entries[i].IsDir
		}
		return strings.ToLower(m.entries[i].Name) < strings.ToLower(m.entries[j].Name)
	})

	m.updateViewport()
	return nil
}

// SetActionCallback sets the callback for user actions.
func (m *FileBrowserModel) SetActionCallback(callback func(FileBrowserAction, string, []string)) {
	m.onAction = callback
}

// loadPreview loads and highlights file content for preview.
func (m *FileBrowserModel) loadPreview(entry FileEntry) {
	m.previewLoadError = ""
	m.previewContent = ""
	m.previewFilePath = ""

	if entry.IsDir || entry.Name == ".." {
		m.previewContent = ""
		return
	}

	// Size check (max 1MB)
	const maxPreviewSize = 1024 * 1024
	if entry.Size > maxPreviewSize {
		m.previewLoadError = "File too large for preview"
		return
	}

	content, err := os.ReadFile(entry.Path)
	if err != nil {
		m.previewLoadError = fmt.Sprintf("Cannot read: %s", err.Error())
		return
	}

	// Binary check
	if isBinaryContent(content) {
		m.previewLoadError = "Binary file"
		return
	}

	// Truncate to maxLines
	lines := strings.Split(string(content), "\n")
	if len(lines) > m.previewMaxLines {
		lines = lines[:m.previewMaxLines]
		lines = append(lines, fmt.Sprintf("... (%d more lines)", len(strings.Split(string(content), "\n"))-m.previewMaxLines))
	}

	// Detect language and highlight
	lang := m.previewHighlighter.DetectLanguage(entry.Name)
	m.previewContent = m.previewHighlighter.HighlightWithLineNumbers(
		strings.Join(lines, "\n"), lang, 1,
	)
	m.previewFilePath = entry.Path
	m.previewViewport.SetContent(m.previewContent)
	m.previewViewport.GotoTop()
}

// isBinaryContent checks if content is binary by looking for null bytes.
func isBinaryContent(content []byte) bool {
	checkLen := 512
	if len(content) < checkLen {
		checkLen = len(content)
	}
	for i := 0; i < checkLen; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
}

// updateViewport updates the viewport content.
func (m *FileBrowserModel) updateViewport() {
	var content strings.Builder

	for i, entry := range m.entries {
		line := m.formatEntryLine(i, entry)
		content.WriteString(line)
		content.WriteString("\n")
	}

	m.viewport.SetContent(content.String())
}

// formatEntryLine formats a single file entry for display.
func (m *FileBrowserModel) formatEntryLine(index int, entry FileEntry) string {
	isSelected := index == m.selectedIndex
	isMultiSelected := m.selectedFiles[entry.Path]

	// Styles
	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorSecondary).
		Background(lipgloss.Color("#1F2937"))
	normalStyle := lipgloss.NewStyle().
		Foreground(ColorText)
	dirStyle := lipgloss.NewStyle().
		Foreground(ColorAccent).
		Bold(true)
	fileStyle := lipgloss.NewStyle().
		Foreground(ColorText)
	sizeStyle := lipgloss.NewStyle().
		Foreground(ColorMuted)
	timeStyle := lipgloss.NewStyle().
		Foreground(ColorDim)
	hiddenStyle := lipgloss.NewStyle().
		Foreground(ColorDim).
		Italic(true)

	// Selection indicator
	prefix := "  "
	if isSelected {
		prefix = "> "
	}
	if isMultiSelected {
		prefix = "* "
	}

	// Icon
	var icon string
	var nameStyle lipgloss.Style
	if entry.IsDir {
		icon = "▸"
		nameStyle = dirStyle
	} else {
		icon = "·"
		nameStyle = fileStyle
	}

	if entry.IsHidden {
		nameStyle = hiddenStyle
	}

	// Format line
	name := entry.Name
	if len(name) > 30 {
		name = name[:27] + "..."
	}

	var line string
	if entry.Name == ".." {
		line = fmt.Sprintf("%s%s %s", prefix, icon, nameStyle.Render(name))
	} else if entry.IsDir {
		line = fmt.Sprintf("%s%s %-32s", prefix, icon, nameStyle.Render(name))
	} else {
		sizeStr := m.formatSize(entry.Size)
		line = fmt.Sprintf("%s%s %-32s %8s  %s",
			prefix,
			icon,
			nameStyle.Render(name),
			sizeStyle.Render(sizeStr),
			timeStyle.Render(entry.ModTime),
		)
	}

	if isSelected {
		return selectedStyle.Render(line)
	}
	return normalStyle.Render(line)
}

// getFileIcon returns a simple Unicode glyph for a file type.
func (m *FileBrowserModel) getFileIcon(name string) string {
	ext := strings.ToLower(filepath.Ext(name))

	iconMap := map[string]string{
		".go":    "◆",
		".py":    "◇",
		".js":    "△",
		".ts":    "△",
		".tsx":   "△",
		".jsx":   "△",
		".rs":    "▪",
		".rb":    "◇",
		".java":  "◇",
		".c":     "▪",
		".cpp":   "▪",
		".h":     "▪",
		".cs":    "◇",
		".php":   "◇",
		".swift": "◇",
		".kt":    "◇",
		".md":    "·",
		".json":  "·",
		".yaml":  "·",
		".yml":   "·",
		".toml":  "·",
		".html":  "△",
		".css":   "△",
		".scss":  "△",
		".sql":   "·",
		".sh":    "$",
		".bash":  "$",
		".zsh":   "$",
		".txt":   "·",
		".log":   "·",
		".pdf":   "·",
		".png":   "·",
		".jpg":   "·",
		".jpeg":  "·",
		".gif":   "·",
		".svg":   "·",
		".zip":   "·",
		".tar":   "·",
		".gz":    "·",
		".lock":  "·",
	}

	if icon, ok := iconMap[ext]; ok {
		return icon
	}

	return "·"
}

// formatSize formats a file size for display.
func (m *FileBrowserModel) formatSize(size int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case size >= GB:
		return fmt.Sprintf("%.1fG", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.1fM", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.1fK", float64(size)/KB)
	default:
		return fmt.Sprintf("%dB", size)
	}
}

// Init initializes the file browser model.
func (m FileBrowserModel) Init() tea.Cmd {
	return nil
}

// Update handles input events for the file browser.
func (m FileBrowserModel) Update(msg tea.Msg) (FileBrowserModel, tea.Cmd) {
	var cmd tea.Cmd

	// Clear error message on any key press (user acknowledgment)
	if _, ok := msg.(tea.KeyMsg); ok && m.errorMsg != "" {
		m.errorMsg = ""
		m.errorTimeout = 0
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// If filter is active, handle text input
		if m.filterActive {
			switch msg.Type {
			case tea.KeyEnter, tea.KeyEsc:
				m.filterActive = false
				return m, nil
			case tea.KeyBackspace:
				if len(m.filterInput) > 0 {
					m.filterInput = m.filterInput[:len(m.filterInput)-1]
					m.filter = m.filterInput
					m.loadEntries()
				}
				return m, nil
			default:
				if msg.Type == tea.KeyRunes {
					m.filterInput += string(msg.Runes)
					m.filter = m.filterInput
					m.loadEntries()
				}
				return m, nil
			}
		}

		switch msg.String() {
		case "j", "down":
			if m.selectedIndex < len(m.entries)-1 {
				m.selectedIndex++
				m.updateViewport()
				// Update preview if enabled
				if m.previewEnabled && m.selectedIndex < len(m.entries) {
					m.loadPreview(m.entries[m.selectedIndex])
				}
			}

		case "k", "up":
			if m.selectedIndex > 0 {
				m.selectedIndex--
				m.updateViewport()
				// Update preview if enabled
				if m.previewEnabled && m.selectedIndex < len(m.entries) {
					m.loadPreview(m.entries[m.selectedIndex])
				}
			}

		case "l", "enter", "right":
			if len(m.entries) > 0 && m.selectedIndex < len(m.entries) {
				entry := m.entries[m.selectedIndex]
				if entry.IsDir {
					if err := m.SetPath(entry.Path); err != nil {
						// Show error message instead of silently failing
						m.errorMsg = fmt.Sprintf("Cannot access: %s", err.Error())
						m.errorTimeout = 30 // Clear after ~3 seconds (10 ticks/sec)
						return m, nil
					}
				} else {
					if m.onAction != nil {
						m.onAction(FileBrowserActionOpen, entry.Path, nil)
					}
					return m, func() tea.Msg {
						return FileBrowserActionMsg{
							Action: FileBrowserActionOpen,
							Path:   entry.Path,
						}
					}
				}
			}

		case "h", "backspace", "left":
			// Go to parent directory - show error if navigation fails
			if m.currentDir != "/" {
				if err := m.SetPath(filepath.Dir(m.currentDir)); err != nil {
					m.errorMsg = fmt.Sprintf("Cannot access parent: %s", err.Error())
					m.errorTimeout = 30
					return m, nil
				}
			}

		case " ":
			// Toggle selection
			if len(m.entries) > 0 && m.selectedIndex < len(m.entries) {
				entry := m.entries[m.selectedIndex]
				if entry.Name != ".." {
					if m.selectedFiles[entry.Path] {
						delete(m.selectedFiles, entry.Path)
					} else {
						m.selectedFiles[entry.Path] = true
					}
					m.updateViewport()
				}
			}

		case "/":
			// Start filter
			m.filterActive = true
			m.filterInput = ""

		case ".":
			// Toggle hidden files
			m.showHidden = !m.showHidden
			m.loadEntries()

		case "p":
			// Toggle preview panel
			m.previewEnabled = !m.previewEnabled
			if m.previewEnabled && len(m.entries) > 0 && m.selectedIndex < len(m.entries) {
				m.loadPreview(m.entries[m.selectedIndex])
			}

		case "ctrl+j":
			// Scroll preview down
			if m.previewEnabled {
				m.previewViewport.LineDown(3)
			}

		case "ctrl+k":
			// Scroll preview up
			if m.previewEnabled {
				m.previewViewport.LineUp(3)
			}

		case "g":
			m.selectedIndex = 0
			m.updateViewport()
			if m.previewEnabled && len(m.entries) > 0 {
				m.loadPreview(m.entries[0])
			}

		case "G":
			if len(m.entries) > 0 {
				m.selectedIndex = len(m.entries) - 1
				m.updateViewport()
				if m.previewEnabled {
					m.loadPreview(m.entries[m.selectedIndex])
				}
			}

		case "~":
			// Go to home directory
			home, err := os.UserHomeDir()
			if err == nil {
				if err := m.SetPath(home); err != nil {
					// Stay in current directory on error
					return m, nil
				}
			}

		case "y":
			// Confirm selection
			if len(m.selectedFiles) > 0 {
				var files []string
				for path := range m.selectedFiles {
					files = append(files, path)
				}
				if m.onAction != nil {
					m.onAction(FileBrowserActionSelect, "", files)
				}
				return m, func() tea.Msg {
					return FileBrowserActionMsg{
						Action: FileBrowserActionSelect,
						Files:  files,
					}
				}
			}

		case "q", "esc":
			if m.onAction != nil {
				m.onAction(FileBrowserActionClose, "", nil)
			}
			return m, func() tea.Msg {
				return FileBrowserActionMsg{Action: FileBrowserActionClose}
			}

		case "c":
			// Clear filter
			m.filter = ""
			m.filterInput = ""
			m.loadEntries()
		}

	case tea.MouseMsg:
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	}

	return m, nil
}

// View renders the file browser.
func (m FileBrowserModel) View() string {
	var builder strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorHighlight).
		Padding(0, 1)

	builder.WriteString(headerStyle.Render("Files"))
	builder.WriteString("\n\n")

	// Current path
	pathStyle := lipgloss.NewStyle().
		Foreground(ColorAccent)
	builder.WriteString(fmt.Sprintf("  %s\n", pathStyle.Render(m.currentDir)))

	// Selection count
	if len(m.selectedFiles) > 0 {
		selectStyle := lipgloss.NewStyle().
			Foreground(ColorSuccess)
		builder.WriteString(selectStyle.Render(fmt.Sprintf("  %d selected\n", len(m.selectedFiles))))
	}

	// Filter indicator
	if m.filter != "" || m.filterActive {
		filterStyle := lipgloss.NewStyle().
			Foreground(ColorWarning)
		filterText := m.filter
		if m.filterActive {
			filterText += "▊"
		}
		builder.WriteString(filterStyle.Render(fmt.Sprintf("  Filter: %s\n", filterText)))
	}

	// Error message (if any)
	if m.errorMsg != "" {
		errorStyle := lipgloss.NewStyle().
			Foreground(ColorError).
			Bold(true)
		builder.WriteString(errorStyle.Render(fmt.Sprintf("  ⚠ %s\n", m.errorMsg)))
	}

	builder.WriteString("\n")

	// Content area - split view or single view
	if m.previewEnabled {
		builder.WriteString(m.renderSplitView())
	} else {
		borderStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(0, 1)
		builder.WriteString(borderStyle.Render(m.viewport.View()))
	}
	builder.WriteString("\n\n")

	// Footer with actions
	m.renderActions(&builder)

	return builder.String()
}

// renderSplitView renders the file browser with a preview panel.
func (m FileBrowserModel) renderSplitView() string {
	// Calculate widths: 40% list, 60% preview
	listWidth := m.width * 40 / 100
	if listWidth < 30 {
		listWidth = 30
	}
	previewWidth := m.width - listWidth - 5 // Account for separator and borders
	if previewWidth < 30 {
		previewWidth = 30
	}

	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Padding(0, 1)

	// Left panel: file list
	leftPanel := borderStyle.Width(listWidth).Render(m.viewport.View())

	// Right panel: preview
	var previewContent string
	if m.previewLoadError != "" {
		errorStyle := lipgloss.NewStyle().
			Foreground(ColorDim).
			Italic(true)
		previewContent = errorStyle.Render(m.previewLoadError)
	} else if m.previewContent != "" {
		previewContent = m.previewViewport.View()
	} else {
		dimStyle := lipgloss.NewStyle().
			Foreground(ColorDim).
			Italic(true)
		previewContent = dimStyle.Render("Select a file to preview")
	}

	// Preview header
	previewHeader := ""
	if m.previewFilePath != "" {
		headerStyle := lipgloss.NewStyle().
			Foreground(ColorHighlight).
			Bold(true)
		fileName := filepath.Base(m.previewFilePath)
		previewHeader = headerStyle.Render(fileName) + "\n" + strings.Repeat("─", previewWidth-2) + "\n"
	}

	rightPanel := borderStyle.Width(previewWidth).Render(previewHeader + previewContent)

	// Join horizontally
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, " ", rightPanel)
}

// renderActions renders the available actions.
func (m *FileBrowserModel) renderActions(builder *strings.Builder) {
	hintStyle := lipgloss.NewStyle().Foreground(ColorDim)
	keyStyle := lipgloss.NewStyle().
		Foreground(ColorSecondary).
		Bold(true)

	hints := []string{
		keyStyle.Render("Enter") + " Open",
		keyStyle.Render("Space") + " Select",
		keyStyle.Render("p") + " Preview",
		keyStyle.Render("/") + " Filter",
		keyStyle.Render(".") + " Hidden",
		keyStyle.Render("q") + " Close",
	}

	builder.WriteString(hintStyle.Render(strings.Join(hints, "  │  ")))
	builder.WriteString("\n")
	if m.previewEnabled {
		builder.WriteString(hintStyle.Render("h/l: Navigate  │  j/k: Move  │  Ctrl+j/k: Scroll preview  │  y: Confirm"))
	} else {
		builder.WriteString(hintStyle.Render("h/l: Navigate  │  j/k: Move  │  ~: Home  │  y: Confirm selection"))
	}
}

// GetCurrentPath returns the current directory path.
func (m FileBrowserModel) GetCurrentPath() string {
	return m.currentDir
}

// GetSelectedFiles returns the selected files.
func (m FileBrowserModel) GetSelectedFiles() []string {
	var files []string
	for path := range m.selectedFiles {
		files = append(files, path)
	}
	return files
}
