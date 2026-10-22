package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/runtime/state"
)

// sinkSummary renders the recorded sink stream as kind[:detail] tokens: the
// SystemEvent subtype or the ToolUseEvent tool name, so order assertions can
// name the events they care about.
func sinkSummary(sink *recordingSink) []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	out := make([]string, 0, len(sink.events))
	for _, ev := range sink.events {
		switch e := ev.(type) {
		case agent.SystemEvent:
			out = append(out, string(e.Kind())+":"+e.Subtype)
		case agent.ToolUseEvent:
			out = append(out, string(e.Kind())+":"+e.ToolName)
		default:
			out = append(out, string(ev.Kind()))
		}
	}
	return out
}

// TestInteractive_ConversationMarkersReachSink drives the real
// dispatchInteractive supervisor with a handle whose event channel carries
// the live-conversation markers the harness tails (a provider user turn
// and turn completion), and asserts they reach the activity sink — in
// order, before the session-ended marker. Other system chatter stays
// runner-internal and never reaches the sink.
func TestInteractive_ConversationMarkersReachSink(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	worktreePath := t.TempDir()
	session := completedRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	base.events <- agent.SystemEvent{Subtype: "user_message", Message: "keep going"}
	base.events <- agent.AssistantTextEvent{Text: "on it"}
	base.events <- agent.SystemEvent{Subtype: "turn_completed", Message: "done"}
	base.events <- agent.SystemEvent{Subtype: "turn_started", Message: "Turn 1 started"}
	handle := &testInteractiveHandle{Handle: base, session: session}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "conversation-markers", Mode: interactiveRunMode}}
	r := minimalRunner(t)

	result, err := r.dispatchInteractive(
		context.Background(), handle, worktreePath, qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
	)
	if err != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, err)
	}

	want := []string{
		string(agent.EventSystem) + ":interactive-session-started",
		string(agent.EventSystem) + ":user_message",
		string(agent.EventAssistantText),
		string(agent.EventSystem) + ":turn_completed",
		string(agent.EventSystem) + ":interactive-session-ended",
	}
	if got := sinkSummary(sink); !slices.Equal(got, want) {
		t.Fatalf("sink stream = %v, want %v", got, want)
	}
}

// TestInteractive_TranscriptEventsReachSink drives the real
// dispatchInteractive supervisor with a handle whose event channel carries
// transcript-mapped agent events, and asserts they reach the activity sink —
// in order, before the session-ended marker — even though the PTY session is
// already Done when the supervisor first looks: Done must not win over
// activity the handle had already delivered.
func TestInteractive_TranscriptEventsReachSink(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	worktreePath := t.TempDir()
	session := completedRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 8)}
	base.events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1", Input: map[string]any{"command": "echo hi"}}
	base.events <- agent.AssistantTextEvent{Text: "running it"}
	base.events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: "hi\n"}
	base.events <- agent.LlmCallEvent{InputTokens: 10, OutputTokens: 20, UsageSource: agent.LlmUsageProvider, TurnCompleted: true}
	handle := &testInteractiveHandle{Handle: base, session: session}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-sink", Mode: interactiveRunMode}}
	r := minimalRunner(t)

	result, err := r.dispatchInteractive(
		context.Background(), handle, worktreePath, qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
	)
	if err != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, err)
	}

	want := []string{
		string(agent.EventSystem) + ":interactive-session-started",
		string(agent.EventToolUse) + ":bash",
		string(agent.EventAssistantText),
		string(agent.EventToolResult),
		string(agent.EventLlmCall),
		string(agent.EventSystem) + ":interactive-session-ended",
	}
	if got := sinkSummary(sink); !slices.Equal(got, want) {
		t.Fatalf("sink stream = %v, want %v", got, want)
	}

	// Transcript events are also mirrored to events.jsonl for audit parity.
	body, err := os.ReadFile(filepath.Join(worktreePath, ".agent", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"toolName":"bash"`, `"text":"running it"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("events.jsonl missing %s:\n%s", want, body)
		}
	}
}

