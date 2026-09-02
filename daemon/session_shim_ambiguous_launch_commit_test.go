package daemon

// Provenance: shim-ambiguous-launch-lineage-2026-09-02 — grep a build for this
// marker to prove it carries a definite disposition for a launch whose
// adoption-batch commit outcome was never learned.
//
// THE STRAND THIS UNDOES
//
// Measured on an installed host: a freshly claimed interactive session got as
// far as its per-session durable adoption (the control plane recorded the
// lineage LIVE) and then its adoption-batch commit came back only as a client
// deadline — the request went out, the answer never came back. The daemon
// correctly refused to treat that ambiguity as terminal, armed the
// asynchronous commit-outcome reconciliation, closed the controller and
// returned a launch failure. Three things were then true at once:
//
//   - The launching lineage was recorded NOWHERE. The rollback restores the
//     last-committed projection, which by construction cannot hold a session
//     whose adoption never finished, and trackLaunchedShim had not run. Every
//     batch the daemon could compose from then on omitted a lineage the control
//     plane held live, so the completeness rule refused it —
//     INCLUDING every reconciliation republish, the one mechanism that could
//     have resolved the ambiguity, and including the reconcile that consumes
//     terminal tombstones, which iterates exactly the quarantine set.
//   - The harness kept running. A controller Close explicitly does not stop the
//     session; the shim keeps its harness and starts its own orphan clock. So
//     the spawner's "aborted" report was false for as long as that clock ran.
//   - The session's recovery obligation was never discharged, so the row could
//     not terminalize.
//
// These tests pin the replacement contract: the ambiguity is driven to a
// DEFINITE outcome inside the launch, and the launch returns either a live
// adoption or a failure whose statement is true — lineage published, harness
// stopped, obligation discharged through the shim's own terminal proof.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// ambiguousCommitAnswers enumerates the three shapes
// sessionShimCommitOutcomeUnknown classifies as OUTCOME-UNKNOWN, so the
// behaviour is pinned against the classifier rather than one error value. The
// live incident produced the third.
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

// terminalEvidenceRecorder is the composer's seat for the obligation-side
// call. Discharging a recovery obligation IS this callback firing for the
// exact incarnation: nothing else the daemon does releases the platform's
// restart fence.
type terminalEvidenceRecorder struct {
	mu   sync.Mutex
	seen []SessionShimTerminalEvidence
}

func (r *terminalEvidenceRecorder) record(_ context.Context, evidence SessionShimTerminalEvidence) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, evidence)
	return nil
}

// forSession returns every terminal report made for one session id.
func (r *terminalEvidenceRecorder) forSession(sessionID string) []SessionShimTerminalEvidence {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SessionShimTerminalEvidence
	for _, evidence := range r.seen {
		if evidence.Identity.SessionID == sessionID {
			out = append(out, evidence)
		}
	}
	return out
}

// ambiguousLaunchFixture is one hosted daemon with a completeness-enforcing
// control plane, a recorded terminal-evidence seat, and one already-adopted
// session standing in for the host's prior truth.
type ambiguousLaunchFixture struct {
	*shimSpawnFixture
	fake     *sessionShimBatchCompletenessFake
	terminal *terminalEvidenceRecorder
	baseline SessionSpec
}

func newAmbiguousLaunchFixture(t *testing.T, hostID string) *ambiguousLaunchFixture {
	t.Helper()
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = hostID
	// The release path's wait for terminal proof is sized from the callback
	// timeout, which otherwise defaults to the 60s launch timeout here and
	// would put a two-minute bound on a test. Shrinking the ONE unit keeps
	// every bound derived from it test-sized without inventing a second number.
	d.opts.SessionShim.CallbackTimeout = 200 * time.Millisecond
	// Hosted/attested so the retained-authority bookkeeping a committed batch
	// advances is real rather than a no-op.
	enableHostedFullHostFramesForTest(t, d, f.orgID)

	fake := &sessionShimBatchCompletenessFake{}
	terminal := &terminalEvidenceRecorder{}
	d.opts.SessionShim.OnAdoption = fake.onAdoption
	d.opts.SessionShim.PrepareAdoptionBatch = fake.prepareBatch
	d.opts.SessionShim.OnAdoptionBatch = fake.onAdoptionBatch
	d.opts.SessionShim.OnTerminalEvidence = terminal.record

	baseline := f.interactiveSpec("already-adopted")
	if _, err := d.spawner.AcceptWork(baseline); err != nil {
		t.Fatalf("AcceptWork for the baseline session: %v", err)
	}
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("baseline session was not adopted: %v", err)
	}
	return &ambiguousLaunchFixture{shimSpawnFixture: f, fake: fake, terminal: terminal, baseline: baseline}
}

