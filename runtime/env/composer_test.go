package env_test

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runtime/env"
)

func TestComposeSpecOverridesBaseAndDeterministic(t *testing.T) {
	t.Parallel()

	c := env.NewComposer()
	base := map[string]string{
		"PATH": "/usr/bin",
		"FOO":  "from-base",
		"BAR":  "from-base",
	}
	spec := agent.Spec{Env: map[string]string{
		"FOO": "from-spec",
		"BAZ": "from-spec",
	}}

	got := c.Compose(base, spec)
	want := []string{
		"BAR=from-base",
		"BAZ=from-spec",
		"FOO=from-spec",
		"PATH=/usr/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compose mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestComposeBlocksDefaultBlocklist(t *testing.T) {
	t.Parallel()

	c := env.NewComposer()
	base := map[string]string{
		"ANTHROPIC_API_KEY":      "leak-1",
		"ANTHROPIC_AUTH_TOKEN":   "leak-2",
		"ANTHROPIC_BASE_URL":     "leak-3",
		"OPENCLAW_GATEWAY_TOKEN": "leak-4",
		"PATH":                   "/usr/bin",
	}

	got := c.Compose(base, agent.Spec{})
	for _, kv := range got {
		switch kv {
		case "ANTHROPIC_API_KEY=leak-1",
			"ANTHROPIC_AUTH_TOKEN=leak-2",
			"ANTHROPIC_BASE_URL=leak-3",
			"OPENCLAW_GATEWAY_TOKEN=leak-4":
			t.Fatalf("blocked key leaked through: %q", kv)
		}
	}
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Fatalf("expected only PATH after blocklist filter, got: %v", got)
	}
}

func TestComposeSpecCanOverrideBlockedKey(t *testing.T) {
	t.Parallel()

	// The blocklist applies to the host pass-through only. Spec.Env is
	// runner-set and intentionally trusted, so a runner-supplied
	// ANTHROPIC_API_KEY is allowed through.
	c := env.NewComposer()
	base := map[string]string{
		"ANTHROPIC_API_KEY": "host-leak",
		"PATH":              "/usr/bin",
	}
	spec := agent.Spec{Env: map[string]string{
		"ANTHROPIC_API_KEY": "runner-set",
	}}

	got := c.Compose(base, spec)
	want := []string{
		"ANTHROPIC_API_KEY=runner-set",
		"PATH=/usr/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compose mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestComposeEmptySpecEnvPreserved(t *testing.T) {
	t.Parallel()

	c := env.NewComposer()
	base := map[string]string{"FOO": "from-base"}
	spec := agent.Spec{Env: map[string]string{
		"FOO": "", // explicit unset-via-empty
	}}

	got := c.Compose(base, spec)
	want := []string{"FOO="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected FOO= preserved, got: %v", got)
	}
}

func TestComposeNilReceiverUsesDefault(t *testing.T) {
	t.Parallel()

	var c *env.Composer
	got := c.Compose(map[string]string{
		"ANTHROPIC_API_KEY": "leak",
		"PATH":              "/usr/bin",
	}, agent.Spec{})
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Fatalf("nil receiver should still apply default blocklist, got: %v", got)
	}
}

func TestComposeCustomBlocklist(t *testing.T) {
	t.Parallel()

	c := &env.Composer{Blocklist: []string{"FOO"}}
	got := c.Compose(map[string]string{
		"FOO":               "blocked",
		"ANTHROPIC_API_KEY": "passes-because-not-in-custom",
		"BAR":               "kept",
	}, agent.Spec{})
	want := []string{
		"ANTHROPIC_API_KEY=passes-because-not-in-custom",
		"BAR=kept",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compose mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestComposeEmptyBlocklistDisablesFiltering(t *testing.T) {
	t.Parallel()

	c := &env.Composer{Blocklist: []string{}}
	got := c.Compose(map[string]string{
		"ANTHROPIC_API_KEY": "passthrough",
	}, agent.Spec{})
	want := []string{"ANTHROPIC_API_KEY=passthrough"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty (non-nil) blocklist should disable filtering, got: %v", got)
	}
}

func TestIsBlocked(t *testing.T) {
	t.Parallel()

	c := env.NewComposer()
	if !c.IsBlocked("ANTHROPIC_API_KEY") {
		t.Fatal("ANTHROPIC_API_KEY should be blocked")
	}
	if c.IsBlocked("PATH") {
		t.Fatal("PATH should not be blocked")
	}
	custom := &env.Composer{Blocklist: []string{"FOO"}}
	if !custom.IsBlocked("FOO") {
		t.Fatal("FOO should be blocked under custom list")
	}
	if custom.IsBlocked("ANTHROPIC_API_KEY") {
		t.Fatal("ANTHROPIC_API_KEY should NOT be blocked under custom list")
	}
}

func TestLooksSensitive(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"GITHUB_TOKEN":    true,
		"OAUTH_SECRET":    true,
		"DB_PASSWORD":     true,
		"ROOT_PASSWD":     true,
		"SSH_PRIVATE_KEY": true,
		"SOME_API_KEY":    true,
		"PATH":            false,
		"HOME":            false,
		"LANG":            false,
	}
	for k, want := range cases {
		if got := env.LooksSensitive(k); got != want {
			t.Errorf("LooksSensitive(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestAgentEnvBlocklistMatchesLegacyTS(t *testing.T) {
	t.Parallel()

	// Verbatim port from
	// ../agentfactory/packages/core/src/orchestrator/orchestrator.ts
	// AGENT_ENV_BLOCKLIST. If the legacy list grows, port the new entries
	// AND update this test.
	want := []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"OPENCLAW_GATEWAY_TOKEN",
	}
	if !reflect.DeepEqual(env.AgentEnvBlocklist, want) {
		t.Fatalf("AgentEnvBlocklist drifted from legacy TS port:\n got: %v\nwant: %v",
			env.AgentEnvBlocklist, want)
	}
}
