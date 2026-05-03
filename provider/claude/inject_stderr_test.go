package claude

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// fakeMultiCLIResumeStderrFail builds a /bin/sh fake claude that:
//   - on the parent invocation (no --resume): emits the canned parent
//     JSONL body and exits zero.
//   - on the resume invocation (--resume present): writes the supplied
//     stderr body to fd 2 and exits 1, producing no stdout events.
//
// Used by TestHandle_Inject_ResumeStderr_* to verify that Inject's
// non-zero exit path captures the resume subprocess's stderr tail and
// surfaces it both in the wrapped error and via h.logger.Warn.
func fakeMultiCLIResumeStderrFail(t *testing.T, parentBody, resumeStderrBody string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-resume-fail.sh")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *' --resume '*)\n" +
		"    cat <<'CLAUDE_RESUME_STDERR_EOF' 1>&2\n" +
		resumeStderrBody + "\n" +
		"CLAUDE_RESUME_STDERR_EOF\n" +
		"    exit 1\n" +
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

// syncBuffer is a goroutine-safe bytes.Buffer for capturing slog
// output. The slog handler may write from multiple goroutines, so we
// guard the underlying buffer with a mutex.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withCapturedDefaultLogger swaps slog.Default() for a JSON handler
// writing to the returned syncBuffer for the duration of the test.
// The handle's logger field is built via slog.With at spawn time, which
// inherits the default handler — so this captures Handle.Inject's
// logger.Warn call too.
func withCapturedDefaultLogger(t *testing.T) *syncBuffer {
	t.Helper()
	prev := slog.Default()
	buf := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestHandle_Inject_ResumeStderr_CapturedInError(t *testing.T) {
	// Not parallel: we mutate slog.Default() to capture structured
	// records emitted by Handle.Inject's logger.Warn.

	logBuf := withCapturedDefaultLogger(t)

	parentBody := `{"type":"system","subtype":"init","session_id":"sess-stderr-1"}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"usage":{}}`
	resumeStderrBody := "claude: oauth token expired and refresh failed"
	cli := fakeMultiCLIResumeStderrFail(t, parentBody, resumeStderrBody)
	p := newProviderForFake(t, cli)

	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "first"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	// Wait for parent init so SessionID is captured.
	_ = drainUntilResult(t, h)
	if h.SessionID() == "" {
		t.Fatal("SessionID empty after parent drain")
	}

	// Drain resume subprocess events in a goroutine — the resume
	// produces no stdout events (it exits 1 after writing to stderr)
	// but the scanner goroutine still needs to observe EOF before
	// cmd.Wait returns; running drain in parallel keeps the channel
	// unblocked.
	go func() { _ = drainUntilResult(t, h) }()

	err = h.Inject(context.Background(), "follow up")
	if err == nil {
		t.Fatal("Inject should error when resume subprocess exits non-zero")
	}
	if !strings.Contains(err.Error(), "stderr tail:") {
		t.Errorf("Inject error %q should contain 'stderr tail:'", err.Error())
	}
	if !strings.Contains(err.Error(), resumeStderrBody) {
		t.Errorf("Inject error %q should contain captured stderr body %q", err.Error(), resumeStderrBody)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "Inject resume subprocess exited non-zero") {
		t.Errorf("expected Warn log for non-zero exit; logs=%q", logs)
	}
	if !strings.Contains(logs, resumeStderrBody) {
		t.Errorf("Warn log %q should carry stderrTail field with body %q", logs, resumeStderrBody)
	}
	if !strings.Contains(logs, "sess-stderr-1") {
		t.Errorf("Warn log %q should carry sessionID field", logs)
	}
	if !strings.Contains(logs, `"exitCode":1`) {
		t.Errorf("Warn log %q should carry exitCode=1", logs)
	}
}
