package muse

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// fakeHost is a goroutine-driven fake `muse serve` host that runs
// against the provider's stdin/stdout pipes. It implements just enough
// MSP behavior to drive a Handle through handshake → session/start →
// turn/start → events → turn/completed.
type fakeHost struct {
	stdin  *io.PipeReader // host reads from here (provider's stdin write end)
	stdout *io.PipeWriter // host writes here (provider's stdout read end)

	mu        sync.Mutex
	methods   []string
	params    []map[string]any
	sessionID string
	// script overrides the default canned event sequence when set.
	script func(t *testing.T, fs *fakeHost, req map[string]any)
	// decideSeen records approval/decide requests.
	decideSeen []map[string]any
	// grantMCP controls the grantedCapabilities on initialize. Default
	// true (grants sessionMcp).
	grantMCP *bool
}

func newFakeHost() (*fakeHost, *io.PipeWriter, *io.PipeReader) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	fs := &fakeHost{stdin: stdinReader, stdout: stdoutWriter}
	grant := true
	fs.grantMCP = &grant
	return fs, stdinWriter, stdoutReader
}

func (fs *fakeHost) write(t *testing.T, body any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = fs.stdout.Write(append(buf, '\n'))
}

func (fs *fakeHost) close() {
	_ = fs.stdin.Close()
	_ = fs.stdout.Close()
}

// run reads JSON-RPC requests and replies per the canned script:
//   - initialize → schema version 1 + granted sessionMcp, then expects
//     the initialized notification (drops anything before it per FR-008)
//   - session/start → fresh echo + session/started + result
//   - turn/start → ack (turnId == commandId) + canned event sequence
//     ending in turn/completed{terminal:completed}
//   - view/subscribe → empty result (gap recovery re-anchor)
//   - approval/decide → recorded + ack
//   - all other requests → empty result
func (fs *fakeHost) run(t *testing.T) {
	t.Helper()
	decoder := json.NewDecoder(fs.stdin)
	initialized := false
	for {
		var req map[string]any
		if err := decoder.Decode(&req); err != nil {
			return
		}
		method, _ := req["method"].(string)
		id := req["id"]
		params, _ := req["params"].(map[string]any)
		if method == "initialized" {
			initialized = true
			continue
		}
		fs.mu.Lock()
		fs.methods = append(fs.methods, method)
		fs.params = append(fs.params, params)
		fs.mu.Unlock()

		// The initialize handshake itself is always answered;
		// FR-008 drops what comes BEFORE the handshake completes
		// (any non-handshake request), not the handshake.
		if method != "initialize" && !initialized && id != nil {
			// FR-008: the host drops anything sent before the
			// handshake completes — answer nothing.
			continue
		}
		switch method {
		case "initialize":
			granted := []string{"sessionMcp"}
			fs.mu.Lock()
			g := fs.grantMCP != nil && *fs.grantMCP
			fs.mu.Unlock()
			if !g {
				granted = []string{}
			}
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"serverInfo":          map[string]any{"name": "muse", "version": "1.4.6"},
					"userAgent":           "muse-test/1.4.6",
					"museHome":            "/tmp/muse-home",
					"grantedCapabilities": granted,
					"experimentalApi":     false,
					"schema":              map[string]any{"version": 1, "fingerprint": "sha256:" + "ab"},
				},
			})
		case "session/start":
			sid, _ := params["sessionId"].(string)
			fs.mu.Lock()
			fs.sessionID = sid
			fs.mu.Unlock()
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"session":    map[string]any{"sessionId": sid, "status": "idle", "activeTurnId": nil, "modelId": "muse-wonder-1.0"},
					"viewCursor": "v:0",
				},
			})
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "method": "session/started",
				"params": map[string]any{"session": map[string]any{"sessionId": sid, "status": "idle", "modelId": "muse-wonder-1.0"}},
			})
		case "session/resume":
			sid, _ := params["sessionId"].(string)
			fs.mu.Lock()
			fs.sessionID = sid
			fs.mu.Unlock()
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{
					"session":         map[string]any{"sessionId": sid, "status": "idle", "activeTurnId": nil, "modelId": "muse-wonder-1.0"},
					"history":         map[string]any{"mode": "none", "items": nil, "snapshot": nil},
					"pendingRequests": []any{},
					"viewCursor":      "v:9",
				},
			})
		case "turn/start":
			sid, _ := params["sessionId"].(string)
			cid, _ := params["commandId"].(string)
			if fs.script != nil {
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": id,
					"result": map[string]any{"commandId": cid, "status": "accepted", "turnId": cid, "startedNewTurn": true, "disposition": "started"},
				})
				fs.script(t, fs, req)
				continue
			}
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"commandId": cid, "status": "accepted", "turnId": cid, "startedNewTurn": true, "disposition": "started"},
			})
			emitCompletedTurn(t, fs, sid, cid)
		case "turn/interrupt", "turn/steer", "view/subscribe", "view/unsubscribe":
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"commandId": params["commandId"], "status": "accepted", "turnId": params["turnId"]},
			})
		case "approval/decide":
			fs.mu.Lock()
			fs.decideSeen = append(fs.decideSeen, params)
			fs.mu.Unlock()
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"approvalId": params["approvalId"], "commandId": params["commandId"], "status": "accepted", "terminal": true},
			})
		default:
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "id": id, "result": map[string]any{},
			})
		}
	}
}

