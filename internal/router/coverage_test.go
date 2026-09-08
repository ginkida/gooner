package router

import (
	"strings"
	"testing"
)

// ===========================================================================
// helpers.go — ExecutionStrategy.String / IsValid / RequiresTools / RequiresMultipleAgents (all 0%)
// ===========================================================================

func TestExecutionStrategy_String(t *testing.T) {
	cases := []struct {
		s    ExecutionStrategy
		want string
	}{
		{StrategyDirect, string(StrategyDirect)},
		{StrategySingleTool, string(StrategySingleTool)},
		{StrategyExecutor, string(StrategyExecutor)},
		{StrategySubAgent, string(StrategySubAgent)},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestExecutionStrategy_IsValid(t *testing.T) {
	valid := []ExecutionStrategy{StrategyDirect, StrategySingleTool, StrategyExecutor, StrategySubAgent}
	for _, s := range valid {
		if !s.IsValid() {
			t.Errorf("%v.IsValid() = false, want true", s)
		}
	}
	invalid := ExecutionStrategy("bogus")
	if invalid.IsValid() {
		t.Error("bogus strategy should be invalid")
	}
}

// TestExecutionStrategy_RequiresTools is gone with the predicate it covered.
// Its central assertion — "StrategyDirect should not require tools" — was
// never true of a released build: executeDirect runs the same executor as the
// other strategies, with core tools and memory. What the strategy actually
// selects is pinned by TestSelectToolSets_DirectStrategyStillCarriesCoreTools.

func TestExecutionStrategy_RequiresMultipleAgents(t *testing.T) {
	// No coordinator support — always false for all strategies.
	for _, s := range []ExecutionStrategy{StrategyDirect, StrategySingleTool, StrategyExecutor, StrategySubAgent} {
		if s.RequiresMultipleAgents() {
			t.Errorf("%v.RequiresMultipleAgents() = true, want false (no coordinator)", s)
		}
	}
}

func TestExecutionStrategy_GetDescription(t *testing.T) {
	cases := []struct {
		s    ExecutionStrategy
		want string
	}{
		{StrategyDirect, "Direct response (core tools, fast model)"},
		{StrategySingleTool, "Single tool call"},
		{StrategyExecutor, "Standard execution"},
		{StrategySubAgent, "Specialized agent"},
		{ExecutionStrategy("bogus"), "Unknown strategy"},
	}
	for _, tc := range cases {
		if got := tc.s.GetDescription(); got != tc.want {
			t.Errorf("%v.GetDescription() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// ===========================================================================
// helpers.go — TaskType.String / GetDescription (0%)
// ===========================================================================

func TestTaskType_String(t *testing.T) {
	cases := []struct {
		t    TaskType
		want string
	}{
		{TaskTypeQuestion, "question"},
		{TaskTypeComplex, "complex"},
		{TaskTypeRefactoring, "refactoring"},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestTaskType_GetDescription(t *testing.T) {
	cases := []struct {
		t    TaskType
		want string
	}{
		{TaskTypeQuestion, "Simple question"},
		{TaskTypeSingleTool, "Single tool"},
		{TaskTypeMultiTool, "Multiple tools"},
		{TaskTypeExploration, "Code exploration"},
		{TaskTypeRefactoring, "Refactoring"},
		{TaskTypeComplex, "Complex task"},
		{TaskTypeBackground, "Background task"},
		{TaskType("bogus"), "Unknown type"},
	}
	for _, tc := range cases {
		if got := tc.t.GetDescription(); got != tc.want {
			t.Errorf("%v.GetDescription() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// ===========================================================================
// helpers.go — TaskComplexity.FormatReasoning / getEmoji (0%)
// ===========================================================================

func TestTaskComplexity_GetEmoji(t *testing.T) {
	cases := []struct {
		taskType TaskType
		want     string
	}{
		{TaskTypeQuestion, "❓"},
		{TaskTypeSingleTool, "🔧"},
		{TaskTypeMultiTool, "🔧🔧"},
		{TaskTypeExploration, "🔍"},
		{TaskTypeRefactoring, "♻️"},
		{TaskTypeComplex, "🚀"},
		{TaskTypeBackground, "⏳"},
		{TaskType("bogus"), "📝"},
	}
	for _, tc := range cases {
		tc_ := &TaskComplexity{Type: tc.taskType}
		if got := tc_.getEmoji(); got != tc.want {
			t.Errorf("getEmoji(%v) = %q, want %q", tc.taskType, got, tc.want)
		}
	}
}

func TestTaskComplexity_FormatReasoning(t *testing.T) {
	tc := &TaskComplexity{
		Type:      TaskTypeComplex,
		Strategy:  StrategySubAgent,
		Reasoning: "Complex multi-step task",
	}
	got := tc.FormatReasoning()
	// Should contain the emoji, reasoning text, and strategy.
	if got == "" {
		t.Fatal("FormatReasoning returned empty string")
	}
	if !strings.HasPrefix(got, "🚀") {
		t.Errorf("FormatReasoning should start with 🚀 emoji for Complex, got %q", got[:4])
	}
	// Must reference the strategy.
	if !strings.Contains(got, string(StrategySubAgent)) {
		t.Errorf("FormatReasoning should contain strategy %q, got %q", StrategySubAgent, got)
	}
}

// ===========================================================================
// analyzer.go — SetLLMClient / SetLLMConfig (0%)
// ===========================================================================

func TestTaskAnalyzer_SetLLMClient(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	if ta.llmClient != nil {
		t.Fatal("llmClient should be nil initially")
	}
	// SetLLMClient accepts any client.Client implementation; pass nil to verify
	// the field is assigned (the setter itself doesn't nil-check).
	ta.SetLLMClient(nil)
	// The setter assigns unconditionally — llmClient is now nil (was nil).
	// To verify the assignment path ran, we check that no panic occurred and
	// the DecomposeWithContext path still works (it nil-checks before use).
	result := ta.DecomposeWithContext(t.Context(), "read file A and read file B")
	if result == nil {
		t.Fatal("DecomposeWithContext returned nil after SetLLMClient(nil)")
	}
}

func TestTaskAnalyzer_SetLLMConfig(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	original := ta.llmConfig
	if original == nil {
		t.Fatal("llmConfig should be non-nil by default (DefaultLLMDecomposerConfig)")
	}
	newCfg := &LLMDecomposerConfig{Enabled: false, MaxSubtasks: 99}
	ta.SetLLMConfig(newCfg)
	if ta.llmConfig != newCfg {
		t.Error("SetLLMConfig did not assign the new config")
	}
	if ta.llmConfig.MaxSubtasks != 99 {
		t.Errorf("llmConfig.MaxSubtasks = %d, want 99", ta.llmConfig.MaxSubtasks)
	}
}

// ===========================================================================
// analyzer.go — calculateScore edge cases (70% → higher)
// ===========================================================================

func TestCalculateScore_BaseOne(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	// Very short message: no keywords, no multi-instruction, no git → score 1.
	score := ta.calculateScore("hi")
	if score != 1 {
		t.Errorf("calculateScore('hi') = %d, want 1 (base only)", score)
	}
}

func TestCalculateScore_WordCountTiers(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	// 21-50 words → +1
	medium := makeLongMessage(30)
	s1 := ta.calculateScore(medium)
	if s1 < 2 {
		t.Errorf("30-word message score = %d, want >= 2 (base+length)", s1)
	}
	// 51-100 words → +2
	long := makeLongMessage(60)
	s2 := ta.calculateScore(long)
	if s2 < 3 {
		t.Errorf("60-word message score = %d, want >= 3", s2)
	}
	// >100 words → +3
	veryLong := makeLongMessage(120)
	s3 := ta.calculateScore(veryLong)
	if s3 < 4 {
		t.Errorf("120-word message score = %d, want >= 4", s3)
	}
}

func TestCalculateScore_CappedAtTen(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	// Construct a message that hits every scoring factor:
	// length (>100 words → +3), complexity keyword (+3), create keyword (+2),
	// multiple instructions (+2), git indicator (+1) = 1+3+3+2+2+1 = 12 → capped 10.
	msg := "analyze and investigate this. create a new module! refactor the code? " +
		"git merge branch " + makeLongMessage(120)
	score := ta.calculateScore(msg)
	if score != 10 {
		t.Errorf("calculateScore(overloaded message) = %d, want 10 (capped)", score)
	}
}

func TestCalculateScore_GitIndicator(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	score := ta.calculateScore("show me the git diff")
	// base(1) + show/find keyword(1) + git(1) = 3
	if score < 3 {
		t.Errorf("calculateScore('show me the git diff') = %d, want >= 3", score)
	}
}

func makeLongMessage(words int) string {
	msg := ""
	for i := 0; i < words; i++ {
		msg += "word "
	}
	return msg
}

// ===========================================================================
// analyzer.go — DecomposeWithContext edge cases (47% → higher)
// ===========================================================================

func TestDecomposeWithContext_EmptyMessage(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	result := ta.DecomposeWithContext(t.Context(), "")
	if result == nil {
		t.Fatal("result is nil")
	}
	if len(result.Subtasks) != 0 {
		t.Errorf("empty message should produce 0 subtasks, got %d", len(result.Subtasks))
	}
}

func TestDecomposeWithContext_MultilineNotDecomposed(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	msg := "line one\nline two\nline three"
	result := ta.DecomposeWithContext(t.Context(), msg)
	if len(result.Subtasks) != 0 {
		t.Errorf("multiline message should not decompose, got %d subtasks", len(result.Subtasks))
	}
	if result.Reasoning == "" {
		t.Error("multiline message should have reasoning explaining it was kept whole")
	}
}

func TestDecomposeWithContext_AndPatternParallel(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	result := ta.DecomposeWithContext(t.Context(), "read file A and read file B")
	if len(result.Subtasks) != 2 {
		t.Fatalf("expected 2 parallel subtasks, got %d", len(result.Subtasks))
	}
	if !result.CanParallel {
		t.Error("and-pattern should be parallel")
	}
}

func TestDecomposeWithContext_ThenPatternSequential(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	result := ta.DecomposeWithContext(t.Context(), "first read the config then update the code")
	if len(result.Subtasks) < 2 {
		t.Fatalf("expected >=2 sequential subtasks, got %d", len(result.Subtasks))
	}
	if result.CanParallel {
		t.Error("then-pattern should NOT be parallel")
	}
	// Second subtask should depend on the first.
	if len(result.Subtasks) >= 2 && len(result.Subtasks[1].Dependencies) == 0 {
		t.Error("second subtask should have a dependency on the first")
	}
}

// ===========================================================================
// analyzer.go — decomposeByType branches (61% → higher)
// ===========================================================================

func TestDecomposeByType_RefactoringPipeline(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	analysis := &TaskComplexity{Type: TaskTypeRefactoring, Score: 7}
	subs := ta.decomposeByType("refactor the auth module", analysis)
	if len(subs) < 3 {
		t.Errorf("Refactoring should decompose into >=3 (explore,plan,execute), got %d", len(subs))
	}
	// Verify sequential dependencies.
	hasDeps := false
	for _, s := range subs {
		if len(s.Dependencies) > 0 {
			hasDeps = true
			break
		}
	}
	if !hasDeps {
		t.Error("Refactoring subtasks should have sequential dependencies")
	}
}

func TestDecomposeByType_ComplexWithTest(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	analysis := &TaskComplexity{Type: TaskTypeComplex, Score: 8}
	subs := ta.decomposeByType("implement feature and run test", analysis)
	if len(subs) < 3 {
		t.Errorf("Complex+test should decompose into >=3 (explore,implement,test), got %d", len(subs))
	}
}

func TestDecomposeByType_ComplexWithoutTest(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	analysis := &TaskComplexity{Type: TaskTypeComplex, Score: 8}
	subs := ta.decomposeByType("implement a new feature", analysis)
	if len(subs) < 2 {
		t.Errorf("Complex (no test) should decompose into >=2 (explore,execute), got %d", len(subs))
	}
}

func TestDecomposeByType_MultiToolWithCreate(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	analysis := &TaskComplexity{Type: TaskTypeMultiTool, Score: 5}
	subs := ta.decomposeByType("create a new handler", analysis)
	if len(subs) < 2 {
		t.Errorf("MultiTool+create should decompose into >=2, got %d", len(subs))
	}
}

func TestDecomposeByType_DefaultSingleSubtask(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	analysis := &TaskComplexity{Type: TaskTypeSingleTool, Score: 3}
	subs := ta.decomposeByType("do something", analysis)
	if len(subs) != 1 {
		t.Errorf("default should produce exactly 1 subtask, got %d", len(subs))
	}
}

// ===========================================================================
// analyzer.go — selectAgentType (0%)
// ===========================================================================

func TestSelectAgentType_AllTypes(t *testing.T) {
	ta := NewTaskAnalyzer(6, 8)
	cases := []struct {
		taskType TaskType
		want     string
	}{
		{TaskTypeExploration, "explore"},
		{TaskTypeBackground, "bash"},
		{TaskTypeRefactoring, "general"},
		{TaskTypeComplex, "general"},
		{TaskTypeQuestion, "general"},
		{TaskTypeSingleTool, "general"},
	}
	for _, tc := range cases {
		if got := ta.selectAgentType(tc.taskType); got != tc.want {
			t.Errorf("selectAgentType(%v) = %q, want %q", tc.taskType, got, tc.want)
		}
	}
}

// ===========================================================================
// router.go — RecordTurn / ResetDepth / GetDepthPressure (0%)
// ===========================================================================

func TestRouter_RecordTurnAccumulates(t *testing.T) {
	r := &Router{}
	r.RecordTurn(3, false)
	r.RecordTurn(2, false)
	if r.depthTurns != 2 {
		t.Errorf("depthTurns = %d, want 2", r.depthTurns)
	}
	if r.depthTools != 5 {
		t.Errorf("depthTools = %d, want 5", r.depthTools)
	}
}

func TestRouter_RecordTurnPressure(t *testing.T) {
	r := &Router{}
	r.RecordTurn(0, true)
	if !r.depthPressure {
		t.Error("depthPressure should be true after RecordTurn(_, true)")
	}
	r.RecordTurn(0, false)
	if r.depthPressure {
		t.Error("depthPressure should be false after RecordTurn(_, false)")
	}
}

func TestRouter_ResetDepth(t *testing.T) {
	r := &Router{}
	r.RecordTurn(5, true)
	r.ResetDepth()
	if r.depthTurns != 0 {
		t.Errorf("depthTurns after reset = %d, want 0", r.depthTurns)
	}
	if r.depthTools != 0 {
		t.Errorf("depthTools after reset = %d, want 0", r.depthTools)
	}
	if r.depthPressure {
		t.Error("depthPressure should be false after reset")
	}
}

// ===========================================================================
// router.go — GetAnalysis (0%)
// ===========================================================================

func TestRouter_GetAnalysis(t *testing.T) {
	r := &Router{analyzer: NewTaskAnalyzer(6, 8)}
	analysis := r.GetAnalysis("what is Go?")
	if analysis == nil {
		t.Fatal("GetAnalysis returned nil")
	}
	if analysis.Type != TaskTypeQuestion {
		t.Errorf("Type = %v, want question", analysis.Type)
	}
}

// ===========================================================================
// router.go — TrackOperation / GetConversationMode / GetErrorRate (77% → higher)
// ===========================================================================

func TestRouter_TrackOperation_ExploringMode(t *testing.T) {
	r := &Router{}
	r.TrackOperation("grep", true)
	r.TrackOperation("read", true)
	if mode := r.GetConversationMode(); mode != "exploring" {
		t.Errorf("mode = %q, want 'exploring'", mode)
	}
}

func TestRouter_TrackOperation_ImplementingMode(t *testing.T) {
	r := &Router{}
	r.TrackOperation("write", true)
	if mode := r.GetConversationMode(); mode != "implementing" {
		t.Errorf("mode = %q, want 'implementing'", mode)
	}
}

func TestRouter_TrackOperation_DebuggingMode(t *testing.T) {
	r := &Router{}
	// Need >2 recent errors with bash to trigger debugging mode.
	r.TrackOperation("bash", false)
	r.TrackOperation("bash", false)
	r.TrackOperation("bash", false)
	if mode := r.GetConversationMode(); mode != "debugging" {
		t.Errorf("mode = %q, want 'debugging'", mode)
	}
}

func TestRouter_TrackOperation_ResetsAfter20(t *testing.T) {
	r := &Router{}
	for i := 0; i < 20; i++ {
		r.TrackOperation("read", false) // all failures
	}
	// After 20 ops, counters reset to 0.
	if r.recentOps != 0 {
		t.Errorf("recentOps after 20 = %d, want 0 (reset)", r.recentOps)
	}
	if r.recentErrors != 0 {
		t.Errorf("recentErrors after 20 = %d, want 0 (reset)", r.recentErrors)
	}
}

func TestRouter_GetConversationMode_Default(t *testing.T) {
	r := &Router{}
	// No operations yet is distinct from a real read-only exploration. The
	// discuss classifier uses empty mode to let fresh ambiguous tasks act.
	if mode := r.GetConversationMode(); mode != "" {
		t.Errorf("default mode = %q, want empty", mode)
	}
}

func TestRouter_GetErrorRate(t *testing.T) {
	r := &Router{}
	r.TrackOperation("read", true)
	r.TrackOperation("read", false)
	// 1 error out of 2 ops = 0.5
	if rate := r.GetErrorRate(); rate != 0.5 {
		t.Errorf("GetErrorRate = %v, want 0.5", rate)
	}
}

// ===========================================================================
// router.go — toolHint (0% if uncovered)
// ===========================================================================

func TestRouter_ToolHint(t *testing.T) {
	r := &Router{}
	if hint := r.toolHint(&TaskComplexity{Type: TaskTypeExploration}); hint == "" {
		t.Error("Exploration should produce a non-empty hint")
	}
	if hint := r.toolHint(&TaskComplexity{Type: TaskTypeRefactoring}); hint == "" {
		t.Error("Refactoring should produce a non-empty hint")
	}
	if hint := r.toolHint(&TaskComplexity{Type: TaskTypeQuestion}); hint != "" {
		t.Error("Question should produce an empty hint")
	}
}

// ===========================================================================
// router.go — selectCostAwareModel (0%)
// ===========================================================================

func TestRouter_SelectCostAwareModel(t *testing.T) {
	r := &Router{fastModel: "fast-model"}
	// Direct → fast model
	if got := r.selectCostAwareModel(&TaskComplexity{Strategy: StrategyDirect, Score: 1}); got != "fast-model" {
		t.Errorf("Direct strategy = %q, want fast-model", got)
	}
	// SingleTool low score → fast model
	if got := r.selectCostAwareModel(&TaskComplexity{Strategy: StrategySingleTool, Score: 2}); got != "fast-model" {
		t.Errorf("SingleTool score 2 = %q, want fast-model", got)
	}
	// SingleTool high score → default (empty)
	if got := r.selectCostAwareModel(&TaskComplexity{Strategy: StrategySingleTool, Score: 5}); got != "" {
		t.Errorf("SingleTool score 5 = %q, want '' (primary)", got)
	}
	// Executor → default (empty)
	if got := r.selectCostAwareModel(&TaskComplexity{Strategy: StrategyExecutor, Score: 6}); got != "" {
		t.Errorf("Executor = %q, want '' (primary)", got)
	}
}

// ===========================================================================
// router.go — RecordRoutingOutcome / getStrategySuccessRate (58% → higher)
// ===========================================================================

func TestRouter_RecordRoutingOutcome_AndSuccessRate(t *testing.T) {
	r := &Router{routingHistory: make([]routingRecord, 0, 100)}
	a1 := &TaskComplexity{Strategy: StrategyExecutor}
	a2 := &TaskComplexity{Strategy: StrategyExecutor}
	r.RecordRoutingOutcome("task1", a1, true)
	r.RecordRoutingOutcome("task2", a2, false)
	// 1 success / 2 total = 0.5
	if rate := r.getStrategySuccessRate(StrategyExecutor); rate != 0.5 {
		t.Errorf("success rate = %v, want 0.5", rate)
	}
}

func TestRouter_RecordRoutingOutcome_CapsAt100(t *testing.T) {
	r := &Router{routingHistory: make([]routingRecord, 0, 100)}
	a := &TaskComplexity{Strategy: StrategyExecutor}
	for i := 0; i < 120; i++ {
		r.RecordRoutingOutcome("task", a, true)
	}
	// History should be capped at 100.
	if len(r.routingHistory) != 100 {
		t.Errorf("history len = %d, want 100 (capped)", len(r.routingHistory))
	}
}

func TestRouter_GetStrategySuccessRate_NoHistory(t *testing.T) {
	r := &Router{routingHistory: make([]routingRecord, 0, 100)}
	// No history → returns 0.5 ("not enough data" sentinel, < 3 samples).
	if rate := r.getStrategySuccessRate(StrategyExecutor); rate != 0.5 {
		t.Errorf("success rate with no history = %v, want 0.5 (not enough data)", rate)
	}
}
