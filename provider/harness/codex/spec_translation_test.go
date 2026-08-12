package codex

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
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
		"InitialContext",
		// ResponseSchema (the one-shot lane's structured-output schema, P4d)
		// rides turn/start.outputSchema — verified against the app-server v2
		// protocol (codex-cli 0.139.0 generate-json-schema). See
		// turnStartParams for the degrade path on older binaries.
		"ResponseSchema",
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
		"OnProcessSpawned",       // documented as honored at spawn time
		"PromptMode",             // selects the exact profile before NewSpawnPlan
		"PreparedHarness",        // consumed as sole authority by agent.PrepareHarness
		"PromptPlan",             // consumed by agent.PreparePrompt before NewSpawnPlan
		"PromptReceipt",          // populated by agent.PreparePrompt; not a JSON-RPC param
		"OnPromptAdapted",        // invoked by agent.PreparePrompt before NewSpawnPlan
		"ToolLifecyclePlan",      // consumed by agent.PrepareHarness before NewSpawnPlan
		"ToolLifecycleReceipt",   // populated by agent.PrepareHarness; not a JSON-RPC param
		"OnToolLifecycleAdapted", // invoked before provider side effects
		// Endpoint is the additive two-axis model-endpoint binding. The claude
		// and gemini harnesses read it (serving-host env knobs / URL routing);
		// codex takes its cardinal-rule-10 position here: Endpoint is
		// INTENTIONALLY IGNORED — the codex × openai azure cell needs the
		// CLI's config-file model_provider wiring (its own change), so the
		// codex Spawn translation stays byte-for-byte identical when Endpoint
		// is the zero value. Registered here (the static coverage mirror) so
		// this guard rail stays green; NewSpawnPlan is unchanged.
		"Endpoint",
		// Interactive (W4, interactive-attach-v1) is INTENTIONALLY IGNORED by
		// NewSpawnPlan/this JSON-RPC translation layer: Provider.Spawn
		// (codex.go) branches on Spec.Interactive != nil BEFORE NewSpawnPlan
		// ever runs, routing to SpawnInteractive (interactive.go) — the bare
		// `codex` TUI under a PTY, entirely independent of the app-server
		// JSON-RPC thread/turn machinery this file translates into. So
		// NewSpawnPlan's own translation table is correctly untouched by
		// Interactive; the field just never reaches it.
		"Interactive",
		// RequiresLiveNotice (the notice-delivery axis) is an ADMISSION input,
		// not a translation input: agent.ValidateSpecCapabilities refuses the
		// Spec before Spawn when the selected harness declares no way to be
		// reached, so by the time NewSpawnPlan runs the question is already
		// settled. codex's own answer lives on its manifest
		// (agent.NoticeDeliveryMCPRPC — the app-server JSON-RPC surface), not
		// in this per-turn param table.
		"RequiresLiveNotice",
		// AdditionalExtensions (ADR-2026-08-12-pi-extension-delivery-seam-and-
		// capability-pack-boundary.md D1) is INTENTIONALLY IGNORED by codex:
		// the seam it carries is for harnesses with a host-side extension API
		// that loads by explicit runner-owned path (pi's `-e`/`--no-extensions`
		// mechanism — provider/harness/pi). codex has no such surface — its
		// tool/MCP delivery is the app-server JSON-RPC thread/turn machinery
		// this file already translates — so a populated
		// Spec.AdditionalExtensions reaching codex has nothing to attach to.
		// Per D5.5 there is no cross-harness "supports extensions" capability;
		// this is codex's own cardinal-rule-10 position, taken here rather than
		// inferred from silence.
		"AdditionalExtensions",
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
	if plan.ThreadStart["serviceName"] != "donmai" {
		t.Fatalf("expected serviceName=donmai, got %v", plan.ThreadStart["serviceName"])
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

// TestNewSpawnPlan_InitialContextRidesTurnInput is the token-amplification
// guard: Spec.InitialContext (e.g. recalled agent memory) must be
// delivered ONCE via the first turn's input array and must NOT appear in
// thread/start developer instructions — which Codex re-includes in the model
// prompt on every turn. Folding it into that session-level field would
// produce O(turns × prefix) input-token blowup on long sessions.
func TestNewSpawnPlan_InitialContextRidesTurnInput(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Cwd:                "/tmp",
		Prompt:             "do work",
		InitialContext:     "MEMORY: prior decisions about X",
		SystemPromptAppend: "AGENT IDENTITY",
	}
	plan := NewSpawnPlan(spec)

	// developerInstructions carries ONLY the session-constant identity, never
	// the volatile InitialContext.
	developer, _ := plan.ThreadStart["developerInstructions"].(string)
	if !strings.Contains(developer, "AGENT IDENTITY") {
		t.Fatalf("developerInstructions should carry identity, got %q", developer)
	}
	if strings.Contains(developer, "MEMORY: prior decisions about X") {
		t.Fatalf("developerInstructions must NOT carry InitialContext, got %q", developer)
	}

	// turn/start input carries InitialContext as the FIRST part, then the
	// prompt — delivered once, then cached in conversation history.
	in, ok := plan.TurnStart["input"].([]map[string]any)
	if !ok || len(in) != 2 {
		t.Fatalf("expected 2 turn input parts (context + prompt), got %v", plan.TurnStart["input"])
	}
	if in[0]["text"] != "MEMORY: prior decisions about X" {
		t.Fatalf("expected first input part = InitialContext, got %v", in[0]["text"])
	}
	if in[1]["text"] != "do work" {
		t.Fatalf("expected second input part = prompt, got %v", in[1]["text"])
	}
}

