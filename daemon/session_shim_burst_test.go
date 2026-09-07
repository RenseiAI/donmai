package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// readoptedBurstFixture is a live session this daemon adopted at STARTUP, at
// generation N+1, from a harness that was already running and producing.
//
// That is the shape the fix is about. A session this daemon just launched has
// produced nothing yet; a re-adopted one is attached to a harness mid-flight,
// and its consumer is additionally parked on the activation gate for the whole
// composing-callback window.
type readoptedBurstFixture struct {
	daemon      *Daemon
	identity    sessionshim.Identity
	shim        *sessionshim.Shim
	registryDir string
}

func newReadoptedBurstFixture(
	t *testing.T,
	backlogBudget int,
	backlogStall time.Duration,
	ringBytes int,
	observe func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent),
) *readoptedBurstFixture {
	t.Helper()
	// Crossing the stall deadline no longer drops anything, so a fixture that
	// wants a drop has to reach the bound the hold is measured against. Twice
	// the deadline keeps the wait proportional to whatever the caller chose.
	return newReadoptedBurstFixtureWithDropBound(t, backlogBudget, backlogStall, 2*backlogStall, ringBytes, observe)
}

func newReadoptedBurstFixtureWithDropBound(
	t *testing.T,
	backlogBudget int,
	backlogStall time.Duration,
	backlogDropBound time.Duration,
	ringBytes int,
	observe func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent),
) *readoptedBurstFixture {
	t.Helper()
	// A Unix socket path has a short platform limit; keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "dsb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registryDir := filepath.Join(dir, "registry")
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-burst", SessionID: "session-burst"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 5,
		Spec: ptyhost.Spec{RingBytes: ringBytes, Command: []string{
			"/bin/sh", "-c",
			`stty -echo; while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`,
		}},
		WorkareaPath: filepath.Join(dir, "workarea"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})

	// The daemon that launched this session. It walks away without stopping
	// anything, exactly as a service-manager restart does.
	first, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "first-controller", RequireFullHostFrames: true,
	})
	if err != nil || len(first.Adopted) != 1 {
		t.Fatalf("first adoption = %+v err=%v", first, err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		//nolint:revive // draining is the whole point; the loop body is empty by design
		for range first.Adopted[0].Events() {
		}
	}()
	first.Close()
	<-drained

	var (
		mu           sync.Mutex
		snapshotSeqs = make(map[sessionshim.Identity]uint64)
	)
	const carrierEpoch = uint64(70)
	// Declared before New so the carrier callback can reach the daemon from the
	// very first frame: the consumer starts inside adoption, before New returns.
	var replacement *Daemon
	replacement = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: registryDir, HostID: "host-burst", OrgID: id.OrgID,
		AdoptionBatchOrgIDs:          []string{id.OrgID},
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		// One bounded attempt with a token backoff. Every assertion in this
		// file is about the DISPOSITION a lost carrier reaches, never about how
		// many times it was retried, and the default policy's three attempts
		// with a doubling backoff add tens of seconds of wall clock to the
		// slowest package in the module — which is how a throughput assertion
		// in an unrelated package starts failing on a loaded runner.
		Readoption: SessionShimReadoptionPolicy{Attempts: 1, Backoff: 5 * time.Millisecond},

		EventBacklogBudget:         backlogBudget,
		EventBacklogStallDeadline:  backlogStall,
		EventBacklogDropBound:      backlogDropBound,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			return sessionshim.PreparedAdoption{
				ControllerGeneration: preparation.CurrentControllerGeneration + 1,
				Extensions: shimwire.Extensions{Values: map[string]string{
					shimwire.ExtCarrierEpoch: fmt.Sprintf("%d", carrierEpoch),
				}},
				ResumeFrom: proofResolvedResume(preparation),
			}, nil
		},
		OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			result, err := evidence.SnapshotProxy.Emit(ctx)
			if err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
			mu.Lock()
			snapshotSeqs[evidence.Identity] = result.AtSeq + 1
			mu.Unlock()
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("burst-adoption")}, nil
		},
		OnSessionEvent: func(id sessionshim.Identity, event sessionshim.ControllerEvent) {
			observe(replacement, id, event)
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("burst-batch"), AdoptionRevision: "burst-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			receipts := make([]SessionShimCarrierActivationReceipt, 0, len(publication.Carriers))
			for _, carrier := range publication.Carriers {
				carrierID := sessionshim.Identity{OrgID: carrier.OrgID, SessionID: carrier.SessionID}
				receipts = append(receipts, SessionShimCarrierActivationReceipt{
					Activation: carrier, AckSeq: snapshotSeqs[carrierID],
				})
			}
			return receipts, nil
		},
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {},
	}})
	enableHostedFullHostFramesForTest(t, replacement, id.OrgID)
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup adoption: %v", err)
	}
	entry, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adopted entry after startup adoption: %v", err)
	}
	if entry.controller.Generation() != 2 || !entry.controller.Adoption().Contiguous {
		t.Fatalf("startup re-adoption = generation %d contiguous %v, want generation 2 contiguous",
			entry.controller.Generation(), entry.controller.Adoption().Contiguous)
	}
	return &readoptedBurstFixture{
		daemon: replacement, identity: id, shim: shim, registryDir: registryDir,
	}
}

