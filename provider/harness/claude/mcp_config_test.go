package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const testMCPGatewayFileEnv = "MCP_GATEWAY_TOKEN_FILE"

func TestSpawn_Headless_MCPGatewayUsesLiveTokenFile(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	seenPath := filepath.Join(workdir, "mcp-seen.json")
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--mcp-config\" ]; then\n" +
		"    cp \"$2\" " + shQuote(seenPath) + "\n" +
		"    shift 2\n" +
		"    continue\n" +
		"  fi\n" +
		"  shift\n" +
		"done\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-mcp-live"}\n'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	cli := writeFakeCLI(t, "fake-claude-mcp.sh", script)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tokenPath := filepath.Join(workdir, "live-token")
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "use the gateway",
		Cwd:    workdir,
		Env:    map[string]string{testMCPGatewayFileEnv: tokenPath},
		MCPServers: []agent.MCPServerConfig{{
			Name:    "donmai-platform",
			Type:    "http",
			URL:     "https://example.com/api/mcp/session",
			Headers: map[string]string{"Authorization": "Bearer spawn-token"},
		}},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })
	_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 45*time.Second)

	seen, err := os.ReadFile(seenPath)
	if err != nil {
		t.Fatalf("read copied MCP config: %v", err)
	}
	assertLiveGatewayConfig(t, seen, tokenPath)
}

func assertLiveGatewayConfig(t *testing.T, body []byte, tokenPath string) {
	t.Helper()
	for _, want := range []string{`"headersHelper"`, `$MCP_GATEWAY_TOKEN_FILE`, `Authorization`, `Bearer %s`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("MCP config omitted %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"Bearer spawn-token", tokenPath} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("MCP config baked %q instead of using the live helper: %s", forbidden, body)
		}
	}
}
