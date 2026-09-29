package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// postControl issues a POST against the test server, attaching bearer when
// non-empty, and returns the status code.
func postControl(t *testing.T, addr, path, bearer string, body any) int {
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
	return res.StatusCode
}

// TestControlAuth_MutatingRoutesRequireToken pins the fix: with the gate
// active, every mutating control route rejects a credential-free caller
// with 401, accepts the bearer the operator CLI carries, and leaves the
// read-only status route open.
func TestControlAuth_MutatingRoutesRequireToken(t *testing.T) {
	setControlAuthForTest(t, "opaque-operator-token")
	_, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	mutating := []struct {
		path string
		body any
	}{
		{"/api/daemon/pause", nil},
		{"/api/daemon/resume", nil},
		{"/api/daemon/drain", afclient.DaemonDrainRequest{TimeoutSeconds: 1}},
		{"/api/daemon/capacity", map[string]string{"key": "capacity.poolMaxDiskGb", "value": "20"}},
		{"/api/daemon/pool/evict", afclient.EvictPoolRequest{RepoURL: "github.com/foo/bar", OlderThanSeconds: 60}},
		{"/api/daemon/sessions", SessionSpec{SessionID: "sess-1", Repository: "github.com/foo/bar", Ref: "main"}},
	}
	for _, tc := range mutating {
		if got := postControl(t, srv.Addr(), tc.path, "", tc.body); got != http.StatusUnauthorized {
			t.Errorf("POST %s without token = %d, want 401", tc.path, got)
		}
		if got := postControl(t, srv.Addr(), tc.path, "wrong-token", tc.body); got != http.StatusUnauthorized {
			t.Errorf("POST %s with wrong token = %d, want 401", tc.path, got)
		}
	}

	var resp afclient.DaemonActionResponse
	status := postControl(t, srv.Addr(), "/api/daemon/pause", "opaque-operator-token", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /api/daemon/pause with token = %d, want 200", status)
	}

	// Read-only status stays available without a credential.
	requireGet(t, srv.Addr(), "/api/daemon/status", &resp)

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
	if got := postControl(t, srv.Addr(), "/api/daemon/stop", "", nil); got != http.StatusUnauthorized {
		t.Errorf("POST /api/daemon/stop from session-like env = %d, want 401", got)
	}
}
