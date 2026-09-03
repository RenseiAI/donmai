package daemon

// Provenance: fresh-dial-boundary-precondition-2026-09-03 — the daemon half of
// the same strand. See session_shim_adoption_redial.go for what it undoes and
// for the disposition contract a re-prepare relies on.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// preparedAsk is one thing the daemon asked the composing authority for.
type preparedAsk struct {
	cause      SessionShimAdoptionPrepareCause
	attempt    int
	generation shimwire.Generation
}

// driftingAdoption is a fake composing authority. Its durable-adoption callback
// refuses with carrier cursor drift — wrapped the way a real caller wraps it —
// until its refuseUntil'th dial; refuseUntil 0 refuses every dial.
type driftingAdoption struct {
	mu    sync.Mutex
	asks  []preparedAsk
	dials int

	refuseUntil int
	// answer, when set, supplies the preparation answer for one attempt. It runs
	// under the lock and is handed the ask, so it can build a value relative to
	// what the daemon actually reported.
	answer func(preparedAsk) (sessionshim.PreparedAdoption, error)
	// beforeRefuse runs on each refused dial, so a fixture can leave behind the
	// state a real refused attempt would have left.
	beforeRefuse func(SessionShimAdoptionEvidence) error
	// onAdopt runs on the dial that succeeds.
	onAdopt func(SessionShimAdoptionEvidence) error
}

func (a *driftingAdoption) prepare(_ context.Context, in SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ask := preparedAsk{cause: in.Cause, attempt: in.Attempt, generation: in.CurrentControllerGeneration}
	a.asks = append(a.asks, ask)
	if a.answer != nil {
		return a.answer(ask)
	}
	return sessionshim.PreparedAdoption{}, nil
}

func (a *driftingAdoption) adopt(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dials++
	if a.refuseUntil == 0 || a.dials < a.refuseUntil {
		if a.beforeRefuse != nil {
			if err := a.beforeRefuse(evidence); err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
		}
		// The exact production shape: the attach client's typed refusal, wrapped
		// by the composing caller with %w.
		return SessionShimAdoptionReceipt{}, fmt.Errorf("dial fresh v2 candidate: %w",
			&attachclient.V2CarrierCursorDriftError{DurableHighWater: 125, CarrierBoundary: 120})
	}
	if a.onAdopt != nil {
		if err := a.onAdopt(evidence); err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
	}
	return SessionShimAdoptionReceipt{DurableCorrelation: []byte("committed-after-redial")}, nil
}

func (a *driftingAdoption) snapshot() ([]preparedAsk, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]preparedAsk(nil), a.asks...), a.dials
}

func newDriftRedialDaemon(t *testing.T, registry, orgID string, adoption *driftingAdoption, batches *[]SessionShimAdoptionBatch, batchMu *sync.Mutex) *Daemon {
	t.Helper()
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:        true,
			RegistryDir:           registry,
			HostID:                "host-drift-redial",
			OrgID:                 orgID,
			PrepareAdoption:       adoption.prepare,
			OnAdoption:            adoption.adopt,
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				batchMu.Lock()
				*batches = append(*batches, cloneSessionShimAdoptionBatch(batch))
				batchMu.Unlock()
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("batch-drift-redial")}, nil
			},
		},
	})
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	return d
}

// launchOneAdoptableLineage launches a lineage and releases it, leaving a live
// harness and an on-disk record for a replacement daemon to adopt.
func launchOneAdoptableLineage(t *testing.T, f *shimSpawnFixture, orgID, sessionID string) SessionSpec {
	t.Helper()
	spec := f.interactiveSpec(sessionID)
	spec.OrganizationID = orgID
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("launch %s: %v", sessionID, err)
	}
	f.daemon.ReleaseAdoptedSessionShims()
	return spec
}

