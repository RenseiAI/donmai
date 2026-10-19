// This file tails the native session files a host interactive session leaves
// behind for the PUBLIC conversation: user and assistant messages, tool
// calls and results, and turn completion. Codex persists one JSONL rollout
// per thread under <home>/sessions/<date>/rollout-*-<threadID>.jsonl (see
// rollout_usage.go, which owns the usage half of that file). The interactive
// PTY surface emits no structured conversation of its own, so newly appended
// rollout records are the session's live conversation.
//
// Every mapped event mirrors the headless app-server lane's mapping (see
// event_mapping.go) onto the same agent event types: assistant messages map
// to AssistantTextEvent exactly as item/completed agentMessage does, shell
// and tool calls map to ToolUseEvent/ToolResultEvent exactly as the
// item/completed CommandExecution/McpToolCall shapes do, and per-turn usage
// records map to LlmCallEvent exactly as turn/completed does. Reasoning
// records carry only encrypted content and never map — hidden reasoning
// stays hidden — and developer/system messages (environment context, memory,
// skills, model-switch instructions) never map: they are harness framing,
// not conversation.
//
// Ownership: only the admitted session's own rollout files are read — the
// thread's file plus its descendant subagent files (see subagentThreadIDs),
// located through the SAME thread identity the terminal-cost read uses.
// Never scan unrelated threads. A rollout file is runner-owned state shared
// with the child, so every open refuses non-regular files (see
// listRolloutFiles) and every read is bounded: the tail never blocks the
// PTY and never wedges on a torn line, a shrunk file (rotation), or a
// malformed record.
//
// Attribution: provider user messages may be injected or automated and must
// not automatically become the viewer's own input. User turns therefore
// surface as SystemEvent markers carrying the message text (never as a
// prompt the viewer typed), mirroring the headless lane, which drops echoed
// userMessage items rather than forwarding them as input.
//
// Event identity and replay: every record carries a stable native id (the
// response_item payload id or the event_msg item id), remembered for the
// tailer's life, so a restart, rotation, or re-read never double-emits.
// Item completion is the delivery point: a started-but-never-completed item
// emits nothing, and a response_item without its matching item/completed
// completion is still delivered once the matching completion arrives — the
// tailer pairs them by id, whichever order the file holds. Turn completion
// (task_complete) surfaces as a SystemEvent turn marker; the terminal
// ResultEvent stays the PTY lifecycle's, never the tailer's.

package codex

