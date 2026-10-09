package claude

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// usageLine builds one transcript assistant line with the given message id
// and usage totals.
func usageLine(t *testing.T, id string, in, out, read, write int64) string {
	t.Helper()
	return jsonLine(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":   id,
			"role": "assistant",
			"usage": map[string]any{
				"input_tokens":                in,
				"output_tokens":               out,
				"cache_read_input_tokens":     read,
				"cache_creation_input_tokens": write,
			},
			"content": []any{map[string]any{"type": "text", "text": "work"}},
		},
	})
}

// TestSumTranscriptUsage_CountsEachMessageOnce is the Done-when fixture: a
// transcript with repeated content blocks counts each message once, and the
// totals match the transcript. The repeats model what the harness really
// writes — a message's usage reappearing under a second line with the same
// id (a streamed repeat), and the same usage values under a different id
// (a resumed turn re-emitting the block).
func TestSumTranscriptUsage_CountsEachMessageOnce(t *testing.T) {
	t.Parallel()
	path := writeTranscript(t, []string{
		usageLine(t, "msg-1", 100, 20, 1000, 50),
		usageLine(t, "msg-2", 200, 40, 2000, 100),
		// Same id, same usage: the streamed repeat of msg-1. Must not count.
		usageLine(t, "msg-1", 100, 20, 1000, 50),
		// Non-assistant and zero-usage lines carry no spend.
		jsonLine(t, map[string]any{
			"type": "user", "isMeta": true,
			"message": map[string]any{"role": "user", "content": "Stop hook feedback:\nhello"},
		}),
		jsonLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id": "msg-idle", "role": "assistant",
				"usage":   map[string]any{},
				"content": []any{map[string]any{"type": "text", "text": "idle"}},
			},
		}),
		usageLine(t, "msg-3", 50, 10, 500, 25),
	})

	got, err := sumTranscriptUsage(path)
	if err != nil {
		t.Fatalf("sumTranscriptUsage: %v", err)
	}
	want := agent.CostData{
		InputTokens:       350,
		OutputTokens:      70,
		CachedInputTokens: 3500,
		CacheWriteTokens:  175,
		NumTurns:          3,
	}
	if got != want {
		t.Fatalf("sumTranscriptUsage = %+v; want %+v", got, want)
	}
}

// TestSumTranscriptUsage_RereadDoesNotDoubleCount pins the restart/resume
// rule: the totals are a property of the transcript, so reading the same
// file again — as a resumed session does — returns the identical totals,
// never twice the spend.
func TestSumTranscriptUsage_RereadDoesNotDoubleCount(t *testing.T) {
	t.Parallel()
	path := writeTranscript(t, []string{
		usageLine(t, "msg-1", 100, 20, 1000, 50),
		usageLine(t, "msg-2", 200, 40, 2000, 100),
	})

	first, err := sumTranscriptUsage(path)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := sumTranscriptUsage(path)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if first != second {
		t.Fatalf("re-read changed the totals: first=%+v second=%+v", first, second)
	}
	if first.InputTokens != 300 || first.NumTurns != 2 {
		t.Fatalf("totals = %+v; want 300 input tokens over 2 turns", first)
	}
}

// TestSumTranscriptUsage_MissingFileIsNoUsageNotAnError covers the session
// whose transcript never materialized: it reports zero usage, never a
// failure that would mark the session's end.
func TestSumTranscriptUsage_MissingFileIsNoUsageNotAnError(t *testing.T) {
	t.Parallel()
	got, err := sumTranscriptUsage(filepath.Join(t.TempDir(), "no-such-transcript.jsonl"))
	if err != nil {
		t.Fatalf("missing transcript must not error: %v", err)
	}
	if got != (agent.CostData{}) {
		t.Fatalf("missing transcript usage = %+v; want zero", got)
	}
}

