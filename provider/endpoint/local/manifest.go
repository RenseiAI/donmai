package local

import "github.com/RenseiAI/donmai/agent"

// Manifest returns the Local company endpoint declaration (§3.4). Local SPEAKS
// only the bare ollama protocol on one local host. Models are discovered
// dynamically at runtime, so the Models slice is empty. SupportsJSONMode is
// declared true via the format:"json" whole-response primitive. No env keys.
func (*Endpoint) Manifest() agent.ModelEndpointManifest {
	return agent.ModelEndpointManifest{
		Company:     agent.CompanyLocal,
		HumanLabel:  "Local (Ollama)",
		Family:      agent.FamilyModelEndpoint,
		ContractABI: "model-endpoint/v1",
		Speaks:      []agent.WireProtocol{agent.ProtoOllama},
		Hosts: []agent.HostDesc{
			{
				Host:          agent.HostLocal,
				Protocol:      agent.ProtoOllama,
				AuthModes:     []agent.AuthMode{agent.AuthLocal},
				BringsOwnAuth: true,
				NeedsAPIKey:   false,
				CostModel:     agent.CostLocalFree,
				BaseURLTmpl:   "http://127.0.0.1:11434",
				EnvKeys:       []string{},
			},
		},
		Models: []agent.ModelDesc{},
	}
}
