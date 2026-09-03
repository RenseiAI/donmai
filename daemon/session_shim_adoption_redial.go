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
// RE-SIGNED proof at a corrected boundary, and the harness on the other side of
// it was alive the entire time.
//
// WHAT THIS PATH CAN AND CANNOT REPAIR
//
// It can repair a boundary. It cannot repair an epoch.
//
// The handshake is over by the time a re-prepare runs. The shim has committed
// one generation, one resume cursor, and one exact extension set — and the
// adoption ADR's D15.3 says no carrier commit occurs before Adopted echoes the
// prepared value. No Adopted frame will ever echo a SECOND carrier epoch,
// because validateAdoptionCommit already froze the echo against the first
// Welcome and there is no second Welcome to send. So a re-prepared answer is
// honourable here only when it re-signs the SAME carrier epoch at a corrected
// boundary. An answer whose epoch supersedes needs a handshake this path does
// not have, and is refused and quarantined — the same disposition a
// disagreeing generation gets. Committing it instead would publish an
// activation naming the epoch the authority was just told to abandon, which is
// precisely the "reactivating its incumbent" the 2026-08-23 retained-candidate
// abandonment correction forbids.
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
//   - the authority disposes of it as preparing_reprepare — the adoption ADR's
//     one abandonment cause whose SOURCE is a preparing handoff and which may
//     keep or change the controller. That ADR states the obligation directly:
//     an admitted preparing handoff has no retained Snapshot replay and must
//     abandon before reprepare. carrier_cursor_drift is a PREPARE cause, a
//     different axis from the abandonment vocabulary, so the mapping is named
//     here rather than left for an authority to guess: drift maps to
//     preparing_reprepare and to nothing else;
//   - a predecessor that has already reached receipt_stored is NOT that case.
//     The same ADR is explicit that a same-controller receipt_stored handoff
//     with changed bytes is a changed-replay conflict, not permission to
//     abandon and resample, and its only same-controller abandonment causes
//     (credential_lifetime_insufficient, lineage_terminal) need evidence
//     carrier cursor drift does not have. An authority in that state answers
//     the typed ErrSessionShimAdoptionPrepareConflict, which is a refusal
//     rather than a failure: this pass spends none of its remaining budget on
//     it and quarantines with the conflict as the detail.
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
	"github.com/RenseiAI/donmai/shimwire"
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
		if bindErr := bindRepreparedAdoptionToController(ctrl, next, prepared); bindErr != nil {
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

// ErrSessionShimRepreparedCarrierEpochSupersedes reports a re-prepared answer
// whose negotiated extensions — carrier_epoch above all — differ from the ones
// the shim committed.
//
// It is separate from the other two disagreements because it is the one an
// authority following this path's own contract will produce if it reads
// "abandon and re-prepare" as "reserve a higher epoch". The commit that would
// follow is unsanctioned: the evidence's extensions are sourced from the shim's
// Adopted frame, which validateAdoptionCommit froze against the FIRST Welcome,
// so the receipt would name the new candidate while carrying the superseded
// epoch, and the activation published from it would reactivate exactly what the
// authority was told to burn.
var ErrSessionShimRepreparedCarrierEpochSupersedes = errors.New(
	"session shim: a re-prepared carrier epoch cannot be committed without a second handshake",
)

// bindRepreparedAdoptionToController checks that a re-prepared answer can
// actually be honoured by the controller that is already adopted.
//
// The first preparation is answered INSIDE the handshake, where its generation,
// cursor, and extensions still have a Welcome to travel on and are validated on
// the way. A re-prepare has none of that: the handshake is over, the shim has
// committed a generation, the resume cursor it is replaying from is fixed, and
// its Adopted frame has already echoed one exact extension set. A second answer
// that resolves different values cannot be applied — and must not be silently
// dropped either, because the receipt would then bind the SECOND proof to the
// FIRST cursor, generation, and carrier epoch, and a legitimately raised floor
// would be discarded with none of its own checks ever running.
//
// So the answer runs the same validation the handshake runs — via the one
// exported resolver both paths call — and is then required to agree with what
// the controller holds. A disagreement is a refusal the caller quarantines,
// naming both values.
//
// The two static-configuration bounds are passed false deliberately: a
// re-prepare has no static generation or cursor to conflict with, because the
// only authority left is the committed adoption itself, and agreement with THAT
// is a strictly stronger requirement than either flag expresses.
func bindRepreparedAdoptionToController(
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
	committed := ctrl.Adoption()
	if !resolved.Extensions.ExactEqual(committed.Extensions) {
		return fmt.Errorf("%w: re-prepared carrier epoch %s, committed %s",
			ErrSessionShimRepreparedCarrierEpochSupersedes,
			shimExtensionCarrierEpoch(resolved.Extensions),
			shimExtensionCarrierEpoch(committed.Extensions))
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

// shimExtensionCarrierEpoch renders an extension set's carrier epoch for an
// operator-facing refusal. An absent epoch is said, not elided: "absent" and
// "the same as the other one" are different facts about a disagreement.
func shimExtensionCarrierEpoch(extensions shimwire.Extensions) string {
	if epoch, ok := extensions.Get(shimwire.ExtCarrierEpoch); ok {
		return epoch
	}
	return "absent"
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