// TestStartupReadoptedSessionDrainsAheadOfItsDurableCursor pins the fix for a
// real installed-service restart failure, and it pins it structurally rather
// than by racing a stopwatch.
//
// The field symptom: a replacement daemon re-adopts a live session, publishes
// it, and then every write to that shim fails with "use of closed network
// connection" while the harness stays alive. The daemon had closed its OWN
// socket. Selected v3 acknowledged each forwarded HostFrame through an
// fsync-backed round trip to the shim, inline on the goroutine that drains the
// stream, which caps the drain at roughly one frame per fsync — measured here
// at ~40 frames a second. The controller's backlog slack is finite and
// fail-closed, so a consumer that falls behind does not slow the stream down:
// the reader drops the connection.
//
// The assertion is the invariant that fix creates: the carrier must be able to
// receive a frame while the durable cursor is still MORE THAN ONE sequence
// behind it. Acknowledging inline makes that arithmetically impossible — the
// cursor is advanced to N before frame N+1 is ever delivered, so the lag is
// exactly one, always. Reverting the off-path acknowledger therefore turns this
// RED by construction, on any machine, at any speed, with no timing threshold
// to tune.
//
// The burst is the product's own acceptance seam at its real depth, because
// that is the volume the guarantee has to cover: the daemon's in-flight budget
// now equals the shim's ring budget, so the controller can no longer be the
// first component to give up on volume the shim absorbs by design. Past the
// budget is covered by TestBacklogBudgetOverrunFailsClosedHonestly instead.
func TestStartupReadoptedSessionDrainsAheadOfItsDurableCursor(t *testing.T) {
	var (
		mu       sync.Mutex
		observed strings.Builder
		frames   int
		maxLag   uint64
	)
	fixture := newReadoptedBurstFixture(t, 0, 0, 0, func(d *Daemon, id sessionshim.Identity, event sessionshim.ControllerEvent) {
		// Sampled from the carrier's own seat, at the instant the frame is
		// handed over, which is the only place the question is meaningful.
		cursor := d.SessionShimForwardedSeq(id.OrgID, id.SessionID)
		mu.Lock()
		defer mu.Unlock()
		frames++
		observed.Write(event.Data)
		if event.Seq > cursor+1 {
			if lag := event.Seq - 1 - cursor; lag > maxLag {
				maxLag = lag
			}
		}
	})
	id := fixture.identity
	daemon := fixture.daemon
	before := daemon.SessionShimForwardedSeq(id.OrgID, id.SessionID)

	// The product's own acceptance seam, at its real depth: 4096 alternating
	// geometry cycles with a redraw every 32. Nothing shallower is a fair test —
	// this is the volume the restart fixture drives to make the shim's ring
	// evict, and the daemon has to still be there when it does.
	if err := daemon.forceSessionShimAcceptanceGap(id); err != nil {
		mu.Lock()
		seen := frames
		mu.Unlock()
		t.Fatalf("acceptance-depth burst through the re-adopted controller: %v (carrier had %d frames)", err, seen)
	}

	// The control channel must still be the daemon's, and still be usable.
	if adopted := daemon.AdoptedSessionShims(); len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("adopted sessions after the burst = %+v, want exactly [%s]", adopted, id)
	}
	if err := daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("after-burst\r")); err != nil {
		t.Fatalf("write to the re-adopted session after the burst: %v", err)
	}
	waitFor(t, 60*time.Second, "the harness to answer through the re-adopted controller", func() bool {
		mu.Lock()
		defer mu.Unlock()
		// The burst floods the shim-owned ring, so the harness answers the
		// accumulated redraw bytes and this line together. Matching the token
		// rather than a line prefix keeps this about liveness, not layout.
		return strings.Contains(observed.String(), "after-burst")
	})
	// Coalescing must not stall the resume point: it still has to advance.
	waitFor(t, 60*time.Second, "the durable forwarded cursor to advance past the burst", func() bool {
		return daemon.SessionShimForwardedSeq(id.OrgID, id.SessionID) > before
	})

	mu.Lock()
	lag, delivered := maxLag, frames
	mu.Unlock()
	if lag < 2 {
		t.Fatalf("carrier never got ahead of the durable cursor: max lag %d over %d frames — "+
			"the acknowledgement round trip is back on the drain path", lag, delivered)
	}
}

