package daemon

// Provenance: shim-adoption-reconvergence-2026-09-01 — grep a build for this
// marker to prove a daemon can re-converge with a control plane that already
// moved, and that a boot batch the control plane has already recorded costs one
// lineage rather than the host.
//
// THE MEASURED STRANDS
//
// (A) An adoption-batch commit's answer was lost AFTER the control plane
// stamped it. Reconciliation then re-prepared, and every re-preparation
// answered that the expected host adoption revision had changed — because it
// had, by exactly the batch this daemon sent. Four attempts spent that answer,
// the loop declared exhaustion, and the daemon served and beat the superseded
// revision from then on: every heartbeat refused, every credential receipt at
// the new revision refused, the host unusable until authority state was edited
// by hand.
//
// (B) A planned restart re-presented a still-live shim at a higher controller
// generation. The control plane answered the boot batch with a closed
// idempotency conflict for that ONE lineage, and the daemon aborted the whole
// composition: durable sessions came up OFF for the entire host, every
// controller closed, and the orphaned shims reaped their own live harnesses.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// reconvergeFixture is the reconciliation fixture plus the preparation hook —
// the callback that carries the control plane's "your expected revision moved"
// answer, and therefore the only place the advance can be observed.
type reconvergeFixture struct {
	mu       sync.Mutex
	batches  []SessionShimAdoptionBatch
	prepares int
	prepare  func(orgID, hostID string) ([]byte, error)
	commit   func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)
}

func (f *reconvergeFixture) setPrepare(prepare func(orgID, hostID string) ([]byte, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepare = prepare
}

func (f *reconvergeFixture) setCommit(commit func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commit = commit
}

func (f *reconvergeFixture) committedBatches() []SessionShimAdoptionBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionShimAdoptionBatch(nil), f.batches...)
}

func (f *reconvergeFixture) preparations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares
}

// newReconvergeFixture installs a composition whose preparation and commit are
// both swappable, and leaves the daemon serving at retained revision-1.
func newReconvergeFixture(ctx context.Context, t *testing.T) (*compositionHarness, *reconvergeFixture) {
	t.Helper()
	h := newCompositionHarness(t)
	h.start(ctx)
	f := &reconvergeFixture{
		prepare: func(string, string) ([]byte, error) { return []byte("expected-revision-1"), nil },
		commit: func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return confirmedReceipt(batch, "revision-1"), nil
		},
	}
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		f.mu.Lock()
		commit := f.commit
		f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
		f.mu.Unlock()
		return commit(batch)
	})
	cfg.PrepareAdoptionBatch = func(_ context.Context, orgID, hostID string) ([]byte, error) {
		f.mu.Lock()
		f.prepares++
		prepare := f.prepare
		f.mu.Unlock()
		return prepare(orgID, hostID)
	}
	// The reconciliation bounds are derived from the callback bound, so
	// shrinking that one unit keeps the whole loop test-sized.
	cfg.CallbackTimeout = 200 * time.Millisecond
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("composition install: %v", err)
	}
	h.setRefreshReceiptState(SessionShimCredentialStateReady)
	// The refresh lane keeps answering the revision the daemon already holds:
	// this is the incident's shape, where the only surface that knew the
	// control plane had moved was the preparation's refusal.
	h.setRefreshReceiptRevision("revision-1")
	if receipt, ok := h.daemon.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "revision-1" {
		t.Fatalf("retained authority after install = %+v (%v), want revision-1", receipt, ok)
	}
	return h, f
}

// revisionAdvancedAnswer is the typed answer a composing preparation returns
// when the control plane reports that the expected compare-and-swap state has
// already moved. The wrapped message is the operator-visible one.
func revisionAdvancedAnswer(lastCommitted, advanced, digest string, committed SessionShimAdoptionBatchReceipt) error {
	return &SessionShimAdoptionRevisionAdvanced{
		LastCommitted:        lastCommitted,
		Advanced:             advanced,
		CommittedBatchDigest: digest,
		Committed:            committed,
		Err:                  errors.New("adoption batch preparation changed the expected host adoption revision"),
	}
}

