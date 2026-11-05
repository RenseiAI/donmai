package runner

// Stage outcomes and transitions (the 1.0 dispatch wire).
//
// When a work item carries `transitions`, the control plane has already
// resolved where the work item goes on each outcome, as a stage key plus the
// tracker's native state id. The runner then:
//
//   - applies `start` when the run starts;
//   - classifies the finished run into one outcome (success, failure,
//     blocked, close) or into "stay" (no verdict, operator cancel, timeout,
//     lost ownership, a deferred provider error);
//   - applies the matching target, where "stay" moves nothing;
//   - never consults the name-keyed status tables in sdlc.go.
//
// Transitions are the single status mover for such work items, so they apply
// whether or not Options.SkipPostSession is set; that option governs only the
// legacy name-keyed block, which runs exactly as before for a work item
// without transitions.

import (
	"context"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/stagewire"
)

// Close reasons a `WORK_RESULT: close` marker may carry. Stable strings.
const (
	CloseReasonDuplicate = "duplicate"
	CloseReasonObsolete  = "obsolete"
	CloseReasonCanceled  = "canceled"
)

// WorkClose is a run's request to close its work item, from a
// `WORK_RESULT: close <reason> [<duplicateOf>]` marker.
type WorkClose struct {
	// Reason is one of the CloseReason constants.
	Reason string `json:"reason"`
	// DuplicateOf names the work item this one duplicates; only with
	// Reason == CloseReasonDuplicate, and optional even then.
	DuplicateOf string `json:"duplicateOf,omitempty"`
}

// parseCloseMarker reads the remainder of a `WORK_RESULT: close` line:
// a reason (duplicate, obsolete or canceled; "cancelled" is accepted), then,
// for a duplicate, an optional identifier of the work item it duplicates. A
// leading colon and a trailing HTML-comment fence are tolerated. A missing or
// unknown reason is not a close.
func parseCloseMarker(rest string) (*WorkClose, bool) {
	rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "-->"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return nil, false
	}
	var reason string
	switch asciiLower(fields[0]) {
	case CloseReasonDuplicate:
		reason = CloseReasonDuplicate
	case CloseReasonObsolete:
		reason = CloseReasonObsolete
	case CloseReasonCanceled, "cancelled":
		reason = CloseReasonCanceled
	default:
		return nil, false
	}
	closeReq := &WorkClose{Reason: reason}
	if reason == CloseReasonDuplicate && len(fields) > 1 && len(fields[1]) <= stagewire.MaxIDBytes {
		closeReq.DuplicateOf = fields[1]
	}
	return closeReq, true
}

// describe is the close request as one short line, used as the decline
// reason on every legacy path.
func (c *WorkClose) describe() string {
	if c.DuplicateOf != "" {
		return "close as duplicate of " + c.DuplicateOf
	}
	return "close as " + c.Reason
}

// StageTransitionRecord is the runner-side record of one stage transition:
// what the runner decided and what the tracker write did. Observability
// only; it never changes the session's terminal status.
type StageTransitionRecord struct {
	// Outcome is start, success, failure, blocked or close; empty when the
	// run ended without an outcome. An outcome whose target is "stay"
	// keeps its Outcome and writes nothing (Attempted false).
	Outcome string `json:"outcome,omitempty"`
	// Reason is a short identifier for the decision ("passed",
	// "no-verdict", "operator-cancelled", "stay", ...).
	Reason string `json:"reason,omitempty"`
	// StageKey and WriteStatus are the applied target; empty on stay.
	StageKey    string `json:"stageKey,omitempty"`
	WriteStatus string `json:"writeStatus,omitempty"`
	// Attempted is true when the runner wrote the tracker state.
	Attempted bool `json:"attempted,omitempty"`
	// Succeeded is true when that write returned no error.
	Succeeded bool `json:"succeeded,omitempty"`
	// Error is the write (or comment) error, when any.
	Error string `json:"error,omitempty"`
	// CommentPosted is true when the runner posted the stay or close
	// comment.
	CommentPosted bool `json:"commentPosted,omitempty"`
}

// stageDecision is the end-of-run classification for a work item that
// carries transitions. An empty outcome means stay.
type stageDecision struct {
	outcome stagewire.Outcome
	reason  string
	// comment is posted to the work item when non-empty.
	comment string
}

