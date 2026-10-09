package pi

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Interactive transcript tailing.
//
// An interactive pi session writes its own session transcript as JSONL under
// the per-session state dir (`--session-dir`, i.e. layout.root — pi appends
// one `<timestamp>_<id>.jsonl` file per session directly under it, older
// builds under a `sessions/` subdirectory). The headless RPC lane maps live
// wire events to agent events; the interactive PTY lane has no wire — so this
// file tails those transcript files and maps each new entry to the SAME agent
// event types the headless mapper emits (assistant text, tool calls, tool
// results, per-turn usage), which the interactive runner forwards through the
// existing activity sink.
//
// Ownership: the state dir is shared. Headless runs use the same directory,
// it survives between sessions, and shared-mode and retained workareas reuse
// it, so it routinely holds transcripts this session did not write. The
// tailer therefore snapshots every transcript present BEFORE the child is
// spawned and never reads those files, not even lines appended to them later:
// only transcripts created after spawn (this session's own, including any a
// human starts from inside the TUI) are tailed. Among those, files with
// unread bytes are visited newest first, so a backlog elsewhere cannot starve
// the live transcript. Known limits: a pre-spawn transcript resumed from
// inside the TUI is not tailed, and a second session started in the same
// checkout after this one cannot be told apart by file.
//
// Posture: best-effort and rate-limited, never blocking the PTY. The tailer
// runs on its own goroutine, polls on a 1s ticker, caps files / lines /
// events / bytes per sweep, and emits via a non-blocking send — when the
// consumer is slow, transcript events are dropped, never queued behind the
// terminal. The caps defer work to the next sweep rather than discarding it:
// a sweep stops BEFORE the line that would exceed a cap, leaving it unread.
const (
	// interactiveTranscriptPollInterval is the cadence at which the tailer
	// re-scans the state dir. One second keeps activities near-live without
	// measurable IO pressure (a stat + occasional bounded read per file).
	interactiveTranscriptPollInterval = time.Second

	// interactiveTranscriptMaxFilesPerSweep bounds how many transcript files
	// one sweep opens. A session normally owns exactly one; only files with
	// unread bytes count against the cap.
	interactiveTranscriptMaxFilesPerSweep = 8

	// interactiveTranscriptMaxLinesPerSweep bounds how many new JSONL lines
	// one sweep consumes. A burst beyond it is consumed on later ticks.
	interactiveTranscriptMaxLinesPerSweep = 200

	// interactiveTranscriptMaxEventsPerSweep bounds how many agent events one
	// sweep emits — the rate limit that keeps a chatty session from
	// flooding the activity buffer. The line that reaches the cap is mapped
	// whole (one line can carry several tool calls); later lines wait for
	// the next tick.
	interactiveTranscriptMaxEventsPerSweep = 50

	// interactiveTranscriptMaxReadBytes bounds one file read per sweep so a
	// huge single-tick append cannot spike memory; the remainder is picked
	// up on the next tick. A single line longer than this cannot be parsed
	// within the bound, so it is skipped through its newline instead of
	// wedging the tail.
	interactiveTranscriptMaxReadBytes = 4 << 20

	// interactiveTranscriptMaxFinalSweeps bounds the end-of-session flush.
	// Trailing writes that landed with the child's exit may exceed one
	// sweep's caps; the flush keeps sweeping until nothing is pending or
	// this many sweeps have run.
	interactiveTranscriptMaxFinalSweeps = 16
)

// interactiveTranscriptTailer tracks the pre-spawn transcript baseline,
// per-file read state, and already-seen transcript entry IDs across sweeps.
// It is not safe for concurrent use; the single tailer goroutine owns it.
type interactiveTranscriptTailer struct {
	stateDir string
	// baseline holds every transcript that existed before the session was
	// spawned. Those belong to earlier or concurrent sessions and are never
	// read.
	baseline map[string]struct{}
	files    map[string]*interactiveTranscriptFile
	seen     map[string]struct{}
}

// interactiveTranscriptFile is the read state of one tailed transcript.
type interactiveTranscriptFile struct {
	offset int64
	// skipping is set while the tail is inside a line longer than
	// interactiveTranscriptMaxReadBytes; bytes are discarded through the
	// line's newline.
	skipping bool
}

// newInteractiveTranscriptTailer snapshots the transcripts already present
// under stateDir. Construct it BEFORE spawning the child: a file the child
// creates before the snapshot would otherwise be mistaken for an earlier
// session's and never tailed.
func newInteractiveTranscriptTailer(stateDir string) *interactiveTranscriptTailer {
	t := &interactiveTranscriptTailer{
		baseline: make(map[string]struct{}),
		files:    make(map[string]*interactiveTranscriptFile),
		seen:     make(map[string]struct{}),
	}
	if stateDir == "" {
		return t
	}
	t.stateDir = filepath.Clean(stateDir)
	walkInteractiveTranscriptFiles(t.stateDir, func(path string, _ os.DirEntry) {
		t.baseline[path] = struct{}{}
	})
	return t
}

