package ui

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	// slowOperationThreshold is the time after which we show a "slow operation" warning
	slowOperationThreshold = 3 * time.Second
)

// Model represents the main TUI model.
type Model struct {
	input   InputModel
	output  OutputModel
	spinner spinner.Model
	styles  *Styles

	state           State
	width           int
	height          int
	currentTool     string
	currentToolInfo string // Brief info about current tool operation (e.g., file path)
	toolStartTime   time.Time
	todoItems       []string
	workDir         string

	// Streaming timeout protection
	streamStartTime time.Time
	streamTimeout   time.Duration

	// Slow operation warning
	lastActivityTime   time.Time // Last time we received any activity (tool call, stream, etc.)
	slowWarningShown   bool      // Whether we've shown the slow warning for current operation

	// Rate limiting / debounce for message submission
	lastSubmitTime time.Time
	minSubmitDelay time.Duration // Minimum delay between submissions (default: 500ms)

	// Response header tracking
	responseHeaderShown bool // True if assistant header was shown for current response

	// Mouse mode (for toggling between scroll/select modes)
	mouseEnabled bool

	// Token usage tracking
	tokenUsage *TokenUsageMsg
	showTokens bool

	// Project info
	projectType string
	projectName string

	// Git info
	gitBranch string

	// Plan progress tracking
	planProgress     *PlanProgressMsg
	planProgressMode bool // True when plan is actively executing

	// Session timing
	sessionStart time.Time

	// Permission prompt state
	permRequest        *PermissionRequestMsg
	permSelectedOption int

	// Question prompt state
	questionRequest        *QuestionRequestMsg
	questionSelectedOption int
	questionCustomInput    bool
	questionInputModel     InputModel

	// Plan approval state
	planRequest        *PlanApprovalRequestMsg
	planSelectedOption int
	planFeedbackMode   bool       // True when entering feedback for "Request Changes"
	planFeedbackInput  InputModel // Input model for feedback

	// Model selector state
	modelSelectorOpen  bool
	modelSelectedIndex int
	availableModels    []ModelInfo
	currentModel       string
	onModelSelect      func(modelID string)

	// Diff preview state
	diffPreview    DiffPreviewModel
	diffRequest    *DiffPreviewRequestMsg
	onDiffDecision func(decision DiffDecision)

	// Search results state
	searchResults  SearchResultsModel
	searchRequest  *SearchResultsRequestMsg
	onSearchAction func(action SearchAction)

	// Git status state
	gitStatusModel   GitStatusModel
	gitStatusRequest *GitStatusRequestMsg
	onGitAction      func(action GitAction)

	// Scratchpad
	scratchpad string

	// File browser state
	fileBrowser       FileBrowserModel
	fileBrowserActive bool
	onFileSelect      func(path string)

	// Progress state
	progressModel  ProgressModel
	progressActive bool

	// Tool progress bar (for long-running tool operations)
	toolProgressBar *ToolProgressBarModel

	// Tool output state (for expand/collapse)
	toolOutput          *ToolOutputModel
	lastToolOutputIndex int // Tool output index

	// Callbacks
	onSubmit                   func(message string)
	onQuit                     func()
	onPermission               func(decision PermissionDecision)
	onQuestion                 func(answer string)
	onPlanApproval             func(decision PlanApprovalDecision)
	onPlanApprovalWithFeedback func(decision PlanApprovalDecision, feedback string) // Extended callback with feedback
	onInterrupt                func()                                               // Called when user presses ESC to interrupt
	onCancel                   func()                                               // Called when user presses ESC to cancel current processing
	onPermissionsToggle        func() bool                                          // Called to toggle permissions
	onApplyCodeBlock           func(filename, content string)                       // Called when user presses Tab to apply code block

	// Todos visibility state
	todosVisible bool

	// Permissions enabled state
	permissionsEnabled bool

	// Planning mode state
	planningModeEnabled  bool
	onPlanningModeToggle func() bool // Called to toggle planning mode

	// Sandbox state
	sandboxEnabled  bool
	onSandboxToggle func() bool
	getSandboxState func() bool

	// === PHASE 4: App reference for data providers ===
	app any // Reference to App instance (use any to avoid import cycle)

	// Hints system (welcome removed)
	hintsEnabled bool // Enable contextual hints
	hintSystem   *HintSystem
	hintsShown   map[string]int // Track how many times each hint was shown

	// Coordinated task tracking (Phase 2)
	coordinatedTasks      map[string]*CoordinatedTaskState // taskID -> state
	coordinatedTaskOrder  []string                         // Ordered list of task IDs
	activeCoordinatedTask string                           // Currently active task ID

	// Command palette (Ctrl+P)
	commandPalette *CommandPalette

	// Toast notifications
	toastManager *ToastManager

	// Plan progress panel (detailed plan execution view)
	planProgressPanel *PlanProgressPanel

	// Background task tracking
	backgroundTasks map[string]*BackgroundTaskState

	// Activity feed panel
	activityFeed  *ActivityFeedPanel
	currentToolID string // For tracking active tool
}

// BackgroundTaskState tracks the state of a background task for UI display.
type BackgroundTaskState struct {
	ID          string
	Type        string // "agent" or "shell"
	Description string
	Status      string // "running", "completed", "failed", "cancelled"
	StartTime   time.Time
}

// CoordinatedTaskState tracks the state of a coordinated task for UI display.
type CoordinatedTaskState struct {
	ID        string
	Message   string
	Status    string // pending, running, completed, failed
	Progress  float64
	StartTime time.Time
	Duration  time.Duration
	Error     error
}

// NewModel creates a new TUI model.
func NewModel() *Model {
	styles := DefaultStyles()

	// Auto-apply macOS theme if on darwin
	if runtime.GOOS == "darwin" {
		styles.ApplyTheme(ThemeMacOS)
	}

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner

	return &Model{
		input:                NewInputModel(styles),
		output:               NewOutputModel(styles),
		spinner:              s,
		styles:               styles,
		state:                StateInput,
		mouseEnabled:         true,                   // Default to scroll mode (mouse enabled)
		streamTimeout:        15 * time.Minute,       // Timeout for stuck streaming states (generous for long operations)
		minSubmitDelay:       500 * time.Millisecond, // Debounce: 500ms between submissions
		sessionStart:         time.Now(),
		diffPreview:          NewDiffPreviewModel(styles),
		searchResults:        NewSearchResultsModel(styles),
		gitStatusModel:       NewGitStatusModel(styles),
		fileBrowser:          NewFileBrowserModel(styles),
		progressModel:        NewProgressModel(styles),
		toolOutput:           NewToolOutputModel(styles),
		lastToolOutputIndex:  -1,
		todosVisible:         false, // Default to hidden
		permissionsEnabled:   true,  // Default to enabled
		sandboxEnabled:       true,  // Default to enabled
		hintsEnabled:         true,  // Enable contextual hints
		hintSystem:           NewHintSystem(styles),
		hintsShown:           make(map[string]int),
		coordinatedTasks:     make(map[string]*CoordinatedTaskState),
		coordinatedTaskOrder: make([]string, 0),
		commandPalette:       NewCommandPalette(styles),
		toastManager:         NewToastManager(styles),
		planProgressPanel:    NewPlanProgressPanel(styles),
		backgroundTasks:      make(map[string]*BackgroundTaskState),
		toolProgressBar:      NewToolProgressBarModel(styles),
		activityFeed:         NewActivityFeedPanel(styles),
	}
}

// Init initializes the TUI.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.input.Init(),
		m.spinner.Tick,
	}

	return tea.Batch(cmds...)
}

// ScratchpadMsg is sent when the agent scratchpad is updated.
type ScratchpadMsg string

