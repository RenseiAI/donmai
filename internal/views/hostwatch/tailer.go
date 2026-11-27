// Package hostwatch implements the local-stream fleet dashboard engine —
// a per-host, per-project live view of all agent work, sourced entirely
// from LOCAL data: the daemon's localhost control API (the live index of
// what is running) joined with each session's on-disk `.agent/events.jsonl`
// (the full-fidelity event stream) and `.agent/state.json` (the per-session
// header). It NEVER round-trips the platform.
//
// The engine is a PURE READER and is OUT-OF-BAND from execution: it opens
// every file read-only, hits only the daemon's read endpoints, spawns no
// child and holds no pipe to any agent. Killing the dashboard closes file
// handles and idle HTTP connections and exits — the daemon and every worker
// (separate processes under launchd/systemd) are untouched. This is the
// categorical fix over the legacy af-worker-fleet, whose SIGINT propagated
// to the worker children through the process group.
//
// Performance at tens of concurrent local projects is achieved by tailing
// (open once, seek to end, read only appended bytes) rather than re-reading,
// and by a single coalesced low-frequency index poll against an in-memory
// daemon snapshot — orders of magnitude lighter than the agents already
// running on the box.
package hostwatch

import (
	"strconv"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/journaltail"
)

// TailEvent is one decoded line from a session's events.jsonl. At is the
// local ingestion time (the events.jsonl line itself carries no timestamp —
// the runner appends the marshaled agent.Event verbatim, so the watcher
// uses read time). At must never drive liveness: replaying history would
// make an idle session look active now. Use EventAt for freshness — the
// event's own time where it carries one — and Replay to gate liveness
// updates off replayed history entirely.
type TailEvent struct {
	// source fences asynchronously read batches to the registered reader.
	// It is local bookkeeping only and never part of the event wire.
	source *Tailer
	// SessionID is the session whose events.jsonl produced this event.
	SessionID string
	// At is the local time the tailer read the line.
	At time.Time
	// Offset is the start byte of this line in the run's journal. It is
	// local-only dedup identity, absent on caller-constructed events.
	Offset *int64
	// Replay marks events that pre-date the watcher's attach: history
	// re-read from the top (scroll-back mode) rather than output observed
	// live. Replay events still feed the stream and cumulative metrics,
	// but never freshness timestamps.
	Replay bool
	// Event is the decoded agent event. Nil only when Err is set.
	Event agent.Event
	// Err is set when the line could not be decoded (malformed JSON or
	// unknown kind). The tailer surfaces it rather than silently dropping
	// so callers can render a diagnostic; the byte offset still advances.
	Err error
}

// EventAt returns the event's own time where it carries one, else the
// ingestion time. LlmCallEvent carries call-boundary nanosecond stamps;
// every other variant has no clock, so At is the only honest value.
func (e TailEvent) EventAt() time.Time {
	if call, ok := e.Event.(agent.LlmCallEvent); ok {
		if ts := parseUnixNano(firstNonEmpty(call.EndTimeUnixNano, call.StartTimeUnixNano)); !ts.IsZero() {
			return ts
		}
	}
	return e.At
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func parseUnixNano(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Tailer tracks one journal across dashboard refreshes.
type Tailer = journaltail.Tailer

const maxEventsPerPoll = journaltail.MaxEventsPerPoll

// NewTailer follows a journal until the terminal result, optionally replaying history.
func NewTailer(sessionID, path string, startAtEnd bool, now func() time.Time) *Tailer {
	return journaltail.NewTailer(sessionID, path, startAtEnd, now)
}

// NewMetricTailer follows the current run across terminal results and continuations.
func NewMetricTailer(sessionID, path string, startOffset *int64, now func() time.Time) *Tailer {
	return journaltail.NewMetricTailer(sessionID, path, startOffset, now)
}
