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
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TailEvent is one decoded line from a session's events.jsonl, stamped
// with the local ingestion time (the events.jsonl line itself carries no
// timestamp — the runner appends the marshaled agent.Event verbatim, so
// the watcher uses read time, exactly as the legacy af-worker-fleet did
// for its multiplexed feed).
type TailEvent struct {
	// SessionID is the session whose events.jsonl produced this event.
	SessionID string
	// At is the local time the tailer read the line.
	At time.Time
	// Event is the decoded agent event. Nil only when Err is set.
	Event agent.Event
	// Err is set when the line could not be decoded (malformed JSON or
	// unknown kind). The tailer surfaces it rather than silently dropping
	// so callers can render a diagnostic; the byte offset still advances.
	Err error
}

// Tailer follows a single `.agent/events.jsonl` file, emitting one
// TailEvent per appended line. It is append- and truncate-safe: a file
// that shrinks (copy-truncate logrotate, or a fresh run reusing the path)
// is detected via a size regression and re-read from the top.
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

	mu      sync.Mutex
	offset  int64  // byte offset already consumed
	partial []byte // bytes of an incomplete trailing line carried to next Poll
	done    bool   // a terminal ResultEvent was seen; Poll is a no-op after
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
	if startAtEnd {
		if info, err := os.Stat(path); err == nil {
			// File exists now — skip its current content.
			t.offset = info.Size()
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

	// Truncation / rotation / reuse detection: the file shrank below our
	// consumed offset, so the bytes we were tracking are gone. Re-read from
	// the top and discard any half-line we were carrying.
	if size < t.offset {
		t.offset = 0
		t.partial = nil
	}
	if size == t.offset {
		return nil, nil // no new bytes
	}

	//nolint:gosec // G304: path comes from the daemon-owned worktree layout,
	// not user input, and is opened read-only.
	f, err := os.Open(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hostwatch: open %s: %w", t.path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("hostwatch: seek %s: %w", t.path, err)
	}

	r := bufio.NewReader(f)
	var out []TailEvent
	for {
		chunk, readErr := r.ReadBytes('\n')
		if len(chunk) > 0 {
			t.offset += int64(len(chunk))
			if chunk[len(chunk)-1] == '\n' {
				// Complete line: prepend any carried partial.
				line := chunk[:len(chunk)-1]
				if len(t.partial) > 0 {
					line = append(t.partial, line...)
					t.partial = nil
				}
				if ev, ok := t.decode(line); ok {
					out = append(out, ev)
					if isTerminal(ev.Event) {
						t.done = true
						return out, nil
					}
				}
			} else {
				// Incomplete trailing line (writer mid-append): carry it.
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
func (t *Tailer) decode(line []byte) (TailEvent, bool) {
	// Trim a trailing CR for CRLF-written files.
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if len(line) == 0 {
		return TailEvent{}, false
	}
	ev, err := agent.UnmarshalEvent(line)
	te := TailEvent{SessionID: t.sessionID, At: t.now()}
	if err != nil {
		te.Err = fmt.Errorf("hostwatch: decode event: %w", err)
		return te, true
	}
	te.Event = ev
	return te, true
}

// isTerminal reports whether ev is the session's terminal ResultEvent.
func isTerminal(ev agent.Event) bool {
	if ev == nil {
		return false
	}
	_, ok := ev.(agent.ResultEvent)
	return ok
}
