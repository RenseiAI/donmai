package ptyhost

import (
	"fmt"
	"testing"

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

func TestComposeEnv_ManyOverridesGrowWithoutCapacitySum(t *testing.T) {
	t.Parallel()
	overrides := make([]string, 4096)
	for i := range overrides {
		overrides[i] = fmt.Sprintf("KEY_%04d=value", i)
	}
	got := envMap(composeEnv([]string{"PATH=/bin"}, overrides))
	if len(got) != 4099 || got["PATH"] != "/bin" || got["KEY_4095"] != "value" {
		t.Fatalf("large composed environment boundaries: len=%d PATH=%q last=%q", len(got), got["PATH"], got["KEY_4095"])
	}
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
