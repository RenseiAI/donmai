package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/span"
)

func TestSpawnDelegationJSONFixture(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("testdata/delegation.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		tools     []string
		failed    bool
		prose     bool
		resume    bool
		malformed bool
	}{
		{name: "declared", tools: []string{"dispatch_child"}},
		{name: "failed", tools: []string{"dispatch_child"}, failed: true},
		{name: "undeclared"},
		{name: "prose-only", tools: []string{"dispatch_child"}, prose: true},
		{name: "malformed-ref", tools: []string{"dispatch_child"}, malformed: true},
		{name: "resume", tools: []string{"dispatch_child"}, resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := []byte("export default function activate(pi) {}\n")
			delivery := agent.ExtensionDelivery{ID: "delegation", Kind: agent.ExtensionDeliveryInline, Source: source, Basename: "delegation.ts", DelegationTools: tc.tools}
			delivery.Digest = delivery.ComputeDigest(source)
			if len(tc.tools) == 0 && delivery.Digest != sha256Hex(source) {
				t.Fatal("empty declarations changed legacy source digest")
			}
			var receipt agent.ToolLifecycleReceipt
			spec := agent.Spec{AdditionalExtensions: []agent.ExtensionDelivery{delivery}, OnToolLifecycleAdapted: func(r agent.ToolLifecycleReceipt) error { receipt = r; return nil }}
			body := string(fixture)
			if tc.failed {
				body = strings.ReplaceAll(body, `"isError":false`, `"isError":true`)
			}
			if tc.prose {
				body = strings.ReplaceAll(body, `"details":{"childSessionRef":{"sessionId":"child-1","transport":"host_cli"}}`, `"details":{}`)
			}
			if tc.malformed {
				body = strings.ReplaceAll(body, `"transport":"host_cli"`, `"transport":42`)
			}
			var h agent.Handle
			var err error
			if tc.resume {
				_, h, err = resumeScripted(t, "parent-1", spec, handshakeEvent("h1"), body)
			} else {
				_, h, err = spawnScripted(t, spec, handshakeEvent("h1"), body)
			}
			if err != nil {
				t.Fatal(err)
			}
			events := drain(t, h)
			var spans []agent.Span
			processor, err := span.NewProcessor(span.ProcessorConfig{SessionID: "parent-1", OrgID: "test-org", WorkspaceID: "test-workspace", Sender: span.SendFunc(func(s agent.Span) bool { spans = append(spans, s); return true })})
			if err != nil {
				t.Fatal(err)
			}
			var subs []agent.SubagentEvent
			for _, e := range events {
				mapped := processor.Process(e)
				for _, e := range mapped {
					if sub, ok := e.(agent.SubagentEvent); ok {
						subs = append(subs, sub)
					}
				}
			}
			var subSpans []agent.SubagentSpan
			for _, s := range spans {
				if sub, ok := s.(agent.SubagentSpan); ok {
					subSpans = append(subSpans, sub)
				}
			}
			// The fixture witnesses child-1 independently of adapter output.
			// A delivery conforms only when that child has a typed lifecycle.
			conforms := false
			for i := 1; i < len(subs); i++ {
				if subs[i-1].Phase == agent.SubagentStarted && subs[i-1].ChildSessionID == "child-1" && subs[i].ToolUseID == subs[i-1].ToolUseID && (subs[i].Phase == agent.SubagentCompleted || subs[i].Phase == agent.SubagentFailed) {
					conforms = true
				}
			}
			if want := len(tc.tools) > 0 && !tc.prose && !tc.malformed; conforms != want {
				t.Fatalf("delivery conformance = %v, want %v", conforms, want)
			}
			if len(tc.tools) == 0 {
				// This fixture independently witnesses a child ref. Without a declared
				// tool the adapter emits nothing, so the delivery cannot conform to a
				// claim that every child is represented by a typed lifecycle.
				if len(subs) != 0 || len(subSpans) != 0 {
					t.Fatalf("undeclared tool emitted lifecycle: %+v", subs)
				}
				return
			}
			if len(subs) != 2 {
				t.Fatalf("subagent events = %d, want 2", len(subs))
			}
			wantPhase := agent.SubagentCompleted
			wantStatus := agent.StatusOK
			if tc.failed {
				wantPhase = agent.SubagentFailed
				wantStatus = agent.StatusError
			}
			if subs[0].Phase != agent.SubagentStarted || subs[1].Phase != wantPhase {
				t.Fatalf("lifecycle = %+v", subs)
			}
			for _, sub := range subs {
				wantID, wantTransport := "child-1", "host_cli"
				if tc.prose || tc.malformed {
					wantID, wantTransport = "", ""
				}
				if sub.ChildSessionID != wantID || sub.ChildSessionTransport != wantTransport || sub.ToolUseID != "call-1" || sub.ToolName != "dispatch_child" {
					t.Fatalf("child ref/call = %+v", sub)
				}
			}
			if len(subSpans) != 1 || subSpans[0].Kind != agent.SpanKindSubagent || subSpans[0].Status.Code != wantStatus || subSpans[0].SpanID != subs[0].SpanID || subs[0].SpanID != subs[1].SpanID {
				t.Fatalf("subagent spans = %+v", subSpans)
			}
			changed := delivery
			changed.DelegationTools = []string{"different_tool"}
			wantDigest := agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{delivery})
			if wantDigest == agent.CapabilityExtensionInputDigest([]agent.ExtensionDelivery{changed}) {
				t.Fatal("delegation declarations did not change input digest")
			}
			found := false
			for _, entry := range receipt.Entries {
				if entry.Channel == agent.ToolChannelToolPlugin {
					found = true
					if entry.InputDigest != wantDigest {
						t.Fatal("receipt did not bind delegation declaration")
					}
				}
			}
			if !found {
				t.Fatal("missing extension receipt")
			}
		})
	}
}

