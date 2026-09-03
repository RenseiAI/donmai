package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/sessionshim"
)

// readinessTestDaemon is a shim-enabled daemon with one retained scope, ready
// to produce heartbeat projections. resolve is the embedder's readiness
// resolver; calls counts how often it was actually consulted.
func readinessTestDaemon(
	t *testing.T,
	cfg SessionShimConfig,
	resolve func() (SessionShimCarrierProofV2Readiness, error),
) (*Daemon, *atomic.Int64) {
	t.Helper()
	attestation := activationTestAttestation()
	var calls atomic.Int64
	cfg.EnableAdoption = true
	cfg.RequireCredentialAttestation = true
	cfg.ControllerID = attestation.ControllerID
	cfg.AttestationCapabilities = attestation.Capabilities
	if cfg.OrgID == "" {
		cfg.OrgID = "org-readiness"
	}
	cfg.GetCarrierProofV2Readiness = func() (SessionShimCarrierProofV2Readiness, error) {
		calls.Add(1)
		return resolve()
	}
	d := New(Options{SessionShim: cfg})
	if err := d.retainSessionShimCredentialReceipts([]SessionShimScopeCredentialReceipt{{
		Scope: cfg.OrgID, WorkerHostID: "stable-host-readiness", AdoptionRevision: "31",
	}}); err != nil {
		t.Fatal(err)
	}
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	return d, &calls
}

func healthyReadiness() (SessionShimCarrierProofV2Readiness, error) {
	return testSessionShimProofV2Readiness()
}

// TestSessionShimReadinessSentinelClassification pins how each failure class is
// classified, which is the rule everything else in this file rests on. The
// classification is by sentinel and errors.Is, so rewording a message can never
// silently turn a definite refusal into a transient one.
func TestSessionShimReadinessSentinelClassification(t *testing.T) {
	incomplete, _ := testSessionShimProofV2Readiness()
	incomplete.RemainingValidityConsumeGate = false
	unreachable := func() (SessionShimCarrierProofV2Readiness, error) {
		return SessionShimCarrierProofV2Readiness{}, errors.New("carrier unreachable")
	}
	for name, tc := range map[string]struct {
		resolver       func() (SessionShimCarrierProofV2Readiness, error)
		establishFirst bool
		omitResolver   bool
		wantState      string
		wantBlocking   error
		wantWithdraw   bool
	}{
		"a resolver error on an established readiness is unknown and never withdraws": {
			resolver:       unreachable,
			establishFirst: true,
			wantState:      SessionShimReadinessUnknown,
		},
		"a resolver error before readiness was ever established fails closed": {
			resolver:     unreachable,
			wantState:    SessionShimReadinessUnknown,
			wantBlocking: ErrSessionShimReadinessUnavailable,
		},
		"incomplete facts are a definite not-ready": {
			resolver:     func() (SessionShimCarrierProofV2Readiness, error) { return incomplete, nil },
			wantState:    SessionShimReadinessNotReady,
			wantBlocking: ErrSessionShimReadinessRejected,
			wantWithdraw: true,
		},
		"a missing resolver is a permanent misconfiguration": {
			omitResolver: true,
			wantState:    SessionShimReadinessNotReady,
			wantBlocking: ErrSessionShimReadinessMisconfigured,
			wantWithdraw: true,
		},
		"established readiness carries no state at all": {
			resolver: healthyReadiness,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := tc.resolver
			var healthy atomic.Bool
			if tc.establishFirst {
				healthy.Store(true)
				resolver = func() (SessionShimCarrierProofV2Readiness, error) {
					if healthy.Load() {
						return testSessionShimProofV2Readiness()
					}
					return tc.resolver()
				}
			}
			d, _ := readinessTestDaemon(t, SessionShimConfig{}, resolver)
			if tc.omitResolver {
				d.opts.SessionShim.GetCarrierProofV2Readiness = nil
			}
			if tc.establishFirst {
				if _, err := d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID()); err != nil {
					t.Fatalf("establish readiness: %v", err)
				}
				healthy.Store(false)
			}
			sample := d.sessionShimReadinessWithin(sessionShimReadinessResolveNow)
			if sample.state != tc.wantState {
				t.Fatalf("state = %q, want %q", sample.state, tc.wantState)
			}
			if (sample.state == "") != (sample.reason == "") {
				t.Fatalf("state %q with reason %q: a non-ready state always carries a reason", sample.state, sample.reason)
			}
			if (sample.state == "") != sample.stateSince.IsZero() {
				t.Fatalf("state %q with observed-at %q", sample.state, sample.observedAt())
			}
			if tc.wantBlocking == nil {
				if sample.blocking != nil {
					t.Fatalf("blocking = %v, want nil", sample.blocking)
				}
			} else if !errors.Is(sample.blocking, tc.wantBlocking) {
				t.Fatalf("blocking = %v, want %v", sample.blocking, tc.wantBlocking)
			}
			_, projectionErr := d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID())
			if errors.Is(sample.blocking, ErrSessionShimReadinessMisconfigured) {
				if projectionErr == nil {
					t.Fatal("a misconfigured resolver produced a projection; it must fail closed")
				}
			} else if projectionErr != nil {
				t.Fatalf("heartbeat projection: %v", projectionErr)
			}
			if got := d.sessionShimReadinessWithdrawn.Load(); got != tc.wantWithdraw {
				t.Fatalf("withdrawn = %v, want %v", got, tc.wantWithdraw)
			}
		})
	}
}