import (
	"bufio"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Interactive conversation tail cadence and caps. The poll interval keeps
// the conversation near-live without measurable IO pressure (a stat +
// occasional bounded read per file); the per-sweep caps defer work to the
// next tick rather than discarding it, and the flush bound covers trailing
// writes that landed with the child's exit.
const (
	// interactiveConversationPollInterval is the cadence at which the
	// tailer re-scans the session's rollout files.
	interactiveConversationPollInterval = time.Second

	// interactiveConversationMaxFilesPerSweep bounds how many rollout
	// files one sweep opens. A session normally owns one plus its
	// subagent files; only files with unread bytes count against the cap.
	interactiveConversationMaxFilesPerSweep = 8

	// interactiveConversationMaxLinesPerSweep bounds how many new rollout
	// lines one sweep consumes. A burst beyond it is consumed on later
	// ticks.
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

	// interactiveConversationMaxPendingAgeSweeps bounds how long a staged
	// tool start waits for its completion (about five minutes at the 1s
	// poll cadence). A completion that arrives later still delivers
	// through the completion-reconstruction path; without the bound an
	// orphaned start (a harness that restarts mid-call, completions that
	// lag) holds its entry for the session's life.
	interactiveConversationMaxPendingAgeSweeps = 300
)

// interactiveConversationTailer tracks the per-file read offsets, the
// already-delivered record ids, and the started tool calls awaiting their
// completion across sweeps. It is not safe for concurrent use; the single
// tailer goroutine owns it.
type interactiveConversationTailer struct {
	home string
	// resolve reports the thread the session actually ran as. It is
	// read on every sweep — not snapshotted at construction — so the
	// fresh-named path starts tailing once naming completes, and a nil
	// resolver (paths where the thread is never known) keeps the tailer
	// quiet: unknown identity reads as absent, never scanned.
	resolve func() string
	files   map[string]*interactiveConversationFile
	// seen holds every delivered record identity (assistant text key,
	// exec/mcp/file-change result id, turn id, or usage key), so a
	// re-read never double-emits.
	seen map[string]struct{}
	// sweeps counts sweepCapped runs; staged starts carry the sweep that
	// staged them so orphaned entries age out (see evictStalePending).
	sweeps uint64
	// pending holds started tool calls awaiting their completion, keyed
	// by call id ("shell-call:<id>" for exec starts, "function:<id>"
	// for namespaced calls). A completion without a start still
	// delivers: the call is reconstructed from the completion record.
	// Entries older than interactiveConversationMaxPendingAgeSweeps are
	// evicted on every sweep, so orphaned starts stay bounded.
	pending map[string]pendingConversationCall
	// pendingExecByCommand queues staged exec starts per shell command
	// (oldest first), so identical consecutive commands pair in order
	// instead of colliding on one entry.
	pendingExecByCommand map[string][]string
}

// interactiveConversationFile is the read state of one tailed rollout.
type interactiveConversationFile struct {
	offset int64
}

// pendingConversationCall is a tool call observed before its completion
// arrived: the ToolUseEvent fields to emit once paired, or to drop when
// the event cap defers the completion past this sweep.
type pendingConversationCall struct {
	toolName  string
	toolUseID string
	input     map[string]any
	raw       string
	// stagedAt is the sweeps count when the start was staged; the entry
	// ages out once it exceeds interactiveConversationMaxPendingAgeSweeps.
	stagedAt uint64
}

func newInteractiveConversationTailer(home string, resolve func() string) *interactiveConversationTailer {
	return &interactiveConversationTailer{
		home:                 home,
		resolve:              resolve,
		files:                make(map[string]*interactiveConversationFile),
		seen:                 make(map[string]struct{}),
		pending:              make(map[string]pendingConversationCall),
		pendingExecByCommand: make(map[string][]string),
	}
}

// run polls until sessionDone closes, emitting newly mapped conversation
// events via emit, then flushes whatever the child wrote on its way out.
// Every emit must be non-blocking — this goroutine never slows the PTY,
// and it returns once the flush is done whether or not anyone reads the
// events.
func (t *interactiveConversationTailer) run(sessionDone <-chan struct{}, emit func(agentEvent)) {
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

// agentEvent is the mapped event type the interactive tailer emits:
// agent.Event on the production wire.
type agentEvent = agent.Event

// flush sweeps until nothing is pending, bounded by
// interactiveConversationMaxFinalSweeps.
func (t *interactiveConversationTailer) flush(emit func(agentEvent)) {
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

// sweep reads newly appended conversation records and maps them to agent
// events. It reports nothing pending beyond what it emits: a cap that
// defers work here only delays the live conversation.
func (t *interactiveConversationTailer) sweep() []agentEvent {
	events, _ := t.sweepCapped()
	return events
}

// sweepCapped is sweep with a more flag reporting that a cap left input
// pending for a later sweep.
func (t *interactiveConversationTailer) sweepCapped() (events []agentEvent, more bool) {
	t.sweeps++
	t.evictStalePending()
	paths := t.sessionRollouts()
	if len(paths) == 0 {
		return nil, false
	}
	lines := 0
	for i, path := range newestRolloutsFirst(paths) {
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
			return events, true
		}
	}
	return events, false
}

// evictStalePending drops staged tool starts older than
// interactiveConversationMaxPendingAgeSweeps sweeps and prunes their
// per-command queue entries, so orphaned starts (a harness that restarts
// mid-call, completions that lag) cannot grow the staging area for the
// session's life. A completion arriving after its start aged out still
// delivers through the completion-reconstruction path.
func (t *interactiveConversationTailer) evictStalePending() {
	for key, staged := range t.pending {
		if t.sweeps-staged.stagedAt >= interactiveConversationMaxPendingAgeSweeps {
			delete(t.pending, key)
		}
	}
	for command, queue := range t.pendingExecByCommand {
		kept := queue[:0]
		for _, key := range queue {
			if _, ok := t.pending[key]; ok {
				kept = append(kept, key)
			}
		}
		if len(kept) == 0 {
			delete(t.pendingExecByCommand, command)
		} else {
			t.pendingExecByCommand[command] = kept
		}
	}
}

// sessionRollouts returns the rollout files this session owns: the
// thread's own file plus every descendant subagent thread's file (see
// subagentThreadIDs), each resolved through findThreadRollout. The thread
// identity resolves on every sweep, so the fresh-named path starts tailing
// once naming completes; an unknown thread (no identity yet, or no file
// yet) contributes nothing.
func (t *interactiveConversationTailer) sessionRollouts() []string {
	if t.home == "" || t.resolve == nil {
		return nil
	}
	threadID := t.resolve()
	if threadID == "" {
		return nil
	}
	var out []string
	if path := findThreadRollout(t.home, threadID); path != "" {
		out = append(out, path)
	}
	for _, sub := range subagentThreadIDs(t.home, threadID) {
		if path := findThreadRollout(t.home, sub); path != "" {
			out = append(out, path)
		}
	}
	return out
}

// newestRolloutsFirst orders rollout paths newest-modification first so a
// backlog elsewhere cannot starve the live file. Paths that cannot be
// statted sort last; the order is stable by path for equal times.
func newestRolloutsFirst(paths []string) []string {
	type candidate struct {
		path string
		mod  time.Time
		ok   bool
	}
	candidates := make([]candidate, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path) //nolint:gosec // path is the session's own rollout file under its CODEX_HOME
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
			return candidates[i].mod.After(candidates[j].mod)
		}
		return candidates[i].path < candidates[j].path
	})
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.path)
	}
	return out
}

