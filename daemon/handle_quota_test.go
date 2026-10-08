package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// newQuotaTestServer hosts the production control stack
// (gateHandler → register → handleSessionSubrouteAuth) against a
// daemon with a live quota cache. Workers reach the usage route
// through this exact dispatch, including the control gate and the
// usage leaf's session-scoped auth — a bare handler would prove
// nothing about the credential path.
func newQuotaTestServer(t *testing.T, d *Daemon) *httptest.Server {
	t.Helper()
	s := NewServer(d)
	srv := httptest.NewServer(s.httpd.Handler)
	t.Cleanup(srv.Close)
	return srv
}

// postSessionUsage POSTs a quota update to the production usage route
// with bearer attached when non-empty, and returns the status code.
func postSessionUsage(t *testing.T, srv *httptest.Server, sessionID, bearer string, harness string, update agent.UsageLimitsUpdate) int {
	t.Helper()
	return postSessionUsageAt(t, strings.TrimPrefix(srv.URL, "http://"), sessionID, bearer, harness, update)
}

// postSessionUsageAt is postSessionUsage against a live daemon server
// address (the mustStartDaemon* fixtures return the daemon server,
// not an httptest one).
func postSessionUsageAt(t *testing.T, addr, sessionID, bearer string, harness string, update agent.UsageLimitsUpdate) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{"harness": harness, "update": update})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/sessions/"+sessionID+"/usage", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build usage request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post usage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// quotaUpdateWindows is one sparse worker-observed update: the primary
// window moves while every other probed row stays untouched.
func quotaUpdateWindows(checkedAt string, primary float64) agent.UsageLimitsUpdate {
	return agent.UsageLimitsUpdate{
		CheckedAt: checkedAt,
		Windows:   []agent.UsageWindow{{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: primary}},
	}
}

// heartbeatQuotaAfterSendNow composes the next beat through the
// production heartbeat service and returns its quota field: what the
// platform would receive on the following POST. It fails the test
// when no beat composes.
func heartbeatQuotaAfterSendNow(t *testing.T, d *Daemon) []agent.UsageAccount {
	t.Helper()
	if d.heartbeat == nil {
		t.Fatal("heartbeat service not running; the quota field has no ride")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = d.heartbeat.SendNow(ctx)
	return d.heartbeat.LastPayload().Quota
}

// TestSessionUsageRoute_MergesStreamUpdate is the end-to-end proof
// for the stream half of the wiring: a worker POSTs the sparse
// windows its harness stream carried to the production usage route,
// and the heartbeat snapshot carries the merged rows beside the
// probe's verdict. The probe seeded the snapshot from the recorded
// codex fixture; the update moves the primary row.
func TestSessionUsageRoute_MergesStreamUpdate(t *testing.T) {
	t.Parallel()

	d := New(Options{})
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	plan, limits := quotaPollerFixtureCodex(t, base)
	d.quota.noteCodexProbe("fixture-codex-account", plan, limits, base)
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-quota-1"})

	srv := newQuotaTestServer(t, d)
	// Open-mode daemon (no control gate): the credential-free worker
	// update the original wiring relied on still merges.
	if got := postSessionUsage(t, srv, "sess-quota-1", "", agent.UsageHarnessCodex, quotaUpdateWindows(agent.ISOTime(base.Add(time.Minute)), 77)); got != http.StatusOK {
		t.Fatalf("post usage status = %d, want 200", got)
	}

	snapshot := d.quotaSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v, want the one probed codex account", snapshot)
	}
	acct := snapshot[0]
	if len(acct.Limits.Windows) != 2 {
		t.Fatalf("windows = %+v, want the probe's two rows with the update merged", acct.Limits.Windows)
	}
	byID := map[string]agent.UsageWindow{}
	for _, w := range acct.Limits.Windows {
		byID[w.ID] = w
	}
	if byID["primary"].UsedPercent != 77 {
		t.Errorf("primary = %+v, want the streamed 77", byID["primary"])
	}
	if byID["secondary"].UsedPercent != 1 {
		t.Errorf("secondary = %+v, want the probe's 1 kept", byID["secondary"])
	}
	// A turn-driven update never refreshes the login verdict: the
	// authCheck still stamps the probe time, ok:true.
	if acct.AuthCheck == nil || !acct.AuthCheck.OK || acct.AuthCheck.CheckedAt != agent.ISOTime(base) {
		t.Errorf("authCheck = %+v, want the probe's ok:true verdict untouched", acct.AuthCheck)
	}
}

