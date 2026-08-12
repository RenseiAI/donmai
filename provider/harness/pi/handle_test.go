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
	cmds, h, _, err := spawnScriptedLive(t, spec, preSpawn, body)
	return cmds, h, err
}

// spawnScriptedLive is spawnScripted's sibling for tests that need to keep
// writing to the scripted stdout AFTER Spawn returns — e.g. to prove a
// post-terminal Inject drives a real follow-up turn's events onto the same
// channel. It returns the pipe writer so the caller can send more scripted
// lines at will; t.Cleanup still closes it.
func spawnScriptedLive(t *testing.T, spec agent.Spec, preSpawn, body string) (*syncBuffer, agent.Handle, *io.PipeWriter, error) {
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
		return cmds, h, pw, err
	}
	// Deliver the rest of the session. The stream is NOT closed here (a real
	// child keeps stdout open across the session); t.Cleanup closes it. A
	// body ending in a FATAL event (a policy-bypass abort, extension_error)
	// makes the pump exit on its own; a body ending in an ordinary completed
	// turn (agent_settled) does NOT — the pump stays up past it (so a later
	// Handle.Inject still has somewhere to land) until t.Cleanup's EOF or an
	// explicit Stop().
	go func() { _, _ = io.WriteString(pw, body) }()
	return cmds, h, pw, err
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

// drain reads events from h until the first terminal event (ResultEvent or
// ErrorEvent) or the channel closes, whichever comes first (bounded by t
// timeout). It mirrors the production consumer, runner.consumeEvents, which
// returns as soon as it observes a ResultEvent rather than waiting for the
// channel to also close — that distinction matters here because an ordinary
// completed turn (agent_settled) no longer closes the channel on its own
// (handle.go's dispatch/run keep the pump alive so a later Handle.Inject has
// somewhere to land); only a fatal abort or Stop does. A fatal-terminal body
// still closes the channel right after its ErrorEvent, so this helper
// returns the same way it always did for those cases.
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
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return out
			}
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

// --- Post-terminal Inject: soft (agent_settled) keeps the pump alive and
// routes to prompt; fatal (policy bypass / extension_error) still fails
// closed. This is the regression coverage for the fix: runner/loop.go's
// drainMemoryInjects and runner/steering.go's attemptSteering both call
// Inject at exactly the post-terminal seam these scripted transcripts drive.

// TestInject_AfterSoftTerminal_RoutesPromptAndDrivesFollowUp proves that
// injecting after an ordinary completed turn (agent_settled) succeeds, is
// sent as a `prompt` command — not `follow_up`, which docs/rpc.md says pi
// will never auto-drain once settled — and that the pump keeps consuming the
// RPC stream so the follow-up turn's own events (including a second terminal
// ResultEvent) reach the caller on the SAME channel.
func TestInject_AfterSoftTerminal_RoutesPromptAndDrivesFollowUp(t *testing.T) {
	t.Parallel()
	firstTurn := getStateResponse("ses_followup") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	cmds, h, pw, err := spawnScriptedLive(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), firstTurn)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	first := drain(t, h)
	if err := checkTerminalLast(first); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	if err := h.Inject(context.Background(), "keep going"); err != nil {
		t.Fatalf("Inject after agent_settled: %v", err)
	}

	followUpTurn := event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "done"}}) +
		event(map[string]any{"type": "message_end"}) +
		event(map[string]any{"type": "agent_settled"})
	go func() { _, _ = io.WriteString(pw, followUpTurn) }()

	second := drain(t, h)
	if err := checkTerminalLast(second); err != nil {
		t.Fatalf("follow-up turn: %v", err)
	}
	var sawFollowUpText bool
	for _, e := range second {
		if at, ok := e.(agent.AssistantTextEvent); ok && at.Text == "done" {
			sawFollowUpText = true
		}
	}
	if !sawFollowUpText {
		t.Errorf("follow-up turn's events never reached the caller; got %+v", second)
	}

	var sawPrompt, sawFollowUpCmd bool
	for _, c := range cmds.commands() {
		if c["type"] == "prompt" && c["message"] == "keep going" {
			sawPrompt = true
		}
		if c["type"] == "follow_up" {
			sawFollowUpCmd = true
		}
	}
	if !sawPrompt {
		t.Errorf("expected a prompt command carrying %q after agent_settled, got %v", "keep going", cmds.commands())
	}
	if sawFollowUpCmd {
		t.Errorf("post-settled Inject must route to prompt, not follow_up — pi does not auto-drain a follow_up once settled (docs/rpc.md agent_settled)")
	}
}