// TestInteractive_StepAdvancesRunningThenStopped pins the interactive lane's
// CurrentStep lifecycle: the runner writes "spawning" at state init and the
// headless lane advances it on its first provider event, but the interactive
// lane consumes no event stream — so dispatchInteractive must advance it to
// "running" once the PTY surface is live and to "stopped" when the session
// ends. Without these transitions the local dashboard renders "spawning"
// for the whole session.
func TestInteractive_StepAdvancesRunningThenStopped(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	r := minimalRunner(t)
	worktreePath := t.TempDir()
	if _, err := r.store.Update(worktreePath, func(s *state.State) error {
		s.CurrentStep = "spawning"
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	readStep := func() string {
		t.Helper()
		st, err := r.store.Read(worktreePath)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		return st.CurrentStep
	}

	session := liveRecordingInteractiveSession()
	base := &fakeHandle{events: make(chan agent.Event, 1)}
	handle := &testInteractiveHandle{Handle: base, session: session}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "interactive-step"}}
	res := &Result{SessionID: qw.SessionID}

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := r.dispatchInteractive(
			context.Background(), handle, worktreePath, qw, res, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
		)
		done <- outcome{result: out, err: err}
	}()

	// The PTY surface is live once the session-started marker lands;
	// the step must already read "running" by then.
	deadline := time.After(5 * time.Second)
	for {
		if got := readStep(); got == "running" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("CurrentStep=%q while the session runs, want running", readStep())
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(session.done)
	select {
	case got := <-done:
		if got.err != nil || got.result.Status != "completed" {
			t.Fatalf("dispatchInteractive = status %q err %v, want completed", got.result.Status, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatchInteractive did not return after the session exited")
	}
	if got := readStep(); got != "stopped" {
		t.Fatalf("CurrentStep=%q after the session ended, want stopped", got)
	}
}

// TestInteractive_EarlyFailureMarksStopped pins the terminal step for
// interactive sessions that fail before the PTY surface is live: an oversize
// seed, a non-interactive handle, or a half-configured relay attach all
// return before the running transition, so without an explicit terminal
// step the local dashboard would render "spawning" for the whole session.
// Each failure must leave CurrentStep at "stopped" and record the
// session-ended lifecycle notice, mirroring the clean-exit terminal path.
func TestInteractive_EarlyFailureMarksStopped(t *testing.T) {
	tests := []struct {
		name            string
		wantFailureMode string
		attachURL       string
		attachToken     string
		oversizePrompt  bool
		nonInteractive  bool
	}{
		{name: "oversize prompt", wantFailureMode: FailureInteractiveInput, oversizePrompt: true},
		{name: "non-interactive handle", wantFailureMode: FailureInteractiveUnsupported, nonInteractive: true},
		{name: "half-configured attach", wantFailureMode: FailureInteractiveConfig, attachURL: "ws://127.0.0.1:0/v1/rooms/room-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envAttachURL, tt.attachURL)
			t.Setenv(envAttachToken, tt.attachToken)

			r := minimalRunner(t)
			worktreePath := t.TempDir()
			if _, err := r.store.Update(worktreePath, func(s *state.State) error {
				s.CurrentStep = "spawning"
				return nil
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}

			var handle agent.Handle
			if tt.nonInteractive {
				handle = &fakeHandle{events: make(chan agent.Event)}
			} else {
				handle = &testInteractiveHandle{
					Handle:  &fakeHandle{events: make(chan agent.Event)},
					session: completedRecordingInteractiveSession(),
				}
			}
			sink := &recordingSink{}
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "interactive-early-" + tt.name, Mode: interactiveRunMode}}
			qw.SessionID = "interactive-early-stopped"
			if tt.oversizePrompt {
				qw.InitialPrompt = strings.Repeat("oversized-seed-", 1500)
			}
			res := &Result{SessionID: qw.SessionID}
			res.ProviderName = agent.ProviderStub

			out, err := r.dispatchInteractive(
				context.Background(), handle, worktreePath, qw, res, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
			)
			if err == nil {
				t.Fatal("expected a terminal error for the early failure")
			}
			if out.Status != "failed" || out.FailureMode != tt.wantFailureMode {
				t.Fatalf("status=%q mode=%q; want failed/%s", out.Status, out.FailureMode, tt.wantFailureMode)
			}
			if st, err := r.store.Read(worktreePath); err != nil {
				t.Fatalf("read state: %v", err)
			} else if st.CurrentStep != "stopped" {
				t.Fatalf("CurrentStep=%q after the early failure, want stopped", st.CurrentStep)
			}
			var sawEnded, sawStarted bool
			for _, ev := range sink.events {
				if system, ok := ev.(agent.SystemEvent); ok {
					switch system.Subtype {
					case "interactive-session-started":
						sawStarted = true
					case "interactive-session-ended":
						sawEnded = true
					}
				}
			}
			if !sawEnded {
				t.Fatal("early failure posted no interactive-session-ended activity")
			}
			if sawStarted {
				t.Fatal("early failure must not post interactive-session-started: the PTY never went live")
			}
			body, err := os.ReadFile(filepath.Join(worktreePath, ".agent", "events.jsonl"))
			if err != nil {
				t.Fatalf("read events.jsonl: %v", err)
			}
			if !strings.Contains(string(body), `"subtype":"interactive-session-ended"`) {
				t.Fatalf("events.jsonl missing the session-ended notice:\n%s", body)
			}
		})
	}
}

