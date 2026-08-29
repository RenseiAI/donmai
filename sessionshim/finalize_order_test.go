package sessionshim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
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
	// The boundary itself, not a stopwatch: the courtesy section announces
	// itself, and the proof must already be on disk when it does. It is
	// installed through Options because Start launches watchHarness before it
	// returns — assigning it on the returned Shim is a cross-goroutine write
	// with no happens-before edge.
	courtesy := make(chan bool, 1)
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V3,
		onTerminalCourtesy: func() {
			// By identity and epoch, not by shim id: the shim id is only known
			// after Start returns, and this callback is installed before it.
			tombstones, scanErr := registry.ScanTombstones()
			if scanErr != nil {
				courtesy <- false
				return
			}
			for _, tombstone := range tombstones {
				if tombstone.Identity() == id && tombstone.ProcessEpoch == 1 {
					courtesy <- true
					return
				}
			}
			courtesy <- false
		},
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

// TestHeartbeatRefusalCarriesTheExitedSentinel pins the one distinction the
// daemon acts on.
//
// A finalized shim answers a late cursor acknowledgement with `exited` —
// "heartbeat rejected: terminal proof is published" (shim.go). That is a FACT
// about the lifecycle, not a broken socket, and the caller has to be able to
// tell them apart: measured on an installed host, reading it as a transport
// failure published a quarantine for a lineage whose tombstone was already on
// disk, which cost an adoption revision, a heartbeat 409, commit-outcome
// reconciliation and a second publication to undo.
func TestHeartbeatRefusalCarriesTheExitedSentinel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code shimwire.ErrorCode
		want bool
	}{
		{name: "terminal proof is published", code: shimwire.CodeExited, want: true},
		{name: "an ordinary refusal is not terminal", code: shimwire.CodeInternal},
		{name: "an unauthenticated refusal is not terminal", code: shimwire.CodeUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := shimwire.EncodeError(shimwire.ErrorMsg{Code: tc.code, Detail: "refused"})
			if err != nil {
				t.Fatal(err)
			}
			c := &Controller{}
			call := &heartbeatCall{done: make(chan heartbeatResult, 1)}
			c.heartbeatCall = call
			refused, terminal := c.failHeartbeatFromError(body)
			if !refused {
				t.Fatal("the refusal did not reach the pending acknowledgement")
			}
			if terminal != tc.want {
				t.Fatalf("terminal refusal = %t, want %t — only a terminal refusal may keep the stream open "+
					"for the Exit frame the shim is still flushing", terminal, tc.want)
			}
			result := <-call.done
			if result.err == nil {
				t.Fatal("a refused heartbeat reported success")
			}
			if got := errors.Is(result.err, ErrShimExited); got != tc.want {
				t.Fatalf("errors.Is(%v, ErrShimExited) = %t, want %t — the daemon distinguishes "+
					"\"terminal proof is published\" from a broken socket on exactly this", result.err, got, tc.want)
			}
		})
	}
}

