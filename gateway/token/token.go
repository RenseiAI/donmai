// Package token mints and compares the per-session bearer tokens that bind an
// inbound loopback request to its dispatch (08 §5). This is what makes cost
// attribution and policy enforcement per-SESSION rather than per-process, and
// prevents cross-session credential bleed when multiple workers share one
// daemon: a request carrying session A's token can only ever reach session A's
// credential scope.
//
// A token is an opaque 256-bit random value (base64url, no padding). It is not
// a JWT and carries no claims — the gateway holds the session→route mapping in
// memory and looks it up by exact token match. Comparison is constant-time.
package token

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// Token is an opaque per-session bearer value.
type Token string

// Mint returns a fresh cryptographically-random token. The value is the only
// thing that authorizes use of a session's route; treat it as a secret (it is
// never logged and never written to the cost ledger).
func Mint() (Token, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gateway/token: mint: %w", err)
	}
	return Token(base64.RawURLEncoding.EncodeToString(b[:])), nil
}

// Equal reports whether two tokens match, in constant time (so a caller cannot
// time-probe the session table).
func Equal(a, b Token) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// FromBearer extracts the token from an HTTP Authorization header value,
// accepting "Bearer <token>" (case-insensitive scheme) or a bare token (some
// OpenAI-compatible clients send the api key with no scheme). Returns "" when
// no token is present.
func FromBearer(header string) Token {
	if header == "" {
		return ""
	}
	const prefix = "bearer "
	if len(header) > len(prefix) && equalFoldASCII(header[:len(prefix)], prefix) {
		return Token(header[len(prefix):])
	}
	return Token(header)
}

// equalFoldASCII is a tiny ASCII-only case-insensitive compare (avoids a
// strings import for one call and stays allocation-free).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
