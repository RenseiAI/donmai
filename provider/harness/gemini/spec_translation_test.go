package gemini

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestBuildSpawnPlan_Minimal(t *testing.T) {
	t.Parallel()
	plan, err := buildSpawnPlan(agent.Spec{Prompt: "hello"}, "gemini-3.5-flash")
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
		SystemPromptAppend: "follow REN-1500",
	}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	if plan.systemInstruction == nil {
		t.Fatal("systemInstruction: want non-nil")
	}
	text := plan.systemInstruction.Parts[0].Text
	if !contains(text, "you are a helpful agent") || !contains(text, "REN-1500") {
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
	if got := toolsFromSpec(agent.Spec{Prompt: "hi"}); got != nil {
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

// TestThinkingConfig_ModelFamilySelection verifies that 3.x models get a
// thinkingLevel and 2.5 models get a thinkingBudget for the same effort.
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
		{name: "2.5-pro high → budget 24576", model: "gemini-2.5-pro", effort: agent.EffortHigh, wantBudget: intp(24576)},
		{name: "2.5-flash medium → budget 8192", model: "gemini-2.5-flash", effort: agent.EffortMedium, wantBudget: intp(8192)},
		{name: "2.5 low → budget 2048", model: "gemini-2.5-flash-lite", effort: agent.EffortLow, wantBudget: intp(2048)},
		{name: "no effort → nil", model: "gemini-3.5-flash", effort: "", wantNilSet: true},
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
		model   string
		in, out int64
		want    float64
	}{
		{"gemini-3.5-flash", 1_000_000, 1_000_000, 1.50 + 9.00},
		{"gemini-3.1-pro-preview", 1_000_000, 0, 2.00},
		{"gemini-2.5-flash-lite", 1_000_000, 1_000_000, 0.10 + 0.40},
		{"unknown-model", 1_000_000, 1_000_000, 0}, // unknown → 0, path wired
	}
	for _, tc := range tests {
		got := calculateCostUSD(tc.in, tc.out, tc.model)
		if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("calculateCostUSD(%s, %d, %d): want %g, got %g", tc.model, tc.in, tc.out, tc.want, got)
		}
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
