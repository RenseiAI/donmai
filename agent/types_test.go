package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestSpec_RoundTrip verifies a Spec round-trips through JSON without
// data loss and that camelCase JSON tags are preserved on the wire.
func TestSpec_RoundTrip(t *testing.T) {
	t.Parallel()
	maxTurns := 25
	in := Spec{
		Prompt:         "do work",
		Cwd:            "/tmp/wt",
		Env:            map[string]string{"FOO": "bar"},
		Autonomous:     true,
		SandboxEnabled: true,
		SandboxLevel:   SandboxWorkspaceWrite,
		AllowedTools:   []string{"Bash(pnpm:*)"},
		DisallowedTools: []string{
			"AskUserQuestion",
		},
		MCPServers: []MCPServerConfig{{
			Name:    "af_linear",
			Command: "pnpm",
			Args:    []string{"af-linear"},
			Env:     map[string]string{"LINEAR_API_KEY": "x"},
		}},
		MCPToolNames:       []string{"mcp__af_linear__af_linear_get_issue"},
		MaxTurns:           &maxTurns,
		Model:              "claude-sonnet-4-5",
		Effort:             EffortHigh,
		BaseInstructions:   "be careful",
		SystemPromptAppend: "extra rules",
		PermissionConfig: &PermissionConfig{
			AllowPatterns:    []string{"Bash(git:*)"},
			DisallowPatterns: []string{"Bash(rm:*)"},
			DefaultDecision:  "prompt",
		},
		CodeIntelEnforcement: &CodeIntelEnforcement{
			EnforceUsage:         true,
			FallbackAfterAttempt: true,
		},
		ProviderConfig:   map[string]any{"speed": "fast"},
		SubAgentProvider: ProviderClaude,
	}
	bytes, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(bytes)
	for _, want := range []string{
		`"prompt":"do work"`,
		`"sandboxLevel":"workspace-write"`,
		`"mcpStdioServers"`,
		`"mcpToolNames"`,
		`"maxTurns":25`,
		`"baseInstructions":"be careful"`,
		`"systemPromptAppend":"extra rules"`,
		`"permissionConfig"`,
		`"codeIntelligenceEnforcement"`,
		`"providerConfig"`,
		`"subAgentProvider":"claude"`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %s\nwire=%s", want, wire)
		}
	}

	var out Spec
	if err := json.Unmarshal(bytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\nin =%+v\nout=%+v", in, out)
	}
}

// TestCapabilities_RoundTrip verifies camelCase JSON tags on
// Capabilities (the values readers see on QueuedWork.resolvedProfile).
func TestCapabilities_RoundTrip(t *testing.T) {
	t.Parallel()
	in := Capabilities{
		SupportsMessageInjection:            false, // v0.5.0 Claude
		SupportsSessionResume:               true,
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               true,
		NeedsPermissionConfig:               true,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 true,
		SupportsReasoningEffort:             true,
		ToolPermissionFormat:                "claude",
		HumanLabel:                          "Claude",
	}
	bytes, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(bytes)
	for _, want := range []string{
		`"supportsMessageInjection":false`,
		`"supportsSessionResume":true`,
		`"supportsToolPlugins":true`,
		`"needsBaseInstructions":true`,
		`"needsPermissionConfig":true`,
		`"emitsSubagentEvents":true`,
		`"supportsReasoningEffort":true`,
		`"toolPermissionFormat":"claude"`,
		`"humanLabel":"Claude"`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %s\nwire=%s", want, wire)
		}
	}

	var out Capabilities
	if err := json.Unmarshal(bytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\nin =%+v\nout=%+v", in, out)
	}
}