// TestSessionShimTransientReadinessFailureWithdrawsAtNoSeam is the rule this
// change exists for. A readiness dependency that is merely unreachable must not
// drain a host at ANY of the seams that consult it.
func TestSessionShimTransientReadinessFailureWithdrawsAtNoSeam(t *testing.T) {
	ctx := context.Background()
	for name, seam := range map[string]func(*Daemon){
		"heartbeat-projection": func(d *Daemon) {
			_, _ = d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID())
		},
		"poll-claim-gate": func(d *Daemon) { _, _ = d.claimSuspended() },
		"admission":       func(d *Daemon) { _, _ = d.AcceptWork(SessionSpec{SessionID: "s-1"}) },
		"credential-refresh": func(d *Daemon) {
			_ = d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{
				SessionShim: &SessionShimCredentialReceipt{
					Enabled: true, State: SessionShimCredentialStateRecovering,
					WorkerHostID: "stable-host-readiness", AdoptionRevision: "31",
				},
			})
		},
		"adoption-prepare": func(d *Daemon) {
			_, _ = d.prepareSessionShimAdoption(ctx, "stable-host-readiness", sessionshim.AdoptionPreparation{})
		},
		"carrier-activation": func(d *Daemon) {
			_ = d.activatePublishedSessionShimCarriers(ctx, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var failing atomic.Bool
			d, _ := readinessTestDaemon(t, SessionShimConfig{}, func() (SessionShimCarrierProofV2Readiness, error) {
				if failing.Load() {
					return SessionShimCarrierProofV2Readiness{}, errors.New("readiness authority unavailable")
				}
				return testSessionShimProofV2Readiness()
			})
			// Establish readiness first: the rule is about not withdrawing an
			// established readiness, so there has to be one to withdraw.
			if _, err := d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID()); err != nil {
				t.Fatalf("establish readiness: %v", err)
			}
			failing.Store(true)
			seam(d)
			if d.sessionShimReadinessWithdrawn.Load() {
				t.Fatalf("a transient readiness failure withdrew readiness at the %s seam", name)
			}
		})
	}
}

