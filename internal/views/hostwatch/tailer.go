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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

// Tailer follows a single `.agent/events.jsonl` file, emitting one
// TailEvent per appended line. It is append- and truncate-safe: a file
// that shrinks (copy-truncate logrotate, or a fresh run reusing the path)
// is detected via a size regression. Metric tailers refuse that incarnation
// until the card receives a new run fence; ordinary tailers retain replay.
//
// A Tailer is single-goroutine: call Poll repeatedly from one goroutine.
// It holds no open file handle between Poll calls — it opens, seeks to its
// last offset, drains, and closes each time — so a paused or off-screen
// session costs nothing but the periodic stat+open, and the watcher never
// pins a deleted inode.
type Tailer struct {
	sessionID string
	path      string
	now       func() time.Time

	mu     sync.Mutex
	offset int64 // byte offset already consumed
	// partial carries an incomplete trailing line (writer mid-append)
	// plus the offset that line started at, so its replay verdict is
	// exact once the line completes on a later poll.
	partial      []byte
	partialStart int64
	done         bool // ordinary tailers stop at ResultEvent; metric tailers continue
	// backlog is the byte length that pre-dates the watcher's attach:
	// construction-time file size in steady-state mode (startAtEnd), or
	// the size of the first observed content when the file did not yet
	// exist at construction. Bytes at offsets below backlog are history
	// re-read after attach and are flagged Replay so they never drive
	// liveness. Bytes at or above backlog were observed live.
	backlog int64
	primed  bool // backlog has been fixed for this file incarnation
	// replayAll marks scroll-back mode (startAtEnd=false): every byte
	// read predates the watcher's attach, so every event is history and
	// is flagged Replay. Steady-state tailers leave this false and use
	// the backlog boundary instead.
	replayAll     bool
	continuous    bool
	fileInfo      os.FileInfo
	awaitRunFence bool
	openFile      func(string) (*os.File, error) // test-local stat/open barrier; nil uses os.Open
	// attachModTime is the journal's modification time when a metric
	// tailer attached to bytes this run already wrote: the true time of the
	// run's last journal append before the watcher started. Zero otherwise.
	attachModTime time.Time
}

// maxEventsPerPoll bounds one continuous tailer Poll so a long backlog is
// replayed in slices rather than in one blocking read.
const maxEventsPerPoll = 256

// NewMetricTailer replays only the current run's journal bytes for metrics,
// then follows appends. Unknown run boundaries start at EOF to avoid
// attributing an older run's events to the current card.
func NewMetricTailer(sessionID, path string, startOffset *int64, now func() time.Time) *Tailer {
	t := NewTailer(sessionID, path, true, now)
	t.continuous = true
	if info, err := os.Stat(path); err == nil {
		t.fileInfo = info
		size := info.Size()
		t.backlog = size
		t.primed = true
		if startOffset != nil && *startOffset >= 0 && *startOffset <= size {
			t.offset = *startOffset
			if *startOffset < size {
				// The journal is append-only within a run, so its mtime is
				// when this run last wrote an event.
				t.attachModTime = info.ModTime()
			}
		}
	}
	return t
}

// AttachModTime returns the journal's modification time at attach when the
// current run had already written events, else zero. It is the honest last
// output time of a session whose earlier events the watcher only replays.
func (t *Tailer) AttachModTime() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attachModTime
}

// NewTailer constructs a Tailer for the events.jsonl at path, attributing
// emitted events to sessionID. now defaults to time.Now when nil.
//
// startAtEnd controls history handling. It means "skip the history that
// already existed when the watcher started" — NOT "skip whatever exists on
// the first Poll":
//
//   - true: if the file exists NOW, the tailer starts past its current
//     content (steady-state "show new output only"). If the file does NOT
//     yet exist, the tailer starts at offset 0, so a session whose
//     events.jsonl is created AFTER the watcher attaches is read in full —
//     all of it is genuinely new relative to watcher start. This is the
//     common case for a freshly-spawned session.
//   - false: the whole file is read from the top (the --replay / scroll-back
//     mode), including history.
//
// The construction-time stat keeps the semantics honest without a fragile
// lazy-seek that could skip a fresh session's opening events.
func NewTailer(sessionID, path string, startAtEnd bool, now func() time.Time) *Tailer {
	if now == nil {
		now = time.Now
	}
	t := &Tailer{sessionID: sessionID, path: path, now: now}
	if !startAtEnd {
		// Scroll-back mode reads the whole file from the top: all of it
		// is history relative to the watcher's attach.
		t.replayAll = true
		return t
	}
	if startAtEnd {
		if info, err := os.Stat(path); err == nil {
			// File exists now — skip its current content. The skipped
			// bytes are history, not live output.
			t.offset = info.Size()
			t.backlog = info.Size()
			t.primed = true
		}
		// File does not exist yet — leave offset 0 so all future content
		// (which is all post-start) is read.
	}
	return t
}

// SessionID returns the session this tailer is attributed to.
func (t *Tailer) SessionID() string { return t.sessionID }