// tail feeds each complete line appended to path since the last read to
// the conversation mapper, in order, advancing the offset past every line
// taken. lines counts consumed lines against
// interactiveConversationMaxLinesPerSweep; taken counts already-emitted
// events against interactiveConversationMaxEventsPerSweep. The line that
// would exceed either cap is left unread for a later sweep, and tail then
// reports pending=true. A torn trailing line without its newline stays
// unread until the writer completes it; a shrunk file (rotation/rewrite)
// restarts from zero against the same dedupe set. Malformed lines are
// skipped without moving the pairing state. Best-effort: any IO failure
// consumes nothing.
func (t *interactiveConversationTailer) tail(path string, st *interactiveConversationFile, lines *int, taken int) ([]agentEvent, bool) {
	f, err := openRolloutFile(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if info.Size() < st.offset {
		// The file shrank (rotation/rewrite): restart from zero. The
		// seen set still holds every delivered id, so the re-read
		// emits nothing twice.
		st.offset = 0
	}
	if info.Size() == st.offset {
		return nil, false
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return nil, false
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var events []agentEvent
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytesTrimSpace(raw)) == 0 {
			st.offset += int64(len(raw)) + 1
			continue
		}
		if *lines >= interactiveConversationMaxLinesPerSweep ||
			taken+len(events) >= interactiveConversationMaxEventsPerSweep {
			return events, true
		}
		*lines++
		events = append(events, t.mapLine(raw)...)
		st.offset += int64(len(raw)) + 1
	}
	if err := scanner.Err(); err != nil {
		// An overlong line (a tool call dumping megabytes into one
		// record) must not wedge the tail: skip past exactly that line
		// so follower lines still deliver. bufio.Scanner discards the
		// overlong line's buffered bytes, so resync by scanning forward
		// from the last good offset to the next newline and continuing
		// past it; the skipped record is lost, the live conversation
		// is not.
		if err == bufio.ErrTooLong {
			if _, seekErr := f.Seek(st.offset, io.SeekStart); seekErr == nil {
				skip := bufio.NewReader(f)
				b, _ := skip.ReadBytes('\n')
				st.offset += int64(len(b))
			}
			// Follower lines past the skipped record are still unread:
			// report pending so the flush loop sweeps again.
			if end, statErr := f.Stat(); statErr == nil && end.Size() > st.offset {
				return events, true
			}
		}
	}
	return events, false
}

// bytesTrimSpace reports whether raw holds only whitespace, without
// allocating the trimmed copy.
func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}
