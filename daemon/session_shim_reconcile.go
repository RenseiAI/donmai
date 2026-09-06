package daemon

// session_shim_reconcile.go — resolving an adoption-batch commit whose outcome
// this daemon never learned.
//
// THE STRAND THIS UNDOES
//
// A batch commit is one round trip to the control plane, and its failure modes
// are not one thing. A decoded refusal (a 4xx with a closed code) means the
// control plane did NOT commit: rolling back to the last-committed projection
// and beating it is exactly right, and stays exactly what happens. But a
// transport error, a deadline, or a 5xx arrives AFTER the request went out —
// the control plane may have stamped the batch and advanced the host's
// adoption revision while this daemon's copy of the answer was lost to the
// same network flake that provoked the retry. Treating that ambiguous outcome
// like a refusal latches the daemon one revision behind a control plane that
// moved: every subsequent beat presents the superseded revision, is answered
// with the closed revision-stale conflict, and nothing on the host ever
// learns the revision the server actually holds. Measured end state: a beat
// loop refused forever, and every clear/republish retry refused with it,
// because the composer's compare-and-swap still carried the stale expected
// revision.
//
// THE RECONCILIATION
//
// An ambiguous commit (and a beat answered revision-stale, which is the same
// divergence observed from the other side) schedules a bounded reconciliation
// for the scope instead of latching a terminal failure:
//
//  1. Learn the control plane's current revision through the ONE
//     CredentialRefresher — never a bare re-presentation, which would race the
//     running lanes off the refresher's lock. Every refresh receipt carries
//     AdoptionRevision, and validateAndRetainSessionShimRefreshReceipt already
//     advances the retained scope authority from it.
//  2. Republish the COMPLETE current batch through the existing publish path.
//     The complete-snapshot rule makes the re-derived batch well-defined; a
//     staged cleared entry rides the republished batch's cleared section and
//     still drops only on a confirmed exact echo. The composer re-resolves its
//     expected revision from SessionShimScopeAuthority.
//  3. Until that republish confirms, the heartbeat keeps presenting the
//     last-committed projection. A reconciliation republish IS a commit
//     attempt through the same choke point as every other batch — never an
//     announcement of a set that was not committed.
//
// The loop is bounded, and the bounds are derived rather than chosen — see
// runSessionShimReconciliation. A control plane that refuses every republish
// exhausts the bound and leaves the daemon serving and beating the
// last-committed projection; the next revision-stale beat may arm one more
// bounded pass, paced by the heartbeat interval.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// ErrSessionShimCommitOutcomeUnknown marks an adoption-batch commit failure
// whose outcome is unknown: the commit request was sent and the failure came
// back without a decoded refusal — a transport error, a deadline, or a 5xx —
// so the control plane MAY have committed the batch. A composing
// OnAdoptionBatch wraps exactly those failures with this sentinel
// (fmt.Errorf("…: %w", ErrSessionShimCommitOutcomeUnknown) or an equivalent
// chain); a decoded 4xx refusal with a closed code is returned without it and
// keeps refusal semantics. Context deadline/cancellation and net.Error values
// are classified outcome-unknown even without the sentinel.
var ErrSessionShimCommitOutcomeUnknown = errors.New("session shim: batch commit outcome unknown")

// errSessionShimAmbiguousBatchCommit is the internal classification wrap the
// commit choke point applies to an outcome-unknown OnAdoptionBatch failure, so
// callers can distinguish the ambiguous commit stage from a preparation or
// receipt-validation failure without re-deriving the classification.
var errSessionShimAmbiguousBatchCommit = errors.New("session shim: adoption batch commit outcome unknown; the control plane may have committed")

// sessionShimReconciliationRefreshReason classifies the reconciliation-driven
// credential refresh in the [runtime-token] log line.
const sessionShimReconciliationRefreshReason = "session-shim-commit-reconciliation"

// sessionShimRevisionStaleCode is the control plane's closed conflict code for
// a heartbeat that presented a superseded session-shim adoption revision.
const sessionShimRevisionStaleCode = "SESSION_SHIM_ADOPTION_REVISION_STALE"

// The classified reconciliation triggers, for the armed/attempt log lines.
const (
	sessionShimReconcileCauseAmbiguous        = "ambiguous-batch-commit"
	sessionShimReconcileCauseAmbiguousLaunch  = "ambiguous-launch-batch-commit"
	sessionShimReconcileCauseRevisionAdvanced = "adoption-revision-advanced"
)

