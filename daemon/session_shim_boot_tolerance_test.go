package daemon

// Provenance: shim-boot-dead-lineage-tolerance-2026-09-06 — grep a build for
// this marker to prove a boot survives a dead lineage and a refused commit
// without withdrawing the rest of the host's durability, and without
// swallowing a failure a restart would have recovered from.
//
// THE MEASURED STRANDS
//
// (A) A shim-owned seat was adopted and running; the daemon came back with the
// shim's process gone and no tombstone. The startup scan named the condition
// exactly — "registry record is stale (process gone, no tombstone)" — dropped
// the lineage from the batch, and the control plane refused the batch for
// omitting a lineage it still held. The daemon exited, and every later start
// repeated it verbatim. One dead seat took the org's durable sessions down
// permanently.
//
// (B) One lineage's durable adoption was refused. The pass quarantined it and
// then failed carrier activation for the WHOLE host, because the quarantined
// lineage's staged Snapshot had nothing left to resolve it — durable sessions
// off for every other seat, all of them still running.
//
// WHAT THESE TESTS PIN, AND WHAT THEY DELIBERATELY DO NOT. They pin the
// OBSERVABLE outcomes: what the batch presents, what the install returns and
// how it is classified, whether the other lineages keep their durability, and
// which failures are NOT reclassified. They do not pin the log text, and they
// do not assert that a stale lineage is terminal — it is not, and
// manufacturing that conclusion is the one thing this whole change refuses to
// do.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// deadLineageRecord publishes one discovery record whose process identity is
// provably not running: pid 1 always exists, and its start time is never 1, so
// ProcessIdentity.Alive answers a definite "gone" on every platform without
// this test having to reap a process of its own.
func deadLineageRecord(t *testing.T, registryDir, orgID, sessionID, shimID string, epoch uint64) sessionshim.Record {
	t.Helper()
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	rec := sessionshim.Record{
		SchemaVersion:     sessionshim.RecordSchemaVersion,
		OrgID:             orgID,
		SessionID:         sessionID,
		ShimID:            shimID,
		ProcessEpoch:      epoch,
		PID:               1,
		ProcessStartedAt:  1,
		SocketPath:        registry.SocketPath(sessionshim.Identity{OrgID: orgID, SessionID: sessionID}),
		ProtocolMin:       shimwire.ProtocolMin,
		ProtocolMax:       shimwire.ProtocolMax,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().Add(-time.Minute).UnixNano(),
	}
	if err := registry.Put(rec); err != nil {
		t.Fatalf("publish stale record: %v", err)
	}
	return rec
}

// batchQuarantine returns the batch's entry for one session, if it has one.
func batchQuarantine(batch SessionShimAdoptionBatch, sessionID string) (sessionshim.QuarantinedSession, bool) {
	for _, entry := range batch.Quarantined {
		if entry.SessionID == sessionID {
			return entry, true
		}
	}
	return sessionshim.QuarantinedSession{}, false
}

