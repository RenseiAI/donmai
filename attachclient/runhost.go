package attachclient

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// RunHost runs the host leg of one interactive PTY session. It BLOCKS until the
// Session ends and the bounded post-Exit final-screen window (§ 12.2) elapses —
// returning nil — or until ctx is cancelled — returning ctx's error — or until a
// terminal condition (epoch-stale, a non-retryable relay error control, an
// unrecoverable degraded-lane ring miss) — returning ErrEpochStale or a
// *RelayStopError.
//
// It dials OUT only (WSS, with degraded HTTPS fallback per § 14); it never opens
// an inbound listener. Reconnect uses cancel-aware exponential backoff, reset on
// success, with TokenSource re-resolution before each top-level carrier attempt;
// degraded-lane 401 recovery and upgrade probes may resolve it concurrently
// (§ 14/§ 15).
func RunHost(ctx context.Context, cfg HostConfig) error {
	if err := cfg.withDefaults(); err != nil {
		return err
	}
	h := &host{cfg: cfg, log: cfg.Logger}
	return h.run(ctx)
}

// host holds the per-RunHost state. The reconnect loop runs on one goroutine;
// per-connection goroutines (pump, reader, SSE-down, upgrade probe) are scoped
// to a single carrier attempt.
type host struct {
	cfg HostConfig
	log *slog.Logger

	// hasStreamed flips true once we have successfully subscribed on any carrier.
	// It selects the reconnect discipline: initial attach streams the full local
	// ring (fromSeq 0); a reconnect continues from the current head (§ 4.1).
	// Mutated only by the single reconnect goroutine.
	hasStreamed bool

	// killed guards the Kill hook against a second invocation (the degraded
	// lane's at-least-once SSE may redeliver a kill, § 14).
	killed atomic.Bool
}

// attemptResult reports what one carrier attempt achieved, independent of its
// error, so the reconnect loop can reset backoff, record the post-Exit window,
// and decide whether it is fully done.
type attemptResult struct {
	// progressed is true once the carrier negotiated open + had its subscribe
	// accepted (or saw an authoritative inbound frame): success → reset backoff.
	progressed bool
	// exitDelivered is true once the Exit frame went out on this carrier.
	exitDelivered bool
	// windowServed is true once the post-Exit final-screen window fully elapsed
	// on this carrier → RunHost is done.
	windowServed bool
	// windowDeadline is the absolute post-Exit deadline the carrier used, so a
	// reconnect after a mid-window drop serves only the remainder.
	windowDeadline time.Time
}

func (h *host) now() time.Time {
	if h.cfg.now != nil {
		return h.cfg.now()
	}
	return time.Now()
}

