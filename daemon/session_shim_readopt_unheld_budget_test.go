package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// These tests pin the initial re-adoption bound for a lineage the composing
// layer does NOT hold past the window: it gets its whole configured window
// (lineage-live) or attempt budget (fixed) before the bound's outcome, and
// ReadoptionHeldPastWindow is not consulted before that bound is spent. A
// predicate answering false — or no predicate at all — must never shorten the
// bound to a single attempt: a dispatched session hit by a short relay blip
// would otherwise be quarantined on the first refusal.

// readoptUnlessOverrun runs one re-adoption and fails the test, rather than
// hanging the package, once overran reports the run went past its own bound
// — a run that never returns is an unbounded hold the case did not ask for.
// overran reads the run's own progress (simulated time, attempts spent), not
// the wall clock, so a loaded host slows the run down without failing it; the
// wall-clock backstop only catches a run that makes no progress at all.
func readoptUnlessOverrun(t *testing.T, f *readoptFixture, lost adoptedShim, overran func() bool) sessionShimReadoptionDisposition {
	t.Helper()
	done := make(chan sessionShimReadoptionDisposition, 1)
	go func() {
		done <- f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0)
	}()
	backstop := time.After(5 * time.Minute)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case got := <-done:
			return got
		case <-tick.C:
			if !overran() {
				continue
			}
			f.daemon.shims.stopRecovery()
			<-done
			t.Fatal("re-adoption ran past its own bound for a lineage not held past it — it held without bound")
			return readoptionShutdown
		case <-backstop:
			f.daemon.shims.stopRecovery()
			<-done
			t.Fatal("re-adoption made no progress to an outcome within 5m")
			return readoptionShutdown
		}
	}
}

// TestUnheldLineageLiveWindowRunsItsWholeWindow pins the lineage-live half: a
// one-minute window with a one-second backoff capped at four seconds spends
// seventeen attempts across 59 s of simulated time (0, 1, 3, 7, 11, … 59 s)
// before it exhausts, whether the continuation predicate answers false or is
// unset. The continuation predicate is consulted only once the window is
// spent: neither the loop nor the keepalive asks it during the window, so the
// keepalive keeps extending the shim's clock on LineageLive alone until then.
//
// Attempts are counted from the window's own backoffs — the only waits that
// reach the virtual clock (the keepalive paces on the real one) — not from
// durable-adoption calls: on a loaded host an attempt can fail at the dial,
// before it reaches durable adoption, and still be an attempt.
func TestUnheldLineageLiveWindowRunsItsWholeWindow(t *testing.T) {
	t.Parallel()
	const (
		window       = time.Minute
		backoff      = time.Second
		backoffCap   = 4 * time.Second
		wantAttempts = 17
	)
	for _, tc := range []struct {
		name string
		// held reports whether to wire a continuation predicate answering false.
		held bool
	}{
		{name: "the composing layer does not hold it past the window", held: true},
		{name: "no continuation predicate", held: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newVirtualShimClock()
			started := clock.Now()
			var (
				mu          sync.Mutex
				firstAsked  time.Duration = -1
				exhausted   atomic.Int64
				heldPredict func(context.Context, sessionshim.Identity) bool
			)
			if tc.held {
				heldPredict = func(context.Context, sessionshim.Identity) bool {
					mu.Lock()
					if firstAsked < 0 {
						firstAsked = clock.elapsed(started)
					}
					mu.Unlock()
					return false
				}
			}
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy: SessionShimReadoptionPolicy{
					Mode: ReadoptionLineageLive, Backoff: backoff, BackoffCap: backoffCap, Window: window,
				},
				adoption:          refusingCarrier(),
				clock:             clock,
				lineageLive:       alwaysLive,
				heldPastWindow:    heldPredict,
				onWindowExhausted: func(context.Context, sessionshim.Identity) { exhausted.Add(1) },
			})
			lost := f.lostEntry(t)

			overran := func() bool { return clock.elapsed(started) > 2*window }
			if got := readoptUnlessOverrun(t, f, lost, overran); got != readoptionWindowExhausted {
				t.Fatalf("disposition = %d, want readoptionWindowExhausted (%d)", got, readoptionWindowExhausted)
			}
			if got := len(clock.requestedWaits()) + 1; got != wantAttempts {
				t.Fatalf("the window ran %d attempts (backoffs %v), want the whole window's %d — the window was cut short",
					got, clock.requestedWaits(), wantAttempts)
			}
			if elapsed := clock.elapsed(started); elapsed < window-backoffCap || elapsed > window {
				t.Fatalf("the window spent %s of simulated time, want the whole %s window", elapsed, window)
			}
			if n := exhausted.Load(); n != 1 {
				t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once", n)
			}
			if tc.held {
				mu.Lock()
				asked := firstAsked
				mu.Unlock()
				if asked < window-backoffCap {
					t.Fatalf("ReadoptionHeldPastWindow first consulted %s into a %s window, want only once the window is spent",
						asked, window)
				}
			}
		})
	}
}

