package clijsonl

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestWriteMCPConfig_NoServers(t *testing.T) {
	t.Parallel()

	path, err := writeMCPConfig(nil)
	if err != nil {
		t.Fatalf("writeMCPConfig(nil): %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty string when no servers", path)
	}
}

func TestWriteMCPConfig_HappyPath(t *testing.T) {
	t.Parallel()

	servers := []agent.MCPServerConfig{
		{
			Name:    "af_linear",
			Command: "node",
			Args:    []string{"dist/stdio.js", "--plugin", "linear"},
			Env: map[string]string{
				"FOO":               "bar",
				"ATTACH_TOKEN":      "must-not-serialize",
				"ATTACH_TOKEN_FILE": "/must/not/serialize",
				"ATTACH_URL":        "wss://must-not-serialize.invalid",
			},
		},
		{
			Name:    "af_code",
			Command: "node",
			Args:    []string{"dist/stdio.js", "--plugin", "code"},
		},
	}

	path, err := writeMCPConfig(servers)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	t.Cleanup(func() { _ = removeMCPConfig(path) })

	if !filepath.IsAbs(path) {
		t.Errorf("path %q should be absolute", path)
	}
	if !strings.HasSuffix(path, ".json") {
		t.Errorf("path %q should end in .json", path)
	}
	if !strings.Contains(path, "donmai-claude-mcp-") {
		t.Errorf("path %q should contain session prefix", path)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmpfile: %v", err)
	}

	var got mcpConfigFile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode tmpfile: %v: %s", err, body)
	}
	if len(got.MCPServers) != 2 {
		t.Errorf("MCPServers count = %d, want 2", len(got.MCPServers))
	}
	linear, ok := got.MCPServers["af_linear"]
	if !ok {
		t.Fatalf("af_linear missing: %v", got.MCPServers)
	}
	if linear.Type != "stdio" {
		t.Errorf("type = %q, want stdio", linear.Type)
	}
	if linear.Command != "node" {
		t.Errorf("command = %q, want node", linear.Command)
	}
	if !strings.Contains(strings.Join(linear.Args, " "), "--plugin linear") {
		t.Errorf("args = %v missing --plugin linear", linear.Args)
	}
	if linear.Env["FOO"] != "bar" {
		t.Errorf("env FOO = %q, want bar", linear.Env["FOO"])
	}
	for _, key := range []string{"ATTACH_TOKEN", "ATTACH_TOKEN_FILE", "ATTACH_URL"} {
		if _, leaked := linear.Env[key]; leaked {
			t.Errorf("serialized runner-only %s: %v", key, linear.Env)
		}
	}
}

