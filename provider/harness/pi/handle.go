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

	idMu      sync.RWMutex
	sessionID string // resolved from the get_state response; guarded by idMu

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
				// Stream closed. If the session cleanly reached a non-retrying
				// agent_end but the terminal agent_settled never arrived, emit a
				// clean ResultEvent; otherwise, if nothing terminal was emitted,
				// surface a crash ErrorEvent so the runner sees the failure.
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

// dispatch handles one rawEvent. Returns true when the session has reached its
// terminal event and the pump should exit.
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
	case "message_update", "tool_execution_start", "turn_start":
		h.turnInFlight.Store(true)
	case "turn_end", "agent_end", "agent_settled":
		h.turnInFlight.Store(false)
	}

	out, terminal := mapEvent(ev, h.state)
	// Keep SessionID() in sync with whatever the mapper resolved (get_state).
	if h.state.sessionID != "" {
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
	select {
	case h.events <- ev:
	case <-h.closed:
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
