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

// interactiveStateLossHandle preserves ptycli's coarse Init/Result contract
// while adding at most one typed state-loss SystemEvent. It listens on the
// public InteractiveSession subscription seam, so no ptyhost or platform wire
// change is required.
type interactiveStateLossHandle struct {
	*ptycli.Handle
	events chan agent.Event
}

func newInteractiveStateLossHandle(handle *ptycli.Handle, stateDir string) *interactiveStateLossHandle {
	h := &interactiveStateLossHandle{
		Handle: handle,
		// Init + one state-loss condition + terminal ResultEvent. The fixed
		// capacity preserves direct-handle callers that never drain Events.
		events: make(chan agent.Event, 3),
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for event := range handle.Events() {
			h.events <- event
		}
	}()
	go func() {
		defer wg.Done()
		scanner := newInteractiveStateLossScanner(stateDir)
		session := handle.InteractiveSession()
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
	go func() {
		wg.Wait()
		close(h.events)
	}()
	return h
}

// Events overrides ptycli.Handle.Events with the same coarse events plus the
// typed state-loss condition above.
func (h *interactiveStateLossHandle) Events() <-chan agent.Event { return h.events }

var _ agent.InteractiveCapable = (*interactiveStateLossHandle)(nil)
