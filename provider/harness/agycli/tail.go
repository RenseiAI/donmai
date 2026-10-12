package agycli

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"os"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// defaultTailInterval is how often the transcript tailer polls for conv-id
// discovery and newly-appended transcript lines while the subprocess runs.
const defaultTailInterval = 250 * time.Millisecond

// transcriptTailer streams agy's on-disk transcript.jsonl DURING the run.
//
// agy emits tool structure only to its transcript file (stdout is plain
// prose), so without tailing, ToolUse/ToolResult events would only surface
// after the subprocess exits (an EOF replay). The tailer closes that gap: it
// polls for the conversation id created by this run (diffing against the
// pre-spawn snapshot), then incrementally drains transcript.jsonl as agy
// appends to it, emitting structured events live. The final catch-up drain
// (after the subprocess exits) replaces the EOF replay outright — the tailer
// is the only transcript emitter, so no event is ever duplicated.
//
// Best-effort like the rest of transcript enrichment: every failure path
// degrades silently to the stdout spine.
//
// Not safe for concurrent use: all methods run on the single tailer
// goroutine (run), except the synchronous whole-file use in
// parseTranscriptFile.
type transcriptTailer struct {
	stateHome string
	cwd       string
	before    map[string]struct{} // pre-spawn conv-id snapshot (read-only)
	emit      func(agent.Event)
	onConvID  func(string) // invoked once, when discovery succeeds
	interval  time.Duration

	path   string // transcript path; set once the conv-id is discovered
	offset int64  // bytes of the transcript consumed so far
	carry  []byte // trailing partial line awaiting its newline
	st     transcriptState

	// seen holds an FNV-1a hash per consumed line so a truncate-recovery
	// re-read can skip exactly the lines already emitted. replaying is set
	// when the file shrank and the re-read is still matching that prefix.
	seen      []uint64
	replay    int
	replaying bool
}

// run polls until stop closes, then performs one final catch-up drain (the
// subprocess has exited by then, so the transcript is complete). The caller
// waits for run to return before emitting the terminal ResultEvent so every
// transcript event precedes it.
func (t *transcriptTailer) run(stop <-chan struct{}) {
	interval := t.interval
	if interval <= 0 {
		interval = defaultTailInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	t.poll(false)
	for {
		select {
		case <-stop:
			t.poll(true)
			return
		case <-ticker.C:
			t.poll(false)
		}
	}
}

// poll discovers the conversation id (once) and drains any newly-appended
// transcript bytes. final marks the post-exit catch-up pass: it flushes a
// trailing unterminated line instead of carrying it.
func (t *transcriptTailer) poll(final bool) {
	if t.path == "" {
		if t.stateHome == "" {
			return
		}
		convID, ok := discoverConvID(t.stateHome, t.cwd, t.before)
		if !ok {
			return
		}
		t.path = transcriptPath(t.stateHome, convID)
		if t.onConvID != nil {
			t.onConvID(convID)
		}
	}
	t.drain(final)
}

// drain reads bytes appended since the last drain, splits them into lines
// (carrying a trailing partial line across calls), and emits each line's
// events via the shared transcriptState so use↔result pairing works across
// incremental reads exactly as it does for a whole-file parse.
func (t *transcriptTailer) drain(final bool) {
	fi, err := os.Stat(t.path)
	if err != nil {
		return
	}
	if fi.Size() < t.offset {
		// The file shrank — agy rewrote it. Re-read from the start,
		// skipping lines whose content matches what was already consumed
		// (hash-verified, so a replaced line still surfaces). Pairing
		// state resets with the replay; results paired across the reset
		// degrade to an empty ToolUseID (best-effort).
		t.replay, t.replaying = 0, true
		t.offset, t.carry, t.st = 0, nil, transcriptState{}
	}
	if fi.Size() == t.offset && (!final || len(t.carry) == 0) {
		return // nothing new
	}

	f, err := os.Open(t.path) //nolint:gosec // path derived from config-discovered conv-id
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if t.offset > 0 {
		if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
			return
		}
	}
	data, err := io.ReadAll(f)
	if len(data) == 0 && err != nil {
		return
	}
	t.offset += int64(len(data))

	buf := data
	if len(t.carry) > 0 {
		joined := make([]byte, 0, len(t.carry)+len(data))
		joined = append(append(joined, t.carry...), data...)
		buf = joined
		t.carry = nil
	}
	for {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			break
		}
		t.consumeLine(buf[:idx])
		buf = buf[idx+1:]
	}
	if final {
		// Scanner parity: a trailing line without a newline still counts.
		t.consumeLine(buf)
		return
	}
	t.carry = append([]byte(nil), buf...)
}

// consumeLine parses one complete transcript line and emits its events.
// Unparseable lines are skipped rather than aborting enrichment.
func (t *transcriptTailer) consumeLine(raw []byte) {
	raw = bytes.TrimSuffix(raw, []byte{'\r'})
	if len(raw) == 0 {
		return
	}
	h := hashLine(raw)
	if t.replaying {
		if t.replay < len(t.seen) && t.seen[t.replay] == h {
			t.replay++
			return // already emitted before the rewrite
		}
		// Diverged from (or replayed past) the consumed prefix: emit from
		// here on; drop the stale hashes beyond the verified prefix.
		t.replaying = false
		t.seen = t.seen[:t.replay]
	}
	t.seen = append(t.seen, h)
	line := append([]byte(nil), raw...)
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return
	}
	for _, ev := range t.st.mapLine(tl, line) {
		t.emit(ev)
	}
}

// hashLine is the FNV-1a content hash used for truncate-recovery dedupe.
func hashLine(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
