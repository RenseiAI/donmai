package ptyhost

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

func TestComposeEnv_InteractiveDefaultsAndExplicitOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		parent    []string
		overrides []string
		want      map[string]string
	}{
		{
			name:   "absent parent uses defaults",
			parent: []string{"PATH=/bin"},
			want:   map[string]string{"PATH": "/bin", "TERM": "xterm-256color", "COLORTERM": "truecolor"},
		},
		{
			name:   "parent terminal values do not weaken PTY defaults",
			parent: []string{"TERM=dumb", "COLORTERM=", "KEEP=parent"},
			want:   map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "KEEP": "parent"},
		},
		{
			name:      "explicit request overrides defaults and parent",
			parent:    []string{"TERM=dumb", "COLORTERM=", "KEEP=parent"},
			overrides: []string{"TERM=vt100", "COLORTERM=24bit", "KEEP=request"},
			want:      map[string]string{"TERM": "vt100", "COLORTERM": "24bit", "KEEP": "request"},
		},
		{
			name: "runner-only controls removed from parent and request",
			parent: []string{
				"ATTACH_TOKEN=parent-secret",
				"ATTACH_URL=wss://parent.invalid",
				"KEEP=parent",
			},
			overrides: []string{
				"ATTACH_TOKEN=override-secret",
				"ATTACH_TOKEN_FILE=/tmp/token",
				"KEEP=request",
			},
			want: map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "KEEP": "request"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := envMap(composeEnv(tt.parent, tt.overrides))
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("%s = %q, want %q (full env: %v)", key, got[key], want, got)
				}
			}
			for _, blocked := range []string{"ATTACH_TOKEN", "ATTACH_TOKEN_FILE", "ATTACH_URL"} {
				if _, ok := got[blocked]; ok {
					t.Errorf("runner-only control %s reached PTY environment", blocked)
				}
			}
		})
	}
}

func TestComposeEnv_InheritedBlocklistHonoursTheInjectionDeclaration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		parent  []string
		want    map[string]string
		wantNot []string
	}{
		{
			name:    "undeclared blocklisted parent value stays stripped",
			parent:  []string{"PATH=/bin", "GEMINI_API_KEY=shell-leak"},
			want:    map[string]string{"PATH": "/bin"},
			wantNot: []string{"GEMINI_API_KEY"},
		},
		{
			name: "declared blocklisted parent value is inherited",
			parent: []string{
				"PATH=/bin",
				"GEMINI_API_KEY=daemon-injected",
				runtimeenv.InjectedEnvKeysVar + "=GEMINI_API_KEY",
			},
			want: map[string]string{"PATH": "/bin", "GEMINI_API_KEY": "daemon-injected"},
		},
		{
			name: "declaration admits only what it lists",
			parent: []string{
				"GEMINI_API_KEY=daemon-injected",
				"OPENAI_API_KEY=shell-leak",
				runtimeenv.InjectedEnvKeysVar + "=GEMINI_API_KEY",
			},
			want:    map[string]string{"GEMINI_API_KEY": "daemon-injected"},
			wantNot: []string{"OPENAI_API_KEY"},
		},
		{
			name: "declaring a runner-only control changes nothing",
			parent: []string{
				"ATTACH_TOKEN=host-token",
				runtimeenv.InjectedEnvKeysVar + "=ATTACH_TOKEN",
			},
			wantNot: []string{"ATTACH_TOKEN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := envMap(composeEnv(tt.parent, nil))
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("%s = %q, want %q (full env: %v)", key, got[key], want, got)
				}
			}
			for _, key := range tt.wantNot {
				if _, ok := got[key]; ok {
					t.Errorf("%s reached the PTY environment (full env: %v)", key, got)
				}
			}
			if _, ok := got[runtimeenv.InjectedEnvKeysVar]; ok {
				t.Errorf("the injection declaration reached the PTY environment: %v", got)
			}
		})
	}
}

