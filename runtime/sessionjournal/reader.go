// Package sessionjournal reads the agent events recorded in a worktree's
// .agent/events.jsonl file.
package sessionjournal

import "github.com/RenseiAI/donmai/internal/journaltail"

// Entry is a decoded journal line with its session ID, zero-based byte offset,
// local read time, and replay flag. Err holds the diagnostic for a malformed
// line in place of Event.
type Entry = journaltail.Entry

// Reader follows the local journal for one run. It holds no open file between
// polls. Create a new Reader when the session, run start, journal path, or run
// offset changes, even if the worktree or session identifier is reused.
type Reader struct {
	tail *journaltail.Tailer
}

// NewReader attaches to path for sessionID. Pass the matching run's
// state.State.EventLogStartOffset to replay that run's entries. A nil,
// negative, or beyond-EOF offset skips existing data. If the journal does not
// exist yet, the reader follows it from its beginning when it appears.
func NewReader(sessionID, path string, startOffset *int64) *Reader {
	return &Reader{tail: journaltail.NewMetricTailer(sessionID, path, startOffset, nil)}
}

// Poll returns up to 256 complete entries in file order, including lines after
// a result event. Replay marks lines starting before attachment. An incomplete
// line is retained until its newline arrives. Missing files and idle journals
// yield no entries. A detected replacement or truncation stops delivery until
// the caller creates a reader for the new run. Poll reports file I/O errors
// separately from per-line decoding errors and can return entries with an
// I/O error.
func (r *Reader) Poll() ([]Entry, error) { return r.tail.Poll() }