// TestInject_AfterFatalTerminal_FailsClosed is the table-driven negative
// half: unlike an ordinary completed turn, a FATAL terminal (the trust
// boundary aborting the session) must still refuse Inject — there is no live
// session left for steer/follow_up/prompt to land on.
func TestInject_AfterFatalTerminal_FailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{
			name: "policy bypass abort",
			body: getStateResponse("ses_fatal_bypass") +
				event(map[string]any{"type": "agent_start"}) +
				event(map[string]any{"type": "tool_execution_end", "toolName": "bash", "toolCallId": "c-unadjudicated", "isError": false}),
		},
		{
			name: "extension_error abort",
			body: getStateResponse("ses_fatal_ext") +
				event(map[string]any{"type": "agent_start"}) +
				event(map[string]any{"type": "extension_error", "error": "donmai policy extension threw"}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi", Cwd: t.TempDir(), Autonomous: true},
				handshakeEvent("h1"), tc.body)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			evs := drain(t, h)
			var sawFatal bool
			for _, e := range evs {
				if _, ok := e.(agent.ErrorEvent); ok {
					sawFatal = true
				}
			}
			if !sawFatal {
				t.Fatalf("expected a fatal ErrorEvent, got %+v", evs)
			}
			if err := h.Inject(context.Background(), "should not land"); err == nil {
				t.Error("Inject after a fatal terminal returned nil error; want a closed-session error")
			}
		})
	}
}

// --- Wake-poll fail-quiet invariant ---
//
// runner/loop.go's drainMemoryInjects and runner/steering.go's attemptSteering
// are the "wake-poll" call sites: a background poll (the heartbeat transport's
// buffered memory injects) or a post-terminal check (steering) wakes a
// completed-but-still-live session by calling Handle.Inject. Both route
// through runner.injectDirective, whose documented contract (steering.go) is
// "soft-fail": ErrUnsupported / clijsonl.ErrSessionNotReady /
// clijsonl.ErrInjectInFlight are swallowed as benign, and — critically for the
// invariant this test pins — ANY OTHER error is returned to the caller rather
// than retried in a loop; drainMemoryInjects's own handling of that returned
// error is to log once and stop draining, deferring the remaining buffered
// blocks to the NEXT heartbeat cycle (loop.go: "the remaining blocks ride the
// next heartbeat re-delivery"). That is this architecture's bounded-retry-
// then-signal shape (013-orchestrator-and-governor.md: "An unreachable
// session does not hold capacity indefinitely... bounded-retry-then-signal")
// applied to steering/memory-inject: no tight retry loop, no hang, a signal
// (the logged Warn) instead of silence, and forward progress via the next
// scheduled wake rather than an immediate re-attempt.
//
// pi has no wake-poll of its own (it is subprocess-push, not poll-driven —
// see doc.go's "Scale hardening" section), so the invariant this package owns
// is narrower and mechanical: Handle.Inject called after Stop (the runner's
// deterministic backstop, or a ctx-cancel teardown, can race a wake-poll
// call landing at the same seam described above) must return QUICKLY with a
// plain, non-panicking error — never hang, never block past a bounded
// window — so the generic runner-level fail-quiet contract has something
// well-behaved to wrap.

// TestInject_AfterStop_ReturnsPromptlyAndDoesNotHang proves Handle.Inject on
// an ALREADY-STOPPED (not merely fatally-terminated) session returns within a
// bounded window rather than hanging — the property the wake-poll fail-quiet
// contract depends on. Unlike TestInject_AfterFatalTerminal_FailsClosed
// (which pins the trust-boundary abort path), this drives the ordinary,
// non-fatal shutdown path: a healthy, settled session that Stop() then tears
// down out from under a racing wake-poll caller.
func TestInject_AfterStop_ReturnsPromptlyAndDoesNotHang(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_wake_poll") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drain(t, h)
	// A short-deadline ctx (rather than context.Background()) makes Stop
	// return via its ctx.Done() branch instead of waiting out the full
	// abortGrace: nothing in this scripted, no-real-process fixture will ever
	// close h.closed on its own (the stubbed stdout pipe stays open until
	// t.Cleanup), so an unbounded Stop call here would otherwise block for
	// the full abort grace window on every run for no correctness reason.
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.Inject(context.Background(), "wake-poll racing Stop") }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Inject after Stop returned nil error; want a closed-session error so the runner's fail-quiet wrapper has something to log-and-defer on")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Inject after Stop did not return within 2s — a wake-poll caller (runner/loop.go drainMemoryInjects, runner/steering.go attemptSteering) would hang instead of deferring to the next scheduled wake")
	}
}

// --- Cleanup idempotence (D8 pi row: "cleanup is idempotent and evidenced") ---

// TestStop_IdempotentAfterChannelClose is the "cleanup idempotence" fixture:
// Handle.Stop's doc comment claims "Idempotent", and the runner relies on
// that. A completed turn (agent_settled) no longer closes the events channel
// on its own — the pump stays up past it so a later Handle.Inject still has
// somewhere to land (see handle.go's dispatch/run) — so the FIRST Stop call
// below is the one doing the real teardown here, not a race against the
// pump's own terminal-triggered close. The second and third prove
// idempotency on top of that, including the case with the least left to do
// and the most latent risk: a second close(channel) panicking if the
// internal guards ever regress.
func TestStop_IdempotentAfterChannelClose(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_cleanup") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	_, h, err := spawnScripted(t, agent.Spec{Prompt: "hi"}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drain(t, h) // drains to the turn's terminal ResultEvent; the pump is still alive

	for i := 1; i <= 3; i++ {
		if err := h.Stop(context.Background()); err != nil {
			t.Errorf("Stop call %d = %v, want nil (Idempotent per the doc comment)", i, err)
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