func TestWriteMCPConfig_GatewayAuthorization(t *testing.T) {
	t.Parallel()

	servers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://example.com/api/mcp/session",
		Headers: map[string]string{
			"Authorization": "Bearer spawn-token",
		},
	}}

	tests := []struct {
		name     string
		env      map[string]string
		wantBody string
	}{
		{
			name: "token file absent preserves static config bytes",
			wantBody: `{
  "mcpServers": {
    "donmai-platform": {
      "type": "http",
      "url": "https://example.com/api/mcp/session",
      "headers": {
        "Authorization": "Bearer spawn-token"
      }
    }
  }
}`,
		},
		{
			name: "token file present emits helper with fallback to static bearer",
			env:  map[string]string{mcpGatewayFileEnv: "/run/session/mcp-token"},
			wantBody: `{
  "mcpServers": {
    "donmai-platform": {
      "type": "http",
      "url": "https://example.com/api/mcp/session",
      "headersHelper": "token=$(cat -- \"$MCP_GATEWAY_TOKEN_FILE\" 2\u003e/dev/null); if [ -z \"$token\" ]; then token='spawn-token'; fi; printf '{\"Authorization\":\"Bearer %s\"}\\n' \"$token\""
    }
  }
}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path, err := writeMCPConfigWithEnv(servers, tc.env)
			if err != nil {
				t.Fatalf("writeMCPConfigWithEnv: %v", err)
			}
			t.Cleanup(func() { _ = removeMCPConfig(path) })

			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read tmpfile: %v", err)
			}
			if got := string(body); got != tc.wantBody {
				t.Fatalf("config bytes mismatch:\n got: %s\nwant: %s", got, tc.wantBody)
			}
		})
	}
}

func TestWriteMCPConfig_GatewayHeadersHelperFallbackWhenFileAbsent(t *testing.T) {
	t.Parallel()

	// REN-2690 V16 delete-seed-RED: absent/empty file must fall back to baked bearer, not ENOENT.
	path, err := writeMCPConfigWithEnv([]agent.MCPServerConfig{{
		Name:    "donmai-platform",
		Type:    "http",
		URL:     "https://example.com/api/mcp/session",
		Headers: map[string]string{"Authorization": "Bearer spawn-token"},
	}}, map[string]string{mcpGatewayFileEnv: "/run/session/mcp-token"})
	if err != nil {
		t.Fatalf("writeMCPConfigWithEnv: %v", err)
	}
	t.Cleanup(func() { _ = removeMCPConfig(path) })

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmpfile: %v", err)
	}
	var cfg mcpConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("decode tmpfile: %v", err)
	}
	helper := cfg.MCPServers["donmai-platform"].HeadersHelper
	wantHelper := mcpGatewayHeadersHelperWithFallback("spawn-token")
	if helper != wantHelper {
		t.Fatalf("gateway headersHelper = %q, want %q", helper, wantHelper)
	}

	// Absent file must not ENOENT — fallback to baked bearer.
	absentPath := filepath.Join(t.TempDir(), "no-such-token")
	cmd := exec.Command("sh", "-c", helper) // #nosec G204 -- helper is fixture under test, input controlled
	cmd.Env = append(os.Environ(), mcpGatewayFileEnv+"="+absentPath)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper with absent file should fallback, not fail: %v", err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(out, &hdr); err != nil {
		t.Fatalf("decode fallback output %q: %v", out, err)
	}
	if got, want := hdr["Authorization"], "Bearer spawn-token"; got != want {
		t.Fatalf("absent-file fallback Authorization = %q, want %q", got, want)
	}
}

func TestWriteMCPConfig_GatewayHeadersHelperReadsLatestToken(t *testing.T) {
	t.Parallel()

	path, err := writeMCPConfigWithEnv([]agent.MCPServerConfig{{
		Name:    "donmai-platform",
		Type:    "http",
		URL:     "https://example.com/api/mcp/session",
		Headers: map[string]string{"Authorization": "Bearer spawn-token"},
	}}, map[string]string{mcpGatewayFileEnv: "/path/is/not/serialized"})
	if err != nil {
		t.Fatalf("writeMCPConfigWithEnv: %v", err)
	}
	t.Cleanup(func() { _ = removeMCPConfig(path) })

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmpfile: %v", err)
	}
	var cfg mcpConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("decode tmpfile: %v", err)
	}
	helper := cfg.MCPServers["donmai-platform"].HeadersHelper
	wantHelper := mcpGatewayHeadersHelperWithFallback("spawn-token")
	if helper != wantHelper {
		t.Fatalf("gateway headersHelper = %q, want %q", helper, wantHelper)
	}

	tokenPath := filepath.Join(t.TempDir(), "current-token")
	tests := []struct {
		name  string
		token string
	}{
		{name: "initial bearer", token: "first-token"},
		{name: "atomically refreshed bearer", token: "second-token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nextPath := tokenPath + ".next"
			if err := os.WriteFile(nextPath, []byte(tc.token), 0o600); err != nil {
				t.Fatalf("write next token: %v", err)
			}
			if err := os.Rename(nextPath, tokenPath); err != nil {
				t.Fatalf("replace token: %v", err)
			}

			cmd := exec.Command("sh", "-c", helper) // #nosec G204 -- helper is fixture under test, input controlled
			cmd.Env = append(os.Environ(), mcpGatewayFileEnv+"="+tokenPath)
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("run headersHelper: %v", err)
			}
			var headers map[string]string
			if err := json.Unmarshal(output, &headers); err != nil {
				t.Fatalf("decode helper output %q: %v", output, err)
			}
			if got, want := headers["Authorization"], "Bearer "+tc.token; got != want {
				t.Fatalf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

func TestWriteMCPConfig_RejectsEmptyName(t *testing.T) {
	t.Parallel()

	_, err := writeMCPConfig([]agent.MCPServerConfig{{Command: "node"}})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "empty Name") {
		t.Errorf("error message should mention empty name: %v", err)
	}
}

func TestWriteMCPConfig_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()

	_, err := writeMCPConfig([]agent.MCPServerConfig{{Name: "x"}})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if !strings.Contains(err.Error(), "empty Command") {
		t.Errorf("error message should mention empty command: %v", err)
	}
}

func TestRemoveMCPConfig_Idempotent(t *testing.T) {
	t.Parallel()

	if err := removeMCPConfig(""); err != nil {
		t.Errorf("remove of empty path returned error: %v", err)
	}
	if err := removeMCPConfig("/tmp/donmai-claude-mcp-does-not-exist.json"); err != nil {
		t.Errorf("remove of missing file returned error: %v", err)
	}

	servers := []agent.MCPServerConfig{{Name: "x", Command: "node"}}
	path, err := writeMCPConfig(servers)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	if err := removeMCPConfig(path); err != nil {
		t.Errorf("remove first call: %v", err)
	}
	if err := removeMCPConfig(path); err != nil {
		t.Errorf("remove second call (idempotent) returned error: %v", err)
	}
}

func TestWriteMCPConfig_DoesNotAliasInputs(t *testing.T) {
	t.Parallel()

	args := []string{"a", "b"}
	env := map[string]string{"K": "v"}
	servers := []agent.MCPServerConfig{{Name: "x", Command: "c", Args: args, Env: env}}

	path, err := writeMCPConfig(servers)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	t.Cleanup(func() { _ = removeMCPConfig(path) })

	// Mutate caller's slices/maps. The on-disk JSON should already
	// reflect the snapshot at the time of write.
	args[0] = "MUTATED"
	env["K"] = "MUTATED"

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "MUTATED") {
		t.Errorf("on-disk body aliased caller mutations: %s", body)
	}
}
