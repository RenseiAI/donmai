package runner

import (
	"errors"

	"github.com/RenseiAI/donmai/runtime/state"
)

// errHeartbeatSessionMismatch fences the heartbeat observation write: the
// on-disk state belongs to a different session or run than the pulser that
// just acked, so the tick must not advance LastHeartbeat. Returned from the
// Store.Update callback (which then skips the write) and swallowed by the
// observer — a fence trip is routine (reused path, stale run), not a failure.
var errHeartbeatSessionMismatch = errors.New("runner: heartbeat session mismatch")

// heartbeatAckObserver builds the heartbeat.NewWithAckObserver callback
// that persists the actual successful session-heartbeat acknowledgement into
// state.State.LastHeartbeat — the field host-watch reads.
//
// The callback fires once per acknowledged tick with the exact tick snapshot
// the pulser stored in LastTick. It performs one session/run-fenced
// Store.Update: the write lands only when the on-disk state carries this
// exact session id and this run's positive startedAt. A
// reused worktree path still holding another session's state, or a stale run
// for the same session, trips errHeartbeatSessionMismatch and leaves
// LastHeartbeat untouched — failed refreshes, replay ingestion, output-only
// activity and unrelated host heartbeats never reach this callback at all
// because the pulser only invokes it on a successful lock-refresh ack.
//
// Best-effort by design: a persistence error is logged and dropped, never
// fatal to the run. A nil store, empty state path or empty session id yields
// nil (observation disabled) so callers wire it unconditionally.
func (r *Runner) heartbeatAckObserver(statePath, sessionID string, startedAt int64) func(int64) {
	if r == nil || r.store == nil || statePath == "" || sessionID == "" {
		return nil
	}
	store := r.store
	logger := r.logger
	return func(tick int64) {
		if tick <= 0 {
			return
		}
		_, err := store.Update(statePath, func(s *state.State) error {
			if s.SessionID != sessionID {
				return errHeartbeatSessionMismatch
			}
			if startedAt <= 0 || s.StartedAt != startedAt {
				return errHeartbeatSessionMismatch
			}
			if tick > s.LastHeartbeat {
				s.LastHeartbeat = tick
			}
			return nil
		})
		if err == nil {
			return
		}
		if errors.Is(err, errHeartbeatSessionMismatch) {
			if logger != nil {
				logger.Debug("heartbeat ack fenced: on-disk state belongs to another session or run",
					"sessionId", sessionID)
			}
			return
		}
		if logger != nil {
			logger.Warn("heartbeat ack persist failed", "sessionId", sessionID, "err", err)
		}
	}
}