// TestBootDeclaresADeadLineageInsteadOfOmittingIt is strand (A), for the half
// of the stale set the absence producer could NOT discharge.
//
// The two passes are an ordered pair. The producer runs first and attests away
// every vanished shim it can prove absent AND get the composer to accept; what
// comes back is what this host still OWES — here, a lineage whose absent
// attestation the composer refused. The fake control plane then enforces the
// completeness rule the real one does: a batch that does not account for a
// lineage it holds is refused. Pre-fix the composition could not produce a
// committable batch at all — the only batch it could compose omitted the one
// thing the rule demanded.
//
// The assertion is deliberately not "the daemon survived": it is that the batch
// PRESENTED the lineage, at its exact shim incarnation, in a disposition that
// makes no claim about the harness. A fix that satisfied the rule by declaring
// the lineage terminal would pass a survival check and forge a reap proof.
func TestBootDeclaresADeadLineageInsteadOfOmittingIt(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const (
		deadSession = "sess-dead-lineage"
		deadShim    = "shim-dead-lineage"
		deadEpoch   = uint64(7)
	)
	rec := deadLineageRecord(t, h.registryDir, h.orgID, deadSession, deadShim, deadEpoch)

	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		// The real rule: the control plane holds this lineage, so a batch that
		// does not account for it in ANY disposition is refused.
		if _, ok := batchQuarantine(batch, deadSession); !ok {
			return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
				Lineages: []SessionShimOmittedLineage{{
					Identity:             sessionshim.Identity{OrgID: h.orgID, SessionID: deadSession},
					ShimID:               deadShim,
					ProcessEpoch:         deadEpoch,
					ControllerGeneration: 1,
				}},
				Err: errors.New("the batch omitted a live lineage"),
			}
		}
		return confirmedReceipt(batch, "revision-dead-lineage"), nil
	})
	// The composer refuses the absent attestation, so the obligation is NOT
	// discharged and the lineage is still owed. That is the state this
	// declaration exists for; a lineage the producer did discharge is covered
	// by TestDischargedStaleLineageIsNotDeclared below.
	cfg.OnTerminalEvidence = func(context.Context, SessionShimTerminalEvidence) error {
		return errors.New("the control plane refused this absent attestation")
	}
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("one dead lineage failed the whole composition: %v", err)
	}

	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF for a host whose only problem was one dead lineage")
	}
	if !h.daemon.SessionShimAdoptionComplete() {
		t.Fatal("the boot pass left the host's adoption incomplete over one dead lineage")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	if len(committed) != 1 {
		// One attempt, not two: the daemon OBSERVED the dead process itself
		// and declared the lineage in the batch it composed. Needing the
		// control plane to name it first would still commit eventually, but it
		// would mean the daemon still drops a fact it already holds — and on
		// the measured host the control plane's refusal was the thing that
		// killed the process.
		t.Fatalf("batch commit attempts = %d, want 1 — the dead lineage was not declared in the first batch", len(committed))
	}
	declared, ok := batchQuarantine(committed[0], deadSession)
	if !ok {
		t.Fatalf("the committed batch omitted the dead lineage; batch = %+v", committed[0])
	}
	if declared.ShimID != rec.ShimID || declared.ProcessEpoch != rec.ProcessEpoch {
		t.Fatalf("declared incarnation = %s@%d, want %s@%d — a completeness rule matches on the exact incarnation",
			declared.ShimID, declared.ProcessEpoch, rec.ShimID, rec.ProcessEpoch)
	}
	if declared.Detail == "" {
		t.Fatal("the declared lineage carries no operator-facing detail")
	}
	if !declared.ConsumesCapacity {
		t.Fatal("a lineage whose harness group is not proven reaped must still charge capacity")
	}
	if len(committed[0].Tombstoned) != 0 {
		t.Fatalf("the batch tombstoned %d lineage(s) — a vanished shim is not proof its harness group was reaped",
			len(committed[0].Tombstoned))
	}

	// And the live projection agrees with what was published, beat for beat:
	// the receiver demotes a host whose heartbeat disagrees with the snapshot
	// its last committed batch stored.
	var live bool
	for _, q := range h.daemon.QuarantinedSessions() {
		if q.SessionID == deadSession && q.ShimID == deadShim && q.ProcessEpoch == deadEpoch {
			live = true
		}
	}
	if !live {
		t.Fatal("the declared lineage is published in the batch but absent from the live quarantine projection")
	}
}

// TestDischargedStaleLineageIsNotDeclared is the ordering pin between the two
// passes that both read the stale set.
//
// The absence producer attests a vanished shim away and the composer converts
// that lineage's recovery obligation to abandoned, dropping it from its
// completeness set. From that moment the batch may legitimately OMIT it — and
// must, because declaring it quarantined re-creates the very obligation the
// attestation just discharged and re-charges the capacity slot it just
// released, in the same batch that carries the attestation.
//
// The two passes read the same field, so the only thing keeping them honest is
// that the declaration is computed AFTER the discharge narrows it. Reading it
// earlier — even one statement earlier, which is where it sat — snapshots the
// pre-discharge set and undoes the discharge silently: the batch still commits,
// the host still boots, and nothing says the obligation came back.
//
// The assertions are deliberately chosen to hold whatever the producer does
// with the on-disk record: they are about what this daemon DECLARES, not about
// what the registry still contains.
func TestDischargedStaleLineageIsNotDeclared(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const (
		goneSession = "sess-discharged-lineage"
		goneShim    = "shim-discharged-lineage"
		goneEpoch   = uint64(11)
	)
	deadLineageRecord(t, h.registryDir, h.orgID, goneSession, goneShim, goneEpoch)

	var (
		mu       sync.Mutex
		batches  []SessionShimAdoptionBatch
		attested []SessionShimTerminalEvidence
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		return confirmedReceipt(batch, "revision-discharged"), nil
	})
	// The composer ACCEPTS the absent attestation, which is what makes the
	// obligation discharged rather than merely observed.
	cfg.OnTerminalEvidence = func(_ context.Context, evidence SessionShimTerminalEvidence) error {
		mu.Lock()
		attested = append(attested, evidence)
		mu.Unlock()
		return nil
	}
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("composition install: %v", err)
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	discharged := append([]SessionShimTerminalEvidence(nil), attested...)
	mu.Unlock()

	if len(discharged) != 1 {
		t.Fatalf("absent attestations = %d, want exactly the one vanished lineage; the fixture did not reach the "+
			"discharge this test is about", len(discharged))
	}
	if discharged[0].Absent == nil || !discharged[0].Absent.Complete() {
		t.Fatalf("the discharge did not carry a complete absent attestation: %+v", discharged[0])
	}
	if len(committed) != 1 {
		t.Fatalf("batch commit attempts = %d, want 1", len(committed))
	}
	if entry, ok := batchQuarantine(committed[0], goneSession); ok {
		t.Fatalf("a lineage the absence producer had already discharged was re-declared as a live quarantine, "+
			"re-creating the obligation and re-charging its capacity slot: %+v", entry)
	}
	for _, q := range h.daemon.QuarantinedSessions() {
		if q.SessionID == goneSession {
			t.Fatalf("the discharged lineage is back in the live quarantine projection: %+v", q)
		}
	}
	if occupied := h.daemon.SessionShimOccupancy(); occupied != 0 {
		t.Fatalf("occupied slots = %d, want 0 — the discharged lineage is still charging capacity", occupied)
	}
}

