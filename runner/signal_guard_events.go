package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
)

// SystemSubtypeSignalGuardRefused is the SystemEvent subtype the runner
// emits for each pattern kill its pkill/killall guards refused: the agent
// asked to signal processes outside its session scope (neither inside the
// session's worktree nor in its process group) and the guard declined to
// signal them. Message carries the guard's refusal line verbatim so the
// session's activity stream shows what was attempted and what was spared.
// The activity poster forwards it as a context marker built from a fixed
// prefix plus that line.
const SystemSubtypeSignalGuardRefused = "signal_guard_refused"

// SignalGuardRefusedEvent builds the refusal event for one guard refusal
// line. Empty lines produce no event.
func SignalGuardRefusedEvent(line string) (agent.SystemEvent, bool) {
	if strings.TrimSpace(line) == "" {
		return agent.SystemEvent{}, false
	}
	return agent.SystemEvent{Subtype: SystemSubtypeSignalGuardRefused, Message: line}, true
}

// reportSignalGuardRefusals drains the session's guard refusal log into
// typed activity events. Each line the pkill/killall guards appended becomes
// one agent.SystemSubtypeSignalGuardRefused event on sink (which the activity
// poster forwards as a context marker) and is mirrored to events.jsonl for
// audit parity with the other runner-emitted markers. The log is removed
// after a successful drain so a retried session does not re-report; both legs
// are best-effort and never fail the run. eventsDir is the runner-owned
// state directory the events.jsonl mirror lands in (never the session
// checkout: in a read-only negotiated workarea the runner must not write
// into the selected repository).
func (r *Runner) reportSignalGuardRefusals(ctx context.Context, layout signalGuardLayout, eventsDir string, sink activitySink) {
	if layout.dir == "" || layout.eventLog == "" || sink == nil {
		return
	}
	body, err := os.ReadFile(layout.eventLog) //nolint:gosec // G304: runner-owned session path, never session wire input.
	if err != nil {
		return
	}
	reported := false
	for _, line := range strings.Split(string(body), "\n") {
		ev, ok := SignalGuardRefusedEvent(line)
		if !ok {
			continue
		}
		sink.Send(ctx, ev)
		if encoded, merr := agent.MarshalEvent(ev); merr == nil {
			r.appendJSONLLine(filepath.Join(eventsDir, state.AgentDirName, "events.jsonl"), encoded)
		}
		reported = true
	}
	if reported {
		_ = os.Remove(layout.eventLog)
	}
}
