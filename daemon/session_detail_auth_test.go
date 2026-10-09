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
	// 5b. A duplicated Authorization header is not a credential either:
	// the read parser rejects it, so the route answers redacted — the
	// same verdict the mutating gate reaches (pinned in the gate's own
	// matrix test). A proxy that duplicates the header must not turn a
	// credentialed read into a full one, nor a refused mutation into an
	// accepted one.
	dupReq, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/api/daemon/sessions/sess-creds", nil) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	dupReq.Header.Add("Authorization", "Bearer "+readTok)
	dupReq.Header.Add("Authorization", "Bearer "+readTok)
	dupRes, err := http.DefaultClient.Do(dupReq)
	if err != nil {
		t.Fatalf("duplicated-header GET: %v", err)
	}
	defer func() { _ = dupRes.Body.Close() }()
	var dupDetail SessionDetail
	dupRaw, _ := io.ReadAll(dupRes.Body)
	if dupRes.StatusCode == http.StatusOK {
		if err := json.Unmarshal(dupRaw, &dupDetail); err != nil {
			t.Fatalf("decode duplicated-header detail: %v", err)
		}
	}
	if dupRes.StatusCode != http.StatusOK {
		t.Fatalf("duplicated-header GET = %d, want 200 redacted", dupRes.StatusCode)
	}
	if dupDetail.AuthToken != "" || dupDetail.McpAuthToken != "" {
		t.Errorf("duplicated-header read leaked: auth=%q mcp=%q", dupDetail.AuthToken, dupDetail.McpAuthToken)
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
// rawSessionDetailBody returns the raw body of GET /api/daemon/sessions/<id>,
// attaching bearer when non-empty, and fails unless the status is 200.
func rawSessionDetailBody(t *testing.T, addr, id, bearer string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/daemon/sessions/"+id, nil) //nolint:gosec,noctx
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
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/daemon/sessions/%s = %d (%v)", id, res.StatusCode, err)
	}
	return string(raw)
}

// TestSessionDetail_RedactedBodyCarriesNoPollCredential seeds the detail
// through the production poll decode path. There the operational payload
// is the canonical projection of the raw work item, so it repeats the
// item's mcpAuthToken, and the agent-card MCP servers arrive verbatim with
// their own header and env values. A credential-free read must carry none
// of those bytes anywhere in the body; both credentialed reads keep them.
func TestSessionDetail_RedactedBodyCarriesNoPollCredential(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	const raw = `{"sessionId":"sess-poll-creds","repository":"github.com/foo/bar","ref":"main",` +
		`"mcpAuthToken":"mcp-item-secret","mcpAuthTokenExpiresAt":"2030-01-01T00:00:00Z",` +
		`"mcpServers":[{"name":"card-http","type":"http","url":"https://mcp.example/x",` +
		`"headers":{"Authorization":"Bearer card-header-secret"}},` +
		`{"name":"card-stdio","type":"stdio","command":"card-server","env":{"CARD_API_KEY":"card-env-secret"}}]}`
	var item PollWorkItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("decode poll item: %v", err)
	}
	detail := PollItemToSessionDetail(item, nil, "https://platform.example.com", "worker-auth-secret", "wkr_1")
	if !strings.Contains(string(detail.OperationalPayload), "mcp-item-secret") {
		t.Fatal("fixture: the operational payload no longer repeats the item's mcpAuthToken")
	}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID: "sess-poll-creds", Repository: "github.com/foo/bar", Ref: "main",
	}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	secrets := []string{"mcp-item-secret", "card-header-secret", "card-env-secret", "worker-auth-secret"}

	bare := rawSessionDetailBody(t, srv.Addr(), "sess-poll-creds", "")
	for _, secret := range secrets {
		if strings.Contains(bare, secret) {
			t.Errorf("credential-free body carries %q: %s", secret, bare)
		}
	}
	var redacted SessionDetail
	if err := json.Unmarshal([]byte(bare), &redacted); err != nil {
		t.Fatalf("decode redacted detail: %v", err)
	}
	if len(redacted.McpServers) != 2 || redacted.McpServers[0].Name != "card-http" ||
		redacted.McpServers[1].Type != "stdio" {
		t.Errorf("redacted MCP servers lost their names or transports: %+v", redacted.McpServers)
	}

	readTok, ok := d.sessionReadToken("sess-poll-creds")
	if !ok {
		t.Fatal("no read credential minted for sess-poll-creds")
	}
	for _, bearer := range []string{testControlToken, readTok} {
		full := rawSessionDetailBody(t, srv.Addr(), "sess-poll-creds", bearer)
		for _, secret := range secrets {
			if !strings.Contains(full, secret) {
				t.Errorf("credentialed body lost %q", secret)
			}
		}
	}
}

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

