package clijsonl

import (
	"encoding/json"
	"errors"
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

	// mcpGatewayFallbackSuffix names the private sibling file, next to the
	// config tmpfile, that holds the gateway entry's static bearer for the
	// helper's fallback. removeMCPConfig deletes it with the config.
	mcpGatewayFallbackSuffix = ".bearer"
)

// mcpGatewayHeadersHelperWithFallback returns a helper that tries the live file
// first and falls back to the static bearer stored in fallbackPath when the
// live file is absent or empty.
//
// The helper text carries paths only, never bearer bytes. Claude runs the
// helper as `sh -c <helper>`, so the text is that shell's argv, which any
// local user can read via ps or /proc/<pid>/cmdline. cat receives only the
// paths, and printf is a shell builtin, so the bearer never reaches an argv.
func mcpGatewayHeadersHelperWithFallback(fallbackPath string) string {
	esc := strings.ReplaceAll(fallbackPath, "'", "'\\''")
	return `token=$(cat -- "$MCP_GATEWAY_TOKEN_FILE" 2>/dev/null); if [ -z "$token" ]; then token=$(cat -- '` + esc + `' 2>/dev/null); fi; printf '{"Authorization":"Bearer %s"}\n' "$token"`
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
	if err := refuseUnrealizedProtectedHeadersHelper(servers); err != nil {
		return "", err
	}

	cfg, err := mcp.BuildConfigFile(servers)
	if err != nil {
		return "", fmt.Errorf("provider/claude: build MCP config: %w", err)
	}

	f, err := os.CreateTemp("", "donmai-claude-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("provider/claude: create MCP tmpfile: %w", err)
	}
	abs, err := filepath.Abs(f.Name())
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("provider/claude: resolve MCP tmpfile path: %w", err)
	}
	if err := writeMCPConfigBody(f, abs, &cfg, servers, env); err != nil {
		_ = f.Close()
		_ = removeMCPConfig(abs)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = removeMCPConfig(abs)
		return "", fmt.Errorf("provider/claude: close MCP tmpfile: %w", err)
	}
	return abs, nil
}

// writeMCPConfigBody applies the live gateway header, stores the gateway's
// static bearer in the private fallback file when the helper needs one, and
// writes the config JSON to f, whose absolute path is path.
func writeMCPConfigBody(f *os.File, path string, cfg *mcp.ConfigFile, servers []agent.MCPServerConfig, env map[string]string) error {
	fallbackPath := path + mcpGatewayFallbackSuffix
	if bearer := preferLiveGatewayHeader(cfg, servers, env, fallbackPath); bearer != "" {
		if err := writeGatewayFallbackBearer(fallbackPath, bearer); err != nil {
			return err
		}
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("provider/claude: marshal MCP config: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("provider/claude: write MCP tmpfile: %w", err)
	}
	return nil
}

// writeGatewayFallbackBearer creates path owner-only and writes bearer to it.
// O_EXCL refuses a file that already exists at the path.
func writeGatewayFallbackBearer(path, bearer string) error {
	fb, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path derives from our own CreateTemp name
	if err != nil {
		return fmt.Errorf("provider/claude: create MCP gateway fallback file: %w", err)
	}
	if _, err := fb.WriteString(bearer); err != nil {
		_ = fb.Close()
		return fmt.Errorf("provider/claude: write MCP gateway fallback file: %w", err)
	}
	if err := fb.Close(); err != nil {
		return fmt.Errorf("provider/claude: close MCP gateway fallback file: %w", err)
	}
	return nil
}

// refuseUnrealizedProtectedHeadersHelper fails closed when a server carries
// process-owned protected header-helper authority. That authority lives in an
// unserialized field, so mcp.BuildConfigFile would silently drop it and write an
// HTTP entry with neither a static Authorization header nor a headersHelper.
// Claude Code builds its OAuth provider for exactly that shape: with no
// Authorization from config or helper it consults the saved MCP OAuth store
// (keyed by server name plus a digest of type, URL and headers), so a token the
// operator once saved for a same-named server would become a second
// Authorization authority. Refusing here keeps the only admitted authority the
// one the runner minted until this writer can realize the protected helper.
func refuseUnrealizedProtectedHeadersHelper(servers []agent.MCPServerConfig) error {
	for _, server := range servers {
		if _, ok := agent.ProtectedRuntimeMCPHeadersHelper(server); ok {
			return fmt.Errorf("provider/claude: MCP server %q carries a protected header helper this config writer cannot realize", server.Name)
		}
	}
	return nil
}

// preferLiveGatewayHeader changes only the runner-authored gateway entry. The
// runner guarantees that entry leads the MCP list; the remaining shape checks
// keep a caller-supplied first server from being rewritten accidentally.
//
// When it installs the helper it returns the static bearer the helper falls
// back to, which the caller must store at fallbackPath. Otherwise it returns "".
func preferLiveGatewayHeader(cfg *mcp.ConfigFile, servers []agent.MCPServerConfig, env map[string]string, fallbackPath string) string {
	if cfg == nil || len(servers) == 0 || strings.TrimSpace(env[mcpGatewayFileEnv]) == "" {
		return ""
	}

	gateway := servers[0]
	if gateway.Type != "http" || !strings.HasSuffix(gateway.Name, "-platform") {
		return ""
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
		return ""
	}

	entry, ok := cfg.MCPServers[gateway.Name]
	if !ok || entry.Type != "http" {
		return ""
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
	entry.HeadersHelper = mcpGatewayHeadersHelperWithFallback(fallbackPath)
	cfg.MCPServers[gateway.Name] = entry
	return staticValue
}

// removeMCPConfig deletes the tmpfile written by writeMCPConfig and its
// gateway fallback file, if any.
// Idempotent: missing files return nil. Empty path returns nil.
func removeMCPConfig(path string) error {
	if path == "" {
		return nil
	}
	var errs []error
	if err := os.Remove(path + mcpGatewayFallbackSuffix); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("provider/claude: remove MCP gateway fallback file: %w", err))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("provider/claude: remove MCP tmpfile: %w", err))
	}
	return errors.Join(errs...)
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