// TestUnheldFixedBudgetSpendsEveryAttempt pins the fixed-attempt half: a
// three-attempt budget spends all three attempts before the spent-budget
// outcome, whether the continuation predicate answers false or is unset, and
// the predicate is not consulted until the budget is spent.
//
// Attempts are counted at the per-attempt LineageLive gate, which every
// attempt passes before it dials — not from durable-adoption calls, which an
// attempt that fails at the dial on a loaded host never reaches. The
// held-past-window conjunction also asks LineageLive, immediately before the
// continuation predicate, so those calls are subtracted.
func TestUnheldFixedBudgetSpendsEveryAttempt(t *testing.T) {
	t.Parallel()
	const attempts = 3
	for _, tc := range []struct {
		name string
		held bool
	}{
		{name: "the composing layer does not hold it past the budget", held: true},
		{name: "no continuation predicate", held: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu         sync.Mutex
				gateCalls  int // LineageLive calls: attempts plus conjunction checks
				asked      int // ReadoptionHeldPastWindow calls
				askedEarly int // asks made before the budget was spent
			)
			attemptsSoFar := func() int { return gateCalls - asked }
			lineageLive := func(context.Context, sessionshim.Identity) bool {
				mu.Lock()
				gateCalls++
				mu.Unlock()
				return true
			}
			var heldPredict func(context.Context, sessionshim.Identity) bool
			if tc.held {
				heldPredict = func(context.Context, sessionshim.Identity) bool {
					mu.Lock()
					asked++
					if attemptsSoFar() < attempts {
						askedEarly++
					}
					mu.Unlock()
					return false
				}
			}
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy:         SessionShimReadoptionPolicy{Attempts: attempts, Backoff: time.Millisecond},
				adoption:       refusingCarrier(),
				lineageLive:    lineageLive,
				heldPastWindow: heldPredict,
			})
			lost := f.lostEntry(t)

			overran := func() bool {
				mu.Lock()
				defer mu.Unlock()
				return attemptsSoFar() > 2*attempts
			}
			if got := readoptUnlessOverrun(t, f, lost, overran); got != readoptionAttemptsSpent {
				t.Fatalf("disposition = %d, want readoptionAttemptsSpent (%d)", got, readoptionAttemptsSpent)
			}
			mu.Lock()
			spent, askedTotal, early := attemptsSoFar(), asked, askedEarly
			mu.Unlock()
			if spent != attempts {
				t.Fatalf("the run spent %d attempts, want the whole budget of %d", spent, attempts)
			}
			if early != 0 {
				t.Fatalf("ReadoptionHeldPastWindow consulted %d times before the budget was spent, want none", early)
			}
			if tc.held && askedTotal == 0 {
				t.Fatal("ReadoptionHeldPastWindow was never consulted once the budget was spent")
			}
		})
	}
}
