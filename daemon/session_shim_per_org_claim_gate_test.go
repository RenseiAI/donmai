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
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/worker"
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
// diagnostics surface exactly as a refused founding leaves it; the fence
// itself is gone (a refused install rolls back), so what holds the scope is
// the retained refusal alone. Built on a lightweight daemon rather than the
// full two-scope install so no first-heartbeat fence is outstanding anywhere
// — the pending-fence half is TestPerOrgClaimGateLetsTheHealthyOrgClaim...'s
// remit, and the two must not cover for each other.
func TestPerOrgClaimGateHoldsTheRefusedOrgWhileTheHealthyOrgClaims(t *testing.T) {
	const healthy, stuck = "org-healthy-claim", "org-refused-claim"
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		ControllerID: "controller-refused-claim", AttestationCapabilities: RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		OrgID:                      healthy,
	}})
	if err := d.refreshSessionShimIdentity().attestationErr; err != nil {
		t.Fatalf("resolve test attestation: %v", err)
	}
	// Adoption and activation read complete so the host-wide gate is open
	// and only the per-org refusal can hold the refused scope. The daemon is
	// shim-enabled and Running — the posture a refused founding rolls back
	// to — so no lifecycle state holds the host-wide lane.
	d.shims.mu.Lock()
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.shims.mu.Unlock()
	d.setState(StateRunning)
	// Both scopes hold authority receipts with no fence outstanding — the
	// posture a refused founding rolls back to, minus the refusal.
	if err := d.retainSessionShimCredentialReceipts([]SessionShimScopeCredentialReceipt{
		{Scope: healthy, WorkerHostID: "stable-host-healthy", AdoptionRevision: "revision-healthy"},
		{Scope: stuck, WorkerHostID: "stable-host-stuck", AdoptionRevision: "revision-stuck"},
	}); err != nil {
		t.Fatalf("retain test receipts: %v", err)
	}
	d.retainSessionShimDurabilityRefusal(&SessionShimDurabilityRefused{
		Scope: stuck, Err: errors.New("first projected heartbeat refused"),
	})

	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("host-wide claim gate closed with no fence outstanding: %s", reason)
	}
	if suspended, _ := d.PollClaimGateForOrg()(stuck); !suspended {
		t.Fatalf("refused scope %q claims", stuck)
	} else if reason := refusedReasonForTest(t, d, stuck); reason == "" {
		t.Fatal("refused scope holds with no reason for the operator line")
	}
	if suspended, reason := d.PollClaimGateForOrg()(healthy); suspended {
		t.Fatalf("healthy scope %q does not claim beside a refused one: %s", healthy, reason)
	}
}

func refusedReasonForTest(t *testing.T, d *Daemon, scope string) string {
	t.Helper()
	_, reason := d.PollClaimGateForOrg()(scope)
	return reason
}

// TestPerOrgClaimGateStillSuspendsEveryScopeForHostWideConditions pins what
// did NOT move: an operator drain, an operator pause, and a recovering host
// keep the host-wide gate — and therefore every scope — closed, each under
// its own named reason. Capacity is deliberately NOT host-wide: a full host
// holds session items per organization while the poll request still goes out,
// so the non-session lanes keep flowing (see
// TestCapacityHoldKeepsNonSessionLanesFlowing).
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

// TestHostWideClaimGateNamesEveryNonRunningState pins the shim seam the
// per-org split must not widen: with the shim enabled, every state other
// than Running suspends the host-wide gate under its own named reason, as
// before. Without this a self-update (Updating) would turn a skipped poll
// into claim-then-NACK churn — the host binding work to itself and handing
// it straight back — and Starting/Stopped would poll with no rail serving.
func TestHostWideClaimGateNamesEveryNonRunningState(t *testing.T) {
	// A shim-enabled daemon in Running, with adoption and activation
	// complete: the only thing that may close the host-wide gate in this
	// test is the lifecycle state under the table.
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		ControllerID: "controller-lifecycle-table", AttestationCapabilities: RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		OrgID:                      "org-lifecycle-table",
	}})
	if err := d.refreshSessionShimIdentity().attestationErr; err != nil {
		t.Fatalf("resolve test attestation: %v", err)
	}
	d.shims.mu.Lock()
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.shims.mu.Unlock()

	want := map[State]string{
		StateStarting:   claimSuspendedReasonStarting,
		StateUpdating:   claimSuspendedReasonUpdating,
		StateStopped:    claimSuspendedReasonStopped,
		StateDraining:   claimSuspendedReasonDraining,
		StatePaused:     claimSuspendedReasonPaused,
		StateRecovering: claimSuspendedReasonRecovering,
	}
	for state, reason := range want {
		d.lifecycleMu.Lock()
		d.setState(state)
		d.lifecycleMu.Unlock()
		suspended, got := d.PollClaimGate()()
		if !suspended {
			t.Errorf("host-wide claim gate open in state %q", state)
			continue
		}
		if got != reason {
			t.Errorf("state %q reason = %q, want the named reason %q", state, got, reason)
		}
		// The host-wide closure fans out: every organization's per-org
		// gate inherits it, so no scope can claim around the lifecycle.
		for _, scope := range []string{"org-lifecycle-table", "org-unrelated"} {
			if held, _ := d.PollClaimGateForOrg()(scope); !held {
				t.Errorf("scope %q claims while the host is %q", scope, state)
			}
		}
	}
	d.lifecycleMu.Lock()
	d.setState(StateRunning)
	d.lifecycleMu.Unlock()
	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("host-wide claim gate closed in state running: %s", reason)
	}
}

