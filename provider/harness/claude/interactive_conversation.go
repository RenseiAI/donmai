package claude

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Live conversation tail for interactive sessions.
//
// An interactive Claude session runs the harness's own REPL under a PTY: no
// stream-json wire exists, so the only durable record of the conversation is
// the harness's own JSONL transcript — the same file the Stop hook's stdin
// names via transcript_path. The usage tail (interactive_usage.go) already
// reads that file for per-turn spend; this file tails it for the public
// conversation itself: user messages, assistant text, tool calls, tool
// results, and turn completion, mapped to the SAME agent event types the
// headless lane emits (see provider/harness/clijsonl/jsonl.go), which the
// interactive runner forwards through the existing activity sink.
//
// Ownership: the transcript is the session's own file, located through the
// Stop hook's durable locator — never by scanning unrelated sessions. Only
// transcripts the hook named for THIS session are read: every distinct path
// the hook names is tailed, in first-seen order, so a conversation clear
// (which moves the session to a new transcript) keeps delivering the turns
// written to the earlier file. Subagent transcripts beside each named one
// (<transcript minus .jsonl>/subagents/*.jsonl) are tailed the same way the
// exit usage sum reads them: a session that delegates holds much of its
// conversation there.
//
// Attribution: provider user messages may be injected or automated — a Stop
// hook block, a skill, or a companion surface — and must not automatically
// become the viewer's own input. User turns therefore surface as SystemEvent
// markers carrying the message text (never as a prompt the viewer typed),
// exactly as the headless lane's mapUser already treats non-tool user lines
// (a "user_message" system event, never an input echo).
//
// Privacy: hidden reasoning and system framing never leave the file. Only
// assistant text blocks, tool_use blocks, and tool_result records map;
// thinking/redacted-thinking blocks, system lines, hook bookkeeping,
// attachments, queue operations, summaries, and every other record type map
// to nothing. Tool input rides the event the same way the headless lane
// carries it (the ToolUseEvent Input map); the activity poster applies its
// own cap downstream, so nothing is cut here.
//
// Posture: best-effort and rate-limited, never blocking the PTY. The tailer
// runs on its own goroutine, polls on a 1s ticker, caps files / lines /
// events per sweep, and emits via a non-blocking send — when the consumer is
// slow, conversation events are dropped, never queued behind the terminal.
// The caps defer work to the next sweep rather than discarding it: a sweep
// stops BEFORE the line that would exceed a cap, leaving it unread.
//
// Event identity and replay: every mapped event carries a stable identity
// through its Raw line (the transcript's own uuid when present, else the
// content hash), and every emitted identity is remembered for the
// tailer's life, so a restart, rotation, or re-read never double-emits.
// A shrunk file (rotation/rewrite) restarts from zero against that same
// dedupe set. A torn trailing line stays unread until the writer completes
// it.
const (
	// interactiveConversationPollInterval is the cadence at which the
	// tailer re-reads the session transcripts. One second keeps
	// conversation near-live without measurable IO pressure (a stat +
	// occasional bounded read per file).
	interactiveConversationPollInterval = time.Second

	// interactiveConversationMaxFilesPerSweep bounds how many transcript
	// files one sweep opens. A session normally owns one plus its
	// subagent files; only files with unread bytes count against the cap.
	interactiveConversationMaxFilesPerSweep = 8

	// interactiveConversationMaxLinesPerSweep bounds how many new
	// transcript lines one sweep consumes. A burst beyond it is consumed
	// on later ticks.
	interactiveConversationMaxLinesPerSweep = 200

	// interactiveConversationMaxEventsPerSweep bounds how many agent
	// events one sweep emits — the rate limit that keeps a chatty session
	// from flooding the activity buffer. The line that reaches the cap is
	// mapped whole; later lines wait for the next tick.
	interactiveConversationMaxEventsPerSweep = 50

	// interactiveConversationMaxFinalSweeps bounds the end-of-session
	// flush. Trailing writes that landed with the child's exit may exceed
	// one sweep's caps; the flush keeps sweeping until nothing is pending
	// or this many sweeps have run.
	interactiveConversationMaxFinalSweeps = 16
)

// interactiveConversationTailer tracks the transcript read offsets and the
// record uuids already emitted across sweeps. It is not safe for concurrent
// use; the single tailer goroutine owns it.
type interactiveConversationTailer struct {
	hook *stopHookChannel
	// paths is every transcript path the hook has named, in first-seen
	// order, plus the subagent files discovered beside each. Offsets are
	// per path: a conversation clear starts a new file at its first byte
	// rather than at the old file's offset.
	files map[string]*interactiveConversationFile
	// seen holds every transcript uuid already emitted, so a re-read
	// never double-emits.
	seen map[string]struct{}
	// since is when the session began; lines stamped earlier belong to a
	// conversation this session resumed, not to this session. Zero
	// disables the filter.
	since time.Time
}