// TestComposeEnv_NoParentYieldsDefaultsAndOverridesOnly pins the exact-env
// composition: with no parent layer the child gets the interactive terminal
// defaults, the UTF-8 locale default, and the request's own entries, nothing
// else, and runner-only controls are still refused.
func TestComposeEnv_NoParentYieldsDefaultsAndOverridesOnly(t *testing.T) {
	t.Parallel()
	got := envMap(composeEnv(nil, []string{"KEEP=request", "ATTACH_TOKEN=request-secret"}))
	want := map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "LANG": "C.UTF-8", "KEEP": "request"}
	if len(got) != len(want) {
		t.Fatalf("exact environment = %v, want exactly %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q (full env: %v)", key, got[key], value, got)
		}
	}
}

// TestSpawn_ExactEnvInheritsNothingFromParent drives a REAL PTY spawn: with
// ExactEnv the child is exec'd with Spec.Env plus the terminal defaults and
// nothing from this process's environment; without it (the control) the
// same parent canary is inherited, so the assertion discriminates.
//
// RED proof: make Spec.parentEnv ignore ExactEnv and the exact-env child
// records the parent canary.
func TestSpawn_ExactEnvInheritsNothingFromParent(t *testing.T) {
	// Not parallel: writes a canary into the process environment.
	const canary = "ptyhost-parent-canary-must-not-reach-an-exact-env-child"
	t.Setenv("PTYHOST_PARENT_CANARY", canary)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	record := func(exact bool) string {
		path := filepath.Join(t.TempDir(), "child-env")
		s := mustSpawn(t, Spec{
			Command:  []string{exe},
			Env:      []string{"PTYHOST_TEST_ROLE=envfile", "PTYHOST_TEST_ENV_FILE=" + path, "KEEP=request"},
			ExactEnv: exact,
		})
		waitDone(t, s, 15*time.Second)
		raw, err := os.ReadFile(path) //nolint:gosec // test-owned path
		if err != nil {
			t.Fatalf("read recorded child env (exact=%v): %v", exact, err)
		}
		return string(raw)
	}

	exact := envMap(strings.Split(strings.TrimSpace(record(true)), "\n"))
	// Report names only: the inheriting failure mode would otherwise print
	// this process's whole environment into the test log.
	if _, ok := exact["PTYHOST_PARENT_CANARY"]; ok {
		t.Errorf("exact-env child inherited the parent canary; child names: %v", envNames(exact))
	}
	if _, ok := exact["PATH"]; ok {
		t.Errorf("exact-env child inherited the parent PATH; child names: %v", envNames(exact))
	}
	for key, want := range map[string]string{"KEEP": "request", "TERM": "xterm-256color", "COLORTERM": "truecolor", "LANG": "C.UTF-8"} {
		if exact[key] != want {
			t.Errorf("exact-env child %s = %q, want %q", key, exact[key], want)
		}
	}

	inherited := envMap(strings.Split(strings.TrimSpace(record(false)), "\n"))
	if inherited["PTYHOST_PARENT_CANARY"] != canary {
		t.Errorf("control: the inheriting child did not see the parent canary; the exact-env assertion would be vacuous")
	}
}

func TestComposeEnv_ManyOverridesGrowWithoutCapacitySum(t *testing.T) {
	t.Parallel()
	overrides := make([]string, 4096)
	for i := range overrides {
		overrides[i] = fmt.Sprintf("KEY_%04d=value", i)
	}
	got := envMap(composeEnv([]string{"PATH=/bin"}, overrides))
	if len(got) != 4100 || got["PATH"] != "/bin" || got["KEY_4095"] != "value" || got["LANG"] != "C.UTF-8" {
		t.Fatalf("large composed environment boundaries: len=%d PATH=%q last=%q LANG=%q", len(got), got["PATH"], got["KEY_4095"], got["LANG"])
	}
}