// TestPerOrgClaimGateHoldsSessionItemsAtCapacity pins the capacity hold
// where it now lives: per organization, not host-wide. A host with no free
// slot keeps its host-wide gate OPEN — the poll request still goes out —
// while every scope's session items are held under the named capacity
// reason. Holding capacity host-wide would also starve the non-session
// lanes riding the same response, which use no session slot.
func TestPerOrgClaimGateHoldsSessionItemsAtCapacity(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)
	d := h.daemon

	ceiling := d.Config().Capacity.MaxConcurrentSessions
	if ceiling <= 0 {
		t.Fatalf("fixture capacity = %d, want a positive ceiling to fill", ceiling)
	}
	// The deterministic "no free slot" seam: shrink the ceiling to zero.
	// Occupancy (empty spawner, no adopted shims) then meets the ceiling,
	// which is exactly the boundary the hold enforces.
	if err := d.spawner.SetMaxConcurrentSessions(0); err != nil {
		t.Fatalf("shrink session ceiling: %v", err)
	}
	// The host-wide gate stays open at capacity: the request still goes out.
	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("host-wide claim gate closed at capacity: %s", reason)
	}
	// …while every scope's session items are held under the named reason.
	for _, scope := range []string{h.orgID, "org-unrelated"} {
		suspended, reason := d.PollClaimGateForOrg()(scope)
		if !suspended {
			t.Fatalf("scope %q claims with no free session slot", scope)
		} else if reason != "host is at capacity (0/0 sessions)" {
			t.Fatalf("capacity reason = %q, want the named capacity reason", reason)
		}
	}
}

// TestCapacityHoldKeepsNonSessionLanesFlowing is F1's regression pin: a poll
// at capacity still dispatches a kgExtractWork item while the work[] session
// item is held. Capacity only ever holds session items per organization —
// AcceptWork already refuses a full host — so closing the whole poll request
// at capacity would starve the kg-extraction lane (its own goroutines, its
// own bound, no session slot) on a host that sits at capacity most of the
// time. Revert proof: with the capacity check back in claimSuspended the
// request never goes out, so no kg item dispatches.
func TestCapacityHoldKeepsNonSessionLanesFlowing(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)
	d := h.daemon

	if err := d.spawner.SetMaxConcurrentSessions(0); err != nil {
		t.Fatalf("shrink session ceiling: %v", err)
	}

	kgItem := kgextract.KgExtractWorkItem{
		BatchJobID:      "batch:kg_extract:capacity-lane",
		WorkType:        kgextract.WorkTypeKGExtraction,
		ContractVersion: kgextract.KGExtractionContractVersion,
		OrgID:           h.orgID,
		ProjectID:       "proj-capacity",
		Observations:    []kgextract.Observation{{ID: "obs-capacity", Content: "hello"}},
	}
	kgRaw, err := json.Marshal(kgItem)
	if err != nil {
		t.Fatalf("marshal kg item: %v", err)
	}
	var pollCalls, nacks atomic.Int64
	kgRan := make(chan worker.BatchWorkItem, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			pollCalls.Add(1)
			// One response carrying both lanes: a session item the
			// capacity hold must skip, and a kg item it must run.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"work":[{"sessionId":"session-at-capacity","organizationId":"` +
				h.orgID + `","repository":"github.com/foo/capped","ref":"main"}],` +
				`"kgExtractWork":[` + string(kgRaw) + `]}`))
		case http.MethodPost:
			nacks.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected method", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var claimed []string
	poller := NewPollService(PollOptions{
		WorkerID: "worker-capacity-lanes", RuntimeJWT: "runtime-capacity-lanes",
		OrchestratorURL: srv.URL, HTTPClient: srv.Client(),
		ClaimSuspended:       d.PollClaimGate(),
		ClaimSuspendedForOrg: d.PollClaimGateForOrg(),
		OnWork: func(item PollWorkItem) error {
			mu.Lock()
			defer mu.Unlock()
			claimed = append(claimed, item.SessionID)
			return nil
		},
		OnKgExtractWork: func(_ context.Context, bi worker.BatchWorkItem) error {
			select {
			case kgRan <- bi:
			default:
			}
			return nil
		},
	})
	poller.pollOnce(context.Background())
	poller.kgWG.Wait()

	if got := pollCalls.Load(); got != 1 {
		t.Fatalf("poll endpoint hits = %d, want 1 (capacity must not close the request)", got)
	}
	mu.Lock()
	held := len(claimed) == 0
	mu.Unlock()
	if !held {
		t.Fatalf("claimed sessions = %v, want none (the session item is held at capacity)", claimed)
	}
	if got := nacks.Load(); got != 1 {
		t.Fatalf("NACK releases = %d, want 1 (the held session item handed back)", got)
	}
	select {
	case bi := <-kgRan:
		if bi.BatchJobID != kgItem.BatchJobID {
			t.Fatalf("kg batchJobId = %q, want %q", bi.BatchJobID, kgItem.BatchJobID)
		}
	default:
		t.Fatal("kg-extraction item never dispatched on a poll at capacity")
	}
}