// interactiveConversationFile is the read state of one tailed transcript.
type interactiveConversationFile struct {
	offset int64
}

func newInteractiveConversationTailer(hook *stopHookChannel, since time.Time) *interactiveConversationTailer {
	return &interactiveConversationTailer{
		hook:  hook,
		files: make(map[string]*interactiveConversationFile),
		seen:  make(map[string]struct{}),
		since: since,
	}
}

// run polls until sessionDone closes, emitting newly mapped conversation
// events via emit, then flushes whatever the child wrote on its way out.
// Every emit must be non-blocking — this goroutine never slows the PTY, and
// it returns once the flush is done whether or not anyone reads the events.
func (t *interactiveConversationTailer) run(sessionDone <-chan struct{}, emit func(agent.Event)) {
	ticker := time.NewTicker(interactiveConversationPollInterval)
	defer ticker.Stop()
	for {
		for _, ev := range t.sweep() {
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
// interactiveConversationMaxFinalSweeps.
func (t *interactiveConversationTailer) flush(emit func(agent.Event)) {
	for range interactiveConversationMaxFinalSweeps {
		events, more := t.sweepCapped()
		for _, ev := range events {
			emit(ev)
		}
		if !more {
			return
		}
	}
}

// sweep reads newly appended conversation lines and maps them to agent
// events. It reports nothing pending beyond what it emits: a cap that
// defers work here only delays the live conversation.
func (t *interactiveConversationTailer) sweep() []agent.Event {
	events, _ := t.sweepCapped()
	return events
}

// sweepCapped is sweep with a more flag reporting that a cap left input
// pending for a later sweep.
func (t *interactiveConversationTailer) sweepCapped() (events []agent.Event, more bool) {
	if t.hook == nil {
		return nil, false
	}
	paths := t.sessionTranscripts()
	if len(paths) == 0 {
		return nil, false
	}
	newest := oldestFirst(paths)
	lines := 0
	for i, path := range newest {
		if i >= interactiveConversationMaxFilesPerSweep {
			return events, true
		}
		st := t.files[path]
		if st == nil {
			st = &interactiveConversationFile{}
			t.files[path] = st
		}
		fileEvents, pending := t.tail(path, st, &lines, len(events))
		events = append(events, fileEvents...)
		if pending {
			return events, true
		}
		if len(events) >= interactiveConversationMaxEventsPerSweep {
			// The cap is on emitted events: later files wait for the
			// next tick.
			return events, true
		}
	}
	return events, false
}

// sessionTranscripts returns every transcript file this session owns: each
// path the hook named plus the subagent files beside it. The hook locator
// read also records newly named paths, so a conversation clear is picked
// up on the next sweep.
func (t *interactiveConversationTailer) sessionTranscripts() []string {
	var named []string
	if path, ok, err := t.hook.transcriptPath(); err == nil && ok {
		named = t.hook.namedTranscripts()
		_ = path
	} else {
		named = t.hook.namedTranscripts()
	}
	seen := make(map[string]struct{}, len(named)*2)
	var out []string
	for _, path := range named {
		if _, dup := seen[path]; !dup {
			seen[path] = struct{}{}
			out = append(out, path)
		}
		for _, sub := range subagentTranscriptPaths(path) {
			if _, dup := seen[sub]; !dup {
				seen[sub] = struct{}{}
				out = append(out, sub)
			}
		}
	}
	return out
}

// subagentTranscriptPaths lists the subagent transcripts beside path, the
// same files the exit usage sum reads (<transcript minus .jsonl>/
// subagents/*.jsonl). A glob failure yields nothing: the session file
// itself is still tailed.
func subagentTranscriptPaths(path string) []string {
	subagents, err := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	if err != nil {
		return nil
	}
	sort.Strings(subagents)
	return subagents
}

// oldestFirst orders transcript paths oldest-modification first so one
// sweep delivers conversation in chronological order: the earlier file's
// turns precede the later file's. A backlog in an old file only delays
// the live file to a later sweep (the caps defer work, never discard it),
// which keeps delivery ordered without starving anything. Paths that
// cannot be statted sort last; the order is stable by path for equal
// times.
func oldestFirst(paths []string) []string {
	type candidate struct {
		path string
		mod  time.Time
		ok   bool
	}
	candidates := make([]candidate, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path) //nolint:gosec // path is the harness's own transcript, resolved from its hook payload
		if err != nil {
			candidates = append(candidates, candidate{path: path})
			continue
		}
		candidates = append(candidates, candidate{path: path, mod: info.ModTime(), ok: true})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ok != candidates[j].ok {
			return candidates[i].ok
		}
		if !candidates[i].mod.Equal(candidates[j].mod) {
			return candidates[i].mod.Before(candidates[j].mod)
		}
		return candidates[i].path < candidates[j].path
	})
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.path)
	}
	return out
}

