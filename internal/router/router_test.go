package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"gokin/internal/config"
	"gokin/internal/testkit"
	"gokin/internal/tools"
)

// TestSelectThinkingBudget_Modes pins that ThinkingMode overrides the adaptive
// default at the extremes: off → never reason; on → reason even on an easy task
// the adaptive path skips; auto (default) → adaptive.
func TestSelectThinkingBudget_Modes(t *testing.T) {
	hard := &TaskComplexity{Strategy: StrategyExecutor, Score: 6}
	easy := &TaskComplexity{Strategy: StrategyDirect, Score: 1}

	off := &Router{thinkingMode: config.ThinkingModeOff}
	if b := off.selectThinkingBudget(hard); b != 0 {
		t.Errorf("off mode hard task = %d, want 0 (never reason)", b)
	}

	on := &Router{thinkingMode: config.ThinkingModeOn}
	if b := on.selectThinkingBudget(easy); b < 4096 {
		t.Errorf("on mode easy task = %d, want >= 4096 (floored on)", b)
	}

	auto := &Router{thinkingMode: config.ThinkingModeAuto}
	if b := auto.selectThinkingBudget(easy); b != 0 {
		t.Errorf("auto mode easy/Direct task = %d, want 0", b)
	}
	if b := auto.selectThinkingBudget(hard); b == 0 {
		t.Error("auto mode hard task = 0, want > 0 (reason on hard tasks)")
	}
}

func TestInferModelCapability(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		wantTier CapabilityTier
	}{
		// Strong tier
		{"glm", "glm-5-plus", CapabilityStrong},
		{"kimi", "k3", CapabilityStrong},              // K3 flagship, Coding Plan endpoint
		{"kimi", "k3-1m", CapabilityStrong},           // K3 variant
		{"kimi", "kimi-k3", CapabilityStrong},         // K3, prefixed form
		{"kimi", "kimi-for-coding", CapabilityStrong}, // K2.7, Coding Plan endpoint
		{"kimi", "kimi-k2.6", CapabilityStrong},
		{"kimi", "kimi-k2.7", CapabilityStrong},
		{"kimi", "kimi-k2.8", CapabilityStrong},
		{"kimi", "kimi-k2.10-preview", CapabilityStrong},
		{"deepseek", "deepseek-v4-pro", CapabilityStrong},

		// Medium tier
		{"kimi", "kimi-k2.5", CapabilityMedium},
		{"kimi", "kimi-k2-thinking", CapabilityMedium},
		{"minimax", "MiniMax-M2.5", CapabilityMedium},
		{"glm", "glm-4", CapabilityMedium},
		{"deepseek", "deepseek-v4-flash", CapabilityMedium},
		{"deepseek", "deepseek-chat", CapabilityMedium},
		{"deepseek", "deepseek-reasoner", CapabilityMedium},

		// Weak tier
		{"ollama", "llama3.2", CapabilityWeak},
		{"unknown", "some-model", CapabilityWeak},
		{"", "", CapabilityWeak},
	}

	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			cap := InferModelCapability(tt.provider, tt.model)
			if cap.Tier != tt.wantTier {
				t.Errorf("InferModelCapability(%q, %q).Tier = %v, want %v",
					tt.provider, tt.model, cap.Tier, tt.wantTier)
			}
			if cap.Provider != tt.provider {
				t.Errorf("Provider = %q, want %q", cap.Provider, tt.provider)
			}
			if cap.ModelName != tt.model {
				t.Errorf("ModelName = %q, want %q", cap.ModelName, tt.model)
			}
		})
	}
}

func TestSetModelCapability_UpdatesLiveRoutingPolicy(t *testing.T) {
	r := &Router{
		thinkingMode:    config.ThinkingModeAuto,
		modelCapability: InferModelCapability("ollama", "llama3.2"),
	}
	hard := &TaskComplexity{Strategy: StrategyExecutor, Score: 5}
	if got := r.selectThinkingBudget(hard); got != 12288 {
		t.Fatalf("weak-model thinking budget = %d, want 12288", got)
	}

	r.SetModelCapability(InferModelCapability("glm", "glm-5.2"))
	if got := r.selectThinkingBudget(hard); got != 8192 {
		t.Fatalf("GLM-5.2 thinking budget after live switch = %d, want 8192", got)
	}

	sets := []tools.ToolSet{tools.ToolSetCore, tools.ToolSetAgent, tools.ToolSetAdvanced}
	if got := r.filterToolSetsByCapability(sets); len(got) != len(sets) {
		t.Fatalf("GLM-5.2 should retain strong-tier tool sets, got %v", got)
	}
}

