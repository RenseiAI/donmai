package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// newQuotaTestServer hosts the production session subroute
// (/api/daemon/sessions/ → handleSessionSubroute) against a daemon
// with a live quota cache. Workers reach the usage route through
// this exact dispatch.
func newQuotaTestServer(t *testing.T, d *Daemon) *httptest.Server {
	t.Helper()
	s := &Server{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/sessions/", s.handleSessionSubroute)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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
	update := agent.UsageLimitsUpdate{
		CheckedAt: agent.ISOTime(base.Add(time.Minute)),
		Windows:   []agent.UsageWindow{{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: 77}},
	}
	body, err := json.Marshal(map[string]any{"harness": agent.UsageHarnessCodex, "update": update})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	resp, err := http.Post(srv.URL+"/api/daemon/sessions/sess-quota-1/usage", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post usage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post usage status = %d, want 200", resp.StatusCode)
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
