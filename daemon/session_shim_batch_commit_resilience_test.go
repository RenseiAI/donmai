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
// WHY THE FAKE MODELS COMPLETENESS
//
// A first version of these tests wired only OnAdoptionBatch, leaving
// OnAdoption/PrepareAdoptionBatch nil. That fake could not see the bug it was
// meant to catch: by the time the exhaustion-restore path runs,
// d.completeSessionShimAdoption has ALREADY succeeded for the launching
// session — the control plane holds a live per-session adoption record for
// it, independent of the batch. The server refuses ANY batch that omits a
// live lineage (adoption_batch_live_lineage_omitted), and the first version
// of the restore batch did exactly that (sessionShimProjectionBatch alone
// excludes the launching session, because trackLaunchedShim has not run
// yet). A fake with no completeness rule cannot refuse an omission, so it
// happily accepted the very batch the real server would refuse — the test
// passed for the wrong reason.
//
// sessionShimBatchCompletenessFake below fixes that: onAdoption marks a
// lineage "live" the moment the (fake) control plane would, and
// onAdoptionBatch refuses any batch that does not present every live lineage
// in Adopted or Quarantined — the same rule the real server enforces. These
// tests exercise the fix through a real launch (d.spawner.AcceptWork), not a
// direct call into the retry helper, because what has to be pinned is the
// launch's OBSERVABLE outcome: does the session end up adopted, does the
// restore batch actually get ACCEPTED by a completeness-enforcing fake
// (not just sent), and does an unrelated already-adopted session survive a
// sibling launch's refused commit.
//
// OnAdoptionPublished is deliberately left unwired here: setting it would
// additionally arm the heartbeat-barrier/checkpoint-rollback machinery in
// launchSessionShim's serializedPublication branch, a SEPARATE pre-existing
// resilience path this PR does not touch — mixing it in would test two
// mechanisms at once instead of pinning this one.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// batchCommitRefusal is the fixture's decoded, non-ambiguous refusal: a
// closed-code compare-and-swap conflict answer, the shape a definite HTTP 409
// takes once decoded — never wrapped in ErrSessionShimCommitOutcomeUnknown,
// never a net.Error, never a context error, so sessionShimCommitOutcomeUnknown
// classifies it as OUTCOME-REFUSED rather than ambiguous.
func batchCommitRefusal() error {
	return errors.New("adoption revision compare-and-swap refused (closed code, decoded 4xx)")
}

// batchCommitResult pairs one committed batch with the fake's answer to it.
type batchCommitResult struct {
	batch SessionShimAdoptionBatch
	err   error
}

// sessionShimBatchCompletenessFake models the server's completeness rule
// (adoption_batch_live_lineage_omitted): once onAdoption durably records a
// lineage — mirroring the real d.completeSessionShimAdoption →
// cfg.OnAdoption round trip — every LATER batch commit must present that
// lineage, in Adopted or Quarantined, or the whole batch is refused. It also
// injects a bounded number of synthetic refusals on demand, reproducing a
// transient control-plane compare-and-swap conflict independent of
// completeness.
type sessionShimBatchCompletenessFake struct {
	mu                sync.Mutex
	live              map[string]bool
	refusalsRemaining int
	revision          atomic.Int64
	results           []batchCommitResult
}

func (f *sessionShimBatchCompletenessFake) onAdoption(_ context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	f.mu.Lock()
	if f.live == nil {
		f.live = make(map[string]bool)
	}
	f.live[evidence.Identity.SessionID] = true
	f.mu.Unlock()
	return SessionShimAdoptionReceipt{DurableCorrelation: []byte("adopted-" + evidence.Identity.SessionID)}, nil
}

func (f *sessionShimBatchCompletenessFake) prepareBatch(context.Context, string, string) ([]byte, error) {
	return []byte(fmt.Sprintf("revision-%d", f.revision.Add(1))), nil
}

