package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

func TestSessionShimAdoptionIsDisabledByDefault(t *testing.T) {
	t.Parallel()

	// §D11 step 1 of the ADR's own migration law: ship the protocol, the
	// registry, and registry INSPECTION with adoption OFF, so a release can be
	// rolled out and observed before it starts taking ownership of live
	// terminals. A daemon that never configures this must behave exactly as it
	// did before the package existed — including not creating a registry
	// directory on disk.
	dir := t.TempDir() + "/never-created"
	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{RegistryDir: dir},
	})

	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims with adoption disabled: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry directory was created with adoption disabled: %v", err)
	}
	if d.SessionShimOccupancy() != 0 {
		t.Fatalf("occupancy with adoption disabled = %d, want 0", d.SessionShimOccupancy())
	}
	if !d.SessionShimAdoptionComplete() {
		t.Fatal("adoption should read as complete when it is disabled; there is nothing left to discover")
	}
	if q := d.QuarantinedSessions(); q != nil {
		t.Fatalf("quarantine projection with adoption disabled = %+v, want nil", q)
	}
}

func TestSessionShimStartupRefusesAnUnsafeOrphanPolicy(t *testing.T) {
	t.Parallel()

	// §D8: a configuration whose orphan bound can outlast an external release
	// threshold is capable of DOUBLE EXECUTION. It must be refused at startup and
	// prevent session admission — discovering it at deadline time means
	// discovering it from the damage.
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    t.TempDir(),
			Orphan: sessionshim.OrphanPolicy{
				Deadline:                 10 * time.Minute,
				TerminationGrace:         5 * time.Second,
				PropagationMargin:        30 * time.Second,
				ExternalReleaseThreshold: time.Minute,
			},
		},
	})
	err := d.adoptSessionShims(context.Background())
	if !errors.Is(err, sessionshim.ErrOrphanPolicyUnsafe) {
		t.Fatalf("adoptSessionShims = %v, want ErrOrphanPolicyUnsafe", err)
	}
}

func TestSessionShimStartupAcceptsASafeOrphanPolicy(t *testing.T) {
	t.Parallel()

	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    t.TempDir(),
			Orphan: sessionshim.OrphanPolicy{
				Deadline:                 90 * time.Second,
				TerminationGrace:         5 * time.Second,
				PropagationMargin:        30 * time.Second,
				ExternalReleaseThreshold: 10 * time.Minute,
			},
		},
	})
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims with a safe policy: %v", err)
	}
	if !d.SessionShimAdoptionComplete() {
		t.Fatal("adoption did not complete against an empty registry")
	}
	if d.SessionShimOccupancy() != 0 {
		t.Fatalf("occupancy against an empty registry = %d, want 0", d.SessionShimOccupancy())
	}
}

// seedShimState injects an adoption outcome so capacity and reporting can be
// exercised without spawning real shim processes (those are covered end-to-end
// in the sessionshim acceptance suite).
func seedShimState(d *Daemon, adopted []sessionshim.Identity, quarantined []sessionshim.QuarantinedSession) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	for _, id := range adopted {
		// No live controller: capacity, fence coverage, and diagnostics read the
		// recorded shim id rather than the connection, which is precisely the
		// independence this seam exercises. End-to-end coverage with real
		// connections lives in the sessionshim acceptance suite.
		d.shims.adopted[id] = adoptedShim{shimID: "shim-" + id.SessionID}
	}
	d.shims.quarantined = quarantined
	d.shims.adoptionComplete = true
}

