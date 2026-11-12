package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// sessionShimReadoptionDisposition is what one re-adoption pass concluded.
//
// It replaces a bool because the caller's answer differs per outcome and one
// of them did not exist before: a window that ended while the shim was still
// observable is not the same fact as a shim that is gone, and quarantining both
// with one reason is how an operator lost the ability to tell a carrier that
// never came back from a harness that died (§D8, amendment 2026-09-03).
type sessionShimReadoptionDisposition uint8

const (
	// readoptionRefused: no re-adoption was attempted — disabled, misconfigured,
	// re-entered inside the previous window, or the daemon is stopping.
	readoptionRefused sessionShimReadoptionDisposition = iota
	// readoptionSucceeded: the lineage is adopted again under a strictly newer
	// generation and the receiver has been told.
	readoptionSucceeded
	// readoptionLineageGone: the shim is no longer observable, or the control
	// plane already holds conflicting adoption evidence. The ordinary
	// controller-loss quarantine applies.
	readoptionLineageGone
	// readoptionWindowExhausted: the lineage-live window ended with the shim
	// still observable — the one outcome the dead-shim path cannot produce.
	readoptionWindowExhausted
	// readoptionAttemptsSpent: a fixed-attempt budget ran out. The caller's
	// disposition is the same as readoptionLineageGone's, but the fact is not:
	// "the attempts ran out" and "the shim is no longer there" are different
	// things, and an enum member whose name says the second while meaning the
	// first is documentation that lies.
	readoptionAttemptsSpent
	// readoptionShutdown leaves the live lineage to final daemon release.
	readoptionShutdown
	// readoptionComposingReleased is internal only: the composing layer no
	// longer holds this lineage (LineageLive answered false inside the
	// attempt). The fixed-attempt hold maps it to readoptionAttemptsSpent
	// while the shim stays observable; every other path maps it to
	// readoptionLineageGone, so a decisive, recorded, or vanished LineageGone
	// is never relabelled by a predicate that flips at the same time.
	readoptionComposingReleased
)

// readoptSessionShimAfterControllerLoss re-adopts ONE live shim whose
// controller stream this daemon closed because its durable carrier refused.
//
// The loss the composing carrier suffered — a relay redeploy, a transport
// reset — leaves the shim, its harness, and its discovery record exactly where
// they were. Quarantining that lineage at once hands it to the shim's orphan
// deadline, which then reaps a healthy harness for a fault nobody at either
// end had (§D8 reserves that outcome for a daemon that never returned). This
// runs the pipeline the startup pass runs — dial, prepare, durable adoption,
// complete batch, carrier activation — for exactly this identity, bounded by
// the configured policy, and reports readoptionSucceeded once the lineage is
// adopted again under a strictly newer generation. The shim disarms its own
// orphan clock on that adoption.
//
// Which bound applies is the policy's mode (§D8, amendment 2026-09-03).
// ReadoptionFixedAttempts spends a fixed attempt count inside the shim's own
// orphan deadline. ReadoptionLineageLive spends a window that may exceed that
// deadline, held open only while this daemon can still SEE the lineage, and
// paid for with a periodic keepalive that re-arms the shim's clock — because a
// daemon that is visibly still dialling is not the absent daemon the orphan
// rule bounds.
//
// The lost entry stays in d.shims.adopted throughout: the receiver holds the
// lineage adopted at that generation, and every projection built meanwhile
// must keep saying so. The swap to the new controller happens under the lock,
// only when the lost controller is still the adopted one, and is undone when
// the batch that would have told the receiver about it does not commit.
// attemptBudget, when positive, replaces the configured policy for THIS run
// with that many fixed attempts. It is how the durable-acknowledgement
// ambiguity path takes its last look at a lineage without either skipping the
// look (which ADR-2026-09-03 rejects by name) or re-entering a full
// lineage-live window it has already spent. The re-entry guard below still uses
// the CONFIGURED policy's window: which window governs "lost again too soon" is
// a property of the deployment, not of one reduced run.
func (d *Daemon) readoptSessionShimAfterControllerLoss(
	id sessionshim.Identity,
	lost adoptedShim,
	attemptBudget int,
) sessionShimReadoptionDisposition {
	return d.readoptSessionShimAfterCarrierLoss(id, lost, attemptBudget, false)
}

// readoptSessionShimAfterPlatformCarrierLoss handles a carrier loss caused by
// the platform's own durable persistence path. It preserves the harness and
// re-adopts with the ordinary bounded policy, but does not count the platform
// outage as a second shim-side loss inside the quarantine window.
func (d *Daemon) readoptSessionShimAfterPlatformCarrierLoss(
	id sessionshim.Identity,
	lost adoptedShim,
	attemptBudget int,
) sessionShimReadoptionDisposition {
	return d.readoptSessionShimAfterCarrierLoss(id, lost, attemptBudget, true)
}