// TestSessionShimDefiniteNotReadyIsNotMaskedByAWarmCache is the other half. The
// cadence throttles how often the resolver is consulted; it must not delay the
// withdrawal decision past the beat that observes it.
func TestSessionShimDefiniteNotReadyIsNotMaskedByAWarmCache(t *testing.T) {
	var refuse atomic.Bool
	d, calls := readinessTestDaemon(t, SessionShimConfig{}, func() (SessionShimCarrierProofV2Readiness, error) {
		ready, _ := testSessionShimProofV2Readiness()
		if refuse.Load() {
			ready.DurableCarrierProofV2Ready = false
		}
		return ready, nil
	})
	scope := d.sessionShimConfig().orgID()
	if _, err := d.SessionShimHeartbeatProjection(scope); err != nil {
		t.Fatalf("establish readiness: %v", err)
	}
	// Warm the cache the way a live host does — through the per-tick seams.
	for i := 0; i < 8; i++ {
		_, _ = d.claimSuspended()
		_, _ = d.AcceptWork(SessionSpec{SessionID: "warm"})
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls while the cache was warm = %d, want 1", got)
	}

	refuse.Store(true)
	projection, err := d.SessionShimHeartbeatProjection(scope)
	if err != nil {
		t.Fatalf("beat after a definite not-ready: %v", err)
	}
	if projection.ReadinessState != SessionShimReadinessNotReady {
		t.Fatalf("beat after a definite not-ready = %q, want %q", projection.ReadinessState, SessionShimReadinessNotReady)
	}
	if !d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("a definite not-ready was masked by the readiness cache")
	}
	if blocked, _ := d.claimSuspended(); !blocked {
		t.Fatal("the claim gate stayed open after a definite not-ready")
	}
}

// TestSessionShimReadinessStalenessBoundIsConfigurable pins the bound itself,
// not only the mechanism: the test's own bound is supplied, never derived from
// the constant under test, so raising the default cannot leave this green. The
// bound is measured from an ESTABLISHED readiness, so the host establishes one
// before the resolver goes away.
func TestSessionShimReadinessStalenessBoundIsConfigurable(t *testing.T) {
	if DefaultSessionShimReadinessStaleAfter != 10*time.Minute {
		t.Fatalf("default staleness bound = %s, want 10m", DefaultSessionShimReadinessStaleAfter)
	}
	const bound = 40 * time.Millisecond
	var failing atomic.Bool
	d, _ := readinessTestDaemon(t, SessionShimConfig{ReadinessStaleAfter: bound},
		func() (SessionShimCarrierProofV2Readiness, error) {
			if failing.Load() {
				return SessionShimCarrierProofV2Readiness{}, errors.New("readiness authority unavailable")
			}
			return testSessionShimProofV2Readiness()
		})
	if got := d.sessionShimReadinessStaleAfter(); got != bound {
		t.Fatalf("configured staleness bound = %s, want %s", got, bound)
	}
	if _, err := d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID()); err != nil {
		t.Fatalf("establish readiness: %v", err)
	}
	failing.Store(true)
	sample := d.sessionShimReadinessWithin(sessionShimReadinessResolveNow)
	if sample.state != SessionShimReadinessUnknown {
		t.Fatalf("first failure = %q, want unknown", sample.state)
	}
	if sample.blocking != nil {
		t.Fatalf("an unknown on an established readiness blocked: %v", sample.blocking)
	}
	if d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("an unknown readiness withdrew before the staleness bound elapsed")
	}
	time.Sleep(bound + 20*time.Millisecond)
	sample = d.sessionShimReadinessWithin(sessionShimReadinessResolveNow)
	if sample.state != SessionShimReadinessNotReady || sample.reason != SessionShimReadinessStaleReason {
		t.Fatalf("past the bound = state %q reason %q, want not-ready/%q",
			sample.state, sample.reason, SessionShimReadinessStaleReason)
	}
	if sample.blocking == nil {
		t.Fatal("a stale readiness did not become a blocking not-ready")
	}
	if _, err := d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID()); err != nil {
		t.Fatalf("beat after the staleness bound: %v", err)
	}
	if !d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("a stale readiness did not withdraw")
	}
}

// TestSessionShimReadinessResolvesOncePerCadence is the load pin: the number of
// resolver calls a host makes is a function of its beat, not of how many
// sessions it is running.
func TestSessionShimReadinessResolvesOncePerCadence(t *testing.T) {
	d, calls := readinessTestDaemon(t, SessionShimConfig{}, healthyReadiness)
	scope := d.sessionShimConfig().orgID()
	if _, err := d.SessionShimHeartbeatProjection(scope); err != nil {
		t.Fatalf("beat: %v", err)
	}
	const sessions = 5
	for i := 0; i < sessions; i++ {
		_, _ = d.AcceptWork(SessionSpec{SessionID: "session"})
		_, _ = d.claimSuspended()
		_, _ = d.prepareSessionShimAdoption(context.Background(), "stable-host-readiness", sessionshim.AdoptionPreparation{})
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls for one beat plus %d sessions = %d, want 1", sessions, got)
	}
}

