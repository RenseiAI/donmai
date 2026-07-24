package pi

import (
	"context"
	"fmt"
	"os/exec"
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
	// abortGrace is how long Stop waits for a clean agent_end after sending
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
// on every built-in tool_execution_start (design §5.3).
type Handle struct {
	client *rpcClient
	cmd    *exec.Cmd
	policy *PolicyEngine
	spec   agent.Spec
	state  *mapperState

	nonce string

	// handshakeResult delivers exactly one verdict to Spawn: nil = verified,
	// err = failed/mismatch. Buffered so the pump never blocks on it.
	handshakeResult chan error
	handshakeOnce   sync.Once

	// adjudicated records the pi call ids that completed a policy round-trip,
	// so the bypass monitor can flag a built-in tool_execution_start that
	// arrived without one.
	adjMu       sync.Mutex
	adjudicated map[string]bool

	// turnInFlight is true between the first streaming event of a turn and its
	// turn_end/agent_end — Inject routes to steer while in flight, follow_up
	// while idle.
	turnInFlight atomic.Bool

	idMu      sync.RWMutex
	sessionID string // resolved from agent_start; guarded by idMu

	// Close protocol (identical discipline to codex/handle.go): eventsMu +
	// eventsClosed gate every send; closeEvents is the single idempotent
	// closer; h.closed broadcasts teardown to unblock a slow send.
	events       chan agent.Event
	closeOnce    sync.Once
	closedOnce   sync.Once
	closed       chan struct{}
	eventsMu     sync.RWMutex
	eventsClosed atomic.Bool
}

func newHandle(client *rpcClient, cmd *exec.Cmd, spec agent.Spec, nonce string) *Handle {
	return &Handle{
		client:          client,
		cmd:             cmd,
		policy:          NewPolicyEngine(spec),
		spec:            spec,
		state:           &mapperState{},
		nonce:           nonce,
		handshakeResult: make(chan error, 1),
		adjudicated:     make(map[string]bool),
		events:          make(chan agent.Event, 256),
		closed:          make(chan struct{}),
	}
}

// SessionID returns the pi session id once agent_start has resolved it.
func (h *Handle) SessionID() string {
	h.idMu.RLock()
	defer h.idMu.RUnlock()
	return h.sessionID
}

// Events returns the read-only event channel; closed after the terminal event.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject sends a follow-up user message. It maps to steer when a turn is in
// flight, else follow_up (design §7). The queue_update events pi emits in
// response are surfaced as SystemEvents by the pump.
func (h *Handle) Inject(_ context.Context, text string) error {
	select {
	case <-h.closed:
		return fmt.Errorf("pi: session closed")
	default:
	}
	cmd := "follow_up"
	if h.turnInFlight.Load() {
		cmd = "steer"
	}
	return h.client.WriteCommand(map[string]any{"command": cmd, "text": text})
}

// Stop aborts the session: send abort, wait ≤abortGrace for a clean end, then
// SIGTERM the process group → termGrace → SIGKILL (design §2). Idempotent.
func (h *Handle) Stop(ctx context.Context) error {
	h.closeOnce.Do(func() {
		_ = h.client.WriteCommand(map[string]any{"command": "abort"})
		// Give pi a moment to emit agent_end and exit cleanly.
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
// extension requests to the trust boundary, maps the rest to agent.Events, and
// closes on terminal/EOF. Started by Spawn.
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
				// Stream closed. If nothing terminal was emitted, surface a
				// crash ErrorEvent so the runner sees the failure.
				if cause := h.client.CloseErr(); cause != nil {
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

// dispatch handles one rawEvent. Returns true when the session has reached its
// terminal event and the pump should exit.
func (h *Handle) dispatch(ev rawEvent) bool {
	// Extension requests are intercepted before the mapper.
	if ev.Type == "extension_ui_request" {
		h.handleExtensionRequest(ev)
		return false
	}

	// Bypass monitor: a built-in tool_execution_start MUST have completed a
	// policy round-trip for its call id (design §5.3). This is belt-and-braces
	// — it should be impossible when the overrides are installed — but it is
	// the fail-closed catch if the extension is subverted.
	if ev.Type == "tool_execution_start" {
		tool := stringField(ev.Fields, "tool", "name", "toolName")
		callID := stringField(ev.Fields, "callId", "call_id", "id")
		if isBuiltInTool(tool) && !h.wasAdjudicated(callID) {
			h.emit(agent.ErrorEvent{
				Message: fmt.Sprintf("policy bypass: built-in tool %q (call %q) executed without a policy adjudication round-trip", tool, callID),
				Code:    "policy_extension_failed",
				Raw:     raw(ev),
			})
			return true
		}
	}

	// Track turn-in-flight for Inject routing.
	switch ev.Type {
	case "message_update", "tool_execution_start":
		h.turnInFlight.Store(true)
	case "turn_end", "agent_end":
		h.turnInFlight.Store(false)
	}

	out, terminal := mapEvent(ev, h.state)
	if ev.Type == "agent_start" {
		// Publish the resolved id under the id lock so SessionID() readers on
		// other goroutines see it race-free.
		h.idMu.Lock()
		h.sessionID = h.state.sessionID
		h.idMu.Unlock()
	}
	for _, e := range out {
		h.emit(e)
	}
	return terminal
}

// handleExtensionRequest verifies the handshake or adjudicates a tool call,
// replying over the RPC stream. This is the synchronous heart of the trust
// boundary.
func (h *Handle) handleExtensionRequest(ev rawEvent) {
	reqID := stringField(ev.Fields, "id", "requestId", "request_id")
	inner, _ := ev.Fields["request"].(map[string]any)
	if inner == nil {
		// Some shapes may inline the payload on the event itself.
		inner = ev.Fields
	}
	reqType := stringField(inner, "type")

	switch reqType {
	case handshakeType:
		claimedSHA := stringField(inner, "extensionSHA", "extension_sha", "sha")
		if !verifyHandshakeSHA(claimedSHA) {
			// SHA mismatch (or empty): the materialized extension is not the
			// one we shipped. Fail closed — no prompt, kill the session.
			_ = h.replyExtension(reqID, map[string]any{"ok": false, "reason": "handshake SHA mismatch"})
			h.emit(agent.ErrorEvent{
				Message: "pi policy extension handshake SHA mismatch — refusing to run unguarded",
				Code:    "policy_extension_failed",
				Raw:     raw(ev),
			})
			h.resolveHandshake(fmt.Errorf("%w: pi policy extension handshake SHA mismatch", agent.ErrSpawnFailed))
			// Tear the session down.
			h.signalClosed()
			return
		}
		// Verified. Adopt the extension's nonce for subsequent correlation.
		if n := stringField(inner, "nonce"); n != "" {
			h.nonce = n
		}
		_ = h.replyExtension(reqID, map[string]any{"ok": true, "nonce": h.nonce})
		h.resolveHandshake(nil)

	case adjudicateType:
		// Nonce must match the handshake's — a request without the session
		// nonce is not from our verified extension.
		if n := stringField(inner, "nonce"); n != "" && n != h.nonce {
			_ = h.replyExtension(reqID, map[string]any{"allow": false, "reason": "nonce mismatch"})
			return
		}
		call := parseToolCall(inner, h.spec.Cwd)
		decision := h.policy.Evaluate(call)
		if callID := stringField(mapField(inner, "call"), "callId", "call_id", "id"); callID != "" {
			h.markAdjudicated(callID)
		}
		_ = h.replyExtension(reqID, map[string]any{"allow": decision.Allow, "reason": decision.Reason})
		h.emit(agent.SystemEvent{
			Subtype: "permission_decision",
			Message: permissionMessage(call, decision),
			Raw:     raw(ev),
		})

	default:
		// Unknown extension request: decline so the extension does not hang,
		// and surface it for observability.
		_ = h.replyExtension(reqID, map[string]any{"ok": false, "reason": "unhandled request type"})
		h.emit(agent.SystemEvent{Subtype: "unhandled_extension_request", Message: reqType, Raw: raw(ev)})
	}
}

// replyExtension writes an extension_ui_response for reqID.
func (h *Handle) replyExtension(reqID string, payload map[string]any) error {
	return h.client.WriteCommand(map[string]any{
		"command":  "extension_ui_response",
		"id":       reqID,
		"response": payload,
	})
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
	select {
	case h.events <- ev:
	case <-h.closed:
	default:
	}
}

// parseToolCall extracts a ToolCall from an adjudicate request payload. The
// extension serializes {call:{tool,args,command,path,cwd}} (design §5.1).
func parseToolCall(inner map[string]any, specCwd string) ToolCall {
	c := mapField(inner, "call")
	tool := stringField(c, "tool", "name")
	kind := builtInToolNames[strings.ToLower(strings.TrimSpace(tool))]
	cwd := stringField(c, "cwd")
	if cwd == "" {
		cwd = specCwd
	}
	return ToolCall{
		Kind:    kind,
		Command: stringField(c, "command"),
		Path:    stringField(c, "path"),
		Cwd:     cwd,
	}
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