// Update handles TUI events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Handle critical messages FIRST - always process these regardless of welcome screen
	switch msg := msg.(type) {
	case ScratchpadMsg:
		m.scratchpad = string(msg)
		return m, nil
	case tea.WindowSizeMsg:
		// This is critical for initializing the viewport
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width)
		m.output.SetSize(msg.Width, msg.Height-5) // More space for content

		if m.planFeedbackMode {
			m.planFeedbackInput.SetWidth(msg.Width - 4)
		}
		if m.state == StateQuestionPrompt {
			m.questionInputModel.SetWidth(msg.Width)
		}

		var cmd tea.Cmd
		m.output, cmd = m.output.Update(msg)
		cmds = append(cmds, cmd)

	case spinner.TickMsg:
		// Always process spinner ticks for animation
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)

		// Flush pending viewport updates (debounced content)
		m.output.FlushPendingUpdate()

		// Update toast manager (clean up expired toasts)
		if m.toastManager != nil {
			m.toastManager.Update()
		}

		// Update plan progress panel animation
		if m.planProgressPanel != nil {
			m.planProgressPanel.Tick()
		}

		// Update tool progress bar animation
		if m.toolProgressBar != nil {
			m.toolProgressBar.Tick()
		}

		// Update activity feed animation
		if m.activityFeed != nil {
			m.activityFeed.Tick()
		}

		// Check for streaming timeout
		if (m.state == StateProcessing || m.state == StateStreaming) &&
			!m.streamStartTime.IsZero() &&
			time.Since(m.streamStartTime) > m.streamTimeout {
			m.state = StateInput
			m.streamStartTime = time.Time{}
			m.lastActivityTime = time.Time{}
			m.slowWarningShown = false
			m.currentTool = ""
			m.currentToolInfo = ""
			m.responseHeaderShown = false
			m.output.FlushStream() // Flush any remaining streamed content
			m.output.AppendLine("")
			m.output.AppendLine(m.styles.FormatError(fmt.Sprintf("request timed out after %v", m.streamTimeout)))
			m.output.AppendLine("")
			if m.onInterrupt != nil {
				m.onInterrupt()
			}
			cmds = append(cmds, m.input.Focus())
		}

		// Check for slow operation warning (show toast after 3 seconds of no activity)
		if (m.state == StateProcessing || m.state == StateStreaming) &&
			!m.slowWarningShown &&
			!m.lastActivityTime.IsZero() &&
			time.Since(m.lastActivityTime) > slowOperationThreshold {
			m.slowWarningShown = true
			if m.toastManager != nil {
				m.toastManager.ShowWarning("Operation taking longer than expected... (ESC to cancel)")
			}
		}
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		cmd := m.handleKeyMsg(msg)
		if cmd != nil {
			return m, cmd
		}

	case tea.WindowSizeMsg:
		// Already handled above before welcome screen check
		// Just skip to avoid duplicate processing

	case spinner.TickMsg:
		// Already handled above before welcome screen check
		// Just skip to avoid duplicate processing

	case tea.MouseMsg:
		// Forward mouse events to output viewport for scrolling
		var cmd tea.Cmd
		m.output, cmd = m.output.Update(msg)
		cmds = append(cmds, cmd)

	default:
		// Handle message types
		cmd := m.handleMessageTypes(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// Update input when in input state
	if m.state == StateInput {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// handleKeyMsg handles keyboard input.
func (m *Model) handleKeyMsg(msg tea.KeyMsg) tea.Cmd {
	// Handle permission prompt keys first
	if m.state == StatePermissionPrompt {
		return m.handlePermissionPromptKeys(msg)
	}

	// Handle question prompt keys
	if m.state == StateQuestionPrompt && m.questionRequest != nil {
		return m.handleQuestionPromptKeys(msg)
	}

	// Handle plan approval keys
	if m.state == StatePlanApproval && m.planRequest != nil {
		return m.handlePlanApprovalKeys(msg)
	}

	// Handle model selector keys
	if m.state == StateModelSelector {
		return m.handleModelSelectorKeys(msg)
	}

	// Handle shortcuts overlay keys
	if m.state == StateShortcutsOverlay {
		// Any key closes the overlay
		m.state = StateInput
		return m.input.Focus()
	}

	// Handle command palette keys
	if m.state == StateCommandPalette {
		return m.handleCommandPaletteKeys(msg)
	}

	// Handle diff preview keys
	if m.state == StateDiffPreview {
		var cmd tea.Cmd
		m.diffPreview, cmd = m.diffPreview.Update(msg)
		return cmd
	}

	// Handle search results keys
	if m.state == StateSearchResults {
		var cmd tea.Cmd
		m.searchResults, cmd = m.searchResults.Update(msg)
		return cmd
	}

	// Handle git status keys
	if m.state == StateGitStatus {
		var cmd tea.Cmd
		m.gitStatusModel, cmd = m.gitStatusModel.Update(msg)
		return cmd
	}

	// Handle file browser keys
	if m.state == StateFileBrowser {
		var cmd tea.Cmd
		m.fileBrowser, cmd = m.fileBrowser.Update(msg)
		return cmd
	}

	// Handle batch progress keys
	if m.state == StateBatchProgress {
		var cmd tea.Cmd
		m.progressModel, cmd = m.progressModel.Update(msg)
		return cmd
	}

	// Handle global keys
	return m.handleGlobalKeys(msg)
}

// handlePermissionPromptKeys handles keys in permission prompt state.
func (m *Model) handlePermissionPromptKeys(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		if m.permSelectedOption > 0 {
			m.permSelectedOption--
		}
	case "down", "j":
		if m.permSelectedOption < 2 {
			m.permSelectedOption++
		}
	case "enter", " ":
		// User made a decision
		decision := PermissionDecision(m.permSelectedOption)
		m.permRequest = nil
		m.permSelectedOption = 0
		// Deny decisions return to input, allow continues processing
		if decision == PermissionDeny || decision == PermissionDenySession {
			m.state = StateInput
			m.output.AppendLine(m.styles.Warning.Render(" Denied - operation cancelled"))
			m.output.AppendLine("")
			if m.onInterrupt != nil {
				m.onInterrupt()
			}
			if m.onPermission != nil {
				m.onPermission(decision)
			}
			return m.input.Focus()
		}
		m.state = StateProcessing
		if m.onPermission != nil {
			m.onPermission(decision)
		}
	case "y":
		// Quick allow
		m.permRequest = nil
		m.permSelectedOption = 0
		m.state = StateProcessing
		if m.onPermission != nil {
			m.onPermission(PermissionAllow)
		}
	case "n", "esc":
		// Quick deny / ESC cancels
		m.permRequest = nil
		m.permSelectedOption = 0
		m.state = StateInput
		m.output.AppendLine(m.styles.Warning.Render(" Denied - operation cancelled"))
		m.output.AppendLine("")
		if m.onInterrupt != nil {
			m.onInterrupt()
		}
		if m.onPermission != nil {
			m.onPermission(PermissionDeny)
		}
		return m.input.Focus()
	case "a":
		// Allow for session
		m.permRequest = nil
		m.permSelectedOption = 0
		m.state = StateProcessing
		if m.onPermission != nil {
			m.onPermission(PermissionAllowSession)
		}
	case "?":
		// Show tool details
		if m.permRequest != nil {
			infoStyle := lipgloss.NewStyle().Foreground(ColorInfo)
			m.output.AppendLine("")
			m.output.AppendLine(infoStyle.Render("  Tool: " + m.permRequest.ToolName))
			if m.permRequest.Reason != "" {
				m.output.AppendLine(m.styles.Dim.Render("  " + m.permRequest.Reason))
			}
			m.output.AppendLine("")
		}
	}
	return nil
}

// handleQuestionPromptKeys handles keys in question prompt state.
func (m *Model) handleQuestionPromptKeys(msg tea.KeyMsg) tea.Cmd {
	// If custom input mode, delegate to input model
	if m.questionCustomInput {
		switch msg.Type {
		case tea.KeyEnter:
			// Submit custom answer
			answer := m.questionInputModel.Value()
			m.questionRequest = nil
			m.questionCustomInput = false
			m.questionInputModel.Reset()
			m.state = StateProcessing
			if m.onQuestion != nil {
				m.onQuestion(answer)
			}
			return nil
		case tea.KeyEsc:
			// Cancel custom input, return to options
			m.questionCustomInput = false
			m.questionInputModel.Reset()
			return nil
		default:
			var cmd tea.Cmd
			m.questionInputModel, cmd = m.questionInputModel.Update(msg)
			return cmd
		}
	}

	// Option selection mode
	optCount := len(m.questionRequest.Options)
	if optCount == 0 {
		// No options - just free text input
		switch msg.Type {
		case tea.KeyEnter:
			answer := m.questionInputModel.Value()
			m.questionRequest = nil
			m.questionInputModel.Reset()
			m.state = StateProcessing
			if m.onQuestion != nil {
				m.onQuestion(answer)
			}
			return nil
		default:
			var cmd tea.Cmd
			m.questionInputModel, cmd = m.questionInputModel.Update(msg)
			return cmd
		}
	}

	switch msg.String() {
	case "esc":
		m.questionRequest = nil
		m.questionSelectedOption = 0
		m.state = StateInput
		m.output.AppendLine("")
		m.output.AppendLine(m.styles.Warning.Render(" Question cancelled"))
		m.output.AppendLine("")
		if m.onQuestion != nil {
			m.onQuestion("")
		}
		return m.input.Focus()
	case "up", "k":
		if m.questionSelectedOption > 0 {
			m.questionSelectedOption--
		}
	case "down", "j":
		if m.questionSelectedOption < optCount { // +1 for "Other" option
			m.questionSelectedOption++
		}
	case "enter", " ":
		if m.questionSelectedOption < optCount {
			// Selected an option
			answer := m.questionRequest.Options[m.questionSelectedOption]
			m.questionRequest = nil
			m.questionSelectedOption = 0
			m.state = StateProcessing
			if m.onQuestion != nil {
				m.onQuestion(answer)
			}
		} else {
			// Selected "Other" - switch to custom input
			m.questionCustomInput = true
			m.questionInputModel = NewInputModel(m.styles)
			m.questionInputModel.SetWidth(m.width)
			return m.questionInputModel.Focus()
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Quick select by number
		idx := int(msg.String()[0] - '1')
		if idx < optCount {
			answer := m.questionRequest.Options[idx]
			m.questionRequest = nil
			m.questionSelectedOption = 0
			m.state = StateProcessing
			if m.onQuestion != nil {
				m.onQuestion(answer)
			}
		}
	}
	return nil
}

// handlePlanApprovalKeys handles keys in plan approval state.
func (m *Model) handlePlanApprovalKeys(msg tea.KeyMsg) tea.Cmd {
	// If in feedback mode, handle input
	if m.planFeedbackMode {
		switch msg.Type {
		case tea.KeyEnter:
			// Submit feedback
			feedback := m.planFeedbackInput.Value()
			m.planRequest = nil
			m.planSelectedOption = 0
			m.planFeedbackMode = false
			m.planFeedbackInput.Reset()
			m.state = StateInput
			// Send decision with feedback
			if m.onPlanApprovalWithFeedback != nil {
				m.onPlanApprovalWithFeedback(PlanModifyRequested, feedback)
			} else if m.onPlanApproval != nil {
				m.onPlanApproval(PlanModifyRequested)
			}
			return m.input.Focus()
		case tea.KeyEsc:
			// Cancel feedback, return to options
			m.planFeedbackMode = false
			m.planFeedbackInput.Reset()
			return nil
		default:
			var cmd tea.Cmd
			m.planFeedbackInput, cmd = m.planFeedbackInput.Update(msg)
			return cmd
		}
	}

	switch msg.String() {
	case "up", "k":
		if m.planSelectedOption > 0 {
			m.planSelectedOption--
		}
	case "down", "j":
		if m.planSelectedOption < 2 {
			m.planSelectedOption++
		}
	case "enter", " ":
		decision := PlanApprovalDecision(m.planSelectedOption)
		if decision == PlanModifyRequested {
			// Enter feedback mode
			m.planFeedbackMode = true
			m.planFeedbackInput = NewInputModel(m.styles)
			m.planFeedbackInput.SetWidth(m.width - 4)
			m.planFeedbackInput.SetPlaceholder("Enter your feedback for plan modifications...")
			return m.planFeedbackInput.Focus()
		}
		m.planRequest = nil
		m.planSelectedOption = 0
		m.state = StateProcessing
		if m.onPlanApproval != nil {
			m.onPlanApproval(decision)
		}
	case "y":
		// Quick approve
		// Initialize plan progress panel with the approved plan
		if m.planRequest != nil && m.planProgressPanel != nil {
			m.planProgressPanel.StartPlan(
				"", // planID - will be filled by progress updates
				m.planRequest.Title,
				m.planRequest.Description,
				m.planRequest.Steps,
			)
			if m.toastManager != nil {
				m.toastManager.ShowSuccess("Plan approved - starting execution")
			}
		}
		m.planRequest = nil
		m.planSelectedOption = 0
		m.planFeedbackMode = false
		m.planFeedbackInput.Reset()
		m.state = StateProcessing
		if m.onPlanApproval != nil {
			m.onPlanApproval(PlanApproved)
		}
	case "n":
		// Quick reject
		m.planRequest = nil
		m.planSelectedOption = 0
		m.planFeedbackMode = false
		m.planFeedbackInput.Reset()
		m.state = StateProcessing
		if m.onPlanApproval != nil {
			m.onPlanApproval(PlanRejected)
		}
	case "m":
		// Quick modify - enter feedback mode
		m.planFeedbackMode = true
		m.planFeedbackInput = NewInputModel(m.styles)
		m.planFeedbackInput.SetWidth(m.width - 4)
		m.planFeedbackInput.SetPlaceholder("Enter your feedback for plan modifications...")
		return m.planFeedbackInput.Focus()
	case "esc":
		// ESC to interrupt plan approval and return to input with context
		m.planRequest = nil
		m.planSelectedOption = 0
		m.planFeedbackMode = false
		m.state = StateInput
		m.output.AppendLine("")
		m.output.AppendLine(m.styles.Warning.Render(" Plan approval interrupted"))
		m.output.AppendLine(lipgloss.NewStyle().Foreground(ColorInfo).Render(" You can now provide feedback or modification requests"))
		m.output.AppendLine("")
		if m.onInterrupt != nil {
			m.onInterrupt()
		}
		return m.input.Focus()
	}
	return nil
}

// handleCommandPaletteKeys handles keys in command palette state.
func (m *Model) handleCommandPaletteKeys(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEscape:
		m.commandPalette.Hide()
		m.state = StateInput
		return m.input.Focus()

	case tea.KeyEnter:
		cmd := m.commandPalette.Execute()
		if cmd == nil {
			m.state = StateInput
			return m.input.Focus()
		}

		// Execute based on command type
		switch cmd.Type {
		case CommandTypeSlash:
			if strings.HasPrefix(cmd.Shortcut, "/") {
				// Slash command - submit to the app
				m.state = StateProcessing
				m.streamStartTime = time.Now()
				m.output.AppendLine(m.styles.FormatUserMessage(cmd.Shortcut))
				m.output.AppendLine("")
				if m.onSubmit != nil {
					m.onSubmit(cmd.Shortcut)
				}
				return nil
			}
		case CommandTypeAction:
			if cmd.Action != nil {
				cmd.Action()
			}
		}

		m.state = StateInput
		return m.input.Focus()

	case tea.KeyUp:
		m.commandPalette.SelectPrev()
		return nil

	case tea.KeyDown:
		m.commandPalette.SelectNext()
		return nil

	case tea.KeyTab:
		// Toggle preview panel
		m.commandPalette.TogglePreview()
		return nil

	case tea.KeyBackspace:
		m.commandPalette.BackspaceQuery()
		return nil

	default:
		// Handle text input for filtering
		if msg.Type == tea.KeyRunes {
			m.commandPalette.AppendQuery(string(msg.Runes))
		}
		return nil
	}
}

// handleModelSelectorKeys handles keys in model selector state.
func (m *Model) handleModelSelectorKeys(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "up", "k":
		if m.modelSelectedIndex > 0 {
			m.modelSelectedIndex--
		}
	case "down", "j":
		if m.modelSelectedIndex < len(m.availableModels)-1 {
			m.modelSelectedIndex++
		}
	case "enter", " ":
		// Select model
		if m.modelSelectedIndex < len(m.availableModels) {
			selected := m.availableModels[m.modelSelectedIndex]
			if selected.ID != m.currentModel {
				m.currentModel = selected.ID
				m.output.AppendLine(m.styles.Spinner.Render(fmt.Sprintf("Switched to %s", selected.Name)))
				if m.onModelSelect != nil {
					m.onModelSelect(selected.ID)
				}
			}
			m.state = StateInput
			return m.input.Focus()
		}
	case "esc", "q":
		// Cancel selection
		m.state = StateInput
		return m.input.Focus()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Quick select by number
		idx := int(msg.String()[0] - '1')
		if idx < len(m.availableModels) {
			selected := m.availableModels[idx]
			if selected.ID != m.currentModel {
				m.currentModel = selected.ID
				m.output.AppendLine(m.styles.Spinner.Render(fmt.Sprintf("Switched to %s", selected.Name)))
				if m.onModelSelect != nil {
					m.onModelSelect(selected.ID)
				}
			}
			m.state = StateInput
			return m.input.Focus()
		}
	}
	return nil
}

