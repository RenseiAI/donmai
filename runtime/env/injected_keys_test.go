package env_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/env"
)

// TestInheritedBlocklistHonoursTheDeclaration is the whole point of the
// declaration: the SAME inherited entry is stripped without it and kept with
// it, and nothing else about either layer changes.
func TestInheritedBlocklistHonoursTheDeclaration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		parent  []string
		want    []string
		wantNot []string
	}{
		{
			name:    "blocklisted inherited name without a declaration is stripped",
			parent:  []string{"PATH=/usr/bin", "GEMINI_API_KEY=shell-leak"},
			want:    []string{"PATH=/usr/bin"},
			wantNot: []string{"GEMINI_API_KEY=shell-leak"},
		},
		{
			name: "declared blocklisted inherited name is kept",
			parent: []string{
				"PATH=/usr/bin",
				"GEMINI_API_KEY=daemon-injected",
				env.InjectedEnvKeysVar + "=GEMINI_API_KEY",
			},
			want: []string{"PATH=/usr/bin", "GEMINI_API_KEY=daemon-injected"},
		},
		{
			name: "declaration admits only the names it lists",
			parent: []string{
				"GEMINI_API_KEY=daemon-injected",
				"OPENAI_API_KEY=shell-leak",
				env.InjectedEnvKeysVar + "=GEMINI_API_KEY",
			},
			want:    []string{"GEMINI_API_KEY=daemon-injected"},
			wantNot: []string{"OPENAI_API_KEY=shell-leak"},
		},
		{
			name: "several declared names are all admitted",
			parent: []string{
				"AMP_API_KEY=a",
				"ANTHROPIC_BASE_URL=b",
				"GOOGLE_API_KEY=c",
				env.InjectedEnvKeysVar + "=AMP_API_KEY,ANTHROPIC_BASE_URL,GOOGLE_API_KEY",
			},
			want: []string{"AMP_API_KEY=a", "ANTHROPIC_BASE_URL=b", "GOOGLE_API_KEY=c"},
		},
		{
			name: "declaring a runner-only attach control changes nothing",
			parent: []string{
				"PATH=/usr/bin",
				"ATTACH_TOKEN=host-token",
				"ATTACH_URL=wss://relay.invalid/v1/rooms/room-1",
				env.InjectedEnvKeysVar + "=ATTACH_TOKEN,ATTACH_URL",
			},
			want:    []string{"PATH=/usr/bin"},
			wantNot: []string{"ATTACH_TOKEN=host-token", "ATTACH_URL=wss://relay.invalid/v1/rooms/room-1"},
		},
		{
			name: "declaring the session-shim launch contract changes nothing",
			parent: []string{
				"PATH=/usr/bin",
				"DONMAI_SESSION_SHIM_REGISTRY_DIR=/host/registry",
				env.InjectedEnvKeysVar + "=DONMAI_SESSION_SHIM_REGISTRY_DIR",
			},
			want:    []string{"PATH=/usr/bin"},
			wantNot: []string{"DONMAI_SESSION_SHIM_REGISTRY_DIR=/host/registry"},
		},
		{
			name: "declaring the declaration variable does not smuggle it through",
			parent: []string{
				"PATH=/usr/bin",
				env.InjectedEnvKeysVar + "=" + env.InjectedEnvKeysVar,
			},
			want:    []string{"PATH=/usr/bin"},
			wantNot: []string{env.InjectedEnvKeysVar + "=" + env.InjectedEnvKeysVar},
		},
		{
			name: "malformed declaration is tolerated",
			parent: []string{
				"GEMINI_API_KEY=daemon-injected",
				"OPENAI_API_KEY=daemon-injected",
				env.InjectedEnvKeysVar + "=  GEMINI_API_KEY , ,OPENAI_API_KEY,,",
			},
			want: []string{"GEMINI_API_KEY=daemon-injected", "OPENAI_API_KEY=daemon-injected"},
		},
		{
			name: "empty declaration admits nothing",
			parent: []string{
				"PATH=/usr/bin",
				"GEMINI_API_KEY=shell-leak",
				env.InjectedEnvKeysVar + "=   ",
			},
			want:    []string{"PATH=/usr/bin"},
			wantNot: []string{"GEMINI_API_KEY=shell-leak"},
		},
		{
			name: "a name is matched exactly, never by prefix or glob",
			parent: []string{
				"GEMINI_API_KEY=shell-leak",
				"OPENAI_API_KEY=shell-leak",
				env.InjectedEnvKeysVar + "=*,GEMINI_,OPENAI_API_KEY_SUFFIX",
			},
			wantNot: []string{"GEMINI_API_KEY=shell-leak", "OPENAI_API_KEY=shell-leak"},
		},
		{
			// The row that catches a future strings.EqualFold "fix". Env var
			// names are case-sensitive on every platform donmai runs on, so a
			// folded match would admit a name the supervisor never wrote.
			name: "a name is matched case-sensitively",
			parent: []string{
				"GEMINI_API_KEY=shell-leak",
				env.InjectedEnvKeysVar + "=gemini_api_key",
			},
			wantNot: []string{"GEMINI_API_KEY=shell-leak"},
		},
		{
			name: "an isolation invariant is never declarable",
			parent: []string{
				"PATH=/usr/bin",
				env.GatewayUpstreamAPIKeyEnv + "=upstream-secret",
				env.GatewayUpstreamBaseURLEnv + "=https://upstream.invalid/v1",
				env.InjectedEnvKeysVar + "=" + env.GatewayUpstreamAPIKeyEnv + "," + env.GatewayUpstreamBaseURLEnv,
			},
			want: []string{"PATH=/usr/bin"},
			wantNot: []string{
				env.GatewayUpstreamAPIKeyEnv + "=upstream-secret",
				env.GatewayUpstreamBaseURLEnv + "=https://upstream.invalid/v1",
			},
		},
		{
			// The gateway's fallback path dials with OPENAI_API_KEY, which is
			// an ordinarily-declarable name. Only the per-session refusal
			// closes it.
			name: "a gateway-named upstream credential is refused for this session",
			parent: []string{
				"PATH=/usr/bin",
				"OPENAI_API_KEY=real-upstream-secret",
				env.InjectedEnvKeysVar + "=OPENAI_API_KEY",
				env.GatewayUpstreamEnvKeysVar + "=OPENAI_API_KEY," + env.GatewayUpstreamBaseURLEnv,
			},
			want:    []string{"PATH=/usr/bin"},
			wantNot: []string{"OPENAI_API_KEY=real-upstream-secret"},
		},
		{
			// ...and the same name stays declarable when no gateway named it,
			// which is the case #529 exists to serve.
			name: "the same name is admitted when no gateway named it",
			parent: []string{
				"OPENAI_API_KEY=daemon-injected",
				env.InjectedEnvKeysVar + "=OPENAI_API_KEY",
			},
			want: []string{"OPENAI_API_KEY=daemon-injected"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := env.ComposeChildEnv(tc.parent)
			for _, want := range tc.want {
				if !contains(got, want) {
					t.Errorf("ComposeChildEnv() = %v, missing %q", got, want)
				}
			}
			for _, unwanted := range tc.wantNot {
				if contains(got, unwanted) {
					t.Errorf("ComposeChildEnv() = %v, leaked %q", got, unwanted)
				}
			}
			assertNoDeclaration(t, "ComposeChildEnv", got)
		})
	}
}

