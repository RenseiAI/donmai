package daemon

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// controlAuthForTest overrides the token gate in tests. Non-nil means the
// gate is active with exactly this token; nil + empty daemon token means
// legacy open mode. Production never sets it.
var controlAuthForTest *string

// setControlAuthForTest activates the mutating-route gate with token for
// the calling test, restoring the previous state on cleanup.
func setControlAuthForTest(t interface {
	Cleanup(func())
	Helper()
}, token string,
) {
	t.Helper()
	prev := controlAuthForTest
	cp := token
	controlAuthForTest = &cp
	t.Cleanup(func() { controlAuthForTest = prev })
}

// controlTokenForRequest returns the gate token when the gate is active:
// the test override wins, otherwise the daemon's own configured token.
// ("", false) means open mode — no gate.
func controlTokenForRequest(d *Daemon) (string, bool) {
	if controlAuthForTest != nil {
		return *controlAuthForTest, true
	}
	if d == nil {
		return "", false
	}
	if tok := strings.TrimSpace(d.opts.ControlToken); tok != "" {
		return tok, true
	}
	return "", false
}

// requireControlAuth gates the mutating methods of a control handler behind
// the per-install bearer token. GET requests pass through untouched so
// read-only status stays available to dashboards, spawned workers fetching
// their own session detail, and liveness probes. When no token is configured
// the gate stays open so pre-token daemons, tests, and local harnesses keep
// working; the production entry point always configures one.
func (s *Server) requireControlAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			next(w, r)
			return
		}
		want, gated := controlTokenForRequest(s.daemon)
		if !gated {
			next(w, r)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid control token"})
			return
		}
		next(w, r)
	}
}
