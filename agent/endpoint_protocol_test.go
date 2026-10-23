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
