package daemon

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestReadoptedOutputBatchWaitsForTheAdoptionGate pins the re-adoption race
// where the event consumer starts before the durable carrier is installed.
//
// The consumer gathers buffered harness output into a batch and — before this
// change — persisted it at once. During re-adoption the carrier dial is still
// in flight at that instant, so the persist met "no carrier candidate" and the
// consumer classified it as carrier loss: it closed the controller while the
// adoption commit was in flight and the lineage never re-adopted. The fix holds
// output batches on the adoption gate the staged-Snapshot path already uses:
// the batch callback runs only after gate.finish(true), and a refused gate
// stops the consumer without manufacturing a carrier loss.
//
// The table covers the incident path — a first dial that times out, then a
// second that succeeds — plus the failing-gate case, which must still surface
// a real carrier loss (there the batch callback itself refuses, after the gate
// committed).
func TestReadoptedOutputBatchWaitsForTheAdoptionGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// failFirstDial models the incident's dial-1 timeout: the first
		// adoption's consumer observes a refused gate and must neither persist
		// nor re-adopt on its own.
		failFirstDial bool
		// failBatch, when set, makes the batch callback refuse AFTER the gate
		// committed — a real carrier loss, which must still close the stream
		// and release the lineage for re-adoption.
		failBatch bool
		// wantBatchCalls is how many batch persist calls the test expects in
		// total across both dials.
		wantBatchCalls int
		// wantQuarantined reports whether the lineage must end quarantined.
		wantQuarantined bool
	}{
		{name: "slow dial holds buffered output until the gate commits", failFirstDial: true, wantBatchCalls: 1},
		{name: "refused batch after commit still surfaces carrier loss", failBatch: true, wantBatchCalls: 1, wantQuarantined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := newAdoptionGateBatchFixture(t, ctx)
			d := f.daemon

			var batchCalls atomic.Int32
			var batchBeforeGate atomic.Int32
			var gateCommitted atomic.Bool
			var batchDone = make(chan struct{})
			d.opts.SessionShim.OnSessionEventDurable = func(_ sessionshim.Identity, _ sessionshim.ControllerEvent) error {
				return nil
			}
			d.opts.SessionShim.OnSessionEventDurableBatch = func(_ context.Context, _ sessionshim.Identity, batch []sessionshim.ControllerEvent) (uint64, error) {
				if !gateCommitted.Load() {
					batchBeforeGate.Add(1)
				}
				n := batchCalls.Add(1)
				if n == int32(tc.wantBatchCalls) {
					close(batchDone)
				}
				if tc.failBatch {
					return 0, errTestCarrierRefused
				}
				if len(batch) != len(f.source) {
					t.Errorf("batch frames = %d, want the %d buffered output frames", len(batch), len(f.source))
				}
				for i, event := range batch {
					if !bytes.Equal(event.FrameBytes, f.source[i]) {
						t.Errorf("batch frame %d bytes changed", i)
					}
				}
				return batch[len(batch)-1].Seq, nil
			}

			// Dial 1: adopt, start its consumer on a gate the test refuses —
			// the incident's timed-out first dial. Its consumer must never
			// persist and must leave the adopted entry alone.
			first := f.adopt(t, ctx, "controller-gate-first")
			gate1 := newShimAdoptionGate()
			d.consumeShimEventsGated(first, gate1)
			if tc.failFirstDial {
				gate1.finish(false)
				_ = first.Close()
				// The refused consumer returns without persisting and without
				// touching the adopted set; give its goroutine a scheduler
				// quantum to exit, then assert nothing was persisted.
				time.Sleep(200 * time.Millisecond)
				if got := batchCalls.Load(); got != 0 {
					t.Fatalf("refused first dial persisted %d batches, want none", got)
				}
				if projected := d.QuarantinedSessions(); len(projected) != 0 {
					t.Fatalf("refused first dial quarantined: %+v", projected)
				}
			} else {
				// No first-dial failure on this path: dial 1's consumer is
				// still parked on its gate. Refuse it now and join it before
				// dial 2, so the waits below observe only the second dial.
				gate1.finish(false)
				_ = first.Close()
				d.shims.wg.Wait()
			}

			// Both paths above joined dial 1's consumer before reaching
			// here, so the waits below observe only dial 2.
			// Dial 2: the succeeding re-adoption. Its consumer reads the same
			// buffered harness output; the commit resolves its gate.
			second := f.adopt(t, ctx, "controller-gate-second")
			// The release path re-adopts inline on the consumer goroutine via
			// the daemon's configured policy; disable re-adoption for this
			// unit so the refused batch surfaces its quarantine row instead
			// of re-entering the adoption pipeline on the same goroutine.
			d.opts.SessionShim.Readoption = SessionShimReadoptionPolicy{Disabled: true}
			seedAdoptedBatchEntry(d, f.id, second)
			gate2 := newShimAdoptionGate()
			d.consumeShimEventsGated(second, gate2)
			// The batch must not be persisted while the dial is in flight.
			select {
			case <-batchDone:
				t.Fatal("batch was persisted before the adoption gate committed")
			case <-time.After(200 * time.Millisecond):
			}
			gateCommitted.Store(true)
			gate2.finish(true)
			select {
			case <-batchDone:
			case <-ctx.Done():
				t.Fatal("committed adoption never flushed the buffered output batch")
			}
			if got := batchCalls.Load(); got != int32(tc.wantBatchCalls) {
				t.Fatalf("batch calls = %d, want %d", got, tc.wantBatchCalls)
			}
			if got := batchBeforeGate.Load(); got != 0 {
				t.Fatalf("batch was persisted %d times before the gate committed", got)
			}
			if tc.wantQuarantined {
				// A refused batch after commit is still a real carrier loss:
				// the consumer closes the stream and the release path —
				// with re-adoption disabled — quarantines at once. Join the
				// consumer, then the adopted entry must be gone and the
				// quarantine row present.
				d.shims.wg.Wait()
				select {
				case <-second.Done():
				default:
					t.Fatal("refused batch left the controller open; want the carrier-loss close")
				}
				if _, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID); err == nil {
					t.Fatal("refused batch left the lineage adopted; want it quarantined")
				}
				waitFor(t, 10*time.Second, "the refused batch to quarantine the lineage", func() bool {
					return len(d.QuarantinedSessions()) == 1
				})
				_ = second.Close()
				d.shims.wg.Wait()
				return
			}
			// End the stream from the shim side: the harness exits, the shim
			// publishes its terminal proof, and the committed consumer lands
			// through the terminal path — never through quarantine.
			terminateCtx, terminateCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := f.shim.Terminate(terminateCtx); err != nil {
				terminateCancel()
				t.Fatalf("terminate harness: %v", err)
			}
			terminateCancel()
			d.shims.wg.Wait()
			if projected := d.QuarantinedSessions(); len(projected) != 0 {
				t.Fatalf("committed adoption quarantined: %+v", projected)
			}
		})
	}
}

