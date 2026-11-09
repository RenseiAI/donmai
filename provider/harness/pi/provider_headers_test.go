package pi

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
)

func TestProviderHeadersPinEnv(t *testing.T) {
	t.Parallel()
	injected := &agent.EndpointBinding{BaseURL: "https://gateway.example/v1", Protocol: agent.ProtoOpenAIChat, Host: agent.HostDirect}
	declared := map[string]string{agent.ProviderHTTPHeadersEnv: "x-title: Example App\nx-cost: $HOME\nx-note: !echo hi"}
	const wantJSON = piHeadersEnvVar + `={"x-cost":"$$HOME","x-note":"$!echo hi","x-title":"Example App"}`
	tests := []struct {
		name string
		spec agent.Spec
		want []string
	}{
		{name: "absent exports nothing", spec: agent.Spec{Model: "m", Endpoint: injected, Env: map[string]string{"OTHER": "v"}}},
		{name: "injected provider", spec: agent.Spec{Model: "m", Endpoint: injected, Env: declared}, want: []string{wantJSON}},
		{
			name: "native route names the built-in provider",
			spec: agent.Spec{Model: "openrouter/some-model", Env: declared},
			want: []string{wantJSON, piHeadersProviderEnvVar + "=openrouter"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := slices.DeleteFunc(providerPinEnv(tc.spec), func(kv string) bool { return !strings.HasPrefix(kv, piHeadersEnvVar) })
			if !slices.Equal(got, tc.want) {
				t.Fatalf("providerPinEnv header entries = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestPiConfigLiteral(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Example App": "Example App",
		"$HOME":       "$$HOME",
		"!cmd":        "$!cmd",
		"a$!b ${X}":   "a$$!b $${X}",
		"$!lead":      "$$!lead",
	} {
		if got := piConfigLiteral(in); got != want {
			t.Errorf("piConfigLiteral(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestSpawnRefusesMalformedProviderHeaders(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "fake-pi-never-runs.sh")
	if err := testexec.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{PiBin: bin, VersionProbe: func(context.Context, string) (string, error) { return PinnedVersion, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	spec := realBinarySpec(t.TempDir(), "x", "https://gateway.example/v1")
	spec.Env = map[string]string{agent.ProviderHTTPHeadersEnv: "cookie: a=b"}
	_, err = p.Spawn(t.Context(), spec)
	var typed *agent.ProviderHTTPHeadersError
	if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &typed) {
		t.Fatalf("Spawn err = %v; want ErrSpawnFailed wrapping *agent.ProviderHTTPHeadersError", err)
	}
}

// TestRealBinary_Endpoint_ProviderHeaders proves, against the real pi binary,
// that declared headers reach the model endpoint on the injected provider
// exactly as written: a "$NAME" value is not read as an env reference and a
// leading "!" is not run as a command.
func TestRealBinary_Endpoint_ProviderHeaders(t *testing.T) {
	realBinaryAvailable(t)
	stub := newRealBinaryStub(t, realBinaryModel)
	var mu sync.Mutex
	var seen []http.Header
	stub.beforeResponse = func(r *http.Request, _ int) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Clone())
	}
	spec := realBinarySpec(t.TempDir(), "reply with a short greeting and nothing else", stub.baseURL())
	spec.Env = map[string]string{agent.ProviderHTTPHeadersEnv: "x-title: Example App\nhttp-referer: https://example.com\nx-literal: $HOME\nx-bang: !echo hi"}
	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	drainToResult(t, h, 30*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the stub recorded no request")
	}
	want := map[string]string{"X-Title": "Example App", "Http-Referer": "https://example.com", "X-Literal": "$HOME", "X-Bang": "!echo hi"}
	for i, hdr := range seen {
		for name, value := range want {
			if got := hdr.Get(name); got != value {
				t.Errorf("request %d: %s = %q; want %q", i, name, got, value)
			}
		}
	}
}
