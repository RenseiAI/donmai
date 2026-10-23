package google

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestProtocolManifestOmitsRetiredTupleWithoutChangingLegacy(t *testing.T) {
	endpoint := New()
	legacy := endpoint.Manifest()
	qualified := endpoint.ProtocolManifest()
	if legacy.ContractABI != "model-endpoint/v1" || qualified.ContractABI != "model-endpoint/v2" ||
		len(legacy.Hosts) != 4 || len(qualified.Hosts) != 3 {
		t.Fatalf("manifest projections changed: legacy=%q/%d qualified=%q/%d",
			legacy.ContractABI, len(legacy.Hosts), qualified.ContractABI, len(qualified.Hosts))
	}
	if legacy.Hosts[2].Host != agent.HostOAuthCLI || legacy.Hosts[2].Protocol != agent.ProtoAntigravityOAuth {
		t.Fatalf("retired legacy row changed")
	}
	for _, host := range qualified.Hosts {
		if host.Host == agent.HostOAuthCLI || host.Protocol == agent.ProtoAntigravityOAuth {
			t.Fatalf("retired tuple advertised by qualified manifest")
		}
	}
	req := agent.EndpointRequest{Model: "gemini-2.5-pro", Host: agent.HostOAuthCLI, Auth: agent.AuthHostSession}
	if _, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, agent.ProtoAntigravityOAuth); !errors.Is(err, agent.ErrEndpointProtocolUnsupported) {
		t.Fatalf("retired protocol accepted: %v", err)
	}
	if _, err := endpoint.ResolveProtocol(context.Background(), req, agent.ProtoAntigravityOAuth); !errors.Is(err, agent.ErrEndpointProtocolUnsupported) {
		t.Fatalf("direct resolver accepted retired protocol: %v", err)
	}
}

func TestProtocolManifestRetainsActiveGoogleRows(t *testing.T) {
	endpoint := New()
	tests := []struct {
		host     agent.ServingHost
		protocol agent.WireProtocol
		auth     agent.AuthMode
	}{
		{agent.HostDirect, agent.ProtoGeminiGenerate, agent.AuthBYOK},
		{agent.HostVertex, agent.ProtoGeminiGenerate, agent.AuthBYOK},
		{agent.HostLocal, agent.ProtoOpenAIChat, agent.AuthLocal},
	}
	for _, tc := range tests {
		t.Run(string(tc.host), func(t *testing.T) {
			req := agent.EndpointRequest{Model: "gemini-2.5-pro", Host: tc.host, Auth: tc.auth}
			if tc.host == agent.HostVertex {
				req.Region = "us-central1"
			}
			got, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, tc.protocol)
			if err != nil || got.Host != tc.host || got.Protocol != tc.protocol {
				t.Fatalf("active tuple refused: protocol=%q error=%v", got.Protocol, err)
			}
		})
	}
}
