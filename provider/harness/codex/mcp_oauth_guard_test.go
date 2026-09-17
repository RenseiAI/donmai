package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestProtectedMCPStoredOAuthGuardUsesActualCredentialIdentity(t *testing.T) {
	server := helperBackedServer(t)
	key, err := codexMCPOAuthCredentialKey(server)
	if err != nil {
		t.Fatal(err)
	}
	if key != "protected|9532c6d617e80628" {
		t.Fatalf("key=%q", key)
	}
	home := t.TempDir()
	entry := map[string]any{
		key: map[string]any{
			"server_name": server.Name, "server_url": server.URL,
			"client_id": "fixture-client", "access_token": "stored-mcp-oauth-sentinel", "scopes": []string{},
		},
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, codexMCPOAuthCredentialsFile)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseProtectedMCPStoredOAuth(home, []agent.MCPServerConfig{server}); err == nil || strings.Contains(err.Error(), "stored-mcp-oauth-sentinel") {
		t.Fatalf("guard err=%v", err)
	}
}

func TestCodexMCPOAuthCredentialKeyMatchesPinnedSerdeJSONBytes(t *testing.T) {
	server := helperBackedServer(t)
	tests := []struct{ url, suffix string }{
		{"https://example.test/mcp?redirect=a&tag=<ready>&end=>", "d57666c23b5362ca"},
		{"https://example.test/mcp?quote=%22&slash=%5C", "13bc8f0c55d14dcc"},
		{"https://example.test/mcp?raw=\"\\&x=1", "88e01bb8a4da445e"},
	}
	for _, tc := range tests {
		server.URL = tc.url
		key, err := codexMCPOAuthCredentialKey(server)
		if err != nil || key != server.Name+"|"+tc.suffix {
			t.Fatalf("URL %q key=%q err=%v", tc.url, key, err)
		}
	}
	server.URL = "https://example.test/mcp?line=bad\nvalue"
	if _, err := codexMCPOAuthCredentialKey(server); err == nil {
		t.Fatal("control-bearing URL was accepted")
	}
	for _, rawUnicode := range []string{"https://example.test/mcp?x=\u2028", "https://example.test/mcp?x=\u2029", "https://é.example/mcp"} {
		server.URL = rawUnicode
		if _, err := codexMCPOAuthCredentialKey(server); err == nil {
			t.Fatalf("raw Unicode URL %q was accepted", rawUnicode)
		}
	}
}

func TestHeadlessProtectedMCPPinsFileStoreAndRefusesStoredOAuthBeforeStart(t *testing.T) {
	server := helperBackedServer(t)
	boundary, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	p := &Provider{config: boundary}
	if err := p.prepareHeadlessProtectedMCPAuthorityLocked([]agent.MCPServerConfig{server}); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(config), `mcp_oauth_credentials_store = "file"`) != 1 {
		t.Fatalf("config=%s", config)
	}
	if !strings.HasPrefix(string(config), `mcp_oauth_credentials_store = "file"`+"\n") {
		t.Fatalf("MCP OAuth store is not a top-level prefix: %s", config)
	}
	key, err := codexMCPOAuthCredentialKey(server)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{key: map[string]any{
		"server_name": server.Name, "server_url": server.URL, "client_id": "fixture-client",
		"access_token": "stored-mcp-oauth-sentinel", "scopes": []string{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boundary.home, codexMCPOAuthCredentialsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.prepareHeadlessProtectedMCPAuthorityLocked([]agent.MCPServerConfig{server}); err == nil {
		t.Fatal("stored OAuth accepted")
	}
	if p.client != nil {
		t.Fatal("app-server started before refusal")
	}
}