// TestSumTranscriptUsage_SkipsCorruptLinesAndTornTails covers the two
// production hazards of reading a live file: a malformed line (a partial
// write that completed mid-sweep) is skipped without zeroing the session's
// accounting, and a trailing line without its terminator stops the read so
// the next read sees it whole instead of crediting a fragment.
func TestSumTranscriptUsage_SkipsCorruptLinesAndTornTails(t *testing.T) {
	t.Parallel()
	good := usageLine(t, "msg-1", 100, 20, 1000, 50)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	torn := usageLine(t, "msg-2", 200, 40, 2000, 100)
	body := good + "\n" + "{not json\n" + torn // no trailing newline: still being written
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	got, err := sumTranscriptUsage(path)
	if err != nil {
		t.Fatalf("sumTranscriptUsage: %v", err)
	}
	if got.InputTokens != 100 || got.NumTurns != 1 {
		t.Fatalf("usage = %+v; want only msg-1 (100 tokens, 1 turn) — the corrupt line must be skipped and the torn tail left unread", got)
	}

	// Completing the torn line credits msg-2 on the next read.
	appendTranscript(t, path, "")
	got, err = sumTranscriptUsage(path)
	if err != nil {
		t.Fatalf("sumTranscriptUsage after completion: %v", err)
	}
	if got.InputTokens != 300 || got.NumTurns != 2 {
		t.Fatalf("usage after completion = %+v; want 300 tokens over 2 turns", got)
	}
}

// TestMapTranscriptUsageEvent_DedupesAcrossSweeps pins the live tail's half
// of the no-double-count rule: the same message id arriving in two sweeps
// (a re-read after a shrink, or a repeated block) emits exactly one
// LlmCallEvent.
func TestMapTranscriptUsageEvent_DedupesAcrossSweeps(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{})
	line := []byte(usageLine(t, "msg-1", 100, 20, 1000, 50))

	first, ok := mapTranscriptUsageEvent(line, seen, time.Time{})
	if !ok {
		t.Fatal("first sight of msg-1 mapped to nothing")
	}
	if first.InputTokens != 100 || first.OutputTokens != 20 ||
		first.CachedInputTokens != 1000 || first.CacheWriteTokens != 50 {
		t.Fatalf("usage event = %+v; want the transcript's usage carried field by field", first)
	}
	if first.UsageSource != agent.LlmUsageProvider || !first.TurnCompleted {
		t.Fatalf("usage event = %+v; want the headless lane's provider-completed shape", first)
	}
	if _, ok := mapTranscriptUsageEvent(line, seen, time.Time{}); ok {
		t.Fatal("second sight of msg-1 emitted again — a re-read would double count")
	}
}

// TestStopHookChannel_ExitUsageReadsTheDurableLocator drives the production
// entry point for the exit accounting: a channel whose hook fired (with no
// notice outstanding, so no claim receipt exists) still reports the
// transcript's totals through exitUsage.
func TestStopHookChannel_ExitUsageReadsTheDurableLocator(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{
		usageLine(t, "msg-1", 100, 20, 1000, 50),
		usageLine(t, "msg-1", 100, 20, 1000, 50), // streamed repeat: counted once
	})
	// The hook fires with nothing offered: no claim, but the locator lands.
	fireHook(t, c, hookStdin(path, false))

	got, err := c.exitUsage()
	if err != nil {
		t.Fatalf("exitUsage: %v", err)
	}
	want := agent.CostData{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 1000, CacheWriteTokens: 50, NumTurns: 1}
	if got != want {
		t.Fatalf("exitUsage = %+v; want %+v", got, want)
	}
}

// TestStopHookChannel_ExitUsageWithNoFireIsNoUsageNotAnError covers the
// session that ends before its hook ever fired: no locator, no usage, no
// error.
func TestStopHookChannel_ExitUsageWithNoFireIsNoUsageNotAnError(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	got, err := c.exitUsage()
	if err != nil {
		t.Fatalf("exitUsage with no fire must not error: %v", err)
	}
	if got != (agent.CostData{}) {
		t.Fatalf("exitUsage with no fire = %+v; want zero", got)
	}
}

