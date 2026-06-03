package geminicli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
)

// geminiSettingsFile is the shape of .gemini/settings.json that the
// Gemini CLI reads for per-project MCP server configuration.
//
// The CLI resolves settings.json by walking up from cwd; writing it
// into the worktree's .gemini/ directory makes it project-scoped.
//
// Source: McpServerConfigSchema in chunk-GPVT36PL.js:
//
//	mcpServers: record({
//	  url:    string().optional(),        // streamable HTTP endpoint
//	  type:   enum(["sse","http"]).optional(),
//	  trust:  boolean().optional(),       // skip per-call approval
//	  headers: record(string()).optional(),
//	})
type geminiSettingsFile struct {
	MCPServers map[string]geminiMCPServerEntry `json:"mcpServers,omitempty"`
}

type geminiMCPServerEntry struct {
	// For stdio servers: command + args + env.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// For HTTP/SSE servers: url + type.
	URL  string `json:"url,omitempty"`
	Type string `json:"type,omitempty"` // "sse" | "http"

	// Headers are sent on every HTTP request (e.g. Authorization bearer).
	Headers map[string]string `json:"headers,omitempty"`

	// Trust=true skips per-call tool-approval prompts for this server.
	// Required for unattended headless runs (--yolo handles tool approval
	// at the CLI level, but trust:true makes MCP tool invocation non-
	// interactive within the session).
	Trust bool `json:"trust"`
}

// settingsFilePath is the relative path within the worktree where the
// Gemini CLI looks for project-scoped settings.
const settingsFilePath = ".gemini/settings.json"

// writeGeminiSettings writes a .gemini/settings.json file into cwd
// configuring the MCP servers from the spec. Returns the absolute path
// of the written file (for cleanup in Stop), or "" when servers is empty.
//
// For stdio servers: Command + Args + Env are set.
// For http servers: URL + Headers + Type="http" + Trust=true.
//
// The file is written with 0600 permissions (world-unreadable) to
// protect the Authorization bearer token in MCP server headers.
func writeGeminiSettings(cwd string, servers []agent.MCPServerConfig) (path string, err error) {
	if len(servers) == 0 {
		return "", nil
	}

	cfg := geminiSettingsFile{
		MCPServers: make(map[string]geminiMCPServerEntry, len(servers)),
	}

	for _, s := range servers {
		if s.Name == "" {
			return "", fmt.Errorf("provider/geminicli: MCP server with empty Name in spec")
		}
		typ := s.Type
		if typ == "" {
			typ = "stdio"
		}

		switch typ {
		case "stdio":
			if s.Command == "" {
				return "", fmt.Errorf("provider/geminicli: MCP server %q (stdio) has empty Command", s.Name)
			}
			args := append([]string(nil), s.Args...)
			var env map[string]string
			if len(s.Env) > 0 {
				env = make(map[string]string, len(s.Env))
				for k, v := range s.Env {
					env[k] = v
				}
			}
			cfg.MCPServers[s.Name] = geminiMCPServerEntry{
				Command: s.Command,
				Args:    args,
				Env:     env,
				Trust:   true,
			}

		case "http":
			if s.URL == "" {
				return "", fmt.Errorf("provider/geminicli: MCP server %q (http) has empty URL", s.Name)
			}
			var headers map[string]string
			if len(s.Headers) > 0 {
				headers = make(map[string]string, len(s.Headers))
				for k, v := range s.Headers {
					headers[k] = v
				}
			}
			cfg.MCPServers[s.Name] = geminiMCPServerEntry{
				URL:     s.URL,
				Type:    "http",
				Headers: headers,
				Trust:   true,
			}

		default:
			return "", fmt.Errorf("provider/geminicli: MCP server %q has unknown type %q (want \"stdio\" or \"http\")", s.Name, typ)
		}
	}

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("provider/geminicli: marshal settings.json: %w", err)
	}

	dir := filepath.Join(cwd, ".gemini")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("provider/geminicli: create .gemini dir: %w", err)
	}

	dest := filepath.Join(dir, "settings.json")
	//nolint:gosec // G306: 0600 intentional — file contains Authorization bearer
	if err := os.WriteFile(dest, body, 0o600); err != nil {
		return "", fmt.Errorf("provider/geminicli: write settings.json: %w", err)
	}

	abs, err := filepath.Abs(dest)
	if err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("provider/geminicli: resolve settings.json path: %w", err)
	}
	return abs, nil
}

// removeGeminiSettings removes the settings.json file written by
// writeGeminiSettings. Idempotent: missing file returns nil.
// Empty path returns nil.
func removeGeminiSettings(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider/geminicli: remove settings.json: %w", err)
	}
	return nil
}
