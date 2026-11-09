package codex

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestGatewayProviderBlock_HTTPHeaders pins the provider block byte for byte:
// no declaration renders today's block exactly, and a declaration adds one
// http_headers inline table, in declaration order.
func TestGatewayProviderBlock_HTTPHeaders(t *testing.T) {
	t.Parallel()
	const today = "model_provider = \"donmai-gateway\"\n[model_providers.donmai-gateway]\nname = \"donmai-gateway\"\n" +
		"base_url = \"https://gateway.example/codex/v1\"\nenv_key = \"AI_GATEWAY_API_KEY\"\nwire_api = \"responses\"\n"
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"absent": {env: map[string]string{"OTHER": "v"}, want: today},
		"declared": {
			env:  map[string]string{agent.ProviderHTTPHeadersEnv: "x-title: Example App\nhttp-referer: https://example.com"},
			want: today + `http_headers = { "x-title" = "Example App", "http-referer" = "https://example.com" }` + "\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			boundary, err := newCodexConfigBoundary(t.TempDir(), false)
			if err != nil {
				t.Fatalf("new boundary: %v", err)
			}
			t.Cleanup(func() { _ = boundary.remove() })
			if _, err := boundary.appendGatewayProviderBlock("https://gateway.example/codex/v1", "k", tc.env); err != nil {
				t.Fatalf("append block: %v", err)
			}
			body, err := os.ReadFile(boundary.configPath)
			if got := strings.TrimPrefix(string(body), codexConfigBaseline); err != nil || got != tc.want {
				t.Fatalf("provider block (%v) =\n%s\nwant\n%s", err, got, tc.want)
			}
		})
	}
}

// TestGatewayHeadlessSpawn_HTTPHeaders drives production Spawn: declared
// headers land in the private config, and gatewayBinding refuses a malformed
// declaration for routed and unrouted sessions alike.
func TestGatewayHeadlessSpawn_HTTPHeaders(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		configTempDir: t.TempDir(), HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-headers")

	spec := gatewayCellSpec("https://gateway.example/codex/v1", "cell-key", "")
	spec.Cwd = t.TempDir()
	spec.Env[agent.ProviderHTTPHeadersEnv] = "x-title: Example App"
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	body, err := os.ReadFile(p.config.configPath)
	if want := `http_headers = { "x-title" = "Example App" }`; err != nil || !strings.Contains(string(body), want) {
		t.Fatalf("private config (%v) missing %q:\n%s", err, want, body)
	}

	for _, s := range []agent.Spec{spec, {}} {
		s.Env = map[string]string{agent.ProviderHTTPHeadersEnv: "x-api-key: override"}
		var typed *agent.ProviderHTTPHeadersError
		if _, _, _, err := gatewayBinding(s); !errors.As(err, &typed) {
			t.Fatalf("gatewayBinding err = %v; want *agent.ProviderHTTPHeadersError", err)
		}
	}
}
