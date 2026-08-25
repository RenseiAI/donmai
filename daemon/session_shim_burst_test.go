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
	daemon   *Daemon
	identity sessionshim.Identity
	shim     *sessionshim.Shim
}

func newReadoptedBurstFixture(
	t *testing.T,
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
		Spec: ptyhost.Spec{Command: []string{
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
	return &readoptedBurstFixture{daemon: replacement, identity: id, shim: shim}
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
// The burst stays inside sessionshim.EventBacklogSlack on purpose. That number
// IS the absorption guarantee, so a burst within it must survive on the slowest
// runner, and a burst beyond it is covered by
// TestBurstBeyondTheAdvertisedSlackFailsClosedHonestly instead.
func TestStartupReadoptedSessionDrainsAheadOfItsDurableCursor(t *testing.T) {
	var (
		mu       sync.Mutex
		observed strings.Builder
		frames   int
		maxLag   uint64
	)
	fixture := newReadoptedBurstFixture(t, func(d *Daemon, id sessionshim.Identity, event sessionshim.ControllerEvent) {
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

	// Real geometry through the shim-owned PTY: one applied-Resize host frame
	// per cycle, which is what a terminal redraw looks like on the wire. Half
	// the advertised slack leaves room for the harness's own echo frames.
	const burst = sessionshim.EventBacklogSlack / 2
	for cycle := range burst {
		cols := uint32(99 + (cycle & 1))
		rows := uint32(29 + ((cycle >> 1) & 1))
		if err := daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, cols, rows, 0, 0); err != nil {
			mu.Lock()
			seen := frames
			mu.Unlock()
			t.Fatalf("resize %d of %d within the advertised backlog slack (%d): %v (carrier had %d frames)",
				cycle, burst, sessionshim.EventBacklogSlack, err, seen)
		}
	}

	// The control channel must still be the daemon's, and still be usable.
	if adopted := daemon.AdoptedSessionShims(); len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("adopted sessions after the burst = %+v, want exactly [%s]", adopted, id)
	}
	if err := daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("after-burst\r")); err != nil {
		t.Fatalf("write to the re-adopted session after the burst: %v", err)
	}
	waitFor(t, 30*time.Second, "the harness to answer through the re-adopted controller", func() bool {
		mu.Lock()
		defer mu.Unlock()
		// The burst floods the shim-owned ring, so the harness answers the
		// accumulated redraw bytes and this line together. Matching the token
		// rather than a line prefix keeps this about liveness, not layout.
		return strings.Contains(observed.String(), "after-burst")
	})
	// Coalescing must not stall the resume point: it still has to advance.
	waitFor(t, 30*time.Second, "the durable forwarded cursor to advance past the burst", func() bool {
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

// TestBurstBeyondTheAdvertisedSlackFailsClosedHonestly covers the other side of
// the same guarantee: what happens past sessionshim.EventBacklogSlack.
//
// The reader must never block on a consumer — it is the only goroutine that can
// receive a durable heartbeat receipt — so beyond the slack the controller
// fails closed and drops the connection. That is deliberate and stays. What the
// daemon owes is honesty about it: release the session rather than keep
// publishing it as adopted against a socket nobody can write to. Before the
// fix it kept the dead entry, so `host status` showed a running session and
// every later call returned "use of closed network connection" forever.
//
// Holding the carrier's own callback is what makes this deterministic: the
// consumer cannot drain while it is held, so the slack is exceeded by
// construction rather than by out-running a scheduler.
func TestBurstBeyondTheAdvertisedSlackFailsClosedHonestly(t *testing.T) {
	var holding atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	held := make(chan struct{}, 1)
	fixture := newReadoptedBurstFixture(t, func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent) {
		// Adoption itself runs through this callback (the mandatory Snapshot is
		// staged on the consumer), so the hold only arms once the session is
		// published and activated.
		if !holding.Load() {
			return
		}
		select {
		case held <- struct{}{}:
		default:
		}
		<-release
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	id := fixture.identity
	daemon := fixture.daemon
	holding.Store(true)

	// Push well past the slack while nothing can drain it.
	const burst = 4 * sessionshim.EventBacklogSlack
	overflowed := false
	for cycle := range burst {
		if err := daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, uint32(99+(cycle&1)), 29, 0, 0); err != nil {
			overflowed = true
			break
		}
	}
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the held carrier callback never received a frame")
	}
	releaseOnce.Do(func() { close(release) })
	if !overflowed {
		waitFor(t, 30*time.Second, "the undrained backlog to fail closed", func() bool {
			return daemon.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, 100, 30, 0, 0) != nil
		})
	}

	waitFor(t, 30*time.Second, "the failed-closed controller to release its session", func() bool {
		return len(daemon.AdoptedSessionShims()) == 0 && len(daemon.QuarantinedSessions()) == 1
	})
	if got := daemon.SessionShimOccupancy(); got != 1 {
		t.Fatalf("occupancy after failing closed = %d, want 1 while the harness is live", got)
	}
	quarantined := daemon.QuarantinedSessions()
	if quarantined[0].Identity() != id ||
		quarantined[0].Reason != sessionshim.QuarantineSocketUnreachable || !quarantined[0].ConsumesCapacity {
		t.Fatalf("quarantine after failing closed = %+v", quarantined[0])
	}
	err := daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("refused\r"))
	if err == nil || !strings.Contains(err.Error(), "is not adopted by this daemon") {
		t.Fatalf("write after failing closed = %v, want an honest not-adopted refusal", err)
	}
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

	waitFor(t, 30*time.Second, "the refused frame to release shim ownership", func() bool {
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
