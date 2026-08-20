package clijsonl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
//	    "<name>": { "type": "http",  "url": "...",     "headersHelper": "..." }
//	  }
//	}
//
// Source: ../donmai-libraries/packages/core/src/providers/claude-provider.ts
// (the `mcpServers` Object.fromEntries block) and the Claude CLI
// `--mcp-config` documentation.
type mcpConfigFile = mcp.ConfigFile

const (
	mcpGatewayFileEnv = "MCP_GATEWAY_TOKEN_FILE"

	// mcpGatewayHeadersHelper is the legacy non-fallback helper (retained for
	// doc/compat; prefer mcpGatewayHeadersHelperWithFallback which never ENOENTs
	// from spawn). Claude runs the helper through a shell and requires a JSON
	// object of string headers on stdout.
	mcpGatewayHeadersHelper = `token=$(cat -- "$MCP_GATEWAY_TOKEN_FILE") || exit 1; printf '{"Authorization":"Bearer %s"}\n' "$token"` //nolint:unused // retained for compat
)

// mcpGatewayHeadersHelperWithFallback returns a helper that tries the live file
// first and falls back to staticBearer when absent/empty. staticBearer is the
// raw token (no "Bearer " prefix), single-quote escaped for sh.
func mcpGatewayHeadersHelperWithFallback(staticBearer string) string {
	esc := strings.ReplaceAll(staticBearer, "'", "'\\''")
	return `token=$(cat -- "$MCP_GATEWAY_TOKEN_FILE" 2>/dev/null); if [ -z "$token" ]; then token='` + esc + `'; fi; printf '{"Authorization":"Bearer %s"}\n' "$token"`
}

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
	return writeMCPConfigWithEnv(servers, nil)
}

// writeMCPConfigWithEnv builds the Claude CLI config with the exact static
// shape writeMCPConfig has always emitted, except when the runner-provided
// gateway token-file variable is non-empty. In that case the leading reserved
// gateway entry uses Claude's headersHelper command so each connection reads
// the current bearer from the atomically replaced file.
func writeMCPConfigWithEnv(servers []agent.MCPServerConfig, env map[string]string) (path string, err error) {
	if len(servers) == 0 {
		return "", nil
	}

	cfg, err := mcp.BuildConfigFile(servers)
	if err != nil {
		return "", fmt.Errorf("provider/claude: build MCP config: %w", err)
	}
	preferLiveGatewayHeader(&cfg, servers, env)

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

// preferLiveGatewayHeader changes only the runner-authored gateway entry. The
// runner guarantees that entry leads the MCP list; the remaining shape checks
// keep a caller-supplied first server from being rewritten accidentally.
func preferLiveGatewayHeader(cfg *mcp.ConfigFile, servers []agent.MCPServerConfig, env map[string]string) {
	if cfg == nil || len(servers) == 0 || strings.TrimSpace(env[mcpGatewayFileEnv]) == "" {
		return
	}

	gateway := servers[0]
	if gateway.Type != "http" || !strings.HasSuffix(gateway.Name, "-platform") {
		return
	}

	authorizationKey := ""
	staticValue := ""
	for key, value := range gateway.Headers {
		if strings.EqualFold(key, "Authorization") && strings.HasPrefix(value, "Bearer ") {
			authorizationKey = key
			staticValue = strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
			break
		}
	}
	if authorizationKey == "" || strings.TrimSpace(staticValue) == "" {
		return
	}

	entry, ok := cfg.MCPServers[gateway.Name]
	if !ok || entry.Type != "http" {
		return
	}
	if len(entry.Headers) == 1 {
		entry.Headers = nil
	} else {
		headers := make(map[string]string, len(entry.Headers)-1)
		for key, value := range entry.Headers {
			if !strings.EqualFold(key, authorizationKey) {
				headers[key] = value
			}
		}
		entry.Headers = headers
	}
	entry.HeadersHelper = mcpGatewayHeadersHelperWithFallback(staticValue)
	cfg.MCPServers[gateway.Name] = entry
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

// WriteMCPConfigWithEnv is the Claude-specific config writer. It enables the
// schema-supported live gateway header when MCP_GATEWAY_TOKEN_FILE is present
// in the exact environment the child receives.
func WriteMCPConfigWithEnv(servers []agent.MCPServerConfig, env map[string]string) (string, error) {
	return writeMCPConfigWithEnv(servers, env)
}

// RemoveMCPConfig is the exported wrapper for removeMCPConfig.
// Callers that received a path from WriteMCPConfig should call this
// when they are done with the session (typically in Handle.Stop).
func RemoveMCPConfig(path string) error {
	return removeMCPConfig(path)
}