// sessionShimCommitOutcomeUnknown classifies one batch-commit error.
// OUTCOME-UNKNOWN — the request may have applied — is the explicit sentinel, a
// context deadline/cancellation (the bound expired around an in-flight
// request), or any net.Error in the chain (transport failures, including the
// *url.Error an HTTP round trip returns). Everything else is OUTCOME-REFUSED
// and keeps today's rollback behavior.
func sessionShimCommitOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSessionShimCommitOutcomeUnknown) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// SessionShimScopeAuthority returns a clone of the retained non-secret
// authority receipt for one served scope, and false when nothing is retained;
// the composer re-resolves its expected revision from this after a
// revision-stale refusal or an ambiguous commit.
func (d *Daemon) SessionShimScopeAuthority(scope string) (SessionShimScopeCredentialReceipt, bool) {
	if d == nil || d.shims == nil {
		return SessionShimScopeCredentialReceipt{}, false
	}
	d.shims.mu.RLock()
	receipt, ok := d.shims.credentialReceipts[scope]
	d.shims.mu.RUnlock()
	if !ok {
		return SessionShimScopeCredentialReceipt{}, false
	}
	return cloneSessionShimScopeCredentialReceipt(receipt), true
}

// sessionShimReconcileCauseEmbedder is the cause recorded when an embedder
// arms reconciliation through ScheduleSessionShimReconciliation without naming
// one of its own.
const sessionShimReconcileCauseEmbedder = "embedder-requested"

// ScheduleSessionShimReconciliation arms the bounded reconcile-and-republish
// pass for one served scope: one credential refresh to learn the control
// plane's committed adoption revision, then one complete-batch republish at
// it, bounded and paced exactly as the pass the daemon's own heartbeat lane
// arms on a revision-stale refusal (HeartbeatOptions.OnSessionShimRevisionStale).
//
// It exists for the heartbeat lanes the daemon does NOT own. A composition
// that serves several scopes runs one beat lane per scope, and a beat on one
// of those lanes answered with the closed revision-stale conflict has, without
// this, no way to reach the pass that repairs it — the host then beats the
// same superseded revision or quarantine set every interval, refused each
// time, with the row demoted until a restart. The embedder's lane calls this
// from its own revision-stale hook with the scope the beat was for.
//
// Safe from any goroutine and at heartbeat cadence: at most one pass runs per
// scope at a time and a call landing while one runs is served by it. A scope
// this daemon holds no authority receipt for is refused with a warning rather
// than armed — a pass for it could learn nothing and republish nothing. An
// empty cause is recorded as embedder-requested.
func (d *Daemon) ScheduleSessionShimReconciliation(scope, cause string) {
	if d == nil || d.shims == nil {
		return
	}
	if cause == "" {
		cause = sessionShimReconcileCauseEmbedder
	}
	if _, known := d.SessionShimScopeAuthority(scope); !known {
		slog.Warn("session shim: reconciliation requested for a scope this daemon holds no authority receipt for; ignoring",
			"scope", scope, "cause", cause)
		return
	}
	d.scheduleSessionShimReconciliation(scope, cause)
}

// scheduleSessionShimReconciliation arms one bounded reconciliation pass for a
// scope whose committed revision this daemon can no longer prove it holds —
// after an ambiguous batch commit, or after a beat was answered with the
// closed revision-stale conflict. At most one pass runs per scope at a time; a
// trigger landing while one runs is already served by it.
func (d *Daemon) scheduleSessionShimReconciliation(scope, cause string) {
	if d == nil || d.shims == nil || scope == "" || !d.sessionShimEnabled() ||
		d.sessionShimConfig().OnAdoptionBatch == nil {
		return
	}
	d.shims.mu.Lock()
	if d.shims.reconcileStopped || d.shims.reconciling[scope] {
		d.shims.mu.Unlock()
		return
	}
	d.shims.reconciling[scope] = true
	d.shims.wg.Add(1)
	d.shims.mu.Unlock()
	slog.Warn("session shim: commit-outcome reconciliation armed (shim-commit-reconciliation-2026-08-27)",
		"scope", scope, "cause", cause)
	go d.runSessionShimReconciliation(scope, cause)
}

