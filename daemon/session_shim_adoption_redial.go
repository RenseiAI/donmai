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
// SCOPE — THIS REPAIRS DRIFT ONLY FOR A LINEAGE THAT NEGOTIATED NO CARRIER EPOCH
//
// Read this before assuming the path is general. It is not, and the limit is
// the corpus's, not an oversight.
//
// The handshake is over by the time a re-prepare runs. The shim has committed
// one generation, one resume cursor, and one exact extension set — and the
// adoption ADR's D15.3 says no carrier commit occurs before Adopted echoes the
// prepared value. No Adopted frame will ever echo a SECOND carrier epoch,
// because validateAdoptionCommit already froze the echo against the first
// Welcome and there is no second Welcome to send.
//
// Now read what the same ADR requires a conforming authority to answer with.
// Its D10 crash matrix, on the row for this exact failure — a carrier journal
// that advances after the proof reservation but before candidate admission —
// says to "mutate no room state and reprepare at a strictly greater carrier
// epoch", and the adoption-fails row says "a later preparation allocates a
// strictly greater value". A conforming authority CANNOT hand back a re-signed
// same epoch: a strictly greater one is refused here for want of a second
// handshake, and re-signing the same reservation is changed bytes under one
// key, which its own contract calls a changed-replay conflict.
//
// So for a lineage whose committed adoption carries a carrier epoch there is no
// answer this path can accept, and asking would burn an epoch and leave a fresh
// admitted reservation undisposed to reach an outcome available without asking.
// Such a lineage is therefore NOT re-prepared at all: it takes the bounded path
// straight to quarantine with ErrSessionShimCarrierEpochBoundDriftNeedsHandshake,
// spending no abandonment. Repairing it needs a second handshake, which is its
// own change and not this one.
//
// What remains, and what this path does repair, is drift on a lineage that
// negotiated no carrier epoch — the shape the live incident had.
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
//     reservation for this lineage is outstanding and stale. It is only ever
//     sent for a lineage with no committed carrier epoch — see SCOPE above;
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
//
// THE SECOND STRAND: A RELAY THAT SAID "NOT NOW"
//
// Drift was the first refusal this pass learned to read. It was never the only
// ambiguous one. A relay that runs as a single always-on process restarts every
// host carrier on the fleet at once on each deploy, and while it drains it
// answers every attach dial with 503 + Retry-After and closes every live leg
// with 1012 and a "redial after <N>s" reason. A dial that never reached the
// relay at all says the same thing more crudely.
//
// None of those is evidence about the lineage. The harness is alive, its
// carrier proof is valid, and the replacement relay is seconds away — but a
// pass that quarantines on a single refusal turns that into a lineage with a
// harness and no controller, which renews no orphan clock and tears itself down
// at the shim's deadline. That is precisely the roomless, unwakeable seat shape
// a relay deploy produced on 2026-09-02, and nothing the relay can send makes a
// single-attempt policy retry: the second half of the planned-restart contract
// is this side's.
//
// So a refusal that wraps attachclient.ErrRelayUnavailable gets its own bounded
// budget of plain re-dials before any terminal consequence, honouring the floor
// the relay named (Retry-After, or the "redial after <N>s" hint) so the fleet
// does not arrive back before the replacement has booted.
//
// It is a re-DIAL, not a re-prepare, and that difference is the whole reason it
// is a separate budget:
//
//   - drift means the proof is stale, so the repair is a NEW proof and the
//     re-dial is worthless without one;
//   - unavailable means the proof was never examined. The relay refused before
//     the upgrade, or closed the leg it had just accepted. Re-preparing here
//     would burn a reservation and hand the authority a supersession to dispose
//     of for a dial nobody read, so the SAME evidence and the SAME preparation
//     are presented again.
//
// The one piece of local state a refused dial can leave behind is a staged
// mandatory Snapshot — a leg that died after the Snapshot request but before
// the receipt — so that is cleared between attempts for the same reason the
// drift path clears it: the next dial must refuse on what the relay says, never
// on this daemon's own leftovers. The Snapshot PROXY is deliberately kept: it
// belongs to the proof being re-presented, and deactivating it would leave the
// re-dial holding evidence it can no longer answer a Snapshot request for.
//
// The two budgets compose without a third counter: each round of re-dials runs
// the whole drift-repairing pass, so a lineage whose refusals alternate is
// still bounded by their product, and every wait is spent inside the startup
// pass's own context. The re-dial budget itself is not a new number either —
// it is the configured re-adoption policy's, the same one the controller-loss
// path spends (see sessionShimUnavailableRedialBound).
//
// SCOPE — STARTUP ONLY
//
// The controller-loss re-adoption path already retries every failed attempt
// with backoff inside its own bound, so it never had this gap and gains no
// second loop here. Startup composition is the one place a single refusal was
// terminal.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
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

