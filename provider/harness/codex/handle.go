package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

	// denyNotice reports the denies Codex does not enforce as asked; it is
	// emitted once, right after the InitEvent the event contract keeps first.
	denyNotice *agent.SystemEvent

	// initEmitted keeps the InitEvent single: a fresh thread emits it from
	// the thread/start response, and a thread/started that does reach the
	// subscription afterwards is a duplicate.
	initEmitted atomic.Bool

	// Per-call request timeout for thread/start, turn/start, etc.
	rpcTimeout time.Duration

	idMu     sync.RWMutex
	threadID string

	events   chan agent.Event
	notifyCh chan notification

	// Close-protocol invariant:
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
	mcpRelease   func() error
	mcpOnce      sync.Once
	mcpErr       error
	// terminal is set once the forwarder has the session's terminal event.
	// From then on the forwarder alone delivers the outcome: the final MCP
	// release can stop the app-server, and the resulting client close must
	// not fail this handle over the result it is about to emit.
	terminal atomic.Bool
	// stopErr records the terminal-stop shutdown acknowledgement: any
	// turn/interrupt or thread/unsubscribe failure (timeout, rejection,
	// transport error) proves the owned writers may still be live. It is
	// written once inside closeOnce and joined into Stop's return so a
	// nil Stop means both shutdown RPCs were acknowledged, never merely
	// that config cleanup succeeded. Callers gate quiescence evidence on
	// that nil; an unproved shutdown stays unquiesced (fail-closed).
	stopErr error

	state *mapperState

	// tail is the rollout context-notice tail start() launched, or nil
	// when the session runs without notices. Set before the forwarder
	// starts; the forwarder drains it on the terminal event.
	tail *rolloutContextTail
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

// codexHome returns the isolated home the session's rollout file lives
// under, or "" when unknown (a unit-test handle with no provider). It is
// the provider's own boundary home — never the ambient environment — so
// the context-notice tail reads exactly this session's file.
func (h *Handle) codexHome() string {
	if h == nil || h.provider == nil || h.provider.config == nil {
		return ""
	}
	return h.provider.config.home
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
// and closes the events channel. Idempotent. Any turn/interrupt or
// thread/unsubscribe failure is joined into the return: a nil Stop
// means both shutdown RPCs were acknowledged and the owned writers were
// stopped and reaped. A timeout or rejection leaves the writers possibly
// live, so the caller must treat a non-nil Stop as unproved shutdown
// (unquiesced evidence, never refused). Config cleanup success alone
// never certifies quiescence.
func (h *Handle) Stop(ctx context.Context) error {
	h.closeOnce.Do(func() {
		threadID := h.SessionID()
		if h.client != nil && threadID != "" {
			if _, err := h.client.Request(ctx, "turn/interrupt", map[string]any{
				"threadId": threadID,
				"turnId":   "current",
			}, h.rpcTimeout); err != nil {
				h.stopErr = errors.Join(h.stopErr, fmt.Errorf("codex stop: turn/interrupt: %w", err))
			}
			if _, err := h.client.Request(ctx, "thread/unsubscribe", map[string]any{
				"threadId": threadID,
			}, h.rpcTimeout); err != nil {
				h.stopErr = errors.Join(h.stopErr, fmt.Errorf("codex stop: thread/unsubscribe: %w", err))
			}
			h.client.Unsubscribe(threadID)
		}
	})
	cleanupErr := h.releaseMCPConfig()
	if cleanupErr != nil {
		h.emitCoarse(mcpCleanupErrorEvent())
	}
	// Broadcast shutdown + close events; both are idempotent guards
	// (closedOnce / eventsClosed) so the call is safe even if the
	// forwarder defer or failNow has already run.
	h.signalClosed()
	h.closeEvents()
	return errors.Join(h.stopErr, cleanupErr)
}

// failNow marks the handle terminal with an ErrorEvent and closes the
// events channel. Used by the Provider when the shared app-server
// crashes. Safe to call concurrently with Stop / forward; close
// ordering is enforced by the eventsClosed flag (see emit / closeEvents).
func (h *Handle) failNow(err error) {
	if h.terminal.Load() {
		// The forwarder already holds this session's terminal event and
		// owns its delivery and close (see handleNotification).
		return
	}
	cleanupErr := h.releaseMCPConfig()
	// emit is safe under concurrent close: it short-circuits when
	// eventsClosed is already set. Posting BEFORE we close lets the
	// runner observe the failure rather than racing with closure.
	if cleanupErr != nil {
		h.emitCoarse(mcpCleanupErrorEvent())
	} else {
		h.emitCoarse(agent.ErrorEvent{
			Message: fmt.Sprintf("codex provider failure: %s", err.Error()),
			Code:    "app_server_crashed",
		})
	}
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
	if h.eventsClosed.Load() {
		h.eventsMu.Unlock()
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
	h.eventsMu.Unlock()

	if h.provider != nil {
		h.provider.unregisterHandle(h)
	}
	_ = h.releaseMCPConfig()
}

func (h *Handle) releaseMCPConfig() error {
	h.mcpOnce.Do(func() {
		if h.mcpRelease != nil {
			h.mcpErr = h.mcpRelease()
		}
	})
	return h.mcpErr
}

// isThreadNotFoundResumeError reports whether a thread/resume rejection
// proves the thread is gone (or has no resumable history) rather than a
// transient or authorization backend failure. The backend rejects two ways:
// a typed JSON-RPC error whose message names the missing rollout/session
// (verified against a real CLI: "no rollout found for thread id ..."),
// or a plain-text rejection in the same shape. Matching is
// case-insensitive and scoped to those shapes so an unrelated failure that
// happens to mention a thread is never misread as gone — misreading it
// would let the restore machine take fresh and orphan the prior work.
func isThreadNotFoundResumeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, agent.ErrSessionNotFound) {
		return true
	}
	lowered := strings.ToLower(err.Error())
	for _, shape := range []string{
		"no rollout found",
		"no saved session found",
		"no conversation found",
		"thread not found",
	} {
		if strings.Contains(lowered, shape) {
			return true
		}
	}
	return false
}

