package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestReadoptedOutputBatchWaitsForCarrierInstall pins the re-adoption race
// where the event consumer starts before the durable carrier is installed.
//
// The consumer gathers buffered harness output into a batch. During
// re-adoption the carrier dial is still in flight at that instant, so a batch
// persisted at once meets "no carrier candidate" and reads as carrier loss:
// the consumer closes the controller while the adoption commit is in flight
// and the lineage never re-adopts. The fix holds the batch until the carrier
// is installed OR the adoption gate finishes, whichever comes first. Staging
// the mandatory Snapshot inside OnAdoption arms the carrier-installed latch;
// the gate itself still carries the adoption verdict, so a refused adoption
// still refuses the parked consumer.
//
// A plain hold on the adoption gate alone deadlocks this pipeline: the
// carrier takeover inside OnAdoption (Emit) waits for the same consumer to
// reach the staged Snapshot, and the Snapshot sits behind the parked batch.
// The latch breaks the cycle: installed releases the batch, the batch unblocks
// the Snapshot, and the Snapshot completes the adoption that resolves the
// gate.
//
// The test runs the real pipeline end to end: a launched shim with buffered
// (unacked) output exchanges, then a replacement daemon running adoptSessionShims
// with OnSessionEventDurableBatch set and an OnAdoption that calls Emit.
// It expects adopted=true, quarantined=0, no Emit error, and the batches
// delivered — and it fails with only the latch removed (the consumer parks on
// the gate, Emit times out waiting for its Snapshot, adoption fails).
func TestReadoptedOutputBatchWaitsForCarrierInstall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A live harness with buffered output the replacement must replay. The
	// batch PTY source emits one exact byte per command behind a barrier, so
	// the stream the replacement adopts holds real unacked output frames.
	dir, err := os.MkdirTemp("/tmp", "rci")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registryDir := filepath.Join(dir, "registry")
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-carrier-install", SessionID: "session-carrier-install"}
	sourceControl, sourceSpec := newBatchPTYSource(ctx, t, dir)
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   sourceSpec,
		Orphan: sessionshim.OrphanPolicy{Deadline: 2 * time.Minute, TerminationGrace: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer termCancel()
		_ = shim.Terminate(termCtx)
	})
	direct, err := shim.Session().Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close() //nolint:errcheck // fixture subscription
	const exchanges = 8
	var source [][]byte
	for i := 0; i < exchanges; i++ {
		source = append(source, sourceControl.emit(ctx, t, direct.Frames(), byte('a'+i)))
	}
	if err := shim.Session().EmitMarker("barrier"); err != nil {
		t.Fatal(err)
	}

	// The first daemon adopts at generation 1 and walks away WITHOUT draining
	// the stream, exactly as a restarted service manager does: the harness
	// keeps running and its output stays buffered and unacked, so the
	// replacement's generation-2 adoption replays it. Draining here would
	// advance the shim-side durable cursor past the buffered output and the
	// replacement would resume past it — no batch, nothing to hold.
	first, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "first-controller", RequireFullHostFrames: true,
	})
	if err != nil || len(first.Adopted) != 1 {
		t.Fatalf("first adoption = %+v err=%v", first, err)
	}
	first.Close()

	var (
		mu          sync.Mutex
		snapshotSeq uint64
		batchBytes  [][]byte
		batchBefore atomic.Int32
		emitErr     atomic.Value
	)
	batchDone := make(chan struct{})
	var batchOnce sync.Once
	replacement := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: registryDir, HostID: "host-carrier-install", OrgID: id.OrgID,
		AdoptionBatchOrgIDs:          []string{id.OrgID},
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		Readoption:                 SessionShimReadoptionPolicy{Attempts: 1, Backoff: 5 * time.Millisecond},
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			// Resume from the start of the stream, not past it: the prior
			// generation never durably acknowledged anything, so the buffered
			// harness output is still unacked and the replacement must replay
			// it through the durable output-batch path. Resuming past it
			// would leave the consumer with nothing to hold and the test
			// would prove nothing.
			fromStart := uint64(1)
			return sessionshim.PreparedAdoption{
				ControllerGeneration: preparation.CurrentControllerGeneration + 1,
				Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "7"}},
				ResumeFrom:           &fromStart,
			}, nil
		},
		// The carrier takeover: Emit stages the mandatory Snapshot (arming the
		// carrier-installed latch) and waits for the consumer to durably cross
		// it. With the latch removed the consumer is parked on the output
		// batch ahead of the Snapshot, so this Emit times out and adoption
		// fails — the deadlock the first hold introduced.
		OnAdoption: func(adoptCtx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			if evidence.SnapshotProxy == nil {
				return SessionShimAdoptionReceipt{}, fmt.Errorf("adoption omitted mandatory Snapshot authority")
			}
			result, err := evidence.SnapshotProxy.Emit(adoptCtx)
			if err != nil {
				emitErr.Store(err)
				return SessionShimAdoptionReceipt{}, err
			}
			mu.Lock()
			snapshotSeq = result.AtSeq + 1
			mu.Unlock()
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("carrier-install-adoption")}, nil
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnSessionEventDurableBatch: func(_ context.Context, _ sessionshim.Identity, batch []sessionshim.ControllerEvent) (uint64, error) {
			for i, event := range batch {
				if i >= len(source) || !bytes.Equal(event.FrameBytes, source[i]) {
					batchBefore.Add(1)
					return 0, fmt.Errorf("batch frame %d bytes changed", i)
				}
			}
			mu.Lock()
			for _, event := range batch {
				batchBytes = append(batchBytes, event.FrameBytes)
			}
			mu.Unlock()
			batchOnce.Do(func() { close(batchDone) })
			return batch[len(batch)-1].Seq, nil
		},
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("carrier-install-batch"), AdoptionRevision: "carrier-install-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			receipts := make([]SessionShimCarrierActivationReceipt, 0, len(publication.Carriers))
			for _, carrier := range publication.Carriers {
				receipts = append(receipts, SessionShimCarrierActivationReceipt{
					Activation: carrier, AckSeq: snapshotSeq,
				})
			}
			return receipts, nil
		},
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {},
	}})
	enableHostedFullHostFramesForTest(t, replacement, id.OrgID)
	// Cleanup order matters: the consumer wait-group join runs first (LIFO),
	// so it joins consumers that ReleaseAdoptedSessionShims stops below.
	// Reversing them hangs the test on shutdown: Wait would block on a
	// consumer whose controller is never closed.
	t.Cleanup(func() { replacement.shims.wg.Wait() })
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(ctx); err != nil {
		t.Fatalf("replacement adoption: %v", err)
	}
	if stored := emitErr.Load(); stored != nil {
		t.Fatalf("carrier takeover Emit failed: %v", stored)
	}
	if _, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID); err != nil {
		t.Fatalf("lineage was not adopted: %v", err)
	}
	if quarantined := replacement.QuarantinedSessions(); len(quarantined) != 0 {
		t.Fatalf("quarantined = %+v, want none", quarantined)
	}
	select {
	case <-batchDone:
	case <-ctx.Done():
		t.Fatal("committed adoption never flushed the buffered output batch")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batchBytes) != len(source) {
		t.Fatalf("delivered batch frames = %d, want the %d buffered output frames", len(batchBytes), len(source))
	}
	for i, want := range source {
		if !bytes.Equal(batchBytes[i], want) {
			t.Fatalf("delivered batch frame %d bytes changed", i)
		}
	}
	if got := batchBefore.Load(); got != 0 {
		t.Fatalf("batch callback reported %d corrupted frames", got)
	}
}