// handleGlobalKeys handles global keyboard shortcuts.
func (m *Model) handleGlobalKeys(msg tea.KeyMsg) tea.Cmd {
	// Handle Ctrl+P for command palette (only when in input state)
	if msg.Type == tea.KeyCtrlP && m.state == StateInput {
		m.commandPalette.Show()
		m.state = StateCommandPalette
		return nil
	}

	// Handle 'a' key for activity feed toggle (only when input is empty)
	if msg.String() == "a" && m.state == StateInput && m.input.Value() == "" {
		if m.activityFeed != nil {
			m.activityFeed.Toggle()
		}
		return nil
	}

	// Handle 'e' key for tool output expand/collapse (only when input is empty)
	if msg.String() == "e" && m.state == StateInput && m.input.Value() == "" {
		if m.toolOutput != nil && m.lastToolOutputIndex >= 0 {
			entry := m.toolOutput.GetEntry(m.lastToolOutputIndex)
			if entry != nil {
				wasExpanded := m.toolOutput.IsExpanded(m.lastToolOutputIndex)
				m.toolOutput.ToggleExpand(m.lastToolOutputIndex)
				if wasExpanded {
					m.output.AppendLine(m.styles.Dim.Render("  [Tool output collapsed]"))
				} else {
					// Re-render the last tool output expanded
					m.output.AppendLine(m.styles.Dim.Render("  [Tool output expanded]"))
					m.output.AppendLine(m.styles.ToolResult.Render("   " + strings.ReplaceAll(entry.FullContent, "\n", "\n    ")))
				}
				m.output.AppendLine("")
			}
		}
		return nil
	}

	// Code block navigation and actions (only when input is empty)
	if m.state == StateInput && m.input.Value() == "" {
		codeBlocks := m.output.GetCodeBlocks()
		if codeBlocks != nil && codeBlocks.Count() > 0 {
			switch msg.String() {
			case "]":
				// Navigate to next code block
				if codeBlocks.SelectNext() {
					m.output.AppendLine(m.styles.Dim.Render(fmt.Sprintf("  [%s]", codeBlocks.RenderSelectionIndicator())))
				}
				return nil
			case "[":
				// Navigate to previous code block
				if codeBlocks.SelectPrev() {
					m.output.AppendLine(m.styles.Dim.Render(fmt.Sprintf("  [%s]", codeBlocks.RenderSelectionIndicator())))
				}
				return nil
				// Single-char hotkeys (c, y, Y) removed - use /copy command instead
			}
		}
	}

	// All commands accessible via Ctrl+P (Command Palette) and slash commands

	switch msg.Type {
	case tea.KeyCtrlC:
		// If tool progress bar is visible and cancellable, cancel the operation
		if m.toolProgressBar != nil && m.toolProgressBar.IsVisible() && m.toolProgressBar.IsCancellable() {
			if m.onCancel != nil {
				m.onCancel()
			}
			m.toolProgressBar.Hide()
			m.state = StateInput
			m.currentTool = ""
			m.currentToolInfo = ""
			m.output.AppendLine("")
			m.output.AppendLine(m.styles.Warning.Render(" Operation cancelled"))
			m.output.AppendLine("")
			return m.input.Focus()
		}
		if m.onQuit != nil {
			m.onQuit()
		}
		return tea.Quit

	case tea.KeyEscape:
		// ESC interrupts processing/streaming and returns to input
		if m.state == StateProcessing || m.state == StateStreaming {
			// Cancel the current processing (API request)
			if m.onCancel != nil {
				m.onCancel()
			}
			m.state = StateInput
			m.currentTool = ""
			m.currentToolInfo = ""
			m.streamStartTime = time.Time{} // Reset timeout tracking
			m.output.AppendLine("")
			m.output.AppendLine(m.styles.Warning.Render(" Interrupted - request cancelled"))
			m.output.AppendLine("")
			if m.onInterrupt != nil {
				m.onInterrupt()
			}
			return m.input.Focus()
		}

	case tea.KeyEnter:
		// Send message on Enter (when input is not empty and no suggestions are shown)
		if m.state == StateInput && !m.input.ShowingSuggestions() {
			value := m.input.Value()
			if value != "" {
				// Rate limiting: prevent rapid message spam
				if time.Since(m.lastSubmitTime) < m.minSubmitDelay {
					return nil // Ignore too-fast submissions
				}
				m.lastSubmitTime = time.Now()

				m.input.AddToHistory(value) // Save to history
				m.input.Reset()
				m.state = StateProcessing
				m.streamStartTime = time.Now()  // Start timeout tracking
				m.lastActivityTime = time.Now() // Start activity tracking
				m.slowWarningShown = false      // Reset slow warning
				m.responseHeaderShown = false   // Reset for new response
				m.output.AppendLine(m.styles.FormatUserMessage(value))
				m.output.AppendLine("")

				if m.onSubmit != nil {
					m.onSubmit(value)
				}
				return nil
			}
		}

	case tea.KeyCtrlL:
		// Clear output screen
		if m.state == StateInput {
			m.output.Clear()
			return nil
		}

	case tea.KeyCtrlU:
		// Clear input line
		if m.state == StateInput {
			m.input.Reset()
			return nil
		}

	case tea.KeyShiftTab:
		// Toggle planning mode (like Claude Code)
		if m.state == StateInput && m.onPlanningModeToggle != nil {
			enabled := m.onPlanningModeToggle()
			m.planningModeEnabled = enabled
			// Show feedback
			if enabled {
				m.output.AppendLine(m.styles.Spinner.Render("Planning mode enabled — complex tasks will be broken into steps"))
			} else {
				m.output.AppendLine(m.styles.Dim.Render("Planning mode disabled — direct execution"))
			}
			m.output.AppendLine("")
			return nil
		}

	case tea.KeyPgUp, tea.KeyPgDown:
		// Forward page up/down to output viewport for scrolling
		var cmd tea.Cmd
		m.output, cmd = m.output.Update(msg)
		return cmd
	}

	return nil
}

