package ui

import (
	"strings"
	"testing"
)

// A panic in View() kills the user's session outright, and the arithmetic that
// causes one is everywhere: any count derived from `width - const` fed to
// strings.Repeat or a slice bound is a crash the moment the terminal is
// narrower than the constant. Width 0 is not hypothetical either — it is the
// value every Model carries until the first WindowSizeMsg lands, so the first
// frame of every session runs this math. The same class has shipped before,
// both as negative repeat counts across four render paths and as hand-written
// `runes[:n-3]` underflows at seven call sites.
//
// This sweeps every State across hostile sizes with content whose WIDTH is not
// its length — CJK, emoji, and long unbroken runs — because a rendering bug
// that only needs a narrow terminal will not appear with the empty model a
// focused test constructs. It costs half a second, has no exemption list to
// keep current, and can only fail on an actual panic.
func TestViewSurvivesHostileTerminalSizes(t *testing.T) {
	states := []struct {
		name string
		s    State
	}{
		{"Input", StateInput}, {"Processing", StateProcessing}, {"Streaming", StateStreaming},
		{"PermissionPrompt", StatePermissionPrompt}, {"QuestionPrompt", StateQuestionPrompt},
		{"PlanApproval", StatePlanApproval}, {"ModelSelector", StateModelSelector},
		{"ShortcutsOverlay", StateShortcutsOverlay}, {"CommandPalette", StateCommandPalette},
		{"DiffPreview", StateDiffPreview}, {"MultiDiffPreview", StateMultiDiffPreview},
		{"SearchResults", StateSearchResults}, {"GitStatus", StateGitStatus},
		{"FileBrowser", StateFileBrowser}, {"BatchProgress", StateBatchProgress},
		{"ContextObservatory", StateContextObservatory}, {"FilePeek", StateFilePeek},
		{"Settings", StateSettings}, {"APIKeyEntry", StateAPIKeyEntry},
		{"NotificationCenter", StateNotificationCenter},
	}
	// 61 sits just past the 60-column gate the bordered cards use, so both
	// sides of that branch are covered.
	widths := []int{0, 1, 2, 3, 4, 5, 8, 12, 20, 40, 59, 61, 80}
	heights := []int{0, 1, 2, 3, 5, 24}

	for _, st := range states {
		for _, w := range widths {
			for _, h := range heights {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("View() panicked in state %s at %dx%d: %v", st.name, w, h, r)
						}
					}()
					m := NewModel()
					m.width, m.height = w, h
					m.state = st.s
					m.output.AppendLine("日本語のとても長い行" + strings.Repeat("漢字", 40))
					m.output.AppendLine("🎉🎉🎉 " + strings.Repeat("x", 200))
					m.output.AppendLine("short")
					m.todoItems = []string{"一つ目のタスク", strings.Repeat("long todo ", 20), "done"}
					m.currentTool = "bash"
					m.currentToolInfo = strings.Repeat("path/segment/", 30)
					m.processingLabel = "実行中"
					m.currentActivity = strings.Repeat("activity ", 25)
					m.agentRecentTools = []string{"read", "grep", "edit", "bash", "write"}
					m.streamIdleMsg = "provider is quiet"
					m.workDir = "/" + strings.Repeat("deep/", 40)
					_ = m.View()
				}()
			}
		}
	}
}