// TestNewSpawnPlan_NoInitialContextEmitsPromptOnly verifies the additive
// guarantee: when InitialContext is empty the turn input is exactly the
// prompt, byte-for-byte the pre-change shape (no empty leading part).
func TestNewSpawnPlan_NoInitialContextEmitsPromptOnly(t *testing.T) {
	t.Parallel()
	for _, ctxVal := range []string{"", "   \n\t "} {
		plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp", Prompt: "do work", InitialContext: ctxVal})
		in, _ := plan.TurnStart["input"].([]map[string]any)
		if len(in) != 1 || in[0]["text"] != "do work" {
			t.Fatalf("InitialContext=%q: expected single prompt part, got %v", ctxVal, plan.TurnStart["input"])
		}
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

// Spec.ResponseSchema must ride turn/start as `outputSchema` — the
// app-server v2 param ("Optional JSON Schema used to constrain the final
// assistant message for this turn"; codex-cli 0.139.0 protocol fixture).
// This is the native-strict half of the one-shot lane; the soft prompt
// instruction remains as the degrade path for older app-servers.
func TestNewSpawnPlan_ResponseSchemaRidesOutputSchema(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp", Prompt: "x", ResponseSchema: schema})

	got, ok := plan.TurnStart["outputSchema"].(json.RawMessage)
	if !ok {
		t.Fatalf("expected outputSchema as json.RawMessage on turn/start, got %T", plan.TurnStart["outputSchema"])
	}
	if string(got) != string(schema) {
		t.Fatalf("outputSchema = %s, want %s", got, schema)
	}

	// The wire encoding must carry the schema as a raw JSON object, not a
	// re-encoded string.
	wire, err := json.Marshal(plan.TurnStart)
	if err != nil {
		t.Fatalf("marshal turn/start params: %v", err)
	}
	if !strings.Contains(string(wire), `"outputSchema":{"type":"object"`) {
		t.Fatalf("wire params should embed the schema object, got %s", wire)
	}
}

// Without a ResponseSchema the turn/start params stay byte-for-byte free
// of the outputSchema key (additive-safety: pre-change shape preserved).
func TestNewSpawnPlan_NoResponseSchemaNoOutputSchema(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{Cwd: "/tmp", Prompt: "x"})
	if v, ok := plan.TurnStart["outputSchema"]; ok {
		t.Fatalf("outputSchema must be absent without ResponseSchema, got %v", v)
	}
}

func TestNewSpawnPlan_BaseInstructionsAndSystemPromptAppend(t *testing.T) {
	t.Parallel()
	plan := NewSpawnPlan(agent.Spec{
		Cwd:                "/tmp",
		BaseInstructions:   "RULES",
		SystemPromptAppend: "EXTRA",
	})
	if got := plan.ThreadStart["baseInstructions"]; got != "RULES" {
		t.Fatalf("baseInstructions = %#v, want RULES", got)
	}
	if got := plan.ThreadStart["developerInstructions"]; got != "EXTRA" {
		t.Fatalf("developerInstructions = %#v, want EXTRA", got)
	}
}

func TestNewSpawnPlan_MCPServers(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Cwd: "/tmp",
		MCPServers: []agent.MCPServerConfig{
			{Name: "af-linear", Command: "node", Args: []string{"server.js"}, Env: map[string]string{
				"FOO":               "bar",
				"ATTACH_TOKEN":      "must-not-serialize",
				"ATTACH_TOKEN_FILE": "/must/not/serialize",
				"ATTACH_URL":        "wss://must-not-serialize.invalid",
			}},
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
	// After JSON marshal/unmarshal roundtrip, env is map[string]interface{}.
	envMap, ok := linear["env"].(map[string]interface{})
	if !ok || envMap["FOO"] != "bar" {
		t.Fatalf("expected env FOO=bar, got %v", linear["env"])
	}
	for _, key := range []string{"ATTACH_TOKEN", "ATTACH_TOKEN_FILE", "ATTACH_URL"} {
		if _, leaked := envMap[key]; leaked {
			t.Fatalf("codex serialized runner-only %s: %v", key, envMap)
		}
	}
	// Native Codex config infers stdio transport from command.
	if _, exists := linear["type"]; exists {
		t.Fatalf("native Codex stdio config must omit type, got %v", linear)
	}
	if linear["default_tools_approval_mode"] != codexMCPToolsApprovalApprove {
		t.Fatalf("headless MCP approval mode = %v, want %q", linear["default_tools_approval_mode"], codexMCPToolsApprovalApprove)
	}
	code, ok := plan.MCPConfig["af-code"].(map[string]any)
	if !ok || code["default_tools_approval_mode"] != codexMCPToolsApprovalApprove {
		t.Fatalf("every requested MCP server must carry the unattended approval seed: %v", plan.MCPConfig)
	}
}

// TestMCPServersConfig_NoArgsStdio verifies that a stdio server with no
// args produces an entry without an "args" key (and certainly not null).
func TestMCPServersConfig_NoArgsStdio(t *testing.T) {
	t.Parallel()
	servers := []agent.MCPServerConfig{
		{Name: "no-args", Command: "mybinary"},
	}
	cfg := mcpServersConfig(servers)
	entry, ok := cfg["no-args"].(map[string]any)
	if !ok {
		t.Fatalf("expected map entry for no-args, got %T: %v", cfg["no-args"], cfg["no-args"])
	}
	if _, hasArgs := entry["args"]; hasArgs {
		t.Fatalf("expected no 'args' key for a server with no args, got entry=%v", entry)
	}
	if entry["command"] != "mybinary" {
		t.Fatalf("expected command=mybinary, got %v", entry["command"])
	}
	if _, exists := entry["type"]; exists {
		t.Fatalf("native Codex stdio config must omit type, got %v", entry)
	}
}

// TestMCPServersConfig_HTTPTransport verifies that an http-type server
// produces an entry with url/http_headers and no type/command/args fields.
func TestMCPServersConfig_HTTPTransport(t *testing.T) {
	t.Parallel()
	servers := []agent.MCPServerConfig{
		{
			Name:    "af-linear-proxy",
			Type:    "http",
			URL:     "https://example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer tok"},
		},
	}
	cfg := mcpServersConfig(servers)
	entry, ok := cfg["af-linear-proxy"].(map[string]any)
	if !ok {
		t.Fatalf("expected map entry for af-linear-proxy, got %T: %v", cfg["af-linear-proxy"], cfg["af-linear-proxy"])
	}
	if _, exists := entry["type"]; exists {
		t.Fatalf("native Codex HTTP config must omit type, got %v", entry)
	}
	if entry["url"] != "https://example.com/mcp" {
		t.Fatalf("expected url=https://example.com/mcp, got %v", entry["url"])
	}
	headers, ok := entry["http_headers"].(map[string]interface{})
	if !ok || headers["Authorization"] != "Bearer tok" {
		t.Fatalf("expected Authorization header, got %v", entry["http_headers"])
	}
	if _, exists := entry["headers"]; exists {
		t.Fatalf("shared JSON headers key is invalid in native Codex config: %v", entry)
	}
	if _, hasCmd := entry["command"]; hasCmd {
		t.Fatalf("http entry must not contain 'command', got entry=%v", entry)
	}
	if _, hasArgs := entry["args"]; hasArgs {
		t.Fatalf("http entry must not contain 'args', got entry=%v", entry)
	}
}

// TestMCPServersConfig_NoNullValues verifies that a mixed config (a
// no-args stdio server plus an http server) marshals to JSON with no
// null values anywhere — this is the direct guard against the codex
// config/batchWrite rejection.
func TestMCPServersConfig_NoNullValues(t *testing.T) {
	t.Parallel()
	servers := []agent.MCPServerConfig{
		{Name: "stdio-noargs", Command: "runner"},
		{
			Name:    "http-proxy",
			Type:    "http",
			URL:     "https://proxy.example.com/mcp",
			Headers: map[string]string{"X-Token": "abc"},
		},
	}
	cfg := mcpServersConfig(servers)
	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	if strings.Contains(string(j), "null") {
		t.Fatalf("marshaled MCP config contains null — codex will reject it:\n%s", string(j))
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
