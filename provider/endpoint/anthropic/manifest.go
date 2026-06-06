package anthropic

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the Anthropic company endpoint declaration (§3.1). Anthropic
// SPEAKS only anthropic-messages. The Messages API has no json_schema mode, so
// SupportsJSONMode is false on every model (structured output rides forced-tool
// on keyed cells). All EnvKeys are env-var NAMES only (OSS-safe).
func (*Endpoint) Manifest() agent.ModelEndpointManifest {
	return agent.ModelEndpointManifest{
		Company:     agent.CompanyAnthropic,
		HumanLabel:  "Anthropic",
		Family:      agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoAnthropicMessages},
		Hosts: []agent.HostDesc{
			{
				Host:          agent.HostOAuthCLI,
				Protocol:      agent.ProtoAnthropicMessages,
				AuthModes:     []agent.AuthMode{agent.AuthHostSession},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostHostSubscription,
				BaseURLTmpl:   "", // host login
				EnvKeys:       []string{},
			},
			{
				Host:          agent.HostDirect,
				Protocol:      agent.ProtoAnthropicMessages,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://api.anthropic.com",
				EnvKeys:       []string{"ANTHROPIC_API_KEY"},
			},
			{
				Host:          agent.HostBedrock,
				Protocol:      agent.ProtoAnthropicMessages,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://bedrock-runtime.{region}.amazonaws.com",
				EnvKeys:       []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION"},
			},
			{
				Host:          agent.HostVertex,
				Protocol:      agent.ProtoAnthropicMessages,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://{region}-aiplatform.googleapis.com",
				EnvKeys:       []string{"GOOGLE_APPLICATION_CREDENTIALS", "ANTHROPIC_VERTEX_PROJECT_ID"},
			},
		},
		Models: []agent.ModelDesc{
			{
				ID:               "claude-sonnet-4-5",
				HumanLabel:       "Claude Sonnet 4.5",
				ContextWindow:    200000,
				SupportsTools:    true,
				SupportsJSONMode: false,
				Hosts:            []agent.ServingHost{agent.HostDirect, agent.HostBedrock, agent.HostVertex},
			},
			{
				ID:               "claude-haiku",
				HumanLabel:       "Claude Haiku",
				ContextWindow:    200000,
				SupportsTools:    true,
				SupportsJSONMode: false,
				Hosts:            []agent.ServingHost{agent.HostDirect, agent.HostBedrock, agent.HostVertex},
			},
		},
	}
}
