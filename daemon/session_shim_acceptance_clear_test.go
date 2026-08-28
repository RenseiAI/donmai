package daemon

// Provenance: acceptance-clear-terminal-tombstone-2026-08-28 — grep for this
// marker to prove a build carries the tombstone-routed acceptance clear.
//
// The measured defect: the acceptance clear used to stage an `abandoned`
// disposition, because the helper it cleared removed its registry record and
// exited without leaving any terminal evidence behind. An abandoned obligation
// is a shim-absent attestation — it closes what the daemon owes the composer
// and never what the session owes the fence — so the lane's own session could
// never terminalize afterwards and cleanup failed forever. The helper now
// reaps its harness process group and publishes a REAL tombstone, and the
// clear routes through the production reconcile that consumes it.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

type acceptanceClearFixture struct {
	daemon      *Daemon
	registry    *sessionshim.Registry
	identity    sessionshim.Identity
	incarnation shimIncarnation

	mu        sync.Mutex
	batches   []SessionShimAdoptionBatch
	terminals []SessionShimTerminalEvidence
}

func (f *acceptanceClearFixture) published() []SessionShimAdoptionBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionShimAdoptionBatch(nil), f.batches...)
}

func (f *acceptanceClearFixture) reported() []SessionShimTerminalEvidence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionShimTerminalEvidence(nil), f.terminals...)
}

// newAcceptanceClearFixture stages exactly what an armed acceptance quarantine
// leaves behind: the lineage projected quarantined, and the acceptance
// bookkeeping holding a process identity that is provably not running (our own
// pid with a deliberately wrong start time, so Alive() answers "gone" without
// depending on some pid being free).
func newAcceptanceClearFixture(t *testing.T) *acceptanceClearFixture {
	t.Helper()
	dir := t.TempDir()
	f := &acceptanceClearFixture{
		identity: sessionshim.Identity{OrgID: "org-acceptance", SessionID: "session-acceptance"},
	}
	f.incarnation = shimIncarnation{identity: f.identity, shimID: "shim-acceptance", processEpoch: 4}
	f.daemon = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:  dir,
		HostIDForOrg: func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.terminals = append(f.terminals, evidence)
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1",
				Cleared: append([]SessionShimClearedQuarantine(nil), batch.Cleared...),
			}, nil
		},
	}})
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	f.registry = registry
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         f.identity.OrgID, SessionID: f.identity.SessionID,
		ShimID: f.incarnation.shimID, ProcessEpoch: f.incarnation.processEpoch,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineProtocolMismatch, "acceptance fixture protocol range has no overlap", time.Now())
	q.ControllerGeneration = 6
	f.daemon.shims.mu.Lock()
	f.daemon.upsertShimQuarantineLocked(q)
	f.daemon.shims.registry = registry
	f.daemon.shims.acceptanceQuarantine[f.incarnation] = sessionshim.ProcessIdentity{PID: os.Getpid(), StartedAt: 1}
	f.daemon.shims.mu.Unlock()
	return f
}

// publishHelperTombstone writes the terminal proof the acceptance helper
// publishes on its way out: a real reap, group-reaped, for this exact
// incarnation.
func (f *acceptanceClearFixture) publishHelperTombstone(t *testing.T) sessionshim.Tombstone {
	t.Helper()
	tombstone := sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         f.identity.OrgID, SessionID: f.identity.SessionID,
		ShimID: f.incarnation.shimID, ProcessEpoch: f.incarnation.processEpoch,
		HarnessPID: os.Getpid(), HarnessStartedAt: 1,
		ExitCode: 143, Signal: "SIGTERM",
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := f.registry.PutTombstone(tombstone); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	return tombstone
}

