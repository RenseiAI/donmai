package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// Handle is one live codex thread. Multiple Handles share the same
// codex app-server subprocess via the parent Provider; each subscribes
// to JSON-RPC notifications carrying its own threadId.
//
// Handle implements agent.Handle. The lifecycle is:
//
//  1. Provider.Spawn assembles a Handle and calls handle.start(ctx,
//     spec) to issue thread/start + the first turn/start.
//  2. The Handle runs a forwarder goroutine that consumes inbound
//     notifications, translates them via mapNotification, and
//     publishes to events.
//  3. On terminal ResultEvent / ErrorEvent / context cancellation /
//     app-server crash, the forwarder closes events and unsubscribes.
type Handle struct {
	provider *Provider // nil for unit tests; non-nil in real use
	client   *Client
	bridge   *ApprovalBridge
	spec     agent.Spec

	// Per-call request timeout for thread/start, turn/start, etc.
	rpcTimeout time.Duration

	idMu     sync.RWMutex
	threadID string

	events   chan agent.Event
	notifyCh chan notification

	// Close-protocol invariant (REN-1460):
	//
	// The events channel has multiple potential senders (the forwarder
	// goroutine running emit, the failNow path posting an
	// app_server_crashed ErrorEvent) and multiple potential closers
	// (forward's defer, Stop, failNow). To avoid "send on closed
	// channel" panics under concurrent shutdown, every events-channel
	// access is gated by eventsMu + eventsClosed:
	//
	//   - Senders take eventsMu.RLock(), check eventsClosed.Load() and
	//     drop if true, otherwise select on send vs h.closed so a
	//     racing closer that has not yet flipped the flag still
	//     unblocks them.
	//   - The single closeEvents() helper takes eventsMu.Lock(),
	//     flips eventsClosed atomically, and closes the channel. It is
	//     idempotent under the flag check; callers may invoke it from
	//     forward's defer, Stop, or failNow without coordination.
	//
	// h.closed is the lifecycle broadcast: closed exactly once via
	// closedOnce. It signals "the handle is being torn down" to any
	// goroutine that might block on a slow send. closeOnce remains for
	// the per-path cleanup work (RPC unsubscribe, Stop's best-effort
	// turn/interrupt) that must run exactly once.
	closeOnce    sync.Once
	closedOnce   sync.Once
	closed       chan struct{}
	eventsMu     sync.RWMutex
	eventsClosed atomic.Bool

	state *mapperState
}

// HandleOptions tweaks handle behavior. Used by tests; production code
// uses the defaults via Provider.Spawn.
type HandleOptions struct {
	// RPCTimeout caps how long a single JSON-RPC request waits.
	// Defaults to 30s (matches the legacy TS PendingRequest timer).
	RPCTimeout time.Duration

	// EventBuffer sets the events channel buffer. Defaults to 256.
	EventBuffer int
}

func newHandle(p *Provider, client *Client, spec agent.Spec, opts HandleOptions) *Handle {
	if opts.RPCTimeout == 0 {
		opts.RPCTimeout = 30 * time.Second
	}
	if opts.EventBuffer == 0 {
		opts.EventBuffer = 256
	}
	return &Handle{
		provider:   p,
		client:     client,
		bridge:     NewApprovalBridge(spec.PermissionConfig),
		spec:       spec,
		rpcTimeout: opts.RPCTimeout,
		events:     make(chan agent.Event, opts.EventBuffer),
		notifyCh:   make(chan notification, 256),
		closed:     make(chan struct{}),
		state:      &mapperState{model: resolveModel(spec)},
	}
}

// SessionID returns the codex thread id once thread/start has resolved.
func (h *Handle) SessionID() string {
	h.idMu.RLock()
	defer h.idMu.RUnlock()
	return h.threadID
}

