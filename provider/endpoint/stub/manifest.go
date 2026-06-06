package stub

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the test-only stub company endpoint declaration (§3.5). It
// SPEAKS the sentinel stub protocol on one local host with a single model, so
// the matrix carries a stub × stub cell that satisfies the parity intersection
// rule without special-casing.
func (*Endpoint) Manifest() agent.ModelEndpointManifest {
	return agent.ModelEndpointManifest{
		Company:     agent.CompanyStub,
		HumanLabel:  "Test Stub Endpoint",
		Family:      agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoStub},
		Hosts: []agent.HostDesc{
			{
				Host:          agent.HostLocal,
				Protocol:      agent.ProtoStub,
				AuthModes:     []agent.AuthMode{agent.AuthLocal},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostLocalFree,
				BaseURLTmpl:   "http://127.0.0.1:0",
				EnvKeys:       []string{},
			},
		},
		Models: []agent.ModelDesc{
			{
				ID:               "stub-model",
				HumanLabel:       "Stub Model",
				ContextWindow:    8192,
				SupportsTools:    true,
				SupportsJSONMode: true,
				Hosts:            []agent.ServingHost{agent.HostLocal},
			},
		},
	}
}
