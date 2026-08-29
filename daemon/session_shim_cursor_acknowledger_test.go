package daemon

// Provenance: terminal-refusal-does-not-drop-the-carrier-2026-08-28 — grep a
// build for this marker to prove the background acknowledger distinguishes a
// published terminal proof from a broken socket.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// refusingCursorController is the controller surface the background
// acknowledger drives, with the one answer under test and a record of whether
// the connection was dropped.
type refusingCursorController struct {
	refusal error

	mu       sync.Mutex
	beats    []uint64
	closes   int
	answered chan struct{}
	done     chan struct{}
}

func (c *refusingCursorController) SupportsFullHostFrames() bool { return true }

func (c *refusingCursorController) Heartbeat(seq uint64) error {
	c.mu.Lock()
	c.beats = append(c.beats, seq)
	first := len(c.beats) == 1
	c.mu.Unlock()
	if first {
		close(c.answered)
	}
	return c.refusal
}

func (c *refusingCursorController) Done() <-chan struct{} { return c.done }

func (c *refusingCursorController) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *refusingCursorController) dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// TestTerminalRefusalDoesNotDropTheShimConnection pins the ONE decision the
// off-path acknowledger makes that nothing downstream can see.
//
// A shim that answers `exited` is not a broken socket: its tombstone is on disk
// and it is still flushing the single Exit HostFrame that ends the session on
// this very connection. Measured on an installed host, dropping the connection
// on that answer took the Exit away from the consume loop — the lineage then
// left through releaseShimIfLive instead of finishAdoptedShim, so
// SessionEventEnded (the only thing that clears the per-session detail cache)
// never fired, and the fallback republished a whole adoption batch that flipped
// the host out of adoption_complete until its next beat.
//
// A genuine persistence failure is the opposite case and must still drop: the
// cursor may never claim a sequence the shim did not durably store.
func TestTerminalRefusalDoesNotDropTheShimConnection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		refusal     error
		wantDropped bool
	}{
		{
			name:    "a published terminal proof keeps the connection",
			refusal: fmt.Errorf("%w: exited", sessionshim.ErrShimExited),
		},
		{
			name:        "a persistence failure still drops the connection",
			refusal:     errors.New("carrier refused the cursor"),
			wantDropped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{shims: newSessionShimState()}
			ctrl := &refusingCursorController{
				refusal:  tc.refusal,
				answered: make(chan struct{}),
				done:     make(chan struct{}),
			}
			id := sessionshim.Identity{OrgID: "org-terminal-refusal", SessionID: "session-terminal-refusal"}
			acknowledger := d.startShimCursorAcknowledger(id, ctrl)
			t.Cleanup(acknowledger.stop)
			acknowledger.record(7)

			select {
			case <-ctrl.answered:
			case <-time.After(10 * time.Second):
				t.Fatal("the acknowledger never attempted the cursor acknowledgement")
			}
			// The loop returns after a refusal either way, so joining it is what
			// makes the drop assertion below a decision rather than a race.
			joined := make(chan struct{})
			go func() {
				d.shims.wg.Wait()
				close(joined)
			}()
			select {
			case <-joined:
			case <-time.After(10 * time.Second):
				t.Fatal("the acknowledger goroutine never returned after the refusal")
			}
			if got := ctrl.dropped(); (got > 0) != tc.wantDropped {
				t.Fatalf("controller dropped %d times, want dropped=%t — %s", got, tc.wantDropped,
					"a terminal refusal must leave the stream open for the Exit frame the shim is flushing")
			}
		})
	}
}

