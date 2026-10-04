package pi

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
)

// harnessStateLostSubtype is an observable, provider-owned condition. It is
// deliberately a SystemEvent rather than a synthetic terminal result: pi can
// still be alive when its append target disappears, and claiming it exited
// would be false. The interactive runner forwards this event through its
// existing activity + events.jsonl seam.
const harnessStateLostSubtype = "harness_state_lost"

const harnessStateLostMessage = "pi reported ENOENT for its own session JSONL; harness state was lost"

// interactiveStateLossScanner recognizes the one fatal-looking-but-live pi
// condition from raw PTY output. It retains a bounded tail because terminal
// frames can split the errno, path, and .jsonl suffix across reads.
type interactiveStateLossScanner struct {
	stateDir string
	tail     string
	emitted  bool
}

func newInteractiveStateLossScanner(stateDir string) *interactiveStateLossScanner {
	return &interactiveStateLossScanner{stateDir: filepath.Clean(stateDir)}
}

func (s *interactiveStateLossScanner) Observe(output []byte) (agent.SystemEvent, bool) {
	if s.emitted || len(output) == 0 {
		return agent.SystemEvent{}, false
	}
	const maxTail = 32 << 10
	s.tail += strings.ToLower(string(output))
	if len(s.tail) > maxTail {
		s.tail = s.tail[len(s.tail)-maxTail:]
	}
	// An errno from one diagnostic must not combine with a normal path mention
	// from another. PTY reads may split one line, so retain the bounded tail,
	// but require both facts inside the same newline-delimited diagnostic.
	for _, record := range strings.FieldsFunc(s.tail, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.Contains(record, "enoent") && containsSessionJSONLPath(record, s.stateDir) {
			s.emitted = true
			return agent.SystemEvent{Subtype: harnessStateLostSubtype, Message: harnessStateLostMessage}, true
		}
	}
	return agent.SystemEvent{}, false
}

