package claude

import (
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestWithProviderHTTPHeaders(t *testing.T) {
	t.Parallel()
	declared := "http-referer: https://example.com\nx-title: Example App"
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "absent leaves env untouched", env: map[string]string{EnvCustomHeaders: "x-own: 1\n", "OTHER": "v"}, want: "x-own: 1\n"},
		{name: "declared only", env: map[string]string{agent.ProviderHTTPHeadersEnv: declared}, want: declared},
		{
			name: "existing lines kept, missing names appended case-insensitively",
			env:  map[string]string{agent.ProviderHTTPHeadersEnv: declared, EnvCustomHeaders: "X-Title: Own Title\nx-proxy-tag: a\n"},
			want: "X-Title: Own Title\nx-proxy-tag: a\nhttp-referer: https://example.com",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := maps.Clone(tc.env)
			first, err := withProviderHTTPHeaders(tc.env)
			if err != nil || first[EnvCustomHeaders] != tc.want {
				t.Fatalf("%s = %q, %v; want %q", EnvCustomHeaders, first[EnvCustomHeaders], err, tc.want)
			}
			if second, err := withProviderHTTPHeaders(first); err != nil || !reflect.DeepEqual(second, first) {
				t.Fatalf("second merge = %v, %v; want the first result unchanged (idempotent)", second, err)
			}
			if !reflect.DeepEqual(tc.env, before) {
				t.Fatal("input env was mutated")
			}
		})
	}
}

// TestProvider_SpawnAndResume_CustomHeadersReachSubprocess proves the merged
// ANTHROPIC_CUSTOM_HEADERS value reaches the CLI child on a fresh spawn and a
// --resume, and that a malformed declaration refuses the session.
func TestProvider_SpawnAndResume_CustomHeadersReachSubprocess(t *testing.T) {
	t.Parallel()
	cli := writeFakeCLI(t, "fake-claude-headers.sh",
		"#!/bin/sh\n"+
			`H=$(printf '%s' "$ANTHROPIC_CUSTOM_HEADERS" | tr '\n' ';')`+"\n"+
			`printf '{"type":"system","subtype":"init","session_id":"sess-hdr-1"}\n'`+"\n"+
			`printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}\n' "$H"`+"\n"+
			`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'`+"\n")
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := agent.Spec{Prompt: "x", Env: map[string]string{EnvCustomHeaders: "x-own: 1", agent.ProviderHTTPHeadersEnv: "x-title: Example App"}}
	for name, launch := range map[string]func() (agent.Handle, error){
		"spawn":  func() (agent.Handle, error) { return p.Spawn(t.Context(), spec) },
		"resume": func() (agent.Handle, error) { return p.Resume(t.Context(), "sess-hdr-1", spec) },
	} {
		h, err := launch()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var text string
		deadline := time.After(30 * time.Second)
		for text == "" {
			select {
			case ev, open := <-h.Events():
				if !open {
					t.Fatalf("%s: events closed before assistant text", name)
				}
				if e, ok := ev.(agent.AssistantTextEvent); ok {
					text = e.Text
				}
			case <-deadline:
				t.Fatalf("%s: timed out waiting for assistant text", name)
			}
		}
		_ = h.Stop(t.Context())
		if want := "x-own: 1;x-title: Example App"; text != want {
			t.Errorf("%s: child %s = %q; want %q", name, EnvCustomHeaders, text, want)
		}
	}

	spec.Env = map[string]string{agent.ProviderHTTPHeadersEnv: "x-title: a\rinjected: b"}
	_, err = p.Spawn(t.Context(), spec)
	var typed *agent.ProviderHTTPHeadersError
	if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &typed) {
		t.Fatalf("Spawn with CR-injected declaration: err = %v; want ErrSpawnFailed wrapping *agent.ProviderHTTPHeadersError", err)
	}
}
