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
	if !platformPersistLoss && lost.readoptedAtUnixNano != 0 {
		if since := d.shimNow().Sub(time.Unix(0, lost.readoptedAtUnixNano)); since < window {
			// Re-adopted inside the window and lost again: this carrier is not
			// one a bounded retry can restore, and every further cycle costs an
			// adoption revision the receiver has to re-attest. The bound is the
			// window that GOVERNED the previous re-adoption, which in
			// lineage-live mode is ten minutes, not the fixed-mode arithmetic.
			slog.Warn("session shim: controller lost again inside the re-adoption window; quarantining rather than re-adopting",
				"session", id.String(), "sinceReadoption", since, "window", window, "mode", policy.Mode)
			return readoptionRefused
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
		return d.readoptSessionShimWithinLivenessWindow(registry, cfg, policy, id, lost, hello)
	}
	return d.readoptSessionShimWithinFixedAttempts(registry, cfg, policy, id, lost, hello)
}

// readoptSessionShimWithinFixedAttempts is the 2026-09-02 bound, unchanged: a
// fixed attempt count whose worst case ends strictly inside the shim's orphan
// deadline, with no keepalive because none is needed.
func (d *Daemon) readoptSessionShimWithinFixedAttempts(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	policy SessionShimReadoptionPolicy,
	id sessionshim.Identity,
	lost adoptedShim,
	hello shimwire.Hello,
) sessionShimReadoptionDisposition {
	backoff := policy.Backoff
	for attempt := 1; attempt <= policy.Attempts; attempt++ {
		if attempt > 1 {
			if !d.sleepSessionShimReconcileBackoff(backoff) {
				return readoptionRefused
			}
			backoff *= 2
		}
		disposition, done := d.readoptSessionShimAttempt(registry, cfg, id, lost, hello, attempt, policy.Attempts)
		if done {
			return disposition
		}
	}
	return readoptionAttemptsSpent
}

// readoptSessionShimWithinLivenessWindow is the lineage-live bound: retry with
// exponential backoff capped at BackoffCap for Window, for exactly as long as
// this daemon can still observe the shim alive and holding the lineage.
//
// The keepalive runs for the whole window and stops with it. That ordering is
// the safety property: the moment this function returns — success, exhaustion,
// or a lineage that vanished — nothing is extending the shim's clock any more,
// so a shim this daemon has stopped working on is back on its ordinary orphan
// deadline without anyone having to remember to put it there.
func (d *Daemon) readoptSessionShimWithinLivenessWindow(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	policy SessionShimReadoptionPolicy,
	id sessionshim.Identity,
	lost adoptedShim,
	hello shimwire.Hello,
) sessionShimReadoptionDisposition {
	deadline := d.shimNow().Add(policy.Window)
	stopKeepalive := d.startSessionShimOrphanKeepalive(registry, cfg, id, hello)
	defer stopKeepalive()
	backoff := policy.Backoff
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			remaining := deadline.Sub(d.shimNow())
			if remaining <= 0 || backoff >= remaining {
				// Either the window is over, or the next attempt could not
				// start inside it. Both are the window ending, and both must
				// reach the same outcome — the exit that breaks one backoff
				// early is the DOMINANT one, and gating the outcome on a clock
				// comparison instead of on this fact is how the exhaustion
				// outcome came to be unreachable in the common case.
				return d.sessionShimWindowExhausted(registry, cfg, id, hello, deadline)
			}
			if !d.sleepSessionShimWindowBackoff(backoff) {
				return readoptionRefused
			}
			if backoff *= 2; backoff > policy.BackoffCap {
				backoff = policy.BackoffCap
			}
		}
		if !d.shimNow().Before(deadline) {
			return d.sessionShimWindowExhausted(registry, cfg, id, hello, deadline)
		}
		disposition, done := d.readoptSessionShimAttempt(registry, cfg, id, lost, hello, attempt, 0)
		if done {
			return disposition
		}
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
	if d.sessionShimReconcileStopped() {
		return readoptionRefused, true
	}
	if !sessionShimIncarnationStillLive(registry, id, hello.ShimID, hello.ProcessEpoch) {
		// The record is gone or the shim already left its proof on disk:
		// there is nothing to re-adopt, and the caller's path consumes the
		// tombstone before it publishes anything.
		return readoptionLineageGone, true
	}
	if !d.sessionShimLineageHeld(cfg, id) {
		// The composing layer no longer holds this lineage. Retrying would
		// re-adopt something nobody upstream wants, and every further keepalive
		// would extend the clock of a harness that should be reaped on the
		// ordinary deadline.
		slog.Warn("session shim: the composing layer no longer holds this lineage; stopping re-adoption",
			"session", id.String(), "attempt", attempt)
		return readoptionLineageGone, true
	}
	err := d.readoptSessionShimOnce(context.Background(), registry, cfg, id, lost, hello, d.shimNow().UnixNano())
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
	slog.Warn("session shim: re-adoption attempt after controller loss failed",
		"session", id.String(), "attempt", attempt, "attempts", attempts, "error", err)
	return readoptionRefused, false
}