// TestSessionShimReadinessResolutionIsSingleFlight pins the concurrent shape of
// the same rule: sixteen simultaneous consumers of a cold cache consult the
// embedder once, not sixteen times.
func TestSessionShimReadinessResolutionIsSingleFlight(t *testing.T) {
	release := make(chan struct{})
	d, calls := readinessTestDaemon(t, SessionShimConfig{}, func() (SessionShimCarrierProofV2Readiness, error) {
		<-release
		return testSessionShimProofV2Readiness()
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.sessionShimReadinessGate(sessionShimReadinessCadence)
		}()
	}
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls from 16 concurrent consumers = %d, want 1", got)
	}
}

func TestSessionShimReadinessRetryBackoff(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 5 * time.Second},
		{failures: 2, want: 10 * time.Second},
		{failures: 3, want: 20 * time.Second},
		{failures: 4, want: 30 * time.Second},
		{failures: 9, want: 30 * time.Second},
	} {
		if got := sessionShimReadinessRetryBackoff(tc.failures); got != tc.want {
			t.Fatalf("backoff after %d failures = %s, want %s", tc.failures, got, tc.want)
		}
	}
	// The backoff shortens the cadence rather than lengthening it, so a failing
	// resolver is retried sooner than a healthy one is re-read — and it resets
	// the moment an answer arrives.
	if sessionShimReadinessRetryBackoff(1) >= sessionShimReadinessCadence {
		t.Fatal("the first retry is not sooner than the healthy cadence")
	}
	var failing atomic.Bool
	failing.Store(true)
	d, calls := readinessTestDaemon(t, SessionShimConfig{}, func() (SessionShimCarrierProofV2Readiness, error) {
		if failing.Load() {
			return SessionShimCarrierProofV2Readiness{}, errors.New("readiness authority unavailable")
		}
		return testSessionShimProofV2Readiness()
	})
	for i := 0; i < 4; i++ {
		_ = d.sessionShimReadinessGate(sessionShimReadinessCadence)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls inside the retry backoff = %d, want 1", got)
	}
	failing.Store(false)
	if _, err := d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID()); err != nil {
		t.Fatalf("recovery beat: %v", err)
	}
	d.readinessMu.Lock()
	failures := d.readinessCache.failures
	d.readinessMu.Unlock()
	if failures != 0 {
		t.Fatalf("consecutive failures after recovery = %d, want 0", failures)
	}
}

// TestSessionShimHeartbeatProjectionWireShape pins the bytes. A healthy beat
// must be byte-identical to one produced before the tri-state existed, and a
// degraded beat must omit the five readiness facts rather than publish them as
// false — an unaware consumer reading false facts would see a transient blip as
// a hard refusal, which is the outcome this change exists to prevent.
func TestSessionShimHeartbeatProjectionWireShape(t *testing.T) {
	ready, _ := testSessionShimProofV2Readiness()
	const wantHealthy = `{"enabled":true,"adoptionComplete":true,"workerHostId":"host","controllerId":"controller"` +
		`,"adoptionRevision":"31","durable_carrier_proof_v2_ready":true,"composingProofV1WritesClosed":true` +
		`,"encryptedOriginalCredentialRetained":true,"remainingValidityConsumeGate":true` +
		`,"adoptedCandidateRecovery":true,"quarantinedSessions":[]}`
	healthy := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true, WorkerHostID: "host", ControllerID: "controller",
		AdoptionRevision: "31", SessionShimCarrierProofV2Readiness: ready,
		QuarantinedSessions: []SessionShimQuarantinedSession{},
	}
	encoded, err := json.Marshal(healthy)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wantHealthy {
		t.Fatalf("healthy projection bytes changed:\n got %s\nwant %s", encoded, wantHealthy)
	}
	if err := healthy.validateReady(); err != nil {
		t.Fatalf("healthy projection rejected: %v", err)
	}

	for _, state := range []string{SessionShimReadinessUnknown, SessionShimReadinessNotReady} {
		degraded := healthy
		degraded.SessionShimCarrierProofV2Readiness = SessionShimCarrierProofV2Readiness{}
		degraded.ReadinessState = state
		degraded.ReadinessReason = "carrier unreachable"
		degraded.ReadinessObservedAt = "2026-09-03T00:00:00Z"
		if err := degraded.validateReady(); err != nil {
			t.Fatalf("%s projection rejected: %v", state, err)
		}
		encodedDegraded, err := json.Marshal(degraded)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encodedDegraded, &fields); err != nil {
			t.Fatal(err)
		}
		for _, fact := range []string{
			"durable_carrier_proof_v2_ready", "composingProofV1WritesClosed",
			"encryptedOriginalCredentialRetained", "remainingValidityConsumeGate", "adoptedCandidateRecovery",
		} {
			if _, present := fields[fact]; present {
				t.Fatalf("%s projection published readiness fact %q: %s", state, fact, encodedDegraded)
			}
		}
		for _, key := range []string{"readinessState", "readinessReason", "readinessObservedAt"} {
			if _, present := fields[key]; !present {
				t.Fatalf("%s projection omitted %q: %s", state, key, encodedDegraded)
			}
		}
	}

	// A non-ready projection is refused without a reason and an observed-at,
	// and a ready one is refused if it carries them.
	missing := healthy
	missing.SessionShimCarrierProofV2Readiness = SessionShimCarrierProofV2Readiness{}
	missing.ReadinessState = SessionShimReadinessUnknown
	if err := missing.validateReady(); err == nil {
		t.Fatal("an unknown projection with no reason or observed-at was accepted")
	}
	invalid := healthy
	invalid.ReadinessState = "degraded"
	if err := invalid.validateReady(); err == nil {
		t.Fatal("an unrecognised readiness state was accepted")
	}
}