// emitCompletedTurn replays the canonical single-turn fixture: turn
// started, one agentMessage item (open → delta → completed), one
// tokenUsage step with cumulative cost, then turn/completed.
func emitCompletedTurn(t *testing.T, fs *fakeHost, sid, turnID string) {
	t.Helper()
	itemID := "item-agent-1"
	fs.write(t, map[string]any{
		"jsonrpc": "2.0", "method": "turn/started",
		"params": map[string]any{"sessionId": sid, "viewCursor": "v:1", "turnId": turnID, "commandId": turnID},
	})
	fs.write(t, map[string]any{
		"jsonrpc": "2.0", "method": "item/started",
		"params": map[string]any{"sessionId": sid, "viewCursor": "v:2", "item": map[string]any{
			"itemId": itemID, "kind": "agentMessage", "revision": 1, "status": "inProgress", "turnId": turnID, "text": "",
		}},
	})
	fs.write(t, map[string]any{
		"jsonrpc": "2.0", "method": "item/delta",
		"params": map[string]any{"sessionId": sid, "viewCursor": "v:3", "itemId": itemID, "field": "text", "delta": "hello from the fixture"},
	})
	fs.write(t, map[string]any{
		"jsonrpc": "2.0", "method": "session/tokenUsage",
		"params": map[string]any{
			"sessionId": sid, "viewCursor": "v:4", "turnId": turnID, "modelId": "muse-wonder-1.0",
			"usage":        map[string]any{"inputTokens": 100, "outputTokens": 20, "cachedTokens": 10, "reasoningTokens": 5},
			"promptTokens": 100, "totalTokens": 120,
			"cumulative": map[string]any{"promptTokens": 100, "outputTokens": 20, "totalTokens": 120, "cost": map[string]any{"usd": 0.0012, "partial": false}},
		},
	})
	fs.write(t, map[string]any{
		"jsonrpc": "2.0", "method": "item/completed",
		"params": map[string]any{"sessionId": sid, "viewCursor": "v:5", "item": map[string]any{
			"itemId": itemID, "kind": "agentMessage", "revision": 2, "status": "completed", "turnId": turnID, "text": "hello from the fixture",
		}},
	})
	fs.write(t, map[string]any{
		"jsonrpc": "2.0", "method": "turn/completed",
		"params": map[string]any{"sessionId": sid, "viewCursor": "v:6", "turnId": turnID, "terminal": "completed", "durationMs": 642},
	})
}

func newTestProvider(t *testing.T, _ *fakeHost, stdinW *io.PipeWriter, stdoutR *io.PipeReader) *Provider {
	t.Helper()
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func drainEvents(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for events; got %d: %v", len(out), kindsOf(out))
			return out
		}
	}
}

func kindsOf(events []agent.Event) []string {
	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind()))
	}
	return kinds
}

func terminalOf(events []agent.Event) agent.Event {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind() {
		case agent.EventResult, agent.EventError:
			return events[i]
		}
	}
	return nil
}
