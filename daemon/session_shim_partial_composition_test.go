package daemon

// Provenance: shim-partial-composition-2026-08-31 — grep a build for this
// marker to prove it carries per-lineage quarantine in the startup
// composition pass.
//
// THE STRAND THIS UNDOES
//
// adoptSessionShims (§D4) iterates every locally-adoptable lineage and asks
// each one's durable-adoption callback (OnAdoption/OnAdoptionV2) to accept
// it. Pre-fix, ONE lineage's callback returning an error aborted the ENTIRE
// composition (result.Close(); return err) — fail-closed for the whole host,
// not just the one lineage that failed. Measured live: one lineage failed
// with an attachclient v2 durable-high-water/carrier-boundary mismatch (its
// OWN retained state disagreeing with what it now proved), and the
// consequence was that durable sessions came up OFF for the whole host and
// every OTHER adoptable lineage was orphaned — three live sessions lost their
// durable ownership because one unrelated lineage had a stale boundary.
//
// This test pins the fix: a failing lineage's own durable-adoption refusal
// quarantines THAT lineage (typed reason, operator-facing detail, presented —
// not omitted — in the batch's quarantined section) and composition proceeds
// for every other lineage, leaving the host durable-enabled.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestStartupCompositionQuarantinesOneFailedLineageAndComposesTheRest is the
// RED/GREEN target for the fix: two adoptable lineages, one whose durable
// adoption callback refuses it. The good lineage must still compose, the bad
// one must be quarantined with its reason surfaced, the host must end up
// durable-enabled, and the quarantined lineage must still be PRESENTED (not
// omitted) in the committed batch's quarantined section.
func TestStartupCompositionQuarantinesOneFailedLineageAndComposesTheRest(t *testing.T) {
	f := newShimSpawnFixture(t)
	const orgID = "org-partial-composition"

	good := f.interactiveSpec("lineage-good")
	good.OrganizationID = orgID
	bad := f.interactiveSpec("lineage-bad")
	bad.OrganizationID = orgID
	if _, err := f.daemon.spawner.AcceptWork(good); err != nil {
		t.Fatalf("launch the good lineage: %v", err)
	}
	if _, err := f.daemon.spawner.AcceptWork(bad); err != nil {
		t.Fatalf("launch the bad lineage: %v", err)
	}
	// Simulate a daemon restart: both shims keep their harnesses and their
	// discovery records on disk, live for a replacement daemon to adopt.
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
			HostID:         "host-partial-composition",
			OrgID:          orgID,
			OnAdoption: func(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				if evidence.Identity.SessionID == bad.SessionID {
					// The measured live failure shape: a per-lineage durable
					// resume-state mismatch, not a host-identity problem.
					return SessionShimAdoptionReceipt{}, errors.New(
						"attachclient: v2 durable high-water does not match signed carrier boundary")
				}
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("good-lineage-committed")}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				batchMu.Lock()
				batches = append(batches, cloneSessionShimAdoptionBatch(batch))
				batchMu.Unlock()
				return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("batch-partial-composition")}, nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup composition with one bad lineage returned a host-wide failure: %v", err)
	}

	// (c) the host ends up durable-enabled, not fail-closed.
	if !replacement.SessionShimAdoptionComplete() {
		t.Fatal("startup composition with one bad lineage left the host's adoption pass incomplete")
	}

	// (a) the good lineage still composed.
	if _, err := replacement.adoptedShimEntry(orgID, good.SessionID); err != nil {
		t.Fatalf("the good lineage was not composed despite an unrelated sibling's failure: %v", err)
	}
	// The bad lineage did NOT gain controller authority.
	if _, err := replacement.adoptedShimEntry(orgID, bad.SessionID); err == nil {
		t.Fatal("the bad lineage was composed despite its own durable adoption failing")
	}

	// (b) the bad lineage is surfaced as quarantined with a typed reason and
	// an operator-facing detail.
	found := false
	for _, q := range replacement.QuarantinedSessions() {
		if q.SessionID != bad.SessionID {
			continue
		}
		found = true
		if q.Reason != sessionshim.QuarantineAdoptionFailed {
			t.Fatalf("bad lineage quarantine reason = %q, want %q", q.Reason, sessionshim.QuarantineAdoptionFailed)
		}
		if q.Detail == "" {
			t.Fatal("bad lineage quarantine carries no operator-facing detail of what failed")
		}
	}
	if !found {
		t.Fatal("the bad lineage was not surfaced in the live quarantine projection")
	}

	// The committed batch is a COMPLETE snapshot: the bad lineage must be
	// PRESENTED in the quarantined section, never silently omitted.
	batchMu.Lock()
	defer batchMu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("adoption batches committed = %d, want exactly 1", len(batches))
	}
	batch := batches[0]
	if len(batch.Adopted) != 1 || batch.Adopted[0].Evidence.Identity.SessionID != good.SessionID {
		t.Fatalf("batch.Adopted = %+v, want exactly the good lineage", batch.Adopted)
	}
	if len(batch.Quarantined) != 1 || batch.Quarantined[0].SessionID != bad.SessionID {
		t.Fatalf("batch.Quarantined = %+v, want the bad lineage presented, not omitted", batch.Quarantined)
	}
}
