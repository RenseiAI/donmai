package afcli

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/provider/harness/stub/stubagent"
)

// stubAgentChildEnv turns this test binary into the stub-agent child.
//
// The end-to-end tests below need a REAL process on the far end of a real PTY
// — one that owns its own signal disposition and its own exit status — and
// they need it to be the production command, not a re-implementation of it.
// Re-executing this test binary and dispatching to newStubAgentCmd is what
// gets both: no build step, and the thing under test is the command the
// harness actually spawns.
const stubAgentChildEnv = "DONMAI_TEST_STUB_AGENT_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(stubAgentChildEnv) == "1" {
		os.Exit(runStubAgentChild())
	}
	os.Exit(m.Run())
}

func runStubAgentChild() int {
	cmd := newStubAgentCmd()
	cmd.SetArgs([]string{})
	err := cmd.ExecuteContext(context.Background())
	if code, ok := StubAgentExitCode(err); ok {
		return code
	}
	if err != nil {
		return 1
	}
	return 0
}

// spawnStubSession spawns the fake agent through the stub harness's real
// interactive Spawn — the same call the runner makes — pointed at this test
// binary's child entry point.
func spawnStubSession(t *testing.T, scenario, prompt string) (agent.Handle, agent.InteractiveSession) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}

	provider, err := stub.New(stub.WithStubAgentCommand(os.Args[0]))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	spec := agent.Spec{
		Cwd:    t.TempDir(),
		Prompt: prompt,
		Env: map[string]string{
			stubAgentChildEnv:     "1",
			stubagent.EnvScenario: scenario,
			"DONMAI_STATE_HOME":   t.TempDir(),
		},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	}

	handle, err := provider.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("stub interactive Spawn: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = handle.Stop(stopCtx)
	})

	capable, ok := handle.(agent.InteractiveCapable)
	if !ok {
		t.Fatalf("handle %T does not implement agent.InteractiveCapable", handle)
	}
	return handle, capable.InteractiveSession()
}

// ptyReader streams the session's output into a buffer in the background, so
// a test can both WAIT for a line and, later, read the whole transcript. A
// test that stops a session needs the first: signalling a child that has not
// yet installed its signal handler proves nothing but the default
// disposition, and the child's own first line is what says it is ready.
type ptyReader struct {
	mu     sync.Mutex
	buf    strings.Builder
	closed chan struct{}
}

