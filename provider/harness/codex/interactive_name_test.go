package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

// TestResumeExistingNamedThreadWithRequest_ExistingSucceeds pins the
// attach-to-existing sequence: a single thread/resume RPC call, keyed on
// SessionName, proves the target exists before any PTY side effect — the
// same primitive Provider.Resume already uses for the headless lane.
func TestResumeExistingNamedThreadWithRequest_ExistingSucceeds(t *testing.T) {
	t.Parallel()
	var gotMethod string
	var gotThreadID any
	request := func(_ context.Context, method string, params map[string]any, _ time.Duration) (json.RawMessage, error) {
		gotMethod = method
		gotThreadID = params["threadId"]
		return json.RawMessage(`{}`), nil
	}
	err := resumeExistingNamedThreadWithRequest(context.Background(), agent.Spec{SessionName: "chief-of-staff"}, request, time.Second)
	if err != nil {
		t.Fatalf("resumeExistingNamedThreadWithRequest: %v", err)
	}
	if gotMethod != "thread/resume" || gotThreadID != "chief-of-staff" {
		t.Fatalf("request = method %q threadId %v, want thread/resume chief-of-staff", gotMethod, gotThreadID)
	}
}

// TestResumeExistingNamedThreadWithRequest_AbsentFailsTyped pins the exact
// failure this exists to produce: a signalled attach whose target does not
// exist must fail with agent.ErrSessionNotFound naming the session — never
// a silent fallback that could go on to spawn a PTY against a different
// (freshly created) thread. The fake error text below is the exact message
// codex-cli 0.151.0 returns for a thread/resume against a thread with no
// persisted rollout (verified against a real codex binary).
func TestResumeExistingNamedThreadWithRequest_AbsentFailsTyped(t *testing.T) {
	t.Parallel()
	request := func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error) {
		return nil, &RPCError{
			Method: "thread/resume", Code: -32600,
			Message: "no rollout found for thread id chief-of-staff",
		}
	}
	err := resumeExistingNamedThreadWithRequest(context.Background(), agent.Spec{SessionName: "chief-of-staff"}, request, time.Second)
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("error = %v, want wrapping agent.ErrSessionNotFound", err)
	}
	if !strings.Contains(err.Error(), "chief-of-staff") {
		t.Fatalf("error does not name the session: %v", err)
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
