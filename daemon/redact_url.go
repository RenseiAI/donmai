// Package daemon redact_url.go — userinfo redaction for operator-configured
// repository URLs served on the local control API.
//
// Operator configuration (daemon.yaml projects, pool members, archived
// workarea manifests) may carry repository URLs with embedded credentials
// (a user:token@ authority). Those URLs are functional for the cloner but
// must never be served back verbatim: the control routes that echo them
// (/api/daemon/stats, /api/daemon/pool/stats, /api/daemon/workareas*,
// the session list) answer unauthenticated local GETs, so the credential
// would be readable by anything on the box. Every such response passes
// its repository URLs through redactRepositoryURL before writing.
package daemon

import (
	"net/url"
	"strings"
)

// redactRepositoryURL drops the userinfo (username, password) from raw when
// it parses as a URL with an authority, returning the cleaned URL. Anything
// that is not such a URL passes through unchanged:
//
//   - owner/name slugs ("github.com/org/repo") have no scheme or authority
//     and no userinfo to drop;
//   - scp-like remotes ("git@github.com:org/repo.git") do not parse as URLs
//     and pass through untouched — the login name there addresses the
//     transport, not a bearer the daemon serves;
//   - unparseable input is returned unchanged rather than refused: this is
//     a display projection, and refusing to serve a misconfigured entry
//     would turn a redact routine into an availability fault.
//
// Only the userinfo is removed. Scheme, host, port, and path survive, so the
// redacted value still identifies the repository it names.
func redactRepositoryURL(raw string) string {
	if raw == "" || !strings.Contains(raw, "@") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// redactRepositoryURLs maps redactRepositoryURL over a list, preserving nil
// (a nil allowlist stays nil on the wire rather than becoming []).
func redactRepositoryURLs(urls []string) []string {
	if urls == nil {
		return nil
	}
	out := make([]string, len(urls))
	for i, raw := range urls {
		out[i] = redactRepositoryURL(raw)
	}
	return out
}
