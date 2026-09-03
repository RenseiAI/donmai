package daemon

// Provenance: fresh-dial-boundary-precondition-2026-09-03 — grep a build for
// this marker to prove its startup composition pass re-prepares a drifted
// carrier proof before it quarantines the lineage that holds it.
//
// THE STRAND THIS UNDOES
//
// adoptSessionShims asked each lineage's durable-adoption callback once. A
// single refusal became QuarantineAdoptionFailed with no second attempt — and a
// quarantined lineage keeps its harness but gains no controller, so nothing
// renews its orphan clock and it tears itself down at the shim's deadline. The
// pass's own comment named the symptom and named the follow-up; this is it.
//
// The recovery corpus is explicit that a conflicting projection is AMBIGUOUS
// evidence, whose only permitted consequences are preserve, recheck, retry, and
// then degrade visibly — never a terminal outcome. A stale carrier proof is
// exactly that: the proof this dial holds has been overtaken, the repair is a
// new proof at a strictly greater carrier epoch, and the harness on the other
// side of it was alive the entire time.
//
// WHAT THIS RELIES ON THE COMPOSING AUTHORITY TO DO
//
// A re-prepare supersedes a reservation that is ADMITTED and pre-active. This
// side has no verb to burn it: abandonment is control-authenticated and belongs
// to the authority that minted it. So the contract this code depends on, stated
// plainly rather than assumed:
//
//   - a re-prepare carries SessionShimPrepareCauseCarrierCursorDrift and its
//     attempt number, which is this daemon declaring that an earlier
//     reservation for this lineage is outstanding and stale;
//   - the authority abandons that reservation before reserving above the
//     all-time carrier-epoch floor. Under the recovery ADR's §D4 disposition
//     rule, changed bytes under the same idempotency key are a re-prepare, not
//     a replay, and every attempt that created or inherited an uncommitted
//     reservation ends with one durable disposition;
//   - an authority that will NOT supersede answers a typed conflict
//     (ErrSessionShimAdoptionPrepareConflict). That is a refusal, not a
//     failure: this pass spends none of its remaining budget on it and
//     quarantines with the conflict as the detail.
//
// A daemon that asked without saying why would leave the authority unable to
// tell a re-prepare from a first ask, which is how an admitted candidate
// becomes an undisposed one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

const (
	// sessionShimDriftRedialAttempts is the TOTAL number of durable-adoption
	// attempts one lineage gets when each refusal is classified as carrier
	// cursor drift — the first, plus two re-prepared re-dials. It is a hard
	// bound because the startup pass composes every lineage on this host before
	// the daemon may advertise capacity: an unbounded retry here would trade one
	// condemned lineage for a host that never becomes ready.
	sessionShimDriftRedialAttempts = 3
	// sessionShimDriftRedialBackoff is the base delay before the second attempt;
	// it doubles for each attempt after that. Drift is repaired by a fresh
	// reservation against the control plane, so the delay exists to let the
	// authority that mints it settle, not to wait out a local condition.
	sessionShimDriftRedialBackoff = 200 * time.Millisecond
)

// isSessionShimCarrierCursorDrift reports whether a durable-adoption refusal is
// the one shape the pass may retry: the local acknowledgement floor and the
// signed carrier boundary disagree in the direction only a stale proof can
// produce. Composing callers wrap the attach client's typed error with %w, so
// this reads through their wrapping and matches nothing else. Every other
// refusal keeps its existing single-attempt disposition.
func isSessionShimCarrierCursorDrift(err error) bool {
	return errors.Is(err, attachclient.ErrV2CarrierCursorDrift)
}

