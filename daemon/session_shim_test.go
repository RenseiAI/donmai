package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

func TestQuarantineCloneAndIncarnationEqualityPreserveGeneration(t *testing.T) {
	t.Parallel()
	original := SessionShimAdoptionBatch{Quarantined: []sessionshim.QuarantinedSession{{
		OrgID: "org", SessionID: "session", ShimID: "shim", ProcessEpoch: 3,
		ControllerGeneration: 7, ConsumesCapacity: true,
	}}}
	cloned := cloneSessionShimAdoptionBatch(original)
	if len(cloned.Quarantined) != 1 || cloned.Quarantined[0].ControllerGeneration != 7 {
		t.Fatalf("cloned quarantine = %+v, want generation 7", cloned.Quarantined)
	}
	cloned.Quarantined[0].ControllerGeneration = 8
	if original.Quarantined[0].ControllerGeneration != 7 {
		t.Fatal("mutating cloned quarantine changed original generation")
	}

	d := New(Options{SkipRegistration: true})
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(original.Quarantined[0])
	newer := original.Quarantined[0]
	newer.ControllerGeneration = 9
	d.upsertShimQuarantineLocked(newer)
	d.shims.mu.Unlock()
	got := d.QuarantinedSessions()
	if len(got) != 1 || got[0].ControllerGeneration != 9 {
		t.Fatalf("same-incarnation generation upsert = %+v, want one row at generation 9", got)
	}
}

func TestControllerIDResolvedOnceGeneratedOrExactOverrideAndRefusesAliases(t *testing.T) {
	t.Parallel()
	a := New(Options{SkipRegistration: true})
	b := New(Options{SkipRegistration: true})
	if a.ControllerID() == "" || b.ControllerID() == "" || a.ControllerID() == b.ControllerID() {
		t.Fatalf("generated controller ids = %q / %q, want non-empty distinct process correlations", a.ControllerID(), b.ControllerID())
	}
	if !strings.HasPrefix(a.ControllerID(), "ctl_") || len(a.ControllerID()) != len("ctl_")+64 {
		t.Fatalf("generated controller id %q is not a 256-bit opaque value", a.ControllerID())
	}
	before := a.ControllerID()
	a.mu.Lock()
	a.workerID = "worker-rotated"
	a.jwt = "not-a-real-token"
	a.mu.Unlock()
	if a.ControllerID() != before {
		t.Fatalf("controller id changed across runtime credential mutation: %q -> %q", before, a.ControllerID())
	}

	const override = "controller-exact-override"
	overridden := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{ControllerID: override, HostID: "stable-host"}})
	if overridden.ControllerID() != override || overridden.controllerIDErr != nil {
		t.Fatalf("override = %q err=%v, want exact %q", overridden.ControllerID(), overridden.controllerIDErr, override)
	}
	for name, cfg := range map[string]SessionShimConfig{
		"literal":     {ControllerID: "daemon"},
		"stable host": {ControllerID: "same", HostID: "same"},
		"whitespace":  {ControllerID: "   "},
		"padded":      {ControllerID: " controller "},
	} {
		bad := New(Options{SkipRegistration: true, SessionShim: cfg})
		if bad.controllerIDErr == nil {
			t.Errorf("%s alias was accepted as controller id", name)
		}
	}
	if err := overridden.validateControllerAlias(override, "worker registration id"); err == nil {
		t.Fatal("worker-registration alias was accepted")
	}
	standaloneHost, err := a.sessionShimHostID(context.Background(), "local")
	if err != nil || standaloneHost != "" {
		t.Fatalf("standalone host identity = %q, %v; want empty with no correlation fallback", standaloneHost, err)
	}
}

