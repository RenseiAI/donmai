package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// rollbackBeatFixture is a healthy, already-committed daemon with a live
// heartbeat lane: adoption complete, carrier activation complete, a retained
// scope receipt at revision "test-recovery-revision", and a recording control
// plane that echoes every projection back. It deliberately does NOT start the
// heartbeat service — every SessionShim option the beat goroutine reads must
// be settled before start(), because the service reads the config on every
// beat and a later mutation would race it.
type rollbackBeatFixture struct {
	f        *shimSpawnFixture
	d        *Daemon
	probe    *dynamicPublicationProbe
	recorder *immediateBeatRecorder
	service  *HeartbeatService
}

func newRollbackBeatFixture(t *testing.T, hostID string) *rollbackBeatFixture {
	t.Helper()
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.opts.SessionShim.HostID = hostID
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(50)
	configureDynamicPublicationProbe(t, d, probe)

	recorder := &immediateBeatRecorder{}
	server := httptest.NewServer(recorder.handler(t))
	t.Cleanup(server.Close)
	service := NewHeartbeatService(HeartbeatOptions{
		WorkerID: "worker-" + hostID, OrchestratorURL: server.URL,
		RuntimeJWT:      "runtime-" + hostID,
		IntervalSeconds: int(HeartbeatDefaultInterval / time.Second),
		GetActiveCount:  func() int { return 0 },
		GetMaxCount:     d.heartbeatMaxConcurrentSessions,
		GetStatus:       d.RegistrationStatus,
		GetSessionShim: func() (SessionShimHeartbeatProjection, error) {
			return d.SessionShimHeartbeatProjection(f.orgID)
		},
		OnSessionShimAcknowledged: func(projection SessionShimHeartbeatProjection) {
			d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, projection)
		},
		HTTPClient: server.Client(),
	})
	d.lifecycleMu.Lock()
	d.heartbeat = service
	d.lifecycleMu.Unlock()
	return &rollbackBeatFixture{f: f, d: d, probe: probe, recorder: recorder, service: service}
}

// start begins the periodic lane and waits out its first beat so tests have a
// baseline. No SessionShim option may be mutated after this returns.
func (rb *rollbackBeatFixture) start(t *testing.T) int {
	t.Helper()
	rb.service.Start()
	t.Cleanup(rb.service.Stop)
	waitFor(t, 5*time.Second, "the periodic lane's first beat", func() bool {
		return rb.recorder.count() >= 1
	})
	return rb.recorder.count()
}

