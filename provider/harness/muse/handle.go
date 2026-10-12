package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Handle is one live MSP session. One Handle owns one `muse serve`
// subprocess (via the parent Provider) for its whole lifetime; turns
// multiplex over that connection as turn/start calls.
//
// Handle implements agent.Handle. The lifecycle is:
//
//  1. Provider.Spawn assembles a Handle and calls handle.start(ctx,
//     plan) to issue session/start + the first turn/start.
//  2. Provider.Resume assembles a Handle and calls handle.resume(ctx,
//     sessionID, spec) to issue session/resume.
//  3. The Handle runs a forwarder goroutine that consumes inbound
//     notifications, translates them via mapNotification, and
//     publishes to events.
//  4. On terminal ResultEvent / ErrorEvent / context cancellation /
//     host crash, the forwarder closes events and unsubscribes.
type Handle struct {
	provider *Provider // nil for unit tests; non-nil in real use
	client   *Client
	bridge   *ApprovalBridge
	spec     agent.Spec

	// rpcTimeout caps a single JSON-RPC request.
	rpcTimeout time.Duration

	idMu      sync.RWMutex
	sessionID string
	// turnID is the latest turn this handle started (or steered);
	// turn/interrupt and turn/steer target it.
	turnMu sync.RWMutex
	turnID string

	events   chan agent.Event
	notifyCh chan notification

	// Close-protocol invariant (cloned from the codex handle):
	//
	// The events channel has multiple potential senders (the forwarder
	// goroutine running emit, the failNow path posting a host-crashed
	// ErrorEvent) and multiple potential closers (forward's defer,
	// Stop, failNow). Every events-channel access is gated by
	// eventsMu + eventsClosed; h.closed is the lifecycle broadcast
	// closed exactly once via closedOnce.
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
	// Defaults to 30s.
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
		bridge:     NewApprovalBridge(spec.PermissionConfig, resolveReadOnly(spec)),
		spec:       spec,
		rpcTimeout: opts.RPCTimeout,
		events:     make(chan agent.Event, opts.EventBuffer),
		notifyCh:   make(chan notification, 256),
		closed:     make(chan struct{}),
		state:      &mapperState{model: DefaultModelID},
	}
}

// SessionID returns the MSP session id once session/start (or
// session/resume) has resolved.
func (h *Handle) SessionID() string {
	h.idMu.RLock()
	defer h.idMu.RUnlock()
	return h.sessionID
}

// CurrentTurnID returns the latest turn id this handle started. Empty
// before the first turn/start resolves.
func (h *Handle) CurrentTurnID() string {
	h.turnMu.RLock()
	defer h.turnMu.RUnlock()
	return h.turnID
}

func (h *Handle) setTurnID(id string) {
	h.turnMu.Lock()
	h.turnID = id
	h.turnMu.Unlock()
}

// Events returns the read-only event channel. Closes after the
// terminal event.
func (h *Handle) Events() <-chan agent.Event { return h.events }

// Inject steers the running turn via turn/steer{expectedTurnId, input}.
// It fails when no turn is running (the steer would have no target):
// callers should consume events until turn/started before steering.
func (h *Handle) Inject(ctx context.Context, text string) error {
	turnID := h.CurrentTurnID()
	if turnID == "" {
		return fmt.Errorf("muse inject: no running turn to steer")
	}
	sessionID := h.SessionID()
	if sessionID == "" {
		return fmt.Errorf("muse inject: session not started")
	}
	var input []map[string]any
	for _, part := range steerInput(text) {
		input = append(input, turnPartToParams(part))
	}
	_, err := h.client.RequestWithRetry(ctx, "turn/steer", map[string]any{
		"sessionId":      sessionID,
		"commandId":      newUUIDv7(),
		"expectedTurnId": turnID,
		"input":          input,
	}, h.rpcTimeout)
	if err != nil {
		return fmt.Errorf("muse turn/steer: %w", err)
	}
	return nil
}