// TestAcceptanceClearLeavesThroughTheHelpersTerminalTombstone is the fix.
//
// The helper's tombstone is real terminal evidence, so the lineage leaves the
// way every quarantined-then-terminal lineage leaves: reported as
// shim_terminal_tombstone, dropped from the quarantine projection, and
// republished. Crucially it carries NO abandoned disposition — the receiver
// converts an abandoned obligation into a shim-absent attestation, which never
// permits a release, and the session that owns this lineage could then never
// terminalize.
func TestAcceptanceClearLeavesThroughTheHelpersTerminalTombstone(t *testing.T) {
	t.Parallel()
	f := newAcceptanceClearFixture(t)
	want := f.publishHelperTombstone(t)

	if err := f.daemon.clearSessionShimAcceptanceQuarantine(f.incarnation); err != nil {
		t.Fatalf("clear: %v", err)
	}

	terminals := f.reported()
	if len(terminals) != 1 {
		t.Fatalf("terminal evidence reported %d times, want exactly once", len(terminals))
	}
	evidence := terminals[0]
	if evidence.Absent != nil {
		t.Fatalf("acceptance clear reported an absent attestation: %+v — an attestation proves "+
			"unobservability, never death, and never permits a release", evidence.Absent)
	}
	if evidence.Tombstone != want || !evidence.Tombstone.GroupReaped {
		t.Fatalf("terminal evidence tombstone = %+v, want the helper's exact reap proof %+v", evidence.Tombstone, want)
	}
	if evidence.ShimID != f.incarnation.shimID || evidence.ProcessEpoch != f.incarnation.processEpoch {
		t.Fatalf("terminal evidence correlation = %s/%d, want the exact quarantined incarnation %s/%d",
			evidence.ShimID, evidence.ProcessEpoch, f.incarnation.shimID, f.incarnation.processEpoch)
	}
	// A quarantined lineage was never adopted, so it has no durable adoption
	// correlation. The receiver resolves the obligation by
	// org/host/session/shim/epoch when none is supplied; sending one would ask
	// it to match an adoption that does not exist.
	if evidence.Adoption != nil || len(evidence.DurableAdoptionCorrelation) != 0 {
		t.Fatalf("quarantined-lineage terminal evidence carried an adoption correlation: %+v / %q",
			evidence.Adoption, evidence.DurableAdoptionCorrelation)
	}

	batches := f.published()
	if len(batches) == 0 {
		t.Fatal("the clear published no batch — the platform demotes a host whose beat disagrees " +
			"with the last committed quarantine set")
	}
	last := batches[len(batches)-1]
	if len(last.Tombstoned) != 1 || last.Tombstoned[0].Tombstone != want {
		t.Fatalf("published batch carried %d tombstoned entries (%+v), want the helper's lineage", len(last.Tombstoned), last.Tombstoned)
	}
	if len(last.Quarantined) != 0 {
		t.Fatalf("published batch still carries the cleared lineage quarantined: %+v", last.Quarantined)
	}
	for i, batch := range batches {
		if len(batch.Cleared) != 0 {
			t.Fatalf("batch %d carried an abandoned disposition for a lineage with terminal evidence: %+v",
				i, batch.Cleared)
		}
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("clear left %d lineages projected quarantined", len(projected))
	}
	f.daemon.shims.mu.RLock()
	_, stillArmed := f.daemon.shims.acceptanceQuarantine[f.incarnation]
	f.daemon.shims.mu.RUnlock()
	if stillArmed {
		t.Fatal("clear left the acceptance bookkeeping armed")
	}
}