// runSessionShimReconciliation is the bounded loop. Its bounds are derived
// from the bounds this subsystem already has — never a fresh number:
//
//   - attempts = sessionShimAdoptionPublicationStages. The loop's whole budget
//     is ONE dynamic adoption publication (adoptionPublicationTimeout), and
//     each attempt holds at least one callbackTimeout for its commit stage, so
//     the attempt count is that quotient:
//     adoptionPublicationTimeout / callbackTimeout = the pipeline depth.
//   - per-attempt budget = adoptionPublicationTimeout(): an attempt re-runs
//     the same publication pipeline (prepare + commit) plus one credential
//     refresh, and the pipeline's own derived bound is the bound that already
//     covers a full publication.
//   - backoff between attempts = callbackTimeout(): the per-stage unit every
//     other bound here is expressed in.
//
// Exhaustion is not a crash and not silence: the daemon keeps serving and the
// heartbeat keeps presenting the last-committed projection; a later
// revision-stale beat may arm one more bounded pass at heartbeat cadence.
func (d *Daemon) runSessionShimReconciliation(scope, cause string) {
	defer d.shims.wg.Done()
	defer func() {
		d.shims.mu.Lock()
		delete(d.shims.reconciling, scope)
		d.shims.mu.Unlock()
	}()
	// A poll refusal must not synchronously wait on the readiness resolver:
	// ResolveNow intentionally bypasses the cache and can serialize behind a
	// slow resolver. Resolve on this bounded repair goroutine instead, before
	// its refresh and republish make the next heartbeat eligible to recover.
	_ = d.sessionShimReadinessGate(sessionShimReadinessResolveNow)
	// A pass that ends because the CONTROL PLANE ITSELF reported a revision it
	// has moved to is not an exhausted pass — it is a pass that learned a new
	// fact and has to be spent against it. Measured on an installed host: four
	// attempts all answered "the expected revision changed", the loop declared
	// exhaustion, and the daemon then served the superseded revision forever
	// with no trigger left to re-arm it. Re-arming on that specific answer is
	// what makes "serve a stale revision forever" unreachable; a control plane
	// that merely refuses keeps the original bounded behaviour exactly.
	//
	// Re-arming is unbounded ON PURPOSE — there is no number of attempts after
	// which serving a superseded revision becomes correct — so it is COUNTED
	// instead of bounded: the count escalates the log from warning to error
	// after a derived threshold, and it is published on the diagnostics surface
	// so an operator reading `status`/doctor sees a host stuck re-converging
	// rather than a host that is merely quiet.
	defer d.clearSessionShimReconvergence(scope)
	for rearms := 0; ; rearms++ {
		d.publishSessionShimReconvergence(scope, cause, rearms, "")
		lastErr := d.runBoundedSessionShimReconciliationPass(scope, cause)
		var advanced *SessionShimAdoptionRevisionAdvanced
		if !errors.As(lastErr, &advanced) || d.sessionShimReconcileStopped() {
			return
		}
		cause = sessionShimReconcileCauseRevisionAdvanced
		d.publishSessionShimReconvergence(scope, cause, rearms+1, advanced.Advanced)
		message := "session shim: reconciliation spent its derived bound against a control plane that keeps reporting a further " +
			"advanced adoption revision; re-arming rather than serving a superseded revision " +
			"(shim-adoption-reconvergence-2026-09-01)"
		if rearms+1 >= sessionShimReconvergenceEscalateAfter {
			// Past the derived threshold this is no longer a transient
			// disagreement: something is wrong that this daemon cannot fix on
			// its own, and a warning nobody pages on would hide it.
			slog.Error(message, "scope", scope, "advancedTo", advanced.Advanced,
				"rearms", rearms+1, "escalateAfter", sessionShimReconvergenceEscalateAfter)
		} else {
			slog.Warn(message, "scope", scope, "advancedTo", advanced.Advanced, "rearms", rearms+1)
		}
		// Paced by the same unit every other bound here is expressed in, so a
		// control plane stuck reporting an advance cannot be spun against.
		if !d.sleepSessionShimReconcileBackoff(d.sessionShimConfig().callbackTimeout()) {
			return
		}
	}
}

// sessionShimReconvergenceEscalateAfter is how many re-arms make a
// re-convergence an operator problem rather than a transient one. Derived, not
// chosen: one re-arm per stage of the publication pipeline means the daemon has
// spent a whole pipeline's worth of complete passes without converging.
const sessionShimReconvergenceEscalateAfter = sessionShimAdoptionPublicationStages

// publishSessionShimReconvergence records the current re-convergence condition
// for one scope on the diagnostics surface.
func (d *Daemon) publishSessionShimReconvergence(scope, cause string, rearms int, advancedTo string) {
	d.shims.mu.Lock()
	defer d.shims.mu.Unlock()
	if d.shims.reconverging == nil {
		d.shims.reconverging = make(map[string]sessionShimReconvergenceState)
	}
	state := d.shims.reconverging[scope]
	state.cause = cause
	state.rearms = rearms
	if advancedTo != "" {
		state.advancedTo = advancedTo
	}
	d.shims.reconverging[scope] = state
}

