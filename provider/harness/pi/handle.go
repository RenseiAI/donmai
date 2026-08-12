package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Compile-time assertion: Handle satisfies agent.Handle.
var _ agent.Handle = (*Handle)(nil)

const (
	// abortGrace is how long Stop waits for a clean agent_settled after sending
	// abort before escalating to signals (design §2).
	abortGrace = 2 * time.Second
	// termGrace is the SIGTERM→SIGKILL grace window (identical to
	// opencode/claude/codex).
	termGrace = 5 * time.Second
)

// Handle is one live pi session: one `pi --mode rpc` child, one rpcClient, one
// PolicyEngine. It implements agent.Handle.
//
// The event pump (run) is the runtime half of the trust boundary: it
// intercepts extension_ui_request events for handshake verification and tool
// adjudication BEFORE they reach the event mapper, and runs the bypass monitor
// on every built-in tool_execution_end (design §5.3). tool_execution_end — not
// _start — is the bypass point because the real pi lifecycle emits
// tool_execution_start BEFORE the tool_call hook that our adjudication rides,
// then executes the tool, then emits tool_execution_end; by then a legitimate
// call has completed its adjudication round-trip (verified against the real
// binary).
type Handle struct {
	client *rpcClient
	cmd    *exec.Cmd
	policy *PolicyEngine
	spec   agent.Spec
	state  *mapperState

	// token is the per-session handshake secret the harness set in the child
	// env (piHandshakeEnvVar). The policy extension echoes it on every
	// round-trip; the handle rejects any request whose token does not match.
	token string

	// handshakeResult delivers exactly one verdict to Spawn: nil = verified,
	// err = failed/mismatch. Buffered so the pump never blocks on it.
	handshakeResult chan error
	handshakeOnce   sync.Once

	// adjudicated records the pi tool call ids that completed a policy
	// round-trip, so the bypass monitor can flag a built-in tool_execution_end
	// that arrived without one.
	adjMu       sync.Mutex
	adjudicated map[string]bool

	// turnInFlight is true between the first streaming event of a turn and its
	// turn_end/agent_end — Inject routes to steer while in flight, follow_up
	// while idle.
	turnInFlight atomic.Bool

	// settled mirrors mapperState.settled for cross-goroutine reads: true
	// once agent_settled has been processed (mutated only from dispatch, on
	// h.run's goroutine, right after mapEvent updates h.state.settled on the
	// same goroutine — this field is the atomic copy Inject reads from a
	// different goroutine). Per docs/rpc.md, agent_settled means "Pi will not
	// continue automatically through ... queued follow-up messages" — so once
	// settled, Inject must send a fresh `prompt` (starts a new low-level run
	// within the SAME session) rather than `follow_up` (which would queue
	// into a run that no longer exists and never be delivered).
	settled atomic.Bool

	idMu      sync.RWMutex
	sessionID string // resolved from the get_state response; guarded by idMu

	// Close protocol (same locking discipline as codex/handle.go: eventsMu +
	// eventsClosed gate every send, closeEvents is the single idempotent
	// closer, h.closed broadcasts teardown to unblock a slow send) — but a
	// DIFFERENT trigger: codex closes on every terminal event, pi closes
	// only on a fatal one (see dispatch/run). An ordinary completed turn
	// leaves both channels open so a later Handle.Inject still has a live
	// pump to deliver into; only Stop, a fatal abort, or the underlying
	// stream's own EOF actually close them.
	events       chan agent.Event
	closeOnce    sync.Once
	closedOnce   sync.Once
	closed       chan struct{}
	eventsMu     sync.RWMutex
	eventsClosed atomic.Bool
}

func newHandle(client *rpcClient, cmd *exec.Cmd, spec agent.Spec, token string) *Handle {
	return &Handle{
		client:          client,
		cmd:             cmd,
		policy:          NewPolicyEngine(spec),
		spec:            spec,
		state:           &mapperState{},
		token:           token,
		handshakeResult: make(chan error, 1),
		adjudicated:     make(map[string]bool),
		events:          make(chan agent.Event, 256),
		closed:          make(chan struct{}),
	}
}

// SessionID returns the pi session id once the get_state response has resolved
// it.
func (h *Handle) SessionID() string {
	h.idMu.RLock()
	defer h.idMu.RUnlock()
	return h.sessionID
}

