package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func writeTranscriptLine(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G304: test fixture path under t.TempDir().
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func transcriptMessage(id string, msg any) map[string]any {
	return map[string]any{"type": "message", "id": id, "parentId": "p", "timestamp": "t", "message": msg}
}

// TestInteractiveTranscriptTailSweep_MapsTurnsToolsAndResultsInOrder feeds a
// fixture transcript through the tailer and asserts the emitted agent events,
// in order: assistant text, one ToolUseEvent per tool call, one
// ToolResultEvent per result, and one per-turn usage event.
func TestInteractiveTranscriptTailSweep_MapsTurnsToolsAndResultsInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	path := filepath.Join(dir, "sess.jsonl")
	writeTranscriptLine(t, path, map[string]any{"type": "session", "id": "s"})
	writeTranscriptLine(t, path, transcriptMessage("m-user", map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "text", "text": "do it"}},
	}))
	writeTranscriptLine(t, path, transcriptMessage("m-asst", map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "running it"},
			map[string]any{"type": "toolCall", "id": "call-1", "name": "bash", "arguments": map[string]any{"command": "echo hi"}},
			map[string]any{"type": "toolCall", "id": "call-2", "name": "read", "arguments": map[string]any{"path": "f"}},
		},
		"provider": "anthropic", "model": "m",
		"usage": map[string]any{"input": 10, "output": 20, "cacheRead": 48000, "cacheWrite": 512, "cost": map[string]any{"total": 0.01}},
	}))
	writeTranscriptLine(t, path, transcriptMessage("m-res1", map[string]any{
		"role": "toolResult", "toolCallId": "call-1", "toolName": "bash",
		"content": []any{map[string]any{"type": "text", "text": "hi\n"}}, "isError": false,
	}))

	events, _ := tailer.sweep()

	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind()))
	}
	want := []string{
		string(agent.EventAssistantText), // "running it"
		string(agent.EventToolUse),       // bash call-1
		string(agent.EventToolUse),       // read call-2
		string(agent.EventLlmCall),       // turn usage
		string(agent.EventToolResult),    // bash result
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	text := events[0].(agent.AssistantTextEvent)
	if text.Text != "running it" {
		t.Errorf("assistant text = %q", text.Text)
	}
	use := events[1].(agent.ToolUseEvent)
	if use.ToolName != "bash" || use.ToolUseID != "call-1" {
		t.Errorf("tool use = %+v", use)
	}
	llm := events[3].(agent.LlmCallEvent)
	if llm.InputTokens != 10 || llm.OutputTokens != 20 || !llm.TurnCompleted {
		t.Errorf("llm call = %+v", llm)
	}
	if llm.CachedInputTokens != 48000 || llm.CacheWriteTokens != 512 {
		t.Errorf("llm cache buckets = read %d write %d; want read 48000 write 512", llm.CachedInputTokens, llm.CacheWriteTokens)
	}
	if llm.ObservedCostUsd == nil || *llm.ObservedCostUsd != 0.01 {
		t.Errorf("llm cost = %+v", llm.ObservedCostUsd)
	}
	res := events[4].(agent.ToolResultEvent)
	if res.ToolName != "bash" || res.Content != "hi\n" || res.IsError {
		t.Errorf("tool result = %+v", res)
	}

	// A second sweep emits nothing — offsets advanced past every line.
	if again, _ := tailer.sweep(); len(again) != 0 {
		t.Fatalf("second sweep emitted %d events, want 0", len(again))
	}

	// A torn trailing write (no newline) is not consumed until completed.
	//nolint:gosec // G304: test fixture path under t.TempDir().
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","id":"m-torn",`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if again, _ := tailer.sweep(); len(again) != 0 {
		t.Fatalf("torn-line sweep emitted %d events, want 0", len(again))
	}
}

// TestInteractiveTranscriptTail_IgnoresNonMessageLines pins the negative
// space: session metadata, unknown types, and malformed lines map to nothing.
func TestInteractiveTranscriptTail_IgnoresNonMessageLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	path := filepath.Join(dir, "sess.jsonl")
	writeTranscriptLine(t, path, map[string]any{"type": "session", "id": "s"})
	writeTranscriptLine(t, path, map[string]any{"type": "model_change", "id": "x"})
	writeTranscriptLine(t, path, transcriptMessage("m-user", map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}},
	}))
	//nolint:gosec // G304: test fixture path under t.TempDir().
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not json\n")
	_ = f.Close()

	if events, _ := tailer.sweep(); len(events) != 0 {
		t.Fatalf("sweep emitted %d events, want 0", len(events))
	}
}