// TestStaleRecordForAnUnservedScopeDoesNotFailTheComposition is the guard on
// the declaration's own blast radius, and it exists because the first version
// of this change reintroduced exactly the bug it was written to remove.
//
// A stale record is the longest-lived residue on a host: nothing removes it
// from disk, so it outlives the credential receipt for its organization by an
// unbounded margin. Declaring it puts that organization into the batch's scope
// set, and resolving a host identity for a scope with no retained receipt is a
// hard failure of the WHOLE composition — on every start, untyped, with no
// operator route out. Pre-change the record was dropped and the host booted.
//
// The served scope must still compose normally in the same pass: a filter that
// bought safety by declaring nothing would pass a "did not brick" check while
// silently reopening the omission this whole change is about.
func TestStaleRecordForAnUnservedScopeDoesNotFailTheComposition(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const (
		servedSession = "sess-served-scope"
		servedShim    = "shim-served-scope"
		servedEpoch   = uint64(2)
		strandedOrg   = "org-no-longer-served"
	)
	deadLineageRecord(t, h.registryDir, h.orgID, servedSession, servedShim, servedEpoch)
	deadLineageRecord(t, h.registryDir, strandedOrg, "sess-stranded", "shim-stranded", 9)

	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		return confirmedReceipt(batch, "revision-unserved-scope"), nil
	})
	// Two things at once. The stranded record's organization has no retained
	// credential receipt — which is what makes resolving a host identity for it
	// fatal — so the declaration must never reach for one. And refusing the
	// attestation keeps the SERVED record owed rather than discharged, so the
	// second half of this test (the served scope still declares) is testing the
	// declaration and not the absence producer.
	cfg.OnTerminalEvidence = func(context.Context, SessionShimTerminalEvidence) error {
		return errors.New("the control plane refused this absent attestation")
	}
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("one leftover record for an organization this host no longer serves failed the whole composition: %v", err)
	}
	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF over a stale record for a scope no batch is composed for")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	if len(committed) != 1 {
		t.Fatalf("batches committed = %d, want exactly the one served scope", len(committed))
	}
	if committed[0].OrgID != h.orgID {
		t.Fatalf("composed a batch for %q, want only the served scope %q", committed[0].OrgID, h.orgID)
	}
	// The served scope still declares — the filter narrows the blast radius, it
	// does not disable the declaration.
	if _, ok := batchQuarantine(committed[0], servedSession); !ok {
		t.Fatalf("the served scope's stale lineage was not declared; batch = %+v", committed[0])
	}
	for _, entry := range committed[0].Quarantined {
		if entry.OrgID == strandedOrg {
			t.Fatalf("a lineage for an unserved organization was declared into a served scope's batch: %+v", entry)
		}
	}
	for _, q := range h.daemon.QuarantinedSessions() {
		if q.OrgID == strandedOrg {
			t.Fatalf("the unserved organization's lineage entered the live projection: %+v", q)
		}
	}
}

// TestBackoffCancellationKeepsItsUntypedError is the other door into the
// classification F2 closed.
//
// The re-composition paces its passes, and a boot whose context is cancelled or
// expires mid-wait ends there. That is the transient class by definition — the
// same class a deadline from the commit callback is already kept out of — but
// it arrives wrapped together with the completeness refusal that the pass was
// answering. Whichever of the two carries the %w decides how the daemon
// classifies it, and getting it wrong stands durable sessions down and stamps
// the status surface permanently for a context that simply ran out.
func TestBackoffCancellationKeepsItsUntypedError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newCompositionHarness(t)
	h.start(ctx)

	held := []SessionShimOmittedLineage{
		{
			Identity: sessionshim.Identity{OrgID: h.orgID, SessionID: "held-first"},
			ShimID:   "shim-first", ProcessEpoch: 1, ControllerGeneration: 1,
		},
		{
			Identity: sessionshim.Identity{OrgID: h.orgID, SessionID: "held-second"},
			ShimID:   "shim-second", ProcessEpoch: 2, ControllerGeneration: 1,
		},
	}
	var (
		mu       sync.Mutex
		attempts int
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt >= 2 {
			// The boot is cancelled while the pass that would answer the next
			// refusal is still waiting out its backoff.
			cancel()
		}
		for _, lineage := range held {
			if _, ok := batchQuarantine(batch, lineage.Identity.SessionID); !ok {
				return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
					Lineages: []SessionShimOmittedLineage{lineage},
					Err:      errors.New("the batch omitted a live lineage"),
				}
			}
		}
		return confirmedReceipt(batch, "revision-cancelled"), nil
	})
	err := h.daemon.InstallSessionShimComposition(ctx, cfg)
	if err == nil {
		t.Fatal("a cancelled boot reported a successful composition")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("install error = %v, want the cancellation to survive on the errors.Is path", err)
	}
	var refused *SessionShimDurabilityRefused
	if errors.As(err, &refused) {
		t.Fatalf("a cancelled boot was classified as an unresolvable durability refusal: %v", err)
	}
	if status := h.daemon.SessionShimDiagnostics(); status.DurabilityRefusal != nil {
		t.Fatalf("a transient cancellation was stamped on the status surface as a permanent refusal: %+v",
			status.DurabilityRefusal)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got < 2 {
		t.Fatalf("batch commit attempts = %d, want at least 2 — the fixture never reached the paced pass", got)
	}
}

