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
	"sync/atomic"
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

// codexRolloutFlushRaceMessage is the fake error text this suite injects for
// codex's transient "rollout file not flushed yet" failure — the same
// -32603 shape a real app-server returns from thread/read immediately after
// thread/name/set when the thread-store's rollout-*.jsonl metadata file has
// not reached disk yet. Both an "is empty"/unreadable phrase and the
// "rollout" artifact name must be present for isRolloutFlushRaceError to
// classify it as this specific race (see that function's doc comment).
const codexRolloutFlushRaceMessage = "failed to read session metadata from " +
	"/home/ci/.codex/sessions/2026/08/31/rollout-2026-08-31T00-00-00-thread-live.jsonl: " +
	"rollout at that path is empty"

// TestAwaitAndNameLiveThreadWithRequest_TransientRolloutFlushRaceRetries pins
// the fix for a live spawn-failure race: a named interactive session's
// thread/read verification (right after thread/name/set) can land before
// codex's app-server has flushed the new thread's rollout file to disk,
// returning a transient -32603 that named this file empty/unreadable. Before
// this fix that single failure was fatal and killed the session seconds
// after start, reproducing reliably on hosts with slow codex-home I/O. A
// bounded number of these transient failures must instead be retried with
// backoff until the flush catches up.
func TestAwaitAndNameLiveThreadWithRequest_TransientRolloutFlushRaceRetries(t *testing.T) {
	t.Parallel()
	await := func(context.Context, string, time.Duration) (json.RawMessage, error) {
		return json.RawMessage(`{"thread":{"id":"thread-live"}}`), nil
	}

	const failCount = 3
	var reads int32
	request := func(_ context.Context, method string, params map[string]any, _ time.Duration) (json.RawMessage, error) {
		switch method {
		case "thread/name/set":
			return json.RawMessage(`{}`), nil
		case "thread/read":
			if params["threadId"] != "thread-live" {
				t.Fatalf("thread/read params = %#v", params)
			}
			n := atomic.AddInt32(&reads, 1)
			if n <= failCount {
				return nil, &RPCError{Method: "thread/read", Code: -32603, Message: codexRolloutFlushRaceMessage}
			}
			return json.RawMessage(`{"thread":{"id":"thread-live","name":"chief-of-staff"}}`), nil
		default:
			t.Fatalf("unexpected request method %q", method)
			return nil, nil
		}
	}

	retry := rolloutReadRetryPolicy{initialBackoff: time.Millisecond, capTotal: 200 * time.Millisecond}
	err := awaitAndNameLiveThreadWithRequestAndRetry(
		context.Background(), agent.Spec{SessionName: "chief-of-staff"}, await, request, time.Second, retry,
	)
	if err != nil {
		t.Fatalf("awaitAndNameLiveThreadWithRequestAndRetry: %v", err)
	}
	if got := atomic.LoadInt32(&reads); got != failCount+1 {
		t.Fatalf("thread/read attempts = %d, want %d", got, failCount+1)
	}
}

// TestAwaitAndNameLiveThreadWithRequest_PersistentRolloutFlushRaceFailsAfterExhaustion
// pins the other half of the contract: a thread/read that NEVER recovers
// (a genuine failure wearing the same -32603/rollout shape, or a host whose
// I/O never catches up) must still fail the spawn — retrying forever would
// just trade one hang for another. The final RPCError must surface, and the
// message must say how many attempts were tolerated so a genuine failure
// stays diagnosable.
func TestAwaitAndNameLiveThreadWithRequest_PersistentRolloutFlushRaceFailsAfterExhaustion(t *testing.T) {
	t.Parallel()
	await := func(context.Context, string, time.Duration) (json.RawMessage, error) {
		return json.RawMessage(`{"thread":{"id":"thread-live"}}`), nil
	}

	var reads int32
	rolloutErr := &RPCError{Method: "thread/read", Code: -32603, Message: codexRolloutFlushRaceMessage}
	request := func(_ context.Context, method string, _ map[string]any, _ time.Duration) (json.RawMessage, error) {
		switch method {
		case "thread/name/set":
			return json.RawMessage(`{}`), nil
		case "thread/read":
			atomic.AddInt32(&reads, 1)
			return nil, rolloutErr
		default:
			t.Fatalf("unexpected request method %q", method)
			return nil, nil
		}
	}

	retry := rolloutReadRetryPolicy{initialBackoff: time.Millisecond, capTotal: 10 * time.Millisecond}
	err := awaitAndNameLiveThreadWithRequestAndRetry(
		context.Background(), agent.Spec{SessionName: "chief-of-staff"}, await, request, time.Second, retry,
	)
	if err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32603 {
		t.Fatalf("expected the final RPCError (code -32603) to surface, got %v", err)
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("expected the error to name the retry attempts made, got %v", err)
	}
	if got := atomic.LoadInt32(&reads); got < 2 {
		t.Fatalf("expected more than one thread/read attempt before giving up, got %d", got)
	}
}

// TestIsRolloutFlushRaceError_ConservativeMatch pins the deliberately narrow
// classification: only a -32603 whose message names BOTH the rollout
// artifact and an empty/unreadable-read phrase is retried. An unrelated
// -32603 (or any other code) must fail fast, exactly like before this fix —
// otherwise a genuine internal error would be silently retried for up to
// the full backoff cap instead of surfacing.
func TestIsRolloutFlushRaceError_ConservativeMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"non-rpc error", errors.New("boom"), false},
		{"matching rollout-empty message", &RPCError{Code: -32603, Message: codexRolloutFlushRaceMessage}, true},
		{
			"matching session-metadata phrasing without 'is empty'",
			&RPCError{Code: -32603, Message: "failed to read session metadata for rollout-thread-live.jsonl"},
			true,
		},
		{"unrelated -32603 internal error", &RPCError{Code: -32603, Message: "panic: nil pointer dereference"}, false},
		{"wrong code with matching text", &RPCError{Code: -32600, Message: codexRolloutFlushRaceMessage}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRolloutFlushRaceError(tc.err); got != tc.want {
				t.Fatalf("isRolloutFlushRaceError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
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
