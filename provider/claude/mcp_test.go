package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
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
			Env:     map[string]string{"FOO": "bar"},
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
	if !strings.Contains(path, "agentfactory-claude-mcp-") {
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
	if err := removeMCPConfig("/tmp/agentfactory-claude-mcp-does-not-exist.json"); err != nil {
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