// Events returns the read-only event channel. Closes after the
// terminal event.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject is intentionally unsupported on codex. Mirrors F.1.1 §3.2 the
// "Codex provider does not support mid-session message injection"
// note — the legacy TS supports turn-level steering, but the v0.5.0
// Go port keeps the surface minimal and routes steering through
// Provider.Resume + a fresh Spec.
func (h *Handle) Inject(_ context.Context, _ string) error {
	return agent.ErrUnsupported
}

// Stop interrupts the active turn (if any), unsubscribes the thread,
// and closes the events channel. Idempotent.
func (h *Handle) Stop(ctx context.Context) error {
	h.closeOnce.Do(func() {
		threadID := h.SessionID()
		if h.client != nil && threadID != "" {
			// Best-effort: interrupt + unsubscribe. Ignore errors —
			// the codex side may have already torn down the thread.
			_, _ = h.client.Request(ctx, "turn/interrupt", map[string]any{
				"threadId": threadID,
				"turnId":   "current",
			}, h.rpcTimeout)
			_, _ = h.client.Request(ctx, "thread/unsubscribe", map[string]any{
				"threadId": threadID,
			}, h.rpcTimeout)
			h.client.Unsubscribe(threadID)
		}
	})
	// Broadcast shutdown + close events; both are idempotent guards
	// (closedOnce / eventsClosed) so the call is safe even if the
	// forwarder defer or failNow has already run.
	h.signalClosed()
	h.closeEvents()
	return nil
}

// failNow marks the handle terminal with an ErrorEvent and closes the
// events channel. Used by the Provider when the shared app-server
// crashes. Safe to call concurrently with Stop / forward; close
// ordering is enforced by the eventsClosed flag (see emit / closeEvents).
func (h *Handle) failNow(err error) {
	// emit is safe under concurrent close: it short-circuits when
	// eventsClosed is already set. Posting BEFORE we close lets the
	// runner observe the failure rather than racing with closure.
	h.emit(agent.ErrorEvent{
		Message: fmt.Sprintf("codex provider failure: %s", err.Error()),
		Code:    "app_server_crashed",
	})
	threadID := h.SessionID()
	if h.client != nil && threadID != "" {
		h.client.Unsubscribe(threadID)
	}
	h.signalClosed()
	h.closeEvents()
}

// signalClosed closes h.closed exactly once. h.closed is the
// "session being torn down" broadcast that unblocks producers stuck on
// a slow consumer.
func (h *Handle) signalClosed() {
	h.closedOnce.Do(func() { close(h.closed) })
}

// closeEvents closes h.events exactly once. The flag is set FIRST
// under the write lock so concurrent emit() callers (which hold the
// read lock + check the flag) cannot race past the check and send to
// the closed channel.
func (h *Handle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

// start performs the JSON-RPC bring-up: optional MCP config push,
// thread/start (or thread/resume), subscribe, first turn/start.
func (h *Handle) start(ctx context.Context, plan SpawnPlan, resumeThreadID string) error {
	// Subscribe BEFORE thread/start to catch the immediate
	// thread/started notification.
	provisionalSub := func(n notification) { h.notifyCh <- n }

	if resumeThreadID != "" {
		h.client.Subscribe(resumeThreadID, provisionalSub)
		h.idMu.Lock()
		h.threadID = resumeThreadID
		h.idMu.Unlock()
		params := map[string]any{
			"threadId":    resumeThreadID,
			"personality": "pragmatic",
		}
		if _, err := h.client.RequestWithRetry(ctx, "thread/resume", params, h.rpcTimeout); err != nil {
			h.client.Unsubscribe(resumeThreadID)
			return fmt.Errorf("thread/resume: %w", err)
		}
	} else {
		raw, err := h.client.RequestWithRetry(ctx, "thread/start", plan.ThreadStart, h.rpcTimeout)
		if err != nil {
			return fmt.Errorf("thread/start: %w", err)
		}
		var resp struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("thread/start: decode response: %w", err)
		}
		if resp.Thread.ID == "" {
			return errors.New("thread/start: empty thread id in response")
		}
		h.idMu.Lock()
		h.threadID = resp.Thread.ID
		h.idMu.Unlock()
		h.client.Subscribe(resp.Thread.ID, provisionalSub)
	}

	// First turn — fill in threadId now that we have it.
	turnParams := plan.TurnStart
	turnParams["threadId"] = h.SessionID()
	if _, err := h.client.RequestWithRetry(ctx, "turn/start", turnParams, h.rpcTimeout); err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}

	// The forwarder is intentionally long-lived: it must survive
	// Spawn() returning and run for the duration of the session.
	// gosec G118 flags this; the forwarder honors ctx.Done() so the
	// caller still controls cancellation.
	go h.forward(ctx) //nolint:gosec // G118: session-lifetime goroutine, ctx is honored
	return nil
}