// start performs the JSON-RPC bring-up: optional MCP config push,
// thread/start (or thread/resume), subscribe, first turn/start.
func (h *Handle) start(ctx context.Context, plan SpawnPlan, resumeThreadID string) error {
	// Subscribe BEFORE thread/start to catch the immediate
	// thread/started notification.
	provisionalSub := func(n notification) { h.notifyCh <- n }
	if plan.DenyErr != nil {
		return plan.DenyErr
	}
	h.denyNotice = plan.denyNotice

	if resumeThreadID != "" {
		h.client.Subscribe(resumeThreadID, provisionalSub)
		h.idMu.Lock()
		h.threadID = resumeThreadID
		h.idMu.Unlock()
		params := threadResumeParams(resumeThreadID, denyPlan{})
		maps.Copy(params, plan.ThreadResume)
		params["threadId"] = resumeThreadID
		if _, err := h.client.RequestWithRetry(ctx, "thread/resume", params, h.rpcTimeout); err != nil {
			h.client.Unsubscribe(resumeThreadID)
			// A rejected resume is not a fresh spawn: when the backend
			// proves the thread is gone (no persisted rollout, no saved
			// session) fail typed-not-found so the restore state
			// machine (agent.DecideRestore / IsNoConversationFound)
			// can admit fresh_primed; any other failure stays a plain
			// resume error and escalates to attention_required. This
			// mirrors the interactive attach path's
			// resumeExistingNamedThreadWithRequest contract.
			if isThreadNotFoundResumeError(err) {
				return fmt.Errorf("%w: codex thread %q does not exist or has no resumable history: %w", agent.ErrSessionNotFound, resumeThreadID, err)
			}
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
		// The response carries the thread id, so the InitEvent is emitted
		// here. The real thread/started notification keys the id under
		// params.thread, not a top-level threadId, so the client routes it
		// to the global handler rather than this subscription; it also
		// races the Subscribe above. Neither can be what delivers Init.
		var initRaw map[string]any
		_ = json.Unmarshal(raw, &initRaw)
		h.emitInit(agent.InitEvent{SessionID: resp.Thread.ID, Raw: initRaw})
		if settings, ok := effectiveSettings(h.spec, raw); ok {
			h.emit(settings)
		}
		if h.spec.SessionName != "" {
			if _, err := h.client.RequestWithRetry(ctx, "thread/name/set", map[string]any{
				"threadId": resp.Thread.ID,
				"name":     h.spec.SessionName,
			}, h.rpcTimeout); err != nil {
				h.client.Unsubscribe(resp.Thread.ID)
				return fmt.Errorf("thread/name/set: %w", err)
			}
		}
	}

	// First turn — fill in threadId now that we have it.
	turnParams := plan.TurnStart
	turnParams["threadId"] = h.SessionID()
	if _, err := h.client.RequestWithRetry(ctx, "turn/start", turnParams, h.rpcTimeout); err != nil {
		return fmt.Errorf("turn/start: %w", err)
	}

	// Context-notice tail: the thread's rollout file carries the token_count
	// events and compacted markers the RPC stream never surfaces. The home
	// is the provider's own isolated home (never the ambient environment)
	// and the thread id is the one just started or resumed above; either
	// unknown (a unit-test handle with no provider) runs without notices.
	h.tail = h.startContextTail(ctx)

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
// Close protocol: the defer here always runs the cleanup
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
			h.emitCoarse(agent.ErrorEvent{Message: "session cancelled: " + ctx.Err().Error(), Code: "context_cancelled"})
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
		switch ev.Kind() {
		case agent.EventResult, agent.EventError:
			// The turn's last token_count lands in the rollout moments
			// before this notification; read it before Events closes.
			h.tail.drain(h.emit)
			// Leave the provider's live set before the final release. On a
			// failed clear the release stops the app-server process, and its
			// exit closes the client; onClientClose then fails every
			// registered handle. This handle already has its terminal event,
			// so that close must not race an mcp_cleanup_failed error (and an
			// Events close) ahead of the outcome emitted below.
			h.terminal.Store(true)
			if h.provider != nil {
				h.provider.unregisterHandle(h)
			}
			if err := h.releaseMCPConfig(); err != nil {
				if result, ok := ev.(agent.ResultEvent); ok && result.Success {
					// The turn finished successfully and its result is real.
					// A cleanup failure (contained or not) never replaces a
					// successful result: report the result and surface the
					// cleanup failure as a warning with its own reason
					// (codex_warning carrying the bounded release detail).
					// The Provider is poisoned for later leases and the private
					// home went through the retention/sweep path. Only a
					// contained removal proves the writer stopped; otherwise
					// shutdown stays unconfirmed and the warning says so.
					contained := mcpCleanupContained(err)
					if contained {
						slog.Warn("codex: isolated MCP config cleanup failed after the turn completed; app-server stopped, provider refuses new sessions",
							"threadId", h.SessionID(), "err", err)
					} else {
						slog.Warn("codex: isolated MCP config cleanup failed after the turn completed; app-server shutdown unconfirmed (writer may still be live), provider refuses new sessions",
							"threadId", h.SessionID(), "err", err)
					}
					h.emitTerminalWithWarning(mcpCleanupWarningEvent(err, contained), ev)
				} else if ev.Kind() == agent.EventResult && mcpCleanupContained(err) {
					// A failed turn whose clear is contained keeps its own
					// result alongside the warning; anything else fails
					// closed so an unfinished turn never reports success.
					// Contained removal proves the writer stopped, so the
					// stopped claim is accurate here.
					slog.Warn("codex: isolated MCP config cleanup failed after the turn completed; app-server stopped, provider refuses new sessions",
						"threadId", h.SessionID(), "err", err)
					h.emitTerminalWithWarning(mcpCleanupWarningEvent(err, true), ev)
				} else {
					h.emitCoarse(mcpCleanupErrorEvent())
				}
			} else {
				h.emitCoarse(ev)
			}
			return true
		}
		if ev.Kind() == agent.EventInit {
			h.emitInit(ev)
			continue
		}
		h.emit(ev)
	}
	return false
}

