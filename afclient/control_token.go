// Package afclient control_token.go — per-install bearer credential for the
// local daemon's HTTP control API.
//
// The daemon stores a random token at the control-token path with mode 0600.
// The operator's CLI reads the same file and attaches it as
// `Authorization: Bearer <token>` on mutating (non-GET) requests. Read-only
// GET routes stay open so status dashboards, spawned workers fetching their
// own session detail, and liveness probes keep working without a credential.
//
// What the token does and does not protect: the gate refuses unauthenticated
// mutating calls (a session or tool calling the control API without the
// token, another local user who cannot read the file). The token does not
// travel in a spawned session's environment: the env names below are
// runner-only and are stripped from every worker environment, including the
// daemon's own inherited environment, and the daemon states only the control
// URL in daemon-owned spawn env. It does NOT stop a same-user, unsandboxed
// session that reads the token file directly — file permissions cannot
// separate processes running as the same user. Hiding the state dir from
// sessions is the job of the execution sandbox, and is follow-up work.
//
// This file intentionally takes explicit paths and never reads the process
// environment: path resolution (state dir, env overrides) belongs to the
// CLI layer, not this library.
package afclient

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ControlTokenEnv names the process-environment variable that may carry the
// control token to the CLI. It is runner-only: it must never reach a spawned
// worker's environment.
const ControlTokenEnv = "DONMAI_CONTROL_TOKEN"

// ControlTokenFileEnv names the variable that overrides the token file path.
// It is runner-only for the same reason.
const ControlTokenFileEnv = "DONMAI_CONTROL_TOKEN_FILE"

// ControlTokenFileName is the token file leaf under the brand state dir.
const ControlTokenFileName = "control-token"

// LoadControlToken reads and trims the token at path. Returns ("", nil) when
// the file does not exist so callers can distinguish "unsecured daemon"
// from a read failure.
func LoadControlToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // intentional operator state file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read control token %q: %w", path, err)
	}
	return string(bytes.TrimSpace(data)), nil
}

// EnsureControlToken returns the token at path, minting and storing a fresh
// random one (mode 0600, parent dir 0700) when absent. Empty path returns
// ("", nil) — the caller stays in legacy open mode.
func EnsureControlToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if existing, err := LoadControlToken(path); err != nil || existing != "" {
		return existing, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create control token dir: %w", err)
	}
	// Mode 0600 keeps the token from other local users. It does not keep it
	// from a session running as this same user without a sandbox: such a
	// session can read this file. Isolating the state dir from sessions is
	// the execution sandbox's job, not this file mode's.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("install control token: %w", err)
	}
	return token, nil
}
