package clijsonl

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// claudeArgs mirrors the fixed headless argv the claude provider's
// buildArgs emits for a basic prompt-only spawn. The clijsonl driver is
// argv-agnostic — the harness supplies the flags — so the driver tests
// drive it with this representative claude-compatible argv.
func claudeArgs() []string {
	return []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
}

// Compile-time assertion: the driver Handle satisfies agent.Handle.
var _ agent.Handle = (*Handle)(nil)

// fakeCLI returns a path to a /bin/sh script that simulates the
// claude CLI's stream-json output. The script writes the canned body
// to stdout and exits zero. Body text is interpolated into a
// here-document; embed only literal lines with no shell-special
// characters that need escaping.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\n" +
		"cat <<'CLAUDE_EOF'\n" +
		body +
		"\nCLAUDE_EOF\n"
	// Write WITHOUT the exec bit, then chmod-add it after close.
	// Linux can throw ETXTBSY on fork+exec when a writable FD is open
	// on an executable inode at the moment of exec — under parallel
	// test load with sibling goroutines forking, the kernel sometimes
	// surfaces a stale write-handle hand-off. Writing with mode 0o600
	// and chmodding to 0o700 post-close means the file never carries
	// the exec bit while any writable FD exists.
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake cli: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("chmod fake cli: %v", err)
	}
	return path
}

// spawnFake spawns the fake CLI via the shared SpawnBinary driver with a
// claude-compatible argv and no MCP config. It is the driver-level
// stand-in for claude.Provider.Spawn used across the clijsonl tests.
func spawnFake(t *testing.T, binary string, spec agent.Spec) *Handle {
	t.Helper()
	return spawnFakeCtx(t.Context(), t, binary, spec)
}

// spawnFakeCtx is spawnFake with a caller-supplied context (for
// cancellation tests). The spec's MCPServers drive the Stop-cleanup
// assertion; pass a spec with none for spawns that have no MCP config.
//
// The spawn retries briefly on ETXTBSY ("text file busy"): the fake CLI
// script is written by this test binary moments before exec, and a
// concurrently forking sibling test can inherit the not-yet-closed write
// fd, making the kernel refuse the exec (golang.org/issue/22315). The
// race is test-fixture-specific — production binaries are not freshly
// written by the spawning process — so the retry lives here, not in
// SpawnBinary.
func spawnFakeCtx(ctx context.Context, t *testing.T, binary string, spec agent.Spec) *Handle {
	t.Helper()
	mcpPath, err := WriteMCPConfig(spec.MCPServers)
	if err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}
	var h *Handle
	for attempt := 0; ; attempt++ {
		h, err = SpawnBinary(ctx, binary, claudeArgs(), spec.Prompt, mcpPath, spec.Cwd, spec.Env, spec.OnProcessSpawned)
		if err == nil {
			return h
		}
		if attempt >= 3 || !strings.Contains(err.Error(), "text file busy") {
			t.Fatalf("SpawnBinary: %v", err)
		}
		time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
		// WriteMCPConfig's file was cleaned up by the failed spawn; re-write it.
		if mcpPath != "" {
			if mcpPath, err = WriteMCPConfig(spec.MCPServers); err != nil {
				t.Fatalf("WriteMCPConfig (retry): %v", err)
			}
		}
	}
}

// collect drains events from h until the channel closes OR a
// terminal ResultEvent / synthetic ErrorEvent is observed. Post
// F.2.3-cap-flip the events channel stays open after the parent
// subprocess EOFs (so Inject() can stream a follow-up turn's events
// onto it); tests that previously relied on close-on-EOF use this
// helper to bound their drain.
//
// Synchronization is channel-based (not wall-clock): we return the
// moment we observe a terminal event or the channel closes. The 30s
// ceiling exists only as a -race + full-suite-load safety net so
// fork/exec + bufio.Scanner setup latency under contention doesn't
// time-bomb a deterministic JSONL fixture. In healthy conditions
// these helpers return in <50 ms via the terminal-event path. This
// mirrors the F3 fix in provider/amp (commit d7df186), replacing the
// historical 5s hard timeout + 200ms post-terminal idle wait that
// flaked under -race + full-suite load.
func collect(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var got []agent.Event
	hardTimeout := time.NewTimer(30 * time.Second)
	defer hardTimeout.Stop()
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
		case <-hardTimeout.C:
			t.Fatalf("timed out waiting for events; got %d so far", len(got))
		}
	}
}

// seenTerminal reports whether events ends in a ResultEvent or an
// ErrorEvent with one of the synthetic terminal codes.
func seenTerminal(events []agent.Event) bool {
	if len(events) == 0 {
		return false
	}
	switch ev := events[len(events)-1].(type) {
	case agent.ResultEvent:
		return true
	case agent.ErrorEvent:
		return ev.Code == "spawn_no_result" || ev.Code == "stdout_scan"
	}
	return false
}

