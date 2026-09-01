package daemon

// Provenance: shim-adoption-reconvergence-2026-09-01 — grep a build for this
// marker to prove a slow durable side can no longer unsupervise a live harness.
//
// THE STRAND THESE TESTS UNDO
//
// A durable write that was merely SLOW pushed the selected-v3 persistence
// receipt past its wait bound. The controller dropped the shim connection over
// it, the background cursor acknowledger dropped it again on the way out, and
// both shims — healthy, with live harnesses doing real work — reaped
// themselves when their orphan deadlines expired. Two working sessions, lost
// to a write that was late.
//
// Two decisions had to change together, because either one alone still kills
// the session: the controller must report the receipt PENDING instead of
// closing (pinned in sessionshim), and this acknowledger must treat that answer
// as a reason to wait rather than a reason to drop. The orphan deadline is the
// third: it is what turns "lost the controller for a minute" into "reaped the
// harness", and ninety seconds was shorter than any control-plane recovery.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// pendingCursorController answers the first pendingAnswers acknowledgements
// with the pending sentinel and succeeds after that, recording every beat and
// every connection drop.
type pendingCursorController struct {
	pendingAnswers int

	mu       sync.Mutex
	beats    []uint64
	closes   int
	settled  chan struct{}
	settleOn sync.Once
	done     chan struct{}
}

func (c *pendingCursorController) SupportsFullHostFrames() bool { return true }

func (c *pendingCursorController) Heartbeat(seq uint64) error {
	c.mu.Lock()
	c.beats = append(c.beats, seq)
	pending := len(c.beats) <= c.pendingAnswers
	c.mu.Unlock()
	if pending {
		return fmt.Errorf("%w: acked sequence %d", sessionshim.ErrHeartbeatReceiptPending, seq)
	}
	c.settleOn.Do(func() { close(c.settled) })
	return nil
}

func (c *pendingCursorController) Done() <-chan struct{} { return c.done }

func (c *pendingCursorController) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *pendingCursorController) dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *pendingCursorController) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.beats)
}

// TestPendingPersistenceReceiptKeepsTheShimConnectionAndRetries pins the
// acknowledger half. A pending receipt must leave the connection up and come
// back to the same sequence after a bounded backoff; the cursor must not
// advance until the shim actually confirms it, and it must advance once the
// shim does.
func TestPendingPersistenceReceiptKeepsTheShimConnectionAndRetries(t *testing.T) {
	t.Parallel()
	d := &Daemon{shims: newSessionShimState()}
	ctrl := &pendingCursorController{
		pendingAnswers: 2,
		settled:        make(chan struct{}),
		done:           make(chan struct{}),
	}
	id := sessionshim.Identity{OrgID: "org-receipt-pending", SessionID: "session-receipt-pending"}
	acknowledger := d.startShimCursorAcknowledger(id, ctrl)
	t.Cleanup(acknowledger.stop)
	acknowledger.record(7)

	select {
	case <-ctrl.settled:
	case <-time.After(30 * time.Second):
		t.Fatalf("the acknowledger stopped retrying a pending receipt after %d attempts", ctrl.attempts())
	}

	// THE POINT: the connection was never dropped. A drop here is the measured
	// regression — the shim loses its controller and reaps its own harness.
	if dropped := ctrl.dropped(); dropped != 0 {
		t.Fatalf("a pending persistence receipt dropped the shim connection %d time(s)", dropped)
	}
	if attempts := ctrl.attempts(); attempts != ctrl.pendingAnswers+1 {
		t.Fatalf("acknowledgement attempts = %d, want the %d pending answers plus the confirming one",
			attempts, ctrl.pendingAnswers)
	}
	waitForCondition(t, 5*time.Second, "the cursor to advance after the shim confirmed it", func() bool {
		return acknowledger.highWater() == 7
	})
}