// TestLineageWithNoReportedGenerationIsNotDeclared is N6's control, and the
// discriminating assertion is the BATCH, not the outcome.
//
// The control plane's omission report renders controllerGeneration nullable,
// and a null arrives here as a zero. Declaring on it looks like progress —
// the batch commits, the host boots — but the row it adds is at a generation
// the receiver's exact-key lookup cannot match: the refusal it was meant to
// answer stays unanswered, and a phantom, capacity-charged obligation is
// created that nothing can clear. Degrading visibly is the honest outcome.
//
// So this asserts that no committed batch ever carries the entry. Asserting
// only that the install degraded would pass with or without the guard, because
// a fixture that refuses unconditionally degrades either way.
func TestLineageWithNoReportedGenerationIsNotDeclared(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const ungenerated = "sess-generation-dropped"
	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		batches = append(batches, cloneSessionShimAdoptionBatch(batch))
		mu.Unlock()
		// Accepts as soon as the lineage is presented in any disposition —
		// so a daemon that declared it on a zero generation would commit and
		// look healthy.
		if _, ok := batchQuarantine(batch, ungenerated); ok {
			return confirmedReceipt(batch, "revision-generation-dropped"), nil
		}
		return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
			Lineages: []SessionShimOmittedLineage{{
				Identity:     sessionshim.Identity{OrgID: h.orgID, SessionID: ungenerated},
				ShimID:       "shim-generation-dropped",
				ProcessEpoch: 5,
			}},
			Err: errors.New("the batch omitted a live lineage"),
		}
	})
	err := h.daemon.InstallSessionShimComposition(ctx, cfg)

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	for _, batch := range committed {
		if entry, ok := batchQuarantine(batch, ungenerated); ok {
			t.Fatalf("a lineage whose reported generation was dropped was declared anyway, at a generation the "+
				"receiver cannot match: %+v", entry)
		}
	}
	var refused *SessionShimDurabilityRefused
	if !errors.As(err, &refused) {
		t.Fatalf("install error = %v, want the undeclarable refusal classified so the host degrades visibly", err)
	}
}

// TestBootRecomposesAroundALineageTheControlPlaneStillHolds pins the second
// half of strand (A): a refusal naming a lineage this daemon has NO record of
// — the control plane holds it, the host does not — is answered by declaring
// it, not by failing the host.
func TestBootRecomposesAroundALineageTheControlPlaneStillHolds(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	const (
		unknownSession = "sess-only-the-control-plane-knows"
		unknownShim    = "shim-only-the-control-plane-knows"
		unknownEpoch   = uint64(3)
	)
	var (
		mu       sync.Mutex
		attempts int
		last     SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		attempts++
		last = cloneSessionShimAdoptionBatch(batch)
		mu.Unlock()
		if _, ok := batchQuarantine(batch, unknownSession); !ok {
			return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
				Lineages: []SessionShimOmittedLineage{{
					Identity:             sessionshim.Identity{OrgID: h.orgID, SessionID: unknownSession},
					ShimID:               unknownShim,
					ProcessEpoch:         unknownEpoch,
					ControllerGeneration: 4,
				}},
				Err: errors.New("the batch omitted a live lineage"),
			}
		}
		return confirmedReceipt(batch, "revision-recomposed"), nil
	})
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("a lineage only the control plane knows about failed the composition: %v", err)
	}
	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF over a lineage the daemon could have declared")
	}
	mu.Lock()
	gotAttempts, committed := attempts, last
	mu.Unlock()
	if gotAttempts != 2 {
		t.Fatalf("batch commit attempts = %d, want 2 (the refusal, then the declared re-composition)", gotAttempts)
	}
	declared, ok := batchQuarantine(committed, unknownSession)
	if !ok {
		t.Fatalf("the re-composed batch still omits the lineage; batch = %+v", committed)
	}
	if declared.ControllerGeneration != 4 {
		t.Fatalf("declared generation = %d, want the 4 the control plane reported", declared.ControllerGeneration)
	}
	var projected bool
	for _, q := range h.daemon.QuarantinedSessions() {
		if q.SessionID == unknownSession && q.ShimID == unknownShim && q.ProcessEpoch == unknownEpoch {
			projected = true
		}
	}
	if !projected {
		t.Fatal("the declared lineage is in the committed batch but not in the live projection the heartbeat publishes")
	}
}