// run polls until sessionDone closes, emitting newly mapped transcript events
// via emit, then flushes whatever the child wrote on its way out. Every emit
// must be non-blocking — this goroutine never slows the PTY, and it returns
// once the flush is done whether or not anyone reads the events.
func (t *interactiveTranscriptTailer) run(sessionDone <-chan struct{}, emit func(agent.Event)) {
	ticker := time.NewTicker(interactiveTranscriptPollInterval)
	defer ticker.Stop()
	for {
		events, _ := t.sweep()
		for _, ev := range events {
			emit(ev)
		}
		select {
		case <-sessionDone:
			t.flush(emit)
			return
		case <-ticker.C:
		}
	}
}

// flush sweeps until nothing is pending, bounded by
// interactiveTranscriptMaxFinalSweeps.
func (t *interactiveTranscriptTailer) flush(emit func(agent.Event)) {
	for range interactiveTranscriptMaxFinalSweeps {
		events, more := t.sweep()
		for _, ev := range events {
			emit(ev)
		}
		if !more {
			return
		}
	}
}

// sweep reads newly appended lines from this session's transcripts and maps
// them to agent events in file order. It reports more=true when a cap
// stopped it with input still pending. Unreadable files, torn trailing
// lines, and unparsable lines are skipped without moving the offsets that
// guard them, so one bad read never wedges the tail nor replays it.
func (t *interactiveTranscriptTailer) sweep() (events []agent.Event, more bool) {
	if t.stateDir == "" {
		return nil, false
	}
	lines := 0
	accept := func(line []byte) bool {
		if lines >= interactiveTranscriptMaxLinesPerSweep || len(events) >= interactiveTranscriptMaxEventsPerSweep {
			return false
		}
		lines++
		events = append(events, mapInteractiveTranscriptLine(line, t.seen)...)
		return true
	}
	paths, morePaths := t.pendingFiles()
	for _, path := range paths {
		st := t.files[path]
		if st == nil {
			st = &interactiveTranscriptFile{}
			t.files[path] = st
		}
		if st.tail(path, accept) {
			more = true
		}
	}
	return events, more || morePaths
}

// pendingFiles returns this session's transcripts that have unread bytes,
// newest modification first, capped at interactiveTranscriptMaxFilesPerSweep
// (more reports that the cap left some for a later sweep). Baseline files
// are never candidates.
func (t *interactiveTranscriptTailer) pendingFiles() (paths []string, more bool) {
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	walkInteractiveTranscriptFiles(t.stateDir, func(path string, entry os.DirEntry) {
		if _, earlier := t.baseline[path]; earlier {
			return
		}
		info, err := entry.Info()
		if err != nil {
			return
		}
		var offset int64
		if st := t.files[path]; st != nil {
			offset = st.offset
		}
		if info.Size() == offset {
			return
		}
		candidates = append(candidates, candidate{path: path, modTime: info.ModTime()})
	})
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].modTime.After(candidates[j].modTime)
		}
		return candidates[i].path < candidates[j].path
	})
	if len(candidates) > interactiveTranscriptMaxFilesPerSweep {
		candidates = candidates[:interactiveTranscriptMaxFilesPerSweep]
		more = true
	}
	paths = make([]string, 0, len(candidates))
	for _, c := range candidates {
		paths = append(paths, c.path)
	}
	return paths, more
}

// walkInteractiveTranscriptFiles calls fn for every .jsonl file under dir,
// recursing into subdirectories (older pi builds nest transcripts under
// sessions/) but never into the per-session config home or the
// injected-extensions dir, which hold no transcripts.
func walkInteractiveTranscriptFiles(dir string, fn func(path string, entry os.DirEntry)) {
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case agentHomeDir, injectedExtensionsDir:
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		fn(path, entry)
		return nil
	})
}