// TestSessionShimReadinessObservedAtIsNotAuthorityIdentity pins the separation
// that the acknowledgement edge depends on: two samples of one unchanged
// authority compare equal even though the observed-at clock moved.
func TestSessionShimReadinessObservedAtIsNotAuthorityIdentity(t *testing.T) {
	base := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true, WorkerHostID: "host", ControllerID: "controller",
		AdoptionRevision: "31", ReadinessState: SessionShimReadinessUnknown,
		ReadinessReason: "carrier unreachable", ReadinessObservedAt: "2026-09-03T00:00:00Z",
		QuarantinedSessions: []SessionShimQuarantinedSession{},
	}
	later := base
	later.ReadinessObservedAt = "2026-09-03T00:00:09Z"
	if !base.exactEqual(later) {
		t.Fatal("a moved observed-at made one unchanged authority compare unequal")
	}
	changed := base
	changed.ReadinessState = SessionShimReadinessNotReady
	if base.exactEqual(changed) {
		t.Fatal("a changed readiness state compared equal")
	}
	reasoned := base
	reasoned.ReadinessReason = "carrier refused"
	if base.exactEqual(reasoned) {
		t.Fatal("a changed readiness reason compared equal")
	}

	// "" and "ready" are one state. validateReady already accepts both, and the
	// exported SessionShimReadinessReady constant invites a consumer to echo
	// "ready" for a healthy beat that sent no field at all; if the comparison
	// disagreed with the validator, that helpful echo would pass validation and
	// then break every healthy beat's response processing.
	ready, _ := testSessionShimProofV2Readiness()
	healthy := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true, WorkerHostID: "host", ControllerID: "controller",
		AdoptionRevision: "31", SessionShimCarrierProofV2Readiness: ready,
		QuarantinedSessions: []SessionShimQuarantinedSession{},
	}
	spelled := healthy
	spelled.ReadinessState = SessionShimReadinessReady
	if !healthy.exactEqual(spelled) {
		t.Fatal("an echo spelling the established state \"ready\" did not compare equal to one that omitted it")
	}
	if err := spelled.validateReady(); err != nil {
		t.Fatalf("an explicit ready state was rejected by the validator: %v", err)
	}
}