// TestTerminalRefusalMidStreamStillEndsThroughTheExit is the same distinction
// end to end, on a real shim and a real socket.
//
// The shim answers an acknowledgement it cannot honour — one BEYOND the
// sequence its terminal proof froze — with `exited`, and is still flushing the
// Exit HostFrame on that connection. The session must end through
// finishAdoptedShim: exactly one SessionEventEnded, and no quarantine. A
// consumer that reads the refusal as a dead stream reaches the same lineage
// through releaseShimIfLive instead, which reports no lifecycle end at all and
// republishes an adoption batch to undo itself.
func TestTerminalRefusalMidStreamStillEndsThroughTheExit(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.opts.SessionShim.HostID = "host-terminal-refusal"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(41)
	configureDynamicPublicationProbe(t, d, probe)

	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatal(err)
	}
	id := f.identity("sess-terminal-refusal")

	var mu sync.Mutex
	lifecycleEnds := 0
	var refusal, resumeAck error
	refused := false
	gateReached := make(chan struct{})
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind != SessionEventEnded {
			return
		}
		mu.Lock()
		lifecycleEnds++
		mu.Unlock()
	})
	d.opts.SessionShim.OnSessionEventDurable = func(evID sessionshim.Identity, event sessionshim.ControllerEvent) error {
		mu.Lock()
		alreadyRefused := refused
		mu.Unlock()
		if alreadyRefused || event.Kind != sessionshim.EventHostFrame ||
			!strings.Contains(string(event.Data), "ack:gate") {
			return nil
		}
		mu.Lock()
		refused = true
		mu.Unlock()
		// Hold the consume loop HERE, before the Exit is dequeued, and wait for
		// the shim's own terminal proof to land. Everything after this point is
		// a genuinely post-terminal exchange on a live stream.
		close(gateReached)
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, tombErr := registry.GetTombstone(evID); tombErr == nil {
				break
			}
			if !time.Now().Before(deadline) {
				mu.Lock()
				refusal = errors.New("the shim never published its terminal proof")
				mu.Unlock()
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		entry, entryErr := d.adoptedShimEntry(evID.OrgID, evID.SessionID)
		if entryErr != nil || entry.controller == nil {
			mu.Lock()
			refusal = fmt.Errorf("adopted entry: %w", entryErr)
			mu.Unlock()
			return nil
		}
		// A sequence the reaped harness can never have allocated: the one
		// acknowledgement a published terminal proof still refuses.
		err := entry.controller.Heartbeat(1 << 40)
		// And immediately afterwards, an ordinary one. It can only be answered
		// on a stream the refusal did NOT drop, which is the whole distinction:
		// the shim is still holding this connection open to flush its Exit.
		resume := entry.controller.Heartbeat(event.Seq)
		mu.Lock()
		refusal, resumeAck = err, resume
		mu.Unlock()
		return nil
	}

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-terminal-refusal")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitFor(t, 30*time.Second, "the launched session to be adopted", func() bool {
		_, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
		return err == nil
	})
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if !entry.controller.SupportsFullHostFrames() {
		t.Fatal("the session is not on the selected-v3 rail; the refusal this test is about does not exist")
	}
	if err := d.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("gate\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}
	select {
	case <-gateReached:
	case <-time.After(30 * time.Second):
		t.Fatal("the consume loop never reached the gate frame")
	}
	if !d.StopSession(id.SessionID) {
		t.Fatal("StopSession did not route to the adopted shim")
	}

	waitFor(t, 60*time.Second, "the adopted session to reach a terminal outcome", func() bool {
		return d.SessionShimOccupancy() == 0
	})
	mu.Lock()
	sawRefusal, sawResume := refusal, resumeAck
	mu.Unlock()
	if !errors.Is(sawRefusal, sessionshim.ErrShimExited) {
		t.Fatalf("post-terminal acknowledgement returned %v, want ErrShimExited — this test proves nothing "+
			"unless the shim really refused on a live stream", sawRefusal)
	}
	if sawResume != nil {
		t.Fatalf("the acknowledgement after the refusal returned %v — the refusal dropped the connection the "+
			"shim was still flushing its Exit on", sawResume)
	}
	if q := d.QuarantinedSessions(); len(q) != 0 {
		t.Fatalf("a refusal that carried a published terminal proof quarantined the lineage anyway: %+v", q)
	}
	waitFor(t, 30*time.Second, "the lifecycle end to be emitted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lifecycleEnds == 1
	})
	mu.Lock()
	ends := lifecycleEnds
	mu.Unlock()
	if ends != 1 {
		t.Fatalf("lifecycle end emitted %d times, want exactly one — a terminal refusal must not divert the "+
			"lineage out of the Exit path", ends)
	}
}