// TestFailedDynamicPublicationRollsBackAndKeepsTheHeartbeatAlive is the
// stranding regression measured in the sandbox: a healthy, ready daemon with a
// committed startup generation claims an interactive session, the dynamic
// adoption's durable batch fails, the claim is NACKed — and before this change
// the daemon then never beat again. The barrier had dropped
// carrierActivationComplete for the attempt, the failure path restored
// nothing, so SessionShimHeartbeatProjection errored ("carrier activation is
// not complete") and HeartbeatService skipped every subsequent beat while the
// platform's cleared row sat unrepaired forever.
//
// After the rollback: the NACK still happens, the next beat is SENT carrying
// exactly the last-committed projection (never the revision the failed attempt
// would have minted — the barrier's invariant), and a subsequent claim is
// admitted once the control plane accepts the beat.
func TestFailedDynamicPublicationRollsBackAndKeepsTheHeartbeatAlive(t *testing.T) {
	rb := newRollbackBeatFixture(t, "host-rollback-beat")
	d, f := rb.d, rb.f
	var refuse atomic.Bool
	refuse.Store(true)
	baseBatch := d.opts.SessionShim.OnAdoptionBatch
	d.opts.SessionShim.OnAdoptionBatch = func(ctx context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		if refuse.Load() {
			return SessionShimAdoptionBatchReceipt{}, errors.New("injected durable-batch refusal")
		}
		return baseBatch(ctx, batch)
	}
	var activationAcks atomic.Int64
	d.opts.SessionShim.OnCarrierActivationAcknowledged = func(SessionShimPublishedBatchReceipt) {
		activationAcks.Add(1)
	}
	baseline := rb.start(t)

	failedAt := time.Now()
	_, err := d.spawner.AcceptWork(f.interactiveSpec("rollback-nack"))
	if err == nil || !strings.Contains(err.Error(), "durable adoption batch") {
		t.Fatalf("failed dynamic publication accept error = %v, want the durable-batch failure the poll loop NACKs on", err)
	}

	// The beat lane must have survived the failure: the projection composes
	// again and carries the last-committed generation.
	projection, projectionErr := d.SessionShimHeartbeatProjection(f.orgID)
	if projectionErr != nil {
		t.Fatalf("heartbeat projection after the failed publication = %v, want the last-committed projection", projectionErr)
	}
	if projection.AdoptionRevision != "test-recovery-revision" {
		t.Fatalf("post-rollback projection revision = %q, want the last-committed %q",
			projection.AdoptionRevision, "test-recovery-revision")
	}
	// The correcting beat is rung immediately on the failure path; within an
	// interval of the failure only that beat can account for the bump.
	waitFor(t, 5*time.Second, "the correcting beat after the rollback", func() bool {
		return rb.recorder.count() > baseline
	})
	if elapsed := time.Since(failedAt); elapsed >= HeartbeatDefaultInterval {
		t.Fatalf("correcting beat arrived after %s — that is the ticker, not the immediate correcting beat", elapsed)
	}
	revisions := rb.recorder.adoptionCompleteRevisions()
	if len(revisions) == 0 || revisions[len(revisions)-1] != "test-recovery-revision" {
		t.Fatalf("adoption-complete revisions on the wire = %v, want the last to re-attest %q",
			revisions, "test-recovery-revision")
	}
	// Barrier invariant, the discriminating control: the failed attempt's
	// revision must never have been announced. A "fix" that force-completed
	// the attempt instead of rolling it back would put dynamic-revision-1 on
	// the wire here.
	for _, revision := range revisions {
		if revision == "dynamic-revision-1" {
			t.Fatalf("the UNcommitted attempt's revision reached the wire: %v", revisions)
		}
	}
	if got := activationAcks.Load(); got != 0 {
		t.Fatalf("OnCarrierActivationAcknowledged fired %d times for a publication that never committed", got)
	}

	// Admission is restored to the pre-attempt base: nothing pending, nothing
	// latched, claims open.
	if d.shims.dynamicPublicationFailed {
		t.Fatal("rolled-back publication latched dynamicPublicationFailed — later launches would be refused forever")
	}
	if d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("rolled-back publication left the readiness fence withdrawn with nothing to clear it")
	}
	if d.State() != StateRunning || !d.spawner.IsAccepting() {
		t.Fatalf("rollback did not restore admission: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
	}
	if suspended, reason := d.PollClaimGate()(); suspended {
		t.Fatalf("poll/claim admission still suspended after the rollback: %s", reason)
	}

	// And the repair is complete end-to-end: with the control plane having
	// accepted the correcting beat, a subsequent claim is admitted and its
	// publication commits.
	refuse.Store(false)
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("rollback-recovered")); err != nil {
		t.Fatalf("claim after the rollback was refused: %v", err)
	}
	waitFor(t, 5*time.Second, "the recovered launch's own beat to clear its barrier", func() bool {
		return !d.sessionShimReadinessWithdrawn.Load() && d.State() == StateRunning
	})
	revisions = rb.recorder.adoptionCompleteRevisions()
	if len(revisions) == 0 || revisions[len(revisions)-1] != "dynamic-revision-1" {
		t.Fatalf("recovered launch revisions on the wire = %v, want the last to be %q", revisions, "dynamic-revision-1")
	}
}

// TestDurableAdoptionPublicationOutlivesTheLaunchClock pins the budget half of
// the same incident: the batch prepare died on "context deadline exceeded" one
// to two seconds in because it inherited the launch clock's remainder, while
// its own retry policy still held a full budget. The publication flow now runs
// on its own bound derived from the per-callback bound, so a durable-batch
// callback that outlives the configured launch clock — but stays inside the
// flow's budget — completes, and the adoption succeeds instead of NACKing.
func TestDurableAdoptionPublicationOutlivesTheLaunchClock(t *testing.T) {
	const launchClock = 6 * time.Second
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.opts.SessionShim.HostID = "host-publication-budget"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(60)
	configureDynamicPublicationProbe(t, d, probe)
	d.opts.SessionShim.LaunchTimeout = launchClock
	d.opts.SessionShim.CallbackTimeout = 30 * time.Second

	var launchStarted atomic.Int64
	baseBatch := d.opts.SessionShim.OnAdoptionBatch
	d.opts.SessionShim.OnAdoptionBatch = func(ctx context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		// Block until the launch clock has demonstrably expired. Before the
		// fix the callback context descended from that clock and died here;
		// with the flow's own budget the wait completes and the batch commits.
		target := time.Unix(0, launchStarted.Load()).Add(launchClock + 750*time.Millisecond)
		timer := time.NewTimer(time.Until(target))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return SessionShimAdoptionBatchReceipt{}, ctx.Err()
		case <-timer.C:
		}
		return baseBatch(ctx, batch)
	}

	launchStarted.Store(time.Now().UnixNano())
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("publication-budget")); err != nil {
		t.Fatalf("adoption whose batch outlived the launch clock failed: %v", err)
	}
	if elapsed := time.Since(time.Unix(0, launchStarted.Load())); elapsed <= launchClock {
		t.Fatalf("publication finished in %s, inside the %s launch clock — the callback never outlived it and this proved nothing", elapsed, launchClock)
	}
	if _, err := d.adoptedShimEntry(f.orgID, "publication-budget"); err != nil {
		t.Fatalf("session was not adopted after the slow publication: %v", err)
	}
	// The launch's barrier clears through the ordinary acknowledgement edge.
	projection, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("heartbeat projection after the slow publication: %v", err)
	}
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, projection)
	if d.State() != StateRunning || d.sessionShimReadinessWithdrawn.Load() {
		t.Fatalf("acknowledged slow publication did not reopen: state=%s withdrawn=%v",
			d.State(), d.sessionShimReadinessWithdrawn.Load())
	}
}

