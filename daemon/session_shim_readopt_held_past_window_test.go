package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// Pins for ADR-2026-09-03-readoption-exhaustion-withdraws, amendment
// 2026-10-21: a lineage the composing layer holds past the lineage-live window
// (SessionShimConfig.ReadoptionHeldPastWindow) is neither withdrawn at the
// window's end nor quarantined when lost again inside it, and everything that
// is NOT so held still withdraws exactly as before.

// heldPastWindowCounter is a ReadoptionHeldPastWindow predicate whose answer a
// test controls and whose calls it can count.
type heldPastWindowCounter struct {
	calls  atomic.Int64
	answer func() bool
}

func (h *heldPastWindowCounter) predicate(context.Context, sessionshim.Identity) bool {
	h.calls.Add(1)
	return h.answer()
}

func alwaysLive(context.Context, sessionshim.Identity) bool { return true }

// TestHeldPastWindowKeepsTheKeepaliveArmedPastWindowEnd is the headline pin,
// in the only shape that cannot pass by accident: the window ends before the
// carrier comes back, and the backoff between attempts is LONGER than the
// shim's own orphan deadline, so between two attempts nothing but the
// keepalive holds the harness. Under the bounded window the daemon exhausts at
// the first window end and withdraws (the field reap this amendment removes);
// a keepalive stopped at window end lets the shim reap before the next attempt.
// Held past the window, the daemon keeps retrying with the keepalive armed, and
// the lineage is re-adopted alive with no terminal evidence.
//
// It runs on the real clock and not in parallel: the thing under test is a
// real shim's real orphan timer. The margins match the window keepalive pin's:
// several keepalives per deadline, because CI runs this beside other real
// shims under -race.
func TestHeldPastWindowKeepsTheKeepaliveArmedPastWindowEnd(t *testing.T) {
	const (
		orphanDeadline   = 3 * time.Second
		window           = 2 * time.Second
		backoff          = 4 * time.Second // > orphanDeadline on purpose
		returnsOnAttempt = 4
	)
	var exhausted atomic.Int64
	held := &heldPastWindowCounter{answer: func() bool { return true }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: backoff, BackoffCap: backoff,
			Window: window, KeepaliveInterval: 500 * time.Millisecond,
		},
		orphan: sessionshim.OrphanPolicy{
			Deadline: orphanDeadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
		adoption: func(_ context.Context, attempt int) error {
			if attempt < returnsOnAttempt {
				return errors.New("injected carrier refusal")
			}
			return nil
		},
		lineageLive:       alwaysLive,
		heldPastWindow:    held.predicate,
		onWindowExhausted: func(context.Context, sessionshim.Identity) { exhausted.Add(1) },
	})
	lost := f.lostEntry(t)
	f.waitForOrphanArmed(t, 5*time.Second)

	started := time.Now()
	if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionSucceeded {
		t.Fatalf("disposition = %d, want readoptionSucceeded (%d): a held lineage was given up at window end",
			got, readoptionSucceeded)
	}
	if elapsed := time.Since(started); elapsed < (returnsOnAttempt-1)*backoff {
		t.Fatalf("re-adoption finished after %s, want at least %s of backoffs so the pin proves the keepalive outlived the window",
			elapsed, (returnsOnAttempt-1)*backoff)
	}
	if n := exhausted.Load(); n != 0 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times for a lineage held past the window, want 0", n)
	}
	if held.calls.Load() == 0 {
		t.Fatal("ReadoptionHeldPastWindow was never consulted, yet the window continued")
	}
	if adoptions, _ := f.snapshot(); adoptions != returnsOnAttempt {
		t.Fatalf("durable adoption attempted %d times, want %d continued retries", adoptions, returnsOnAttempt)
	}
	if keepalive := f.daemon.sessionShimKeepaliveObservations(f.id); keepalive.extensions == 0 {
		t.Fatal("the continued window never extended the shim's orphan clock")
	}
	if _, err := f.registry.GetTombstoneIncarnation(f.id, lost.shimID, lost.controller.Hello().ProcessEpoch); err == nil {
		t.Fatal("the shim left a terminal tombstone: its harness was reaped while the lineage was held")
	}
	if phase := f.recordPhase(t); phase == "exited" {
		t.Fatalf("discovery record phase = %q after a held lineage was re-adopted", phase)
	}
}

