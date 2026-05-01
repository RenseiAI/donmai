package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// ConfigFile is the JSON shape Claude CLI's --mcp-config flag consumes
// and the Codex app-server config/batchWrite mcpServers value the codex
// provider sends over JSON-RPC.
//
// It mirrors the legacy TS SDK's McpStdioServerConfig record-of-records
// (Object.fromEntries(specs.map(s => [s.name, {type:'stdio', ...}]))).
type ConfigFile struct {
	MCPServers map[string]StdioServer `json:"mcpServers"`
}

// StdioServer is one stdio-transport MCP server entry inside ConfigFile.
//
// Type is always "stdio" today; declared so the shape matches the legacy
// TS SDK exactly and so the JSON unmarshaler can validate it.
type StdioServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// BuildConfigFile transforms the agent.MCPServerConfig slice into the
// on-disk ConfigFile shape. Pure: no I/O.
//
// Returns an error when any entry has an empty Name or Command. Args
// and Env are defensively copied to prevent later mutation through the
// JSON encoder.
func BuildConfigFile(servers []agent.MCPServerConfig) (ConfigFile, error) {
	cfg := ConfigFile{MCPServers: make(map[string]StdioServer, len(servers))}
	for i, s := range servers {
		if s.Name == "" {
			return ConfigFile{}, fmt.Errorf("runtime/mcp: server[%d] has empty Name", i)
		}
		if s.Command == "" {
			return ConfigFile{}, fmt.Errorf("runtime/mcp: server %q has empty Command", s.Name)
		}
		args := append([]string(nil), s.Args...)
		var env map[string]string
		if len(s.Env) > 0 {
			env = make(map[string]string, len(s.Env))
			for k, v := range s.Env {
				env[k] = v
			}
		}
		cfg.MCPServers[s.Name] = StdioServer{
			Type:    "stdio",
			Command: s.Command,
			Args:    args,
			Env:     env,
		}
	}
	return cfg, nil
}

// Builder writes per-session MCP config tmpfiles.
//
// The zero value is valid: TempDir defaults to os.TempDir() and Prefix
// defaults to "agentfactory-mcp-".
type Builder struct {
	// TempDir is the directory tmpfiles are created in. Empty falls
	// back to os.TempDir(). Tests inject a t.TempDir() value.
	TempDir string
	// Prefix is the os.CreateTemp prefix; the suffix is always ".json".
	Prefix string
}

// NewBuilder returns a Builder with default TempDir and Prefix.
func NewBuilder() *Builder {
	return &Builder{}
}

// Build serializes servers to a JSON tmpfile under b.TempDir() and
// returns its absolute path plus a cleanup closure. The cleanup is
// idempotent and safe to call from defer.
//
// Returns ("", noop, nil) when servers is empty so callers can still
// defer the closure unconditionally — providers that omit --mcp-config
// when len(servers)==0 do not need to special-case this.
//
// The tmpfile is created with mode 0600 (CreateTemp default) so the
// agent process — which the runner spawns under the same UID — is the
// only reader of the file's contents.
func (b *Builder) Build(servers []agent.MCPServerConfig) (path string, cleanup func(), err error) {
	noop := func() {}
	if len(servers) == 0 {
		return "", noop, nil
	}

	cfg, err := BuildConfigFile(servers)
	if err != nil {
		return "", noop, err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", noop, fmt.Errorf("runtime/mcp: marshal config: %w", err)
	}

	tempDir := b.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	prefix := b.Prefix
	if prefix == "" {
		prefix = "agentfactory-mcp-"
	}

	f, err := os.CreateTemp(tempDir, prefix+"*.json")
	if err != nil {
		return "", noop, fmt.Errorf("runtime/mcp: create tmpfile: %w", err)
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
		return "", noop, fmt.Errorf("runtime/mcp: write tmpfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", noop, fmt.Errorf("runtime/mcp: close tmpfile: %w", err)
	}
	closed = true

	abs, err := filepath.Abs(f.Name())
	if err != nil {
		_ = os.Remove(f.Name())
		return "", noop, fmt.Errorf("runtime/mcp: resolve tmpfile path: %w", err)
	}
	cleanup = func() {
		_ = os.Remove(abs)
	}
	return abs, cleanup, nil
}

// LoadConfigFile reads and parses an existing config tmpfile. Test
// helper for roundtrip assertions; production code uses Build.
func LoadConfigFile(path string) (ConfigFile, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: caller-supplied path is the tmpfile Build returned
	if err != nil {
		return ConfigFile{}, fmt.Errorf("runtime/mcp: read %q: %w", path, err)
	}
	var cfg ConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		return ConfigFile{}, fmt.Errorf("runtime/mcp: parse %q: %w", path, err)
	}
	return cfg, nil
}