// TestDeclarationNeverReachesAChild pins the variable's runner-only status at
// every layer a caller can reach it from.
func TestDeclarationNeverReachesAChild(t *testing.T) {
	t.Parallel()

	if !env.IsRunnerOnly(env.InjectedEnvKeysVar) {
		t.Fatalf("env.IsRunnerOnly(%q) = false, want true", env.InjectedEnvKeysVar)
	}

	assertNoDeclaration(t, "ComposeChildEnv inherited", env.ComposeChildEnv(
		[]string{"PATH=/usr/bin", env.InjectedEnvKeysVar + "=GEMINI_API_KEY"},
	))
	assertNoDeclaration(t, "ComposeChildEnv explicit", env.ComposeChildEnv(
		[]string{"PATH=/usr/bin"},
		map[string]string{env.InjectedEnvKeysVar: "GEMINI_API_KEY"},
	))
	assertNoDeclaration(t, "FilterRunnerOnly", env.FilterRunnerOnly(
		[]string{"PATH=/usr/bin", env.InjectedEnvKeysVar + "=GEMINI_API_KEY"},
	))
	if kept := env.FilterRunnerOnlyMap(map[string]string{
		"PATH":                 "/usr/bin",
		env.InjectedEnvKeysVar: "GEMINI_API_KEY",
	}); len(kept) != 1 {
		t.Errorf("FilterRunnerOnlyMap kept the declaration: %v", kept)
	}
	assertNoDeclaration(t, "Compose base", env.NewComposer().Compose(
		map[string]string{"PATH": "/usr/bin", env.InjectedEnvKeysVar: "GEMINI_API_KEY"},
		agent.Spec{},
	))
	assertNoDeclaration(t, "Compose spec", env.NewComposer().Compose(
		map[string]string{"PATH": "/usr/bin"},
		agent.Spec{Env: map[string]string{env.InjectedEnvKeysVar: "GEMINI_API_KEY"}},
	))
}

