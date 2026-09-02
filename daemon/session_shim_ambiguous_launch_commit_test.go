package daemon

// Provenance: shim-ambiguous-launch-lineage-2026-09-02 — grep a build for this
// marker to prove it carries the launching lineage through an adoption-batch
// commit whose outcome was never learned.
//
// THE STRAND THIS UNDOES
//
// Measured on an installed host: a freshly claimed interactive session got as
// far as its per-session durable adoption (the control plane recorded the
// lineage LIVE), and then its adoption-batch commit came back only as
// "context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)" — the request went out, the answer never came back. The daemon
// correctly refused to treat that ambiguity as terminal and armed
// commit-outcome reconciliation. What it did NOT do was record the launching
// lineage anywhere: the launch's rollback restored the last-committed
// projection, which by construction does not contain a session that never
// finished being adopted, and nothing else ever put it back.
//
// From that moment every batch this daemon could compose for the scope OMITS
// a lineage the control plane holds live, so the server's completeness rule
// (adoption_batch_live_lineage_omitted) refuses it — including every
// reconciliation republish, which is the one mechanism that could have
// resolved the ambiguity. The bounded reconciliation therefore exhausted
// against a rule it could not satisfy, no later launch could commit either,
// and the session's row sat in its pre-running state for hours with no
// process on the host and nothing left to release it.
//
// The DEFINITE-refusal path already had exactly this fix
// (restoreSessionShimReadinessAfterExhaustedBatchCommit records the lineage
// into d.shims.quarantined BEFORE sending anything, so every later batch
// carries it). The AMBIGUOUS path did not, and these tests pin it closed on
// the same terms: the reconciliation republish must be a batch the server's
// own completeness rule accepts, and an unrelated later launch must still be
// able to commit.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ambiguousCommitAnswers enumerates the three shapes
// sessionShimCommitOutcomeUnknown classifies as OUTCOME-UNKNOWN, so the fix is
// pinned against the classifier rather than against one error value. The live
// incident produced the third.
func ambiguousCommitAnswers() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"transport answer lost", transportLostCommitAnswer()},
		{"explicit outcome-unknown sentinel", fmt.Errorf("commit adoption batch: %w", ErrSessionShimCommitOutcomeUnknown)},
		{"client deadline around an in-flight commit", fmt.Errorf(
			"request adoption-batches/commit: %w (Client.Timeout exceeded while awaiting headers)", context.DeadlineExceeded)},
	}
}

// launchWithAmbiguousBatchCommit drives one launch whose adoption-batch commit
// answer is lost, after a healthy baseline launch has established the host's
// prior truth. It returns the fixture, the completeness-enforcing fake, and
// the two specs.
func launchWithAmbiguousBatchCommit(t *testing.T, hostID string, ambiguous error) (
	*shimSpawnFixture, *sessionShimBatchCompletenessFake, SessionSpec, SessionSpec,
) {
	t.Helper()
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = hostID
	// Hosted/attested so the retained-authority bookkeeping the republish
	// depends on is real rather than a no-op.
	enableHostedFullHostFramesForTest(t, d, f.orgID)

	fake := &sessionShimBatchCompletenessFake{}
	d.opts.SessionShim.OnAdoption = fake.onAdoption
	d.opts.SessionShim.PrepareAdoptionBatch = fake.prepareBatch
	d.opts.SessionShim.OnAdoptionBatch = fake.onAdoptionBatch

	baseline := f.interactiveSpec("already-adopted")
	if _, err := d.spawner.AcceptWork(baseline); err != nil {
		t.Fatalf("AcceptWork for the baseline session: %v", err)
	}
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("baseline session was not adopted: %v", err)
	}

	// Exactly one lost answer: the resilient retry loop never retries an
	// ambiguous outcome (re-sending a guessed revision would race the one
	// mechanism that can resolve it), so this is the whole injection.
	fake.setAmbiguous(1, ambiguous)
	lost := f.interactiveSpec("commit-answer-lost")
	if _, err := d.spawner.AcceptWork(lost); err == nil {
		t.Fatal("AcceptWork succeeded despite an adoption-batch commit whose outcome was never learned")
	}
	if _, err := d.adoptedShimEntry(f.orgID, lost.SessionID); err == nil {
		t.Fatal("the session whose commit outcome was never learned was tracked as adopted anyway")
	}
	return f, fake, baseline, lost
}

