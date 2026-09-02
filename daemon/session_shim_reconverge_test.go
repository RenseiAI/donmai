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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
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
// both swappable, and leaves the daemon serving at retained 27.
func newReconvergeFixture(ctx context.Context, t *testing.T) (*compositionHarness, *reconvergeFixture) {
	t.Helper()
	h := newCompositionHarness(t)
	h.start(ctx)
	f := &reconvergeFixture{
		prepare: func(string, string) ([]byte, error) { return []byte("expected-27"), nil },
		commit: func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return confirmedReceipt(batch, "27"), nil
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
	h.setRefreshReceiptRevision("27")
	if receipt, ok := h.daemon.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "27" {
		t.Fatalf("retained authority after install = %+v (%v), want 27", receipt, ok)
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
		// SERVER COMMITTED: it stamps this exact batch and moves to 28…
		serverMu.Lock()
		committedDigest = batch.BatchDigest
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
			return []byte("expected-27"), nil
		}
		return nil, revisionAdvancedAnswer("27", "28", digest,
			SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("durable-correlation-28"),
				AdoptionRevision:   "28",
			})
	})
	h.setHeartbeatRequireRevision("28")

	// The quarantine-arm tail: publish the changed projection.
	d.publishSessionShimProjection(ctx, h.orgID)

	// The strand, undone. Pre-fix this never happens: the retained authority
	// stays at 27 and the reconciliation loop exhausts against a
	// preparation it cannot satisfy.
	waitForCondition(t, 10*time.Second, "the daemon to adopt the control plane's advanced revision", func() bool {
		receipt, ok := d.SessionShimScopeAuthority(h.orgID)
		return ok && receipt.AdoptionRevision == "28"
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
	if !ok || beat.SessionShim == nil || beat.SessionShim.AdoptionRevision != "28" {
		t.Fatalf("beat after adoption = %+v, want the adopted 28 projection", beat.SessionShim)
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
			return []byte("expected-27"), nil
		}
		// Same advance, but the control plane names a batch that is NOT this
		// daemon's: some other writer moved the scope.
		return nil, revisionAdvancedAnswer("27", "28", "digest-of-a-batch-this-daemon-never-sent",
			SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("durable-correlation-28"),
				AdoptionRevision:   "28",
			})
	})

	d.publishSessionShimProjection(ctx, h.orgID)

	// Never adopted: a revision this daemon cannot prove is the outcome of its
	// own batch is a revision it must not attest.
	preparesPast := sessionShimAdoptionPublicationStages + 2
	waitForCondition(t, 15*time.Second, "reconciliation to keep re-presenting past its derived bound", func() bool {
		return f.preparations() > preparesPast
	})
	if receipt, ok := d.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "27" {
		t.Fatalf("retained authority = %+v (%v), want 27 — a mismatched digest was adopted", receipt, ok)
	}

	// A host that is serving correctly but not converging looks healthy in every
	// other status field, which is exactly how one went unnoticed. The condition
	// has to be readable from the diagnostics surface, and the re-arm count has
	// to be the thing that grows.
	diagnostics := d.SessionShimDiagnostics()
	if len(diagnostics.Reconverging) != 1 || diagnostics.Reconverging[0].Scope != h.orgID {
		t.Fatalf("diagnostics.Reconverging = %+v, want the stuck scope surfaced", diagnostics.Reconverging)
	}
	stuck := diagnostics.Reconverging[0]
	if stuck.AdvancedTo != "28" || stuck.Cause != sessionShimReconcileCauseRevisionAdvanced {
		t.Fatalf("diagnostics.Reconverging[0] = %+v, want the advance and its classified cause", stuck)
	}
	if stuck.Rearms < 1 {
		t.Fatalf("diagnostics.Reconverging[0].Rearms = %d, want the re-arms to be counted", stuck.Rearms)
	}

	// And it re-converges the honest way the moment the control plane will take
	// the daemon's complete current projection at the advanced revision.
	f.setPrepare(func(string, string) ([]byte, error) { return []byte("expected-28"), nil })
	f.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return confirmedReceipt(batch, "29"), nil
	})
	waitForCondition(t, 15*time.Second, "the daemon to re-present its projection at the advanced revision", func() bool {
		receipt, ok := d.SessionShimScopeAuthority(h.orgID)
		return ok && receipt.AdoptionRevision == "29"
	})
	batches := f.committedBatches()
	republished := batches[len(batches)-1]
	if len(republished.Quarantined) != 1 || len(republished.Adopted) != 0 {
		t.Fatalf("re-presented %d quarantined / %d adopted, want the COMPLETE current projection",
			len(republished.Quarantined), len(republished.Adopted))
	}
	// Converged: the condition clears from the diagnostics surface, so a stuck
	// host is distinguishable from one that recovered.
	waitForCondition(t, 5*time.Second, "the re-convergence condition to clear", func() bool {
		return len(d.SessionShimDiagnostics().Reconverging) == 0
	})
}