// errTestCarrierRefused is the batch callback's post-commit refusal: a real
// carrier loss the consumer must still act on.
var errTestCarrierRefused = errTestCarrierRefusedValue{}

type errTestCarrierRefusedValue struct{}

func (errTestCarrierRefusedValue) Error() string { return "test carrier refused durable output batch" }

// adoptionGateBatchFixture is one live shim with deterministic buffered output
// the test can re-adopt at will: each adopt replays the same output frames.
type adoptionGateBatchFixture struct {
	daemon   *Daemon
	registry *sessionshim.Registry
	id       sessionshim.Identity
	shim     *sessionshim.Shim
	source   [][]byte
	dir      string
}

// newAdoptionGateBatchFixture starts a real shim behind a PTY source with a
// fixed, barrier-terminated output prefix, and a daemon whose adopted entry is
// intentionally left empty: each subtest seeds exactly the entry its dial
// owns.
func newAdoptionGateBatchFixture(t *testing.T, ctx context.Context) *adoptionGateBatchFixture {
	t.Helper()
	// A Unix socket path has a short platform limit, and t.TempDir() bakes
	// the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "rgb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(dir + "/registry")
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-gate", SessionID: "session-gate"}
	sourceControl, sourceSpec := newBatchPTYSource(ctx, t, dir)
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   sourceSpec,
		Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second, PropagationMargin: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shim.Terminate(context.Background()) })
	direct, err := shim.Session().Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close() //nolint:errcheck // fixture subscription
	var source [][]byte
	for _, b := range []byte("abcdef") {
		source = append(source, sourceControl.emit(ctx, t, direct.Frames(), b))
	}
	if err := shim.Session().EmitMarker("barrier"); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{shims: newSessionShimState()}
	d.opts.SessionShim = SessionShimConfig{CallbackTimeout: 5 * time.Second}
	d.shimIdentityRef.Store(&sessionShimIdentity{config: &d.opts.SessionShim})
	d.shims.mu.Lock()
	d.shims.registry = registry
	d.shims.mu.Unlock()
	f := &adoptionGateBatchFixture{daemon: d, registry: registry, id: id, shim: shim, source: source, dir: dir}
	t.Cleanup(func() { d.shims.wg.Wait() })
	return f
}

func (f *adoptionGateBatchFixture) adopt(t *testing.T, ctx context.Context, controllerID string) *sessionshim.Controller {
	t.Helper()
	result, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: f.registry, ControllerID: controllerID, RequireFullHostFrames: true,
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("adopt = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	return result.Adopted[0]
}

// seedAdoptedBatchEntry installs the adopted entry a consumer's release path
// checks: the exact controller under test, so a carrier loss the consumer
// reports is observable as a quarantine rather than a no-op.
func seedAdoptedBatchEntry(d *Daemon, id sessionshim.Identity, ctrl *sessionshim.Controller) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	d.shims.adopted[id] = adoptedShim{
		controller: ctrl, shimID: ctrl.Hello().ShimID,
		adoption: SessionShimAdoptionEvidence{Identity: id, ControllerGeneration: uint64(ctrl.Generation())},
	}
}