// TestRecompositionAnswersMoreOmissionsThanTheBatchItStartedFrom pins the bound.
//
// The number of lineages the RECEIVER holds is not bounded by what this host
// has on disk, and a receiver that reports its omissions a page at a time is
// the normal shape rather than an adversarial one. A bound derived from the
// batch is therefore the wrong quantity: an empty local registry facing three
// pages of omissions gets one pass, and the scope degrades over a refusal the
// daemon was one commit away from answering.
func TestRecompositionAnswersMoreOmissionsThanTheBatchItStartedFrom(t *testing.T) {
	ctx := context.Background()
	h := newCompositionHarness(t)
	h.start(ctx)

	held := []SessionShimOmittedLineage{
		{
			Identity: sessionshim.Identity{OrgID: h.orgID, SessionID: "held-one"},
			ShimID:   "shim-one", ProcessEpoch: 1, ControllerGeneration: 1,
		},
		{
			Identity: sessionshim.Identity{OrgID: h.orgID, SessionID: "held-two"},
			ShimID:   "shim-two", ProcessEpoch: 2, ControllerGeneration: 1,
		},
		{
			Identity: sessionshim.Identity{OrgID: h.orgID, SessionID: "held-three"},
			ShimID:   "shim-three", ProcessEpoch: 3, ControllerGeneration: 1,
		},
	}
	var (
		mu   sync.Mutex
		last SessionShimAdoptionBatch
	)
	cfg := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		mu.Lock()
		last = cloneSessionShimAdoptionBatch(batch)
		mu.Unlock()
		// One page per refusal, and the untruncated total travels with it —
		// exactly the shape a report limit produces.
		for _, lineage := range held {
			if _, ok := batchQuarantine(batch, lineage.Identity.SessionID); !ok {
				return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
					Lineages:     []SessionShimOmittedLineage{lineage},
					TotalOmitted: len(held),
					Err:          errors.New("the batch omitted a live lineage"),
				}
			}
		}
		return confirmedReceipt(batch, "revision-paged"), nil
	})
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("a paged completeness refusal failed the composition: %v", err)
	}
	if !h.daemon.SessionShimHostAttestation().Supports() {
		t.Fatal("durable sessions came up OFF over omissions the daemon could have declared one page at a time")
	}
	mu.Lock()
	committed := last
	mu.Unlock()
	for _, lineage := range held {
		if _, ok := batchQuarantine(committed, lineage.Identity.SessionID); !ok {
			t.Fatalf("the committed batch omits %s; batch = %+v", lineage.Identity.SessionID, committed)
		}
	}
}