// TestInteractiveTranscriptTail_SkipsConfigAndInjectedDirs pins that the
// state-dir walk never descends into the per-session config home or the
// injected-extensions dir, which hold no transcripts.
func TestInteractiveTranscriptTail_SkipsConfigAndInjectedDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	for _, sub := range []string{agentHomeDir, injectedExtensionsDir} {
		p := filepath.Join(dir, sub, "sess.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTranscriptLine(t, p, transcriptMessage("m", map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
		}))
	}
	if events, _ := tailer.sweep(); len(events) != 0 {
		t.Fatalf("sweep emitted %d events from excluded dirs, want 0", len(events))
	}
}

// TestInteractiveTranscriptTail_CapsEventsPerSweep pins the rate limit: a
// transcript burst maps many events but one sweep emits at most the cap, and
// the remainder is deferred to the next sweep — never dropped, never replayed.
func TestInteractiveTranscriptTail_CapsEventsPerSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	path := filepath.Join(dir, "sess.jsonl")
	for i := 0; i < interactiveTranscriptMaxEventsPerSweep+10; i++ {
		writeTranscriptLine(t, path, transcriptMessage(jsonID(i), map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
		}))
	}
	first, more := tailer.sweep()
	if len(first) != interactiveTranscriptMaxEventsPerSweep || !more {
		t.Fatalf("first sweep emitted %d events (more=%v), want cap %d with more pending", len(first), more, interactiveTranscriptMaxEventsPerSweep)
	}
	rest, more := tailer.sweep()
	if len(rest) != 10 || more {
		t.Fatalf("second sweep emitted %d events (more=%v), want the 10 deferred and nothing pending", len(rest), more)
	}
	if again, _ := tailer.sweep(); len(again) != 0 {
		t.Fatalf("third sweep emitted %d events, want 0 (no replay)", len(again))
	}
}

func jsonID(i int) string {
	return "m-" + strings.Repeat("a", 4) + string(rune('0'+i%10)) + string(rune('a'+i/10%26))
}

// toolCallLine is one assistant transcript line carrying a single tool call.
func toolCallLine(id, name string) map[string]any {
	return transcriptMessage(id, map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "toolCall", "id": "call-" + id, "name": name, "arguments": map[string]any{}},
		},
	})
}

// transcriptToolNames returns the ToolName of every ToolUseEvent, in order.
func transcriptToolNames(events []agent.Event) []string {
	var names []string
	for _, ev := range events {
		if use, ok := ev.(agent.ToolUseEvent); ok {
			names = append(names, use.ToolName)
		}
	}
	return names
}

// TestInteractiveTranscriptTail_IgnoresEarlierSessionTranscripts is the
// shared-state-dir guard. Headless runs, retained workareas, and shared-mode
// workareas leave other sessions' transcripts in the same .pi directory;
// none of them may be posted as this session's activity — neither their
// existing lines nor lines appended after spawn — and they must not crowd
// the live transcript out of a sweep. The 8-file case is the old starvation
// shape: the earlier files sort before the live one by name.
func TestInteractiveTranscriptTail_IgnoresEarlierSessionTranscripts(t *testing.T) {
	t.Parallel()
	for _, earlier := range []int{1, interactiveTranscriptMaxFilesPerSweep} {
		t.Run(fmt.Sprintf("%d_earlier", earlier), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var oldPaths []string
			for i := 0; i < earlier; i++ {
				path := filepath.Join(dir, fmt.Sprintf("2026-01-0%d_old.jsonl", i))
				writeTranscriptLine(t, path, toolCallLine(fmt.Sprintf("old-%d", i), fmt.Sprintf("OLD_SESSION_TOOL_%d", i)))
				oldPaths = append(oldPaths, path)
			}
			tailer := newInteractiveTranscriptTailer(dir) // constructed before spawn
			writeTranscriptLine(t, filepath.Join(dir, "2026-12-31_live.jsonl"), toolCallLine("live", "LIVE_TOOL"))
			writeTranscriptLine(t, oldPaths[0], toolCallLine("old-late", "OLD_SESSION_TOOL_LATE"))

			events, _ := tailer.sweep()
			if got := transcriptToolNames(events); !slices.Equal(got, []string{"LIVE_TOOL"}) {
				t.Fatalf("tool calls = %v, want only [LIVE_TOOL]", got)
			}
		})
	}
}

