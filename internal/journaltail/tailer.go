// Package journaltail reads newline-delimited local session events.
package journaltail

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

// Entry is a complete journal line and its position within the file.
type Entry struct {
	// SessionID is the identifier supplied by the caller.
	SessionID string
	// At is local read time, including for replayed entries.
	At time.Time
	// Offset is the zero-based byte position of the beginning of the line.
	Offset int64
	// Replay is true for lines beginning before the attach-time file boundary.
	Replay bool
	// Event is the decoded event, or nil if the line could not be decoded.
	Event agent.Event
	// Err describes an invalid event. Subsequent lines are still delivered.
	Err error
}

// Tailer follows a single `.agent/events.jsonl` file, emitting one
// Entry per appended line. It is append- and truncate-safe: a file
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

// MaxEventsPerPoll bounds one continuous tailer Poll so a long backlog is
// replayed in slices rather than in one blocking read.
const MaxEventsPerPoll = 256

// NewMetricTailer replays only the current run's journal bytes for metrics,
// then follows appends. Unknown run boundaries start at EOF to avoid
// attributing an older run's events to the current card.
func NewMetricTailer(sessionID, path string, startOffset *int64, now func() time.Time) *Tailer {
	t := newTailer(sessionID, path, now)
	t.continuous = true
	if info, err := os.Stat(path); err == nil {
		t.fileInfo = info
		size := info.Size()
		t.offset = size
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
	t := newTailer(sessionID, path, now)
	if !startAtEnd {
		// Scroll-back mode reads the whole file from the top: all of it
		// is history relative to the watcher's attach.
		t.replayAll = true
		return t
	}
	if info, err := os.Stat(path); err == nil {
		// File exists now — skip its current content. The skipped
		// bytes are history, not live output.
		t.offset = info.Size()
		t.backlog = info.Size()
		t.primed = true
	}
	return t
}

func newTailer(sessionID, path string, now func() time.Time) *Tailer {
	if now == nil {
		now = time.Now
	}
	return &Tailer{sessionID: sessionID, path: path, now: now}
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
// Entry.Err, not via the returned error, so one bad line never stalls
// the stream.
func (t *Tailer) Poll() ([]Entry, error) {
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
		return nil, fmt.Errorf("sessionjournal: stat %s: %w", t.path, err)
	}
	size := info.Size()
	if t.continuous && t.awaitRunFence {
		t.offset = size
		return nil, nil
	}
	if t.continuous && t.fileInfo != nil && (!os.SameFile(info, t.fileInfo) || size < t.fileInfo.Size()) {
		// A replacement inode or a shrink anywhere in the observed file,
		// even inside unread backlog, can mean the worktree was reused.
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
	// The caller selects the local journal. The reader only opens it read-only.
	f, err := open(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionjournal: open %s: %w", t.path, err)
	}
	defer func() { _ = f.Close() }()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sessionjournal: inspect opened journal: %w", err)
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
		return nil, fmt.Errorf("sessionjournal: seek %s: %w", t.path, err)
	}

	r := bufio.NewReader(f)
	var out []Entry
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
					if t.continuous && len(out) >= MaxEventsPerPoll {
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
			if !errors.Is(readErr, io.EOF) {
				return out, fmt.Errorf("sessionjournal: read %s: %w", t.path, readErr)
			}
			break
		}
	}
	return out, nil
}

// decode turns one trimmed line into an Entry. Blank lines are skipped
// (ok=false). Decode failures yield ok=true with Err set so callers can
// render a diagnostic without losing stream position.
func (t *Tailer) decode(line []byte, offset int64) (Entry, bool) {
	// Trim a trailing CR for CRLF-written files.
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if len(line) == 0 {
		return Entry{}, false
	}
	ev, err := agent.UnmarshalEvent(line)
	te := Entry{SessionID: t.sessionID, At: t.now(), Offset: offset}
	if err != nil {
		te.Err = fmt.Errorf("sessionjournal: decode event: %w", err)
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
