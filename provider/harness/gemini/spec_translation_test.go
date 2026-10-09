package gemini

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestBuildSpawnPlan_Minimal(t *testing.T) {
	t.Parallel()
	plan, err := buildSpawnPlan(agent.Spec{Prompt: "hello", AllowedTools: []string{}}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if len(plan.initialContents) != 1 {
		t.Fatalf("initialContents: want 1, got %d", len(plan.initialContents))
	}
	if plan.initialContents[0].Role != "user" {
		t.Errorf("Role: want user, got %q", plan.initialContents[0].Role)
	}
	if plan.initialContents[0].Parts[0].Text != "hello" {
		t.Errorf("Text: want hello, got %q", plan.initialContents[0].Parts[0].Text)
	}
	if plan.systemInstruction != nil {
		t.Errorf("systemInstruction: want nil, got %#v", plan.systemInstruction)
	}
	if plan.tools != nil {
		t.Errorf("tools: want nil for no-tools spec, got %#v", plan.tools)
	}
	if plan.generationConfig != nil {
		t.Errorf("generationConfig: want nil for minimal spec, got %#v", plan.generationConfig)
	}
}

func TestBuildSpawnPlan_EmptyPromptRejected(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"", "   ", "\n\t"} {
		if _, err := buildSpawnPlan(agent.Spec{Prompt: p}, "gemini-3.5-flash"); err == nil {
			t.Errorf("prompt %q: want error, got nil", p)
		}
	}
}

func TestBuildSpawnPlan_SystemInstruction(t *testing.T) {
	t.Parallel()
	plan, err := buildSpawnPlan(agent.Spec{
		Prompt:             "do work",
		BaseInstructions:   "you are a helpful agent",
		SystemPromptAppend: "follow ENG-1500",
	}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.systemInstruction == nil {
		t.Fatal("systemInstruction: want non-nil")
	}
	text := plan.systemInstruction.Parts[0].Text
	if !contains(text, "you are a helpful agent") || !contains(text, "ENG-1500") {
		t.Errorf("systemInstruction text missing parts: %q", text)
	}
}

func TestToolsFromSpec_AllowedToolsAndMCP(t *testing.T) {
	t.Parallel()
	tools := toolsFromSpec(agent.Spec{
		AllowedTools: []string{"Bash(git:*)", "Edit", "Read"},
		MCPToolNames: []string{"mcp__af-code-intelligence__af_code_get_repo_map"},
		MCPServers: []agent.MCPServerConfig{
			{Name: "af-linear", Command: "rensei", Args: []string{"linear", "mcp"}},
		},
	})
	if len(tools) != 1 {
		t.Fatalf("tools: want 1 grouped entry, got %d", len(tools))
	}
	names := sortedToolNames(tools)
	want := []string{
		"Bash", "Edit", "Read",
		"mcp__af-code-intelligence__af_code_get_repo_map",
		"mcp__af-linear",
	}
	if !equalStrings(names, want) {
		t.Errorf("function names: want %v, got %v", want, names)
	}
	// Every declaration must carry a parameters object (Gemini requires it).
	for _, d := range tools[0].FunctionDeclarations {
		if d.Parameters == nil {
			t.Errorf("declaration %q: want non-nil parameters", d.Name)
		}
	}
}

func TestToolsFromSpec_NoToolsReturnsNil(t *testing.T) {
	t.Parallel()
	// An explicitly empty allow list (what a one-shot completion sends)
	// declares no tools; a nil one is unconfigured and offers the native
	// surface (TestToolsFromSpecOffersTheNativeSurfaceWithoutAnAllowGate).
	if got := toolsFromSpec(agent.Spec{Prompt: "hi", AllowedTools: []string{}}); got != nil {
		t.Errorf("toolsFromSpec: want nil for no-tools spec, got %#v", got)
	}
}

func TestBuildSpawnPlan_ToolConfigModeAuto(t *testing.T) {
	t.Parallel()
	plan, err := buildSpawnPlan(agent.Spec{
		Prompt:       "work",
		Autonomous:   true,
		AllowedTools: []string{"Edit"},
	}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.toolConfig == nil || plan.toolConfig.FunctionCallingConfig == nil {
		t.Fatal("toolConfig: want non-nil with functionCallingConfig")
	}
	if got := plan.toolConfig.FunctionCallingConfig.Mode; got != functionCallingModeAuto {
		t.Errorf("mode: want AUTO, got %q", got)
	}
}

// TestThinkingConfig_ModelFamilySelection verifies that level-knob models
// (major >= 3, plus unversioned ids) get a thinkingLevel and budget-knob
// models (2.x) get a thinkingBudget for the same effort.
func TestThinkingConfig_ModelFamilySelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		model      string
		effort     agent.EffortLevel
		wantLevel  string
		wantBudget *int
		wantNilSet bool
	}{
		{name: "3.5-flash high → level high", model: "gemini-3.5-flash", effort: agent.EffortHigh, wantLevel: "high"},
		{name: "3.1-pro medium → level medium", model: "gemini-3.1-pro-preview", effort: agent.EffortMedium, wantLevel: "medium"},
		{name: "3.x low → level low", model: "gemini-3.1-flash-lite", effort: agent.EffortLow, wantLevel: "low"},
		{name: "4-pro medium → level medium", model: "gemini-4-pro", effort: agent.EffortMedium, wantLevel: "medium"},
		{name: "4.1-flash high → level high", model: "gemini-4.1-flash", effort: agent.EffortHigh, wantLevel: "high"},
		{name: "4 xhigh → level high", model: "gemini-4-pro", effort: agent.EffortXHigh, wantLevel: "high"},
		{name: "unversioned latest → level", model: "gemini-flash-latest", effort: agent.EffortMedium, wantLevel: "medium"},
		{name: "2.5-pro high → budget 24576", model: "gemini-2.5-pro", effort: agent.EffortHigh, wantBudget: intp(24576)},
		{name: "2.5-flash medium → budget 8192", model: "gemini-2.5-flash", effort: agent.EffortMedium, wantBudget: intp(8192)},
		{name: "2.5 low → budget 2048", model: "gemini-2.5-flash-lite", effort: agent.EffortLow, wantBudget: intp(2048)},
		{name: "no effort → nil", model: "gemini-3.5-flash", effort: "", wantNilSet: true},
		{name: "no effort on 4.x → nil", model: "gemini-4-pro", effort: "", wantNilSet: true},
		{name: "3.x max → level high, never below", model: "gemini-3.1-pro-preview", effort: agent.EffortMax, wantLevel: "high"},
		{name: "2.5 max → budget 24576, never below", model: "gemini-2.5-pro", effort: agent.EffortMax, wantBudget: intp(24576)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc := tc
			plan, err := buildSpawnPlan(agent.Spec{Prompt: "x", Effort: tc.effort}, tc.model)
			if err != nil {
				t.Fatalf("buildSpawnPlan: %v", err)
			}
			if tc.wantNilSet {
				if plan.generationConfig != nil && plan.generationConfig.ThinkingConfig != nil {
					t.Fatalf("thinkingConfig: want nil, got %#v", plan.generationConfig.ThinkingConfig)
				}
				return
			}
			if plan.generationConfig == nil || plan.generationConfig.ThinkingConfig == nil {
				t.Fatal("thinkingConfig: want non-nil")
			}
			tc2 := plan.generationConfig.ThinkingConfig
			if tc.wantLevel != "" {
				if tc2.ThinkingLevel != tc.wantLevel {
					t.Errorf("thinkingLevel: want %q, got %q", tc.wantLevel, tc2.ThinkingLevel)
				}
				if tc2.ThinkingBudget != nil {
					t.Errorf("thinkingBudget: want nil for 3.x, got %d", *tc2.ThinkingBudget)
				}
			}
			if tc.wantBudget != nil {
				if tc2.ThinkingBudget == nil || *tc2.ThinkingBudget != *tc.wantBudget {
					t.Errorf("thinkingBudget: want %d, got %v", *tc.wantBudget, tc2.ThinkingBudget)
				}
				if tc2.ThinkingLevel != "" {
					t.Errorf("thinkingLevel: want empty for 2.5, got %q", tc2.ThinkingLevel)
				}
			}
		})
	}
}