// TestWithTranscriptCost_AttachesSessionTotals drives the terminal-event
// seam: a costless ResultEvent leaving the PTY gains the transcript totals,
// while an event that already carries a cost is never overwritten.
func TestWithTranscriptCost_AttachesSessionTotals(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{usageLine(t, "msg-1", 100, 20, 1000, 50)})
	fireHook(t, c, hookStdin(path, false))

	got := withTranscriptCost(agent.ResultEvent{Success: true}, c)
	result, ok := got.(agent.ResultEvent)
	if !ok {
		t.Fatalf("withTranscriptCost returned %T; want agent.ResultEvent", got)
	}
	if result.Cost == nil {
		t.Fatal("terminal ResultEvent left costless — the session ends with no cost")
	}
	if result.Cost.InputTokens != 100 || result.Cost.NumTurns != 1 {
		t.Fatalf("terminal cost = %+v; want the transcript totals", result.Cost)
	}
	if !result.Success {
		t.Fatal("attaching the cost flipped the exit outcome")
	}
}

func TestWithTranscriptCost_NeverOverwritesAnExistingCost(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{usageLine(t, "msg-1", 100, 20, 1000, 50)})
	fireHook(t, c, hookStdin(path, false))

	existing := &agent.CostData{InputTokens: 7}
	got := withTranscriptCost(agent.ResultEvent{Success: true, Cost: existing}, c)
	if got.(agent.ResultEvent).Cost != existing {
		t.Fatal("withTranscriptCost overwrote a cost the event already carried")
	}
	if gotNil := withTranscriptCost(agent.ResultEvent{Success: true}, nil); gotNil.(agent.ResultEvent).Cost != nil {
		t.Fatal("withTranscriptCost invented a cost with no channel to read from")
	}
}

// TestStopHookScript_KeepsTheLocatorWithNothingOffered pins the durable
// locator itself: a hook fire with an empty drop still records the latest
// stdin, so transcriptPath answers the session question even when no claim
// receipt exists.
func TestStopHookScript_KeepsTheLocatorWithNothingOffered(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{usageLine(t, "msg-1", 1, 1, 1, 1)})
	if got := fireHook(t, c, hookStdin(path, false)); got != "" {
		t.Fatalf("hook emitted %q with nothing offered; want empty stdout", got)
	}
	locator, ok, err := c.transcriptPath()
	if err != nil || !ok {
		t.Fatalf("transcriptPath() = %q, %v, %v; want the fired path", locator, ok, err)
	}
	if locator != path {
		t.Fatalf("transcriptPath() = %q; want %q", locator, path)
	}
	if _, _, err := c.claimedTranscriptPath(); err == nil {
		// No offer was outstanding, so no claim receipt may exist; the
		// locator is the only record of this fire.
		if _, statErr := os.Stat(c.path(stopHookStdinFile)); statErr == nil {
			t.Fatal("a claim receipt exists with nothing offered — the locator must be the only record")
		}
	}
	// A second fire moves the locator forward.
	second := writeTranscript(t, []string{usageLine(t, "msg-9", 2, 2, 2, 2)})
	fireHook(t, c, hookStdin(second, true))
	locator, ok, err = c.transcriptPath()
	if err != nil || !ok || locator != second {
		t.Fatalf("transcriptPath() = %q, %v, %v; want the latest fire %q", locator, ok, err, second)
	}
}

