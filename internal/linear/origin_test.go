package linear

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCanonicalProxyOrigin_RejectsMalformedOrigins pins the credential-free
// bare-origin contract. The first case is the shape that broke a live headless
// dispatch: an injected origin with a trailing delimiter, which made every
// proxied Linear read fail late and opaquely.
func TestCanonicalProxyOrigin_RejectsMalformedOrigins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"trailing semicolon", "https://agent.example.com;"},
		{"trailing semicolon after slash", "https://agent.example.com/;"},
		{"trailing comma", "https://agent.example.com,"},
		{"trailing colon without port", "https://agent.example.com:"},
		{"trailing dot", "https://agent.example.com."},
		{"two origins joined", "https://agent.example.com;https://evil.example.net"},
		{"userinfo", "https://user:lin_api_secret@agent.example.com"},
		{"userinfo only", "https://token@agent.example.com"},
		{"path", "https://agent.example.com/api"},
		{"double trailing slash", "https://agent.example.com//"},
		{"query", "https://agent.example.com?next=1"},
		{"bare question mark", "https://agent.example.com?"},
		{"fragment", "https://agent.example.com#frag"},
		{"bare hash", "https://agent.example.com#"},
		{"non-loopback http", "http://agent.example.com"},
		{"non-http scheme", "ftp://agent.example.com"},
		{"websocket scheme", "wss://agent.example.com"},
		{"scheme relative", "//agent.example.com"},
		{"host only", "agent.example.com"},
		{"opaque", "https:agent.example.com"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"embedded space", "https://agent.example.com /x"},
		{"crlf injection", "https://agent.example.com\r\nHost: evil.example.net"},
		{"embedded tab", "https://agent\t.rensei.dev"},
		{"port out of range", "https://agent.example.com:70000"},
		{"non numeric port", "https://agent.example.com:https"},
		{"empty label", "https://agent..rensei.dev"},
		{"leading hyphen label", "https://-agent.example.com"},
		{"no host", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := canonicalProxyOrigin(tc.raw)
			if err == nil {
				t.Fatalf("canonicalProxyOrigin(%q) = %q, want error", tc.raw, got)
			}
			if !errors.Is(err, ErrInvalidPlatformURL) {
				t.Fatalf("error %v does not wrap ErrInvalidPlatformURL", err)
			}
			assertValueFree(t, err.Error(), tc.raw)
		})
	}
}

// TestCanonicalProxyOrigin_AcceptsBareOrigins covers the accepted shapes,
// including the "valid slash origin canonicalizes" case: a single trailing
// slash is normalized away rather than rejected.
func TestCanonicalProxyOrigin_AcceptsBareOrigins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare https origin", "https://platform.example.com", "https://platform.example.com"},
		{"trailing slash canonicalizes", "https://platform.example.com/", "https://platform.example.com"},
		{"surrounding whitespace trimmed", "  https://platform.example.com/  ", "https://platform.example.com"},
		{"explicit port", "https://platform.example.com:8443", "https://platform.example.com:8443"},
		{"uppercase host lowercased", "https://PLATFORM.Example.COM/", "https://platform.example.com"},
		{"loopback http by name", "http://localhost:7734", "http://localhost:7734"},
		{"loopback http by ipv4", "http://127.0.0.1:7734", "http://127.0.0.1:7734"},
		{"loopback http by ipv6", "http://[::1]:7734", "http://[::1]:7734"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := canonicalProxyOrigin(tc.raw)
			if err != nil {
				t.Fatalf("canonicalProxyOrigin(%q): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("canonicalProxyOrigin(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNewProxiedClient_MalformedOriginIssuesNoRequest is the fail-closed proof:
// a malformed origin is rejected by the constructor, so no HTTP request — and
// therefore no Authorization header — is ever produced.
func TestNewProxiedClient_MalformedOriginIssuesNoRequest(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const token = "rsk_super_secret_token"
	malformed := srv.URL + ";"

	c, err := NewProxiedClient(malformed, token)
	if err == nil {
		t.Fatalf("NewProxiedClient(malformed) = %#v, want error", c)
	}
	if c != nil {
		t.Fatal("NewProxiedClient returned a usable client for a malformed origin")
	}
	if !errors.Is(err, ErrInvalidPlatformURL) {
		t.Fatalf("error %v does not wrap ErrInvalidPlatformURL", err)
	}
	assertValueFree(t, err.Error(), malformed)
	if strings.Contains(err.Error(), token) {
		t.Fatal("constructor error leaked the platform token")
	}
	if hits.Load() != 0 {
		t.Fatalf("malformed origin produced %d HTTP request(s); want 0", hits.Load())
	}
}

// TestNewProxiedClient_LoopbackHTTPStillWorks guards the httptest-backed
// callers (and `donmai host` loopback): plaintext HTTP against the local
// machine must keep working, and the composed endpoint must be exact.
func TestNewProxiedClient_LoopbackHTTPStillWorks(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := NewProxiedClient(srv.URL+"/", "rsk_abc")
	if err != nil {
		t.Fatalf("NewProxiedClient(loopback): %v", err)
	}
	// The composed endpoint is rooted at the canonical loopback origin with a
	// single separator; the exact proxy path is pinned by TestNewProxiedClient.
	if !strings.HasPrefix(c.BaseURL, srv.URL+"/") || strings.Contains(c.BaseURL, "//api") {
		t.Fatalf("BaseURL = %q, want the canonical loopback origin plus one path separator", c.BaseURL)
	}
	if !c.ProxyMode {
		t.Fatal("ProxyMode = false, want true")
	}
}

// assertValueFree fails when message echoes the rejected origin. Proxy origins
// arrive from operator configuration and can carry a bearer in their userinfo
// or query, and these errors land in stderr, logs and session records.
func assertValueFree(t *testing.T, message, raw string) {
	t.Helper()
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return
	}
	if strings.Contains(message, trimmed) {
		t.Fatalf("error message echoed the rejected origin: %q", message)
	}
	for _, secret := range []string{"lin_api_secret", "agent.example.com", "evil.example.net"} {
		if strings.Contains(trimmed, secret) && strings.Contains(message, secret) {
			t.Fatalf("error message leaked %q from the rejected origin: %q", secret, message)
		}
	}
}
