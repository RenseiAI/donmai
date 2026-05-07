package provider_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/afview/provider"
)

func sampleProviders() []afclient.Provider {
	return []afclient.Provider{
		{
			ID:      "sandbox-e2b",
			Name:    "e2b",
			Version: "1.0.0",
			Family:  afclient.FamilySandbox,
			Scope:   afclient.ScopeGlobal,
			Status:  afclient.StatusReady,
			Source:  afclient.SourceBundled,
			Trust:   afclient.TrustSignedVerified,
			Capabilities: map[string]any{
				"transportModel":      "dial-in",
				"supportsPauseResume": false,
			},
		},
		{
			ID:      "vcs-github",
			Name:    "github",
			Version: "2.1.0",
			Family:  afclient.FamilyVCS,
			Scope:   afclient.ScopeOrg,
			Status:  afclient.StatusDegraded,
			Source:  afclient.SourceRegistry,
			Trust:   afclient.TrustSignedUnverified,
			Capabilities: map[string]any{
				"mergeStrategy":   "three-way-text",
				"hasPullRequests": true,
			},
		},
		{
			ID:      "kit-local",
			Name:    "local-kit",
			Version: "0.3.1",
			Family:  afclient.FamilyKit,
			Scope:   afclient.ScopeProject,
			Status:  afclient.StatusInactive,
			Source:  afclient.SourceLocal,
			Trust:   afclient.TrustUnsigned,
		},
	}
}

func TestRenderList_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := provider.RenderList(&buf, sampleProviders(), true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()

	wantContains := []string{
		// Family headers
		"sandbox",
		"vcs",
		"kit",
		// Provider names
		"e2b",
		"github",
		"local-kit",
		// Versions
		"1.0.0",
		"2.1.0",
		"0.3.1",
		// Scopes
		"global",
		"org",
		"project",
		// Statuses
		"ready",
		"degraded",
		"inactive",
		// Column headers
		"NAME",
		"VERSION",
		"SCOPE",
		"STATUS",
	}
	for _, want := range wantContains {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderList_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := provider.RenderList(&buf, nil, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No providers registered") {
		t.Errorf("empty list should print 'No providers registered', got:\n%s", buf.String())
	}
}

func TestRenderList_FamilyOrdering(t *testing.T) {
	// Providers supplied in reverse order of AllProviderFamilies — output
	// must still be grouped in canonical order.
	ps := []afclient.Provider{
		{ID: "k1", Name: "kit-one", Family: afclient.FamilyKit, Status: afclient.StatusReady, Version: "1.0.0", Scope: afclient.ScopeGlobal},
		{ID: "s1", Name: "sandbox-one", Family: afclient.FamilySandbox, Status: afclient.StatusReady, Version: "1.0.0", Scope: afclient.ScopeGlobal},
	}
	var buf bytes.Buffer
	if err := provider.RenderList(&buf, ps, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	sandboxPos := strings.Index(out, "sandbox")
	kitPos := strings.Index(out, "kit")

	if sandboxPos == -1 || kitPos == -1 {
		t.Fatalf("expected both family headers; got:\n%s", out)
	}
	if sandboxPos > kitPos {
		t.Errorf("sandbox (%d) should appear before kit (%d) per AllProviderFamilies ordering", sandboxPos, kitPos)
	}
}

func TestRenderList_SkipsEmptyFamilies(t *testing.T) {
	ps := []afclient.Provider{
		{ID: "s1", Name: "sandbox-one", Family: afclient.FamilySandbox, Status: afclient.StatusReady, Version: "1.0.0", Scope: afclient.ScopeGlobal},
	}
	var buf bytes.Buffer
	if err := provider.RenderList(&buf, ps, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	// Families not in the input must not appear.
	for _, absent := range []string{"workarea", "agent-runtime", "vcs", "issue-tracker", "deployment", "agent-registry", "kit"} {
		if strings.Contains(out, absent) {
			t.Errorf("unexpected family %q in output:\n%s", absent, out)
		}
	}
}

func TestPlainList_DelegatesToRenderListNoColor(t *testing.T) {
	var plain bytes.Buffer
	if err := provider.PlainList(&plain, sampleProviders()); err != nil {
		t.Fatalf("PlainList: %v", err)
	}
	var rendered bytes.Buffer
	if err := provider.RenderList(&rendered, sampleProviders(), true); err != nil {
		t.Fatalf("RenderList: %v", err)
	}
	if plain.String() != rendered.String() {
		t.Errorf("PlainList output diverges from RenderList(noColor=true)\n--- PlainList ---\n%s\n--- RenderList ---\n%s",
			plain.String(), rendered.String())
	}
}

func TestPlainList_ContainsNoANSIEscapes(t *testing.T) {
	var buf bytes.Buffer
	if err := provider.PlainList(&buf, sampleProviders()); err != nil {
		t.Fatalf("PlainList: %v", err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("PlainList output contains ANSI escapes:\n%s", buf.String())
	}
}

func TestTrustSymbol_NoColor(t *testing.T) {
	tests := []struct {
		trust afclient.ProviderTrustState
		want  string
	}{
		{afclient.TrustSignedVerified, "[verified]"},
		{afclient.TrustSignedUnverified, "[signed/unverified]"},
		{afclient.TrustUnsigned, "[unsigned]"},
	}
	for _, tc := range tests {
		got := provider.TrustSymbol(tc.trust, true)
		if got != tc.want {
			t.Errorf("TrustSymbol(%q, noColor=true) = %q, want %q", tc.trust, got, tc.want)
		}
	}
}

func TestTrustSymbol_Color(t *testing.T) {
	tests := []struct {
		trust afclient.ProviderTrustState
		want  string
	}{
		{afclient.TrustSignedVerified, "✅"},
		{afclient.TrustSignedUnverified, "⚠"},
		{afclient.TrustUnsigned, "🔓"},
	}
	for _, tc := range tests {
		got := provider.TrustSymbol(tc.trust, false)
		if got != tc.want {
			t.Errorf("TrustSymbol(%q, noColor=false) = %q, want %q", tc.trust, got, tc.want)
		}
	}
}

func TestNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if provider.NoColorEnv() {
		t.Errorf("NoColorEnv() = true with empty NO_COLOR")
	}
	t.Setenv("NO_COLOR", "1")
	if !provider.NoColorEnv() {
		t.Errorf("NoColorEnv() = false with NO_COLOR=1")
	}
}