// TestBootStandsDurableSessionsDownWhenTheRefusalCannotBeResolved is the
// posture that makes every other recovery optional rather than load-bearing.
//
// The control plane refuses with a completeness refusal that names nothing this
// daemon can declare — the shape where no bounded recovery can help. The
// install must come back CLASSIFIED, so an embedder that exits on any error
// can keep its host by classifying this one, and the daemon must still be
// serving, stood down, with the reason readable from the status surface.
func TestBootStandsDurableSessionsDownWhenTheRefusalCannotBeResolved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal error
		want    string
	}{
		{
			name: "refusal names no lineage at all",
			refusal: &SessionShimAdoptionBatchLineagesOmitted{
				Err: errors.New("the batch omitted a live lineage"),
			},
			want: "none named",
		},
		{
			name: "refusal names a lineage with no exact incarnation to declare",
			refusal: &SessionShimAdoptionBatchLineagesOmitted{
				Lineages: []SessionShimOmittedLineage{{
					Identity: sessionshim.Identity{OrgID: "org-composition", SessionID: "sess-unnameable"},
				}},
				Err: errors.New("the batch omitted a live lineage"),
			},
			want: "org-composition/sess-unnameable",
		},
		{
			name: "recorded-evidence conflict the narrow recovery could not settle",
			refusal: &SessionShimAdoptionEvidenceRecorded{
				Lineages: []sessionshim.Identity{{OrgID: "org-composition", SessionID: "sess-recorded"}},
				Err:      errors.New("adoption evidence idempotency conflict"),
			},
			want: "org-composition/sess-recorded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newCompositionHarness(t)
			h.start(ctx)
			cfg := h.composedConfig(func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return SessionShimAdoptionBatchReceipt{}, tc.refusal
			})
			err := h.daemon.InstallSessionShimComposition(ctx, cfg)
			var refused *SessionShimDurabilityRefused
			if !errors.As(err, &refused) {
				t.Fatalf("install error = %v, want it classified as a durability refusal so an embedder can act on it", err)
			}
			if refused.Scope != h.orgID {
				t.Fatalf("refused scope = %q, want %q", refused.Scope, h.orgID)
			}
			if got := refused.LineageIDs(); got != tc.want {
				t.Fatalf("refused lineages = %q, want %q", got, tc.want)
			}
			if h.daemon.SessionShimHostAttestation().Supports() {
				t.Fatal("the composition was left installed after its batch was refused")
			}
			if state := h.daemon.State(); state != StateRunning {
				t.Fatalf("daemon state = %q, want %q — the host must keep serving direct-owned sessions", state, StateRunning)
			}

			// The reason is on the operator surface, not only in the process
			// log of a host that has already lost the feature.
			status := h.daemon.SessionShimDiagnostics()
			if status.DurabilityRefusal == nil {
				t.Fatal("host status reports durable sessions off with no reason at all")
			}
			if status.DurabilityRefusal.Scope != h.orgID || status.DurabilityRefusal.Reason == "" {
				t.Fatalf("status refusal = %+v, want the scope and a reason", status.DurabilityRefusal)
			}

			// And the posture is reversible: a later install against a control
			// plane that accepts the batch works, and clears the reason.
			good := h.composedConfig(func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return confirmedReceipt(batch, "revision-after-stand-down"), nil
			})
			if err := h.daemon.InstallSessionShimComposition(ctx, good); err != nil {
				t.Fatalf("the stand-down latched: a later healthy install was refused: %v", err)
			}
			if !h.daemon.SessionShimHostAttestation().Supports() {
				t.Fatal("the healthy install did not bring durable sessions back")
			}
			if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
				t.Fatal("host status still reports a refusal the daemon has since recovered from")
			}
		})
	}
}

// TestOrdinaryBatchFailuresAreNotReclassifiedAsARefusal is the blast-radius
// guard on the classification above, and it is the one that keeps a transient
// outage transient.
//
// The composition install is one-shot: nothing re-runs it. So a failure that
// this daemon reclassifies as "durable sessions are refused" is a failure the
// host never retries — for the life of the process. A transport blip, a
// deadline, an expired credential, an opaque status refusal, or a commit whose
// outcome was never learned are all recovered by a supervised restart TODAY,
// and every one of them must keep its untyped error so that recovery still
// happens.
func TestOrdinaryBatchFailuresAreNotReclassifiedAsARefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"opaque status refusal", errors.New("adoption batch refused for reasons this daemon cannot read")},
		{"expired host credential", fmt.Errorf("commit: %w", afclient.ErrUnauthorized)},
		{"control plane unavailable", fmt.Errorf("commit: %w", afclient.ErrUnavailable)},
		{"transport failure", fmt.Errorf("commit: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})},
		{"deadline", fmt.Errorf("commit: %w", context.DeadlineExceeded)},
		{
			name: "completeness refusal whose outcome was never learned",
			failure: fmt.Errorf("%w: %w", ErrSessionShimCommitOutcomeUnknown,
				&SessionShimAdoptionBatchLineagesOmitted{Err: errors.New("the batch omitted a live lineage")}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newCompositionHarness(t)
			h.start(ctx)
			cfg := h.composedConfig(func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return SessionShimAdoptionBatchReceipt{}, tc.failure
			})
			err := h.daemon.InstallSessionShimComposition(ctx, cfg)
			if err == nil {
				t.Fatal("an ordinary batch failure reported success")
			}
			var refused *SessionShimDurabilityRefused
			if errors.As(err, &refused) {
				t.Fatalf("a failure a restart recovers from was reclassified as an unresolvable refusal: %v", err)
			}
			if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
				t.Fatal("an ordinary batch failure was recorded as a durability refusal on the status surface")
			}
			if state := h.daemon.State(); state != StateRunning {
				t.Fatalf("daemon state = %q, want %q", state, StateRunning)
			}
		})
	}
}