// TestHeldPastWindowWithdrawsTheMomentTheComposingLayerLetsGo pins the other
// end: continuation lasts exactly as long as the composing layer holds the
// lineage past the window — a requested stop or a spent duration quota turns
// the answer false — and then the window ends the ordinary way, through the
// disconnect path an operator sees: the hook fires once, the lineage is
// withdrawn under the exhausted-window detail.
func TestHeldPastWindowWithdrawsTheMomentTheComposingLayerLetsGo(t *testing.T) {
	t.Parallel()
	const (
		window = time.Minute
		heldBy = 3 * time.Minute
	)
	clock := newVirtualShimClock()
	started := clock.Now()
	var exhausted atomic.Int64
	held := &heldPastWindowCounter{answer: func() bool { return clock.Now().Before(started.Add(heldBy)) }}
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy: SessionShimReadoptionPolicy{
			Mode: ReadoptionLineageLive, Backoff: time.Second, BackoffCap: 4 * time.Second, Window: window,
		},
		adoption:          refusingCarrier(),
		clock:             clock,
		lineageLive:       alwaysLive,
		heldPastWindow:    held.predicate,
		onWindowExhausted: func(context.Context, sessionshim.Identity) { exhausted.Add(1) },
	})

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	if elapsed := clock.elapsed(started); elapsed < heldBy {
		t.Fatalf("the window ended after %s of simulated time, want it continued until the composing layer let go at %s",
			elapsed, heldBy)
	}
	if elapsed := clock.elapsed(started); elapsed > heldBy+window {
		t.Fatalf("the window ran %s, more than one window past the composing layer letting go at %s", elapsed, heldBy)
	}
	if n := exhausted.Load(); n != 1 {
		t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once when the hold ended", n)
	}
	if adopted := f.daemon.AdoptedSessionShims(); len(adopted) != 0 {
		t.Fatalf("adopted set = %+v after the hold ended, want the lineage withdrawn", adopted)
	}
	projected := f.daemon.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Detail != sessionShimReadoptionWindowExhaustedDetail {
		t.Fatalf("quarantine projection = %+v, want the lineage withdrawn under the exhausted-window detail", projected)
	}
}

// TestUnheldLineageStillWithdrawsAtWindowEnd pins that nothing but the
// conjunction continues a window: an unset predicate (the OSS default), a
// predicate answering false, and a predicate answering true for a lineage the
// composing layer no longer holds at all (LineageLive false) all exhaust
// exactly one window and fire the hook once.
func TestUnheldLineageStillWithdrawsAtWindowEnd(t *testing.T) {
	t.Parallel()
	const window = time.Minute
	for _, tc := range []struct {
		name string
		// lineageLive and heldPastWindow are built against the case's clock.
		lineageLive    func(clock *virtualShimClock, started time.Time) func(context.Context, sessionshim.Identity) bool
		heldPastWindow func(context.Context, sessionshim.Identity) bool
	}{
		{
			name:        "no continuation predicate",
			lineageLive: func(*virtualShimClock, time.Time) func(context.Context, sessionshim.Identity) bool { return alwaysLive },
		},
		{
			name:           "the composing layer does not hold it past the window",
			lineageLive:    func(*virtualShimClock, time.Time) func(context.Context, sessionshim.Identity) bool { return alwaysLive },
			heldPastWindow: func(context.Context, sessionshim.Identity) bool { return false },
		},
		{
			name: "held past the window but no longer held at all",
			lineageLive: func(clock *virtualShimClock, started time.Time) func(context.Context, sessionshim.Identity) bool {
				return func(context.Context, sessionshim.Identity) bool { return clock.Now().Before(started.Add(window)) }
			},
			heldPastWindow: func(context.Context, sessionshim.Identity) bool { return true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newVirtualShimClock()
			started := clock.Now()
			var exhausted atomic.Int64
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy: SessionShimReadoptionPolicy{
					Mode: ReadoptionLineageLive, Backoff: time.Second, BackoffCap: 4 * time.Second, Window: window,
				},
				adoption:          refusingCarrier(),
				clock:             clock,
				lineageLive:       tc.lineageLive(clock, started),
				heldPastWindow:    tc.heldPastWindow,
				onWindowExhausted: func(context.Context, sessionshim.Identity) { exhausted.Add(1) },
			})
			lost := f.lostEntry(t)

			if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != readoptionWindowExhausted {
				t.Fatalf("disposition = %d, want readoptionWindowExhausted (%d)", got, readoptionWindowExhausted)
			}
			if elapsed := clock.elapsed(started); elapsed > window+4*time.Second {
				t.Fatalf("the window ran %s, want it to end at its own %s bound", elapsed, window)
			}
			if n := exhausted.Load(); n != 1 {
				t.Fatalf("OnReadoptionWindowExhausted fired %d times, want exactly once", n)
			}
		})
	}
}