func (d *Daemon) clearSessionShimReconvergence(scope string) {
	d.shims.mu.Lock()
	delete(d.shims.reconverging, scope)
	d.shims.mu.Unlock()
}

// sessionShimReconvergenceState is one scope's live re-convergence condition.
type sessionShimReconvergenceState struct {
	cause      string
	rearms     int
	advancedTo string
}

// runBoundedSessionShimReconciliationPass is ONE derived-bound pass. It returns
// nil once a republish confirms (or the daemon released its shims), and
// otherwise the last attempt's failure, which is what the caller classifies.
func (d *Daemon) runBoundedSessionShimReconciliationPass(scope, cause string) error {
	cfg := d.sessionShimConfig()
	attempts := sessionShimAdoptionPublicationStages
	backoff := cfg.callbackTimeout()
	budget := cfg.adoptionPublicationTimeout()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && !d.sleepSessionShimReconcileBackoff(backoff) {
			return nil
		}
		if d.sessionShimReconcileStopped() {
			return nil
		}
		err := d.reconcileSessionShimScope(scope, budget)
		if err == nil {
			// The republish confirmed and the retained revision advanced. The
			// republish itself rang the immediate beat that re-attests it
			// (republishSessionShimProjection), so nothing waits out a
			// heartbeat interval here.
			slog.Info("session shim: reconciliation republished the complete batch at the control plane's committed revision",
				"scope", scope, "cause", cause, "attempt", attempt)
			return nil
		}
		lastErr = err
		if d.sessionShimReconcileStopped() {
			return nil
		}
		slog.Warn("session shim: commit-outcome reconciliation attempt failed",
			"scope", scope, "cause", cause, "attempt", attempt, "attempts", attempts, "error", err)
	}
	slog.Warn("session shim: commit-outcome reconciliation exhausted its derived bound; serving and beating the last-committed projection",
		"scope", scope, "cause", cause, "attempts", attempts)
	return lastErr
}

// reconcileSessionShimScope is one attempt: one refresh through the ONE
// credential refresher to learn the control plane's current revision, then one
// complete-batch republish through the existing publish path. The republish is
// a commit attempt — the retained revision advances only from server-issued
// receipts (the refresh receipt, then the confirmed batch receipt), so no beat
// ever announces a set or revision the control plane did not issue.
func (d *Daemon) reconcileSessionShimScope(scope string, budget time.Duration) error {
	if !d.sessionShimEnabled() {
		return errors.New("session shim: reconciliation requires a composed attestation")
	}
	d.mu.RLock()
	credentials := d.credentials
	d.mu.RUnlock()
	if credentials == nil {
		return errors.New("session shim: reconciliation requires a registered daemon")
	}
	stopCtx, cancelStop := context.WithCancel(context.Background())
	defer cancelStop()
	go func() {
		select {
		case <-d.shims.reconcileStop:
			cancelStop()
		case <-stopCtx.Done():
		}
	}()
	ctx, cancel := context.WithTimeout(stopCtx, budget)
	defer cancel()
	if _, err := credentials.Refresh(ctx, sessionShimReconciliationRefreshReason); err != nil {
		return fmt.Errorf("session shim: reconciliation refresh: %w", err)
	}
	// Serialize with dynamic adoption publications: the republish reads the
	// complete current projection and commits it, and interleaving that with a
	// launch's own publication would let each validate a transient set.
	d.shims.publicationMu.Lock()
	defer d.shims.publicationMu.Unlock()
	return d.republishSessionShimProjection(ctx, scope)
}

// sleepSessionShimReconcileBackoff waits one derived backoff unit, or returns
// false when the daemon released its shims first.
// It stays on the REAL clock. Its other call sites — the reconcile passes at
// :253 and :305 and the acceptance-clear poll in session_shim_spawn.go — pair it
// with `time.Now()` deadlines, so routing it through the injectable clock would
// turn those loops into hot spins the moment any fixture injected one. The
// re-adoption window has its own wait (sleepSessionShimWindowBackoff) precisely
// so one clock governs one loop.
func (d *Daemon) sleepSessionShimReconcileBackoff(backoff time.Duration) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-d.shims.reconcileStop:
		return false
	}
}

func (d *Daemon) sessionShimReconcileStopped() bool {
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	return d.shims.reconcileStopped
}