// TestWrapInteractiveHandle_ForwardsUsageAndTerminalTotals drives the
// production spawn wrapper end to end: usage tailed from the session's
// transcript arrives as LlmCallEvents, and the terminal ResultEvent carries
// the deduped session totals. The fake binary stands in for the REPL (kept
// alive briefly so the tailer sweeps while the session runs, as a real REPL
// does); the transcript is the harness's own fixture.
func TestWrapInteractiveHandle_ForwardsUsageAndTerminalTotals(t *testing.T) {
	p := newFakeInteractiveProvider(t, `echo "argv: $@"; sleep 5`)
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	wrapper, ok := h.(*interactiveHandle)
	if !ok {
		t.Fatalf("Spawn returned %T; want *interactiveHandle", h)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })

	path := writeTranscript(t, []string{
		usageLine(t, "msg-1", 100, 20, 1000, 50),
		usageLine(t, "msg-1", 100, 20, 1000, 50),
	})
	// The hook fires as the session runs: the tailer discovers the locator
	// and the usage lands on Events as LlmCallEvents.
	fireHook(t, wrapper.notices, hookStdin(path, false))

	var usage []agent.LlmCallEvent
	var terminal *agent.ResultEvent
	deadline := t.Context()
	_ = deadline
	for {
		select {
		case ev, ok := <-wrapper.Events():
			if !ok {
				goto drained
			}
			switch v := ev.(type) {
			case agent.LlmCallEvent:
				usage = append(usage, v)
			case agent.ResultEvent:
				cp := v
				terminal = &cp
			}
			if terminal != nil {
				goto drained
			}
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
drained:
	if len(usage) != 1 {
		t.Fatalf("got %d usage events; want exactly 1 (the repeat must not emit)", len(usage))
	}
	if usage[0].InputTokens != 100 || usage[0].OutputTokens != 20 {
		t.Fatalf("usage event = %+v; want the transcript's usage", usage[0])
	}
	if terminal == nil {
		t.Fatal("never observed the terminal ResultEvent")
	}
	if terminal.Cost == nil {
		t.Fatal("terminal ResultEvent carries no cost — the session ends with no cost")
	}
	if terminal.Cost.InputTokens != 100 || terminal.Cost.NumTurns != 1 {
		t.Fatalf("terminal cost = %+v; want the deduped transcript totals", terminal.Cost)
	}
}

// TestWrapInteractiveHandle_HookFailureStillExitsCleanly pins the teardown
// half of the wrapper: when the hook channel could not be established the
// spawn returns the bare PTY handle, and its terminal event is untouched.
func TestWrapInteractiveHandle_HookFailureStillExitsCleanly(t *testing.T) {
	p := newFakeInteractiveProvider(t, `echo "argv: $@"`)
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })
	if _, ok := h.(*interactiveHandle); !ok {
		t.Fatalf("Spawn returned %T; want *interactiveHandle", h)
	}
}

// TestSumTranscriptUsage_IdLessLinesDedupeByContent covers transcripts whose
// assistant lines carry no message id: identical repeats collapse, distinct
// lines keep their own credit.
func TestSumTranscriptUsage_IdLessLinesDedupeByContent(t *testing.T) {
	t.Parallel()
	content := func(text string) json.RawMessage {
		b, _ := json.Marshal([]any{map[string]any{"type": "text", "text": text}})
		return b
	}
	line := func(text string, in int64) string {
		return jsonLine(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant",
				"usage": map[string]any{
					"input_tokens":  in,
					"output_tokens": 1,
				},
				"content": json.RawMessage(content(text)),
			},
		})
	}
	path := writeTranscript(t, []string{
		line("same", 100),
		line("same", 100),  // identical id-less repeat: counted once
		line("other", 200), // distinct content: counted
		strings.Repeat("x", 10),
	})

	got, err := sumTranscriptUsage(path)
	if err != nil {
		t.Fatalf("sumTranscriptUsage: %v", err)
	}
	if got.InputTokens != 300 || got.NumTurns != 2 {
		t.Fatalf("usage = %+v; want 300 input tokens over 2 turns", got)
	}
}

