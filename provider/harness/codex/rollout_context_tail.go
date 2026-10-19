package codex

import (
	"bufio"
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Live rollout context-notice tail for headless sessions.
//
// The app-server writes token_count events (the last call's usage, the
// cumulative totals, and the model window) and top-level compacted markers to the thread's rollout file
// while the session runs. The JSON-RPC stream carries neither, so the
// handle tails its OWN session file — home and thread id are spawn-known,
// never environment — and emits each new signal as the typed
// agent.ContextUsageEvent the runner's headless loop already forwards to
// the activity sink, the execution-event journal, and the JSONL audit
// trail. No runner wiring: emission rides h.emit on the handle's own
// Events channel, exactly like every other mapped event.
//
// Posture mirrors the transcript tailer (provider/harness/claude/
// interactive_usage.go): best-effort and rate-limited, never blocking the
// session. The tailer runs on its own goroutine, polls on a 1s ticker,
// and emits via the handle's non-blocking emit — when the consumer is
// slow, notices are dropped, never queued. Threshold gating matches the
// interactive watcher (50/75/90 rising bounds, re-arming below) so both
// lanes report crossings the same way; compaction markers emit once per
// observed state.
//
// The app-server writes a turn's last token_count line moments before it
// sends turn/completed, well inside one poll interval, so the ticker alone
// would lose the final crossing of every turn (and every crossing of a
// sub-second session). The forwarder therefore drains the tail once more
// on the terminal event, before it emits that event and closes Events,
// and the tail goroutine ends with the handle.
const rolloutTailPollInterval = time.Second

// rolloutTailThresholdBounds are the used-percentage bounds that emit a
// threshold notice, shared value-for-value with the runner's interactive
// watcher (a crossing is reported once per bound per direction).
func rolloutTailThresholdBounds() []float64 { return []float64{50, 75, 90} }

// rolloutContextTail tracks the rollout read offset, the threshold arming,
// and the last reported compaction for one session. poll and classify are
// not safe for concurrent use; the tailer goroutine and the terminal drain
// share the tail through emitNew, which holds mu across the read AND the
// emission so a notice read by one is on Events before the other runs.
type rolloutContextTail struct {
	mu sync.Mutex

	home     string
	threadID string

	path   string
	offset int64

	armed map[float64]bool

	lastCompaction string
}

func newRolloutContextTail(home, threadID string) *rolloutContextTail {
	armed := make(map[float64]bool, 3)
	for _, bound := range rolloutTailThresholdBounds() {
		armed[bound] = true
	}
	return &rolloutContextTail{home: home, threadID: threadID, armed: armed}
}

// startContextTail launches the session's rollout context-notice tail.
// It returns the tail so tests can assert the wiring, or nil when the
// session has no known home/thread and runs without notices (a unit-test
// handle with no provider). A non-nil tail is already running on its own
// goroutine, polling the thread's own rollout file and emitting notices
// via h.emit until ctx ends or the handle closes; the caller keeps it for
// the forwarder's terminal drain.
func (h *Handle) startContextTail(ctx context.Context) *rolloutContextTail {
	home := h.codexHome()
	threadID := h.SessionID()
	if home == "" || threadID == "" {
		return nil
	}
	tail := newRolloutContextTail(home, threadID)
	go tail.run(ctx, h.closed, h.emit) //nolint:gosec // G118: session-lifetime goroutine, ctx is honored
	return tail
}

// run tails until ctx ends or done closes, emitting notices via emit.
// Every emit must be non-blocking — this goroutine never slows the
// session — and it returns when ctx ends or done closes whether or not
// anyone reads the events.
func (t *rolloutContextTail) run(ctx context.Context, done <-chan struct{}, emit func(agent.Event)) {
	ticker := time.NewTicker(rolloutTailPollInterval)
	defer ticker.Stop()
	for {
		t.emitNew(emit)
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}
	}
}