// tail feeds each complete line appended to path since the last read to the
// conversation mapper, in order, advancing the offset past every line taken.
// lines counts consumed lines against
// interactiveConversationMaxLinesPerSweep; taken counts already-emitted
// events against interactiveConversationMaxEventsPerSweep. The line that
// would exceed either cap is left unread for a later sweep, and tail then
// reports pending=true. A torn trailing line without its newline stays
// unread until the writer completes it; a shrunk file (rotation/rewrite)
// restarts from zero against the same dedupe set. Best-effort: any IO
// failure consumes nothing.
func (t *interactiveConversationTailer) tail(path string, st *interactiveConversationFile, lines *int, taken int) ([]agent.Event, bool) {
	f, err := openTranscript(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if info.Size() < st.offset {
		// The transcript shrank (rotation/rewrite): restart from zero.
		// The seen set still holds every emitted uuid, so the re-read
		// emits nothing twice.
		st.offset = 0
	}
	if info.Size() == st.offset {
		return nil, false
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return nil, false
	}
	reader := bufio.NewReader(f)
	var events []agent.Event
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			if err != nil {
				return events, false
			}
			continue
		}
		if len(line) > 0 && line[len(line)-1] != '\n' {
			// A torn trailing line stays unread until the writer
			// completes it; the offset does not advance past it.
			return events, false
		}
		if *lines >= interactiveConversationMaxLinesPerSweep ||
			taken+len(events) >= interactiveConversationMaxEventsPerSweep {
			return events, true
		}
		*lines++
		events = append(events, mapConversationLine(line, t.seen, t.since)...)
		st.offset += int64(len(line))
		if err != nil {
			return events, false
		}
	}
}

// conversationContentBlock is one block inside a transcript message's
// content array. Only text, tool_use, and tool_result blocks map; thinking
// and redacted-thinking blocks are hidden reasoning and never leave the
// file.
type conversationContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// conversationRecord is the narrow slice of the transcript schema the
// conversation tail reads: the record type, its uuid identity, its
// timestamp for the resume filter, and the message payload for
// assistant/user lines. Every other field stays unread on purpose.
type conversationRecord struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	IsMeta    bool   `json:"isMeta,omitempty"`
	Message   *struct {
		Role    string                    `json:"role,omitempty"`
		Content conversationRecordContent `json:"content,omitempty"`
	} `json:"message,omitempty"`
}

// conversationRecordContent decodes the message content field, which the
// harness writes either as a bare string or as an array of typed blocks.
type conversationRecordContent struct {
	Text   string
	Blocks []conversationContentBlock
}

// UnmarshalJSON accepts the harness's two content shapes: a bare string or
// an array of typed blocks. Any other shape decodes to empty.
func (c *conversationRecordContent) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		c.Text = s
		return nil
	}
	var blocks []conversationContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err == nil {
		c.Blocks = blocks
		return nil
	}
	return nil
}