// TestBacklogBudgetOverrunFailsClosedHonestly covers the other side of the same
// guarantee: what happens when a consumer STOPS.
//
// Reaching the budget is not that decision — a consumer that is merely behind
// gets back-pressure, which is what
// TestBackPressureKeepsTheCarrierWhenTheConsumerIsBehind covers. Crossing the
// stall deadline is not that decision either, any more: that publishes the
// carrier as degraded and keeps stalling, which is what
// TestSlowDurableConsumerKeepsItsCarrierAndIsPublishedDegraded covers. But the
// reader still may not stall forever — it is the only goroutine that can
// receive a durable heartbeat receipt — so past the DROP BOUND the controller
// fails closed and drops the connection. That is deliberate and stays. What the
// daemon owes is honesty about it: release the session rather than keep
// publishing it as adopted against a socket nobody can write to. Before the fix
// it kept the dead entry, so `host status` showed a running session and every
// later call returned "use of closed network connection" forever.
//
// Three things make this deterministic rather than a race with the scheduler:
// the carrier's own callback is held for the whole decision, so the consumer
// genuinely never drains; the budget is set small through the public config
// seam, so the bound is reached by construction instead of by generating
// megabytes through a PTY; and the stall deadline and drop bound are set short
// through the same seam, so the test does not wait out the production ten
// minutes.
func TestBacklogBudgetOverrunFailsClosedHonestly(t *testing.T) {
	var holding atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	fixture := newReadoptedBurstFixtureWithDropBound(t, 8<<10, 500*time.Millisecond, 500*time.Millisecond, 0,
		func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent) {
			// Adoption itself runs through this callback (the mandatory Snapshot
			// is staged on the consumer), so the hold only arms once the session
			// is published and activated.
			if !holding.Load() {
				return
			}
			<-release
		})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	id := fixture.identity
	daemon := fixture.daemon
	// The session is idle here, so the consumer is parked on an empty stream and
	// the FIRST frame of the burst is the one that blocks in the carrier.
	holding.Store(true)

	// Push well past the small budget while nothing can drain it.
	const burst = 4096
	overflowed := false
	for cycle := range burst {
		if err := daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, uint32(99+(cycle&1)), 29, 0, 0); err != nil {
			overflowed = true
			break
		}
	}
	if !overflowed {
		waitFor(t, 60*time.Second, "the stalled backlog to fail closed on a stuck consumer", func() bool {
			return daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, 100, 30, 0, 0) != nil
		})
	}
	// Released only AFTER the fail-closed decision, so the consumer was
	// genuinely stopped for the whole of it rather than merely behind.
	releaseOnce.Do(func() { close(release) })

	waitFor(t, 60*time.Second, "the failed-closed controller to release its session", func() bool {
		return len(daemon.AdoptedSessionShims()) == 0 && len(daemon.QuarantinedSessions()) == 1
	})
	if got := daemon.SessionShimOccupancy(); got != 1 {
		t.Fatalf("occupancy after failing closed = %d, want 1 while the harness is live", got)
	}
	quarantined := daemon.QuarantinedSessions()
	// The REASON moved with the classification, and that is the point of it: a
	// reader that gave up on this daemon's own consumer observed nothing about
	// the socket, so it may not publish `socket_unreachable` — the reason a
	// control plane terminalizes on. It reaches quarantine only after the
	// re-adoption attempts are spent, which the detail records.
	if quarantined[0].Identity() != id || !quarantined[0].ConsumesCapacity {
		t.Fatalf("quarantine after failing closed = %+v", quarantined[0])
	}
	if quarantined[0].Reason == sessionshim.QuarantineSocketUnreachable {
		t.Fatalf("quarantine reason = %q for a shim that answered throughout", quarantined[0].Reason)
	}
	if quarantined[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine reason = %q, want %q", quarantined[0].Reason, sessionshim.QuarantineDurableAckTimeout)
	}
	if quarantined[0].Detail != sessionShimReadoptionAttemptsSpentDetail {
		t.Fatalf("quarantine detail = %q, want %q — the re-adoption path was never taken",
			quarantined[0].Detail, sessionShimReadoptionAttemptsSpentDetail)
	}
	err := daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("refused\r"))
	if err == nil || !strings.Contains(err.Error(), "is not adopted by this daemon") {
		t.Fatalf("write after failing closed = %v, want an honest not-adopted refusal", err)
	}
}

