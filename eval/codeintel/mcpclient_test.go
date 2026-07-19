package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestCallMCPTool_ChildEnvSanitized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake MCP server uses /bin/sh; skip on windows")
	}

	server := filepath.Join(t.TempDir(), "fake-mcp.sh")
	script := "#!/bin/sh\n" +
		"status=leaked\n" +
		"if [ \"${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}\" = \"\" ] && [ \"$SAFE_MCP_ENV\" = \"present\" ]; then status=clean; fi\n" +
		"IFS= read -r initialize\n" +
		"printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}'\n" +
		"IFS= read -r call\n" +
		"printf '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"%s\"}]}}\\n' \"$status\"\n"
	if err := os.WriteFile(server, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake MCP server: %v", err)
	}
	if err := os.Chmod(server, 0o700); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatalf("chmod fake MCP server: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := callMCPTool(ctx, agent.MCPServerConfig{
		Name:    "fake",
		Command: server,
	}, []string{
		"SAFE_MCP_ENV=present",
		"ATTACH_TOKEN=explicit-secret",
		"ATTACH_TOKEN_FILE=/explicit/token",
		"ATTACH_URL=wss://explicit.invalid/v1/rooms/room-1",
	}, "echo", nil)
	if err != nil {
		t.Fatalf("callMCPTool: %v", err)
	}
	if result.IsError || result.Text != "clean" {
		t.Fatalf("MCP child result = %+v, want clean", result)
	}
}
