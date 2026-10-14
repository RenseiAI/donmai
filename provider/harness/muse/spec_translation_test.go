package muse

import (
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestResolveModel_DefaultAndSentinel(t *testing.T) {
	if got := resolveModel(agent.Spec{}); got != DefaultModel {
		t.Errorf("empty model=%q, want %q", got, DefaultModel)
	}
	if got := resolveModel(agent.Spec{Model: "default"}); got != DefaultModel {
		t.Errorf("sentinel model=%q, want %q (never forward \"default\")", got, DefaultModel)
	}
	if got := resolveModel(agent.Spec{Model: "muse-wonder-full-1.0"}); got != "muse-wonder-full-1.0" {
		t.Errorf("explicit model=%q", got)
	}
}

func TestResolveApprovalMode(t *testing.T) {
	if got := resolveApprovalMode(agent.Spec{Autonomous: true}); got != ApprovalModeAllowAll {
		t.Errorf("autonomous without policy=%q, want allowAll", got)
	}
	if got := resolveApprovalMode(agent.Spec{Autonomous: true, PermissionConfig: &agent.PermissionConfig{}}); got != ApprovalModePromptUnmatched {
		t.Errorf("autonomous with policy=%q, want promptUnmatched", got)
	}
	if got := resolveApprovalMode(agent.Spec{Autonomous: true, PermissionConfig: &agent.PermissionConfig{DefaultDecision: "deny"}}); got != ApprovalModeDenyUnmatched {
		t.Errorf("deny policy=%q, want denyUnmatched", got)
	}
	if got := resolveApprovalMode(agent.Spec{}); got != ApprovalModePromptUnmatched {
		t.Errorf("default=%q, want promptUnmatched", got)
	}
}

func TestPromptInput_LeadingInstructions(t *testing.T) {
	spec := agent.Spec{
		Prompt:             "do the thing",
		BaseInstructions:   "be careful",
		SystemPromptAppend: "stay brief",
		InitialContext:     "remember this",
	}
	parts := promptInput(spec)
	if len(parts) != 3 {
		t.Fatalf("parts=%d, want 3 (instructions, context, prompt)", len(parts))
	}
	if !strings.Contains(*parts[0].Text, "be careful") || !strings.Contains(*parts[0].Text, "stay brief") {
		t.Errorf("leading part=%q, want both instruction layers", *parts[0].Text)
	}
	if *parts[1].Text != "remember this" {
		t.Errorf("context part=%q", *parts[1].Text)
	}
	if *parts[2].Text != "do the thing" {
		t.Errorf("prompt part=%q", *parts[2].Text)
	}
}

func TestMCPServersConfig_HTTPInline(t *testing.T) {
	servers := []agent.MCPServerConfig{{
		Name: "platform", Type: "http",
		URL:     "https://example.invalid/mcp",
		Headers: map[string]string{"Authorization": "Bearer x"},
	}}
	got := mcpServersConfig(servers)
	entry, ok := got["platform"]
	if !ok {
		t.Fatal("platform server missing")
	}
	if entry.Transport != "streamableHttp" {
		t.Errorf("transport=%q, want streamableHttp", entry.Transport)
	}
	if entry.URL != "https://example.invalid/mcp" {
		t.Errorf("url=%q", entry.URL)
	}
	if entry.Headers["Authorization"] != "Bearer x" {
		t.Errorf("headers=%v", entry.Headers)
	}
	if entry.Mode == nil || *entry.Mode != SessionMCPServerModeOptional {
		t.Errorf("mode=%v, want optional", entry.Mode)
	}
}

func TestMCPServersConfig_StdioAndInvalid(t *testing.T) {
	servers := []agent.MCPServerConfig{
		{Name: "good", Type: "stdio", Command: "server-bin", Args: []string{"--x"}},
		{Name: "", Type: "stdio", Command: "nameless"},
		{Name: "nope", Type: "http"},
		{Name: "weird", Type: "carrier-pigeon"},
	}
	got := mcpServersConfig(servers)
	if len(got) != 1 {
		t.Errorf("servers=%v, want only the valid stdio entry", got)
	}
	if got["good"].Transport != "stdio" || got["good"].Command != "server-bin" {
		t.Errorf("stdio entry=%+v", got["good"])
	}
}

func TestServeArgs_ReadOnlyLane(t *testing.T) {
	args := serveArgs(agent.Spec{SandboxLevel: agent.SandboxReadOnly}, false, false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--disable-shell") || !strings.Contains(joined, "--disable-write") {
		t.Errorf("read-only args=%v, want --disable-shell --disable-write", args)
	}
	args = serveArgs(agent.Spec{}, false, true)
	if !strings.Contains(strings.Join(args, " "), "--disable-sandbox") {
		t.Errorf("disable-sandbox args=%v", args)
	}
}

func TestServeEnv_StripsKeyUnlessSpecSetsIt(t *testing.T) {
	parent := []string{"PATH=/bin", "META_API_KEY=parent-secret"}
	got := serveEnv(parent, nil, map[string]string{"OTHER": "1"})
	for _, kv := range got {
		if strings.HasPrefix(kv, "META_API_KEY=") {
			t.Errorf("parent META_API_KEY leaked: %q", kv)
		}
	}
	foundUpdate := false
	for _, kv := range got {
		if kv == "MUSE_NO_AUTO_UPDATE=1" {
			foundUpdate = true
		}
	}
	if !foundUpdate {
		t.Error("MUSE_NO_AUTO_UPDATE=1 missing")
	}
	// The Spec-owned key survives.
	got = serveEnv(parent, nil, map[string]string{"META_API_KEY": "spec-secret"})
	found := false
	for _, kv := range got {
		if kv == "META_API_KEY=spec-secret" {
			found = true
		}
	}
	if !found {
		t.Errorf("spec META_API_KEY missing: %v", got)
	}
}

func TestCanonicalWorkspaceRoot_ResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	got, err := canonicalWorkspaceRoot(dir)
	if err != nil {
		t.Fatalf("canonicalWorkspaceRoot: %v", err)
	}
	if got == "" {
		t.Fatal("empty root")
	}
}