// TestStartupCompositionRedialsADriftedCarrierProofBeforeQuarantining pins the
// recovery-corpus obligation: a refusal classified as carrier cursor drift is
// ambiguous evidence, so the pass re-prepares and dials again. The lineage's
// harness was alive throughout; it must end up ADOPTED, not condemned.
//
// It also pins what the re-prepare must SAY. The composing authority is the
// only party that can dispose of the reservation being superseded, so the ask
// carries the drift cause, its attempt number, and the generation this daemon
// has since committed — and the receipt that reaches the batch must belong to
// the SECOND proof, not the first.
func TestStartupCompositionRedialsADriftedCarrierProofBeforeQuarantining(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-drift-redial"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-drifted")

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	var replacement *Daemon
	id := sessionshim.Identity{OrgID: orgID, SessionID: spec.SessionID}
	// Refuse the first dial, accept the second: exactly the live shape, where a
	// freshly minted proof resolves the disagreement the stale one carried.
	adoption := &driftingAdoption{
		refuseUntil: 2,
		answer: func(ask preparedAsk) (sessionshim.PreparedAdoption, error) {
			if ask.attempt == 1 {
				return sessionshim.PreparedAdoption{Correlation: []byte("candidate-1")}, nil
			}
			// A genuinely FRESH candidate: new correlation, and a generation
			// answer that agrees with what the shim has already committed.
			return sessionshim.PreparedAdoption{Correlation: []byte("candidate-2")}, nil
		},
		// A refused attempt leaves a staged mandatory Snapshot behind — the same
		// state the authoritative-snapshot proxy stages through this primitive.
		// If the retry does not clear it, the next attempt refuses on this
		// daemon's own leftovers rather than on anything the carrier said.
		beforeRefuse: func(SessionShimAdoptionEvidence) error {
			return replacement.beginStagedSessionShimSnapshot(id)
		},
		onAdopt: func(SessionShimAdoptionEvidence) error {
			// Proves the retry released what the refused attempt reserved.
			if err := replacement.beginStagedSessionShimSnapshot(id); err != nil {
				return fmt.Errorf("the re-dial inherited the refused attempt's staged Snapshot: %w", err)
			}
			replacement.cancelStagedSessionShimSnapshot(id)
			return nil
		},
	}
	replacement = newDriftRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	asks, dials := adoption.snapshot()
	if dials != 2 {
		t.Fatalf("durable adoption dials = %d, want 2 (the drifted one, then the re-dial)", dials)
	}
	if len(asks) != 2 {
		t.Fatalf("preparation asks = %+v, want 2 — a re-dial on the SAME stale proof cannot resolve drift", asks)
	}
	if asks[0].cause != SessionShimPrepareCauseInitial || asks[0].attempt != 1 {
		t.Fatalf("first ask = %+v, want the initial cause at attempt 1", asks[0])
	}
	// The authority cannot dispose of the reservation it is superseding unless
	// it is told this is a supersession.
	if asks[1].cause != SessionShimPrepareCauseCarrierCursorDrift || asks[1].attempt != 2 {
		t.Fatalf("re-prepare ask = %+v, want the drift cause at attempt 2", asks[1])
	}
	entry, err := replacement.adoptedShimEntry(orgID, spec.SessionID)
	if err != nil {
		t.Fatalf("the lineage was quarantined over a drifted proof the pass could have re-prepared: %v", err)
	}
	// The re-prepare reports what is true NOW: the committed controller
	// generation, which is strictly past the one the first ask carried.
	if asks[1].generation != entry.controller.Generation() {
		t.Fatalf("re-prepare reported generation %d, want the committed %d",
			asks[1].generation, entry.controller.Generation())
	}
	if asks[1].generation <= asks[0].generation {
		t.Fatalf("re-prepare reported generation %d, want it past the first ask's %d",
			asks[1].generation, asks[0].generation)
	}
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID == spec.SessionID {
			t.Fatalf("the re-adopted lineage is still surfaced as quarantined (%s: %s)", q.Reason, q.Detail)
		}
	}

	batchMu.Lock()
	defer batchMu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("adoption batches committed = %d, want exactly 1", len(batches))
	}
	batch := batches[0]
	if len(batch.Adopted) != 1 || batch.Adopted[0].Evidence.Identity.SessionID != spec.SessionID {
		t.Fatalf("batch.Adopted = %+v, want the re-dialled lineage", batch.Adopted)
	}
	// The committed evidence must belong to the SECOND proof. Binding the new
	// receipt to the superseded candidate's correlation is the silent version of
	// the bug this whole path exists to fix.
	if string(batch.Adopted[0].Evidence.PreparedCorrelation) != "candidate-2" {
		t.Fatalf("batch carries prepared correlation %q, want the re-prepared candidate's",
			batch.Adopted[0].Evidence.PreparedCorrelation)
	}
	if string(batch.Adopted[0].Receipt.DurableCorrelation) != "committed-after-redial" {
		t.Fatalf("batch carries receipt %q, want the one the successful re-dial returned",
			batch.Adopted[0].Receipt.DurableCorrelation)
	}
	if len(batch.Quarantined) != 0 {
		t.Fatalf("batch.Quarantined = %+v, want empty", batch.Quarantined)
	}
}

// TestStartupCompositionRefusesAReprepareTheControllerCannotHonour pins B2's
// half: the first preparation is answered inside the handshake, where its
// generation and cursor still have a Welcome to travel on. A re-prepare has
// neither. An answer that resolves a generation the shim did not commit cannot
// be applied — and must not be silently dropped, because the receipt would then
// bind the SECOND proof to the FIRST generation.
func TestStartupCompositionRefusesAReprepareTheControllerCannotHonour(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-drift-unhonourable"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-unhonourable")

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	adoption := &driftingAdoption{
		refuseUntil: 2,
		answer: func(ask preparedAsk) (sessionshim.PreparedAdoption, error) {
			if ask.attempt == 1 {
				return sessionshim.PreparedAdoption{}, nil
			}
			// Resolves a generation the adopted controller does not hold.
			return sessionshim.PreparedAdoption{ControllerGeneration: ask.generation + 7}, nil
		},
	}
	replacement := newDriftRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err == nil {
		t.Fatal("a receipt was bound to a re-prepared proof the adopted controller cannot honour")
	}
	_, dials := adoption.snapshot()
	if dials != 1 {
		t.Fatalf("durable adoption dials = %d, want 1 — an unusable answer is not dialled", dials)
	}
	found := false
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID != spec.SessionID {
			continue
		}
		found = true
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("quarantine reason = %q, want %q", q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		for _, want := range []string{"unusable", "does not match the committed generation"} {
			if !strings.Contains(q.Detail, want) {
				t.Fatalf("quarantine detail %q does not say the re-prepared answer was unusable (%q)", q.Detail, want)
			}
		}
	}
	if !found {
		t.Fatal("the lineage was not surfaced in the live quarantine projection")
	}
}

