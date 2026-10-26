package claude

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// Live transcript usage tail for interactive sessions.
//
// While the session runs, its JSONL transcript is polled and each newly
// appended assistant message.usage is forwarded as the same LlmCallEvent the
// headless lane emits for a model call (see clijsonl.mapAssistant), so an
// interactive session's spend reaches the activity stream turn by turn. The
// statusline sidecar's cumulative USD total rides alongside as cost-only
// delta events (one LlmCallEvent per finite positive increase, carrying only
// ObservedCostUsd): it invents no turn or tokens, and the transcript remains
// the token and turn owner. The terminal session totals come from the whole-file sum at exit (see
// transcript_usage.go), not from these events: the events are the live meter,
// the exit sum is the reconciled total, and the runner's own budget meter
// already reconciles the two without double counting.
//
// The transcript is located through the Stop hook's durable locator, falling
// back to the conversation id pinned on the CLI argv before the first Stop.
// Both the live tail and exit sum use the same session-scoped locator.
//
// Posture: best-effort and rate-limited, never blocking the PTY. The tailer
// runs on its own goroutine, polls on a 1s ticker, caps lines and events per
// sweep, and emits via a non-blocking send — when the consumer is slow,
// usage events are dropped, never queued behind the terminal. The caps defer
// work to the next sweep rather than discarding it: a sweep stops BEFORE the
// line that would exceed a cap, leaving it unread. The final flush after the
// session ends re-reads the file whole (bounded sweeps), so dropped live
// events still count in the exit totals.
//
// A restart or resume re-reads the same file, so dedupe is structural: every
// message id already emitted is remembered for the tailer's life, and the
// exit sum dedupes independently over the whole file. Re-reading never
// double counts.
const (
	// interactiveUsagePollInterval is the cadence at which the tailer
	// re-reads the transcript. One second keeps usage near-live without
	// measurable IO pressure (a stat + occasional bounded read).
	interactiveUsagePollInterval = time.Second

	// interactiveUsageMaxLinesPerSweep bounds how many new transcript lines
	// one sweep consumes. A burst beyond it is consumed on later ticks.
	interactiveUsageMaxLinesPerSweep = 200

	// interactiveUsageMaxEventsPerSweep bounds how many usage events one
	// sweep emits — the rate limit that keeps a chatty session from
	// flooding the activity buffer.
	interactiveUsageMaxEventsPerSweep = 16

	// interactiveUsageMaxFinalSweeps bounds the end-of-session flush.
	// Trailing writes that landed with the child's exit may exceed one
	// sweep's caps; the flush keeps sweeping until nothing is pending or
	// this many sweeps have run.
	interactiveUsageMaxFinalSweeps = 16
)

// interactiveUsageTailer tracks the transcript read offset and the message
// ids already emitted across sweeps. It is not safe for concurrent use; the
// single tailer goroutine owns it.
type interactiveUsageTailer struct {
	hook *stopHookChannel
	// activity is the session's hook-telemetry relay, or nil when the
	// session runs without it. Swept beside the transcript on every tick
	// so lifecycle markers share the usage tail's cadence and flush.
	activity *activityrelay.Tailer
	// path is the transcript offset belongs to. Clearing the conversation
	// moves the session to a new transcript; the read restarts at its first
	// byte rather than at the old file's offset.
	path   string
	offset int64
	seen   map[string]struct{}
	// sidecarMax is the greatest accepted statusline cumulative total so far.
	// The statusline value is a session-cumulative snapshot: each sweep emits
	// only the finite positive increase over this baseline as a cost-only
	// delta, and repeated or regressing snapshots never move it — so a later
	// catch-up cannot recharge spend the session already reported.
	sidecarMax float64
}

func newInteractiveUsageTailer(hook *stopHookChannel) *interactiveUsageTailer {
	return &interactiveUsageTailer{hook: hook, seen: make(map[string]struct{})}
}

// newInteractiveUsageTailerWithActivity is newInteractiveUsageTailer with
// the session's activity relay attached.
func newInteractiveUsageTailerWithActivity(hook *stopHookChannel, activity *activityrelay.Tailer) *interactiveUsageTailer {
	return &interactiveUsageTailer{hook: hook, activity: activity, seen: make(map[string]struct{})}
}

// run polls until sessionDone closes, emitting newly mapped usage events via
// emit, then flushes whatever the child wrote on its way out. Every emit
// must be non-blocking — this goroutine never slows the PTY, and it returns
// once the flush is done whether or not anyone reads the events.
func (t *interactiveUsageTailer) run(sessionDone <-chan struct{}, emit func(agent.Event)) {
	ticker := time.NewTicker(interactiveUsagePollInterval)
	defer ticker.Stop()
	for {
		for _, ev := range t.sweepBoth() {
			emit(ev)
		}
		select {
		case <-sessionDone:
			t.flushBoth(emit)
			return
		case <-ticker.C:
		}
	}
}

// sweepBoth sweeps the activity relay beside the transcript and the
// statusline sidecar: hook fires become harness-lifecycle events on the
// same channel as the usage meter, and the sidecar's cumulative total
// becomes one cost-only delta event (see sweepSidecarCost).
func (t *interactiveUsageTailer) sweepBoth() []agent.Event {
	events := sweepActivityRelay(t.activity)
	events = append(events, t.sweep()...)
	if ev, ok := t.sweepSidecarCost(); ok {
		events = append(events, ev)
	}
	return events
}

