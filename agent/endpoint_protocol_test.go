package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type protocolTestEndpoint struct {
	manifest ModelEndpointManifest
	binding  EndpointBinding
	calls    *int
}

func (e protocolTestEndpoint) Manifest() ModelEndpointManifest {
	manifest := e.manifest
	manifest.ContractABI = "model-endpoint/v1"
	// This fixture's retained default is Chat; only its opt-in view has both rows.
	manifest.Hosts = []HostDesc{{Host: HostDirect, Protocol: ProtoOpenAIChat, AuthModes: []AuthMode{AuthBYOK}, CostModel: CostMeteredPerToken, EnvKeys: []string{"TEST_API_KEY"}}}
	return manifest
}

func (e protocolTestEndpoint) ProtocolManifest() ModelEndpointManifest {
	return e.manifest
}

func (e protocolTestEndpoint) Resolve(context.Context, EndpointRequest) (EndpointBinding, error) {
	return EndpointBinding{}, errors.New("legacy resolver called")
}

func (e protocolTestEndpoint) ResolveProtocol(context.Context, EndpointRequest, WireProtocol) (EndpointBinding, error) {
	*e.calls++
	return e.binding, nil
}

type legacyProtocolTestEndpoint struct {
	manifest ModelEndpointManifest
	binding  EndpointBinding
	calls    *int
}

type failingProtocolTestEndpoint struct{ protocolTestEndpoint }

func (e failingProtocolTestEndpoint) ResolveProtocol(context.Context, EndpointRequest, WireProtocol) (EndpointBinding, error) {
	return EndpointBinding{}, errors.New("secret-fixture from provider")
}

func (e legacyProtocolTestEndpoint) Manifest() ModelEndpointManifest { return e.manifest }
func (e legacyProtocolTestEndpoint) Resolve(context.Context, EndpointRequest) (EndpointBinding, error) {
	*e.calls++
	return e.binding, nil
}

func protocolTestDeclaration() (ModelEndpointManifest, EndpointRequest, EndpointBinding) {
	manifest := ModelEndpointManifest{
		Company: CompanyOpenAI, Family: FamilyModelEndpoint, ContractABI: "model-endpoint/v2",
		Speaks: []WireProtocol{ProtoOpenAIChat, ProtoOpenAIResponses},
		Hosts: []HostDesc{
			{Host: HostDirect, Protocol: ProtoOpenAIChat, AuthModes: []AuthMode{AuthBYOK}, CostModel: CostMeteredPerToken, EnvKeys: []string{"TEST_API_KEY"}},
			{Host: HostDirect, Protocol: ProtoOpenAIResponses, AuthModes: []AuthMode{AuthBYOK}, CostModel: CostMeteredPerToken, EnvKeys: []string{"TEST_API_KEY"}},
		},
	}
	req := EndpointRequest{Model: "test-model", Host: HostDirect, Mechanism: AuthAPIKey, Auth: AuthBYOK, EnvProvided: map[string]string{"TEST_API_KEY": "secret-fixture"}}
	binding := EndpointBinding{
		Company: CompanyOpenAI, Model: req.Model, Host: req.Host, Protocol: ProtoOpenAIResponses,
		Mechanism: req.Mechanism, Auth: req.Auth, CostModel: CostMeteredPerToken,
		Env: map[string]string{"TEST_API_KEY": "secret-fixture"},
	}
	return manifest, req, binding
}

func TestResolveEndpointProtocolExactTuple(t *testing.T) {
	manifest, req, binding := protocolTestDeclaration()
	calls := 0
	got, err := ResolveEndpointProtocol(context.Background(), protocolTestEndpoint{manifest, binding, &calls}, req, ProtoOpenAIResponses)
	if err != nil {
		t.Fatalf("resolve exact tuple: %v", err)
	}
	if calls != 1 || got.Protocol != ProtoOpenAIResponses || got.Host != HostDirect || got.Env["TEST_API_KEY"] != "secret-fixture" {
		t.Fatalf("incorrect exact binding or resolver call count: protocol=%q host=%q calls=%d", got.Protocol, got.Host, calls)
	}
}

