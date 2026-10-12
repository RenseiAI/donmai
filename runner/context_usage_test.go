package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
)

func writeSidecar(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "sidecar.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeRollout(t *testing.T, dir string, lines []string) (home, path string) {
	t.Helper()
	home = filepath.Join(dir, "codex-home")
	sub := filepath.Join(home, "sessions", "2026", "10", "09")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(sub, "rollout-2026-10-09T00-00-00-thread-test.jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

// appendRollout appends raw bytes to the fixture rollout at path.
func appendRollout(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func drainNotices(w *contextUsageWatcher) []agent.Event {
	return w.poll()
}

// TestContextWatcher_SidecarThresholdCrossings uses a fixture sidecar file:
// each rising bound (50/75/90) reports once, a steady reading reports
// nothing, and falling below re-arms the bound.
func TestContextWatcher_SidecarThresholdCrossings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeSidecar(t, dir, `{"used_percentage": 10}`)
	w := newContextUsageWatcher(contextUsageSource{harness: "claude-code", sidecarPath: path})

	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("10%% yielded %d notices, want none", len(got))
	}
	if err := os.WriteFile(path, []byte(`{"used_percentage": 62.5, "total_input_tokens": 125000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := drainNotices(w)
	if len(got) != 1 {
		t.Fatalf("62.5%% yielded %d notices, want 1 (the 50 bound)", len(got))
	}
	notice, ok := got[0].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("notice type = %T, want ContextUsageEvent", got[0])
	}
	if notice.NoticeKind != agent.ContextUsageThreshold {
		t.Errorf("kind = %q, want threshold", notice.NoticeKind)
	}
	if notice.InputTokens == nil || *notice.InputTokens != 125000 {
		t.Errorf("inputTokens = %v, want 125000", notice.InputTokens)
	}
	if notice.OutputTokens != nil {
		t.Errorf("outputTokens = %v for an input-only sidecar; want nil", notice.OutputTokens)
	}
	// Steady state: the same crossing reports nothing twice.
	if err := os.WriteFile(path, []byte(`{"used_percentage": 63}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 63 is still past 50 only — the 50 bound is disarmed, so nothing fires.
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("steady 63%% yielded %d notices, want none", len(got))
	}
	if err := os.WriteFile(path, []byte(`{"used_percentage": 91}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("91%% yielded %d notices, want 1 (75+90 bounds, one notice)", len(got))
	}
	// Fall back below 50 re-arms it; rising again re-fires.
	if err := os.WriteFile(path, []byte(`{"used_percentage": 20}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("fall to 20%% yielded %d notices, want none (re-arm only)", len(got))
	}
	if err := os.WriteFile(path, []byte(`{"used_percentage": 55}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("re-rise to 55%% yielded %d notices, want 1", len(got))
	}
}

// TestContextWatcher_SidecarCompactionMarker uses a fixture sidecar with a
// compaction mark: it reports once per observed state, not on every poll.
func TestContextWatcher_SidecarCompactionMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeSidecar(t, dir, `{"compacted": true, "used_percentage": 30}`)
	w := newContextUsageWatcher(contextUsageSource{harness: "claude-code", sidecarPath: path})

	got := drainNotices(w)
	if len(got) != 1 {
		t.Fatalf("compaction marker yielded %d notices, want 1", len(got))
	}
	if notice := got[0].(agent.ContextUsageEvent); notice.NoticeKind != agent.ContextUsageCompaction {
		t.Fatalf("kind = %q, want compaction", notice.NoticeKind)
	}
	// Same bytes: unchanged file emits nothing.
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("unchanged sidecar yielded %d notices, want none", len(got))
	}
	// New compaction state (different counts) reports again.
	if err := os.WriteFile(path, []byte(`{"compacted": true, "used_percentage": 31}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("new compaction state yielded %d notices, want 1", len(got))
	}
}

// TestContextWatcher_RolloutTokenCounts uses a fixture rollout file: only
// token_count lines yield notices, each line once, with torn tails held.
func TestContextWatcher_RolloutTokenCounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Fixture lines in the real event_msg/token_count shape: the window
	// share derives as 100*total/window (55, 80 — one bound crossing
	// each), beside unrelated line shapes the watcher skips.
	home, path := writeRollout(t, dir, []string{
		`{"type":"session_meta","payload":{"id":"thread-1"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":110000,"output_tokens":0,"total_tokens":110000},"model_context_window":200000}}}`,
		`{"type":"event_msg","payload":{"message":"done"}}`,
		`{"type":"token_usage_record","payload":{"thread_id":"thread-1"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":160000,"output_tokens":0,"total_tokens":160000},"model_context_window":200000}}}`,
	})
	// The source carries the session home + thread — never a planted file
	// path — so this test proves the production resolve→poll→notice loop.
	w := newContextUsageWatcher(contextUsageSource{harness: "codex", rolloutHome: home, rolloutThread: "thread-test"})

	got := drainNotices(w)
	if len(got) != 2 {
		t.Fatalf("fixture rollout yielded %d notices, want 2", len(got))
	}
	for _, ev := range got {
		notice, ok := ev.(agent.ContextUsageEvent)
		if !ok {
			t.Fatalf("notice type = %T, want ContextUsageEvent", ev)
		}
		if notice.Source != agent.ContextUsageSourceRolloutTokenCount {
			t.Errorf("source = %q, want rollout-token-count", notice.Source)
		}
		if notice.OutputTokens != nil {
			t.Errorf("outputTokens = %v for an input-only line; want nil (never zero)", notice.OutputTokens)
		}
	}
	// Fully read: nothing more.
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("drained rollout yielded %d notices, want none", len(got))
	}
	// A torn trailing line stays unread until completed.
	appendRollout(t, path, `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens": 99`)
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("torn tail yielded %d notices, want none", len(got))
	}
	appendRollout(t, path, `000, "output_tokens":0, "total_tokens":199000},"model_context_window":200000}}}`+"\n")
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("completed tail yielded %d notices, want 1", len(got))
	}
}

