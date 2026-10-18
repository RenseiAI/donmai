package meta

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the Meta company endpoint declaration. Meta SPEAKS only
// muse-msp (the `muse serve` MSP session host over stdio JSONL): the
// oauth-cli host is the subscriber login (host-session auth, brings its own
// auth — the serve child inherits the login, no key is injected), and the
// direct host is the keyed API route (BYOK/metered, META_API_KEY). The MSP
// turn has no structured-output primitive, so SupportsJSONMode is false on
// every model (structured output rides spawn-collect). All EnvKeys are
// env-var NAMES only (OSS-safe).
func (*Endpoint) Manifest() agent.ModelEndpointManifest {
	return agent.ModelEndpointManifest{
		Company:     agent.CompanyMeta,
		HumanLabel:  "Meta",
		Family:      agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoMuseMSP},
		Hosts: []agent.HostDesc{
			{
				Host:          agent.HostOAuthCLI,
				Protocol:      agent.ProtoMuseMSP,
				AuthModes:     []agent.AuthMode{agent.AuthHostSession},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostHostSubscription,
				BaseURLTmpl:   "", // host login
				EnvKeys:       []string{},
			},
			{
				Host:          agent.HostDirect,
				Protocol:      agent.ProtoMuseMSP,
				AuthModes:     []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered},
				BringsOwnAuth: false,
				NeedsAPIKey:   true,
				CostModel:     agent.CostMeteredPerToken,
				BaseURLTmpl:   "https://api.meta.ai/v1",
				EnvKeys:       []string{"META_API_KEY"},
			},
		},
		Models: []agent.ModelDesc{
			{
				ID:               "muse-wonder-1.0",
				HumanLabel:       "Muse Wonder",
				ContextWindow:    256000,
				SupportsTools:    true,
				SupportsJSONMode: false,
				Hosts:            []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect},
			},
			{
				ID:               "muse-wonder-full-1.0",
				HumanLabel:       "Muse Wonder Full",
				ContextWindow:    256000,
				SupportsTools:    true,
				SupportsJSONMode: false,
				Hosts:            []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect},
			},
			{
				ID:               "muse-spark-1.3",
				HumanLabel:       "Muse Spark",
				ContextWindow:    256000,
				SupportsTools:    true,
				SupportsJSONMode: false,
				Hosts:            []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect},
			},
			{
				ID:               "muse-spark-1.2",
				HumanLabel:       "Muse Spark 1.2",
				ContextWindow:    256000,
				SupportsTools:    true,
				SupportsJSONMode: false,
				Hosts:            []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect},
			},
		},
	}
}