func (d *Daemon) readoptSessionShimAfterCarrierLoss(
	id sessionshim.Identity,
	lost adoptedShim,
	attemptBudget int,
	platformPersistLoss bool,
) sessionShimReadoptionDisposition {
	if d.shims.recoveryCtx.Err() != nil {
		return readoptionShutdown
	}
	cfg := d.sessionShimConfig()
	policy := cfg.readoption()
	if policy.Disabled || lost.controller == nil {
		return readoptionRefused
	}
	if err := policy.Validate(); err != nil {
		// adoptSessionShims refuses to start on this, so reaching it here means
		// a daemon that never ran startup adoption. Refuse the same way rather
		// than running a policy whose own fields disagree.
		slog.Warn("session shim: re-adoption policy is invalid", "session", id.String(), "error", err)
		return readoptionRefused
	}
	window := policy.WorstCaseWindow()
	reentered := false
	if !platformPersistLoss && lost.readoptedAtUnixNano != 0 {
		if since := d.shimNow().Sub(time.Unix(0, lost.readoptedAtUnixNano)); since < window {
			// Re-adopted inside the window and lost again: this carrier is not
			// one a bounded retry can restore, and every further cycle costs an
			// adoption revision the receiver has to re-attest. The bound is the
			// window that GOVERNED the previous re-adoption, which in
			// lineage-live mode is ten minutes, not the fixed-mode arithmetic.
			//
			// The one exception is a lineage the composing layer still holds:
			// quarantining it here hands a harness its owner never asked to
			// stop to the orphan deadline — the exact reap the hold exists to
			// prevent. It re-enters as an already-continued hold, paced by one
			// backoff before its first attempt, so a flapping carrier still
			// costs at most one adoption revision per backoff. While the shim
			// stays observable the daemon never releases a held lineage for
			// flapping alone; only positive evidence (shim gone, lineage no
			// longer held, decisive release) ends it.
			if !d.sessionShimLineageHeldContext(d.shims.recoveryCtx, cfg, id) {
				slog.Warn("session shim: controller lost again inside the re-adoption window; quarantining rather than re-adopting",
					"session", id.String(), "sinceReadoption", since, "window", window, "mode", policy.Mode)
				return readoptionRefused
			}
			if d.shims.recoveryCtx.Err() != nil {
				return readoptionShutdown
			}
			slog.Warn("session shim: controller lost again inside the re-adoption window; the composing layer holds the lineage past the window, re-adopting",
				"session", id.String(), "sinceReadoption", since, "window", window, "mode", policy.Mode)
			// With attemptBudget > 0 (the durable-acknowledgement ambiguity
			// path's last look) the policy below becomes fixed attempts, which
			// run strictly inside the orphan deadline with no keepalive, so
			// "reentered" has nothing to pace there and is dropped: the held
			// lineage gets that bounded look instead of an outright refusal.
			reentered = true
		}
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		slog.Warn("session shim: re-adoption after controller loss has no registry", "session", id.String(), "error", err)
		return readoptionRefused
	}
	if attemptBudget > 0 {
		policy = SessionShimReadoptionPolicy{
			Mode:           ReadoptionFixedAttempts,
			Attempts:       attemptBudget,
			Backoff:        policy.Backoff,
			AttemptTimeout: policy.AttemptTimeout,
		}
	}
	hello := lost.controller.Hello()
	if policy.Mode == ReadoptionLineageLive {
		return d.readoptSessionShimWithinLivenessWindow(registry, cfg, policy, id, lost, hello, reentered)
	}
	// A reduced run (attemptBudget > 0, the durable-acknowledgement ambiguity
	// path's last look) never holds past its budget: the single look the
	// ambiguity path buys stays a single look, even for a held lineage.
	return d.readoptSessionShimWithinFixedAttempts(registry, cfg, policy, id, lost, hello, attemptBudget == 0)
}

// readoptSessionShimWithinFixedAttempts runs the fixed-attempt initial burst
// and then holds past the budget while the shim stays observable. The budget
// (Attempts of at most AttemptTimeout each) sizes the initial burst, not the
// outage the daemon survives: past it, the lineage stays adopted, the orphan
// keepalive keeps extending the shim's clock, and re-adoption retries with
// capped backoff (see DefaultSessionShimReadoptionBackoffCap) until the
// carrier returns or positive evidence ends the hold (shim gone, lineage no
// longer held, decisive released answer, expired credential, recorded
// conflict, shutdown). A reduced run — the durable-acknowledgement ambiguity
// path's last look, which replaces the configured policy with one fixed
// attempt (allowHold=false) — never holds: that look runs strictly inside the
// orphan deadline with no keepalive, so the held lineage gets that bounded
// look instead of an unbounded hold against a durable side the ambiguity
// design says cannot answer it.
func (d *Daemon) readoptSessionShimWithinFixedAttempts(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	policy SessionShimReadoptionPolicy,
	id sessionshim.Identity,
	lost adoptedShim,
	hello shimwire.Hello,
	allowHold bool,
) sessionShimReadoptionDisposition {
	if !allowHold {
		// The reduced last look: one bounded attempt, no hold, no keepalive.
		backoff := policy.Backoff
		for attempt := 1; attempt <= policy.Attempts; attempt++ {
			if attempt > 1 {
				if !d.sleepSessionShimRecoveryBackoff(backoff) {
					return readoptionShutdown
				}
				backoff *= 2
			}
			disposition, done := d.readoptSessionShimAttempt(registry, cfg, id, lost, hello, attempt, policy.Attempts)
			if done {
				if disposition == readoptionComposingReleased {
					return readoptionLineageGone
				}
				return disposition
			}
		}
		return readoptionAttemptsSpent
	}
	// Full run: the initial burst, then an unbounded hold while observable.
	// The keepalive runs for the whole hold (not just past the budget), so a
	// shim whose deadline is shorter than the backoff never reaps mid-hold.
	stopKeepalive := d.startSessionShimOrphanKeepalive(registry, cfg, id, hello)
	defer stopKeepalive()
	backoffCap := policy.BackoffCap
	if backoffCap <= 0 {
		backoffCap = DefaultSessionShimReadoptionBackoffCap
	}
	backoff := policy.Backoff
	if backoff <= 0 {
		backoff = defaultSessionShimReadoptionBackoff
	}
	if backoff > backoffCap {
		backoff = backoffCap
	}
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			if !d.sleepSessionShimWindowBackoff(backoff) {
				return readoptionShutdown
			}
			if backoff *= 2; backoff > backoffCap {
				backoff = backoffCap
			}
		}
		disposition, done := d.readoptSessionShimAttempt(registry, cfg, id, lost, hello, attempt, 0)
		if done {
			if disposition == readoptionComposingReleased {
				// The composing layer let go. While the shim stays
				// observable that is the hold ending under the ordinary
				// bound outcome, so it reports the spent budget; a
				// vanished shim keeps its own LineageGone whatever a
				// predicate re-asked afterwards would say. The detail
				// names the harness liveness at release (see
				// quarantineLostSessionShim).
				if sessionShimIncarnationStillLive(registry, id, hello.ShimID, hello.ProcessEpoch) {
					return readoptionAttemptsSpent
				}
				return readoptionLineageGone
			}
			return disposition
		}
		// Silence while observable is not evidence: keep retrying. Only
		// positive evidence ends the hold, and the attempt above already
		// reports it (shim gone, lineage released, decisive answer).
		// The loop re-checks observability through the next attempt's
		// liveness gate rather than duplicating it here.
	}
}