// TestHeldLineageLostAgainInsideTheWindowReadopts pins the re-entry guard's
// one exception: a controller lost again inside the previous window is
// re-adopted — after one paced backoff — when the composing layer holds the
// lineage past the window, and still refused when it does not.
func TestHeldLineageLostAgainInsideTheWindowReadopts(t *testing.T) {
	t.Parallel()
	const backoff = 5 * time.Second
	policy := SessionShimReadoptionPolicy{
		Mode: ReadoptionLineageLive, Backoff: backoff, BackoffCap: 30 * time.Second, Window: 10 * time.Minute,
	}
	for _, tc := range []struct {
		name           string
		lineageLive    func(context.Context, sessionshim.Identity) bool
		heldPastWindow func(context.Context, sessionshim.Identity) bool
		want           sessionShimReadoptionDisposition
		wantAdoptions  int
	}{
		{
			name:           "held past the window: re-adopted",
			lineageLive:    alwaysLive,
			heldPastWindow: func(context.Context, sessionshim.Identity) bool { return true },
			want:           readoptionSucceeded,
			wantAdoptions:  1,
		},
		{
			name:           "not held past the window: quarantined as before",
			lineageLive:    alwaysLive,
			heldPastWindow: func(context.Context, sessionshim.Identity) bool { return false },
			want:           readoptionRefused,
		},
		{
			name:           "no longer held at all: quarantined as before",
			lineageLive:    func(context.Context, sessionshim.Identity) bool { return false },
			heldPastWindow: func(context.Context, sessionshim.Identity) bool { return true },
			want:           readoptionRefused,
		},
		{
			name:        "no continuation predicate: quarantined as before",
			lineageLive: alwaysLive,
			want:        readoptionRefused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newVirtualShimClock()
			f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
				policy: policy, clock: clock,
				lineageLive: tc.lineageLive, heldPastWindow: tc.heldPastWindow,
			})
			lost := f.lostEntry(t)
			// Re-adopted 5m9s ago, inside the ten-minute governing window.
			lost.readoptedAtUnixNano = clock.Now().Add(-(5*time.Minute + 9*time.Second)).UnixNano()

			if got := f.daemon.readoptSessionShimAfterControllerLoss(f.id, lost, 0); got != tc.want {
				t.Fatalf("disposition = %d, want %d", got, tc.want)
			}
			if adoptions, _ := f.snapshot(); adoptions != tc.wantAdoptions {
				t.Fatalf("durable adoption attempted %d times, want %d", adoptions, tc.wantAdoptions)
			}
			if tc.want != readoptionSucceeded {
				return
			}
			// The re-entry is paced: the first attempt waits one backoff, so a
			// flapping carrier costs at most one adoption revision per backoff.
			if waits := clock.requestedWaits(); len(waits) == 0 || waits[0] != backoff {
				t.Fatalf("waits before the re-entered attempt = %v, want the first to be the %s backoff", waits, backoff)
			}
		})
	}
}

// TestHeldPastWindowValidation pins the configuration refusals: the
// continuation predicate needs a window to continue and a LineageLive
// predicate that can end it.
func TestHeldPastWindowValidation(t *testing.T) {
	t.Parallel()
	orphan := sessionshim.OrphanPolicy{Deadline: 90 * time.Second, TerminationGrace: 5 * time.Second}
	held := func(context.Context, sessionshim.Identity) bool { return true }
	for _, tc := range []struct {
		name    string
		cfg     SessionShimConfig
		refused bool
	}{
		{
			name: "lineage-live with both predicates",
			cfg: SessionShimConfig{
				Orphan: orphan, Readoption: DefaultLineageLiveSessionShimReadoptionPolicy(),
				LineageLive: alwaysLive, ReadoptionHeldPastWindow: held,
			},
		},
		{
			name: "continuation without LineageLive",
			cfg: SessionShimConfig{
				Orphan: orphan, Readoption: DefaultLineageLiveSessionShimReadoptionPolicy(),
				ReadoptionHeldPastWindow: held,
			},
			refused: true,
		},
		{
			name: "continuation in fixed-attempt mode",
			cfg: SessionShimConfig{
				Orphan: orphan, Readoption: DefaultSessionShimReadoptionPolicy(),
				LineageLive: alwaysLive, ReadoptionHeldPastWindow: held,
			},
			refused: true,
		},
		{
			name: "continuation with re-adoption disabled",
			cfg: SessionShimConfig{
				Orphan:      orphan,
				Readoption:  SessionShimReadoptionPolicy{Mode: ReadoptionLineageLive, Disabled: true},
				LineageLive: alwaysLive, ReadoptionHeldPastWindow: held,
			},
			refused: true,
		},
	} {
		err := tc.cfg.validateReadoptionAgainstOrphanPolicy()
		if tc.refused != errors.Is(err, ErrSessionShimReadoptionPolicy) {
			t.Errorf("%s: validate = %v, refused want %t", tc.name, err, tc.refused)
		}
	}
}
