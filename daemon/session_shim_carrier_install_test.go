package daemon

// The durable output-batch hold during adoption, pinned through the real
// pipeline.
//
// A replacement daemon adopts a live shim whose harness already produced
// output, so its event consumer meets buffered output frames before the
// composing takeover has dialled and installed its carrier. Persisting a batch
// at once met "no carrier candidate" and was classified as carrier loss: the
// consumer closed the controller while the adoption commit was in flight and
// the lineage never re-adopted. The fix holds output batches until the carrier
// install the takeover stages — or until the adoption gate itself resolves
// when no takeover stages one — whichever comes first.
//
// A hold on the adoption gate alone deadlocks this same pipeline: the takeover
// Snapshot the adoption waits on is queued behind the held batch, so the
// adoption never commits and the gate never resolves. This test drives the real
// startup composition — a first dial refused by a draining carrier, then an
// admitted dial whose takeover emits the mandatory Snapshot — with output
// buffered ahead of both, so either defect (false carrier loss, or the
// deadlock) fails it.

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

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
)

// carrierInstallAdoption is a composing authority whose durable-adoption
// callback dials a draining carrier on every attempt: the first dial meets the
// drain and is refused, the second is admitted and runs the takeover.
type carrierInstallAdoption struct {
	relay *drainingRelay

	mu            sync.Mutex
	dials         int
	snapshotSeqs  map[sessionshim.Identity]uint64
	emitDone      map[sessionshim.Identity]chan struct{}
	batchCalls    atomic.Int32
	batchBytes    [][]byte
	batchDuringIn bool
	adoptReturned chan struct{}
}

func (a *carrierInstallAdoption) prepare(_ context.Context, in SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
	return sessionshim.PreparedAdoption{
		ControllerGeneration: in.CurrentControllerGeneration + 1,
		Correlation:          []byte("carrier-install-1"),
		// Replay from the start of the stream: the replacement daemon holds no
		// durable cursor, so every exchange the harness produced is unacked.
		ResumeFrom: proofResolvedResumeFloor(),
	}, nil
}

func proofResolvedResumeFloor() *uint64 {
	var floor uint64 = 1
	return &floor
}

func (a *carrierInstallAdoption) adopt(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
	a.mu.Lock()
	a.dials++
	dial := a.dials
	a.mu.Unlock()
	// The carrier dial. While the carrier drains, nothing was learned about
	// the lineage: the refusal keeps its production type so the composition
	// redials rather than quarantines.
	if err := a.relay.dialPath(""); err != nil {
		return SessionShimAdoptionReceipt{}, err
	}
	if dial != 2 {
		return SessionShimAdoptionReceipt{}, fmt.Errorf("carrier admitted dial %d, want the drain to refuse exactly the first", dial)
	}
	// The admitted dial runs the takeover: the mandatory Snapshot, whose
	// staging installs the carrier the held output batch waits on.
	result, err := evidence.SnapshotProxy.Emit(ctx)
	if err != nil {
		return SessionShimAdoptionReceipt{}, err
	}
	a.mu.Lock()
	if a.snapshotSeqs == nil {
		a.snapshotSeqs = make(map[sessionshim.Identity]uint64)
	}
	a.snapshotSeqs[evidence.Identity] = result.AtSeq + 1
	if ch, ok := a.emitDone[evidence.Identity]; ok {
		close(ch)
	}
	a.mu.Unlock()
	return SessionShimAdoptionReceipt{DurableCorrelation: []byte("carrier-install-adopted")}, nil
}

func (a *carrierInstallAdoption) dialsSoFar() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dials
}

