package openai

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// legacyManifestBeforeProtocolSelection is the exact model-endpoint/v1
// declaration this endpoint published before explicit protocol selection
// existed. The legacy projection must stay equal to it: old first/last-host
// consumers and Resolve must never see a qualified variant.
func legacyManifestBeforeProtocolSelection() agent.ModelEndpointManifest {
	return agent.ModelEndpointManifest{
		Company:     agent.CompanyOpenAI,
		HumanLabel:  "OpenAI",
		Family:      agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoOpenAIResponses, agent.ProtoOpenAIChat},
		Hosts: []agent.HostDesc{
			{
				Host:          agent.HostOAuthCLI,
				Protocol:      agent.ProtoOpenAIResponses,
				AuthModes:     []agent.AuthMode{agent.AuthHostSession},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostHostSubscription,
				BaseURLTmpl:   "",
				EnvKeys:       []string{},
			},
			{
				Host:          agent.HostDirect,
				Protocol:      agent.ProtoOpenAIChat,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://api.openai.com/v1",
				EnvKeys:       []string{"OPENAI_API_KEY"},
			},
			{
				Host:          agent.HostAzure,
				Protocol:      agent.ProtoOpenAIChat,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://{resource}.openai.azure.com",
				EnvKeys:       []string{"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT"},
			},
			{
				Host:          agent.HostGateway,
				Protocol:      agent.ProtoOpenAIChat,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "http://127.0.0.1:{port}/v1",
				EnvKeys:       []string{"DONMAI_GW_TOKEN"},
			},
		},
		Models: []agent.ModelDesc{
			{
				ID:               "gpt-5-codex",
				HumanLabel:       "GPT-5 Codex",
				ContextWindow:    400000,
				SupportsTools:    true,
				SupportsJSONMode: true,
				Hosts:            []agent.ServingHost{agent.HostDirect, agent.HostAzure},
			},
			{
				ID:               "gpt-6-sol",
				HumanLabel:       "GPT-6 Sol",
				ContextWindow:    1050000,
				SupportsTools:    true,
				SupportsJSONMode: true,
				Hosts:            []agent.ServingHost{agent.HostOAuthCLI},
			},
		},
	}
}

