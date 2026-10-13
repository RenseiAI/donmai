package daemon

// session_shim_per_org_claim_gate_test.go — the per-organization new-work
// claim gate.
//
// A host that serves more than one organization raises one first-heartbeat
// fence per scope. The fence for a scope whose heartbeat is still pending —
// or whose founding the platform refused — must hold only that scope's work
// items: every other ready organization keeps claiming on the same tick, and
// the host-wide gate stays open. These tests drive the real claim loop
// (PollService.pollOnce with the daemon's own gate callbacks) with two
// organizations, one stuck pending.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// twoOrgClaimFixture is a composition whose adoption publishes two scopes:
// the harness's primary scope, whose first heartbeat the harness acknowledges
// immediately, and a second scope no beat ever acknowledges, so its
// first-heartbeat fence stays pending while the host is otherwise healthy.
type twoOrgClaimFixture struct {
	harness   *compositionHarness
	healthy   string
	stuck     string
	pollCalls atomic.Int64
	nacks     atomic.Int64
	mu        sync.Mutex
	claimed   []PollWorkItem
}

// installTwoOrgClaimComposition starts the harness daemon and installs a
// composition serving a second scope beside the primary one. It returns with
// the primary scope acknowledged and healthy and the second scope stuck with
// its first-heartbeat fence pending.
func installTwoOrgClaimComposition(t *testing.T) *twoOrgClaimFixture {
	t.Helper()
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.start(ctx)

	const stuck = "org-second-claim"
	cfg := h.composedConfig(acceptingBatch)
	cfg.AdoptionBatchOrgIDs = []string{stuck}
	cfg.AcquireRecoveryScopes = func(
		context.Context, SessionShimHostAttestation, SessionShimScopeCredentialReceipt,
	) ([]SessionShimScopeCredentialReceipt, error) {
		return []SessionShimScopeCredentialReceipt{
			{Scope: stuck, WorkerHostID: "stable-host-second", AdoptionRevision: "revision-second-declared"},
		}, nil
	}
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("two-scope composition install: %v", err)
	}

	f := &twoOrgClaimFixture{harness: h, healthy: h.orgID, stuck: stuck}
	d := h.daemon
	if state := d.State(); state != StateRunning {
		t.Fatalf("daemon state after the two-scope install = %q, want %q", state, StateRunning)
	}
	d.shims.mu.RLock()
	pending := append([]string(nil), keysOfPendingHeartbeatAcks(d)...)
	d.shims.mu.RUnlock()
	if len(pending) != 1 || pending[0] != stuck {
		t.Fatalf("pending first-heartbeat fences = %v, want exactly [%s]", pending, stuck)
	}
	if !d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("the stuck scope's fence did not raise the readiness latch; the fixture proves nothing")
	}
	return f
}

func keysOfPendingHeartbeatAcks(d *Daemon) []string {
	out := make([]string, 0, len(d.shims.pendingHeartbeatAcks))
	for scope := range d.shims.pendingHeartbeatAcks {
		out = append(out, scope)
	}
	return out
}

// pollServer serves one work item per organization on every poll tick and
// counts NACK releases, so the test observes both halves: what this host
// claimed and what it handed back.
func (f *twoOrgClaimFixture) pollServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			f.pollCalls.Add(1)
			_ = json.NewEncoder(w).Encode(PollResponse{Work: []PollWorkItem{
				{SessionID: "session-healthy", OrganizationID: f.healthy, Repository: "github.com/foo/healthy", Ref: "main"},
				{SessionID: "session-stuck", OrganizationID: f.stuck, Repository: "github.com/foo/stuck", Ref: "main"},
			}})
		case r.Method == http.MethodPost:
			f.nacks.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected method", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pollOnce drives the real claim loop once: the daemon's host-wide gate
