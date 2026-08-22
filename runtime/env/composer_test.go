package env_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/env"
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
		"AMP_API_KEY":                      "leak-0",
		"ANTHROPIC_API_KEY":                "leak-1",
		"ANTHROPIC_AUTH_TOKEN":             "leak-2",
		"ANTHROPIC_BASE_URL":               "leak-3",
		"OPENCLAW_GATEWAY_TOKEN":           "leak-4",
		"DONMAI_GATEWAY_UPSTREAM_API_KEY":  "leak-5",
		"DONMAI_GATEWAY_UPSTREAM_BASE_URL": "http://127.0.0.1:9999/v1",
		"PATH":                             "/usr/bin",
	}

	got := c.Compose(base, agent.Spec{})
	for _, kv := range got {
		switch kv {
		case "AMP_API_KEY=leak-0",
			"ANTHROPIC_API_KEY=leak-1",
			"ANTHROPIC_AUTH_TOKEN=leak-2",
			"ANTHROPIC_BASE_URL=leak-3",
			"OPENCLAW_GATEWAY_TOKEN=leak-4",
			"DONMAI_GATEWAY_UPSTREAM_API_KEY=leak-5",
			"DONMAI_GATEWAY_UPSTREAM_BASE_URL=http://127.0.0.1:9999/v1":
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

func TestComposeAlwaysBlocksRunnerOnlyControls(t *testing.T) {
	t.Parallel()

	c := &env.Composer{Blocklist: []string{}}
	got := c.Compose(
		map[string]string{
			"ATTACH_TOKEN":      "host-token",
			"ATTACH_TOKEN_FILE": "/host/token",
			"PATH":              "/usr/bin",
		},
		agent.Spec{Env: map[string]string{
			"ATTACH_TOKEN": "spec-token",
			"ATTACH_URL":   "wss://relay.invalid/v1/rooms/room-1",
			"SAFE":         "kept",
		}},
	)
	want := []string{"PATH=/usr/bin", "SAFE=kept"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runner-only controls reached composed provider env:\n got: %v\nwant: %v", got, want)
	}
}

func TestFilterRunnerOnly(t *testing.T) {
	t.Parallel()

	got := env.FilterRunnerOnly([]string{
		"PATH=/usr/bin",
		"ATTACH_TOKEN=secret",
		"ATTACH_TOKEN_FILE=/tmp/token",
		"ATTACH_URL=wss://relay.invalid/v1/rooms/room-1",
		"ATTACH_TOKEN",
		"SAFE=a=b",
	})
	want := []string{"PATH=/usr/bin", "SAFE=a=b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterRunnerOnly():\n got: %v\nwant: %v", got, want)
	}
}

func TestComposeChildEnvFiltersEveryLayer(t *testing.T) {
	t.Parallel()

	got := env.ComposeChildEnv(
		[]string{
			"PATH=/usr/bin",
			"SAFE=parent",
			"ATTACH_TOKEN=parent-secret",
			"DONMAI_GATEWAY_UPSTREAM_API_KEY=worker-secret",
			"DONMAI_GATEWAY_UPSTREAM_BASE_URL=https://worker.invalid/private",
			"OPENAI_API_KEY=parent-provider-secret",
		},
		map[string]string{
			"BASE_ONLY":         "base",
			"SAFE":              "base",
			"ATTACH_TOKEN_FILE": "/base/token",
			"OPENAI_API_KEY":    "session-provider-secret",
		},
		map[string]string{
			"SAFE":       "command",
			"ATTACH_URL": "wss://command.invalid",
		},
	)
	want := []string{
		"PATH=/usr/bin",
		"SAFE=parent",
		"BASE_ONLY=base",
		"OPENAI_API_KEY=session-provider-secret",
		"SAFE=command",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposeChildEnv():\n got: %v\nwant: %v", got, want)
	}
}

func TestComposeChildEnvManyExplicitEntriesGrowWithoutCapacitySum(t *testing.T) {
	t.Parallel()
	explicit := make(map[string]string, 4096)
	for i := 0; i < 4096; i++ {
		explicit[fmt.Sprintf("KEY_%04d", i)] = "value"
	}
	got := env.ComposeChildEnv([]string{"PATH=/usr/bin"}, explicit)
	if len(got) != 4097 || got[0] != "PATH=/usr/bin" || got[len(got)-1] != "KEY_4095=value" {
		t.Fatalf("large child environment boundaries: len=%d first=%q last=%q", len(got), got[0], got[len(got)-1])
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
	// ../donmai-libraries/packages/core/src/orchestrator/orchestrator.ts
	// AGENT_ENV_BLOCKLIST, plus the donmai-native entries that have no legacy
	// counterpart (DONMAI_GATEWAY_UPSTREAM_API_KEY and
	// DONMAI_GATEWAY_UPSTREAM_BASE_URL — the worker-local gateway's upstream
	// credential and route, which must never reach a harness child — and
	// AMP_API_KEY, the amp harness's model-provider credential; see the
	// AgentEnvBlocklist doc comment). If the legacy list grows, port the new
	// entries AND update this test.
	want := []string{
		"AMP_API_KEY",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"DONMAI_GATEWAY_UPSTREAM_API_KEY",
		"DONMAI_GATEWAY_UPSTREAM_BASE_URL",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"OPENCLAW_GATEWAY_TOKEN",
		"OPENAI_API_KEY",
	}
	if !reflect.DeepEqual(env.AgentEnvBlocklist, want) {
		t.Fatalf("AgentEnvBlocklist drifted from legacy TS port:\n got: %v\nwant: %v",
			env.AgentEnvBlocklist, want)
	}
}

// TestRunnerOnlyRefusesTheSessionShimLaunchContract pins the boundary for
// ADR-2026-08-17 §D1's launch contract at every input layer.
//
// The values carry no secret — a lifecycle identity, a registry directory,
// four durations — but they ADDRESS the supervisor of the process that would
// receive them. A harness child is a session's workload; handing it the path to
// its own shim's registry gives a workload a handle on the boundary that owns
// it, and an explicit Spec.Env entry must not be able to buy that back.
func TestRunnerOnlyRefusesTheSessionShimLaunchContract(t *testing.T) {
	t.Parallel()

	shimKeys := []string{
		"DONMAI_SESSION_SHIM",
		"DONMAI_SESSION_SHIM_ORG_ID",
		"DONMAI_SESSION_SHIM_SESSION_ID",
		"DONMAI_SESSION_SHIM_REGISTRY_DIR",
		"DONMAI_SESSION_SHIM_PROCESS_EPOCH",
		"DONMAI_SESSION_SHIM_ORPHAN_DEADLINE_MS",
		"DONMAI_SESSION_SHIM_TERMINATION_GRACE_MS",
		"DONMAI_SESSION_SHIM_PROPAGATION_MARGIN_MS",
		"DONMAI_SESSION_SHIM_EXTERNAL_RELEASE_MS",
		// Matched by prefix, so a key added to the contract later is refused the
		// moment it exists rather than the moment someone remembers to list it.
		"DONMAI_SESSION_SHIM_SOMETHING_ADDED_LATER",
	}
	for _, key := range shimKeys {
		if !env.IsRunnerOnly(key) {
			t.Errorf("env.IsRunnerOnly(%q) = false, want true", key)
		}
	}
	// Neighbouring daemon/session keys are ORDINARY worker environment and must
	// keep reaching the child — over-broad matching here would silently break
	// every harness that reads its own session id.
	for _, key := range []string{"DONMAI_SESSION_ID", "DONMAI_WORKER_ID", "DONMAI_PROJECT_ID"} {
		if env.IsRunnerOnly(key) {
			t.Errorf("env.IsRunnerOnly(%q) = true; this is ordinary worker environment", key)
		}
	}

	// The boundary holds through every layer, including an explicit override.
	c := env.NewComposer()
	got := c.Compose(
		map[string]string{"DONMAI_SESSION_SHIM_REGISTRY_DIR": "/host/registry"},
		agent.Spec{Env: map[string]string{"DONMAI_SESSION_SHIM": "1"}},
	)
	for _, entry := range got {
		if strings.HasPrefix(entry, "DONMAI_SESSION_SHIM") {
			t.Errorf("Compose leaked %q into the child environment", entry)
		}
	}
	if kept := env.FilterRunnerOnly([]string{"DONMAI_SESSION_SHIM_ORG_ID=o", "PATH=/bin"}); len(kept) != 1 || kept[0] != "PATH=/bin" {
		t.Errorf("FilterRunnerOnly = %v, want only PATH", kept)
	}
}