// TestSessionShimDurabilityRefusedClassification pins the classifier directly,
// including the shapes it must REFUSE to claim.
func TestSessionShimDurabilityRefusedClassification(t *testing.T) {
	id := func(session string) sessionshim.Identity {
		return sessionshim.Identity{OrgID: "org", SessionID: session}
	}
	for _, tc := range []struct {
		name         string
		err          error
		unresolvable bool
		want         string
	}{
		{
			name: "omitted lineages",
			err: &SessionShimAdoptionBatchLineagesOmitted{
				Lineages: []SessionShimOmittedLineage{{Identity: id("b")}, {Identity: id("a")}},
				Err:      errors.New("refused"),
			},
			unresolvable: true,
			want:         "org/a,org/b",
		},
		{
			name: "already-recorded evidence",
			err: &SessionShimAdoptionEvidenceRecorded{
				Lineages: []sessionshim.Identity{id("c")},
				Err:      errors.New("refused"),
			},
			unresolvable: true,
			want:         "org/c",
		},
		{
			name:         "completeness refusal naming nothing",
			err:          &SessionShimAdoptionBatchLineagesOmitted{Err: errors.New("refused")},
			unresolvable: true,
			want:         "none named",
		},
		{name: "opaque refusal", err: errors.New("refused")},
		{name: "auth failure", err: fmt.Errorf("commit: %w", afclient.ErrUnauthorized)},
		{name: "transport", err: fmt.Errorf("commit: %w", &net.OpError{Op: "dial", Err: errors.New("refused")})},
		{name: "deadline", err: fmt.Errorf("commit: %w", context.DeadlineExceeded)},
		{
			name: "typed refusal whose outcome was never learned",
			err: fmt.Errorf("%w: %w", ErrSessionShimCommitOutcomeUnknown,
				&SessionShimAdoptionEvidenceRecorded{Lineages: []sessionshim.Identity{id("d")}, Err: errors.New("refused")}),
		},
		{name: "no error", err: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := newSessionShimDurabilityRefused("org", tc.err)
			if (refused != nil) != tc.unresolvable {
				t.Fatalf("newSessionShimDurabilityRefused(%v) unresolvable = %v, want %v", tc.err, refused != nil, tc.unresolvable)
			}
			if refused == nil {
				return
			}
			if got := refused.LineageIDs(); got != tc.want {
				t.Fatalf("LineageIDs() = %q, want %q", got, tc.want)
			}
			if !errors.Is(refused, tc.err) && !strings.Contains(refused.Error(), "refused") {
				t.Fatalf("the refusal lost its cause: %v", refused)
			}
		})
	}
}

// TestOneQuarantinedLineageKeepsTheRestOfTheHostDurable is strand (B), and the
// one that measured as a host-wide outage.
//
// The refused lineage stages the mandatory carrier Snapshot its takeover would
// stage and is then refused. Pre-fix the quarantine left that stage behind,
// carrier activation refused the whole publication over it, and durable
// sessions went off for every seat on the host — including the healthy one,
// which was never involved.
func TestOneQuarantinedLineageKeepsTheRestOfTheHostDurable(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-quarantine-blast-radius"

	healthy := f.interactiveSpec("lineage-healthy")
	healthy.OrganizationID = orgID
	refused := f.interactiveSpec("lineage-refused")
	refused.OrganizationID = orgID
	for _, spec := range []SessionSpec{healthy, refused} {
		if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
			t.Fatalf("launch %s: %v", spec.SessionID, err)
		}
	}
	f.daemon.ReleaseAdoptedSessionShims()

	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:        true,
			RegistryDir:           f.registry,
			HostID:                "host-quarantine-blast-radius",
			OrgID:                 orgID,
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				mu.Lock()
				batches = append(batches, cloneSessionShimAdoptionBatch(batch))
				mu.Unlock()
				return confirmedReceipt(batch, "revision-blast-radius"), nil
			},
		},
	})
	replacement.opts.SessionShim.OnAdoption = func(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if evidence.Identity.SessionID != refused.SessionID {
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("committed-" + evidence.Identity.SessionID)}, nil
		}
		// What a composing carrier's takeover does before its commit: stage the
		// mandatory Snapshot, then ask the control plane to record the adoption.
		if err := replacement.beginStagedSessionShimSnapshot(evidence.Identity); err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		replacement.retainStagedSessionShimSnapshot(evidence.Identity, sessionshim.ControllerEvent{
			Kind: sessionshim.EventHostFrame, FrameType: attachwire.TypeSnapshot, Seq: 1, RequestID: 1,
		})
		return SessionShimAdoptionReceipt{}, errors.New("the control plane refused this lineage's durable adoption")
	}
	replacement.refreshSessionShimIdentity()
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("one refused lineage withdrew the whole host's durable sessions: %v", err)
	}
	if !replacement.SessionShimAdoptionComplete() {
		t.Fatal("the host's adoption never completed")
	}
	if !replacement.SessionShimCarrierActivationComplete() {
		t.Fatal("carrier activation never settled: a quarantined lineage left its staged Snapshot behind")
	}
	if _, err := replacement.adoptedShimEntry(orgID, healthy.SessionID); err != nil {
		t.Fatalf("the healthy lineage lost its durable adoption over a sibling's refusal: %v", err)
	}
	if _, err := replacement.adoptedShimEntry(orgID, refused.SessionID); err == nil {
		t.Fatal("the refused lineage was composed anyway")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	if len(committed) == 0 {
		t.Fatal("no adoption batch was composed")
	}
	batch := committed[len(committed)-1]
	if _, ok := batchQuarantine(batch, refused.SessionID); !ok {
		t.Fatalf("the refused lineage was omitted from the batch rather than presented quarantined: %+v", batch)
	}
	var adoptedHealthy bool
	for _, outcome := range batch.Adopted {
		if outcome.Evidence.Identity.SessionID == healthy.SessionID {
			adoptedHealthy = true
		}
	}
	if !adoptedHealthy {
		t.Fatalf("the healthy lineage is not adopted in the committed batch: %+v", batch)
	}

	// The refused lineage keeps its harness: released, never killed.
	registry, err := replacement.sessionShimRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	id := sessionshim.Identity{OrgID: orgID, SessionID: refused.SessionID}
	if _, err := registry.Get(id); err != nil {
		t.Fatalf("the refused lineage lost its discovery record — it was stopped, not released: %v", err)
	}
	if _, err := registry.GetTombstone(id); err == nil {
		t.Fatal("the refused lineage has a terminal tombstone — it was reaped, not released")
	}
}

