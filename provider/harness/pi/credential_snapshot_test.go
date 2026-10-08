package pi

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// credentialSnapshotKeys is the representative full-set credential snapshot a
// spawn-time Spec.Env layer carries: the model pin plus every sibling-harness
// and tool credential the platform fans out. Every name here is already
// referenced in-tree (harness endpoint/auth paths, aggregator routing, local
// VCS/tracker credentials), so the set proves the rail, not any one key.
// Blocklisted-from-inheritance names (ANTHROPIC_API_KEY, OPENAI_API_KEY,
// GEMINI_API_KEY, ANTHROPIC_BASE_URL) are deliberately included: they must
// still ride when they arrive on the trusted Spec.Env layer — the blocklist
// strips the inherited parent env only.
var credentialSnapshotKeys = []string{
	PiKeyEnvVar,
	gatewayBearerEnvVar,
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"ANTHROPIC_BASE_URL",
	"AI_GATEWAY_API_KEY",
	"ZAI_API_KEY",
	"OPENROUTER_API_KEY",
	"CODEX_API_KEY",
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"LINEAR_API_KEY",
}

// snapshotSpecEnv builds a Spec.Env carrying the full snapshot set with
// per-key cell values distinguishable from any host value.
func snapshotSpecEnv() map[string]string {
	env := make(map[string]string, len(credentialSnapshotKeys))
	for _, k := range credentialSnapshotKeys {
		env[k] = "cell-" + k
	}
	return env
}

// gatewayBinding returns a routable translating-gateway binding with no Env of
// its own, so applyEndpoint runs its full projection (model honor, key
// mirror) over a snapshot-supplied Spec.Env.
func gatewayBinding(model string) *agent.EndpointBinding {
	return &agent.EndpointBinding{
		Company:  agent.CompanyAnthropic,
		BaseURL:  "http://127.0.0.1:8080/v1",
		Host:     agent.HostGateway,
		Protocol: agent.ProtoAnthropicMessages,
		Model:    model,
	}
}

// TestCredentialSnapshotParity_FullSnapshotRidesHeadlessChildEnv proves pi's
// credential parity rides the shared spawn-time Spec.Env snapshot rail, not a
// pi-specific fan-out list: every snapshot credential placed on Spec.Env
// survives applyEndpoint + the session credential file with its cell value
// intact, while the headless child env itself carries no credential value.
// The blocklist/redaction contract holds at the same time: blocklisted
// host-inherited credentials never leak in, and a runner-only control is
// refused even when it arrives on the trusted Spec.Env layer.
func TestCredentialSnapshotParity_FullSnapshotRidesHeadlessChildEnv(t *testing.T) {
	// Not parallel: mutates process env.
	const hostCanary = "host-canary-must-not-leak"
	t.Setenv("ANTHROPIC_API_KEY", hostCanary)
	t.Setenv("OPENAI_API_KEY", hostCanary)
	t.Setenv("GEMINI_API_KEY", hostCanary)
	t.Setenv("ATTACH_TOKEN", "runner-control-must-not-reach-pi")

	snapshot := snapshotSpecEnv()
	// A runner-only control on the trusted layer must still be refused.
	snapshot["ATTACH_TOKEN"] = "spec-layer-runner-control-must-not-reach-pi"

	layout := newSessionLayout(t.TempDir())
	projected, err := applyEndpoint(agent.Spec{
		Cwd:      t.TempDir(),
		Env:      snapshot,
		Endpoint: gatewayBinding("served-model"),
	})
	if err != nil {
		t.Fatalf("applyEndpoint rejected a routable gateway binding: %v", err)
	}
	env := composeChildEnv(projected, layout, "sess-token")

	// No credential value rides the child env; non-credential bindings do.
	for _, e := range env {
		if isSessionCredentialEnv(e) {
			t.Fatalf("snapshot credential rides the headless child env: %q", e)
		}
	}
	if !hasEnvVal(env, "ANTHROPIC_BASE_URL", "cell-ANTHROPIC_BASE_URL") {
		t.Errorf("non-credential snapshot binding ANTHROPIC_BASE_URL missing from pi headless child env")
	}
	entries := sessionCredentialEntries(projected)
	byValue := make(map[string]string, len(entries))
	for _, e := range entries {
		byValue[e.Env] = e.Value
	}
	for _, k := range credentialSnapshotKeys {
		switch k {
		case "ANTHROPIC_BASE_URL", "GITHUB_TOKEN", "GH_TOKEN", "LINEAR_API_KEY", "CODEX_API_KEY":
			continue // non-model credentials: out of the session-file rail's scope
		}
		if byValue[k] != "cell-"+k {
			t.Errorf("snapshot key %s = %q in the credential file, want %q", k, byValue[k], "cell-"+k)
		}
	}
	// The snapshot-supplied model pin survives the binding merge untouched:
	// applyEndpoint mirrors a cell key onto PiKeyEnvVar only when absent —
	// and the file rail carries that same surviving value.
	if byValue[PiKeyEnvVar] != "cell-"+PiKeyEnvVar {
		t.Errorf("snapshot %s = %q in the credential file, want the surviving snapshot value", PiKeyEnvVar, byValue[PiKeyEnvVar])
	}
	for _, e := range env {
		if strings.Contains(e, hostCanary) {
			t.Fatalf("blocklisted host credential leaked into pi child env: %q", e)
		}
	}
	if hasEnvKey(env, "ATTACH_TOKEN") {
		t.Errorf("runner-only ATTACH_TOKEN reached the pi child env from either layer")
	}
	if !hasEnvVal(env, piHandshakeEnvVar, "sess-token") {
		t.Errorf("handshake token missing; the env under test is not the headless lane")
	}
}