// readoptSessionShimWithinLivenessWindow retries with exponential backoff
// capped at BackoffCap (see DefaultSessionShimReadoptionBackoffCap) for as long
// as this daemon can still observe the shim alive and holding the lineage.
// There is no fixed attempt window: while the control plane is unreachable or
// answering 5xx/timeouts, a session whose shim and harness are still alive is
// NOT released or reaped. The Window sizes the initial budget for sizing and
// for the re-entry guard only; past it the daemon continues in place while the
// lineage stays held, with the keepalive still armed.
//
// The keepalive runs for the whole hold and stops with it. That ordering is
// the safety property: the moment this function returns — success or a lineage
// that vanished or is no longer held — nothing is extending the shim's clock
// any more, so a shim this daemon has stopped working on is back on its
// ordinary orphan deadline without anyone having to remember to put it there.
// Re-adoptions that flap inside the previous window re-enter paced by one
// backoff (see readoptSessionShimAfterCarrierLoss); from then on the hold is
// re-asked on every attempt and every paced keepalive tick, so the instant the
// composing layer lets go (a stop was requested, a duration quota ran out)
// the hold ends through the ordinary withdrawal.
func (d *Daemon) readoptSessionShimWithinLivenessWindow(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	policy SessionShimReadoptionPolicy,
	id sessionshim.Identity,
	lost adoptedShim,
	hello shimwire.Hello,
	reentered bool,
) sessionShimReadoptionDisposition {
	// The keepalive is gated on the lineage being held (LineageLive, nil means
	// held for a standalone daemon). It stops the moment the composing layer
	// lets go, so a released harness reaps on its own deadline.
	stopKeepalive := d.startSessionShimOrphanKeepalive(registry, cfg, id, hello)
	defer stopKeepalive()
	backoffCap := policy.BackoffCap
	if backoffCap <= 0 {
		backoffCap = DefaultSessionShimReadoptionBackoffCap
	}
	backoff := policy.Backoff
	if backoff <= 0 {
		backoff = defaultSessionShimReadoptionBackoff
	}
	if backoff > backoffCap {
		backoff = backoffCap
	}
	for attempt := 1; ; attempt++ {
		if attempt > 1 || reentered {
			if !d.sleepSessionShimWindowBackoff(backoff) {
				return readoptionShutdown
			}
			if backoff *= 2; backoff > backoffCap {
				backoff = backoffCap
			}
		}
		reentered = false
		disposition, done := d.readoptSessionShimAttempt(registry, cfg, id, lost, hello, attempt, 0)
		if done {
			if disposition == readoptionComposingReleased {
				// The composing layer let go. The hold ends through the
				// ordinary withdrawal; the detail names the harness
				// liveness at release (see quarantineLostSessionShim).
				return readoptionLineageGone
			}
			return disposition
		}
		// Silence while observable is not evidence: keep retrying with capped
		// backoff. Only positive evidence ends the hold, and the attempt above
		// already reports it (shim gone, lineage released, decisive answer).
	}
}