func TestOccupancyCountsAdoptedAndQuarantinedShims(t *testing.T) {
	t.Parallel()

	// §D7: a quarantined shim is occupied capacity. Its harness is running; this
	// daemon simply has no authority over it. Counting only what the spawner
	// launched would advertise a restarted daemon as idle while every pre-restart
	// terminal is still live.
	d := New(Options{SkipRegistration: true})
	seedShimState(d,
		[]sessionshim.Identity{
			{OrgID: "o", SessionID: "adopted-1"},
			{OrgID: "o", SessionID: "adopted-2"},
		},
		[]sessionshim.QuarantinedSession{
			{OrgID: "o", SessionID: "quarantined-1", Reason: sessionshim.QuarantineProtocolMismatch, ConsumesCapacity: true},
		},
	)

	if got := d.SessionShimOccupancy(); got != 3 {
		t.Fatalf("SessionShimOccupancy = %d, want 3 (2 adopted + 1 quarantined)", got)
	}

	active, interactive := d.spawnerActiveSessionCounts()
	if active != 3 {
		t.Fatalf("active occupancy = %d, want 3", active)
	}
	// Shim ownership is interactive-only in its first delivery (§D11), so adopted
	// shims count as interactive. Quarantined ones do not: this daemon could not
	// negotiate with them, and classifying their run mode would be a guess.
	if interactive != 2 {
		t.Fatalf("interactive occupancy = %d, want 2 (adopted only)", interactive)
	}
	if interactive > active {
		t.Fatalf("interactive %d exceeds active %d; the pair must stay coherent", interactive, active)
	}

	// The unclassed accessors must agree with the paired snapshot — two occupancy
	// answers that drift apart is how a host advertises capacity it does not have.
	if got := d.spawnerActiveCount(); got != active {
		t.Fatalf("spawnerActiveCount = %d, want %d", got, active)
	}
	if got := d.spawnerActiveInteractiveCount(); got != interactive {
		t.Fatalf("spawnerActiveInteractiveCount = %d, want %d", got, interactive)
	}
}

func TestRegistrationStatusWithholdsReadinessUntilAdoptionCompletes(t *testing.T) {
	t.Parallel()

	// §D4: adopt BEFORE advertising. Until the pass finishes, this daemon does
	// not know what is occupied, so "idle" would be a claim it cannot support.
	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{EnableAdoption: true, RegistryDir: t.TempDir()},
	})
	d.config = &Config{Capacity: CapacityConfig{MaxConcurrentSessions: 4}}
	d.setState(StateRunning)

	if got := d.RegistrationStatus(); got != RegistrationDraining {
		t.Fatalf("RegistrationStatus before adoption = %q, want %q", got, RegistrationDraining)
	}

	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}
	if got := d.RegistrationStatus(); got != RegistrationIdle {
		t.Fatalf("RegistrationStatus after adoption = %q, want %q", got, RegistrationIdle)
	}
}

func TestRegistrationStatusReportsBusyWhenShimsFillCapacity(t *testing.T) {
	t.Parallel()

	// Surviving shims alone can fill a host. A daemon that reported idle here
	// would claim work it has no room to run.
	d := New(Options{SkipRegistration: true})
	d.config = &Config{Capacity: CapacityConfig{MaxConcurrentSessions: 2}}
	d.setState(StateRunning)
	seedShimState(d,
		[]sessionshim.Identity{{OrgID: "o", SessionID: "a"}},
		[]sessionshim.QuarantinedSession{
			{OrgID: "o", SessionID: "q", Reason: sessionshim.QuarantineProtocolMismatch, ConsumesCapacity: true},
		},
	)
	if got := d.RegistrationStatus(); got != RegistrationBusy {
		t.Fatalf("RegistrationStatus with capacity filled by shims = %q, want %q", got, RegistrationBusy)
	}
}