// TestInteractiveTranscriptTail_VisitsNewestTranscriptFirst pins the
// starvation guard among this session's own files: when more transcripts
// have unread bytes than one sweep visits, the most recently written one is
// in the first sweep (even though it sorts last by name) and the rest
// follow on the next.
func TestInteractiveTranscriptTail_VisitsNewestTranscriptFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	base := time.Now().Add(-time.Hour)
	n := interactiveTranscriptMaxFilesPerSweep + 1
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("backlog-%02d.jsonl", i))
		name := fmt.Sprintf("BACKLOG_%d", i)
		if i == n-1 {
			name = "NEWEST_TOOL"
		}
		writeTranscriptLine(t, path, toolCallLine(fmt.Sprintf("b-%d", i), name))
		mod := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	first, more := tailer.sweep()
	if names := transcriptToolNames(first); !slices.Contains(names, "NEWEST_TOOL") || !more {
		t.Fatalf("first sweep tool calls = %v (more=%v), want NEWEST_TOOL with the oldest file pending", names, more)
	}
	second, _ := tailer.sweep()
	if names := transcriptToolNames(second); !slices.Equal(names, []string{"BACKLOG_0"}) {
		t.Fatalf("second sweep tool calls = %v, want [BACKLOG_0]", names)
	}
}

// TestInteractiveTranscriptTail_SkipsOversizedLine pins that a single line
// longer than the read window is skipped through its newline instead of
// wedging the tail at its offset forever — whether the line is complete when
// first seen or still being written.
func TestInteractiveTranscriptTail_SkipsOversizedLine(t *testing.T) {
	t.Parallel()
	hugeText := strings.Repeat("x", interactiveTranscriptMaxReadBytes)
	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tailer := newInteractiveTranscriptTailer(dir)
		path := filepath.Join(dir, "sess.jsonl")
		writeTranscriptLine(t, path, transcriptMessage("huge", map[string]any{
			"role": "toolResult", "toolName": "bash", "toolCallId": "c",
			"content": []any{map[string]any{"type": "text", "text": hugeText}},
		}))
		writeTranscriptLine(t, path, toolCallLine("after", "TOOL_AFTER_HUGE_LINE"))

		var events []agent.Event
		for range 4 {
			swept, _ := tailer.sweep()
			events = append(events, swept...)
		}
		if len(events) != 1 || !slices.Equal(transcriptToolNames(events), []string{"TOOL_AFTER_HUGE_LINE"}) {
			t.Fatalf("events = %d, tool calls = %v, want exactly [TOOL_AFTER_HUGE_LINE]", len(events), transcriptToolNames(events))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := tailer.files[path].offset; got != info.Size() {
			t.Fatalf("offset = %d, want end of file %d", got, info.Size())
		}
	})
	t.Run("still_being_written", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tailer := newInteractiveTranscriptTailer(dir)
		path := filepath.Join(dir, "sess.jsonl")
		appendRaw := func(text string) {
			//nolint:gosec // G304: test fixture path under t.TempDir().
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if _, err := f.WriteString(text); err != nil {
				t.Fatal(err)
			}
		}
		appendRaw(`{"type":"message","id":"huge","message":{"role":"assistant","content":[{"type":"text","text":"` + hugeText + "yy")
		if events, _ := tailer.sweep(); len(events) != 0 {
			t.Fatalf("partial oversized line emitted %d events", len(events))
		}
		appendRaw(`"}]}}` + "\n")
		writeTranscriptLine(t, path, toolCallLine("after", "TOOL_AFTER_HUGE_LINE"))
		var names []string
		for range 3 {
			swept, _ := tailer.sweep()
			names = append(names, transcriptToolNames(swept)...)
		}
		if !slices.Equal(names, []string{"TOOL_AFTER_HUGE_LINE"}) {
			t.Fatalf("tool calls = %v, want [TOOL_AFTER_HUGE_LINE]", names)
		}
	})
}

// TestInteractiveTranscriptTail_LinesPastCapProcessedNextSweep pins that the
// per-sweep line cap defers rather than discards: line 201 is mapped on the
// next sweep.
func TestInteractiveTranscriptTail_LinesPastCapProcessedNextSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	path := filepath.Join(dir, "sess.jsonl")
	for i := 0; i < interactiveTranscriptMaxLinesPerSweep; i++ {
		writeTranscriptLine(t, path, map[string]any{"type": "session_info", "id": fmt.Sprintf("info-%d", i)})
	}
	writeTranscriptLine(t, path, toolCallLine("line-201", "TOOL_AT_LINE_201"))

	first, more := tailer.sweep()
	if len(first) != 0 || !more {
		t.Fatalf("first sweep emitted %d events (more=%v), want 0 with line 201 pending", len(first), more)
	}
	second, more := tailer.sweep()
	if names := transcriptToolNames(second); !slices.Equal(names, []string{"TOOL_AT_LINE_201"}) || more {
		t.Fatalf("second sweep tool calls = %v (more=%v), want [TOOL_AT_LINE_201] and nothing pending", names, more)
	}
}

