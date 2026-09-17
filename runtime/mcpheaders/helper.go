// Package mcpheaders implements the local credential helper used by native
// MCP clients. It deliberately has no configuration, network, or credential
// store dependencies.
package mcpheaders

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

const MaxBearerFileBytes = 64 * 1024

// ResolveExecutablePath returns the canonical physical path of this process.
func ResolveExecutablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve MCP header helper executable: %w", err)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("resolve MCP header helper executable: path is not absolute")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve MCP header helper executable symlinks: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("resolve MCP header helper executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("resolve MCP header helper executable: path is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("resolve MCP header helper executable: path is not executable")
	}
	return filepath.Clean(path), nil
}

// BuildHelperCommand returns the exact command native MCP clients execute.
func BuildHelperCommand(executablePath, tokenFilePath string) (string, error) {
	return buildHelperCommandForOS(runtime.GOOS, executablePath, tokenFilePath)
}

func buildHelperCommandForOS(goos, executablePath, tokenFilePath string) (string, error) {
	for label, candidate := range map[string]string{"executable": executablePath, "token file": tokenFilePath} {
		if !validPathForOS(goos, candidate) {
			return "", fmt.Errorf("build MCP header helper command: %s path must be absolute, clean, and valid UTF-8", label)
		}
	}
	quote := quoteUnix
	if goos == "windows" {
		quote = quoteWindows
	}
	return strings.Join([]string{
		quote(executablePath), "mcp", "gateway-headers", "--token-file", quote(tokenFilePath),
	}, " "), nil
}

func validPathForOS(goos, value string) bool {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return false
	}
	if goos == "windows" {
		return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
			value[1] == ':' && (value[2] == '\\' || value[2] == '/') &&
			!strings.Contains(strings.ReplaceAll(value, "\\", "/"), "/../") &&
			!strings.Contains(strings.ReplaceAll(value, "\\", "/"), "/./")
	}
	return strings.HasPrefix(value, "/") && path.Clean(value) == value
}

func quoteUnix(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// quoteWindows follows the CommandLineToArgvW escaping convention.
func quoteWindows(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	slashes := 0
	for _, r := range value {
		switch r {
		case '\\':
			slashes++
		case '"':
			out.WriteString(strings.Repeat("\\", slashes*2+1))
			out.WriteRune(r)
			slashes = 0
		default:
			out.WriteString(strings.Repeat("\\", slashes))
			slashes = 0
			out.WriteRune(r)
		}
	}
	out.WriteString(strings.Repeat("\\", slashes*2))
	out.WriteByte('"')
	return out.String()
}

// ReadAuthorizationJSON rereads one private bearer file and emits the native
// helper protocol object. The credential is never cached.
func ReadAuthorizationJSON(tokenFilePath string) ([]byte, error) {
	if !filepath.IsAbs(tokenFilePath) || filepath.Clean(tokenFilePath) != tokenFilePath {
		return nil, errors.New("read MCP bearer file: path must be absolute and clean")
	}
	info, err := os.Lstat(tokenFilePath)
	if err != nil {
		return nil, fmt.Errorf("read MCP bearer file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("read MCP bearer file: path must be a regular non-symlink file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("read MCP bearer file: permissions must be private")
	}
	file, err := os.Open(tokenFilePath)
	if err != nil {
		return nil, fmt.Errorf("read MCP bearer file: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, MaxBearerFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("read MCP bearer file: %w", errors.Join(readErr, closeErr))
	}
	if len(raw) > MaxBearerFileBytes {
		return nil, errors.New("read MCP bearer file: content exceeds limit")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("read MCP bearer file: content is empty")
	}
	for _, b := range []byte(token) {
		if b < 0x21 || b > 0x7e {
			return nil, errors.New("read MCP bearer file: content contains unsupported bytes")
		}
	}
	return json.Marshal(map[string]string{"Authorization": "Bearer " + token})
}