// TestComposeEnv_UTF8LocaleOnlyWhenNoneIsSet pins the UTF-8 locale rule: the
// child gets LANG=C.UTF-8 when no layer names a locale, and any explicit
// locale wins — from the parent or from the request — including LC_ALL=C,
// which keeps every category's C behaviour. An empty or bare locale entry is
// treated as unset: the C library skips it, so it must not suppress the
// default. The terminal defaults are asserted alongside so the LANG column
// cannot silently merge into their story.
//
// RED proof: drop the LANG default from composeEnv and the default cases fail
// with LANG missing; drop the explicit-locale check and the explicit cases
// fail with LANG=C.UTF-8 overriding the stated locale.
func TestComposeEnv_UTF8LocaleOnlyWhenNoneIsSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		parent    []string
		overrides []string
		want      map[string]string
		wantNot   []string
		wantEmpty []string
	}{
		{
			name:   "no locale anywhere gets the UTF-8 default",
			parent: []string{"PATH=/bin"},
			want:   map[string]string{"LANG": "C.UTF-8", "TERM": "xterm-256color", "COLORTERM": "truecolor"},
		},
		{
			name:   "parent LANG wins",
			parent: []string{"LANG=en_US.UTF-8"},
			want:   map[string]string{"LANG": "en_US.UTF-8"},
		},
		{
			name:    "parent LC_CTYPE wins",
			parent:  []string{"LC_CTYPE=UTF-8"},
			want:    map[string]string{"LC_CTYPE": "UTF-8"},
			wantNot: []string{"LANG", "LC_ALL"},
		},
		{
			name:   "parent LC_ALL=C wins and is not upgraded",
			parent: []string{"LC_ALL=C"},
			want:   map[string]string{"LC_ALL": "C"},
		},
		{
			name:      "request LANG replaces an inherited locale",
			parent:    []string{"LANG=en_US.UTF-8"},
			overrides: []string{"LANG=C.UTF-8"},
			want:      map[string]string{"LANG": "C.UTF-8"},
		},
		{
			name:      "request locale replaces the default",
			parent:    []string{"PATH=/bin"},
			overrides: []string{"LC_ALL=C"},
			want:      map[string]string{"LC_ALL": "C"},
			wantNot:   []string{"LANG"},
		},
		{
			name:      "request LANG=C wins and is kept verbatim",
			parent:    []string{"PATH=/bin"},
			overrides: []string{"LANG=C"},
			want:      map[string]string{"LANG": "C"},
		},
		{
			name:      "exact env with only a request locale keeps it",
			parent:    nil,
			overrides: []string{"LANG=fr_FR.UTF-8"},
			want:      map[string]string{"LANG": "fr_FR.UTF-8"},
		},
		{
			name:   "empty parent locale does not suppress the default",
			parent: []string{"LANG="},
			want:   map[string]string{"LANG": "C.UTF-8"},
		},
		{
			name:      "empty request locale is kept verbatim but still gets the default",
			parent:    []string{"LC_ALL=en_US.UTF-8"},
			overrides: []string{"LC_ALL="},
			want:      map[string]string{"LANG": "C.UTF-8"},
			wantEmpty: []string{"LC_ALL"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := envMap(composeEnv(tt.parent, tt.overrides))
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("%s = %q, want %q (full env: %v)", key, got[key], want, got)
				}
			}
			for _, key := range tt.wantNot {
				if _, ok := got[key]; ok {
					t.Errorf("%s reached the PTY environment (full env: %v)", key, got)
				}
			}
			for _, key := range tt.wantEmpty {
				if v, ok := got[key]; !ok || v != "" {
					t.Errorf("%s = %q, want it present-but-empty (full env: %v)", key, v, got)
				}
			}
			// Every row also proves the terminal defaults still compose and the
			// LANG default appears at most once.
			if got["TERM"] != "xterm-256color" || got["COLORTERM"] != "truecolor" {
				t.Errorf("terminal defaults = TERM %q COLORTERM %q, want the interactive defaults (full env: %v)", got["TERM"], got["COLORTERM"], got)
			}
			if n := localeDefaultCount(composeEnv(tt.parent, tt.overrides)); n > 1 {
				t.Errorf("LANG default appears %d times, want at most one", n)
			}
		})
	}
}

// localeDefaultCount counts LANG entries in a composed environment. exec
// resolves duplicates last-wins, but a doubled default would still signal a
// composition bug, so the pin counts raw entries rather than the map.
func localeDefaultCount(entries []string) int {
	n := 0
	for _, entry := range entries {
		if entry == "LANG=C.UTF-8" {
			n++
		}
	}
	return n
}

// envNames returns env's names, sorted, for failure messages that must not
// print values.
func envNames(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		for i := range entry {
			if entry[i] == '=' {
				out[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	return out
}