// completeSessionShimAdoptionWithDriftRedial runs the durable-adoption callback
// and, while it refuses with carrier cursor drift, re-prepares the lineage's
// carrier proof and dials again inside the bound above.
//
// It returns the evidence and preparation the FINAL attempt used: a re-prepare
// mints a new proof and a new Snapshot authority, so the caller must retain the
// pair the receipt actually belongs to. On exhaustion it returns the last
// refusal, which the caller quarantines — visibly degraded, after the bound,
// never on the first ambiguous answer.
func (d *Daemon) completeSessionShimAdoptionWithDriftRedial(
	ctx context.Context,
	ctrl *sessionshim.Controller,
	preparations *sessionShimAdoptionPreparations,
	hostID string,
	evidence SessionShimAdoptionEvidence,
	preparation SessionShimAdoptionPreparationResult,
) (SessionShimAdoptionEvidence, SessionShimAdoptionPreparationResult, SessionShimAdoptionReceipt, error) {
	id := ctrl.Identity()
	receipt, err := d.completeSessionShimAdoption(ctx, evidence, preparation)
	for attempt := 2; attempt <= sessionShimDriftRedialAttempts; attempt++ {
		if err == nil || !isSessionShimCarrierCursorDrift(err) {
			return evidence, preparation, receipt, err
		}
		input, retained := preparations.inputs[id]
		if !retained {
			// No prepare hook ran for this lineage, so there is no proof this
			// daemon can re-mint. Say so in the detail rather than pretending a
			// re-dial with the same stale evidence would answer differently.
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; no retained adoption preparation to re-prepare against", err)
		}
		slog.Warn("session shim: durable adoption refused a drifted carrier proof; re-preparing before quarantine",
			"session", id.String(), "attempt", attempt, "of", sessionShimDriftRedialAttempts, "error", err)

		// Release what the refused attempt reserved. The Snapshot authority is
		// per-attempt and a fresh prepare stages its own; leaving either behind
		// makes the next attempt refuse on this daemon's own leftovers instead of
		// on anything the carrier said.
		evidence.SnapshotProxy.deactivate()
		d.cancelStagedSessionShimSnapshot(id)

		if waitErr := d.waitSessionShimDriftBackoff(ctx, attempt); waitErr != nil {
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; re-prepare %d abandoned: %v", err, attempt, waitErr)
		}

		// Re-ask with what is true NOW, not with the frozen copy: the controller
		// generation this daemon committed, and the highest resume floor either
		// side has proved. Both may only rise, so a re-prepare can never hand the
		// authority a cursor below the one it already admitted. The cause travels
		// with it — see SessionShimPrepareCauseCarrierCursorDrift for what the
		// authority is being told to do about its previous reservation.
		next := input
		next.CurrentControllerGeneration = ctrl.Generation()
		if resumeFrom := ctrl.ResumeFrom(); resumeFrom > next.LocalResumeFrom {
			next.LocalResumeFrom = resumeFrom
			next.LastForwardedSeq = resumeFrom - 1
		}
		prepared, prepareErr := d.prepareSessionShimAdoptionForCause(
			ctx, hostID, next, SessionShimPrepareCauseCarrierCursorDrift, attempt,
		)
		if prepareErr != nil {
			if errors.Is(prepareErr, ErrSessionShimAdoptionPrepareConflict) {
				// A refusal, not a failure. The authority holds state for this
				// lineage it will not supersede, so the remaining budget cannot
				// change the answer and spending it would only mint more asks
				// against the same conflict.
				return evidence, preparation, SessionShimAdoptionReceipt{},
					fmt.Errorf("%w; re-prepare %d refused: %v", err, attempt, prepareErr)
			}
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; re-prepare %d failed: %v", err, attempt, prepareErr)
		}
		if bindErr := bindReprepatedAdoptionToController(ctrl, next, prepared); bindErr != nil {
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; re-prepare %d unusable: %v", err, attempt, bindErr)
		}
		refreshed, evidenceErr := d.sessionShimAdoptionEvidence(ctx, ctrl, prepared, hostID)
		if evidenceErr != nil {
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; re-prepare %d evidence failed: %v", err, attempt, evidenceErr)
		}
		preparations.inputs[id] = next
		preparations.prepared[id] = prepared
		evidence, preparation = refreshed, prepared
		receipt, err = d.completeSessionShimAdoption(ctx, evidence, preparation)
	}
	if err != nil && isSessionShimCarrierCursorDrift(err) {
		// The bound is spent. The refusal is still the operator-facing fact, so
		// it stays wrapped rather than replaced — the quarantine detail carries
		// both cursors AND how many times this daemon tried to reconcile them.
		err = fmt.Errorf("%w; unresolved after %d bounded re-prepared dials", err, sessionShimDriftRedialAttempts)
	}
	return evidence, preparation, receipt, err
}

// bindReprepatedAdoptionToController checks that a re-prepared answer can
// actually be honoured by the controller that is already adopted.
//
// The first preparation is answered INSIDE the handshake, where its generation
// and cursor still have a Welcome to travel on and are validated on the way.
// A re-prepare has neither: the handshake is over, the shim has committed a
// generation, and the resume cursor it is replaying from is fixed. A second
// answer that resolves different values cannot be applied — and must not be
// silently dropped either, because the receipt would then bind the SECOND
// proof to the FIRST cursor and generation, and a legitimately raised floor
// would be discarded with none of its own checks ever running.
//
// So the answer runs the same validation the handshake runs — via the one
// exported resolver both paths call — and is then required to agree with what
// the controller holds. A disagreement is a refusal the caller quarantines,
// naming both values.
func bindReprepatedAdoptionToController(
	ctrl *sessionshim.Controller,
	asked sessionshim.AdoptionPreparation,
	prepared SessionShimAdoptionPreparationResult,
) error {
	hello := ctrl.Hello()
	resolved, err := sessionshim.ResolvePreparedAdoption(prepared.PreparedAdoption, sessionshim.PreparedAdoptionBounds{
		LocalResumeFrom: asked.LocalResumeFrom,
		HelloLastSeq:    hello.LastSeq,
	})
	if err != nil {
		return err
	}
	if resolved.ControllerGeneration != 0 && resolved.ControllerGeneration != ctrl.Generation() {
		return fmt.Errorf(
			"re-prepared controller generation %d does not match the committed generation %d",
			resolved.ControllerGeneration, ctrl.Generation(),
		)
	}
	if resolved.ResumeProvided && resolved.ResumeFrom != ctrl.ResumeFrom() {
		return fmt.Errorf(
			"re-prepared resume cursor %d does not match the adopted cursor %d",
			resolved.ResumeFrom, ctrl.ResumeFrom(),
		)
	}
	return nil
}

// waitSessionShimDriftBackoff spends the doubling delay before attempt n
// without outliving the pass's context.
func (d *Daemon) waitSessionShimDriftBackoff(ctx context.Context, attempt int) error {
	delay := sessionShimDriftRedialBackoff
	for i := 2; i < attempt; i++ {
		delay *= 2
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
