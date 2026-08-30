package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/coder/websocket"
)

// TestAwaitAndNameLiveThreadWithRequest_NamesTheObservedThread pins the
// fresh-session sequence: the PTY creates its own thread (observed via a
// thread/started notification), and this function names THAT exact thread
// post-hoc and verifies the readback — never issuing thread/start itself,
// since a thread created here before the PTY exists cannot later be
// reattached by the PTY (see interactive_name.go's package doc comment for
// the production defect this replaced).
func TestAwaitAndNameLiveThreadWithRequest_NamesTheObservedThread(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{SessionName: "chief-of-staff"}

	await := func(_ context.Context, method string, _ time.Duration) (json.RawMessage, error) {
		if method != "thread/started" {
			t.Fatalf("awaitNotification method = %q, want thread/started", method)
		}
		return json.RawMessage(`{"thread":{"id":"thread-live"}}`), nil
	}

	var calls []string
	request := func(_ context.Context, method string, params map[string]any, _ time.Duration) (json.RawMessage, error) {
		calls = append(calls, method)
		switch method {
		case "thread/name/set":
			if params["threadId"] != "thread-live" || params["name"] != "chief-of-staff" {
				t.Fatalf("thread/name/set params = %#v", params)
			}
			return json.RawMessage(`{}`), nil
		case "thread/read":
			if params["threadId"] != "thread-live" {
				t.Fatalf("thread/read params = %#v", params)
			}
			return json.RawMessage(`{"thread":{"id":"thread-live","name":"chief-of-staff"}}`), nil
		default:
			t.Fatalf("unexpected request method %q", method)
			return nil, nil
		}
	}

	if err := awaitAndNameLiveThreadWithRequest(context.Background(), spec, await, request, time.Second); err != nil {
		t.Fatalf("awaitAndNameLiveThreadWithRequest: %v", err)
	}
	if len(calls) != 2 || calls[0] != "thread/name/set" || calls[1] != "thread/read" {
		t.Fatalf("request call order = %v, want [thread/name/set thread/read]", calls)
	}
}

func TestAwaitAndNameLiveThreadWithRequest_NotificationTimeoutFails(t *testing.T) {
	t.Parallel()
	await := func(context.Context, string, time.Duration) (json.RawMessage, error) {
		return nil, errors.New("timed out waiting for thread/started")
	}
	request := func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error) {
		t.Fatal("request must not be called when the thread/started wait fails")
		return nil, nil
	}
	err := awaitAndNameLiveThreadWithRequest(context.Background(), agent.Spec{SessionName: "chief-of-staff"}, await, request, time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestAwaitAndNameLiveThreadWithRequest_ReadbackMismatchFails(t *testing.T) {
	t.Parallel()
	await := func(context.Context, string, time.Duration) (json.RawMessage, error) {
		return json.RawMessage(`{"thread":{"id":"thread-live"}}`), nil
	}
	request := func(_ context.Context, method string, _ map[string]any, _ time.Duration) (json.RawMessage, error) {
		if method == "thread/name/set" {
			return json.RawMessage(`{}`), nil
		}
		// thread/read reports a DIFFERENT name than what was set — must fail
		// closed rather than report success on an unverified rename.
		return json.RawMessage(`{"thread":{"id":"thread-live","name":"someone-else"}}`), nil
	}
	err := awaitAndNameLiveThreadWithRequest(context.Background(), agent.Spec{SessionName: "chief-of-staff"}, await, request, time.Second)
	if err == nil {
		t.Fatal("expected a readback-mismatch error")
	}
}

func TestAwaitAndNameLiveThreadWithRequest_EmptyNameFailsClosed(t *testing.T) {
	t.Parallel()
	await := func(context.Context, string, time.Duration) (json.RawMessage, error) {
		t.Fatal("must not wait for a notification with no session name to apply")
		return nil, nil
	}
	request := func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error) {
		t.Fatal("must not issue any RPC with no session name to apply")
		return nil, nil
	}
	if err := awaitAndNameLiveThreadWithRequest(context.Background(), agent.Spec{}, await, request, time.Second); err == nil {
		t.Fatal("expected an error")
	}
}

// testThreadUUID is a codex-thread-id-shaped fixture value: the same
// 8-4-4-4-12 hex form observed from a real codex-cli 0.151.0 thread/start
// response (e.g. "01a0548d-9a06-7a30-a72c-f7c94b8c899c").
const testThreadUUID = "01a0548d-9a06-7a30-a72c-f7c94b8c899c"

// TestResumeExistingNamedThreadWithRequest_ExistingSucceeds pins the
// attach-to-existing sequence: a single thread/resume RPC call, keyed on a
// thread-id-shaped SessionName, proves the target exists before any PTY
// side effect — the same primitive Provider.Resume already uses for the
// headless lane. A real codex-cli 0.151.0 probe confirmed thread/resume
// accepts a still-live thread by id even when it has taken no turn yet (it
// is only the `codex resume` CLI subcommand's own separate rollout-file
// lookup — used by the fresh-session path's PTY attach, not this RPC — that
// requires persistence).
func TestResumeExistingNamedThreadWithRequest_ExistingSucceeds(t *testing.T) {
	t.Parallel()
	var gotMethod string
	var gotThreadID any
	request := func(_ context.Context, method string, params map[string]any, _ time.Duration) (json.RawMessage, error) {
		gotMethod = method
		gotThreadID = params["threadId"]
		return json.RawMessage(`{}`), nil
	}
	err := resumeExistingNamedThreadWithRequest(context.Background(), agent.Spec{SessionName: testThreadUUID}, request, time.Second)
	if err != nil {
		t.Fatalf("resumeExistingNamedThreadWithRequest: %v", err)
	}
	if gotMethod != "thread/resume" || gotThreadID != testThreadUUID {
		t.Fatalf("request = method %q threadId %v, want thread/resume %s", gotMethod, gotThreadID, testThreadUUID)
	}
}

