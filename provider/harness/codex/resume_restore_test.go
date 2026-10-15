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

// resumeScriptServer is a fake app-server script for the restore path: it
// answers thread/start with a fixed id and thread/resume with the real
// CLI's acceptance/rejection shapes — a known thread resumes cleanly, an
// unknown one is rejected with the exact "no rollout found" error text a
// real CLI returns (see
// TestResumeExistingNamedThreadWithRequest_AbsentFailsTyped).
func resumeScriptServer(t *testing.T, fs *fakeServer, knownThread string) {
	t.Helper()
	dec := json.NewDecoder(fs.stdin)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			return
		}
		method, _ := msg["method"].(string)
		idRaw, hasID := msg["id"]
		params, _ := msg["params"].(map[string]any)
		switch {
		case method == "initialize" && hasID:
			fs.replyOK(t, idRaw)
		case method == "config/batchWrite" && hasID:
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{"filePath": "x", "status": "ok", "version": "fake"},
			})
		case method == "config/read" && hasID:
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{"config": map[string]any{codexMCPConfigKeyPath: map[string]any{}}, "origins": map[string]any{}},
			})
		case method == "mcpServerStatus/list" && hasID:
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{"data": []any{}},
			})
		case method == "thread/start" && hasID:
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"result": map[string]any{"thread": map[string]any{"id": knownThread}},
			})
		case method == "thread/resume" && hasID:
			threadID, _ := params["threadId"].(string)
			if threadID == knownThread {
				fs.replyOK(t, idRaw)
				continue
			}
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": idRaw,
				"error": map[string]any{
					"code":    -32600,
					"message": "no rollout found for thread id " + threadID,
				},
			})
		case method == "turn/start" && hasID:
			fs.replyOK(t, idRaw)
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "thread/started",
				"params":  map[string]any{"threadId": knownThread, "thread": map[string]any{"id": knownThread}},
			})
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "turn/completed",
				"params": map[string]any{
					"threadId": knownThread,
					"turn":     map[string]any{"status": "completed"},
				},
			})
		case hasID:
			fs.replyOK(t, idRaw)
		}
	}
}

func newResumeTestProvider(t *testing.T, knownThread string) (*Provider, *fakeServer) {
	t.Helper()
	fs, stdinW, stdoutR := newFakeServer()
	go resumeScriptServer(t, fs, knownThread)
	p, err := New(Options{
		skipProcess:       true,
		stdinOverride:     stdinW,
		stdoutOverride:    stdoutR,
		verifyMCPReadback: true,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	if err := p.ensureStarted(); err != nil {
		fs.close()
		t.Fatalf("ensureStarted: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	return p, fs
}

// TestResume_CapturesThreadIDFromServerState pins the Codex half of owned
// resume tokens: after launch the handle's session id is the server-state
// thread id (from thread/start), announced on the InitEvent the runner
// mirrors into receipts — and a later Resume of that id re-attaches to
// the same thread instead of starting a new one.
func TestResume_CapturesThreadIDFromServerState(t *testing.T) {
	t.Parallel()

	p, _ := newResumeTestProvider(t, "thread-owned-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Captured from server state after launch: the synchronous thread id
	// AND the InitEvent announcement must agree — the InitEvent is what
	// the runner persists as the restore token.
	if h.SessionID() != "thread-owned-1" {
		t.Fatalf("SessionID = %q, want the server-state thread id", h.SessionID())
	}
	var initID string
	for _, ev := range drainEvents(t, h.Events(), 10*time.Second) {
		if init, ok := ev.(agent.InitEvent); ok {
			initID = init.SessionID
		}
	}
	if initID != "thread-owned-1" {
		t.Fatalf("InitEvent session id = %q, want the server-state thread id", initID)
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rh, err := p.Resume(ctx, "thread-owned-1", agent.Spec{Prompt: "continue", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Resume of the captured thread id: %v", err)
	}
	defer func() { _ = rh.Stop(ctx) }()
	if rh.SessionID() != "thread-owned-1" {
		t.Fatalf("resumed SessionID = %q, want the same thread id", rh.SessionID())
	}
	drainEvents(t, rh.Events(), 10*time.Second)
}

// TestResume_UnknownThreadFailsTypedNotFound pins the never-fresh rule on
// the headless lane: resuming a thread the backend has never seen fails
// with agent.ErrSessionNotFound naming the thread — never a silent fresh
// spawn — so the restore machine admits fresh_primed for exactly this
// shape via agent.IsNoConversationFound.
func TestResume_UnknownThreadFailsTypedNotFound(t *testing.T) {
	t.Parallel()

	p, _ := newResumeTestProvider(t, "thread-owned-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "thread-gone-9", agent.Spec{Prompt: "continue", Cwd: "/tmp/wt"})
	if h != nil {
		_ = h.Stop(ctx)
		t.Fatal("Resume of an unknown thread must not return a handle")
	}
	if err == nil {
		t.Fatal("Resume of an unknown thread must fail")
	}
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("error = %v, want wrapping agent.ErrSessionNotFound", err)
	}
	if !strings.Contains(err.Error(), "thread-gone-9") {
		t.Fatalf("error does not name the thread: %v", err)
	}
	if !agent.IsNoConversationFound(err) {
		t.Fatalf("typed not-found must classify via agent.IsNoConversationFound: %v", err)
	}
}

func TestIsThreadNotFoundResumeError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed sentinel", agent.ErrSessionNotFound, true},
		{"codex rollout text", &RPCError{Method: "thread/resume", Code: -32600, Message: "no rollout found for thread id x"}, true},
		{"backend failure code", &RPCError{Method: "thread/resume", Code: -32000, Message: "app-server unreachable"}, false},
		{"unrelated mentioning threads", errors.New("too many threads"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isThreadNotFoundResumeError(tt.err); got != tt.want {
				t.Fatalf("isThreadNotFoundResumeError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestRestore_EndToEndDecideFromHarnessErrors wires the two halves
// together: the restore machine decides fresh_primed from the Codex
// not-found shape and attention_required from a backend failure, with
// the harness token echoed on the receipt.
func TestRestore_EndToEndDecideFromHarnessErrors(t *testing.T) {
	t.Parallel()

	notFound := &RPCError{Method: "thread/resume", Code: -32600, Message: "no rollout found for thread id thread-gone-9"}
	receipt := agent.DecideRestore(agent.RestoreRequest{ResumeToken: "thread-gone-9", Harness: agent.HarnessCodex}, nil, notFound)
	if receipt.Outcome != agent.RestoreFreshPrimed || receipt.FreshReason != agent.FreshReasonNoConversation {
		t.Fatalf("not-found must decide fresh_primed/no_conversation_found: %+v", receipt)
	}

	backendDown := &RPCError{Method: "thread/resume", Code: -32000, Message: "app-server unreachable"}
	receipt = agent.DecideRestore(agent.RestoreRequest{ResumeToken: "thread-owned-1", Harness: agent.HarnessCodex}, nil, backendDown)
	if receipt.Outcome != agent.RestoreAttentionRequired {
		t.Fatalf("backend failure must decide attention_required, never fresh: %+v", receipt)
	}
}
