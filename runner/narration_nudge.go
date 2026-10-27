package runner

import (
	"context"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/activity"
)

// Narration nudge — one automatic continuation for a development turn that
// ends on a narration-only assistant message with no commit.
//
// A harness can report turn end while the agent only wrote a progress note:
// the last assistant message carries text and no tool call, and the session
// has neither a new commit nor a pull request, nor a structured
// blocked/verdict result. Without a nudge the session completes and the
// delivery gate fails it (for example a continued pull request with no new
// commit since dispatch). The runner instead sends one continuation over the
// existing steer channel (message injection, or session resume where the
// harness takes no inject), bounded by NarrationNudgeLimit (default 1).
//
// An explicit blocked or verdict turn result is never nudged: a deliberate
// decline carries its own outcome. The nudge is recorded as a session
// activity, its count rides Result.TurnContinuations, and a session that
// stops the same way once the bound is spent fails with the bound named in
// Result.Error.

// DefaultNarrationNudgeLimit is the bound applied when
// Options.NarrationNudgeLimit is zero: one narration-only nudge per session.
const DefaultNarrationNudgeLimit = 1

// narrationNudgePrompt is the continuation sent after a narration-only stop.
// It names the missing work (commit, push, report) without naming any
// tracker, platform, repository or harness: the same bytes ride every lane.
const narrationNudgePrompt = "You ended your turn with a progress note. Continue the task to completion: commit, push, and report the result."

// narrationNudgeActivitySubtype aliases the activity poster's exported
// marker instead of duplicating the literal: a change to the wire subtype
// breaks this build instead of drifting silently.
const narrationNudgeActivitySubtype = activity.NarrationNudgeSubtype

// narrationNudgeActivityMessage aliases the poster content for the same
// reason: the runner-sent message and the rendered content stay byte-equal.
const narrationNudgeActivityMessage = activity.NarrationNudgeContent

// narrationNudgeState is one session's narration-nudge budget.
type narrationNudgeState struct {
	limit int
	sent  int
}

// newNarrationNudgeState resolves the configured bound. Zero is the default;
// negative disables the nudge.
func (r *Runner) newNarrationNudgeState() *narrationNudgeState {
	limit := r.narrationNudgeLimit
	switch {
	case limit == 0:
		limit = DefaultNarrationNudgeLimit
	case limit < 0:
		limit = 0
	}
	return &narrationNudgeState{limit: limit}
}

// canNudge reports whether another narration nudge may be sent.
func (s *narrationNudgeState) canNudge() bool {
	return s.sent < s.limit
}

// recordSent counts one delivered nudge.
func (s *narrationNudgeState) recordSent() {
	s.sent++
}

// isNarrationOnlyStop reports whether the latest turn ended on a
// narration-only assistant message that the runner should nudge once:
// development work that owes a pull request, no delivering pull request
// (verified or reported on the session's own repository), no turn-result
// manifest, no structured blocked or verdict result in this or any earlier
// turn, a clean terminal, and a last assistant message that carries text
// with no tool call after it. A provider error, a crash, or an unsuccessful
// terminal never counts: those have their own recovery. undelivered is the
// tail-recovery reason the session's pull request does not deliver the work
// ("" when it delivers or there is none): a continued pull request whose
// head has not moved (undeliveredNoNewCommit) counts as no pull request so
// the nudge fires on the undelivered path instead of the generic
// no-new-commit continuation. Any other pull request (delivered, or a draft
// with its own remedy prompt) suppresses the nudge.
func isNarrationOnlyStop(qw QueuedWork, res *Result, session, turn streamObservation, reportedPR bool, undelivered string) bool {
	if !RequiresPRURL(qw.WorkType, qw.WorkTypeRole) {
		return false
	}
	if res.PullRequestURL != "" && undelivered != undeliveredNoNewCommit {
		return false
	}
	if reportedPR {
		return false
	}
	if res.Manifest != nil {
		return false
	}
	if res.WorkResult != "" {
		return false
	}
	if session.blocked || turn.blocked {
		return false
	}
	if turn.verdict() != "" {
		return false
	}
	if res.Status == "failed" || res.FailureMode != "" {
		return false
	}
	if turn.errorEvent != nil || turn.terminalEvent == nil {
		return false
	}
	if !turn.terminalSuccess {
		return false
	}
	if turn.providerError != "" {
		return false
	}
	if strings.TrimSpace(turn.lastAssistantText) == "" {
		return false
	}
	if turn.toolAfterLastAssistant {
		return false
	}
	return true
}

// sessionHasNewCommit reports whether the session's checkout holds a commit
// of its own since provisioning: the local HEAD differs from the base the
// rescue recorded at provision. A read that fails counts as having a commit
// (fail closed: an unreadable checkout never reads as having done nothing,
// so the nudge is skipped rather than sent on unknown state). With no rescue
// target the session has no checkout to measure, so it reports none.
func (r *Runner) sessionHasNewCommit(ctx context.Context, res *Result, worktreePath string) bool {
	if res == nil || len(res.rescueTargets) == 0 {
		return false
	}
	target := res.rescueTargets[0]
	for _, t := range res.rescueTargets {
		if t.path == worktreePath {
			target = t
			break
		}
	}
	if strings.TrimSpace(target.base) == "" {
		return true
	}
	headCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	head, err := captureHeadSHA(headCtx, target.path)
	if err != nil || strings.TrimSpace(head) == "" {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(head), strings.TrimSpace(target.base))
}

// narrationNudgePromptFor renders the nudge prompt for the session: the fixed
// narration text plus the session's goal restatement when one exists, so the
// continuation never arrives as a bare note the seat can decline for lack of
// a goal. Empty goal keeps the bare prompt byte for byte.
func narrationNudgePromptFor(qw QueuedWork) string {
	goal := strings.TrimSpace(continueGoalBlock(qw))
	if goal == "" {
		return narrationNudgePrompt
	}
	return narrationNudgePrompt + "\n\n" + goal
}

// postNarrationNudgeActivity records the nudge as a session activity. It is
// best-effort like every other activity post: a refused endpoint never
// affects the run.
func (r *Runner) postNarrationNudgeActivity(ctx context.Context, sink activitySink) {
	if sink == nil {
		return
	}
	sink.Send(ctx, agent.SystemEvent{
		Subtype: narrationNudgeActivitySubtype,
		Message: narrationNudgeActivityMessage,
	})
}

// The nudge never fails the session on its own: once the budget is spent a
// further narration-only stop takes the generic continuation path and the
// existing continuation bounds (unproductive streak, ceiling, undelivered)
// decide exhaustion. A productive agent that ends several turns on a
// progress note keeps working; only the first such continuation carries the
// narration wording.