// TestContextWatcher_RolloutPathReResolves pins the late-file posture: a
// session whose rollout file does not exist yet emits nothing, then tails
// it once the harness creates it — the path is re-resolved every poll,
// never once at start.
func TestContextWatcher_RolloutPathReResolves(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "codex-home")
	w := newContextUsageWatcher(contextUsageSource{harness: "codex", rolloutHome: home, rolloutThread: "thread-late"})
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("absent rollout yielded %d notices, want none", len(got))
	}
	sub := filepath.Join(home, "sessions", "2026", "10", "09")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(sub, "rollout-2026-10-09T00-00-00-thread-late.jsonl")
	body := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":110000,"output_tokens":0,"total_tokens":110000},"model_context_window":200000}}}` + "\n"
	if err := os.WriteFile(late, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("late rollout yielded %d notices, want 1", len(got))
	}
}

// TestContextWatcher_RolloutNewestFileWins pins the unnamed-session
// posture: with a home but no thread, the watcher tails the home's newest
// rollout — the home is session-private, so the newest file is this
// session's.
func TestContextWatcher_RolloutNewestFileWins(t *testing.T) {
	t.Parallel()
	home, _ := writeRollout(t, t.TempDir(), []string{
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":110000,"output_tokens":0,"total_tokens":110000},"model_context_window":200000}}}`,
	})
	w := newContextUsageWatcher(contextUsageSource{harness: "codex", rolloutHome: home})
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("threadless rollout yielded %d notices, want 1", len(got))
	}
}

// TestContextWatcher_RolloutCompactionMarker pins the compaction leg: a
// top-level compacted line in the real rollout shape emits one compaction
// notice, exactly as a live session file carries it.
func TestContextWatcher_RolloutCompactionMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	home, _ := writeRollout(t, dir, []string{
		`{"type":"compacted","payload":{"message":""}}`,
	})
	w := newContextUsageWatcher(contextUsageSource{harness: "codex", rolloutHome: home, rolloutThread: "thread-test"})
	got := drainNotices(w)
	if len(got) != 1 {
		t.Fatalf("compacted marker yielded %d notices, want 1", len(got))
	}
	if notice := got[0].(agent.ContextUsageEvent); notice.NoticeKind != agent.ContextUsageCompaction {
		t.Fatalf("kind = %q, want compaction", notice.NoticeKind)
	}
	// Fully read: the marker reports once.
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("drained rollout yielded %d notices, want none", len(got))
	}
}