// sessionShimLineageHeld asks the composing layer whether it still holds this
// lineage. A nil predicate means yes: a standalone daemon has nothing above it
// to ask, and the discovery record it checked already is then the whole
// observation.
func (d *Daemon) sessionShimLineageHeld(cfg SessionShimConfig, id sessionshim.Identity) bool {
	if cfg.LineageLive == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.callbackTimeout())
	defer cancel()
	return cfg.LineageLive(ctx, id)
}

// sessionShimWindowExhausted classifies the end of a lineage-live window and,
// when the shim is still observable, raises the one notification that
// distinguishes it from a shim that died.
//
// The notification runs BEFORE the caller withdraws the lineage, and the
// withdrawal that follows is unconditional
// (ADR-2026-09-03-readoption-exhaustion-withdraws, amending rule 8).
//
// It is the ONLY producer of readoptionWindowExhausted, which is what makes
// "fires exactly once per exhausted window" a property of the code shape rather
// than of a flag every exit has to remember to set.
func (d *Daemon) sessionShimWindowExhausted(
	registry *sessionshim.Registry,
	cfg SessionShimConfig,
	id sessionshim.Identity,
	hello shimwire.Hello,
	deadline time.Time,
) sessionShimReadoptionDisposition {
	if !sessionShimIncarnationStillLive(registry, id, hello.ShimID, hello.ProcessEpoch) {
		return readoptionLineageGone
	}
	slog.Warn("session shim: the re-adoption window ended with the shim still observable",
		"session", id.String(), "deadline", deadline)
	if cfg.OnReadoptionWindowExhausted != nil {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.callbackTimeout())
		cfg.OnReadoptionWindowExhausted(ctx, id)
		cancel()
	}
	return readoptionWindowExhausted
}

// sleepSessionShimWindowBackoff waits one re-adoption backoff, or returns false
// when the daemon released its shims first. It is the ONE loop the injectable
// session-shim clock governs.
func (d *Daemon) sleepSessionShimWindowBackoff(backoff time.Duration) bool {
	select {
	case <-d.shimAfter(backoff):
		return true
	case <-d.shims.reconcileStop:
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
			if paced {
				// The predicate is consulted on PACED ticks only. It is the
				// composing layer's answer, which in a composed deployment is a
				// control-plane query; asking it on every fast probe is how a
				// millisecond-scale fallback comes to drive a network call at
				// the same rate. A fast probe cannot extend anything the paced
				// tick before it was not already allowed to extend.
				if !d.sessionShimLineageHeld(cfg, id) {
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
			case <-d.shims.reconcileStop:
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
	ctx, cancel := context.WithTimeout(context.Background(), sessionshim.DefaultKeepAliveTimeout)
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
	evidence, err := d.sessionShimAdoptionEvidence(ctx, ctrl, preparation, preparations.hosts[id])
	if err != nil {
		_ = ctrl.Close()
		return fmt.Errorf("session shim: resolve re-adoption host: %w", err)
	}
	gate := newShimAdoptionGate()
	d.consumeShimEventsGated(ctrl, gate)
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
