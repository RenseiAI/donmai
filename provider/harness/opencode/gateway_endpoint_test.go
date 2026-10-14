package opencode

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestApplyEndpoint_GatewayHostProjectsLoopbackBearer is the headless-lane
// acceptance half: a gateway binding reaches the harness as the loopback
// surface plus the short-TTL per-session bearer as the injected provider's
// key — never the upstream credential, never an off-host route. A hostile
// repo hook reading the child env gets a bearer that dies in minutes and
// dials nothing but the worker-local gateway.
func TestApplyEndpoint_GatewayHostProjectsLoopbackBearer(t *testing.T) {
	t.Parallel()
	const bearer = "sess-bearer-gateway"
	got, err := applyEndpoint(agent.Spec{
		Model: "spec-model-loses",
		Env:   map[string]string{"SAFE_SESSION": "kept", "OPENAI_API_KEY": "ambient-must-not-win"},
		Endpoint: &agent.EndpointBinding{
			Company:   agent.CompanyOpenAI,
			Model:     "served-model",
			BaseURL:   "http://127.0.0.1:9/v1",
			Protocol:  agent.ProtoOpenAIChat,
			Host:      agent.HostGateway,
			Mechanism: agent.AuthAPIKey,
			Env:       map[string]string{"DONMAI_GW_TOKEN": bearer},
		},
	})
	if err != nil {
		t.Fatalf("applyEndpoint gateway: %v", err)
	}
	if got.Model != "served-model" {
		t.Errorf("Model = %q, want the binding model", got.Model)
	}
	if got.Env[OCKeyEnvVar] != bearer {
		t.Errorf("injected key = %q, want the session bearer", got.Env[OCKeyEnvVar])
	}
	if got.Env["SAFE_SESSION"] != "kept" {
		t.Errorf("unrelated session env lost: %v", got.Env)
	}
	if got.Env["OPENAI_API_KEY"] != "" {
		t.Errorf("ambient provider key survived the gateway projection: %v", got.Env)
	}
	cfg := buildConfig(got)
	prov := cfg.Provider[OCProviderID]
	if prov.Options.BaseURL != "http://127.0.0.1:9/v1" {
		t.Errorf("injected baseURL = %q, want the loopback gateway surface", prov.Options.BaseURL)
	}
	if prov.Options.APIKey != "{env:"+OCKeyEnvVar+"}" {
		t.Errorf("apiKey = %q, want the env indirection (never inline)", prov.Options.APIKey)
	}
}
