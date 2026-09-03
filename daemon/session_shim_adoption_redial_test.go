package daemon

// Provenance: fresh-dial-boundary-precondition-2026-09-03 — the daemon half of
// the same strand. See session_shim_adoption_redial.go for what it undoes.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

// driftingAdoption is a durable-adoption callback that refuses with carrier
// cursor drift — wrapped the way a composing caller wraps it — until its
// refuseUntil'th dial. refuseUntil 0 refuses every dial.
type driftingAdoption struct {
	mu               sync.Mutex
	prepares         int
	dials            int
	refuseUntil      int
	preparedResumeAt []uint64
}

func (a *driftingAdoption) prepare(_ context.Context, in SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prepares++
	a.preparedResumeAt = append(a.preparedResumeAt, in.LocalResumeFrom)
	return sessionshim.PreparedAdoption{}, nil
}

func (a *driftingAdoption) adopt(_ context.Context, _ SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dials++
	if a.refuseUntil == 0 || a.dials < a.refuseUntil {
		// The exact production shape: the attach client's typed refusal, wrapped
		// by the composing caller with %w.
		return SessionShimAdoptionReceipt{}, fmt.Errorf("dial fresh v2 candidate: %w",
			&attachclient.V2CarrierCursorDriftError{DurableHighWater: 125, CarrierBoundary: 120})
	}
	return SessionShimAdoptionReceipt{DurableCorrelation: []byte("committed-after-redial")}, nil
}

func (a *driftingAdoption) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prepares, a.dials
}

// resumeFloors is the local acknowledgement floor each prepare was asked
// against, in order.
func (a *driftingAdoption) resumeFloors() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.preparedResumeAt...)
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

// TestStartupCompositionRedialsADriftedCarrierProofBeforeQuarantining pins the
// recovery-corpus obligation: a refusal classified as carrier cursor drift is
// ambiguous evidence, so the pass re-prepares and dials again. The lineage's
// harness was alive throughout; it must end up ADOPTED, not condemned.
func TestStartupCompositionRedialsADriftedCarrierProofBeforeQuarantining(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-drift-redial"

	spec := f.interactiveSpec("lineage-drifted")
	spec.OrganizationID = orgID
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("launch the drifted lineage: %v", err)
	}
	f.daemon.ReleaseAdoptedSessionShims()

	var (
		batchMu sync.Mutex
		batches []SessionShimAdoptionBatch
	)
	// Refuse the first dial, accept the second: exactly the live shape, where a
	// freshly minted proof resolves the disagreement the stale one carried.
	adoption := &driftingAdoption{refuseUntil: 2}
	replacement := newDriftRedialDaemon(t, f.registry, orgID, adoption, &batches, &batchMu)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition returned a host-wide failure: %v", err)
	}

	prepares, dials := adoption.counts()
	if dials != 2 {
		t.Fatalf("durable adoption dials = %d, want 2 (the drifted one, then the re-dial)", dials)
	}
	if prepares != 2 {
		t.Fatalf("adoption prepares = %d, want 2 — a re-dial on the SAME stale proof cannot resolve drift", prepares)
	}
	// A second prepare must be asked against evidence that is at least as
	// advanced as the first. Both cursors it carries only ever rise, so a
	// re-prepare that regressed one would ask the authority to mint a proof
	// BELOW the reservation it already admitted.
	floors := adoption.resumeFloors()
	if len(floors) != 2 || floors[1] < floors[0] {
		t.Fatalf("prepared local resume floors = %v, want two, the second not regressing the first", floors)
	}
	if _, err := replacement.adoptedShimEntry(orgID, spec.SessionID); err != nil {
		t.Fatalf("the lineage was quarantined over a drifted proof the pass could have re-prepared: %v", err)
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
	if string(batch.Adopted[0].Receipt.DurableCorrelation) != "committed-after-redial" {
		t.Fatalf("batch carries receipt %q, want the one the successful re-dial returned",
			batch.Adopted[0].Receipt.DurableCorrelation)
	}
	if len(batch.Quarantined) != 0 {
		t.Fatalf("batch.Quarantined = %+v, want empty", batch.Quarantined)
	}
}

// TestStartupCompositionQuarantinesDriftOnlyAfterTheBound pins the other side:
// the retry is BOUNDED. A proof that never reconciles still ends in quarantine,
// with the typed reason and a detail naming both cursors and the dials spent.
func TestStartupCompositionQuarantinesDriftOnlyAfterTheBound(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-drift-exhausted"

	spec := f.interactiveSpec("lineage-unreconciled")
	spec.OrganizationID = orgID
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("launch the drifted lineage: %v", err)
	}
	f.daemon.ReleaseAdoptedSessionShims()

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
	prepares, dials := adoption.counts()
	if dials != wantAttempts {
		t.Fatalf("durable adoption dials = %d, want the bound %d", dials, wantAttempts)
	}
	if prepares != wantAttempts {
		t.Fatalf("adoption prepares = %d, want %d — each re-dial needs its own fresh proof",
			prepares, wantAttempts)
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
