package anthropic

import (
	"context"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestManifest_Shape asserts the Anthropic manifest declares the expected
// company / family / ABI / protocol surface (the SoT the matrix harvests).
func TestManifest_Shape(t *testing.T) {
	m := New().Manifest()
	if m.Company != agent.CompanyAnthropic {
		t.Errorf("company = %q, want anthropic", m.Company)
	}
	if m.Family != agent.FamilyModelEndpoint {
		t.Errorf("family = %q, want model-endpoint", m.Family)
	}
	if m.ContractABI != "model-endpoint/v1" {
		t.Errorf("contractAbi = %q, want model-endpoint/v1", m.ContractABI)
	}
	if len(m.Speaks) != 1 || m.Speaks[0] != agent.ProtoAnthropicMessages {
		t.Errorf("speaks = %v, want [anthropic-messages]", m.Speaks)
	}
	// Anthropic Messages has no json_schema mode.
	for _, md := range m.Models {
		if md.SupportsJSONMode {
			t.Errorf("model %q: SupportsJSONMode=true, want false (Messages has no json_schema)", md.ID)
		}
	}
}

// TestResolve_Direct verifies pure resolution templates the base URL and copies
// only the host's declared env keys from EnvProvided (never reads process env).
func TestResolve_Direct(t *testing.T) {
	e := New()
	got, err := e.Resolve(context.Background(), agent.EndpointRequest{
		Model:     "claude-sonnet-4-5",
		Host:      agent.HostDirect,
		Mechanism: agent.AuthAPIKey,
		Auth:      agent.AuthBYOK,
		EnvProvided: map[string]string{
			"ANTHROPIC_API_KEY": "sk-test",
			"UNRELATED":         "leak", // must NOT be copied
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Company != agent.CompanyAnthropic {
		t.Errorf("company = %q", got.Company)
	}
	if got.BaseURL != "https://api.anthropic.com" {
		t.Errorf("baseURL = %q", got.BaseURL)
	}
	if got.Protocol != agent.ProtoAnthropicMessages {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.Mechanism != agent.AuthAPIKey {
		t.Errorf("mechanism = %q, want api_key", got.Mechanism)
	}
	if got.CostModel != agent.CostMeteredPerToken {
		t.Errorf("costModel = %q", got.CostModel)
	}
	if got.Env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("env ANTHROPIC_API_KEY not copied: %v", got.Env)
	}
	if _, leaked := got.Env["UNRELATED"]; leaked {
		t.Errorf("Resolve copied an undeclared env key: %v", got.Env)
	}
	if got.IsZero() {
		t.Errorf("resolved binding reports IsZero")
	}
}

// TestResolve_RegionTemplating verifies {region} substitution on bedrock.
func TestResolve_RegionTemplating(t *testing.T) {
	got, err := New().Resolve(context.Background(), agent.EndpointRequest{
		Model:  "claude-sonnet-4-5",
		Host:   agent.HostBedrock,
		Auth:   agent.AuthMetered,
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.BaseURL != "https://bedrock-runtime.us-east-1.amazonaws.com" {
		t.Errorf("region not templated: %q", got.BaseURL)
	}
	if got.Region != "us-east-1" {
		t.Errorf("Region not recorded on the binding: %q (harness env/path mapping reads it)", got.Region)
	}
}

// TestResolve_UnknownHost errors rather than dialing or guessing.
func TestResolve_UnknownHost(t *testing.T) {
	_, err := New().Resolve(context.Background(), agent.EndpointRequest{
		Model: "claude-sonnet-4-5",
		Host:  agent.HostLocal, // Anthropic has no local host
	})
	if err == nil {
		t.Fatalf("expected error for unknown host, got nil")
	}
}