// tail feeds each complete line appended to path since the last read to
// accept, in order, advancing the offset past every line accept takes.
// accept returns false to leave the line (and everything after it) for a
// later sweep; tail then reports stopped=true. A torn trailing line without
// its newline stays unread until the writer completes it; a shrunk file
// (rotation/rewrite) restarts from zero; a line longer than the read window
// is skipped through its newline. tail also reports stopped=true when the
// read window filled, since more bytes may follow. Best-effort: any IO
// failure consumes nothing.
func (st *interactiveTranscriptFile) tail(path string, accept func([]byte) bool) (stopped bool) {
	//nolint:gosec // G304: path is discovered by walking the session's own state dir.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Size() < st.offset {
		st.offset, st.skipping = 0, false
	}
	if info.Size() == st.offset {
		return false
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, interactiveTranscriptMaxReadBytes))
	if err != nil || len(data) == 0 {
		return false
	}
	windowFull := len(data) == interactiveTranscriptMaxReadBytes
	if st.skipping {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// Still inside the oversized line: discard the whole window.
			st.offset += int64(len(data))
			return windowFull
		}
		st.offset += int64(idx + 1)
		data = data[idx+1:]
		st.skipping = false
	}
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimRight(data[:idx], "\r")
		if len(bytes.TrimSpace(line)) > 0 && !accept(line) {
			return true
		}
		st.offset += int64(idx + 1)
		data = data[idx+1:]
	}
	if len(data) == interactiveTranscriptMaxReadBytes {
		// One line fills the whole window: it can never be parsed within the
		// read bound, so skip it rather than re-reading it forever.
		st.offset += int64(len(data))
		st.skipping = true
	}
	return windowFull
}

// interactiveTranscriptRecord is one decoded transcript JSONL line. Type is
// the discriminant pi stamps ("session", "session_info", "model_change",
// "message", …); ID dedupes re-reads; Message carries the payload for
// "message" lines.
type interactiveTranscriptRecord struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Message json.RawMessage `json:"message"`
}

// mapInteractiveTranscriptLine maps one transcript line to the same agent
// event types the headless mapper emits for the equivalent turn activity:
// assistant text chunks, tool calls, tool results, and per-turn usage. Only
// "message" lines map; session metadata, user messages (the initial prompt
// already travels as a lifecycle marker), and unknown shapes map to nothing.
// Entries already in seen are skipped so a re-read never double-emits.
func mapInteractiveTranscriptLine(line []byte, seen map[string]struct{}) []agent.Event {
	var rec interactiveTranscriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	if rec.Type != "message" || len(rec.Message) == 0 {
		return nil
	}
	if rec.ID != "" {
		if _, dup := seen[rec.ID]; dup {
			return nil
		}
		seen[rec.ID] = struct{}{}
	}
	var msg interactiveTranscriptMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		return nil
	}
	raw := string(line)
	switch msg.Role {
	case "assistant":
		var out []agent.Event
		for _, part := range msg.Content {
			switch part.Type {
			case "text":
				if part.Text == "" {
					continue
				}
				out = append(out, agent.AssistantTextEvent{Text: part.Text, Raw: raw})
			case "toolCall":
				if part.Name == "" {
					continue
				}
				input := part.Arguments
				if input == nil {
					input = map[string]any{}
				}
				out = append(out, agent.ToolUseEvent{
					ToolName:  part.Name,
					ToolUseID: part.ID,
					Input:     input,
					Raw:       raw,
				})
			}
		}
		if msg.Usage != nil {
			out = append(out, agent.LlmCallEvent{
				System:            msg.Provider,
				Model:             msg.Model,
				InputTokens:       int64(msg.Usage.Input),
				OutputTokens:      int64(msg.Usage.Output),
				CachedInputTokens: int64(msg.Usage.CacheRead),
				CacheWriteTokens:  int64(msg.Usage.CacheWrite),
				UsageSource:       agent.LlmUsageProvider,
				ObservedCostUsd:   transcriptCost(msg.Usage.Cost.Total),
				TurnCompleted:     true,
			})
		}
		return out
	case "toolResult":
		var b bytes.Buffer
		for _, part := range msg.Content {
			if part.Type != "text" || part.Text == "" {
				continue
			}
			b.WriteString(part.Text)
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  msg.ToolName,
			ToolUseID: msg.ToolCallID,
			Content:   b.String(),
			IsError:   msg.IsError,
			Raw:       raw,
		}}
	default:
		return nil
	}
}

// interactiveTranscriptMessage is the payload of a transcript "message" line,
// verified against the real pi session transcript (pi 0.85.x): assistant
// content parts carry {"type":"text","text"} or
// {"type":"toolCall","id","name","arguments"}; tool results arrive as
// role:"toolResult" with text content parts; per-turn usage rides the
// assistant message in pi's own usage shape ({input, output, cacheRead,
// cacheWrite}, with input already excluding both cache buckets — the same
// shape the headless turn_end mapper reads).
type interactiveTranscriptMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type      string         `json:"type"`
		Text      string         `json:"text"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"content"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Usage      *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Cost       struct {
			Total *float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

// transcriptCost mirrors the headless observedPiCost guard: only a finite,
// non-negative reported total becomes an ObservedCostUsd.
func transcriptCost(total *float64) *float64 {
	if total == nil || math.IsNaN(*total) || math.IsInf(*total, 0) || *total < 0 {
		return nil
	}
	return total
}
