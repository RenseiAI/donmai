package clijsonl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// mcpConfigFile is the JSON shape Claude CLI's `--mcp-config` flag
// consumes. It mirrors the SDK's McpStdioServerConfig record and extends
// it with the Streamable HTTP transport shape used by the platform's
// per-session MCP endpoint:
//
//	{
//	  "mcpServers": {
//	    "<name>": { "type": "stdio", "command": "...", "args": [...], "env": {...} }
//	    "<name>": { "type": "http",  "url": "...",     "headers": {...} }
//	  }
//	}
//
// Source: ../donmai-libraries/packages/core/src/providers/claude-provider.ts
// (the `mcpServers` Object.fromEntries block) and the Claude CLI
// `--mcp-config` documentation.
type mcpConfigFile = mcp.ConfigFile

// writeMCPConfig serializes Spec.MCPServers to a JSON tmpfile and
// returns its absolute path. Returns "" with nil error when the spec
// has no MCP servers (the caller omits `--mcp-config` in that case).
//
// Per coordinator decision #10 in F.1.1 §10, the file is per-session
// — written under os.TempDir() with a session-stable prefix and
// deleted by the Handle's Stop method (see handle.go cleanup).
//
// Supports both stdio (Command/Args/Env) and http (URL/Headers)
// transports; the entry's Type field discriminates. Empty Type defaults
// to "stdio" for back-compat with the legacy shape.
func writeMCPConfig(servers []agent.MCPServerConfig) (path string, err error) {
	if len(servers) == 0 {
		return "", nil
	}

	cfg, err := mcp.BuildConfigFile(servers)
	if err != nil {
		return "", fmt.Errorf("provider/claude: build MCP config: %w", err)
	}

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("provider/claude: marshal MCP config: %w", err)
	}

	f, err := os.CreateTemp("", "donmai-claude-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("provider/claude: create MCP tmpfile: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("provider/claude: write MCP tmpfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("provider/claude: close MCP tmpfile: %w", err)
	}
	closed = true

	abs, err := filepath.Abs(f.Name())
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("provider/claude: resolve MCP tmpfile path: %w", err)
	}
	return abs, nil
}

// removeMCPConfig deletes the tmpfile written by writeMCPConfig.
// Idempotent: missing file returns nil. Empty path returns nil.
func removeMCPConfig(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider/claude: remove MCP tmpfile: %w", err)
	}
	return nil
}

// WriteMCPConfig is the exported wrapper for writeMCPConfig, allowing
// other providers (amp, opencode) that share the same --mcp-config flag
// format to reuse the tmpfile serialization. Returns the absolute path
// of the written file, or "" with nil error when servers is empty.
func WriteMCPConfig(servers []agent.MCPServerConfig) (string, error) {
	return writeMCPConfig(servers)
}

// RemoveMCPConfig is the exported wrapper for removeMCPConfig.
// Callers that received a path from WriteMCPConfig should call this
// when they are done with the session (typically in Handle.Stop).
func RemoveMCPConfig(path string) error {
	return removeMCPConfig(path)
}