func TestMaxOutputTokens_ProviderConfigWins(t *testing.T) {
	t.Parallel()
	turns := 3
	plan, err := buildSpawnPlan(agent.Spec{
		Prompt:         "x",
		MaxTurns:       &turns,
		ProviderConfig: map[string]any{"maxOutputTokens": float64(12000)},
	}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.generationConfig == nil {
		t.Fatal("generationConfig: want non-nil")
	}
	if plan.generationConfig.MaxOutputTokens != 12000 {
		t.Errorf("MaxOutputTokens: want 12000 (ProviderConfig wins), got %d", plan.generationConfig.MaxOutputTokens)
	}
}

func TestMaxOutputTokens_MaxTurnsFallback(t *testing.T) {
	t.Parallel()
	turns := 3
	plan, err := buildSpawnPlan(agent.Spec{Prompt: "x", MaxTurns: &turns}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.generationConfig == nil || plan.generationConfig.MaxOutputTokens != 6144 {
		t.Errorf("MaxOutputTokens: want 6144 (3*2048), got %#v", plan.generationConfig)
	}
}

func TestCalculateCostUSD(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		model  string
		in     int64
		cached int64
		out    int64
		pc     map[string]any
		want   float64
	}{
		{name: "3.5-flash uncached", model: "gemini-3.5-flash", in: 1_000_000, out: 1_000_000, want: 1.50 + 9.00},
		{name: "3.1-pro input only", model: "gemini-3.1-pro-preview", in: 1_000_000, want: 2.00},
		{name: "2.5-flash-lite uncached", model: "gemini-2.5-flash-lite", in: 1_000_000, out: 1_000_000, want: 0.10 + 0.40},
		{name: "3.6-flash flash rate", model: "gemini-3.6-flash", in: 1_000_000, out: 1_000_000, want: 0.75 + 3.75},
		{name: "3.7-flash flash rate", model: "gemini-3.7-flash", in: 1_000_000, out: 1_000_000, want: 0.75 + 3.75},
		{name: "3.8-flash flash rate", model: "gemini-3.8-flash", in: 1_000_000, out: 1_000_000, want: 0.75 + 3.75},
		{name: "cached slice priced at cached rate", model: "gemini-3.6-flash", in: 1_000_000, cached: 400_000, out: 1_000_000, want: (600_000.0 / 1_000_000 * 0.75) + (400_000.0 / 1_000_000 * 0.075) + 3.75},
		{name: "legacy table entry prices cached at input rate", model: "gemini-3.5-flash", in: 1_000_000, cached: 500_000, out: 0, want: 1.50},
		{name: "dispatcher prices win", model: "gemini-4-pro", in: 1_000_000, cached: 200_000, out: 1_000_000, pc: map[string]any{"inputPricePer1M": 2.0, "cachedPricePer1M": 0.5, "outputPricePer1M": 8.0}, want: (800_000.0 / 1_000_000 * 2.0) + (200_000.0 / 1_000_000 * 0.5) + 8.0},
		{name: "cached-only override applies", model: "gemini-3.5-flash", in: 1_000_000, cached: 1_000_000, out: 0, pc: map[string]any{"cachedPricePer1M": 0.25}, want: 0.25},
		{name: "cached-only override on unknown model", model: "unknown-model", in: 1_000_000, cached: 1_000_000, out: 0, pc: map[string]any{"cachedPricePer1M": 0.25}, want: 0.25},
		{name: "partial override keeps table side", model: "gemini-3.5-flash", in: 1_000_000, out: 1_000_000, pc: map[string]any{"outputPricePer1M": 5.0}, want: 1.50 + 5.00},
		{name: "unknown → 0", model: "unknown-model", in: 1_000_000, out: 1_000_000, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := calculateCostUSD(tc.in, tc.cached, tc.out, tc.model, tc.pc)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("calculateCostUSD(%s, in=%d cached=%d out=%d): want %g, got %g", tc.model, tc.in, tc.cached, tc.out, tc.want, got)
			}
		})
	}
}

