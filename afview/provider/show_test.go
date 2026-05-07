package provider_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/provider"
)

func fullProvider() *afclient.Provider {
	return &afclient.Provider{
		ID:         "sandbox-e2b",
		Name:       "e2b",
		Version:    "1.0.0",
		Family:     afclient.FamilySandbox,
		Scope:      afclient.ScopeGlobal,
		Status:     afclient.StatusReady,
		Source:     afclient.SourceBundled,
		Trust:      afclient.TrustSignedVerified,
		SignerID:   "did:key:z6Mkf...",
		SignedAt:   "2026-04-01T12:00:00Z",
		ManifestOK: true,
		Capabilities: map[string]any{
			"transportModel":      "dial-in",
			"supportsPauseResume": false,
			"billingModel":        "wall-clock",
		},
	}
}

func TestRenderShow_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := provider.RenderShow(&buf, fullProvider(), true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	wantContains := []string{
		"ID:",
		"sandbox-e2b",
		"Name:",
		"e2b",
		"Family:",
		"sandbox",
		"Version:",
		"1.0.0",
		"Scope:",
		"global",
		"Status:",
		"ready",
		"Source:",
		"bundled",
		"Trust:",
		"[verified]",
		"signed and verified",
		"Signer:",
		"did:key:z6Mkf...",
		"SignedAt:",
		"2026-04-01T12:00:00Z",
		"Capabilities:",
		"transportModel",
		"dial-in",
		"billingModel",
		"wall-clock",
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderShow_NoCapabilities(t *testing.T) {
	p := fullProvider()
	p.Capabilities = nil

	var buf bytes.Buffer
	if err := provider.RenderShow(&buf, p, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(buf.String(), "Capabilities:") {
		t.Errorf("should not print Capabilities section when empty; got:\n%s", buf.String())
	}
}

func TestRenderShow_NilProviderErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := provider.RenderShow(&buf, nil, true); err == nil {
		t.Errorf("expected error on nil provider, got nil")
	}
}

func TestRenderShow_TrustStates_NoColor(t *testing.T) {
	tests := []struct {
		trust   afclient.ProviderTrustState
		wantSym string
		wantLbl string
	}{
		{afclient.TrustSignedVerified, "[verified]", "signed and verified"},
		{afclient.TrustSignedUnverified, "[signed/unverified]", "signed but unverified"},
		{afclient.TrustUnsigned, "[unsigned]", "unsigned"},
	}
	for _, tc := range tests {
		t.Run(string(tc.trust), func(t *testing.T) {
			p := fullProvider()
			p.Trust = tc.trust
			p.SignerID = ""
			p.SignedAt = ""

			var buf bytes.Buffer
			if err := provider.RenderShow(&buf, p, true); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.wantSym) {
				t.Errorf("missing trust symbol %q; got:\n%s", tc.wantSym, out)
			}
			if !strings.Contains(out, tc.wantLbl) {
				t.Errorf("missing trust label %q; got:\n%s", tc.wantLbl, out)
			}
		})
	}
}

func TestRenderShow_NoSignerFields_WhenEmpty(t *testing.T) {
	p := fullProvider()
	p.SignerID = ""
	p.SignedAt = ""

	var buf bytes.Buffer
	if err := provider.RenderShow(&buf, p, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "Signer:") {
		t.Errorf("should not print Signer when empty; got:\n%s", out)
	}
	if strings.Contains(out, "SignedAt:") {
		t.Errorf("should not print SignedAt when empty; got:\n%s", out)
	}
}

func TestRenderShow_CapabilityOrderIsDeterministic(t *testing.T) {
	p := &afclient.Provider{
		ID:     "p1",
		Family: afclient.FamilyAgentRuntime,
		Capabilities: map[string]any{
			"zKey":  true,
			"aKey":  "first",
			"mKey":  42,
			"bKey":  "second",
			"yyKey": "near-end",
		},
	}
	// Render twice — output must be byte-identical because keys are
	// sorted before iteration.
	var a, b bytes.Buffer
	if err := provider.RenderShow(&a, p, true); err != nil {
		t.Fatalf("RenderShow A: %v", err)
	}
	if err := provider.RenderShow(&b, p, true); err != nil {
		t.Fatalf("RenderShow B: %v", err)
	}
	if a.String() != b.String() {
		t.Errorf("RenderShow output not deterministic across runs:\n--- A ---\n%s\n--- B ---\n%s", a.String(), b.String())
	}
	// Sanity: aKey appears before zKey in output.
	out := a.String()
	if strings.Index(out, "aKey") >= strings.Index(out, "zKey") {
		t.Errorf("expected aKey to appear before zKey in:\n%s", out)
	}
}

func TestPlainShow_DelegatesToRenderShowNoColor(t *testing.T) {
	var plain bytes.Buffer
	if err := provider.PlainShow(&plain, fullProvider()); err != nil {
		t.Fatalf("PlainShow: %v", err)
	}
	var rendered bytes.Buffer
	if err := provider.RenderShow(&rendered, fullProvider(), true); err != nil {
		t.Fatalf("RenderShow: %v", err)
	}
	if plain.String() != rendered.String() {
		t.Errorf("PlainShow output diverges from RenderShow(noColor=true)\n--- PlainShow ---\n%s\n--- RenderShow ---\n%s",
			plain.String(), rendered.String())
	}
	if strings.Contains(plain.String(), "\033[") {
		t.Errorf("PlainShow output contains ANSI escapes:\n%s", plain.String())
	}
}