// TestTerminalCursorAcknowledgementSurvivesTheTombstone is the other half of
// the ordering above, and the one that decides whether the ordering is free.
//
// Publishing the tombstone BEFORE the courtesy waits means the terminal proof
// is already published when the controller answers the Exit — and a shim that
// refuses every acknowledgement from that instant refuses the exact receipt its
// own durable-ack courtesy is waiting for. Measured on an installed host: every
// terminal exit burned the whole flush bound, and the daemon read the refusal as
// a reason to drop a live connection.
//
// The rail is FROZEN by the proof, not closed by it: an acknowledgement at or
// below the sequence the tombstone recorded is the cursor an adopting
// controller resumes from and is honoured; only a claim beyond it — a sequence
// that can never exist, because the harness is reaped — is refused `exited`.
func TestTerminalCursorAcknowledgementSurvivesTheTombstone(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-terminal-ack", SessionID: "session-terminal-ack"}
	// One courtesy window. The elapsed assertion below is against HALF of it,
	// so the window has to be wide enough that half of it still comfortably
	// covers a spawn, a handshake and a Terminate on a loaded parallel -race
	// run — while the whole of it is what the pre-fix code burned every time.
	const grace = 4 * time.Second
	// The acknowledgement is released by the tombstone, not by a sleep: the
	// courtesy boundary fires immediately after the proof is durable, so an
	// acknowledgement gated on it is provably a POST-terminal one. It is
	// installed through Options because Start launches watchHarness before it
	// returns — assigning it on the returned Shim is a cross-goroutine write
	// with no happens-before edge.
	tombstoned := make(chan struct{})
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V3,
		Spec:               ptyhost.Spec{Command: []string{"/bin/sh", "-c", "while :; do sleep 0.05; done"}},
		Orphan:             OrphanPolicy{Deadline: 90 * time.Second, TerminationGrace: grace, PropagationMargin: 30 * time.Second},
		onTerminalCourtesy: func() { close(tombstoned) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shim.Close() })

	result, err := Adopt(context.Background(), AdoptOptions{
		Registry: registry, ControllerID: "controller-terminal-ack", RequireFullHostFrames: true,
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("adoption = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	controller := result.Adopted[0]

	type ackOutcome struct {
		exitSeq uint64
		beyond  error
		exact   error
	}
	acknowledged := make(chan ackOutcome, 1)
	go func() {
		for event := range controller.Events() {
			if event.Kind != EventHostFrame || event.FrameType != attachwire.TypeExit {
				continue
			}
			<-tombstoned
			out := ackOutcome{exitSeq: event.Seq}
			// Refusal FIRST, on purpose: a terminal refusal that dropped the
			// stream would take the acknowledgement below down with it, which
			// is exactly the connection loss the read loop must no longer cause.
			out.beyond = controller.Heartbeat(event.Seq + 1)
			out.exact = controller.Heartbeat(event.Seq)
			acknowledged <- out
			return
		}
	}()

	terminated := make(chan error, 1)
	started := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		terminated <- shim.Terminate(ctx)
	}()

	var out ackOutcome
	select {
	case out = <-acknowledged:
	case <-time.After(30 * time.Second):
		t.Fatal("the controller never saw the terminal frame it had to acknowledge")
	}
	select {
	case err := <-terminated:
		if err != nil {
			t.Fatalf("Terminate: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Terminate never returned")
	}
	elapsed := time.Since(started)

	if out.exact != nil {
		t.Fatalf("the acknowledgement of the terminal sequence was refused: %v — a controller resuming from "+
			"the exact terminal cursor is what the durable-ack courtesy exists to allow", out.exact)
	}
	if !errors.Is(out.beyond, ErrShimExited) {
		t.Fatalf("an acknowledgement BEYOND the terminal sequence returned %v, want ErrShimExited — no such "+
			"sequence can exist once the harness is reaped", out.beyond)
	}
	shim.recordMu.Lock()
	acked, frozen := shim.ackedSeq, shim.terminalSeq
	shim.recordMu.Unlock()
	if acked != out.exitSeq {
		t.Fatalf("shim stored acknowledgement = %d, want the terminal sequence %d", acked, out.exitSeq)
	}
	if frozen != out.exitSeq {
		t.Fatalf("terminal proof froze sequence %d, want the Exit frame's own %d", frozen, out.exitSeq)
	}
	// Half the injected bound, not a picked number. The pre-fix shim refused
	// the very acknowledgement its durable-ack courtesy was waiting for and
	// therefore burned the WHOLE window on every terminal exit; anything under
	// half of it cannot be that, and the other half is slack for the spawn,
	// handshake and Terminate this span also contains.
	if budget := grace / 2; elapsed >= budget {
		t.Fatalf("finalization took %s, over half the %s courtesy window (budget %s) — the shim refused the "+
			"acknowledgement it was waiting for, so every terminal exit pays the full bound", elapsed, grace, budget)
	}
}