// mapConversationLine maps one transcript line to the same agent event
// types the headless lane emits for the equivalent activity: user turns as
// attribution-preserving system markers, assistant text chunks,
// tool calls, and tool results. Only assistant and user lines map; system
// framing, hook bookkeeping, attachments, summaries, queue operations, and
// unknown shapes map to nothing — as do thinking blocks, which are hidden
// reasoning. Lines already in seen map to nothing so a re-read never
// double-emits; lines stamped before since are history this session did
// not run and map to nothing.
func mapConversationLine(line []byte, seen map[string]struct{}, since time.Time) []agent.Event {
	var rec conversationRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	// The pre-check answers whether this exact line already delivered:
	// uuid when the transcript carries one, else the line's content
	// hash, WITHOUT the trailing newline (the tail offset advances
	// past it, but the claim below sees only the mapper's line — both
	// must hash the same bytes). The claim after mapping keys the same
	// way, so a rotation or re-read of the same line collapses either
	// way.
	if key, ok := conversationDedupeKey(rec.UUID, trimConversationNewline(line)); ok {
		if _, dup := seen[key]; dup {
			return nil
		}
	}
	if !since.IsZero() && rec.Timestamp != "" {
		if at, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && at.Before(since) {
			return nil
		}
	}
	raw := string(line)
	switch rec.Type {
	case "assistant":
		if rec.Message == nil {
			return nil
		}
		var out []agent.Event
		for _, block := range rec.Message.Content.Blocks {
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				out = append(out, agent.AssistantTextEvent{Text: block.Text, Raw: raw})
			case "tool_use":
				if block.Name == "" {
					continue
				}
				out = append(out, agent.ToolUseEvent{
					ToolName:  block.Name,
					ToolUseID: block.ID,
					Input:     decodeConversationInput(block.Input),
					Raw:       raw,
				})
			case "thinking", "redacted_thinking":
				// Hidden reasoning: never exported.
			}
		}
		if len(out) == 0 {
			return nil
		}
		claimConversationUUID(seen, rec.UUID, trimConversationNewline(line))
		return out
	case "user":
		if rec.Message == nil {
			return nil
		}
		var out []agent.Event
		for _, block := range rec.Message.Content.Blocks {
			if block.Type != "tool_result" {
				continue
			}
			out = append(out, agent.ToolResultEvent{
				ToolUseID: block.ToolUseID,
				Content:   decodeConversationToolResult(block.Content),
				IsError:   block.IsError,
				Raw:       raw,
			})
		}
		if len(out) > 0 {
			claimConversationUUID(seen, rec.UUID, trimConversationNewline(line))
			return out
		}
		// A user turn with no tool results is a provider-side message —
		// injected or automated — never the viewer's own input. It
		// surfaces as a marker carrying the text, mirroring the
		// headless lane's "user_message" system event.
		text := rec.Message.Content.Text
		if text == "" {
			for _, block := range rec.Message.Content.Blocks {
				if block.Type == "text" && block.Text != "" {
					if text != "" {
						text += "\n"
					}
					text += block.Text
				}
			}
		}
		// Stop-hook feedback is the notice channel's own receipt: it
		// records that a runner-authored message entered the
		// conversation, not a turn to stream.
		if text == "" || isStopHookFeedback(text) {
			return nil
		}
		claimConversationUUID(seen, rec.UUID, trimConversationNewline(line))
		return []agent.Event{agent.SystemEvent{Subtype: "user_message", Message: text, Raw: raw}}
	default:
		return nil
	}
}

// trimConversationNewline strips the single trailing newline the tail
// reader leaves on the mapper's line, so the pre-check hashes the same
// bytes the post-mapping claim hashed.
func trimConversationNewline(line []byte) []byte {
	return bytes.TrimSuffix(line, []byte("\n"))
}

// conversationDedupeKey returns the dedupe key for one transcript line
// and whether the line is mappable: the transcript uuid when present,
// else the line's content hash (mirroring the usage tail's usageLineKey),
// so rotation or a re-read of the same id-less line collapses while
// genuinely different id-less lines keep their own identity.
func conversationDedupeKey(uuid string, line []byte) (string, bool) {
	if uuid != "" {
		return uuid, true
	}
	sum := sha256.Sum256(line)
	return "line:" + hex.EncodeToString(sum[:]), true
}

// claimConversationUUID remembers a mapped record's dedupe key so a
// re-read never double-emits. Records with a uuid key on it; records
// without one key on the line's content hash (mirroring the usage tail's
// usageLineKey), so rotation or a re-read of the same line collapses
// while genuinely different id-less lines keep their own identity.
func claimConversationUUID(seen map[string]struct{}, uuid string, line []byte) {
	key, _ := conversationDedupeKey(uuid, line)
	seen[key] = struct{}{}
}

// isStopHookFeedback reports whether text is the harness's own record of a
// Stop-hook block entering the conversation. The notice channel acks on
// that record; streaming it back as a user turn would echo the runner's
// own message as conversation.
func isStopHookFeedback(text string) bool {
	return strings.HasPrefix(text, "Stop hook feedback:")
}

// decodeConversationInput unmarshals a tool_use input as map[string]any.
// Any decode failure returns nil so the runner still sees the tool call,
// exactly as the headless lane's decodeInput.
func decodeConversationInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// decodeConversationToolResult normalizes the tool_result content shape
// (string or structured JSON) to a string, mirroring the headless lane's
// decodeToolResultContent: a string rides verbatim, anything else is
// stringified rather than dropped.
func decodeConversationToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
