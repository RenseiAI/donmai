package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSessionResolvedProfile_HarnessRoundTrips verifies the daemon's
// SessionResolvedProfile wire shape (nested in SessionDetail) round-trips
// the additive Harness field so the platform's catalog loop-driver
// attribute survives the daemon→`donmai agent run` hop (the runner reads it
// to select the harness-native provider). It marshals the profile directly
// — the daemon forwards it opaquely inside SessionDetail.
func TestSessionResolvedProfile_HarnessRoundTrips(t *testing.T) {
	t.Parallel()

	in := SessionResolvedProfile{
		Harness:  "agy",
		Provider: "gemini",
		Model:    "gemini-3.1-pro",
		Endpoint: &SessionEndpointBinding{
			Company: "google", Model: "gemini-3.1-pro", Protocol: "google-genai", Host: "vertex",
			EndpointID: "endpoint-google", EndpointOperator: "google", EndpointRevision: "r1", ModelAuthor: "google",
			AuthBindingID: "auth-google", AuthAuthority: "google", AuthCommercialMode: "usage_billed",
			AuthBindingScope: "process", AuthPortability: "portable", AuthDelivery: "environment", Mechanism: "service_account",
		},
	}

	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal SessionResolvedProfile: %v", err)
	}
	// The camelCase wire tag must be present so the platform producer and
	// the runner consumer agree on the key.
	if !strings.Contains(string(buf), `"harness":"agy"`) {
		t.Fatalf("marshalled JSON missing harness key: %s", buf)
	}

	var out SessionResolvedProfile
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal SessionResolvedProfile: %v", err)
	}
	if got := out.Harness; got != "agy" {
		t.Errorf("Harness round-trip = %q; want %q", got, "agy")
	}
	if got := out.Provider; got != "gemini" {
		t.Errorf("Provider round-trip = %q; want %q", got, "gemini")
	}
	if out.Endpoint == nil || out.Endpoint.EndpointID != "endpoint-google" || out.Endpoint.AuthBindingID != "auth-google" {
		t.Errorf("Endpoint round-trip = %+v", out.Endpoint)
	}
	if strings.Contains(string(buf), `"env"`) || strings.Contains(string(buf), "credentialValue") {
		t.Fatalf("endpoint wire projection is not secret-free: %s", buf)
	}
}

// TestSessionResolvedProfile_HarnessOmitemptyAbsent verifies the additive
// field is omitted when empty, so legacy dispatches that predate the
// harness attribute do not gain a spurious "harness":"" key on the wire.
func TestSessionResolvedProfile_HarnessOmitemptyAbsent(t *testing.T) {
	t.Parallel()

	buf, err := json.Marshal(&SessionResolvedProfile{Provider: "claude"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(buf), "harness") {
		t.Errorf("empty Harness leaked into wire shape: %s", buf)
	}
}

// TestSessionEndpointBinding_BaseURLRoundTrips verifies the additive BaseURL
// field (the dispatch-wire endpoint-baseurl ADR) survives marshal/unmarshal
// under its camelCase wire tag, so an endpoint-driven harness's aggregator
// base URL reaches `donmai agent run` across the poll/detail boundary.
func TestSessionEndpointBinding_BaseURLRoundTrips(t *testing.T) {
	t.Parallel()

	in := SessionEndpointBinding{
		Company:  "openai",
		Model:    "openai/gpt-test",
		BaseURL:  "https://ai-gateway.example.com/v1",
		Protocol: "openai-chat",
		Host:     "direct",
	}

	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal SessionEndpointBinding: %v", err)
	}
	if !strings.Contains(string(buf), `"baseUrl":"https://ai-gateway.example.com/v1"`) {
		t.Fatalf("marshalled JSON missing baseUrl key: %s", buf)
	}

	var out SessionEndpointBinding
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal SessionEndpointBinding: %v", err)
	}
	if out.BaseURL != in.BaseURL {
		t.Errorf("BaseURL round-trip = %q; want %q", out.BaseURL, in.BaseURL)
	}
}

// TestSessionEndpointBinding_BaseURLOmitemptyAbsent verifies the additive
// field is omitted when empty, so a legacy dispatch that predates baseUrl
// round-trips byte-identically (mixed-version-safe both directions: an
// older platform producer omits the key, and a newer daemon reading an
// older payload sees a correctly-empty BaseURL rather than a spurious key).
func TestSessionEndpointBinding_BaseURLOmitemptyAbsent(t *testing.T) {
	t.Parallel()

	buf, err := json.Marshal(&SessionEndpointBinding{Company: "anthropic", Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(buf), "baseUrl") {
		t.Errorf("empty BaseURL leaked into wire shape: %s", buf)
	}

	var out SessionEndpointBinding
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty on a pre-baseUrl payload", out.BaseURL)
	}
}
