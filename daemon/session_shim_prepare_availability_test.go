package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestSessionShimPrepareFailureClassification pins the rule everything else in
// this file rests on: a composing authority's REFUSAL and its failure to answer
// are different facts, and only the second one may be retried. The
// classification is by sentinel and errors.Is, so rewording an embedder's
// message can never silently turn a refusal into a retryable failure or the
// other way round.
func TestSessionShimPrepareFailureClassification(t *testing.T) {
	t.Parallel()
	conflict := fmt.Errorf("authority says no: %w", ErrSessionShimAdoptionPrepareConflict)
	for name, tc := range map[string]struct {
		err             error
		wantUnavailable bool
		wantSame        bool
	}{
		"nil is not a failure at all": {
			err: nil, wantSame: true,
		},
		"a typed conflict is an answer and is returned unchanged": {
			err: conflict, wantSame: true,
		},
		"a deadline is the authority not answering": {
			err:             fmt.Errorf("post adoption preparation: %w", context.DeadlineExceeded),
			wantUnavailable: true,
		},
		"a transport failure is the authority not answering": {
			err:             errors.New("unexpected EOF"),
			wantUnavailable: true,
		},
		"a server error is the authority not answering": {
			err:             errors.New("adoption preparation returned HTTP 503"),
			wantUnavailable: true,
		},
		"classification is idempotent": {
			err:             fmt.Errorf("%w: already classified", ErrSessionShimAdoptionPrepareUnavailable),
			wantUnavailable: true, wantSame: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := classifySessionShimPrepareFailure(tc.err)
			if tc.wantSame && !errors.Is(got, tc.err) && got != tc.err {
				t.Fatalf("classify(%v) = %v, want it returned unchanged", tc.err, got)
			}
			if gotUnavailable := errors.Is(got, ErrSessionShimAdoptionPrepareUnavailable); gotUnavailable != tc.wantUnavailable {
				t.Fatalf("classify(%v) unavailable = %v, want %v", tc.err, gotUnavailable, tc.wantUnavailable)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Fatalf("classify(%v) = %v; the original cause must survive classification", tc.err, got)
			}
			if errors.Is(tc.err, ErrSessionShimAdoptionPrepareConflict) &&
				errors.Is(got, ErrSessionShimAdoptionPrepareUnavailable) {
				t.Fatalf("classify(%v) turned an authority's refusal into a retryable failure", tc.err)
			}
		})
	}
}

// TestAdoptionPreparationRefusesItselfWithoutWithdrawingHostReadiness is the
// readiness half of the rule. A definite not-ready still refuses the
// preparation — nothing is loosened — but the host-wide admission fence belongs
// to the seam that resolves the host's OWN carrier proof, which is the beat. A
// per-lineage seam that raised the fence made one session's failed round trip
// look like a verdict about the host, and the fence only reopens on an
// acknowledged beat, so the host could not recover on its own.
func TestAdoptionPreparationRefusesItselfWithoutWithdrawingHostReadiness(t *testing.T) {
	t.Parallel()
	incomplete, err := testSessionShimProofV2Readiness()
	if err != nil {
		t.Fatalf("build readiness: %v", err)
	}
	incomplete.DurableCarrierProofV2Ready = false
	d, _ := readinessTestDaemon(t, SessionShimConfig{}, func() (SessionShimCarrierProofV2Readiness, error) {
		return incomplete, nil
	})
	scope := d.sessionShimConfig().orgID()

	_, prepareErr := d.prepareSessionShimAdoption(
		context.Background(), "stable-host-readiness", sessionshim.AdoptionPreparation{},
	)
	if !errors.Is(prepareErr, ErrSessionShimReadinessRejected) {
		t.Fatalf("preparation under a definite not-ready = %v, want the gate's refusal", prepareErr)
	}
	if d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("one lineage's preparation withdrew the host's published readiness")
	}
	// No loosening: every new-work seam consults the same gate for itself, so
	// the host refuses work through the gate whether or not the fence is up.
	if suspended, _ := d.claimSuspended(); !suspended {
		t.Fatal("the claim gate stayed open under a definite not-ready")
	}
	if _, err := d.AcceptWork(SessionSpec{SessionID: "must-be-refused"}); err == nil {
		t.Fatal("admission stayed open under a definite not-ready")
	}
	// The beat is the seam that owns the fence, and it still raises it.
	if _, err := d.SessionShimHeartbeatProjection(scope); err != nil {
		t.Fatalf("beat under a definite not-ready: %v", err)
	}
	if !d.sessionShimReadinessWithdrawn.Load() {
		t.Fatal("the beat did not withdraw on the host's own definite not-ready")
	}
}