func TestHeartbeatCarriesTheQuarantineProjection(t *testing.T) {
	t.Parallel()

	// §D7: quarantined shims are always present in heartbeat payloads until they
	// exit or are reconciled, and always marked consumesCapacity. A consumer that
	// heard about a quarantine once and then stopped hearing about it would
	// reasonably conclude it had cleared.
	var (
		mu   sync.Mutex
		body map[string]any
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits++
		_ = json.Unmarshal(buf, &body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:        "wkr_shim",
		Hostname:        "h",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "jwt",
		IntervalSeconds: 1,
		GetActiveCount:  func() int { return 1 },
		GetMaxCount:     func() int { return 4 },
		GetStatus:       func() RegistrationStatus { return RegistrationIdle },
		GetQuarantinedSessions: func() []sessionshim.QuarantinedSession {
			return []sessionshim.QuarantinedSession{{
				OrgID: "org-1", SessionID: "sess-1", ShimID: "shim-1", ProcessEpoch: 23,
				ProtocolMin: 9, ProtocolMax: 9,
				Reason:     sessionshim.QuarantineProtocolMismatch,
				AgeSeconds: 42, ConsumesCapacity: true,
			}}
		},
	})
	hs.Start()
	t.Cleanup(hs.Stop)

	waitForHeartbeat(t, &mu, &hits)

	mu.Lock()
	defer mu.Unlock()
	raw, ok := body["quarantinedSessions"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("body.quarantinedSessions = %v, want one entry", body["quarantinedSessions"])
	}
	entry, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("quarantinedSessions[0] = %v, want an object", raw[0])
	}
	if got, _ := entry["consumesCapacity"].(bool); !got {
		t.Error("quarantinedSessions[0].consumesCapacity = false; §D7 requires it to be visible capacity")
	}
	if got, _ := entry["reason"].(string); got != string(sessionshim.QuarantineProtocolMismatch) {
		t.Errorf("quarantinedSessions[0].reason = %q, want %q", got, sessionshim.QuarantineProtocolMismatch)
	}
	if got, _ := entry["sessionId"].(string); got != "sess-1" {
		t.Errorf("quarantinedSessions[0].sessionId = %q, want sess-1", got)
	}
	if got, _ := entry["processEpoch"].(float64); got != 23 {
		t.Errorf("quarantinedSessions[0].processEpoch = %v, want 23", entry["processEpoch"])
	}
	// The projection is bounded and secret-free by construction: it carries only
	// identity, correlation, protocol range, reason, and age.
	for _, forbidden := range []string{"token", "bearer", "credential", "env", "prompt", "output"} {
		if _, present := entry[forbidden]; present {
			t.Errorf("quarantine projection carries a %q field; the projection must stay secret-free", forbidden)
		}
	}
}

