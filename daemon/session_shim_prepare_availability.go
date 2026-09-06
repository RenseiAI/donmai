package daemon

// Provenance: prepare-availability-2026-09-06 — grep a build for this marker to
// prove that ONE lineage's unanswered adoption preparation is retried on a
// fresh dial, and that a spent bound terminalizes that lineage alone.
//
// THE STRAND THIS UNDOES
//
// A host holding four live shim-held sessions launched a fifth. Its composing
// authority was mid-rotation, so the single adoption-preparation round trip a
// launch makes came back "context deadline exceeded". The launch asked once, so
// one unanswered ask condemned the lineage — and because nothing on this side
// distinguished "this preparation was not answered" from "this host is not
// ready", the same seam pulled the host's own admission fence down with it. The
// host stopped claiming, its published readiness went with the fence, and the
// only thing that put it back was a hand-written repair. Four healthy sessions
// and a whole host were spent on one unlucky timeout.
//
// Two rules come out of that, and they are separate:
//
//  1. AN UNANSWERED PREPARATION IS AMBIGUOUS EVIDENCE ABOUT ONE LINEAGE. The
//     recovery corpus allows ambiguous evidence to be preserved, rechecked,
//     retried and then degraded visibly, and forbids a terminal outcome on the
//     first ambiguous answer. So the ask is repeated on a fresh dial inside a
//     bound, and only the spent bound terminalizes — that lineage, by itself,
//     with the sentinel below as the reason.
//  2. IT IS NOT EVIDENCE ABOUT THE HOST. Whether this host may serve is
//     answered by the host's OWN carrier proof, resolved on the beat; the
//     staleness bound that turns a long unknown into a definite not-ready
//     measures that resolver's silence and nothing else. A preparation seam
//     therefore refuses ITSELF when the readiness gate refuses, and leaves the
//     host-wide fence to the seam that holds the host's own proof — see
//     prepareSessionShimAdoptionForCause and sessionShimHeartbeatProjection.
//
// WHY A FRESH DIAL RATHER THAN A SECOND ASK INSIDE THE SAME HANDSHAKE
//
// The preparation runs between Hello and Welcome, and a v3 shim holds an output
// barrier — and a connection deadline sized for one whole handshake — across
// exactly that window. A retry spent inside the handshake would be paid for out
// of that barrier, and a second ask bounded by the budget the first one just
// exhausted is not a second chance. The shim's accept loop serves controller
// after controller and logs a refused handshake without ending the session, so
// hanging up and dialling again is the sanctioned shape: the next attempt gets
// a fresh barrier on the shim's side and a fresh callback budget on this one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

const (
	// sessionShimPrepareAttempts is the TOTAL number of adoption-preparation
	// asks one launched lineage gets while every failure is the composing
	// authority not answering: the first, plus two on fresh dials. It is a hard
	// bound because a launch holds a claimed piece of work and a live harness
	// the whole time — an unbounded retry here would trade one condemned
	// lineage for a claim that is never released and a harness that is never
	// adopted.
	sessionShimPrepareAttempts = 3
	// sessionShimPrepareRetryBackoff is the base delay before the second
	// attempt; it doubles for each attempt after that. The delay exists to let
	// an authority that is rotating finish rotating, not to wait out anything
	// local, so it is short: the whole ladder is under a second.
	sessionShimPrepareRetryBackoff = 200 * time.Millisecond
)

// ErrSessionShimAdoptionPrepareUnavailable reports that the composing authority
// did not ANSWER an adoption preparation: the round trip deadlined, the
// connection failed, or the authority returned a failure rather than a verdict.
//
// It is the exact complement of ErrSessionShimAdoptionPrepareConflict. A
// conflict is an answer — the authority holds state for this lineage it will
// not supersede — so re-asking cannot change it and no budget is spent on it.
// This one is the absence of an answer, which says nothing about the lineage
// and nothing at all about the host, and is therefore retried before it is
// allowed to condemn anything.
var ErrSessionShimAdoptionPrepareUnavailable = errors.New("session shim: adoption preparation was not answered")

// classifySessionShimPrepareFailure marks a composing authority's failure to
// answer so callers can tell it from a refusal.
//
// A typed conflict is returned unchanged: it is the authority's verdict. So is
// a failure already carrying the sentinel, so classification is idempotent
// across the nested seams a preparation passes through. Everything else the
// authority hands back is a round trip that did not complete — a deadline, a
// 5xx, a reset connection — and gains the sentinel. Local refusals raised
// BEFORE the authority is asked (the readiness gate, the configuration checks)
// and local validation of what it answered never reach here: both are this
// daemon's own definite verdicts, and re-asking would only repeat them.
func classifySessionShimPrepareFailure(err error) error {
	if err == nil ||
		errors.Is(err, ErrSessionShimAdoptionPrepareConflict) ||
		errors.Is(err, ErrSessionShimAdoptionPrepareUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrSessionShimAdoptionPrepareUnavailable, err)
}