// TestComposeBaseHonoursTheDeclaration covers the other inherited layer: the
// runner composes agent.Spec.Env from the worker's own environment, so a
// credential stripped there never reaches any harness, declared or not.
func TestComposeBaseHonoursTheDeclaration(t *testing.T) {
	t.Parallel()

	c := env.NewComposer()

	got := c.Compose(map[string]string{
		"PATH":           "/usr/bin",
		"GEMINI_API_KEY": "shell-leak",
	}, agent.Spec{})
	if want := []string{"PATH=/usr/bin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Compose() without a declaration:\n got: %v\nwant: %v", got, want)
	}

	got = c.Compose(map[string]string{
		"PATH":                 "/usr/bin",
		"GEMINI_API_KEY":       "daemon-injected",
		env.InjectedEnvKeysVar: "GEMINI_API_KEY",
	}, agent.Spec{})
	if want := []string{"GEMINI_API_KEY=daemon-injected", "PATH=/usr/bin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Compose() with a declaration:\n got: %v\nwant: %v", got, want)
	}

	// spec.Env still wins over a declared inherited value — the declaration
	// re-admits a name, it does not promote it above the explicit layer.
	got = c.Compose(map[string]string{
		"GEMINI_API_KEY":       "daemon-injected",
		env.InjectedEnvKeysVar: "GEMINI_API_KEY",
	}, agent.Spec{Env: map[string]string{"GEMINI_API_KEY": "session-resolved"}})
	if want := []string{"GEMINI_API_KEY=session-resolved"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Compose() precedence:\n got: %v\nwant: %v", got, want)
	}
}

// TestExplicitLayerUnchangedByTheDeclaration pins that the trusted layers were
// not touched: they bypassed the blocklist before and still do, with or
// without a declaration.
func TestExplicitLayerUnchangedByTheDeclaration(t *testing.T) {
	t.Parallel()

	for _, parent := range [][]string{
		{"PATH=/usr/bin"},
		{"PATH=/usr/bin", env.InjectedEnvKeysVar + "=OPENAI_API_KEY"},
	} {
		got := env.ComposeChildEnv(parent, map[string]string{
			"ANTHROPIC_API_KEY": "session-resolved",
			"SAFE":              "explicit",
		})
		want := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=session-resolved", "SAFE=explicit"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ComposeChildEnv(parent=%v):\n got: %v\nwant: %v", parent, got, want)
		}
	}
}

func TestParseInjectedEnvKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty", value: ""},
		{name: "whitespace only", value: " \t "},
		{name: "commas only", value: ",,,"},
		{name: "single", value: "GEMINI_API_KEY", want: []string{"GEMINI_API_KEY"}},
		{
			name:  "padded and trailing comma",
			value: " GEMINI_API_KEY , OPENAI_API_KEY ,",
			want:  []string{"GEMINI_API_KEY", "OPENAI_API_KEY"},
		},
		{
			name:  "duplicates collapse",
			value: "GEMINI_API_KEY,GEMINI_API_KEY",
			want:  []string{"GEMINI_API_KEY"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			set := env.ParseInjectedEnvKeys(tc.value, "")
			for _, name := range tc.want {
				if !set.Allows(name) {
					t.Errorf("ParseInjectedEnvKeys(%q).Allows(%q) = false, want true", tc.value, name)
				}
			}
			if set.Allows("SOMETHING_ELSE") {
				t.Errorf("ParseInjectedEnvKeys(%q) admitted an undeclared name", tc.value)
			}
		})
	}

	if env.InjectedEnvKeysFrom(nil).Allows("GEMINI_API_KEY") {
		t.Error("InjectedEnvKeysFrom(nil) admitted a name")
	}
	if env.InjectedEnvKeysFromMap(nil).Allows("GEMINI_API_KEY") {
		t.Error("InjectedEnvKeysFromMap(nil) admitted a name")
	}
	// exec.Cmd.Env resolves a duplicate to the LAST entry; the declaration
	// must be read the same way or the filter disagrees with the child's view.
	last := env.InjectedEnvKeysFrom([]string{
		env.InjectedEnvKeysVar + "=GEMINI_API_KEY",
		env.InjectedEnvKeysVar + "=OPENAI_API_KEY",
	})
	if last.Allows("GEMINI_API_KEY") || !last.Allows("OPENAI_API_KEY") {
		t.Errorf("InjectedEnvKeysFrom did not take the last duplicate: %v", last)
	}
	// The zero value answers every question with "not declared".
	var none env.InjectedEnvKeySet
	if none.Allows("GEMINI_API_KEY") {
		t.Error("the zero InjectedEnvKeySet admitted a name")
	}
	// The gateway refusal list is read from the same slice, last-wins too.
	gw := env.InjectedEnvKeysFrom([]string{
		env.InjectedEnvKeysVar + "=OPENAI_API_KEY",
		env.GatewayUpstreamEnvKeysVar + "=SOMETHING_ELSE",
		env.GatewayUpstreamEnvKeysVar + "=OPENAI_API_KEY",
	})
	if gw.Allows("OPENAI_API_KEY") {
		t.Error("InjectedEnvKeysFrom did not apply the last gateway refusal list")
	}
}

