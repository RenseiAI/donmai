package openai

import "github.com/RenseiAI/donmai/agent"

// openAIHostDeclaration is the single source for both manifest projections.
// A legacy default is named on its declaration, never chosen by row order.
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
				CostModel: agent.CostHostSubscription, EnvKeys: []string{},
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

// Manifest retains exactly the original one protocol row per serving host.
func (*Endpoint) Manifest() agent.ModelEndpointManifest { return openAIManifest(false) }

// ProtocolManifest exposes the qualified rows to explicit-protocol callers.
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