// TestResumeExistingNamedThreadWithRequest_AbsentFailsTyped pins the exact
// failure this exists to produce: a signalled attach whose properly-shaped
// (thread-id-shaped) target does not exist must fail with
// agent.ErrSessionNotFound naming the session — never a silent fallback
// that could go on to spawn a PTY against a different (freshly created)
// thread. The fake error text below is the exact message codex-cli 0.151.0
// returns for a thread/resume against a thread with no persisted rollout
// (verified against a real codex binary).
func TestResumeExistingNamedThreadWithRequest_AbsentFailsTyped(t *testing.T) {
	t.Parallel()
	request := func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error) {
		return nil, &RPCError{
			Method: "thread/resume", Code: -32600,
			Message: "no rollout found for thread id " + testThreadUUID,
		}
	}
	err := resumeExistingNamedThreadWithRequest(context.Background(), agent.Spec{SessionName: testThreadUUID}, request, time.Second)
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("error = %v, want wrapping agent.ErrSessionNotFound", err)
	}
	if !strings.Contains(err.Error(), testThreadUUID) {
		t.Fatalf("error does not name the session: %v", err)
	}
}

// TestResumeExistingNamedThreadWithRequest_NonUUIDNameFailsWithoutProbing
// pins the F3 fix: thread/resume's threadId parameter is a thread id, never
// a human-assigned name (a real codex-cli 0.151.0 probe rejects a non-UUID
// id with its own distinct "invalid session id" error — a different failure
// than "not found"). A name-shaped SessionName must fail closed with
// ErrResumeRequiresThreadID BEFORE any RPC is attempted, never silently
// probed as though it were an id and misreported as agent.ErrSessionNotFound.
func TestResumeExistingNamedThreadWithRequest_NonUUIDNameFailsWithoutProbing(t *testing.T) {
	t.Parallel()
	request := func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error) {
		t.Fatal("must not probe thread/resume with a non-thread-id-shaped name")
		return nil, nil
	}
	err := resumeExistingNamedThreadWithRequest(context.Background(), agent.Spec{SessionName: "chief-of-staff"}, request, time.Second)
	if !errors.Is(err, ErrResumeRequiresThreadID) {
		t.Fatalf("error = %v, want wrapping ErrResumeRequiresThreadID", err)
	}
	if errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("a wrong-shape name must not be misreported as agent.ErrSessionNotFound: %v", err)
	}
}

func TestResumeExistingNamedThreadWithRequest_EmptyNameFailsClosed(t *testing.T) {
	t.Parallel()
	request := func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error) {
		t.Fatal("must not issue any RPC with no session name to resume")
		return nil, nil
	}
	if err := resumeExistingNamedThreadWithRequest(context.Background(), agent.Spec{}, request, time.Second); err == nil {
		t.Fatal("expected an error")
	}
}

// TestNamedInteractiveAppServer_CloseClientConcurrentWithFullClose hammers
// closeClient's two real callers against each other: the spawn goroutine's
// eager close right after naming/the existence check completes (see
// finishNamingLiveInteractiveThread and startNamedInteractiveAppServer), and
// ptycli's own cleanup goroutine (Handle.run), which calls close and can run
// concurrently if the TUI exits inside the naming window. Before the
// clientMu guard this raced (and could nil-deref) on the client field
// itself; -race must stay clean here and every iteration must leave the
// client cleared exactly once.
func TestNamedInteractiveAppServer_CloseClientConcurrentWithFullClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process signaling; the production code path is unix-only for the same reason (signal_windows.go)")
	}
	t.Parallel()

	// A minimal local WebSocket peer gives closeClient/close a real
	// *websocket.Conn to operate on, mirroring what a live bootstrap
	// connection looks like in production, without spawning a real codex
	// binary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		<-r.Context().Done()
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	const iterations = 50
	for i := 0; i < iterations; i++ {
		conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
		if err != nil {
			t.Fatalf("iteration %d: dial: %v", i, err)
		}
		cmd := exec.Command("true")
		if err := cmd.Start(); err != nil {
			t.Fatalf("iteration %d: start fixture process: %v", i, err)
		}
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()

		server := &namedInteractiveAppServer{
			remoteURL: "unix:///dev/null",
			cmd:       cmd,
			waitCh:    waitCh,
			socketDir: t.TempDir(),
		}
		server.setClient(&interactiveWebSocketClient{conn: conn, transport: &http.Transport{}})

		var wg sync.WaitGroup
		wg.Add(2)
		// Caller 1: the spawn goroutine's eager close.
		go func() {
			defer wg.Done()
			_ = server.closeClient()
		}()
		// Caller 2: ptycli's cleanup path (Handle.run -> cleanupFn -> close).
		go func() {
			defer wg.Done()
			_ = server.close()
		}()
		wg.Wait()

		if got := server.getClient(); got != nil {
			t.Fatalf("iteration %d: client still set after both closers ran: %v", i, got)
		}
		// close is independently idempotent (closeOnce); calling it again
		// here must not hang, panic, or change the outcome.
		if err := server.close(); err != nil {
			t.Fatalf("iteration %d: repeat close: %v", i, err)
		}
	}
}