// flushBoth flushes trailing transcript writes, then the trailing sidecar
// cost, then trailing relay lines, so activity the child wrote on its way
// out is still delivered.
func (t *interactiveUsageTailer) flushBoth(emit func(agent.Event)) {
	t.flush(emit)
	if ev, ok := t.sweepSidecarCost(); ok {
		emit(ev)
	}
	for _, ev := range sweepActivityRelay(t.activity) {
		emit(ev)
	}
}

// flush sweeps until nothing is pending, bounded by
// interactiveUsageMaxFinalSweeps.
func (t *interactiveUsageTailer) flush(emit func(agent.Event)) {
	for range interactiveUsageMaxFinalSweeps {
		events, more := t.sweepCapped()
		for _, ev := range events {
			emit(ev)
		}
		if !more {
			return
		}
	}
}

// sweep reads newly appended usage lines and maps them to LlmCallEvents. It
// reports nothing pending beyond what it emits: the exit sum re-reads the
// file whole, so a cap that defers work here only delays the live meter.
func (t *interactiveUsageTailer) sweep() []agent.Event {
	events, _ := t.sweepCapped()
	return events
}

// sweepSidecarCost maps the statusline sidecar's cumulative USD snapshot to
// one cost-only LlmCallEvent carrying the increase since the greatest total
// accepted so far. The sidecar value is session-cumulative, so the emitted
// delta is current minus maxPreviouslyAccepted: equality, regression,
// malformed, non-finite, negative, zero, and absent readings emit nothing
// and never move the baseline, so a later catch-up cannot recharge old
// spend. The event is cost-only: it invents no turn or tokens
// (TurnCompleted stays false, token counts stay zero) and the transcript
// remains the token and turn owner; the terminal withTranscriptCost
// reconciliation stays authoritative and is never added to this delta sum.
func (t *interactiveUsageTailer) sweepSidecarCost() (agent.LlmCallEvent, bool) {
	if t == nil || t.hook == nil {
		return agent.LlmCallEvent{}, false
	}
	total := t.hook.readSidecarUsage().TotalCostUsd
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return agent.LlmCallEvent{}, false
	}
	if total <= t.sidecarMax {
		return agent.LlmCallEvent{}, false
	}
	delta := total - t.sidecarMax
	if delta <= 0 || math.IsNaN(delta) || math.IsInf(delta, 0) {
		return agent.LlmCallEvent{}, false
	}
	t.sidecarMax = total
	cost := delta
	return agent.LlmCallEvent{
		ObservedCostUsd: &cost,
		UsageSource:     agent.LlmUsageProvider,
	}, true
}

// sweepCapped is sweep with a more flag reporting that a cap left input
// pending for a later sweep.
func (t *interactiveUsageTailer) sweepCapped() (events []agent.Event, more bool) {
	if t.hook == nil {
		return nil, false
	}
	path, ok, err := t.hook.transcriptPath()
	if err != nil || !ok {
		return nil, false
	}
	if path != t.path {
		t.path, t.offset = path, 0
	}
	f, err := openTranscript(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if info.Size() < t.offset {
		// The transcript shrank (rotation/rewrite): restart from zero. The
		// seen set still holds every emitted message id, so the re-read
		// emits nothing twice.
		t.offset = 0
	}
	if info.Size() == t.offset {
		return nil, false
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, false
	}
	reader := bufio.NewReader(f)
	lines := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			if err != nil {
				return events, false
			}
			continue
		}
		if len(line) > 0 && line[len(line)-1] != '\n' {
			// A torn trailing line stays unread until the writer completes
			// it; the offset does not advance past it.
			return events, false
		}
		if lines >= interactiveUsageMaxLinesPerSweep || len(events) >= interactiveUsageMaxEventsPerSweep {
			return events, true
		}
		lines++
		if ev, ok := mapTranscriptUsageEvent(line, t.seen, t.hook.createdAt); ok {
			events = append(events, ev)
		}
		t.offset += int64(len(line))
		if err != nil {
			return events, false
		}
	}
}

// mapTranscriptUsageEvent maps one transcript line to the LlmCallEvent the
// headless lane emits for the equivalent model call. Lines already in seen
// map to nothing so a re-read never double-emits; usage is credited per
// message id, the same identity the exit sum dedupes on, and only for the
// lines the exit sum counts (classifyUsageLine): a line stamped before since
// or carrying an impossible count emits nothing.
func mapTranscriptUsageEvent(line []byte, seen map[string]struct{}, since time.Time) (agent.LlmCallEvent, bool) {
	var parsed transcriptUsageLine
	if err := json.Unmarshal(line, &parsed); err != nil {
		return agent.LlmCallEvent{}, false
	}
	if classifyUsageLine(parsed, since) != usageCredit {
		return agent.LlmCallEvent{}, false
	}
	u := parsed.Message.Usage
	key, ok := usageLineKey(parsed)
	if !ok {
		return agent.LlmCallEvent{}, false
	}
	if _, dup := seen[key]; dup {
		return agent.LlmCallEvent{}, false
	}
	seen[key] = struct{}{}
	return agent.LlmCallEvent{
		GenerationID:      parsed.Message.ID,
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
		UsageSource:       agent.LlmUsageProvider,
		TurnCompleted:     true,
	}, true
}
