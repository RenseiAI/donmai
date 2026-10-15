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

// detectOpaqueCredential reports whether an opaque (non-URL) remote — an
// scp-like "host:path" form or another form with no parseable scheme —
// carries password-shaped userinfo ("user:password@..."). URL-scheme
// remotes are not opaque: their userinfo is stripped by
// canonicalization, never persisted. Local filesystem paths (absolute,
// "./" or "../" relative, "~/", "file://", or a Windows drive) are
// not remotes at all and are never credential-bearing. Anything else of
// the "segment@segment:segment" shape is treated as an scp-like remote
// with userinfo and inspected for a password separator.
func detectOpaqueCredential(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") ||
		strings.HasPrefix(trimmed, "../") || strings.HasPrefix(trimmed, "~/") {
		return false
	}
	if len(trimmed) >= 2 && trimmed[1] == ':' && ((trimmed[0] >= 'A' && trimmed[0] <= 'Z') || (trimmed[0] >= 'a' && trimmed[0] <= 'z')) {
		return false
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" {
		// Hierarchical URLs (ssh://, file://, https://, ...) carry
		// userinfo in a parseable field that canonicalization strips,
		// so they are never opaque. A scheme:opaque form
		// ("deploy:tok@host:path" parses with scheme "deploy" and an
		// opaque remainder) hashes verbatim and is opaque: inspect it
		// below. file:///path has no opaque remainder and is a local
		// path, not a remote.
		if parsed.Host != "" || parsed.Opaque == "" {
			return false
		}
	}
	at := strings.LastIndex(trimmed, "@")
	colon := strings.LastIndex(trimmed, ":")
	if at < 0 || colon < 0 || colon < at {
		return false
	}
	userinfo := trimmed[:at]
	// scp-like userinfo is "user" or "user:password" before the host;
	// only the password separator marks it credential-bearing. A bare
	// user ("deploy@host:path") is an identity, not a secret.
	return strings.Contains(userinfo, ":")
}

// TrimURLCredentials strips any embedded userinfo (user:password@) from
// an http(s) git URL and returns the bare form, so auth travels through
// the per-invocation http.extraHeader instead of persisting a token in
// .git/config. It returns (clean, true) when userinfo was present and
// stripped, and (rawURL, false) when there was nothing to strip or the
// input is not a parseable http(s) URL (an ssh/scp-style remote, a local
// path, or a file:// URL) — in which case the caller leaves the URL
// untouched. Non-http(s) URLs are never rewritten here: file:// userinfo
// is part of what the local transport reads, and ssh/scp forms have no
// URL userinfo field to strip.
func TrimURLCredentials(rawURL string) (string, bool) {
	if rawURL == "" {
		return rawURL, false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return rawURL, false
	}
	if u.User == nil {
		return rawURL, false
	}
	u.User = nil
	return u.String(), true
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
	// Opaque (non-URL) credential check first: a scheme:opaque form such
	// as "deploy:tok@host:path" parses with a scheme, so it would
	// otherwise fall into the URL branch below — where there is no
	// parseable userinfo field to strip — and persist verbatim.
	if detectOpaqueCredential(trimmed) {
		return "", "", "", fmt.Errorf("%w", ErrOpaqueCredential)
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
		// An http(s) clone argv never carries userinfo: auth resolved
		// through the GitAuth seam travels in the per-invocation
		// http.extraHeader instead, so a crash between clone and the
		// origin reset leaves no credential in the staging config.
		// file:// and ssh:// userinfo is part of what those
		// transports read, so only http(s) is trimmed here.
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			if trimmed, ok := TrimURLCredentials(cloneURL); ok {
				cloneURL = trimmed
			}
		}
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

// MirrorDigest resolves one (source, scope) pair to its mirror directory
// digest: the canonical remote's digest joined with the scope. It is the
// single choke point the daemon layer uses to name a mirror without
// cloning, so budget, revocation, and eviction all address exactly the
// directory the store would serve. An unparseable or empty source is an
// error, never a digest.
func MirrorDigest(source, scope string) (string, error) {
	_, remoteDigest, err := CanonicalRemote(source)
	if err != nil {
		return "", err
	}
	return ScopeDigest(remoteDigest, scope)
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
