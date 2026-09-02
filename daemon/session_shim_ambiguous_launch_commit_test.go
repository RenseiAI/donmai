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
	"sync/atomic"
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
	fake      *sessionShimBatchCompletenessFake
	terminal  *terminalEvidenceRecorder
	baseline  SessionSpec
	discharge chan bool
}

// testAmbiguousCallbackTimeout keeps every bound derived from the callback
// timeout test-sized. The release's wait for terminal proof is sized from it,
// and the fixture's default is the 60s launch timeout, which would put a
// two-minute bound on a test; shrinking the ONE unit moves all of them.
const testAmbiguousCallbackTimeout = 200 * time.Millisecond

func newAmbiguousLaunchFixture(t *testing.T, hostID string) *ambiguousLaunchFixture {
	return newAmbiguousLaunchFixtureWithCallbackTimeout(t, hostID, testAmbiguousCallbackTimeout)
}

// newAmbiguousLaunchFixtureWithCallbackTimeout builds the fixture. Every
// configure hook runs BEFORE the baseline launch: d.opts.SessionShim is read
// without a lock by the per-session consumer goroutines a launch starts, so a
// config field written after one is running is a genuine data race, not a test
// artifact.
func newAmbiguousLaunchFixtureWithCallbackTimeout(
	t *testing.T, hostID string, callbackTimeout time.Duration, configure ...func(*Daemon),
) *ambiguousLaunchFixture {
	t.Helper()
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = hostID
	d.opts.SessionShim.CallbackTimeout = callbackTimeout
	// Hosted/attested so the retained-authority bookkeeping a committed batch
	// advances is real rather than a no-op.
	enableHostedFullHostFramesForTest(t, d, f.orgID)

	fake := &sessionShimBatchCompletenessFake{}
	terminal := &terminalEvidenceRecorder{}
	d.opts.SessionShim.OnAdoption = fake.onAdoption
	d.opts.SessionShim.PrepareAdoptionBatch = fake.prepareBatch
	d.opts.SessionShim.OnAdoptionBatch = fake.onAdoptionBatch
	d.opts.SessionShim.OnTerminalEvidence = terminal.record
	for _, apply := range configure {
		apply(d)
	}

	// The discharge is deliberately OFF the accept goroutine, so every
	// assertion about it has to join it explicitly. Buffered so a discharge
	// that finishes before the test reaches its receive cannot block a
	// goroutine the daemon's own shutdown joins.
	discharge := make(chan bool, 4)
	d.shims.mu.Lock()
	d.shims.afterAmbiguousLaunchDischarge = func(_ sessionshim.Identity, ok bool) {
		select {
		case discharge <- ok:
		default:
		}
	}
	d.shims.mu.Unlock()

	baseline := f.interactiveSpec("already-adopted")
	if _, err := d.spawner.AcceptWork(baseline); err != nil {
		t.Fatalf("AcceptWork for the baseline session: %v", err)
	}
	if _, err := d.adoptedShimEntry(f.orgID, baseline.SessionID); err != nil {
		t.Fatalf("baseline session was not adopted: %v", err)
	}
	fixture := &ambiguousLaunchFixture{
		shimSpawnFixture: f, fake: fake, terminal: terminal, baseline: baseline, discharge: discharge,
	}
	fixture.acknowledgeRecoveryBeat(t)
	return fixture
}

// acknowledgeRecoveryBeat is the ONE dynamic reopening edge. A serialized
// publication raises the recovery heartbeat barrier and latches admission
// closed; only an exact server acknowledgement reopens it. Production gets that
// from the heartbeat service, and a test that skips it finds the very next
// launch refused with "not accepting new work".
//
// It is a no-op when the barrier is not armed, so callers need not know which
// configuration they are in.
func (f *ambiguousLaunchFixture) acknowledgeRecoveryBeat(t *testing.T) {
	t.Helper()
	if f.daemon.sessionShimConfig().OnAdoptionPublished == nil {
		return
	}
	projection, err := f.daemon.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("heartbeat projection: %v", err)
	}
	f.daemon.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, projection)
}

