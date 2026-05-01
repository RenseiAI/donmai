package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// fakeServer is a goroutine-driven fake codex app-server that runs
// against the provider's stdin/stdout pipes. It implements just enough
// JSON-RPC behavior to drive a Handle through start → events → stop.
type fakeServer struct {
	stdin  *io.PipeReader // server reads from here (provider's stdin write end)
	stdout *io.PipeWriter // server writes here (provider's stdout read end)

	mu      sync.Mutex
	threads map[string]bool
}

func newFakeServer() (*fakeServer, *io.PipeWriter, *io.PipeReader) {
	// Create the two pipes the provider uses:
	//   provider.stdin  → server reads
	//   provider.stdout ← server writes
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	fs := &fakeServer{
		stdin:   stdinReader,
		stdout:  stdoutWriter,
		threads: map[string]bool{},
	}
	return fs, stdinWriter, stdoutReader
}

func (fs *fakeServer) write(t *testing.T, body any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = fs.stdout.Write(append(buf, '\n'))
}

func (fs *fakeServer) close() {
	_ = fs.stdin.Close()
	_ = fs.stdout.Close()
}

// run reads JSON-RPC requests from stdin and replies according to a
// deterministic script. Specifically it:
//   - replies to `initialize` with empty result
//   - replies to `thread/start` with a fresh thread id
//   - replies to `turn/start` with empty result, then emits a canned
//     event sequence ending in turn/completed
//   - replies to all other requests with empty result
func (fs *fakeServer) run(t *testing.T, threadID string) {
	dec := json.NewDecoder(fs.stdin)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			return
		}
		method, _ := msg["method"].(string)
		idRaw, hasID := msg["id"]
		switch {
		case method == "initialize" && hasID:
			fs.replyOK(t, idRaw)
		case method == "thread/start" && hasID:
			fs.mu.Lock()
			fs.threads[threadID] = true
			fs.mu.Unlock()
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      idRaw,
				"result":  map[string]any{"thread": map[string]any{"id": threadID}},
			})
		case method == "turn/start" && hasID:
			fs.replyOK(t, idRaw)
			// Drive a canned event sequence on the stream.
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "thread/started",
				"params":  map[string]any{"threadId": threadID, "thread": map[string]any{"id": threadID}},
			})
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "turn/started",
				"params":  map[string]any{"threadId": threadID, "turn": map[string]any{"id": "t1"}},
			})
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "item/agentMessage/delta",
				"params":  map[string]any{"threadId": threadID, "delta": "hello world"},
			})
			fs.write(t, map[string]any{
				"jsonrpc": "2.0",
				"method":  "turn/completed",
				"params": map[string]any{
					"threadId": threadID,
					"turn": map[string]any{
						"status": "completed",
						"usage":  map[string]any{"input_tokens": 100, "output_tokens": 50},
					},
				},
			})
		case hasID:
			fs.replyOK(t, idRaw)
		}
	}
}

func (fs *fakeServer) replyOK(t *testing.T, id any) {
	fs.write(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{},
	})
}

func newTestProvider(t *testing.T) (*Provider, *fakeServer) {
	t.Helper()
	fs, stdinW, stdoutR := newFakeServer()
	// Start the fake server BEFORE New() — the initialize handshake
	// writes to stdin and blocks on a response, so the consumer must
	// already be running.
	go fs.run(t, "thread-A")
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	return p, fs
}

func TestHandle_SpawnEventsTerminalResult(t *testing.T) {
	p, _ := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	got := drainEvents(t, h.Events(), 5*time.Second)

	// Expect the canned sequence: init, system (turn_started),
	// assistant_text, result.
	wantKinds := []agent.EventKind{
		agent.EventInit,
		agent.EventSystem,
		agent.EventAssistantText,
		agent.EventResult,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("expected %d events, got %d: %v", len(wantKinds), len(got), kindsOf(got))
	}
	for i, k := range wantKinds {
		if got[i].Kind() != k {
			t.Fatalf("event[%d] kind: expected %s, got %s", i, k, got[i].Kind())
		}
	}
	if h.SessionID() != "thread-A" {
		t.Fatalf("expected SessionID=thread-A, got %q", h.SessionID())
	}
}

