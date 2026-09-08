package router

import (
	"testing"

	"gokin/internal/config"
)

// A terse imperative is not a question, however few words it is. The score<=2
// fallback typed anything short as a Question, and a Question routes to
// StrategyDirect — whose thinking budget is zero on the grounds that it is "a
// pure conversational answer". "fix the failing test" is four words and none
// of that is true of it, so the most ordinary request shape there is ran with
// reasoning switched off on exactly the models that need it.
func TestDetermineTaskType_TerseImperativeIsNotAQuestion(t *testing.T) {
	ta := NewTaskAnalyzer(4, 3)
	for _, msg := range []string{
		"почини падающий тест",
		"fix the failing test",
		"удали этот файл",
		"delete the dead branch",
		"перепиши эту функцию",
	} {
		analysis := ta.Analyze(msg)
		if analysis.Strategy == StrategyDirect {
			t.Errorf("%q routed to the no-reasoning strategy; it is work, not a question "+
				"(score=%d type=%s)", msg, analysis.Score, analysis.Type)
		}
	}
}

// The other half: a short question must still take the cheap path, or the fix
// would have traded a wrong answer for the opposite one and spent reasoning
// budget on "list all tools".
func TestDetermineTaskType_ShortQuestionsStayDirect(t *testing.T) {
	ta := NewTaskAnalyzer(4, 3)
	for _, msg := range []string{
		"где определяется DefaultConfig",
		"what does executeLoop do",
		"list all tools",
		"покажи конфиг",
	} {
		if analysis := ta.Analyze(msg); analysis.Strategy != StrategyDirect {
			t.Errorf("%q is a question and should take the cheap path, got %s (score=%d)",
				msg, analysis.Strategy, analysis.Score)
		}
	}
}

// The consequence the routing decides, asserted where it lands.
func TestSelectThinkingBudget_TerseImperativeKeepsReasoning(t *testing.T) {
	ta := NewTaskAnalyzer(4, 3)
	auto := &Router{thinkingMode: config.ThinkingModeAuto}

	analysis := ta.Analyze("почини падающий тест")
	if b := auto.selectThinkingBudget(analysis); b == 0 {
		t.Fatalf("a repair request runs with reasoning off (strategy=%s score=%d)",
			analysis.Strategy, analysis.Score)
	}
}
