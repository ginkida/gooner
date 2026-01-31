package commands

import "runtime"

// CommandCategory represents a category for grouping commands.
type CommandCategory string

const (
	CategoryGettingStarted CommandCategory = "getting_started"
	CategoryAuthentication CommandCategory = "authentication"
	CategoryModelSession   CommandCategory = "model_session"
	CategoryGit            CommandCategory = "git"
	CategoryContext        CommandCategory = "context"
	CategorySemanticSearch CommandCategory = "semantic_search"
	CategoryContracts      CommandCategory = "contracts"
	CategoryPlanning       CommandCategory = "planning"
	CategorySettings       CommandCategory = "settings"
	CategoryInteractive    CommandCategory = "interactive"
	CategoryMacOS          CommandCategory = "macos"
)

// CategoryInfo contains display information for a category.
type CategoryInfo struct {
	ID       CommandCategory
	Name     string
	Icon     string
	Priority int // Lower is higher priority (shown first)
}

// GetCategoryInfo returns display information for a category.
func GetCategoryInfo(cat CommandCategory) CategoryInfo {
	info, ok := categoryInfoMap[cat]
	if !ok {
		return CategoryInfo{ID: cat, Name: string(cat), Icon: "?", Priority: 999}
	}
	return info
}

// GetAllCategories returns all categories in display order.
func GetAllCategories() []CategoryInfo {
	return []CategoryInfo{
		categoryInfoMap[CategoryGettingStarted],
		categoryInfoMap[CategoryAuthentication],
		categoryInfoMap[CategoryModelSession],
		categoryInfoMap[CategoryGit],
		categoryInfoMap[CategoryContext],
		categoryInfoMap[CategorySemanticSearch],
		categoryInfoMap[CategoryContracts],
		categoryInfoMap[CategoryPlanning],
		categoryInfoMap[CategorySettings],
		categoryInfoMap[CategoryInteractive],
		categoryInfoMap[CategoryMacOS],
	}
}

var categoryInfoMap = map[CommandCategory]CategoryInfo{
	CategoryGettingStarted: {ID: CategoryGettingStarted, Name: "Getting Started", Icon: "rocket", Priority: 0},
	CategoryAuthentication: {ID: CategoryAuthentication, Name: "Authentication", Icon: "lock", Priority: 1},
	CategoryModelSession:   {ID: CategoryModelSession, Name: "Model & Session", Icon: "chat", Priority: 2},
	CategoryGit:            {ID: CategoryGit, Name: "Git", Icon: "git", Priority: 3},
	CategoryContext:        {ID: CategoryContext, Name: "Context", Icon: "note", Priority: 4},
	CategorySemanticSearch: {ID: CategorySemanticSearch, Name: "Semantic Search", Icon: "search", Priority: 5},
	CategoryContracts:      {ID: CategoryContracts, Name: "Contracts", Icon: "scroll", Priority: 6},
	CategoryPlanning:       {ID: CategoryPlanning, Name: "Planning", Icon: "tree", Priority: 7},
	CategorySettings:       {ID: CategorySettings, Name: "Settings", Icon: "gear", Priority: 8},
	CategoryInteractive:    {ID: CategoryInteractive, Name: "Interactive", Icon: "screen", Priority: 9},
	CategoryMacOS:          {ID: CategoryMacOS, Name: "macOS Only", Icon: "apple", Priority: 10},
}

// CommandMetadata contains extended information about a command.
type CommandMetadata struct {
	Category    CommandCategory
	Icon        string // Icon key for UI rendering
	Priority    int    // Sort priority within category (lower = higher)
	Platform    string // "darwin", "linux", "windows", or "" for all
	RequiresGit bool   // Requires git repository
	RequiresAPI bool   // Requires API key configured
	HasArgs     bool   // Command accepts arguments
	ArgHint     string // Short hint for arguments (e.g., "[name]", "-m msg")
	Hidden      bool   // Hide from palette (internal commands)
}

// DefaultMetadata returns a default metadata for commands without custom metadata.
func DefaultMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "command",
		Priority: 50,
	}
}

// MetadataProvider is an optional interface for commands to provide metadata.
type MetadataProvider interface {
	GetMetadata() CommandMetadata
}

// PaletteContext provides runtime context for determining command states.
type PaletteContext struct {
	IsGitRepo    bool
	HasAPIKey    bool
	Platform     string
	HasHistory   bool // Has undo history
	SessionCount int  // Number of saved sessions
}

// NewPaletteContext creates a new PaletteContext with detected values.
func NewPaletteContext(workDir string, hasAPIKey bool) PaletteContext {
	return PaletteContext{
		IsGitRepo:  isGitRepoCheck(workDir),
		HasAPIKey:  hasAPIKey,
		Platform:   runtime.GOOS,
		HasHistory: false, // Set by caller
	}
}

// isGitRepoCheck checks if the directory is a git repository.
func isGitRepoCheck(dir string) bool {
	_, err := runGitCommand(dir, "rev-parse", "--git-dir")
	return err == nil
}

// CommandState represents the enabled/disabled state of a command.
type CommandState struct {
	Enabled bool
	Reason  string // Why it's disabled (shown in UI)
}

// EnabledState returns an enabled state.
func EnabledState() CommandState {
	return CommandState{Enabled: true}
}

// DisabledState returns a disabled state with a reason.
func DisabledState(reason string) CommandState {
	return CommandState{Enabled: false, Reason: reason}
}

// PaletteCommand represents a command ready for display in the palette.
type PaletteCommand struct {
	Name        string
	Description string
	Usage       string
	Category    CategoryInfo
	Icon        string
	ArgHint     string
	State       CommandState
	Priority    int
}

// PaletteProvider generates palette commands from the handler.
type PaletteProvider interface {
	GetPaletteCommands(ctx PaletteContext) []PaletteCommand
	GetCommandState(name string, ctx PaletteContext) CommandState
}