func TestLegacyManifestUnchanged(t *testing.T) {
	if got, want := New().Manifest(), legacyManifestBeforeProtocolSelection(); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy manifest changed:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestLegacyResolveUnchangedForEveryHost pins the old Resolve output for every
// OpenAI serving host: OAuth-CLI selects Responses; direct, Azure and gateway
// select Chat.
func TestLegacyResolveUnchangedForEveryHost(t *testing.T) {
	tests := []struct {
		name string
		req  agent.EndpointRequest
		want agent.EndpointBinding
	}{
		{
			name: "oauth-cli",
			req:  agent.EndpointRequest{Model: "gpt-6-sol", Host: agent.HostOAuthCLI, Mechanism: agent.AuthCLISession, Auth: agent.AuthHostSession},
			want: agent.EndpointBinding{
				Company: agent.CompanyOpenAI, Model: "gpt-6-sol", BaseURL: "", Protocol: agent.ProtoOpenAIResponses,
				Host: agent.HostOAuthCLI, Mechanism: agent.AuthCLISession, Auth: agent.AuthHostSession,
				CostModel: agent.CostHostSubscription, BringsOwnAuth: true, Env: map[string]string{},
			},
		},
		{
			name: "direct",
			req: agent.EndpointRequest{
				Model: "gpt-5-codex", Host: agent.HostDirect, Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
				EnvProvided: map[string]string{"OPENAI_API_KEY": "fixture-direct", "UNDECLARED": "ignored"},
			},
			want: agent.EndpointBinding{
				Company: agent.CompanyOpenAI, Model: "gpt-5-codex", BaseURL: "https://api.openai.com/v1", Protocol: agent.ProtoOpenAIChat,
				Host: agent.HostDirect, Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
				CostModel: agent.CostMeteredPerToken, Env: map[string]string{"OPENAI_API_KEY": "fixture-direct"},
			},
		},
		{
			name: "azure",
			req: agent.EndpointRequest{
				Model: "gpt-5-codex", Host: agent.HostAzure, Mechanism: agent.AuthAPIKey, Auth: agent.AuthMetered, Region: "eastus",
				EnvProvided: map[string]string{"AZURE_OPENAI_API_KEY": "fixture-azure", "AZURE_OPENAI_ENDPOINT": "fixture-endpoint"},
			},
			want: agent.EndpointBinding{
				Company: agent.CompanyOpenAI, Model: "gpt-5-codex", BaseURL: "https://{resource}.openai.azure.com", Protocol: agent.ProtoOpenAIChat,
				Host: agent.HostAzure, Mechanism: agent.AuthAPIKey, Auth: agent.AuthMetered, Region: "eastus",
				CostModel: agent.CostMeteredPerToken,
				Env:       map[string]string{"AZURE_OPENAI_API_KEY": "fixture-azure", "AZURE_OPENAI_ENDPOINT": "fixture-endpoint"},
			},
		},
		{
			name: "gateway",
			req: agent.EndpointRequest{
				Model: "gpt-5-codex", Host: agent.HostGateway, Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
				EnvProvided: map[string]string{"DONMAI_GW_TOKEN": "fixture-gateway"},
			},
			want: agent.EndpointBinding{
				Company: agent.CompanyOpenAI, Model: "gpt-5-codex", BaseURL: "http://127.0.0.1:{port}/v1", Protocol: agent.ProtoOpenAIChat,
				Host: agent.HostGateway, Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
				CostModel: agent.CostMeteredPerToken, Env: map[string]string{"DONMAI_GW_TOKEN": "fixture-gateway"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New().Resolve(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("legacy resolve: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("legacy binding changed:\n got=%+v\nwant=%+v", got, tc.want)
			}
		})
	}
	if _, err := New().Resolve(context.Background(), agent.EndpointRequest{Host: agent.HostVertex}); err == nil {
		t.Fatal("legacy resolve accepted an undeclared host")
	}
}

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
	// Both projections share identity and every non-host field.
	legacy.Hosts, full.Hosts = nil, nil
	full.ContractABI = legacy.ContractABI
	if !reflect.DeepEqual(legacy, full) {
		t.Fatalf("projections diverge outside hosts:\nlegacy=%+v\nqualified=%+v", legacy, full)
	}
}

// TestHostDeclarationsNameOneLegacyDefaultPerHost guards the single source:
// every serving host it declares carries exactly one legacy default.
func TestHostDeclarationsNameOneLegacyDefaultPerHost(t *testing.T) {
	defaults := make(map[agent.ServingHost]int)
	for _, declaration := range openAIHostDeclarations() {
		host := declaration.host.Host
		if _, ok := defaults[host]; !ok {
			defaults[host] = 0
		}
		if declaration.legacyDefault {
			defaults[host]++
		}
	}
	for host, count := range defaults {
		if count != 1 {
			t.Errorf("host %q declares %d legacy defaults, want exactly 1", host, count)
		}
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
	if legacy.Protocol != agent.ProtoOpenAIChat || !reflect.DeepEqual(chat, legacy) {
		t.Fatalf("explicit Chat differs from the legacy default:\n chat=%+v\nlegacy=%+v", chat, legacy)
	}
	wantResponses := legacy
	wantResponses.Protocol = agent.ProtoOpenAIResponses
	if !reflect.DeepEqual(responses, wantResponses) {
		t.Fatalf("explicit Responses is not the direct declaration:\n got=%+v\nwant=%+v", responses, wantResponses)
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

// TestProtocolResolutionNeverLeaksOrReadsCredentials uses a secret-shaped
// value, both in the process environment and in the request, and checks that
// no refusal carries it and that resolution never reads process env.
func TestProtocolResolutionNeverLeaksOrReadsCredentials(t *testing.T) {
	const secret = "sk-proj-SECRETSHAPEDvalue0123456789abcdefABCDEF"
	t.Setenv("OPENAI_API_KEY", secret)
	t.Setenv("AZURE_OPENAI_API_KEY", secret)
	endpoint := New()
	base := agent.EndpointRequest{
		Model: "gpt-5-codex", Host: agent.HostDirect, Mechanism: agent.AuthAPIKey, Auth: agent.AuthBYOK,
		EnvProvided: map[string]string{"OPENAI_API_KEY": secret, "AZURE_OPENAI_API_KEY": secret},
	}
	refusals := []struct {
		name     string
		mutate   func(*agent.EndpointRequest)
		protocol agent.WireProtocol
		want     error
	}{
		{"empty protocol", func(*agent.EndpointRequest) {}, "", agent.ErrEndpointProtocolRequired},
		{"unknown protocol", func(*agent.EndpointRequest) {}, agent.ProtoGeminiGenerate, agent.ErrEndpointProtocolUnsupported},
		{"wrong host", func(r *agent.EndpointRequest) { r.Host = agent.HostVertex }, agent.ProtoOpenAIChat, agent.ErrEndpointProtocolUnsupported},
		{"azure Responses", func(r *agent.EndpointRequest) { r.Host = agent.HostAzure }, agent.ProtoOpenAIResponses, agent.ErrEndpointProtocolUnsupported},
		{"gateway Responses", func(r *agent.EndpointRequest) { r.Host = agent.HostGateway }, agent.ProtoOpenAIResponses, agent.ErrEndpointProtocolUnsupported},
		{"auth outside row", func(r *agent.EndpointRequest) { r.Auth = agent.AuthHostSession }, agent.ProtoOpenAIResponses, agent.ErrEndpointProtocolUnsupported},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			got, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, tc.protocol)
			if !errors.Is(err, tc.want) || !got.IsZero() {
				t.Fatalf("error=%v binding zero=%v", err, got.IsZero())
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "SECRETSHAPED") {
				t.Fatal("credential value leaked in error")
			}
			if _, err := endpoint.ResolveProtocol(context.Background(), req, tc.protocol); err == nil || strings.Contains(err.Error(), "SECRETSHAPED") {
				t.Fatalf("endpoint method error=%v", err)
			}
		})
	}
	// Without a caller-provided value, the binding carries no credential even
	// though the process environment holds one.
	req := base
	req.EnvProvided = nil
	got, err := agent.ResolveEndpointProtocol(context.Background(), endpoint, req, agent.ProtoOpenAIResponses)
	if err != nil {
		t.Fatalf("resolve without provided env: %v", err)
	}
	if len(got.Env) != 0 {
		t.Fatalf("resolution read process env: %d keys bound", len(got.Env))
	}
}
