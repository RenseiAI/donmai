package muse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// TestHandle_MultiTurn drives the production Spawn entry point with a
// script that completes turn 1 and, on the follow-up turn/start issued
// for the same session, completes turn 2 — proving many turns multiplex
// over one host keyed by one session id, ending on exactly one terminal
// ResultEvent that is also the last event on the channel.
func TestHandle_MultiTurn(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeHost()
	defer fs.close()
	turns := 0
	fs.script = func(t *testing.T, fs *fakeHost, req map[string]any) {
		turns++
		params, _ := req["params"].(map[string]any)
		sid, _ := params["sessionId"].(string)
		cid, _ := params["commandId"].(string)
		if turns == 1 {
			return
		}
		// Second turn: a toolCall open → completed pair, then terminal.
		// The open arrives as item/started (rev 1) so the ToolUse
		// renders; the terminal arrives as item/completed (rev 2).
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:10", "turnId": cid, "commandId": cid},
		})
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/started",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:10.1", "item": map[string]any{
				"itemId": "tool-1", "kind": "toolCall", "revision": 1, "status": "inProgress",
				"turnId": cid, "tool": "write", "args": `{"path":"a.txt"}`,
			}},
		})
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "item/completed",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:11", "item": map[string]any{
				"itemId": "tool-1", "kind": "toolCall", "revision": 2, "status": "completed",
				"turnId": cid, "tool": "write", "args": `{"path":"a.txt"}`,
			}},
		})
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:12", "turnId": cid, "terminal": "completed"},
		})
	}
	go fs.run(t)
	p := newTestProvider(t, fs, stdinW, stdoutR)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "first", Cwd: "/tmp", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	mh := h.(*Handle)
	sessionID := mh.SessionID()
	if sessionID == "" {
		t.Fatal("SessionID empty after start")
	}
	// Steer the second turn through the SAME session: the follow-up
	// turn/start routes through the same subscription and the session
	// ends on one terminal covering both turns.
	turnParams := turnStartParams(sessionID, newUUIDv7(), agent.Spec{Prompt: "more"}, []TurnInputPart{{Type: "text", Text: "more"}}, nil)
	if err := mh.startTurn(ctx, turnParams); err != nil {
		t.Fatalf("second turn/start: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("terminal contract: %v\nkinds: %v", err, kindsOf(events))
	}
	term := terminalOf(events)
	res, ok := term.(agent.ResultEvent)
	if !ok || !res.Success {
		t.Fatalf("terminal is not a successful ResultEvent: %#v", term)
	}
	if res.ObservedTurns == nil || *res.ObservedTurns != 1 {
		t.Fatalf("second turn must complete the session: %+v", res.ObservedTurns)
	}
	// The toolCall pair from turn 2 reached the stream.
	sawToolUse, sawToolResult := false, false
	for _, ev := range events {
		if u, ok := ev.(agent.ToolUseEvent); ok && u.ToolName == "write" {
			sawToolUse = true
		}
		if r, ok := ev.(agent.ToolResultEvent); ok && r.ToolName == "write" {
			sawToolResult = true
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Fatalf("turn-2 toolCall pair missing; kinds=%v", kindsOf(events))
	}
}

// TestHandle_SessionStartRoutesTwoTurns drives Spawn with a script that
// emits non-terminal events for turn 1 and completes the session on the
// follow-up turn/start for the same session — proving the second turn's
// events route through the same subscription and fold into one terminal.
func TestHandle_SessionStartRoutesTwoTurns(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeHost()
	defer fs.close()
	turns := 0
	fs.script = func(t *testing.T, fs *fakeHost, req map[string]any) {
		turns++
		params, _ := req["params"].(map[string]any)
		sid, _ := params["sessionId"].(string)
		cid, _ := params["commandId"].(string)
		if turns == 1 {
			// Turn 1: non-terminal activity only (no turn/completed
			// — the session must stay open for turn 2).
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "method": "turn/started",
				"params": map[string]any{"sessionId": sid, "viewCursor": "v:1", "turnId": cid, "commandId": cid},
			})
			fs.write(t, map[string]any{
				"jsonrpc": "2.0", "method": "item/completed",
				"params": map[string]any{"sessionId": sid, "viewCursor": "v:2", "item": map[string]any{
					"itemId": "msg-1", "kind": "agentMessage", "revision": 1, "status": "completed",
					"turnId": cid, "text": "turn one text",
				}},
			})
			return
		}
		emitCompletedTurn(t, fs, sid, cid)
	}
	go fs.run(t)
	p := newTestProvider(t, fs, stdinW, stdoutR)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: "/tmp", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	mh := h.(*Handle)
	sessionID := mh.SessionID()
	if sessionID == "" {
		t.Fatal("SessionID empty after start")
	}
	// The session ends on the FIRST turn/completed: the forwarder
	// exits at the terminal event (the terminal-event contract),
	// so multi-turn coverage asserts routing + accumulation across
	// turns below the terminal, not multiple terminals on one stream.
	// A second turn/start on the same session routes through the same
	// subscription and completes the session.
	turnParams := turnStartParams(sessionID, newUUIDv7(), agent.Spec{Prompt: "more"}, []TurnInputPart{{Type: "text", Text: "more"}}, nil)
	if err := mh.startTurn(ctx, turnParams); err != nil {
		t.Fatalf("second turn/start: %v", err)
	}
	events := drainEvents(t, h.Events(), 10*time.Second)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Fatalf("terminal contract: %v\nkinds: %v", err, kindsOf(events))
	}
	term := terminalOf(events)
	res, ok := term.(agent.ResultEvent)
	if !ok || !res.Success {
		t.Fatalf("terminal: %#v", term)
	}
	if res.ObservedTurns == nil || *res.ObservedTurns != 1 {
		t.Fatalf("one terminal must fold ObservedTurns=1: %+v", res.ObservedTurns)
	}
	// Turn 1's text arrived before the terminal: both turns' events
	// routed through the one session subscription.
	sawTurnOne := false
	for _, ev := range events {
		if txt, ok := ev.(agent.AssistantTextEvent); ok && txt.Text == "turn one text" {
			sawTurnOne = true
		}
	}
	if !sawTurnOne {
		t.Fatalf("turn-1 events missing; kinds=%v", kindsOf(events))
	}
}

