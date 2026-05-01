package codex

import (
	"reflect"
	"sort"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// TestSpecFieldCoverage asserts that every field on agent.Spec is
// either translated by NewSpawnPlan or explicitly listed in
// SpawnPlan.IgnoredFields. This is the cardinal-rule-10 guard rail:
// when agent.Spec grows a new field, this test fails until the codex
// provider takes a position on it.
func TestSpecFieldCoverage(t *testing.T) {
	t.Parallel()

	specType := reflect.TypeOf(agent.Spec{})
	allFields := make([]string, 0, specType.NumField())
	for i := 0; i < specType.NumField(); i++ {
		allFields = append(allFields, specType.Field(i).Name)
	}
	sort.Strings(allFields)

	// translatedFields lists every Spec field NewSpawnPlan does
	// translate to a JSON-RPC param. ignoredFields names every Spec
	// field that is intentionally NOT translated. The union must
	// equal allFields exactly — no orphans, no double-counting.
	translatedFields := []string{
		"Prompt",
		"Cwd",
		"Autonomous",
		"SandboxEnabled",
		"SandboxLevel",
		"MCPServers",
		"Model",
		"Effort",
		"BaseInstructions",
		"SystemPromptAppend",
	}

	// All fields ignoredSpecFields can return — independent of
	// whether the test Spec actually populates them — so the union
	// coverage check works regardless of input.
	ignoredFields := []string{
		"Env",
		"AllowedTools",
		"DisallowedTools",
		"MCPToolNames",
		"MaxTurns",
		"PermissionConfig",
		"CodeIntelEnforcement",
		"ProviderConfig",
		"SubAgentProvider",
		"OnProcessSpawned", // documented as honored at spawn time
	}
	all := append([]string{}, translatedFields...)
	all = append(all, ignoredFields...)
	sort.Strings(all)

	if !reflect.DeepEqual(all, allFields) {
		t.Fatalf("spec field coverage mismatch:\nall=%v\nrecorded=%v", allFields, all)
	}
}

func TestNewSpawnPlan_Defaults(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Prompt: "do work",
		Cwd:    "/tmp/wt",
	}
	plan := NewSpawnPlan(spec)

	// thread/start params
	if plan.ThreadStart["cwd"] != "/tmp/wt" {
		t.Fatalf("expected cwd=/tmp/wt, got %v", plan.ThreadStart["cwd"])
	}
	if plan.ThreadStart["approvalPolicy"] != "untrusted" {
		t.Fatalf("expected approvalPolicy=untrusted (non-autonomous), got %v", plan.ThreadStart["approvalPolicy"])
	}
	if plan.ThreadStart["model"] != DefaultCodexModel {
		t.Fatalf("expected default model %q, got %v", DefaultCodexModel, plan.ThreadStart["model"])
	}
	if plan.ThreadStart["serviceName"] != "agentfactory" {
		t.Fatalf("expected serviceName=agentfactory, got %v", plan.ThreadStart["serviceName"])
	}
	if _, ok := plan.ThreadStart["sandbox"]; ok {
		t.Fatalf("expected no sandbox by default, got %v", plan.ThreadStart["sandbox"])
	}

	// turn/start params: input carries the prompt
	in, _ := plan.TurnStart["input"].([]map[string]any)
	if len(in) != 1 || in[0]["text"] != "do work" {
		t.Fatalf("unexpected turn input: %v", plan.TurnStart["input"])
	}
}

func TestNewSpawnPlan_AutonomousFlipsApprovalPolicy(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{Prompt: "x", Cwd: "/tmp", Autonomous: true})
	if plan.ThreadStart["approvalPolicy"] != "on-request" {
		t.Fatalf("expected approvalPolicy=on-request, got %v", plan.ThreadStart["approvalPolicy"])
	}
	if plan.TurnStart["approvalPolicy"] != "on-request" {
		t.Fatalf("expected turn approvalPolicy=on-request, got %v", plan.TurnStart["approvalPolicy"])
	}
}

func TestNewSpawnPlan_SandboxLevels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level    agent.SandboxLevel
		threadV  string
		policyOk bool
	}{
		{agent.SandboxReadOnly, "read-only", true},
		{agent.SandboxWorkspaceWrite, "workspace-write", true},
		{agent.SandboxFullAccess, "danger-full-access", true},
	}
	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp", SandboxLevel: tt.level})
			if plan.ThreadStart["sandbox"] != tt.threadV {
				t.Fatalf("expected sandbox=%q, got %v", tt.threadV, plan.ThreadStart["sandbox"])
			}
			policy, ok := plan.TurnStart["sandboxPolicy"]
			if tt.policyOk && !ok {
				t.Fatalf("expected sandboxPolicy on turn/start, got none")
			}
			if !tt.policyOk && ok {
				t.Fatalf("did not expect sandboxPolicy, got %v", policy)
			}
		})
	}
}