// TestIsolationInvariantsAreASubsetOfTheBlocklist keeps the two lists honest:
// a never-declarable name that is not blocked in the first place would be a
// refusal with nothing to refuse, and a blocked name silently dropped from the
// invariants would become declarable.
func TestIsolationInvariantsAreASubsetOfTheBlocklist(t *testing.T) {
	t.Parallel()

	blocked := make(map[string]struct{}, len(env.AgentEnvBlocklist))
	for _, key := range env.AgentEnvBlocklist {
		blocked[key] = struct{}{}
	}
	for _, key := range env.AgentEnvIsolationInvariants {
		if _, ok := blocked[key]; !ok {
			t.Errorf("%q is an isolation invariant but not in AgentEnvBlocklist", key)
		}
	}
	// Written out literally rather than iterated from the source list, so
	// deleting an entry fails here instead of passing vacuously.
	want := []string{"DONMAI_GATEWAY_UPSTREAM_API_KEY", "DONMAI_GATEWAY_UPSTREAM_BASE_URL"}
	if !reflect.DeepEqual(env.AgentEnvIsolationInvariants, want) {
		t.Errorf("AgentEnvIsolationInvariants = %v, want %v", env.AgentEnvIsolationInvariants, want)
	}
	declared := env.ParseInjectedEnvKeys(strings.Join(want, ","), "")
	for _, key := range want {
		if declared.Allows(key) {
			t.Errorf("a declaration re-admitted the isolation invariant %q", key)
		}
	}
}

// TestDeclareGatewayUpstreamEnvKeys covers the producer side: the gateway
// publishes the names into its own process and every filter reads them back.
func TestDeclareGatewayUpstreamEnvKeys(t *testing.T) {
	// Not parallel: it mutates the process environment.
	t.Setenv(env.GatewayUpstreamEnvKeysVar, "")
	t.Setenv(env.InjectedEnvKeysVar, "OPENAI_API_KEY")
	t.Setenv("OPENAI_API_KEY", "real-upstream-secret")

	if !composedChildEnvContains("OPENAI_API_KEY=real-upstream-secret") {
		t.Fatal("precondition: a declared name should be admitted before the gateway names it")
	}
	if err := env.DeclareGatewayUpstreamEnvKeys(" OPENAI_API_KEY ", "", env.GatewayUpstreamBaseURLEnv); err != nil {
		t.Fatalf("DeclareGatewayUpstreamEnvKeys: %v", err)
	}
	if got := os.Getenv(env.GatewayUpstreamEnvKeysVar); got != "OPENAI_API_KEY,"+env.GatewayUpstreamBaseURLEnv {
		t.Errorf("%s = %q, want the trimmed non-empty names", env.GatewayUpstreamEnvKeysVar, got)
	}
	if composedChildEnvContains("OPENAI_API_KEY=real-upstream-secret") {
		t.Error("the gateway upstream credential reached the child after being named")
	}
	// The marker itself is runner-only and must not reach the child either.
	if !env.IsRunnerOnly(env.GatewayUpstreamEnvKeysVar) {
		t.Errorf("env.IsRunnerOnly(%q) = false, want true", env.GatewayUpstreamEnvKeysVar)
	}
	assertNoDeclaration(t, "ComposeChildEnv after DeclareGatewayUpstreamEnvKeys", env.ComposeChildEnv(os.Environ()))
}

func contains(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}

// composedChildEnvContains composes a child environment from the CURRENT
// process environment and reports whether want survived it. It is how the
// gateway producer test observes the filter the way a real spawn would.
func composedChildEnvContains(want string) bool {
	return contains(env.ComposeChildEnv(os.Environ()), want)
}

func assertNoDeclaration(t *testing.T, where string, entries []string) {
	t.Helper()
	for _, entry := range entries {
		for _, runnerOnly := range []string{env.InjectedEnvKeysVar, env.GatewayUpstreamEnvKeysVar} {
			if strings.HasPrefix(entry, runnerOnly+"=") || entry == runnerOnly {
				t.Errorf("%s leaked the runner-only control %q into the child environment", where, entry)
			}
		}
	}
}