// TestInteractiveTranscriptTail_FlushDrainsPastOneSweep pins the end-of-
// session flush: trailing writes larger than one sweep's caps all reach emit.
func TestInteractiveTranscriptTail_FlushDrainsPastOneSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	path := filepath.Join(dir, "sess.jsonl")
	n := 5*interactiveTranscriptMaxEventsPerSweep + 7
	for i := 0; i < n; i++ {
		writeTranscriptLine(t, path, toolCallLine(fmt.Sprintf("t-%d", i), "TRAILING_TOOL"))
	}
	var got []agent.Event
	tailer.flush(func(ev agent.Event) { got = append(got, ev) })
	if len(got) != n {
		t.Fatalf("flush emitted %d events, want %d", len(got), n)
	}
}

// fakePiTranscriptPrelude is a fake-pi script prelude: it finds the
// --session-dir the interactive argv passes and defines tool_line, which
// appends one tool-call transcript line to this session's own transcript
// (a file the child creates after spawn, as real pi does).
const fakePiTranscriptPrelude = `
dir=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--session-dir" ]; then dir="$2"; shift 2; continue; fi
  shift
done
live="$dir/2099-01-01T00-00-00-000Z_live.jsonl"
tool_line() {
  printf '{"type":"message","id":"%s","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-%s","name":"%s","arguments":{}}]}}\n' "$1" "$1" "$2" >> "$live"
}
`

// seedEarlierTranscript leaves another session's transcript in the
// checkout's shared state dir before spawn.
func seedEarlierTranscript(t *testing.T, workdir string) {
	t.Helper()
	stateDir := filepath.Join(workdir, piStateDir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTranscriptLine(t, filepath.Join(stateDir, "2026-01-01T00-00-00-000Z_old.jsonl"), toolCallLine("old", "OLD_SESSION_TOOL"))
}

// TestSpawn_Interactive_TranscriptHandleTearsDownWithoutReader drives the
// production Spawn wiring (a fake pi under a real PTY) with a caller that
// never drains Events while the child writes far more transcript activity
// than the channel holds. Every helper goroutine — the 1 s transcript poller
// included — must still return at session end and Events must close. The
// buffered stream then holds only this session's activity and ends with the
// terminal ResultEvent. RED if emit blocks (the tailer wedges on the full
// channel) or if transcript events may take the coarse slots (the terminal
// ResultEvent send wedges).
func TestSpawn_Interactive_TranscriptHandleTearsDownWithoutReader(t *testing.T) {
	workdir := t.TempDir()
	seedEarlierTranscript(t, workdir)
	p := newFakeInteractivePiProvider(t, fakePiTranscriptPrelude+`
for i in $(seq 1 200); do tool_line "live-$i" LIVE_TOOL; done
`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	wrapped, ok := h.(*interactiveStateLossHandle)
	if !ok {
		t.Fatalf("interactive Spawn returned %T, want the transcript-tailing handle", h)
	}

	select {
	case <-wrapped.finished:
	case <-time.After(20 * time.Second):
		t.Fatal("handle helpers never returned with no reader: a transcript or coarse send is wedged on the full event channel (goroutine leak)")
	}

	var events []agent.Event
	for ev := range h.Events() {
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("no events buffered")
	}
	if result, ok := events[len(events)-1].(agent.ResultEvent); !ok || !result.Success {
		t.Fatalf("last event = %#v, want the successful terminal ResultEvent", events[len(events)-1])
	}
	names := transcriptToolNames(events)
	if len(names) == 0 || len(names) > interactiveTranscriptEventSlots {
		t.Fatalf("buffered %d transcript tool calls, want 1..%d", len(names), interactiveTranscriptEventSlots)
	}
	for _, name := range names {
		if name != "LIVE_TOOL" {
			t.Fatalf("tool call %q is not this session's (earlier transcript leaked into the activity stream)", name)
		}
	}
}

// TestSpawn_Interactive_TranscriptFinalFlushPrecedesResult drives the
// production Spawn wiring with a fake pi that writes its last transcript
// line just before exiting — after the tailer's first sweep and before its
// first tick, so only the final flush can deliver it. The line must reach
// Events, ActivityFlushed must have closed by the time the terminal
// ResultEvent is forwarded, and the ResultEvent must come last.
func TestSpawn_Interactive_TranscriptFinalFlushPrecedesResult(t *testing.T) {
	workdir := t.TempDir()
	seedEarlierTranscript(t, workdir)
	p := newFakeInteractivePiProvider(t, fakePiTranscriptPrelude+`
sleep 0.3
tool_line final FINAL_TOOL
`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	flusher, ok := h.(agent.InteractiveActivityFlusher)
	if !ok {
		t.Fatalf("interactive Spawn returned %T, which does not signal its activity flush", h)
	}

	var events []agent.Event
	deadline := time.After(20 * time.Second)
read:
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				break read
			}
			if _, terminal := ev.(agent.ResultEvent); terminal {
				select {
				case <-flusher.ActivityFlushed():
				default:
					t.Error("terminal ResultEvent forwarded before the transcript flush completed")
				}
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatal("timed out reading interactive events")
		}
	}
	if names := transcriptToolNames(events); !slices.Equal(names, []string{"FINAL_TOOL"}) {
		t.Fatalf("tool calls = %v, want [FINAL_TOOL] delivered by the final flush", names)
	}
	if _, ok := events[len(events)-1].(agent.ResultEvent); !ok {
		t.Fatalf("last event = %#v, want the terminal ResultEvent", events[len(events)-1])
	}
}

// TestSpawn_Interactive_RealBinary_TranscriptToolCallReachesEvents is the
// real-`pi` conformance control for the transcript tail: the stub model asks
// pi for one bash call, real pi records it in the transcript it creates
// under --session-dir after spawn, and the handle surfaces it as a
// ToolUseEvent (plus the tool's result) on Events. An earlier session's
// transcript seeded in the same state dir must not surface.
func TestSpawn_Interactive_RealBinary_TranscriptToolCallReachesEvents(t *testing.T) {
	realBinaryAvailable(t)
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}

	stub := newRealBinaryStub(t, realBinaryModel)
	stub.responses = []stubResponse{
		{ToolCall: &stubToolCall{ID: "transcript-call", Name: "bash", Arguments: `{"command":"echo transcript-marker"}`}},
		{Text: "transcript-turn-done"},
	}
	workdir := t.TempDir()
	seedEarlierTranscript(t, workdir)
	spec := realBinarySpec(workdir, "run the requested command", stub.baseURL())
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}

	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New(real pi): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	var names []string
	gotResult := false
	deadline := time.After(40 * time.Second)
	for !gotResult || !slices.Contains(names, "bash") {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatalf("events closed before the transcript tool call surfaced; tool calls = %v", names)
			}
			switch e := ev.(type) {
			case agent.ToolUseEvent:
				names = append(names, e.ToolName)
			case agent.ToolResultEvent:
				if e.ToolName == "bash" && strings.Contains(e.Content, "transcript-marker") {
					gotResult = true
				}
			}
		case <-deadline:
			t.Fatalf("real pi transcript activity did not surface; tool calls = %v, bash result seen = %v", names, gotResult)
		}
	}
	if slices.Contains(names, "OLD_SESSION_TOOL") {
		t.Fatalf("an earlier session's transcript surfaced as this session's activity: %v", names)
	}
}