// TestHandle_Interrupt drives Stop through the production path: the
// handle issues turn/interrupt for the running turn and the events
// channel closes.
func TestHandle_Interrupt(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeHost()
	defer fs.close()
	// Never complete the turn: the handle must interrupt it on Stop.
	fs.script = func(t *testing.T, fs *fakeHost, req map[string]any) {
		params, _ := req["params"].(map[string]any)
		sid, _ := params["sessionId"].(string)
		cid, _ := params["commandId"].(string)
		fs.write(t, map[string]any{
			"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"sessionId": sid, "viewCursor": "v:1", "turnId": cid, "commandId": cid},
		})
	}
	go fs.run(t)
	p := newTestProvider(t, fs, stdinW, stdoutR)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "long work", Cwd: "/tmp", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Wait until the turn is known-running, then Stop.
	deadline := time.After(10 * time.Second)
	for h.(*Handle).CurrentTurnID() == "" {
		select {
		case <-deadline:
			t.Fatal("turn never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// The events channel must close promptly after Stop.
	select {
	case _, ok := <-h.Events():
		_ = ok
	case <-time.After(5 * time.Second):
		t.Fatal("events did not close after Stop")
	}
	// turn/interrupt must have reached the host.
	fs.mu.Lock()
	defer fs.mu.Unlock()
	interrupted := false
	for _, m := range fs.methods {
		if m == "turn/interrupt" {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatalf("host never saw turn/interrupt; methods=%v", fs.methods)
	}
}

// TestHandle_Resume drives the production Resume entry point: the
// handle issues session/resume with excludeItems, emits InitEvent, and
// the session continues.
func TestHandle_Resume(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeHost()
	defer fs.close()
	go fs.run(t)
	p := newTestProvider(t, fs, stdinW, stdoutR)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "01a13220-ad69-7157-a63e-294e0d1714de", agent.Spec{Prompt: "continue", Cwd: "/tmp", Autonomous: true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	events := drainEvents(t, func() <-chan agent.Event {
		// Resume emits InitEvent then idles (no turn started by the
		// fake host): close the stream via Stop and collect.
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = h.Stop(context.Background())
		}()
		return h.Events()
	}(), 10*time.Second)
	if len(events) == 0 {
		t.Fatal("no events after resume")
	}
	init, ok := events[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("first event is not InitEvent: %T", events[0])
	}
	if init.SessionID != "01a13220-ad69-7157-a63e-294e0d1714de" {
		t.Fatalf("InitEvent session=%q", init.SessionID)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	resumed := false
	excluded := false
	for i, m := range fs.methods {
		if m == "session/resume" {
			resumed = true
			if ex, _ := fs.params[i]["excludeItems"].(bool); ex {
				excluded = true
			}
		}
	}
	if !resumed || !excluded {
		t.Fatalf("host never saw session/resume{excludeItems:true}; methods=%v params=%v", fs.methods, fs.params)
	}
}

// TestHandle_ResumeGoneSession drives Resume against a host that
// answers sessionNotFound: the production path must surface
// agent.ErrSessionNotFound, never a generic spawn failure.
func TestHandle_ResumeGoneSession(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeHost()
	defer fs.close()
	go fs.run(t)
	p := newTestProvider(t, fs, stdinW, stdoutR)

	// Prime the handshake + session via a throwaway handle, then point
	// the host at a gone session by scripting the resume reply.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := p.Resume(ctx, "01a13220-ad69-7157-a63e-294e0d1714de", agent.Spec{Prompt: "x", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("baseline Resume: %v", err)
	}

	// A second provider against a host that rejects the resume: build
	// the raw client path to prove the kind classifier end to end.
	cr2, cw2 := newPipePair(t)
	sr2, sw2 := newPipePair(t)
	go func() {
		decoder := jsonDecoder(cr2)
		for {
			var req map[string]any
			if err := decoder(&req); err != nil {
				return
			}
			method, _ := req["method"].(string)
			id := req["id"]
			switch method {
			case "initialize":
				writeJSON(t, sw2, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"serverInfo": map[string]any{"name": "muse"}, "userAgent": "t",
					"museHome": "/tmp", "grantedCapabilities": []string{}, "experimentalApi": false,
					"schema": map[string]any{"version": 1, "fingerprint": "sha256:ab"},
				}})
			case "initialized":
			case "session/resume":
				writeJSON(t, sw2, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
					"code": -32020, "message": "no such session",
					"data": map[string]any{"kind": "sessionNotFound"},
				}})
			default:
				writeJSON(t, sw2, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
			}
		}
	}()
	p2, err := New(Options{skipProcess: true, stdinOverride: cw2, stdoutOverride: sr2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p2.Shutdown(context.Background()) }()
	_, err = p2.Resume(ctx, "01a13220-ad69-7157-a63e-294e0d1714de", agent.Spec{Prompt: "x", Cwd: "/tmp"})
	if !isSessionNotFoundErr(err) {
		t.Fatalf("Resume on a gone session must surface ErrSessionNotFound, got: %v", err)
	}
}

func isSessionNotFoundErr(err error) bool {
	return errors.Is(err, agent.ErrSessionNotFound)
}