// TestAdvanceSurvivesARefusedEchoedReceipt pins the classification at the last
// step. The control plane names this daemon's own batch, but the receipt it
// echoes is incomplete, so the adoption is refused. That refusal must STILL be
// the typed advance: a caller that receives an untyped error stops re-arming
// reconciliation and goes straight back to serving the superseded revision —
// the exact failure the type exists to prevent, reintroduced one layer down.
func TestAdvanceSurvivesARefusedEchoedReceipt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, f := newReconvergeFixture(ctx, t)
	d := h.daemon
	stageReconciliationQuarantine(t, d, h.orgID)

	// The control plane already committed this exact projection, so the digest
	// it names is the one the daemon's very next republish will carry.
	hostID, err := d.sessionShimHostID(ctx, h.orgID)
	if err != nil {
		t.Fatalf("resolve host authority: %v", err)
	}
	pending := d.sessionShimProjectionBatch(h.orgID, hostID)
	sortSessionShimAdoptionBatch(&pending)
	committedDigest, err := sessionShimAdoptionBatchDigest(pending)
	if err != nil {
		t.Fatalf("digest the pending projection: %v", err)
	}
	f.setPrepare(func(string, string) ([]byte, error) {
		// Everything the adoption gate needs, EXCEPT a receipt that survives
		// validation: the durable correlation is present so the gate admits it,
		// and the cleared echo claims an entry this batch never sent.
		return nil, revisionAdvancedAnswer("27", "28", committedDigest,
			SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("durable-correlation-28"),
				AdoptionRevision:   "28",
				Cleared: []SessionShimClearedQuarantine{{
					OrgID: h.orgID, SessionID: "a-lineage-this-batch-never-cleared",
				}},
			})
	})

	err = d.republishSessionShimProjection(ctx, h.orgID)
	var advanced *SessionShimAdoptionRevisionAdvanced
	if !errors.As(err, &advanced) {
		t.Fatalf("republish error = %v, want the refusal to stay classified as an advance", err)
	}
	if advanced.Advanced != "28" {
		t.Fatalf("re-wrapped advance = %+v, want it to still name the advanced revision", advanced)
	}
	if receipt, ok := d.SessionShimScopeAuthority(h.orgID); !ok || receipt.AdoptionRevision != "27" {
		t.Fatalf("retained authority = %+v (%v), want 27 — a refused receipt was retained anyway", receipt, ok)
	}
	// And because the classification survived, reconciliation was armed.
	waitForCondition(t, 5*time.Second, "the re-wrapped advance to arm reconciliation", func() bool {
		return d.reconcilingScopes() > 0 || len(d.SessionShimDiagnostics().Reconverging) > 0
	})
}