// Stop interrupts the active turn (if any), unsubscribes the session,
// and closes the events channel. Idempotent.
func (h *Handle) Stop(ctx context.Context) error {
	h.closeOnce.Do(func() {
		sessionID := h.SessionID()
		if h.client != nil && sessionID != "" {
			// Best-effort: interrupt + unsubscribe. Ignore errors —
			// the host may have already torn down the session.
			turnID := h.CurrentTurnID()
			if turnID != "" {
				_, _ = h.client.Request(ctx, "turn/interrupt", map[string]any{
					"sessionId": sessionID,
					"commandId": newUUIDv7(),
					"turnId":    turnID,
				}, h.rpcTimeout)
			}
			_, _ = h.client.Request(ctx, "view/unsubscribe", map[string]any{
				"sessionId": sessionID,
			}, h.rpcTimeout)
			h.client.Unsubscribe(sessionID)
		}
	})
	h.signalClosed()
	h.closeEvents()
	return nil
}

// failNow marks the handle terminal with an ErrorEvent and closes the
// events channel. Used by the Provider when the `muse serve` host
// crashes. Safe to call concurrently with Stop / forward.
func (h *Handle) failNow(err error) {
	h.emit(agent.ErrorEvent{
		Message: fmt.Sprintf("muse provider failure: %s", err.Error()),
		Code:    "serve_host_crashed",
	})
	sessionID := h.SessionID()
	if h.client != nil && sessionID != "" {
		h.client.Unsubscribe(sessionID)
	}
	h.signalClosed()
	h.closeEvents()
}

// signalClosed closes h.closed exactly once.
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
}