// Events returns the read-only event channel. Closed once the session is
// truly over: a fatal abort (handshake/policy failure, a policy-bypass
// abort, or an unrecoverable stream error) closes it immediately; an
// ordinary completed turn (agent_settled) does NOT — the pump stays up so a
// later Handle.Inject can still land and drive a follow-up turn. Stop
// always closes it, whichever path got there first.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject sends a follow-up user message, routed by session state (design §7,
// corrected against docs/rpc.md's agent_settled semantics — see the settled
// field's comment):
//
//   - Turn in flight (turnInFlight) → "steer": delivered after the current
//     assistant turn finishes its tool calls, before the next LLM call.
//   - Idle but not yet settled (a run is active server-side even though this
//     Handle has not observed a streaming event yet — e.g. immediately after
//     Spawn, before the first event arrives) → "follow_up": queued and
//     delivered once that active run has no more tool calls or steering
//     messages.
//   - Already settled (agent_settled observed) → "prompt": pi's own docs are
//     explicit that a settled run does NOT automatically drain a queued
//     follow_up ("Pi will not continue automatically through ... queued
//     follow-up messages"), so a follow_up sent here would sit in the queue
//     forever. A fresh `prompt` command starts a new low-level agent run
//     within the SAME RPC session (conversation history intact) — unlike
//     new_session, which would discard it — so it is the correct way to
//     resume a session the runner wants to steer or memory-inject into after
//     it has already completed a turn (runner/loop.go's drainMemoryInjects
//     and runner/steering.go's attemptSteering both call Inject at exactly
//     this post-terminal seam).
//
// The queue_update / turn_start / ... events pi emits in response are
// surfaced as SystemEvents by the pump.
//
// Injecting after a FATAL close (handshake/policy failure, a policy-bypass
// abort, or Stop) fails with "session closed": there is no live session left
// to send any of the three commands to.
func (h *Handle) Inject(_ context.Context, text string) error {
	select {
	case <-h.closed:
		return fmt.Errorf("pi: session closed")
	default:
	}
	cmd := "follow_up"
	switch {
	case h.turnInFlight.Load():
		cmd = "steer"
	case h.settled.Load():
		cmd = "prompt"
	}
	return h.client.WriteCommand(map[string]any{"type": cmd, "message": text})
}

// Stop aborts the session: send abort, wait ≤abortGrace for a clean settle,
// then SIGTERM the process group → termGrace → SIGKILL (design §2). Idempotent.
func (h *Handle) Stop(ctx context.Context) error {
	h.closeOnce.Do(func() {
		_ = h.client.WriteCommand(map[string]any{"type": "abort"})
		// Give pi a moment to settle and exit cleanly.
		select {
		case <-h.closed:
		case <-time.After(abortGrace):
		case <-ctx.Done():
		}
		h.killChild(ctx)
	})
	h.signalClosed()
	h.closeEvents()
	return nil
}

// killChild escalates SIGTERM→SIGKILL over the child's process group.
func (h *Handle) killChild(ctx context.Context) {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	signalProcessGroup(h.cmd, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = h.cmd.Process.Wait(); close(done) }()
	grace := termGrace
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < grace {
			grace = rem
		}
	}
	select {
	case <-done:
	case <-time.After(grace):
		signalProcessGroup(h.cmd, syscall.SIGKILL)
		<-done
	}
}