func TestAuthoritativeSnapshotCarrierConfigFailsClosedBeforeAdoption(t *testing.T) {
	t.Parallel()
	adopt := func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		return SessionShimAdoptionReceipt{}, nil
	}
	batch := func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{}, nil
	}
	published := func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		return nil, nil
	}
	for name, cfg := range map[string]SessionShimConfig{
		"no callbacks": {RequireAuthoritativeSnapshot: true},
		"no exact attestation": {
			RequireAuthoritativeSnapshot: true,
			OnAdoption:                   adopt, OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: batch, OnAdoptionPublished: published,
		},
		"no durable stream": {
			EnableAdoption: true, RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
			AttestationCapabilities: RequiredSessionShimHostCapabilities(),
			OnAdoption:              adopt, OnAdoptionBatch: batch, OnAdoptionPublished: published,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := New(Options{SkipRegistration: true, SessionShim: cfg})
			if err := d.adoptSessionShims(context.Background()); !errors.Is(err, ErrSessionShimCarrierConfig) {
				t.Fatalf("adoptSessionShims = %v, want ErrSessionShimCarrierConfig", err)
			}
		})
	}
}

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
				ControllerGeneration: 29,
				ProtocolMin:          9, ProtocolMax: 9,
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
	if got, _ := entry["controllerGeneration"].(float64); got != 29 {
		t.Errorf("quarantinedSessions[0].controllerGeneration = %v, want 29", entry["controllerGeneration"])
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
			{OrgID: "o", SessionID: "quarantined", ShimID: "shim-q", ProcessEpoch: 31, ControllerGeneration: 37, ConsumesCapacity: true},
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
			if covered.ShimID != "shim-q" || covered.ProcessEpoch != 31 || covered.ControllerGeneration != 37 {
				t.Errorf("quarantined fence correlation = %+v, want shim-q/processEpoch 31/generation 37", covered)
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
			SessionID            string `json:"sessionId"`
			ProcessEpoch         uint64 `json:"processEpoch"`
			ControllerGeneration uint64 `json:"controllerGeneration"`
			LastForwardedSeq     uint64 `json:"lastForwardedSeq"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rawFence, &wireFence); err != nil {
		t.Fatalf("unmarshal restart fence wire: %v", err)
	}
	foundWireCorrelation := false
	for _, covered := range wireFence.Sessions {
		if covered.SessionID == "quarantined" && covered.ProcessEpoch == 31 && covered.ControllerGeneration == 37 {
			foundWireCorrelation = true
		}
	}
	if !foundWireCorrelation {
		t.Fatalf("restart fence JSON omitted quarantined processEpoch/controllerGeneration: %s", rawFence)
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

type retryExactFenceStore struct {
	mu       sync.Mutex
	failed   bool
	requests map[string][][]byte
}

func (s *retryExactFenceStore) AcknowledgeExact(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
	orgID := request.Fence.Sessions[0].OrgID
	s.mu.Lock()
	if s.requests == nil {
		s.requests = make(map[string][][]byte)
	}
	s.requests[orgID] = append(s.requests[orgID], append([]byte(nil), request.RequestBytes...))
	if orgID == "org-beta" && !s.failed {
		s.failed = true
		s.mu.Unlock()
		return sessionshim.FenceAcknowledgement{}, errors.New("transient beta refusal")
	}
	s.mu.Unlock()
	return sessionshim.FenceAcknowledgement{
		RequestBytes:    append([]byte(nil), request.RequestBytes...),
		DurableRevision: "revision-" + orgID,
	}, nil
}

func TestGroupedRestartFenceRetryReusesExactRequestBytes(t *testing.T) {
	t.Parallel()

	store := &retryExactFenceStore{}
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
	if fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-retry"); !errors.Is(err, sessionshim.ErrFenceRequired) || len(fences) != 1 {
		t.Fatalf("first grouped fence = %+v, %v; want alpha ack plus beta refusal", fences, err)
	}
	if fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-retry"); err != nil || len(fences) != 2 {
		t.Fatalf("retry grouped fence = %+v, %v; want both acknowledgements", fences, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, orgID := range []string{"org-alpha", "org-beta"} {
		requests := store.requests[orgID]
		if len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
			t.Fatalf("%s retry request bytes changed: %q then %q", orgID, requests[0], requests[1])
		}
	}
}

func TestGroupedRestartFenceRefusesUnknownOrganizationBeforeStore(t *testing.T) {
	t.Parallel()

	store := &exactFenceRecorder{}
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			ExactFenceStore: store,
			HostIDForOrg: func(_ context.Context, orgID string) (string, error) {
				return "host-" + orgID, nil
			},
		},
	})
	seedShimState(d, nil, []sessionshim.QuarantinedSession{{
		SessionID:        "unknown-scope",
		Reason:           sessionshim.QuarantineRecordMalformed,
		ConsumesCapacity: true,
	}})
	fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-unknown-scope")
	if !errors.Is(err, sessionshim.ErrFenceRequired) || len(fences) != 0 {
		t.Fatalf("unknown-scope fences = %+v, %v; want fail-closed before acknowledgement", fences, err)
	}
	if requests := store.snapshot(); len(requests) != 0 {
		t.Fatalf("exact store received %d requests for unknown organization scope", len(requests))
	}
}

func TestComposingRestartFenceRefusesControllerAsStableHostFallback(t *testing.T) {
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

	fence, err := d.RequestSessionShimRestartFence(context.Background(), "fence-no-host-alias")
	if !errors.Is(err, sessionshim.ErrFenceRequired) {
		t.Fatalf("RequestSessionShimRestartFence = %+v, %v; want ErrFenceRequired", fence, err)
	}
	requests := store.snapshot()
	if len(requests) != 0 {
		t.Fatalf("store received %d requests despite missing stable host identity", len(requests))
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
	if got := d.stopSessionShimByID("same-session"); got != sessionShimStopRefused {
		t.Fatalf("ambiguous bare session stop = %d, want explicit refusal", got)
	}
	if got := len(d.AdoptedSessionShims()); got != 2 {
		t.Fatalf("ambiguous stop changed adopted set length to %d, want 2", got)
	}
}

func TestStopSessionDoesNotFallThroughAfterAmbiguousShimRefusal(t *testing.T) {
	t.Parallel()

	const sessionID = "colliding-session"
	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "project", Repository: "https://example.invalid/org/repo"}},
		EnabledProjectIDs:     []string{"project"},
		MaxConcurrentSessions: 3,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
	})
	spawner.Resume()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = spawner.DrainContext(ctx)
	})
	if _, err := spawner.AcceptWork(SessionSpec{
		SessionID:  sessionID,
		ProjectID:  "project",
		Repository: "https://example.invalid/org/repo",
	}); err != nil {
		t.Fatalf("AcceptWork direct collision: %v", err)
	}
	d := New(Options{SkipRegistration: true})
	d.spawner = spawner
	seedShimState(d, []sessionshim.Identity{
		{OrgID: "org-alpha", SessionID: sessionID},
		{OrgID: "org-beta", SessionID: sessionID},
	}, nil)

	if d.StopSession(sessionID) {
		t.Fatal("public StopSession reported success for an ambiguous shim identity")
	}
	spawner.mu.Lock()
	direct := spawner.sessions[sessionID]
	if direct == nil {
		spawner.mu.Unlock()
		t.Fatal("ambiguous shim refusal fell through and released the colliding direct child")
	}
	stopRequested := direct.stopRequested
	owner := direct.groupTerminationOwner
	spawner.mu.Unlock()
	if stopRequested || owner != groupTerminationOpen {
		t.Fatalf("colliding direct child was mutated after shim ambiguity: stop=%v owner=%d", stopRequested, owner)
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

func TestStartupReportsEveryDuplicateTombstoneIncarnation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-terminal-duplicates", SessionID: "session-terminal-duplicates"}
	first := sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-a", ProcessEpoch: 1, GroupReaped: true,
		ObservedAtUnixNano: time.Now().UnixNano(),
	}
	second := first
	second.ShimID = "shim-b"
	second.ProcessEpoch = 2
	second.ObservedAtUnixNano++
	if err := registry.PutTombstone(first); err != nil {
		t.Fatalf("PutTombstone first: %v", err)
	}
	if err := registry.PutTombstone(second); err != nil {
		t.Fatalf("PutTombstone second: %v", err)
	}
	var received []string
	batchCalled := false
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:      true,
			RegistryDir:         dir,
			HostID:              "host-terminal-duplicates",
			AdoptionBatchOrgIDs: []string{id.OrgID},
			OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
				received = append(received, fmt.Sprintf("%s/%d", evidence.ShimID, evidence.ProcessEpoch))
				return nil
			},
			PrepareAdoptionBatch: func(_ context.Context, orgID, hostID string) ([]byte, error) {
				if orgID != id.OrgID || hostID != "host-terminal-duplicates" {
					return nil, fmt.Errorf("batch scope = %s/%s", orgID, hostID)
				}
				return []byte("expected-7"), nil
			},
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				if len(received) != 2 {
					return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("batch ran before per-session terminal callbacks: %v", received)
				}
				if string(batch.ExpectedRevision) != "expected-7" || len(batch.Tombstoned) != 2 ||
					len(batch.Adopted) != 0 || len(batch.Quarantined) != 0 {
					return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("incomplete batch: %+v", batch)
				}
				batchCalled = true
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("revision-8")}, nil
			},
		},
	})
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}
	if !slices.Equal(received, []string{"shim-a/1", "shim-b/2"}) {
		t.Fatalf("reported terminal incarnations = %v, want both exact proofs", received)
	}
	if !batchCalled {
		t.Fatal("per-organization adoption batch was not published before completion")
	}
	if receipt, ok := d.SessionShimAdoptionBatchReceipt(id.OrgID); !ok || string(receipt.DurableCorrelation) != "revision-8" {
		t.Fatalf("retained adoption batch receipt = %+v/%v", receipt, ok)
	}
	if got := d.SessionShimTerminalProof(id.OrgID, id.SessionID); len(got.Correlations) != 2 {
		t.Fatalf("retained terminal proof correlations = %+v, want 2", got)
	}
	if tombstones, err := registry.ScanTombstones(); err != nil || len(tombstones) != 0 {
		t.Fatalf("durably reported tombstones remain on disk: %+v, %v", tombstones, err)
	}
}

func TestAdoptionBatchRequiresExpectedAndDurableRevisionsBeforeReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		expected       []byte
		durableReceipt []byte
	}{
		{name: "empty expected revision", durableReceipt: []byte("revision-8")},
		{name: "empty durable receipt", expected: []byte("revision-7")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := New(Options{
				SkipRegistration: true,
				SessionShim: SessionShimConfig{
					EnableAdoption:      true,
					RegistryDir:         t.TempDir(),
					HostID:              "host-batch-refusal",
					AdoptionBatchOrgIDs: []string{"org-batch-refusal"},
					PrepareAdoptionBatch: func(context.Context, string, string) ([]byte, error) {
						return append([]byte(nil), tc.expected...), nil
					},
					OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
						return SessionShimAdoptionBatchReceipt{
							DurableCorrelation: append([]byte(nil), tc.durableReceipt...),
						}, nil
					},
				},
			})
			err := d.adoptSessionShims(context.Background())
			if err == nil {
				t.Fatal("adoption completed without both expected and durable batch revisions")
			}
			if d.SessionShimAdoptionComplete() {
				t.Fatal("adoptionComplete became true after empty batch revision")
			}
			if _, ok := d.SessionShimAdoptionBatchReceipt("org-batch-refusal"); ok {
				t.Fatal("empty batch revision was retained as a durable receipt")
			}
		})
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