// TestBackPressureKeepsTheCarrierWhenTheConsumerIsBehind is the daemon-level pin
// for the failure this whole change is about.
//
// Field symptom, twice in one day on production hosts: a re-adopted lineage
// pushed more through its carrier than the consumer could take at that instant,
// the controller answered "event backlog exceeded the in-flight budget of
// 8388608", dropped the shim connection, and a healthy seat was quarantined and
// later reaped. The consumer was never stuck — it was BEHIND, which every other
// layer of this stack answers with back-pressure.
//
// So the budget is a high-water mark now: the reader stalls, which stalls the
// shim's output pump behind a socket nobody is draining, and the shim's ring
// does the one job it exists for. The carrier survives.
//
// The consumer here drains on every event but slowly enough that a burst of
// thousands cannot keep up with it, against a budget small enough that the bound
// is reached by construction. Restoring the immediate refusal in
// eventBacklog.push turns this RED at the first assertion: the session is gone
// from the adopted set and sitting in quarantine.
func TestBackPressureKeepsTheCarrierWhenTheConsumerIsBehind(t *testing.T) {
	var delivered atomic.Int64
	fixture := newReadoptedBurstFixture(t, 8<<10, 0, 0, func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent) {
		delivered.Add(1)
		// Slow, never stopped. This is the distinction the fix turns on.
		time.Sleep(200 * time.Microsecond)
	})
	id := fixture.identity
	daemon := fixture.daemon

	const burst = 2048
	for cycle := range burst {
		//nolint:gosec // G115: the alternation is 0 or 1 on a small literal
		if err := daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, uint32(99+(cycle&1)), 29, 0, 0); err != nil {
			t.Fatalf("resize %d through a merely-behind carrier: %v — "+
				"back-pressure was replaced by a drop (%d events delivered)", cycle, err, delivered.Load())
		}
	}

	if adopted := daemon.AdoptedSessionShims(); len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("adopted sessions after the burst = %+v, want exactly [%s]; quarantined %+v",
			adopted, id, daemon.QuarantinedSessions())
	}
	if quarantined := daemon.QuarantinedSessions(); len(quarantined) != 0 {
		t.Fatalf("quarantined after a burst a behind consumer absorbed = %+v", quarantined)
	}
	// The control channel is still this daemon's, and still usable.
	if err := daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("after-backpressure\r")); err != nil {
		t.Fatalf("write to the session after the burst: %v", err)
	}
	waitFor(t, 60*time.Second, "the behind consumer to keep receiving after the burst", func() bool {
		return delivered.Load() > burst/2
	})
}

// TestConsumerDropReleasesShimOwnershipInsteadOfStrandingIt pins the same
// release contract on the reachable non-overflow path: a durable carrier that
// refuses a frame.
//
// Before the fix the consumer closed the connection and returned, leaving the
// entry in the adopted map forever: capacity stayed charged as adopted rather
// than quarantined, and every input/resize came back with "use of closed
// network connection" instead of an honest refusal.
//
// Restoring the bare `return` on that path turns this RED at the quarantine
// assertion.
func TestConsumerDropReleasesShimOwnershipInsteadOfStrandingIt(t *testing.T) {
	f := newShimSpawnFixture(t)
	f.daemon.opts.SessionShim.OnSessionEventDurable = func(sessionshim.Identity, sessionshim.ControllerEvent) error {
		return fmt.Errorf("durable carrier refused the frame")
	}
	spec := f.interactiveSpec("sess-carrier-drop")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	entry, err := f.daemon.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if err := f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("drop\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}

	waitFor(t, 60*time.Second, "the refused frame to release shim ownership", func() bool {
		return len(f.daemon.AdoptedSessionShims()) == 0 && len(f.daemon.QuarantinedSessions()) == 1
	})
	if got := f.daemon.SessionShimOccupancy(); got != 1 {
		t.Fatalf("occupancy after the consumer dropped its connection = %d, want 1 while the harness is live", got)
	}
	quarantined := f.daemon.QuarantinedSessions()
	if quarantined[0].Identity() != id || quarantined[0].ShimID != entry.shimID ||
		quarantined[0].Reason != sessionshim.QuarantineSocketUnreachable || !quarantined[0].ConsumesCapacity {
		t.Fatalf("quarantine after the consumer dropped its connection = %+v", quarantined[0])
	}
	err = f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("refused\r"))
	if err == nil || !strings.Contains(err.Error(), "is not adopted by this daemon") {
		t.Fatalf("write after release = %v, want an honest not-adopted refusal", err)
	}
}

