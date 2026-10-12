package repokeeper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// CanonicalRemote normalizes a clone source into the stable identity the
// keeper keys mirrors on: URL query and fragment are removed before
// hashing. Userinfo is handled per scheme: ssh keeps its login user (there
// is no header mechanism to carry it, and a later fetch must connect as
// the recorded user), while every other URL scheme refuses userinfo
// outright — a credential in the URL would reach git's argv (visible via
// ps) and the persisted origin. Non-URL sources (scp-style remotes, local
// paths) hash verbatim, login user included. An empty source, an
// unsupported transport scheme, a credential-bearing non-ssh URL, or a
// URL-shaped source that does not parse, is an error that never echoes the
// source.
func CanonicalRemote(source string) (canonical string, digest string, err error) {
	canonical, digest, err = resolveSource(source)
	return canonical, digest, err
}

// resolveSource derives the canonical remote the mirror records from one
// parse. The clone fetches exactly the canonical remote: git treats a
// file:// (or ssh://) query or fragment as part of the path, so the
// identity drops both and the clone must too — otherwise the mirror
// recorded for one remote would hold another repository's content. A
// URL-shaped source that does not parse fails closed, because its verbatim
// form may still carry userinfo that would otherwise reach the sidecar,
// the persisted origin, and the directory key. Neither value is echoed in
// an error.
//
// Only the file, https, and ssh transports are supported: anything else
// (git://, ext::, remote helpers) is refused before git starts, and the
// keeper's git environment would block it again at transport time.
// Distinct ssh login users address distinct mirrors, so home-relative
// paths under different users never share one.
func resolveSource(source string) (canonical, digest string, err error) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return "", "", fmt.Errorf("repo keeper: repository source is empty")
	}
	canonical = trimmed
	parsed, parseErr := url.Parse(trimmed)
	switch {
	case parseErr == nil && parsed.Scheme != "":
		scheme := strings.ToLower(parsed.Scheme)
		switch scheme {
		case "https", "http", "ssh", "file":
		default:
			return "", "", fmt.Errorf("repo keeper: repository source uses an unsupported transport")
		}
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		parsed.RawFragment = ""
		if parsed.User != nil && scheme != "ssh" {
			// No credential may reach git's argv or the persisted
			// origin. Per-invocation HTTP auth travels via gitexec.Auth;
			// anything else userinfo-shaped is refused, never stripped
				// and cloned.
			return "", "", fmt.Errorf("repo keeper: repository source carries userinfo")
		}
		canonical = parsed.String()
	case strings.Contains(trimmed, "://"):
		return "", "", fmt.Errorf("repo keeper: repository source is not a well-formed URL")
	}
	sum := sha256.Sum256([]byte(canonical))
	return canonical, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ScopeDigest binds one opaque credential-scope name to a short,
// filesystem-safe identity. The empty scope is the host-wide default: it
// hashes under the reserved name "default", so an explicit scope literally
// named "default" is refused — accepting it would merge that scope's
// mirror with the host default's. Any other scope keeps its own mirror
// directory so no repository content crosses a credential boundary. An
// empty remote digest is rejected so a caller cannot address the bare
// repository namespace by accident.
func ScopeDigest(remoteDigest, scope string) (string, error) {
	if remoteDigest == "" {
		return "", fmt.Errorf("repo keeper: remote digest is empty")
	}
	if scope == "default" {
		return "", fmt.Errorf("repo keeper: scope name is reserved")
	}
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