// TestSessionUsageRoute_RejectsUnknownSessionsAndHarnesses pins the
// route's guards: unknown session ids 404 so a stray worker cannot
// invent quota state, and an unknown harness 400s.
func TestSessionUsageRoute_RejectsUnknownSessionsAndHarnesses(t *testing.T) {
	t.Parallel()

	d := New(Options{})
	srv := newQuotaTestServer(t, d)
	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if got := post("/api/daemon/sessions/no-such-session/usage", `{"harness":"codex","update":{"checkedAt":"2026-10-06T12:00:00Z","windows":[]}}`); got != http.StatusNotFound {
		t.Errorf("unknown session status = %d, want 404", got)
	}
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-quota-2"})
	if got := post("/api/daemon/sessions/sess-quota-2/usage", `{"harness":"mystery","update":{"checkedAt":"2026-10-06T12:00:00Z","windows":[]}}`); got != http.StatusBadRequest {
		t.Errorf("unknown harness status = %d, want 400", got)
	}
	if got := post("/api/daemon/sessions/sess-quota-2/usage", `not json`); got != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", got)
	}
}

// TestSessionUsageRoute_SessionCredentialAcceptedUnderEnforcedGate is
// the live-update proof: with the control gate enforced, a worker
// POSTing its streamed windows with its own session read credential —
// the credential the spawner states in its environment, never the
// operator control token — is accepted, and the streamed rows land in
// the next composed heartbeat beside the probe snapshot. The operator
// control token keeps working on the same route.
func TestSessionUsageRoute_SessionCredentialAcceptedUnderEnforcedGate(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	plan, limits := quotaPollerFixtureCodex(t, base)
	d.quota.noteCodexProbe("fixture-codex-account", plan, limits, base)
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-live-1"})
	readTok, ok := d.sessionReadToken("sess-live-1")
	if !ok || readTok == "" {
		t.Fatal("no read credential minted for sess-live-1")
	}
	if readTok == testControlToken {
		t.Fatal("read credential must differ from the operator control token")
	}

	updateAt := agent.ISOTime(base.Add(time.Minute))
	if got := postSessionUsageAt(t, srv.Addr(), "sess-live-1", readTok, agent.UsageHarnessCodex, quotaUpdateWindows(updateAt, 77)); got != http.StatusOK {
		t.Fatalf("worker update with its session credential = %d, want 200", got)
	}
	// The operator token still opens the same route.
	if got := postSessionUsageAt(t, srv.Addr(), "sess-live-1", testControlToken, agent.UsageHarnessCodex, quotaUpdateWindows(updateAt, 78)); got != http.StatusOK {
		t.Fatalf("operator update = %d, want 200", got)
	}

	// The next composed beat carries the streamed rows merged onto the
	// probe snapshot: the primary row moved twice (77, then 78) while
	// the probe's secondary row and login verdict stand.
	quota := heartbeatQuotaAfterSendNow(t, d)
	if len(quota) != 1 {
		t.Fatalf("heartbeat quota = %+v, want the one probed codex account", quota)
	}
	byID := map[string]agent.UsageWindow{}
	for _, w := range quota[0].Limits.Windows {
		byID[w.ID] = w
	}
	if byID["primary"].UsedPercent != 78 {
		t.Errorf("primary = %+v, want the latest streamed 78", byID["primary"])
	}
	if byID["secondary"].UsedPercent != 1 {
		t.Errorf("secondary = %+v, want the probe's 1 kept", byID["secondary"])
	}
	if quota[0].AuthCheck == nil || !quota[0].AuthCheck.OK || quota[0].AuthCheck.CheckedAt != agent.ISOTime(base) {
		t.Errorf("authCheck = %+v, want the probe's ok:true verdict untouched", quota[0].AuthCheck)
	}
}