// TestBootBatchSurvivesAlreadyRecordedAdoptionEvidence is strand (B).
//
// Three live lineages survive a restart; the control plane refuses the boot
// batch because it already holds adoption evidence for two of them — the
// collision a planned restart provokes by re-presenting a still-live shim at a
// higher controller generation — and it reports them ONE AT A TIME, which a
// single-shot recovery would still turn into a host-wide failure on the second
// name. Pre-fix the first refusal aborted the composition outright and brought
// durable sessions up OFF for the whole host, orphaning every shim.
//
// WHAT THE SURVIVING LINEAGES COST. Each conflicted lineage loses this daemon's
// control socket — the same disposition every other failed per-lineage adoption
// gets — and is presented quarantined. It is NOT killed and NOT stopped: the
// shim keeps its harness and starts its own bounded orphan clock. This test
// asserts that disposition explicitly rather than describing it, because the
// difference between "released" and "killed" is the whole reason the orphan
// deadline matters.
func TestBootBatchSurvivesAlreadyRecordedAdoptionEvidence(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-recorded-evidence"

	good := f.interactiveSpec("lineage-good")
	good.OrganizationID = orgID
	firstConflict := f.interactiveSpec("lineage-conflicted-one")
	firstConflict.OrganizationID = orgID
	secondConflict := f.interactiveSpec("lineage-conflicted-two")
	secondConflict.OrganizationID = orgID
	for _, spec := range []SessionSpec{good, firstConflict, secondConflict} {
		if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
			t.Fatalf("launch %s: %v", spec.SessionID, err)
		}
	}
	// A planned restart: every shim keeps its harness and its discovery record,
	// live for the replacement daemon to re-adopt.
	f.daemon.ReleaseAdoptedSessionShims()

	conflictOrder := []string{firstConflict.SessionID, secondConflict.SessionID}
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
				if attempt <= len(conflictOrder) {
					// One name per refusal: the control plane reports the
					// collision it happened to hit first.
					return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionEvidenceRecorded{
						Lineages: []sessionshim.Identity{{OrgID: orgID, SessionID: conflictOrder[attempt-1]}},
						Err:      errors.New("adoption evidence idempotency conflict"),
					}
				}
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("batch-recorded-evidence")}, nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("already-recorded evidence for some lineages failed the whole host's composition: %v", err)
	}
	if !replacement.SessionShimAdoptionComplete() {
		t.Fatal("the boot pass left the host's adoption incomplete over recorded evidence for two lineages")
	}

	if _, err := replacement.adoptedShimEntry(orgID, good.SessionID); err != nil {
		t.Fatalf("the unaffected lineage lost its durable adoption: %v", err)
	}
	for _, sessionID := range conflictOrder {
		if _, err := replacement.adoptedShimEntry(orgID, sessionID); err == nil {
			t.Fatalf("%s was composed despite the control plane holding its evidence", sessionID)
		}
	}

	quarantined := make(map[string]sessionshim.QuarantinedSession)
	for _, q := range replacement.QuarantinedSessions() {
		quarantined[q.SessionID] = q
	}
	for _, sessionID := range conflictOrder {
		q, ok := quarantined[sessionID]
		if !ok {
			t.Fatalf("%s was not surfaced in the live quarantine projection", sessionID)
		}
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("%s quarantine reason = %q, want %q", sessionID, q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		if q.Detail == "" {
			t.Fatalf("%s quarantine carries no operator-facing detail", sessionID)
		}
	}

	// The explicit disposition: this daemon released its control socket, and the
	// shim still holds a LIVE harness. A stopped or reaped shim would have
	// withdrawn its discovery record and left a tombstone instead.
	registry, err := replacement.sessionShimRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	for _, sessionID := range conflictOrder {
		id := sessionshim.Identity{OrgID: orgID, SessionID: sessionID}
		record, err := registry.Get(id)
		if err != nil {
			t.Fatalf("%s lost its discovery record — the recovery stopped a shim it was only meant to release: %v",
				sessionID, err)
		}
		if _, err := registry.GetTombstone(id); err == nil {
			t.Fatalf("%s has a terminal tombstone — the recovery reaped a lineage it was only meant to release", sessionID)
		}
		alive, err := sessionshim.ProcessIdentity{
			PID: record.PID, StartedAt: record.ProcessStartedAt,
		}.Alive()
		if err != nil {
			t.Fatalf("%s shim liveness: %v", sessionID, err)
		}
		if !alive {
			t.Fatalf("%s shim (pid %d) is not live — the recovery stopped a shim it was only meant to release",
				sessionID, record.PID)
		}
	}

	batchMu.Lock()
	defer batchMu.Unlock()
	if want := len(conflictOrder) + 1; len(batches) != want {
		t.Fatalf("adoption batches committed = %d, want %d — one refusal per named lineage plus the accepted re-commit",
			len(batches), want)
	}
	// The accepted re-commit is still a COMPLETE snapshot: every conflicted
	// lineage is presented in the quarantined section, never silently omitted.
	recommitted := batches[len(batches)-1]
	if len(recommitted.Adopted) != 1 || recommitted.Adopted[0].Evidence.Identity.SessionID != good.SessionID {
		t.Fatalf("re-committed Adopted = %+v, want exactly the unaffected lineage", recommitted.Adopted)
	}
	if len(recommitted.Quarantined) != len(conflictOrder) {
		t.Fatalf("re-committed Quarantined = %+v, want both conflicted lineages presented", recommitted.Quarantined)
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
			got, err := sessionShimAdoptionBatchDigest(tc.mutate(base))
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			want, err := sessionShimAdoptionBatchDigest(base)
			if err != nil {
				t.Fatalf("baseline digest: %v", err)
			}
			if same := got == want; same != tc.wantSame {
				t.Fatalf("digest equal = %v, want %v", same, tc.wantSame)
			}
		})
	}
}