func newPTYReader(t *testing.T, session agent.InteractiveSession) *ptyReader {
	t.Helper()
	sub, err := session.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	r := &ptyReader{closed: make(chan struct{})}
	go func() {
		defer close(r.closed)
		defer func() { _ = sub.Close() }()
		for frame := range sub.Frames() {
			if frame.Type != attachwire.TypeOutput {
				continue
			}
			r.mu.Lock()
			r.buf.Write(attachwire.DecodeOutput(frame.Payload).Data)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *ptyReader) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// waitFor blocks until want appears in the transcript.
func (r *ptyReader) waitFor(t *testing.T, want string, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		if strings.Contains(r.text(), want) {
			return
		}
		select {
		case <-r.closed:
			if strings.Contains(r.text(), want) {
				return
			}
			t.Fatalf("session ended before %q appeared; transcript:\n%s", want, r.text())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %q; transcript:\n%s", want, r.text())
}

// waitClosed blocks until the child's output stream ends and returns the whole
// transcript.
func (r *ptyReader) waitClosed(t *testing.T, deadline time.Duration) string {
	t.Helper()
	select {
	case <-r.closed:
		return r.text()
	case <-time.After(deadline):
		t.Fatalf("timed out waiting for the session to end; transcript so far:\n%s", r.text())
		return r.text()
	}
}

// awaitResult drains the coarse handle events and returns the terminal one.
func awaitResult(t *testing.T, handle agent.Handle, deadline time.Duration) agent.ResultEvent {
	t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case event, ok := <-handle.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if result, isResult := event.(agent.ResultEvent); isResult {
				return result
			}
		case <-timeout:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
}

// TestStubAgentSessionOutcomes drives the fake agent end to end through a real
// PTY and asserts the two things every session-lifecycle assertion upstream is
// built on: what the terminal said, and what the exit status was.
func TestStubAgentSessionOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		scenario    string
		prompt      string
		wantSuccess bool
		wantOut     []string
		wantErrSub  string
	}{
		{
			name:        "clean exit",
			scenario:    `{"version":1,"name":"clean","steps":[{"print":"stub up"}]}`,
			wantSuccess: true,
			wantOut:     []string{"stub up"},
		},
		{
			name:        "scripted failure reaches the terminal result",
			scenario:    `{"version":1,"name":"fail","exitCode":1,"steps":[{"print":"about to fail"}]}`,
			wantSuccess: false,
			wantOut:     []string{"about to fail"},
			wantErrSub:  "code 1",
		},
		{
			name: "the PTY seed reaches the child",
			// The prompt is written INTO the terminal by the harness's seed
			// delivery; echoing it back is how the test observes that the
			// input direction of the PTY works, not just the output one.
			scenario:    `{"version":1,"name":"seeded","steps":[{"awaitInput":{"timeout":"20s","echo":true}}]}`,
			prompt:      "hello stub",
			wantSuccess: true,
			wantOut:     []string{stubagent.EchoPrefix + "hello stub"},
		},
		{
			name:        "agent-to-agent line is emitted in its wire shape",
			scenario:    `{"version":1,"name":"chatty","seed":2,"steps":[{"a2a":{"text":"work started","contextId":"ctx-7"}}]}`,
			wantSuccess: true,
			wantOut:     []string{stubagent.A2ALinePrefix},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handle, session := spawnStubSession(t, tc.scenario, tc.prompt)
			out := newPTYReader(t, session).waitClosed(t, 30*time.Second)
			result := awaitResult(t, handle, 10*time.Second)

			if result.Success != tc.wantSuccess {
				t.Errorf("ResultEvent.Success = %v, want %v (errors: %v)", result.Success, tc.wantSuccess, result.Errors)
			}
			if tc.wantErrSub != "" && !strings.Contains(strings.Join(result.Errors, " "), tc.wantErrSub) {
				t.Errorf("ResultEvent.Errors = %v, want one containing %q", result.Errors, tc.wantErrSub)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("PTY transcript missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}

// TestStubAgentA2ALineParsesOffTheTerminal proves the marked line survives the
// terminal round trip — CRLF translation included — and decodes as the real
// protocol type a consumer will assert against.
func TestStubAgentA2ALineParsesOffTheTerminal(t *testing.T) {
	scenario := `{"version":1,"name":"wire","seed":4,"steps":[{"a2a":{"text":"work started","contextId":"ctx-7","taskId":"task-9"}}]}`
	_, session := spawnStubSession(t, scenario, "")
	out := newPTYReader(t, session).waitClosed(t, 30*time.Second)

	var found bool
	for _, line := range strings.Split(out, "\n") {
		message, err := stubagent.ParseA2ALine(line)
		if err != nil {
			continue
		}
		found = true
		if message.ContextID != "ctx-7" || message.TaskID != "task-9" {
			t.Errorf("ContextID/TaskID = %q/%q, want ctx-7/task-9", message.ContextID, message.TaskID)
		}
		if len(message.Parts) != 1 {
			t.Fatalf("len(Parts) = %d, want 1", len(message.Parts))
		}
		if text, ok := message.Parts[0].Text(); !ok || text != "work started" {
			t.Errorf("Parts[0].Text() = %q/%v, want \"work started\"/true", text, ok)
		}
	}
	if !found {
		t.Fatalf("no agent-to-agent line in the transcript:\n%s", out)
	}
}

// TestStubAgentCooperativeStop is the pair that makes a stop assertion mean
// something. A harness that answers the stop exits with the status the
// scenario declared; a harness that refuses it has to be killed. Without the
// second case, "the stop succeeded" is satisfied by a stop path that does
// nothing at all.
func TestStubAgentCooperativeStop(t *testing.T) {
	// The first step is a readiness line, and it is load-bearing: the child
	// installs its signal handler before it prints, so a signal sent after the
	// line is one the child was able to observe. Signalling earlier would test
	// the default disposition of an unprepared process — which kills it in
	// every mode, making both cases below pass for the wrong reason.
	const ready = "stub agent ready to be stopped"

	t.Run("answered", func(t *testing.T) {
		scenario := `{"version":1,"name":"stops","steps":[{"print":"` + ready + `"}],` +
			`"hangFor":"forever","stop":{"mode":"respond","exitCode":0}}`
		handle, session := spawnStubSession(t, scenario, "")
		reader := newPTYReader(t, session)
		reader.waitFor(t, ready, 30*time.Second)

		go func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = handle.Stop(stopCtx)
		}()

		out := reader.waitClosed(t, 30*time.Second)
		result := awaitResult(t, handle, 10*time.Second)

		if !strings.Contains(out, "mode=respond") {
			t.Errorf("transcript does not record the observed stop; got:\n%s", out)
		}
		if !result.Success {
			t.Errorf("ResultEvent.Success = false (%v); a stop the child answered with status 0 is a clean end", result.Errors)
		}
	})

	t.Run("refused", func(t *testing.T) {
		scenario := `{"version":1,"name":"ignores","steps":[{"print":"` + ready + `"}],` +
			`"hangFor":"forever","stop":{"mode":"ignore"}}`
		handle, session := spawnStubSession(t, scenario, "")
		reader := newPTYReader(t, session)
		reader.waitFor(t, ready, 30*time.Second)

		go func() {
			// A short deadline forces the escalation quickly: ptyhost's Stop
			// sends SIGTERM, then SIGKILL when the context ends. The test is
			// about the child refusing the first signal, not about the length
			// of anyone's grace window.
			stopCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_ = handle.Stop(stopCtx)
		}()

		// The refusal notice is the discriminating observation: it can only be
		// written by a child that RECEIVED the stop and stayed alive.
		reader.waitFor(t, "mode=ignore", 30*time.Second)
		out := reader.waitClosed(t, 30*time.Second)
		result := awaitResult(t, handle, 10*time.Second)

		if result.Success {
			t.Error("ResultEvent.Success = true; a child that had to be killed did not end cleanly")
		}
		if joined := strings.Join(result.Errors, " "); !strings.Contains(joined, "signal") {
			t.Errorf("ResultEvent.Errors = %v, want one naming the signal that ended the child", result.Errors)
		}
		if strings.Contains(out, "mode=respond") {
			t.Errorf("transcript records a respond-mode stop in the ignore scenario:\n%s", out)
		}
	})
}

// TestStubAgentDefaultsWithoutAScenario pins the no-configuration path: the
// child still runs a real, clean session rather than failing to start.
func TestStubAgentDefaultsWithoutAScenario(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	provider, err := stub.New(stub.WithStubAgentCommand(os.Args[0]))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	handle, err := provider.Spawn(context.Background(), agent.Spec{
		Cwd:         t.TempDir(),
		Env:         map[string]string{stubAgentChildEnv: "1", "DONMAI_STATE_HOME": t.TempDir()},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("stub interactive Spawn: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = handle.Stop(stopCtx)
	})

	if result := awaitResult(t, handle, 30*time.Second); !result.Success {
		t.Errorf("default scenario did not end cleanly: %v", result.Errors)
	}
}
