package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestStartupReadoptedSessionSurvivesAHostFrameBurst pins the control channel of
// a session this daemon adopted at startup against the shim outrunning it.
//
// The failure it reproduces is a real one from an installed-service restart. The
// replacement daemon re-adopts a live session at generation N+1, publishes it,
// and then — minutes later, on its first ordinary write — every call to that
// shim fails with "use of closed network connection". Nothing had gone wrong on
// the shim's side: the harness was alive and heartbeating. The daemon had closed
// its OWN socket and gone on believing it still owned the session.
//
// The mechanism is throughput, not lifecycle. Selected-v3 acknowledges each
// forwarded HostFrame through an fsync-backed round trip to the shim, so a
// consumer that acknowledges inline runs at roughly the cost of one fsync per
// frame. The controller's priority event queue is deliberately bounded and
// fail-closed, so a consumer that falls behind does not slow the stream down —
// the socket reader drops the connection. One dense terminal redraw is enough,
// and a session adopted at startup is attached to a harness that is ALREADY
// producing, unlike one this daemon just launched.
//
// Reverting the off-path acknowledger in consumeShimEventsGated (acknowledging
// inline with d.recordShimForwardedSeqForController again) turns this RED at the
// burst with exactly the field error.
func TestStartupReadoptedSessionSurvivesAHostFrameBurst(t *testing.T) {
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
		observed     strings.Builder
		carrierEpoch = uint64(70)
	)
	replacement := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
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
		OnSessionEvent: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) {
			mu.Lock()
			observed.Write(event.Data)
			mu.Unlock()
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
	before := replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID)

	// The burst: dense real geometry and attributed input through the shim-owned
	// PTY, which is what a terminal application's redraw looks like on the wire.
	if err := replacement.forceSessionShimAcceptanceGap(id); err != nil {
		t.Fatalf("host-frame burst through the re-adopted controller: %v", err)
	}

	// The burst deliberately floods the shim-owned ring, so the harness answers
	// the accumulated redraw bytes and this line together. Matching the token
	// rather than a line prefix keeps the assertion about liveness, not layout.
	// The control channel must still be the daemon's, and still be usable.
	if adopted := replacement.AdoptedSessionShims(); len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("adopted sessions after the burst = %+v, want exactly [%s]", adopted, id)
	}
	if err := replacement.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("after-burst\r")); err != nil {
		t.Fatalf("write to the re-adopted session after the burst: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := observed.String()
		mu.Unlock()
		if strings.Contains(seen, "after-burst") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	seen := observed.String()
	mu.Unlock()
	if !strings.Contains(seen, "after-burst") {
		t.Fatalf("the re-adopted session never answered after the burst; carrier saw %d bytes", len(seen))
	}
	// Coalescing must not stop the cursor: the resume point still has to advance.
	waitFor(t, 20*time.Second, "the durable forwarded cursor to advance past the burst", func() bool {
		return replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID) > before
	})
}

// TestConsumerDropReleasesShimOwnershipInsteadOfStrandingIt pins the other half
// of the same field report: whatever drops an adopted controller, the daemon
// must not keep publishing that session as adopted against a socket it can no
// longer write to.
//
// A durable carrier that refuses a frame is the reachable case. Before the fix
// the consumer closed the connection and returned, leaving the entry in the
// adopted map forever: `host status` showed a running session, capacity stayed
// charged as adopted rather than quarantined, and every input/resize came back
// with "use of closed network connection" instead of an honest refusal.
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

	waitFor(t, 20*time.Second, "the refused frame to release shim ownership", func() bool {
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