// unansweredPrepareAuthority is a composing authority that answers the first n
// adoption preparations with a deadline and every one after that normally. It
// records the Attempt number each ask carried, which is what proves the daemon
// told the authority a re-ask was a re-ask.
type unansweredPrepareAuthority struct {
	mu       sync.Mutex
	attempts []int
	refusals int
	healthy  func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error)
}

func (a *unansweredPrepareAuthority) prepare(
	ctx context.Context,
	preparation SessionShimAdoptionPreparation,
) (sessionshim.PreparedAdoption, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, preparation.Attempt)
	unanswered := a.refusals > 0
	if unanswered {
		a.refusals--
	}
	a.mu.Unlock()
	if unanswered {
		// The exact field shape: the control plane was mid-rotation and the
		// round trip never completed.
		return sessionshim.PreparedAdoption{}, fmt.Errorf("post session-shim adoption preparation: %w", context.DeadlineExceeded)
	}
	return a.healthy(ctx, preparation)
}

func (a *unansweredPrepareAuthority) arm(refusals int) {
	a.mu.Lock()
	a.attempts = nil
	a.refusals = refusals
	a.mu.Unlock()
}

// invariantSnapshot is everything a failed launch of ONE lineage must leave
// exactly as it found it.
type invariantSnapshot struct {
	state              State
	withdrawn          bool
	accepting          bool
	adoptionComplete   bool
	activationComplete bool
	revision           string
	adopted            int
	quarantined        int
}

func (a *invariantSnapshot) String() string {
	return fmt.Sprintf(
		"state=%s withdrawn=%v accepting=%v adoptionComplete=%v activationComplete=%v revision=%q adopted=%d quarantined=%d",
		a.state, a.withdrawn, a.accepting, a.adoptionComplete, a.activationComplete, a.revision, a.adopted, a.quarantined,
	)
}

func snapshotHostInvariants(t *testing.T, d *Daemon, scope string) invariantSnapshot {
	t.Helper()
	projection, err := d.SessionShimHeartbeatProjection(scope)
	if err != nil {
		t.Fatalf("heartbeat projection: %v", err)
	}
	return invariantSnapshot{
		state:              d.State(),
		withdrawn:          d.sessionShimReadinessWithdrawn.Load(),
		accepting:          d.spawner.IsAccepting(),
		adoptionComplete:   d.SessionShimAdoptionComplete(),
		activationComplete: d.SessionShimCarrierActivationComplete(),
		revision:           projection.AdoptionRevision,
		adopted:            len(d.AdoptedSessionShims()),
		quarantined:        len(d.QuarantinedSessions()),
	}
}