// handleMessageTypes handles various message types.
func (m *Model) handleMessageTypes(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case StreamTextMsg:
		m.streamStartTime = time.Now() // Reset timeout on streaming activity
		m.lastActivityTime = time.Now()
		m.slowWarningShown = false
		m.state = StateStreaming

		// Mark response as started (no header in Claude Code style)
		if !m.responseHeaderShown {
			m.responseHeaderShown = true
		}

		m.output.AppendTextStream(string(msg))

	case ToolCallMsg:
		m.streamStartTime = time.Now() // Reset timeout on tool activity
		m.lastActivityTime = time.Now()
		m.slowWarningShown = false
		m.currentTool = msg.Name
		m.currentToolInfo = "" // Reset tool info
		m.toolStartTime = time.Now()

		// Mark response as started (no header in Claude Code style)
		if !m.responseHeaderShown {
			m.responseHeaderShown = true
		}

		// Generate tool info for status line display
		m.currentToolInfo = m.extractToolInfoFromArgs(msg.Name, msg.Args)

		// Update plan progress panel with current tool (live activity)
		if m.planProgressPanel != nil && m.planProgressPanel.IsVisible() {
			m.planProgressPanel.SetCurrentTool(msg.Name, m.currentToolInfo)
		}

		// Add to activity feed
		if m.activityFeed != nil {
			toolID := fmt.Sprintf("tool-%d", time.Now().UnixNano())
			m.currentToolID = toolID
			m.activityFeed.AddEntry(ActivityFeedEntry{
				ID:          toolID,
				Type:        ActivityTypeTool,
				Name:        msg.Name,
				Description: formatToolActivity(msg.Name, msg.Args),
				Status:      ActivityRunning,
				StartTime:   time.Now(),
				Details:     msg.Args,
			})
		}

		// Show tool call in output with enhanced block formatting
		m.output.AppendLine(m.styles.FormatToolExecutingBlock(msg.Name, msg.Args))

	case ToolResultMsg:
		m.streamStartTime = time.Now() // Reset timeout on tool result
		m.lastActivityTime = time.Now()
		m.slowWarningShown = false
		m.handleToolResult(string(msg))

		// Complete entry in activity feed
		if m.activityFeed != nil && m.currentToolID != "" {
			m.activityFeed.CompleteEntry(m.currentToolID, true, "")
			m.currentToolID = ""
		}

		// Clear current tool after result - next ToolCallMsg will set new tool
		m.currentTool = ""
		m.currentToolInfo = ""
		// Hide tool progress bar
		if m.toolProgressBar != nil {
			m.toolProgressBar.Hide()
		}
		// Clear current tool in plan progress panel
		if m.planProgressPanel != nil && m.planProgressPanel.IsVisible() {
			m.planProgressPanel.ClearCurrentTool()
		}

	case ToolProgressMsg:
		// Reset timeout on progress heartbeat (keeps UI alive during long operations)
		m.streamStartTime = time.Now()
		m.lastActivityTime = time.Now()
		m.slowWarningShown = false
		// Update tool progress bar
		if m.toolProgressBar != nil {
			m.toolProgressBar.Update(msg)
		}

	case ResponseDoneMsg:
		m.state = StateInput
		m.currentTool = ""
		m.currentToolInfo = ""
		m.streamStartTime = time.Time{}  // Reset timeout tracking
		m.lastActivityTime = time.Time{} // Reset activity tracking
		m.slowWarningShown = false       // Reset slow warning
		m.responseHeaderShown = false    // Reset for next response
		m.output.FlushStream()           // Flush any remaining streamed content
		m.output.AppendLine("")
		cmds = append(cmds, m.input.Focus())

	case ResponseMetadataMsg:
		// Render response metadata footer
		footer := m.renderResponseMetadata(msg)
		m.output.AppendLine(footer)
		m.output.AppendLine("")

	case ErrorMsg:
		m.state = StateInput
		m.currentTool = ""
		m.currentToolInfo = ""
		m.streamStartTime = time.Time{} // Reset timeout tracking
		m.responseHeaderShown = false   // Reset for next response
		m.output.FlushStream()          // Flush any remaining streamed content

		// Use enhanced error guidance system
		errStr := msg.Error()
		m.output.AppendLine("")
		m.output.AppendLine(FormatErrorWithGuidance(m.styles, errStr))
		m.output.AppendLine("")
		cmds = append(cmds, m.input.Focus())

	case TodoUpdateMsg:
		m.todoItems = msg

	case TokenUsageMsg:
		m.tokenUsage = &msg

	case ProjectInfoMsg:
		m.projectType = msg.ProjectType
		m.projectName = msg.ProjectName
		// Update iTerm2 badge when project info is received
		if runtime.GOOS == "darwin" && m.projectName != "" {
			badge := "Gokin: " + m.projectName
			if m.gitBranch != "" {
				badge += " (" + m.gitBranch + ")"
			}
			cmds = append(cmds, SetBadgeCmd(badge))
		}

	case PermissionRequestMsg:
		m.permRequest = &msg
		m.permSelectedOption = 0
		m.state = StatePermissionPrompt

	case QuestionRequestMsg:
		m.questionRequest = &msg
		m.questionSelectedOption = 0
		m.questionCustomInput = false
		m.state = StateQuestionPrompt
		// If no options, initialize input model for free text
		if len(msg.Options) == 0 {
			m.questionInputModel = NewInputModel(m.styles)
			m.questionInputModel.SetWidth(m.width)
			cmds = append(cmds, m.questionInputModel.Focus())
		}

	case PlanApprovalRequestMsg:
		m.planRequest = &msg
		m.planSelectedOption = 0
		m.state = StatePlanApproval

	case PlanProgressMsg:
		m.planProgress = &msg
		m.planProgressMode = (msg.Status == "in_progress")

		// Update plan progress panel
		if m.planProgressPanel != nil {
			oldStepID := m.planProgressPanel.currentStepID
			newStepID := msg.CurrentStepID

			// Handle different status types
			switch msg.Status {
			case "in_progress":
				// New step started
				if newStepID != oldStepID && newStepID > 0 {
					m.planProgressPanel.StartStep(newStepID)
					if m.toastManager != nil {
						stepTitle := msg.CurrentTitle
						if len(stepTitle) > 30 {
							stepTitle = stepTitle[:27] + "..."
						}
						m.toastManager.ShowInfo(fmt.Sprintf("Step %d: %s", newStepID, stepTitle))
					}
				}

			case "completed":
				// Step completed - use the step that was just completed
				stepID := msg.CurrentStepID
				if stepID > 0 {
					m.planProgressPanel.CompleteStep(stepID, "")
					if m.toastManager != nil {
						m.toastManager.ShowSuccess(fmt.Sprintf("Step %d completed", stepID))
					}
				}

			case "failed":
				// Step failed
				stepID := msg.CurrentStepID
				if stepID > 0 {
					m.planProgressPanel.FailStep(stepID, "")
					if m.toastManager != nil {
						m.toastManager.ShowError(fmt.Sprintf("Step %d failed", stepID))
					}
				}

			case "skipped":
				// Step skipped
				stepID := msg.CurrentStepID
				if stepID > 0 {
					m.planProgressPanel.SkipStep(stepID)
					if m.toastManager != nil {
						m.toastManager.ShowWarning(fmt.Sprintf("Step %d skipped", stepID))
					}
				}
			}
		}

	case PlanCompleteMsg:
		m.planProgressMode = false
		m.planProgress = nil

		// End plan in progress panel
		if m.planProgressPanel != nil {
			m.planProgressPanel.EndPlan()
			if m.toastManager != nil {
				if msg.Success {
					m.toastManager.ShowSuccess(fmt.Sprintf("Plan completed in %s", formatElapsed(msg.Duration)))
				} else {
					m.toastManager.ShowError("Plan execution failed")
				}
			}
		}

	case DiffPreviewRequestMsg:
		m.diffRequest = &msg
		m.diffPreview.SetSize(m.width, m.height)
		m.diffPreview.SetContent(msg.FilePath, msg.OldContent, msg.NewContent, msg.ToolName, msg.IsNewFile)
		m.state = StateDiffPreview

	case DiffPreviewResponseMsg:
		m.diffRequest = nil
		if msg.Decision == DiffApply {
			m.state = StateProcessing
			if m.onDiffDecision != nil {
				m.onDiffDecision(msg.Decision)
			}
		} else {
			m.state = StateInput
			m.output.AppendLine(m.styles.Warning.Render(" Changes rejected"))
			m.output.AppendLine("")
			if m.onDiffDecision != nil {
				m.onDiffDecision(msg.Decision)
			}
			cmds = append(cmds, m.input.Focus())
		}

	case SearchResultsRequestMsg:
		m.searchRequest = &msg
		m.searchResults.SetSize(m.width, m.height)
		m.searchResults.SetResults(msg.Query, msg.Tool, msg.Results)
		m.state = StateSearchResults

	case SearchResultsActionMsg:
		m.searchRequest = nil
		if msg.Action == SearchActionClose {
			m.state = StateInput
			cmds = append(cmds, m.input.Focus())
		} else {
			// Reset state for any other action (Open, Edit, CopyPath)
			m.state = StateInput
			cmds = append(cmds, m.input.Focus())
			if m.onSearchAction != nil {
				m.onSearchAction(msg.Action)
			}
		}

	case GitStatusRequestMsg:
		m.gitStatusRequest = &msg
		m.gitStatusModel.SetSize(m.width, m.height)
		m.gitStatusModel.SetStatus(msg.Entries, msg.Branch, msg.Upstream, msg.AheadBehind)
		m.state = StateGitStatus

	case GitStatusActionMsg:
		// Always reset state and request for any action
		m.gitStatusRequest = nil
		m.state = StateInput
		cmds = append(cmds, m.input.Focus())

		// Call action handler for non-close actions
		if msg.Action != GitActionClose && m.onGitAction != nil {
			m.onGitAction(msg.Action)
		}

	case FileBrowserRequestMsg:
		m.fileBrowser.SetSize(m.width, m.height)
		if err := m.fileBrowser.SetPath(msg.StartPath); err == nil {
			m.fileBrowserActive = true
			m.state = StateFileBrowser
		} else {
			m.output.AppendLine(m.styles.FormatError(fmt.Sprintf("Could not open file browser: %s", err)))
			m.output.AppendLine("")
		}

	case FileBrowserActionMsg:
		if msg.Action == FileBrowserActionClose {
			m.fileBrowserActive = false
			m.state = StateInput
			cmds = append(cmds, m.input.Focus())
		} else if msg.Action == FileBrowserActionOpen {
			m.fileBrowserActive = false
			m.state = StateInput
			cmds = append(cmds, m.input.Focus())
			if m.onFileSelect != nil {
				m.onFileSelect(msg.Path)
			}
		} else if msg.Action == FileBrowserActionSelect {
			m.fileBrowserActive = false
			m.state = StateInput
			cmds = append(cmds, m.input.Focus())
		}

	case ProgressUpdateMsg:
		m.progressModel.UpdateProgress(msg.Current, msg.CurrentItem, msg.Message)

	case ProgressCompleteMsg:
		m.progressModel.Complete()
		m.progressActive = false
		if m.state == StateBatchProgress {
			m.state = StateInput
			cmds = append(cmds, m.input.Focus())
		}

	case CloseOverlayMsg:
		m.state = StateInput
		cmds = append(cmds, m.input.Focus())

	// Unused message types - no-op for backward compatibility
	case ShowDependencyGraphMsg, ShowParallelExecutionMsg, ShowTaskQueueMsg:
		// These features were removed

	case DependencyGraphTickMsg, DependencyGraphUpdatedMsg,
		ParallelExecutionTickMsg, ParallelExecutionUpdatedMsg,
		TaskQueueTickMsg, TaskQueueUpdatedMsg:
		// These features were removed

	// Config update message - refresh UI state
	case ConfigUpdateMsg:
		m.permissionsEnabled = msg.PermissionsEnabled
		m.sandboxEnabled = msg.SandboxEnabled

	// Coordinated task events (Phase 2)
	case TaskStartedEvent:
		m.handleTaskStarted(msg)
	case TaskCompletedEvent:
		m.handleTaskCompleted(msg)
	case TaskProgressEvent:
		m.handleTaskProgress(msg)

	// Background task tracking
	case BackgroundTaskMsg:
		m.handleBackgroundTask(msg)

	// Sub-agent activity tracking
	case SubAgentActivityMsg:
		if m.activityFeed != nil {
			switch msg.Status {
			case "start":
				m.activityFeed.StartSubAgent(msg.AgentID, msg.AgentType,
					fmt.Sprintf("Sub-agent: %s", msg.AgentType))
			case "tool_start":
				m.activityFeed.UpdateSubAgentTool(msg.AgentID, msg.ToolName, msg.ToolArgs)
			case "tool_end":
				// Tool completed, but agent continues
				m.activityFeed.UpdateSubAgentTool(msg.AgentID, "", nil)
			case "complete":
				m.activityFeed.CompleteSubAgent(msg.AgentID, true, "")
			case "failed":
				m.activityFeed.CompleteSubAgent(msg.AgentID, false, "")
			}
		}

	// Status updates from client (retry, rate limit, stream idle)
	case StatusUpdateMsg:
		if m.toastManager != nil {
			switch msg.Type {
			case StatusRetry:
				m.toastManager.ShowWarning(msg.Message)
			case StatusRateLimit:
				m.toastManager.ShowWarning(msg.Message)
			case StatusStreamIdle:
				m.toastManager.ShowInfo(msg.Message)
			case StatusStreamResume:
				// Clear any stream idle warning - toasts auto-expire
			case StatusRecoverableError:
				m.toastManager.ShowError(msg.Message)
			}
		}
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	return nil
}