// TestAdvancedRevisionForThisDaemonsOwnBatchIsAdopted is strand (A) end to end.
//
// The control plane commits the batch and the answer is lost. Every later
// preparation reports the advance — and names the digest of the batch it
// committed, which is this daemon's own. Post-fix the daemon adopts that
// revision and its receipt as the outcome of its own commit, without
// re-committing anything; pre-fix it argued with the preparation until the
// bound ran out and then served the superseded revision forever.
func TestAdvancedRevisionForThisDaemonsOwnBatchIsAdopted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconvergeFixture(ctx, t)
	d := h.daemon
	stageReconciliationQuarantine(t, d, h.orgID)
	commitsBefore := len(f.committedBatches())

	var (
		serverMu        sync.Mutex
		committedDigest string
	)
	f.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		// SERVER COMMITTED: it stamps this exact batch and moves to revision-2…
		serverMu.Lock()
		committedDigest = batch.OperationalDigest
		serverMu.Unlock()
		// …while this daemon's copy of the answer is lost to the flake that
		// provoked the retry.
		return SessionShimAdoptionBatchReceipt{}, transportLostCommitAnswer()
	})
	f.setPrepare(func(string, string) ([]byte, error) {
		serverMu.Lock()
		digest := committedDigest
		serverMu.Unlock()
		if digest == "" {
			return []byte("expected-revision-1"), nil
		}
		return nil, revisionAdvancedAnswer("revision-1", "revision-2", digest,
			SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("durable-correlation-revision-2"),
				AdoptionRevision:   "revision-2",
			})
	})
	h.setHeartbeatRequireRevision("revision-2")

	// The quarantine-arm tail: publish the changed projection.
	d.publishSessionShimProjection(ctx, h.orgID)

	// The strand, undone. Pre-fix this never happens: the retained authority
	// stays at revision-1 and the reconciliation loop exhausts against a
	// preparation it cannot satisfy.
	waitForCondition(t, 10*time.Second, "the daemon to adopt the control plane's advanced revision", func() bool {
		receipt, ok := d.SessionShimScopeAuthority(h.orgID)
		return ok && receipt.AdoptionRevision == "revision-2"
	})
	waitForCondition(t, 10*time.Second, "reconciliation to settle", func() bool {
		return d.reconcilingScopes() == 0
	})

	// The adoption is not a second commit: the control plane already holds this
	// batch, and re-sending it would be exactly the duplicate publication the
	// digest exists to recognise.
	if got := len(f.committedBatches()) - commitsBefore; got != 1 {
		t.Fatalf("commits after the ambiguous one = %d, want 0 — adopting an already-committed batch must not re-commit it", got-1)
	}

	// And the beat re-attests at the adopted revision instead of 409ing forever.
	if err := d.heartbeat.SendNow(ctx); err != nil {
		t.Fatalf("beat after adopting the advanced revision: %v", err)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil || beat.SessionShim.AdoptionRevision != "revision-2" {
		t.Fatalf("beat after adoption = %+v, want the adopted revision-2 projection", beat.SessionShim)
	}
}

// TestAdvancedRevisionForSomebodyElsesBatchIsNotAdopted is the control, and the
// other half of the requirement: a digest that DISAGREES is somebody else's
// commit, so the daemon must not take the revision, and must keep re-presenting
// its own complete projection instead of settling on the superseded one.
func TestAdvancedRevisionForSomebodyElsesBatchIsNotAdopted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconvergeFixture(ctx, t)
	d := h.daemon
	stageReconciliationQuarantine(t, d, h.orgID)

	var (
		serverMu sync.Mutex
		moved    bool
	)
	f.setCommit(func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		serverMu.Lock()
		moved = true
		serverMu.Unlock()
		return SessionShimAdoptionBatchReceipt{}, transportLostCommitAnswer()
	})
	f.setPrepare(func(string, string) ([]byte, error) {
		serverMu.Lock()
		advanced := moved
		serverMu.Unlock()
		if !advanced {
			return []byte("expected-revision-1"), nil
		}
		// Same advance, but the control plane names a batch that is NOT this
		// daemon's: some other writer moved the scope.
		return nil, revisionAdvancedAnswer("revision-1", "revision-2", "digest-of-a-batch-this-daemon-never-sent",
			SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("durable-correlation-revision-2"),
				AdoptionRevision:   "revision-2",
			})
	})

	d.publishSessionShimProjection(ctx, h.orgID)

	// Never adopted: a revision this daemon cannot prove is the outcome of its
	// own batch is a revision it must not attest.
	preparesPast := sessionShimAdoptionPublicationStages + 2
	waitForCondition(t, 15*time.Second, "reconciliation to keep re-presenting past its derived bound", func() bool {
		return f.preparations() > preparesPast
	})
	if receipt, ok := d.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "revision-1" {
		t.Fatalf("retained authority = %+v (%v), want revision-1 — a mismatched digest was adopted", receipt, ok)
	}

	// And it re-converges the honest way the moment the control plane will take
	// the daemon's complete current projection at the advanced revision.
	f.setPrepare(func(string, string) ([]byte, error) { return []byte("expected-revision-2"), nil })
	f.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return confirmedReceipt(batch, "revision-3"), nil
	})
	waitForCondition(t, 15*time.Second, "the daemon to re-present its projection at the advanced revision", func() bool {
		receipt, ok := d.SessionShimScopeAuthority(h.orgID)
		return ok && receipt.AdoptionRevision == "revision-3"
	})
	batches := f.committedBatches()
	republished := batches[len(batches)-1]
	if len(republished.Quarantined) != 1 || len(republished.Adopted) != 0 {
		t.Fatalf("re-presented %d quarantined / %d adopted, want the COMPLETE current projection",
			len(republished.Quarantined), len(republished.Adopted))
	}
}

