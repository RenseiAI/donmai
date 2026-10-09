package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
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

// controlBearer extracts the bearer credential from r's Authorization
// header, or "" when the header is absent or malformed. It is the
// single parser for control-surface bearer checks: the mutating gate and
// the session-detail read agree on what counts as a presented credential.
// The credential itself is never padded or normalized: a bearer with
// leading, trailing, or interior whitespace that a proxy added is not the
// bearer the daemon issued, so it compares unequal instead of passing
// stripped. Callers that present such a header receive the unauthenticated
// answer, never another session's detail.
func controlBearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if strings.TrimSpace(token) != token || token == "" {
		return ""
	}
	return token
}

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

// sessionReadTokenEnv names the per-session read credential the daemon
// states in every spawned worker's environment. It carries no operator
// privilege: it authorizes exactly one session-detail read, for exactly
// the session named beside it, and nothing else on the control API.
// The canonical spelling lives in runtime/env (SessionReadTokenEnv) so
// the worker-side strip and every inherited-env filter agree on the name.
const sessionReadTokenEnv = runtimeenv.SessionReadTokenEnv

// mintSessionReadToken returns a fresh random per-session read credential.
// The token is opaque to every holder: the daemon compares it with the
// stored value in constant time and never derives meaning from its bytes.
func mintSessionReadToken() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	return hex.EncodeToString(raw)
}

// requireControlAuth gates the mutating methods of a control handler behind
// the per-install bearer token. GET requests pass through untouched so
// read-only status stays available to dashboards, spawned workers fetching
// their own session detail, and liveness probes. When the gate is required
// but no token is loaded, mutating requests get 503 — the gate fails
// closed. Only a daemon built with neither a token nor RequireControlToken
// (tests, harnesses) runs the routes open.
//
// The session-detail read (GET /api/daemon/sessions/<id>) is NOT covered
// by this gate's GET pass-through: that payload carries per-session
// credentials, so handleSessionDetail enforces its own read rule
// (operator control token or the session's own read credential) and this
// wrapper must not pre-empt it. The wrapper therefore passes the
// detail-GET path straight to the handler, which owns the decision.
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
			// The local file runtime has a separate protected operator identity.
			// Its outer gate already authenticates mutating requests; recheck
			// the same exact bearer here before accepting it as the control
			// route's local alternative. The controller token remains required
			// for every non-local daemon.
			if local := s.daemon.localRuntime.Load(); local != nil && local.auth != nil &&
				local.auth.VerifyOperator(localBearer(r)) == nil {
				next(w, r)
				return
			}
			got := controlBearer(r)
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