// mcpCleanupWarningEvent reports a cleanup failure that accompanies the
// turn's own result. The detail is the bounded release error
// (codexMCPConfigFailureDetail never carries config values), so the cause is
// visible without leaking the session's MCP config. writerStopped reports
// whether owner removal proved the writer stopped (contained): only then does
// the message claim the app-server was stopped, otherwise it reports shutdown
// as unconfirmed so a live writer is never described as stopped.
func mcpCleanupWarningEvent(err error, writerStopped bool) agent.SystemEvent {
	if writerStopped {
		return agent.SystemEvent{
			Subtype: "codex_warning",
			Message: "codex isolated MCP configuration cleanup failed after the turn completed; the app-server was stopped and this provider refuses new sessions: " + err.Error(),
		}
	}
	return agent.SystemEvent{
		Subtype: "codex_warning",
		Message: "codex isolated MCP configuration cleanup failed after the turn completed; app-server shutdown unconfirmed and this provider refuses new sessions: " + err.Error(),
	}
}

// terminalReservedSlots holds the coarse Init/Result contract: stream deltas
// never take these slots, so the terminal warning plus result always fit
// without discarding queued history. Mirrors the interactive lane's
// coarse-slot reservation (interactive.go).
const terminalReservedSlots = 2

// emitTerminalWithWarning emits a cleanup warning plus the turn's terminal
// outcome without discarding queued history. Stream deltas never fill the
// reserved slots (see emit), so both normally fit; when only one slot
// remains the terminal outcome wins and the warning is dropped (it stays in
// the logs). A completely full buffer can only happen when the channel was
// filled bypassing emit (tiny test buffers, direct sends): the terminal then
// waits for the consumer to drain rather than discarding the oldest queued
// event, which carries the session identity the runner needs for the result
// and recovery state.
func (h *Handle) emitTerminalWithWarning(warning agent.SystemEvent, terminal agent.Event) {
	if cap(h.events)-len(h.events) >= 2 {
		h.emitCoarse(warning)
		h.emitCoarse(terminal)
		return
	}
	if cap(h.events)-len(h.events) == 1 {
		h.emitCoarse(terminal)
		return
	}
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- terminal:
	case <-h.closed:
	}
}