// TestSessionDetail_StatedTokenPathReachesOnlyCredentialedReads pins the
// accept-time stamp of the daemon-resolved token path: the stored detail
// carries the daemon's own answer (never a work item's), the session's own
// read credential and the operator token receive it, and the
// credential-free read carries none of it.
func TestSessionDetail_StatedTokenPathReachesOnlyCredentialedReads(t *testing.T) {
	const stated = "/var/lib/host/control-token"
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
		o.ControlTokenPath = stated
	})
	defer cleanup()

	// A work item that tries to aim its own seat's deny at an arbitrary
	// host file must not survive the accept: the daemon's answer wins.
	detail := &SessionDetail{
		SessionID:        "sess-path",
		Repository:       "github.com/foo/bar",
		Ref:              "main",
		WorkerID:         "wkr_1",
		AuthToken:        "tok-worker-live",
		ControlTokenPath: "/tmp/smuggled-path",
	}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID:  "sess-path",
		Repository: detail.Repository,
		Ref:        detail.Ref,
	}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	stored, ok := d.SessionDetail("sess-path")
	if !ok {
		t.Fatal("stored detail missing after accept")
	}
	if stored.ControlTokenPath != stated {
		t.Fatalf("stored ControlTokenPath = %q, want the daemon's stated %q", stored.ControlTokenPath, stated)
	}

	// Credential-free read: redacted shape, no stated path anywhere.
	status, redacted := getSessionDetail(t, srv.Addr(), "sess-path", "")
	if status != http.StatusOK {
		t.Fatalf("unauthenticated GET = %d, want 200 with redacted fields", status)
	}
	if redacted.ControlTokenPath != "" {
		t.Errorf("redacted ControlTokenPath = %q, want empty", redacted.ControlTokenPath)
	}
	raw := rawSessionDetailBody(t, srv.Addr(), "sess-path", "")
	if strings.Contains(raw, stated) {
		t.Errorf("redacted body carries the stated token path:\n%s", raw)
	}

	// The session's own read credential receives the full detail with it.
	readTok, ok := d.sessionReadToken("sess-path")
	if !ok || readTok == "" {
		t.Fatal("no read credential minted for sess-path")
	}
	if status, own := getSessionDetail(t, srv.Addr(), "sess-path", readTok); status != http.StatusOK {
		t.Fatalf("session-credential GET = %d, want 200", status)
	} else if own.ControlTokenPath != stated {
		t.Errorf("session-credential ControlTokenPath = %q, want %q", own.ControlTokenPath, stated)
	}

	// The operator token receives it too: it is the operator's own host
	// topology, and the worker-equivalent operator read stays full.
	if status, full := getSessionDetail(t, srv.Addr(), "sess-path", testControlToken); status != http.StatusOK {
		t.Fatalf("operator GET = %d, want 200", status)
	} else if full.ControlTokenPath != stated {
		t.Errorf("operator ControlTokenPath = %q, want %q", full.ControlTokenPath, stated)
	}
	if raw := rawSessionDetailBody(t, srv.Addr(), "sess-path", testControlToken); !strings.Contains(raw, stated) {
		t.Errorf("operator body lost the stated token path:\n%s", raw)
	}

	// A spawned session's environment still carries neither the token
	// nor its path override: the path travels in the credentialed
	// detail read, never in env.
	for _, kv := range d.spawner.sessionEnv(SessionSpec{SessionID: "sess-path"}, nil) {
		if strings.HasPrefix(kv, "DONMAI_CONTROL_TOKEN=") || strings.HasPrefix(kv, "DONMAI_CONTROL_TOKEN_FILE=") {
			t.Errorf("spawn env carries operator control credential %q", kv)
		}
		if strings.Contains(kv, stated) {
			t.Errorf("spawn env carries the stated token path: %q", kv)
		}
	}
}

// TestSessionDetail_UnstatedTokenPathKeepsLegacyShape pins the
// mixed-version default: a daemon that states no path stores and serves
// an empty one, so older workers resolve the path from their own
// environment exactly as before.
func TestSessionDetail_UnstatedTokenPathKeepsLegacyShape(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	seedCredentialedSession(t, d, "sess-legacy")
	stored, ok := d.SessionDetail("sess-legacy")
	if !ok {
		t.Fatal("stored detail missing after accept")
	}
	if stored.ControlTokenPath != "" {
		t.Fatalf("stored ControlTokenPath = %q, want empty without a stated path", stored.ControlTokenPath)
	}
	if status, full := getSessionDetail(t, srv.Addr(), "sess-legacy", testControlToken); status != http.StatusOK {
		t.Fatalf("operator GET = %d, want 200", status)
	} else if full.ControlTokenPath != "" {
		t.Errorf("operator ControlTokenPath = %q, want empty without a stated path", full.ControlTokenPath)
	}
}