// forward is the per-Handle goroutine that consumes inbound
// notifications, translates them to agent.Events, and publishes.
//
// The loop terminates when:
//   - ctx is cancelled (we send turn/interrupt + close)
//   - h.closed is signalled by Stop / failNow
//   - a terminal ResultEvent / ErrorEvent has been emitted
//
// Close protocol (REN-1460): the defer here always runs the cleanup
// helpers, but each helper is internally idempotent — it is safe for
// failNow to have already closed events from a parallel goroutine.
// Without this, a Stop call from inside ctx.Done could leave events
// open while a concurrent failNow racing through onClientClose would
// close events under our feet, panicking the next emit.
func (h *Handle) forward(ctx context.Context) {
	defer func() {
		h.closeOnce.Do(func() {
			threadID := h.SessionID()
			if h.client != nil && threadID != "" {
				h.client.Unsubscribe(threadID)
			}
		})
		h.signalClosed()
		h.closeEvents()
	}()

	for {
		select {
		case <-ctx.Done():
			// Send turn/interrupt + thread/unsubscribe before
			// returning so codex tears down its side cleanly.
			ctxStop, cancel := context.WithTimeout(context.Background(), h.rpcTimeout)
			_ = h.Stop(ctxStop)
			cancel()
			h.emit(agent.ErrorEvent{Message: "session cancelled: " + ctx.Err().Error(), Code: "context_cancelled"})
			return
		case <-h.closed:
			return
		case n := <-h.notifyCh:
			done := h.handleNotification(n)
			if done {
				return
			}
		}
	}
}

// handleNotification dispatches one inbound notification. Returns
// true when the session has reached its terminal event and the
// forwarder should exit.
func (h *Handle) handleNotification(n notification) bool {
	// Server-requests come down the same channel; route to the
	// approval bridge first.
	if len(n.ServerRequestID) > 0 {
		h.handleServerRequest(n)
		return false
	}

	// Decode params for inspection. Use the raw json + a generic
	// map so the bridge / mapper can both work from it.
	var rawObj map[string]any
	if len(n.Params) > 0 {
		_ = json.Unmarshal(n.Params, &rawObj)
	}

	events := mapNotification(n.Method, n.Params, h.state, rawObj)
	for _, ev := range events {
		h.emit(ev)
		switch ev.Kind() {
		case agent.EventResult, agent.EventError:
			return true
		}
	}
	return false
}

