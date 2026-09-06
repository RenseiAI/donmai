package attachclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// RunHost runs the host leg of one interactive PTY session. It BLOCKS until the
// Session ends and the bounded post-Exit final-screen window (§ 12.2) elapses —
// returning nil — or until ctx is cancelled — returning ctx's error — or until a
// terminal condition (a locally-confirmed successor epoch, exhausted bounded
// epoch-stale recovery, or a non-retryable relay error control other than
// ring-miss) — returning ErrEpochStale or a *RelayStopError.
//
// A ring miss — the relay (or the host's own retained ring) having lost history,
// most commonly from a relay restart — is deliberately NOT terminal (§ 13: "ring
// misses are always safe"). RunHost never returns for it: internally it resets
// the local resume position and keeps re-attaching, backed off to a slow,
// unbounded cadence (RingMissRetryCeiling) rather than giving up, so a session
// reacquires its view whenever the relay returns however late. See the
// isRelayRingMiss case in run.
//
// A PLANNED relay restart — announced as a relay-restarting error control, as a
// 1012 Service Restart close, or as a 503 with Retry-After on the dial itself —
// is likewise never terminal and never a fallback trigger. The relay is telling
// this host that it is going away deliberately and will be back; RunHost waits
// out the redial floor it named and dials the replacement.
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

	// authorityMu protects the effective local PTY epoch and immutable spawn
	// session id because degraded refresh and upgrade probes may call
	// validatedToken concurrently. A valid legacy zero snapshot uses the
	// InitialAuthorityToken rather than trusting the shared live token rail.
	authorityMu      sync.Mutex
	authoritySet     bool
	localPTYEpoch    uint64
	authoritySession string

	// killed guards the Kill hook against a second invocation (the degraded
	// lane's at-least-once SSE may redeliver a kill, § 14).
	killed atomic.Bool
}

// attemptResult reports what one carrier attempt achieved, independent of its
// error, so the reconnect loop can reset backoff, record the post-Exit window,
// and decide whether it is fully done.
type attemptResult struct {
	// progressed is true once the carrier negotiated open + had its subscribe
	// written: transport progress → reset ordinary reconnect backoff.
	progressed bool
	// authorityConfirmed means the relay proved this carrier was admitted: SSE
	// returned 200 after bind, or WSS delivered an inbound frame. A bare WSS
	// subscribe write is not confirmation.
	authorityConfirmed bool
	// progressedAt is when authority was confirmed. Epoch-stale budget reset
	// measures actual admitted dwell, never dial/backoff/socket-hold time.
	progressedAt time.Time
	// exitDelivered is true once the Exit frame went out on this carrier.
	exitDelivered bool
	// windowServed is true once the post-Exit final-screen window fully elapsed
	// on this carrier → RunHost is done.
	windowServed bool
	// windowDeadline is the absolute post-Exit deadline the carrier used, so a
	// reconnect after a mid-window drop serves only the remainder.
	windowDeadline time.Time
}

// epochRetryState is one finite recovery episode. It is independent from the
// ordinary carrier backoff so a WSS subscribe that reaches an epoch-stale
// response cannot reset the stale cadence to its floor.
type epochRetryState struct {
	bo        *backoff
	attempts  int
	startedAt time.Time
}

func newEpochRetryState(lo, hi time.Duration) *epochRetryState {
	return &epochRetryState{bo: newBackoff(lo, hi)}
}

func (s *epochRetryState) active() bool { return !s.startedAt.IsZero() }

func (s *epochRetryState) expired(now time.Time, cfg HostConfig) bool {
	if !s.active() {
		return false
	}
	return !now.Before(s.startedAt.Add(cfg.EpochStaleRetryWindow))
}

func (s *epochRetryState) next(now time.Time, cfg HostConfig) (time.Duration, bool) {
	if !s.active() {
		s.startedAt = now
	}
	if s.attempts >= cfg.EpochStaleMaxRetries || s.expired(now, cfg) {
		return 0, false
	}
	s.attempts++
	delay := s.bo.next()
	remaining := s.startedAt.Add(cfg.EpochStaleRetryWindow).Sub(now)
	if delay > remaining {
		delay = remaining
	}
	if delay <= 0 {
		return 0, false
	}
	return delay, true
}

func (s *epochRetryState) reset() {
	s.attempts = 0
	s.startedAt = time.Time{}
	s.bo.reset()
}