// TestContextWatcher_MissingFilesAreSilent pins the best-effort posture: a
// session with no sidecar or rollout yet emits nothing, never an error.
func TestContextWatcher_MissingFilesAreSilent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := newContextUsageWatcher(contextUsageSource{
		harness:     "claude-code",
		sidecarPath: filepath.Join(dir, "nope.json"),
		rolloutHome: filepath.Join(dir, "nope-home"),
	})
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("missing files yielded %d notices, want none", len(got))
	}
}

// TestObservePiRPCUsage_ThresholdGating pins the pi leg: the first crossing
// reports, a repeat does not, and an unknown-only object stays silent.
func TestObservePiRPCUsage_ThresholdGating(t *testing.T) {
	t.Parallel()
	in, out, window, pct := int64(60000), int64(3000), int64(1000000), 82.0
	state := newContextUsageWatcher(contextUsageSource{harness: "pi"})
	ev := ObservePiRPCUsage(&agent.PiRPCUsage{
		InputTokens: &in, OutputTokens: &out,
		ContextWindowSize: &window, UsedPercentage: &pct,
	}, "pi", state)
	if ev == nil {
		t.Fatal("no notice for the first 82% reading")
	}
	notice, ok := (*ev).(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("notice type = %T, want ContextUsageEvent", *ev)
	}
	if notice.Source != agent.ContextUsageSourceRPCUsage {
		t.Errorf("source = %q, want rpc-usage", notice.Source)
	}
	if notice.InputTokens == nil || *notice.InputTokens != 60000 {
		t.Errorf("inputTokens = %v, want 60000", notice.InputTokens)
	}
	// Same reading again: disarmed bounds stay silent.
	if ev := ObservePiRPCUsage(&agent.PiRPCUsage{UsedPercentage: &pct}, "pi", state); ev != nil {
		t.Fatalf("repeat reading yielded %+v, want nil", *ev)
	}
	// Nil usage carries no signal.
	if ev := ObservePiRPCUsage(nil, "pi", state); ev != nil {
		t.Fatalf("nil usage yielded %+v, want nil", *ev)
	}
}

// TestContextWatcher_RunStopsOnCancel pins the goroutine lifecycle: run
// returns when the context ends whether or not anyone reads the events.
func TestContextWatcher_RunStopsOnCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeSidecar(t, dir, `{"used_percentage": 60}`)
	w := newContextUsageWatcher(contextUsageSource{harness: "claude-code", sidecarPath: path})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx, func(agent.Event) {})
	}()
	select {
	case <-done:
		t.Fatal("run returned before cancel")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

// blockingContextSink parks the first Send until released, standing in for
// a notice that is mid-delivery when the session tears down.
type blockingContextSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingContextSink) Send(context.Context, agent.Event) {
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.entered)
		<-s.release
	}
}

// TestContextUsageWatchStopJoinsInFlightEmit pins the teardown contract:
// the stop func the interactive dispatch defers must not return while a
// notice is still in flight to the sink. The dispatch's caller stops the
// activity poster (closing its queue) and closes the execution-event
// journal right after the dispatch returns, so an emit that outlives stop
// would race the queue close or append after the terminal record.
func TestContextUsageWatchStopJoinsInFlightEmit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeSidecar(t, dir, `{"used_percentage": 60}`)
	w := newContextUsageWatcher(contextUsageSource{harness: "claude-code", sidecarPath: path})
	sink := &blockingContextSink{entered: make(chan struct{}), release: make(chan struct{})}
	r := &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	stop := r.runContextUsageWatch(context.Background(), w, dir, sink)
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		close(sink.release)
		stop()
		t.Fatal("watcher never emitted the 60% crossing")
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		close(sink.release)
		t.Fatal("stop returned while a notice was still in flight to the sink")
	case <-time.After(100 * time.Millisecond):
	}
	close(sink.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the in-flight emit finished")
	}
}

// contextUsageTestHandle is a production-seam test double: it implements
// agent.ContextUsageSourcer the way the claude interactive handle and the
// codex interactive wrapper do — session-scoped paths, no environment.
type contextUsageTestHandle struct {
	agent.Handle
	sidecar string
	home    string
	thread  string
}

