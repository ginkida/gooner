package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The render sweep covers View(); this covers the other half. Update() is where
// every message from the app lands, and a panic there kills the session just as
// dead — except it happens while the agent is mid-turn, so the work in flight
// goes with it. The payloads that reach it are not the tidy ones a focused test
// constructs: a tool result with no content, a progress message before any tool
// started, a modal request whose data has not arrived yet, a status line of CJK.
// Width 0 applies here too, since messages can arrive before the first
// WindowSizeMsg.
func TestUpdateSurvivesHostileMessages(t *testing.T) {
	wide := "日本語" + strings.Repeat("漢字", 60)
	msgs := []tea.Msg{
		// Zero values: every field unset, the shape an early or failed path sends.
		StreamTextMsg(""), StreamThinkingMsg(""), QueuedCountMsg(0),
		QueuedMessageRejectedMsg{}, ToolCallMsg{}, ToolResultMsg{}, ToolProgressMsg{},
		ResponseDoneMsg{}, StatusUpdateMsg{}, RuntimeStatusMsg{},
		PlanningModeToggledMsg{}, SessionModeCycledMsg{}, CloseOverlayMsg{},
		OpenModelSelectorMsg{}, OpenSettingsMsg{}, OpenKeyEntryMsg{}, KeyEntryResultMsg{},
		DiffPreviewRequestMsg{}, DiffPreviewResponseMsg{},
		MultiDiffPreviewRequestMsg{}, MultiDiffPreviewResponseMsg{},
		GitStatusRequestMsg{}, GitStatusActionMsg{}, GitStatusDiffMsg{},
		SearchResultsRequestMsg{}, SearchResultsActionMsg{},
		FileBrowserRequestMsg{}, FileBrowserActionMsg{},
		ProgressUpdateMsg{}, ProgressCompleteMsg{}, ProgressActionMsg{},
		SettingToggleResultMsg{}, CoordinatedTaskCleanupMsg{},
		ScratchpadMsg(""), InitialPromptMsg(""),

		// Hostile payloads: text whose width is not its length, and counts that
		// no sane producer sends.
		StreamTextMsg(wide), StreamThinkingMsg(wide), ScratchpadMsg(wide),
		QueuedCountMsg(-1), QueuedCountMsg(9999),
		ToolCallMsg{Name: wide, Args: map[string]any{"path": wide}},
		ToolResultMsg{Name: wide, Content: wide, Failed: true, Error: wide,
			Diff: wide, DiffAdded: -1, DiffRemoved: -1},
		ToolProgressMsg{Name: wide, Progress: -1, Elapsed: time.Hour},
		StatusUpdateMsg{Message: wide},
		QueuedMessageRejectedMsg{Message: wide, Reason: wide, Waiting: -1},

		// Terminal events.
		tea.WindowSizeMsg{Width: 0, Height: 0},
		tea.WindowSizeMsg{Width: 1, Height: 1},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyEsc},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(wide)},
	}

	states := []State{
		StateInput, StateProcessing, StateStreaming, StatePermissionPrompt,
		StateQuestionPrompt, StatePlanApproval, StateModelSelector,
		StateShortcutsOverlay, StateCommandPalette, StateDiffPreview,
		StateMultiDiffPreview, StateSearchResults, StateGitStatus,
		StateFileBrowser, StateBatchProgress, StateContextObservatory,
		StateFilePeek, StateSettings, StateAPIKeyEntry, StateNotificationCenter,
	}
	sizes := [][2]int{{0, 0}, {1, 1}, {3, 2}, {80, 24}}

	for si, st := range states {
		for _, size := range sizes {
			for mi, msg := range msgs {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("Update panicked: state index %d, size %dx%d, message %d (%T): %v",
								si, size[0], size[1], mi, msg, r)
						}
					}()
					m := NewModel()
					m.width, m.height = size[0], size[1]
					m.state = st
					updated, _ := m.Update(msg)
					// Rendering right after is part of the same crash surface:
					// a message that leaves the model inconsistent shows up in
					// View, not in Update.
					if um, ok := updated.(Model); ok {
						_ = um.View()
					}
				}()
			}
		}
	}
}
