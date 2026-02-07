package commands

import (
	"context"
	"fmt"
	"strings"

	"gokin/internal/agent"
	"gokin/internal/chat"
	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/plan"
	"gokin/internal/semantic"
	"gokin/internal/tools"
	"gokin/internal/undo"
)

// TokenStats holds token usage statistics for the session.
type TokenStats struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// Command represents a slash command.
type Command interface {
	Name() string
	Description() string
	Usage() string
	Execute(ctx context.Context, args []string, app AppInterface) (string, error)
}

// ModelSetter allows changing the current model.
type ModelSetter interface {
	GetModel() string
	SetModel(modelName string)
}

// AppInterface defines what commands need from the application.
type AppInterface interface {
	GetSession() *chat.Session
	GetHistoryManager() (*chat.HistoryManager, error)
	GetContextManager() *appcontext.ContextManager
	GetUndoManager() *undo.Manager
	GetWorkDir() string
	ClearConversation()
	GetTodoTool() *tools.TodoTool
	GetConfig() *config.Config
	GetTokenStats() TokenStats
	GetModelSetter() ModelSetter
	GetProjectInfo() *appcontext.ProjectInfo
	GetSemanticIndexer() (*semantic.EnhancedIndexer, error)
	GetPlanManager() *plan.Manager
	GetTreePlanner() *agent.TreePlanner
	IsPlanningModeEnabled() bool
	TogglePlanningMode() bool // Returns new state
	ApplyConfig(cfg *config.Config) error
	GetVersion() string
	AddSystemMessage(msg string)
	GetAgentTypeRegistry() *agent.AgentTypeRegistry
}

// Handler manages slash commands.
type Handler struct {
	commands map[string]Command
}

// NewHandler creates a new command handler with built-in commands.
func NewHandler() *Handler {
	h := &Handler{
		commands: make(map[string]Command),
	}

	// Register built-in commands
	h.Register(&HelpCommand{handler: h})
	h.Register(&ClearCommand{})
	h.Register(&CompactCommand{})
	h.Register(&SaveCommand{})
	h.Register(&ResumeCommand{})
	h.Register(&SessionsCommand{})
	// Register git commands
	h.Register(&CommitCommand{})
	h.Register(&PRCommand{})

	// Register utility commands
	h.Register(&InitCommand{})
	h.Register(&DoctorCommand{})
	h.Register(&ConfigCommand{})
	h.Register(&LoginCommand{})
	h.Register(&LogoutCommand{})
	h.Register(&OAuthLoginCommand{})
	h.Register(&OAuthLogoutCommand{})
	h.Register(&ProviderCommand{})
	h.Register(&StatusCommand{})
	h.Register(&ModelCommand{})
	h.Register(&PermissionsCommand{})
	h.Register(&SandboxCommand{})

	// Register interactive commands
	h.Register(&BrowseCommand{})
	h.Register(&ClearTodosCommand{})

	// Register context commands
	h.Register(&InstructionsCommand{})

	// Register semantic commands
	h.Register(&SemanticStatsCommand{})
	h.Register(&SemanticReindexCommand{})

	// Register onboarding commands
	h.Register(&QuickstartCommand{})

	// Register stats command
	h.Register(&StatsCommand{})

	// Register theme command
	h.Register(&ThemeCommand{})

	// Register planning mode command
	h.Register(&PlanCommand{})
	h.Register(&ResumePlanCommand{})

	// Register tree planner command
	h.Register(&TreeStatsCommand{})

	// Register agent type commands
	h.Register(&RegisterAgentTypeCommand{})
	h.Register(&ListAgentTypesCommand{})
	h.Register(&UnregisterAgentTypeCommand{})

	// Register clipboard commands (cross-platform)
	h.Register(&CopyCommand{})
	h.Register(&PasteCommand{})
	h.Register(&QuickLookCommand{})

	// Register update command
	h.Register(&UpdateCommand{})

	return h
}

// Register adds a command to the handler.
func (h *Handler) Register(cmd Command) {
	h.commands[cmd.Name()] = cmd
}

// Parse checks if input is a slash command and extracts name and args.
// Returns (name, args, isCommand).
// Important: paths like /home/user/... are NOT treated as commands.
func (h *Handler) Parse(input string) (string, []string, bool) {
	input = strings.TrimSpace(input)

	// Must start with /
	if !strings.HasPrefix(input, "/") {
		return "", nil, false
	}

	// Split into parts
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", nil, false
	}

	// Extract command name (without /)
	name := strings.TrimPrefix(parts[0], "/")

	// Check if it's a known command (not a path)
	if _, exists := h.commands[name]; !exists {
		return "", nil, false
	}

	// Return name and args
	var args []string
	if len(parts) > 1 {
		args = parts[1:]
	}

	return name, args, true
}

// Execute runs a command by name.
func (h *Handler) Execute(ctx context.Context, name string, args []string, app AppInterface) (string, error) {
	cmd, exists := h.commands[name]
	if !exists {
		return "", fmt.Errorf("unknown command: /%s", name)
	}

	return cmd.Execute(ctx, args, app)
}

