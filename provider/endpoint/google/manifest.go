package google

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the Google company endpoint declaration (§3.3) — the
// collapse cell. Google SPEAKS gemini-generate (direct/vertex) AND openai-chat
// (the local /v1 host that serves the north-star opencode×google cell), plus
// antigravity-oauth on the agy host-login cell. nativeJsonMode is per-host:
// true on direct/vertex/local (json mode honored), false on oauth-cli (no json
// surface over the pty channel). All EnvKeys are env-var NAMES only.
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
				Host:          agent.HostOAuthCLI,
				Protocol:      agent.ProtoAntigravityOAuth,
				AuthModes:     []agent.AuthMode{agent.AuthHostSession, agent.AuthLocal},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostHostSubscription,
				BaseURLTmpl:   "", // agy host login
				EnvKeys:       []string{},
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