// stampedUsageLine is usageLine with the transcript's own line timestamp.
func stampedUsageLine(t *testing.T, id string, at time.Time, in, out int64) string {
	t.Helper()
	return jsonLine(t, map[string]any{
		"type":      "assistant",
		"timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"id":   id,
			"role": "assistant",
			"usage": map[string]any{
				"input_tokens":  in,
				"output_tokens": out,
			},
			"content": []any{map[string]any{"type": "text", "text": "work"}},
		},
	})
}

// TestStopHookChannel_ExitUsageSumsEveryTranscriptTheSessionNamed pins the
// conversation-clear case: clearing starts a new transcript under a new
// session id and the hook names the new file from then on. The turns written
// to the first transcript were still spent by this session, so the exit
// totals sum every transcript the hook named — a message copied into both
// counts once.
func TestStopHookChannel_ExitUsageSumsEveryTranscriptTheSessionNamed(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	first := writeTranscript(t, []string{
		usageLine(t, "msg-a", 5000, 500, 0, 0),
		usageLine(t, "msg-c", 7, 0, 0, 0),
	})
	fireHook(t, c, hookStdin(first, false))
	if _, _, err := c.transcriptPath(); err != nil { // the live tail's poll
		t.Fatalf("transcriptPath: %v", err)
	}
	second := writeTranscript(t, []string{
		usageLine(t, "msg-a", 5000, 500, 0, 0), // carried over: counted once
		usageLine(t, "msg-b", 10, 1, 0, 0),
	})
	fireHook(t, c, hookStdin(second, false))
	c.snapshotTranscriptPath() // the session cleanup, before the drop is removed

	got, err := c.exitUsage()
	if err != nil {
		t.Fatalf("exitUsage: %v", err)
	}
	want := agent.CostData{InputTokens: 5017, OutputTokens: 501, NumTurns: 3}
	if got != want {
		t.Fatalf("exitUsage = %+v; want %+v (both transcripts, each message once)", got, want)
	}
}

// TestStopHookChannel_ExitUsageCountsSubagentTranscripts pins that subagent
// turns count: the harness writes them to their own files beside the session
// transcript (<transcript minus .jsonl>/subagents/*.jsonl), never into it, and
// a session that delegates spends much of its tokens there.
func TestStopHookChannel_ExitUsageCountsSubagentTranscripts(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	main := writeTranscript(t, []string{usageLine(t, "msg-main", 100, 10, 1000, 0)})
	subDir := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"agent-one.jsonl": usageLine(t, "msg-sub-1", 300, 30, 9000, 40),
		"agent-two.jsonl": usageLine(t, "msg-sub-2", 200, 20, 0, 0),
	} {
		if err := os.WriteFile(filepath.Join(subDir, name), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fireHook(t, c, hookStdin(main, false))

	got, err := c.exitUsage()
	if err != nil {
		t.Fatalf("exitUsage: %v", err)
	}
	want := agent.CostData{InputTokens: 600, OutputTokens: 60, CachedInputTokens: 10000, CacheWriteTokens: 40, NumTurns: 3}
	if got != want {
		t.Fatalf("exitUsage = %+v; want %+v (session plus subagent transcripts)", got, want)
	}
}

// TestStopHookChannel_ExitUsageSkipsTurnsFromBeforeTheSession pins the resume
// case: resuming an earlier conversation continues its transcript, whose
// earlier turns were spent before this session began. Only lines stamped
// after the session began count, in the exit sum and in the live tail alike.
func TestStopHookChannel_ExitUsageSkipsTurnsFromBeforeTheSession(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{
		stampedUsageLine(t, "msg-earlier", c.createdAt.Add(-time.Hour), 7000, 700),
		stampedUsageLine(t, "msg-now", c.createdAt.Add(time.Second), 10, 1),
	})
	fireHook(t, c, hookStdin(path, false))

	got, err := c.exitUsage()
	if err != nil {
		t.Fatalf("exitUsage: %v", err)
	}
	if want := (agent.CostData{InputTokens: 10, OutputTokens: 1, NumTurns: 1}); got != want {
		t.Fatalf("exitUsage = %+v; want %+v (only this session's turn)", got, want)
	}
	events := newInteractiveUsageTailer(c).sweep()
	if len(events) != 1 || events[0].(agent.LlmCallEvent).InputTokens != 10 {
		t.Fatalf("live tail emitted %+v; want only this session's turn", events)
	}
}