// TestSessionShimHealthyProjectionSamplesAreIdentical is the acknowledgement
// convergence pin. A beat and the re-sample its acknowledgement takes must
// produce the same projection whether or not a resolution happened in between,
// or admission never reopens.
func TestSessionShimHealthyProjectionSamplesAreIdentical(t *testing.T) {
	d, _ := readinessTestDaemon(t, SessionShimConfig{}, healthyReadiness)
	scope := d.sessionShimConfig().orgID()
	refreshed, err := d.SessionShimHeartbeatProjection(scope)
	if err != nil {
		t.Fatalf("refreshing sample: %v", err)
	}
	cached, err := d.sessionShimHeartbeatProjection(scope, sessionShimReadinessCadence)
	if err != nil {
		t.Fatalf("cached sample: %v", err)
	}
	if !refreshed.exactEqual(cached) {
		t.Fatalf("samples straddling a refresh differ:\n refreshed=%+v\n    cached=%+v", refreshed, cached)
	}
	if refreshed.ReadinessObservedAt != "" || cached.ReadinessObservedAt != "" {
		t.Fatalf("a healthy sample carried observed-at: %q / %q",
			refreshed.ReadinessObservedAt, cached.ReadinessObservedAt)
	}
	again, err := d.SessionShimHeartbeatProjection(scope)
	if err != nil {
		t.Fatalf("second refreshing sample: %v", err)
	}
	if !refreshed.exactEqual(again) {
		t.Fatalf("two refreshed healthy samples differ:\n first=%+v\nsecond=%+v", refreshed, again)
	}
}

// TestCompositionDeclarationFailsClosedOnAnUnestablishedReadiness covers the
// seventh seam, the one the table above cannot reach without the composition
// harness. The founding declaration is the resolver's FIRST question about this
// scope's host authority, so there is nothing established to keep serving on: a
// resolver that cannot be consulted refuses the install rather than admitting a
// composition on an unproven readiness.
func TestCompositionDeclarationFailsClosedOnAnUnestablishedReadiness(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	embedder := &foundingEmbedder{}
	cfg := h.composedConfig(acceptingBatch)
	cfg.AcquireRecoveryScopes = embedder.acquireRecoveryScopes
	cfg.GetCarrierProofV2Readiness = func() (SessionShimCarrierProofV2Readiness, error) {
		return SessionShimCarrierProofV2Readiness{}, errors.New("readiness authority unavailable")
	}
	err := h.daemon.InstallSessionShimComposition(ctx, cfg)
	if !errors.Is(err, ErrSessionShimReadinessUnavailable) {
		t.Fatalf("composition install on an unestablished readiness = %v, want %v",
			err, ErrSessionShimReadinessUnavailable)
	}
	// The failed install stands the composition back down, so the daemon's own
	// readiness sample is no longer the thing under test; the refusal above is.
	if h.daemon.sessionShimEnabled() {
		t.Fatal("a composition refused for readiness was left installed")
	}
}

// TestSessionShimReadinessReasonCutsOnARuneBoundary pins the reason bound
// against the failure it would otherwise cause. A byte cut through a multi-byte
// rune produces invalid UTF-8; json.Marshal substitutes U+FFFD for it, so the
// value the consumer receives differs from the one still in memory and a
// PERFECT echo fails exactEqual — which costs the beat its whole response, host
// status and pending-mutation drain included, for the duration of the outage.
func TestSessionShimReadinessReasonCutsOnARuneBoundary(t *testing.T) {
	// The em dash starts at byte 255 and spans 255..257, so a cut at the byte
	// limit lands inside it. Long enough after it that trimming is forced.
	raw := strings.Repeat("a", sessionShimReadinessReasonLimit-1) + "—" + strings.Repeat("b", 64)
	reason := boundedSessionShimReadinessReason(errors.New(raw))
	if len(reason) > sessionShimReadinessReasonLimit {
		t.Fatalf("reason is %d bytes, want at most %d", len(reason), sessionShimReadinessReasonLimit)
	}
	if !utf8.ValidString(reason) {
		t.Fatalf("bounded reason is not valid UTF-8: %q", reason)
	}
	if reason == raw {
		t.Fatal("the reason was not bounded at all, so this pin proves nothing")
	}

	degraded := SessionShimHeartbeatProjection{
		Enabled: true, AdoptionComplete: true, WorkerHostID: "host", ControllerID: "controller",
		AdoptionRevision: "31", ReadinessState: SessionShimReadinessUnknown,
		ReadinessReason: reason, ReadinessObservedAt: "2026-09-03T00:00:00Z",
		QuarantinedSessions: []SessionShimQuarantinedSession{},
	}
	if err := degraded.validateReady(); err != nil {
		t.Fatalf("degraded projection rejected: %v", err)
	}
	encoded, err := json.Marshal(degraded)
	if err != nil {
		t.Fatal(err)
	}
	var echoed SessionShimHeartbeatProjection
	if err := json.Unmarshal(encoded, &echoed); err != nil {
		t.Fatal(err)
	}
	if !degraded.exactEqual(echoed) {
		t.Fatalf("a perfect echo of a degraded beat does not compare equal:\n sent=%q\nechoed=%q",
			degraded.ReadinessReason, echoed.ReadinessReason)
	}
}

