package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// readoptFixture is one live shim adopted by a daemon whose durable adoption
// callback is scripted per attempt, so a test can say exactly which
// re-adoption attempt the composing carrier accepts.
type readoptFixture struct {
	daemon     *Daemon
	registry   *sessionshim.Registry
	id         sessionshim.Identity
	controller *sessionshim.Controller

	mu        sync.Mutex
	adoptions int
	// batches are the batches the composing carrier ACCEPTED; refused are the
	// ones it turned away (see refuseBatches). Only an accepted batch is
	// something the receiver holds.
	batches      []SessionShimAdoptionBatch
	refused      []SessionShimAdoptionBatch
	batchOutcome func(batch SessionShimAdoptionBatch) error
}

// refuseBatches scripts the carrier's answer to every subsequent
// OnAdoptionBatch: a non-nil error refuses the batch without recording it as
// published.
func (f *readoptFixture) refuseBatches(outcome func(batch SessionShimAdoptionBatch) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchOutcome = outcome
}

// newReadoptFixture starts a real shim, adopts it once through the ordinary
// sessionshim.Adopt path, and seeds the daemon's adopted entry with the exact
// evidence a startup adoption would have recorded. adoptionOutcome answers the
// n-th OnAdoption call (1-based) with nil or the error it should refuse with.
func newReadoptFixture(t *testing.T, policy SessionShimReadoptionPolicy, adoptionOutcome func(attempt int) error) *readoptFixture {
	t.Helper()
	return newReadoptFixtureWithAdoption(t, policy, func(_ context.Context, attempt int) error { return adoptionOutcome(attempt) })
}

// newReadoptFixtureBlockingAdoption is newReadoptFixture with a durable
// adoption callback that never answers: it blocks until the context the
// daemon hands it ends, then refuses with that context's error.
func newReadoptFixtureBlockingAdoption(t *testing.T, policy SessionShimReadoptionPolicy) *readoptFixture {
	t.Helper()
	return newReadoptFixtureWithAdoption(t, policy, func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

func newReadoptFixtureWithAdoption(t *testing.T, policy SessionShimReadoptionPolicy, adoptionOutcome func(ctx context.Context, attempt int) error) *readoptFixture {
	t.Helper()
	// A Unix socket path has a short platform limit, and t.TempDir() bakes
	// the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "drd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	f := &readoptFixture{registry: registry}
	f.id = sessionshim.Identity{OrgID: "org-readopt", SessionID: "session-readopt"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: f.id, Registry: registry, ProcessEpoch: 5,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
		// Long enough that a test never races the shim's own reaper; the
		// point of re-adoption is that the deadline is never reached.
		Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second, PropagationMargin: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	adoption, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "controller-readopt-first",
	})
	if err != nil || len(adoption.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", adoption, err)
	}
	f.controller = adoption.Adopted[0]

	f.daemon = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		RegistryDir:     filepath.Join(dir, "registry"),
		CallbackTimeout: 5 * time.Second,
		HostIDForOrg:    func(context.Context, string) (string, error) { return "wh_readopt_host", nil },
		OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			f.mu.Lock()
			f.adoptions++
			attempt := f.adoptions
			f.mu.Unlock()
			if err := adoptionOutcome(ctx, attempt); err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("readopt-" + evidence.Identity.Key())}, nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.batchOutcome != nil {
				if err := f.batchOutcome(batch); err != nil {
					f.refused = append(f.refused, cloneSessionShimAdoptionBatch(batch))
					return SessionShimAdoptionBatchReceipt{}, err
				}
			}
			f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{DurableCorrelation: []byte("rev-readopt"), AdoptionRevision: "readopt-revision"}, nil
		},
		Readoption: policy,
	}})
	t.Cleanup(f.daemon.ReleaseAdoptedSessionShims)
	evidence, err := f.daemon.sessionShimAdoptionEvidence(context.Background(), f.controller, SessionShimAdoptionPreparationResult{}, "wh_readopt_host")
	if err != nil {
		t.Fatalf("adoption evidence: %v", err)
	}
	evidence.SnapshotProxy = nil
	f.daemon.shims.mu.Lock()
	f.daemon.shims.registry = registry
	f.daemon.shims.adopted[f.id] = adoptedShim{
		controller: f.controller, shimID: f.controller.Hello().ShimID,
		adoption: evidence, adoptionReceipt: SessionShimAdoptionReceipt{DurableCorrelation: []byte("first")},
	}
	f.daemon.shims.mu.Unlock()
	return f
}