func (h *host) now() time.Time {
	if h.cfg.now != nil {
		return h.cfg.now()
	}
	return time.Now()
}

func (h *host) run(ctx context.Context) error {
	bo := newBackoff(h.cfg.BackoffMin, h.cfg.BackoffMax)
	epochRetry := newEpochRetryState(h.cfg.BackoffMin, h.cfg.BackoffMax)
	// ringBo is the RESET-AND-RETRY cadence for §13 ring misses — deliberately
	// separate from bo (which governs ordinary dial/network reconnects): it
	// shares the same floor but a slower, dedicated ceiling, and repeated ring
	// misses are never fatal (see the isRelayRingMiss case below), so this
	// backoff is reset only on an attempt that does NOT itself ring-miss.
	ringBo := newBackoff(h.cfg.BackoffMin, h.cfg.RingMissRetryCeiling)

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
		if epochRetry.expired(h.now(), h.cfg) {
			return ErrEpochStale
		}

		tok, terr := h.validatedToken(ctx) // per-attempt re-resolution (§ 15)
		if terr != nil {
			if errors.Is(terr, errEpochGrantSuperseded) {
				return ErrEpochStale
			}
			h.log.Warn("attachclient: token source failed", "err", terr)
			if epochRetry.active() || errors.Is(terr, errEpochGrantAmbiguous) {
				if err := h.waitEpochRetry(ctx, epochRetry, "grant authority is ambiguous", terr); err != nil {
					return err
				}
				continue
			}
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
		if epochRetry.active() && res.authorityConfirmed && !res.progressedAt.IsZero() &&
			!errors.Is(rerr, ErrEpochStale) &&
			h.now().Sub(res.progressedAt) >= h.cfg.EpochStaleStableWindow {
			epochRetry.reset()
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
			ringBo.reset()
			// Clean carrier end without a fully-served window: loop to serve the
			// remaining window (or detect it elapsed at the top).
			if !exitDeadline.IsZero() {
				continue
			}
			return nil
		case errors.Is(rerr, errEpochGrantSuperseded):
			return ErrEpochStale
		case errors.Is(rerr, ErrEpochStale), errors.Is(rerr, errEpochGrantAmbiguous):
			if err := h.waitEpochRetry(ctx, epochRetry, "relay rejected the current PTY epoch", rerr); err != nil {
				return err
			}
			continue
		case isRelayStop(rerr):
			return rerr
		case isRelayRingMiss(rerr):
			// §13: the relay (or our own retained ring) lost history — the
			// designed repair path, RESET-AND-RETRY, never terminal. Drop the
			// local resume position so the next top-level attempt subscribes
			// fresh from 0 (no resume_from) and back off on the dedicated,
			// slower, never-exhausted ring-miss cadence — NOT bo, which stays
			// reserved for ordinary dial/network failures. ringBo is reset only
			// when a later attempt does not itself ring-miss, so a multi-bounce
			// window (several restarts in a row) keeps deepening the backoff
			// instead of hammering a relay that keeps coming back wiped.
			h.log.Warn("attachclient: relay lost ring state — resetting for a fresh re-attach", "err", rerr)
			// Receiving ring-miss control proves this carrier was admitted for
			// the current local PTY epoch. Clear any older stale-ownership episode
			// so its elapsed bound can never override §13's non-terminal repair.
			epochRetry.reset()
			h.hasStreamed = false
			if err := sleepCtx(ctx, ringBo.next()); err != nil {
				return err
			}
			continue
		case IsRelayRestarting(rerr):
			// A PLANNED restart. The relay said so — as a control, as a 1012
			// close, or as a 503 on the dial — so this is the one disconnect
			// that is known in advance to be transient. Never terminal, and
			// never a fallback trigger either: the drain refuses BOTH lanes, so
			// counting it toward the § 14 degraded fallback would move a healthy
			// carrier onto the slow lane for a condition the slow lane shares.
			//
			// The relay's own hint is a FLOOR, not a replacement for backoff:
			// arriving back before the replacement process has booted is how a
			// whole fleet turns one restart into a thundering herd.
			ringBo.reset()
			delay := bo.next()
			if hint, _ := RelayRedialAfter(rerr); hint > delay {
				delay = hint
			}
			h.log.Warn("attachclient: the relay announced a planned restart — waiting out its redial floor",
				"delay", delay, "err", rerr)
			if err := sleepCtx(ctx, delay); err != nil {
				return err
			}
			continue
		case errors.Is(rerr, errUpgraded):
			ringBo.reset()
			degraded = false
			bo.reset()
			consecFails = 0
			continue
		default:
			ringBo.reset()
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

// validatedToken binds every bearer resolution — including degraded 401
// refresh and upgrade probes — to the live Session's effective PTY epoch. A
// nonzero Snapshot epoch is ground truth. The shipped legacy session surface
// reports the valid epoch zero, so that compatibility shape requires the
// immutable InitialAuthorityToken instead of trusting the first live token-file
// read. A later higher grant is successor evidence and is never used by this
// Session; a lower grant is ambiguous refresh lag.
func (h *host) validatedToken(ctx context.Context) (string, error) {
	tok, err := h.cfg.TokenSource(ctx)
	if err != nil {
		return "", err
	}
	claims, err := parseHostClaims(tok)
	if err != nil {
		return "", err
	}
	if claims.Epoch < 0 {
		return "", fmt.Errorf("%w: negative grant epoch %d", errEpochGrantAmbiguous, claims.Epoch)
	}

	h.authorityMu.Lock()
	defer h.authorityMu.Unlock()
	if !h.authoritySet {
		screen, _, snapErr := h.cfg.Session.Snapshot()
		if snapErr != nil {
			return "", fmt.Errorf("attachclient: reading local PTY epoch: %w", snapErr)
		}
		initialClaims := claims
		if h.cfg.InitialAuthorityToken != "" {
			parsedInitial, initialErr := parseHostClaims(h.cfg.InitialAuthorityToken)
			if initialErr != nil {
				return "", fmt.Errorf("attachclient: parsing InitialAuthorityToken: %w", initialErr)
			}
			initialClaims = parsedInitial
		}
		if screen.Epoch == 0 {
			if h.cfg.InitialAuthorityToken == "" {
				return "", errors.New("attachclient: HostConfig.InitialAuthorityToken is required when Session snapshot epoch is zero")
			}
			if initialClaims.Epoch < 0 {
				return "", fmt.Errorf("attachclient: InitialAuthorityToken has negative epoch %d", initialClaims.Epoch)
			}
			h.localPTYEpoch = uint64(initialClaims.Epoch)
		} else {
			h.localPTYEpoch = screen.Epoch
			if initialClaims.Epoch < 0 || uint64(initialClaims.Epoch) != screen.Epoch {
				return "", fmt.Errorf("%w: initial authority epoch %d does not match local PTY epoch %d",
					errEpochGrantSuperseded, initialClaims.Epoch, screen.Epoch)
			}
		}
		h.authoritySession = initialClaims.SessionID
		h.authoritySet = true
	}
	if claims.SessionID != h.authoritySession {
		return "", fmt.Errorf("%w: grant session changed from %q to %q",
			errEpochGrantSuperseded, h.authoritySession, claims.SessionID)
	}
	grantEpoch := uint64(claims.Epoch)
	switch {
	case grantEpoch > h.localPTYEpoch:
		return "", fmt.Errorf("%w: grant epoch %d exceeds local PTY epoch %d",
			errEpochGrantSuperseded, grantEpoch, h.localPTYEpoch)
	case grantEpoch < h.localPTYEpoch:
		return "", fmt.Errorf("%w: grant epoch %d trails local PTY epoch %d",
			errEpochGrantAmbiguous, grantEpoch, h.localPTYEpoch)
	default:
		return tok, nil
	}
}

func (h *host) waitEpochRetry(
	ctx context.Context,
	state *epochRetryState,
	reason string,
	cause error,
) error {
	delay, ok := state.next(h.now(), h.cfg)
	if !ok {
		return ErrEpochStale
	}
	h.log.Warn("attachclient: epoch authority unresolved — retrying with bounded backoff",
		"reason", reason,
		"attempt", state.attempts,
		"maxAttempts", h.cfg.EpochStaleMaxRetries,
		"delay", delay,
		"err", cause)
	sleep := sleepCtx
	if h.cfg.epochStaleSleep != nil {
		sleep = h.cfg.epochStaleSleep
	}
	return sleep(ctx, delay)
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
//   - §13 ring-miss reset: run's isRelayRingMiss case sets hasStreamed back to
//     false before the next attempt, so this resolves to fromSeq 0 exactly like
//     initial attach — a fresh re-attach with NO resume position, letting the
//     relay rebuild the room from a requested Snapshot instead of asking for a
//     specific seq it (or we) no longer hold.
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