// sessionShimRelayUnavailable reports whether a durable-adoption refusal is the
// second shape the pass may retry: the relay declined to be reached at all —
// the drain-window 503, the 1012 planned-restart close, or a dial that never
// got through — so nothing was learned about the lineage behind it. It also
// returns the redial floor the relay named, zero when it named none.
//
// Composing callers wrap the attach client's typed refusal with %w, so this
// reads through their wrapping. Every refusal that is neither this nor carrier
// cursor drift keeps its existing single-attempt disposition.
func sessionShimRelayUnavailable(err error) (time.Duration, bool) {
	if err == nil || !attachclient.IsRelayUnavailable(err) {
		return 0, false
	}
	hint, _ := attachclient.RelayRedialAfter(err)
	return hint, true
}

// completeSessionShimAdoptionWithBoundedRedial is the startup pass's single
// entry point into durable adoption for one lineage. It runs the
// drift-repairing pass and, while the answer is the relay declining to be
// reached, waits out the floor the relay named and runs it again, inside the
// bound above.
//
// A quarantine decision is only ever reached after BOTH budgets — the ADR's
// "the re-adoption check must not be shortcut" rule applies to every ambiguous
// refusal, not only to the first one this pass learned to read.
func (d *Daemon) completeSessionShimAdoptionWithBoundedRedial(
	ctx context.Context,
	ctrl *sessionshim.Controller,
	preparations *sessionShimAdoptionPreparations,
	hostID string,
	evidence SessionShimAdoptionEvidence,
	preparation SessionShimAdoptionPreparationResult,
) (SessionShimAdoptionEvidence, SessionShimAdoptionPreparationResult, SessionShimAdoptionReceipt, error) {
	id := ctrl.Identity()
	dials, base, ceiling := d.sessionShimUnavailableRedialBound()
	evidence, preparation, receipt, err := d.completeSessionShimAdoptionRepreparingDrift(
		ctx, ctrl, preparations, hostID, evidence, preparation,
	)
	for dial := 2; dial <= dials; dial++ {
		hint, unavailable := sessionShimRelayUnavailable(err)
		if !unavailable {
			return evidence, preparation, receipt, err
		}
		slog.Warn("session shim: the relay refused durable adoption without reading the proof; re-dialling before quarantine",
			"session", id.String(), "dial", dial, "of", dials,
			"relayRedialFloor", hint, "error", err)
		// Clear only what a refused dial can have staged locally. The proof and
		// its Snapshot authority are re-presented unchanged: the relay never
		// looked at them, so there is nothing about them to repair.
		d.cancelStagedSessionShimSnapshot(id)
		if waitErr := waitSessionShimUnavailableRedial(ctx, dial, hint, base, ceiling); waitErr != nil {
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; re-dial %d abandoned: %v", err, dial, waitErr)
		}
		evidence, preparation, receipt, err = d.completeSessionShimAdoptionRepreparingDrift(
			ctx, ctrl, preparations, hostID, evidence, preparation,
		)
	}
	if _, unavailable := sessionShimRelayUnavailable(err); unavailable {
		// Still the operator-facing fact, so it stays wrapped rather than
		// replaced — the quarantine detail says the relay never answered AND how
		// many times this daemon waited for it to.
		err = fmt.Errorf("%w; the relay was still unavailable after %d bounded re-dials", err, dials)
	}
	return evidence, preparation, receipt, err
}

// sessionShimUnavailableRedialBound resolves how hard the startup pass chases a
// relay that is not answering — and does it from the CONFIGURED re-adoption
// policy rather than from a second set of constants.
//
// "How long does this daemon chase a carrier that is not answering" is one
// deployment decision, and the controller-loss path already owns it (3 attempts
// with a 5s doubling backoff by default). A startup pass with its own private
// numbers would be a third policy nobody can tune and nobody can read off the
// configuration.
//
// What it deliberately does NOT read is that policy's Disabled and Mode
// switches. Those answer "may this daemon re-adopt a lineage whose controller
// it lost", which is a different question from "may this daemon dial the relay
// twice while the relay is restarting" — a deployment that turned the first off
// did not ask to quarantine every lineage on the host the next time a deploy
// lands mid-composition. Lineage-live mode leaves Attempts zero because its own
// bound is a window, so the fixed default stands in for it here.
func (d *Daemon) sessionShimUnavailableRedialBound() (dials int, base, ceiling time.Duration) {
	policy := d.sessionShimConfig().readoption()
	dials = policy.Attempts
	if dials <= 0 {
		dials = defaultSessionShimReadoptionAttempts
	}
	base = policy.Backoff
	if base <= 0 {
		base = defaultSessionShimReadoptionBackoff
	}
	ceiling = policy.BackoffCap
	if ceiling <= 0 {
		ceiling = defaultSessionShimReadoptionBackoffCap
	}
	return dials, base, ceiling
}

