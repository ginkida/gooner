package donegate

import "testing"

// looksLikeCodingTask decides one narrow thing: whether a turn in which bash
// was the only mutating tool gets verified. Every other mutating tool enforces
// unconditionally, so the whole weight of this function falls on bash-driven
// edits — sed -i, patch, formatters, generators. A miss there means the agent
// rewrote files and finished without a build ever running.
//
// The list is vocabulary, so it can only ever be as good as its coverage. It
// was measured against realistic phrasings and missed fourteen of nineteen,
// including "почини тест", "rename Foo to Bar" and "add a retry to the client".
func TestLooksLikeCodingTask_CoversOrdinaryChangeRequests(t *testing.T) {
	for _, msg := range []string{
		"fix the failing test",
		"implement retry",
		"refactor this",
		"add a retry to the client",
		"make it work",
		"rename Foo to Bar",
		"remove the dead branch",
		"write a parser for the header",
		"delete the unused helper",
		"apply the patch",
		"почини тест",
		"перепиши функцию",
		"убери дубли",
		"сделай так, чтобы компилировалось",
		"добавь ретрай в клиент",
		"доработай приложение",
		"исправь ошибку",
	} {
		if !looksLikeCodingTask(msg) {
			t.Errorf("bash-driven edits would finish unverified for %q", msg)
		}
	}
}

// The other half of the trade, and the reason the list holds change verbs
// rather than every word that appears near code: bash is also how the agent
// investigates. A question must not drag a build into a turn that only read
// things — the same reasoning that keeps bash out of IsImplementationTool.
func TestLooksLikeCodingTask_QuestionsDoNotDragInVerification(t *testing.T) {
	for _, msg := range []string{
		"why does this crash",
		"the tests are red",
		"",
		"what does this function return",
		"где лежит конфиг",
	} {
		if looksLikeCodingTask(msg) {
			t.Errorf("%q is not a change request; verification must stay out of analysis", msg)
		}
	}
}
