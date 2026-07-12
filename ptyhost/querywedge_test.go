package ptyhost

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// TestQueryRepliesWedgeNonReadingChild is the T10 lane's documented reproducer
// for a REAL robustness bug found while building the cross-package invariant
// test (crosswire_test.go). It is skipped by default so the suite stays green;
// remove the skip once the bug is fixed and flip the final assertion.
//
// THE BUG (report, not redesign — T10 lane boundary):
//
// A PTY child that emits terminal queries (DA1 `CSI c`, DA2, OSC 10/11 color
// queries, CPR `CSI 6n`, ...) but never reads its stdin wedges the ENTIRE
// session. Mechanism, confirmed from a live goroutine dump:
//
//	Session.run (the read loop)
//	  -> Session.onOutput            [ACQUIRES s.mu, session.go]
//	  -> vtHost.write -> Emulator.Write
//	  -> registerResponders' DA1 handler (vt.go)
//	  -> io.WriteString(v.resp)      [v.resp == the PTY master, vt.go]
//	  -> syscall.Write BLOCKS: the kernel slave input queue (~1 KiB on
//	     macOS) is full because the child never reads stdin.
//
// The read loop is now parked in a blocking master write while HOLDING s.mu,
// so every other Session surface deadlocks behind it: WriteInput is unaffected
// but Resize, Snapshot, EmitSnapshot, EmitMarker, Subscribe, and all frame
// emission stall; the child also stalls once the slave OUTPUT buffer fills
// (nobody is reading the master anymore). The session emits nothing further —
// a permanent live-wedge driven entirely by hostile-or-buggy child behavior
// (any TUI that probes its terminal and then stops reading stdin, or a child
// that inherits the PTY but ignores stdin, triggers it; ~14 loops of the
// tmux_vim fixture = ~56 query replies = ~1 KiB is enough).
//
// Stop() DOES recover the session (SIGTERM/SIGKILL to the process group kills
// the child; slave-side closure unblocks the master write; teardown proceeds),
// so this is a liveness bug for a RUNNING session, not a resource leak.
//
// Suggested direction for the fix owner (NOT applied here): the VT's query
// replies must never do a blocking write to the master while the session
// mutex is held — e.g. write replies via a bounded, drop-oldest buffer
// flushed by a separate goroutine (mirroring how subscription fan-out already
// decouples slow consumers), or a non-blocking write that drops the reply at
// EAGAIN (a real terminal whose input queue is full drops replies too).
func TestQueryRepliesWedgeNonReadingChild(t *testing.T) {
	// Fixed by ptyhost/replywriter.go (bounded async reply queue,
	// drop-when-child-not-reading): the session must now survive the full
	// query storm and exit cleanly.
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}

	// A child that emits DA1 queries forever and never reads stdin. Each
	// query makes the host VT write a ~13-byte reply to the master; after
	// ~80 replies (~1 KiB) the slave input queue is full and the session
	// wedges.
	queries := 200
	script := "i=0; while [ $i -lt " + strconv.Itoa(queries) + " ]; do printf '\\033[c padding-padding-padding\\n'; i=$((i+1)); done; echo DONE-ALL-QUERIES"
	s, err := Spawn(Spec{Command: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	// A healthy session delivers the DONE marker and the Exit frame well
	// within the deadline. The wedged session stops emitting mid-stream.
	deadline := time.After(20 * time.Second)
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				t.Fatal("subscription closed without an Exit frame")
			}
			if f.Type == attachwire.TypeExit {
				return // fixed: the full query storm drained and the session exited cleanly
			}
		case <-deadline:
			t.Fatal("session wedged: no Exit within deadline (read loop blocked writing a query reply to a full slave input queue while holding s.mu)")
		}
	}
}