// TestBootBatchSurvivesAlreadyRecordedAdoptionEvidence is strand (B).
//
// Two live lineages survive a restart; the control plane refuses the boot batch
// because it already holds adoption evidence for one of them — the collision a
// planned restart provokes by re-presenting a still-live shim at a higher
// controller generation. Pre-fix that aborted the composition and brought
// durable sessions up OFF for the whole host, orphaning both shims. Post-fix it
// costs exactly the one lineage: quarantined, presented, still holding its
// harness, with the rest of the host durable.
func TestBootBatchSurvivesAlreadyRecordedAdoptionEvidence(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-recorded-evidence"

	good := f.interactiveSpec("lineage-good")
	good.OrganizationID = orgID
	conflicted := f.interactiveSpec("lineage-conflicted")
	conflicted.OrganizationID = orgID
	if _, err := f.daemon.spawner.AcceptWork(good); err != nil {
		t.Fatalf("launch the good lineage: %v", err)
	}
	if _, err := f.daemon.spawner.AcceptWork(conflicted); err != nil {
		t.Fatalf("launch the conflicted lineage: %v", err)
	}
	// A planned restart: both shims keep their harnesses and their discovery
	// records, live for the replacement daemon to re-adopt.
	f.daemon.ReleaseAdoptedSessionShims()

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    f.registry,
			HostID:         "host-recorded-evidence",
			OrgID:          orgID,
			OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("lineage-committed")}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				batchMu.Lock()
				batches = append(batches, cloneSessionShimAdoptionBatch(batch))
				attempt := len(batches)
				batchMu.Unlock()
				if attempt == 1 {
					return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionEvidenceRecorded{
						Lineages: []sessionshim.Identity{{OrgID: orgID, SessionID: conflicted.SessionID}},
						Err:      errors.New("adoption evidence idempotency conflict"),
					}
				}
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("batch-recorded-evidence")}, nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("a single lineage's already-recorded evidence failed the whole host's composition: %v", err)
	}
	if !replacement.SessionShimAdoptionComplete() {
		t.Fatal("the boot pass left the host's adoption incomplete over one lineage's recorded evidence")
	}

	if _, err := replacement.adoptedShimEntry(orgID, good.SessionID); err != nil {
		t.Fatalf("the unaffected lineage lost its durable adoption: %v", err)
	}
	if _, err := replacement.adoptedShimEntry(orgID, conflicted.SessionID); err == nil {
		t.Fatal("the conflicted lineage was composed despite the control plane holding its evidence")
	}

	found := false
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID != conflicted.SessionID {
			continue
		}
		found = true
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("conflicted lineage quarantine reason = %q, want %q", q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		if q.Detail == "" {
			t.Fatal("conflicted lineage quarantine carries no operator-facing detail")
		}
	}
	if !found {
		t.Fatal("the conflicted lineage was not surfaced in the live quarantine projection")
	}

	batchMu.Lock()
	defer batchMu.Unlock()
	if len(batches) != 2 {
		t.Fatalf("adoption batches committed = %d, want the refused one plus exactly one re-commit", len(batches))
	}
	// The re-commit is still a COMPLETE snapshot: the conflicted lineage is
	// presented in the quarantined section, never silently omitted.
	recommitted := batches[1]
	if len(recommitted.Adopted) != 1 || recommitted.Adopted[0].Evidence.Identity.SessionID != good.SessionID {
		t.Fatalf("re-committed Adopted = %+v, want exactly the unaffected lineage", recommitted.Adopted)
	}
	if len(recommitted.Quarantined) != 1 || recommitted.Quarantined[0].SessionID != conflicted.SessionID {
		t.Fatalf("re-committed Quarantined = %+v, want the conflicted lineage presented", recommitted.Quarantined)
	}
}