func TestSelectToolSetsForMessageUsesAdaptiveHybridPolicy(t *testing.T) {
	r := &Router{engineMode: "auto"}
	analysis := &TaskComplexity{Strategy: StrategyExecutor, Type: TaskTypeExploration}

	sets := r.selectToolSetsForMessage(analysis, "fix the auth bug")
	if hasAdaptiveToolSet(sets, tools.ToolSetHybrid) || hasAdaptiveToolSet(sets, tools.ToolSetHarness) {
		t.Fatalf("ordinary auto request received hybrid sets: %v", sets)
	}

	sets = r.selectToolSetsForMessage(analysis, "Rank repository files by how many TODO comments they contain")
	if !hasAdaptiveToolSet(sets, tools.ToolSetHybrid) || hasAdaptiveToolSet(sets, tools.ToolSetHarness) {
		t.Fatalf("aggregation auto request sets = %v, want hybrid without harness", sets)
	}
	for _, targeted := range []string{
		"Count TODO lines in this file only",
		"Compare `pair/left.json` with `pair/right.json` in this repository",
	} {
		sets = r.selectToolSetsForMessage(analysis, targeted)
		if hasAdaptiveToolSet(sets, tools.ToolSetHybrid) || hasAdaptiveToolSet(sets, tools.ToolSetHarness) {
			t.Fatalf("targeted auto request %q received hybrid sets: %v", targeted, sets)
		}
	}

	r.SetEngineMode("hybrid")
	sets = r.selectToolSetsForMessage(analysis, "fix the auth bug")
	if !hasAdaptiveToolSet(sets, tools.ToolSetHybrid) || !hasAdaptiveToolSet(sets, tools.ToolSetHarness) {
		t.Fatalf("explicit hybrid request sets = %v, want hybrid and harness", sets)
	}
}

func TestExecuteWithPolicyMessageKeepsHybridExposureStableAcrossRetryScaffolding(t *testing.T) {
	tests := []struct {
		name          string
		policyMessage string
		retryMessage  string
		planMode      bool
		schemaCeiling []string
		wantREPL      bool
	}{
		{
			name:          "eligible request stays eligible",
			policyMessage: "Count TODOs per directory across the repository",
			retryMessage:  "[System note: continue after the interrupted edit attempt.] Fix only the response formatting.",
			wantREPL:      true,
		},
		{
			name:          "ordinary request cannot gain repl from continuation",
			policyMessage: "Fix the authentication error in this file",
			retryMessage:  "[System note: previous response counted TODOs across every repository file.] Continue the fix.",
			wantREPL:      false,
		},
		{
			name:          "plan mode keeps original eligibility",
			policyMessage: "Count TODOs per directory across the repository",
			retryMessage:  "[System note: continue after the interrupted edit attempt.] Fix only the response formatting.",
			planMode:      true,
			wantREPL:      true,
		},
		{
			name:          "plan mode cannot gain repl from continuation",
			policyMessage: "Explain this function",
			retryMessage:  "[System note: count TODOs across all repository files before continuing.]",
			planMode:      true,
			wantREPL:      false,
		},
		{
			name:          "request schema ceiling cannot be widened",
			policyMessage: "Count TODOs per directory across the repository",
			retryMessage:  "Count TODOs per directory across the repository",
			schemaCeiling: []string{"read", "grep"},
			wantREPL:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := testkit.NewMockClient()
			mock.EnqueueText("done")
			registry := tools.DefaultRegistry(t.TempDir())
			executor := tools.NewExecutor(registry, mock, time.Second)
			router := NewRouter(&RouterConfig{
				Enabled: true, DecomposeThreshold: 100, ParallelThreshold: 100, EngineMode: "auto",
			}, executor, nil, mock, registry, false, t.TempDir())
			router.SetPlanMode(test.planMode)

			ctx := context.Background()
			if test.schemaCeiling != nil {
				ctx = tools.ContextWithToolSchemaCeiling(ctx, executor, test.schemaCeiling)
			}
			if _, _, err := router.ExecuteWithPolicyMessage(
				ctx, nil, test.retryMessage, test.policyMessage,
			); err != nil {
				t.Fatalf("ExecuteWithPolicyMessage: %v", err)
			}
			gotREPL := declNames(mock.GetTools())["repl_exec"]
			if gotREPL != test.wantREPL {
				t.Fatalf("repl exposure = %t, want %t for policy %q and retry %q",
					gotREPL, test.wantREPL, test.policyMessage, test.retryMessage)
			}
		})
	}
}