func TestCheckHandshakeResult_RequiresSchemaV1AndMCP(t *testing.T) {
	good := `{"experimentalApi":false,"grantedCapabilities":["sessionMcp"],"museHome":"/h","platformFamily":"unix","platformOs":"linux","schema":{"fingerprint":"` + schemaExportFingerprint + `","version":1},"serverInfo":{"name":"m","version":"1.4.6"},"userAgent":"u"}`
	if _, err := checkHandshakeResult([]byte(good)); err != nil {
		t.Errorf("good handshake: %v", err)
	}
	badSchema := `{"experimentalApi":false,"grantedCapabilities":["sessionMcp"],"museHome":"/h","platformFamily":"unix","platformOs":"linux","schema":{"fingerprint":"x","version":2},"serverInfo":{"name":"m","version":"1.4.6"},"userAgent":"u"}`
	if _, err := checkHandshakeResult([]byte(badSchema)); err == nil {
		t.Error("schema version 2 accepted, want rejection")
	}
	noMCP := `{"experimentalApi":false,"grantedCapabilities":[],"museHome":"/h","platformFamily":"unix","platformOs":"linux","schema":{"fingerprint":"` + schemaExportFingerprint + `","version":1},"serverInfo":{"name":"m","version":"1.4.6"},"userAgent":"u"}`
	if _, err := checkHandshakeResult([]byte(noMCP)); err == nil {
		t.Error("missing sessionMcp grant accepted, want rejection")
	}
}