// TestSessionUsageRoute_RefusesForeignAndMissingCredentials pins the
// session scope: without a credential, with a wrong bearer, or with
// another session's read credential, the update is refused before any
// merge — and the refused POSTs leave the published snapshot exactly
// as the probe left it. An unknown session id 404s rather than 401s,
// so the gate never confirms or denies a session it cannot name.
func TestSessionUsageRoute_RefusesForeignAndMissingCredentials(t *testing.T) {
	d, srv, cleanup := mustStartDaemonWith(t, func(o *Options) {
		o.RequireControlToken = true
		o.ControlToken = testControlToken
	})
	defer cleanup()

	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	plan, limits := quotaPollerFixtureCodex(t, base)
	d.quota.noteCodexProbe("fixture-codex-account", plan, limits, base)
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-own"})
	d.sessionDetails.Set(&SessionDetail{SessionID: "sess-foreign"})
	ownTok, ok := d.sessionReadToken("sess-own")
	if !ok || ownTok == "" {
		t.Fatal("no read credential minted for sess-own")
	}
	foreignTok, ok := d.sessionReadToken("sess-foreign")
	if !ok || foreignTok == "" {
		t.Fatal("no read credential minted for sess-foreign")
	}

	update := quotaUpdateWindows(agent.ISOTime(base.Add(time.Minute)), 99)
	cases := []struct {
		name    string
		session string
		bearer  string
		want    int
	}{
		{"missing credential", "sess-own", "", http.StatusUnauthorized},
		{"wrong bearer", "sess-own", "wrong-bearer", http.StatusUnauthorized},
		{"foreign session credential", "sess-own", foreignTok, http.StatusUnauthorized},
		{"unknown session", "sess-missing", ownTok, http.StatusNotFound},
		{"unknown session without credential", "sess-missing", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		if got := postSessionUsageAt(t, srv.Addr(), tc.session, tc.bearer, agent.UsageHarnessCodex, update); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}

	// None of the refused POSTs merged: the snapshot still carries the
	// probe's rows with the login verdict stamped at the probe time.
	snapshot := d.quotaSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v, want the one probed codex account", snapshot)
	}
	for _, w := range snapshot[0].Limits.Windows {
		if w.ID == "primary" && w.UsedPercent == 99 {
			t.Errorf("refused update merged: primary = %+v", w)
		}
	}
	if snapshot[0].AuthCheck == nil || !snapshot[0].AuthCheck.OK || snapshot[0].AuthCheck.CheckedAt != agent.ISOTime(base) {
		t.Errorf("authCheck = %+v, want the probe's verdict untouched", snapshot[0].AuthCheck)
	}
}

// TestDaemon_QuotaProbesDefaultDisabled pins the seam default: a
// daemon built without probe binaries configures no live probes, so
// no test or embedder ever shells out to a real harness login. Only
// the production entry point resolves binaries.
func TestDaemon_QuotaProbesDefaultDisabled(t *testing.T) {
	t.Parallel()

	d := New(Options{})
	if probes := d.quotaProbes(); len(probes) != 0 {
		t.Errorf("default quota probes = %d, want none without configured binaries", len(probes))
	}
	dOpts := New(Options{QuotaProbeBinaries: map[string]string{
		agent.UsageHarnessCodex:  "/bin/false",
		agent.UsageHarnessClaude: "/bin/false",
	}})
	probes := dOpts.quotaProbes()
	if len(probes) != 2 {
		t.Fatalf("configured quota probes = %d, want codex and claude", len(probes))
	}
	for _, harness := range []string{agent.UsageHarnessCodex, agent.UsageHarnessClaude} {
		if probes[harness] == nil {
			t.Errorf("no probe for %s", harness)
		}
	}
}
