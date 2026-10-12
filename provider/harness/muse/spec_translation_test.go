package muse

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestSpawnPlan_Translation drives the production Spec → params
// translation: session/start carries client UUIDv7 ids, the canonical
// root, meta routing and inline MCP; turn/start carries the prompt
// input with ifBusy queue; the read-only lane pins denyUnmatched.
func TestSpawnPlan_Translation(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Prompt:           "do the thing",
		Cwd:              "/tmp/wt",
		Autonomous:       true,
		Model:            "muse-wonder-full-1.0",
		Effort:           agent.EffortLow,
		BaseInstructions: "be brief",
		InitialContext:   "memory: foo",
		MCPServers: []agent.MCPServerConfig{{
			Name: "search", Type: "http", URL: "https://mcp.example/session",
			Headers: map[string]string{"Authorization": "Bearer x"},
		}},
	}
	plan := NewSpawnPlan(spec, "/private/tmp/wt")
	if !isUUIDv7(plan.SessionID) || !isUUIDv7(plan.CommandID) {
		t.Fatalf("ids are not UUIDv7: %q %q", plan.SessionID, plan.CommandID)
	}
	if plan.SessionStart["sessionId"] != plan.SessionID {
		t.Fatalf("session/start sessionId mismatch: %v", plan.SessionStart)
	}
	if plan.SessionStart["providerId"] != "meta" {
		t.Fatalf("providerId: %v", plan.SessionStart["providerId"])
	}
	if plan.SessionStart["modelId"] != "muse-wonder-full-1.0" {
		t.Fatalf("modelId: %v", plan.SessionStart["modelId"])
	}
	if plan.SessionStart["approvalMode"] != "onRequest" {
		t.Fatalf("approvalMode: %v", plan.SessionStart["approvalMode"])
	}
	cfg, _ := plan.SessionStart["config"].(map[string]any)
	servers, _ := cfg["mcpServers"].(map[string]any)
	srv, _ := servers["search"].(map[string]any)
	if srv["transport"] != "streamableHttp" || srv["mode"] != "optional" {
		t.Fatalf("inline MCP server: %v", srv)
	}
	if plan.TurnStart["ifBusy"] != "queue" {
		t.Fatalf("ifBusy: %v", plan.TurnStart["ifBusy"])
	}
	if plan.TurnStart["reasoningEffort"] != "low" {
		t.Fatalf("reasoningEffort: %v", plan.TurnStart["reasoningEffort"])
	}
	input, _ := plan.TurnStart["input"].([]map[string]any)
	if len(input) != 3 {
		t.Fatalf("leading instruction parts + prompt = 3 parts, got %d", len(input))
	}
	if input[0]["text"] != "be brief" || input[1]["text"] != "memory: foo" || input[2]["text"] != "do the thing" {
		t.Fatalf("input order: %v", input)
	}

	ro := NewSpawnPlan(agent.Spec{Prompt: "review", SandboxLevel: agent.SandboxReadOnly}, "/private/tmp/wt")
	if !ro.ReadOnly {
		t.Fatal("read-only lane not detected")
	}
	if ro.SessionStart["approvalMode"] != "denyUnmatched" {
		t.Fatalf("read-only approvalMode: %v", ro.SessionStart["approvalMode"])
	}
}

// TestSpawnPlan_DefaultModelOmitted drives the "never send a default
// sentinel" rule: empty and literal-"default" models omit modelId.
func TestSpawnPlan_DefaultModelOmitted(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"", "default", "Default", " DEFAULT "} {
		plan := NewSpawnPlan(agent.Spec{Prompt: "x", Model: model}, "/tmp/wt")
		if _, ok := plan.SessionStart["modelId"]; ok {
			t.Fatalf("model %q must omit modelId, got %v", model, plan.SessionStart["modelId"])
		}
	}
}

// TestServeEnv_APIKeyStrip drives the production env composition: an
// ambient META_API_KEY is stripped unless the Spec sets it, and
// MUSE_NO_AUTO_UPDATE=1 is always pinned.
func TestServeEnv_APIKeyStrip(t *testing.T) {
	t.Parallel()
	parent := []string{"PATH=/bin", "META_API_KEY=ambient-secret"}
	stripped := serveEnv(parent, nil, nil, false)
	for _, kv := range stripped {
		if strings.HasPrefix(kv, "META_API_KEY=") {
			t.Fatalf("ambient key survived the strip: %q", kv)
		}
	}
	kept := serveEnv(parent, nil, map[string]string{"META_API_KEY": "spec-key"}, true)
	found := false
	for _, kv := range kept {
		if kv == "META_API_KEY=spec-key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Spec key missing: %v", kept)
	}
	pinned := false
	for _, kv := range stripped {
		if kv == "MUSE_NO_AUTO_UPDATE=1" {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("no-auto-update not pinned: %v", stripped)
	}
}