func TestSpawnRejectsMalformedDelegationTools(t *testing.T) {
	t.Parallel()
	for _, tools := range [][]string{{""}, {" "}, {"dispatch_child", "dispatch_child"}} {
		t.Run(strings.Join(tools, "/"), func(t *testing.T) {
			t.Parallel()
			source := []byte("export default function activate(pi) {}\n")
			spec := agent.Spec{AdditionalExtensions: []agent.ExtensionDelivery{{ID: "delegation", Kind: agent.ExtensionDeliveryInline, Source: source, Basename: "delegation.ts", DelegationTools: tools}}}
			spec.AdditionalExtensions[0].Digest = spec.AdditionalExtensions[0].ComputeDigest(source)
			_, h, err := spawnScripted(t, spec, handshakeEvent("h1"), event(map[string]any{"type": "agent_settled"}))
			if err == nil {
				drain(t, h)
			}
			if err == nil || !strings.Contains(err.Error(), "delegation tool") {
				t.Fatalf("Spawn error = %v, want delegation tool denial", err)
			}
		})
	}
}

func TestSpawnDelegationDigestBindsDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tools        []string
		path         bool
		tamperSource bool
	}{
		{name: "removed"},
		{name: "renamed", tools: []string{"other_child"}},
		{name: "added", tools: []string{"dispatch_child", "other_child"}},
		{name: "source", tools: []string{"dispatch_child"}, tamperSource: true},
		{name: "path-declaration", path: true},
		{name: "path-source", tools: []string{"dispatch_child"}, path: true, tamperSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(piExtCacheDirEnvVar, t.TempDir())
			source := []byte("export default function activate(pi) {}\n")
			delivery := agent.ExtensionDelivery{ID: "delegation", Kind: agent.ExtensionDeliveryInline, Source: source, Basename: "delegation.ts", DelegationTools: []string{"dispatch_child"}}
			delivery.Digest = delivery.ComputeDigest(source)
			delivery.DelegationTools = tc.tools
			if tc.tamperSource {
				delivery.Source = []byte("tampered source")
			}
			if tc.path {
				delivery.Kind = agent.ExtensionDeliveryPath
				delivery.Path = filepath.Join(t.TempDir(), "delegation.ts")
				if err := os.WriteFile(delivery.Path, delivery.Source, 0o600); err != nil {
					t.Fatal(err)
				}
				delivery.Source = nil
			}
			_, h, err := spawnScripted(t, agent.Spec{AdditionalExtensions: []agent.ExtensionDelivery{delivery}}, handshakeEvent("h1"), event(map[string]any{"type": "agent_settled"}))
			if err == nil {
				drain(t, h)
			}
			if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("Spawn error = %v, want digest mismatch after delivery change", err)
			}
		})
	}
}

func TestSpawnDelegationDigestReceiptAdmission(t *testing.T) {
	base, _, _ := testReceiptAdmission(t)
	source := []byte("export default function activate(pi) {}\n")
	path := filepath.Join(t.TempDir(), "delegation.ts")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	delivery := agent.ExtensionDelivery{ID: "delegation", Kind: agent.ExtensionDeliveryPath, Path: path, DelegationTools: []string{"dispatch_child"}, Required: true}
	delivery.Digest = delivery.ComputeDigest(source)
	_, h, _, err := launchScripted(t, scriptedLaunch{
		spec:     agent.Spec{Autonomous: true, AdditionalExtensions: []agent.ExtensionDelivery{delivery}},
		preSpawn: handshakeEvent("h1"), body: event(map[string]any{"type": "agent_settled"}),
		configure: func(p *Provider) {
			p.artifact = base.artifact
			p.trustedExtensions = []TrustedExtensionIdentity{{ID: delivery.ID, Digest: delivery.Digest}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, h)
	receipt := h.(*Handle).receipt
	if receipt == nil {
		t.Fatal("declared extension lost receipt admission")
	}
	delivery.DelegationTools[0] = "caller_mutation"
	if err := receipt.revalidate(); err != nil {
		t.Fatalf("receipt declaration was not independently owned: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := receipt.revalidate(); err == nil {
		t.Fatal("changed declared extension retained receipt trust")
	}
}
