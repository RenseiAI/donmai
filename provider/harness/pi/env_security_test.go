package pi

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestEnvHygiene_BlocklistedHostSecretNeverLeaks is smoke 10: a blocklisted
// credential present in the daemon (host) env must never reach the pi child.
// Because pi runs tools with the full permissions of the spawning user, a
// leaked host key would be exfiltratable by any tool call (design §5.3).
func TestEnvHygiene_BlocklistedHostSecretNeverLeaks(t *testing.T) {
	// Not parallel: mutates process env.
	const canary = "sk-host-canary-must-not-leak"
	t.Setenv("OPENAI_API_KEY", canary)      // blocklisted host credential
	t.Setenv("ANTHROPIC_API_KEY", canary)   // blocklisted host credential
	t.Setenv("DONMAI_HARMLESS_VAR", "keep") // non-blocklisted — should pass through

	layout := newSessionLayout(t.TempDir())
	env := composeChildEnv(agent.Spec{Cwd: t.TempDir()}, layout, "sess-token")

	for _, e := range env {
		if strings.Contains(e, canary) {
			t.Fatalf("blocklisted host secret leaked into pi child env: %q", e)
		}
	}
	// A harmless host var still passes through (we filter creds, not everything).
	if !hasEnvKey(env, "DONMAI_HARMLESS_VAR") {
		t.Errorf("non-blocklisted host var was dropped; env composition is too aggressive")
	}
	// pi's documented agent/config home is redirected into the per-session
	// subdirectory (auth isolation), NEVER the session root itself — see
	// sessionLayout.agentHome for why collapsing the two breaks pi's own
	// resume lookup.
	if !hasEnvVal(env, piCodingAgentDirEnvVar, layout.agentHome) {
		t.Errorf("%s not redirected to the session agent-home dir; a host ~/.pi/agent could leak in", piCodingAgentDirEnvVar)
	}
	// The per-session handshake token rides the child env (the extension reads
	// it to prove liveness/identity on the handshake round-trip).
	if !hasEnvVal(env, piHandshakeEnvVar, "sess-token") {
		t.Errorf("handshake token not set on the child env under %s", piHandshakeEnvVar)
	}
}

// TestConfigHomeIsolation_DocumentedVarsRedirected is the "config-home
// isolation" fixture (ADR-2026-08-06 D8, pi row; ADR-2026-08-12 D4.1/D4.2):
// composeChildEnv redirects the TWO documented pi variables
// (docs/environment-variables.md — PI_CODING_AGENT_DIR for the config/agent
// home, PI_CODING_AGENT_SESSION_DIR for session storage) into the session
// dir, and the redirect must win over whatever a fleet box's own shell
// profile set, not merely be present alongside it.
//
// A prior cut set four UNDOCUMENTED candidate names instead (PI_HOME,
// PI_CONFIG_DIR, PI_STATE_DIR, XDG_CONFIG_HOME) because the exact variable
// was unverified against a real binary; none of them were load-bearing
// (ADR-2026-08-12 F3). Per that ADR's Implementation notes, "the candidate
// set is deleted in the same change that names the real one — not left
// standing beside it" — this test replaces
// TestConfigHomeIsolation_AllHomeVarsRedirected rather than joining it.
func TestConfigHomeIsolation_DocumentedVarsRedirected(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv(piCodingAgentDirEnvVar, "/home/fleetuser/.pi/agent")
	t.Setenv(piCodingAgentSessionDirEnvVar, "/home/fleetuser/.pi/agent/sessions")
	// The four RETIRED candidate names are set on the host too, to prove this
	// package no longer ATTACHES any significance to them — it never reads
	// or redirects them; whatever value the host happens to have is inert.
	t.Setenv("PI_HOME", "/home/fleetuser/.pi")
	t.Setenv("PI_CONFIG_DIR", "/home/fleetuser/.config/pi")
	t.Setenv("PI_STATE_DIR", "/home/fleetuser/.local/state/pi")
	t.Setenv("XDG_CONFIG_HOME", "/home/fleetuser/.config")

	layout := newSessionLayout(t.TempDir())
	env := composeChildEnv(agent.Spec{Cwd: t.TempDir()}, layout, "sess-token")

	if !hasEnvVal(env, piCodingAgentDirEnvVar, layout.agentHome) {
		t.Errorf("%s not redirected to the session agent-home dir %q; a fleet box's personal pi agent dir could leak in", piCodingAgentDirEnvVar, layout.agentHome)
	}
	if !hasEnvVal(env, piCodingAgentSessionDirEnvVar, layout.root) {
		t.Errorf("%s not redirected to the session dir %q", piCodingAgentSessionDirEnvVar, layout.root)
	}
}