// TestReadoptPathBufferedOutputFlowsThroughTakeoverEmit drives the
// re-adoption incident path itself: the event consumer starts before the
// adoption publication installs the durable carrier, with buffered harness
// output already in the stream.
//
// The consumer parks its first durable output batch until the carrier is
// installed OR the adoption resolves. The carrier takeover inside OnAdoption
// (Emit) stages the mandatory Snapshot, which arms the carrier-installed
// latch on the re-adoption gate; the latch releases the parked batch, the
// batch unblocks the Snapshot, and the Snapshot completes the adoption that
// resolves the gate. Without the re-adoption wiring the latch never arms:
// the consumer parks on the gate, Emit times out waiting for its Snapshot,
// and the adoption commit is never sent — the same user-visible outcome as
// the original race, through the deadlock the plain hold introduced.
//
// It fails with only the re-adoption wiring reverted (nil-gate evidence)
// while the startup-path test above stays green, which isolates this path's
// fix from the startup one.
func TestReadoptPathBufferedOutputFlowsThroughTakeoverEmit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A live harness with buffered output the re-adoption must replay. The
	// batch PTY source emits one exact byte per command behind a barrier, so
	// the stream the re-adoption dials holds real unacked output frames.
	dir, err := os.MkdirTemp("/tmp", "rrp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registryDir := filepath.Join(dir, "registry")
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-readopt-path", SessionID: "session-readopt-path"}
	sourceControl, sourceSpec := newBatchPTYSource(ctx, t, dir)
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   sourceSpec,
		Orphan: sessionshim.OrphanPolicy{Deadline: 2 * time.Minute, TerminationGrace: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer termCancel()
		_ = shim.Terminate(termCtx)
	})
	direct, err := shim.Session().Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close() //nolint:errcheck // fixture subscription
	const exchanges = 8
	var source [][]byte
	for i := 0; i < exchanges; i++ {
		source = append(source, sourceControl.emit(ctx, t, direct.Frames(), byte('a'+i)))
	}
	if err := shim.Session().EmitMarker("barrier"); err != nil {
		t.Fatal(err)
	}

	// The first generation adopts and walks away WITHOUT draining the
	// stream, exactly as a restarted service manager does: the harness keeps
	// running and its output stays buffered and unacked, so the generation-2
	// re-adoption replays it. Draining here would advance the shim-side
	// durable cursor past the buffered output and the re-adoption would
	// resume past it — no batch, nothing to hold.
	first, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "first-controller", RequireFullHostFrames: true,
	})
	if err != nil || len(first.Adopted) != 1 {
		t.Fatalf("first adoption = %+v err=%v", first, err)
	}
	firstCtrl := first.Adopted[0]
	firstGen := firstCtrl.Generation()
	lostHello := firstCtrl.Hello()

	var (
		mu          sync.Mutex
		snapshotSeq uint64
		batchBytes  [][]byte
		emitErr     atomic.Value
	)
	batchDone := make(chan struct{})
	var batchOnce sync.Once
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: registryDir, HostID: "host-readopt-path", OrgID: id.OrgID,
		AdoptionBatchOrgIDs:          []string{id.OrgID},
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		Readoption:                 SessionShimReadoptionPolicy{Attempts: 1, Backoff: 5 * time.Millisecond},
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			// Resume from the start of the stream, not past it: the prior
			// generation never durably acknowledged anything, so the buffered
			// harness output is still unacked and the re-adoption must replay
			// it through the durable output-batch path.
			fromStart := uint64(1)
			return sessionshim.PreparedAdoption{
				ControllerGeneration: preparation.CurrentControllerGeneration + 1,
				Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "7"}},
				ResumeFrom:           &fromStart,
			}, nil
		},
		// The carrier takeover: Emit stages the mandatory Snapshot (arming the
		// carrier-installed latch on the re-adoption gate) and waits for the
		// consumer to durably cross it. Without the re-adoption wiring the
		// consumer is parked on the output batch ahead of the Snapshot, so
		// this Emit times out and the adoption fails.
		OnAdoption: func(adoptCtx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			if evidence.SnapshotProxy == nil {
				return SessionShimAdoptionReceipt{}, fmt.Errorf("adoption omitted mandatory Snapshot authority")
			}
			result, err := evidence.SnapshotProxy.Emit(adoptCtx)
			if err != nil {
				emitErr.Store(err)
				return SessionShimAdoptionReceipt{}, err
			}
			mu.Lock()
			snapshotSeq = result.AtSeq + 1
			mu.Unlock()
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("readopt-path-adoption")}, nil
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnSessionEventDurableBatch: func(_ context.Context, _ sessionshim.Identity, batch []sessionshim.ControllerEvent) (uint64, error) {
			for i, event := range batch {
				if i >= len(source) || !bytes.Equal(event.FrameBytes, source[i]) {
					return 0, fmt.Errorf("batch frame %d bytes changed", i)
				}
			}
			mu.Lock()
			for _, event := range batch {
				batchBytes = append(batchBytes, event.FrameBytes)
			}
			mu.Unlock()
			batchOnce.Do(func() { close(batchDone) })
			return batch[len(batch)-1].Seq, nil
		},
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("readopt-path-batch"), AdoptionRevision: "readopt-path-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			mu.Lock()
			defer mu.Unlock()
			receipts := make([]SessionShimCarrierActivationReceipt, 0, len(publication.Carriers))
			for _, carrier := range publication.Carriers {
				receipts = append(receipts, SessionShimCarrierActivationReceipt{
					Activation: carrier, AckSeq: snapshotSeq,
				})
			}
			return receipts, nil
		},
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {},
	}})
	enableHostedFullHostFramesForTest(t, d, id.OrgID)
	// Cleanup order matters: the consumer wait-group join runs first (LIFO),
	// so it joins consumers that ReleaseAdoptedSessionShims stops below.
	// Reversing them hangs the test on shutdown: Wait would block on a
	// consumer whose controller is never closed.
	t.Cleanup(func() { d.shims.wg.Wait() })
	t.Cleanup(d.ReleaseAdoptedSessionShims)

	// The lost entry exactly as the disconnect path hands it to re-adoption:
	// the first controller, closed, still the adopted entry the receiver
	// holds the lineage at.
	lostEvidence, err := d.sessionShimAdoptionEvidence(ctx, firstCtrl, SessionShimAdoptionPreparationResult{}, "host-readopt-path")
	if err != nil {
		t.Fatalf("lost adoption evidence: %v", err)
	}
	lostEvidence.SnapshotProxy = nil
	_ = firstCtrl.Close()
	lost := adoptedShim{
		controller: firstCtrl, shimID: lostHello.ShimID,
		adoption: lostEvidence, adoptionReceipt: SessionShimAdoptionReceipt{DurableCorrelation: []byte("first")},
		carrierBound:           true,
		carrierBoundAtUnixNano: d.shimNow().UnixNano(),
	}
	d.shims.mu.Lock()
	d.shims.registry = registry
	d.shims.adopted[id] = lost
	d.shims.mu.Unlock()

	// The production entry point for the incident path: one bounded
	// re-adoption attempt for exactly this identity.
	if err := d.readoptSessionShimOnce(ctx, registry, d.sessionShimConfig(), id, lost, lostHello, d.shimNow().UnixNano()); err != nil {
		t.Fatalf("re-adoption with buffered output: %v", err)
	}
	if stored := emitErr.Load(); stored != nil {
		t.Fatalf("carrier takeover Emit failed: %v", stored)
	}
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("lineage was not re-adopted: %v", err)
	}
	if entry.controller == firstCtrl || entry.controller.Generation() <= firstGen {
		t.Fatalf("re-adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
	if quarantined := d.QuarantinedSessions(); len(quarantined) != 0 {
		t.Fatalf("quarantined = %+v, want none", quarantined)
	}
	select {
	case <-batchDone:
	case <-ctx.Done():
		t.Fatal("re-adopted lineage never flushed the buffered output batch")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batchBytes) != len(source) {
		t.Fatalf("delivered batch frames = %d, want the %d buffered output frames", len(batchBytes), len(source))
	}
	for i, want := range source {
		if !bytes.Equal(batchBytes[i], want) {
			t.Fatalf("delivered batch frame %d bytes changed", i)
		}
	}
}

// TestReadoptedOutputBatchRefusedGatePersistsNothing keeps the original hold's
// unit case: a refused adoption gate stops the parked consumer without
// persisting and without manufacturing a carrier loss.
func TestReadoptedOutputBatchRefusedGatePersistsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
		Orphan: sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second},
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
	for _, b := range []byte("abcdef") {
		sourceControl.emit(ctx, t, direct.Frames(), b)
	}
	if err := shim.Session().EmitMarker("barrier"); err != nil {
		t.Fatal(err)
	}
	result, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "controller-gate-first", RequireFullHostFrames: true,
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("adopt = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	ctrl := result.Adopted[0]

	d := &Daemon{shims: newSessionShimState()}
	t.Cleanup(func() { d.shims.wg.Wait() })
	d.opts.SessionShim = SessionShimConfig{
		CallbackTimeout: 5 * time.Second,
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error {
			return nil
		},
		OnSessionEventDurableBatch: func(context.Context, sessionshim.Identity, []sessionshim.ControllerEvent) (uint64, error) {
			t.Error("refused adoption persisted a batch; want the parked consumer to stop")
			return 0, fmt.Errorf("test batch persisted before the carrier was installed")
		},
	}
	d.shimIdentityRef.Store(&sessionShimIdentity{config: &d.opts.SessionShim})
	d.shims.mu.Lock()
	d.shims.registry = registry
	d.shims.mu.Unlock()

	// The incident's timed-out first dial: the consumer starts, the adoption
	// refuses, and the parked consumer must stop without persisting and
	// without touching the adopted entry.
	gate := newShimAdoptionGate()
	d.consumeShimEventsGated(ctrl, gate)
	gate.finish(false)
	_ = ctrl.Close()
	d.shims.wg.Wait()
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("refused first dial quarantined: %+v", projected)
	}
}