// TestCapabilities_OmitEmpty verifies that a zero-value Capabilities
// only emits the two non-omitempty flags. Important for keeping the
// wire compact.
func TestCapabilities_OmitEmpty(t *testing.T) {
	t.Parallel()
	bytes, err := json.Marshal(Capabilities{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(bytes)
	want := `{"supportsMessageInjection":false,"supportsSessionResume":false,"emitsSubagentEvents":false}`
	if got != want {
		t.Fatalf("zero-value wire mismatch:\nwant=%s\ngot =%s", want, got)
	}
}

// TestResult_RoundTrip verifies the full agent.Result wire shape
// (camelCase, embedded reports).
func TestResult_RoundTrip(t *testing.T) {
	t.Parallel()
	in := Result{
		Status:            "completed",
		ProviderName:      ProviderClaude,
		ProviderSessionID: "sess-123",
		WorktreePath:      "/tmp/wt/REN-1",
		PullRequestURL:    "https://github.com/x/y/pull/1",
		CommitSHA:         "abc123",
		Summary:           "did the thing",
		WorkResult:        "passed",
		Cost: &CostData{
			InputTokens:  100,
			OutputTokens: 50,
			TotalCostUsd: 0.125,
			NumTurns:     3,
		},
		FailureMode: "",
		BackstopReport: &BackstopReport{
			Triggered:      true,
			Pushed:         true,
			PRCreated:      true,
			PRURL:          "https://github.com/x/y/pull/2",
			UnfilledFields: []string{"work_result"},
			Diagnostics:    "branch had unpushed commits",
		},
		QualityReport: &QualityReport{
			BaselineCaptured: true,
			TestDelta:        -1,
			TypeDelta:        0,
			LintDelta:        0,
			BlockedPromotion: false,
		},
	}
	bytes, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(bytes)
	for _, want := range []string{
		`"status":"completed"`,
		`"providerName":"claude"`,
		`"providerSessionId":"sess-123"`,
		`"worktreePath":"/tmp/wt/REN-1"`,
		`"pullRequestUrl":"https://github.com/x/y/pull/1"`,
		`"commitSha":"abc123"`,
		`"workResult":"passed"`,
		`"backstopReport"`,
		`"qualityReport"`,
		`"unfilledFields":["work_result"]`,
		`"prCreated":true`,
		`"prUrl":"https://github.com/x/y/pull/2"`,
		`"baselineCaptured":true`,
		`"testDelta":-1`,
		`"blockedPromotion":false`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %s\nwire=%s", want, wire)
		}
	}

	var out Result
	if err := json.Unmarshal(bytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\nin =%+v\nout=%+v", in, out)
	}
}

// TestIsSupported is a table-driven check on the named-capability gate.
func TestIsSupported(t *testing.T) {
	t.Parallel()
	caps := Capabilities{
		SupportsMessageInjection:            true,
		SupportsSessionResume:               true,
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               true,
		NeedsPermissionConfig:               true,
		SupportsCodeIntelligenceEnforcement: true,
		EmitsSubagentEvents:                 true,
		SupportsReasoningEffort:             true,
		ToolPermissionFormat:                "claude",
	}
	tests := []struct {
		name string
		cap  Capability
		caps Capabilities
		want bool
	}{
		{"injection-true", CapMessageInjection, caps, true},
		{"injection-false", CapMessageInjection, Capabilities{}, false},
		{"resume-true", CapSessionResume, caps, true},
		{"toolplugins", CapToolPlugins, caps, true},
		{"baseinstructions", CapBaseInstructions, caps, true},
		{"permissionconfig", CapPermissionConfig, caps, true},
		{"codeintel", CapCodeIntelEnforcement, caps, true},
		{"subagent", CapSubagentEvents, caps, true},
		{"effort", CapReasoningEffort, caps, true},
		{"tool-perm-claude-explicit", CapToolPermissionFormatClaude, caps, true},
		{"tool-perm-claude-empty-default", CapToolPermissionFormatClaude, Capabilities{}, true},
		{"tool-perm-claude-codex", CapToolPermissionFormatClaude, Capabilities{ToolPermissionFormat: "codex"}, false},
		{"unknown-capability", Capability("nonexistent"), caps, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSupported(tt.caps, tt.cap); got != tt.want {
				t.Errorf("IsSupported(%v, %q) = %v, want %v", tt.caps, tt.cap, got, tt.want)
			}
		})
	}
}

// TestEnumValues guards the wire shape of the public enums against
// accidental rename.
func TestEnumValues(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		string(ProviderClaude):        "claude",
		string(ProviderCodex):         "codex",
		string(ProviderStub):          "stub",
		string(ProviderSpringAI):      "spring-ai",
		string(ProviderA2A):           "a2a",
		string(SandboxReadOnly):       "read-only",
		string(SandboxWorkspaceWrite): "workspace-write",
		string(SandboxFullAccess):     "full-access",
		string(EffortLow):             "low",
		string(EffortXHigh):           "xhigh",
		string(EventInit):             "init",
		string(EventAssistantText):    "assistant_text",
		string(EventToolUse):          "tool_use",
		string(EventResult):           "result",
		string(EventError):            "error",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("enum value %q != %q", got, want)
		}
	}
}