func TestNewSpawnPlan_EffortPropagatesToTurn(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp", Effort: agent.EffortHigh})
	if plan.TurnStart["reasoningEffort"] != "high" {
		t.Fatalf("expected reasoningEffort=high, got %v", plan.TurnStart["reasoningEffort"])
	}
}

func TestNewSpawnPlan_BaseInstructionsAndSystemPromptAppend(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{
		Cwd:                "/tmp",
		BaseInstructions:   "RULES",
		SystemPromptAppend: "EXTRA",
	})
	got, _ := plan.ThreadStart["baseInstructions"].(string)
	if got != "RULES\n\nEXTRA" {
		t.Fatalf("expected RULES\\n\\nEXTRA, got %q", got)
	}
}

func TestNewSpawnPlan_MCPServers(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Cwd: "/tmp",
		MCPServers: []agent.MCPServerConfig{
			{Name: "af-linear", Command: "node", Args: []string{"server.js"}, Env: map[string]string{"FOO": "bar"}},
			{Name: "af-code", Command: "node", Args: []string{"code.js"}},
		},
	}
	plan := NewSpawnPlan(spec)
	if plan.MCPConfig == nil {
		t.Fatalf("expected MCPConfig, got nil")
	}
	linear, ok := plan.MCPConfig["af-linear"].(map[string]any)
	if !ok {
		t.Fatalf("missing af-linear: %v", plan.MCPConfig)
	}
	if linear["command"] != "node" {
		t.Fatalf("expected command=node, got %v", linear["command"])
	}
	if envMap, ok := linear["env"].(map[string]string); !ok || envMap["FOO"] != "bar" {
		t.Fatalf("expected env FOO=bar, got %v", linear["env"])
	}
}

func TestNewSpawnPlan_NoMCPServers(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp"})
	if plan.MCPConfig != nil {
		t.Fatalf("expected nil MCPConfig when MCPServers is empty, got %v", plan.MCPConfig)
	}
}

func TestNewSpawnPlan_ModelTierFallback(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{
		Cwd: "/tmp",
		Env: map[string]string{"CODEX_MODEL_TIER": "sonnet"},
	})
	if plan.ThreadStart["model"] != "gpt-5.2-codex" {
		t.Fatalf("expected sonnet→gpt-5.2-codex, got %v", plan.ThreadStart["model"])
	}
}

func TestNewSpawnPlan_ExplicitModelWins(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{
		Cwd:   "/tmp",
		Model: "gpt-5-codex-special",
		Env:   map[string]string{"CODEX_MODEL_TIER": "sonnet"},
	})
	if plan.ThreadStart["model"] != "gpt-5-codex-special" {
		t.Fatalf("expected explicit model to win, got %v", plan.ThreadStart["model"])
	}
}

func TestNewSpawnPlan_IgnoredFieldsRecorded(t *testing.T) {
	t.Parallel()
	maxTurns := 7
	spec := agent.Spec{
		Cwd:                  "/tmp",
		Env:                  map[string]string{"K": "V"},
		AllowedTools:         []string{"shell"},
		DisallowedTools:      []string{"Edit"},
		MCPToolNames:         []string{"mcp__foo__bar"},
		MaxTurns:             &maxTurns,
		PermissionConfig:     &agent.PermissionConfig{},
		CodeIntelEnforcement: &agent.CodeIntelEnforcement{EnforceUsage: true},
		ProviderConfig:       map[string]any{"x": 1},
		SubAgentProvider:     agent.ProviderClaude,
	}
	plan := NewSpawnPlan(spec)
	got := make(map[string]bool, len(plan.IgnoredFields))
	for _, n := range plan.IgnoredFields {
		got[n.Field] = true
	}
	for _, want := range []string{
		"Env", "AllowedTools", "DisallowedTools", "MCPToolNames",
		"MaxTurns", "PermissionConfig", "CodeIntelEnforcement",
		"ProviderConfig", "SubAgentProvider",
	} {
		if !got[want] {
			t.Errorf("expected ignored field %q in record, missing", want)
		}
	}
}