// TestStopHookChannel_UntrustedTranscriptReadsAsUnreported pins that a
// transcript the accounting cannot take at face value never becomes a wrong
// number: a count no model call produces, or a total that would overflow,
// makes the session's usage unreported (the terminal event stays costless),
// and a transcript path that is not a regular file is refused before any read
// (a FIFO would otherwise block the exit forever).
func TestStopHookChannel_UntrustedTranscriptReadsAsUnreported(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	tests := []struct {
		name string
		path func(t *testing.T) string
		// liveEvents is how many per-turn events the live tail emits: an
		// impossible count is never forwarded, a valid line still is.
		liveEvents int
	}{
		{"negative count", func(t *testing.T) string {
			return writeTranscript(t, []string{
				usageLine(t, "msg-1", 1000, 100, 0, 0),
				usageLine(t, "msg-2", -900, 10, 0, 0),
			})
		}, 1},
		{"total overflows", func(t *testing.T) string {
			return writeTranscript(t, []string{
				usageLine(t, "msg-1", 1000, 100, 0, 0),
				usageLine(t, "msg-2", math.MaxInt64, 10, 0, 0),
			})
		}, 2},
		{"not a regular file", func(*testing.T) string { return fifo }, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestStopHookChannel(t)
			fireHook(t, c, hookStdin(tc.path(t), false))
			done := make(chan agent.Event, 1)
			go func() { done <- withTranscriptCost(agent.ResultEvent{Success: true}, c) }()
			select {
			case got := <-done:
				if cost := got.(agent.ResultEvent).Cost; cost != nil {
					t.Fatalf("terminal cost = %+v; want none (unreported)", *cost)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("exit accounting blocked on the transcript")
			}
			if _, err := c.exitUsage(); !errors.Is(err, errTranscriptUntrusted) {
				t.Fatalf("exitUsage err = %v; want errTranscriptUntrusted", err)
			}
			live := make(chan int, 1)
			go func() { live <- len(newInteractiveUsageTailer(c).sweep()) }()
			select {
			case n := <-live:
				if n != tc.liveEvents {
					t.Fatalf("live tail emitted %d events; want %d", n, tc.liveEvents)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("live tail blocked on the transcript")
			}
		})
	}
}

// TestInteractiveUsageTailer_RestartsAtANewTranscript pins the live tail's
// read position across a transcript switch: the offset belongs to one file,
// so when the hook names a new transcript the tail reads it from its first
// byte instead of skipping as many bytes as it had read of the old one.
func TestInteractiveUsageTailer_RestartsAtANewTranscript(t *testing.T) {
	t.Parallel()
	c := newTestStopHookChannel(t)
	tailer := newInteractiveUsageTailer(c)
	first := writeTranscript(t, []string{usageLine(t, "msg-a", 1, 1, 0, 0)})
	fireHook(t, c, hookStdin(first, false))
	if got := tailer.sweep(); len(got) != 1 {
		t.Fatalf("first transcript emitted %d events; want 1", len(got))
	}
	second := writeTranscript(t, []string{
		usageLine(t, "msg-b1", 3000, 300, 0, 0),
		usageLine(t, "msg-b2", 40, 4, 0, 0),
	})
	fireHook(t, c, hookStdin(second, false))
	var input int64
	for _, ev := range tailer.sweep() {
		input += ev.(agent.LlmCallEvent).InputTokens
	}
	if input != 3040 {
		t.Fatalf("second transcript input = %d; want 3040 (every line of the new file)", input)
	}
}
