package openai

import "github.com/RenseiAI/donmai/agent"

// openAIHostDeclaration is the single source for both manifest projections.
// A legacy default is named on its declaration, never chosen by row order:
// each serving host has exactly one legacyDefault row, and adding a variant
// for a host never changes which row that host's legacy projection carries.
type openAIHostDeclaration struct {
	host          agent.HostDesc
	legacyDefault bool
}

func openAIHostDeclarations() []openAIHostDeclaration {
	return []openAIHostDeclaration{
		{
			legacyDefault: true,
			host: agent.HostDesc{
				Host: agent.HostOAuthCLI, Protocol: agent.ProtoOpenAIResponses,
				AuthModes: []agent.AuthMode{agent.AuthHostSession}, BringsOwnAuth: true,
				CostModel: agent.CostHostSubscription, EnvKeys: []string{}, // codex app-server host login
			},
		},
		{
			legacyDefault: true,
			host: agent.HostDesc{
				Host: agent.HostDirect, Protocol: agent.ProtoOpenAIChat,
				AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, NeedsAPIKey: true,
				CostModel: agent.CostMeteredPerToken, BaseURLTmpl: "https://api.openai.com/v1",
				EnvKeys: []string{"OPENAI_API_KEY"},
			},
		},
		{
			legacyDefault: true,
			host: agent.HostDesc{
				Host: agent.HostAzure, Protocol: agent.ProtoOpenAIChat,
				AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, NeedsAPIKey: true,
				CostModel: agent.CostMeteredPerToken, BaseURLTmpl: "https://{resource}.openai.azure.com",
				EnvKeys: []string{"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT"},
			},
		},
		// Gateway host (ADR-2026-07-24 / 08 §2). The translating gateway
		// presents OpenAI models on an openai-chat surface served from the
		// local loopback listener; the harness reaches them through a
		// per-session bearer (DONMAI_GW_TOKEN) rather than a provider key —
		// the actual upstream credential is the gateway's own pool
		// credential, never injected into the harness. BaseURLTmpl documents
		// the loopback shape; the concrete port is filled at Bind time
		// (gateway.Gateway.Bind), which overrides this template.
		{
			legacyDefault: true,
			host: agent.HostDesc{
				Host: agent.HostGateway, Protocol: agent.ProtoOpenAIChat,
				AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, NeedsAPIKey: true,
				CostModel: agent.CostMeteredPerToken, BaseURLTmpl: "http://127.0.0.1:{port}/v1",
				EnvKeys: []string{"DONMAI_GW_TOKEN"},
			},
		},
		// The keyed direct API exposes both documented wire surfaces. This
		// variant does not change the historical Chat default or advertise a
		// gateway Responses codec.
		{
			host: agent.HostDesc{
				Host: agent.HostDirect, Protocol: agent.ProtoOpenAIResponses,
				AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, NeedsAPIKey: true,
				CostModel: agent.CostMeteredPerToken, BaseURLTmpl: "https://api.openai.com/v1",
				EnvKeys: []string{"OPENAI_API_KEY"},
			},
		},
	}
}

// Manifest returns the legacy OpenAI company endpoint declaration (§3.2,
// model-endpoint/v1). OpenAI SPEAKS openai-responses (codex app-server host
// login) and openai-chat (keyed direct/azure/gateway). It retains exactly the
// original one protocol row per serving host, so old first/last-host
// consumers and Resolve keep their historical defaults. json_schema strict is
// honored on the keyed cells, so models declare SupportsJSONMode:true. All
// EnvKeys are env-var NAMES only.
func (*Endpoint) Manifest() agent.ModelEndpointManifest { return openAIManifest(false) }

// ProtocolManifest exposes every qualified (host, protocol) row to
// explicit-protocol callers (model-endpoint/v2, ADR-2026-10-01 D1/D2). It is
// projected from the same declarations as Manifest.
func (*Endpoint) ProtocolManifest() agent.ModelEndpointManifest { return openAIManifest(true) }

func openAIManifest(qualified bool) agent.ModelEndpointManifest {
	manifest := agent.ModelEndpointManifest{
		Company: agent.CompanyOpenAI, HumanLabel: "OpenAI", Family: agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoOpenAIResponses, agent.ProtoOpenAIChat},
		Hosts:       make([]agent.HostDesc, 0, 5),
		Models: []agent.ModelDesc{
			{
				ID: "gpt-5-codex", HumanLabel: "GPT-5 Codex", ContextWindow: 400000,
				SupportsTools: true, SupportsJSONMode: true,
				Hosts: []agent.ServingHost{agent.HostDirect, agent.HostAzure},
			},
			{
				ID: "gpt-6-sol", HumanLabel: "GPT-6 Sol", ContextWindow: 1050000,
				SupportsTools: true, SupportsJSONMode: true,
				Hosts: []agent.ServingHost{agent.HostOAuthCLI},
			},
		},
	}
	if qualified {
		manifest.ContractABI = "model-endpoint/v2"
	}
	for _, declaration := range openAIHostDeclarations() {
		if qualified || declaration.legacyDefault {
			manifest.Hosts = append(manifest.Hosts, declaration.host)
		}
	}
	return manifest
}
