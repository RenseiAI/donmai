package agent

import (
	"fmt"
	"strings"
)

// ProviderHTTPHeadersEnv names the session env var declaring extra HTTP
// headers a harness sends on every model-provider request (app attribution
// for a model gateway, say): newline-separated "Name: Value" lines, the
// format the Claude Code CLI reads from ANTHROPIC_CUSTOM_HEADERS. Absent or
// blank means none. The parser sits beside the endpoint-binding contract
// because every harness imports this package and materializes the headers
// into its own provider config or env at spec translation.
const ProviderHTTPHeadersEnv = "DONMAI_PROVIDER_HTTP_HEADERS"

// MaxProviderHTTPHeaders caps how many headers one declaration may carry.
const MaxProviderHTTPHeaders = 16

// ProviderHTTPHeader is one declared header, in declaration order.
type ProviderHTTPHeader struct{ Name, Value string }

// ProviderHTTPHeadersError refuses a malformed declaration. Line is 1-based
// (0 for the whole value); the error never echoes a value.
type ProviderHTTPHeadersError struct {
	Line   int
	Reason string
}

func (e *ProviderHTTPHeadersError) Error() string {
	if e.Line == 0 {
		return ProviderHTTPHeadersEnv + ": " + e.Reason
	}
	return fmt.Sprintf("%s line %d: %s", ProviderHTTPHeadersEnv, e.Line, e.Reason)
}

// reservedProviderHeaderNames would override the provider credential or the
// request's transport. Beyond the shared auth and framing names: api-key is
// Azure OpenAI's credential header, and transfer-encoding, connection, te and
// upgrade are hop-by-hop framing headers (RFC 7230 §6.1) whose override would
// desynchronize the request rather than annotate it. Every x-goog-api* name
// is refused by prefix.
var reservedProviderHeaderNames = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "host": true,
	"content-length": true, "content-type": true, "x-api-key": true, "anthropic-api-key": true,
	"api-key": true, "transfer-encoding": true, "connection": true, "te": true, "upgrade": true,
}

// ProviderHTTPHeadersFromEnv parses env[ProviderHTTPHeadersEnv].
func ProviderHTTPHeadersFromEnv(env map[string]string) ([]ProviderHTTPHeader, error) {
	return ParseProviderHTTPHeaders(env[ProviderHTTPHeadersEnv])
}

// ParseProviderHTTPHeaders parses a declaration: each line is trimmed, blank
// lines are skipped, and a line splits on its first ':'. A name must be an
// RFC 7230 token, unreserved and unique case-insensitively. A value must be
// non-empty printable ASCII or tab, so CR, LF, NUL and other control bytes
// are refused, as is non-ASCII text some harness HTTP clients reject at
// request time. Any violation fails the whole declaration, so a header is
// never silently dropped. A blank declaration returns nil.
func ParseProviderHTTPHeaders(value string) ([]ProviderHTTPHeader, error) {
	var out []ProviderHTTPHeader
	seen := map[string]bool{}
	for i, raw := range strings.Split(value, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fail := func(format string, args ...any) error {
			return &ProviderHTTPHeadersError{Line: i + 1, Reason: fmt.Sprintf(format, args...)}
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fail(`want "Name: Value"`)
		}
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		lower := strings.ToLower(name)
		switch {
		case !isHTTPToken(name):
			return nil, fail("header name is not an RFC 7230 token")
		case reservedProviderHeaderNames[lower] || strings.HasPrefix(lower, "x-goog-api"):
			return nil, fail("header %q is reserved for auth or transport", lower)
		case seen[lower]:
			return nil, fail("header %q is declared more than once", lower)
		case val == "" || strings.ContainsFunc(val, func(r rune) bool { return (r < 0x20 && r != '\t') || r > 0x7e }):
			return nil, fail("header %q value must be non-empty printable ASCII", lower)
		case len(out) == MaxProviderHTTPHeaders:
			return nil, &ProviderHTTPHeadersError{Reason: fmt.Sprintf("more than %d headers declared", MaxProviderHTTPHeaders)}
		}
		seen[lower] = true
		out = append(out, ProviderHTTPHeader{Name: name, Value: val})
	}
	return out, nil
}

// isHTTPToken reports whether s is a non-empty RFC 7230 token (tchar+).
func isHTTPToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return s != ""
}