// drain emits every notice appended since the last read. The forwarder
// calls it on the terminal event, before emitting that event, so the
// turn's last token_count reaches Events ahead of the result. A nil tail
// (a session that runs without notices) drains nothing.
func (t *rolloutContextTail) drain(emit func(agent.Event)) {
	if t == nil {
		return
	}
	t.emitNew(emit)
}

// emitNew reads the newly appended lines and emits their notices under
// mu, so the ticker and the terminal drain never interleave a read with
// another's emission.
func (t *rolloutContextTail) emitNew(emit func(agent.Event)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ev := range t.poll() {
		emit(ev)
	}
}

// poll resolves the session file (it appears after thread start — the
// rollout-flush race — so an absent file is retried next tick, not an
// error) and converts newly appended token_count events into notices.
func (t *rolloutContextTail) poll() []agent.Event {
	if t.home == "" || t.threadID == "" {
		return nil
	}
	path := findThreadRollout(t.home, t.threadID)
	if path == "" {
		return nil
	}
	if path != t.path {
		t.path, t.offset = path, 0
	}
	f, err := openRolloutFile(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() < t.offset {
		t.offset = 0
	}
	if info.Size() == t.offset {
		return nil
	}
	if _, err := f.Seek(t.offset, 0); err != nil {
		return nil
	}
	var out []agent.Event
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			if err != nil {
				return out
			}
			continue
		}
		if len(line) > 0 && line[len(line)-1] != '\n' {
			// A torn trailing line stays unread until the writer
			// completes it; the offset does not advance past it.
			return out
		}
		notice, perr := agent.ParseCodexTokenCountNotice(line, string(agent.HarnessCodex))
		if perr != nil || notice == nil {
			t.offset += int64(len(line))
			if err != nil {
				return out
			}
			continue
		}
		t.offset += int64(len(line))
		if ev := t.classify(notice); ev != nil {
			out = append(out, *ev)
		}
		if err != nil {
			return out
		}
	}
}

// classify turns one parsed notice into the event to emit, or nil when the
// notice reports nothing new: a threshold notice emits only on a rising
// bound crossing (re-arming below), and a compaction notice emits once per
// observed compaction identity. A threshold notice with no percentage
// carries only token counts the per-turn usage events already meter.
func (t *rolloutContextTail) classify(notice *agent.ContextUsageEvent) *agent.Event {
	if notice == nil {
		return nil
	}
	if notice.NoticeKind == agent.ContextUsageCompaction {
		identity := rolloutCompactionIdentity(notice)
		if identity == t.lastCompaction {
			return nil
		}
		t.lastCompaction = identity
		var ev agent.Event = *notice
		return &ev
	}
	if notice.UsedPercent == nil {
		return nil
	}
	seen := *notice.UsedPercent
	var fired bool
	for _, bound := range rolloutTailThresholdBounds() {
		switch {
		case seen >= bound && t.armed[bound]:
			t.armed[bound] = false
			fired = true
		case seen < bound:
			t.armed[bound] = true
		}
	}
	if !fired {
		return nil
	}
	var ev agent.Event = *notice
	return &ev
}

func rolloutCompactionIdentity(notice *agent.ContextUsageEvent) string {
	identity := string(notice.Harness) + "\x00" + notice.Source + "\x00"
	if notice.UsedPercent != nil {
		identity += strconv.FormatFloat(*notice.UsedPercent, 'f', -1, 64)
	}
	identity += "\x00"
	if notice.InputTokens != nil {
		identity += strconv.FormatInt(*notice.InputTokens, 10)
	}
	identity += "\x00"
	if notice.OutputTokens != nil {
		identity += strconv.FormatInt(*notice.OutputTokens, 10)
	}
	identity += "\x00"
	if notice.ContextWindowSize != nil {
		identity += strconv.FormatInt(*notice.ContextWindowSize, 10)
	}
	return identity
}