// flushingInteractiveHandle is a PTY handle that enqueues activity after its
// session is Done and signals agent.InteractiveActivityFlusher when done.
type flushingInteractiveHandle struct {
	testInteractiveHandle
	flushed chan struct{}
}

func (h *flushingInteractiveHandle) ActivityFlushed() <-chan struct{} { return h.flushed }

// TestInteractive_DrainsTrailingActivityAfterDone pins the supervisor side of
// the flush contract: activity a flushing handle enqueues only AFTER the PTY
// session is Done still reaches the sink, ahead of the session-ended marker.
func TestInteractive_DrainsTrailingActivityAfterDone(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	base := &fakeHandle{events: make(chan agent.Event, 8)}
	handle := &flushingInteractiveHandle{
		testInteractiveHandle: testInteractiveHandle{Handle: base, session: completedRecordingInteractiveSession()},
		flushed:               make(chan struct{}),
	}
	go func() {
		// Long after the supervisor has observed the already-closed Done.
		time.Sleep(100 * time.Millisecond)
		base.events <- agent.ToolUseEvent{ToolName: "TRAILING_TOOL", ToolUseID: "call-9"}
		base.events <- agent.ToolResultEvent{ToolName: "TRAILING_TOOL", ToolUseID: "call-9", Content: "done"}
		close(handle.flushed)
	}()
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "transcript-flush", Mode: interactiveRunMode}}
	result, err := minimalRunner(t).dispatchInteractive(
		context.Background(), handle, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
	)
	if err != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, err)
	}
	want := []string{
		string(agent.EventSystem) + ":interactive-session-started",
		string(agent.EventToolUse) + ":TRAILING_TOOL",
		string(agent.EventToolResult),
		string(agent.EventSystem) + ":interactive-session-ended",
	}
	if got := sinkSummary(sink); !slices.Equal(got, want) {
		t.Fatalf("sink stream = %v, want %v", got, want)
	}
}

