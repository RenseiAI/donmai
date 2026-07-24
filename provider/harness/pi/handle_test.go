package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// syncBuffer is a concurrency-safe io.Writer that captures every command the
// handle writes to its stdin (the pump writes from its goroutine while the
// test reads after). Non-blocking, so replyExtension never deadlocks the pump.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// commands returns every JSONL command line decoded into maps.
func (s *syncBuffer) commands() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, ln := range strings.Split(s.buf.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// event marshals a pi event to a JSONL line.
func event(fields map[string]any) string {
	b, _ := json.Marshal(fields)
	return string(b) + "\n"
}

// handshake builds a valid handshake event with the real embedded SHA.
func handshakeEvent(reqID string) string {
	return event(map[string]any{
		"type": "extension_ui_request",
		"id":   reqID,
		"request": map[string]any{
			"type":         handshakeType,
			"nonce":        "ext-nonce-1",
			"extensionSHA": extensionSHA(),
		},
	})
}

// spawnScripted spawns a pi session over an io.Pipe stdout the test controls,
// so the child cannot race to EOF before Spawn sends the prompt (a real child
// would not emit agent_end before receiving the prompt). preSpawn is written
// and must be fully consumed before Spawn's handshake gate resolves; body is
// written after Spawn returns, then the stream is closed. Every command the
// handle writes is captured.
func spawnScripted(t *testing.T, spec agent.Spec, preSpawn, body string) (*syncBuffer, agent.Handle, error) {
	t.Helper()
	if spec.Cwd == "" {
		spec.Cwd = t.TempDir()
	}
	cmds := &syncBuffer{}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    cmds,
		stdoutOverride:   pr,
		HandshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// preSpawn (typically the handshake) is delivered while Spawn blocks on the
	// handshake gate.
	go func() { _, _ = io.WriteString(pw, preSpawn) }()
	h, err := p.Spawn(context.Background(), spec)
	if err != nil {
		// On fail-closed the caller inspects cmds; close the stream so any
		// pending reader unwinds.
		_ = pw.Close()
		return cmds, h, err
	}
	// Deliver the rest of the session. The stream is NOT closed here (a real
	// child keeps stdout open across the session); t.Cleanup closes it. The
	// pump terminates on the body's terminal event, not on EOF, so leaving the
	// write side open keeps the client live for in-session extension replies.
	go func() { _, _ = io.WriteString(pw, body) }()
	return cmds, h, err
}

// drain reads every event from h until the channel closes (bounded by t
// timeout).
func drain(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("timed out draining events; got %d so far", len(out))
		}
	}
}

// --- Smoke 1: spawn + handshake verified; tampered SHA fails closed ---

func TestSpawn_HandshakeVerified(t *testing.T) {
	t.Parallel()
	body := event(map[string]any{"type": "agent_start", "sessionId": "ses_ok"}) +
		event(map[string]any{"type": "agent_end", "success": true})
	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn should succeed on a verified handshake: %v", err)
	}
	drain(t, h)
	// The handshake was acknowledged AND the prompt was sent only AFTER it.
	got := cmds.commands()
	var sawHandshakeAck, sawPrompt bool
	var ackIdx, promptIdx int
	for i, c := range got {
		if c["command"] == "extension_ui_response" {
			sawHandshakeAck = true
			ackIdx = i
		}
		if c["command"] == "prompt" {
			sawPrompt = true
			promptIdx = i
		}
	}
	if !sawHandshakeAck || !sawPrompt {
		t.Fatalf("expected a handshake ack and a prompt command, got %v", got)
	}
	if ackIdx > promptIdx {
		t.Errorf("prompt was sent BEFORE the handshake ack — fail-closed ordering violated")
	}
}

func TestSpawn_TamperedExtensionFailsClosed(t *testing.T) {
	t.Parallel()
	// Handshake with a WRONG SHA — the materialized extension is not ours.
	tampered := event(map[string]any{
		"type": "extension_ui_request",
		"id":   "h1",
		"request": map[string]any{
			"type":         handshakeType,
			"nonce":        "x",
			"extensionSHA": strings.Repeat("a", 64),
		},
	})

	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, tampered, "")
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("tampered handshake must fail spawn with ErrSpawnFailed, got %v", err)
	}
	if h != nil {
		t.Errorf("Spawn must return a nil Handle on fail-closed")
	}
	// The prompt must NEVER have been sent.
	for _, c := range cmds.commands() {
		if c["command"] == "prompt" {
			t.Errorf("prompt was sent despite a tampered extension — trust boundary breached")
		}
	}
}

// --- Smoke 2: no extension materialized ⇒ no handshake ⇒ fail closed ---

func TestSpawn_NoHandshakeFailsClosed(t *testing.T) {
	t.Parallel()
	// stdout emits session events but NEVER a handshake.
	noHandshake := event(map[string]any{"type": "agent_start", "sessionId": "ses"}) +
		event(map[string]any{"type": "agent_end", "success": true})
	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, noHandshake, "")
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("missing handshake must fail spawn closed, got err=%v", err)
	}
	if h != nil {
		t.Errorf("Spawn must return nil Handle when the handshake never arrives")
	}
	for _, c := range cmds.commands() {
		if c["command"] == "prompt" {
			t.Errorf("prompt sent without a verified policy extension — trust boundary breached")
		}
	}
}