// classifyStageOutcome classifies a finished run. Order matters: an explicit
// close or decline wins; runs that did not finish (operator cancel, timeout,
// lost ownership) and a deferred provider error stay; a failed session or a
// failed verdict is a failure; then the completion contract decides.
func classifyStageOutcome(qw QueuedWork, res *Result) stageDecision {
	if res == nil {
		return stageDecision{reason: "no-result"}
	}
	switch {
	case res.WorkClose != nil:
		return stageDecision{outcome: stagewire.OutcomeClose, reason: "close", comment: closeCommentBody(*res.WorkClose)}
	case res.FailureMode == FailureAgentBlocked:
		return stageDecision{outcome: stagewire.OutcomeBlocked, reason: "blocked"}
	case res.FailureMode == FailureOperatorCancelled:
		return stageDecision{reason: "operator-cancelled", comment: stayCommentBody("an operator cancelled it")}
	case res.FailureMode == FailureTimeout:
		return stageDecision{reason: "timeout", comment: stayCommentBody("it timed out")}
	case res.FailureMode == FailureLostOwnership:
		// Another worker may own the work item now: never move it.
		return stageDecision{reason: "lost-ownership"}
	case shouldDeferFailureTransition(qw.DeferFailureTransition, res.FailureMode, res.ToolCalls):
		return stageDecision{reason: "deferred-provider-error"}
	case res.Status == "failed":
		reason := "failed"
		if res.FailureMode != "" {
			reason = res.FailureMode
		}
		return stageDecision{outcome: stagewire.OutcomeFailure, reason: reason}
	case res.WorkResult == "failed", res.ReviewVerdict == "REQUEST_CHANGES":
		return stageDecision{outcome: stagewire.OutcomeFailure, reason: "failed"}
	}
	contract := qw.completionContract()
	switch {
	case contract.pr && res.PullRequestURL == "":
		return stageDecision{outcome: stagewire.OutcomeFailure, reason: "no-pull-request"}
	case (contract.verdict || contract.workResult) && res.WorkResult != "passed":
		return stageDecision{reason: "no-verdict", comment: diagnosticCommentBody()}
	case res.Status == "completed":
		reason := "completed"
		if res.WorkResult == "passed" {
			reason = "passed"
		}
		return stageDecision{outcome: stagewire.OutcomeSuccess, reason: reason}
	default:
		return stageDecision{reason: "no-status"}
	}
}

// stageTransitionsApply reports whether the stage-transition path owns this
// work item's status moves.
func stageTransitionsApply(qw QueuedWork) bool {
	return qw.Transitions != nil && qw.IssueID != ""
}

// applyStageStart applies the start transition when the run starts. Nil
// when the work item carries no transitions; a record that stays when the
// stage declares no start.
func (r *Runner) applyStageStart(ctx context.Context, qw QueuedWork) *StageTransitionRecord {
	if !stageTransitionsApply(qw) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, postSessionTimeout)
	defer cancel()
	return r.writeStageTransition(ctx, qw, stagewire.OutcomeStart, "start")
}

// finishStageTransition classifies the finished run and applies the
// matching target, recording both records and any warning on res. The
// tracker calls outlive run cancellation (a timed-out run still gets its
// comment) but stay bounded.
func (r *Runner) finishStageTransition(ctx context.Context, qw QueuedWork, res *Result, start *StageTransitionRecord) {
	if res == nil || !stageTransitionsApply(qw) {
		return
	}
	res.StageStart = start
	if start != nil && start.Error != "" {
		res.PostSessionWarnings = append(res.PostSessionWarnings,
			fmt.Sprintf("stage start transition failed (stageKey=%s): %s", start.StageKey, start.Error))
	}
	ctx, cancel := terminalResultPostContext(ctx)
	defer cancel()
	decision := classifyStageOutcome(qw, res)
	record := &StageTransitionRecord{Reason: decision.reason}
	if decision.outcome != "" {
		record = r.writeStageTransition(ctx, qw, decision.outcome, decision.reason)
	}
	if record.Error != "" {
		res.PostSessionWarnings = append(res.PostSessionWarnings,
			fmt.Sprintf("stage %s transition failed (stageKey=%s): %s", record.Outcome, record.StageKey, record.Error))
	}
	if decision.comment != "" {
		if err := r.poster.CreateIssueComment(ctx, qw.IssueID, decision.comment); err != nil {
			res.PostSessionWarnings = append(res.PostSessionWarnings,
				fmt.Sprintf("stage %s comment failed: %v", decision.reason, err))
		} else {
			record.CommentPosted = true
		}
	}
	res.StageTransition = record
	r.logger.Info("stage transition decided",
		"sessionId", qw.SessionID,
		"issueId", qw.IssueID,
		"outcome", record.Outcome,
		"reason", record.Reason,
		"stageKey", record.StageKey,
		"attempted", record.Attempted,
		"succeeded", record.Succeeded,
		"commentPosted", record.CommentPosted,
	)
}

// writeStageTransition writes the target of outcome. A stay target (an
// absent outcome, or the literal "stay") writes nothing.
func (r *Runner) writeStageTransition(ctx context.Context, qw QueuedWork, outcome stagewire.Outcome, reason string) *StageTransitionRecord {
	record := &StageTransitionRecord{Outcome: string(outcome), Reason: reason}
	target := qw.Transitions.For(outcome)
	if target.Stay() {
		return record
	}
	record.StageKey, record.WriteStatus, record.Attempted = target.StageKey, target.WriteStatus, true
	if err := r.poster.UpdateIssueState(ctx, qw.IssueID, target.WriteStatus); err != nil {
		record.Error = err.Error()
		r.logger.Warn("stage transition write failed",
			"sessionId", qw.SessionID, "issueId", qw.IssueID,
			"outcome", outcome, "stageKey", target.StageKey, "err", err)
		return record
	}
	record.Succeeded = true
	return record
}

// stayCommentBody is posted when a run ends without an outcome and the work
// item stays where it is.
func stayCommentBody(why string) string {
	return "The agent run ended before it finished because " + why + ". " +
		"The issue was left in its current status; re-run it or move it by hand."
}

// closeCommentBody records why the run closed the work item.
func closeCommentBody(c WorkClose) string {
	if c.DuplicateOf != "" {
		return "The agent closed this issue as a duplicate of " + c.DuplicateOf + "."
	}
	return "The agent closed this issue as " + c.Reason + "."
}