func (f *readoptFixture) snapshot() (int, []SessionShimAdoptionBatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adoptions, append([]SessionShimAdoptionBatch(nil), f.batches...)
}

func (f *readoptFixture) refusedBatches() []SessionShimAdoptionBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionShimAdoptionBatch(nil), f.refused...)
}

// lostEntry is the adopted entry exactly as the disconnect path hands it to
// re-adoption: the first controller, closed the way releaseShimIfLive closes
// it before it re-dials.
func (f *readoptFixture) lostEntry(t *testing.T) adoptedShim {
	t.Helper()
	f.daemon.shims.mu.RLock()
	entry, ok := f.daemon.shims.adopted[f.id]
	f.daemon.shims.mu.RUnlock()
	if !ok || entry.controller != f.controller {
		t.Fatalf("adopted entry = %+v, want the fixture's first controller", entry)
	}
	_ = f.controller.Close()
	return entry
}

func (f *readoptFixture) recordPhase(t *testing.T) shimwire.Phase {
	t.Helper()
	rec, err := f.registry.Get(f.id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	return rec.Phase
}

// TestControllerLossReadoptsALiveShimBeforeQuarantining pins the recovery a
// relay restart needs.
//
// The controller stream ended, but the shim and its harness are alive and the
// discovery record is live: the loss was the CARRIER's, not the shim's. Before
// this change the disconnect path quarantined the lineage `socket_unreachable`
// at once and nothing ever dialled it again, so every carrier blip cost a
// healthy harness its orphan deadline. The first re-adoption attempt is refused
// (the carrier is still coming back); the second is accepted. No quarantine may
// reach the composer, the lineage must stay adopted under a strictly newer
// generation, and the shim's own orphan clock must be disarmed.
func TestControllerLossReadoptsALiveShimBeforeQuarantining(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 10 * time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			return errors.New("carrier candidate dial refused: relay restarting")
		}
		return nil
	})
	d := f.daemon
	previousGeneration := f.controller.Generation()

	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	adoptions, batches := f.snapshot()
	if adoptions != 2 {
		t.Fatalf("durable adoption attempted %d times, want 2 — one refused, the second accepted", adoptions)
	}
	for i, batch := range batches {
		if len(batch.Quarantined) != 0 {
			t.Fatalf("batch %d quarantined a lineage whose shim was alive and re-adoptable: %+v", i, batch.Quarantined)
		}
	}
	if len(batches) == 0 {
		t.Fatal("no batch was published for the re-adoption; the receiver never learned the new generation")
	}
	last := batches[len(batches)-1]
	if len(last.Adopted) != 1 || last.Adopted[0].Evidence.Identity != f.id {
		t.Fatalf("last batch adopted = %+v, want exactly the re-adopted lineage", last.Adopted)
	}
	if got := last.Adopted[0].Evidence.ControllerGeneration; got <= uint64(previousGeneration) {
		t.Fatalf("re-adopted generation = %d, want strictly newer than the lost controller's %d", got, previousGeneration)
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("re-adoption left %d lineages projected quarantined: %+v", len(projected), projected)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
	if entry.adoption.ControllerGeneration != uint64(entry.controller.Generation()) {
		t.Fatalf("adopted evidence generation %d disagrees with the live controller's %d",
			entry.adoption.ControllerGeneration, entry.controller.Generation())
	}
	waitFor(t, 5*time.Second, "the shim to disarm its orphan clock", func() bool {
		return f.recordPhase(t) == shimwire.PhaseRunning
	})
}