// extractToolInfoFromArgs generates tool info for status line display.
func (m *Model) extractToolInfoFromArgs(name string, args map[string]any) string {
	// Use larger limits to show more of the file path (important for clarity)
	const pathLimit = 80      // Increased from 40 for better visibility
	const pathLimitShort = 60 // For cases with additional info

	switch name {
	case "read":
		if path, ok := args["file_path"].(string); ok {
			return shortenPath(path, pathLimit)
		}
	case "write":
		if path, ok := args["file_path"].(string); ok {
			info := shortenPath(path, pathLimitShort)
			if content, ok := args["content"].(string); ok {
				lines := len(strings.Split(content, "\n"))
				info += fmt.Sprintf(" (%d lines)", lines)
			}
			return info
		}
	case "edit":
		if path, ok := args["file_path"].(string); ok {
			return shortenPath(path, pathLimit)
		}
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			preview := cmd
			if len(preview) > 60 {
				preview = preview[:57] + "..."
			}
			preview = strings.ReplaceAll(preview, "\n", " ")
			return "$ " + preview
		}
	case "grep":
		if pattern, ok := args["pattern"].(string); ok {
			info := pattern
			if len(info) > 30 {
				info = info[:27] + "..."
			}
			if path, ok := args["path"].(string); ok {
				info += " in " + shortenPath(path, 40)
			}
			return info
		}
	case "glob":
		if pattern, ok := args["pattern"].(string); ok {
			return pattern
		}
	case "list_dir", "tree":
		path := "."
		if p, ok := args["directory_path"].(string); ok && p != "" {
			path = p
		}
		return shortenPath(path, pathLimit)
	case "web_fetch":
		if url, ok := args["url"].(string); ok {
			return shortenPath(url, pathLimit)
		}
	case "web_search":
		if query, ok := args["query"].(string); ok {
			return query
		}
	}

	// Fallback: use extractToolInfo
	if len(args) > 0 {
		return extractToolInfo(args)
	}
	return ""
}