func TestHandle_InjectReturnsUnsupported(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "x", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := h.Inject(ctx, "follow up"); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestHandle_ApprovalBridge_AutoApproveEmitsToolEvents(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()

	// Custom server: respond to initialize/thread/start/turn/start
	// then send an approval server-request mid-turn. Started BEFORE
	// New() so the initialize handshake unblocks.
	threadID := "thread-AP"
	go func() {
		dec := json.NewDecoder(fs.stdin)
		emittedApproval := false
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			method, _ := msg["method"].(string)
			idRaw, hasID := msg["id"]
			switch {
			case method == "initialize" && hasID:
				fs.replyOK(t, idRaw)
			case method == "thread/start" && hasID:
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"thread": map[string]any{"id": threadID}},
				})
			case method == "turn/start" && hasID:
				fs.replyOK(t, idRaw)
				if !emittedApproval {
					emittedApproval = true
					// Approval server-request.
					fs.write(t, map[string]any{
						"jsonrpc": "2.0", "id": 999,
						"method": "execCommandApproval",
						"params": map[string]any{
							"threadId": threadID,
							"command":  "pnpm test",
						},
					})
					// Then a terminal turn/completed.
					fs.write(t, map[string]any{
						"jsonrpc": "2.0",
						"method":  "turn/completed",
						"params": map[string]any{
							"threadId": threadID,
							"turn":     map[string]any{"status": "completed"},
						},
					})
				}
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "hi", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	got := drainEvents(t, h.Events(), 5*time.Second)

	// Must include exactly one ToolUseEvent + ToolResultEvent for
	// the approval, plus the terminal ResultEvent.
	var sawToolUse, sawToolResult, sawResult bool
	for _, ev := range got {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			if e.ToolName == "shell" && e.Input["command"] == "pnpm test" {
				sawToolUse = true
			}
		case agent.ToolResultEvent:
			if e.ToolName == "shell" {
				sawToolResult = true
			}
		case agent.ResultEvent:
			sawResult = true
		}
	}
	if !sawToolUse || !sawToolResult || !sawResult {
		t.Fatalf("missing approval events (toolUse=%v toolResult=%v result=%v) in: %v",
			sawToolUse, sawToolResult, sawResult, kindsOf(got))
	}
}

func TestHandle_ApprovalBridge_DenyEmitsSystemEventAndDeclines(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()

	threadID := "thread-D"
	var approvalReply atomic.Value // map[string]any — captured response
	go func() {
		dec := json.NewDecoder(fs.stdin)
		emittedApproval := false
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			method, _ := msg["method"].(string)
			idRaw, hasID := msg["id"]
			// Capture the response to our approval (no method, has
			// id 999).
			if !hasID && msg["jsonrpc"] != nil {
				continue
			}
			if method == "" && hasID {
				if id, ok := idRaw.(float64); ok && int(id) == 999 {
					approvalReply.Store(msg)
				}
				continue
			}
			switch {
			case method == "initialize" && hasID:
				fs.replyOK(t, idRaw)
			case method == "thread/start" && hasID:
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"thread": map[string]any{"id": threadID}},
				})
			case method == "turn/start" && hasID:
				fs.replyOK(t, idRaw)
				if !emittedApproval {
					emittedApproval = true
					fs.write(t, map[string]any{
						"jsonrpc": "2.0", "id": 999,
						"method": "execCommandApproval",
						"params": map[string]any{
							"threadId": threadID,
							"command":  "rm -rf /",
						},
					})
					fs.write(t, map[string]any{
						"jsonrpc": "2.0",
						"method":  "turn/completed",
						"params": map[string]any{
							"threadId": threadID,
							"turn":     map[string]any{"status": "completed"},
						},
					})
				}
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "hi", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got := drainEvents(t, h.Events(), 5*time.Second)

	var sawDeny bool
	for _, ev := range got {
		if se, ok := ev.(agent.SystemEvent); ok && se.Subtype == "approval_denied" {
			sawDeny = true
		}
	}
	if !sawDeny {
		t.Fatalf("expected approval_denied SystemEvent, got: %v", kindsOf(got))
	}

	// Wait briefly for the JSON-RPC response on the wire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if approvalReply.Load() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rep, _ := approvalReply.Load().(map[string]any)
	if rep == nil {
		t.Fatalf("no approval reply observed")
	}
	result, ok := rep["result"].(map[string]any)
	if !ok || result["decision"] != "decline" {
		t.Fatalf("expected decline, got result=%v", rep)
	}
}

func TestHandle_StopIsIdempotent(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "x", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func drainEvents(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var out []agent.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timer.C:
			t.Fatalf("timed out draining events; collected %d so far: %v", len(out), kindsOf(out))
			return out
		}
	}
}

func kindsOf(events []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind()
	}
	return out
}
