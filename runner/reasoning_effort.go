package runner

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
)

// recordReasoningEffort records the reasoning effort the session was spawned
// with. It sends one agent.SystemSubtypeReasoningEffort event to sink (the
// activity poster forwards it as a context marker) and persists the same fact
// to state.json. Both are best-effort: a failed state write is logged and the
// run continues.
func (r *Runner) recordReasoningEffort(ctx context.Context, statePath, sessionID string, effort agent.EffortLevel, sink activitySink) {
	sink.Send(ctx, agent.ReasoningEffortEvent(effort))
	if _, err := r.store.Update(statePath, func(s *state.State) error {
		s.ReasoningEffort = stateReasoningEffort(effort)
		return nil
	}); err != nil {
		r.logger.Warn("state reasoning-effort update failed", "sessionId", sessionID, "err", err)
	}
}

// stateReasoningEffort is the state.json value for a spawned effort.
func stateReasoningEffort(effort agent.EffortLevel) string {
	if effort == "" {
		return state.ReasoningEffortNotConfigured
	}
	return string(effort)
}
