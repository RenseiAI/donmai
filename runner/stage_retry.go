package runner

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
)

// Mechanical retries under a dispatched stage retry policy (the 1.0 wire's
// `retry`). When the work item carries one it governs every retry the runner
// makes for a mechanical error:
//
//   - provider-error retries of a turn (turnFollowUps.retryLimit, with the
//     policy's fixed backoff in waitRetryBackoffFor);
//   - stall retries of a hung model request (the stall bound);
//   - provider spawn failures (spawnWithRetry below).
//
// Without a policy each keeps today's behaviour: the configured
// continuation limit and growing backoff for provider errors, the configured
// stall bound, and a single spawn attempt.

// spawnWithRetry spawns the provider, retrying a failed spawn up to the
// stage retry policy's retries with its backoff between attempts. With no
// policy it is exactly one Spawn call. A cancelled context ends the retries
// with the last spawn error.
func (r *Runner) spawnWithRetry(ctx context.Context, provider agent.Provider, spec agent.Spec, qw QueuedWork) (agent.Handle, error) {
	handle, err := provider.Spawn(ctx, spec)
	if err == nil || qw.Retry == nil {
		return handle, err
	}
	for attempt := 1; attempt <= qw.Retry.Retries; attempt++ {
		r.logger.Warn("provider spawn failed; retrying under the stage retry policy",
			"sessionId", qw.SessionID,
			"attempt", attempt,
			"retries", qw.Retry.Retries,
			"backoffSeconds", qw.Retry.BackoffSeconds,
			"err", err,
		)
		if waitErr := r.waitRetryBackoffFor(ctx, qw.Retry, attempt); waitErr != nil {
			return nil, err
		}
		if handle, err = provider.Spawn(ctx, spec); err == nil {
			return handle, nil
		}
	}
	return nil, err
}