func TestClassifyStartError_AuthKinds(t *testing.T) {
	p, err := New(Options{skipProcess: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		msg  string
		want error
	}{
		{
			name: "model not entitled",
			msg:  "turn/start: model muse-wonder-full-1.0 does not exist or you lack access",
			want: agent.ErrModelNotEntitled,
		},
		{
			name: "unauthorized",
			msg:  "session/start: unauthorized",
			want: agent.ErrModelNotEntitled,
		},
		{
			name: "logged out",
			msg:  "turn/start: not logged in, run login first",
			want: agent.ErrNotAuthenticated,
		},
		{
			name: "unauthenticated",
			msg:  "session/start: unauthenticated: login required",
			want: agent.ErrNotAuthenticated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.classifyStartError(errors.New(tc.msg))
			if !errors.Is(err, agent.ErrSpawnFailed) {
				t.Errorf("err=%v, want ErrSpawnFailed in the chain", err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err=%v, want %v in the chain", err, tc.want)
			}
		})
	}
	// A generic first-turn failure stays a plain spawn failure:
	// no auth cause is fabricated.
	plain := p.classifyStartError(errors.New("turn/start: host overloaded"))
	if !errors.Is(plain, agent.ErrSpawnFailed) {
		t.Errorf("plain err=%v, want ErrSpawnFailed", plain)
	}
	if errors.Is(plain, agent.ErrNotAuthenticated) || errors.Is(plain, agent.ErrModelNotEntitled) {
		t.Errorf("plain err=%v, want no auth cause", plain)
	}
}

func TestCheckHandshakeResult_MCPGrantPolicy(t *testing.T) {
	fp := schemaExportFingerprint
	good := func(grants string) string {
		return `{"experimentalApi":false,"grantedCapabilities":[` + grants + `],"museHome":"/h","platformFamily":"unix","platformOs":"linux","schema":{"fingerprint":"` + fp + `","version":1},"serverInfo":{"name":"m","version":"1.4.6"},"userAgent":"u"}`
	}
	// A session carrying inline MCP servers hard-denies a missing
	// grant: starting silently tool-less is worse than refusing.
	if _, err := checkHandshakeResultForPlan([]byte(good(``)), true); err == nil {
		t.Error("MCP session accepted a missing sessionMcp grant, want rejection")
	}
	if _, err := checkHandshakeResultForPlan([]byte(good(`"sessionMcp"`)), true); err != nil {
		t.Errorf("MCP session with the grant rejected: %v", err)
	}
	// A session with no mcpServers soft-passes the same host: it
	// never asked for MCP tools, so there is nothing to lose.
	if _, err := checkHandshakeResultForPlan([]byte(good(``)), false); err != nil {
		t.Errorf("non-MCP session rejected over a grant it never needed: %v", err)
	}
}

func TestIsSessionGone(t *testing.T) {
	rpc := &RPCError{Method: "session/resume", Code: -32000, Message: "gone", Data: []byte(`{"kind":"sessionNotFound"}`)}
	if !IsSessionGone(rpc) {
		t.Error("sessionNotFound not recognized")
	}
	rpc.Data = []byte(`{"kind":"notFound"}`)
	if !IsSessionGone(rpc) {
		t.Error("notFound not recognized")
	}
	rpc.Data = []byte(`{"kind":"internal"}`)
	if IsSessionGone(rpc) {
		t.Error("internal misclassified as gone")
	}
}

func TestApprovalChoiceFor(t *testing.T) {
	req := ApprovalRequestParams{AvailableChoices: []ApprovalChoice{
		{ChoiceID: "allow", Decision: MSPApprovalApproved},
		{ChoiceID: "deny", Decision: MSPApprovalDenied},
	}}
	c, ok := approvalChoiceFor(req, true)
	if !ok || c.ChoiceID != "allow" {
		t.Errorf("allow choice=%+v,%v", c, ok)
	}
	c, ok = approvalChoiceFor(req, false)
	if !ok || c.ChoiceID != "deny" {
		t.Errorf("deny choice=%+v,%v", c, ok)
	}
}

func TestApprovalChoiceFor_DeclineMapsToAbort(t *testing.T) {
	// Live hosts offer the abort arm (not denied) on shell subjects:
	// a bridge decline must still find a denial choice.
	req := ApprovalRequestParams{AvailableChoices: []ApprovalChoice{
		{ChoiceID: "proceed", Decision: MSPApprovalApproved},
		{ChoiceID: "stop", Decision: MSPApprovalAbort},
	}}
	c, ok := approvalChoiceFor(req, false)
	if !ok || c.ChoiceID != "stop" {
		t.Errorf("decline choice=%+v,%v, want the abort arm", c, ok)
	}
	if c.Decision != MSPApprovalAbort {
		t.Errorf("decline decision=%q, want abort", c.Decision)
	}
}

func TestParseApprovalRequest_Kinds(t *testing.T) {
	cmd := "echo hi"
	shell := parseApprovalRequest(ApprovalRequestParams{Subject: ApprovalSubject{Kind: "shell", Command: &cmd}}, "/w")
	if shell.Kind != ApprovalKindCommand || shell.Command != "echo hi" {
		t.Errorf("shell=%+v", shell)
	}
	path := "/w/a.txt"
	file := parseApprovalRequest(ApprovalRequestParams{Subject: ApprovalSubject{Kind: "fileAccess", Path: &path}}, "/w")
	if file.Kind != ApprovalKindFileAccess || file.Path != "/w/a.txt" {
		t.Errorf("file=%+v", file)
	}
	unknown := parseApprovalRequest(ApprovalRequestParams{Subject: ApprovalSubject{Kind: "teleport"}}, "/w")
	if unknown.Kind != ApprovalKindUnknown {
		t.Errorf("unknown kind=%q, want unknown", unknown.Kind)
	}
	// Unknown kinds never auto-approve, even with an empty policy.
	if got := NewApprovalBridge(nil).Evaluate(unknown); got.Action != ActionDecline {
		t.Errorf("unknown approval evaluated %v, want decline", got)
	}
}
