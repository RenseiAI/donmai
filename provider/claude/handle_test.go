package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

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
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("write fake cli: %v", err)
	}
	return path
}

func newProviderForFake(t *testing.T, fakePath string) *Provider {
	t.Helper()
	p, err := New(Options{
		Binary:   fakePath,
		LookPath: func(name string) (string, error) { return name, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// collect drains events from h until the channel closes OR a brief
// idle deadline elapses after observing a terminal ResultEvent /
// ErrorEvent. Post F.2.3-cap-flip the events channel stays open after
// the parent subprocess EOFs (so Inject() can stream a follow-up
// turn's events onto it); tests that previously relied on close-on-
// EOF use this helper to bound their drain.
func collect(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var got []agent.Event
	hardTimeout := time.NewTimer(5 * time.Second)
	defer hardTimeout.Stop()
	for {
		var idle <-chan time.Time
		if seenTerminal(got) {
			t := time.NewTimer(200 * time.Millisecond)
			defer t.Stop()
			idle = t.C
		}
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-idle:
			return got
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

	p := newProviderForFake(t, cli)
	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "test"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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
	p := newProviderForFake(t, cli)

	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "x"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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

	p := newProviderForFake(t, cli)
	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "x"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
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
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("write: %v", err)
	}
	p := newProviderForFake(t, path)

	ctx, cancel := context.WithCancel(t.Context())
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "x"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

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
	p := newProviderForFake(t, cli)

	// Use the package-private spawn so we can read the MCP path off
	// the concrete *Handle without exposing it on the public
	// agent.Handle interface.
	h, err := p.spawn(t.Context(), agent.Spec{
		Prompt: "x",
		MCPServers: []agent.MCPServerConfig{
			{Name: "af_linear", Command: "/bin/echo", Args: []string{"hi"}},
		},
	}, "")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
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
