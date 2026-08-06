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