// containsSessionJSONLPath discriminates pi's own append target from an
// unrelated ENOENT. pi may render it as the absolute materialized path or as
// the worktree-relative .pi/sessions/...jsonl path; both designate this
// session's state root. Similar names such as .pi-cache never qualify.
func containsSessionJSONLPath(output, stateDir string) bool {
	stateDir = strings.ToLower(filepath.Clean(stateDir))
	for _, candidate := range []string{stateDir, ".pi"} {
		start := 0
		for {
			idx := strings.Index(output[start:], candidate)
			if idx < 0 {
				break
			}
			idx += start
			if candidate == ".pi" && !relativeStateDirTokenStart(output, idx) {
				start = idx + len(candidate)
				continue
			}
			after := output[idx+len(candidate):]
			if strings.HasPrefix(after, "/") || strings.HasPrefix(after, `\`) {
				if end := strings.IndexAny(after, "\t\r\n '\\\""); end >= 0 && strings.HasSuffix(after[:end], ".jsonl") {
					return true
				}
				if strings.HasSuffix(after, ".jsonl") {
					return true
				}
			}
			start = idx + len(candidate)
		}
	}
	return false
}

// relativeStateDirTokenStart accepts a worktree-relative `.pi/...` only at a
// shell/path-token boundary (for example `open '.pi/x.jsonl'` or
// `./.pi/x.jsonl`). It must not treat the `.pi` suffix of another absolute
// path as this session's state directory; absolute paths are handled above by
// the exact configured stateDir candidate.
func relativeStateDirTokenStart(output string, idx int) bool {
	if idx == 0 {
		return true
	}
	prev := output[idx-1]
	if strings.ContainsRune(" \t\r\n'\"`=([{", rune(prev)) {
		return true
	}
	return (prev == '/' || prev == '\\') && idx >= 2 && output[idx-2] == '.'
}

// Event channel sizing for interactiveStateLossHandle. The coarse events —
// InitEvent, at most one state-loss SystemEvent, and the terminal
// ResultEvent — always have a reserved slot, so their blocking sends complete
// even when nobody drains Events. Transcript activity only ever fills the
// remaining slots, and is dropped (never queued) once they are taken.
const (
	interactiveCoarseEventSlots     = 3
	interactiveTranscriptEventSlots = 64
)

// interactiveStateLossHandle preserves ptycli's coarse Init/Result contract
// while adding at most one typed state-loss SystemEvent plus the session's
// own transcript activity (assistant turns, tool calls, tool results). It
// listens on the public InteractiveSession subscription seam, so no ptyhost
// or platform wire change is required.
//
// Ordering: transcript activity the child wrote on its way out can only be
// read after InteractiveSession().Done closes, so the final transcript flush
// is enqueued strictly after Done. ActivityFlushed closes once that flush is
// enqueued, and the terminal ResultEvent is forwarded only after it, so a
// consumer reading to the ResultEvent (or waiting on ActivityFlushed after
// Done) sees every delivered transcript event.
//
// Teardown: every helper goroutine returns at session end whether or not the
// caller drains Events — the tailer stops on Done and never blocks, and the
// coarse sends always fit the reserved slots — after which Events closes.
type interactiveStateLossHandle struct {
	*ptycli.Handle
	events chan agent.Event
	// activityFlushed closes once the transcript tailer has enqueued its
	// final flush (or dropped it on a full channel) and returned.
	activityFlushed chan struct{}
	// finished closes after events is closed, i.e. once every helper
	// goroutine has returned.
	finished chan struct{}
}

// newInteractiveStateLossHandle wraps a spawned PTY handle. tailer must have
// been constructed before the child was spawned (see
// newInteractiveTranscriptTailer).
func newInteractiveStateLossHandle(handle *ptycli.Handle, stateDir string, tailer *interactiveTranscriptTailer) *interactiveStateLossHandle {
	h := &interactiveStateLossHandle{
		Handle:          handle,
		events:          make(chan agent.Event, interactiveCoarseEventSlots+interactiveTranscriptEventSlots),
		activityFlushed: make(chan struct{}),
		finished:        make(chan struct{}),
	}
	session := handle.InteractiveSession()
	// emit forwards one transcript event best-effort. Only the tailer
	// goroutine calls it, and it never takes a slot reserved for the coarse
	// events: with at most interactiveCoarseEventSlots coarse sends over the
	// handle's life, the coarse senders below can never block on a channel
	// transcript events filled. When the consumer is slow the event is
	// dropped rather than blocking the tailer behind the terminal.
	emit := func(ev agent.Event) {
		if len(h.events) >= cap(h.events)-interactiveCoarseEventSlots {
			return
		}
		select {
		case h.events <- ev:
		default:
		}
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for event := range handle.Events() {
			if _, terminal := event.(agent.ResultEvent); terminal {
				// ptycli sends the result strictly after Done; the tailer's
				// final flush, which also starts at Done, goes first.
				<-h.activityFlushed
			}
			h.events <- event
		}
	}()
	go func() {
		defer wg.Done()
		scanner := newInteractiveStateLossScanner(stateDir)
		subscription, err := session.Subscribe(0)
		if err != nil {
			return
		}
		defer func() { _ = subscription.Close() }()
		for frame := range subscription.Frames() {
			if frame.Type != attachwire.TypeOutput {
				continue
			}
			if event, ok := scanner.Observe(frame.Payload); ok {
				h.events <- event
			}
		}
	}()
	// Transcript tailer: while the session runs, its JSONL transcript is
	// tailed and mapped to the same agent events headless emits, so an
	// interactive session's turns and tool calls reach the session activity
	// stream. Best-effort and rate-limited (see interactive_transcript.go).
	// It stops when the PTY child has exited and drained, after one final
	// flush of trailing writes.
	go func() {
		defer wg.Done()
		defer close(h.activityFlushed)
		tailer.run(session.Done(), emit)
	}()
	go func() {
		wg.Wait()
		close(h.events)
		close(h.finished)
	}()
	return h
}

// Events overrides ptycli.Handle.Events with the same coarse events plus the
// typed state-loss condition and transcript activity above.
func (h *interactiveStateLossHandle) Events() <-chan agent.Event { return h.events }

// ActivityFlushed implements agent.InteractiveActivityFlusher: it closes once
// the session's final transcript activity has been enqueued on Events.
func (h *interactiveStateLossHandle) ActivityFlushed() <-chan struct{} { return h.activityFlushed }

var _ agent.InteractiveActivityFlusher = (*interactiveStateLossHandle)(nil)

var _ agent.InteractiveCapable = (*interactiveStateLossHandle)(nil)