// adoptionPreparationTimeout bounds one launch's whole bounded set of adoption
// preparation asks.
//
// Derived, not chosen, for the reason adoptionPublicationTimeout gives: every
// ask already runs under callbackTimeout, the ladder makes at most
// sessionShimPrepareAttempts of them, and one further callback budget covers
// the sub-second delays between them. A second hand-picked number here is how
// one stream ends up bounded in two units.
func (c SessionShimConfig) adoptionPreparationTimeout() time.Duration {
	return (sessionShimPrepareAttempts + 1) * c.callbackTimeout()
}

// sessionShimLaunchControllerOptionsFor builds the controller options for one
// dial attempt. It is a function rather than a value because the preparation
// closure has to carry the attempt number and run on the attempt's own context.
type sessionShimLaunchControllerOptionsFor func(context.Context, int) sessionshim.ControllerOptions

// dialLaunchedSessionShim dials the shim this launch started and, while the
// dial fails ONLY because the composing authority did not answer the adoption
// preparation, backs off and dials again inside sessionShimPrepareAttempts.
//
// It returns the context the winning attempt ran on together with a release
// function the caller must defer: everything the caller still has to do with
// this controller — resolving adoption evidence above all — belongs to the same
// bounded attempt, and running it on a launch clock that was already spent is
// what made a late-but-successful discovery look like a failed adoption.
//
// The first attempt runs on the caller's own context and returns it unchanged,
// so a launch whose first preparation is answered behaves exactly as it did
// before this path existed.
func (d *Daemon) dialLaunchedSessionShim(
	ctx context.Context,
	rec sessionshim.Record,
	options sessionShimLaunchControllerOptionsFor,
) (*sessionshim.Controller, context.Context, func(), error) {
	cfg := d.sessionShimConfig()
	id := sessionshim.Identity{OrgID: rec.OrgID, SessionID: rec.SessionID}
	// One budget for the whole ladder, detached from the launch clock. The
	// launch clock is sized for discovery plus the handshake it was written
	// for; sizing a bounded repair by whatever remainder that clock happens to
	// have left is the mistake adoptionPublicationTimeout already exists to
	// undo — a slow discovery would hand the repair no attempts at all and call
	// the lineage unpreparable on the strength of one timeout.
	repairCtx, cancelRepair := context.WithTimeout(
		context.WithoutCancel(ctx), cfg.adoptionPreparationTimeout(),
	)
	var lastErr error
	for attempt := 1; attempt <= sessionShimPrepareAttempts; attempt++ {
		attemptCtx, cancelAttempt := ctx, context.CancelFunc(func() {})
		if attempt > 1 {
			attemptCtx, cancelAttempt = context.WithTimeout(repairCtx, cfg.callbackTimeout())
		}
		ctrl, err := sessionshim.Dial(attemptCtx, rec, options(attemptCtx, attempt))
		if err == nil {
			return ctrl, attemptCtx, func() { cancelAttempt(); cancelRepair() }, nil
		}
		cancelAttempt()
		lastErr = err
		if !errors.Is(err, ErrSessionShimAdoptionPrepareUnavailable) {
			// A refusal, a protocol mismatch, an unreachable socket: this dial
			// got an answer, and it was no. Every one of those keeps the
			// single-attempt disposition it already had.
			cancelRepair()
			return nil, ctx, func() {}, err
		}
		if attempt == sessionShimPrepareAttempts {
			break
		}
		slog.Warn("session shim: the composing authority did not answer this launch's adoption preparation; "+
			"dialling the shim again rather than condemning the lineage (prepare-availability-2026-09-06)",
			"session", id.String(), "attempt", attempt, "of", sessionShimPrepareAttempts, "error", err)
		if waitErr := waitSessionShimRetryBackoff(repairCtx, attempt+1, sessionShimPrepareRetryBackoff); waitErr != nil {
			cancelRepair()
			return nil, ctx, func() {}, fmt.Errorf("%w; preparation attempt %d abandoned: %w", err, attempt+1, waitErr)
		}
	}
	cancelRepair()
	// The bound is spent. This lineage — and only this lineage — is terminal:
	// the error becomes the spawner's abort reason and, through it, the reason
	// the control plane records for the released claim. Nothing host-wide moved
	// on the way here: no readiness was withdrawn, no composition was
	// republished, and every other lineage on this host is exactly as durable
	// as it was before this launch started.
	slog.Error("session shim: the composing authority answered none of this launch's bounded adoption preparations; "+
		"terminalizing this lineage alone (prepare-availability-2026-09-06)",
		"session", id.String(), "attempts", sessionShimPrepareAttempts, "error", lastErr)
	return nil, ctx, func() {}, fmt.Errorf(
		"%w; unanswered after %d bounded preparation attempts", lastErr, sessionShimPrepareAttempts,
	)
}

// waitSessionShimRetryBackoff spends the doubling delay before attempt n
// without outliving ctx. attempt counts from 1, so the first delay this can be
// asked for is the one before attempt 2, and that one is base.
func waitSessionShimRetryBackoff(ctx context.Context, attempt int, base time.Duration) error {
	delay := base
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
