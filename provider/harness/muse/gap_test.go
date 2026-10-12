package muse

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// TestHandle_GapRecovery drives the production gap path: a view/gap
// notification on the live stream triggers view/subscribe re-anchor,
// the tracked item revisions reset, and the session continues to its
// terminal instead of failing.
func TestHandle_GapRecovery(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeHost()
	defer fs.close()
	fs.script = func(t *testing.T, fs *fakeHost, req map[string]any) {
		params, _ := req["params"].(map[string]any)
		sid, _ := params["sessionId"].(string)
		cid, _ := params["commandId"].(string)
		itemID := "item-agent-gap"
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:1", "turnId": cid, "commandId": cid},
		})
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/started",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:2", "item": map[string]any{
				"itemId": itemID, "kind": "agentMessage", "revision": 1, "status": "inProgress", "turnId": cid,
			}},
		})
		// The hole: cursors v:3..v:5 were dropped.
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "view/gap",
			"params": map[string]any{"sessionId": sid, "after": "v:2", "next": "v:6"},
		})
		// Post-gap replay: the item completes at a higher revision.
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/completed",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:6", "item": map[string]any{
				"itemId": itemID, "kind": "agentMessage", "revision": 3, "status": "completed",
				"turnId": cid, "text": "recovered text",
			}},
		})
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:7", "turnId": cid, "terminal": "completed"},
		})
	}
	go fs.run(t)
	p := newTestProvider(t, fs, stdinW, stdoutR)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: "/tmp", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("terminal contract: %v\nkinds: %v", err, kindsOf(events))
	}
	term := terminalOf(events)
	res, ok := term.(agent.ResultEvent)
	if !ok || !res.Success {
		t.Fatalf("gap must not fail the session; terminal: %#v", term)
	}
	// The gap is visible on the stream, the re-anchor reached the
	// host, and the replayed text arrived.
	sawGap := false
	sawText := false
	for _, ev := range events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == "view_gap" {
			sawGap = true
		}
		if txt, ok := ev.(agent.AssistantTextEvent); ok && strings.Contains(txt.Text, "recovered text") {
			sawText = true
		}
	}
	if !sawGap || !sawText {
		t.Fatalf("gap event=%v replayed text=%v; kinds=%v", sawGap, sawText, kindsOf(events))
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	subscribed := false
	reanchored := false
	for i, m := range fs.methods {
		if m == "view/subscribe" {
			subscribed = true
			if after, _ := fs.params[i]["after"].(string); after == "v:2" {
				reanchored = true
			}
		}
	}
	if !subscribed || !reanchored {
		t.Fatalf("host never saw view/subscribe{after:v:2}; methods=%v params=%v", fs.methods, fs.params)
	}
}

// TestHandle_GapRecoveryFailure drives a gap whose re-anchor the host
// rejects: the production path must end the session with a terminal
// ErrorEvent (Code view_gap_unrecoverable), never hang or close
// silently.
func TestHandle_GapRecoveryFailure(t *testing.T) {
	t.Parallel()
	cr, cw := newPipePair(t)
	sr, sw := newPipePair(t)
	go func() {
		decoder := json.NewDecoder(cr)
		for {
			var req map[string]any
			if err := decoder.Decode(&req); err != nil {
				return
			}
			method, _ := req["method"].(string)
			id := req["id"]
			params, _ := req["params"].(map[string]any)
			switch method {
			case "initialize":
				writeJSON(t, sw, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"serverInfo": map[string]any{"name": "muse"}, "userAgent": "t",
					"museHome": "/tmp", "grantedCapabilities": []string{}, "experimentalApi": false,
					"schema": map[string]any{"version": 1, "fingerprint": "sha256:ab"},
				}})
			case "initialized":
			case "session/start":
				sid, _ := params["sessionId"].(string)
				writeJSON(t, sw, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"session": map[string]any{"sessionId": sid, "status": "idle", "activeTurnId": nil}, "viewCursor": "v:0",
				}})
				writeJSON(t, sw, map[string]any{
					"jsonrpc": "2.0", "method": "session/started",
					"params": map[string]any{"session": map[string]any{"sessionId": sid}},
				})
			case "turn/start":
				sid, _ := params["sessionId"].(string)
				cid, _ := params["commandId"].(string)
				writeJSON(t, sw, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"commandId": cid, "status": "accepted", "turnId": cid, "startedNewTurn": true, "disposition": "started",
				}})
				writeJSON(t, sw, map[string]any{
					"jsonrpc": "2.0", "method": "view/gap",
					"params": map[string]any{"sessionId": sid, "after": "v:1", "next": "v:9"},
				})
			case "view/subscribe":
				writeJSON(t, sw, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
					"code": -32011, "message": "no such anchor",
				}})
			default:
				writeJSON(t, sw, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			}
		}
	}()
	p, err := New(Options{skipProcess: true, stdinOverride: cw, stdoutOverride: sr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	term := terminalOf(events)
	ee, ok := term.(agent.ErrorEvent)
	if !ok {
		t.Fatalf("unrecoverable gap must end on ErrorEvent, got %#v", term)
	}
	if ee.Code != "view_gap_unrecoverable" {
		t.Fatalf("ErrorEvent code=%q", ee.Code)
	}
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("terminal contract: %v", err)
	}
}
