package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

const codexMCPOAuthCredentialsFile = ".credentials.json"

func protectedMCPHelperServers(servers []agent.MCPServerConfig) []agent.MCPServerConfig {
	var selected []agent.MCPServerConfig
	for _, server := range servers {
		if _, ok := agent.ProtectedRuntimeMCPHeadersHelper(server); ok {
			selected = append(selected, server)
		}
	}
	return selected
}

// pinProtectedMCPFileStore confines native MCP OAuth lookup to the isolated
// CODEX_HOME so an OS keyring cannot become a second Authorization authority.
func pinProtectedMCPFileStore(configPath string, servers []agent.MCPServerConfig) error {
	if len(protectedMCPHelperServers(servers)) == 0 {
		return nil
	}
	baseline, err := os.ReadFile(configPath) //nolint:gosec // provider-owned isolated config path
	if err != nil {
		return fmt.Errorf("codex: pin protected MCP OAuth store: %w", err)
	}
	const setting = "mcp_oauth_credentials_store = \"file\"\n"
	if bytes.Contains(baseline, []byte("mcp_oauth_credentials_store")) {
		if bytes.HasPrefix(baseline, []byte(setting)) {
			return nil
		}
		return errors.New("codex: isolated MCP OAuth store already has conflicting configuration")
	}
	// Prepend so the key remains top-level even if the private baseline later
	// grows TOML tables below its current inline mcp_servers declaration.
	if err := os.WriteFile(configPath, append([]byte(setting), baseline...), codexConfigMode); err != nil { //nolint:gosec // G703: provider-owned absolute isolated config path, never wire input.
		return fmt.Errorf("codex: pin protected MCP OAuth store: %w", err)
	}
	return nil
}

func codexMCPOAuthCredentialKey(server agent.MCPServerConfig) (string, error) {
	if server.Name == "" || server.Type != "http" || server.URL == "" {
		return "", errors.New("codex: protected MCP OAuth identity is incomplete")
	}
	for _, b := range []byte(server.URL) {
		if b < 0x20 || b > 0x7e {
			return "", errors.New("codex: protected MCP OAuth URL must use printable ASCII bytes")
		}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	// Rust serde_json writes '&', '<', and '>' literally. Go's package-level
	// Marshal HTML-escapes them, which would produce a different native store
	// key for the same exact URL.
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}{Type: "http", URL: server.URL, Headers: map[string]string{}})
	if err != nil {
		return "", err
	}
	payload := encoded.Bytes()
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		return "", errors.New("codex: protected MCP OAuth identity encoding is incomplete")
	}
	payload = payload[:len(payload)-1]
	sum := sha256.Sum256(payload)
	return server.Name + "|" + hex.EncodeToString(sum[:])[:16], nil
}

// refuseProtectedMCPStoredOAuth fails before native process activity when the
// exact helper-backed server also has saved MCP OAuth authority.
func refuseProtectedMCPStoredOAuth(codexHome string, servers []agent.MCPServerConfig) error {
	selected := protectedMCPHelperServers(servers)
	if len(selected) == 0 {
		return nil
	}
	path := filepath.Join(codexHome, codexMCPOAuthCredentialsFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("codex: inspect protected MCP OAuth store")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("codex: protected MCP OAuth store must be a private regular file")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // fixed file inside provider-owned CODEX_HOME
	if err != nil {
		return errors.New("codex: inspect protected MCP OAuth store")
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return errors.New("codex: protected MCP OAuth store is malformed")
	}
	for _, server := range selected {
		key, err := codexMCPOAuthCredentialKey(server)
		if err != nil {
			return err
		}
		if _, exists := entries[key]; exists {
			return errors.New("codex: protected MCP header helper conflicts with saved MCP OAuth authority for " + strings.TrimSpace(server.Name))
		}
	}
	return nil
}
