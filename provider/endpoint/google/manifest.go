package google

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the Google company endpoint declaration: gemini-generate
// on direct/vertex and openai-chat on the local /v1 host. All EnvKeys are
// env-var names only.
func (*Endpoint) Manifest() agent.ModelEndpointManifest {
	return agent.ModelEndpointManifest{
		Company:     agent.CompanyGoogle,
		HumanLabel:  "Google",
		Family:      agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoGeminiGenerate, agent.ProtoOpenAIChat},
		Hosts: []agent.HostDesc{
			{
				Host:          agent.HostDirect,
				Protocol:      agent.ProtoGeminiGenerate,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://generativelanguage.googleapis.com",
				EnvKeys:       []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			},
			{
				Host:          agent.HostVertex,
				Protocol:      agent.ProtoGeminiGenerate,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://{region}-aiplatform.googleapis.com",
				EnvKeys:       []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_VERTEX_PROJECT_ID"},
			},
			{
				Host:          agent.HostLocal,
				Protocol:      agent.ProtoOpenAIChat,
				AuthModes:     []agent.AuthMode{agent.AuthLocal},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostLocalFree,
				BaseURLTmpl:   "http://127.0.0.1:11434/v1",
				EnvKeys:       []string{},
			},
		},
		Models: []agent.ModelDesc{
			{
				ID:               "gemini-2.5-pro",
				HumanLabel:       "Gemini 2.5 Pro",
				ContextWindow:    1000000,
				SupportsTools:    true,
				SupportsJSONMode: true,
				Hosts:            []agent.ServingHost{agent.HostDirect, agent.HostVertex},
			},
		},
	}
}