// TestCredentialSnapshotParity_FullSnapshotRidesInteractiveChildEnv is the
// interactive-lane companion: the same full snapshot set on the projected
// spec fans out to the session credential file, while interactiveChildEnv —
// the override layer the PTY child is spawned with — carries no credential
// value. A runner-only control riding the same Spec.Env layer is refused,
// exactly as the headless lane refuses it. RED proof: drop a snapshot key
// from sessionCredentialEntries and this test fails.
func TestCredentialSnapshotParity_FullSnapshotRidesInteractiveChildEnv(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	projected, err := applyEndpoint(agent.Spec{
		Cwd:      t.TempDir(),
		Env:      snapshotSpecEnv(),
		Endpoint: gatewayBinding("served-model"),
	})
	if err != nil {
		t.Fatalf("applyEndpoint rejected a routable gateway binding: %v", err)
	}
	// A runner-only control on the trusted layer must be refused here too —
	// the parity contract is full-snapshot delivery MINUS supervisor controls.
	projected.Env["ATTACH_TOKEN"] = "spec-layer-runner-control-must-not-reach-pi"
	got := interactiveChildEnv(projected, layout)
	for _, k := range credentialSnapshotKeys {
		switch k {
		case "ANTHROPIC_BASE_URL", "GITHUB_TOKEN", "GH_TOKEN", "LINEAR_API_KEY", "CODEX_API_KEY":
			continue // non-model credentials: ride env, out of the file rail's scope
		}
		// Session-carried credential names were fanned out to the file;
		// session-absent ones are shadowed EMPTY (the PTY host inherits
		// unmentioned parent names, so absence must be explicit). Either
		// way no credential VALUE may ride the override map.
		if v, present := got[k]; present && v != "" && v != "cell-"+k {
			t.Errorf("snapshot credential %s = %q reached the interactive child env; it must ride the file", k, v)
		}
		if v := got[k]; v != "" && len(projected.Env[k]) == 0 {
			t.Errorf("session-absent snapshot credential %s shadowed non-empty: %q", k, v)
		}
	}
	if got["ANTHROPIC_BASE_URL"] != "cell-ANTHROPIC_BASE_URL" {
		t.Errorf("non-credential snapshot binding ANTHROPIC_BASE_URL = %q, want %q", got["ANTHROPIC_BASE_URL"], "cell-ANTHROPIC_BASE_URL")
	}
	if _, present := got["ATTACH_TOKEN"]; present {
		t.Errorf("runner-only ATTACH_TOKEN reached the interactive child env from Spec.Env")
	}
	entries := sessionCredentialEntries(projected)
	byValue := make(map[string]string, len(entries))
	for _, e := range entries {
		byValue[e.Env] = e.Value
	}
	for _, k := range credentialSnapshotKeys {
		switch k {
		case "ANTHROPIC_BASE_URL", "GITHUB_TOKEN", "GH_TOKEN", "LINEAR_API_KEY", "CODEX_API_KEY":
			continue // non-model credentials: out of the session-file rail's scope
		}
		if byValue[k] != "cell-"+k {
			t.Errorf("snapshot key %s = %q in the credential file, want %q", k, byValue[k], "cell-"+k)
		}
	}
}