// waitSessionShimUnavailableRedial spends the delay before one re-dial without
// outliving the pass's context. The relay's own floor wins when it is larger
// than the doubling schedule — it is a floor, not a substitute for backoff —
// and the ceiling bounds both, so a relay that asks for longer than a startup
// pass can hold a host's capacity for gets dialled anyway and refused again,
// which spends a bounded attempt instead of an unbounded wait.
func waitSessionShimUnavailableRedial(ctx context.Context, dial int, hint, base, ceiling time.Duration) error {
	timer := time.NewTimer(sessionShimUnavailableRedialDelay(dial, hint, base, ceiling))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sessionShimUnavailableRedialDelay is the arithmetic, kept out of the wait so
// the schedule can be pinned exactly rather than inferred from a clock.
func sessionShimUnavailableRedialDelay(dial int, hint, base, ceiling time.Duration) time.Duration {
	delay := base
	for i := 2; i < dial && delay < ceiling; i++ {
		delay *= 2
	}
	if hint > delay {
		delay = hint
	}
	if delay > ceiling {
		delay = ceiling
	}
	return delay
}

// completeSessionShimAdoptionRepreparingDrift runs the durable-adoption
// callback and, while it refuses with carrier cursor drift, re-prepares the
// lineage's carrier proof and dials again inside the bound above.
//
// It returns the evidence and preparation the FINAL attempt used: a re-prepare
// mints a new proof and a new Snapshot authority, so the caller must retain the
// pair the receipt actually belongs to. On exhaustion it returns the last
// refusal, which the caller quarantines — visibly degraded, after the bound,
// never on the first ambiguous answer.
func (d *Daemon) completeSessionShimAdoptionRepreparingDrift(
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
		// SCOPE gate. A lineage whose committed adoption carries a carrier epoch
		// has no answer this path can accept — see the file header. Refuse it
		// here, BEFORE the ask, so no abandonment is spent and no successor
		// reservation is minted for a refusal that is already certain.
		if epoch, bound := ctrl.Adoption().Extensions.Get(shimwire.ExtCarrierEpoch); bound {
			return evidence, preparation, SessionShimAdoptionReceipt{},
				fmt.Errorf("%w; %w (committed carrier epoch %s)",
					err, ErrSessionShimCarrierEpochBoundDriftNeedsHandshake, epoch)
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

// ErrSessionShimCarrierEpochBoundDriftNeedsHandshake reports drift on a lineage
// whose committed adoption negotiated a carrier epoch.
//
// It is a refusal BEFORE the ask, not after it. The adoption ADR's D10 matrix
// requires a conforming authority to reprepare at a strictly greater carrier
// epoch, and a greater epoch cannot be committed here for want of a second
// handshake — so the ask has exactly one possible outcome, and making it would
// burn an epoch and leave a fresh admitted reservation undisposed on the way to
// that outcome. Repairing this case needs a second handshake; that is its own
// change.
var ErrSessionShimCarrierEpochBoundDriftNeedsHandshake = errors.New(
	"session shim: carrier-epoch-bound drift needs a new handshake, not a re-prepare",
)

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
		return fmt.Errorf("%w: %s",
			ErrSessionShimRepreparedCarrierEpochSupersedes,
			describeShimExtensionDifference(resolved.Extensions, committed.Extensions))
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

// describeShimExtensionDifference renders which negotiated extensions disagree,
// by key.
//
// The comparison behind it is canonical-JSON equality over the WHOLE extension
// set, so naming only the carrier epoch would one day print
// "carrier epoch 7, committed 7" for a difference somewhere else and read as a
// contradiction. Each differing key is named instead, and an absent value is
// said rather than elided: "absent" and "the same as the other one" are
// different facts about a disagreement.
func describeShimExtensionDifference(reprepared, committed shimwire.Extensions) string {
	names := make([]string, 0, len(reprepared.Values)+len(committed.Values))
	seen := make(map[string]struct{}, len(reprepared.Values)+len(committed.Values))
	for _, values := range []map[string]string{reprepared.Values, committed.Values} {
		for name := range values {
			if _, already := seen[name]; already {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	differences := make([]string, 0, len(names)+1)
	for _, name := range names {
		got, hasGot := reprepared.Values[name]
		want, hasWant := committed.Values[name]
		if hasGot == hasWant && got == want {
			continue
		}
		differences = append(differences, fmt.Sprintf("%s: re-prepared %s, committed %s",
			name, shimExtensionValueOrAbsent(got, hasGot), shimExtensionValueOrAbsent(want, hasWant)))
	}
	if !slices.Equal(reprepared.Required, committed.Required) {
		differences = append(differences, fmt.Sprintf("required: re-prepared %v, committed %v",
			reprepared.Required, committed.Required))
	}
	if len(differences) == 0 {
		// ExactEqual said they differ and every field compares equal, so the
		// difference is in the encoding. Say that rather than an empty reason.
		return "the negotiated extension sets differ in encoding"
	}
	return strings.Join(differences, "; ")
}

func shimExtensionValueOrAbsent(value string, present bool) string {
	if !present {
		return "absent"
	}
	return value
}

// waitSessionShimDriftBackoff spends the doubling delay before attempt n
// without outliving the pass's context.
func (d *Daemon) waitSessionShimDriftBackoff(ctx context.Context, attempt int) error {
	return waitSessionShimRetryBackoff(ctx, attempt, sessionShimDriftRedialBackoff)
}