// decides whether the request goes out, the per-org gate decides per item.
func (f *twoOrgClaimFixture) pollOnce(t *testing.T, srv *httptest.Server) {
	t.Helper()
	poller := NewPollService(PollOptions{
		WorkerID: "worker-per-org-claim", RuntimeJWT: "runtime-per-org-claim",
		OrchestratorURL: srv.URL, HTTPClient: srv.Client(),
		ClaimSuspended:       f.harness.daemon.PollClaimGate(),
		ClaimSuspendedForOrg: f.harness.daemon.PollClaimGateForOrg(),
		OnWork: func(item PollWorkItem) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.claimed = append(f.claimed, item)
			return nil
		},
	})
	poller.pollOnce(context.Background())
}

func (f *twoOrgClaimFixture) claimedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.claimed))
	for _, item := range f.claimed {
		out = append(out, item.SessionID)
	}
	return out
}

// TestPerOrgClaimGateLetsTheHealthyOrgClaimWhileOneOrgIsStuckPending is the
// issue's core: with one scope's first heartbeat stuck pending, the host-wide
// gate stays open, the healthy scope claims through the real claim loop, and
// the stuck scope's item is held (and released) rather than dispatched.
func TestPerOrgClaimGateLetsTheHealthyOrgClaimWhileOneOrgIsStuckPending(t *testing.T) {
	f := installTwoOrgClaimComposition(t)
	d := f.harness.daemon

	// The host-wide gate is open: one stuck scope stalls nothing.
	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("host-wide claim gate closed behind one stuck scope: %s", reason)
	}
	// Per scope: the healthy scope claims, the stuck one does not.
	if suspended, reason := d.PollClaimGateForOrg()(f.healthy); suspended {
		t.Fatalf("healthy scope %q does not claim: %s", f.healthy, reason)
	}
	if suspended, _ := d.PollClaimGateForOrg()(f.stuck); !suspended {
		t.Fatalf("stuck scope %q claims while its first heartbeat is pending", f.stuck)
	}
	// Legacy items that name no organization are gated by the primary scope,
	// which is healthy here.
	if suspended, reason := d.PollClaimGateForOrg()(""); suspended {
		t.Fatalf("unscoped claim does not follow the primary scope: %s", reason)
	}

	srv := f.pollServer(t)
	f.pollOnce(t, srv)

	if got := f.pollCalls.Load(); got != 1 {
		t.Fatalf("poll endpoint hits = %d, want 1 (the host-wide gate held the request)", got)
	}
	if got := f.claimedSessions(); len(got) != 1 || got[0] != "session-healthy" {
		t.Fatalf("claimed sessions = %v, want exactly [session-healthy]", got)
	}
	if got := f.nacks.Load(); got != 1 {
		t.Fatalf("NACK releases = %d, want 1 (the stuck scope's item handed back)", got)
	}

	// The healthy scope's admission holds end to end: direct admission for a
	// session of the healthy scope is governed by the same per-org fence, so
	// it must not carry a withdrawal refusal.
	if suspended, reason := d.claimSuspendedForOrg(f.healthy); suspended {
		t.Fatalf("healthy scope admission closed: %s", reason)
	}
	if suspended, _ := d.claimSuspendedForOrg(f.stuck); !suspended {
		t.Fatalf("stuck scope admission open while its first heartbeat is pending: %s", f.stuck)
	}
}

// TestPerOrgClaimGateHoldsTheRefusedOrgWhileTheHealthyOrgClaims pins the
// other half: a scope whose founding the platform refused does not claim,
// while the healthy scope keeps claiming. The refusal is retained on the
// diagnostics surface exactly as a refused founding leaves it.
func TestPerOrgClaimGateHoldsTheRefusedOrgWhileTheHealthyOrgClaims(t *testing.T) {
	f := installTwoOrgClaimComposition(t)
	d := f.harness.daemon

	d.retainSessionShimDurabilityRefusal(&SessionShimDurabilityRefused{
		Scope: f.stuck, Err: errors.New("first projected heartbeat refused"),
	})

	if suspended, _ := d.PollClaimGateForOrg()(f.stuck); !suspended {
		t.Fatalf("refused scope %q claims", f.stuck)
	} else if reason := refusedReasonForTest(t, d, f.stuck); reason == "" {
		t.Fatal("refused scope holds with no reason for the operator line")
	}
	if suspended, reason := d.PollClaimGateForOrg()(f.healthy); suspended {
		t.Fatalf("healthy scope %q does not claim beside a refused one: %s", f.healthy, reason)
	}
	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("host-wide claim gate closed behind one refused scope: %s", reason)
	}

	srv := f.pollServer(t)
	f.pollOnce(t, srv)
	if got := f.claimedSessions(); len(got) != 1 || got[0] != "session-healthy" {
		t.Fatalf("claimed sessions = %v, want exactly [session-healthy]", got)
	}
}

