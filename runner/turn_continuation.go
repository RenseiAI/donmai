package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
)

// Turn continuation — tail recovery for a turn that ended before the work was
// finished.
//
// A harness can end a turn without the agent being done: the model writes a
// one-line progress note and makes no tool call, often right after a model
// gateway returned a 503 or 504. Treating that turn end as the end of the work
// sent the one "open a pull request" nudge, and a second early stop left the
// session with its work unfinished. For work that owes a pull request the
// runner now reads the latest turn's ending and follows up:
//
//   - the turn left a result — a turn-result manifest, a verified pull
//     request, or an anchored verdict — or the runner recorded a failure:
//     nothing to continue. A blocked or failed verdict ends the work; a
//     passed or unknown one without a pull request gets the pull request
//     nudge (steering), once, as before.
//   - the turn ended on a model provider error (agent.SystemSubtypeProviderError
//     with no recovery later in the turn): it is RETRIED — a short wait, then
//     "continue from where you stopped" — never nudged to open a pull request.
//   - otherwise the turn stopped early: it gets a short "continue the task"
//     prompt.
//
// Continuations and retries are each bounded by Options.TurnContinuationLimit
// (default DefaultTurnContinuationLimit). A turn that still ends unfinished
// once its bound is reached fails the session — FailureContinuationsExhausted
// for early stops, FailureProviderError for provider errors — and the
// unfinished work is not published by steering or the backstop. The counts
// ride Result.TurnContinuations onto the terminal status.

// DefaultTurnContinuationLimit is the per-kind follow-up bound applied when
// Options.TurnContinuationLimit is zero.
const DefaultTurnContinuationLimit = 3

// defaultProviderRetryBackoff spaces the retries after a provider error. The
// harness has usually retried the model call itself before giving up, so the
// runner waits a little longer each time before asking again.
func defaultProviderRetryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 10 * time.Second
}

// turnEnding is how the latest turn of work that owes a pull request ended.
type turnEnding int

const (
	// turnFinished: the turn left a result, the runner recorded a failure,
	// or the turn did not end cleanly — there is nothing to continue.
	turnFinished turnEnding = iota
	// turnStoppedEarly: a clean end with no turn-result manifest, no
	// verified pull request and no verdict.
	turnStoppedEarly
	// turnProviderError: the turn ended on a model provider error.
	turnProviderError
)

// classifyTurnEnding reads the latest turn's ending. session is the running
// observation (its blocked flag carries a decline from any turn); turn is the
// latest turn's own observation; res carries the manifest, the verified pull
// request and the verdict resolved so far. reportedPR is set when the turn
// carried a pull request URL on the session's own repository that did not
// verify: the agent reported a pull request, so the turn did not stop early,
// and the nudge and the backstop settle it instead.
func classifyTurnEnding(res *Result, session, turn streamObservation, reportedPR bool) turnEnding {
	switch {
	case res.FailureMode != "" || res.Status == "failed":
		return turnFinished
	case res.Manifest != nil, res.PullRequestURL != "", reportedPR, session.blocked, res.WorkResult == "failed":
		return turnFinished
	case turn.errorEvent != nil || turn.terminalEvent == nil:
		return turnFinished
	case turn.providerError != "":
		return turnProviderError
	case !turn.terminalSuccess, turn.verdict() != "":
		return turnFinished
	default:
		return turnStoppedEarly
	}
}

// tailStep is the next tail-recovery action.
type tailStep int

const (
	tailDone tailStep = iota
	tailContinue
	tailRetry
	tailSteer
	tailExhausted
)

// turnFollowUps is one session's runner-driven follow-up state.
type turnFollowUps struct {
	limit     int
	continued int
	retried   int
	steered   bool
	// exhausted records the ending that ran out of follow-ups.
	exhausted     bool
	exhaustedBy   turnEnding
	providerError string
}

// newTurnFollowUps resolves the configured bound: zero is the default and a
// negative value disables continuations and retries.
func (r *Runner) newTurnFollowUps() *turnFollowUps {
	limit := r.turnContinuationLimit
	switch {
	case limit == 0:
		limit = DefaultTurnContinuationLimit
	case limit < 0:
		limit = 0
	}
	return &turnFollowUps{limit: limit}
}

