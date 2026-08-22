package linear

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// canonicalProxyOrigin validates raw as a bare, credential-free HTTP(S) origin
// and returns its canonical "scheme://host[:port]" form (no trailing slash).
//
// The proxied client composes its GraphQL endpoint by string-appending a path
// to this value, so anything richer than an origin — userinfo, a path, a query,
// a fragment, or a trailing delimiter — either redirects the request or smuggles
// material into the composed URL. A production incident reached this code as
// `https://agent.rensei.dev;`: an injected origin whose trailing delimiter made
// every proxied read fail late and opaquely. Validation happens in the
// constructor so a malformed origin fails closed BEFORE any HTTP is attempted
// and before an Authorization header is ever built.
//
// Errors deliberately never quote raw. A proxy origin arrives from operator
// configuration and can carry a bearer in its userinfo or query, while this
// error is surfaced to stderr, structured logs, and session records.
func canonicalProxyOrigin(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a platform origin is required", ErrInvalidPlatformURL)
	}
	if strings.ContainsFunc(trimmed, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", fmt.Errorf("%w: must not contain whitespace or control characters", ErrInvalidPlatformURL)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: must be a parseable absolute URL", ErrInvalidPlatformURL)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("%w: must use the http or https scheme", ErrInvalidPlatformURL)
	}
	if parsed.Opaque != "" {
		return "", fmt.Errorf("%w: must use the //host form, not an opaque URL", ErrInvalidPlatformURL)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%w: must not embed credentials in the URL", ErrInvalidPlatformURL)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%w: must not include a path", ErrInvalidPlatformURL)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", fmt.Errorf("%w: must not include a query string", ErrInvalidPlatformURL)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(trimmed, "#") {
		return "", fmt.Errorf("%w: must not include a fragment", ErrInvalidPlatformURL)
	}
	if !validOriginHost(parsed.Host) {
		return "", fmt.Errorf("%w: host must be a bare hostname or IP with an optional numeric port", ErrInvalidPlatformURL)
	}
	// Plaintext HTTP is only ever acceptable against the local machine (test
	// servers, `donmai host` loopback). A remote origin must be TLS-protected:
	// the request carries a platform bearer token.
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("%w: http is only permitted for a loopback host; use https", ErrInvalidPlatformURL)
	}

	canonical := parsed.Scheme + "://" + strings.ToLower(parsed.Host)
	// Anything the caller wrote that is not the canonical origin (optionally
	// with a single trailing slash) is a trailing delimiter or a shape the
	// checks above cannot see. Fail closed rather than silently normalizing.
	if !strings.EqualFold(trimmed, canonical) && !strings.EqualFold(trimmed, canonical+"/") {
		return "", fmt.Errorf("%w: must be a bare origin with no trailing delimiters", ErrInvalidPlatformURL)
	}
	return canonical, nil
}

// validOriginHost reports whether host is a syntactically valid authority of
// the form hostname[:port], ipv4[:port] or [ipv6][:port]. Unlike url.Parse it
// rejects an empty port (`host:`) and any delimiter that leaked into the
// authority (`host;`, `host,`, `host.`).
func validOriginHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "[") {
		end := strings.LastIndexByte(host, ']')
		if end < 0 || !validOriginPort(host[end+1:]) {
			return false
		}
		return net.ParseIP(host[1:end]) != nil
	}
	name := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		name = host[:i]
		if !validOriginPort(host[i:]) {
			return false
		}
	}
	return validOriginHostname(name)
}

// validOriginPort validates the ":port" suffix of an authority. The empty
// string (no port at all) is valid; a bare ":" is not.
func validOriginPort(port string) bool {
	if port == "" {
		return true
	}
	digits, found := strings.CutPrefix(port, ":")
	if !found || digits == "" || len(digits) > 5 {
		return false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(digits)
	return err == nil && n >= 1 && n <= 65535
}

func validOriginHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	if net.ParseIP(name) != nil {
		return true
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