// TestOfflinePostureEnv_DefaultsOnUnlessExplicit pins ADR-2026-08-12 D4.3:
// PI_OFFLINE and PI_SKIP_VERSION_CHECK default to "1" for every
// execution-layer-spawned session, but an explicit spec.Env binding always
// wins over the default — "a session may re-enable either, recorded as an
// explicit environment-binding entry rather than acquired by omission."
func TestOfflinePostureEnv_DefaultsOnUnlessExplicit(t *testing.T) {
	t.Parallel()

	layout := newSessionLayout(t.TempDir())
	env := composeChildEnv(agent.Spec{Cwd: t.TempDir()}, layout, "sess-token")
	if !hasEnvVal(env, piOfflineEnvVar, "1") {
		t.Errorf("%s not defaulted to 1", piOfflineEnvVar)
	}
	if !hasEnvVal(env, piSkipVersionCheckEnvVar, "1") {
		t.Errorf("%s not defaulted to 1", piSkipVersionCheckEnvVar)
	}

	explicit := agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{piOfflineEnvVar: "0", piSkipVersionCheckEnvVar: "0"},
	}
	env2 := composeChildEnv(explicit, layout, "sess-token")
	if !hasEnvVal(env2, piOfflineEnvVar, "0") {
		t.Errorf("explicit %s=0 was overridden by the default", piOfflineEnvVar)
	}
	if !hasEnvVal(env2, piSkipVersionCheckEnvVar, "0") {
		t.Errorf("explicit %s=0 was overridden by the default", piSkipVersionCheckEnvVar)
	}
}

// TestEnvHygiene_ResolvedCellKeyRides confirms the resolved cell's key still
// reaches the child via Spec.Env (trusted layer) even though the same key name
// is blocklisted from the host env — the cell must be able to authenticate.
func TestEnvHygiene_ResolvedCellKeyRides(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "host-value-blocked")
	layout := newSessionLayout(t.TempDir())
	spec := agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{"ANTHROPIC_API_KEY": "cell-resolved-key"},
	}
	env := composeChildEnv(spec, layout, "sess-token")
	if !hasEnvVal(env, "ANTHROPIC_API_KEY", "cell-resolved-key") {
		t.Errorf("resolved cell key did not ride Spec.Env into the child")
	}
	if hasEnvVal(env, "ANTHROPIC_API_KEY", "host-value-blocked") {
		t.Errorf("host value won over the resolved cell key")
	}
}

// TestApplyEndpoint_MirrorsKeyAndRejectsUnroutable covers the endpoint read
// site: gateway-hosted cells are admitted, the cell key is mirrored onto
// PiKeyEnvVar (for the pin's env reference), and an unsupported host is
// refused loudly.
func TestApplyEndpoint_MirrorsKeyAndRejectsUnroutable(t *testing.T) {
	t.Parallel()
	spec, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyAnthropic,
			BaseURL:  "http://127.0.0.1:8080/v1",
			Host:     agent.HostGateway,
			Protocol: agent.ProtoAnthropicMessages,
			Model:    "claude-x",
			Env:      map[string]string{"ANTHROPIC_API_KEY": "k-123"},
		},
	})
	if err != nil {
		t.Fatalf("routable endpoint rejected: %v", err)
	}
	if spec.Env[PiKeyEnvVar] != "k-123" {
		t.Errorf("cell key not mirrored onto %s: %v", PiKeyEnvVar, spec.Env)
	}
	if spec.Model != "claude-x" {
		t.Errorf("Endpoint.Model not honored: %q", spec.Model)
	}

	for _, tt := range []struct {
		name    string
		baseURL string
	}{
		{name: "empty BaseURL", baseURL: ""},
		{name: "relative BaseURL", baseURL: "127.0.0.1:8080/v1"},
		{name: "missing hostname", baseURL: "http:///v1"},
		{name: "non-loopback BaseURL", baseURL: "https://api.example.com/v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := applyEndpoint(agent.Spec{
				Endpoint: &agent.EndpointBinding{
					Company:  agent.CompanyAnthropic,
					BaseURL:  tt.baseURL,
					Host:     agent.HostGateway,
					Protocol: agent.ProtoAnthropicMessages,
				},
			})
			if err == nil {
				t.Errorf("gateway endpoint with %s should be refused", tt.name)
			}
		})
	}

	// Unroutable host must fail loudly (mis-routing would mis-bill).
	if _, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{Company: agent.CompanyAnthropic, Host: agent.HostAzure, Protocol: agent.ProtoAnthropicMessages},
	}); err == nil {
		t.Errorf("unroutable host (azure) should be refused by applyEndpoint")
	}
}

func hasEnvKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

func hasEnvVal(env []string, key, val string) bool {
	for _, e := range env {
		if e == key+"="+val {
			return true
		}
	}
	return false
}