// TestAdoptionBatchDigestIsStableAcrossRepresentations pins the property the
// whole (A) fix rests on: two presentations of the same set, seconds apart,
// must produce the same idempotency key, and any change to what the batch SAYS
// must produce a different one.
func TestAdoptionBatchDigestIsStableAcrossRepresentations(t *testing.T) {
	t.Parallel()
	base := SessionShimAdoptionBatch{
		OrgID: "org-digest", HostID: "host-digest",
		Adopted: []SessionShimAdoptionOutcome{{
			Evidence: SessionShimAdoptionEvidence{
				Identity: sessionshim.Identity{OrgID: "org-digest", SessionID: "session-a"},
				ShimID:   "shim-a", ProcessEpoch: 3, ControllerGeneration: 2, CarrierCompatible: true,
			},
		}},
		Quarantined: []sessionshim.QuarantinedSession{{
			OrgID: "org-digest", SessionID: "session-b", ShimID: "shim-b",
			ProcessEpoch: 4, ControllerGeneration: 1,
			Reason: sessionshim.QuarantineAdoptionFailed, ConsumesCapacity: true,
		}},
	}
	for _, tc := range []struct {
		name     string
		mutate   func(SessionShimAdoptionBatch) SessionShimAdoptionBatch
		wantSame bool
	}{
		{
			name:     "an identical re-presentation",
			mutate:   func(b SessionShimAdoptionBatch) SessionShimAdoptionBatch { return b },
			wantSame: true,
		},
		{
			name: "a different expected compare-and-swap state",
			mutate: func(b SessionShimAdoptionBatch) SessionShimAdoptionBatch {
				b.ExpectedRevision = []byte("expected-99")
				return b
			},
			wantSame: true,
		},
		{
			name: "a quarantine that merely aged",
			mutate: func(b SessionShimAdoptionBatch) SessionShimAdoptionBatch {
				b.Quarantined = append([]sessionshim.QuarantinedSession(nil), b.Quarantined...)
				b.Quarantined[0].AgeSeconds += 47
				return b
			},
			wantSame: true,
		},
		{
			name: "a lineage that changed controller generation",
			mutate: func(b SessionShimAdoptionBatch) SessionShimAdoptionBatch {
				b.Adopted = append([]SessionShimAdoptionOutcome(nil), b.Adopted...)
				b.Adopted[0].Evidence.ControllerGeneration = 3
				return b
			},
		},
		{
			name: "a lineage that moved from adopted to quarantined",
			mutate: func(b SessionShimAdoptionBatch) SessionShimAdoptionBatch {
				amended, quarantines := sessionShimBatchAfterEvidenceRecorded(
					b, []sessionshim.Identity{{OrgID: "org-digest", SessionID: "session-a"}})
				if len(quarantines) != 1 {
					t.Fatalf("fixture produced %d quarantines, want 1", len(quarantines))
				}
				return amended
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionShimAdoptionBatchDigest(tc.mutate(base))
			if same := got == sessionShimAdoptionBatchDigest(base); same != tc.wantSame {
				t.Fatalf("digest equal = %v, want %v", same, tc.wantSame)
			}
		})
	}
}

// TestRevisionAdvancedByOne pins the arithmetic the adoption gate depends on:
// exactly one step, same prefix, and nothing adopted from a shape this cannot
// read.
func TestRevisionAdvancedByOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		last, advanced string
		want           bool
	}{
		{name: "bare counter", last: "28", advanced: "29", want: true},
		{name: "prefixed counter", last: "revision-1", advanced: "revision-2", want: true},
		{name: "two steps is somebody else's commit too", last: "28", advanced: "30"},
		{name: "backwards", last: "29", advanced: "28"},
		{name: "same revision is not an advance", last: "28", advanced: "28"},
		{name: "a changed prefix is a changed scope", last: "revision-1", advanced: "other-2"},
		{name: "an unreadable shape is never a successor", last: "opaque", advanced: "opaque"},
		{name: "empty", last: "", advanced: "1"},
		{name: "leading zeros are two spellings of one number", last: "07", advanced: "08"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionShimRevisionAdvancedByOne(tc.last, tc.advanced); got != tc.want {
				t.Fatalf("advancedByOne(%q, %q) = %v, want %v", tc.last, tc.advanced, got, tc.want)
			}
		})
	}
}
