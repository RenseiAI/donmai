package daemon

// Provenance: shim-batch-commit-resilience-2026-08-31 — grep a build for this
// marker to prove it carries the retry-then-restore discipline around a
// dynamic launch's adoption-batch commit.
//
// THE STRAND THIS UNDOES
//
// The control plane's adoption-batch compare-and-swap clears the host's
// durably-published readiness the moment PrepareAdoptionBatch resolves a new
// expected revision — restoring it is the COMMIT's job, not the prepare's.
// Measured live: the commit that followed one prepare came back HTTP 409,
// this daemon surfaced that as an immediately fatal launch failure, and the
// host was left exactly as demoted as the moment prepare ran — every later
// poll refused with the control plane's durable-publication gate until an
// operator restarted the daemon. A fresh boot's own §D4 pass re-ran
// prepare+commit from scratch and committed cleanly, proving the condition
// was transient.
//
// These tests exercise the fix through a real launch (d.spawner.AcceptWork),
// not a direct call into the retry helper, because what has to be pinned is
// the launch's OBSERVABLE outcome: does the session end up adopted, and does
// an unrelated already-adopted session survive a sibling launch's refused
// commit.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// batchCommitRefusal is the fixture's decoded, non-ambiguous refusal: a
// closed-code compare-and-swap conflict answer, the shape a definite HTTP 409
// takes once decoded — never wrapped in ErrSessionShimCommitOutcomeUnknown,
// never a net.Error, never a context error, so sessionShimCommitOutcomeUnknown
// classifies it as OUTCOME-REFUSED rather than ambiguous.
func batchCommitRefusal() error {
	return errors.New("adoption revision compare-and-swap refused (closed code, decoded 4xx)")
}

// recordingBatchCommit is a swappable OnAdoptionBatch fake that records every
// batch it is asked to commit.
type recordingBatchCommit struct {
	mu      sync.Mutex
	batches []SessionShimAdoptionBatch
	commit  func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)
}

func (r *recordingBatchCommit) handle(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
	r.mu.Lock()
	commit := r.commit
	r.batches = append(r.batches, cloneSessionShimAdoptionBatch(batch))
	r.mu.Unlock()
	return commit(batch)
}

func (r *recordingBatchCommit) setCommit(commit func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commit = commit
}

func (r *recordingBatchCommit) snapshot() []SessionShimAdoptionBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SessionShimAdoptionBatch(nil), r.batches...)
}

func batchAdoptsSession(batch SessionShimAdoptionBatch, sessionID string) bool {
	for _, outcome := range batch.Adopted {
		if outcome.Evidence.Identity.SessionID == sessionID {
			return true
		}
	}
	return false
}

// TestLaunchAdoptionBatchCommitRetriesADefiniteRefusalThenSucceeds is FIX 2's
// (a): a commit that comes back refused once, then succeeds, must result in a
// working adoption — not an immediately failed launch.
func TestLaunchAdoptionBatchCommitRetriesADefiniteRefusalThenSucceeds(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-retry-then-succeeds"

	fake := &recordingBatchCommit{}
	var calls atomic.Int64
	fake.setCommit(func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		if calls.Add(1) == 1 {
			return SessionShimAdoptionBatchReceipt{}, batchCommitRefusal()
		}
		return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("revision-retry-succeeds")}, nil
	})
	d.opts.SessionShim.OnAdoptionBatch = fake.handle

	spec := f.interactiveSpec("retry-then-succeeds")
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork with a commit that refuses once then succeeds: %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("OnAdoptionBatch was called %d times, want at least 2 (the refusal, then a retry that commits)", got)
	}
	if _, err := d.adoptedShimEntry(f.orgID, spec.SessionID); err != nil {
		t.Fatalf("session was not adopted after the retried commit succeeded: %v", err)
	}
}

// TestLaunchAdoptionBatchCommitExhaustionRestoresPriorTruthWithoutStranding is
// FIX 2's (b) and (c): a commit that always fails must exhaust its retries,
// fail only the ONE launch it belongs to, and leave the host's actual prior
// truth restored rather than durably-unpublished — an ALREADY-adopted sibling
// session must survive a second session's refused commit.
func TestLaunchAdoptionBatchCommitExhaustionRestoresPriorTruthWithoutStranding(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-exhaustion-restores"

	fake := &recordingBatchCommit{}
	fake.setCommit(func(batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("revision-baseline")}, nil
	})
	d.opts.SessionShim.OnAdoptionBatch = fake.handle

	// A first session adopts cleanly — this is the host's "prior truth" the
	// second session's failed launch must not disturb.
	baseline := f.interactiveSpec("already-adopted")
	if _, err := d.spawner.AcceptWork(baseline); err != nil {
		t.Fatalf("AcceptWork for the baseline session: %v", err)
	}
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("baseline session was not adopted: %v", err)
	}

	// Now every commit is refused — the exact 409-forever shape that stranded
	// the host live.
	var calls atomic.Int64
	fake.setCommit(func(SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		calls.Add(1)
		return SessionShimAdoptionBatchReceipt{}, batchCommitRefusal()
	})

	failing := f.interactiveSpec("commit-always-refused")
	_, err := d.spawner.AcceptWork(failing)
	if err == nil {
		t.Fatal("AcceptWork succeeded despite a batch commit that always refuses")
	}

	if got := calls.Load(); int64(got) < sessionShimAdoptionBatchCommitAttempts+1 {
		t.Fatalf("OnAdoptionBatch was called %d times after exhaustion, want at least attempts+1 restore = %d",
			got, sessionShimAdoptionBatchCommitAttempts+1)
	}

	// The failed session was never adopted.
	if _, err := d.adoptedShimEntry(f.orgID, failing.SessionID); err == nil {
		t.Fatal("the session whose commit always refused was adopted anyway")
	}

	// The best-effort restore attempt is the LAST recorded batch, and it must
	// present the host's truth WITHOUT the session that could not durably
	// commit — otherwise the "restore" is just another attempt to publish the
	// same doomed content.
	batches := fake.snapshot()
	if len(batches) == 0 {
		t.Fatal("no batch was ever committed")
	}
	last := batches[len(batches)-1]
	if batchAdoptsSession(last, failing.SessionID) {
		t.Fatalf("the best-effort restore batch still carried the session whose commit was refused: %+v", last)
	}
	if !batchAdoptsSession(last, baseline.SessionID) {
		t.Fatalf("the best-effort restore batch dropped the already-adopted baseline session: %+v", last)
	}

	// The baseline session survived its sibling's refused commit — the host's
	// ability to claim on ITS behalf was never stranded.
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("the baseline session was collaterally lost after the sibling launch's commit exhausted: %v", err)
	}
}