// TestFailedPublicationCarryingAQuarantineChangeRollsBackAndKeepsBeating is
// the quarantine flavor: the projection batch that fails mid-publication is
// one carrying a releaseShimIfLive-driven set change — a session quarantined
// after its controller stream ended, committed through the republish path.
// The rollback must restore the beat to that committed generation: revision
// AND quarantine set, so the host keeps attesting the exact state the control
// plane last acknowledged instead of falling silent over it.
func TestFailedPublicationCarryingAQuarantineChangeRollsBackAndKeepsBeating(t *testing.T) {
	rb := newRollbackBeatFixture(t, "host-quarantine-flavor")
	d, f := rb.d, rb.f

	// Seed exactly what releaseShimIfLive records for a controller stream that
	// ended without a terminal observation, then commit it through the real
	// republish path so "last-committed" includes the quarantine.
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         f.orgID, SessionID: "quarantine-flavor-drop",
		ShimID: "shim-quarantine-flavor", ProcessEpoch: 3,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable,
		"controller stream ended before a terminal observation", time.Now())
	q.ControllerGeneration = 2
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.mu.Unlock()
	d.publishSessionShimProjection(context.Background(), f.orgID)
	committed, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("projection after the committed republish: %v", err)
	}
	if committed.AdoptionRevision != "dynamic-revision-1" || len(committed.QuarantinedSessions) != 1 {
		t.Fatalf("committed republish projection = revision %q with %d quarantined, want dynamic-revision-1 with 1",
			committed.AdoptionRevision, len(committed.QuarantinedSessions))
	}

	var refuse atomic.Bool
	refuse.Store(true)
	baseBatch := d.opts.SessionShim.OnAdoptionBatch
	d.opts.SessionShim.OnAdoptionBatch = func(ctx context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		if refuse.Load() {
			return SessionShimAdoptionBatchReceipt{}, errors.New("injected mid-batch refusal")
		}
		return baseBatch(ctx, batch)
	}
	baseline := rb.start(t)

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("quarantine-flavor-launch")); err == nil {
		t.Fatal("launch whose batch was refused mid-publication unexpectedly succeeded")
	}
	projection, projectionErr := d.SessionShimHeartbeatProjection(f.orgID)
	if projectionErr != nil {
		t.Fatalf("heartbeat projection after the failed quarantine-carrying batch = %v, want the committed projection", projectionErr)
	}
	if projection.AdoptionRevision != committed.AdoptionRevision {
		t.Fatalf("post-rollback revision = %q, want the committed %q", projection.AdoptionRevision, committed.AdoptionRevision)
	}
	// TWO entries now, not one: the pre-existing "quarantine-flavor-drop" the
	// committed republish already carried, PLUS "quarantine-flavor-launch"
	// itself. The launch's own OnAdoption succeeded (configureDynamicPublicationProbe's
	// fake durably records it) before the batch commit ever ran, so the
	// control plane already holds "quarantine-flavor-launch" live regardless
	// of how many times the batch that would durably publish it gets
	// refused. completeLaunchedSessionShimAdoptionBatchResilient's exhaustion
	// path records exactly that into this daemon's own quarantine projection
	// — never leaving local state claiming "nothing happened" when the
	// control plane's own per-session record says otherwise, which is what
	// let a later, unrelated batch get refused for omitting it (measured in
	// review, independent of this rollback path).
	//
	// NOT free of consequence, though: in THIS test the restore batch also
	// fails (refuse never clears), so this entry was never durably committed
	// to the control plane either. The beat now presents a quarantine set
	// the platform's own last-COMMITTED batch does not know about, and
	// sessionShimProjectionBatch's own doc comment says exactly what a real
	// control plane does with that mismatch: demote the host to draining
	// until the sets agree again. This test's fake heartbeat receiver does
	// not model that comparison, so it cannot fail this assertion — but a
	// real deployment would ride out one bounded demotion window here,
	// cleared by the same orphan-deadline tombstone path that already
	// clears every other quarantined lineage (measured: ~2.26s from
	// exhaustion to occupancy 1→0 in review). Bounded and self-healing, and
	// strictly better than a prior round's permanent full-batch refusal —
	// but not orthogonal to, and not something the checkpoint/rollback
	// machinery here corrects for. That machinery genuinely never touches
	// d.shims.quarantined at all; it just doesn't make this consequence-free.
	wantQuarantined := map[string]bool{"quarantine-flavor-drop": true, "quarantine-flavor-launch": true}
	if len(projection.QuarantinedSessions) != len(wantQuarantined) {
		t.Fatalf("post-rollback quarantine set = %+v, want exactly %v", projection.QuarantinedSessions, wantQuarantined)
	}
	for _, q := range projection.QuarantinedSessions {
		if !wantQuarantined[q.SessionID] {
			t.Fatalf("post-rollback quarantine set = %+v, want exactly %v", projection.QuarantinedSessions, wantQuarantined)
		}
	}
	waitFor(t, 5*time.Second, "the correcting beat after the quarantine-flavor rollback", func() bool {
		return rb.recorder.count() > baseline
	})
	revisions := rb.recorder.adoptionCompleteRevisions()
	if len(revisions) == 0 || revisions[len(revisions)-1] != committed.AdoptionRevision {
		t.Fatalf("wire revisions after the rollback = %v, want the last to re-attest %q", revisions, committed.AdoptionRevision)
	}
	if d.State() != StateRunning || !d.spawner.IsAccepting() || d.shims.dynamicPublicationFailed {
		t.Fatalf("quarantine-flavor rollback did not restore admission: state=%s accepting=%v latched=%v",
			d.State(), d.spawner.IsAccepting(), d.shims.dynamicPublicationFailed)
	}
}