// readoptSessionShimAttempt runs the liveness gate and ONE re-adoption attempt,
// reporting whether the pass is over. Both modes share it so neither can come
// to gate liveness differently from the other.
func (d *Daemon) readoptSessionShimAttempt(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	lost adoptedShim,
	hello shimwire.Hello,
	attempt int,
	attempts int,
) (sessionShimReadoptionDisposition, bool) {
	d.shims.mu.RLock()
	hook := d.shims.transportRecoveryAttemptHook
	d.shims.mu.RUnlock()
	if hook != nil && lost.carrierTransportLoss != nil {
		hook()
	}
	// This is the admission fence shared with Stop. A child of the lifetime
	// context is cancelled synchronously, including when Stop wins immediately
	// after admission; no scheduled channel watcher grants permission to start.
	d.lifecycleMu.Lock()
	if d.stopGen != nil || d.shims.recoveryCtx.Err() != nil {
		d.lifecycleMu.Unlock()
		return readoptionShutdown, true
	}
	attemptCtx, cancel := context.WithCancel(d.shims.recoveryCtx)
	d.lifecycleMu.Unlock()
	defer cancel()
	if !sessionShimIncarnationStillLive(registry, id, hello.ShimID, hello.ProcessEpoch) {
		// The record is gone or the shim already left its proof on disk:
		// there is nothing to re-adopt, and the caller's path consumes the
		// tombstone before it publishes anything.
		return readoptionLineageGone, true
	}
	if !d.sessionShimLineageHeldContext(attemptCtx, cfg, id) {
		// The composing layer no longer holds this lineage. Retrying would
		// re-adopt something nobody upstream wants, and every further keepalive
		// would extend the clock of a harness that should be reaped on the
		// ordinary deadline. The distinct disposition carries that reason out
		// so the fixed-attempt hold can report the spent budget while every
		// other path reports the ordinary withdrawal — without re-asking a
		// predicate that may have flipped under a decisive, recorded, or
		// vanished answer on the same attempt.
		slog.Warn("session shim: the composing layer no longer holds this lineage; stopping re-adoption",
			"session", id.String(), "attempt", attempt)
		if d.shims.recoveryCtx.Err() != nil {
			return readoptionShutdown, true
		}
		return readoptionComposingReleased, true
	}
	if d.shims.recoveryCtx.Err() != nil {
		return readoptionShutdown, true
	}
	err := d.readoptSessionShimOnce(attemptCtx, registry, cfg, id, lost, hello, d.shimNow().UnixNano())
	if d.shims.recoveryCtx.Err() != nil {
		return readoptionShutdown, true
	}
	if err == nil {
		slog.Info("session shim: re-adopted a live shim after controller loss",
			"session", id.String(), "attempt", attempt)
		return readoptionSucceeded, true
	}
	var recorded *SessionShimAdoptionEvidenceRecorded
	if errors.As(err, &recorded) {
		// The control plane already holds adoption evidence this batch
		// conflicts with; presenting the same lineage again cannot change
		// that answer, so every further attempt would only spend prepares.
		slog.Warn("session shim: re-adoption refused as already-recorded evidence; quarantining",
			"session", id.String(), "attempt", attempt, "error", err)
		return readoptionLineageGone, true
	}
	if isSessionShimDecisiveRelease(err) {
		// A decisive, authenticated released answer — or an expired credential
		// the hold must never extend — is positive evidence the session should
		// end, even when the shim is still observable. It ends a continued
		// window or a fixed-attempt hold at once rather than retrying
		// through it.
		slog.Warn("session shim: re-adoption answered with positive evidence the lineage should end",
			"session", id.String(), "attempt", attempt, "error", err)
		return readoptionLineageGone, true
	}
	slog.Warn("session shim: re-adoption attempt after controller loss failed",
		"session", id.String(), "attempt", attempt, "attempts", attempts, "error", err)
	return readoptionRefused, false
}

// sessionShimLineageHeld asks the composing layer whether it still holds this
// lineage. A nil predicate means yes: a standalone daemon has nothing above it
// to ask, and the discovery record it checked already is then the whole
// observation.
func (d *Daemon) sessionShimLineageHeldContext(parent context.Context, cfg SessionShimConfig, id sessionshim.Identity) bool {
	if cfg.LineageLive == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(parent, cfg.callbackTimeout())
	defer cancel()
	return cfg.LineageLive(ctx, id)
}

// sessionShimReadoptionHeldPastWindow is the conjunction a continued window
// needs: the composing layer still holds the lineage at all (LineageLive) AND
// holds it on terms the window does not bound (ReadoptionHeldPastWindow). An
// unset continuation predicate — the OSS default — or an unset LineageLive
// answers false, so nothing continues without both.
func (d *Daemon) sessionShimReadoptionHeldPastWindow(parent context.Context, cfg SessionShimConfig, id sessionshim.Identity) bool {
	if cfg.ReadoptionHeldPastWindow == nil || cfg.LineageLive == nil {
		return false
	}
	if !d.sessionShimLineageHeldContext(parent, cfg, id) {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, cfg.callbackTimeout())
	defer cancel()
	return callReadoptionHeldPastWindow(ctx, cfg.ReadoptionHeldPastWindow, id)
}

// callReadoptionHeldPastWindow runs the embedder's predicate and answers false
// if it panics. The predicate is consulted from the keepalive goroutine, where
// an unrecovered panic would take the whole daemon down; a predicate that
// cannot answer has not said the lineage is held, so false is the fail-closed
// reading.
func callReadoptionHeldPastWindow(ctx context.Context, held func(context.Context, sessionshim.Identity) bool, id sessionshim.Identity) (answer bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("session shim: ReadoptionHeldPastWindow panicked; treating the lineage as not held past the window",
				"session", id.String(), "panic", r)
			answer = false
		}
	}()
	return held(ctx, id)
}