// run is the event pump. It consumes rawEvents from the client, routes
// extension requests to the trust boundary, maps the rest to agent.Events,
// and closes on a FATAL abort or EOF. Started by Spawn.
//
// A completed turn (agent_settled → ResultEvent) does NOT stop the pump: pi
// accepts a follow_up/steer command after a turn finishes and drives another
// one, so returning here would strand a later Handle.Inject with nothing
// listening on the RPC stream. dispatch distinguishes this ordinary,
// non-fatal terminal from a genuine abort (handshake/policy failure, a
// policy-bypass abort) — only the latter makes it return true. The loop
// then simply goes back to waiting on h.client.Events() for either the
// follow-up turn's events or h.closed (Stop, or the process exiting once
// idle).
func (h *Handle) run() {
	defer func() {
		// If the pump exits before the handshake resolved, that is itself a
		// failure (child died / stream closed pre-handshake) — unblock Spawn.
		h.resolveHandshake(fmt.Errorf("pi: event stream closed before handshake"))
		h.signalClosed()
		h.closeEvents()
	}()

	for {
		select {
		case <-h.closed:
			return
		case ev, ok := <-h.client.Events():
			if !ok {
				// Stream closed. If agent_settled already ran, its ResultEvent
				// was the session's one terminal — a later stream close (Stop's
				// teardown, or the child exiting once idle) carries no new
				// terminal information and must not produce a second one.
				if h.state.settled {
					return
				}
				// Otherwise: if the session cleanly reached a non-retrying
				// agent_end but the terminal agent_settled never arrived, emit a
				// clean ResultEvent; if nothing terminal was emitted at all,
				// surface a crash ErrorEvent so the runner sees the failure.
				// This whole branch is followed unconditionally by `return`
				// (the stream is already closed — there is no pump left to
				// come back to), so signalClosed() must precede whichever
				// emit() fires: the same happens-before argument as
				// dispatch()'s fatal paths applies here too, since the
				// deferred signalClosed() below would otherwise race a
				// caller reacting to this terminal event.
				h.signalClosed()
				if h.state.sawAgentEnd && h.state.endSuccess {
					h.emit(agent.ResultEvent{Success: true})
				} else if cause := h.client.CloseErr(); cause != nil {
					h.emit(agent.ErrorEvent{Message: "pi child stream error: " + cause.Error(), Code: "pi_crashed"})
				}
				return
			}
			if h.dispatch(ev) {
				return
			}
		}
	}
}

// dispatch handles one rawEvent. Returns true only when the session has
// reached a FATAL terminal (a policy bypass or a fatal ErrorEvent from the
// mapper — see mapEvent's extension_error case) and the pump must exit.
// An ordinary completed-turn terminal (agent_settled → ResultEvent) returns
// false: the caller (run) keeps the pump alive so a later Handle.Inject has
// somewhere to land. The fatal/non-fatal split is read off the emitted
// event's Kind() rather than mapEvent's own terminal bool, because that bool
// answers "is this turn/session over" for BOTH cases; only the emitted
// event's kind says whether it is safe to keep going.
func (h *Handle) dispatch(ev rawEvent) bool {
	// Extension requests are intercepted before the mapper.
	if ev.Type == "extension_ui_request" {
		h.handleExtensionRequest(ev)
		return false
	}

	// Bypass monitor: a built-in tool_execution_END MUST have completed a policy
	// round-trip for its call id (design §5.3). This is belt-and-braces — it
	// should be impossible when the policy extension is loaded — but it is the
	// fail-closed catch if the extension is subverted. tool_execution_start is
	// NOT the check point (the real lifecycle emits it before the tool_call hook
	// our adjudication rides).
	if ev.Type == "tool_execution_end" {
		tool := stringField(ev.Fields, "toolName", "tool", "name")
		callID := stringField(ev.Fields, "toolCallId", "callId", "call_id", "id")
		if isBuiltInTool(tool) && !h.wasAdjudicated(callID) {
			// signalClosed BEFORE emit: a caller that observes this event on
			// h.Events() and immediately calls Inject must find h.closed
			// already closed. Go's memory model only guarantees the SEND
			// half of a channel op happens-before the corresponding RECEIVE
			// completes — nothing about what the sender does afterward is
			// ordered against what the receiver does next. Closing h.closed
			// here, strictly before the emit() that makes this event
			// observable, puts the close on the happens-before side of that
			// edge instead of racing it (closeEvents() still runs later, in
			// run()'s deferred teardown — only the closed-signal needs to
			// lead the fatal event, not the channel close itself).
			h.signalClosed()
			h.emit(agent.ErrorEvent{
				Message: fmt.Sprintf("policy bypass: built-in tool %q (call %q) executed without a policy adjudication round-trip", tool, callID),
				Code:    "policy_extension_failed",
				Raw:     raw(ev),
			})
			return true // fatal: the trust boundary was violated, abort now.
		}
	}

	// Track turn-in-flight for Inject routing.
	switch ev.Type {
	case "message_update", "tool_execution_start", "turn_start":
		h.turnInFlight.Store(true)
	case "turn_end", "agent_end", "agent_settled":
		h.turnInFlight.Store(false)
	}

	out, _ := mapEvent(ev, h.state)
	// Keep SessionID() in sync with whatever the mapper resolved (get_state).
	if h.state.sessionID != "" {
		h.idMu.Lock()
		h.sessionID = h.state.sessionID
		h.idMu.Unlock()
	}
	// Mirror mapperState.settled (mutated only here, on this goroutine) onto
	// the atomic h.settled so Inject — called from a different goroutine —
	// can read it without racing h.state.
	if h.state.settled {
		h.settled.Store(true)
	}
	fatal := false
	for _, e := range out {
		// Same ordering rule as the bypass-monitor branch above: a fatal
		// event's h.signalClosed() must precede the h.emit() that makes it
		// observable, not follow it via run()'s deferred teardown — see that
		// branch's comment for the happens-before argument in full.
		if e.Kind() == agent.EventError {
			fatal = true
			h.signalClosed()
		}
		h.emit(e)
	}
	return fatal
}

