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
	cfg := d.sessionShimConfig()
	attempts := sessionShimAdoptionPublicationStages
	backoff := cfg.callbackTimeout()
	budget := cfg.adoptionPublicationTimeout()
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && !d.sleepSessionShimReconcileBackoff(backoff) {
			return
		}
		if d.sessionShimReconcileStopped() {
			return
		}
		err := d.reconcileSessionShimScope(scope, budget)
		if err == nil {
			slog.Info("session shim: reconciliation republished the complete batch at the control plane's committed revision",
				"scope", scope, "cause", cause, "attempt", attempt)
			// The republish confirmed and the retained revision advanced; ring
			// one immediate beat so the re-attestation does not wait out a
			// heartbeat interval. One fresh callback-sized bound, mirroring
			// the correcting beat on the dynamic launch path.
			beatCtx, cancelBeat := context.WithTimeout(context.Background(), cfg.callbackTimeout())
			d.ringSessionShimPostActivationHeartbeat(beatCtx)
			cancelBeat()
			return
		}
		if d.sessionShimReconcileStopped() {
			return
		}
		slog.Warn("session shim: commit-outcome reconciliation attempt failed",
			"scope", scope, "cause", cause, "attempt", attempt, "attempts", attempts, "error", err)
	}
	slog.Warn("session shim: commit-outcome reconciliation exhausted its derived bound; serving and beating the last-committed projection",
		"scope", scope, "cause", cause, "attempts", attempts)
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