func TestResolveEndpointProtocolRefusalsBeforeProviderCall(t *testing.T) {
	base, req, binding := protocolTestDeclaration()
	tests := []struct {
		name     string
		manifest ModelEndpointManifest
		req      EndpointRequest
		protocol WireProtocol
		want     error
	}{
		{"empty protocol", base, req, "", ErrEndpointProtocolRequired},
		{"undeclared protocol", base, req, ProtoGeminiGenerate, ErrEndpointProtocolUnsupported},
		{"missing host tuple", base, EndpointRequest{Host: HostAzure, Auth: AuthBYOK}, ProtoOpenAIResponses, ErrEndpointProtocolUnsupported},
		{"auth outside selected row", base, EndpointRequest{Host: HostDirect, Auth: AuthHostSession}, ProtoOpenAIResponses, ErrEndpointProtocolUnsupported},
		{"duplicate exact tuple", func() ModelEndpointManifest {
			m := base
			m.Hosts = append(append([]HostDesc{}, base.Hosts...), base.Hosts[1])
			return m
		}(), req, ProtoOpenAIResponses, ErrEndpointHostProtocolAmbiguous},
		// A duplicate of a tuple other than the requested one still makes the
		// declaration ambiguous: uniqueness is a property of the whole manifest.
		{"duplicate other tuple", func() ModelEndpointManifest {
			m := base
			m.Hosts = append(append([]HostDesc{}, base.Hosts...), base.Hosts[0])
			return m
		}(), req, ProtoOpenAIResponses, ErrEndpointHostProtocolAmbiguous},
		{"row without protocol", func() ModelEndpointManifest {
			m := base
			m.Hosts = append(append([]HostDesc{}, base.Hosts...), HostDesc{Host: HostAzure, AuthModes: []AuthMode{AuthBYOK}})
			return m
		}(), req, ProtoOpenAIResponses, ErrEndpointHostProtocolAmbiguous},
		// A row exists for the tuple, but the protocol is outside the declared
		// Speaks surface, so the row must not be selected.
		{"row protocol outside Speaks", func() ModelEndpointManifest {
			m := base
			m.Speaks = []WireProtocol{ProtoOpenAIChat}
			return m
		}(), req, ProtoOpenAIResponses, ErrEndpointProtocolUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := ResolveEndpointProtocol(context.Background(), protocolTestEndpoint{tc.manifest, binding, &calls}, tc.req, tc.protocol)
			if !errors.Is(err, tc.want) || calls != 0 || !got.IsZero() {
				t.Fatalf("error=%v calls=%d binding=%+v", err, calls, got)
			}
		})
	}
}

func TestResolveEndpointProtocolRejectsBindingMismatchWithoutSecret(t *testing.T) {
	manifest, req, good := protocolTestDeclaration()
	tests := []struct {
		name   string
		mutate func(*EndpointBinding)
	}{
		{"protocol relabel", func(b *EndpointBinding) { b.Protocol = ProtoOpenAIChat }},
		{"wrong company", func(b *EndpointBinding) { b.Company = CompanyGoogle }},
		{"wrong model", func(b *EndpointBinding) { b.Model = "different" }},
		{"wrong auth", func(b *EndpointBinding) { b.Auth = AuthMetered }},
		{"wrong cost", func(b *EndpointBinding) { b.CostModel = CostLocalFree }},
		{"extra environment", func(b *EndpointBinding) { b.Env["EXTRA"] = "secret-extra" }},
		{"wrong environment", func(b *EndpointBinding) { b.Env["TEST_API_KEY"] = "secret-other" }},
		{"missing environment", func(b *EndpointBinding) { delete(b.Env, "TEST_API_KEY") }},
		{"wrong host", func(b *EndpointBinding) { b.Host = HostAzure }},
		{"wrong mechanism", func(b *EndpointBinding) { b.Mechanism = AuthOAuth }},
		{"wrong region", func(b *EndpointBinding) { b.Region = "elsewhere" }},
		{"wrong own-auth flag", func(b *EndpointBinding) { b.BringsOwnAuth = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binding := good
			binding.Env = map[string]string{"TEST_API_KEY": "secret-fixture"}
			tc.mutate(&binding)
			calls := 0
			got, err := ResolveEndpointProtocol(context.Background(), protocolTestEndpoint{manifest, binding, &calls}, req, ProtoOpenAIResponses)
			if !errors.Is(err, ErrEndpointProtocolMismatch) || calls != 1 || !got.IsZero() {
				t.Fatalf("error=%v calls=%d binding=%+v", err, calls, got)
			}
			if strings.Contains(err.Error(), "secret-") {
				t.Fatalf("credential leaked in error")
			}
		})
	}
}

func TestResolveEndpointProtocolLegacyAmbiguity(t *testing.T) {
	manifest, req, binding := protocolTestDeclaration()
	manifest.ContractABI = "model-endpoint/v1"
	calls := 0
	_, err := ResolveEndpointProtocol(context.Background(), legacyProtocolTestEndpoint{manifest, binding, &calls}, req, ProtoOpenAIResponses)
	if !errors.Is(err, ErrEndpointHostProtocolAmbiguous) || calls != 0 {
		t.Fatalf("ambiguous legacy host error=%v calls=%d", err, calls)
	}
	manifest.Hosts = manifest.Hosts[1:]
	got, err := ResolveEndpointProtocol(context.Background(), legacyProtocolTestEndpoint{manifest, binding, &calls}, req, ProtoOpenAIResponses)
	if err != nil || calls != 1 || got.Protocol != ProtoOpenAIResponses {
		t.Fatalf("unambiguous legacy binding error=%v calls=%d protocol=%q", err, calls, got.Protocol)
	}
	// The single legacy row is Responses; a Chat request has no row.
	calls = 0
	if _, err := ResolveEndpointProtocol(context.Background(), legacyProtocolTestEndpoint{manifest, binding, &calls}, req, ProtoOpenAIChat); !errors.Is(err, ErrEndpointProtocolUnsupported) || calls != 0 {
		t.Fatalf("legacy host without the requested protocol error=%v calls=%d", err, calls)
	}
	// An old Resolve that relabels the protocol is refused after the call.
	relabeled := binding
	relabeled.Protocol = ProtoOpenAIChat
	got, err = ResolveEndpointProtocol(context.Background(), legacyProtocolTestEndpoint{manifest, relabeled, &calls}, req, ProtoOpenAIResponses)
	if !errors.Is(err, ErrEndpointProtocolMismatch) || calls != 1 || !got.IsZero() {
		t.Fatalf("legacy relabel error=%v calls=%d binding=%+v", err, calls, got)
	}
}