func mcpCleanupErrorEvent() agent.ErrorEvent {
	return agent.ErrorEvent{
		Message: "codex isolated MCP configuration cleanup failed",
		Code:    "mcp_cleanup_failed",
	}
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
			Message: fmt.Sprintf("Cancelled MCP elicitation from %s; autonomous mode has no user to prompt", emptyToUnknown(mcpServer)),
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

// emit publishes a stream-delta event on the events channel without
// blocking. Deltas never take the terminalReservedSlots held for the
// terminal warning plus result; once only those remain the delta is dropped
// silently — the runner is expected to keep up, and emitting backpressure
// into the JSON-RPC stream would deadlock the codex side.
//
// Close-protocol invariant: emit holds eventsMu.RLock for
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
	// Never take a slot reserved for the terminal warning plus result:
	// with at most terminalReservedSlots coarse sends over the handle's
	// life, the coarse senders below always fit what deltas left free.
	// When the consumer is slow the delta is dropped rather than stalling
	// the forwarder or displacing queued history.
	if len(h.events) >= cap(h.events)-terminalReservedSlots {
		return
	}
	select {
	case h.events <- ev:
	case <-h.closed:
	default:
	}
}

// emitCoarse publishes a terminal event (every Result/Error terminal above
// plus the Stop/failNow/cancel paths below) without the delta reservation:
// the reserved slots exist exactly so these sends fit. Non-blocking with the
// same close-protocol guard as emit.
func (h *Handle) emitCoarse(ev agent.Event) {
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

// emitInit publishes the session's single InitEvent and, right behind it,
// the deny notice. A later InitEvent (a thread/started after the
// thread/start response already emitted one) is dropped.
func (h *Handle) emitInit(ev agent.Event) {
	if !h.initEmitted.CompareAndSwap(false, true) {
		return
	}
	h.emit(ev)
	if h.denyNotice != nil {
		h.emit(*h.denyNotice)
		h.denyNotice = nil
	}
}
