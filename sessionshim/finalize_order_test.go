package sessionshim

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestTombstoneIsPublishedBeforeTheCourtesyWaits pins the ordering that decides
// whether a reaped harness leaves proof at all.
//
// finalizeTerminal spends up to two full courtesy windows delivering the Exit
// to a controller that may already be gone — a pump flush and a durable-ack
// wait. Measured on an installed host: the host's own grace expired in the same
// instant the tombstone was about to be written, the process exited, and a
// provably-reaped harness left NO proof and no live shim to run the orphan
// clock. Nothing in those waits changes a field of the observation, so the
// proof goes first and the courtesy second.
func TestTombstoneIsPublishedBeforeTheCourtesyWaits(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-finalize-order", SessionID: "session-finalize-order"}
	// One courtesy window, long enough that "before" and "after" are not a
	// scheduling accident.
	const grace = 2 * time.Second
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V3,
		// Real output: the durable-ack wait is skipped for a session that never
		// allocated a sequence, and a courtesy window that returns instantly
		// would make this test prove nothing.
		// Output that keeps arriving: the durable-ack window only blocks while
		// the last allocated sequence is ahead of anything acknowledged, and a
		// courtesy window that returns instantly would prove nothing.
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "while :; do echo tick; sleep 0.05; done"}},
		Orphan: OrphanPolicy{Deadline: 90 * time.Second, TerminationGrace: grace, PropagationMargin: 30 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shim.Close() })

	// A real adopted controller that never acknowledges: the durable-ack wait
	// then spends its whole window, which is exactly the stalled-controller
	// case the ordering has to survive.
	result, err := Adopt(context.Background(), AdoptOptions{Registry: registry, ControllerID: "controller-finalize-order"})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("adoption = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	// Nobody drains this controller and nobody ever acknowledges: both courtesy
	// windows spend their full bound.
	// Enough unread output that the shim's own pump is blocked on socket
	// backpressure: the flush window then spends its full bound.
	waitForShimSequence(t, shim, 1, 10*time.Second)
	if want := shim.FinalizeBound(); want != 2*grace {
		t.Fatalf("FinalizeBound() = %s, want the two courtesy windows (%s)", want, 2*grace)
	}

	// The boundary itself, not a stopwatch: the courtesy section announces
	// itself, and the proof must already be on disk when it does.
	courtesy := make(chan bool, 1)
	shim.onTerminalCourtesy = func() {
		_, tombErr := registry.GetTombstoneIncarnation(id, shim.ShimID(), 1)
		courtesy <- tombErr == nil
	}

	terminated := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		terminated <- shim.Terminate(ctx)
	}()

	select {
	case published := <-courtesy:
		if !published {
			t.Fatal("the courtesy delivery started before the tombstone was durable — a host that stops " +
				"waiting mid-courtesy then takes the only proof of a reaped harness to the grave")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("finalization never reached its courtesy section")
	}
	select {
	case err := <-terminated:
		if err != nil {
			t.Fatalf("Terminate: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Terminate never returned")
	}
}

// waitForShimSequence blocks until the shim has allocated at least want host
// output sequences, so the durable-ack courtesy window has something to wait for.
func waitForShimSequence(t *testing.T, shim *Shim, want uint64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, seq, _ := shim.sess.Snapshot(); uint64(seq) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the shim never allocated %d host output sequences", want)
}
