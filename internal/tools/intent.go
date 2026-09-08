package tools

import "strings"

// Discuss-mode (v0.100.51): a foreground interactive guard so the agent does not
// jump into IMPLEMENTATION (writing/editing code) when the user is merely
// ANALYZING/discussing. It is deliberately SOFT (ask-once on the first mutation,
// then proceed) and CONTEXT-AWARE (a clear imperative bypasses it entirely).
// Autonomous paths (/loop, sub-agents) and headless never engage it.

// implementationTools is the set of code-/repo-MUTATING tools gated in
// discuss-mode (the "going into implementation" signal). It is intentionally
// NARROWER than the executor's writeTools: it EXCLUDES `bash` and `run_tests`,
// which during an analysis are verification/exploration (`go test`, `go build`)
// — gating those would turn into an annoying "confirm to run the tests". The
// trade-off (a model could write via `bash` `cat > file` and dodge the gate) is
// accepted: the real "реализация" path is edit/write/refactor, which IS gated.
var implementationTools = map[string]bool{
	"write":      true,
	"edit":       true,
	"delete":     true,
	"move":       true,
	"copy":       true,
	"mkdir":      true,
	"refactor":   true,
	"batch":      true,
	"git_commit": true,
	"git_add":    true,
	"ssh":        true,
}

// IsImplementationTool reports whether name writes/mutates code or repo state —
// the discuss-mode gate set. bash/run_tests are deliberately excluded so
// verification stays free during analysis.
func IsImplementationTool(name string) bool { return implementationTools[name] }

// discussFrames are analytical/exploratory framings. When ANY is present the
// turn is discussion EVEN IF action verbs also appear — "analyze how to fix X"
// is discussion (the verb is the subject of analysis, not a command).
var discussFrames = []string{
	// English
	"analyze", "analyse", "let's think", "lets think", "think about",
	"think through", "what do you think", "what are your thoughts",
	"how would", "how should", "how do you", "how does", "how is",
	"how to", "what's the best", "what is the best", "best way", "best approach",
	"let's discuss", "lets discuss", "can we discuss", "should we", "should i",
	"what are the options", "what options", "pros and cons", "trade-off", "tradeoff",
	"explain", "walk me through", "review the", "compare",
	"evaluate", "assess", "is it possible", "is it worth", "what if", "why ",
	"what is", "what are", "what's", "any ideas", "any thoughts", "thoughts?",
	// Russian
	"проанализир", "анализ", "давай подума", "подума", "как думаешь",
	"что думаешь", "как бы", "как лучше", "лучший способ", "лучший подход",
	"давай обсуд", "обсуд", "стоит ли", "какие вариант", "за и против",
	"объясни", "почему", "что такое", "как устроен", "как работает",
	"можно ли", "оцени", "сравни", "разбер", "рассмотр",
	"что лучше", "имеет ли смысл", "как бы ты",
}

// softDiscussFrames ask the agent to look before acting. Alone they mean
// analysis, but paired with an explicit imperative they are a preamble to the
// work, not a request for deliberation — so they are checked AFTER
// actionImperatives and lose to them.
var softDiscussFrames = []string{
	"take a look", "look at", "посмотри", "глянь",
}

// actionImperatives are clear "do it now" commands. When present (and no discuss
// frame), the turn is action — never gated.
var actionImperatives = []string{
	// English
	"implement", "go ahead", "do it", "just do", "make the change",
	"make the changes", "apply", "ship it", "fix ", "fix the", "add ", "create ",
	"write ", "refactor ", "rename ", "delete ", "remove ", "build it",
	"let's do", "lets do", "let's implement", "lets implement", "proceed",
	"now do", "now implement", "go for it", "make it so",
	// Russian
	"реализу", "сделай", "сделать это", "почини", "исправь", "исправить",
	"добавь", "напиши", "создай", "переименуй", "удали", "примени",
	"давай сделаем", "давай реализ", "вперёд", "вперед", "запили", "поправь",
	"измени", "перепиши", "внеси", "сделаем это", "го ", "погнали",
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ClassifyTurnDiscuss reports whether a user message is ANALYTICAL/discussion
// (true) rather than a request to IMPLEMENT (false). Pure + heuristic, so it is
// instant and unit-testable; the caller supplies the router's inferred
// conversationMode ("exploring"/"implementing"/…) for the ambiguous tail.
//
// Order (each rule wins over the ones below it):
//  1. An analytical FRAME ("analyze", "how would", "what do you think",
//     "проанализируй", "как лучше"…) → discussion, even if action verbs appear.
//  2. A clear action IMPERATIVE ("implement", "fix the", "сделай", "go ahead")
//     with no frame → action (never gated).
//  3. Ambiguous (neither): lean on context — if the conversation has been
//     EXPLORING (read-only so far), continue in discussion; otherwise action.
//
// Bias note: the gate is ask-once/soft, so a discussion false-positive costs the
// user a single confirm — cheap. A fresh ambiguous task (no exploring history)
// defaults to action so normal "here's a bug" requests are not gated.
func ClassifyTurnDiscuss(message, conversationMode string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	// Frames that ask for an OPINION settle the turn on their own. Frames that
	// only ask the agent to LOOK FIRST do not: "посмотри и исправь падающий
	// тест" and "take a look and fix the failing test" are unambiguous work
	// requests with a preamble, and they used to be read as analysis — so the
	// agent explored, then hit a confirmation prompt on its own first edit.
	if containsAny(lower, discussFrames) {
		return true
	}
	if containsAny(lower, actionImperatives) {
		return false
	}
	if containsAny(lower, softDiscussFrames) {
		return true
	}
	return conversationMode == "exploring"
}