// handleToolResult handles tool result message with clean, minimal formatting.
func (m *Model) handleToolResult(content string) {
	dimStyle := lipgloss.NewStyle().Foreground(ColorDim)
	contentStyle := lipgloss.NewStyle().Foreground(ColorMuted)

	// Build summary and duration
	var summary string
	var dur string
	if m.currentTool != "" && !m.toolStartTime.IsZero() {
		duration := time.Since(m.toolStartTime)
		summary = generateToolResultSummary(m.currentTool, content, m.currentToolInfo)

		// Format duration
		if duration < time.Second {
			dur = fmt.Sprintf("%dms", duration.Milliseconds())
		} else if duration < time.Minute {
			dur = fmt.Sprintf("%.1fs", duration.Seconds())
		} else {
			dur = fmt.Sprintf("%.1fm", duration.Minutes())
		}
	}

	// Append summary to the tool call line: "  summary    duration"
	if summary != "" || dur != "" {
		var summaryLine strings.Builder
		summaryLine.WriteString("  ")
		if summary != "" {
			summaryLine.WriteString(dimStyle.Render(summary))
		}
		if dur != "" {
			if summary != "" {
				summaryLine.WriteString("  ")
			}
			summaryLine.WriteString(dimStyle.Render(dur))
		}
		m.output.AppendLine(summaryLine.String())
	}

	if content == "" {
		// No content to show
		m.output.AppendLine("")
		return
	}

	// Store for expand/collapse
	if m.toolOutput != nil {
		m.lastToolOutputIndex = m.toolOutput.AddEntry(m.currentTool, content)
	}

	// Content lines (truncated) with simple indent
	displayContent := FormatToolOutput(content, 6, false)
	lines := strings.Split(displayContent, "\n")
	for _, line := range lines {
		if line != "" {
			m.output.AppendLine("    " + contentStyle.Render(line))
		}
	}
	m.output.AppendLine("")
}