// TestAdoptionBatchDigestIsTheCorpusEncoding pins the encoding itself, not just
// its stability: the corpus fixes RFC 8785 canonical JSON over a document that
// omits its own digest member, hashed to exactly 64 lowercase hex characters,
// with every epoch and generation carried as a canonical uint64 decimal string
// rather than a JSON number. An ad-hoc byte layout that merely happened to be
// stable would satisfy every other test in this file and still be the wrong
// digest for the receiver comparing it.
func TestAdoptionBatchDigestIsTheCorpusEncoding(t *testing.T) {
	t.Parallel()
	batch := SessionShimAdoptionBatch{
		OrgID: "org-encoding", HostID: "host-encoding",
		Adopted: []SessionShimAdoptionOutcome{{
			Evidence: SessionShimAdoptionEvidence{
				Identity: sessionshim.Identity{OrgID: "org-encoding", SessionID: "session-a"},
				ShimID:   "shim-a", ProcessEpoch: 1 << 60, ControllerGeneration: 2,
				CarrierCompatible: true,
			},
		}},
	}
	got, err := sessionShimAdoptionBatchDigest(batch)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("digest = %q (%d chars), want exactly 64", got, len(got))
	}
	if got != strings.ToLower(got) {
		t.Fatalf("digest = %q, want lowercase hexadecimal", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("digest %q is not hexadecimal: %v", got, err)
	}

	// Recompute the corpus encoding independently and require an exact match.
	canonical, err := executioncell.CanonicalJSON(map[string]any{
		"encoding": sessionShimAdoptionBatchDigestEncoding,
		"orgId":    "org-encoding",
		"hostId":   "host-encoding",
		"adopted": []any{map[string]any{
			"orgId":                "org-encoding",
			"sessionId":            "session-a",
			"shimId":               "shim-a",
			"processEpoch":         strconv.FormatUint(1<<60, 10),
			"controllerGeneration": "2",
			"carrierCompatible":    true,
		}},
		"quarantined": []any{},
		"tombstoned":  []any{},
		"cleared":     []any{},
	})
	if err != nil {
		t.Fatalf("independent canonicalization: %v", err)
	}
	sum := sha256.Sum256(canonical)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("digest = %s, want the RFC 8785 encoding %s", got, want)
	}
	// A uint64 above 2^53 survives only because it is a STRING; a JSON number
	// would have been rounded on the way through and two distinct epochs would
	// have collided.
	if !strings.Contains(string(canonical), strconv.FormatUint(1<<60, 10)) {
		t.Fatalf("canonical document lost the exact process epoch: %s", canonical)
	}
	// The digest member is never part of the document it digests.
	if strings.Contains(string(canonical), got) {
		t.Fatalf("canonical document carries its own digest: %s", canonical)
	}
}