// handleServerRequest evaluates an approval request via the bridge
// and replies on the JSON-RPC stream. Mirrors handleApprovalRequest in
// the legacy TS.
func (h *Handle) handleServerRequest(n notification) {
	method := n.Method

	// Synthesize a typed request from params.
	var rawObj map[string]any
	_ = json.Unmarshal(n.Params, &rawObj)

	if h.isApprovalMethod(method) {
		req := parseApprovalRequest(rawObj, h.spec.Cwd)
		// Always run the bridge — built-in safety deny rules cannot
		// be opted out of via AutoApproveAll. The bridge's own
		// default already accepts when no policy is configured, so
		// AutoApproveAll only matters when downgrading explicit
		// "prompt"/"deny" defaults to "allow"; that's intentional.
		decision := h.bridge.Evaluate(req)

		// Emit ToolUse + ToolResult so the runner sees the call
		// flow even when it auto-approves. The legacy TS did not
		// do this — but the F.1.1 spec specifically calls out
		// "Emits ToolUse + ToolResult events through the approval
		// pipeline so runner sees them." (§F.2.4 spec for this
		// task).
		toolName := approvalToolName(req)
		toolUseID := fmt.Sprintf("approval-%s", method)
		h.emit(agent.ToolUseEvent{
			ToolName:  toolName,
			ToolUseID: toolUseID,
			Input:     approvalInput(req),
			Raw:       rawObj,
		})

		// Reply on the stream.
		_ = h.client.RespondToServerRequest(n.ServerRequestID, map[string]any{
			"decision": string(decision.Action),
		})

		isError := decision.Action == ActionDecline
		content := string(decision.Action)
		if decision.Reason != "" {
			content += ": " + decision.Reason
		}
		h.emit(agent.ToolResultEvent{
			ToolName:  toolName,
			ToolUseID: toolUseID,
			Content:   content,
			IsError:   isError,
			Raw:       rawObj,
		})

		if decision.Action == ActionDecline {
			h.emit(agent.SystemEvent{
				Subtype: "approval_denied",
				Message: "Blocked: " + decision.Reason,
				Raw:     rawObj,
			})
		}
		return
	}

	// MCP elicitation: codex forwards MCP-server "ask the user"
	// prompts as server-requests. Autonomous mode has no human;
	// reply with cancel per the MCP spec.
	if method == "mcpServer/elicitation/request" {
		_ = h.client.RespondToServerRequest(n.ServerRequestID, map[string]any{"action": "cancel"})
		mcpServer, _ := rawObj["mcpServer"].(string)
		h.emit(agent.SystemEvent{
			Subtype: "elicitation_cancelled",
			Message: fmt.Sprintf("Cancelled MCP elicitation from %s — autonomous mode has no user to prompt", emptyToUnknown(mcpServer)),
			Raw:     rawObj,
		})
		return
	}

	// Anything else: respond -32601 so codex stops waiting.
	_ = h.client.RespondToServerRequestWithError(n.ServerRequestID, -32601, "Client does not implement "+method)
	h.emit(agent.SystemEvent{
		Subtype: "unhandled_server_request",
		Message: "Declined unhandled codex server request: " + method,
		Raw:     rawObj,
	})
}

// emit publishes an event on the events channel without blocking. If
// the channel is full, the event is dropped silently — the runner is
// expected to keep up; emitting backpressure into the JSON-RPC stream
// would deadlock the codex side.
//
// Close-protocol invariant (REN-1460): emit holds eventsMu.RLock for
// the duration of the send and aborts early when eventsClosed is set.
// The matching writer (closeEvents) takes the write lock before
// flipping the flag and closing the channel, so emit can never observe
// a closed channel via the send path. The h.closed broadcast is also
// selected against so a slow send unblocks promptly when shutdown
// begins, even if the closer has not yet acquired the write lock.
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

func (h *Handle) isApprovalMethod(method string) bool {
	if strings.Contains(method, "pproval") || strings.Contains(method, "requestApproval") {
		return true
	}
	switch method {
	case "applyPatchApproval", "execCommandApproval":
		return true
	}
	return false
}

func approvalToolName(req ApprovalRequest) string {
	switch req.Kind {
	case ApprovalKindCommand:
		return "shell"
	case ApprovalKindFileChange:
		return "file_change"
	default:
		return "approval_unknown"
	}
}

func approvalInput(req ApprovalRequest) map[string]any {
	switch req.Kind {
	case ApprovalKindCommand:
		return map[string]any{"command": req.Command}
	case ApprovalKindFileChange:
		return map[string]any{"path": req.Path}
	default:
		return map[string]any{}
	}
}

func emptyToUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