// Done reports whether a terminal ResultEvent has been observed. A done
// tailer's Poll returns no further events; callers may drop it.
func (t *Tailer) Done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// Poll reads any bytes appended since the last call and returns the decoded
// events in order. It returns (nil, nil) when there is nothing new (or the
// file does not yet exist). A non-nil error indicates an I/O failure
// opening or reading the file (a missing file is NOT an error — it returns
// nil, nil); decode failures of individual lines are surfaced per-event via
// TailEvent.Err, not via the returned error, so one bad line never stalls
// the stream.
func (t *Tailer) Poll() ([]TailEvent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, nil
	}

	info, err := os.Stat(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hostwatch: stat %s: %w", t.path, err)
	}
	size := info.Size()
	if t.continuous && t.awaitRunFence {
		t.offset = size
		return nil, nil
	}
	if t.continuous && t.fileInfo != nil && !os.SameFile(info, t.fileInfo) {
		// A new journal inode may belong to a replacement run. Wait for the
		// next index snapshot to supply its run-start fence.
		t.fileInfo = info
		t.offset = size
		t.backlog = size
		t.partial = nil
		t.partialStart = 0
		t.awaitRunFence = true
		return nil, nil
	}
	// Truncation / rotation / reuse detection: the file shrank below our
	// consumed offset, so the bytes we were tracking are gone. Re-read from
	// the top and discard any half-line we were carrying. A reused path is
	// a new incarnation: the bytes present now appeared while this
	// incarnation was unwatched, so they are history (replay) and only
	// later appends read as live.
	if size < t.offset {
		if t.continuous {
			// Copy-truncation can also be a new run. Do not replay its old
			// bytes under the current card's session/run identity.
			t.offset = size
			t.backlog = size
			t.partial = nil
			t.partialStart = 0
			t.awaitRunFence = true
			return nil, nil
		}
		t.offset = 0
		t.partial = nil
		t.partialStart = 0
		t.backlog = size
		t.primed = true
	}
	if size == t.offset {
		// Prime the backlog boundary on first contact with content: in
		// steady-state mode the construction-time stat may have missed a
		// file created later, so the first observed size is the history
		// cutoff — everything below it is replay, everything appended
		// after is live. In scroll-back mode the boundary stays 0 and
		// every event reads as replay.
		if !t.primed {
			t.backlog = size
			t.primed = true
		}
		return nil, nil // no new bytes
	}

	open := t.openFile
	if open == nil {
		open = os.Open
	}
	//nolint:gosec // G304: path comes from the daemon-owned worktree layout,
	// not user input, and is opened read-only.
	f, err := open(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hostwatch: open %s: %w", t.path, err)
	}
	defer func() { _ = f.Close() }()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("hostwatch: inspect opened journal: %w", err)
	}
	// A writer may append to the same inode between path Stat and f.Stat.
	// Growth is ordinary live output; shrinking, replacement, or changed
	// same-size content cannot be attributed to this run safely.
	if !os.SameFile(info, openedInfo) || openedInfo.Size() < info.Size() ||
		(openedInfo.Size() == info.Size() && !openedInfo.ModTime().Equal(info.ModTime())) ||
		openedInfo.Size() < t.offset ||
		(t.continuous && t.fileInfo != nil && !os.SameFile(t.fileInfo, openedInfo)) {
		if t.continuous {
			t.fileInfo = openedInfo
			t.offset = openedInfo.Size()
			t.backlog = openedInfo.Size()
			t.partial = nil
			t.partialStart = 0
			t.awaitRunFence = true
		}
		return nil, nil // never read a different or truncated opened inode
	}
	t.fileInfo = openedInfo

	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("hostwatch: seek %s: %w", t.path, err)
	}

	r := bufio.NewReader(f)
	var out []TailEvent
	for {
		chunk, readErr := r.ReadBytes('\n')
		if len(chunk) > 0 {
			lineStart := t.offset
			t.offset += int64(len(chunk))
			if chunk[len(chunk)-1] == '\n' {
				// Complete line: prepend any carried partial.
				line := chunk[:len(chunk)-1]
				if len(t.partial) > 0 {
					line = append(t.partial, line...)
					lineStart = t.partialStart
					t.partial = nil
					t.partialStart = 0
				}
				if ev, ok := t.decode(line, lineStart); ok {
					ev.Replay = t.replayAll || t.isReplay(lineStart)
					out = append(out, ev)
					if !t.continuous && isTerminal(ev.Event) {
						t.done = true
						return out, nil
					}
					if t.continuous && len(out) >= maxEventsPerPoll {
						return out, nil
					}
				}
			} else {
				// Incomplete trailing line (writer mid-append): carry it.
				if len(t.partial) == 0 {
					t.partialStart = lineStart
				}
				t.partial = append(t.partial, chunk...)
			}
		}
		if readErr != nil {
			// io.EOF is the normal stop; any other read error stops too,
			// but we keep what we decoded (the offset advanced for the
			// bytes we consumed).
			break
		}
	}
	return out, nil
}

// decode turns one trimmed line into a TailEvent. Blank lines are skipped
// (ok=false). Decode failures yield ok=true with Err set so callers can
// render a diagnostic without losing stream position.
func (t *Tailer) decode(line []byte, offset int64) (TailEvent, bool) {
	// Trim a trailing CR for CRLF-written files.
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if len(line) == 0 {
		return TailEvent{}, false
	}
	ev, err := agent.UnmarshalEvent(line)
	te := TailEvent{SessionID: t.sessionID, At: t.now(), Offset: &offset}
	if err != nil {
		te.Err = fmt.Errorf("hostwatch: decode event: %w", err)
		return te, true
	}
	te.Event = ev
	return te, true
}

// isReplay reports whether a line starting at byte offset start predates
// the watcher's attach. Offsets below the primed backlog boundary are
// history (scroll-back mode, pre-existing content, or a reused path's
// earlier incarnation); offsets at or above it were observed live.
func (t *Tailer) isReplay(start int64) bool {
	if start < 0 {
		return true
	}
	return start < t.backlog
}

// isTerminal reports whether ev is the session's terminal ResultEvent.
func isTerminal(ev agent.Event) bool {
	if ev == nil {
		return false
	}
	_, ok := ev.(agent.ResultEvent)
	return ok
}