// TestRecomposedDeclarationDoesNotEvictALiveIncarnation is the eviction guard.
//
// A completeness refusal legitimately names an OLDER incarnation of a session
// this boot has just re-adopted at a newer one: the receiver accepts
// `adopted@X` and `quarantined@Y` under one session id as two independent
// lineages, and that is the ordinary controller-handoff shape. A recovery that
// declared by incarnation but EVICTED by session id would answer such a
// refusal by closing this daemon's socket to a healthy shim and publishing a
// projection that contradicts the batch it just committed — which is exactly
// what the receiver demotes a host for.
func TestRecomposedDeclarationDoesNotEvictALiveIncarnation(t *testing.T) {
	f := newShimSpawnFixture(t)
	const (
		orgID      = "org-incarnation-eviction"
		olderShim  = "shim-predecessor"
		olderEpoch = uint64(1)
	)

	live := f.interactiveSpec("lineage-handed-over")
	live.OrganizationID = orgID
	if _, err := f.daemon.spawner.AcceptWork(live); err != nil {
		t.Fatalf("launch: %v", err)
	}
	f.daemon.ReleaseAdoptedSessionShims()

	var (
		mu      sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    f.registry,
			HostID:         "host-incarnation-eviction",
			OrgID:          orgID,
			OnAdoption: func(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("committed-" + evidence.Identity.SessionID)}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				mu.Lock()
				batches = append(batches, cloneSessionShimAdoptionBatch(batch))
				attempt := len(batches)
				mu.Unlock()
				if attempt == 1 {
					// The receiver still holds the PREDECESSOR of the session
					// this boot just re-adopted.
					return SessionShimAdoptionBatchReceipt{}, &SessionShimAdoptionBatchLineagesOmitted{
						Lineages: []SessionShimOmittedLineage{{
							Identity:             sessionshim.Identity{OrgID: orgID, SessionID: live.SessionID},
							ShimID:               olderShim,
							ProcessEpoch:         olderEpoch,
							ControllerGeneration: 1,
						}},
						Err: errors.New("the batch omitted a live lineage"),
					}
				}
				return confirmedReceipt(batch, "revision-incarnation"), nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("a refusal naming a predecessor failed the composition: %v", err)
	}

	entry, err := replacement.adoptedShimEntry(orgID, live.SessionID)
	if err != nil {
		t.Fatalf("the LIVE incarnation was evicted by a refusal about its predecessor: %v", err)
	}
	if entry.adoption.ShimID == olderShim {
		t.Fatal("the fixture did not model two distinct incarnations")
	}

	mu.Lock()
	committed := append([]SessionShimAdoptionBatch(nil), batches...)
	mu.Unlock()
	batch := committed[len(committed)-1]
	var adoptedLive bool
	for _, outcome := range batch.Adopted {
		if outcome.Evidence.Identity.SessionID == live.SessionID && outcome.Evidence.ShimID == entry.adoption.ShimID {
			adoptedLive = true
		}
	}
	if !adoptedLive {
		t.Fatalf("the committed batch dropped the live incarnation it had just adopted: %+v", batch)
	}
	declared, ok := batchQuarantine(batch, live.SessionID)
	if !ok || declared.ShimID != olderShim || declared.ProcessEpoch != olderEpoch {
		t.Fatalf("the predecessor was not declared at its own incarnation: %+v", batch.Quarantined)
	}

	// And the live projection carries BOTH, exactly as the batch does.
	var projectedPredecessor bool
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID == live.SessionID && q.ShimID == olderShim && q.ProcessEpoch == olderEpoch {
			projectedPredecessor = true
		}
	}
	if !projectedPredecessor {
		t.Fatal("the declared predecessor is in the committed batch but missing from the live projection")
	}
	if len(replacement.AdoptedSessionShims()) != 1 {
		t.Fatalf("adopted shims = %d, want the one live incarnation still held",
			len(replacement.AdoptedSessionShims()))
	}
}
