package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

const testControlToken = "opaque-operator-token"

// controlRoute is one mutating control route and a body that reaches its
// handler.
type controlRoute struct {
	path string
	body any
	// destructive routes tear down or re-plan the test daemon when they
	// reach their handler; the "correct bearer passes" loop skips them and
	// leaves that leg to the gate unit test.
	destructive bool
}

// mutatingControlRoutes lists every mutating route registered behind
// requireControlAuth. Keep it in step with Server.register.
var mutatingControlRoutes = []controlRoute{
	{path: "/api/daemon/pause"},
	{path: "/api/daemon/resume"},
	{path: "/api/daemon/stop", destructive: true},
	{path: "/api/daemon/drain", body: afclient.DaemonDrainRequest{TimeoutSeconds: 1}, destructive: true},
	{path: "/api/daemon/restart/prepare", destructive: true},
	{path: "/api/daemon/update", destructive: true},
	{path: "/api/daemon/capacity", body: map[string]string{"key": "capacity.poolMaxDiskGb", "value": "20"}},
	{path: "/api/daemon/pool/evict", body: afclient.EvictPoolRequest{RepoURL: "github.com/foo/bar", OlderThanSeconds: 60}},
	{path: "/api/daemon/sessions", body: SessionSpec{SessionID: "sess-gate", Repository: "github.com/foo/bar", Ref: "main"}},
	{path: "/api/daemon/sessions/sess-unknown/stop"},
	{path: kitRoutePrefix + "kit-unknown/install"},
	{path: kitRoutePrefix + "kit-unknown/enable"},
	{path: kitRoutePrefix + "kit-unknown/disable"},
	{path: kitSourceRoutePrefix + "source-unknown/enable"},
	{path: kitSourceRoutePrefix + "source-unknown/disable"},
	{path: "/api/daemon/workareas/archive-unknown/restore"},
}

// postControl issues a POST against the test server, attaching bearer when
// non-empty, and returns the status code and the decoded `error` field.
func postControl(t *testing.T, addr, path, bearer string, body any) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, &buf) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	var payload struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &payload)
	return res.StatusCode, payload.Error
}

// getControlStatus issues a credential-free GET and returns the status code.
func getControlStatus(t *testing.T, addr, path string) int {
	t.Helper()
	res, err := http.Get("http://" + addr + path) //nolint:gosec
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// TestControlAuth_FailsClosedWithoutToken pins the fail-closed contract:
// a daemon whose gate is required but holds no token (minting or reading
// it failed) refuses every mutating route with 503, whatever bearer the
// caller presents, and none of the refused requests reaches its handler.
func TestControlAuth_FailsClosedWithoutToken(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = ""
	})
	defer cleanup()

	before := d.State()
	for _, rt := range mutatingControlRoutes {
		for _, bearer := range []string{"", "any-guess"} {
			status, msg := postControl(t, srv.Addr(), rt.path, bearer, rt.body)
			if status != http.StatusServiceUnavailable {
				t.Errorf("POST %s (bearer %q) without a daemon token = %d, want 503", rt.path, bearer, status)
			}
			if msg != controlTokenUnavailableMessage {
				t.Errorf("POST %s error = %q, want %q", rt.path, msg, controlTokenUnavailableMessage)
			}
		}
	}

	// The refused pause/stop/drain never ran: the daemon is still in the
	// state it started in and still serving.
	if got := d.State(); got != before {
		t.Errorf("daemon state after refused mutations = %q, want %q", got, before)
	}
	if got := getControlStatus(t, srv.Addr(), "/api/daemon/status"); got != http.StatusOK {
		t.Errorf("GET /api/daemon/status after refused mutations = %d, want 200", got)
	}
}

// TestControlAuth_ReadOnlyRoutesOpenWithoutToken pins that the fail-closed
// gate only touches mutating methods: every read-only route, including
// the GET legs of the gated prefixes, answers a credential-free caller on
// a daemon that holds no token.
func TestControlAuth_ReadOnlyRoutesOpenWithoutToken(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = ""
	})
	defer cleanup()

	// Seed a session in-process (HTTP accept is gated) so the per-session
	// detail GET a spawned worker relies on has something to return, and
	// point the workarea registry at an empty temp archive root.
	spec := SessionSpec{SessionID: "sess-read", Repository: "github.com/foo/bar", Ref: "main"}
	detail := &SessionDetail{SessionID: spec.SessionID, Repository: spec.Repository, Ref: spec.Ref}
	if _, err := d.AcceptWorkWithDetail(spec, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	d.SetWorkareaArchiveRegistry(NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: t.TempDir()}))

	cases := []struct {
		path string
		want int
	}{
		{"/api/daemon/status", http.StatusOK},
		{"/api/daemon/stats", http.StatusOK},
		{"/api/daemon/heartbeat", http.StatusOK},
		{"/api/daemon/doctor", http.StatusOK},
		{"/api/daemon/pool/stats", http.StatusOK},
		{"/api/daemon/capabilities", http.StatusOK},
		{"/healthz", http.StatusOK},
		{"/api/daemon/sessions", http.StatusOK},
		{"/api/daemon/sessions/sess-read", http.StatusOK},
		{"/api/daemon/sessions/sess-missing", http.StatusNotFound},
		{"/api/daemon/kits", http.StatusOK},
		{kitRoutePrefix + "kit-unknown", http.StatusNotFound},
		{"/api/daemon/kit-sources", http.StatusOK},
		{"/api/daemon/workareas", http.StatusOK},
		{"/api/daemon/workareas/archive-unknown", http.StatusNotFound},
	}
	for _, tc := range cases {
		if got := getControlStatus(t, srv.Addr(), tc.path); got != tc.want {
			t.Errorf("GET %s without a token = %d, want %d", tc.path, got, tc.want)
		}
	}
}

