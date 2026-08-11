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

// testHandshakeToken is the per-session token the pipe-stub tests pin (via
// Options.handshakeToken) so a scripted handshake fixture can echo the exact
// value the handle expects. A real session generates a random token per Spawn
// and sets it in the child env (piHandshakeEnvVar).
const testHandshakeToken = "test-session-token-0123456789ab"

// syncBuffer is a concurrency-safe io.Writer that captures every command the
// handle writes to its stdin (the pump writes from its goroutine while the
// test reads after). Non-blocking, so the extension replies never deadlock the
// pump.
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

// uiRequest builds a real extension_ui_request (method:"input") carrying the
// donmai marker placeholder and a JSON payload in `title` — the exact shape the
// policy extension emits over `ctx.ui.input(payload, DONMAI_UI_MARKER)`.
func uiRequest(reqID string, payload map[string]any) string {
	title, _ := json.Marshal(payload)
	return event(map[string]any{
		"type":        "extension_ui_request",
		"id":          reqID,
		"method":      "input",
		"title":       string(title),
		"placeholder": donmaiUIMarker,
	})
}

// handshakeEvent builds a valid handshake request with the pinned test token and
// the real embedded SHA.
func handshakeEvent(reqID string) string {
	return uiRequest(reqID, map[string]any{
		"donmai": handshakeKind,
		"token":  testHandshakeToken,
		"sha":    extensionSHA(),
	})
}

// adjudicateEvent builds an adjudication request for one tool call.
func adjudicateEvent(reqID, toolName, toolCallID string, input map[string]any, cwd string) string {
	return uiRequest(reqID, map[string]any{
		"donmai":     adjudicateKind,
		"token":      testHandshakeToken,
		"toolName":   toolName,
		"toolCallId": toolCallID,
		"input":      input,
		"cwd":        cwd,
	})
}

// getStateResponse builds a real get_state command response carrying sessionId.
func getStateResponse(sessionID string) string {
	return event(map[string]any{
		"type":    "response",
		"command": "get_state",
		"success": true,
		"data":    map[string]any{"sessionId": sessionID},
	})
}

// spawnScripted spawns a pi session over an io.Pipe stdout the test controls,
// so the child cannot race to EOF before Spawn sends the prompt. preSpawn is
// written and must be consumed before Spawn's handshake gate resolves; body is
// written after Spawn returns, then the stream is closed by t.Cleanup. Every
// command the handle writes is captured.
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
		handshakeToken:   testHandshakeToken,
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
		_ = pw.Close()
		return cmds, h, err
	}
	// Deliver the rest of the session. The stream is NOT closed here (a real
	// child keeps stdout open across the session); t.Cleanup closes it. The
	// pump terminates on the body's terminal event, not on EOF.
	go func() { _, _ = io.WriteString(pw, body) }()
	return cmds, h, err
}

// resumeScripted mirrors spawnScripted but drives Resume(sessionID, spec)
// instead of Spawn — the replay/resume half of the D8 pi fixture family.
func resumeScripted(t *testing.T, sessionID string, spec agent.Spec, preSpawn, body string) (*syncBuffer, agent.Handle, error) {
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
		handshakeToken:   testHandshakeToken,
		HandshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() { _, _ = io.WriteString(pw, preSpawn) }()
	h, err := p.Resume(context.Background(), sessionID, spec)
	if err != nil {
		_ = pw.Close()
		return cmds, h, err
	}
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

// --- Smoke 1 / D8 "RPC policy-handshake boundary" fixture family (positive +
// tampered/forged negatives): spawn + handshake verified; tampered SHA or
// forged token fails closed. See doc.go's "D8 fixture family" section for
// the full family roster across files. ---

func TestSpawn_HandshakeVerified(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_ok") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
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
		if c["type"] == "extension_ui_response" && c["value"] == "ok" {
			sawHandshakeAck = true
			ackIdx = i
		}
		if c["type"] == "prompt" {
			sawPrompt = true
			promptIdx = i
			if msg, _ := c["message"].(string); msg != "hi" {
				t.Errorf("prompt message = %q, want the real `message` field carrying the prompt", msg)
			}
		}
	}
	if !sawHandshakeAck || !sawPrompt {
		t.Fatalf("expected a handshake ack (value:ok) and a prompt command, got %v", got)
	}
	if ackIdx > promptIdx {
		t.Errorf("prompt was sent BEFORE the handshake ack — fail-closed ordering violated")
	}
}

func TestSpawn_TamperedExtensionFailsClosed(t *testing.T) {
	t.Parallel()
	// Handshake with a WRONG SHA (but the right token) — the materialized
	// extension bytes are not ours.
	tampered := uiRequest("h1", map[string]any{
		"donmai": handshakeKind,
		"token":  testHandshakeToken,
		"sha":    strings.Repeat("a", 64),
	})
	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, tampered, "")
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("tampered handshake must fail spawn with ErrSpawnFailed, got %v", err)
	}
	if h != nil {
		t.Errorf("Spawn must return a nil Handle on fail-closed")
	}
	for _, c := range cmds.commands() {
		if c["type"] == "prompt" {
			t.Errorf("prompt was sent despite a tampered extension — trust boundary breached")
		}
	}
}

