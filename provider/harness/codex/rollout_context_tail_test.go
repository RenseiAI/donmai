package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/executionevent"
)

// tokenCountLine renders one token_count event in the real event_msg shape:
// cumulative totals beside the model window, exactly as live rollout files
// carry it.
func tokenCountLine(in, out, total, window int64) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{` +
		`"input_tokens":` + itoa64(in) + `,"output_tokens":` + itoa64(out) + `,"total_tokens":` + itoa64(total) +
		`},"last_token_usage":{"total_tokens":` + itoa64(total) + `},"model_context_window":` + itoa64(window) + `}}}`
}

func writeTailRollout(t *testing.T, home, thread string, lines []string) {
	t.Helper()
	dir := filepath.Join(home, codexSessionStateSubdir, "2026", "10", "09")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-09T00-00-00-"+thread+".jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHandle_StartLaunchesContextTail pins the headless wiring: start()
// launches the rollout context-notice tail against the fixture home and
// the thread start() just resolved. It drives the production start()
// entry point against the fake app-server, then reads the notice off the
// handle's own Events channel — so deleting the tail wiring in start()
// leaves the notice undelivered and the test RED. A handle with no
// provider home runs without notices.
func TestHandle_StartLaunchesContextTail(t *testing.T) {
	p, _ := newTestProvider(t)
	home := p.config.home
	thread := "thread-context-tail-start"
	// 110000/200000 = 55%: fires the 50 bound on the first poll. The
	// rollout file must exist BEFORE start() so the tail's first tick
	// (one second out) is not the only read: the test also seeds one
	// token_count line after start and waits for the live tail to deliver
	// it on the handle's own Events channel.
	writeTailRollout(t, home, thread, []string{
		`{"type":"session_meta","payload":{"id":"` + thread + `"}}`,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	ch, ok := h.(*Handle)
	if !ok {
		t.Fatalf("Spawn returned %T, want *Handle", h)
	}
	sessionThread := ch.SessionID()
	if sessionThread == "" {
		t.Fatal("Spawn resolved no thread id; the tail has no session file to read")
	}
	// 160000/200000 = 80%: fires the 75 bound. Written after start so
	// only a live tail launched by start() can deliver it.
	writeTailRollout(t, home, sessionThread, []string{
		tokenCountLine(160000, 5000, 165000, 200000),
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events closed before the tail delivered the fixture notice")
			}
			notice, ok := ev.(agent.ContextUsageEvent)
			if !ok {
				continue
			}
			if notice.Source != agent.ContextUsageSourceRolloutTokenCount {
				t.Fatalf("source = %q, want rollout-token-count", notice.Source)
			}
			if notice.UsedPercent == nil || *notice.UsedPercent != 82.5 {
				t.Fatalf("usedPercent = %v, want 82.5 (100*165000/200000)", notice.UsedPercent)
			}
			// A handle with no provider home runs without notices.
			bare := newHandle(nil, nil, agent.Spec{Prompt: "x", Cwd: t.TempDir()}, HandleOptions{})
			if tail := bare.startContextTail(context.Background()); tail != nil {
				t.Fatalf("startContextTail on a provider-less handle = %+v; want nil (no notices)", tail)
			}
			return
		case <-time.After(time.Until(deadline)):
			t.Fatal("timed out waiting for the headless tail to deliver the fixture notice on the handle's Events channel")
		default:
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for the headless tail to deliver the fixture notice on the handle's Events channel")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestRolloutContextTail_ThresholdCrossingsAreRealNotices pins the codex
// headless leg end to end: a session file in the real rollout shape tails
// into typed threshold notices whose window share derives from the
// reported totals, each bound firing once and re-arming below.
func TestRolloutContextTail_ThresholdCrossingsAreRealNotices(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	thread := "thread-context-tail"
	// 110000/200000 = 55% (fires 50), then 160000/200000 = 80% (fires 75).
	writeTailRollout(t, home, thread, []string{
		`{"type":"session_meta","payload":{"id":"` + thread + `"}}`,
		tokenCountLine(110000, 0, 110000, 200000),
		`{"type":"event_msg","payload":{"message":"done"}}`,
		tokenCountLine(160000, 0, 160000, 200000),
	})
	tail := newRolloutContextTail(home, thread)
	got := tail.poll()
	if len(got) != 2 {
		t.Fatalf("fixture rollout yielded %d notices, want 2", len(got))
	}
	first, ok := got[0].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("notice type = %T, want ContextUsageEvent", got[0])
	}
	if first.Source != agent.ContextUsageSourceRolloutTokenCount {
		t.Errorf("source = %q, want rollout-token-count", first.Source)
	}
	if first.Harness != string(agent.HarnessCodex) {
		t.Errorf("harness = %q, want codex", first.Harness)
	}
	if first.UsedPercent == nil || *first.UsedPercent != 55 {
		t.Errorf("usedPercent = %v, want 55", first.UsedPercent)
	}
	if first.ContextWindowSize == nil || *first.ContextWindowSize != 200000 {
		t.Errorf("contextWindowSize = %v, want 200000", first.ContextWindowSize)
	}
	if first.OutputTokens != nil {
		t.Errorf("outputTokens = %v for a zero-output total; want nil (never zero)", first.OutputTokens)
	}
	// Fully read: nothing more.
	if got := tail.poll(); len(got) != 0 {
		t.Fatalf("drained rollout yielded %d notices, want none", len(got))
	}
}

// TestRolloutContextTail_CompactedMarkerEmits pins the compaction leg: a
// top-level compacted line in the real rollout shape emits one compaction
// notice, once.
func TestRolloutContextTail_CompactedMarkerEmits(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	thread := "thread-context-compacted"
	writeTailRollout(t, home, thread, []string{
		`{"type":"compacted","payload":{"message":""}}`,
	})
	tail := newRolloutContextTail(home, thread)
	got := tail.poll()
	if len(got) != 1 {
		t.Fatalf("compacted marker yielded %d notices, want 1", len(got))
	}
	if notice := got[0].(agent.ContextUsageEvent); notice.NoticeKind != agent.ContextUsageCompaction {
		t.Fatalf("kind = %q, want compaction", notice.NoticeKind)
	}
	if got := tail.poll(); len(got) != 0 {
		t.Fatalf("drained rollout yielded %d notices, want none", len(got))
	}
}

// TestRolloutContextTail_MissingFileIsSilent pins the best-effort posture:
// the file appears after thread start (the rollout-flush race), so an
// absent file retries next tick rather than erroring.
func TestRolloutContextTail_MissingFileIsSilent(t *testing.T) {
	t.Parallel()
	tail := newRolloutContextTail(t.TempDir(), "thread-absent")
	if got := tail.poll(); len(got) != 0 {
		t.Fatalf("absent rollout yielded %d notices, want none", len(got))
	}
	// Unknown home or thread never tails.
	empty := newRolloutContextTail("", "")
	if got := empty.poll(); len(got) != 0 {
		t.Fatalf("empty tail yielded %d notices, want none", len(got))
	}
}

// TestRolloutContextTail_RunStopsOnCancel pins the goroutine lifecycle.
func TestRolloutContextTail_RunStopsOnCancel(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	thread := "thread-context-cancel"
	writeTailRollout(t, home, thread, []string{tokenCountLine(110000, 0, 110000, 200000)})
	tail := newRolloutContextTail(home, thread)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tail.run(ctx, func(agent.Event) {})
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

// TestRolloutContextTail_EndToEndTailToSeries pins the whole codex
// headless leg: a session file in the real rollout shape tails into a
// typed notice that normalizes to the usage.recorded series record. It
// drives the production tail→poll→normalize chain, not a helper.
func TestRolloutContextTail_EndToEndTailToSeries(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	thread := "thread-context-e2e"
	writeTailRollout(t, home, thread, []string{
		tokenCountLine(110000, 5000, 115000, 200000),
	})
	tail := newRolloutContextTail(home, thread)
	notices := tail.poll()
	if len(notices) != 1 {
		t.Fatalf("fixture rollout yielded %d notices, want 1", len(notices))
	}
	notice, ok := notices[0].(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("notice type = %T, want ContextUsageEvent", notices[0])
	}
	record, emitted, err := executionevent.NormalizeEvent("session_e2e", 3, time.Now(), notice)
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
	if wire.Payload["usedPercent"] != 57.5 {
		t.Errorf("usedPercent = %v, want 57.5 (100*115000/200000)", wire.Payload["usedPercent"])
	}
	if wire.Payload["harness"] != "codex" || wire.Payload["source"] != "rollout-token-count" {
		t.Errorf("payload identity = %v; want codex/rollout-token-count", wire.Payload)
	}
}