// start performs the JSON-RPC bring-up: subscribe, session/start, the
// first turn/start, then the forwarder.
func (h *Handle) start(ctx context.Context, plan SpawnPlan) error {
	sessionID := plan.SessionID
	h.client.Subscribe(sessionID, func(n notification) {
		select {
		case h.notifyCh <- n:
		default:
		}
	})
	h.idMu.Lock()
	h.sessionID = sessionID
	h.idMu.Unlock()

	raw, err := h.client.RequestWithRetry(ctx, "session/start", plan.SessionStart, h.rpcTimeout)
	if err != nil {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/start: %w", err)
	}
	var started struct {
		Session SessionSummary `json:"session"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/start: decode response: %w", err)
	}
	if started.Session.SessionID == "" {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/start: empty session id in response")
	}
	if started.Session.ModelID != "" {
		h.state.model = started.Session.ModelID
	}
	h.emit(agent.InitEvent{SessionID: started.Session.SessionID, Raw: rawToAny(raw)})

	if err := h.startTurn(ctx, plan.TurnStart); err != nil {
		return err
	}

	go h.forward(ctx) //nolint:gosec // G118: session-lifetime goroutine, ctx is honored
	return nil
}

// resume performs the JSON-RPC re-attach: subscribe, session/resume,
// then the forwarder. A session-gone error surfaces
// agent.ErrSessionNotFound so the runner can fail the retry honestly.
func (h *Handle) resume(ctx context.Context, sessionID string, params map[string]any) error {
	h.client.Subscribe(sessionID, func(n notification) {
		select {
		case h.notifyCh <- n:
		default:
		}
	})
	h.idMu.Lock()
	h.sessionID = sessionID
	h.idMu.Unlock()

	raw, err := h.client.RequestWithRetry(ctx, "session/resume", params, h.rpcTimeout)
	if err != nil {
		h.client.Unsubscribe(sessionID)
		if IsSessionNotFound(err) {
			return fmt.Errorf("%w: %v", agent.ErrSessionNotFound, err)
		}
		return fmt.Errorf("session/resume: %w", err)
	}
	var resumed struct {
		Session SessionSummary `json:"session"`
	}
	if err := json.Unmarshal(raw, &resumed); err != nil {
		h.client.Unsubscribe(sessionID)
		return fmt.Errorf("session/resume: decode response: %w", err)
	}
	if resumed.Session.ModelID != "" {
		h.state.model = resumed.Session.ModelID
	}
	h.emit(agent.InitEvent{SessionID: sessionID, Raw: rawToAny(raw)})

	go h.forward(ctx) //nolint:gosec // G118: session-lifetime goroutine, ctx is honored
	return nil
}

// startTurn issues one turn/start and records the turn id from the
// ack. The ack's turnId is authoritative — always taken from the ack,
// never derived.
func (h *Handle) startTurn(ctx context.Context, turnParams map[string]any) error {
	raw, err := h.client.RequestWithRetry(ctx, "turn/start", turnParams, h.rpcTimeout)
	if err != nil {
		return h.classifyFirstTurnFailure(fmt.Errorf("turn/start: %w", err))
	}
	var ack struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return fmt.Errorf("turn/start: decode ack: %w", err)
	}
	if ack.TurnID == "" {
		return fmt.Errorf("turn/start: empty turn id in ack")
	}
	h.setTurnID(ack.TurnID)
	return nil
}

// classifyFirstTurnFailure maps the known logged-out failure shape to a
// named error. model/list answers from cache even when logged out, so
// the first turn is where a missing login surfaces — typically as a
// "does not exist or you lack access" failure. The classification is
// advisory (a prefix on the wrapped error), never a branch: the turn
// still failed and the session still ends.
func (h *Handle) classifyFirstTurnFailure(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, marker := range []string{
		"does not exist or you lack access",
		"not logged in",
		"authentication",
		"unauthorized",
	} {
		if containsFold(msg, marker) {
			return fmt.Errorf("muse turn/start (possible logged-out host — no login-status command exists; META_API_KEY takes priority over the login): %w", err)
		}
	}
	return err
}

// forward is the per-Handle goroutine that consumes inbound
// notifications, translates them to agent.Events, and publishes.
func (h *Handle) forward(ctx context.Context) {
	defer func() {
		h.closeOnce.Do(func() {
			sessionID := h.SessionID()
			if h.client != nil && sessionID != "" {
				h.client.Unsubscribe(sessionID)
			}
		})
		h.signalClosed()
		h.closeEvents()
	}()

	for {
		select {
		case <-ctx.Done():
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
	// Server-requests come down the same channel; the host REQUIRES
	// an answer (approval/request, userInput/request) and re-issues
	// unanswered requests on the next subscribe.
	if len(n.ServerRequestID) > 0 {
		h.handleServerRequest(n)
		return false
	}

	// Route guard: only this handle's session is mapped. Other
	// sessions never share this Client (one host per Handle), so a
	// mismatch means a stale redelivery — drop it.
	sessionID := extractSessionID(n.Params)
	if sessionID != "" && sessionID != h.SessionID() {
		return false
	}

	// view/gap is fatal to the live view: re-anchor before trusting
	// the transcript again. The gap event itself is still mapped so
	// the hole is visible on the stream.
	if n.Method == "view/gap" {
		var gap ViewGapParams
		_ = json.Unmarshal(n.Params, &gap)
		for _, ev := range mapNotification(n.Method, n.Params, h.state, rawToAny(n.Params)) {
			h.emit(ev)
		}
		if err := h.recoverFromGap(gap); err != nil {
			h.emit(agent.ErrorEvent{
				Message: fmt.Sprintf("muse view/gap recovery failed: %s", err.Error()),
				Code:    "view_gap_unrecoverable",
			})
			return true
		}
		return false
	}

	var rawObj map[string]any
	if len(n.Params) > 0 {
		_ = json.Unmarshal(n.Params, &rawObj)
	}

	events := mapNotification(n.Method, n.Params, h.state, rawObj)
	for _, ev := range events {
		switch ev.Kind() {
		case agent.EventResult, agent.EventError:
			h.emit(ev)
			return true
		}
		h.emit(ev)
	}
	return false
}

// recoverFromGap re-anchors the live view after a view/gap: re-issue
// view/subscribe at the gap's After cursor so the host replays (after,
// head] as ordinary view notifications before any live event — one
// gapless cursor sequence. Item state tracked before the gap is
// dropped so stale revisions cannot shadow the replayed transcript.
func (h *Handle) recoverFromGap(gap ViewGapParams) error {
	sessionID := h.SessionID()
	if sessionID == "" {
		return fmt.Errorf("no session to re-anchor")
	}
	h.state.items = make(map[string]*Item)
	params := map[string]any{"sessionId": sessionID}
	if gap.After != "" {
		params["after"] = gap.After
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.rpcTimeout)
	defer cancel()
	if _, err := h.client.Request(ctx, "view/subscribe", params, h.rpcTimeout); err != nil {
		return fmt.Errorf("view/subscribe: %w", err)
	}
	return nil
}

// handleServerRequest answers host-initiated requests and routes
// approval decisions through the bridge.
func (h *Handle) handleServerRequest(n notification) {
	switch n.Method {
	case "approval/request":
		// Presentation receipt first (required, changes no state),
		// then the decision travels as approval/decide.
		_ = h.client.RespondToServerRequest(n.ServerRequestID)
		req, err := parseApprovalRequest(n.Params)
		if err != nil {
			return
		}
		h.decideApproval(req)
	case "userInput/request":
		// Autonomous sessions have no user to prompt: acknowledge
		// presentation and surface the prompt on the stream. The
		// request stays pending host-side.
		_ = h.client.RespondToServerRequest(n.ServerRequestID)
		h.emit(agent.SystemEvent{
			Subtype: "user_input_requested",
			Message: "host requested user input; autonomous session has no user to prompt",
			Raw:     rawToAny(n.Params),
		})
	default:
		// Unknown server-request: answer the receipt so the host
		// does not re-issue it forever, and surface it.
		_ = h.client.RespondToServerRequest(n.ServerRequestID)
		h.emit(agent.SystemEvent{
			Subtype: "unhandled_server_request",
			Message: "Answered unhandled muse server request: " + n.Method,
			Raw:     rawToAny(n.Params),
		})
	}
}

// decideApproval evaluates an approval/requested notification through
// the bridge and answers with approval/decide. It also handles the
// approval/requested NOTIFICATION path (called from handleNotification
// via mapNotification's approval_requested event + explicit decide;
// see below).
func (h *Handle) decideApproval(req ApprovalRequestParams) {
	verdict := h.bridge.Evaluate(req)

	toolName := req.ToolName
	if toolName == "" {
		toolName = "approval_unknown"
	}
	toolUseID := "approval-" + req.ApprovalID
	h.emit(agent.ToolUseEvent{
		ToolName:  toolName,
		ToolUseID: toolUseID,
		Input:     approvalInput(req),
		Raw:       rawToAny(mustMarshal(req)),
	})
	h.emit(agent.ToolResultEvent{
		ToolName:  toolName,
		ToolUseID: toolUseID,
		Content:   verdict.Reason,
		IsError:   !verdict.Allow,
		Raw:       rawToAny(mustMarshal(req)),
	})

	choiceID, err := h.bridge.PickChoice(req, verdict)
	if err != nil {
		h.emit(agent.SystemEvent{
			Subtype: "approval_no_choice",
			Message: "muse approval has no matching choice arm: " + err.Error(),
			Raw:     rawToAny(mustMarshal(req)),
		})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.rpcTimeout)
	defer cancel()
	_, derr := h.client.Request(ctx, "approval/decide", map[string]any{
		"sessionId":     h.SessionID(),
		"commandId":     newUUIDv7(),
		"approvalId":    req.ApprovalID,
		"requirementId": req.CurrentRequirementID,
		"choiceId":      choiceID,
	}, h.rpcTimeout)
	if derr != nil {
		h.emit(agent.SystemEvent{
			Subtype: "approval_decide_failed",
			Message: "muse approval/decide failed: " + derr.Error(),
			Raw:     rawToAny(mustMarshal(req)),
		})
		return
	}
	if !verdict.Allow {
		h.emit(agent.SystemEvent{
			Subtype: "approval_denied",
			Message: "Denied: " + verdict.Reason,
			Raw:     rawToAny(mustMarshal(req)),
		})
	}
}

func approvalInput(req ApprovalRequestParams) map[string]any {
	subject := req.Subject
	switch {
	case subject.Command != "":
		return map[string]any{"command": subject.Command}
	case subject.Path != "":
		return map[string]any{"path": subject.Path}
	case subject.Target != "":
		return map[string]any{"target": subject.Target}
	default:
		return map[string]any{}
	}
}

// emit publishes an event on the events channel without blocking. If
// the channel is full, the event is dropped silently — the runner is
// expected to keep up; backpressuring into the JSON-RPC stream would
// deadlock the host.
//
// Close-protocol invariant: emit holds eventsMu.RLock for the duration
// of the send and aborts early when eventsClosed is set. The h.closed
// broadcast is also selected against so a slow send unblocks promptly
// when shutdown begins.
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

func rawToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func mustMarshal(v any) json.RawMessage {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return buf
}

func containsFold(haystack, needle string) bool {
	return len(haystack) >= len(needle) && searchFold(haystack, needle)
}

func searchFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
