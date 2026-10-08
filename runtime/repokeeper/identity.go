package repokeeper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// CanonicalRemote normalizes a clone source into the stable identity the
// keeper keys mirrors on: URL userinfo, query, and fragment are removed
// before hashing, so a credential embedded in the URL can neither change
// the mirror directory nor reach the catalog. Non-URL sources (scp-style
// remotes, local paths) hash verbatim. An empty source is an error.
func CanonicalRemote(source string) (canonical string, digest string, err error) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return "", "", fmt.Errorf("repo keeper: repository source is empty")
	}
	canonical = trimmed
	if parsed, parseErr := url.Parse(trimmed); parseErr == nil && parsed.Scheme != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		canonical = parsed.String()
	}
	sum := sha256.Sum256([]byte(canonical))
	return canonical, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ScopeDigest binds one opaque credential-scope name to a short,
// filesystem-safe identity. The empty scope is the host-wide default; any
// other scope keeps its own mirror directory so no repository content
// crosses a credential boundary. An empty digest input is rejected so a
// caller cannot address the bare repository namespace by accident.
func ScopeDigest(remoteDigest, scope string) (string, error) {
	name := "default"
	if scope != "" {
		name = scope
	}
	sum := sha256.Sum256([]byte("repo-keeper/v1/" + remoteDigest + "\x00" + name))
	return hex.EncodeToString(sum[:]), nil
}

// validDigest reports whether value carries the sha256:<64-hex> shape
// produced by CanonicalRemote and ScopeDigest.
func validDigest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == sha256.Size
}