// TestControllerLossQuarantinesWhenEveryReadoptionFails keeps the fallback
// exactly what it was: a shim that cannot be re-adopted inside the bound is
// quarantined `socket_unreachable`, capacity-charged, and published — never
// killed, never released.
func TestControllerLossQuarantinesWhenEveryReadoptionFails(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("carrier candidate dial refused")
	})
	d := f.daemon

	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	adoptions, batches := f.snapshot()
	if adoptions != 3 {
		t.Fatalf("durable adoption attempted %d times, want the policy's 3 before giving up", adoptions)
	}
	if _, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err == nil {
		t.Fatal("an unre-adoptable lineage stayed in the adopted set")
	}
	projected := d.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Reason != sessionshim.QuarantineSocketUnreachable || !projected[0].ConsumesCapacity {
		t.Fatalf("quarantine projection = %+v, want one capacity-charged socket_unreachable lineage", projected)
	}
	if len(batches) == 0 {
		t.Fatal("the quarantine was never published")
	}
	last := batches[len(batches)-1]
	if len(last.Quarantined) != 1 || last.Quarantined[0].SessionID != f.id.SessionID || len(last.Adopted) != 0 {
		t.Fatalf("last batch = adopted:%+v quarantined:%+v, want the lineage quarantined and nothing adopted",
			last.Adopted, last.Quarantined)
	}
}

// TestControllerLossReadoptionCanBeDisabled keeps the embedder in charge: a
// composition that declares no re-adoption gets the pre-existing disposition
// with no dial at all.
func TestControllerLossReadoptionCanBeDisabled(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Disabled: true}, func(int) error { return nil })

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	adoptions, _ := f.snapshot()
	if adoptions != 0 {
		t.Fatalf("durable adoption attempted %d times with re-adoption disabled, want none", adoptions)
	}
	if projected := f.daemon.QuarantinedSessions(); len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want the lineage quarantined as before", projected)
	}
}

// TestReadoptionWhoseBatchIsRefusedRestoresTheLostEntry pins the undo. The
// re-adoption dialled, prepared, and durably adopted the shim under a newer
// generation and swapped the new controller in — and then the batch that
// would have told the receiver about it was refused. Nothing durable advanced
// that this daemon can prove, so the projection must present exactly what the
// receiver still holds: the LOST controller at the OLD generation, with the
// old receipt and correlation, and no batch carrying the new generation may
// remain published. The caller's quarantine path builds its row from that
// restored entry.
func TestReadoptionWhoseBatchIsRefusedRestoresTheLostEntry(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond}, func(int) error { return nil })
	f.refuseBatches(func(SessionShimAdoptionBatch) error { return errors.New("injected durable-batch refusal") })
	d := f.daemon
	lost := f.lostEntry(t)
	lostGeneration := lost.adoption.ControllerGeneration

	if d.readoptSessionShimAfterControllerLoss(f.id, lost) {
		t.Fatal("re-adoption reported success though its batch was refused")
	}

	adoptions, published := f.snapshot()
	if adoptions != 1 {
		t.Fatalf("durable adoption attempted %d times, want exactly the one attempt", adoptions)
	}
	if len(published) != 0 {
		t.Fatalf("%d batches remain published after the refusal: %+v", len(published), published)
	}
	refused := f.refusedBatches()
	if len(refused) != 1 || len(refused[0].Adopted) != 1 || refused[0].Adopted[0].Evidence.ControllerGeneration <= lostGeneration {
		t.Fatalf("refused batches = %+v, want exactly one carrying the lineage under a strictly newer generation", refused)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set: %v", err)
	}
	if entry.controller != f.controller {
		t.Fatalf("adopted entry names controller generation %d, want the lost controller (generation %d) restored",
			entry.controller.Generation(), f.controller.Generation())
	}
	if entry.adoption.ControllerGeneration != lostGeneration {
		t.Fatalf("restored evidence generation = %d, want the lost %d", entry.adoption.ControllerGeneration, lostGeneration)
	}
	if string(entry.adoptionReceipt.DurableCorrelation) != "first" {
		t.Fatalf("restored receipt correlation = %q, want the lost adoption's", entry.adoptionReceipt.DurableCorrelation)
	}
	d.shims.mu.RLock()
	correlation, ok := d.shims.correlations[shimIncarnationFor(lost.adoption)]
	d.shims.mu.RUnlock()
	if !ok || correlation.evidence.ControllerGeneration != lostGeneration || string(correlation.receipt.DurableCorrelation) != "first" {
		t.Fatalf("incarnation correlation = %+v (present %v), want the lost adoption's evidence and receipt", correlation, ok)
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("re-adoption quarantined on its own: %+v — that disposition belongs to the caller", projected)
	}
}