func hasAdaptiveToolSet(sets []tools.ToolSet, want tools.ToolSet) bool {
	for _, set := range sets {
		if set == want {
			return true
		}
	}
	return false
}

func TestCapabilityTierAdjustments(t *testing.T) {
	weak := InferModelCapability("ollama", "llama3.2")
	if weak.DecomposeAdjust != -2 {
		t.Errorf("weak DecomposeAdjust = %d, want -2", weak.DecomposeAdjust)
	}
	if weak.ThinkingMultiplier != 1.5 {
		t.Errorf("weak ThinkingMultiplier = %f, want 1.5", weak.ThinkingMultiplier)
	}
	if !weak.SelfReviewBoost {
		t.Error("weak SelfReviewBoost should be true")
	}

	medium := InferModelCapability("kimi", "kimi-k2.5")
	if medium.DecomposeAdjust != -1 {
		t.Errorf("medium DecomposeAdjust = %d, want -1", medium.DecomposeAdjust)
	}
	if medium.ThinkingMultiplier != 1.2 {
		t.Errorf("medium ThinkingMultiplier = %f, want 1.2", medium.ThinkingMultiplier)
	}

	strong := InferModelCapability("glm", "glm-5.1")
	if strong.DecomposeAdjust != 0 {
		t.Errorf("strong DecomposeAdjust = %d, want 0", strong.DecomposeAdjust)
	}
	if strong.ThinkingMultiplier != 1.0 {
		t.Errorf("strong ThinkingMultiplier = %f, want 1.0", strong.ThinkingMultiplier)
	}
}