// Without a tombstone there is nothing to report and nothing to fabricate. The
// clear refuses, the lineage stays visible and capacity-charged, and no
// abandoned disposition is published as a fallback — falling back is exactly
// what stranded the lane's session before.
func TestAcceptanceClearRefusesWithoutTerminalEvidence(t *testing.T) {
	t.Parallel()
	f := newAcceptanceClearFixture(t)

	if err := f.daemon.clearSessionShimAcceptanceQuarantine(f.incarnation); err == nil {
		t.Fatal("clear reported success for a lineage with no terminal evidence")
	}

	if len(f.reported()) != 0 {
		t.Fatalf("clear reported terminal evidence it does not have: %+v", f.reported())
	}
	for i, batch := range f.published() {
		if len(batch.Cleared) != 0 {
			t.Fatalf("batch %d fell back to an abandoned disposition: %+v", i, batch.Cleared)
		}
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 1 {
		t.Fatalf("refused clear left %d lineages projected, want the lineage retained", len(projected))
	}
	f.daemon.shims.mu.RLock()
	_, stillArmed := f.daemon.shims.acceptanceQuarantine[f.incarnation]
	f.daemon.shims.mu.RUnlock()
	if !stillArmed {
		t.Fatal("refused clear dropped the acceptance bookkeeping")
	}
}

// The reconcile runs from every occupancy and heartbeat surface, so a beat can
// consume the helper's tombstone — and dispose it — before the clear verb
// arrives. The clear must accept that ordering on the retained terminal
// observation rather than refusing because the proof it was going to read has
// already done its job.
func TestAcceptanceClearAcceptsAnAlreadyReconciledLineage(t *testing.T) {
	t.Parallel()
	f := newAcceptanceClearFixture(t)
	f.publishHelperTombstone(t)

	// A beat gets there first.
	if occupancy := f.daemon.SessionShimOccupancy(); occupancy != 0 {
		t.Fatalf("occupancy after the beat's reconcile = %d, want the tombstoned lineage withdrawn", occupancy)
	}
	if len(f.reported()) != 1 {
		t.Fatalf("the beat's reconcile reported %d terminal observations, want one", len(f.reported()))
	}

	if err := f.daemon.clearSessionShimAcceptanceQuarantine(f.incarnation); err != nil {
		t.Fatalf("clear after an already-reconciled lineage: %v", err)
	}
	if len(f.reported()) != 1 {
		t.Fatalf("the clear re-reported terminal evidence (%d total), want the single durable handoff", len(f.reported()))
	}
	f.daemon.shims.mu.RLock()
	_, stillArmed := f.daemon.shims.acceptanceQuarantine[f.incarnation]
	f.daemon.shims.mu.RUnlock()
	if stillArmed {
		t.Fatal("clear left the acceptance bookkeeping armed")
	}
}

// forwarded is keyed by LIFECYCLE IDENTITY while the reconcile works per
// INCARNATION. One identity can hold a quarantined lineage and a live adopted
// one at the same time — exactly what the acceptance quarantine creates — and
// dropping the identity's durable high-water because a sibling incarnation
// terminalized would regress the surviving session's fence correlation to zero.
func TestReconcileKeepsTheForwardedHighWaterForALiveSibling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		liveSibling        bool
		quarantinedSibling bool
		want               uint64
	}{
		{name: "live adopted sibling retains it", liveSibling: true, want: 42},
		{name: "remaining quarantined sibling retains it", quarantinedSibling: true, want: 42},
		{name: "last lineage of the identity drops it", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// No batch composer: this exercises the reconcile's own bookkeeping.
			d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: dir}})
			registry, err := sessionshim.NewRegistry(dir)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			id := sessionshim.Identity{OrgID: "org-forwarded", SessionID: "session-forwarded"}
			q := sessionshim.NewQuarantinedSession(sessionshim.Record{
				SchemaVersion: sessionshim.RecordSchemaVersion,
				OrgID:         id.OrgID, SessionID: id.SessionID,
				ShimID: "shim-helper", ProcessEpoch: 1,
				CreatedAtUnixNano: time.Now().UnixNano(),
			}, sessionshim.QuarantineProtocolMismatch, "acceptance fixture protocol range has no overlap", time.Now())
			if err := registry.PutTombstone(sessionshim.Tombstone{
				SchemaVersion: sessionshim.RecordSchemaVersion,
				OrgID:         id.OrgID, SessionID: id.SessionID,
				ShimID: "shim-helper", ProcessEpoch: 1,
				GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
			}); err != nil {
				t.Fatalf("PutTombstone: %v", err)
			}
			d.shims.mu.Lock()
			d.shims.registry = registry
			d.upsertShimQuarantineLocked(q)
			d.shims.forwarded[id] = 42
			if tc.liveSibling {
				d.shims.adopted[id] = adoptedShim{shimID: "shim-live"}
			}
			if tc.quarantinedSibling {
				sibling := sessionshim.NewQuarantinedSession(sessionshim.Record{
					SchemaVersion: sessionshim.RecordSchemaVersion,
					OrgID:         id.OrgID, SessionID: id.SessionID,
					ShimID: "shim-sibling", ProcessEpoch: 2,
					CreatedAtUnixNano: time.Now().UnixNano(),
				}, sessionshim.QuarantineSocketUnreachable, "sibling lineage still quarantined", time.Now())
				d.upsertShimQuarantineLocked(sibling)
			}
			d.shims.mu.Unlock()

			d.reconcileQuarantinedTombstones()

			d.shims.mu.RLock()
			got := d.shims.forwarded[id]
			remaining := len(d.shims.quarantined)
			d.shims.mu.RUnlock()
			wantRemaining := 0
			if tc.quarantinedSibling {
				wantRemaining = 1
			}
			if remaining != wantRemaining {
				t.Fatalf("reconcile left %d quarantined lineages, want %d", remaining, wantRemaining)
			}
			if got != tc.want {
				t.Fatalf("forwarded high-water = %d, want %d", got, tc.want)
			}
		})
	}
}

