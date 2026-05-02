package runner

// Post-session Linear state transition (REN-1467).
//
// This file implements the runner-side of the WORK_RESULT → Linear
// state-transition wire that closes Wave 6 Phase F.2.5's outstanding
// gap. The legacy TS orchestrator
// (packages/core/src/orchestrator/event-processor.ts:300-450) ran this
// post-session block; the Go runner did not until v0.5.4 — Linear
// issues stayed in Backlog after dev sessions despite the agent
// emitting `WORK_RESULT:passed`, breaking the await_and_route_group
// dispatch chain on the platform side.
//
// The post-session block is intentionally non-fatal. A failed Linear
// transition or diagnostic comment surfaces as a
// PostSessionWarnings[] entry on the Result envelope; the session's
// terminal Status (passed to /api/sessions/<id>/status) is unchanged.
// This matches the legacy TS behaviour and keeps the runner from
// flapping a session's status because Linear was briefly unavailable.

import (
	"context"
	"fmt"
	"time"
)

// postSessionTimeout caps the time the runner spends on the
// transition + diagnostic-comment HTTP calls. Generous enough for the
// proxy retry loop (3 attempts with 1s/2s backoff) plus a buffer.
const postSessionTimeout = 30 * time.Second

// runPostSession is the runner-side port of event-processor.ts:300-450
// (the work-result → Linear state-transition block). Pure side-effect
// function: writes its outcome onto res.LinearStatusTransition +
// res.PostSessionWarnings. Never returns an error — the post-session
// block is best-effort by design.
func (r *Runner) runPostSession(parentCtx context.Context, qw QueuedWork, res *Result) {
	// Detached timeout so a parent ctx that is already on its way to
	// cancellation (e.g. lost-ownership at end of stream) still gets a
	// fair shot at the transition. We DO honour parent ctx
	// cancellation propagation up to the timeout — a hard cancel
	// (operator stop) skips the proxy call.
	ctx, cancel := context.WithTimeout(context.Background(), postSessionTimeout)
	defer cancel()
	// Wire parent cancellation into our detached ctx so a fast-stop
	// still aborts the proxy call before the timeout fires.
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	workType := qw.WorkType
	workResult := res.WorkResult
	// REN-1467: when the runner has no merge-queue adapter (today: the
	// Go runner ships without one), shouldDeferAcceptanceTransition is
	// always false. Reserved for parity with the TS path; flip when a
	// Go merge-queue adapter lands.
	hasMergeQueueAdapter := false

	decision := resolveTargetStatus(workType, res.Status, workResult, hasMergeQueueAdapter)

	// Mirror the decision onto the result envelope before any side
	// effect so the caller sees the chosen branch even if the proxy
	// call below errors out.
	transition := &LinearStatusTransition{
		WorkType:     decision.WorkType,
		WorkResult:   decision.WorkResult,
		TargetStatus: decision.TargetStatus,
		Reason:       decision.Reason,
	}
	res.LinearStatusTransition = transition

	r.logger.Info("post-session: transition decided",
		"sessionId", qw.SessionID,
		"issueId", qw.IssueID,
		"workType", decision.WorkType,
		"workResult", decision.WorkResult,
		"targetStatus", decision.TargetStatus,
		"shouldTransition", decision.ShouldTransition,
		"postDiagnostic", decision.PostDiagnostic,
		"deferred", decision.Deferred,
		"reason", decision.Reason,
	)

	// --- Branch 1: transition the issue --------------------------------
	if decision.ShouldTransition {
		transition.Attempted = true
		err := r.poster.UpdateIssueStatus(ctx, qw.IssueID, decision.TargetStatus)
		if err != nil {
			transition.Error = err.Error()
			warning := fmt.Sprintf(
				"linear updateIssueStatus failed (workType=%s targetStatus=%s): %v",
				decision.WorkType, decision.TargetStatus, err,
			)
			res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
			r.logger.Warn("post-session: updateIssueStatus failed",
				"sessionId", qw.SessionID,
				"issueId", qw.IssueID,
				"targetStatus", decision.TargetStatus,
				"err", err,
			)
			return
		}
		transition.Succeeded = true
		r.logger.Info("post-session: issue status updated",
			"sessionId", qw.SessionID,
			"issueId", qw.IssueID,
			"workType", decision.WorkType,
			"targetStatus", decision.TargetStatus,
		)
		return
	}

	// --- Branch 2: diagnostic comment for unknown WORK_RESULT ---------
	if decision.PostDiagnostic {
		err := r.poster.CreateIssueComment(ctx, qw.IssueID, diagnosticCommentBody())
		if err != nil {
			warning := fmt.Sprintf(
				"linear createComment (diagnostic) failed (workType=%s): %v",
				decision.WorkType, err,
			)
			res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
			r.logger.Warn("post-session: diagnostic comment failed",
				"sessionId", qw.SessionID,
				"issueId", qw.IssueID,
				"err", err,
			)
			return
		}
		transition.DiagnosticPosted = true
		r.logger.Info("post-session: diagnostic comment posted",
			"sessionId", qw.SessionID,
			"issueId", qw.IssueID,
			"workType", decision.WorkType,
		)
		return
	}

	// --- Branch 3: deferred / no-mapping -------------------------------
	// Nothing to do for these branches; the decision is already
	// recorded on transition.Reason so dashboards can surface it.
	if decision.Deferred {
		r.logger.Info("post-session: transition deferred to merge queue",
			"sessionId", qw.SessionID,
			"workType", decision.WorkType,
		)
	} else {
		r.logger.Debug("post-session: no transition required",
			"sessionId", qw.SessionID,
			"workType", decision.WorkType,
			"reason", decision.Reason,
		)
	}
}