func refusedReasonForTest(t *testing.T, d *Daemon, scope string) string {
	t.Helper()
	_, reason := d.PollClaimGateForOrg()(scope)
	return reason
}

// TestPerOrgClaimGateStillSuspendsEveryScopeForHostWideConditions pins what
// did NOT move: an operator drain, an operator pause, and a full host keep
// the host-wide gate — and therefore every scope — closed, each under its own
// named reason.
func TestPerOrgClaimGateStillSuspendsEveryScopeForHostWideConditions(t *testing.T) {
	f := installTwoOrgClaimComposition(t)
	d := f.harness.daemon

	d.lifecycleMu.Lock()
	d.setState(StateDraining)
	d.lifecycleMu.Unlock()
	if suspended, reason := d.PollClaimGate()(); !suspended {
		t.Fatal("host-wide claim gate open while draining")
	} else if reason != claimSuspendedReasonDraining {
		t.Fatalf("draining reason = %q, want the named draining reason %q", reason, claimSuspendedReasonDraining)
	}
	for _, scope := range []string{f.healthy, f.stuck} {
		if suspended, _ := d.PollClaimGateForOrg()(scope); !suspended {
			t.Fatalf("scope %q claims while the host drains", scope)
		}
	}

	d.lifecycleMu.Lock()
	d.setState(StatePaused)
	d.lifecycleMu.Unlock()
	if suspended, reason := d.PollClaimGate()(); !suspended {
		t.Fatal("host-wide claim gate open while paused")
	} else if reason != claimSuspendedReasonPaused {
		t.Fatalf("paused reason = %q, want the named paused reason %q", reason, claimSuspendedReasonPaused)
	}

	d.lifecycleMu.Lock()
	d.setState(StateRunning)
	d.lifecycleMu.Unlock()
	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("host-wide claim gate closed after resume: %s", reason)
	}

	// A recovering host that is NOT draining still claims for its healthy
	// scope through the host-wide gate; only a scope fence holds one scope.
	// (The per-scope fence is what TestPerOrgClaimGateLetsTheHealthyOrgClaim...
	// pins; here the point is resume reopened the host-wide lane.)
	srv := f.pollServer(t)
	before := f.pollCalls.Load()
	f.pollOnce(t, srv)
	if got := f.pollCalls.Load(); got != before+1 {
		t.Fatalf("poll endpoint hits = %d, want one more tick after resume", got)
	}
}

// TestPerOrgClaimGateNamesFullCapacity pins the third host-wide condition: a
// host with no free slot suspends every scope under the named capacity
// reason.
func TestPerOrgClaimGateNamesFullCapacity(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)
	d := h.daemon

	max := d.Config().Capacity.MaxConcurrentSessions
	if max <= 0 {
		t.Fatalf("fixture capacity = %d, want a positive ceiling to fill", max)
	}
	// The deterministic "no free slot" seam: shrink the ceiling to zero.
	// Occupancy (empty spawner, no adopted shims) then meets the ceiling,
	// which is exactly the boundary the gate enforces.
	if err := d.spawner.SetMaxConcurrentSessions(0); err != nil {
		t.Fatalf("shrink session ceiling: %v", err)
	}
	if suspended, reason := d.PollClaimGate()(); !suspended {
		t.Fatal("host-wide claim gate open with no free session slot")
	} else if reason != "host is at capacity (0/0 sessions)" {
		t.Fatalf("capacity reason = %q, want the named capacity reason", reason)
	}
}