// TestAmbiguousLaunchCommitReDrivesToACommittedAdoption is the "the control
// plane DID commit" branch, and the reason the re-drive is worth doing at all:
// the overwhelmingly common cause of a lost answer is a transient flake that is
// already over by the time the daemon can ask again. Re-composing the COMPLETE
// projection and re-reading the expected revision through PrepareAdoptionBatch
// resolves it in one round trip, and the launch continues as a normal adoption
// — live session, adopted entry, no quarantine, no release.
//
// RED before the fix: the launch failed outright on the lost answer.
func TestAmbiguousLaunchCommitReDrivesToACommittedAdoption(t *testing.T) {
	for _, tc := range ambiguousCommitAnswers() {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmbiguousLaunchFixture(t, "host-ambiguous-redrive")
			d := f.daemon

			// One lost answer, then an honest control plane.
			f.fake.setAnswers(tc.err)
			launched := f.interactiveSpec("commit-answer-lost")
			if _, err := d.spawner.AcceptWork(launched); err != nil {
				t.Fatalf("AcceptWork failed even though the control plane answered the very next commit: %v", err)
			}
			if _, err := d.adoptedShimEntry(f.orgID, launched.SessionID); err != nil {
				t.Fatalf("the re-driven session was not adopted, so the launch did not continue the normal adoption: %v", err)
			}

			committed := f.fake.lastCommittedBatch(t)
			if !batchAdoptsSession(committed, launched.SessionID) {
				t.Fatalf("the re-driven batch does not adopt the session: %+v", committed)
			}
			if !batchAdoptsSession(committed, f.baseline.SessionID) {
				t.Fatalf("the re-driven batch dropped the already-adopted baseline session: %+v", committed)
			}
			if _, quarantined := batchQuarantinesSession(committed, launched.SessionID); quarantined {
				t.Fatalf("a session that committed on the re-drive was published quarantined anyway: %+v", committed)
			}
			// A committed adoption must not be reported terminal: the session
			// is running.
			if reports := f.terminal.forSession(launched.SessionID); len(reports) != 0 {
				t.Fatalf("terminal evidence was reported for a session that adopted successfully: %+v", reports)
			}
		})
	}
}

// releaseCase is one way the re-drive can end in a definite "not adopted".
type releaseCase struct {
	name    string
	answers func(ambiguous error) []error
}

// releaseCases are the two definite non-adoptions. Both must release
// identically: the difference between them is what the control plane said, not
// what this host owes.
func releaseCases() []releaseCase {
	return []releaseCase{
		{
			name: "decoded refusal on the re-drive",
			answers: func(ambiguous error) []error {
				// The lost answer, then a decoded refusal: the control plane
				// did not commit, and said so. The re-drive stops there.
				return []error{ambiguous, batchCommitRefusal()}
			},
		},
		{
			name: "never resolved inside the derived bound",
			answers: func(ambiguous error) []error {
				// The lost answer and then nothing but lost answers, for the
				// whole derived bound. The daemon never learns the outcome and
				// must not report an adoption it cannot prove.
				answers := make([]error, 0, sessionShimAdoptionPublicationStages+1)
				for i := 0; i <= sessionShimAdoptionPublicationStages; i++ {
					answers = append(answers, ambiguous)
				}
				return answers
			},
		},
	}
}

