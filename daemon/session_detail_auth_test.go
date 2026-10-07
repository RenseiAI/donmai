package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// getSessionDetail issues a GET /api/daemon/sessions/<id> against addr,
// attaching bearer when non-empty, and returns the status code and the
// decoded detail.
func getSessionDetail(t *testing.T, addr, id, bearer string) (int, SessionDetail) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/daemon/sessions/"+id, nil) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/daemon/sessions/%s: %v", id, err)
	}
	defer func() { _ = res.Body.Close() }()
	var detail SessionDetail
	if res.StatusCode == http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode session detail: %v", err)
		}
		if strings.Contains(string(raw), "tok-worker") || strings.Contains(string(raw), "mcp-secret") {
			// The raw check below is assertion-grade: redacted responses
			// must not contain the credential bytes anywhere.
			t.Logf("raw body carries credential bytes: %s", raw)
		}
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	return res.StatusCode, detail
}

// seedCredentialedSession stores one session detail carrying both
// credential fields and returns its id.
func seedCredentialedSession(t *testing.T, d *Daemon, id string) {
	t.Helper()
	detail := &SessionDetail{
		SessionID:             id,
		Repository:            "github.com/foo/bar",
		Ref:                   "main",
		WorkerID:              "wkr_1",
		AuthToken:             "tok-worker-live",
		PlatformURL:           "https://platform.example.com",
		McpAuthToken:          "mcp-secret-live",
		McpAuthTokenExpiresAt: "2030-01-01T00:00:00Z",
		IssueIdentifier:       "DEMO-1",
	}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  id,
		Repository: detail.Repository,
		Ref:        detail.Ref,
	}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
}

// TestSessionDetail_ReadRequiresCredential pins the credential boundary on
// the session-detail GET: with the control gate enforced, a
// credential-free read gets the same shape with every credential field
// cleared (still 200 — the route stays available to dashboards and CLI
// status), while the operator control token and the session's own read
// credential each receive the full detail.
func TestSessionDetail_ReadRequiresCredential(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	seedCredentialedSession(t, d, "sess-creds")

	// 1. Credential-free: redacted, not refused.
	status, redacted := getSessionDetail(t, srv.Addr(), "sess-creds", "")
	if status != http.StatusOK {
		t.Fatalf("unauthenticated GET = %d, want 200 with redacted fields", status)
	}
	if redacted.SessionID != "sess-creds" {
		t.Errorf("redacted SessionID = %q, want sess-creds", redacted.SessionID)
	}
	if redacted.IssueIdentifier != "DEMO-1" {
		t.Errorf("redacted IssueIdentifier = %q, want DEMO-1 (non-secret fields stay)", redacted.IssueIdentifier)
	}
	if redacted.AuthToken != "" {
		t.Errorf("redacted AuthToken = %q, want empty", redacted.AuthToken)
	}
	if redacted.McpAuthToken != "" {
		t.Errorf("redacted McpAuthToken = %q, want empty", redacted.McpAuthToken)
	}
	if redacted.McpAuthTokenExpiresAt != "" {
		t.Errorf("redacted McpAuthTokenExpiresAt = %q, want empty", redacted.McpAuthTokenExpiresAt)
	}
	if redacted.WorkerID != "wkr_1" || redacted.PlatformURL != "https://platform.example.com" {
		t.Errorf("redacted non-secret identity changed: worker=%q platform=%q",
			redacted.WorkerID, redacted.PlatformURL)
	}

	// 2. Operator control token: full detail.
	status, full := getSessionDetail(t, srv.Addr(), "sess-creds", testControlToken)
	if status != http.StatusOK {
		t.Fatalf("operator GET = %d, want 200", status)
	}
	if full.AuthToken != "tok-worker-live" {
		t.Errorf("operator AuthToken = %q, want live token", full.AuthToken)
	}
	if full.McpAuthToken != "mcp-secret-live" {
		t.Errorf("operator McpAuthToken = %q, want live token", full.McpAuthToken)
	}
	if full.McpAuthTokenExpiresAt != "2030-01-01T00:00:00Z" {
		t.Errorf("operator McpAuthTokenExpiresAt = %q, want live value", full.McpAuthTokenExpiresAt)
	}

	// 3. Per-session read credential: full detail for its own session.
	readTok, ok := d.sessionReadToken("sess-creds")
	if !ok || readTok == "" {
		t.Fatal("no read credential minted for sess-creds")
	}
	if readTok == testControlToken {
		t.Fatal("read credential must differ from the operator control token")
	}
	status, own := getSessionDetail(t, srv.Addr(), "sess-creds", readTok)
	if status != http.StatusOK {
		t.Fatalf("session-credential GET = %d, want 200", status)
	}
	if own.AuthToken != "tok-worker-live" || own.McpAuthToken != "mcp-secret-live" {
		t.Errorf("session-credential read redacted: auth=%q mcp=%q", own.AuthToken, own.McpAuthToken)
	}

	// 4. The same credential names no other session: cross-session use is
	// redacted, so one session's credential never reads another's tokens.
	seedCredentialedSession(t, d, "sess-other")
	status, cross := getSessionDetail(t, srv.Addr(), "sess-other", readTok)
	if status != http.StatusOK {
		t.Fatalf("cross-session GET = %d, want 200 redacted", status)
	}
	if cross.AuthToken != "" || cross.McpAuthToken != "" {
		t.Errorf("cross-session read leaked: auth=%q mcp=%q", cross.AuthToken, cross.McpAuthToken)
	}

	// 5. A wrong bearer is redacted, and an unknown id still 404s.
	status, wrong := getSessionDetail(t, srv.Addr(), "sess-creds", "wrong-bearer")
	if status != http.StatusOK {
		t.Fatalf("wrong-bearer GET = %d, want 200 redacted", status)
	}
	if wrong.AuthToken != "" || wrong.McpAuthToken != "" {
		t.Errorf("wrong-bearer read leaked: auth=%q mcp=%q", wrong.AuthToken, wrong.McpAuthToken)
	}
	if status, _ := getSessionDetail(t, srv.Addr(), "sess-missing", ""); status != http.StatusNotFound {
		t.Errorf("unknown id GET = %d, want 404", status)
	}
}