// TestPendingReceiptSentinelIsDistinguishedFromARealPersistenceFailure is the
// control: only the pending sentinel keeps the connection. A persistence
// failure still drops it, because the cursor may never claim a sequence the
// shim did not durably store.
func TestPendingReceiptSentinelIsDistinguishedFromARealPersistenceFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		answer      error
		wantDropped bool
	}{
		{
			name:   "a pending receipt keeps the connection",
			answer: fmt.Errorf("%w: acked sequence 7", sessionshim.ErrHeartbeatReceiptPending),
		},
		{
			name:        "a persistence failure still drops the connection",
			answer:      errors.New("carrier refused the cursor"),
			wantDropped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{shims: newSessionShimState()}
			ctrl := &refusingCursorController{
				refusal:  tc.answer,
				answered: make(chan struct{}),
				done:     make(chan struct{}),
			}
			id := sessionshim.Identity{OrgID: "org-receipt-control", SessionID: "session-receipt-control"}
			acknowledger := d.startShimCursorAcknowledger(id, ctrl)
			t.Cleanup(acknowledger.stop)
			acknowledger.record(7)

			select {
			case <-ctrl.answered:
			case <-time.After(10 * time.Second):
				t.Fatal("the acknowledger never attempted the cursor acknowledgement")
			}
			// A dropping answer returns from the loop; a pending one does not,
			// so the negative has to outlast a retry it forbids rather than
			// join a goroutine that will never end.
			if tc.wantDropped {
				waitForCondition(t, 10*time.Second, "the connection to be dropped", func() bool {
					return ctrl.dropped() > 0
				})
				return
			}
			time.Sleep(3 * sessionShimAdoptionBatchCommitBaseBackoff)
			if dropped := ctrl.dropped(); dropped != 0 {
				t.Fatalf("a pending persistence receipt dropped the shim connection %d time(s)", dropped)
			}
		})
	}
}

// TestOrphanGraceOutlivesAControlPlaneStall pins the third decision. The §D8
// deadline is what converts "this shim lost its controller" into "reap the
// harness", and at ninety seconds it converted every transient control-plane
// stall into a dead session. The bound it exists to enforce is the inequality,
// not the number, so the default is now long enough that a daemon restart or a
// control-plane outage resolves inside it — and an operator can still shorten
// it on an installed host without an embedder change.
func TestOrphanGraceOutlivesAControlPlaneStall(t *testing.T) {
	const floor = 15 * time.Minute
	if sessionshim.DefaultOrphanDeadline < floor {
		t.Fatalf("default orphan deadline = %s, want at least %s so a slow control plane cannot reap a live harness",
			sessionshim.DefaultOrphanDeadline, floor)
	}
	if err := sessionshim.DefaultOrphanPolicy().Validate(); err != nil {
		t.Fatalf("the default orphan policy no longer satisfies the double-execution bound: %v", err)
	}

	t.Run("the daemon adopts the default", func(t *testing.T) {
		d := &Daemon{}
		if got := d.sessionShimConfig().Orphan.Deadline; got != sessionshim.DefaultOrphanDeadline {
			t.Fatalf("resolved orphan deadline = %s, want the default %s", got, sessionshim.DefaultOrphanDeadline)
		}
	})

	t.Run("an operator can set it on an installed host", func(t *testing.T) {
		t.Setenv(sessionshim.EnvOrphanDeadlineMS, "1800000")
		d := &Daemon{}
		if got := d.sessionShimConfig().Orphan.Deadline; got != 30*time.Minute {
			t.Fatalf("resolved orphan deadline = %s, want the operator's 30m", got)
		}
	})

	t.Run("an unreadable operator value is ignored, not obeyed", func(t *testing.T) {
		t.Setenv(sessionshim.EnvOrphanDeadlineMS, "not-a-duration")
		d := &Daemon{}
		if got := d.sessionShimConfig().Orphan.Deadline; got != sessionshim.DefaultOrphanDeadline {
			t.Fatalf("resolved orphan deadline = %s, want the default %s", got, sessionshim.DefaultOrphanDeadline)
		}
	})

	t.Run("an embedder's explicit policy still wins", func(t *testing.T) {
		t.Setenv(sessionshim.EnvOrphanDeadlineMS, "1800000")
		d := &Daemon{}
		d.shimIdentityRef.Store(&sessionShimIdentity{config: &SessionShimConfig{
			Orphan: sessionshim.OrphanPolicy{Deadline: 4 * time.Minute},
		}})
		if got := d.sessionShimConfig().Orphan.Deadline; got != 4*time.Minute {
			t.Fatalf("resolved orphan deadline = %s, want the embedder's explicit 4m", got)
		}
	})
}
