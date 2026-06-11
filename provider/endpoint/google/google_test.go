package google

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestManifest_Shape asserts the Google manifest declares the expected
// company / family / ABI / protocol surface (the SoT the matrix harvests),
// and pins the vertex cell's routing contract: a {region}-templated
// aiplatform base URL plus the project-id env key the gemini harness
// requires to build the publishers resource path. Losing either silently
// strands the declared raw × google × vertex cell.
func TestManifest_Shape(t *testing.T) {
	m := New().Manifest()
	if m.Company != agent.CompanyGoogle {
		t.Errorf("company = %q, want google", m.Company)
	}
	if m.Family != agent.FamilyModelEndpoint {
		t.Errorf("family = %q, want model-endpoint", m.Family)
	}
	if m.ContractABI != "model-endpoint/v1" {
		t.Errorf("contractAbi = %q, want model-endpoint/v1", m.ContractABI)
	}

	var vertex *agent.HostDesc
	for i := range m.Hosts {
		if m.Hosts[i].Host == agent.HostVertex {
			vertex = &m.Hosts[i]
			break
		}
	}
	if vertex == nil {
		t.Fatal("manifest declares no vertex host")
	}
	if !strings.Contains(vertex.BaseURLTmpl, "{region}") {
		t.Errorf("vertex BaseURLTmpl = %q, want a {region} placeholder", vertex.BaseURLTmpl)
	}
	if !strings.Contains(vertex.BaseURLTmpl, "aiplatform.googleapis.com") {
		t.Errorf("vertex BaseURLTmpl = %q, want an aiplatform.googleapis.com host", vertex.BaseURLTmpl)
	}
	hasProjectKey := false
	for _, k := range vertex.EnvKeys {
		if k == "GOOGLE_VERTEX_PROJECT_ID" {
			hasProjectKey = true
		}
	}
	if !hasProjectKey {
		t.Errorf("vertex EnvKeys = %v, want GOOGLE_VERTEX_PROJECT_ID (the gemini harness builds the publishers path from it)", vertex.EnvKeys)
	}
}

// TestResolve_Vertex verifies pure resolution of the vertex cell: the base
// URL is region-templated, the region rides the binding for harness path
// mapping, and only the host's declared env keys are copied from
// EnvProvided (never read from process env).
func TestResolve_Vertex(t *testing.T) {
	got, err := New().Resolve(context.Background(), agent.EndpointRequest{
		Model:  "gemini-2.5-pro",
		Host:   agent.HostVertex,
		Auth:   agent.AuthBYOK,
		Region: "us-central1",
		EnvProvided: map[string]string{
			"GOOGLE_VERTEX_PROJECT_ID": "proj-123",
			"UNDECLARED_KEY":           "must-not-copy",
		},
	})
	if err != nil {
		t.Fatalf("Resolve(vertex) error: %v", err)
	}
	if got.BaseURL != "https://us-central1-aiplatform.googleapis.com" {
		t.Errorf("BaseURL = %q, want the us-central1 aiplatform host", got.BaseURL)
	}
	if got.Region != "us-central1" {
		t.Errorf("Region = %q, want us-central1", got.Region)
	}
	if got.Protocol != agent.ProtoGeminiGenerate {
		t.Errorf("Protocol = %q, want gemini-generate", got.Protocol)
	}
	if got.Env["GOOGLE_VERTEX_PROJECT_ID"] != "proj-123" {
		t.Errorf("Env[GOOGLE_VERTEX_PROJECT_ID] = %q, want proj-123", got.Env["GOOGLE_VERTEX_PROJECT_ID"])
	}
	if _, leaked := got.Env["UNDECLARED_KEY"]; leaked {
		t.Error("Env copied UNDECLARED_KEY, want only the host's declared keys")
	}
}

// TestResolve_UnknownHost verifies an unmapped serving host fails loudly
// instead of resolving a mis-routed binding.
func TestResolve_UnknownHost(t *testing.T) {
	_, err := New().Resolve(context.Background(), agent.EndpointRequest{
		Model: "gemini-2.5-pro",
		Host:  agent.ServingHost("mainframe"),
	})
	if err == nil {
		t.Fatal("Resolve(unknown host) = nil error, want loud failure")
	}
}