// TestReadoptedOutputBatchPersistsAfterTheCarrierInstalls drives the real
// startup composition with output buffered ahead of the takeover.
//
// The harness produces eight exchanges, the first controller walks away
// without acknowledging them, and the replacement adopts with a carrier that
// refuses the first dial and admits the second. The admitted dial's takeover
// emits the mandatory Snapshot while the replayed output is already held in
// the consumer: the batch must persist after the carrier installs (never as a
// carrier loss on the first dial), the takeover Snapshot must still reach the
// consumer through the held batch (never a deadlock), and the adoption batch
// must commit the lineage adopted with no quarantine anywhere.
func TestReadoptedOutputBatchPersistsAfterTheCarrierInstalls(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// A Unix socket path has a short platform limit; keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "cib")
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
	echoSpec, err := daemonShimEchoSpec()
	if err != nil {
		t.Fatal(err)
	}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 5,
		Spec:         echoSpec,
		WorkareaPath: filepath.Join(dir, "workarea"),
		Orphan:       sessionshim.OrphanPolicy{Deadline: time.Minute, TerminationGrace: time.Second, PropagationMargin: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer termCancel()
		_ = shim.Terminate(termCtx)
	})

	// The first controller drains the stream, then produces eight exchanges it
	// never acknowledges: it reads each echo to prove the harness produced it,
	// but sends no heartbeat, so the replacement replays all eight.
	first, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "controller-carrier-install-first",
		RequireFullHostFrames: true,
	})
	if err != nil || len(first.Adopted) != 1 {
		t.Fatalf("first adoption = %+v, %v", first, err)
	}
	firstCtrl := first.Adopted[0]
	const exchanges = 8
	var want [][]byte
	for i := 0; i < exchanges; i++ {
		token := fmt.Sprintf("install-%d", i)
		if err := firstCtrl.WriteInput([]byte(token + "\r")); err != nil {
			t.Fatalf("write exchange %d: %v", i, err)
		}
		want = append(want, waitForEcho(t, ctx, firstCtrl, "ack:"+token))
	}
	first.Close()

	// The replacement: a draining carrier that refuses exactly the first dial,
	// then admits the second and runs the Snapshot takeover on it.
	adoption := &carrierInstallAdoption{
		relay:    newDrainingRelay(t, 1, 1),
		emitDone: map[sessionshim.Identity]chan struct{}{id: make(chan struct{})},
	}
	var (
		batchMu    sync.Mutex
		batches    []SessionShimAdoptionBatch
		publicated []SessionShimAdoptionPublication
	)
	replacement := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: registryDir, HostID: "host-carrier-install", OrgID: id.OrgID,
		AdoptionBatchOrgIDs:          []string{id.OrgID},
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		// One drain window to spend on the refused first dial; the admitted
		// second dial must land inside it.
		StartupRelayDrainWindow: relayRestartFixtureWindow,
		Readoption:              SessionShimReadoptionPolicy{Attempts: 1, Backoff: 5 * time.Millisecond},
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoption:            adoption.prepare,
		OnAdoption:                 adoption.adopt,
		OnSessionEventDurable:      func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnSessionEventDurableBatch: func(_ context.Context, _ sessionshim.Identity, batch []sessionshim.ControllerEvent) (uint64, error) {
			n := adoption.batchCalls.Add(1)
			if n == 1 {
				// The first batch must persist only after the carrier
				// installed: the takeover Emit resolves before the adoption
				// commits, so a batch persisted before Emit returned met no
				// carrier candidate. Failing here keeps the defect visible as
				// an assertion rather than as a quarantined lineage.
				select {
				case <-adoption.emitDone[id]:
				default:
					t.Errorf("the first output batch persisted before the takeover installed the carrier")
				}
			}
			batchMu.Lock()
			for _, event := range batch {
				adoption.batchBytes = append(adoption.batchBytes, append([]byte(nil), event.FrameBytes...))
			}
			batchMu.Unlock()
			return batch[len(batch)-1].Seq, nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			batchMu.Lock()
			batches = append(batches, cloneSessionShimAdoptionBatch(batch))
			batchMu.Unlock()
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("carrier-install-batch"), AdoptionRevision: "carrier-install-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			batchMu.Lock()
			publicated = append(publicated, publication)
			batchMu.Unlock()
			adoption.mu.Lock()
			defer adoption.mu.Unlock()
			receipts := make([]SessionShimCarrierActivationReceipt, 0, len(publication.Carriers))
			for _, carrier := range publication.Carriers {
				carrierID := sessionshim.Identity{OrgID: carrier.OrgID, SessionID: carrier.SessionID}
				receipts = append(receipts, SessionShimCarrierActivationReceipt{
					Activation: carrier, AckSeq: adoption.snapshotSeqs[carrierID],
				})
			}
			return receipts, nil
		},
		OnCarrierActivationAcknowledged: func(SessionShimPublishedBatchReceipt) {},
	}})
	enableHostedFullHostFramesForTest(t, replacement, id.OrgID)
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)

	if err := replacement.adoptSessionShims(ctx); err != nil {
		t.Fatalf("startup composition: %v", err)
	}

	// The incident path: dial 1 refused by the drain, dial 2 admitted.
	if got := adoption.dialsSoFar(); got != 2 {
		t.Fatalf("durable adoption dials = %d, want 2 (one drain refusal, then the admitted dial)", got)
	}
	// The takeover ran on the admitted dial.
	select {
	case <-adoption.emitDone[id]:
	default:
		t.Fatal("the admitted dial never ran the Snapshot takeover")
	}
	// No carrier loss anywhere: the lineage is adopted at the next generation
	// and nothing is quarantined.
	entry, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("the lineage was not adopted: %v (quarantined: %+v)", err, replacement.QuarantinedSessions())
	}
	if entry.controller.Generation() != 2 {
		t.Fatalf("adopted generation = %d, want 2", entry.controller.Generation())
	}
	if projected := replacement.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("quarantine projection = %+v, want none", projected)
	}
	// The buffered output flushed through the batch path exactly once per
	// exchange, in order.
	if got := adoption.batchCalls.Load(); got == 0 {
		t.Fatal("no output batch was ever persisted")
	}
	batchMu.Lock()
	defer batchMu.Unlock()
	if len(adoption.batchBytes) != len(want) {
		t.Fatalf("persisted batch frames = %d, want the %d buffered exchanges", len(adoption.batchBytes), len(want))
	}
	for i := range want {
		if string(adoption.batchBytes[i]) != string(want[i]) {
			t.Fatalf("persisted batch frame %d = %q, want %q", i, adoption.batchBytes[i], want[i])
		}
	}
	// The platform commit went out carrying the lineage adopted.
	if len(batches) != 1 || len(batches[0].Adopted) != 1 || batches[0].Adopted[0].Evidence.Identity != id {
		t.Fatalf("committed adoption batches = %+v, want exactly one carrying the lineage adopted", batches)
	}
	if len(publicated) == 0 {
		t.Fatal("no adoption publication ran")
	}
	var sawTakeover bool
	for _, frame := range adoption.batchBytes {
		if strings.Contains(string(frame), "install-") {
			sawTakeover = true
		}
	}
	if !sawTakeover {
		t.Fatal("the persisted batches carry none of the buffered harness output")
	}
}

// waitForEcho reads the first controller's stream until the harness answers
// the written line, returning the exact output frame bytes the replacement
// must later replay and persist.
func waitForEcho(t *testing.T, ctx context.Context, ctrl *sessionshim.Controller, token string) []byte {
	t.Helper()
	var buf strings.Builder
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatalf("harness did not answer %q: %v", token, ctx.Err())
		case ev, ok := <-ctrl.Events():
			if !ok {
				t.Fatalf("first controller stream ended before the harness answered %q", token)
			}
			if ev.Kind == sessionshim.EventHostFrame && ev.FrameType == attachwire.TypeOutput {
				buf.Write(ev.Data)
				if strings.Contains(buf.String(), token) {
					return append([]byte(nil), ev.FrameBytes...)
				}
			}
		}
	}
	t.Fatalf("harness did not answer %q within the bound (saw %q)", token, buf.String())
	return nil
}