// TestControlAuth_MutatingRoutesRequireToken pins the enforced gate: with
// a token loaded, every mutating route rejects a missing or wrong bearer
// with 401, the correct bearer reaches the handler, and read-only status
// stays open.
func TestControlAuth_MutatingRoutesRequireToken(t *testing.T) {
	_, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	for _, rt := range mutatingControlRoutes {
		if got, _ := postControl(t, srv.Addr(), rt.path, "", rt.body); got != http.StatusUnauthorized {
			t.Errorf("POST %s without token = %d, want 401", rt.path, got)
		}
		if got, _ := postControl(t, srv.Addr(), rt.path, "wrong-token", rt.body); got != http.StatusUnauthorized {
			t.Errorf("POST %s with wrong token = %d, want 401", rt.path, got)
		}
	}
	for _, rt := range mutatingControlRoutes {
		if rt.destructive {
			continue
		}
		got, msg := postControl(t, srv.Addr(), rt.path, testControlToken, rt.body)
		if got == http.StatusUnauthorized || got == http.StatusServiceUnavailable {
			t.Errorf("POST %s with the correct token = %d (%s), want it past the gate", rt.path, got, msg)
		}
	}

	// Read-only status stays available without a credential.
	if got := getControlStatus(t, srv.Addr(), "/api/daemon/status"); got != http.StatusOK {
		t.Errorf("GET /api/daemon/status without token = %d, want 200", got)
	}

	// A spawned session's environment carries no token and no token-file
	// override: simulate exactly that caller by composing the worker env
	// and confirming the credential-free request is refused.
	sessionEnv := composeEnv(
		nil,
		map[string]string{afclient.ControlTokenEnv: "smuggled", afclient.ControlTokenFileEnv: "/tmp/smuggled"},
		nil,
	)
	for _, key := range []string{afclient.ControlTokenEnv, afclient.ControlTokenFileEnv} {
		for _, kv := range sessionEnv {
			if strings.HasPrefix(kv, key+"=") {
				t.Fatalf("composed worker env carries %s; sessions must never receive the control token", key)
			}
		}
	}
	if got, _ := postControl(t, srv.Addr(), "/api/daemon/stop", "", nil); got != http.StatusUnauthorized {
		t.Errorf("POST /api/daemon/stop from session-like env = %d, want 401", got)
	}
}

// TestRequireControlAuth_Matrix drives the gate directly with a stub
// handler, covering every mode against GET and mutating methods and the
// bearer shapes a caller can present. It proves the pass leg for the
// destructive routes the server-level test does not invoke.
func TestRequireControlAuth_Matrix(t *testing.T) {
	t.Parallel()

	daemonWith := func(token string, required bool) *Daemon {
		return &Daemon{opts: Options{ControlToken: token, RequireControlToken: required}}
	}
	cases := []struct {
		name       string
		d          *Daemon
		method     string
		auth       string
		wantStatus int
		wantNext   bool
	}{
		{"required without token refuses POST", daemonWith("", true), http.MethodPost, "", http.StatusServiceUnavailable, false},
		{"required without token refuses a presented bearer", daemonWith("", true), http.MethodPost, "Bearer anything", http.StatusServiceUnavailable, false},
		{"required without token refuses DELETE", daemonWith("", true), http.MethodDelete, "", http.StatusServiceUnavailable, false},
		{"required without token refuses whitespace token", daemonWith("   ", true), http.MethodPost, "Bearer    ", http.StatusServiceUnavailable, false},
		{"required without token passes GET", daemonWith("", true), http.MethodGet, "", http.StatusOK, true},
		{"enforced passes correct bearer", daemonWith(testControlToken, true), http.MethodPost, "Bearer " + testControlToken, http.StatusOK, true},
		{"enforced without the required flag still enforces", daemonWith(testControlToken, false), http.MethodPost, "", http.StatusUnauthorized, false},
		{"enforced rejects missing bearer", daemonWith(testControlToken, true), http.MethodPost, "", http.StatusUnauthorized, false},
		{"enforced rejects wrong bearer", daemonWith(testControlToken, true), http.MethodPost, "Bearer wrong-token", http.StatusUnauthorized, false},
		{"enforced rejects empty bearer", daemonWith(testControlToken, true), http.MethodPost, "Bearer ", http.StatusUnauthorized, false},
		{"enforced rejects token prefix", daemonWith(testControlToken, true), http.MethodPost, "Bearer " + testControlToken[:5], http.StatusUnauthorized, false},
		{"enforced passes GET without bearer", daemonWith(testControlToken, true), http.MethodGet, "", http.StatusOK, true},
		{"legacy open mode passes POST", daemonWith("", false), http.MethodPost, "", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			srv := &Server{daemon: tc.d}
			h := srv.requireControlAuth(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(tc.method, "/api/daemon/pause", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantNext {
				t.Errorf("handler reached = %v, want %v", called, tc.wantNext)
			}
			if tc.wantStatus == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), testControlToken) {
				t.Errorf("refusal body leaks the token: %s", rec.Body.String())
			}
		})
	}
}
