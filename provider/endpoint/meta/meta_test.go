package meta

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestManifest_Shape asserts the Meta manifest declares the expected
// company / family / ABI / protocol surface (the SoT the matrix harvests).
func TestManifest_Shape(t *testing.T) {
	m := New().Manifest()
	if m.Company != agent.CompanyMeta {
		t.Errorf("company = %q, want meta", m.Company)
	}
	if m.Family != agent.FamilyModelEndpoint {
		t.Errorf("family = %q, want model-endpoint", m.Family)
	}
	if m.ContractABI != "model-endpoint/v1" {
		t.Errorf("contractAbi = %q, want model-endpoint/v1", m.ContractABI)
	}
	if len(m.Speaks) != 1 || m.Speaks[0] != agent.ProtoMuseMSP {
		t.Errorf("speaks = %v, want [muse-msp]", m.Speaks)
	}
	// The MSP turn has no structured-output primitive.
	for _, md := range m.Models {
		if md.SupportsJSONMode {
			t.Errorf("model %q: SupportsJSONMode=true, want false (MSP has no structured-output primitive)", md.ID)
		}
	}
	// The host-login cell brings its own auth; the keyed cell needs a key.
	for _, h := range m.Hosts {
		switch h.Host {
		case agent.HostOAuthCLI:
			if !h.BringsOwnAuth || h.NeedsAPIKey {
				t.Errorf("oauth-cli host = %+v, want bringsOwnAuth=true needsApiKey=false", h)
			}
		case agent.HostDirect:
			if h.BringsOwnAuth || !h.NeedsAPIKey {
				t.Errorf("direct host = %+v, want bringsOwnAuth=false needsApiKey=true", h)
			}
		default:
			t.Errorf("unexpected host %q", h.Host)
		}
	}
}

// TestResolve_OAuthCLI verifies pure resolution on the host-login cell:
// no base URL, host-subscription cost, no env copied.
func TestResolve_OAuthCLI(t *testing.T) {
	e := New()
	got, err := e.Resolve(context.Background(), agent.EndpointRequest{
		Model: "muse-wonder-1.0",
		Host:  agent.HostOAuthCLI,
		Auth:  agent.AuthHostSession,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Company != agent.CompanyMeta {
		t.Errorf("company = %q", got.Company)
	}
	if got.Protocol != agent.ProtoMuseMSP {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.BaseURL != "" {
		t.Errorf("baseURL = %q, want empty (host login)", got.BaseURL)
	}
	if got.CostModel != agent.CostHostSubscription {
		t.Errorf("costModel = %q", got.CostModel)
	}
	if !got.BringsOwnAuth {
		t.Errorf("bringsOwnAuth = false, want true")
	}
	if got.IsZero() {
		t.Errorf("resolved binding reports IsZero")
	}
}

// TestResolve_Direct verifies pure resolution templates the base URL and
// copies only the host's declared env keys from EnvProvided (never reads
// process env).
func TestResolve_Direct(t *testing.T) {
	e := New()
	got, err := e.Resolve(context.Background(), agent.EndpointRequest{
		Model:     "muse-wonder-full-1.0",
		Host:      agent.HostDirect,
		Mechanism: agent.AuthAPIKey,
		Auth:      agent.AuthBYOK,
		EnvProvided: map[string]string{
			"META_API_KEY": "test-key",
			"UNRELATED":    "leak", // must NOT be copied
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Company != agent.CompanyMeta {
		t.Errorf("company = %q", got.Company)
	}
	if got.BaseURL != "https://api.meta.ai/v1" {
		t.Errorf("baseURL = %q", got.BaseURL)
	}
	if got.Protocol != agent.ProtoMuseMSP {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.CostModel != agent.CostMeteredPerToken {
		t.Errorf("costModel = %q", got.CostModel)
	}
	if got.Env["META_API_KEY"] != "test-key" {
		t.Errorf("env META_API_KEY not copied: %v", got.Env)
	}
	if _, leaked := got.Env["UNRELATED"]; leaked {
		t.Errorf("Resolve copied an undeclared env key: %v", got.Env)
	}
	if got.IsZero() {
		t.Errorf("resolved binding reports IsZero")
	}
}

// TestResolve_UnknownHost errors rather than dialing or guessing.
func TestResolve_UnknownHost(t *testing.T) {
	_, err := New().Resolve(context.Background(), agent.EndpointRequest{
		Model: "muse-wonder-1.0",
		Host:  agent.HostLocal, // Meta has no local host
	})
	if err == nil {
		t.Fatalf("expected error for unknown host, got nil")
	}
}

// TestResolveEndpointProtocol_LegacyPath resolves a Meta tuple through the
// explicit helper. Meta declares one protocol per host and does not opt in to
// qualified rows, so the helper must use the legacy Manifest and Resolve and
// return exactly the legacy binding; any other protocol refuses.
func TestResolveEndpointProtocol_LegacyPath(t *testing.T) {
	var provider agent.ModelEndpointProvider = New()
	if _, optedIn := provider.(agent.ProtocolEndpointResolver); optedIn {
		t.Fatal("meta endpoint unexpectedly declares qualified protocol rows")
	}
	req := agent.EndpointRequest{
		Model: "muse-spark-1.3", Host: agent.HostOAuthCLI, Mechanism: agent.AuthCLISession, Auth: agent.AuthHostSession,
	}
	legacy, err := provider.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("legacy resolve: %v", err)
	}
	got, err := agent.ResolveEndpointProtocol(context.Background(), provider, req, agent.ProtoMuseMSP)
	if err != nil {
		t.Fatalf("explicit muse-msp on oauth-cli: %v", err)
	}
	if !reflect.DeepEqual(got, legacy) {
		t.Fatalf("explicit binding differs from legacy:\n got=%+v\nwant=%+v", got, legacy)
	}
	if _, err := agent.ResolveEndpointProtocol(context.Background(), provider, req, agent.ProtoOpenAIChat); !errors.Is(err, agent.ErrEndpointProtocolUnsupported) {
		t.Fatalf("openai-chat on meta error = %v, want ErrEndpointProtocolUnsupported", err)
	}
}