// sessionShimWindowExhausted is retained for the OnReadoptionWindowExhausted
// hook's contract only: with unbounded holds while the shim stays observable,
// a live window no longer exhausts, so this no longer produces
// readoptionWindowExhausted for an observable shim. It still classifies a
// window end that finds the shim gone as LineageGone, and fires the hook
// before withdrawing an observable lineage when a caller still routes through
// it (legacy bounded paths and tests).
//
//nolint:unused // retained for hook contract and legacy callers; live holds no longer route through it.
func (d *Daemon) sessionShimWindowExhausted(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	hello shimwire.Hello,
	deadline time.Time,
) sessionShimReadoptionDisposition {
	if d.shims.recoveryCtx.Err() != nil {
		return readoptionShutdown
	}
	if !sessionShimIncarnationStillLive(registry, id, hello.ShimID, hello.ProcessEpoch) {
		return readoptionLineageGone
	}
	slog.Warn("session shim: the re-adoption window ended with the shim still observable",
		"session", id.String(), "deadline", deadline)
	if cfg.OnReadoptionWindowExhausted != nil {
		ctx, cancel := context.WithTimeout(d.shims.recoveryCtx, cfg.callbackTimeout())
		cfg.OnReadoptionWindowExhausted(ctx, id)
		cancel()
	}
	if d.shims.recoveryCtx.Err() != nil {
		return readoptionShutdown
	}
	return readoptionWindowExhausted
}

// isSessionShimDecisiveRelease reports whether a re-adoption failure is positive
// evidence the session should end rather than silence to hold through. Only a
// decisive, authenticated released answer and an expired credential qualify: a
// timeout, 5xx, connection refused, relay draining, or any other refusal the
// carrier never examined carries no fact about the lineage, so a continued
// window or a fixed-attempt hold keeps retrying through it. Recorded-evidence
// conflicts are classified by the caller before this runs and never reach it.
func isSessionShimDecisiveRelease(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrSessionShimLineageReleased) || errors.Is(err, ErrSessionShimCredentialExpired)
}

// sleepSessionShimRecoveryBackoff uses the fixed policy clock and recovery lifetime.
func (d *Daemon) sleepSessionShimRecoveryBackoff(backoff time.Duration) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return d.shims.recoveryCtx.Err() == nil
	case <-d.shims.recoveryCtx.Done():
		return false
	}
}

// sleepSessionShimWindowBackoff uses the injectable window clock and recovery lifetime.
func (d *Daemon) sleepSessionShimWindowBackoff(backoff time.Duration) bool {
	select {
	case <-d.shimAfter(backoff):
		return true
	case <-d.shims.recoveryCtx.Done():
		return false
	}
}

// startSessionShimOrphanKeepalive extends the shim's orphan clock for as long
// as the returned stop function has not been called AND the composing layer
// still holds the lineage.
//
// This is the daemon half of the amendment's obligation, and without it the
// ten-minute window is a fiction under any deadline shorter than it: the shim
// would reap its own harness mid-window and the exhaustion outcome would be
// unreachable because the incarnation was already gone. A failing keepalive is
// deliberately NOT fatal to the loop — it extends nothing, which is exactly the
// "shim becomes unobservable falls back to the orphan deadline immediately"
// behaviour the amendment asks for, and the window's own liveness gate is what
// concludes the lineage is gone.
//
// The LineageLive check on every tick is the other half of the amendment's
// "and still holds the lineage". The attempt loop checks it too, but it checks
// it once per backoff — up to a whole BackoffCap apart — and an extension sent
// after the composing layer released the lineage is exactly the extension §D8's
// inequality forbids. The keepalive is the thing holding the harness alive, so
// the keepalive is where that question has to be asked.
//
// It paces on the REAL clock, not on the daemon's injectable one, and that is
// not an inconsistency: the thing being fed is the SHIM's orphan timer, which
// is a real timer in another process. Pacing this against a clock the shim
// cannot see is how a keepalive comes to be "sent" every thirty simulated
// seconds while the shim reaps itself after ninety real ones. The window's own
// arithmetic — which is this daemon's alone — stays entirely on the injectable
// clock.
func (d *Daemon) startSessionShimOrphanKeepalive(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	hello shimwire.Hello,
) func() {
	return d.startSessionShimOrphanKeepaliveWhile(registry, cfg, id, hello, func(ctx context.Context) bool {
		return d.sessionShimLineageHeldContext(ctx, cfg, id)
	})
}