// setRefusals arms n leading onAdoptionBatch calls to answer with a
// synthetic, decoded, non-ambiguous refusal regardless of content —
// reproducing a transient compare-and-swap conflict. Calls after the n are
// exhausted fall through to the completeness check.
func (f *sessionShimBatchCompletenessFake) setRefusals(n int) {
	f.mu.Lock()
	f.refusalsRemaining = n
	f.mu.Unlock()
}

func (f *sessionShimBatchCompletenessFake) onAdoptionBatch(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record := func(err error) (SessionShimAdoptionBatchReceipt, error) {
		f.results = append(f.results, batchCommitResult{batch: cloneSessionShimAdoptionBatch(batch), err: err})
		if err != nil {
			return SessionShimAdoptionBatchReceipt{}, err
		}
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte(fmt.Sprintf("batch-revision-%d", f.revision.Load())),
			AdoptionRevision:   fmt.Sprintf("revision-%d", f.revision.Load()),
		}, nil
	}
	if f.refusalsRemaining > 0 {
		f.refusalsRemaining--
		return record(batchCommitRefusal())
	}
	present := make(map[string]bool, len(batch.Adopted)+len(batch.Quarantined))
	for _, outcome := range batch.Adopted {
		present[outcome.Evidence.Identity.SessionID] = true
	}
	for _, q := range batch.Quarantined {
		present[q.SessionID] = true
	}
	for sessionID := range f.live {
		if !present[sessionID] {
			// The server's actual closed conflict code, reproduced verbatim
			// so a RED run's log line is directly comparable to the live one.
			return record(fmt.Errorf("adoption_batch_live_lineage_omitted: sc-refused (session %s)", sessionID))
		}
	}
	return record(nil)
}

func (f *sessionShimBatchCompletenessFake) snapshot() []batchCommitResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]batchCommitResult(nil), f.results...)
}

func batchAdoptsSession(batch SessionShimAdoptionBatch, sessionID string) bool {
	for _, outcome := range batch.Adopted {
		if outcome.Evidence.Identity.SessionID == sessionID {
			return true
		}
	}
	return false
}

func batchQuarantinesSession(batch SessionShimAdoptionBatch, sessionID string) (sessionshim.QuarantinedSession, bool) {
	for _, q := range batch.Quarantined {
		if q.SessionID == sessionID {
			return q, true
		}
	}
	return sessionshim.QuarantinedSession{}, false
}

// TestLaunchAdoptionBatchCommitRetriesADefiniteRefusalThenSucceeds is FIX 2's
// (a): a commit that comes back refused once, then succeeds, must result in a
// working adoption — not an immediately failed launch. The fake enforces
// completeness throughout, so a pass here proves the retried batch is one the
// real server's own rule would actually accept, not merely one the daemon
// stopped resending.
func TestLaunchAdoptionBatchCommitRetriesADefiniteRefusalThenSucceeds(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-retry-then-succeeds"

	fake := &sessionShimBatchCompletenessFake{}
	d.opts.SessionShim.OnAdoption = fake.onAdoption
	d.opts.SessionShim.PrepareAdoptionBatch = fake.prepareBatch
	d.opts.SessionShim.OnAdoptionBatch = fake.onAdoptionBatch
	fake.setRefusals(1)

	spec := f.interactiveSpec("retry-then-succeeds")
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork with a commit that refuses once then succeeds: %v", err)
	}
	results := fake.snapshot()
	if len(results) < 2 {
		t.Fatalf("OnAdoptionBatch was called %d times, want at least 2 (the refusal, then a retry that commits)", len(results))
	}
	last := results[len(results)-1]
	if last.err != nil {
		t.Fatalf("final retried batch was refused by the completeness-enforcing fake: %v", last.err)
	}
	if !batchAdoptsSession(last.batch, spec.SessionID) {
		t.Fatalf("committed batch %+v does not adopt %s", last.batch, spec.SessionID)
	}
	if _, err := d.adoptedShimEntry(f.orgID, spec.SessionID); err != nil {
		t.Fatalf("session was not adopted after the retried commit succeeded: %v", err)
	}
}