// TestAnUnansweredPreparationIsBoundedAndCostsOneLineageOnly reproduces the
// live sequence end to end on real shim harnesses: a host is serving one
// adopted, durable lineage when the composing authority stops answering
// adoption preparations. Before this path existed, the ONE unanswered round
// trip a launch makes condemned the lineage on the first try and took the
// host's published readiness with it, and nothing short of a hand-written
// repair brought it back.
//
// Three facts are asserted, in the order the incident produced them:
//
//  1. the ask is repeated on fresh dials inside its bound, and each re-ask
//     tells the authority its attempt number;
//  2. a spent bound terminalizes THAT lineage and nothing else — readiness,
//     lifecycle, admission, the committed revision and every other lineage are
//     bit-for-bit what they were before the launch;
//  3. a later launch whose preparation IS answered adopts and republishes with
//     no daemon restart in between.
func TestAnUnansweredPreparationIsBoundedAndCostsOneLineageOnly(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.opts.SessionShim.HostID = "host-prepare-availability"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	// The bounded ladder dials the harness up to three times and spends 600ms
	// of backoff doing it; the fixture's two-second deadline would reap the
	// harness mid-ladder and hide the recovery under test behind a dead socket.
	// Kept short all the same: the lineage this test cannot prepare is left for
	// its own orphan clock to reap, exactly as the corpus requires, and every
	// second of that is a live harness competing with the rest of the suite.
	d.opts.SessionShim.Orphan.Deadline = 6 * time.Second
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(40)
	configureDynamicPublicationProbe(t, d, probe)
	authority := &unansweredPrepareAuthority{healthy: d.opts.SessionShim.PrepareAdoption}
	d.opts.SessionShim.PrepareAdoption = authority.prepare

	// A durable, adopted, acknowledged lineage: the thing the incident cost.
	authority.arm(0)
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("durable-one")); err != nil {
		t.Fatalf("baseline launch: %v", err)
	}
	baseline, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("baseline projection: %v", err)
	}
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, baseline)
	if d.State() != StateRunning || d.sessionShimReadinessWithdrawn.Load() {
		t.Fatalf("baseline did not reopen: state=%s withdrawn=%v", d.State(), d.sessionShimReadinessWithdrawn.Load())
	}
	before := snapshotHostInvariants(t, d, f.orgID)
	if before.adopted != 1 || before.quarantined != 0 || !before.adoptionComplete || !before.activationComplete {
		t.Fatalf("baseline host = %s, want one adopted durable lineage", &before)
	}

	// The authority stops answering. Bound spent, lineage terminal, host intact.
	authority.arm(sessionShimPrepareAttempts)
	handle, err := d.spawner.AcceptWork(f.interactiveSpec("prepare-unanswered"))
	if err == nil {
		t.Fatalf("AcceptWork = %+v, nil; an unpreparable launch must fail the accept", handle)
	}
	if !errors.Is(err, ErrSessionShimAdoptionPrepareUnavailable) {
		t.Fatalf("unpreparable launch error = %v, want the unanswered-preparation sentinel", err)
	}
	authority.mu.Lock()
	asks := append([]int(nil), authority.attempts...)
	authority.mu.Unlock()
	if len(asks) != sessionShimPrepareAttempts {
		t.Fatalf("preparation asks = %v, want %d bounded attempts", asks, sessionShimPrepareAttempts)
	}
	for i, attempt := range asks {
		if attempt != i+1 {
			t.Fatalf("preparation asks = %v; ask %d carried attempt %d, so the authority cannot tell a re-ask from a first ask",
				asks, i+1, attempt)
		}
	}
	after := snapshotHostInvariants(t, d, f.orgID)
	if after != before {
		t.Fatalf("one lineage's unanswered preparation changed the host:\n before %s\n  after %s", &before, &after)
	}

	// Recovery, with no daemon restart: the next launch's first re-ask is
	// answered and the lineage adopts.
	authority.arm(1)
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("prepare-recovers")); err != nil {
		t.Fatalf("a launch after a spent preparation bound: %v", err)
	}
	authority.mu.Lock()
	asks = append([]int(nil), authority.attempts...)
	authority.mu.Unlock()
	if len(asks) != 2 || asks[0] != 1 || asks[1] != 2 {
		t.Fatalf("recovering launch asks = %v, want one unanswered ask then attempt 2", asks)
	}
	recovered, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("projection after recovery: %v", err)
	}
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, recovered)
	restored := snapshotHostInvariants(t, d, f.orgID)
	if restored.state != StateRunning || restored.withdrawn || !restored.accepting {
		t.Fatalf("host did not restore without a restart: %s", &restored)
	}
	if restored.adopted != 2 || restored.quarantined != 0 {
		t.Fatalf("host after recovery = %s, want both lineages adopted and none quarantined", &restored)
	}
	if restored.revision == before.revision {
		t.Fatalf("the recovered launch published no new revision: still %q", restored.revision)
	}
}