// TestCanonicalWorkspaceRoot drives the production realpath rule: a
// symlinked cwd (macOS /tmp → /private/tmp) resolves canonical.
func TestCanonicalWorkspaceRoot(t *testing.T) {
	t.Parallel()
	got, err := canonicalWorkspaceRoot("/tmp")
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if got != "/private/tmp" {
		t.Fatalf("realpath(/tmp)=%q", got)
	}
	if _, err := canonicalWorkspaceRoot("/no/such/dir-xyz"); err == nil {
		t.Fatal("missing dir must fail")
	}
}

// TestUUIDv7Shape checks the minted ids parse as v7 (version nibble 7,
// RFC 4122 variant) and differ call to call.
func TestUUIDv7Shape(t *testing.T) {
	t.Parallel()
	a, b := newUUIDv7(), newUUIDv7()
	if a == b {
		t.Fatal("two UUIDv7 mints collided")
	}
	for _, id := range []string{a, b} {
		if !isUUIDv7(id) {
			t.Fatalf("not UUIDv7: %q", id)
		}
	}
}

func isUUIDv7(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		case 14:
			if c != '7' {
				return false
			}
		case 19:
			if c != '8' && c != '9' && c != 'a' && c != 'b' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}

// TestApprovalBridge_AllowDeny drives the production approval entry
// point: disallow wins over allow, the default denies, decline maps to
// the abort choice arm.
func TestApprovalBridge_AllowDeny(t *testing.T) {
	t.Parallel()
	bridge := NewApprovalBridge(&agent.PermissionConfig{
		AllowPatterns:    []string{"edit *"},
		DisallowPatterns: []string{"*secret*"},
		DefaultDecision:  "prompt",
	}, false)
	allow := ApprovalRequestParams{
		ApprovalID: "a1", CurrentRequirementID: "r1",
		Subject:          ApprovalSubject{Kind: "fileAccess", Path: "edit a.txt"},
		AvailableChoices: []ApprovalChoice{{ChoiceID: "c-allow", Decision: "approved", Label: "Allow"}, {ChoiceID: "c-abort", Decision: "abort", Label: "Abort"}},
	}
	verdict := bridge.Evaluate(allow)
	if !verdict.Allow {
		t.Fatalf("allow pattern must approve: %+v", verdict)
	}
	choice, err := bridge.PickChoice(allow, verdict)
	if err != nil || choice != "c-allow" {
		t.Fatalf("choice=%q err=%v", choice, err)
	}
	deny := ApprovalRequestParams{
		ApprovalID: "a2", CurrentRequirementID: "r1",
		Subject:          ApprovalSubject{Kind: "shell", Command: "read secret file"},
		AvailableChoices: []ApprovalChoice{{ChoiceID: "c-allow", Decision: "approved", Label: "Allow"}, {ChoiceID: "c-abort", Decision: "abort", Label: "Abort"}},
	}
	verdict = bridge.Evaluate(deny)
	if verdict.Allow {
		t.Fatalf("disallow pattern must deny: %+v", verdict)
	}
	choice, err = bridge.PickChoice(deny, verdict)
	if err != nil || choice != "c-abort" {
		t.Fatalf("decline must map to abort: choice=%q err=%v", choice, err)
	}
	unmatched := ApprovalRequestParams{
		ApprovalID: "a3", CurrentRequirementID: "r1",
		Subject:          ApprovalSubject{Kind: "shell", Command: "something new"},
		AvailableChoices: []ApprovalChoice{{ChoiceID: "c-allow", Decision: "approved", Label: "Allow"}, {ChoiceID: "c-abort", Decision: "abort", Label: "Abort"}},
	}
	if verdict := bridge.Evaluate(unmatched); verdict.Allow {
		t.Fatalf("unmatched must fall to default deny: %+v", verdict)
	}
	// Read-only lane denies everything.
	ro := NewApprovalBridge(&agent.PermissionConfig{AllowPatterns: []string{"*"}}, true)
	if verdict := ro.Evaluate(allow); verdict.Allow {
		t.Fatalf("read-only lane must deny: %+v", verdict)
	}
}
