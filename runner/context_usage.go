package runner

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Harness context/compaction watcher.
//
// An interactive session runs its harness's own UI under a PTY: no
// stream-json wire exists, so compaction and context-threshold crossings
// that the harness observes natively would otherwise never reach the
// session event stream. This watcher polls each harness's native context
// signal and delivers it as a typed agent.ContextUsageEvent — the session
// event the platform appends to its usage series:
//
//   - claude reads the statusline sidecar JSON (used_percentage,
//     total_input_tokens, total_output_tokens, context_window_size) at
//     sidecarPath;
//   - codex tails token_count events in the rollout JSONL at rolloutPath;
//   - pi converts its RPC usage object, when the session's RPC stream
//     carries one (see ObservePiRPCUsage), rather than polling a file.
//
// Every path is best-effort and rate-limited, never blocking the PTY. The
// watcher runs on its own goroutine, polls on a 1s ticker, and emits via a
// non-blocking send — when the consumer is slow, notices are dropped, never
// queued behind the terminal. Unknown measurements stay absent on every
// emitted notice, never zero: the parsers (agent.ParseClaudeSidecarNotice,
// agent.ParseCodexTokenCountNotice, agent.ParsePiRPCUsageNotice) omit what
// the harness did not report.
//
// Dedupe is structural: a sidecar whose bytes are unchanged since the last
// poll emits nothing, and a rollout offset only advances past lines already
// read. A compaction marker is emitted once per observed compaction state —
// the watcher remembers the last compaction identity it reported.
const contextUsagePollInterval = time.Second

// contextUsageThresholds are the used-percentage bounds that emit a
// threshold notice. A crossing is reported once per bound per direction:
// rising past a bound emits, and the bound re-arms only after the reading
// falls back below it.
var contextUsageThresholds = []float64{50, 75, 90}

// contextUsageSource describes one harness-native signal the watcher polls.
type contextUsageSource struct {
	harness string
	// sidecarPath is the statusline sidecar JSON file (claude).
	sidecarPath string
	// rolloutPath is the rollout JSONL file tailed for token_count
	// events (codex).
	rolloutPath string
}

// contextUsageWatcher polls the harness-native context signals for one
// session and emits typed notices. It is not safe for concurrent use; the
// single watcher goroutine owns it.
type contextUsageWatcher struct {
	source contextUsageSource

	mu sync.Mutex
	// lastSidecar holds the last sidecar bytes read, so an unchanged
	// file emits nothing.
	lastSidecar string
	// rolloutOffset is the rollout read offset; lines already read never
	// re-emit.
	rolloutOffset int64
	// armed tracks which threshold bounds still need a rising crossing
	// before they report again. A bound disarms when it fires and
	// re-arms when the reading falls back below it.
	armed map[float64]bool
	// lastCompaction is the last reported compaction identity
	// (harness + source + measurements), so a steady compacted state
	// reports once.
	lastCompaction string
}

func newContextUsageWatcher(source contextUsageSource) *contextUsageWatcher {
	armed := make(map[float64]bool, len(contextUsageThresholds))
	for _, bound := range contextUsageThresholds {
		armed[bound] = true
	}
	return &contextUsageWatcher{source: source, armed: armed}
}