// TestSlowDurableConsumerKeepsItsCarrierAndIsPublishedDegraded is the daemon-level
// pin for the kill switch this change removes.
//
// Field shape, seven interactive seats in one day on production hosts: the
// durable consumer — a control plane persisting every host frame — stalled for
// tens of seconds on a slow datastore path. Every lost seat was output-heavy, so
// the in-flight budget filled in seconds; the no-progress window then elapsed,
// the controller dropped the shim connection, the session was failed, and the
// harness was signalled. Nothing was wrong with the socket, the shim, or the
// harness. Persistence was slow.
//
// So a stall past the deadline is now a published DEGRADATION with the carrier
// held, and the seat survives its consumer coming back. This drives exactly
// that: the consumer is held for longer than the whole stall deadline while the
// producer saturates the budget, and the assertions are that the session is
// still adopted, still unquarantined, published as degraded with the bytes it is
// holding, and — once the consumer returns — usable on the SAME carrier with the
// degradation withdrawn.
//
// Restoring the fail-closed decision at the stall deadline turns this RED at the
// first assertion: the session is gone from the adopted set and sitting in
// quarantine.
func TestSlowDurableConsumerKeepsItsCarrierAndIsPublishedDegraded(t *testing.T) {
	var holding atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	// A stall deadline the package clamps up to its floor, against a drop bound
	// far beyond it: the window under test is the one BETWEEN the two, which is
	// where every one of those seats died.
	fixture := newReadoptedBurstFixtureWithDropBound(t, 8<<10, time.Millisecond, 10*time.Minute, 0,
		func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent) {
			if !holding.Load() {
				return
			}
			<-release
		})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	id := fixture.identity
	daemon := fixture.daemon
	holding.Store(true)

	// Saturate the budget behind a consumer that is not draining. Every one of
	// these used to be the frame that severed the carrier.
	go func() {
		for cycle := range 4096 {
			//nolint:gosec // G115: the alternation is 0 or 1 on a small literal
			if err := daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, uint32(99+(cycle&1)), 29, 0, 0); err != nil {
				return
			}
		}
	}()

	// The degradation has to become VISIBLE, not merely survivable: an operator
	// looking at this host must be able to see that the session is alive and
	// that nothing is draining it.
	var degraded bool
	waitFor(t, 120*time.Second, "the stalled carrier to be published as degraded", func() bool {
		status := daemon.SessionShimDiagnostics()
		for _, adopted := range status.Adopted {
			if adopted.SessionID != id.SessionID || adopted.StreamBackPressure != "degraded" {
				continue
			}
			if adopted.StreamQueuedBytes <= 0 || adopted.StreamBudgetBytes <= 0 {
				t.Errorf("degraded carrier published %+v, want the bytes it holds and the budget it holds them against", adopted)
			}
			if adopted.StreamStalledSince <= 0 {
				t.Errorf("degraded carrier published no stall start: %+v", adopted)
			}
			degraded = true
			return true
		}
		return false
	})
	if !degraded {
		t.Fatal("the carrier was never published as degraded")
	}

	// The carrier is still this daemon's, and the session is still supervised.
	if adopted := daemon.AdoptedSessionShims(); len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("adopted sessions during the stall = %+v, want exactly [%s]; quarantined %+v",
			adopted, id, daemon.QuarantinedSessions())
	}
	if quarantined := daemon.QuarantinedSessions(); len(quarantined) != 0 {
		t.Fatalf("quarantined over a consumer that was slow, not gone: %+v", quarantined)
	}

	// The consumer comes back. The stream continues on the same carrier, and
	// the degradation is withdrawn rather than left standing.
	holding.Store(false)
	releaseOnce.Do(func() { close(release) })
	if err := daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("after-stall\r")); err != nil {
		t.Fatalf("write to the session after the stall: %v", err)
	}
	waitFor(t, 60*time.Second, "the published degradation to be withdrawn", func() bool {
		for _, adopted := range daemon.SessionShimDiagnostics().Adopted {
			if adopted.SessionID == id.SessionID {
				return adopted.StreamBackPressure == ""
			}
		}
		return false
	})
}