// TestGeminiMajorVersion_Routing pins the family parse behind the
// thinking-knob switch: later majors route to the level knob, 2.x keeps
// the budget knob, and shapes without a numeric major default to level.
func TestGeminiMajorVersion_Routing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model     string
		wantMajor int
		wantOK    bool
		wantLevel bool
	}{
		{model: "gemini-4-pro", wantMajor: 4, wantOK: true, wantLevel: true},
		{model: "gemini-4.1-flash", wantMajor: 4, wantOK: true, wantLevel: true},
		{model: "gemini-3.5-flash", wantMajor: 3, wantOK: true, wantLevel: true},
		{model: "gemini-2.5-pro", wantMajor: 2, wantOK: true, wantLevel: false},
		{model: "gemini-2.5-flash-lite", wantMajor: 2, wantOK: true, wantLevel: false},
		{model: "gemini-flash-latest", wantOK: false, wantLevel: true},
		{model: "", wantOK: false, wantLevel: true},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			major, ok := geminiMajorVersion(tc.model)
			if ok != tc.wantOK || (ok && major != tc.wantMajor) {
				t.Errorf("geminiMajorVersion(%q) = (%d, %t), want (%d, %t)", tc.model, major, ok, tc.wantMajor, tc.wantOK)
			}
			if got := usesThinkingLevel(tc.model); got != tc.wantLevel {
				t.Errorf("usesThinkingLevel(%q) = %t, want %t", tc.model, got, tc.wantLevel)
			}
		})
	}
}

func intp(v int) *int { return &v }

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildSpawnPlan_NativeResponseSchema(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}},"required":["v"]}`)
	plan, err := buildSpawnPlan(agent.Spec{Prompt: "x", ResponseSchema: schema}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.generationConfig == nil {
		t.Fatal("generationConfig nil; want native responseSchema set")
	}
	if plan.generationConfig.ResponseMimeType != "application/json" {
		t.Errorf("ResponseMimeType = %q, want application/json", plan.generationConfig.ResponseMimeType)
	}
	if string(plan.generationConfig.ResponseSchema) != string(schema) {
		t.Errorf("ResponseSchema = %q, want %q", plan.generationConfig.ResponseSchema, schema)
	}
}

func TestBuildSpawnPlan_NoResponseSchema_NoStructuredFields(t *testing.T) {
	t.Parallel()
	plan, err := buildSpawnPlan(agent.Spec{Prompt: "x"}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.generationConfig != nil &&
		(plan.generationConfig.ResponseMimeType != "" || plan.generationConfig.ResponseSchema != nil) {
		t.Errorf("structured fields set without ResponseSchema: %#v", plan.generationConfig)
	}
}
