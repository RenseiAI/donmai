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
	// pi state home is redirected into the session dir (auth isolation).
	if !hasEnvVal(env, "PI_HOME", layout.root) {
		t.Errorf("PI_HOME not redirected to the session state dir; a host ~/.pi could leak in")
	}
	// The per-session handshake token rides the child env (the extension reads
	// it to prove liveness/identity on the handshake round-trip).
	if !hasEnvVal(env, piHandshakeEnvVar, "sess-token") {
		t.Errorf("handshake token not set on the child env under %s", piHandshakeEnvVar)
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
// site: the cell key is mirrored onto PiKeyEnvVar (for the pin's env
// reference), and a company/host outside pi's Drive surface is refused loudly.
func TestApplyEndpoint_MirrorsKeyAndRejectsUnroutable(t *testing.T) {
	t.Parallel()
	spec, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyAnthropic,
			Host:     agent.HostDirect,
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
