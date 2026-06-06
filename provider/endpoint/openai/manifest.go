package openai

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the OpenAI company endpoint declaration (§3.2). OpenAI
// SPEAKS openai-responses (codex app-server host login) and openai-chat (keyed
// direct/azure). json_schema strict is honored on the keyed cells, so models
// declare SupportsJSONMode:true. All EnvKeys are env-var NAMES only.
func (*Endpoint) Manifest() agent.ModelEndpointManifest {
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
				BaseURLTmpl:   "", // codex app-server host login
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
		},
	}
}