// --- Smoke 3: prompt → event stream shape ---

func TestSpawn_EventStreamShape(t *testing.T) {
	t.Parallel()
	body := event(map[string]any{"type": "agent_start", "sessionId": "ses_stream"}) +
		event(map[string]any{"type": "message_update", "text": "Hello "}) +
		event(map[string]any{"type": "message_update", "text": "world"}) +
		event(map[string]any{"type": "message_update", "part": "thinking", "text": "(secret reasoning)"}) +
		event(map[string]any{"type": "message_end"}) +
		event(map[string]any{"type": "agent_end", "success": true})

	_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)

	inits, texts, results := 0, 0, 0
	var text string
	for _, e := range evs {
		switch ev := e.(type) {
		case agent.InitEvent:
			inits++
		case agent.AssistantTextEvent:
			texts++
			text = ev.Text
		case agent.ResultEvent:
			results++
		}
	}
	if inits != 1 {
		t.Errorf("want exactly 1 InitEvent, got %d", inits)
	}
	if texts != 1 || text != "Hello world" {
		t.Errorf("want 1 buffered AssistantText 'Hello world', got %d %q (thinking must be dropped)", texts, text)
	}
	if results != 1 {
		t.Errorf("want exactly 1 terminal ResultEvent, got %d", results)
	}
	if err := checkTerminalLast(evs); err != nil {
		t.Error(err)
	}
	// SessionID resolved from agent_start.
	if h.SessionID() != "ses_stream" {
		t.Errorf("SessionID = %q, want ses_stream", h.SessionID())
	}
}

// --- Smoke 4: permission-denial round-trip through the pump ---

func TestSpawn_PermissionDenialRoundTrip(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	// Two adjudication requests: a dangerous bash and an out-of-tree write.
	adjudicate := func(reqID, callID string, call map[string]any) string {
		call["callId"] = callID
		return event(map[string]any{
			"type": "extension_ui_request",
			"id":   reqID,
			"request": map[string]any{
				"type":  adjudicateType,
				"nonce": "ext-nonce-1",
				"call":  call,
			},
		})
	}
	body := event(map[string]any{"type": "agent_start", "sessionId": "ses_deny"}) +
		adjudicate("r1", "c1", map[string]any{"tool": "bash", "command": "rm -rf /"}) +
		adjudicate("r2", "c2", map[string]any{"tool": "write", "path": "/etc/passwd"}) +
		event(map[string]any{"type": "agent_end", "success": true})

	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: cwd, Autonomous: true}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)

	// Both replies must be deny, carrying a visible reason (the model sees it).
	denies := 0
	for _, c := range cmds.commands() {
		if c["command"] != "extension_ui_response" {
			continue
		}
		resp, _ := c["response"].(map[string]any)
		if resp == nil {
			continue
		}
		if allow, ok := resp["allow"].(bool); ok && !allow {
			denies++
			if reason, _ := resp["reason"].(string); reason == "" {
				t.Errorf("deny reply carried no reason — the model cannot see why")
			}
		}
	}
	if denies != 2 {
		t.Errorf("want 2 deny replies (rm -rf and out-of-tree write), got %d", denies)
	}
	// Two permission_decision SystemEvents recorded.
	decisions := 0
	for _, e := range evs {
		if se, ok := e.(agent.SystemEvent); ok && se.Subtype == "permission_decision" {
			decisions++
		}
	}
	if decisions != 2 {
		t.Errorf("want 2 permission_decision SystemEvents, got %d", decisions)
	}
}

// --- Smoke 5: bypass monitor ---

func TestSpawn_BypassMonitorAborts(t *testing.T) {
	t.Parallel()
	// A built-in tool_execution_start with NO preceding adjudication.
	body := event(map[string]any{"type": "agent_start", "sessionId": "ses_bypass"}) +
		event(map[string]any{"type": "tool_execution_start", "tool": "bash", "callId": "c-unadjudicated"}) +
		event(map[string]any{"type": "agent_end", "success": true})

	_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)

	var sawBypass bool
	for _, e := range evs {
		if ee, ok := e.(agent.ErrorEvent); ok && ee.Code == "policy_extension_failed" {
			sawBypass = true
		}
		// The agent_end that followed must NOT have produced a ResultEvent:
		// the session aborts at the bypass.
		if _, ok := e.(agent.ResultEvent); ok {
			t.Errorf("session emitted a ResultEvent after a policy bypass — must abort instead")
		}
	}
	if !sawBypass {
		t.Errorf("bypass monitor did not abort on an unadjudicated built-in tool execution")
	}
	if err := checkTerminalLast(evs); err != nil {
		t.Error(err)
	}
}

// checkTerminalLast asserts exactly one terminal event, and it is last.
func checkTerminalLast(evs []agent.Event) error {
	term, idx := 0, -1
	for i, e := range evs {
		switch e.(type) {
		case agent.ResultEvent, agent.ErrorEvent:
			term++
			idx = i
		}
	}
	if term != 1 {
		return fmt.Errorf("want exactly 1 terminal event, got %d", term)
	}
	if idx != len(evs)-1 {
		return fmt.Errorf("terminal event at index %d of %d is not last", idx, len(evs))
	}
	return nil
}