// run polls until ctx ends, emitting notices via emit. Every emit must be
// non-blocking — this goroutine never slows the PTY — and it returns when
// ctx ends whether or not anyone reads the events.
func (w *contextUsageWatcher) run(ctx context.Context, emit func(agent.Event)) {
	ticker := time.NewTicker(contextUsagePollInterval)
	defer ticker.Stop()
	for {
		for _, ev := range w.poll() {
			emit(ev)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// poll reads every configured source once and returns the notices it
// yields. Pure aside from the file reads.
func (w *contextUsageWatcher) poll() []agent.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []agent.Event
	if w.source.sidecarPath != "" {
		if ev := w.pollSidecar(); ev != nil {
			out = append(out, *ev)
		}
	}
	if w.source.rolloutPath != "" {
		out = append(out, w.pollRollout()...)
	}
	return out
}

// pollSidecar reads the statusline sidecar file and converts a CHANGED
// document into notices: threshold crossings for rising used_percentage
// bounds, plus one compaction notice per newly observed compacted state.
func (w *contextUsageWatcher) pollSidecar() *agent.Event {
	//nolint:gosec // G304: path is the harness's own sidecar, resolved from the session's trusted config.
	raw, err := os.ReadFile(w.source.sidecarPath)
	if err != nil {
		return nil
	}
	if string(raw) == w.lastSidecar {
		return nil
	}
	w.lastSidecar = string(raw)
	notice, err := agent.ParseClaudeSidecarNotice(raw, w.source.harness)
	if err != nil || notice == nil {
		return nil
	}
	return w.classify(notice)
}

// pollRollout tails the rollout JSONL for new token_count events. A torn
// trailing line stays unread until the writer completes it; the offset does
// not advance past it. Malformed lines are skipped, not fatal.
func (w *contextUsageWatcher) pollRollout() []agent.Event {
	//nolint:gosec // G304: path is the harness's own rollout file, resolved from the session's trusted config.
	f, err := os.Open(w.source.rolloutPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() < w.rolloutOffset {
		w.rolloutOffset = 0
	}
	if info.Size() == w.rolloutOffset {
		return nil
	}
	if _, err := f.Seek(w.rolloutOffset, 0); err != nil {
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
			return out
		}
		notice, perr := agent.ParseCodexTokenCountNotice(line, w.source.harness)
		if perr != nil || notice == nil {
			w.rolloutOffset += int64(len(line))
			if err != nil {
				return out
			}
			continue
		}
		w.rolloutOffset += int64(len(line))
		if ev := w.classify(notice); ev != nil {
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
// observed compaction identity.
func (w *contextUsageWatcher) classify(notice *agent.ContextUsageEvent) *agent.Event {
	if notice == nil {
		return nil
	}
	if notice.NoticeKind == agent.ContextUsageCompaction {
		identity := compactionIdentity(notice)
		if identity == w.lastCompaction {
			return nil
		}
		w.lastCompaction = identity
		var ev agent.Event = *notice
		return &ev
	}
	if notice.UsedPercent == nil {
		// A threshold notice with no percentage carries only token counts
		// the per-turn usage events already meter: nothing new to report.
		return nil
	}
	seen := *notice.UsedPercent
	var fired bool
	for _, bound := range contextUsageThresholds {
		switch {
		case seen >= bound && w.armed[bound]:
			w.armed[bound] = false
			fired = true
		case seen < bound:
			w.armed[bound] = true
		}
	}
	if !fired {
		return nil
	}
	var ev agent.Event = *notice
	return &ev
}

func compactionIdentity(notice *agent.ContextUsageEvent) string {
	var b strings.Builder
	b.WriteString(notice.Harness)
	b.WriteByte(0)
	b.WriteString(notice.Source)
	b.WriteByte(0)
	if notice.UsedPercent != nil {
		b.WriteString(strconv.FormatFloat(*notice.UsedPercent, 'f', -1, 64))
	}
	b.WriteByte(0)
	if notice.InputTokens != nil {
		b.WriteString(strconv.FormatInt(*notice.InputTokens, 10))
	}
	b.WriteByte(0)
	if notice.OutputTokens != nil {
		b.WriteString(strconv.FormatInt(*notice.OutputTokens, 10))
	}
	b.WriteByte(0)
	if notice.ContextWindowSize != nil {
		b.WriteString(strconv.FormatInt(*notice.ContextWindowSize, 10))
	}
	return b.String()
}

// ObservePiRPCUsage converts one pi RPC usage object into the typed context
// notice for the session, or nil when the object carries no signal. It is
// the pi leg of the watcher: pi reports usage on its RPC stream rather than
// in a file, so the pi handle calls this per turn_end instead of polling.
// Threshold gating matches the file legs (classify), so all three harnesses
// report crossings the same way.
func ObservePiRPCUsage(usage *agent.PiRPCUsage, harness string, state *contextUsageWatcher) *agent.Event {
	if usage == nil {
		return nil
	}
	notice, err := agent.ParsePiRPCUsageNotice(usage, harness)
	if err != nil || notice == nil {
		return nil
	}
	if state == nil {
		var ev agent.Event = *notice
		return &ev
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.classify(notice)
}