// handleExtensionRequest verifies the handshake or adjudicates a tool call,
// replying over the RPC stream. This is the synchronous heart of the trust
// boundary. The real extension_ui_request shape (docs/rpc.md) is
// {type, id, method, title, placeholder, ...}; the policy extension stamps the
// donmaiUIMarker placeholder and puts a JSON payload in `title`.
func (h *Handle) handleExtensionRequest(ev rawEvent) {
	reqID := stringField(ev.Fields, "id")
	placeholder := stringField(ev.Fields, "placeholder")
	if placeholder != donmaiUIMarker {
		// Not one of our round-trips — cancel so the extension does not hang,
		// and surface it for observability. (For fire-and-forget methods with
		// no id this is a harmless no-op reply.)
		if reqID != "" {
			_ = h.replyExtensionCancelled(reqID)
		}
		h.emit(agent.SystemEvent{Subtype: "unhandled_extension_request", Message: stringField(ev.Fields, "method"), Raw: raw(ev)})
		return
	}

	var payload map[string]any
	if title := stringField(ev.Fields, "title"); title != "" {
		_ = json.Unmarshal([]byte(title), &payload)
	}
	switch stringField(payload, "donmai") {
	case handshakeKind:
		claimedToken := stringField(payload, "token")
		claimedSHA := stringField(payload, "sha")
		if !verifyHandshakeToken(claimedToken, h.token) || !verifyHandshakeSHA(claimedSHA) {
			// Token/SHA mismatch (or empty): the materialized extension is not
			// the one we shipped, or not the child we spawned. Fail closed — no
			// prompt, kill the session.
			_ = h.replyExtensionValue(reqID, "reject")
			h.emit(agent.ErrorEvent{
				Message: "pi policy extension handshake failed (token/SHA mismatch) — refusing to run unguarded",
				Code:    "policy_extension_failed",
				Raw:     raw(ev),
			})
			h.resolveHandshake(fmt.Errorf("%w: pi policy extension handshake token/SHA mismatch", agent.ErrSpawnFailed))
			h.signalClosed()
			return
		}
		_ = h.replyExtensionValue(reqID, "ok")
		h.resolveHandshake(nil)

	case adjudicateKind:
		// The token must match the handshake's — a request without the session
		// token is not from our verified extension.
		if !verifyHandshakeToken(stringField(payload, "token"), h.token) {
			_ = h.replyExtensionValue(reqID, mustDecisionJSON(Decision{Allow: false, Reason: "token mismatch"}))
			return
		}
		call := parseAdjudicateCall(payload, h.spec.Cwd)
		decision := h.policy.Evaluate(call)
		if callID := stringField(payload, "toolCallId"); callID != "" {
			h.markAdjudicated(callID)
		}
		_ = h.replyExtensionValue(reqID, mustDecisionJSON(decision))
		h.emit(agent.SystemEvent{
			Subtype: "permission_decision",
			Message: permissionMessage(call, decision),
			Raw:     raw(ev),
		})

	default:
		_ = h.replyExtensionCancelled(reqID)
		h.emit(agent.SystemEvent{Subtype: "unhandled_extension_request", Message: stringField(payload, "donmai"), Raw: raw(ev)})
	}
}