// TestRevisionAdvancedByOne pins the arithmetic the adoption gate depends on.
// The control boundary defines exactly one spelling for a revision — a
// canonical uint64 decimal — so anything else is refused rather than guessed
// at, and only the exact successor is an advance this daemon may adopt.
func TestRevisionAdvancedByOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		last, advanced string
		want           bool
	}{
		{name: "the exact successor", last: "28", advanced: "29", want: true},
		{name: "zero is a canonical revision", last: "0", advanced: "1", want: true},
		{name: "two steps is somebody else's commit too", last: "28", advanced: "30"},
		{name: "backwards", last: "29", advanced: "28"},
		{name: "same revision is not an advance", last: "28", advanced: "28"},
		{name: "a prefixed spelling is not the canonical one", last: "revision-1", advanced: "revision-2"},
		{name: "an unreadable shape is never a successor", last: "opaque", advanced: "opaque"},
		{name: "empty", last: "", advanced: "1"},
		{name: "leading zeros are two spellings of one number", last: "07", advanced: "08"},
		{name: "a sign is not canonical", last: "+7", advanced: "8"},
		{name: "whitespace is not canonical", last: " 7", advanced: "8"},
		{name: "overflow has no successor", last: "18446744073709551615", advanced: "18446744073709551616"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionShimRevisionAdvancedByOne(tc.last, tc.advanced); got != tc.want {
				t.Fatalf("advancedByOne(%q, %q) = %v, want %v", tc.last, tc.advanced, got, tc.want)
			}
		})
	}
}

// TestOrphanDeadlineFitsADeclaredExternalReleaseThreshold is the BLOCKING pin.
//
// §D8 rejects a policy that violates its inequality AT STARTUP and prevents
// session admission, so a default sized for a standalone host cannot simply be
// handed to a composed deployment that declared an external release threshold:
// the host would come up refusing every session. The resolved deadline must fit
// under the declared threshold, and must be the largest value this derivation
// yields rather than an arbitrary small one.
func TestOrphanDeadlineFitsADeclaredExternalReleaseThreshold(t *testing.T) {
	// The composing deployment's exact posture: it declares only how soon
	// something outside this host would consider a session abandoned, and
	// leaves every other bound to the default policy.
	const declaredThreshold = 3 * time.Minute
	d := &Daemon{}
	d.shimIdentityRef.Store(&sessionShimIdentity{config: &SessionShimConfig{
		Orphan: sessionshim.OrphanPolicy{ExternalReleaseThreshold: declaredThreshold},
	}})
	policy := d.sessionShimConfig().Orphan

	if err := policy.Validate(); err != nil {
		t.Fatalf("the resolved policy would refuse the daemon at startup: %v", err)
	}
	if policy.ExternalReleaseThreshold != declaredThreshold {
		t.Fatalf("declared threshold = %s, want it preserved as %s", policy.ExternalReleaseThreshold, declaredThreshold)
	}
	if policy.TotalBound() >= declaredThreshold {
		t.Fatalf("total bound %s is not strictly under the declared threshold %s", policy.TotalBound(), declaredThreshold)
	}
	// Maximal under the bound: the headroom left below the exclusive ceiling is
	// exactly one propagation margin, so nothing longer is available from this
	// derivation.
	ceiling := declaredThreshold - policy.TerminationGrace - policy.PropagationMargin
	if want := ceiling - policy.PropagationMargin; policy.Deadline != want {
		t.Fatalf("resolved deadline = %s, want the maximal %s under the %s ceiling",
			policy.Deadline, want, ceiling)
	}

	t.Run("an operator value is still capped by the declared threshold", func(t *testing.T) {
		t.Setenv(sessionshim.EnvOrphanDeadlineMS, "1800000")
		capped := &Daemon{}
		capped.shimIdentityRef.Store(&sessionShimIdentity{config: &SessionShimConfig{
			Orphan: sessionshim.OrphanPolicy{ExternalReleaseThreshold: declaredThreshold},
		}})
		resolved := capped.sessionShimConfig().Orphan
		if err := resolved.Validate(); err != nil {
			t.Fatalf("an operator's longer deadline was taken literally: %v", err)
		}
		if resolved.Deadline != policy.Deadline {
			t.Fatalf("operator-set deadline = %s, want the same %s ceiling the default gets",
				resolved.Deadline, policy.Deadline)
		}
	})

	t.Run("no declared threshold keeps the full default", func(t *testing.T) {
		standalone := &Daemon{}
		if got := standalone.sessionShimConfig().Orphan.Deadline; got != sessionshim.DefaultOrphanDeadline {
			t.Fatalf("standalone deadline = %s, want the full default %s", got, sessionshim.DefaultOrphanDeadline)
		}
	})
}
