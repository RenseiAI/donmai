package openai

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestProtocolManifestKeepsLegacyDefaults(t *testing.T) {
	endpoint := New()
	legacy := endpoint.Manifest()
	full := endpoint.ProtocolManifest()
	if legacy.ContractABI != "model-endpoint/v1" || full.ContractABI != "model-endpoint/v2" {
		t.Fatalf("unexpected manifest ABIs: legacy=%q qualified=%q", legacy.ContractABI, full.ContractABI)
	}
	defaults := []struct {
		host     agent.ServingHost
		protocol agent.WireProtocol
	}{
		{agent.HostOAuthCLI, agent.ProtoOpenAIResponses},
		{agent.HostDirect, agent.ProtoOpenAIChat},
		{agent.HostAzure, agent.ProtoOpenAIChat},
		{agent.HostGateway, agent.ProtoOpenAIChat},
	}
	if len(legacy.Hosts) != len(defaults) || len(full.Hosts) != len(defaults)+1 {
		t.Fatalf("host rows changed: legacy=%d qualified=%d", len(legacy.Hosts), len(full.Hosts))
	}
	for i, want := range defaults {
		if legacy.Hosts[i].Host != want.host || legacy.Hosts[i].Protocol != want.protocol {
			t.Fatalf("legacy host %d changed: host=%q protocol=%q", i, legacy.Hosts[i].Host, legacy.Hosts[i].Protocol)
		}
	}
	seen := make(map[[2]string]bool)
	for _, row := range full.Hosts {
		key := [2]string{string(row.Host), string(row.Protocol)}
		if seen[key] {
			t.Fatalf("duplicate qualified row for host=%q protocol=%q", row.Host, row.Protocol)
		}
		seen[key] = true
	}
	if !seen[[2]string{string(agent.HostDirect), string(agent.ProtoOpenAIResponses)}] ||
		seen[[2]string{string(agent.HostGateway), string(agent.ProtoOpenAIResponses)}] {
		t.Fatalf("direct Responses missing or gateway Responses advertised early")
	}
}

func TestDirectProtocolSelectionPreservesLegacyChat(t *testing.T) {
	endpoint := New()
	req := agent.EndpointRequest{
		Model: "gpt-5-codex", Host: agent.HostDirect,
		Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
		EnvProvided: map[string]string{"OPENAI_API_KEY": "fixture-only"},
	}
	legacy, err := endpoint.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("legacy resolve: %v", err)
	}
	chat, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, agent.ProtoOpenAIChat)
	if err != nil {
		t.Fatalf("explicit Chat: %v", err)
	}
	responses, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, agent.ProtoOpenAIResponses)
	if err != nil {
		t.Fatalf("explicit Responses: %v", err)
	}
	if legacy.Protocol != agent.ProtoOpenAIChat || chat.Protocol != legacy.Protocol ||
		responses.Protocol != agent.ProtoOpenAIResponses || responses.BaseURL != legacy.BaseURL ||
		responses.Env["OPENAI_API_KEY"] != "fixture-only" {
		t.Fatalf("direct selection changed defaults or declaration")
	}
	req.Auth = agent.AuthHostSession
	if _, err := endpoint.ResolveProtocol(context.Background(), req, agent.ProtoOpenAIResponses); !errors.Is(err, agent.ErrEndpointProtocolUnsupported) {
		t.Fatalf("direct method accepted wrong auth: %v", err)
	}
	req.Auth = agent.AuthBYOK
	req.Host = agent.HostGateway
	if _, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, agent.ProtoOpenAIResponses); !errors.Is(err, agent.ErrEndpointProtocolUnsupported) {
		t.Fatalf("gateway Responses was advertised before codec: %v", err)
	}
}
