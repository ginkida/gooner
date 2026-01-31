package ui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// SetCallbacks sets the callback functions.
func (m *Model) SetCallbacks(onSubmit func(string), onQuit func()) {
	m.onSubmit = onSubmit
	m.onQuit = onQuit
}

// SetPermissionCallback sets the permission decision callback.
func (m *Model) SetPermissionCallback(onPermission func(PermissionDecision)) {
	m.onPermission = onPermission
}

// SetQuestionCallback sets the question answer callback.
func (m *Model) SetQuestionCallback(onQuestion func(string)) {
	m.onQuestion = onQuestion
}

// SetPlanApprovalCallback sets the plan approval callback.
func (m *Model) SetPlanApprovalCallback(onPlanApproval func(PlanApprovalDecision)) {
	m.onPlanApproval = onPlanApproval
}

// SetPlanApprovalWithFeedbackCallback sets the plan approval callback with feedback support.
func (m *Model) SetPlanApprovalWithFeedbackCallback(onPlanApproval func(PlanApprovalDecision, string)) {
	m.onPlanApprovalWithFeedback = onPlanApproval
}

// SetInterruptCallback sets the interrupt callback (called when user presses ESC).
func (m *Model) SetInterruptCallback(onInterrupt func()) {
	m.onInterrupt = onInterrupt
}

// SetCancelCallback sets the cancel callback for ESC interrupt.
// This is called when the user presses ESC to cancel the current processing.
func (m *Model) SetCancelCallback(onCancel func()) {
	m.onCancel = onCancel
}

// SetPermissionsToggleCallback sets the callback for permissions toggle.
func (m *Model) SetPermissionsToggleCallback(onToggle func() bool) {
	m.onPermissionsToggle = onToggle
}

// SetPermissionsEnabled sets the permissions enabled state for display.
func (m *Model) SetPermissionsEnabled(enabled bool) {
	m.permissionsEnabled = enabled
}

// GetPermissionsEnabled returns the current permissions enabled state.
func (m *Model) GetPermissionsEnabled() bool {
	return m.permissionsEnabled
}

// SetPlanningModeToggleCallback sets the callback for planning mode toggle.
func (m *Model) SetPlanningModeToggleCallback(onToggle func() bool) {
	m.onPlanningModeToggle = onToggle
}

// SetPlanningModeEnabled sets the planning mode enabled state for display.
func (m *Model) SetPlanningModeEnabled(enabled bool) {
	m.planningModeEnabled = enabled
}

// GetPlanningModeEnabled returns the current planning mode enabled state.
func (m *Model) GetPlanningModeEnabled() bool {
	return m.planningModeEnabled
}

// SetSandboxToggleCallback sets the callback for sandbox toggle.
func (m *Model) SetSandboxToggleCallback(onToggle func() bool, getState func() bool) {
	m.onSandboxToggle = onToggle
	m.getSandboxState = getState
}

// SetSandboxEnabled sets the sandbox enabled state for display.
func (m *Model) SetSandboxEnabled(enabled bool) {
	m.sandboxEnabled = enabled
}

// GetSandboxEnabled returns the current sandbox enabled state.
func (m *Model) GetSandboxEnabled() bool {
	return m.sandboxEnabled
}

// SetDiffDecisionCallback sets the callback for diff preview decisions.
func (m *Model) SetDiffDecisionCallback(onDiffDecision func(DiffDecision)) {
	m.onDiffDecision = onDiffDecision
}

// SetSearchActionCallback sets the callback for search result actions.
func (m *Model) SetSearchActionCallback(onSearchAction func(SearchAction)) {
	m.onSearchAction = onSearchAction
}

// SetGitActionCallback sets the callback for git status actions.
func (m *Model) SetGitActionCallback(onGitAction func(GitAction)) {
	m.onGitAction = onGitAction
}

// SetFileSelectCallback sets the callback for file browser selections.
func (m *Model) SetFileSelectCallback(onFileSelect func(string)) {
	m.onFileSelect = onFileSelect
}

// SetApplyCodeBlockCallback sets the callback for applying code blocks.
func (m *Model) SetApplyCodeBlockCallback(onApply func(filename, content string)) {
	m.onApplyCodeBlock = onApply
}

// SetWorkDir sets the working directory for display.
func (m *Model) SetWorkDir(dir string) {
	m.workDir = dir
}

// SetShowTokens enables or disables token usage display.
func (m *Model) SetShowTokens(show bool) {
	m.showTokens = show
}

// SetMouseEnabled enables or disables mouse capture.
func (m *Model) SetMouseEnabled(enabled bool) {
	m.mouseEnabled = enabled
	m.output.SetMouseEnabled(enabled)
}

// SetProjectInfo sets the project information for display.
func (m *Model) SetProjectInfo(projectType, projectName string) {
	m.projectType = projectType
	m.projectName = projectName
}

// SetAvailableModels sets the list of available models for the selector.
func (m *Model) SetAvailableModels(models []ModelInfo) {
	m.availableModels = models
}

// SetCurrentModel sets the current model name.
func (m *Model) SetCurrentModel(modelID string) {
	m.currentModel = modelID
}

// SetModelSelectCallback sets the callback for model selection.
func (m *Model) SetModelSelectCallback(callback func(modelID string)) {
	m.onModelSelect = callback
}

// SetGitBranch sets the current git branch for display.
func (m *Model) SetGitBranch(branch string) {
	m.gitBranch = branch
}

// SetPaletteProvider sets the palette provider for command fetching.
func (m *Model) SetPaletteProvider(provider PaletteProvider) {
	if m.commandPalette != nil {
		m.commandPalette.SetPaletteProvider(provider)
	}
}

// RefreshPaletteCommands refreshes the command palette commands.
func (m *Model) RefreshPaletteCommands() {
	if m.commandPalette != nil {
		m.commandPalette.RefreshCommands()
	}
}

// SetState sets the UI state.
func (m *Model) SetState(state State) {
	m.state = state
}

// GetProgram returns a new Bubbletea program.
func (m *Model) GetProgram() *tea.Program {
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if m.mouseEnabled {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	return tea.NewProgram(m, opts...)
}