// startSessionShimOrphanKeepaliveWhile is startSessionShimOrphanKeepalive with
// the paced-tick predicate supplied by the window: LineageLive alone inside the
// window, and the held-past-window conjunction once it has been continued.
func (d *Daemon) startSessionShimOrphanKeepaliveWhile(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	hello shimwire.Hello,
	held func(context.Context) bool,
) func() {
	interval := cfg.readoptionKeepaliveInterval()
	// The observations are per WINDOW, not per daemon lifetime: "the daemon
	// extended this shim's clock" is a question about the window being asked
	// about.
	d.shims.mu.Lock()
	delete(d.shims.keepalives, id)
	d.shims.mu.Unlock()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		honoured := false
		// retry escalates from the fast probe up to the paced interval. It
		// ESCALATES rather than repeating because the two reasons a keepalive
		// goes unhonoured have opposite time constants: the benign one resolves
		// in milliseconds, and the other one — a shim binary that predates this
		// contract, which is every shim already running the first time this
		// change is rolled out — never resolves at all. Repeating the fast
		// probe held the loop at fifty exchanges a second for a whole window
		// against exactly those shims, each one taking the shim's handshake
		// lock and freezing its harness's output sequence through a round trip.
		retry := min(sessionShimKeepaliveRetryInterval, interval)
		paced := true
		for {
			if d.shims.recoveryCtx.Err() != nil {
				return
			}
			if paced {
				// The predicate is consulted on PACED ticks only. It is the
				// composing layer's answer, which in a composed deployment is a
				// control-plane query; asking it on every fast probe is how a
				// millisecond-scale fallback comes to drive a network call at
				// the same rate. A fast probe cannot extend anything the paced
				// tick before it was not already allowed to extend.
				if !held(d.shims.recoveryCtx) {
					// The composing layer let this lineage go. Stop extending
					// at once: from here the shim's own deadline governs,
					// unextended, which is what keeps the §D8 inequality true.
					slog.Warn("session shim: the composing layer released the lineage; stopping the orphan keepalive",
						"session", id.String())
					return
				}
			}
			if d.extendSessionShimOrphanDeadline(registry, id, hello) {
				honoured = true
			}
			wait := interval
			if !honoured {
				// Nothing has been honoured yet in this window. The reason
				// the FIRST probe is fast is one specific, benign race: the
				// shim arms its orphan clock from its own serve-loop teardown,
				// so a daemon that re-dials promptly can arrive before there is
				// a deadline to extend, and that resolves in milliseconds.
				// Waiting a whole interval to discover it would spend most of a
				// short deadline on it.
				//
				// The escalation is NOT a promise about how soon the first
				// honoured extension lands in general. A shim that stays
				// unextendable pushes the schedule to the cap — at the shipped
				// defaults the probes run 20, 40, 80 … 20480 ms, about 41 s in
				// total, and from there one paced 30 s interval, so the worst
				// case before a first honoured extension is roughly 1m11s. That
				// is deliberate and safe: until an extension is honoured the
				// shim is on its own unextended deadline, which is the bound
				// the amendment falls back to anyway. What the escalation buys
				// is that a shim which will never answer costs no more than one
				// that answers every time.
				wait = retry
				retry = min(retry*2, interval)
			}
			// Once the escalation has reached the configured interval the loop
			// is paced again, and the predicate resumes with it.
			paced = wait >= interval
			select {
			case <-d.shimKeepaliveAfter(wait):
			case <-stop:
				return
			case <-d.shims.recoveryCtx.Done():
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// extendSessionShimOrphanDeadline sends ONE keepalive and records what the shim
// answered, reporting whether the clock was extended.
//
// The first failure of a window is logged at Warn, not Debug. A host running a
// shim binary that predates this contract answers every keepalive `malformed`,
// reaps at its plain orphan deadline mid-window, and the lineage is quarantined
// as if it had died — the daemon knows the obligation was never honoured, and
// an operator has no other way to find out.
func (d *Daemon) extendSessionShimOrphanDeadline(
	registry *sessionshim.Registry,
	id sessionshim.Identity,
	hello shimwire.Hello,
) bool {
	record, err := registry.Get(id)
	if err != nil {
		d.noteSessionShimKeepaliveRefused(id, "session shim: orphan keepalive found no discovery record", err)
		return false
	}
	ctx, cancel := context.WithTimeout(d.shims.recoveryCtx, sessionshim.DefaultKeepAliveTimeout)
	defer cancel()
	deadline, err := sessionshim.KeepAlive(ctx, record, sessionshim.KeepAliveOptions{
		ExpectedShimID:       hello.ShimID,
		ExpectedProcessEpoch: hello.ProcessEpoch,
	})
	if err != nil {
		d.noteSessionShimKeepaliveRefused(id, "session shim: orphan keepalive did not extend the deadline", err)
		return false
	}
	d.shims.mu.Lock()
	state := d.shims.keepalives[id]
	state.extensions++
	state.refusals = 0
	state.lastDeadlineUnixNano = deadline.UnixNano()
	d.shims.keepalives[id] = state
	d.shims.mu.Unlock()
	return true
}

// shimKeepaliveAfter is the keepalive's own wait. It is deliberately the REAL
// clock even when a fixture has injected one: the thing being paced against is
// the shim's orphan timer, which is a real timer in another process.
func (d *Daemon) shimKeepaliveAfter(wait time.Duration) <-chan time.Time {
	return time.After(wait)
}

// noteSessionShimKeepaliveRefused records one unhonoured keepalive and says so
// out loud the first time it happens in a window.
func (d *Daemon) noteSessionShimKeepaliveRefused(id sessionshim.Identity, msg string, err error) {
	d.shims.mu.Lock()
	state := d.shims.keepalives[id]
	state.refusals++
	first := state.refusals == 1
	extensions := state.extensions
	d.shims.keepalives[id] = state
	d.shims.mu.Unlock()
	if first {
		slog.Warn(msg, "session", id.String(), "extensionsThisWindow", extensions, "error", err)
		return
	}
	slog.Debug(msg, "session", id.String(), "error", err)
}

// sessionShimKeepaliveObservations reports what the current window's keepalive
// achieved for a lineage: honoured extensions, consecutive refusals, and the
// deadline the last honoured one re-armed to.
func (d *Daemon) sessionShimKeepaliveObservations(id sessionshim.Identity) sessionShimKeepaliveState {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.keepalives[id]
}

// sessionShimIncarnationStillLive answers whether the exact incarnation still
// has a live discovery record and no terminal tombstone.
func sessionShimIncarnationStillLive(registry *sessionshim.Registry, id sessionshim.Identity, shimID string, processEpoch uint64) bool {
	if _, err := registry.GetTombstoneIncarnation(id, shimID, processEpoch); err == nil {
		return false
	}
	live, err := registry.HasIncarnation(id, shimID, processEpoch)
	return err == nil && live
}

// readoptSessionShimOnce is one bounded attempt: the startup adoption pass
// filtered to this identity, then the dynamic publication the launch path runs
// after its own dial.
//
// The attempt runs under the policy's AttemptTimeout, not the dynamic
// publication timeout the launch path uses: the shim's orphan clock is already
// running and an attempt sized for a launch would spend the caller's whole
// bound on one dial.
//
// That bound is the ONLY real timer in the re-adoption path. Everything about
// the window — when it started, what remains, whether another attempt fits —
// is arithmetic over the daemon's own clock in the loop above, so an injected
// clock moves the window without moving this timeout and neither can be
// measured against the other by accident.
// readoptedAtUnixNano is the instant the installed entry records as its last
// AUTOMATIC re-adoption. The window passes "now"; an operator-driven rebind
// passes the lost entry's own value, so a repair does not spend the automatic
// budget — see RebindAdoptedSessionShim.
func (d *Daemon) readoptSessionShimOnce(
	ctx context.Context,
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	lost adoptedShim,
	lostHello shimwire.Hello,
	readoptedAtUnixNano int64,
) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.readoptionAttemptTimeout())
	defer cancel()
	opts, preparations, err := d.sessionShimAdoptOptions(registry, cfg)
	if err != nil {
		return err
	}
	opts.Filter = func(candidate sessionshim.Identity) bool { return candidate == id }
	result, err := sessionshim.Adopt(ctx, opts)
	if err != nil {
		return fmt.Errorf("session shim: re-adopt: %w", err)
	}
	if len(result.Adopted) != 1 {
		result.Close()
		if len(result.Quarantined) > 0 {
			q := result.Quarantined[0]
			return fmt.Errorf("session shim: re-adoption classified the shim %s: %s", q.Reason, q.Detail)
		}
		return errors.New("session shim: re-adoption found no adoptable record")
	}
	ctrl := result.Adopted[0]
	if got := ctrl.Hello(); got.ShimID != lostHello.ShimID || got.ProcessEpoch != lostHello.ProcessEpoch {
		_ = ctrl.Close()
		return fmt.Errorf("session shim: a different incarnation answered the re-adoption (shim %q epoch %d, lost %q epoch %d)",
			got.ShimID, got.ProcessEpoch, lostHello.ShimID, lostHello.ProcessEpoch)
	}
	preparation := preparations.prepared[id]
	// The consumer and its carrier-installed latch start together (see
	// startGatedAdoptionConsumer): staging the mandatory Snapshot inside
	// OnAdoption releases a consumer parked on a durable output batch ahead
	// of the Snapshot, while the gate itself still carries the adoption
	// verdict. On evidence failure the controller close below releases the
	// parked consumer through the stream ending.
	gate, evidence, err := d.startGatedAdoptionConsumer(ctx, ctrl, preparation, preparations.hosts[id])
	if err != nil {
		_ = ctrl.Close()
		return fmt.Errorf("session shim: resolve re-adoption host: %w", err)
	}
	// Everything below is the launch path's publication, on the same barrier,
	// checkpoint, and heartbeat-barrier discipline — see launchSessionShim for
	// why each step is where it is.
	serializedPublication := cfg.OnAdoptionPublished != nil
	publicationSucceeded := !serializedPublication
	publicationCommitted := false
	ringPostActivationHeartbeat := false
	ringCorrectingHeartbeat := false
	defer func() {
		if ringPostActivationHeartbeat {
			activated := d.sessionShimActivatedScopes()
			d.ringSessionShimPostActivationHeartbeat(ctx)
			d.notifySessionShimAdoptionActivated(ctx, activated)
			return
		}
		if ringCorrectingHeartbeat {
			d.ringSessionShimHeartbeatDetached()
		}
	}()
	if serializedPublication {
		d.shims.publicationMu.Lock()
		if d.shims.dynamicPublicationFailed {
			d.shims.publicationMu.Unlock()
			evidence.SnapshotProxy.deactivate()
			gate.finish(false)
			_ = ctrl.Close()
			return errors.New("session shim: a prior dynamic adoption publication failed")
		}
		checkpoint := d.checkpointSessionShimPublication(id.OrgID)
		defer func() {
			if !publicationSucceeded {
				if publicationCommitted {
					d.shims.dynamicPublicationFailed = true
					d.restoreSessionShimHeartbeatLane(checkpoint)
				} else {
					d.rollbackSessionShimPublication(checkpoint)
				}
				ringCorrectingHeartbeat = true
			}
			d.shims.publicationMu.Unlock()
		}()
	}
	heartbeatBarrier := cfg.OnAdoptionPublished != nil && cfg.OnCarrierActivationAcknowledged != nil &&
		d.sessionShimEnabled()
	if cfg.OnAdoptionPublished != nil {
		if heartbeatBarrier {
			d.beginSessionShimRecoveryHeartbeatBarrier()
		} else {
			d.setState(StateRecovering)
		}
		d.shims.mu.Lock()
		d.shims.carrierActivationComplete = false
		d.shims.mu.Unlock()
	}
	receipt, err := d.completeSessionShimAdoption(ctx, evidence, preparation)
	evidence.SnapshotProxy.deactivate()
	if err != nil {
		d.cancelStagedSessionShimSnapshot(id)
		gate.finish(false)
		_ = ctrl.Close()
		return fmt.Errorf("session shim: durable re-adoption: %w", err)
	}
	evidence.SnapshotProxy = nil
	readopted := adoptedShim{
		controller:          ctrl,
		shimID:              ctrl.Hello().ShimID,
		handle:              lost.handle,
		spec:                lost.spec,
		launched:            lost.launched,
		adoption:            evidence,
		adoptionReceipt:     cloneSessionShimAdoptionReceipt(receipt),
		consumedRecovery:    newSessionShimConsumedRecovery(preparation, receipt),
		readoptedAtUnixNano: readoptedAtUnixNano,
		// The re-adoption ran the whole publication pipeline, so the carrier
		// binding holds again. The loss instant is carried forward: it is the
		// answer to "when was this lineage last unbound", and a re-adoption is
		// not a reason to forget it.
		carrierBound:           true,
		carrierBoundAtUnixNano: d.shimNow().UnixNano(),
		carrierLostAtUnixNano:  lost.carrierLostAtUnixNano,
	}
	if !d.installReadoptedSessionShim(id, lost.controller, readopted) {
		d.cancelStagedSessionShimSnapshot(id)
		gate.finish(false)
		_ = ctrl.Close()
		return errors.New("session shim: the lost controller is no longer the adopted entry")
	}
	batchReceipt, err := d.completeSessionShimAdoptionBatch(ctx, d.sessionShimProjectionBatch(id.OrgID, evidence.HostID))
	if err != nil {
		// Nothing durable advanced that this daemon can prove. Put the lost
		// entry back so the projection presents exactly what the receiver
		// holds — the caller's quarantine path builds its row from it — and
		// let an OUTCOME-UNKNOWN answer reach the reconciliation pass that
		// learns the committed revision through the refresher.
		d.restoreLostSessionShim(id, ctrl, lost)
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		if errors.Is(err, errSessionShimAmbiguousBatchCommit) {
			d.scheduleSessionShimReconciliation(id.OrgID, sessionShimReconcileCauseAmbiguous)
		}
		return fmt.Errorf("session shim: re-adoption batch: %w", err)
	}
	publicationCommitted = true
	if err := d.updateSessionShimAdoptionRevision(id.OrgID, batchReceipt.AdoptionRevision, heartbeatBarrier); err != nil {
		d.failPendingSessionShimActivations()
		gate.finish(false)
		_ = ctrl.Close()
		return fmt.Errorf("session shim: retain re-adoption revision: %w", err)
	}
	d.shims.mu.Lock()
	if len(batchReceipt.DurableCorrelation) > 0 {
		d.shims.batchReceipts[id.OrgID] = batchReceipt
	}
	d.shims.mu.Unlock()
	gate.finish(true)
	if cfg.OnAdoptionPublished != nil {
		published := map[sessionshim.Identity]adoptedShim{id: readopted}
		if activationErr := d.activatePublishedSessionShimCarriers(ctx, published); activationErr != nil {
			// The batch is committed and the shim is adopted; only the carrier
			// did not activate. Same disposition as the launch path: keep the
			// claim and the visible capacity, withhold further claims.
			slog.Error("session shim: post-publication carrier activation failed after re-adoption",
				"session", id.String(), "error", activationErr)
			d.failPendingSessionShimActivations()
			_ = ctrl.Close()
			// This is the exact state RebindAdoptedSessionShim exists to
			// repair: adopted, live, and silent because the carrier binding
			// never completed. Marking it bound here — which the entry above
			// does by default, because every other path that installs one IS
			// bound — is what would make that state undetectable.
			if d.noteSessionShimCarrierBindLost(id, ctrl) {
				d.raiseSessionShimCarrierBindLost(cfg, id)
			}
			return nil
		}
		if !heartbeatBarrier {
			d.setState(StateRunning)
		}
	}
	publicationSucceeded = true
	ringPostActivationHeartbeat = heartbeatBarrier
	if !heartbeatBarrier {
		// No barrier was raised, but the batch still advanced the receiver's
		// revision and demoted its readiness until a beat re-attests it.
		d.ringSessionShimHeartbeatDetached()
	}
	return nil
}

// installReadoptedSessionShim swaps the re-adopted controller in for the lost
// one, under the lock, only while the lost one is still the adopted entry and
// the daemon has not released its shims. The correlation for the incarnation
// is replaced — same shim id and process epoch, new generation and receipt —
// and the durable forwarded cursor is seeded from the new controller's resume
// point exactly as the startup pass seeds it.
func (d *Daemon) installReadoptedSessionShim(id sessionshim.Identity, lost *sessionshim.Controller, entry adoptedShim) bool {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if d.shims.reconcileStopped {
		return false
	}
	current, ok := d.shims.adopted[id]
	if !ok || current.controller != lost {
		return false
	}
	d.shims.adopted[id] = entry
	d.shims.correlations[shimIncarnationFor(entry.adoption)] = sessionShimAdoptionCorrelation{
		evidence: entry.adoption,
		receipt:  cloneSessionShimAdoptionReceipt(entry.adoptionReceipt),
	}
	if resumeFrom := entry.controller.ResumeFrom(); resumeFrom > 0 {
		if durableBeforeAdoption := resumeFrom - 1; durableBeforeAdoption > d.shims.forwarded[id] {
			d.shims.forwarded[id] = durableBeforeAdoption
		}
	}
	return true
}

// restoreLostSessionShim puts the lost entry back after a re-adoption whose
// batch did not commit, so the projection presents what the receiver holds.
func (d *Daemon) restoreLostSessionShim(id sessionshim.Identity, readopted *sessionshim.Controller, lost adoptedShim) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	current, ok := d.shims.adopted[id]
	if !ok || current.controller != readopted {
		return
	}
	d.shims.adopted[id] = lost
	d.shims.correlations[shimIncarnationFor(lost.adoption)] = sessionShimAdoptionCorrelation{
		evidence: lost.adoption,
		receipt:  cloneSessionShimAdoptionReceipt(lost.adoptionReceipt),
	}
}