// TestProjectionBuiltDuringTheReadoptionWindowPresentsTheLineageAdopted pins
// the complete-snapshot rule across the window. Between the controller loss
// and the re-adoption's batch the receiver holds this lineage adopted at the
// lost generation, and every projection built meanwhile — the batch a
// republish would carry, the heartbeat's quarantine view — must say exactly
// that: adopted at the lost generation, never quarantined, never absent. The
// probe runs from inside the first (refused) durable adoption, which is as
// deep inside the window as a projection can be built.
func TestProjectionBuiltDuringTheReadoptionWindowPresentsTheLineageAdopted(t *testing.T) {
	t.Parallel()
	// The revision enableHostedFullHostFramesForTest retains for the scope:
	// what the receiver holds until the re-adoption's batch commits.
	const retainedRevision = "test-recovery-revision"
	var (
		f              *readoptFixture
		lostGeneration uint64
		probed         bool
		findings       []string
	)
	// The probe runs on the daemon's re-adoption goroutine, so it records what
	// it sees instead of failing there (FailNow from a non-test goroutine
	// never returns); the test asserts on the findings once the window ends.
	probe := func() {
		probed = true
		d := f.daemon
		batch := d.sessionShimProjectionBatch(f.id.OrgID, "wh_readopt_host")
		switch {
		case len(batch.Adopted) != 1 || batch.Adopted[0].Evidence.Identity != f.id:
			findings = append(findings, fmt.Sprintf("mid-window batch adopted = %+v, want exactly the lost lineage", batch.Adopted))
		case batch.Adopted[0].Evidence.ControllerGeneration != lostGeneration:
			findings = append(findings, fmt.Sprintf("mid-window batch presents generation %d, want the lost %d the receiver holds",
				batch.Adopted[0].Evidence.ControllerGeneration, lostGeneration))
		}
		if len(batch.Quarantined) != 0 {
			findings = append(findings, fmt.Sprintf("mid-window batch quarantined = %+v, want none", batch.Quarantined))
		}
		if projected := d.QuarantinedSessions(); len(projected) != 0 {
			findings = append(findings, fmt.Sprintf("mid-window quarantine projection = %+v, want none", projected))
		}
		// The heartbeat carries no adopted list: it presents the lineage as
		// adopted by NOT quarantining it while echoing the adoption revision the
		// receiver retained before the loss — the revision a batch would only
		// advance once it commits.
		projection, err := d.SessionShimHeartbeatProjection(f.id.OrgID)
		switch {
		case err != nil:
			findings = append(findings, fmt.Sprintf("mid-window heartbeat projection: %v", err))
		case len(projection.QuarantinedSessions) != 0:
			findings = append(findings, fmt.Sprintf("mid-window heartbeat projection quarantined = %+v, want none", projection.QuarantinedSessions))
		case !projection.Enabled || !projection.AdoptionComplete:
			findings = append(findings, fmt.Sprintf("mid-window heartbeat projection enabled=%v adoptionComplete=%v, want both", projection.Enabled, projection.AdoptionComplete))
		case projection.AdoptionRevision != retainedRevision:
			findings = append(findings, fmt.Sprintf("mid-window heartbeat projection revision = %q, want the retained %q the receiver holds", projection.AdoptionRevision, retainedRevision))
		}
	}
	f = newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 2, Backoff: time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			probe()
			return errors.New("carrier candidate dial refused: relay restarting")
		}
		return nil
	})
	d := f.daemon
	// The heartbeat projection is only built for a composed host: startup
	// adoption enabled, an attestation resolved, a retained authority receipt
	// for the scope, and both startup passes reported complete.
	d.opts.SessionShim.EnableAdoption = true
	d.opts.SessionShim.HostID = "wh_readopt_host"
	enableHostedFullHostFramesForTest(t, d, f.id.OrgID)
	d.shims.mu.Lock()
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.shims.mu.Unlock()
	lostGeneration = uint64(f.controller.Generation())
	before, err := d.SessionShimHeartbeatProjection(f.id.OrgID)
	if err != nil || before.AdoptionRevision != retainedRevision {
		t.Fatalf("heartbeat projection before the loss = %+v (err %v), want revision %q retained", before, err, retainedRevision)
	}

	d.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	if !probed {
		t.Fatal("the mid-window probe never ran")
	}
	for _, finding := range findings {
		t.Error(finding)
	}
	if len(findings) > 0 {
		t.FailNow()
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set: %v", err)
	}
	if entry.adoption.ControllerGeneration <= lostGeneration {
		t.Fatalf("after the window the lineage is at generation %d, want strictly newer than %d", entry.adoption.ControllerGeneration, lostGeneration)
	}
	// Only the committed batch advances what the heartbeat echoes.
	after, err := d.SessionShimHeartbeatProjection(f.id.OrgID)
	if err != nil || after.AdoptionRevision != "readopt-revision" || len(after.QuarantinedSessions) != 0 {
		t.Fatalf("heartbeat projection after the window = %+v (err %v), want the re-adoption batch's revision and nothing quarantined", after, err)
	}
}

