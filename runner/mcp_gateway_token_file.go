package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mcpGatewayTokenFileEnv is the generic live-bearer rail shared by harness
// MCP config helpers and in-session clients. A provisioning daemon may own and
// refresh the named file. When no provisioner published one, the runner seeds
// a private bootstrap file from the platform-minted session bearer so direct
// runner launches expose the same session authority.
const mcpGatewayTokenFileEnv = "MCP_GATEWAY_TOKEN_FILE" //nolint:gosec // G101: variable name, never credential bytes.

type sessionMCPBearerFile struct {
	Path    string
	Cleanup func()
}

// prepareSessionMCPBearerEnv ensures a platform-minted session bearer is
// reachable from the child without placing bearer bytes directly in its env.
//
// An existing provisioner-published file wins and is left untouched: it may be
// atomically refreshed throughout a long-running session. Otherwise a private
// runner-owned bootstrap directory is created, the exact session bearer is
// written mode 0600, and its path is added to env. The returned cleanup is
// idempotent and removes only that uniquely-created directory.
//
// AuthToken is deliberately ineligible. It is worker/runtime authority, not
// proof of the child session's MCP principal. A platform that minted no
// session bearer keeps the pre-existing behavior and receives no file.
func prepareSessionMCPBearerEnv(
	qw QueuedWork,
	env map[string]string,
	existingTokenFile string,
) (sessionMCPBearerFile, error) {
	cleanup := func() {}
	if strings.TrimSpace(existingTokenFile) != "" {
		if env == nil {
			return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: nil destination")
		}
		path := strings.TrimSpace(existingTokenFile)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: existing path must be absolute and clean")
		}
		info, err := os.Lstat(path) //nolint:gosec // G703: process-owned absolute managed-file path, never session wire input.
		if err != nil {
			return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: validate existing file: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: existing file must be regular, non-symlink, and private")
		}
		env[mcpGatewayTokenFileEnv] = path
		return sessionMCPBearerFile{Path: path, Cleanup: cleanup}, nil
	}
	token := strings.TrimSpace(qw.McpAuthToken)
	if token == "" {
		return sessionMCPBearerFile{Cleanup: cleanup}, nil
	}
	if env == nil {
		return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: nil destination")
	}

	dir, err := os.MkdirTemp("", "donmai-mcp-bearer-")
	if err != nil {
		return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: create private directory: %w", err)
	}
	path := filepath.Join(dir, "mcp-token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: write bootstrap file: %w", err)
	}
	// WriteFile applies the process umask but cannot widen 0600. Chmod keeps the
	// on-disk contract exact if a non-standard filesystem changed the mode.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return sessionMCPBearerFile{}, fmt.Errorf("prepare session MCP bearer env: secure bootstrap file: %w", err)
	}

	env[mcpGatewayTokenFileEnv] = path
	return sessionMCPBearerFile{Path: path, Cleanup: func() { _ = os.RemoveAll(dir) }}, nil
}