func TestSpawn_ForgedTokenFailsClosed(t *testing.T) {
	t.Parallel()
	// Right SHA, WRONG token — a foreign extension that copied our source but
	// cannot know the per-session token the harness set in the child env.
	forged := uiRequest("h1", map[string]any{
		"donmai": handshakeKind,
		"token":  "not-the-session-token",
		"sha":    extensionSHA(),
	})
	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, forged, "")
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("forged-token handshake must fail spawn closed, got %v", err)
	}
	if h != nil {
		t.Errorf("Spawn must return nil Handle on a forged token")
	}
	for _, c := range cmds.commands() {
		if c["type"] == "prompt" {
			t.Errorf("prompt sent despite a forged handshake token — trust boundary breached")
		}
	}
}

// --- Smoke 2: no extension materialized ⇒ no handshake ⇒ fail closed ---

func TestSpawn_NoHandshakeFailsClosed(t *testing.T) {
	t.Parallel()
	// stdout emits session events but NEVER a handshake.
	noHandshake := event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, noHandshake, "")
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("missing handshake must fail spawn closed, got err=%v", err)
	}
	if h != nil {
		t.Errorf("Spawn must return nil Handle when the handshake never arrives")
	}
	for _, c := range cmds.commands() {
		if c["type"] == "prompt" {
			t.Errorf("prompt sent without a verified policy extension — trust boundary breached")
		}
	}
}

// --- Smoke 3: prompt → event stream shape ---

func TestSpawn_EventStreamShape(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_stream") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "Hello "}}) +
		event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "world"}}) +
		event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "(secret reasoning)"}}) +
		event(map[string]any{"type": "message_end"}) +
		event(map[string]any{"type": "agent_end", "willRetry": false}) +
		event(map[string]any{"type": "agent_settled"})

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
	// SessionID resolved from the get_state response.
	if h.SessionID() != "ses_stream" {
		t.Errorf("SessionID = %q, want ses_stream", h.SessionID())
	}
}

// --- Smoke 4: permission-denial round-trip through the pump ---

func TestSpawn_PermissionDenialRoundTrip(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	// Two adjudication requests: a dangerous bash and an out-of-tree write.
	body := getStateResponse("ses_deny") +
		event(map[string]any{"type": "agent_start"}) +
		adjudicateEvent("r1", "bash", "c1", map[string]any{"command": "rm -rf /"}, cwd) +
		adjudicateEvent("r2", "write", "c2", map[string]any{"path": "/etc/passwd"}, cwd) +
		event(map[string]any{"type": "agent_settled"})

	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: cwd, Autonomous: true}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)

	// Both replies must be deny, carrying a visible reason (the model sees it)
	// in the top-level `value` decision JSON.
	denies := 0
	for _, c := range cmds.commands() {
		if c["type"] != "extension_ui_response" {
			continue
		}
		val, _ := c["value"].(string)
		if val == "" {
			continue
		}
		var d struct {
			Allow  bool   `json:"allow"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(val), &d) != nil {
			continue
		}
		if !d.Allow {
			denies++
			if d.Reason == "" {
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
	// A built-in tool_execution_END with NO preceding adjudication round-trip.
	// (tool_execution_end — not _start — is the real bypass check point.)
	body := getStateResponse("ses_bypass") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "tool_execution_end", "toolName": "bash", "toolCallId": "c-unadjudicated", "isError": false}) +
		event(map[string]any{"type": "agent_settled"})

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

// --- Smoke 6 (unit half): Inject routes steer vs follow_up ---

func TestInject_SteerWhenInFlight_FollowUpWhenIdle(t *testing.T) {
	t.Parallel()
	cmds, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"},
		handshakeEvent("h1"),
		getStateResponse("ses_inject")+event(map[string]any{"type": "agent_start"}))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ph := h.(*Handle)

	ph.turnInFlight.Store(true)
	if err := h.Inject(context.Background(), "steer me"); err != nil {
		t.Fatalf("Inject (in flight): %v", err)
	}
	ph.turnInFlight.Store(false)
	if err := h.Inject(context.Background(), "follow me"); err != nil {
		t.Fatalf("Inject (idle): %v", err)
	}
	_ = h.Stop(context.Background())

	var sawSteer, sawFollowUp bool
	for _, c := range cmds.commands() {
		switch c["type"] {
		case "steer":
			if c["message"] == "steer me" {
				sawSteer = true
			}
		case "follow_up":
			if c["message"] == "follow me" {
				sawFollowUp = true
			}
		}
	}
	if !sawSteer {
		t.Errorf("in-flight Inject did not map to a steer command with the real `message` field")
	}
	if !sawFollowUp {
		t.Errorf("idle Inject did not map to a follow_up command with the real `message` field")
	}
}

// --- Cleanup idempotence (D8 pi row: "cleanup is idempotent and evidenced") ---

// TestStop_IdempotentAfterChannelClose is the "cleanup idempotence" fixture:
// Handle.Stop's doc comment claims "Idempotent", and the runner relies on
// that (a caller may race Stop against the pump's own terminal-triggered
// teardown). This proves it against a session that already reached its
// terminal event and closed the events channel on its own — the case where
// a second Stop has the least left to do and the most latent risk (a second
// close(channel) panics if the internal guards ever regress).
func TestStop_IdempotentAfterChannelClose(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_cleanup") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drain(t, h) // drains to the channel's own close, ahead of any Stop call

	for i := 1; i <= 3; i++ {
		if err := h.Stop(context.Background()); err != nil {
			t.Errorf("Stop call %d after channel close = %v, want nil (Idempotent per the doc comment)", i, err)
		}
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