// TestLaunchAdoptionBatchCommitExhaustionRestoresPriorTruthWithoutStranding is
// FIX 2's (b) and (c): a commit that always fails must exhaust its retries,
// fail only the ONE launch it belongs to, and leave the host's actual prior
// truth restored — through a batch the completeness-enforcing fake actually
// ACCEPTS, not one it refuses for omitting the very lineage the control plane
// already considers live. An ALREADY-adopted sibling session must survive a
// second session's refused commit.
func TestLaunchAdoptionBatchCommitExhaustionRestoresPriorTruthWithoutStranding(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-exhaustion-restores"

	fake := &sessionShimBatchCompletenessFake{}
	d.opts.SessionShim.OnAdoption = fake.onAdoption
	d.opts.SessionShim.PrepareAdoptionBatch = fake.prepareBatch
	d.opts.SessionShim.OnAdoptionBatch = fake.onAdoptionBatch

	// A first session adopts cleanly — this is the host's "prior truth" the
	// second session's failed launch must not disturb. No refusals armed yet.
	baseline := f.interactiveSpec("already-adopted")
	if _, err := d.spawner.AcceptWork(baseline); err != nil {
		t.Fatalf("AcceptWork for the baseline session: %v", err)
	}
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("baseline session was not adopted: %v", err)
	}

	// Every retry of the next launch's batch commit is synthetically refused
	// — the exact 409-forever shape that stranded the host live. The
	// exhaustion restore attempt that follows is the (attempts+1)th call and
	// is NOT covered by this count, so it reaches the completeness check for
	// real.
	fake.setRefusals(sessionShimAdoptionBatchCommitAttempts)

	failing := f.interactiveSpec("commit-always-refused")
	_, err := d.spawner.AcceptWork(failing)
	if err == nil {
		t.Fatal("AcceptWork succeeded despite a batch commit that always refuses")
	}

	results := fake.snapshot()
	if got := len(results); got < sessionShimAdoptionBatchCommitAttempts+1 {
		t.Fatalf("OnAdoptionBatch was called %d times after exhaustion, want at least attempts+1 restore = %d",
			got, sessionShimAdoptionBatchCommitAttempts+1)
	}

	// The failed session was never adopted.
	if _, err := d.adoptedShimEntry(f.orgID, failing.SessionID); err == nil {
		t.Fatal("the session whose commit always refused was adopted anyway")
	}

	// The best-effort restore attempt is the LAST recorded call, and — this
	// is the reviewer's spot-check — it must be ACCEPTED by the
	// completeness-enforcing fake, not refused for omitting a lineage the
	// fake's onAdoption already marked live.
	last := results[len(results)-1]
	if last.err != nil {
		t.Fatalf("best-effort restore batch was refused by the completeness-enforcing fake: %v", last.err)
	}
	if batchAdoptsSession(last.batch, failing.SessionID) {
		t.Fatalf("the best-effort restore batch claimed durable Adopted status for the session whose commit was refused: %+v", last.batch)
	}
	quarantine, quarantined := batchQuarantinesSession(last.batch, failing.SessionID)
	if !quarantined {
		t.Fatalf("the best-effort restore batch omitted the refused session entirely (the exact omission the server's "+
			"adoption_batch_live_lineage_omitted rule refuses): %+v", last.batch)
	}
	if !quarantine.ConsumesCapacity {
		t.Fatalf("the refused session's restore-quarantine entry does not consume capacity: %+v", quarantine)
	}
	if !batchAdoptsSession(last.batch, baseline.SessionID) {
		t.Fatalf("the best-effort restore batch dropped the already-adopted baseline session: %+v", last.batch)
	}

	// The baseline session survived its sibling's refused commit — the host's
	// ability to claim on ITS behalf was never stranded.
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("the baseline session was collaterally lost after the sibling launch's commit exhausted: %v", err)
	}
}
