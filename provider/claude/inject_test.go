package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// fakeMultiCLI returns a /bin/sh script that emits one body for the
// initial spawn (no --resume argv) and a different body for `claude
// --resume <id>` invocations. Used to drive Inject() tests where we
// need both the parent and the resume subprocess to produce
// distinguishable JSONL streams.
//
// Both bodies are interpolated via heredoc — embed only literal
// lines; no shell-special characters that would need escaping.
func fakeMultiCLI(t *testing.T, parentBody, resumeBody, traceFile string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-multi.sh")
	// The script writes argv to traceFile so tests can assert on the
	// flags the provider passed for resume invocations.
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + traceFile + "\n" +
		"case \" $* \" in\n" +
		"  *' --resume '*)\n" +
		"    cat <<'CLAUDE_RESUME_EOF'\n" +
		resumeBody + "\n" +
		"CLAUDE_RESUME_EOF\n" +
		"    ;;\n" +
		"  *)\n" +
		"    cat <<'CLAUDE_PARENT_EOF'\n" +
		parentBody + "\n" +
		"CLAUDE_PARENT_EOF\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("write fake cli: %v", err)
	}
	return path
}

func TestHandle_Inject_HappyPath(t *testing.T) {
	t.Parallel()

	traceFile := filepath.Join(t.TempDir(), "argv-trace.log")
	parentBody := `{"type":"system","subtype":"init","session_id":"sess-inject-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Turn 1."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.001,"usage":{"input_tokens":5,"output_tokens":2}}`
	resumeBody := `{"type":"system","subtype":"init","session_id":"sess-inject-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Turn 2 (resumed)."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.001,"usage":{"input_tokens":4,"output_tokens":3}}`
	cli := fakeMultiCLI(t, parentBody, resumeBody, traceFile)
	p := newProviderForFake(t, cli)

	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "first turn"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	// Drain parent's events: init + assistant + result. Then call
	// Inject and continue draining the resume subprocess's events.
	parentEvents := drainUntilResult(t, h)
	if len(parentEvents) != 3 {
		t.Fatalf("parent events = %d, want 3 (init+assistant+result): %v", len(parentEvents), parentEvents)
	}
	if h.SessionID() != "sess-inject-1" {
		t.Fatalf("SessionID = %q after parent, want sess-inject-1", h.SessionID())
	}

	if err := h.Inject(t.Context(), "follow up please"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// Drain the resume subprocess's events: init + assistant + result.
	resumeEvents := drainUntilResult(t, h)
	if len(resumeEvents) != 3 {
		t.Fatalf("resume events = %d, want 3: %v", len(resumeEvents), resumeEvents)
	}
	if at, ok := resumeEvents[1].(agent.AssistantTextEvent); !ok || !strings.Contains(at.Text, "Turn 2") {
		t.Errorf("resume assistant event mismatch: %#v", resumeEvents[1])
	}

	// Verify the resume subprocess was invoked with --resume <session-id>.
	traceBytes, err := os.ReadFile(traceFile) //nolint:gosec // traceFile is a temp path created in this test
	if err != nil {
		t.Fatalf("read argv trace: %v", err)
	}
	trace := string(traceBytes)
	if !strings.Contains(trace, "--resume sess-inject-1") {
		t.Errorf("argv trace missing '--resume sess-inject-1': %q", trace)
	}
}

func TestHandle_Inject_BeforeInit_Errors(t *testing.T) {
	t.Parallel()

	// Use a CLI that sleeps before emitting anything, so the
	// SessionID never gets captured before we call Inject.
	dir := t.TempDir()
	path := filepath.Join(dir, "slow-init-claude.sh")
	script := "#!/bin/sh\nsleep 5\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("write: %v", err)
	}
	p := newProviderForFake(t, path)
	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "x"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	err = h.Inject(t.Context(), "too early")
	if err == nil {
		t.Fatal("Inject before InitEvent should error")
	}
	if !errors.Is(err, ErrSessionNotReady) {
		t.Errorf("Inject error %v should wrap ErrSessionNotReady", err)
	}
}