// TestDefaultReadoptionPolicyWindowFitsInsideTheTightestOrphanDeadline pins
// the arithmetic the policy exists for. The shim's orphan clock starts when
// the controller stream ends; only a Welcome it accepts disarms it; so every
// attempt and every backoff must end before the orphan deadline the daemon
// resolves — with room for the last Welcome to land.
//
// The deadline is computed here the way sessionShimConfig computes it, not
// quoted: a composing deployment declares an external release threshold and
// leaves Orphan.Deadline zero, so the deadline is
// sessionShimOrphanDeadlineUnderExternalRelease over the default policy —
// for the tightest threshold known (three minutes), 3m − 5s − 30s − 30s =
// 115 s. Shims launched before that derivation existed carry the old 90 s
// constant in their environment for life, so the window is held below that
// too. The standalone default (fifteen minutes) is looser than either.
func TestDefaultReadoptionPolicyWindowFitsInsideTheTightestOrphanDeadline(t *testing.T) {
	t.Parallel()
	const (
		tightestExternalReleaseThreshold = 3 * time.Minute
		legacyLaunchedShimDeadline       = 90 * time.Second
	)
	composed := sessionshim.DefaultOrphanPolicy()
	composed.ExternalReleaseThreshold = tightestExternalReleaseThreshold
	composedDeadline := sessionShimOrphanDeadlineUnderExternalRelease(composed, composed.Deadline)
	if want := tightestExternalReleaseThreshold - composed.TerminationGrace - 2*composed.PropagationMargin; composedDeadline != want {
		t.Fatalf("composed orphan deadline = %s, want the derived %s (threshold − grace − margin − one margin of headroom)", composedDeadline, want)
	}
	if composedDeadline != 115*time.Second {
		t.Fatalf("composed orphan deadline = %s, want 115 s for a three-minute threshold", composedDeadline)
	}
	window := DefaultSessionShimReadoptionPolicy().WorstCaseWindow()
	for _, deadline := range []struct {
		name string
		d    time.Duration
	}{
		{name: "composed deadline derived from the tightest external release threshold", d: composedDeadline},
		{name: "legacy launched-shim deadline", d: legacyLaunchedShimDeadline},
		{name: "standalone default", d: sessionshim.DefaultOrphanDeadline},
	} {
		if window >= deadline.d {
			t.Errorf("default re-adoption window %s is not strictly inside the %s (%s)", window, deadline.name, deadline.d)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	// 3 × 15 s + (5 s + 10 s) — see DefaultSessionShimReadoptionPolicy.
	if want := 60 * time.Second; window != want {
		t.Fatalf("default re-adoption window = %s, want %s", window, want)
	}
	if got := (SessionShimConfig{}).readoption(); got != DefaultSessionShimReadoptionPolicy() {
		t.Fatalf("zero config resolves to %+v, want the default policy %+v", got, DefaultSessionShimReadoptionPolicy())
	}
	// A hosted callback timeout of 30 s would have bounded one attempt at
	// 4 × 30 s = 120 s; the policy's own bound must win.
	hosted := SessionShimConfig{CallbackTimeout: 30 * time.Second}
	if got := hosted.readoptionAttemptTimeout(); got != defaultSessionShimReadoptionAttemptTimeout {
		t.Fatalf("hosted attempt timeout = %s, want the policy's %s", got, defaultSessionShimReadoptionAttemptTimeout)
	}
	// And a publication timeout smaller than the policy's still binds.
	tight := SessionShimConfig{CallbackTimeout: time.Second}
	if got := tight.readoptionAttemptTimeout(); got != 4*time.Second {
		t.Fatalf("tight attempt timeout = %s, want the 4 s publication timeout", got)
	}
}

// TestReadoptionAttemptIsBoundedByThePolicyAttemptTimeout pins that the bound
// WorstCaseWindow sums is the bound an attempt really runs under. The durable
// adoption callback blocks until its context ends; with the policy's attempt
// timeout far below the callback timeout the attempt must end at the policy's
// bound, not the launch path's.
func TestReadoptionAttemptIsBoundedByThePolicyAttemptTimeout(t *testing.T) {
	t.Parallel()
	const attemptTimeout = 200 * time.Millisecond
	f := newReadoptFixtureBlockingAdoption(t, SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond, AttemptTimeout: attemptTimeout})
	d := f.daemon
	lost := f.lostEntry(t)

	started := time.Now()
	readopted := d.readoptSessionShimAfterControllerLoss(f.id, lost)
	elapsed := time.Since(started)
	if readopted {
		t.Fatal("re-adoption reported success though its durable adoption never answered")
	}
	// Generous slack for a loaded -race run, but far below the fixture's 5 s
	// callback timeout (and the 20 s publication timeout) the launch path
	// would allow the same callback.
	if elapsed >= 2*time.Second {
		t.Fatalf("one re-adoption attempt took %s, want it bounded by the policy's %s attempt timeout", elapsed, attemptTimeout)
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("re-adoption quarantined on its own: %+v — that disposition belongs to the caller", projected)
	}
}

func TestReadoptionPolicyWorstCaseWindowIsAttemptsPlusBackoffs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy SessionShimReadoptionPolicy
		want   time.Duration
	}{
		{name: "zero resolves to the default", policy: SessionShimReadoptionPolicy{}, want: 60 * time.Second},
		{name: "one attempt sleeps nothing", policy: SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Hour, AttemptTimeout: 7 * time.Second}, want: 7 * time.Second},
		{name: "backoff doubles between attempts", policy: SessionShimReadoptionPolicy{Attempts: 4, Backoff: time.Second, AttemptTimeout: 2 * time.Second}, want: 4*2*time.Second + (1+2+4)*time.Second},
	} {
		if got := tc.policy.WorstCaseWindow(); got != tc.want {
			t.Errorf("%s: WorstCaseWindow() = %s, want %s", tc.name, got, tc.want)
		}
	}
}
