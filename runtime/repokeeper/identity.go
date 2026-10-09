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
// remotes, local paths) hash verbatim. An empty source, or a URL-shaped
// source that does not parse, is an error.
func CanonicalRemote(source string) (canonical string, digest string, err error) {
	canonical, _, digest, err = resolveSource(source)
	return canonical, digest, err
}

// resolveSource derives, from one parse, both the canonical remote the
// mirror records and the exact URL the clone fetches. The clone URL is the
// canonical remote plus only its userinfo, so what the clone reads is what
// the identity names: git treats a file:// (or ssh://) query or fragment as
// part of the path, so cloning the raw source could fetch another
// repository than the one the canonical remote records. A URL-shaped
// source that does not parse fails closed, because its verbatim form may
// still carry userinfo that would otherwise reach the sidecar, the
// persisted origin, and the directory key. Neither value is echoed in an
// error.
func resolveSource(source string) (canonical, cloneURL, digest string, err error) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return "", "", "", fmt.Errorf("repo keeper: repository source is empty")
	}
	canonical, cloneURL = trimmed, trimmed
	parsed, parseErr := url.Parse(trimmed)
	switch {
	case parseErr == nil && parsed.Scheme != "":
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		parsed.RawFragment = ""
		cloneURL = parsed.String()
		parsed.User = nil
		canonical = parsed.String()
	case strings.Contains(trimmed, "://"):
		return "", "", "", fmt.Errorf("repo keeper: repository source is not a well-formed URL")
	}
	sum := sha256.Sum256([]byte(canonical))
	return canonical, cloneURL, "sha256:" + hex.EncodeToString(sum[:]), nil
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
