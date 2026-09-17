package codex

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func helperBackedServer(t *testing.T) agent.MCPServerConfig {
	t.Helper()
	server, err := agent.WithProtectedRuntimeMCPHeadersHelper(agent.MCPServerConfig{
		Name: "protected", Type: "http", URL: "https://example.test/mcp",
	}, "'/opt/rensei' mcp gateway-headers --token-file '/tmp/token'")
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestCodexInteractiveProjectsPrivateHTTPHeadersHelper(t *testing.T) {
	override, env, err := codexCLIMCPOverride([]agent.MCPServerConfig{helperBackedServer(t)}, nil, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(override, `"http_headers_helper"=`) || strings.Contains(override, "env_http_headers") || strings.Contains(override, "Authorization") {
		t.Fatalf("override = %s", override)
	}
	for key := range env {
		if strings.HasPrefix(key, "DONMAI_MCP_HEADER_") {
			t.Fatalf("synthetic header env present: %s", key)
		}
	}
}

func TestCodexHeadlessProjectsPrivateHTTPHeadersHelper(t *testing.T) {
	cfg := mcpServersConfig([]agent.MCPServerConfig{helperBackedServer(t)})
	entry, ok := cfg["protected"].(map[string]any)
	if !ok {
		t.Fatalf("entry type = %T", cfg["protected"])
	}
	if entry["http_headers_helper"] == "" || entry["http_headers"] != nil {
		t.Fatalf("entry = %#v", entry)
	}
}