func TestCapabilityTierString(t *testing.T) {
	tests := []struct {
		tier CapabilityTier
		want string
	}{
		{CapabilityWeak, "weak"},
		{CapabilityMedium, "medium"},
		{CapabilityStrong, "strong"},
		{CapabilityTier(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("CapabilityTier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestExecutionStrategyHelpers(t *testing.T) {
	// IsValid
	if !StrategyDirect.IsValid() {
		t.Error("StrategyDirect should be valid")
	}
	if ExecutionStrategy("invalid").IsValid() {
		t.Error("invalid strategy should not be valid")
	}

	// RequiresTools is gone, and this is where its claim was pinned: the
	// assertion read "StrategyDirect should not require tools", which was never
	// true of a released build. Coverage of a falsehood is how it survives.

	// GetDescription
	if desc := StrategyDirect.GetDescription(); desc != "Direct response (core tools, fast model)" {
		t.Errorf("StrategyDirect.GetDescription() = %q", desc)
	}
}

func TestTaskTypeHelpers(t *testing.T) {
	if desc := TaskTypeQuestion.GetDescription(); desc != "Simple question" {
		t.Errorf("TaskTypeQuestion.GetDescription() = %q", desc)
	}
	if desc := TaskTypeComplex.GetDescription(); desc != "Complex task" {
		t.Errorf("TaskTypeComplex.GetDescription() = %q", desc)
	}
	if desc := TaskType("unknown").GetDescription(); desc != "Unknown type" {
		t.Errorf("unknown type GetDescription() = %q", desc)
	}
}

func TestFilterToolSetsByCapability(t *testing.T) {
	allSets := []tools.ToolSet{
		tools.ToolSetCore,
		tools.ToolSetGit,
		tools.ToolSetFileOps,
		tools.ToolSetAdvanced,
		tools.ToolSetWeb,
		tools.ToolSetPlanning,
		tools.ToolSetMemory,
	}

	// Strong: all sets pass through
	r := &Router{modelCapability: &ModelCapability{Tier: CapabilityStrong}}
	filtered := r.filterToolSetsByCapability(allSets)
	if len(filtered) != len(allSets) {
		t.Errorf("strong tier: got %d sets, want %d", len(filtered), len(allSets))
	}

	// Medium: Core, Git, FileOps, Advanced, Memory, Web, Planning.
	// Agent set is the only stripped one (orchestration primitives).
	r.modelCapability.Tier = CapabilityMedium
	filtered = r.filterToolSetsByCapability(allSets)
	allowed := map[tools.ToolSet]bool{
		tools.ToolSetCore: true, tools.ToolSetGit: true,
		tools.ToolSetFileOps: true, tools.ToolSetAdvanced: true,
		tools.ToolSetMemory: true, tools.ToolSetWeb: true,
		tools.ToolSetPlanning: true,
	}
	for _, s := range filtered {
		if !allowed[s] {
			t.Errorf("medium tier: unexpected tool set %v", s)
		}
	}
	if len(filtered) != 7 {
		t.Errorf("medium tier: got %d sets, want 7", len(filtered))
	}

	// Weak: Core, Git, FileOps, Memory, Web, Planning (no Advanced).
	r.modelCapability.Tier = CapabilityWeak
	filtered = r.filterToolSetsByCapability(allSets)
	weakAllowed := map[tools.ToolSet]bool{
		tools.ToolSetCore: true, tools.ToolSetGit: true,
		tools.ToolSetFileOps: true, tools.ToolSetMemory: true,
		tools.ToolSetWeb: true, tools.ToolSetPlanning: true,
	}
	for _, s := range filtered {
		if !weakAllowed[s] {
			t.Errorf("weak tier: unexpected tool set %v", s)
		}
	}
	if len(filtered) != 6 {
		t.Errorf("weak tier: got %d sets, want 6", len(filtered))
	}

	// Nil capability: all sets pass through
	r.modelCapability = nil
	filtered = r.filterToolSetsByCapability(allSets)
	if len(filtered) != len(allSets) {
		t.Errorf("nil capability: got %d sets, want %d", len(filtered), len(allSets))
	}
}

func TestSelectThinkingBudget(t *testing.T) {
	r := &Router{}

	tests := []struct {
		name     string
		analysis *TaskComplexity
		wantZero bool
	}{
		{
			name:     "direct strategy has no budget",
			analysis: &TaskComplexity{Strategy: StrategyDirect, Score: 1},
			wantZero: true,
		},
		{
			name:     "simple single tool has no budget",
			analysis: &TaskComplexity{Strategy: StrategySingleTool, Score: 1},
			wantZero: true,
		},
		{
			name:     "complex single tool gets budget",
			analysis: &TaskComplexity{Strategy: StrategySingleTool, Score: 3},
			wantZero: false,
		},
		{
			name:     "executor gets budget",
			analysis: &TaskComplexity{Strategy: StrategyExecutor, Score: 4},
			wantZero: false,
		},
		{
			name:     "sub-agent gets largest budget",
			analysis: &TaskComplexity{Strategy: StrategySubAgent, Score: 8},
			wantZero: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget := r.selectThinkingBudget(tt.analysis)
			if tt.wantZero && budget != 0 {
				t.Errorf("budget = %d, want 0", budget)
			}
			if !tt.wantZero && budget == 0 {
				t.Error("budget = 0, want > 0")
			}
		})
	}

	// Test thinking multiplier for weak model
	r.modelCapability = &ModelCapability{Tier: CapabilityWeak, ThinkingMultiplier: 1.5}
	budget := r.selectThinkingBudget(&TaskComplexity{Strategy: StrategySubAgent, Score: 8})
	if budget != 12288 { // 8192 (SubAgent floor) * 1.5 weak-model multiplier
		t.Errorf("weak model budget = %d, want 12288", budget)
	}
}

// TestNewRouterWiresModelCapability pins the v0.98.x fix: the LIVE router gets
// ModelCapability from its config. Before, builder.go only set it on the unused
// SmartRouter, so r.modelCapability was nil on the live router and the
// weak-model thinking multiplier / capability adaptations never fired.
func TestNewRouterWiresModelCapability(t *testing.T) {
	capb := &ModelCapability{Tier: CapabilityWeak, ThinkingMultiplier: 1.5}
	r := NewRouter(&RouterConfig{Enabled: true, ModelCapability: capb}, nil, nil, nil, nil, false, "")
	if r.modelCapability != capb {
		t.Fatal("NewRouter did not wire RouterConfig.ModelCapability into r.modelCapability")
	}
}

func TestSelectCostAwareModel(t *testing.T) {
	r := &Router{
		costAware: true,
		fastModel: "gemini-flash",
	}

	// Direct uses fast model
	model := r.selectCostAwareModel(&TaskComplexity{Strategy: StrategyDirect})
	if model != "gemini-flash" {
		t.Errorf("direct strategy model = %q, want %q", model, "gemini-flash")
	}

	// Simple single tool uses fast model
	model = r.selectCostAwareModel(&TaskComplexity{Strategy: StrategySingleTool, Score: 1})
	if model != "gemini-flash" {
		t.Errorf("simple single tool model = %q, want %q", model, "gemini-flash")
	}

	// Complex single tool uses default
	model = r.selectCostAwareModel(&TaskComplexity{Strategy: StrategySingleTool, Score: 5})
	if model != "" {
		t.Errorf("complex single tool model = %q, want empty", model)
	}

	// Executor uses default
	model = r.selectCostAwareModel(&TaskComplexity{Strategy: StrategyExecutor, Score: 5})
	if model != "" {
		t.Errorf("executor model = %q, want empty", model)
	}
}

func TestRouterErrorRateTracking(t *testing.T) {
	r := &Router{}

	// No operations: error rate is 0
	if rate := r.GetErrorRate(); rate != 0 {
		t.Errorf("initial error rate = %f, want 0", rate)
	}

	// Track some operations
	r.TrackOperation("read", true)
	r.TrackOperation("write", true)
	r.TrackOperation("bash", false)
	r.TrackOperation("bash", false)

	rate := r.GetErrorRate()
	expected := 2.0 / 4.0 // 2 errors out of 4 ops
	if rate != expected {
		t.Errorf("error rate = %f, want %f", rate, expected)
	}

	// Conversation mode should track tool usage
	r.TrackOperation("grep", true)
	if mode := r.GetConversationMode(); mode != "exploring" {
		t.Errorf("mode after grep = %q, want %q", mode, "exploring")
	}

	r.TrackOperation("edit", true)
	if mode := r.GetConversationMode(); mode != "implementing" {
		t.Errorf("mode after edit = %q, want %q", mode, "implementing")
	}
}

func TestRouterHistoryBounded(t *testing.T) {
	r := &Router{routingHistory: make([]routingRecord, 0, 100)}

	// Fill routing history beyond capacity
	for i := 0; i < 150; i++ {
		analysis := &TaskComplexity{Type: TaskTypeQuestion, Strategy: StrategyDirect}
		r.RecordRoutingOutcome("test", analysis, true)
	}

	r.historyMu.RLock()
	histLen := len(r.routingHistory)
	r.historyMu.RUnlock()

	if histLen > 100 {
		t.Errorf("history length = %d, should be bounded at 100", histLen)
	}
}

func TestToolHint(t *testing.T) {
	r := &Router{}

	// Exploration should get a hint
	hint := r.toolHint(&TaskComplexity{Type: TaskTypeExploration})
	if hint == "" {
		t.Error("exploration should get a tool hint")
	}

	// Refactoring should get a hint
	hint = r.toolHint(&TaskComplexity{Type: TaskTypeRefactoring})
	if hint == "" {
		t.Error("refactoring should get a tool hint")
	}

	// Question should not get a hint
	hint = r.toolHint(&TaskComplexity{Type: TaskTypeQuestion})
	if hint != "" {
		t.Errorf("question should not get a hint, got %q", hint)
	}
}

func TestToolHintForRequestDoesNotContradictExposedHybridTool(t *testing.T) {
	r := &Router{}
	analysis := &TaskComplexity{Type: TaskTypeExploration}
	candidate := "Count TODOs per directory across the repository"
	hint := r.toolHintForRequest(analysis, candidate, true)
	if !strings.Contains(hint, "repl_exec") || strings.Contains(hint, "prefer read, glob") {
		t.Fatalf("candidate hint contradicts hybrid schema: %q", hint)
	}
	if fallback := r.toolHintForRequest(analysis, candidate, false); !strings.Contains(fallback, "prefer read, glob") {
		t.Fatalf("unavailable REPL did not retain structured-tool hint: %q", fallback)
	}
	if ordinary := r.toolHintForRequest(analysis, "fix the auth bug", true); strings.Contains(ordinary, "repl_exec") {
		t.Fatalf("explicit hybrid exposure pushed REPL into an ordinary edit: %q", ordinary)
	}
}