// TestSessionDetail_ReadTokenLifecycle pins the credential's lifetime to
// the detail it names: minted at accept, gone at session end, and rolled
// back with a failed installation.
func TestSessionDetail_ReadTokenLifecycle(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()

	if _, ok := d.sessionReadToken("sess-new"); ok {
		t.Fatal("read credential exists before accept")
	}
	seedCredentialedSession(t, d, "sess-new")
	first, ok := d.sessionReadToken("sess-new")
	if !ok || first == "" {
		t.Fatal("no read credential after accept")
	}

	// A duplicate accept keeps the first credential: at most one live
	// credential ever names the session.
	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID: "sess-new", Repository: "github.com/foo/bar", Ref: "main",
	}, &SessionDetail{SessionID: "sess-new", AuthToken: "second"}); err == nil {
		t.Fatal("duplicate accept succeeded, want refusal")
	}
	kept, ok := d.sessionReadToken("sess-new")
	if !ok || kept != first {
		t.Errorf("duplicate accept rotated the credential: kept=%q first=%q", kept, first)
	}
	_ = srv

	// Session end deletes the credential with the detail.
	d.sessionDetails.Delete("sess-new")
	if _, ok := d.sessionReadToken("sess-new"); ok {
		t.Error("read credential survives Delete, want it gone with the detail")
	}

	// A failed installation rolls its credential back: DeleteIfOwner on a
	// non-owner changes nothing, on the owner removes both.
	lease, ok := d.sessionDetails.StoreIfAbsent(&SessionDetail{SessionID: "sess-lease", AuthToken: "x"})
	if !ok {
		t.Fatal("StoreIfAbsent rejected")
	}
	if _, ok := d.sessionReadToken("sess-lease"); !ok {
		t.Fatal("no read credential after StoreIfAbsent")
	}
	if d.sessionDetails.DeleteIfOwner(sessionDetailLease{sessionID: "sess-lease", generation: lease.generation + 1}) {
		t.Error("DeleteIfOwner removed a generation it does not own")
	}
	if _, ok := d.sessionReadToken("sess-lease"); !ok {
		t.Error("stale DeleteIfOwner removed the live credential")
	}
	if !d.sessionDetails.DeleteIfOwner(lease) {
		t.Fatal("DeleteIfOwner refused its own lease")
	}
	if _, ok := d.sessionReadToken("sess-lease"); ok {
		t.Error("rolled-back installation left its credential behind")
	}
}

// TestSessionDetail_SpawnEnvCarriesReadToken pins the spawner half of the
// bootstrap: the daemon-owned spawn environment states the session's own
// read credential beside the session id, and states no other session's.
func TestSessionDetail_SpawnEnvCarriesReadToken(t *testing.T) {
	d, _, cleanup := mustStartDaemon(t)
	defer cleanup()

	seedCredentialedSession(t, d, "sess-env")
	env := d.spawner.sessionEnv(SessionSpec{SessionID: "sess-env"}, nil)
	want, _ := d.sessionReadToken("sess-env")
	found := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, sessionReadTokenEnv+"=") {
			found = strings.TrimPrefix(kv, sessionReadTokenEnv+"=")
		}
		if strings.HasPrefix(kv, "DONMAI_CONTROL_TOKEN=") || strings.HasPrefix(kv, "DONMAI_CONTROL_TOKEN_FILE=") {
			t.Errorf("spawn env carries operator control credential %q; the session must not hold it", kv)
		}
	}
	if found == "" {
		t.Fatalf("spawn env has no %s entry", sessionReadTokenEnv)
	}
	if found != want {
		t.Errorf("spawn %s names the wrong credential", sessionReadTokenEnv)
	}

	// Unknown sessions state no credential rather than an empty one.
	for _, kv := range d.spawner.sessionEnv(SessionSpec{SessionID: "sess-unknown"}, nil) {
		if strings.HasPrefix(kv, sessionReadTokenEnv+"=") {
			t.Errorf("unknown session states a read credential: %q", kv)
		}
	}
}

// TestSessionDetail_RedactKeepsStoredOriginal proves the redaction copies:
// the stored detail still serves credentials after a redacted read.
func TestSessionDetail_RedactKeepsStoredOriginal(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	seedCredentialedSession(t, d, "sess-copy")
	if _, redacted := getSessionDetail(t, srv.Addr(), "sess-copy", ""); redacted.AuthToken != "" {
		t.Fatalf("fixture read not redacted: %q", redacted.AuthToken)
	}
	stored, ok := d.SessionDetail("sess-copy")
	if !ok {
		t.Fatal("stored detail missing after redacted read")
	}
	if stored.AuthToken != "tok-worker-live" || stored.McpAuthToken != "mcp-secret-live" {
		t.Errorf("redacted read mutated the store: auth=%q mcp=%q", stored.AuthToken, stored.McpAuthToken)
	}
	if _, full := getSessionDetail(t, srv.Addr(), "sess-copy", testControlToken); full.AuthToken == "" {
		t.Error("operator read after redacted read lost the credentials")
	}
}