func (h *host) run(ctx context.Context) error {
	bo := newBackoff(h.cfg.BackoffMin, h.cfg.BackoffMax)

	var (
		degraded     bool      // current carrier: false = WSS, true = degraded lane
		consecFails  int       // consecutive dial failures (WSS) → fallback trigger
		exitDeadline time.Time // zero until Exit delivered; the post-Exit window end
	)

	for {
		// Terminal: the session ended and its post-Exit window has elapsed.
		if !exitDeadline.IsZero() && !h.now().Before(exitDeadline) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		tok, terr := h.cfg.TokenSource(ctx) // per-attempt re-resolution (§ 15)
		if terr != nil {
			h.log.Warn("attachclient: token source failed", "err", terr)
			if err := sleepCtx(ctx, bo.next()); err != nil {
				return err
			}
			continue
		}
		cl, cerr := parseHostClaims(tok)
		if cerr != nil {
			h.log.Warn("attachclient: parsing host token claims", "err", cerr)
			if h.maybeFallback(&degraded, &consecFails, bo) {
				continue
			}
			if err := sleepCtx(ctx, bo.next()); err != nil {
				return err
			}
			continue
		}

		var (
			res  attemptResult
			rerr error
		)
		if degraded {
			res, rerr = h.runDegraded(ctx, tok, cl, exitDeadline)
		} else {
			res, rerr = h.runWSS(ctx, tok, cl, exitDeadline)
		}

		if res.progressed {
			bo.reset()
			consecFails = 0
		}
		if res.exitDelivered && exitDeadline.IsZero() && !res.windowDeadline.IsZero() {
			exitDeadline = res.windowDeadline
		}

		// Clean session end wins over a simultaneous ctx cancel (return nil).
		if rerr == nil && res.windowServed {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		switch {
		case rerr == nil:
			// Clean carrier end without a fully-served window: loop to serve the
			// remaining window (or detect it elapsed at the top).
			if !exitDeadline.IsZero() {
				continue
			}
			return nil
		case errors.Is(rerr, ErrEpochStale):
			return ErrEpochStale
		case isRelayStop(rerr):
			return rerr
		case errors.Is(rerr, errUpgraded):
			degraded = false
			bo.reset()
			consecFails = 0
			continue
		default:
			h.log.Debug("attachclient: carrier attempt ended", "degraded", degraded, "err", rerr)
			if !degraded && h.maybeFallback(&degraded, &consecFails, bo) {
				continue
			}
			if err := sleepCtx(ctx, bo.next()); err != nil {
				return err
			}
			continue
		}
	}
}

// maybeFallback bumps the consecutive-failure counter and, if the degraded lane
// is enabled and the threshold is reached (and we are not already degraded),
// switches to it immediately (no backoff, counters reset). It reports whether
// the caller should continue the loop without sleeping.
func (h *host) maybeFallback(degraded *bool, consecFails *int, bo *backoff) bool {
	if *degraded {
		return false
	}
	*consecFails++
	if !h.cfg.DisableDegraded && *consecFails >= h.cfg.FallbackAfterN {
		h.log.Info("attachclient: falling back to the degraded SSE lane (§14)", "afterFailures", *consecFails)
		*degraded = true
		*consecFails = 0
		bo.reset()
		return true
	}
	return false
}

// subscribeFromSeq resolves the fromSeq for Session.Subscribe on a fresh
// carrier connection, encoding the § 4.1 reconnect discipline:
//
//   - initial attach (never streamed): fromSeq 0 → the full local ring from its
//     oldest buffered frame, so the relay's fresh ring is populated;
//   - reconnect: the CURRENT stream head (Snapshot().atSeq) → only genuinely new
//     frames go out. Resuming from the last seq handed to the dropped connection
//     would re-send the gap the relay already truncated (WSS has no host-ack) —
//     the spec forbids retransmit; the relay repairs via a resync Snapshot.
func (h *host) subscribeFromSeq() (attachwire.HostSeq, error) {
	if !h.hasStreamed {
		return 0, nil
	}
	_, atSeq, err := h.cfg.Session.Snapshot()
	if err != nil {
		return 0, err
	}
	return atSeq, nil
}

func (h *host) markStreamed() { h.hasStreamed = true }

// invokeKill runs the Kill hook at most once (§ 12.2). Idempotent by design.
func (h *host) invokeKill(ctx context.Context, reason, signal string) {
	if h.killed.Swap(true) {
		return
	}
	if h.cfg.Kill == nil {
		h.log.Warn("attachclient: kill requested but no Kill hook configured", "reason", reason, "signal", signal)
		return
	}
	if err := h.cfg.Kill(ctx, reason, signal); err != nil {
		h.log.Error("attachclient: kill hook returned an error", "err", err, "reason", reason)
	}
}

// buildHostSubscribe builds the § 7 host subscribe control frame:
// {sessionId, asRole:"host", epoch, resumeFrom:null}. Host resume is by
// re-subscribe (§ 4.1), so resumeFrom is always null here.
func buildHostSubscribe(cl hostClaims) (attachwire.Frame, error) {
	epoch := cl.Epoch
	return attachwire.BuildControlFrame(attachwire.Subscribe{
		SessionID:  cl.SessionID,
		AsRole:     attachwire.RoleHost,
		Epoch:      &epoch,
		ResumeFrom: nil,
	})
}

// buildErrorControl builds an outbound error control frame (§ 7) the host sends
// before closing a leg on a framing violation (§ 2.1/§ 3). The message is
// length-capped (§ 9 UI-string treatment).
func buildErrorControl(code attachwire.ErrorCode, msg string) attachwire.Frame {
	f, err := attachwire.BuildControlFrame(attachwire.ControlError{
		Code:      code,
		Message:   capString(msg, 200),
		Retryable: false,
	})
	if err != nil {
		return attachwire.NewControlFrame(nil)
	}
	return f
}

func capString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