func TestHandle_HappyPath_FakeCLI(t *testing.T) {
	t.Parallel()

	body := `{"type":"system","subtype":"init","session_id":"sess-fake-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello."}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":3}}`
	cli := fakeCLI(t, body)

	h := spawnFake(t, cli, agent.Spec{Prompt: "test"})
	defer func() { _ = h.Stop(t.Context()) }()

	events := collect(t, h)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %v", len(events), events)
	}
	if _, ok := events[0].(agent.InitEvent); !ok {
		t.Errorf("events[0] %T, want InitEvent", events[0])
	}
	if _, ok := events[1].(agent.AssistantTextEvent); !ok {
		t.Errorf("events[1] %T, want AssistantTextEvent", events[1])
	}
	r, ok := events[2].(agent.ResultEvent)
	if !ok {
		t.Fatalf("events[2] %T, want ResultEvent", events[2])
	}
	if !r.Success {
		t.Error("ResultEvent.Success should be true")
	}

	if h.SessionID() != "sess-fake-1" {
		t.Errorf("SessionID = %q, want sess-fake-1", h.SessionID())
	}
}

func TestHandle_StopIdempotent(t *testing.T) {
	t.Parallel()

	cli := fakeCLI(t, `{"type":"system","subtype":"init","session_id":"x"}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"usage":{}}`)

	h := spawnFake(t, cli, agent.Spec{Prompt: "x"})
	_ = collect(t, h)

	if err := h.Stop(t.Context()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Errorf("Stop (second call should be idempotent): %v", err)
	}
}

func TestHandle_NoTerminal_SyntheticErrorEvent(t *testing.T) {
	t.Parallel()

	// CLI exits cleanly without emitting a result line. Provider
	// should synthesize an ErrorEvent so the runner observes the
	// failure.
	body := `{"type":"system","subtype":"init","session_id":"sx"}`
	cli := fakeCLI(t, body)

	h := spawnFake(t, cli, agent.Spec{Prompt: "x"})
	defer func() { _ = h.Stop(t.Context()) }()

	events := collect(t, h)
	if len(events) < 2 {
		t.Fatalf("got %d events, want init + synthetic error", len(events))
	}
	last := events[len(events)-1]
	er, ok := last.(agent.ErrorEvent)
	if !ok {
		t.Fatalf("last event %T, want ErrorEvent", last)
	}
	if er.Code != "spawn_no_result" {
		t.Errorf("Code = %q, want spawn_no_result", er.Code)
	}
}

func TestHandle_CtxCancel_Stops(t *testing.T) {
	t.Parallel()

	// Long-running fake CLI that sleeps; ctx cancel should kill it.
	dir := t.TempDir()
	path := filepath.Join(dir, "sleep-claude.sh")
	script := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init","session_id":"sx"}'` + "\n" +
		"sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("chmod: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	h := spawnFakeCtx(ctx, t, path, agent.Spec{Prompt: "x"})

	// Wait for the init event so we know the subprocess started.
	// Generous deadline because coverage instrumentation + -race
	// scheduler load can stretch shell-fork startup well beyond 3s.
	select {
	case <-h.Events():
	case <-time.After(15 * time.Second):
		t.Fatal("init event never arrived")
	}

	cancel()

	// Events channel should close within a few seconds (SIGTERM grace + cleanup).
	// Generous deadline because coverage instrumentation slows things down.
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return // closed; success
			}
		case <-deadline.C:
			t.Fatal("events channel did not close after ctx cancel")
		}
	}
}

func TestHandle_StopCleansMCPTmpfile(t *testing.T) {
	t.Parallel()

	cli := fakeCLI(t, `{"type":"system","subtype":"init","session_id":"x"}
{"type":"result","subtype":"success","is_error":false,"num_turns":0,"usage":{}}`)

	// spawnFakeCtx returns the concrete *Handle so we can read the MCP
	// path off it (mcpConfigPath is unexported on the driver Handle).
	h := spawnFakeCtx(t.Context(), t, cli, agent.Spec{
		Prompt: "x",
		MCPServers: []agent.MCPServerConfig{
			{Name: "af_linear", Command: "/bin/echo", Args: []string{"hi"}},
		},
	})
	mcpPath := h.mcpConfigPath
	if mcpPath == "" {
		t.Fatal("expected mcpConfigPath to be set")
	}
	if _, err := os.Stat(mcpPath); err != nil {
		t.Errorf("mcp config tmpfile missing during run: %v", err)
	}

	_ = collect(t, h)
	if err := h.Stop(t.Context()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if _, err := os.Stat(mcpPath); !os.IsNotExist(err) {
		t.Errorf("mcp config tmpfile not cleaned up: %v", err)
	}
}

func TestBoundedBuffer_DropsOldestBytes(t *testing.T) {
	t.Parallel()

	b := newBoundedBuffer(8)
	if _, err := b.Write([]byte("123")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := b.Write([]byte("4567")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := b.String(), "1234567"; got != want {
		t.Errorf("under cap: %q want %q", got, want)
	}
	if _, err := b.Write([]byte("89AB")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := b.String(); len(got) != 8 {
		t.Errorf("at cap: len = %d, want 8", len(got))
	}
	if !strings.HasSuffix(b.String(), "89AB") {
		t.Errorf("at cap should retain newest bytes: %q", b.String())
	}
}

func TestBoundedBuffer_OversizedWrite(t *testing.T) {
	t.Parallel()

	b := newBoundedBuffer(4)
	if _, err := b.Write([]byte("ABCDEFGHIJ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := b.String(), "GHIJ"; got != want {
		t.Errorf("oversized: %q want %q", got, want)
	}
}