// awaitDischarge joins the detached discharge and reports whether it discharged
// the obligation inside its bound.
func (f *ambiguousLaunchFixture) awaitDischarge(t *testing.T) bool {
	t.Helper()
	// Generously above the discharge's own derived bound, so a timeout here
	// means the goroutine never ran rather than that it was merely slow.
	wait := 4 * acceptanceClearDeadlineFor(f.daemon.sessionShimConfig().callbackTimeout())
	select {
	case ok := <-f.discharge:
		return ok
	case <-time.After(wait):
		t.Fatalf("the detached discharge never finished within %s", wait)
		return false
	}
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
				// The lost answer, then a second lost answer for the one
				// re-drive. The daemon never learns the outcome and must not
				// report an adoption it cannot prove.
				return []error{ambiguous, ambiguous}
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
				if !f.awaitDischarge(t) {
					t.Fatal("the detached discharge did not discharge the released lineage's obligation inside its bound")
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
			f.awaitDischarge(t)

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

// assertBeatAgreesWithPublishedBatch is the demotion invariant itself: the
// platform compares each beat's quarantine set against the snapshot the last
// COMMITTED batch stored, byte for byte, and drains a host whose beat
// disagrees. Every step of the release has to leave the two equal.
func assertBeatAgreesWithPublishedBatch(t *testing.T, d *Daemon, fake *sessionShimBatchCompletenessFake) {
	t.Helper()
	results := fake.snapshot()
	var published SessionShimAdoptionBatch
	found := false
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].err == nil {
			published = results[i].batch
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no adoption batch was ever committed, so nothing pins the beat")
	}
	beat := d.QuarantinedSessions()
	if len(beat) != len(published.Quarantined) {
		t.Fatalf("the beat's quarantine set (%d) disagrees with the last COMMITTED batch (%d) — the platform drains a "+
			"host whose beat disagrees\n beat=%+v\n batch=%+v", len(beat), len(published.Quarantined), beat, published.Quarantined)
	}
	for i := range beat {
		if beat[i] != published.Quarantined[i] {
			t.Fatalf("quarantine entry %d differs between the beat and the last committed batch:\n beat=%+v\n batch=%+v",
				i, beat[i], published.Quarantined[i])
		}
	}
}

// TestAmbiguousLaunchReleaseSurvivesABurnedCallerBudget pins the repair whose
// absence drained a host.
//
// Every stage of the release is a control-plane round trip, and by the time the
// release runs the caller's own publication budget may be entirely spent — the
// re-drive is allowed to spend a whole stage of it, and the deadline that
// provoked the ambiguity may BE the caller's. A stage handed that dead budget
// fails instantly at PrepareAdoptionBatch. The measured shape: the final
// republish was issued on the caller's context through the fire-and-forget
// wrapper, so its DeadlineExceeded was discarded; a plain deadline arms no
// reconciliation (only a revision advance or an outcome-unknown commit does);
// and the success line was logged regardless. The lineage left the daemon's own
// quarantine set while the last committed batch still presented it — the exact
// disagreement the platform drains a host for, sticky on an idle host until a
// restart.
//
// The caller's context is passed in ALREADY EXPIRED here, which is that
// condition in its purest form. The fakes honor their contexts, so anything
// issued on a dead budget genuinely fails.
//
// RED before the fix: the beat/batch invariant breaks after the discharge.
func TestAmbiguousLaunchReleaseSurvivesABurnedCallerBudget(t *testing.T) {
	f := newAmbiguousLaunchFixture(t, "host-ambiguous-burned-budget")
	d := f.daemon

	// A real adopted shim, so the release has a live controller and true
	// evidence to work from.
	victim := f.interactiveSpec("burned-budget-release")
	if _, err := d.spawner.AcceptWork(victim); err != nil {
		t.Fatalf("AcceptWork for the session the release will act on: %v", err)
	}
	entry, err := d.adoptedShimEntry(f.orgID, victim.SessionID)
	if err != nil {
		t.Fatalf("adopted entry: %v", err)
	}
	// Model the launch-failure state the release is only ever reached from:
	// trackLaunchedShim never ran, so the lineage is in no adopted set.
	d.shims.mu.Lock()
	delete(d.shims.adopted, sessionshim.Identity{OrgID: f.orgID, SessionID: victim.SessionID})
	d.shims.mu.Unlock()

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	d.releaseAmbiguousLaunchSessionShim(expired, entry.controller, entry.adoption,
		fmt.Errorf("commit adoption batch: %w", context.DeadlineExceeded))

	// The quarantine publish runs on a FRESH detached budget, so it lands even
	// though the caller's is dead. If it did not, the lineage would be
	// unpublished and the beat would disagree immediately.
	afterQuarantine := f.fake.lastCommittedBatch(t)
	if _, quarantined := batchQuarantinesSession(afterQuarantine, victim.SessionID); !quarantined {
		t.Fatalf("the release's quarantine publish did not land on its own budget: %+v", afterQuarantine)
	}
	assertBeatAgreesWithPublishedBatch(t, d, f.fake)

	if !f.awaitDischarge(t) {
		t.Fatal("the detached discharge did not discharge the obligation inside its bound")
	}
	// THE PIN: after the discharge dropped the lineage from the daemon's own
	// projection, the republish that retires it must have been issued on a LIVE
	// context — or, if it genuinely could not land, reconciliation must own the
	// repair. Either way the beat and the last committed batch agree.
	assertBeatAgreesWithPublishedBatch(t, d, f.fake)
	final := f.fake.lastCommittedBatch(t)
	if _, stillQuarantined := batchQuarantinesSession(final, victim.SessionID); stillQuarantined {
		t.Fatalf("the discharge's republish never retired the released lineage, so it was issued on a dead budget: %+v", final)
	}
}

// TestAmbiguousLaunchReleaseDoesNotHoldTheAcceptGoroutine pins the other half
// of the same fix.
//
// The release runs on the sequential accept goroutine, holding publicationMu.
// Waiting there for the stopped shim's terminal proof — a wait sized by
// acceptanceClearDeadlineFor, over a minute at production defaults — blocks
// every sibling launch on that mutex, stops the worker polling entirely, blocks
// AcknowledgeSessionShimRecoveryHeartbeat on the same mutex, and leaves the
// local control route that asked for this session unanswerable. None of it is
// needed for the abort to be true: the lineage is already published quarantined
// and capacity-consuming before the error returns.
//
// The discharge is parked inside its reconcile here (through the seam that
// exists for exactly this) so the assertions run while it is provably still
// waiting.
//
// RED before the fix: AcceptWork does not return until the discharge finishes.
func TestAmbiguousLaunchReleaseDoesNotHoldTheAcceptGoroutine(t *testing.T) {
	// Production-like: the embedder sets neither timeout, so callbackTimeout
	// falls back to the launch timeout and every bound derived from it is
	// minutes wide. That is the configuration the hold was measured under.
	f := newAmbiguousLaunchFixtureWithCallbackTimeout(t, "host-ambiguous-not-held", 30*time.Second)
	d := f.daemon

	entered := make(chan struct{})
	hold := make(chan struct{})
	var parkOnce sync.Once
	var parked atomic.Bool
	parked.Store(true)
	d.shims.mu.Lock()
	d.shims.afterTombstoneFetch = func(incarnation shimIncarnation) {
		if incarnation.identity.SessionID != "commit-answer-lost" || !parked.Load() {
			return
		}
		parkOnce.Do(func() { close(entered) })
		<-hold
	}
	d.shims.mu.Unlock()

	f.fake.setAnswers(transportLostCommitAnswer(), batchCommitRefusal())
	released := f.interactiveSpec("commit-answer-lost")
	start := time.Now()
	if _, err := d.spawner.AcceptWork(released); err == nil {
		t.Fatal("AcceptWork succeeded even though the adoption batch was never committed")
	}
	accepted := time.Since(start)

	// The discharge must be genuinely in flight and parked: this proves the
	// wait it is parked in was NOT on the accept goroutine.
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the detached discharge never reached its reconcile")
	}

	// The dominant term of the old synchronous release. Returning inside it is
	// what "not held" means.
	held := acceptanceClearDeadlineFor(d.sessionShimConfig().callbackTimeout())
	if accepted >= held {
		t.Fatalf("AcceptWork took %s, which is at or past the discharge's own wait bound %s — the accept goroutine is "+
			"still being held for the terminal-proof wait", accepted, held)
	}

	// publicationMu must be free WHILE the discharge waits: it is what every
	// sibling launch and every heartbeat acknowledgement contends for. TryLock
	// rather than a timed Lock, so a held mutex fails immediately and loudly
	// instead of being reported as slowness.
	if !d.shims.publicationMu.TryLock() {
		t.Fatal("publicationMu is still held while the discharge waits — sibling launches and heartbeat acknowledgements " +
			"are blocked behind a wait that is not on their path")
	}
	d.shims.publicationMu.Unlock()

	// The lineage is already published quarantined, so the control plane's view
	// is correct and conservative from the moment the error returned — the
	// discharge only improves it.
	published := f.fake.lastCommittedBatch(t)
	if _, quarantined := batchQuarantinesSession(published, released.SessionID); !quarantined {
		t.Fatalf("the released lineage was not published quarantined before the error returned: %+v", published)
	}

	parked.Store(false)
	close(hold)
	if !f.awaitDischarge(t) {
		t.Fatal("the discharge did not discharge the obligation once unparked")
	}
	assertBeatAgreesWithPublishedBatch(t, d, f.fake)
}