// replyExtensionValue writes an extension_ui_response carrying a top-level
// `value` (the real dialog-response shape for method:"input", docs/rpc.md).
func (h *Handle) replyExtensionValue(reqID, value string) error {
	return h.client.WriteCommand(map[string]any{
		"type":  "extension_ui_response",
		"id":    reqID,
		"value": value,
	})
}

// replyExtensionCancelled dismisses a dialog we do not handle.
func (h *Handle) replyExtensionCancelled(reqID string) error {
	return h.client.WriteCommand(map[string]any{
		"type":      "extension_ui_response",
		"id":        reqID,
		"cancelled": true,
	})
}

// mustDecisionJSON serializes a Decision as the compact JSON the extension
// parses from the adjudication `value` reply ({allow, reason}).
func mustDecisionJSON(d Decision) string {
	b, err := json.Marshal(map[string]any{"allow": d.Allow, "reason": d.Reason})
	if err != nil {
		return `{"allow":false,"reason":"decision encode error"}`
	}
	return string(b)
}

// resolveHandshake delivers the handshake verdict to Spawn exactly once.
func (h *Handle) resolveHandshake(err error) {
	h.handshakeOnce.Do(func() { h.handshakeResult <- err })
}

func (h *Handle) markAdjudicated(callID string) {
	h.adjMu.Lock()
	h.adjudicated[callID] = true
	h.adjMu.Unlock()
}

func (h *Handle) wasAdjudicated(callID string) bool {
	if callID == "" {
		// Fail closed: a built-in call with no correlatable id cannot be
		// proven to have been adjudicated.
		return false
	}
	h.adjMu.Lock()
	defer h.adjMu.Unlock()
	return h.adjudicated[callID]
}

// signalClosed closes h.closed exactly once (teardown broadcast).
func (h *Handle) signalClosed() {
	h.closedOnce.Do(func() { close(h.closed) })
}

// closeEvents closes h.events exactly once, flag-first under the write lock.
func (h *Handle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

// emit publishes without blocking; drops when the channel is full or closed.
func (h *Handle) emit(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	// Deliberately does NOT select on <-h.closed. h.events is buffered
	// (256), so the send below is already non-blocking regardless — default
	// covers the one case that matters, a genuinely full buffer. Racing
	// h.closed here as a second ready case would make Go's select pick
	// between "deliver ev" and "notice closed and drop it" pseudo-randomly
	// whenever both are simultaneously ready, which is exactly the case for
	// a fatal event: dispatch()/run() now close h.closed BEFORE emitting a
	// fatal terminal (so a racing Inject reliably observes "closed"), and
	// that same close would otherwise be racing THIS send for the terminal
	// event itself — silently dropping the one event callers most need to
	// see, on no predictable schedule.
	select {
	case h.events <- ev:
	default:
	}
}

// parseAdjudicateCall extracts a ToolCall from an adjudicate payload. The
// extension serializes {toolName, toolCallId, input, cwd} (the real tool_call
// event fields). File paths are resolved against cwd for containment.
func parseAdjudicateCall(payload map[string]any, specCwd string) ToolCall {
	tool := stringField(payload, "toolName")
	kind := builtInToolNames[strings.ToLower(strings.TrimSpace(tool))]
	cwd := stringField(payload, "cwd")
	if cwd == "" {
		cwd = specCwd
	}
	input := mapField(payload, "input")
	command := stringField(input, "command")
	path := resolveInputPath(cwd, input)
	return ToolCall{
		Kind:    kind,
		Command: command,
		Path:    path,
		Cwd:     cwd,
	}
}

// resolveInputPath resolves a tool input's file path (path/file/filename)
// against cwd so containment checks operate on absolute paths.
func resolveInputPath(cwd string, input map[string]any) string {
	p := stringField(input, "path", "file", "filename")
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func permissionMessage(call ToolCall, d Decision) string {
	verb := "allowed"
	if !d.Allow {
		verb = "denied: " + d.Reason
	}
	subj := call.Command
	if subj == "" {
		subj = call.Path
	}
	return fmt.Sprintf("%s %s(%s)", verb, call.Kind, subj)
}