func TestHeartbeatOmitsTheQuarantineKeyWhenNothingIsQuarantined(t *testing.T) {
	t.Parallel()

	// An empty slice is omitted so a beat only carries the key when there is
	// something to report — an always-present empty array is noise a consumer
	// has to learn to ignore.
	var (
		mu   sync.Mutex
		body map[string]any
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits++
		_ = json.Unmarshal(buf, &body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	hs := NewHeartbeatService(HeartbeatOptions{
		WorkerID:               "wkr_clean",
		Hostname:               "h",
		OrchestratorURL:        srv.URL,
		RuntimeJWT:             "jwt",
		IntervalSeconds:        1,
		GetActiveCount:         func() int { return 0 },
		GetMaxCount:            func() int { return 4 },
		GetStatus:              func() RegistrationStatus { return RegistrationIdle },
		GetQuarantinedSessions: func() []sessionshim.QuarantinedSession { return nil },
	})
	hs.Start()
	t.Cleanup(hs.Stop)

	waitForHeartbeat(t, &mu, &hits)

	mu.Lock()
	defer mu.Unlock()
	if _, present := body["quarantinedSessions"]; present {
		t.Fatalf("body carries quarantinedSessions with nothing quarantined: %v", body["quarantinedSessions"])
	}
}

func waitForHeartbeat(t *testing.T, mu *sync.Mutex, hits *int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := *hits
		mu.Unlock()
		if got > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no heartbeat round-tripped")
}

func TestReleaseDecisionRoutesThroughOnePredicate(t *testing.T) {
	t.Parallel()

	// §D9's named top risk is "a release path forgets the restart fence", and a
	// per-reaper check recreates split-brain through whichever path is omitted.
	// This pins the daemon-level entry point to the same rule.
	d := New(Options{SkipRegistration: true})

	if got := d.SessionShimReleaseDecision("o", "s", sessionshim.TerminalProof{}); got != sessionshim.ReleaseReconcile {
		t.Fatalf("no proof, no fence = %q, want %q", got, sessionshim.ReleaseReconcile)
	}
	if got := d.SessionShimReleaseDecision("o", "s", sessionshim.TerminalProof{AdoptedReceipt: true}); got != sessionshim.ReleaseAllowed {
		t.Fatalf("with a terminal receipt = %q, want %q", got, sessionshim.ReleaseAllowed)
	}

	// A held fence covering the session suppresses release; its EXPIRY does not
	// grant one.
	now := time.Now()
	d.shims.mu.Lock()
	d.shims.fence = &sessionshim.Fence{
		FenceID:           "f1",
		State:             sessionshim.FenceHeld,
		Sessions:          []sessionshim.FencedSession{{OrgID: "o", SessionID: "s"}},
		HoldUntilUnixNano: now.Add(time.Hour).UnixNano(),
	}
	d.shims.mu.Unlock()
	if got := d.SessionShimReleaseDecision("o", "s", sessionshim.TerminalProof{}); got != sessionshim.ReleaseHeld {
		t.Fatalf("under a live fence = %q, want %q", got, sessionshim.ReleaseHeld)
	}

	d.shims.mu.Lock()
	d.shims.fence.HoldUntilUnixNano = now.Add(-time.Hour).UnixNano()
	d.shims.mu.Unlock()
	if got := d.SessionShimReleaseDecision("o", "s", sessionshim.TerminalProof{}); got != sessionshim.ReleaseReconcile {
		t.Fatalf("after fence expiry with no proof = %q, want %q — elapsed time is not proof of death",
			got, sessionshim.ReleaseReconcile)
	}
}

func TestRestartFenceCoversAdoptedAndQuarantinedSessions(t *testing.T) {
	t.Parallel()

	// §D9: the fence enumerates every adopted AND quarantined session, because
	// both kinds of harness are still running across the restart. Omitting the
	// quarantined ones would leave exactly the sessions this daemon cannot see
	// into unprotected.
	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{RestartBudget: time.Minute},
	})
	seedShimState(d,
		[]sessionshim.Identity{
			{OrgID: "o", SessionID: "z-adopted"},
			{OrgID: "o", SessionID: "a-adopted"},
		},
		[]sessionshim.QuarantinedSession{
			{OrgID: "o", SessionID: "quarantined", ShimID: "shim-q", ProcessEpoch: 31, ConsumesCapacity: true},
		},
	)
	d.shims.mu.Lock()
	d.shims.forwarded[sessionshim.Identity{OrgID: "o", SessionID: "a-adopted"}] = 17
	d.shims.forwarded[sessionshim.Identity{OrgID: "o", SessionID: "quarantined"}] = 29
	d.shims.forwarded[sessionshim.Identity{OrgID: "o", SessionID: "z-adopted"}] = 41
	d.shims.mu.Unlock()

	fence, err := d.RequestSessionShimRestartFence(context.Background(), "fence-1")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFence: %v", err)
	}
	if !fence.Covers(sessionshim.Identity{OrgID: "o", SessionID: "a-adopted"}) ||
		!fence.Covers(sessionshim.Identity{OrgID: "o", SessionID: "z-adopted"}) {
		t.Error("fence does not cover both adopted sessions")
	}
	if !fence.Covers(sessionshim.Identity{OrgID: "o", SessionID: "quarantined"}) {
		t.Error("fence does not cover the quarantined session; its harness is still running")
	}
	wantOrder := []string{"a-adopted", "quarantined", "z-adopted"}
	wantForwarded := []uint64{17, 29, 41}
	if len(fence.Sessions) != len(wantOrder) {
		t.Fatalf("covered session count = %d, want %d", len(fence.Sessions), len(wantOrder))
	}
	foundQuarantine := false
	for _, covered := range fence.Sessions {
		if covered.SessionID == "quarantined" {
			foundQuarantine = true
			if covered.ShimID != "shim-q" || covered.ProcessEpoch != 31 {
				t.Errorf("quarantined fence correlation = %+v, want shim-q/processEpoch 31", covered)
			}
		}
	}
	for i, covered := range fence.Sessions {
		if covered.SessionID != wantOrder[i] || covered.LastForwardedSeq != wantForwarded[i] {
			t.Errorf("fence Sessions[%d] = %s/seq=%d, want %s/seq=%d",
				i, covered.SessionID, covered.LastForwardedSeq, wantOrder[i], wantForwarded[i])
		}
	}
	if !foundQuarantine {
		t.Fatal("quarantined session missing from fence correlation set")
	}
	rawFence, err := json.Marshal(fence)
	if err != nil {
		t.Fatalf("marshal restart fence: %v", err)
	}
	var wireFence struct {
		Sessions []struct {
			SessionID        string `json:"sessionId"`
			ProcessEpoch     uint64 `json:"processEpoch"`
			LastForwardedSeq uint64 `json:"lastForwardedSeq"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rawFence, &wireFence); err != nil {
		t.Fatalf("unmarshal restart fence wire: %v", err)
	}
	foundWireEpoch := false
	for _, covered := range wireFence.Sessions {
		if covered.SessionID == "quarantined" && covered.ProcessEpoch == 31 {
			foundWireEpoch = true
		}
	}
	if !foundWireEpoch {
		t.Fatalf("restart fence JSON omitted quarantined processEpoch: %s", rawFence)
	}
	for i, covered := range wireFence.Sessions {
		if covered.SessionID != wantOrder[i] || covered.LastForwardedSeq != wantForwarded[i] {
			t.Fatalf("restart fence JSON Sessions[%d] = %s/seq=%d, want %s/seq=%d: %s",
				i, covered.SessionID, covered.LastForwardedSeq, wantOrder[i], wantForwarded[i], rawFence)
		}
	}
	if fence.State != sessionshim.FenceHeld {
		t.Errorf("fence state = %q, want %q", fence.State, sessionshim.FenceHeld)
	}
	// The hold window covers the restart budget plus the whole orphan bound.
	if got := fence.HoldUntil().Sub(fence.IssuedAt()); got <= time.Minute {
		t.Errorf("hold window = %s; it must exceed the restart budget by the orphan bound", got)
	}
}

func TestRestartFenceRefusalIsSurfacedToTheCaller(t *testing.T) {
	t.Parallel()

	// §D9: if no durable acknowledgement arrives, the update is REFUSED and the
	// old daemon keeps serving. Surfacing the error is what lets the caller do
	// that instead of restarting blind.
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			FenceStore: refusingFenceStore{},
		},
	})
	seedShimState(d, []sessionshim.Identity{{OrgID: "o", SessionID: "adopted"}}, nil)

	_, err := d.RequestSessionShimRestartFence(context.Background(), "fence-1")
	if !errors.Is(err, sessionshim.ErrFenceRequired) {
		t.Fatalf("RequestSessionShimRestartFence = %v, want ErrFenceRequired", err)
	}
}

func TestGroupedRestartFencePartialAcknowledgementStillRefusesRestartSafely(t *testing.T) {
	t.Parallel()

	store := &exactFenceRecorder{failOrg: "org-beta"}
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			ExactFenceStore: store,
			HostIDForOrg: func(_ context.Context, orgID string) (string, error) {
				return "host-" + orgID, nil
			},
		},
	})
	seedShimState(d, []sessionshim.Identity{
		{OrgID: "org-alpha", SessionID: "session-alpha"},
		{OrgID: "org-beta", SessionID: "session-beta"},
	}, nil)

	fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-partial")
	if !errors.Is(err, sessionshim.ErrFenceRequired) {
		t.Fatalf("RequestSessionShimRestartFences = %+v, %v; want ErrFenceRequired", fences, err)
	}
	if len(fences) != 1 || len(fences[0].Sessions) != 1 || fences[0].Sessions[0].OrgID != "org-alpha" {
		t.Fatalf("acknowledged prefix = %+v, want only org-alpha", fences)
	}
	if got := d.SessionShimReleaseDecision("org-alpha", "session-alpha", sessionshim.TerminalProof{}); got != sessionshim.ReleaseHeld {
		t.Fatalf("acknowledged org release decision = %q, want held", got)
	}
	if got := d.SessionShimReleaseDecision("org-beta", "session-beta", sessionshim.TerminalProof{}); got != sessionshim.ReleaseReconcile {
		t.Fatalf("unacknowledged org release decision = %q, want reconciliation refusal", got)
	}
}

func TestLegacySingleRestartFencePreservesOneRequestAndControllerFallback(t *testing.T) {
	t.Parallel()

	store := &exactFenceRecorder{}
	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{ExactFenceStore: store},
	})
	d.mu.Lock()
	d.workerID = "legacy-controller-id"
	d.mu.Unlock()
	seedShimState(d, []sessionshim.Identity{
		{OrgID: "org-alpha", SessionID: "session-alpha"},
		{OrgID: "org-beta", SessionID: "session-beta"},
	}, nil)

	fence, err := d.RequestSessionShimRestartFence(context.Background(), "fence-legacy-single")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFence: %v", err)
	}
	requests := store.snapshot()
	if len(requests) != 1 || len(fence.Sessions) != 2 {
		t.Fatalf("legacy single request = %d calls, fence %+v", len(requests), fence)
	}
	if requests[0].Fence.HostID != "legacy-controller-id" {
		t.Fatalf("legacy host fallback = %q, want pre-field controller behavior", requests[0].Fence.HostID)
	}
}

func TestGroupedRestartFenceRetainsDuplicateIdentityIncarnations(t *testing.T) {
	t.Parallel()

	d := New(Options{SkipRegistration: true})
	seedShimState(d, nil, []sessionshim.QuarantinedSession{
		{OrgID: "org-duplicate", SessionID: "session-duplicate", ShimID: "shim-a", ProcessEpoch: 1, ConsumesCapacity: true},
		{OrgID: "org-duplicate", SessionID: "session-duplicate", ShimID: "shim-b", ProcessEpoch: 2, ConsumesCapacity: true},
	})
	fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-duplicates")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFences: %v", err)
	}
	if len(fences) != 1 || len(fences[0].Sessions) != 2 {
		t.Fatalf("duplicate-identity fence = %+v, want both exact incarnations", fences)
	}
	if fences[0].Sessions[0].ShimID != "shim-a" || fences[0].Sessions[1].ShimID != "shim-b" {
		t.Fatalf("duplicate identity was collapsed or reordered ambiguously: %+v", fences[0].Sessions)
	}
}

type refusingFenceStore struct{}

func (refusingFenceStore) Acknowledge(context.Context, sessionshim.Fence) (sessionshim.Fence, error) {
	return sessionshim.Fence{}, errors.New("no durable acknowledgement")
}

func TestStopIsRefusedForSessionsThisDaemonDidNotAdopt(t *testing.T) {
	t.Parallel()

	// §D7: quarantine means no stop authority. Honouring that here is what keeps
	// "quarantine, not kill" true at the daemon boundary rather than only inside
	// the adoption pass.
	d := New(Options{SkipRegistration: true})
	seedShimState(d, nil, []sessionshim.QuarantinedSession{
		{OrgID: "o", SessionID: "quarantined", ConsumesCapacity: true},
	})
	if err := d.StopAdoptedSessionShim("o", "quarantined", "operator"); err == nil {
		t.Fatal("a quarantined session accepted a stop; quarantine grants no stop authority")
	}
	if err := d.StopAdoptedSessionShim("o", "never-seen", "operator"); err == nil {
		t.Fatal("an unknown session accepted a stop")
	}
}

func TestBareSessionStopRefusesAmbiguousOrganizations(t *testing.T) {
	t.Parallel()

	d := New(Options{SkipRegistration: true})
	seedShimState(d, []sessionshim.Identity{
		{OrgID: "org-alpha", SessionID: "same-session"},
		{OrgID: "org-beta", SessionID: "same-session"},
	}, nil)
	if d.stopSessionShimByID("same-session") {
		t.Fatal("bare session stop selected one of two organization-scoped identities")
	}
	if got := len(d.AdoptedSessionShims()); got != 2 {
		t.Fatalf("ambiguous stop changed adopted set length to %d, want 2", got)
	}
}

func TestReleaseAdoptedShimsClearsControlWithoutClaimingTermination(t *testing.T) {
	t.Parallel()

	// Shutting the daemon down drops sockets; it does not end sessions. After the
	// release the daemon holds no controllers and — importantly — reports
	// adoption as INCOMPLETE, so a subsequent readiness check cannot advertise
	// capacity it has not re-established.
	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{EnableAdoption: true, RegistryDir: t.TempDir()},
	})
	seedShimState(d, []sessionshim.Identity{{OrgID: "o", SessionID: "a"}}, nil)
	if len(d.AdoptedSessionShims()) != 1 {
		t.Fatalf("precondition: adopted = %d, want 1", len(d.AdoptedSessionShims()))
	}

	d.ReleaseAdoptedSessionShims()

	if len(d.AdoptedSessionShims()) != 0 {
		t.Fatalf("adopted after release = %d, want 0", len(d.AdoptedSessionShims()))
	}
	if got := d.QuarantinedSessions(); len(got) != 0 {
		t.Fatalf("intentional controller release manufactured quarantine: %+v", got)
	}
	if d.SessionShimAdoptionComplete() {
		t.Fatal("adoption still reads complete after releasing every controller")
	}
	// Releasing is emphatically NOT terminal evidence: nothing about dropping a
	// socket observed a harness stopping.
	if got := d.SessionShimReleaseDecision("o", "a", d.SessionShimTerminalProof("o", "a")); got != sessionshim.ReleaseReconcile {
		t.Fatalf("release decision after dropping controllers = %q, want %q", got, sessionshim.ReleaseReconcile)
	}
}

func TestTerminalProofOnlyAcceptsPositiveObservations(t *testing.T) {
	t.Parallel()

	// §D10: neither an absent record nor a dead PID is proof. Only an adopted
	// terminal receipt or a tombstone recording a PROVEN reap closes the loop.
	dir := t.TempDir()
	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{EnableAdoption: true, RegistryDir: dir},
	})
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}

	// Nothing on disk at all: no proof.
	if proof := d.SessionShimTerminalProof("o", "missing"); proof.Proves() {
		t.Fatal("an absent registry entry was treated as proof of death")
	}

	reg, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "o", SessionID: "ended"}

	// A tombstone that could NOT prove the reap is still not proof.
	unproven := sessionshim.Tombstone{
		SchemaVersion: 1, OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-1", GroupReaped: false, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := reg.PutTombstone(unproven); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	if proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID); proof.Proves() {
		t.Fatal("a tombstone without a proven reap was treated as proof")
	}

	// A tombstone recording a verified reap IS proof.
	proven := unproven
	proven.GroupReaped = true
	if err := reg.PutTombstone(proven); err != nil {
		t.Fatalf("PutTombstone proven: %v", err)
	}
	proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID)
	if !proof.Proves() {
		t.Fatal("a tombstone recording a verified reap was not accepted as proof")
	}
	if got := d.SessionShimReleaseDecision(id.OrgID, id.SessionID, proof); got != sessionshim.ReleaseAllowed {
		t.Fatalf("release with a proven tombstone = %q, want %q", got, sessionshim.ReleaseAllowed)
	}
}

func TestStartupTombstoneCallbackDoesNotFabricateLiveAdoption(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tombstone := sessionshim.Tombstone{
		SchemaVersion:      sessionshim.RecordSchemaVersion,
		OrgID:              "org-orphan",
		SessionID:          "session-orphan",
		ShimID:             "shim-orphan",
		ProcessEpoch:       12,
		GroupReaped:        true,
		ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(tombstone); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	var received SessionShimTerminalEvidence
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    dir,
			HostIDForOrg: func(_ context.Context, orgID string) (string, error) {
				return "host-" + orgID, nil
			},
			OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
				received = evidence
				return nil
			},
		},
	})
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}
	if received.Identity != tombstone.Identity() || received.HostID != "host-org-orphan" ||
		received.ShimID != tombstone.ShimID || received.ProcessEpoch != tombstone.ProcessEpoch {
		t.Fatalf("startup terminal evidence = %+v", received)
	}
	if received.Adoption != nil || len(received.DurableAdoptionCorrelation) != 0 {
		t.Fatalf("startup orphan tombstone fabricated live adoption: %+v", received)
	}
	if !d.SessionShimAdoptionComplete() {
		t.Fatal("startup did not complete after durable tombstone handoff")
	}
	if _, err := registry.GetTombstone(tombstone.Identity()); err == nil {
		t.Fatal("startup tombstone remained after durable terminal callback")
	}
}

func TestSessionShimConfigDefaultsResolveThroughTheStateSeam(t *testing.T) {
	t.Parallel()

	// No install-specific path is compiled in: an empty RegistryDir resolves
	// through the injected state-directory seam.
	d := New(Options{SkipRegistration: true})
	cfg := d.sessionShimConfig()
	if cfg.RegistryDir == "" {
		t.Fatal("RegistryDir did not resolve through the state-directory seam")
	}
	if cfg.Orphan.Deadline != sessionshim.DefaultOrphanDeadline {
		t.Fatalf("default orphan deadline = %s, want %s", cfg.Orphan.Deadline, sessionshim.DefaultOrphanDeadline)
	}
	if err := cfg.Orphan.Validate(); err != nil {
		t.Fatalf("the default orphan policy does not validate: %v", err)
	}
}