// TestInteractiveTranscriptTail_ImpossibleTokenCountsMapToRefused pins the
// transcript's half of the "never a wrong number" rule: a usage count no
// model call produces (negative, or beyond the exact JSON integer range,
// where a plain float-to-int conversion is platform-dependent) maps to -1,
// the value the runner's usage meter refuses, while in-range counts pass
// through unchanged.
func TestInteractiveTranscriptTail_ImpossibleTokenCountsMapToRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tailer := newInteractiveTranscriptTailer(dir)
	path := filepath.Join(dir, "sess.jsonl")
	writeTranscriptLine(t, path, transcriptMessage("m-bad", map[string]any{
		"role": "assistant",
		"usage": map[string]any{
			"input": -5000, "output": 10, "cacheRead": 1e300, "cacheWrite": float64(transcriptTokenLimit),
		},
	}))

	events, _ := tailer.sweep()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 usage event", len(events))
	}
	llm, ok := events[0].(agent.LlmCallEvent)
	if !ok {
		t.Fatalf("event = %T, want agent.LlmCallEvent", events[0])
	}
	if llm.InputTokens != -1 || llm.CachedInputTokens != -1 {
		t.Errorf("impossible counts = input %d cacheRead %d; want -1 each (refused by the meter)", llm.InputTokens, llm.CachedInputTokens)
	}
	if llm.OutputTokens != 10 || llm.CacheWriteTokens != transcriptTokenLimit {
		t.Errorf("in-range counts = output %d cacheWrite %d; want 10 and %d", llm.OutputTokens, llm.CacheWriteTokens, int64(transcriptTokenLimit))
	}
}