// TestAmbiguousLaunchCommitReleasesTheHarnessAndDischargesTheObligation is the
// "the control plane did NOT commit" branch, and the whole point of resolving
// inside the launch. The spawner turns this function's error into an aborted
// spawn, so every part of that statement has to be true BEFORE the error
// returns:
//
//   - the session is not adopted;
//   - the lineage is PUBLISHED, not omitted, because the control plane holds it
//     live from the launch's own per-session adoption;
//   - the harness is STOPPED rather than left running to its orphan deadline;
//   - the recovery obligation is DISCHARGED, through the shim's own
//     group-reaped tombstone reported for the exact incarnation — never a
//     manufactured one, and never an absent attestation standing in for a reap
//     proof.
//
// RED before the fix: the launch returned immediately, the lineage was recorded
// nowhere, the harness ran on, and no terminal evidence was ever reported.
func TestAmbiguousLaunchCommitReleasesTheHarnessAndDischargesTheObligation(t *testing.T) {
	for _, release := range releaseCases() {
		for _, tc := range ambiguousCommitAnswers() {
			t.Run(release.name+"/"+tc.name, func(t *testing.T) {
				f := newAmbiguousLaunchFixture(t, "host-ambiguous-release")
				d := f.daemon

				f.fake.setAnswers(release.answers(tc.err)...)
				released := f.interactiveSpec("commit-answer-lost")
				if _, err := d.spawner.AcceptWork(released); err == nil {
					t.Fatal("AcceptWork succeeded even though the adoption batch was never committed")
				}
				if _, err := d.adoptedShimEntry(f.orgID, released.SessionID); err == nil {
					t.Fatal("a session whose batch never committed was tracked as adopted")
				}

				// THE OBLIGATION-SIDE CALL. Without it the platform's restart
				// fence is held forever and the row can never terminalize, no
				// matter what this host does locally.
				reports := f.terminal.forSession(released.SessionID)
				if len(reports) == 0 {
					t.Fatal("no terminal evidence was reported for the released lineage — its recovery obligation is " +
						"never discharged and the session can never terminalize")
				}
				report := reports[len(reports)-1]
				if report.Absent != nil {
					t.Fatalf("the release reported an ABSENT attestation instead of a reap proof; an attestation is "+
						"strictly weaker than a tombstone and must never stand in for one: %+v", report.Absent)
				}
				if !report.Tombstone.GroupReaped {
					t.Fatalf("the released lineage's terminal evidence does not prove its harness group was reaped: %+v",
						report.Tombstone)
				}
				if report.Tombstone.ShimID != report.ShimID || report.Tombstone.ProcessEpoch != report.ProcessEpoch {
					t.Fatalf("terminal evidence does not name the exact incarnation it reports: %+v", report)
				}

				// The published projection must agree with the daemon's own
				// state at every step, or the beat is refused and the host is
				// demoted. After the discharge the lineage is gone from both.
				final := f.fake.lastCommittedBatch(t)
				if !batchAdoptsSession(final, f.baseline.SessionID) {
					t.Fatalf("the release dropped the already-adopted baseline session: %+v", final)
				}
				if batchAdoptsSession(final, released.SessionID) {
					t.Fatalf("the release published a released lineage as adopted: %+v", final)
				}
				beat := d.QuarantinedSessions()
				if len(beat) != len(final.Quarantined) {
					t.Fatalf("the beat's quarantine set (%d) disagrees with the last published batch (%d) — the platform "+
						"demotes a host whose beat disagrees", len(beat), len(final.Quarantined))
				}

				// And the harness itself is gone: a group-reaped tombstone for
				// the exact incarnation is the proof, and the registry no
				// longer holds a live record for it.
				assertShimRecordWithdrawn(t, d, report.Identity, report.ShimID, report.ProcessEpoch)
			})
		}
	}
}

// assertShimRecordWithdrawn proves the released incarnation is no longer a live
// shim this host has to account for.
func assertShimRecordWithdrawn(t *testing.T, d *Daemon, id sessionshim.Identity, shimID string, epoch uint64) {
	t.Helper()
	registry, err := d.sessionShimRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	present, err := registry.HasIncarnation(id, shimID, epoch)
	if err != nil {
		t.Fatalf("HasIncarnation: %v", err)
	}
	if present {
		t.Fatalf("the released shim %s/%d still has a live registry record, so its harness was never stopped", shimID, epoch)
	}
}

// TestAmbiguousLaunchCommitDoesNotStrandTheHost is the operator-visible half:
// a released launch must cost the host nothing afterwards. The next healthy
// launch has to commit.
//
// RED before the fix: the released lineage was recorded nowhere, so every later
// batch omitted a lineage the control plane held live and was refused —
// a healthy, unrelated session stranded by a lost answer from a launch ago.
func TestAmbiguousLaunchCommitDoesNotStrandTheHost(t *testing.T) {
	for _, tc := range ambiguousCommitAnswers() {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmbiguousLaunchFixture(t, "host-ambiguous-no-strand")
			d := f.daemon

			f.fake.setAnswers(tc.err, batchCommitRefusal())
			released := f.interactiveSpec("commit-answer-lost")
			if _, err := d.spawner.AcceptWork(released); err == nil {
				t.Fatal("AcceptWork succeeded even though the adoption batch was never committed")
			}

			healthy := f.interactiveSpec("healthy-after-ambiguity")
			if _, err := d.spawner.AcceptWork(healthy); err != nil {
				t.Fatalf("an entirely healthy launch failed after an unrelated sibling's commit answer was lost: %v", err)
			}
			if _, err := d.adoptedShimEntry(f.orgID, healthy.SessionID); err != nil {
				t.Fatalf("the healthy session was not adopted: %v", err)
			}
			committed := f.fake.lastCommittedBatch(t)
			if !batchAdoptsSession(committed, f.baseline.SessionID) || !batchAdoptsSession(committed, healthy.SessionID) {
				t.Fatalf("the healthy launch's batch = %+v, want both the baseline and the new session adopted", committed)
			}
			if _, err := d.adoptedShimEntry(f.orgID, f.baseline.SessionID); err != nil {
				t.Fatalf("the baseline session was collaterally lost after a sibling launch's released commit: %v", err)
			}
		})
	}
}

// TestAmbiguousCommitClassificationIsUnchangedForDefiniteRefusals guards the
// blast radius: a DECODED refusal must keep refusal semantics and its existing
// retry-then-restore path, so none of the re-drive or release behaviour above
// can be reached by widening the classifier.
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