// TestAmbiguousLaunchReleaseUnderASerializedPublication runs the release inside
// the checkpoint/rollback critical section production always uses and no other
// test here enters.
//
// serializedPublication is gated on OnAdoptionPublished, which the embedder
// REQUIRES and the spawn fixture leaves nil — so every other subtest exercises
// the release with the checkpoint machinery switched off. In production the
// whole release happens between checkpointSessionShimPublication and a deferred
// rollbackSessionShimPublication that runs with publicationCommitted=false,
// because this launch's own batch never committed. That rollback must not undo
// what the release COMMITTED on the way out: the lineage's publication, and the
// revision the control plane issued for it.
func TestAmbiguousLaunchReleaseUnderASerializedPublication(t *testing.T) {
	// The D13 activation edge: wiring it turns on serializedPublication AND
	// (with the carrier acknowledgement enableHostedFullHostFramesForTest
	// already provides) the heartbeat barrier. It has to be in place before the
	// first launch, so it is a fixture hook rather than a later assignment.
	f := newAmbiguousLaunchFixtureWithCallbackTimeout(t, "host-ambiguous-serialized", testAmbiguousCallbackTimeout,
		func(d *Daemon) {
			// The §D4 startup pass never runs in this fixture, and the barrier's
			// reopening edge refuses to project a heartbeat for a daemon that has
			// not finished adopting. Production has both by the time any dynamic
			// publication happens.
			d.setState(StateRunning)
			d.shims.mu.Lock()
			d.shims.adoptionComplete = true
			d.shims.mu.Unlock()
			d.opts.SessionShim.OnAdoptionPublished = func(
				context.Context, SessionShimAdoptionPublication,
			) ([]SessionShimCarrierActivationReceipt, error) {
				return nil, nil
			}
		})
	d := f.daemon

	before, ok := d.SessionShimScopeAuthority(f.orgID)
	if !ok {
		t.Fatal("no retained authority before the release")
	}

	f.fake.setAnswers(transportLostCommitAnswer(), batchCommitRefusal())
	released := f.interactiveSpec("commit-answer-lost")
	if _, err := d.spawner.AcceptWork(released); err == nil {
		t.Fatal("AcceptWork succeeded even though the adoption batch was never committed")
	}
	if !f.awaitDischarge(t) {
		t.Fatal("the detached discharge did not discharge the obligation inside its bound")
	}

	// The deferred rollback has run by now (it fires as launchSessionShim
	// returns, before AcceptWork did). It restores the last-committed heartbeat
	// posture — and must leave the release's own committed facts alone.
	after, ok := d.SessionShimScopeAuthority(f.orgID)
	if !ok {
		t.Fatal("no retained authority after the release")
	}
	if after.AdoptionRevision == before.AdoptionRevision {
		t.Fatalf("retained revision after the release = %q, unchanged from %q — the release committed batches whose "+
			"revisions were dropped, so the next beat presents a superseded one and the host is demoted",
			after.AdoptionRevision, before.AdoptionRevision)
	}
	assertBeatAgreesWithPublishedBatch(t, d, f.fake)
	final := f.fake.lastCommittedBatch(t)
	if _, stillQuarantined := batchQuarantinesSession(final, released.SessionID); stillQuarantined {
		t.Fatalf("the rollback undid the discharge's republish: %+v", final)
	}
	if !batchAdoptsSession(final, f.baseline.SessionID) {
		t.Fatalf("the rollback dropped the already-adopted baseline session: %+v", final)
	}
	// And the host can still work: the failed launch's rollback reopened
	// admission on the base it started from, and the next launch commits.
	f.acknowledgeRecoveryBeat(t)
	healthy := f.interactiveSpec("healthy-after-serialized-release")
	if _, err := d.spawner.AcceptWork(healthy); err != nil {
		t.Fatalf("a healthy launch failed after a released one inside the serialized publication: %v", err)
	}
}