// The clear's break condition has two halves and both must hold. A lineage that
// simply VANISHED from the projection — dropped by some other path, or never
// there — is unobservable, not ended, and accepting it would put back exactly
// the disposition this change removed.
func TestAcceptanceClearRefusesALineageThatVanishedWithoutATombstone(t *testing.T) {
	t.Parallel()
	f := newAcceptanceClearFixture(t)
	// Someone else drops the projection row; no tombstone is ever written.
	f.daemon.shims.mu.Lock()
	f.daemon.shims.quarantined = nil
	f.daemon.shims.mu.Unlock()

	if err := f.daemon.clearSessionShimAcceptanceQuarantine(f.incarnation); err == nil {
		t.Fatal("clear accepted a lineage that vanished with no terminal evidence")
	}
	if len(f.reported()) != 0 {
		t.Fatalf("clear reported terminal evidence for a lineage that left no proof: %+v", f.reported())
	}
	f.daemon.shims.mu.RLock()
	_, stillArmed := f.daemon.shims.acceptanceQuarantine[f.incarnation]
	f.daemon.shims.mu.RUnlock()
	if !stillArmed {
		t.Fatal("refused clear dropped the acceptance bookkeeping")
	}
}

// A SIBLING's tombstone must never release a live session's claim.
//
// The scalar terminal proof is identity-scoped, so before this guard a
// quarantined helper's group-reaped tombstone answered ReleaseAllowed for a
// session whose real harness was still running — and kept answering it for the
// rest of the daemon's life, because the retained tombstone never expires.
// Invariant 10 requires proof that THAT EXACT harness process group was reaped.
func TestSiblingTombstoneDoesNotReleaseALiveLineage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: dir}})
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-sibling", SessionID: "session-sibling"}
	sibling := sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-helper", ProcessEpoch: 1,
		HarnessPID: os.Getpid(), HarnessStartedAt: 1,
		GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutTombstone(sibling); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	d.shims.mu.Lock()
	d.shims.registry = registry
	d.shims.adopted[id] = adoptedShim{shimID: "shim-live"}
	d.shims.mu.Unlock()

	proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID)
	if !proof.Proves() {
		t.Fatal("precondition: the sibling tombstone is not even a proof; this test would pass vacuously")
	}
	if verdict := d.SessionShimReleaseDecision(id.OrgID, id.SessionID, proof); verdict == sessionshim.ReleaseAllowed {
		t.Fatal("a sibling incarnation's tombstone released a session whose adopted lineage is still live — " +
			"§D10 requires proof for that exact harness process group")
	}

	// Control: once the live lineage's OWN tombstone exists, release is allowed.
	live := sibling
	live.ShimID = "shim-live"
	live.ProcessEpoch = 0
	if err := registry.PutTombstone(live); err != nil {
		t.Fatalf("PutTombstone(live): %v", err)
	}
	proof = d.SessionShimTerminalProof(id.OrgID, id.SessionID)
	if verdict := d.SessionShimReleaseDecision(id.OrgID, id.SessionID, proof); verdict != sessionshim.ReleaseAllowed {
		t.Fatalf("release verdict with the adopted lineage's own reap proof = %q, want %q",
			verdict, sessionshim.ReleaseAllowed)
	}
}