// TestFailedQuarantineRepublishLeavesTheCommittedProjectionBeatable pins the
// republish path's own failure posture: when publishSessionShimProjection's
// batch fails, nothing durable moved, so the beat must keep flowing and keep
// attesting the last-committed revision — the platform's snapshot comparison
// is then the one arguing, and the next successful republish repairs it. This
// is the control that the republish path never inherits the launch path's
// silence.
func TestFailedQuarantineRepublishLeavesTheCommittedProjectionBeatable(t *testing.T) {
	rb := newRollbackBeatFixture(t, "host-republish-refused")
	d, f := rb.d, rb.f
	var refuse atomic.Bool
	refuse.Store(true)
	baseBatch := d.opts.SessionShim.OnAdoptionBatch
	d.opts.SessionShim.OnAdoptionBatch = func(ctx context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		if refuse.Load() {
			return SessionShimAdoptionBatchReceipt{}, errors.New("injected republish refusal")
		}
		return baseBatch(ctx, batch)
	}

	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         f.orgID, SessionID: "republish-refused-drop",
		ShimID: "shim-republish-refused", ProcessEpoch: 5,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable,
		"controller stream ended before a terminal observation", time.Now())
	d.shims.mu.Lock()
	d.upsertShimQuarantineLocked(q)
	d.shims.mu.Unlock()
	d.publishSessionShimProjection(context.Background(), f.orgID)

	projection, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("projection after the refused republish = %v, want it beatable", err)
	}
	if projection.AdoptionRevision != "test-recovery-revision" {
		t.Fatalf("revision after the refused republish = %q, want the committed %q untouched",
			projection.AdoptionRevision, "test-recovery-revision")
	}
	if len(projection.QuarantinedSessions) != 1 {
		t.Fatalf("beat hides the live quarantine after the refused republish: %+v", projection.QuarantinedSessions)
	}
	if d.State() != StateRunning {
		t.Fatalf("refused republish moved the lifecycle: %s", d.State())
	}
	// start() itself waits out a sent beat; reaching here proves the lane
	// still flows after the refused republish.
	rb.start(t)
	revisions := rb.recorder.adoptionCompleteRevisions()
	if len(revisions) == 0 || revisions[len(revisions)-1] != "test-recovery-revision" {
		t.Fatalf("wire revisions after the refused republish = %v, want %q", revisions, "test-recovery-revision")
	}
}
