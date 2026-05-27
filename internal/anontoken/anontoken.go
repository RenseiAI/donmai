// Package anontoken manages the donmai machine token (~/.donmai/token).
// The token is a `dmk_*` prefixed opaque value used to authenticate the
// machine to the donmai-dashboard via the browser claim flow.
package anontoken

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/internal/statepath"
)

// tokenPath is a package-level variable so tests can override it.
var tokenPath = func() string {
	return statepath.Resolve("token", "/tmp/.donmai/token")
}

// TokenPath returns the canonical token file path (~/.donmai/token).
func TokenPath() string {
	return tokenPath()
}

// ReadToken returns the existing token or "" if no file is present.
// Returns an error only on read failures (not on missing-file).
func ReadToken() (string, error) {
	path := tokenPath()
	data, err := os.ReadFile(path) //nolint:gosec // intentional state file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("anontoken: read %s: %w", path, err)
	}
	return string(data), nil
}

// MintAndStore generates a new dmk_* token, writes it to TokenPath with
// mode 0600, and creates the parent directory with mode 0700 if missing.
func MintAndStore() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("anontoken: generate random bytes: %w", err)
	}
	token := "dmk_" + hex.EncodeToString(b)

	path := tokenPath()
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("anontoken: create state dir %s: %w", dir, err)
	}

	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("anontoken: write token %s: %w", path, err)
	}

	return token, nil
}

// EnsureToken reads the existing token or mints a new one.
// Returns (token, justMinted, error).
func EnsureToken() (string, bool, error) {
	existing, err := ReadToken()
	if err != nil {
		return "", false, err
	}
	if existing != "" {
		return existing, false, nil
	}

	token, err := MintAndStore()
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}

// ClaimURL builds the browser claim URL for a token.
// baseURL should be the dashboard base URL (e.g. "https://donmai.dev/dashboard").
func ClaimURL(token, baseURL string) string {
	if baseURL == "" {
		baseURL = "https://donmai.dev/dashboard"
	}
	return baseURL + "/api/auth/claim?token=" + token
}