// The reconcile runs from eight surfaces. Two passes reading one tombstone
// before either has finished its durable handoff would both commit it, and the
// composer would record the same terminal observation twice.
func TestConcurrentReconcilesReportOneTombstoneOnce(t *testing.T) {
	t.Parallel()
	f := newAcceptanceClearFixture(t)
	f.publishHelperTombstone(t)
	// Hold the report inside the hook so both passes are genuinely in flight.
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	f.daemon.opts.SessionShim.OnTerminalEvidence = func(_ context.Context, evidence SessionShimTerminalEvidence) error {
		entered <- struct{}{}
		<-release
		f.mu.Lock()
		f.terminals = append(f.terminals, evidence)
		f.mu.Unlock()
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.daemon.reconcileQuarantinedTombstones()
		}()
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("no reconcile pass reported the tombstone")
	}
	close(release)
	wg.Wait()

	if got := len(f.reported()); got != 1 {
		t.Fatalf("concurrent reconciles reported the same tombstone %d times, want exactly one", got)
	}
	if extra := len(entered); extra != 0 {
		t.Fatalf("%d additional passes entered the durable handoff for one incarnation", extra)
	}
}

// A quarantined lineage is reported WITHOUT an adoption correlation, even when
// this daemon still retains one from before the lineage was quarantined.
//
// The composer's obligation for a quarantined lineage is quarantined-kind and
// resolves on lifecycle identity plus shim id and process epoch. An attached
// adoption receipt asks the receiver for the ADOPTED-kind predicate instead,
// which matches nothing once the lineage has been reported quarantined:
// measured on an installed host as a terminal observation that committed while
// the obligation stayed `active`, after which every complete batch was refused
// `adoption_batch_live_lineage_omitted` and the host could not recover.
func TestQuarantinedLineageIsReportedWithoutAnAdoptionCorrelation(t *testing.T) {
	t.Parallel()
	f := newAcceptanceClearFixture(t)
	// The lineage was adopted once and this daemon still holds the receipt.
	f.daemon.shims.mu.Lock()
	f.daemon.shims.correlations[f.incarnation] = sessionShimAdoptionCorrelation{
		evidence: SessionShimAdoptionEvidence{
			Identity: f.identity, ShimID: f.incarnation.shimID,
			ProcessEpoch: f.incarnation.processEpoch, ControllerGeneration: 6,
		},
		receipt: SessionShimAdoptionReceipt{DurableCorrelation: []byte("adoption-receipt")},
	}
	f.daemon.shims.mu.Unlock()
	f.publishHelperTombstone(t)

	f.daemon.reconcileQuarantinedTombstones()

	reported := f.reported()
	if len(reported) != 1 {
		t.Fatalf("terminal evidence reported %d times, want exactly once", len(reported))
	}
	if reported[0].Adoption != nil || len(reported[0].DurableAdoptionCorrelation) != 0 {
		t.Fatalf("a quarantined lineage was reported with an adoption correlation (%+v / %q) — the receiver "+
			"then looks for an adopted-kind obligation that no longer exists and the quarantined one stays active",
			reported[0].Adoption, reported[0].DurableAdoptionCorrelation)
	}
}
