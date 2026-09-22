package env_test

import (
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

			set := env.ParseInjectedEnvKeys(tc.value)
			if len(set) != len(tc.want) {
				t.Fatalf("ParseInjectedEnvKeys(%q) has %d names, want %d", tc.value, len(set), len(tc.want))
			}
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

	if got := env.InjectedEnvKeysFrom(nil); got != nil {
		t.Errorf("InjectedEnvKeysFrom(nil) = %v, want nil", got)
	}
	if got := env.InjectedEnvKeysFromMap(nil); got != nil {
		t.Errorf("InjectedEnvKeysFromMap(nil) = %v, want nil", got)
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
	// A nil set answers every question with "not declared".
	var none env.InjectedEnvKeySet
	if none.Allows("GEMINI_API_KEY") {
		t.Error("a nil InjectedEnvKeySet admitted a name")
	}
}

func contains(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}

func assertNoDeclaration(t *testing.T, where string, entries []string) {
	t.Helper()
	for _, entry := range entries {
		if strings.HasPrefix(entry, env.InjectedEnvKeysVar+"=") || entry == env.InjectedEnvKeysVar {
			t.Errorf("%s leaked the declaration %q into the child environment", where, entry)
		}
	}
}