// TestInteractive_PiTranscriptFinalFlushReachesSink is the end-to-end proof
// through the production pi Spawn wiring: a fake pi under a real PTY writes
// its last transcript line just before exiting, so only the handle's final
// flush (enqueued after Done) carries it. The supervisor must deliver it to
// the sink before the session-ended marker. An earlier session's transcript
// left in the shared state dir must not appear at all.
func TestInteractive_PiTranscriptFinalFlushReachesSink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	bin := filepath.Join(t.TempDir(), "fake-pi")
	script := `#!/bin/bash
dir=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--session-dir" ]; then dir="$2"; shift 2; continue; fi
  shift
done
sleep 0.3
printf '{"type":"message","id":"final","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-final","name":"FINAL_TOOL","arguments":{}}]}}\n' >> "$dir/2099-01-01T00-00-00-000Z_live.jsonl"
`
	if err := testexec.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture needs the exec bit
		t.Fatal(err)
	}
	provider, err := pi.New(pi.Options{
		PiBin:        bin,
		VersionProbe: func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("pi.New: %v", err)
	}

	workdir := t.TempDir()
	stateDir := filepath.Join(workdir, ".pi")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	earlier := `{"type":"message","id":"old","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-old","name":"OLD_SESSION_TOOL","arguments":{}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "2026-01-01T00-00-00-000Z_old.jsonl"), []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}

	handle, err := provider.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = handle.Stop(context.Background()) })

	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{SessionID: "pi-transcript-e2e", Mode: interactiveRunMode}}
	result, err := minimalRunner(t).dispatchInteractive(
		context.Background(), handle, workdir, qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, provider.Manifest().Caps.NoticeDelivery,
	)
	if err != nil || result.Status != "completed" {
		t.Fatalf("dispatchInteractive = status %q err %v, want completed", result.Status, err)
	}
	want := []string{
		string(agent.EventSystem) + ":interactive-session-started",
		string(agent.EventToolUse) + ":FINAL_TOOL",
		string(agent.EventSystem) + ":interactive-session-ended",
	}
	if got := sinkSummary(sink); !slices.Equal(got, want) {
		t.Fatalf("sink stream = %v, want %v", got, want)
	}
}

// TestInteractive_HarnessLifecycleReachesActivitySink pins that a
// harness-lifecycle event off a seat's handle is forwarded to the
// activity sink (and mirrored to events.jsonl) like the other coarse
// lifecycle signals, while an unknown SystemEvent subtype stays
// runner-internal.
func TestInteractive_HarnessLifecycleReachesActivitySink(t *testing.T) {
	r := minimalRunner(t)
	sink := &recordingSink{}
	dir := t.TempDir()
	ctx := context.Background()
	r.forwardInteractiveHandleEvent(ctx, dir, sink, agent.HarnessLifecycleEvent(agent.HarnessLifecycleStop), nil)
	r.forwardInteractiveHandleEvent(ctx, dir, sink, agent.SystemEvent{Subtype: "some-future-subtype", Message: "x"}, nil)
	if len(sink.events) != 1 {
		t.Fatalf("sink got %d events, want exactly the lifecycle one", len(sink.events))
	}
	system, ok := sink.events[0].(agent.SystemEvent)
	if !ok || system.Subtype != agent.SystemSubtypeHarnessLifecycle || system.Message != agent.HarnessLifecycleStop {
		t.Fatalf("sink event = %#v, want harness_lifecycle/stop", sink.events[0])
	}
}

// TestInteractive_TranscriptActivityForwardsContentVerbatim pins that the
// interactive lane does not pre-cut transcript content: a byte cut here split
// multi-byte runes and dropped the tail of tool output before the activity
// poster's own rune-safe head+tail cap ever saw it. Content rides verbatim,
// exactly as on the headless lane.
func TestInteractive_TranscriptActivityForwardsContentVerbatim(t *testing.T) {
	r := minimalRunner(t)
	sink := &recordingSink{}
	dir := t.TempDir()
	ctx := context.Background()
	text := strings.Repeat("é", 3000) + " end-of-thought"
	output := strings.Repeat("日本", 2000) + "\nhttps://example.invalid/pull/1"
	r.forwardInteractiveHandleEvent(ctx, dir, sink, agent.AssistantTextEvent{Text: text}, nil)
	r.forwardInteractiveHandleEvent(ctx, dir, sink, agent.ToolResultEvent{ToolName: "bash", Content: output}, nil)
	if len(sink.events) != 2 {
		t.Fatalf("sink got %d events, want 2", len(sink.events))
	}
	if got := sink.events[0].(agent.AssistantTextEvent).Text; got != text || !utf8.ValidString(got) {
		t.Errorf("assistant text altered: %d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
	}
	if got := sink.events[1].(agent.ToolResultEvent).Content; got != output || !utf8.ValidString(got) {
		t.Errorf("tool output altered: %d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
	}
}