// TestAmbiguousLaunchCommitResolvesToACommittedSnapshot pins the bounded
// resolution path end to end, through both of its doors.
//
// Door one is the launch's own immediate republish: it re-reads the control
// plane's expected revision rather than resending the one that was in flight,
// so a flake that is already over resolves in a single round trip. Door two is
// the reconciliation pass's republish, which is the ONLY channel that can
// resolve the ambiguity when door one also fails. Both compose from the same
// projection, so both must produce a batch the server's completeness rule
// accepts.
//
// RED before the fix: the launching lineage is recorded nowhere, every batch
// composed from that moment on omits a lineage the (fake) control plane marked
// live during the launch's own per-session adoption, and both doors are refused
// adoption_batch_live_lineage_omitted — the bounded reconciliation exhausting
// against a rule no retry could satisfy.
func TestAmbiguousLaunchCommitResolvesToACommittedSnapshot(t *testing.T) {
	for _, tc := range ambiguousCommitAnswers() {
		t.Run(tc.name, func(t *testing.T) {
			f, fake, baseline, lost := launchWithAmbiguousBatchCommit(t, "host-ambiguous-reconcilable", tc.err)
			d := f.daemon

			assertResolvedSnapshot := func(door string, batch SessionShimAdoptionBatch) {
				t.Helper()
				if !batchAdoptsSession(batch, baseline.SessionID) {
					t.Fatalf("%s dropped the already-adopted baseline session: %+v", door, batch)
				}
				quarantine, quarantined := batchQuarantinesSession(batch, lost.SessionID)
				if !quarantined {
					t.Fatalf("%s omits the lineage whose commit outcome was never learned — the exact omission the "+
						"server's adoption_batch_live_lineage_omitted rule refuses: %+v", door, batch)
				}
				if !quarantine.ConsumesCapacity {
					t.Fatalf("%s presents the ambiguous lineage without consuming capacity, so the host would advertise "+
						"a seat it may still be holding: %+v", door, quarantine)
				}
				if batchAdoptsSession(batch, lost.SessionID) {
					t.Fatalf("%s claims durable Adopted status for a lineage whose commit was never confirmed: %+v", door, batch)
				}
			}

			// Door one ran inside the failed launch: its committed batch is
			// the most recent one the fake accepted.
			assertResolvedSnapshot("the launch's immediate republish", fake.lastCommittedBatch(t))

			// Door two: one reconciliation attempt is a credential refresh
			// followed by exactly this republish.
			ctx, cancel := context.WithTimeout(context.Background(), d.sessionShimConfig().adoptionPublicationTimeout())
			defer cancel()
			if err := d.republishSessionShimProjection(ctx, f.orgID); err != nil {
				t.Fatalf("the reconciliation republish — the only path that can resolve an ambiguous commit when the "+
					"immediate one fails — was refused: %v", err)
			}
			assertResolvedSnapshot("the reconciliation republish", fake.lastCommittedBatch(t))
		})
	}
}

// TestAmbiguousLaunchCommitDoesNotStrandTheHost is the operator-visible half:
// once the flake is over, the very next healthy launch must commit. A host
// that cannot commit ANY batch after one lost answer is a host parked forever
// — which is what the incident measured, six and a half hours of it.
//
// RED before the fix: the third launch's batch still omits the ambiguous
// lineage, the completeness rule refuses it, the definite-refusal retry loop
// exhausts, and AcceptWork fails.
func TestAmbiguousLaunchCommitDoesNotStrandTheHost(t *testing.T) {
	for _, tc := range ambiguousCommitAnswers() {
		t.Run(tc.name, func(t *testing.T) {
			f, fake, baseline, lost := launchWithAmbiguousBatchCommit(t, "host-ambiguous-no-strand", tc.err)
			d := f.daemon

			healthy := f.interactiveSpec("healthy-after-ambiguity")
			if _, err := d.spawner.AcceptWork(healthy); err != nil {
				t.Fatalf("an entirely healthy launch failed after an unrelated sibling's commit answer was lost: %v", err)
			}
			if _, err := d.adoptedShimEntry(f.orgID, healthy.SessionID); err != nil {
				t.Fatalf("the healthy session was not adopted: %v", err)
			}

			committed := fake.lastCommittedBatch(t)
			if !batchAdoptsSession(committed, baseline.SessionID) || !batchAdoptsSession(committed, healthy.SessionID) {
				t.Fatalf("the healthy launch's batch = %+v, want both the baseline and the new session adopted", committed)
			}
			if _, quarantined := batchQuarantinesSession(committed, lost.SessionID); !quarantined {
				t.Fatalf("the healthy launch's batch dropped the still-live ambiguous lineage instead of continuing to "+
					"present it: %+v", committed)
			}
			if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
				t.Fatalf("the baseline session was collaterally lost after a sibling launch's ambiguous commit: %v", err)
			}
		})
	}
}

// TestAmbiguousCommitClassificationIsUnchangedForDefiniteRefusals guards the
// blast radius of the fix: a DECODED refusal must keep refusal semantics —
// no ambiguity quarantine, the existing exhaustion restore instead — so the
// new recording cannot be reached by widening the classifier.
func TestAmbiguousCommitClassificationIsUnchangedForDefiniteRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		unknown bool
	}{
		{"decoded compare-and-swap refusal", batchCommitRefusal(), false},
		{"decoded completeness refusal", errors.New("adoption_batch_live_lineage_omitted: sc-refused"), false},
		{"no error at all", nil, false},
		{"transport answer lost", transportLostCommitAnswer(), true},
		{"outcome-unknown sentinel", fmt.Errorf("wrapped: %w", ErrSessionShimCommitOutcomeUnknown), true},
		{"context deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), true},
		{"context cancellation", fmt.Errorf("wrapped: %w", context.Canceled), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionShimCommitOutcomeUnknown(tc.err); got != tc.unknown {
				t.Fatalf("sessionShimCommitOutcomeUnknown(%v) = %v, want %v", tc.err, got, tc.unknown)
			}
		})
	}
}