// View renders the TUI.
func (m Model) View() string {
	var builder strings.Builder

	// Toast notifications (top of screen)
	if m.toastManager != nil && m.toastManager.Count() > 0 {
		toasts := m.toastManager.View(m.width)
		if toasts != "" {
			builder.WriteString(toasts)
			builder.WriteString("\n\n")
		}
	}

	// Output viewport
	builder.WriteString(m.output.View())
	builder.WriteString("\n")

	// Scratchpad panel
	if m.scratchpad != "" {
		builder.WriteString(m.renderScratchpad())
		builder.WriteString("\n")
	}

	// Plan progress panel (when actively executing a plan)
	if m.planProgressPanel != nil && m.planProgressPanel.IsVisible() {
		builder.WriteString(m.planProgressPanel.View(m.width))
		builder.WriteString("\n")
	}

	// Activity feed panel
	if m.activityFeed != nil && m.activityFeed.IsVisible() && m.activityFeed.HasActiveEntries() {
		builder.WriteString(m.activityFeed.View(m.width))
		builder.WriteString("\n")
	}

	// Status line with todos
	if len(m.todoItems) > 0 && m.todosVisible {
		builder.WriteString(m.renderTodos())
		builder.WriteString("\n")
	}

	// Tool progress bar (for long-running operations)
	if m.toolProgressBar != nil && m.toolProgressBar.IsVisible() {
		builder.WriteString(m.toolProgressBar.View(m.width))
		builder.WriteString("\n")
	}

	// Processing indicator with enhanced styling
	if m.state == StateProcessing || m.state == StateStreaming {
		status := m.spinner.View() + " "
		if m.currentTool != "" {
			// Tool execution status - clean Claude Code style

			// Tool name without emoji icon
			status += m.styles.ToolCall.Render(capitalizeToolName(m.currentTool))

			// Show tool info (file path, command, etc.) - more visible with gradient
			if m.currentToolInfo != "" {
				infoStyle := lipgloss.NewStyle().
					Foreground(ColorGradient2). // Indigo for contrast
					Bold(true)
				status += "  " + infoStyle.Render(m.currentToolInfo)
			}

			// Color-coded duration (warning if >5s, error if >30s) — guard against zero time
			if !m.toolStartTime.IsZero() {
				elapsed := time.Since(m.toolStartTime)
				durationStr := formatDuration(elapsed)
				var durationStyle lipgloss.Style
				if elapsed > 30*time.Second {
					durationStyle = lipgloss.NewStyle().Foreground(ColorRose)
				} else if elapsed > 5*time.Second {
					durationStyle = lipgloss.NewStyle().Foreground(ColorWarning)
				} else {
					durationStyle = lipgloss.NewStyle().Foreground(ColorMint)
				}
				status += " " + durationStyle.Render(durationStr)
			}
		} else if m.state == StateProcessing {
			// Thinking phase — show elapsed time for feedback
			elapsed := time.Since(m.streamStartTime)
			thinkStyle := lipgloss.NewStyle().
				Foreground(ColorGradient1).
				Bold(true)
			dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

			status += thinkStyle.Render("Thinking")

			// Smooth animated dots using 300ms frame intervals
			dotFrames := []string{"", ".", "..", "..."}
			frameIdx := int(elapsed.Milliseconds()/300) % len(dotFrames)
			dots := dotFrames[frameIdx]
			status += thinkStyle.Render(dots)
			// Pad to fixed width so status bar doesn't jump
			status += strings.Repeat(" ", 3-len(dots))

			// Show elapsed time after 2 seconds
			if elapsed >= 2*time.Second {
				durationStr := formatDuration(elapsed)
				status += " " + dimStyle.Render(durationStr)
			}

			// Show plan step context if plan is executing
			if m.planProgress != nil && m.planProgressMode {
				stepInfo := fmt.Sprintf(" [step %d/%d: %s]",
					m.planProgress.CurrentStepID,
					m.planProgress.TotalSteps,
					m.planProgress.CurrentTitle)
				if len(stepInfo) > 50 {
					stepInfo = stepInfo[:47] + "...]"
				}
				status += " " + dimStyle.Render(stepInfo)
			}

			// Hint for ESC after 2 seconds
			if elapsed >= 2*time.Second {
				hintStyle := lipgloss.NewStyle().Foreground(ColorMuted).Italic(true)
				status += "  " + hintStyle.Render("(ESC to cancel)")
			}
		} else {
			// Streaming phase — show activity
			elapsed := time.Since(m.streamStartTime)
			streamStyle := lipgloss.NewStyle().
				Foreground(ColorGradient2).
				Bold(true)
			dimStyle := lipgloss.NewStyle().Foreground(ColorDim)

			status += streamStyle.Render("Generating...")

			if elapsed >= 2*time.Second {
				status += " " + dimStyle.Render(formatDuration(elapsed))
			}
		}
		builder.WriteString(status)
		builder.WriteString("\n")
	}

	// Permission prompt
	if m.state == StatePermissionPrompt && m.permRequest != nil {
		builder.WriteString(m.renderPermissionPrompt())
		builder.WriteString("\n")
	}

	// Question prompt
	if m.state == StateQuestionPrompt && m.questionRequest != nil {
		builder.WriteString(m.renderQuestionPrompt())
		builder.WriteString("\n")
	}

	// Plan approval prompt
	if m.state == StatePlanApproval && m.planRequest != nil {
		builder.WriteString(m.renderPlanApproval())
		builder.WriteString("\n")
	}

	// Model selector
	if m.state == StateModelSelector {
		builder.WriteString(m.renderModelSelector())
		builder.WriteString("\n")
	}

	// Shortcuts overlay
	if m.state == StateShortcutsOverlay {
		builder.WriteString(m.renderShortcutsOverlay())
		builder.WriteString("\n")
	}

	// Command palette
	if m.state == StateCommandPalette {
		builder.WriteString(m.commandPalette.View(m.width, m.height))
		builder.WriteString("\n")
	}

	// Diff preview
	if m.state == StateDiffPreview {
		builder.WriteString(m.diffPreview.View())
		builder.WriteString("\n")
	}

	// Search results
	if m.state == StateSearchResults {
		builder.WriteString(m.searchResults.View())
		builder.WriteString("\n")
	}

	// Git status
	if m.state == StateGitStatus {
		builder.WriteString(m.gitStatusModel.View())
		builder.WriteString("\n")
	}

	// File browser
	if m.state == StateFileBrowser {
		builder.WriteString(m.fileBrowser.View())
		builder.WriteString("\n")
	}

	// Batch progress
	if m.state == StateBatchProgress {
		builder.WriteString(m.progressModel.View())
		builder.WriteString("\n")
	}

	// Input area
	if m.state == StateInput {
		builder.WriteString(m.input.View())

		// Command hints
		inputText := m.input.Value()
		if strings.HasPrefix(inputText, "/") && len(inputText) > 1 {
			hint := m.getCommandHint(inputText)
			if hint != "" {
				hintStyle := lipgloss.NewStyle().
					Foreground(ColorMuted).
					Italic(true).
					MarginTop(0)
				builder.WriteString("\n" + hintStyle.Render("  "+hint))
			}
		}
	}

	// Enhanced status bar
	builder.WriteString("\n")
	builder.WriteString(m.renderStatusBar())

	return m.styles.App.Render(builder.String())
}

// SetHintsEnabled enables or disables contextual hints.
func (m *Model) SetHintsEnabled(enabled bool) {
	m.hintsEnabled = enabled
}

// ShowHint displays a contextual hint if hints are enabled and the hint
// hasn't been shown too many times (max 3 times per hint).
func (m *Model) ShowHint(hintID, text string) {
	if !m.hintsEnabled {
		return
	}
	if m.hintsShown[hintID] >= 3 {
		return // Already shown 3 times, auto-dismiss
	}
	m.hintsShown[hintID]++
	m.output.AppendLine(m.styles.FormatHint(text))
	m.output.AppendLine("")
}

// ========== Coordinated Task Handlers (Phase 2) ==========

// handleTaskStarted handles the TaskStartedEvent message.
func (m *Model) handleTaskStarted(msg TaskStartedEvent) {
	taskState := &CoordinatedTaskState{
		ID:        msg.TaskID,
		Message:   msg.Message,
		Status:    "running",
		Progress:  0,
		StartTime: time.Now(),
	}

	m.coordinatedTasks[msg.TaskID] = taskState
	m.coordinatedTaskOrder = append(m.coordinatedTaskOrder, msg.TaskID)
	m.activeCoordinatedTask = msg.TaskID

	// Display task start in output
	taskStartStyle := lipgloss.NewStyle().
		Foreground(ColorAccent).
		Bold(true)

	m.output.AppendLine("")
	m.output.AppendLine(taskStartStyle.Render(fmt.Sprintf("Subtask: %s", msg.Message)))
}

