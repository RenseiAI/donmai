package daemon

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// controlAuthMode is the state of the mutating-route gate for one request.
type controlAuthMode int

const (
	// controlAuthOpen passes every request through. Only reachable when
	// the daemon was built without RequireControlToken and without a
	// token — the legacy mode tests and local harnesses use.
	controlAuthOpen controlAuthMode = iota
	// controlAuthEnforced requires the configured bearer token.
	controlAuthEnforced
	// controlAuthUnavailable refuses every mutating request: the gate is
	// required but the daemon holds no token (minting or reading it
	// failed). Fail closed rather than run the routes unauthenticated.
	controlAuthUnavailable
)

// controlTokenUnavailableMessage is the body the gate returns while it is
// required but holds no token. It names the remedy without echoing any
// path, error detail, or credential.
const controlTokenUnavailableMessage = "control token unavailable: mutating control routes are disabled until the daemon restarts with a readable control token; read-only routes stay available"

// controlAuthState reports the gate mode and, when enforced, the token.
func controlAuthState(d *Daemon) (controlAuthMode, string) {
	if d == nil {
		return controlAuthOpen, ""
	}
	if tok := strings.TrimSpace(d.opts.ControlToken); tok != "" {
		return controlAuthEnforced, tok
	}
	if d.opts.RequireControlToken {
		return controlAuthUnavailable, ""
	}
	return controlAuthOpen, ""
}

// requireControlAuth gates the mutating methods of a control handler behind
// the per-install bearer token. GET requests pass through untouched so
// read-only status stays available to dashboards, spawned workers fetching
// their own session detail, and liveness probes. When the gate is required
// but no token is loaded, mutating requests get 503 — the gate fails
// closed. Only a daemon built with neither a token nor RequireControlToken
// (tests, harnesses) runs the routes open.
func (s *Server) requireControlAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			next(w, r)
			return
		}
		mode, want := controlAuthState(s.daemon)
		switch mode {
		case controlAuthOpen:
			next(w, r)
		case controlAuthEnforced:
			got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid control token"})
				return
			}
			next(w, r)
		default:
			// controlAuthUnavailable, and any mode added later without a
			// case here, refuses: the gate fails closed.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": controlTokenUnavailableMessage})
		}
	}
}
