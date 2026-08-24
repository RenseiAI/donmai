package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHostStatusDetail_SuspendsClaiming pins the three-way distinction the
// claim gate depends on: absent status and healthy status both claim; only a
// pool_* status stops claiming. An unrecognised status from a newer control
// plane must NOT take a working daemon offline.
func TestHostStatusDetail_SuspendsClaiming(t *testing.T) {
	tests := []struct {
		name   string
		status *HostStatusDetail
		want   bool
	}{
		{name: "no status observed yet", status: nil, want: false},
		{name: "empty status string", status: &HostStatusDetail{}, want: false},
		{name: "ok", status: &HostStatusDetail{Status: "ok"}, want: false},
		{name: "ok with padding", status: &HostStatusDetail{Status: " OK "}, want: false},
		{name: "pool deleted", status: &HostStatusDetail{Status: "pool_deleted"}, want: true},
		{name: "pool draining", status: &HostStatusDetail{Status: "pool_draining"}, want: true},
		{name: "pool disabled", status: &HostStatusDetail{Status: "pool_disabled"}, want: true},
		{name: "future pool state", status: &HostStatusDetail{Status: "pool_paused"}, want: true},
		{name: "unauthorized has its own recovery rail", status: &HostStatusDetail{Status: "unauthorized"}, want: false},
		{name: "unknown status claims normally", status: &HostStatusDetail{Status: "something_new"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.SuspendsClaiming(); got != tt.want {
				t.Errorf("SuspendsClaiming() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDaemonClaimSuspended covers the daemon-side callback the poll loop
// consults, including the reason string it hands the transition log.
func TestDaemonClaimSuspended(t *testing.T) {
	tests := []struct {
		name         string
		status       *HostStatusDetail
		wantBlocked  bool
		wantContains []string
	}{
		{name: "no host status received", status: nil, wantBlocked: false},
		{name: "ok", status: &HostStatusDetail{Status: "ok"}, wantBlocked: false},
		{
			name: "pool deleted carries the recommended action",
			status: &HostStatusDetail{
				Status:            "pool_deleted",
				RecommendedAction: "re-bind this host to another pool",
			},
			wantBlocked:  true,
			wantContains: []string{"pool_deleted", "re-bind this host to another pool"},
		},
		{
			name:         "pool disabled with no recommended action",
			status:       &HostStatusDetail{Status: "pool_disabled"},
			wantBlocked:  true,
			wantContains: []string{"pool_disabled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(Options{})
			if tt.status != nil {
				d.setLastHostStatus(*tt.status)
			}
			blocked, reason := d.PollClaimGate()()
			if blocked != tt.wantBlocked {
				t.Fatalf("claimSuspended() blocked = %v, want %v", blocked, tt.wantBlocked)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not contain %q", reason, want)
				}
			}
			if !blocked && reason != "" {
				t.Errorf("reason = %q, want empty when not suspended", reason)
			}
		})
	}
}

// countingPollServer stands up a poll endpoint that counts requests and always
// returns one work item, so any claim that is NOT suppressed is observable
// twice over: as an HTTP hit and as an OnWork dispatch.
func countingPollServer(t *testing.T, workerID string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/workers/"+workerID+"/poll") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{{
			SessionID:  "sess-gated",
			Repository: "github.com/foo/bar",
			Ref:        "main",
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestPollServiceHostStatusGate is the core claim-path assertion: a daemon
// whose host status says the pool is deleted / draining / disabled never
// reaches the poll endpoint, so no work can be claimed. A daemon that has
// received NO host status claims exactly as before.
func TestPollServiceHostStatusGate(t *testing.T) {
	tests := []struct {
		name          string
		status        *HostStatusDetail
		wantPollHits  int32
		wantDispatch  int
		wantSuspended bool
	}{
		{name: "no host status received claims normally", status: nil, wantPollHits: 1, wantDispatch: 1},
		{name: "ok claims normally", status: &HostStatusDetail{Status: "ok"}, wantPollHits: 1, wantDispatch: 1},
		{
			name:          "pool deleted suspends claiming",
			status:        &HostStatusDetail{Status: "pool_deleted"},
			wantSuspended: true,
		},
		{
			name:          "pool draining suspends claiming",
			status:        &HostStatusDetail{Status: "pool_draining"},
			wantSuspended: true,
		},
		{
			name:          "pool disabled suspends claiming",
			status:        &HostStatusDetail{Status: "pool_disabled"},
			wantSuspended: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hits := countingPollServer(t, "wkr_gate")

			d := New(Options{})
			if tt.status != nil {
				d.setLastHostStatus(*tt.status)
			}

			var mu sync.Mutex
			dispatched := 0
			p := NewPollService(PollOptions{
				WorkerID:        "wkr_gate",
				OrchestratorURL: srv.URL,
				RuntimeJWT:      "rt-jwt",
				IntervalSeconds: 1,
				OnWork: func(PollWorkItem) error {
					mu.Lock()
					defer mu.Unlock()
					dispatched++
					return nil
				},
				ClaimSuspended: d.PollClaimGate(),
			})

			p.pollOnce(context.Background())

			if got := hits.Load(); got != tt.wantPollHits {
				t.Errorf("poll endpoint hits = %d, want %d", got, tt.wantPollHits)
			}
			mu.Lock()
			got := dispatched
			mu.Unlock()
			if got != tt.wantDispatch {
				t.Errorf("OnWork dispatches = %d, want %d", got, tt.wantDispatch)
			}
			if p.ClaimsSuspended() != tt.wantSuspended {
				t.Errorf("ClaimsSuspended() = %v, want %v", p.ClaimsSuspended(), tt.wantSuspended)
			}
		})
	}
}

// TestPollServiceResumesClaimingWhenHostStatusReturnsToOK proves recovery is
// automatic — no restart, no operator step — and that the loop logs the
// suspend/resume TRANSITIONS once each rather than once per tick.
func TestPollServiceResumesClaimingWhenHostStatusReturnsToOK(t *testing.T) {
	srv, hits := countingPollServer(t, "wkr_resume")

	d := New(Options{})
	d.setLastHostStatus(HostStatusDetail{
		Status:            "pool_deleted",
		RecommendedAction: "re-bind this host to another pool",
	})

	var logMu sync.Mutex
	var lines []string
	record := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	countLines := func(substr string) int {
		logMu.Lock()
		defer logMu.Unlock()
		n := 0
		for _, l := range lines {
			if strings.Contains(l, substr) {
				n++
			}
		}
		return n
	}

	p := NewPollService(PollOptions{
		WorkerID:        "wkr_resume",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		LogInfo:         record,
		LogWarn:         record,
		OnWork:          func(PollWorkItem) error { return nil },
		ClaimSuspended:  d.PollClaimGate(),
	})

	// Several suspended ticks: no claims, exactly one suspend log line.
	for range 3 {
		p.pollOnce(context.Background())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("poll endpoint hits while suspended = %d, want 0", got)
	}
	if got := countLines("suspending new-work claims"); got != 1 {
		t.Errorf("suspend log lines = %d, want exactly 1 (no per-tick logging)", got)
	}

	// The control plane reports the host healthy again — no restart involved.
	d.setLastHostStatus(HostStatusDetail{Status: "ok"})
	p.pollOnce(context.Background())
	p.pollOnce(context.Background())

	if got := hits.Load(); got != 2 {
		t.Errorf("poll endpoint hits after resume = %d, want 2", got)
	}
	if p.ClaimsSuspended() {
		t.Error("ClaimsSuspended() still true after host status returned to ok")
	}
	if got := countLines("resuming new-work claims"); got != 1 {
		t.Errorf("resume log lines = %d, want exactly 1", got)
	}
}

// TestHostStatusSuspensionLeavesInFlightSessionRunning proves the gate is
// claim-side only: a session that was already accepted keeps running across
// the suspend transition and across the resume that follows. Nothing is
// killed, and the spawner keeps accepting.
func TestHostStatusSuspensionLeavesInFlightSessionRunning(t *testing.T) {
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "github.com/foo/bar"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
	})
	t.Cleanup(func() { _ = spawner.ForceKillSession("inflight-1") })

	d := New(Options{})
	d.spawner = spawner
	d.setState(StateRunning)

	handle, err := d.AcceptWork(SessionSpec{
		SessionID:  "inflight-1",
		Repository: "github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	if handle.PID == 0 {
		t.Fatal("accepted session has no pid")
	}

	// The pool this host is bound to is deleted mid-flight.
	d.setLastHostStatus(HostStatusDetail{
		Status:            "pool_deleted",
		RecommendedAction: "re-bind this host to another pool",
	})
	if blocked, _ := d.PollClaimGate()(); !blocked {
		t.Fatal("claim gate did not engage on pool_deleted")
	}

	// Drive the real poll path while the session runs: no new claim may
	// happen, and the running session must be untouched by the tick.
	srv, hits := countingPollServer(t, "wkr_inflight")
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_inflight",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt-jwt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		ClaimSuspended:  d.PollClaimGate(),
	})
	p.pollOnce(context.Background())
	if got := hits.Load(); got != 0 {
		t.Errorf("poll endpoint hits while suspended = %d, want 0", got)
	}

	// Give any (incorrect) teardown a window to happen.
	time.Sleep(150 * time.Millisecond)

	active := d.ActiveSessions()
	if len(active) != 1 {
		t.Fatalf("active sessions after suspension = %d, want 1 (in-flight work must survive)", len(active))
	}
	if active[0].SessionID != "inflight-1" {
		t.Errorf("active session id = %q, want inflight-1", active[0].SessionID)
	}
	if active[0].State != SessionStarting && active[0].State != SessionRunning {
		t.Errorf("in-flight session state = %q, want starting or running", active[0].State)
	}
	if active[0].PID != handle.PID {
		t.Errorf("in-flight session pid changed: %d -> %d", handle.PID, active[0].PID)
	}
	if !spawner.IsAccepting() {
		t.Error("spawner stopped accepting — suspension must gate claims, not the spawner")
	}
	if d.State() != StateRunning {
		t.Errorf("daemon state = %q, want running (suspension is not a lifecycle transition)", d.State())
	}

	// And the same session survives the recovery transition.
	d.setLastHostStatus(HostStatusDetail{Status: "ok"})
	if blocked, _ := d.PollClaimGate()(); blocked {
		t.Fatal("claim gate still engaged after host status returned to ok")
	}
	if got := len(d.ActiveSessions()); got != 1 {
		t.Fatalf("active sessions after resume = %d, want 1", got)
	}
}
