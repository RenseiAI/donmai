package ptyhost

import "testing"

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