func (h *contextUsageTestHandle) ContextUsageSidecarPath() string { return h.sidecar }
func (h *contextUsageTestHandle) ContextUsageRollout() (string, string) {
	return h.home, h.thread
}

// TestResolveContextUsageSource_ReadsTheHandleNotTheProcess pins the gate's
// core finding: the watcher resolves its paths from the spawned handle's
// own session-scoped seam. A process-wide variable can never fan one
// session's readings out to another session, because no variable is read.
func TestResolveContextUsageSource_ReadsTheHandleNotTheProcess(t *testing.T) {
	// No t.Parallel: this test asserts process-wide variables are never
	// consulted, so it sets them to decoy values.
	t.Setenv("CLAUDE_STATUSLINE_SIDECAR", "/tmp/process-wide-sidecar.json")
	t.Setenv("CODEX_HOME", "/tmp/process-wide-home")
	t.Setenv("CODEX_THREAD_ID", "thread-process-wide")

	// A handle with no seam reports no source even with the process
	// variables set: there is deliberately no environment fallback.
	if got := resolveContextUsageSource(nil); got.harness != "" {
		t.Fatalf("nil handle resolved %+v; want empty", got)
	}
	// The claude leg: the handle's own sidecar path wins; the process
	// variable is never consulted.
	claude := &contextUsageTestHandle{sidecar: "/tmp/session-a/sidecar.json"}
	got := resolveContextUsageSource(claude)
	if got.harness != string(agent.HarnessClaudeCode) || got.sidecarPath != "/tmp/session-a/sidecar.json" {
		t.Fatalf("claude resolved %+v; want the handle's sidecar", got)
	}
	// The codex leg: the handle's own home + thread.
	codex := &contextUsageTestHandle{home: "/tmp/session-b/home", thread: "thread-b"}
	got = resolveContextUsageSource(codex)
	if got.harness != string(agent.HarnessCodex) || got.rolloutHome != "/tmp/session-b/home" || got.rolloutThread != "thread-b" {
		t.Fatalf("codex resolved %+v; want the handle's home+thread", got)
	}
}

// TestContextWatcher_EndToEndHandleToSeries pins the whole claude leg: a
// sidecar file at the handle's own path (as the session's statusLine
// command maintains it) polls into a typed notice that normalizes to the
// usage.recorded series record with unknown measurements absent, never
// zero. It drives the production resolve→poll→normalize chain, not a
// helper.
func TestContextWatcher_EndToEndHandleToSeries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sidecar := writeSidecar(t, dir, `{"used_percentage": 62.5, "total_input_tokens": 125000}`)
	handle := &contextUsageTestHandle{sidecar: sidecar}
	source := resolveContextUsageSource(handle)
	if source.harness == "" {
		t.Fatal("no source for a handle with a sidecar")
	}
	w := newContextUsageWatcher(source)
	notices := drainNotices(w)
	if len(notices) != 1 {
		t.Fatalf("sidecar yielded %d notices, want 1", len(notices))
	}
	notice, ok := notices[0].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("notice type = %T, want ContextUsageEvent", notices[0])
	}
	record, emitted, err := executionevent.NormalizeEvent("session_e2e", 7, time.Now(), notice)
	if err != nil {
		t.Fatalf("NormalizeEvent: %v", err)
	}
	if !emitted {
		t.Fatal("notice did not emit a series record")
	}
	raw, _ := json.Marshal(record)
	var wire struct {
		EventType string         `json:"eventType"`
		Payload   map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode series record: %v", err)
	}
	if wire.EventType != "usage.recorded" {
		t.Fatalf("eventType = %q, want usage.recorded (in %s)", wire.EventType, raw)
	}
	payload := wire.Payload
	if payload["usedPercent"] != 62.5 {
		t.Errorf("usedPercent = %v, want 62.5", payload["usedPercent"])
	}
	for _, absent := range []string{"outputTokens", "contextWindowSize"} {
		if _, present := payload[absent]; present {
			t.Errorf("series stores %q for an unknown measurement; want absent (never zero)", absent)
		}
	}
}
