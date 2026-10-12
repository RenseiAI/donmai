package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func writeSidecar(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "sidecar.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeRollout(t *testing.T, dir string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "rollout.jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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
	path := writeRollout(t, dir, []string{
		`{"type":"session_meta","payload":{"id":"thread-1"}}`,
		`{"type":"token_count","payload":{"total_input_tokens": 60000, "used_percentage": 55}}`,
		`{"type":"event_msg","payload":{"message":"done"}}`,
		`{"type":"token_count","payload":{"total_input_tokens": 95000, "used_percentage": 80}}`,
	})
	w := newContextUsageWatcher(contextUsageSource{harness: "codex", rolloutPath: path})

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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"token_count","payload":{"total_input_tokens": 99`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := drainNotices(w); len(got) != 0 {
		t.Fatalf("torn tail yielded %d notices, want none", len(got))
	}
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`000, "used_percentage": 95}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := drainNotices(w); len(got) != 1 {
		t.Fatalf("completed tail yielded %d notices, want 1", len(got))
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
		rolloutPath: filepath.Join(dir, "nope.jsonl"),
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