// TestDropBoundReadoptsAndNeverPublishesSocketUnreachable drives the drop bound
// end to end and pins what happens on the other side of it.
//
// This is the half the first cut of this change missed. Holding the carrier for
// ten minutes instead of thirty seconds is a very large improvement, but the
// drop that eventually happens was still classified as an ordinary ending: the
// lineage went straight to `socket_unreachable` quarantine with NO re-dial at
// all, and that reason is the one the control plane terminalizes ninety-five
// seconds later. The kill window had moved, not closed.
//
// At the drop the shim is alive — its harness is retained, its socket answers,
// and the closing controller releases the shim's PTY gate on the way out — so
// the drop must take the re-adoption path first, and a re-adopted controller
// arrives with an empty backlog, which is also the recovery.
//
// The two facts asserted here are the two the disposition turns on, and they
// are read from the published quarantine because that is what a control plane
// sees: the REASON is not a claim about a socket nobody observed, and the
// DETAIL says the re-adoption attempts were spent — which only a lineage that
// took the re-dial path can produce. (This fixture's re-adoption cannot
// actually land: it has no acknowledged proof-v2 recovery heartbeat. That is
// what makes the detail the right assertion here; a re-adoption that SUCCEEDS
// keeping the lineage adopted is pinned by TestConsumerStallKeepsTheLineageAdopted.)
//
// Reverting classifyShimStreamEnd's ErrEventBacklogExceeded case turns this RED
// twice over: the reason becomes `socket_unreachable` and the detail becomes the
// plain controller-lost one, because no re-adoption was ever attempted.
func TestDropBoundReadoptsAndNeverPublishesSocketUnreachable(t *testing.T) {
	var holding atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	// Both durations clamp up to the package floor, so the drop lands one whole
	// floor after the consumer first falls behind — reachable in a test without
	// waiting out the production ten minutes.
	fixture := newReadoptedBurstFixtureWithDropBound(t, 8<<10, time.Millisecond, time.Millisecond, 0,
		func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent) {
			if !holding.Load() {
				return
			}
			<-release
		})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	id := fixture.identity
	daemon := fixture.daemon

	holding.Store(true)
	// Long enough to cross the drop, then out of the way: the re-adoption
	// attempts that follow must not additionally queue behind this hold.
	timer := time.AfterFunc(15*time.Second, func() {
		holding.Store(false)
		releaseOnce.Do(func() { close(release) })
	})
	t.Cleanup(func() { timer.Stop() })

	go func() {
		for cycle := range 8192 {
			//nolint:gosec // G115: the alternation is 0 or 1 on a small literal
			if err := daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, uint32(99+(cycle&1)), 29, 0, 0); err != nil {
				return
			}
		}
	}()

	waitFor(t, 180*time.Second, "the drop bound to run the re-adoption path out", func() bool {
		return len(daemon.QuarantinedSessions()) == 1
	})

	quarantined := daemon.QuarantinedSessions()[0]
	if quarantined.Identity() != id {
		t.Fatalf("quarantined the wrong lineage: %+v", quarantined)
	}
	if quarantined.Reason == sessionshim.QuarantineSocketUnreachable {
		t.Fatal("the drop bound published `socket_unreachable` for a shim that answered throughout; " +
			"that is the reason the control plane terminalizes on")
	}
	if quarantined.Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine reason = %q, want %q", quarantined.Reason, sessionshim.QuarantineDurableAckTimeout)
	}
	if quarantined.Detail != sessionShimReadoptionAttemptsSpentDetail {
		t.Fatalf("quarantine detail = %q, want %q — the re-adoption path was never taken",
			quarantined.Detail, sessionShimReadoptionAttemptsSpentDetail)
	}
	if !quarantined.ConsumesCapacity {
		t.Fatal("the withdrawn lineage stopped consuming capacity; its harness is still held")
	}
}