// legacyOverrideEndpoint is an opt-in endpoint whose legacy Manifest() is set
// independently, to exercise the identity checks between the two projections.
type legacyOverrideEndpoint struct {
	protocolTestEndpoint
	legacy ModelEndpointManifest
}

func (e legacyOverrideEndpoint) Manifest() ModelEndpointManifest { return e.legacy }

func TestResolveEndpointProtocolRefusesInconsistentDeclaration(t *testing.T) {
	qualified, req, binding := protocolTestDeclaration()
	legacy := protocolTestEndpoint{manifest: qualified}.Manifest()
	tests := []struct {
		name     string
		provider func(calls *int) ModelEndpointProvider
	}{
		{"qualified manifest declares v1", func(calls *int) ModelEndpointProvider {
			m := qualified
			m.ContractABI = "model-endpoint/v1"
			return protocolTestEndpoint{m, binding, calls}
		}},
		{"qualified manifest changes company", func(calls *int) ModelEndpointProvider {
			l := legacy
			l.Company = CompanyGoogle
			return legacyOverrideEndpoint{protocolTestEndpoint{qualified, binding, calls}, l}
		}},
		{"qualified manifest changes family", func(calls *int) ModelEndpointProvider {
			m := qualified
			m.Family = FamilyHarness
			return protocolTestEndpoint{m, binding, calls}
		}},
		{"legacy manifest declares v2", func(calls *int) ModelEndpointProvider {
			l := legacy
			l.ContractABI = "model-endpoint/v2"
			return legacyOverrideEndpoint{protocolTestEndpoint{qualified, binding, calls}, l}
		}},
		{"old-only provider declares v2", func(calls *int) ModelEndpointProvider {
			m := qualified
			m.Hosts = m.Hosts[1:]
			return legacyProtocolTestEndpoint{m, binding, calls}
		}},
		{"missing company", func(calls *int) ModelEndpointProvider {
			m := qualified
			m.Hosts = m.Hosts[1:]
			m.ContractABI = "model-endpoint/v1"
			m.Company = ""
			return legacyProtocolTestEndpoint{m, binding, calls}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := ResolveEndpointProtocol(context.Background(), tc.provider(&calls), req, ProtoOpenAIResponses)
			if !errors.Is(err, ErrEndpointProtocolMismatch) || calls != 0 || !got.IsZero() {
				t.Fatalf("error=%v calls=%d binding=%+v", err, calls, got)
			}
		})
	}
}

func TestResolveEndpointProtocolNilProvider(t *testing.T) {
	if _, err := ResolveEndpointProtocol(context.Background(), nil, EndpointRequest{Host: HostDirect}, ProtoOpenAIChat); !errors.Is(err, ErrEndpointProtocolUnsupported) {
		t.Fatalf("nil provider error=%v", err)
	}
}

func TestResolveEndpointProtocolRedactsProviderError(t *testing.T) {
	manifest, req, binding := protocolTestDeclaration()
	calls := 0
	got, err := ResolveEndpointProtocol(context.Background(), failingProtocolTestEndpoint{protocolTestEndpoint{manifest, binding, &calls}}, req, ProtoOpenAIResponses)
	if !errors.Is(err, ErrEndpointProtocolMismatch) || !got.IsZero() || strings.Contains(err.Error(), "secret-fixture") {
		t.Fatalf("unsafe resolver failure: %v", err)
	}
}

func TestResolveEndpointProtocolValidatesReturnedBaseURL(t *testing.T) {
	manifest, req, good := protocolTestDeclaration()
	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"direct HTTPS", "https://api.example.test/v1", false},
		{"loopback HTTP", "http://127.0.0.1:8080/v1", false},
		{"malformed URL", "https://bad host/v1", true},
		{"userinfo", "https://user:secret-url@api.example.test/v1", true},
		{"nonloopback HTTP", "http://api.example.test/v1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binding := good
			binding.BaseURL = tc.baseURL
			calls := 0
			got, err := ResolveEndpointProtocol(context.Background(), protocolTestEndpoint{manifest, binding, &calls}, req, ProtoOpenAIResponses)
			if calls != 1 {
				t.Fatalf("resolver calls=%d", calls)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrEndpointProtocolMismatch) || !got.IsZero() || strings.Contains(err.Error(), "secret-url") {
					t.Fatalf("unsafe URL refusal: %v", err)
				}
			} else if err != nil || got.BaseURL != tc.baseURL {
				t.Fatalf("valid URL refused: %v", err)
			}
		})
	}
}