// next decides the next step for a turn that ended as ending. continuable is
// false when this session cannot be continued at all (work that owes no pull
// request, or a harness that takes no follow-up prompt); canSteer reports
// whether the pull request nudge's own conditions hold.
func (f *turnFollowUps) next(ending turnEnding, continuable, canSteer bool) tailStep {
	if continuable && f.limit > 0 {
		switch ending {
		case turnProviderError:
			if f.retried >= f.limit {
				f.exhausted, f.exhaustedBy = true, ending
				return tailExhausted
			}
			return tailRetry
		case turnStoppedEarly:
			if f.continued >= f.limit {
				f.exhausted, f.exhaustedBy = true, ending
				return tailExhausted
			}
			return tailContinue
		}
	}
	if canSteer && !f.steered {
		return tailSteer
	}
	return tailDone
}

// fail ends an exhausted session as failed, never relabelling a failure the
// runner already recorded.
func (f *turnFollowUps) fail(res *Result) {
	if res.FailureMode != "" {
		return
	}
	res.Status = "failed"
	if f.exhaustedBy == turnProviderError {
		res.FailureMode = FailureProviderError
		res.Error = fmt.Sprintf("the turn still ended on a model provider error after %d retries: %s", f.retried, f.providerError)
		return
	}
	res.FailureMode = FailureContinuationsExhausted
	res.Error = fmt.Sprintf("the turn still ended unfinished after %d continuation prompts: no turn-result manifest, no pull request and no verdict", f.continued)
}

// report is the counts for Result.TurnContinuations; nil when no follow-up
// of either kind was needed.
func (f *turnFollowUps) report() *agent.TurnContinuations {
	if f.continued == 0 && f.retried == 0 && !f.exhausted {
		return nil
	}
	return &agent.TurnContinuations{Continued: f.continued, Retried: f.retried, Limit: f.limit, Exhausted: f.exhausted}
}

// continuePrompt is the short prompt sent after a turn that stopped early.
const continuePrompt = "Your previous turn ended before the task was finished: it left no turn result, " +
	"no pull request, and no blocked or failed verdict. Continue the task from where you stopped. " +
	"When the work is done, commit it, push the branch, open the pull request and report the result. " +
	"If you cannot go on, end with an explicit blocked verdict and the reason."

// retryPrompt is sent after a turn that ended on a model provider error.
const retryPrompt = "Your previous turn was cut off by a model provider error before the task was finished. " +
	"Continue the task from where you stopped."

// waitRetryBackoff waits before the attempt-th provider-error retry, or
// returns the context's error when it ends first.
func (r *Runner) waitRetryBackoff(ctx context.Context, attempt int) error {
	backoff := r.providerRetryBackoff
	if backoff == nil {
		backoff = defaultProviderRetryBackoff
	}
	delay := backoff(attempt)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// deliverFollowUp sends text as the session's next turn: through
// Handle.Inject, or — when the live handle rejects the inject as unsupported
// and the harness declares session resume — through stop-and-resume. It
// returns the handle to keep draining and whether the prompt was delivered;
// an undelivered prompt ends the follow-ups.
func (r *Runner) deliverFollowUp(
	ctx context.Context,
	provider agent.Provider,
	handle agent.Handle,
	spec agent.Spec,
	caps agent.Capabilities,
	qw QueuedWork,
	text string,
) (agent.Handle, bool, error) {
	switch err := handle.Inject(ctx, text); {
	case err == nil:
		return handle, true, nil
	case errors.Is(err, agent.ErrUnsupported) && caps.SupportsSessionResume:
		return r.resumeWithDirective(ctx, provider, handle, spec, qw, text, nil)
	case errors.Is(err, agent.ErrUnsupported), errors.Is(err, clijsonl.ErrSessionNotReady), errors.Is(err, clijsonl.ErrInjectInFlight):
		return handle, false, nil
	default:
		return handle, false, fmt.Errorf("inject: %w", err)
	}
}