// handleTaskCompleted handles the TaskCompletedEvent message.
func (m *Model) handleTaskCompleted(msg TaskCompletedEvent) {
	if taskState, ok := m.coordinatedTasks[msg.TaskID]; ok {
		taskState.Duration = msg.Duration
		taskState.Error = msg.Error

		if msg.Success {
			taskState.Status = "completed"
			taskState.Progress = 1.0

			successStyle := lipgloss.NewStyle().
				Foreground(ColorSuccess)
			m.output.AppendLine(successStyle.Render(fmt.Sprintf("  Completed in %s", msg.Duration.Round(time.Millisecond))))
		} else {
			taskState.Status = "failed"

			errorStyle := lipgloss.NewStyle().
				Foreground(ColorError)
			errMsg := "unknown error"
			if msg.Error != nil {
				errMsg = msg.Error.Error()
			}
			m.output.AppendLine(errorStyle.Render(fmt.Sprintf("  Error: %s", errMsg)))
		}
	}

	// Update active task
	if m.activeCoordinatedTask == msg.TaskID {
		m.activeCoordinatedTask = ""
	}
}

// handleTaskProgress handles the TaskProgressEvent message.
func (m *Model) handleTaskProgress(msg TaskProgressEvent) {
	if taskState, ok := m.coordinatedTasks[msg.TaskID]; ok {
		taskState.Progress = msg.Progress
		taskState.Message = msg.Message

		// Update progress display
		progressStyle := lipgloss.NewStyle().
			Foreground(ColorMuted)

		progressBar := m.renderProgressBar(msg.Progress, 20)
		m.output.AppendLine(progressStyle.Render(fmt.Sprintf("  %s %s", progressBar, msg.Message)))
	}
}

// renderProgressBar renders a simple progress bar.
func (m *Model) renderProgressBar(progress float64, width int) string {
	filled := int(progress * float64(width))
	if filled > width {
		filled = width
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("[%s] %.0f%%", bar, progress*100)
}

// GetCoordinatedTasksSummary returns a summary of coordinated tasks for status display.
func (m *Model) GetCoordinatedTasksSummary() string {
	if len(m.coordinatedTasks) == 0 {
		return ""
	}

	completed := 0
	running := 0
	failed := 0

	for _, task := range m.coordinatedTasks {
		switch task.Status {
		case "completed":
			completed++
		case "running":
			running++
		case "failed":
			failed++
		}
	}

	total := len(m.coordinatedTasks)
	return fmt.Sprintf("%d/%d subtasks (running: %d, done: %d, failed: %d)", completed, total, running, completed, failed)
}

// ClearCoordinatedTasks clears all coordinated task tracking.
func (m *Model) ClearCoordinatedTasks() {
	m.coordinatedTasks = make(map[string]*CoordinatedTaskState)
	m.coordinatedTaskOrder = make([]string, 0)
	m.activeCoordinatedTask = ""
}

// ========== Background Task Handlers ==========

// handleBackgroundTask handles the BackgroundTaskMsg message.
func (m *Model) handleBackgroundTask(msg BackgroundTaskMsg) {
	switch msg.Status {
	case "running":
		// New task started
		m.backgroundTasks[msg.ID] = &BackgroundTaskState{
			ID:          msg.ID,
			Type:        msg.Type,
			Description: msg.Description,
			Status:      "running",
			StartTime:   time.Now(),
		}
		// Show toast notification
		if m.toastManager != nil {
			desc := msg.Description
			if len(desc) > 40 {
				desc = desc[:37] + "..."
			}
			m.toastManager.ShowInfo(fmt.Sprintf("Background task started: %s", desc))
		}

	case "completed", "failed", "cancelled":
		// Task finished - remove from tracking
		if task, ok := m.backgroundTasks[msg.ID]; ok {
			// Show completion toast
			if m.toastManager != nil {
				desc := task.Description
				if len(desc) > 30 {
					desc = desc[:27] + "..."
				}
				switch msg.Status {
				case "completed":
					m.toastManager.ShowSuccess(fmt.Sprintf("Task completed: %s", desc))
				case "failed":
					m.toastManager.ShowError(fmt.Sprintf("Task failed: %s", desc))
				case "cancelled":
					m.toastManager.ShowWarning(fmt.Sprintf("Task cancelled: %s", desc))
				}
			}
			delete(m.backgroundTasks, msg.ID)
		}
	}
}

// GetBackgroundTaskCount returns the number of running background tasks.
func (m *Model) GetBackgroundTaskCount() int {
	return len(m.backgroundTasks)
}

// GetBackgroundTasks returns all background tasks.
func (m *Model) GetBackgroundTasks() map[string]*BackgroundTaskState {
	return m.backgroundTasks
}

// ========== Toast Notification Methods ==========

// ShowToastSuccess displays a success toast notification.
func (m *Model) ShowToastSuccess(message string) {
	if m.toastManager != nil {
		m.toastManager.ShowSuccess(message)
	}
}

// ShowToastError displays an error toast notification.
func (m *Model) ShowToastError(message string) {
	if m.toastManager != nil {
		m.toastManager.ShowError(message)
	}
}

// ShowToastInfo displays an info toast notification.
func (m *Model) ShowToastInfo(message string) {
	if m.toastManager != nil {
		m.toastManager.ShowInfo(message)
	}
}

// ShowToastWarning displays a warning toast notification.
func (m *Model) ShowToastWarning(message string) {
	if m.toastManager != nil {
		m.toastManager.ShowWarning(message)
	}
}

// ========== Command Palette Methods ==========

// ShowCommandPalette shows the command palette.
func (m *Model) ShowCommandPalette() {
	if m.commandPalette != nil {
		m.commandPalette.Show()
		m.state = StateCommandPalette
	}
}

// SetCommandPaletteCommands sets the available commands in the palette.
func (m *Model) SetCommandPaletteCommands(commands []PaletteCommand) {
	if m.commandPalette != nil {
		m.commandPalette.SetCommands(commands)
	}
}

// AddCommandPaletteCommand adds a command to the palette.
func (m *Model) AddCommandPaletteCommand(cmd PaletteCommand) {
	if m.commandPalette != nil {
		m.commandPalette.AddCommand(cmd)
	}
}

// ========== Plan Progress Panel Methods ==========

// StartPlanExecution initializes the plan progress panel for a new plan.
func (m *Model) StartPlanExecution(planID, title, description string, steps []PlanStepInfo) {
	if m.planProgressPanel != nil {
		m.planProgressPanel.StartPlan(planID, title, description, steps)
	}
}

// UpdatePlanStep updates a step's status in the plan progress panel.
func (m *Model) UpdatePlanStep(stepID int, status PlanStepStatus, message string) {
	if m.planProgressPanel == nil {
		return
	}

	switch status {
	case PlanStepInProgress:
		m.planProgressPanel.StartStep(stepID)
	case PlanStepCompleted:
		m.planProgressPanel.CompleteStep(stepID, message)
	case PlanStepFailed:
		m.planProgressPanel.FailStep(stepID, message)
	case PlanStepSkipped:
		m.planProgressPanel.SkipStep(stepID)
	}
}

// EndPlanExecution marks the plan as finished.
func (m *Model) EndPlanExecution() {
	if m.planProgressPanel != nil {
		m.planProgressPanel.EndPlan()
	}
}

// Cleanup performs final UI cleanup before exit.
func (m *Model) Cleanup() {
	if runtime.GOOS == "darwin" && os.Getenv("TERM_PROGRAM") == "iTerm.app" {
		// Clear iTerm2 badge via escape sequence
		// Note: Cleanup runs after program finishes, so direct Print is okay here
		// but ideally should have been a Cmd if we were still in the loop.
		fmt.Print("\033]1337;SetBadgeFormat=\a")
	}
}

// HidePlanProgress hides the plan progress panel.
func (m *Model) HidePlanProgress() {
	if m.planProgressPanel != nil {
		m.planProgressPanel.Hide()
	}
}

// IsPlanProgressVisible returns whether the plan progress panel is visible.
func (m Model) IsPlanProgressVisible() bool {
	return m.planProgressPanel != nil && m.planProgressPanel.IsVisible()
}