func TestHandle_Inject_InFlightConflict_Errors(t *testing.T) {
	t.Parallel()

	traceFile := filepath.Join(t.TempDir(), "argv-trace.log")
	parentBody := `{"type":"system","subtype":"init","session_id":"sess-inflight-1"}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"usage":{}}`
	// Resume body sleeps so the first Inject is still running when
	// the second one fires. The script's case branch handles this:
	// when --resume is present, sleep before emitting events.
	dir := t.TempDir()
	path := filepath.Join(dir, "slow-resume-claude.sh")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + traceFile + "\n" +
		"case \" $* \" in\n" +
		"  *' --resume '*)\n" +
		"    sleep 2\n" +
		"    echo '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"sess-inflight-1\"}'\n" +
		"    echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"usage\":{}}'\n" +
		"    ;;\n" +
		"  *)\n" +
		"    cat <<'PARENT_EOF'\n" +
		parentBody + "\n" +
		"PARENT_EOF\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("write: %v", err)
	}
	p := newProviderForFake(t, path)

	h, err := p.spawn(t.Context(), agent.Spec{Prompt: "x"}, "")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	// Wait for parent init so SessionID is captured.
	_ = drainUntilResult(t, h)
	if h.SessionID() == "" {
		t.Fatal("SessionID empty after parent drain")
	}

	// Fire first Inject in a goroutine; the slow-resume sleep keeps
	// it in flight long enough for the second Inject to race.
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- h.Inject(t.Context(), "first")
	}()

	// Spin briefly to let the goroutine acquire injectInFlight.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !h.injectInFlight.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !h.injectInFlight.Load() {
		t.Fatal("first Inject did not flip injectInFlight")
	}

	// Second Inject should immediately error.
	err = h.Inject(t.Context(), "second")
	if err == nil {
		t.Fatal("second Inject should error while first is in flight")
	}
	if !errors.Is(err, ErrInjectInFlight) {
		t.Errorf("second Inject error %v should wrap ErrInjectInFlight", err)
	}

	// Drain resume events so the first Inject can complete.
	go func() { _ = drainUntilResult(t, h) }()

	// Wait for first Inject to complete.
	select {
	case err := <-firstDone:
		if err != nil {
			t.Errorf("first Inject: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("first Inject did not complete")
	}
}

func TestHandle_Inject_CtxCancel_KillsSubprocess(t *testing.T) {
	t.Parallel()

	traceFile := filepath.Join(t.TempDir(), "argv-trace.log")
	parentBody := `{"type":"system","subtype":"init","session_id":"sess-cancel-1"}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"usage":{}}`
	// The resume subprocess sleeps so we have time to cancel.
	dir := t.TempDir()
	path := filepath.Join(dir, "long-resume-claude.sh")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + traceFile + "\n" +
		"case \" $* \" in\n" +
		"  *' --resume '*)\n" +
		"    sleep 30\n" +
		"    ;;\n" +
		"  *)\n" +
		"    cat <<'PARENT_EOF'\n" +
		parentBody + "\n" +
		"PARENT_EOF\n" +
		"    ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("write: %v", err)
	}
	p := newProviderForFake(t, path)

	h, err := p.spawn(t.Context(), agent.Spec{Prompt: "x"}, "")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	_ = drainUntilResult(t, h)
	if h.SessionID() == "" {
		t.Fatal("SessionID empty")
	}

	injectCtx, cancel := context.WithCancel(t.Context())
	injectDone := make(chan error, 1)
	go func() { injectDone <- h.Inject(injectCtx, "go") }()

	// Wait for the inject to start, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !h.injectInFlight.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !h.injectInFlight.Load() {
		t.Fatal("Inject did not start")
	}
	cancel()

	// Inject should return promptly after cancel.
	select {
	case err := <-injectDone:
		if err == nil {
			t.Fatal("Inject after cancel should return non-nil error")
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Logf("Inject returned %v (expected ctx.Err)", err)
		}
	case <-time.After(stopGracePeriod + 5*time.Second):
		t.Fatal("Inject did not return after ctx cancel + grace period")
	}
}

// drainUntilResult reads from h.Events until a terminal ResultEvent
// or ErrorEvent (with synthetic terminal codes) is observed, then
// returns. Used by Inject tests where the events channel stays open
// after a turn completes (between-turn injection semantic).
func drainUntilResult(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var got []agent.Event
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
			if seenTerminal(got) {
				return got
			}
		case <-deadline.C:
			t.Fatalf("drainUntilResult: timed out; got %d events", len(got))
		}
	}
}