// ListCommands returns all registered commands.
func (h *Handler) ListCommands() []Command {
	cmds := make([]Command, 0, len(h.commands))
	for _, cmd := range h.commands {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// GetCommand returns a command by name.
func (h *Handler) GetCommand(name string) (Command, bool) {
	cmd, exists := h.commands[name]
	return cmd, exists
}

// GetPaletteCommands returns all commands formatted for the palette.
func (h *Handler) GetPaletteCommands(ctx PaletteContext) []PaletteCommand {
	var result []PaletteCommand

	for _, cmd := range h.commands {
		meta := h.getCommandMetadata(cmd)

		// Skip hidden commands
		if meta.Hidden {
			continue
		}

		state := h.GetCommandState(cmd.Name(), ctx)
		catInfo := GetCategoryInfo(meta.Category)

		result = append(result, PaletteCommand{
			Name:        cmd.Name(),
			Description: cmd.Description(),
			Usage:       cmd.Usage(),
			Category:    catInfo,
			Icon:        meta.Icon,
			ArgHint:     meta.ArgHint,
			State:       state,
			Priority:    catInfo.Priority*100 + meta.Priority,
			Advanced:    meta.Advanced,
		})
	}

	return result
}

// GetCommandState returns the enabled/disabled state for a command.
func (h *Handler) GetCommandState(name string, ctx PaletteContext) CommandState {
	cmd, exists := h.commands[name]
	if !exists {
		return DisabledState("Unknown command")
	}

	meta := h.getCommandMetadata(cmd)

	// Platform check
	if meta.Platform != "" && meta.Platform != ctx.Platform {
		return DisabledState(meta.Platform + " only")
	}

	// Git requirement check
	if meta.RequiresGit && !ctx.IsGitRepo {
		return DisabledState("Not a git repo")
	}

	// API key requirement check
	if meta.RequiresAPI && !ctx.HasAPIKey {
		return DisabledState("API key required")
	}

	return EnabledState()
}

// getCommandMetadata retrieves metadata from a command.
func (h *Handler) getCommandMetadata(cmd Command) CommandMetadata {
	if provider, ok := cmd.(MetadataProvider); ok {
		return provider.GetMetadata()
	}
	return DefaultMetadata()
}

// PaletteProviderAdapter wraps a Handler with a context to implement ui.PaletteProvider.
type PaletteProviderAdapter struct {
	handler *Handler
	ctx     PaletteContext
}

// NewPaletteProvider creates a new palette provider adapter.
func NewPaletteProvider(handler *Handler, ctx PaletteContext) *PaletteProviderAdapter {
	return &PaletteProviderAdapter{handler: handler, ctx: ctx}
}

// UpdateContext updates the palette context.
func (p *PaletteProviderAdapter) UpdateContext(ctx PaletteContext) {
	p.ctx = ctx
}

// PaletteCommandForUI represents command info in UI-friendly format.
// It implements ui.PaletteCommandData interface.
type PaletteCommandForUI struct {
	name         string
	description  string
	usage        string
	categoryName string
	categoryIcon string
	categoryPrio int
	icon         string
	argHint      string
	enabled      bool
	reason       string
	priority     int
	advanced     bool
}

// Implement ui.PaletteCommandData interface
func (c *PaletteCommandForUI) GetName() string           { return c.name }
func (c *PaletteCommandForUI) GetDescription() string    { return c.description }
func (c *PaletteCommandForUI) GetUsage() string          { return c.usage }
func (c *PaletteCommandForUI) GetCategoryName() string   { return c.categoryName }
func (c *PaletteCommandForUI) GetCategoryIcon() string   { return c.categoryIcon }
func (c *PaletteCommandForUI) GetCategoryPriority() int  { return c.categoryPrio }
func (c *PaletteCommandForUI) GetIcon() string           { return c.icon }
func (c *PaletteCommandForUI) GetArgHint() string        { return c.argHint }
func (c *PaletteCommandForUI) IsEnabled() bool           { return c.enabled }
func (c *PaletteCommandForUI) GetReason() string         { return c.reason }
func (c *PaletteCommandForUI) GetPriority() int          { return c.priority }
func (c *PaletteCommandForUI) IsAdvanced() bool          { return c.advanced }

// GetPaletteCommandsForUI implements ui.PaletteProvider interface.
// Returns []any where each element implements ui.PaletteCommandData.
func (p *PaletteProviderAdapter) GetPaletteCommandsForUI() []any {
	paletteCmds := p.handler.GetPaletteCommands(p.ctx)
	result := make([]any, 0, len(paletteCmds))

	for _, pc := range paletteCmds {
		result = append(result, &PaletteCommandForUI{
			name:         pc.Name,
			description:  pc.Description,
			usage:        pc.Usage,
			categoryName: pc.Category.Name,
			categoryIcon: pc.Category.Icon,
			categoryPrio: pc.Category.Priority,
			icon:         pc.Icon,
			argHint:      pc.ArgHint,
			enabled:      pc.State.Enabled,
			reason:       pc.State.Reason,
			priority:     pc.Priority,
			advanced:     pc.Advanced,
		})
	}

	return result
}