// TestRefusedScopeAdmissionRefusedDirectly is F3's regression pin: with the
// shim enabled, AcceptWorkWithDetail refuses the refused scope whether or
// not the readiness latch is set, while the healthy scope is admitted. The
// poll-layer per-org fence alone leaves the direct admission path — the
// local daemon API, an embedder OnWork calling AcceptWork — open for the
// scope the issue says does not claim.
func TestRefusedScopeAdmissionRefusedDirectly(t *testing.T) {
	const healthy, refused = "org-healthy-direct", "org-refused-direct"
	d := New(Options{SessionShim: SessionShimConfig{
		EnableAdoption: true, RequireCredentialAttestation: true,
		ControllerID: "controller-refused-direct", AttestationCapabilities: RequiredSessionShimHostCapabilities(),
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		OrgID:                      healthy,
	}})
	if err := d.refreshSessionShimIdentity().attestationErr; err != nil {
		t.Fatalf("resolve test attestation: %v", err)
	}
	d.shims.mu.Lock()
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.shims.mu.Unlock()
	if err := d.retainSessionShimCredentialReceipts([]SessionShimScopeCredentialReceipt{
		{Scope: healthy, WorkerHostID: "stable-host-healthy", AdoptionRevision: "revision-healthy"},
		{Scope: refused, WorkerHostID: "stable-host-refused", AdoptionRevision: "revision-refused"},
	}); err != nil {
		t.Fatalf("retain test receipts: %v", err)
	}
	d.retainSessionShimDurabilityRefusal(&SessionShimDurabilityRefused{
		Scope: refused, Err: errors.New("first projected heartbeat refused"),
	})
	// No latch outstanding anywhere: the posture a refused founding rolls
	// back to. The fence under test must hold WITHOUT it.
	if d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("fixture raised the readiness latch; the fence must hold without it")
	}
	d.setState(StateRunning)
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "github.com/foo/bar"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
	})
	d.spawner.Resume()

	if _, err := d.AcceptWorkWithDetail(
		SessionSpec{SessionID: "refused-direct", OrganizationID: refused, Repository: "github.com/foo/bar"},
		nil,
	); err == nil {
		t.Fatalf("direct admission for refused scope %q succeeded with no latch set", refused)
	} else if got := err.Error(); !strings.Contains(got, "new-work claim suspended") || !strings.Contains(got, refused) {
		t.Fatalf("refused-scope admission error = %q, want the per-org refusal fence", got)
	}
	if _, err := d.AcceptWorkWithDetail(
		SessionSpec{SessionID: "healthy-direct", OrganizationID: healthy, Repository: "github.com/foo/bar"},
		nil,
	); err != nil {
		t.Fatalf("direct admission for healthy scope %q refused beside a refused one: %v", healthy, err)
	}
	t.Cleanup(func() {
		_ = d.spawner.ForceKillSession("healthy-direct")
	})
}