// TestSessionShimUnknownFailsClosedUntilReadinessIsEstablished pins the scope of
// the non-withdrawal rule. It is a rule about not withdrawing a readiness this
// host already proved; a host that comes up into the outage has proved nothing,
// and must refuse new work rather than serve on an unresolved readiness. The
// beat still goes out — that half of the rule is unconditional.
func TestSessionShimUnknownFailsClosedUntilReadinessIsEstablished(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	d, _ := readinessTestDaemon(t, SessionShimConfig{}, func() (SessionShimCarrierProofV2Readiness, error) {
		if failing.Load() {
			return SessionShimCarrierProofV2Readiness{}, errors.New("readiness authority unavailable")
		}
		return testSessionShimProofV2Readiness()
	})
	d.setState(StateRunning)
	scope := d.sessionShimConfig().orgID()

	if err := d.sessionShimReadinessGate(sessionShimReadinessResolveNow); err == nil {
		t.Fatal("the readiness gate is open on a host that has never established readiness")
	}
	if blocked, _ := d.claimSuspended(); !blocked {
		t.Fatal("the claim gate is open on a host that has never established readiness")
	}
	// The beat is not a new-work seam: it goes out carrying the unknown.
	projection, err := d.SessionShimHeartbeatProjection(scope)
	if err != nil {
		t.Fatalf("beat on an unestablished readiness: %v", err)
	}
	if projection.ReadinessState != SessionShimReadinessUnknown {
		t.Fatalf("beat state = %q, want %q", projection.ReadinessState, SessionShimReadinessUnknown)
	}

	// Once one resolution has answered, the SAME failure is ridden out.
	failing.Store(false)
	if _, err := d.SessionShimHeartbeatProjection(scope); err != nil {
		t.Fatalf("establish readiness: %v", err)
	}
	failing.Store(true)
	if err := d.sessionShimReadinessGate(sessionShimReadinessResolveNow); err != nil {
		t.Fatalf("the readiness gate closed on an established readiness: %v", err)
	}
}

// TestSessionShimWithdrawalFenceAloneDrainsTheBeat pins the fence's own
// contribution. withdrawSessionShimProofV2Readiness also moves the lifecycle to
// recovering, and the State() != StateRunning arms then produce the same
// draining/zero answer — so the fence's branches survive every mutation unless
// the fence is raised WITHOUT that transition. That window, between the atomic
// CAS and the setState, is exactly what the fence exists for.
func TestSessionShimWithdrawalFenceAloneDrainsTheBeat(t *testing.T) {
	d, _ := readinessTestDaemon(t, SessionShimConfig{}, healthyReadiness)
	config := DefaultConfig()
	config.Capacity.MaxConcurrentSessions = 4
	d.mu.Lock()
	d.config = config
	d.mu.Unlock()
	d.setState(StateRunning)
	if got := d.RegistrationStatus(); got == RegistrationDraining {
		t.Fatalf("registration status = %s before the fence; this pin proves nothing", got)
	}
	if got := d.heartbeatMaxConcurrentSessions(); got == 0 {
		t.Fatal("advertised capacity is already zero before the fence; this pin proves nothing")
	}
	d.sessionShimReadinessWithdrawn.Store(true)
	if got := d.RegistrationStatus(); got != RegistrationDraining {
		t.Fatalf("registration status with the fence raised = %s, want %s", got, RegistrationDraining)
	}
	if got := d.heartbeatMaxConcurrentSessions(); got != 0 {
		t.Fatalf("advertised capacity with the fence raised = %d, want 0", got)
	}
}