// TestStartupCompositionStopsRepreparingOnAControlPlaneConflict pins the other
// disposition: an authority that will NOT supersede its outstanding reservation
// answers a typed conflict. That is a refusal, not a failure — re-asking cannot
// change it, and spending the rest of the budget would only pile more asks onto
// the same conflict.
func TestStartupCompositionStopsRepreparingOnAControlPlaneConflict(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-drift-conflict"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-conflicted")

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	adoption := &driftingAdoption{
		answer: func(ask preparedAsk) (sessionshim.PreparedAdoption, error) {
			if ask.attempt == 1 {
				return sessionshim.PreparedAdoption{}, nil
			}
			return sessionshim.PreparedAdoption{}, fmt.Errorf(
				"%w: a reservation for this lineage is already admitted",
				ErrSessionShimAdoptionPrepareConflict,
			)
		},
	}
	replacement := newDriftRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	asks, dials := adoption.snapshot()
	if len(asks) != 2 {
		t.Fatalf("preparation asks = %d, want 2 — a conflict ends the budget, it does not spend it", len(asks))
	}
	if dials != 1 {
		t.Fatalf("durable adoption dials = %d, want 1 — a conflicted re-prepare is never dialled", dials)
	}
	found := false
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID != spec.SessionID {
			continue
		}
		found = true
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("quarantine reason = %q, want %q", q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		for _, want := range []string{"refused", "already admitted", "125", "120"} {
			if !strings.Contains(q.Detail, want) {
				t.Fatalf("quarantine detail %q does not carry %q", q.Detail, want)
			}
		}
	}
	if !found {
		t.Fatal("the conflicted lineage was not surfaced in the live quarantine projection")
	}
}

// TestStartupCompositionQuarantinesDriftOnlyAfterTheBound pins the other side:
// the retry is BOUNDED. A proof that never reconciles still ends in quarantine,
// with the typed reason and a detail naming both cursors and the dials spent.
func TestStartupCompositionQuarantinesDriftOnlyAfterTheBound(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-drift-exhausted"
	spec := launchOneAdoptableLineage(t, f, orgID, "lineage-unreconciled")

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	adoption := &driftingAdoption{} // refuseUntil 0 — every dial drifts
	replacement := newDriftRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	// The bound is pinned BY VALUE, not by reading the constant back: a test
	// phrased against sessionShimDriftRedialAttempts moves with it and would
	// call an unbounded retry "bounded". Widening the budget is a deliberate
	// change that has to move this literal too.
	const wantAttempts = 3
	if sessionShimDriftRedialAttempts != wantAttempts {
		t.Fatalf("the drift re-dial budget is %d, want %d", sessionShimDriftRedialAttempts, wantAttempts)
	}
	asks, dials := adoption.snapshot()
	if dials != wantAttempts {
		t.Fatalf("durable adoption dials = %d, want the bound %d", dials, wantAttempts)
	}
	if len(asks) != wantAttempts {
		t.Fatalf("preparation asks = %d, want %d — each re-dial needs its own fresh proof", len(asks), wantAttempts)
	}
	for i, ask := range asks[1:] {
		if ask.cause != SessionShimPrepareCauseCarrierCursorDrift || ask.attempt != i+2 {
			t.Fatalf("re-prepare ask %d = %+v, want the drift cause at attempt %d", i+2, ask, i+2)
		}
	}
	if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err == nil {
		t.Fatal("a lineage whose proof never reconciled was composed anyway")
	}

	found := false
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID != spec.SessionID {
			continue
		}
		found = true
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("quarantine reason = %q, want %q", q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		for _, want := range []string{"125", "120", fmt.Sprint(wantAttempts)} {
			if !strings.Contains(q.Detail, want) {
				t.Fatalf("quarantine detail %q does not carry %q", q.Detail, want)
			}
		}
	}
	if !found {
		t.Fatal("the unreconciled lineage was not surfaced in the live quarantine projection")
	}

	batchMu.Lock()
	defer batchMu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("adoption batches committed = %d, want exactly 1", len(batches))
	}
	if len(batches[0].Quarantined) != 1 || batches[0].Quarantined[0].SessionID != spec.SessionID {
		t.Fatalf("batch.Quarantined = %+v, want the unreconciled lineage presented", batches[0].Quarantined)
	}
}
